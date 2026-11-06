package transcode

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// upstreamClientQuery returns the client query the upstream may see. The
// Gemini alt selector describes the client's own representation, so it is
// removed before the allowlist check. The result is a copy: r.URL is never
// mutated. A malformed query is refused (see parseClientQuery), never
// partially read.
func upstreamClientQuery(r *http.Request, client ClientProtocol) (url.Values, error) {
	query, err := parseClientQuery(r.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	if client == ClientGemini {
		query.Del("alt")
	}
	return query, nil
}

// parseClientQuery parses the client query for validation and forwarding.
// url.ParseQuery skips a pair it cannot parse (a ';' separator, an invalid
// percent escape) and reports it only through its error. Ignoring that error
// would let a pair escape the allowlist check while still sitting in the
// journaled URL, so a malformed query is a client fault and is refused
// whole. The error names only the parse failure, never a value: url's
// EscapeError embeds raw bytes of the offending escape, so the underlying
// url.ParseQuery error is deliberately discarded and only the sentinel
// errClientQueryParameter is wrapped with %w.
func parseClientQuery(rawQuery string) (url.Values, error) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, fmt.Errorf("%w: the query string is malformed", errClientQueryParameter)
	}
	return query, nil
}

// errGeminiQueryCredential refuses a credential carried in the query string.
// The message names the header to use and never echoes the value.
var errGeminiQueryCredential = errors.New(
	"API keys must not be sent in the query string; use the X-Goog-Api-Key header",
)

// HasQueryCredential reports whether the raw query string carries a
// credential parameter (case-insensitive). Gemini client routes — transcoded
// and native — refuse query credentials outright because the URL is journaled
// and logged. The scan runs over the raw query, not parsed url.Values:
// url.ParseQuery silently drops pairs whose key or value fails QueryUnescape
// (e.g. key=secret%zz), which would bypass the check, and it drops
// ';'-delimited segments entirely. Both '&' and ';' delimit pairs here, so a
// credential after a ';' (?foo=1;key=SECRET) is still found.
func HasQueryCredential(rawQuery string) bool {
	for _, pair := range rawQueryPairs(rawQuery) {
		name, _, _ := strings.Cut(pair, "=")
		// Cut on a raw %3D / %3d before QueryUnescape: unescaping is
		// all-or-nothing, so a malformed escape later in the pair (e.g.
		// "key%3Dsecret%zz") would otherwise discard the unescape and hide
		// the credential name. The scan reads raw bytes only: lowercasing
		// first would change the byte length for non-ASCII input (invalid
		// UTF-8 becomes the 3-byte RuneError; U+023A lowercases to a 3-byte
		// rune), so an index into the lowered string could exceed len(name)
		// and panic, or shift left and miss the marker.
		name = cutRawPercent3D(name)
		if unescaped, err := url.QueryUnescape(name); err == nil {
			// An escaped '=' (raw "key%3DSECRET" or "key%3D%3D=x") still
			// names the credential parameter, so the unescaped name is cut
			// at its first '=' as well.
			name, _, _ = strings.Cut(unescaped, "=")
		}
		switch strings.ToLower(name) {
		case "key", "access_token", "oauth_token":
			return true
		}
	}
	return false
}

// cutRawPercent3D returns s cut at the first raw "%3d" / "%3D" spelling,
// scanning bytes without changing length. It exists because
// strings.ToLower can expand or shrink non-ASCII input, making an index into
// the lowered string unusable on the original.
func cutRawPercent3D(s string) string {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '%' && s[i+1] == '3' && (s[i+2] == 'd' || s[i+2] == 'D') {
			return s[:i]
		}
	}
	return s
}

// rawQueryPairs splits a raw query into its non-empty pairs, treating both
// '&' and ';' as delimiters: net/http keeps ';' in RawQuery, and a scan that
// split only on '&' would read "foo=1;key=SECRET" as one pair named foo.
func rawQueryPairs(rawQuery string) []string {
	return strings.FieldsFunc(rawQuery, func(r rune) bool {
		return r == '&' || r == ';'
	})
}

// geminiClientQuery applies the client query rules of a Gemini operation.
// Credential parameters are refused outright (the scan runs first, over the
// raw query, so a credential in a malformed pair still gets the credential
// refusal), the query must parse, alt may be given at most once, and
// generateContent accepts only alt=json or no alt. The query is parsed by the
// same parser as the allowlist check, so both see the same pairs.
func geminiClientQuery(match GeminiRouteMatch, rawQuery string) error {
	if HasQueryCredential(rawQuery) {
		return errGeminiQueryCredential
	}
	query, err := parseClientQuery(rawQuery)
	if err != nil {
		return err
	}
	alt := query["alt"]
	if len(alt) > 1 {
		return errors.New("alt must be given at most once")
	}
	if match.Operation == GeminiStream {
		if len(alt) != 1 || alt[0] != "sse" {
			return errors.New("streamGenerateContent requires alt=sse")
		}
		return nil
	}
	if len(alt) == 1 && alt[0] != "json" {
		return fmt.Errorf("generateContent accepts alt=json only; alt=%q is not supported", alt[0])
	}
	return nil
}

// geminiBodyModel checks the body model, which the Gemini API carries in the
// path. A body model that names the path model is accepted and noted; any
// other non-empty body model is a conflict.
func geminiBodyModel(facts GeminiRequestFacts, pathModel string, report *ConversionReport) error {
	if facts.BodyModel == nil || *facts.BodyModel == "" {
		return nil
	}
	body := *facts.BodyModel
	if body != pathModel && body != "models/"+pathModel {
		return fmt.Errorf("body model %q conflicts with path model %q", body, pathModel)
	}
	return report.Note(
		FeatureGeminiBodyModel, "model",
		"the body model names the path model and was not forwarded",
	)
}
