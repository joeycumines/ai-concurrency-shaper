package proxy

// Autopsy E2E replay conformance suite (blueprint task 17): the three field
// failure modes from scratch/observed-issue-autopsy replayed END-TO-END
// through Proxy.ServeHTTP with transcode mappings, against httptest chat
// upstreams. Each scenario uses the exact captured client shapes; each
// asserts the field failure is dead at the proxy boundary.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// replayUpstream is a capturing chat-completions httptest upstream: every
// request body is recorded and a fixed valid completion returned. The usage
// carries the pinned detail objects so no usage-timing loss fires.
func replayUpstream(t *testing.T) (*httptest.Server, func() [][]byte) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"m","choices":[{"index":0,"logprobs":null,"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...)
	}
}

// TestReplayCodexTwoTurn proves the autopsy-01 fix end-to-end: the observed
// Codex turn-2 shape (an assistant history item carrying output_text with NO
// annotations key) proxies to a 200 with the history converted upstream —
// Codex sessions survive turn 2+ against the proxy.
func TestReplayCodexTwoTurn(t *testing.T) {
	upstream, captured := replayUpstream(t)
	p := j2LimitedProxy(t, upstream, nil)

	// Turn 1: the initial user prompt.
	req1 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","instructions":"You are helpful.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"turn 1"}]}]}`),
	)
	rec1 := httptest.NewRecorder()
	p.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("turn 1 status = %d, want 200: %s", rec1.Code, rec1.Body.String())
	}

	// Turn 2: the byte-faithful observed failing shape — assistant history
	// item with output_text and NO annotations key.
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","instructions":"You are helpful.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"turn 1"}]},{"type":"message","id":"item_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"turn 2"}]}]}`),
	)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("turn 2 status = %d, want 200: %s", rec2.Code, rec2.Body.String())
	}

	bodies := captured()
	if len(bodies) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(bodies))
	}
	var chat struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodies[1], &chat); err != nil {
		t.Fatalf("turn-2 upstream body: %v\n%s", err, bodies[1])
	}
	foundHistory := false
	for _, message := range chat.Messages {
		if message.Role == "assistant" && strings.Contains(string(message.Content), `"hi"`) {
			foundHistory = true
		}
	}
	if !foundHistory {
		t.Fatalf("converted assistant history missing from the turn-2 upstream body: %s", bodies[1])
	}
}

// TestReplayClaudeCodeMidConversationSystem proves the autopsy-02 fix
// end-to-end: a Claude Code Messages exchange with envelope.system plus an
// inline mid-conversation system turn proxies to a 200 whose single upstream
// system message sits at index 0 — no 'System message must be at the
// beginning.' rejection.
func TestReplayClaudeCodeMidConversationSystem(t *testing.T) {
	upstream, captured := replayUpstream(t)

	key, err := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	mapping := transcode.Mapping{
		ClientRoute:      key,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		// The CLI-default approvals this scenario needs: the position loss
		// is approved out of the box (Claude Code sends mid-conversation
		// system on every request), alongside the usage-timing losses.
		LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureMidConversationSystem:  {},
			transcode.FeatureUsageCacheReadUnknown:  {},
			transcode.FeatureUsageCacheWriteUnknown: {},
			transcode.FeatureUsageReasoningUnknown:  {},
			transcode.FeatureUsageUnknown:           {},
		}},
		ModelMap: transcode.ModelMap{AllowIdentity: true},
		Auth:     transcode.AuthPolicy{Mode: transcode.AuthNone},
		// The CLI default query forwarding: Claude Code gates every request
		// with ?beta=true.
		AllowedClientQuery: map[string]struct{}{"beta": {}},
	}
	u, _ := url.Parse(upstream.URL)
	pattern, err := route.Parse("POST /v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher([]route.Pattern{pattern})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(transcodeMapping(mapping)),
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages?beta=true",
		strings.NewReader(`{"model":"m","max_tokens":100,"system":[{"type":"text","text":"top"}],"messages":[{"role":"user","content":"first"},{"role":"system","content":"inline"},{"role":"user","content":"second"}]}`),
	)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	bodies := captured()
	if len(bodies) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(bodies))
	}
	var chat struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodies[0], &chat); err != nil {
		t.Fatalf("upstream body: %v\n%s", err, bodies[0])
	}
	for i, message := range chat.Messages {
		if i > 0 && message.Role == "system" {
			t.Fatalf("system message at index %d violates the Jinja property: %s", i, bodies[0])
		}
	}
	if len(chat.Messages) == 0 || chat.Messages[0].Role != "system" {
		t.Fatalf("first message = %+v, want the consolidated system message: %s", chat.Messages, bodies[0])
	}
	if !bytes.Contains(bodies[0], []byte("top")) || !bytes.Contains(bodies[0], []byte("inline")) {
		t.Fatalf("consolidated system content lost: %s", bodies[0])
	}
}

// TestReplayPoisonUsageSuccess proves the autopsy-03 fix end-to-end and
// composes task 16's retry pin: a chat upstream 200 carrying top-level
// reasoning_tokens yields a 200 to the client with the converted usage, zero
// breaker failures, and exactly one upstream hit even with retries enabled.
func TestReplayPoisonUsageSuccess(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"m","choices":[{"index":0,"logprobs":null,"finish_reason":"stop","message":{"role":"assistant","content":"done"}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"reasoning_tokens":15}}`))
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)

	// Retries and the production replay cap armed so the single-hit
	// assertion exercises the real retry decision (task 16).
	u, _ := url.Parse(upstream.URL)
	pattern, err := route.Parse("POST /v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher([]route.Pattern{pattern})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
		WithBreaker(breaker),
		WithMaxRetries(3),
		WithMaxBodyBytes(1<<20),
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"hi"}`),
	)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want exactly 1", hits)
	}
	if got := breaker.Stats().ConsecutiveFailures; got != 0 {
		t.Fatalf("breaker consecutive failures = %d, want 0", got)
	}
	// The converted usage reaches the Responses client as real usage: both
	// the reasoning breakdown (from the top-level extension) and the totals
	// must be present — asserted independently, never OR-shaped.
	if !strings.Contains(rec.Body.String(), `"reasoning_tokens":15`) {
		t.Fatalf("converted reasoning breakdown missing from the client body: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"total_tokens":30`) {
		t.Fatalf("converted usage totals missing from the client body: %s", rec.Body.String())
	}
}
