package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// newTranscodeProxy builds a Proxy with the given transcode mappings, backed
// by an upstream that reports the path and body it received as JSON.
func newTranscodeProxy(t *testing.T, mappings ...TranscodeMapping) (*Proxy, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"path":` + marshalString(r.URL.Path) + `,"body":` + string(body) + `}`))
	}))
	t.Cleanup(upstream.Close)
	return newTranscodeProxyUpstream(t, upstream, mappings...), upstream
}

// newTranscodeProxyUpstream builds a Proxy with the given transcode mappings,
// forwarding to the given upstream.
func newTranscodeProxyUpstream(t *testing.T, upstream *httptest.Server, mappings ...TranscodeMapping) *Proxy {
	t.Helper()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(mappings...),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p
}

func marshalString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestProxyTranscodeDispatch verifies that a mapped route is intercepted,
// converted to the upstream schema, forwarded on the rewritten path, and
// converted back for the client, while unmapped routes pass through
// transparently.
func TestProxyTranscodeDispatch(t *testing.T) {
	var (
		mu      sync.Mutex
		gotPath string
		gotBody []byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		gotBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(TranscodeMapping{
			ClientPath:     "/v1/responses",
			UpstreamPath:   "/v1/chat/completions",
			ClientFormat:   transcode.FormatResponses,
			UpstreamFormat: transcode.FormatChatCompletions,
		}),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1","input":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	upstreamPath, upstreamBody := gotPath, gotBody
	mu.Unlock()
	if upstreamPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", upstreamPath)
	}
	var chatReq transcode.ChatRequest
	if err := json.Unmarshal(upstreamBody, &chatReq); err != nil {
		t.Fatalf("unmarshal upstream chat request: %v", err)
	}
	if chatReq.Model != "gpt-4.1" {
		t.Errorf("upstream chat model = %q", chatReq.Model)
	}
	if len(chatReq.Messages) != 1 || chatReq.Messages[0].Role != transcode.ChatMessageRoleUser {
		t.Errorf("upstream chat messages = %+v", chatReq.Messages)
	}

	var responsesResp transcode.ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &responsesResp); err != nil {
		t.Fatalf("unmarshal client responses response: %v", err)
	}
	if responsesResp.ID != "chatcmpl-1" || responsesResp.Model != "gpt-4.1" {
		t.Errorf("responses envelope = %+v", responsesResp)
	}
	if responsesResp.Status == nil || *responsesResp.Status != "completed" {
		t.Errorf("status = %v, want completed", responsesResp.Status)
	}
	if responsesResp.Usage == nil || responsesResp.Usage.InputTokens != 5 || responsesResp.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", responsesResp.Usage)
	}

	// Unmapped routes pass through untouched.
	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("passthrough status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"message":{"role":"assistant","content":"hello"}`) {
		t.Errorf("passthrough body = %s", rec.Body.String())
	}
}

// TestWithTranscodeMappingValidation verifies that empty paths and
// unsupported format pairs are rejected at configuration time.
func TestWithTranscodeMappingValidation(t *testing.T) {
	upstreamURL, _ := url.Parse("http://example.com")
	base := []Option{
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
	}

	tests := []struct {
		name    string
		mapping TranscodeMapping
	}{
		{"empty client path", TranscodeMapping{UpstreamPath: "/v1/x", ClientFormat: transcode.FormatResponses, UpstreamFormat: transcode.FormatChatCompletions}},
		{"empty upstream path", TranscodeMapping{ClientPath: "/v1/x", ClientFormat: transcode.FormatResponses, UpstreamFormat: transcode.FormatChatCompletions}},
		{"unsupported pair", TranscodeMapping{ClientPath: "/v1/x", UpstreamPath: "/v1/y", ClientFormat: transcode.FormatChatCompletions, UpstreamFormat: transcode.FormatMessages}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := append(append([]Option{}, base...), WithTranscodeMapping(tt.mapping))
			if _, err := New(opts...); err == nil {
				t.Error("proxy.New: want error for invalid mapping")
			}
		})
	}
}

