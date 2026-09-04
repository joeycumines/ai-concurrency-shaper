package transcode

// Review-08 blocker 10 regression tests: transcoded routes use explicit
// request and response header allowlists — nothing the client sent reaches
// the target provider unless it is on the list, and nothing the upstream
// sent reaches the client-facing origin unless it is documented
// entity/metadata (request-id family, rate-limit informational headers).

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// TestTranscodeRequestHeaderAllowlist proves every client-supplied header
// outside the documented allowlist is stripped from the outbound request:
// cookies, forwarding headers, range/conditional controls, idempotency keys,
// source-provider controls, credentials, and extension/tenant headers never
// reach the target provider (review-08 blocker 10).
func TestTranscodeRequestHeaderAllowlist(t *testing.T) {
	var (
		gotHeader http.Header
	)
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		gotHeader = req.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	for _, name := range []string{
		"Cookie", "Forwarded", "X-Real-IP", "X-Forwarded-For", "X-Forwarded-Host",
		"X-Forwarded-Proto", "Range", "If-Match", "If-None-Match", "Expect",
		"Idempotency-Key", "X-Provider-Extension", "X-Tenant-Id", "Origin",
		"Referer", "X-Api-Key", "X-Goog-Api-Key", "Anthropic-Version", "X-Custom",
	} {
		req.Header.Set(name, "leak")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, name := range []string{
		"Cookie", "Forwarded", "X-Real-IP", "X-Forwarded-For", "X-Forwarded-Host",
		"X-Forwarded-Proto", "Range", "If-Match", "If-None-Match", "Expect",
		"Idempotency-Key", "X-Provider-Extension", "X-Tenant-Id", "Origin",
		"Referer", "X-Api-Key", "X-Goog-Api-Key", "Anthropic-Version", "X-Custom",
	} {
		if got := gotHeader.Get(name); got != "" {
			t.Errorf("client header %s leaked to the upstream: %q", name, got)
		}
	}
	if got := gotHeader.Get("Accept"); got != "application/json" {
		t.Errorf("outbound Accept = %q, want application/json", got)
	}
	if got := gotHeader.Get("Content-Type"); got != "application/json" {
		t.Errorf("outbound Content-Type = %q, want application/json", got)
	}
}

// TestTranscodeResponseHeaderAllowlist proves the upstream response headers
// exposed to the client-facing origin are limited to the request-id family
// and the rate-limit informational headers: Set-Cookie and other control or
// extension headers never leak across providers (review-08 blocker 10).
func TestTranscodeResponseHeaderAllowlist(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		header := http.Header{"Content-Type": []string{"application/json"}}
		header.Set("X-Request-Id", "req_1")
		header.Set("x-ratelimit-limit-requests", "100")
		header.Set("x-ratelimit-remaining-tokens", "50")
		header.Set("Retry-After", "5")
		header.Set("Set-Cookie", "session=secret")
		header.Set("ETag", `"abc"`)
		header.Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
		header.Set("X-Provider-Extension", "leak")
		header.Set("Set-Cookie2", "x=y")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, name := range []string{"Set-Cookie", "Set-Cookie2", "ETag", "Last-Modified", "X-Provider-Extension"} {
		if got := rec.Header().Get(name); got != "" {
			t.Errorf("upstream header %s leaked to the client: %q", name, got)
		}
	}
	for _, name := range []string{"X-Request-Id", "X-Ratelimit-Limit-Requests", "X-Ratelimit-Remaining-Tokens", "Retry-After"} {
		if got := rec.Header().Get(name); got == "" {
			t.Errorf("allowed header %s missing", name)
		}
	}
}

// TestTranscodeOutboundAcceptNormalization proves the outbound Accept always
// matches the negotiated stream mode after the allowlist applies (review-08
// blockers 1 and 10).
func TestTranscodeOutboundAcceptNormalization(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantAccept string
	}{
		{
			name:       "accept says sse but body stream false",
			body:       `{"model":"m","input":"x","stream":false}`,
			wantAccept: "application/json",
		},
		{
			name:       "body stream true overrides json accept",
			body:       `{"model":"m","input":"x","stream":true}`,
			wantAccept: "text/event-stream",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAccept string
			mapping := responsesMapping(t)
			handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
				gotAccept = req.Header.Get("Accept")
				contentType := "application/json"
				body := testcorpus.ChatCompletionsResponseJSON()
				if tt.wantAccept == "text/event-stream" {
					contentType = "text/event-stream"
					body = testcorpus.ChatCompletionsStreamSSE()
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{contentType}},
					Body:       io.NopCloser(bytes.NewReader(body)),
				}, nil
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tt.body))
			req.Header.Set("Accept", "text/event-stream;q=0.5, application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if gotAccept != tt.wantAccept {
				t.Fatalf("outbound Accept = %q, want %q", gotAccept, tt.wantAccept)
			}
		})
	}
}
