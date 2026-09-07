package transcode

// Autopsy 2026-09-06 M9: a programmatic mapping with a live SecretSource
// resolved the credential per request, so caller mutation (or a mutable
// source) changed live behavior. The secret is resolved once at
// NewTranscodeHandler into a static source.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type rotatingSecret struct {
	calls atomic.Int64
}

func (s *rotatingSecret) Secret(context.Context) (string, error) {
	n := s.calls.Add(1)
	if n == 1 {
		return "first-secret", nil
	}
	return "rotated-secret", nil
}

func TestMappingSecretResolvedOnceAtConstruction(t *testing.T) {
	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = StrictLossPolicy()
	mapping.Auth = AuthPolicy{
		Mode:   AuthBearer,
		Secret: &rotatingSecret{},
	}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true}
	mapping.AllowedClientQuery = map[string]struct{}{}
	for _, feature := range []Feature{
		FeatureUsageUnknown,
		FeatureUsageCacheReadUnknown,
		FeatureUsageCacheWriteUnknown,
		FeatureUsageReasoningUnknown,
	} {
		mapping.LossPolicy.Allowed[feature] = struct{}{}
	}

	var seen atomic.Value
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
		},
		func(req *http.Request) (*http.Response, error) {
			seen.Store(req.Header.Get("Authorization"))
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"r","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
				Request:    req,
			}, nil
		},
		nil,
	)

	serve := func() string {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"x"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
		}
		v, _ := seen.Load().(string)
		return v
	}

	first := serve()
	if first != "Bearer first-secret" {
		t.Fatalf("first request credential = %q, want the construction-time resolution", first)
	}
	second := serve()
	if second != "Bearer first-secret" {
		t.Fatalf("second request credential = %q, want the SAME construction-time resolution (the source must be frozen, autopsy M9)", second)
	}
}