// TestProxyTranscodeDefaultOff verifies that without mappings, transcoding
// is fully inert and all routes pass through transparently.
func TestProxyTranscodeDefaultOff(t *testing.T) {
	p, _ := newTranscodeProxy(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1"}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// Without a mapping the path must not be rewritten.
	if !strings.Contains(rec.Body.String(), `"path":"/v1/responses"`) {
		t.Errorf("unmapped path was rewritten: %s", rec.Body.String())
	}
}

// TestWithTranscodeMappingDuplicatePaths verifies that duplicate client paths
// are rejected at configuration time.
func TestWithTranscodeMappingDuplicatePaths(t *testing.T) {
	upstreamURL, _ := url.Parse("http://example.com")
	opts := []Option{
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(
			TranscodeMapping{ClientPath: "/v1/messages", UpstreamPath: "/v1/chat/completions", ClientFormat: transcode.FormatMessages, UpstreamFormat: transcode.FormatChatCompletions},
			TranscodeMapping{ClientPath: "/v1/messages", UpstreamPath: "/v1/responses", ClientFormat: transcode.FormatMessages, UpstreamFormat: transcode.FormatResponses},
		),
	}
	if _, err := New(opts...); err == nil {
		t.Error("proxy.New: want error for duplicate transcode client path")
	}
}

// proxyFlushRecorder records flush calls made through the full proxy pipeline
// (the proxy's statusRecorder unwraps to this recorder's Flush).
type proxyFlushRecorder struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	flushes int
}

func (r *proxyFlushRecorder) Flush() {
	r.mu.Lock()
	r.flushes++
	r.mu.Unlock()
}

func (r *proxyFlushRecorder) flushCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushes
}

