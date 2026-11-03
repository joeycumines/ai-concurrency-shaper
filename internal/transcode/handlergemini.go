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
// mutated.
func upstreamClientQuery(r *http.Request, client ClientProtocol) url.Values {
	query := r.URL.Query()
	if client == ClientGemini {
		query.Del("alt")
	}
	return query
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
// ';'-delimited segments entirely.
func HasQueryCredential(rawQuery string) bool {
	for pair := range strings.SplitSeq(rawQuery, "&") {
		name, _, _ := strings.Cut(pair, "=")
		if unescaped, err := url.QueryUnescape(name); err == nil {
			name = unescaped
		}
		// A name that unescapes to "key=" (raw "key%3D") still names the
		// credential parameter, so compare the name with any trailing '='
		// stripped as well.
		name = strings.TrimSuffix(name, "=")
		switch strings.ToLower(name) {
		case "key", "access_token", "oauth_token":
			return true
		}
	}
	return false
}

// parseQueryTolerant parses a raw query into url.Values, keeping pairs whose
// value fails to unescape (with the raw value) so alt validation still sees
// them; only the credential scan must see malformed pairs, and it runs on
// the raw query before this.
func parseQueryTolerant(rawQuery string) url.Values {
	query := make(url.Values)
	for pair := range strings.SplitSeq(rawQuery, "&") {
		if pair == "" {
			continue
		}
		name, value, _ := strings.Cut(pair, "=")
		if unescaped, err := url.QueryUnescape(name); err == nil {
			name = unescaped
		}
		if unescaped, err := url.QueryUnescape(value); err == nil {
			value = unescaped
		}
		query[name] = append(query[name], value)
	}
	return query
}

// geminiClientQuery applies the client query rules of a Gemini operation.
// Credential parameters are refused outright, alt may be given at most once,
// and generateContent accepts only alt=json or no alt.
func geminiClientQuery(match GeminiRouteMatch, rawQuery string) error {
	if HasQueryCredential(rawQuery) {
		return errGeminiQueryCredential
	}
	query := parseQueryTolerant(rawQuery)
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
