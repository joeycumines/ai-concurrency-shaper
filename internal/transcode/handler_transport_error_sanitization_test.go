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
// nested url.Error chains (reviewer F1 parity with the native sanitizer): a
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