// TestProxyTranscodeStreaming verifies the full-pipeline streaming round
// trip: a mapped responses route forwards a converted chat request upstream,
// receives chat completions SSE frame by frame, and delivers converted
// responses events flushed after every chunk. The upstream first consumes the
// complete request body (the request-side EOF), then streams the response —
// proving the asymmetrical half-close leaves the response flowing.
func TestProxyTranscodeStreaming(t *testing.T) {
	chat, err := testcorpus.OpenAIChatCompletionsFixtures()
	if err != nil {
		t.Fatalf("load chat fixtures: %v", err)
	}
	frames := testcorpus.ParseSSEFrames([]byte(chat.Stream))

	var (
		mu              sync.Mutex
		upstreamPath    string
		upstreamBodyLen int
		upstreamJSON    bool
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consume the request body to EOF before streaming: the converted
		// request must arrive complete, and the request half-close must not
		// interrupt the response.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream read request body: %v", err)
		}
		mu.Lock()
		upstreamPath = r.URL.Path
		upstreamBodyLen = len(body)
		upstreamJSON = r.Header.Get("Content-Type") == "application/json"
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			_, _ = w.Write([]byte("data: " + frame + "\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(upstream.Close)

	p := newTranscodeProxyUpstream(t, upstream, TranscodeMapping{
		ClientPath:     "/v1/responses",
		UpstreamPath:   "/v1/chat/completions",
		ClientFormat:   transcode.FormatResponses,
		UpstreamFormat: transcode.FormatChatCompletions,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec := &proxyFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	mu.Lock()
	gotPath, gotBodyLen, gotJSON := upstreamPath, upstreamBodyLen, upstreamJSON
	mu.Unlock()
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
	if gotBodyLen == 0 {
		t.Error("upstream received an empty request body")
	}
	if !gotJSON {
		t.Error("upstream content-type = not application/json")
	}

	outFrames := testcorpus.ParseSSEFrames(rec.Body.Bytes())
	wantTypes := []transcode.ResponsesStreamResponseType{
		transcode.ResponsesStreamResponseTypeCreated,
		transcode.ResponsesStreamResponseTypeInProgress,
		transcode.ResponsesStreamResponseTypeOutputItemAdded,
		transcode.ResponsesStreamResponseTypeReasoningSummaryPartAdded,
		transcode.ResponsesStreamResponseTypeReasoningSummaryTextDelta,
		transcode.ResponsesStreamResponseTypeReasoningSummaryTextDone,
		transcode.ResponsesStreamResponseTypeReasoningSummaryPartDone,
		transcode.ResponsesStreamResponseTypeOutputItemDone,
		transcode.ResponsesStreamResponseTypeOutputItemAdded,
		transcode.ResponsesStreamResponseTypeContentPartAdded,
		transcode.ResponsesStreamResponseTypeOutputTextDelta,
		transcode.ResponsesStreamResponseTypeOutputTextDelta,
		transcode.ResponsesStreamResponseTypeOutputTextDone,
		transcode.ResponsesStreamResponseTypeContentPartDone,
		transcode.ResponsesStreamResponseTypeOutputItemDone,
		transcode.ResponsesStreamResponseTypeCompleted,
	}
	if len(outFrames) != len(wantTypes) {
		t.Fatalf("frames = %d, want %d: %q", len(outFrames), len(wantTypes), outFrames)
	}
	for i, want := range wantTypes {
		var event transcode.ResponsesStreamResponse
		if err := json.Unmarshal([]byte(outFrames[i]), &event); err != nil {
			t.Fatalf("unmarshal frame %d: %v", i, err)
		}
		if event.Type != want {
			t.Errorf("frame %d type = %q, want %q", i, event.Type, want)
		}
	}
	// Incremental deltas must be preserved across the pipeline.
	if !strings.Contains(rec.Body.String(), "The weather in Tokyo is ") {
		t.Errorf("streamed text delta missing from client stream")
	}
	var terminal transcode.ResponsesStreamResponse
	if err := json.Unmarshal([]byte(outFrames[len(outFrames)-1]), &terminal); err != nil {
		t.Fatalf("unmarshal terminal frame: %v", err)
	}
	if terminal.Response == nil || terminal.Response.Usage == nil || terminal.Response.Usage.TotalTokens != 60 {
		t.Errorf("terminal = %+v", terminal.Response)
	}
	// Every upstream event chunk must be flushed downstream. The trailing
	// usage chunk produces no client writes, the [DONE] frame releases the
	// held terminal event, and the stream ends, so the write count is one per
	// content frame plus one terminal flush.
	if wantFlushes := len(frames) - 1; rec.flushCount() < wantFlushes {
		t.Errorf("flushes = %d, want at least %d", rec.flushCount(), wantFlushes)
	}
}

// TestProxyTranscodeStreamingMessagesChat verifies the composed messages-to-
// chat streaming round trip through the full pipeline: chat completions
// chunks arrive as anthropic SSE events with message_start ... message_stop.
func TestProxyTranscodeStreamingMessagesChat(t *testing.T) {
	chat, err := testcorpus.OpenAIChatCompletionsFixtures()
	if err != nil {
		t.Fatalf("load chat fixtures: %v", err)
	}
	frames := testcorpus.ParseSSEFrames([]byte(chat.Stream))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			_, _ = w.Write([]byte("data: " + frame + "\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(upstream.Close)

	p := newTranscodeProxyUpstream(t, upstream, TranscodeMapping{
		ClientPath:     "/v1/messages",
		UpstreamPath:   "/v1/chat/completions",
		ClientFormat:   transcode.FormatMessages,
		UpstreamFormat: transcode.FormatChatCompletions,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(testcorpus.AnthropicMessagesRequestJSON())))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	outFrames := testcorpus.ParseSSEFrames(rec.Body.Bytes())
	if len(outFrames) == 0 {
		t.Fatal("no frames received")
	}
	var start transcode.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(outFrames[0]), &start); err != nil {
		t.Fatalf("unmarshal first frame: %v", err)
	}
	if start.Type != transcode.AnthropicStreamEventTypeMessageStart {
		t.Errorf("first event = %q, want message_start", start.Type)
	}
	var last transcode.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(outFrames[len(outFrames)-1]), &last); err != nil {
		t.Fatalf("unmarshal last frame: %v", err)
	}
	if last.Type != transcode.AnthropicStreamEventTypeMessageStop {
		t.Errorf("last event = %q, want message_stop", last.Type)
	}
	var sawTextDelta, sawThinkingDelta bool
	for _, frame := range outFrames {
		var event transcode.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(frame), &event); err != nil {
			continue
		}
		if event.Delta != nil {
			if event.Delta.Type == transcode.AnthropicStreamDeltaTypeTextDelta {
				sawTextDelta = true
			}
			if event.Delta.Type == transcode.AnthropicStreamDeltaTypeThinkingDelta {
				sawThinkingDelta = true
			}
		}
	}
	if !sawTextDelta || !sawThinkingDelta {
		t.Errorf("delta coverage: text=%v thinking=%v", sawTextDelta, sawThinkingDelta)
	}
}

// TestProxyTranscodeMessagesChatNonStreaming verifies the composed messages-
// to-chat non-streaming round trip through the full pipeline.
func TestProxyTranscodeMessagesChatNonStreaming(t *testing.T) {
	chat, err := testcorpus.OpenAIChatCompletionsFixtures()
	if err != nil {
		t.Fatalf("load chat fixtures: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testcorpus.ChatCompletionsResponseJSON())
	}))
	t.Cleanup(upstream.Close)

	p := newTranscodeProxyUpstream(t, upstream, TranscodeMapping{
		ClientPath:     "/v1/messages",
		UpstreamPath:   "/v1/chat/completions",
		ClientFormat:   transcode.FormatMessages,
		UpstreamFormat: transcode.FormatChatCompletions,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(testcorpus.AnthropicMessagesRequestJSON())))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var out transcode.AnthropicMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal client anthropic response: %v", err)
	}
	if out.ID != chat.Response.ID || out.Model != chat.Response.Model {
		t.Errorf("anthropic envelope = %+v", out)
	}
	if len(out.Content) != 2 || out.Content[0].Type != transcode.AnthropicContentBlockTypeThinking ||
		out.Content[1].Type != transcode.AnthropicContentBlockTypeText {
		t.Errorf("content = %+v, want thinking then text", out.Content)
	}
	if out.Usage == nil || out.Usage.InputTokens != 37 || out.Usage.CacheReadInputTokens != 5 || out.Usage.OutputTokens != 18 {
		t.Errorf("usage = %+v, want input 37 cache_read 5 output 18", out.Usage)
	}
}

// TestProxyTranscodeClientDisconnect verifies that cancelling the client
// context mid-stream through the full pipeline aborts the stream proxy,
// releases the upstream connection (the upstream sees its request context
// cancelled), and the proxy request returns promptly without tripping
// the circuit breaker or recording phantom failures.
func TestProxyTranscodeClientDisconnect(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		close(upstreamCancelled)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	breaker, err := circuitbreaker.New()
	if err != nil {
		t.Fatalf("breaker.New: %v", err)
	}
	met := metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(met),
		WithBreaker(breaker),
		WithTranscodeMapping(TranscodeMapping{
			ClientPath:     "/v1/responses",
			UpstreamPath:   "/v1/chat/completions",
			ClientFormat:   transcode.FormatResponses,
			UpstreamFormat: transcode.FormatChatCompletions,
		}),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(testcorpus.ResponsesRequestJSON()))).WithContext(ctx)
	rec := &proxyFlushRecorder{ResponseRecorder: httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request never started")
	}
	// Wait for the first converted frame to reach the client before cancelling.
	deadline := time.Now().Add(5 * time.Second)
	for rec.flushCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-upstreamCancelled:
	case <-time.After(5 * time.Second):
		t.Error("upstream request context was not cancelled after client disconnect")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("proxy request did not return after cancellation")
	}
	// The circuit breaker must remain clean: no phantom trip, no recorded
	// failure/penalty from the client cancel (which is a suppressible abort,
	// not an upstream failure).
	stats := breaker.Stats()
	if stats.Failures != 0 {
		t.Errorf("breaker failures after client disconnect = %d, want 0", stats.Failures)
	}
	if stats.ConsecutiveFailures != 0 {
		t.Errorf("breaker consecutive failures after client disconnect = %d, want 0", stats.ConsecutiveFailures)
	}
	if stats.CurrentPenalty > 0 {
		t.Errorf("breaker penalty after client disconnect = %v, want 0", stats.CurrentPenalty)
	}
}

