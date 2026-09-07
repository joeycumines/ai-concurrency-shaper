package transcode

// Autopsy 2026-09-06 REM-L (test honesty): three regression tests that pin
// behavior whose tests could no longer fail on the fixed code.
//
// 1. GAP-018 dedup: the aggregated loss line collapses duplicate
//    feature@path entries preserving first-seen order — pinned by removing
//    the seen-map mentally: without dedup the line would list the duplicate.
// 2. GAP-008/021 decode-acceptance: the field-capture fixtures carry
//    cache_cost/completion_cost; the replay tests assert decode succeeds and
//    the fields never render — the decode-ACCEPTANCE half is pinned here by
//    asserting the modeled shadow fields are present after decode.
// 3. Messages→Chat strip pinning: the client Authorization and x-api-key
//    headers never reach the upstream request on the Messages→Chat
//    direction.

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

func TestLogConversionReportDedupesFeaturePathsInFirstSeenOrder(t *testing.T) {
	mapping := responsesMapping(t)
	handler := &TranscodeHandler{cfg: HandlerConfig{Mapping: mapping}}
	report := ConversionReport{}
	// The same feature@path twice, plus a second feature, in an order the
	// output must preserve. The policy must approve both features or Lose
	// records nothing.
	mapping.LossPolicy.Allowed = map[Feature]struct{}{}
	for _, feature := range []Feature{FeatureUsageUnknown, FeatureLogprobs} {
		mapping.LossPolicy.Allowed[feature] = struct{}{}
	}
	if err := report.Lose(mapping.LossPolicy, FeatureUsageUnknown, "usage", "first"); err != nil {
		t.Fatal(err)
	}
	if err := report.Lose(mapping.LossPolicy, FeatureLogprobs, "choices[].logprobs", "second"); err != nil {
		t.Fatal(err)
	}
	if err := report.Lose(mapping.LossPolicy, FeatureUsageUnknown, "usage", "duplicate"); err != nil {
		t.Fatal(err)
	}
	report.Note(FeatureUsageUnknown, "usage", "note duplicate")

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	handler.logConversionReport(report, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "response")
	line := buf.String()
	if strings.Count(line, "\n") > 1 {
		t.Fatalf("log lines = %d, want 1 aggregated line: %q", strings.Count(line, "\n"), line)
	}
	if got := strings.Count(line, "usage_unknown at usage"); got != 1 {
		t.Fatalf("feature@path appears %d times, want exactly 1 (deduped): %s", got, line)
	}
	usageIdx := strings.Index(line, "usage_unknown at usage")
	logprobsIdx := strings.Index(line, "logprobs at choices[].logprobs")
	if usageIdx < 0 || logprobsIdx < 0 || usageIdx > logprobsIdx {
		t.Fatalf("first-seen order not preserved: %s", line)
	}
}

func TestFieldCaptureFixturesDecodeWithModeledCostFields(t *testing.T) {
	// GAP-008/021 decode-acceptance half: the modeled CacheCost and
	// CompletionCost shadow fields must be PRESENT after decode (the
	// tolerant decode alone would also succeed with the fields deleted from
	// the shadows — this pins that they are modeled).
	body := []byte(`{
		"id":"c","object":"chat.completion","created":1,"model":"m",
		"cache_cost":{"usd":0.01},"completion_cost":{"usd":0.02},
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"cache_cost":{"usd":0.005}}
	}`)
	var shadow chatResponseShadow
	if err := wire.DecodeTolerant(body, &shadow); err != nil {
		t.Fatalf("decode with modeled cost fields: %v", err)
	}
	if shadow.CacheCost == nil {
		t.Fatal("envelope cache_cost not captured into the modeled shadow field")
	}
	if shadow.CompletionCost == nil {
		t.Fatal("envelope completion_cost not captured into the modeled shadow field")
	}
	if shadow.Usage == nil || shadow.Usage.CacheCost == nil {
		t.Fatal("usage cache_cost not captured into the modeled shadow field")
	}

	// The streaming shadow accepts the same fields on a chunk.
	chunk := []byte(`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","cache_cost":{"usd":0.01},"completion_cost":{"usd":0.02},"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"cache_cost":{"usd":0.005}}}`)
	var chunkShadow chatStreamChunkShadow
	if err := wire.DecodeTolerant(chunk, &chunkShadow); err != nil {
		t.Fatalf("chunk decode with modeled cost fields: %v", err)
	}
	if chunkShadow.CacheCost == nil || chunkShadow.CompletionCost == nil {
		t.Fatal("chunk cost fields not captured into the modeled shadow fields")
	}
	if chunkShadow.Usage == nil || chunkShadow.Usage.CacheCost == nil {
		t.Fatal("chunk usage cache_cost not captured into the modeled shadow field")
	}
}

func TestTranscodeMessagesToChatStripsClientCredentials(t *testing.T) {
	key, err := NewRouteKey(http.MethodPost, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	mapping := Mapping{
		ClientRoute:      key,
		ClientProtocol:   ClientMessages,
		UpstreamProtocol: UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		Auth:             AuthPolicy{Mode: AuthNone},
		ModelMap:         ModelMap{AllowIdentity: true},
		LossPolicy: LossPolicy{Allowed: map[Feature]struct{}{
			FeatureTopK:                   {},
			FeatureUsageUnknown:           {},
			FeatureUsageCacheReadUnknown:  {},
			FeatureUsageCacheWriteUnknown: {},
			FeatureUsageReasoningUnknown:  {},
		}},
	}
	var upstreamHeaders http.Header
	handler := NewTranscodeHandler(
		HandlerConfig{Mapping: mapping, Upstream: mustParseURL(t, "https://upstream.example")},
		func(req *http.Request) (*http.Response, error) {
			upstreamHeaders = req.Header.Clone()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
				Request:    req,
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(testcorpus.AnthropicMessagesRequestJSON()))
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("X-Api-Key", "client-anthropic-key")
	req.Header.Set("Proxy-Authorization", "Bearer proxy-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	for _, name := range []string{"Authorization", "X-Api-Key", "Proxy-Authorization", "Api-Key", "X-Goog-Api-Key"} {
		if got := upstreamHeaders.Get(name); got != "" {
			t.Fatalf("client credential header %q reached the upstream (strip-then-apply, autopsy REM-L): %q", name, got)
		}
	}
}
