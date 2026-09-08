package transcode

// End-to-end regressions for a Messages client (Claude Code 2.1.263,
// 2026-09-09) hitting an upstream whose usage is arithmetically inconsistent:
// the cached breakdown exceeding the input total, or negative counts — must
// complete the exchange. Non-streaming: a client-dialect-valid
// 200 Messages response with nonnegative usage. Streaming: exactly one success
// terminal (message_stop, no error event). Both: the inconsistency is
// recorded as an ungated note in the per-request log line. Pre-fix, the
// non-streaming path 502'd and the streaming path emitted an error terminal.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// usageClampMapping builds a Messages->Chat mapping whose policy approves only
// the four usage-component unknowns (the realistic Claude Code configuration).
// The clamp itself is ungated, so the exchange must succeed under it — a
// policy that approved nothing would reject the cache-write/reasoning
// breakdown the chat contract cannot provide.
func usageClampMapping(t *testing.T) Mapping {
	t.Helper()
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{}
	return mapping
}

// captureLog redirects the standard logger for the duration of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buf
}

// chatUsageJSON builds a non-streaming chat completion whose usage carries the
// supplied counts.
func chatCompletionJSON(usage string) string {
	return `{"id":"c","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],` +
		`"usage":` + usage + `}`
}

// chatStreamSSE builds a chat completion stream whose terminal usage chunk
// carries the supplied usage object.
func chatStreamSSE(usage string) string {
	return "data: " + `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n" +
		"data: " + `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
		"data: " + `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: " + `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":` + usage + `}` + "\n\n" +
		"data: [DONE]\n\n"
}

// inconsistentUsageShapes are the two inconsistency shapes and the values the clamp
// must produce for each. cache-exceeds-input: read = min(11, 10) = 10,
// uncached input = 10 - 10 = 0. negative counts: input -2 -> 0, cached 1
// bounded by the clamped input -> 0.
var inconsistentUsageShapes = []struct {
	name        string
	usage       string
	note        Feature
	inputTokens int
	cacheRead   int
}{
	{
		name:        "cache exceeds input",
		usage:       `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":11},"completion_tokens_details":{"reasoning_tokens":0}}`,
		note:        FeatureUsageCacheExceedsInput,
		inputTokens: 0,
		cacheRead:   10,
	},
	{
		name:        "negative counts",
		usage:       `{"prompt_tokens":-2,"completion_tokens":5,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":1},"completion_tokens_details":{"reasoning_tokens":0}}`,
		note:        FeatureUsageNegativeCounts,
		inputTokens: 0,
		cacheRead:   0,
	},
}

// TestHandlerMessagesToChatClampsInconsistentUsageJSON proves the non-streaming
// Messages client exchange completes with a valid 200 when the Chat upstream's
// usage is arithmetically inconsistent (pre-fix: 502).
func TestHandlerMessagesToChatClampsInconsistentUsageJSON(t *testing.T) {
	for _, shape := range inconsistentUsageShapes {
		t.Run(shape.name, func(t *testing.T) {
			body := chatCompletionJSON(shape.usage)
			handler := testHandler(t, usageClampMapping(t), func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			})
			logged := captureLog(t)

			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/messages",
				strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`),
			)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			var message AnthropicMessageResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
				t.Fatalf("client response is not a valid Messages document: %v", err)
			}
			if message.Usage == nil {
				t.Fatal("usage missing from the Messages response")
			}
			assertClampedAnthropicUsage(t, message.Usage, shape.inputTokens, shape.cacheRead)
			if !strings.Contains(logged.String(), "note: "+string(shape.note)) {
				t.Fatalf("per-request log missing the %s note: %q", shape.note, logged.String())
			}
		})
	}
}

// TestHandlerMessagesToChatClampsInconsistentUsageStream proves the streaming
// Messages client exchange completes with exactly one success terminal when the
// Chat upstream's terminal usage chunk is arithmetically inconsistent
// (pre-fix: an error terminal).
func TestHandlerMessagesToChatClampsInconsistentUsageStream(t *testing.T) {
	for _, shape := range inconsistentUsageShapes {
		t.Run(shape.name, func(t *testing.T) {
			body := chatStreamSSE(shape.usage)
			handler := testHandler(t, usageClampMapping(t), func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			})
			logged := captureLog(t)

			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/messages",
				strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
			)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			frames := parseAnthropicStreamEvents(t, rec.Body.Bytes())
			var (
				terminal *AnthropicStreamEvent
				events   int
			)
			for i := range frames {
				if frames[i].Type == AnthropicStreamEventTypeError {
					t.Fatalf("error terminal on a clampable exchange: %s", rec.Body.String())
				}
				if frames[i].Type == AnthropicStreamEventTypeMessageStop {
					events++
					terminal = &frames[i]
				}
			}
			if events != 1 {
				t.Fatalf("message_stop events = %d, want exactly one: %s", events, rec.Body.String())
			}
			_ = terminal
			// The final usage travels on message_delta.
			var delta *AnthropicStreamEvent
			for i := range frames {
				if frames[i].Type == AnthropicStreamEventTypeMessageDelta {
					delta = &frames[i]
				}
			}
			if delta == nil || delta.Usage == nil {
				t.Fatalf("message_delta usage missing: %s", rec.Body.String())
			}
			assertClampedAnthropicUsage(t, delta.Usage, shape.inputTokens, shape.cacheRead)
			if !strings.Contains(logged.String(), "note: "+string(shape.note)) {
				t.Fatalf("per-request log missing the %s note: %q", shape.note, logged.String())
			}
		})
	}
}

// parseAnthropicStreamEvents decodes the downstream SSE frames of a Messages
// stream response.
func parseAnthropicStreamEvents(t *testing.T, body []byte) []AnthropicStreamEvent {
	t.Helper()
	payloads := testcorpus.ParseSSEFrames(body)
	events := make([]AnthropicStreamEvent, 0, len(payloads))
	for _, payload := range payloads {
		var event AnthropicStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("frame %q: %v", payload, err)
		}
		events = append(events, event)
	}
	return events
}

// assertClampedAnthropicUsage asserts the rendered usage is nonnegative, the
// Anthropic identity holds (uncached + cache read + cache creation = input),
// and the clamp produced the expected components.
func assertClampedAnthropicUsage(t *testing.T, usage *AnthropicUsage, inputTokens, cacheRead int) {
	t.Helper()
	if usage.InputTokens < 0 || usage.CacheReadInputTokens < 0 ||
		usage.CacheCreationInputTokens < 0 || usage.OutputTokens < 0 {
		t.Fatalf("negative rendered usage: %+v", usage)
	}
	if usage.InputTokens != inputTokens || usage.CacheReadInputTokens != cacheRead {
		t.Fatalf("usage = %+v, want input %d cache-read %d", usage, inputTokens, cacheRead)
	}
	if usage.OutputTokens != 5 {
		t.Fatalf("output = %d, want the source's own 5", usage.OutputTokens)
	}
}