// TestProxyTranscodeLimitedRoute verifies that a transcoded route is bounded
// by the concurrency limiter exactly like an ordinary limited route: with a
// limit of one, a second concurrent transcoded request must queue until the
// first completes, rather than bypassing the limiter.
func TestProxyTranscodeLimitedRoute(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := active.Add(1)
		defer active.Add(-1)
		for {
			max := maxActive.Load()
			if cur <= max || maxActive.CompareAndSwap(max, cur) {
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testcorpus.ChatCompletionsResponseJSON())
		if maxActive.Load() == 1 {
			close(firstStarted)
			<-releaseFirst
		}
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	// WithLimitAll routes every request through the default limiter, so the
	// transcoded route must acquire a slot like any limited route.
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithLimitAll(true),
		WithTranscodeMapping(TranscodeMapping{
			ClientPath:     "/v1/responses",
			UpstreamPath:   "/v1/chat/completions",
			ClientFormat:   transcode.FormatResponses,
			UpstreamFormat: transcode.FormatChatCompletions,
		}),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	rec1 := httptest.NewRecorder()
	done1 := make(chan struct{})
	go func() {
		p.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(testcorpus.ResponsesRequestJSON()))))
		close(done1)
	}()
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first transcoded request never reached the upstream")
	}

	// The second request must queue behind the limiter: it must not reach
	// the upstream while the first holds the single slot.
	rec2 := httptest.NewRecorder()
	done2 := make(chan struct{})
	go func() {
		p.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(testcorpus.ResponsesRequestJSON()))))
		close(done2)
	}()
	select {
	case <-done2:
		t.Fatal("second transcoded request completed while the limiter slot was held")
	case <-time.After(200 * time.Millisecond):
		// Expected: the second request is still queued behind the limiter.
	}
	if got := maxActive.Load(); got != 1 {
		t.Errorf("max concurrent upstream requests = %d, want 1", got)
	}

	close(releaseFirst)
	select {
	case <-done1:
	case <-time.After(5 * time.Second):
		t.Fatal("first transcoded request did not complete after release")
	}
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("second transcoded request did not complete after the slot freed")
	}
}

