package transcode

import (
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

func isEventStreamMediaType(mediaType string) bool {
	return mediaType == "text/event-stream"
}

// acceptRange is one parsed media range of an Accept header.

func splitAcceptRanges(accept string) []string {
	var parts []string
	start := 0
	inQuotes := false
	escaped := false
	for i := 0; i < len(accept); i++ {
		switch c := accept[i]; {
		case escaped:
			escaped = false
		case inQuotes && c == '\\':
			escaped = true
		case c == '"':
			inQuotes = !inQuotes
		case c == ',' && !inQuotes:
			parts = append(parts, accept[start:i])
			start = i + 1
		}
	}
	return append(parts, accept[start:])
}

// parseAcceptRanges parses every media range of an Accept header. Ranges
// that fail to parse, or whose q value is outside 0..1, are skipped: a
// malformed range expresses no usable preference and is ignored — it never
// decides and never disturbs the ordering of the ranges around it.

func parseAcceptRanges(accept string) []acceptRange {
	var ranges []acceptRange
	for order, part := range splitAcceptRanges(accept) {
		mediaType, params, err := mime.ParseMediaType(part)
		if err != nil {
			continue
		}
		r := acceptRange{mediaType: mediaType, q: 1, order: order}
		if raw, ok := params["q"]; ok {
			q, err := strconv.ParseFloat(raw, 64)
			// math.IsNaN rejects NaN, which passes the numeric bounds check
			// (NaN compares false against everything) and would otherwise
			// poison same-specificity selection as an undefeatable exclusion.
			if err != nil || math.IsNaN(q) || q < 0 || q > 1 {
				continue
			}
			r.q = q
		}
		ranges = append(ranges, r)
	}
	return ranges
}

// acceptPreference is the client's preference for one representation derived
// from an Accept header.

func effectiveAcceptPreference(ranges []acceptRange, mediaType, typeWildcard string) acceptPreference {
	var best acceptPreference
	best.specificity = -1
	for _, r := range ranges {
		specificity := -1
		switch r.mediaType {
		case mediaType:
			specificity = 2
		case typeWildcard:
			specificity = 1
		case "*/*":
			specificity = 0
		}
		if specificity < 0 {
			continue
		}
		if specificity > best.specificity ||
			specificity == best.specificity &&
				(r.q > best.q || r.q == best.q && r.order < best.order) {
			best = acceptPreference{
				q:           r.q,
				specificity: specificity,
				order:       r.order,
			}
		}
	}
	best.acceptable = !(best.specificity >= 0 && best.q == 0)
	return best
}

// AcceptIsEventStream reports whether the client's Accept header selects
// text/event-stream as the most-preferred acceptable representation over
// application/json. Every media range is parsed (media type and q); a q=0
// range excludes the representation it names (an unmentioned representation
// remains acceptable by default); malformed ranges are ignored. When exactly
// one representation is acceptable it is selected; otherwise the documented
// precedence applies: (1) effective quality, where the most specific
// applicable range determines a representation's quality; (2) specificity
// (exact type over type/* over */*); (3) order of appearance in the header.
// A tie that remains (e.g. Accept: */*, or no acceptable representation at
// all) defaults to application/json, the non-streaming representation.

func AcceptIsEventStream(accept string) bool {
	ranges := parseAcceptRanges(accept)
	sse := effectiveAcceptPreference(ranges, "text/event-stream", "text/*")
	jsonPref := effectiveAcceptPreference(ranges, "application/json", "application/*")
	if !sse.acceptable && !jsonPref.acceptable {
		return false
	}
	if !jsonPref.acceptable {
		return true
	}
	if !sse.acceptable {
		return false
	}
	if sse.q != jsonPref.q {
		return sse.q > jsonPref.q
	}
	if sse.specificity != jsonPref.specificity {
		return sse.specificity > jsonPref.specificity
	}
	if sse.order != jsonPref.order {
		return sse.order < jsonPref.order
	}
	return false
}

// isEventStream reports whether the response is an SSE stream, matched by
// media type exactly — text/event-streaming and other lookalikes are not
// streams.

func IsUpgradeRequest(r *http.Request) bool {
	if r == nil || r.Header == nil {
		return false
	}
	return isUpgradeRequest(r)
}

// isUpgradeRequest reports whether the request carries an Upgrade token.

func isUpgradeRequest(r *http.Request) bool {
	if len(r.Header.Values("Upgrade")) > 0 {
		return true
	}
	for _, value := range r.Header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// isContextCancellationError reports whether the error is cancellation-derived
// (context.Canceled or context.DeadlineExceeded, possibly wrapped). Client
// abort suppression applies only to such errors: an unrelated transport or
// body failure racing with a client cancellation must win as its true
// classification.

type acceptRange struct {
	mediaType string  // lowercased
	q         float64 // 0..1
	order     int     // position in the header; the first range is 0
}

// splitAcceptRanges splits an Accept header into its media ranges on commas
// that are outside quoted strings. RFC 9110 quoted-string parameter values
// may contain commas (and backslash-escaped characters); splitting naively
// on every comma would destroy such a range.

type acceptPreference struct {
	acceptable  bool    // the representation is not excluded (see below)
	q           float64 // effective quality of the deciding range
	specificity int     // specificity of the deciding range
	order       int     // position of the deciding range
}

// effectiveAcceptPreference derives the client's preference for mediaType
// from parsed Accept ranges, matched by the exact type, typeWildcard, or
// */*. The most specific applicable range determines the representation's
// quality (RFC 9110 §12.5.1): a q=0 exact exclusion wins over a later */*,
// and a less specific range never dilutes the most specific decision. Among
// equally specific ranges the highest q wins, and the earliest such range
// decides order ties. A representation with no applicable range is
// acceptable by default (RFC 9110: unmentioned representations are
// acceptable); it is excluded only when an applicable range exists and the
// most specific tier's quality is 0.
