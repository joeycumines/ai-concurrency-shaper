package transcode

import (
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

// Mirrors the standard-library algorithm:
//
// https://go.dev/src/net/http/httputil/reverseproxy.go
// See hopHeaders and removeHopByHopHeaders.
//
// RFC connection handling:
// https://www.rfc-editor.org/rfc/rfc9110.html#section-7.6.1

// fixedHopByHopHeaders is the fixed list removed by the standard library's
// reverse proxy, including the non-standard but widely emitted
// Proxy-Connection.
var fixedHopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection", // non-standard but widely emitted
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// RemoveHopByHopHeaders removes every header nominated by a Connection token
// and then the fixed hop-by-hop list. This must run before deleting
// Connection itself.
func RemoveHopByHopHeaders(h http.Header) {
	for _, connectionValue := range h.Values("Connection") {
		for _, token := range strings.Split(connectionValue, ",") {
			token = textproto.TrimString(token)
			if token == "" {
				continue
			}
			if validHTTPFieldName(token) {
				h.Del(token)
			}
		}
	}

	for _, name := range fixedHopByHopHeaders {
		h.Del(name)
	}
}

// validHTTPFieldName reports whether s is a valid HTTP field name using token
// characters. Malformed Connection tokens are rejected rather than passed to
// Header.Del as arbitrary input.
func validHTTPFieldName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)):
		default:
			return false
		}
	}
	return true
}

// RemoveTransformedRepresentationHeaders removes entity headers that describe
// the upstream representation. A converted body is a new representation, so
// these upstream values no longer describe the bytes sent downstream.
//
// Digest fields:
// https://www.rfc-editor.org/rfc/rfc9530.html
//
// HTTP message signatures:
// https://www.rfc-editor.org/rfc/rfc9421.html
func RemoveTransformedRepresentationHeaders(h http.Header) {
	for _, name := range []string{
		"Content-Length",
		"Content-Encoding",
		"Content-Md5",
		"Content-Range",
		"Accept-Ranges",
		"Digest",
		"Content-Digest",
		"Repr-Digest",
		"Etag",
		"Last-Modified",
		"Signature",
		"Signature-Input",
	} {
		h.Del(name)
	}
}

// RemoveTransformedRequestRepresentationHeaders removes representation
// metadata from a transformed request (review-j finding 12): the converted
// body is a new representation, so inbound integrity digests, message
// signatures, content metadata, and validators describe the original bytes
// and must never be forwarded or signed. Content-Length is removed so
// BuildConvertedRequest recomputes it from the converted body; the remaining
// headers are dropped entirely.
func RemoveTransformedRequestRepresentationHeaders(h http.Header) {
	for _, name := range []string{
		"Content-Length",
		"Content-Encoding",
		"Content-Md5",
		"Content-Range",
		"Digest",
		"Content-Digest",
		"Repr-Digest",
		"Signature",
		"Signature-Input",
		"Etag",
		"Last-Modified",
		"If-Match",
		"If-None-Match",
		"If-Modified-Since",
		"If-Unmodified-Since",
		"If-Range",
	} {
		h.Del(name)
	}
}

// BuildMappedURL joins the configured upstream base path with the mapping's
// upstream path and preserves the base query. Client query parameters are
// rejected unless explicitly allowed: unknown client query parameters can
// alter target behavior, and the configured base query is required for
// deployments whose endpoint encodes controls such as api-version.
//
// The standard library's reverse-proxy query joining can be reviewed at:
// https://go.dev/src/net/http/httputil/reverseproxy.go
func BuildMappedURL(
	base *url.URL,
	upstreamPath string,
	clientQuery url.Values,
	allowedClientQuery map[string]struct{},
) (*url.URL, error) {
	if base == nil {
		return nil, fmt.Errorf("nil upstream URL")
	}

	out := *base

	joined, err := url.JoinPath(base.Path, upstreamPath)
	if err != nil {
		return nil, fmt.Errorf("join upstream path: %w", err)
	}
	// url.JoinPath drops the leading slash when the base path is empty; the
	// upstream path is absolute and the result must stay absolute.
	if !strings.HasPrefix(joined, "/") {
		joined = "/" + joined
	}
	out.Path = joined
	out.RawPath = ""

	query := base.Query()
	for key, values := range clientQuery {
		if _, ok := allowedClientQuery[key]; !ok {
			return nil, fmt.Errorf(
				"client query parameter %q is not allowed on transcoded routes",
				key,
			)
		}
		for _, value := range values {
			query.Add(key, value)
		}
	}
	out.RawQuery = query.Encode()

	return &out, nil
}