// TestProxyTranscodeStreamingMessagesResponses verifies the messages-to-
// responses streaming round trip through the full pipeline: responses SSE
// events arrive as anthropic SSE events with message_start ... message_stop.
func TestProxyTranscodeStreamingMessagesResponses(t *testing.T) {
	responses, err := testcorpus.OpenAIResponsesFixtures()
	if err != nil {
		t.Fatalf("load responses fixtures: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(responses.Stream))
	}))
	t.Cleanup(upstream.Close)

	p := newTranscodeProxyUpstream(t, upstream, TranscodeMapping{
		ClientPath:     "/v1/messages",
		UpstreamPath:   "/v1/responses",
		ClientFormat:   transcode.FormatMessages,
		UpstreamFormat: transcode.FormatResponses,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(testcorpus.AnthropicMessagesRequestJSON())))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	outFrames := testcorpus.ParseSSEFrames(rec.Body.Bytes())
	if len(outFrames) == 0 {
		t.Fatal("no frames received")
	}
	var start transcode.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(outFrames[0]), &start); err != nil {
		t.Fatalf("unmarshal first frame: %v", err)
	}
	if start.Type != transcode.AnthropicStreamEventTypeMessageStart {
		t.Errorf("first event = %q, want message_start", start.Type)
	}
	var last transcode.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(outFrames[len(outFrames)-1]), &last); err != nil {
		t.Fatalf("unmarshal last frame: %v", err)
	}
	if last.Type != transcode.AnthropicStreamEventTypeMessageStop {
		t.Errorf("last event = %q, want message_stop", last.Type)
	}
	var sawTextDelta, sawThinkingDelta bool
	for _, frame := range outFrames {
		var event transcode.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(frame), &event); err != nil {
			continue
		}
		if event.Delta != nil {
			if event.Delta.Type == transcode.AnthropicStreamDeltaTypeTextDelta {
				sawTextDelta = true
			}
			if event.Delta.Type == transcode.AnthropicStreamDeltaTypeThinkingDelta {
				sawThinkingDelta = true
			}
		}
	}
	if !sawTextDelta || !sawThinkingDelta {
		t.Errorf("delta coverage: text=%v thinking=%v", sawTextDelta, sawThinkingDelta)
	}
}
