package transcode

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestTransportErrorBodyNeverContainsUpstreamURL pins the client-facing
// sanitization of upstream transport errors: the
// transcode 502 body must never echo the outbound URL — a credential-bearing
// upstream base query (e.g. Gemini ?key=) must not reach the client through
// a url.Error-shaped transport failure. The detail remains available
// server-side via the log.
func TestTransportErrorBodyNeverContainsUpstreamURL(t *testing.T) {
	mapping := responsesMapping(t)
	handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		u := &url.URL{
			Scheme:   "https",
			Host:     "upstream.example",
			Path:     "/v1/chat/completions",
			RawQuery: "key=AIzaSySECRETVALUE",
		}
		return nil, &url.Error{
			Op:  "Post",
			URL: u.String(),
			Err: errors.New("dial tcp 127.0.0.1:1: connect: connection refused"),
		}
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	outcome := <-outcomes
	if outcome.Provenance != ProvenanceUpstreamTransportError {
		t.Fatalf("provenance = %v, want upstream_transport_error", outcome.Provenance)
	}
	body := rec.Body.String()
	for _, leak := range []string{
		"AIzaSySECRETVALUE",
		"key=AIzaSy",
		"https://upstream.example",
		"upstream.example/v1/chat",
	} {
		if strings.Contains(body, leak) {
			t.Fatalf("client body leaks upstream URL material %q: %s", leak, body)
		}
	}
	if !strings.Contains(body, "upstream") {
		t.Fatalf("client body should still identify an upstream failure: %s", body)
	}
}

// TestTransportErrorLogRedactsNestedChainURLs pins the log-side redaction of
// nested url.Error chains (parity with the native sanitizer): a
// custom transport wrapping an inner url.Error must not leak the inner
// credential-bearing query into the logged detail.
func TestTransportErrorLogRedactsNestedChainURLs(t *testing.T) {
	inner := &url.Error{
		Op:  "Post",
		URL: "https://upstream.example/v1/chat/completions?key=AIzaSyINNERSECRET",
		Err: errors.New("connection reset by peer"),
	}

	outer := &url.Error{
		Op:  "Post",
		URL: "https://upstream.example/v1/chat/completions",
		Err: inner,
	}
	sanitized := sanitizeUpstreamTransportError(outer)
	if strings.Contains(sanitized.Error(), "AIzaSyINNERSECRET") {
		t.Fatalf("nested chain URL leaked: %v", sanitized)
	}
	// A non-url.Error wrapper carrying a url.Error subtree is scrubbed in its
	// rendered form.
	wrapped := fmt.Errorf("roundtrip: %w", outer)
	sanitizedWrapped := sanitizeUpstreamTransportError(wrapped)
	if strings.Contains(sanitizedWrapped.Error(), "AIzaSyINNERSECRET") {
		t.Fatalf("wrapped chain URL leaked: %v", sanitizedWrapped)
	}
	if !strings.HasPrefix(sanitizedWrapped.Error(), "roundtrip:") {
		t.Fatalf("wrapper context lost: %v", sanitizedWrapped)
	}
	// A plain non-url.Error error passes through unchanged.
	plain := errors.New("dial tcp: no such host")
	if got := sanitizeUpstreamTransportError(plain); got.Error() != plain.Error() {
		t.Fatalf("plain error mutated: %v", got)
	}
}

func TestTransportErrorLogRedactsWrappedNestedURLsAndQuotedURLText(t *testing.T) {
	inner := &url.Error{
		Op:  "Post",
		URL: "https://upstream.example/v1/chat/completions?key=inner-secret",
		Err: errors.New("connection reset"),
	}
	middle := fmt.Errorf("redirect wrapper: %w", inner)
	outerURL := "https://upstream.example/v1/responses?token=outer-secret"
	outer := &url.Error{
		Op:  "Post",
		URL: outerURL,
		Err: middle,
	}
	wrapped := fmt.Errorf("failed URL %q: %w", outerURL, outer)

	for _, input := range []error{outer, wrapped} {
		got := sanitizeUpstreamTransportError(input)
		for _, secret := range []string{"inner-secret", "outer-secret"} {
			if strings.Contains(got.Error(), secret) {
				t.Fatalf("sanitized error leaks %q: %v", secret, got)
			}
		}
	}
}

// TestTransportErrorLogRedactsEveryURLInTheChain pins the transcode
// sanitizer to the same guarantee as the native one: no secret query value
// survives in the message or behind Unwrap, including an outer URL that
// prefixes an unquoted inner URL and an outer URL that cannot be parsed.
func TestTransportErrorLogRedactsEveryURLInTheChain(t *testing.T) {
	const outerURL = "https://upstream.example/v1?key=outer-secret"
	prefixInner := &url.Error{Op: "Get", URL: outerURL + "&token=inner-secret", Err: errors.New("connection refused")}
	for name, input := range map[string]error{
		"outer prefixes inner": &url.Error{
			Op:  "Post",
			URL: outerURL,
			Err: fmt.Errorf("redirect to %s: %w", prefixInner.URL, prefixInner),
		},
		"wrapped nested chain": fmt.Errorf("round trip: %w", &url.Error{
			Op:  "Post",
			URL: outerURL,
			Err: fmt.Errorf("redirect detail: %w", &url.Error{
				Op: "Get", URL: "https://upstream.example/v2?token=inner-secret", Err: errors.New("refused"),
			}),
		}),
		"unparseable outer": &url.Error{
			Op:  "Post",
			URL: "https://upstream.example/v1\x7f?key=outer-secret",
			Err: &url.Error{Op: "Get", URL: "https://upstream.example/v2?token=inner-secret", Err: errors.New("refused")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := sanitizeUpstreamTransportError(input)
			for e := error(got); e != nil; e = errors.Unwrap(e) {
				for _, secret := range []string{"outer-secret", "inner-secret"} {
					if strings.Contains(e.Error(), secret) {
						t.Errorf("sanitized chain leaks %q at %T: %s", secret, e, e)
					}
				}
			}
			if _, ok := errors.AsType[*url.Error](got); !ok {
				t.Fatalf("errors.As(%T) found no *url.Error", got)
			}
		})
	}
}
