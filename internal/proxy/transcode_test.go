package proxy

import (
	"bytes"
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

// testResponsesMapping returns a Responses->Chat mapping.
func testResponsesMapping(t *testing.T) transcode.Mapping {
	t.Helper()
	key, err := transcode.NewRouteKey(http.MethodPost, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	return transcode.Mapping{
		ClientRoute:      key,
		ClientProtocol:   transcode.ClientResponses,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		// The pinned Responses usage requires the breakdown detail objects
		// the Chat source may not provide (review-k finding 6): the test
		// fixtures' usage lacks them, so the shared mapping permits the
		// usage-timing loss.
		LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureUsageCacheReadUnknown:  {},
			transcode.FeatureUsageCacheWriteUnknown: {},
			transcode.FeatureUsageReasoningUnknown:  {},
			transcode.FeatureUsageUnknown:           {}}},
		ModelMap: transcode.ModelMap{AllowIdentity: true},
		Auth:     transcode.AuthPolicy{Mode: transcode.AuthNone},
	}
}

// testMessagesResponsesMapping returns a Messages->Responses mapping.
func testMessagesResponsesMapping(t *testing.T) transcode.Mapping {
	t.Helper()
	key, err := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	mapping := transcode.Mapping{
		ClientRoute:      key,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap:         transcode.ModelMap{AllowIdentity: true},
		Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
	}
	// Messages tools carry no strictness; a messages->responses mapping
	// under the strict policy is a startup rejection (review-z commit 6).
	// These tests exercise error/stream behavior, not the strictness loss.
	mapping.LossPolicy = transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureToolSchemaStrictness: {},
	}}
	return mapping
}

func transcodeMapping(m transcode.Mapping) TranscodeMapping {
	return TranscodeMapping{Mapping: m}
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
		WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"gpt-4.1","input":"hello"}`),
	)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	path, body := gotPath, gotBody
	mu.Unlock()
	if path != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q", path)
	}
	// The upstream body is a Chat request.
	var chat struct {
		Model    string `json:"model"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		t.Fatalf("upstream body: %v\n%s", err, body)
	}
	if chat.Model != "gpt-4.1" || len(chat.Messages) != 1 || chat.Messages[0].Role != "user" {
		t.Fatalf("chat = %+v", chat)
	}
	// The client response is a Responses envelope.
	var envelope struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("client body: %v\n%s", err, rec.Body.Bytes())
	}
	if envelope.Object != "response" || envelope.Status != "completed" {
		t.Fatalf("envelope = %+v", envelope)
	}
}

// TestProxyTranscodeMethodScopedDispatch verifies that non-POST methods on a
// mapped path pass through transparently while POST is transcoded.
func TestProxyTranscodeMethodScopedDispatch(t *testing.T) {
	var (
		mu           sync.Mutex
		upstreamHits int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstreamHits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodPost {
			// The transcoded POST expects a Chat response for conversion.
			_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Non-POST methods on the mapped path are NOT intercepted: they pass
	// through transparently to the upstream (no conversion).
	for _, method := range []string{http.MethodGet, http.MethodOptions, http.MethodHead, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/responses", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", method, rec.Code, rec.Body.String())
		}
	}

	// POST is transcoded.
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d", rec.Code)
	}

	mu.Lock()
	hits := upstreamHits
	mu.Unlock()
	if hits != 5 {
		t.Fatalf("upstream hits = %d, want 5 (4 transparent + 1 transcoded)", hits)
	}
}

// TestProxyWithTranscodeMappingValidation verifies invalid mappings are
// rejected at option time.
func TestProxyWithTranscodeMappingValidation(t *testing.T) {
	tests := []struct {
		name    string
		mapping transcode.Mapping
	}{
		{"non-POST client route", func() transcode.Mapping {
			key, _ := transcode.NewRouteKey(http.MethodGet, "/v1/responses")
			return transcode.Mapping{
				ClientRoute:      key,
				ClientProtocol:   transcode.ClientResponses,
				UpstreamProtocol: transcode.UpstreamChatCompletions,
				UpstreamPath:     "/v1/chat/completions",
			}
		}()},
		{"unsupported direction", func() transcode.Mapping {
			key, _ := transcode.NewRouteKey(http.MethodPost, "/v1/responses")
			return transcode.Mapping{
				ClientRoute:      key,
				ClientProtocol:   transcode.ClientResponses,
				UpstreamProtocol: transcode.UpstreamMessages,
				UpstreamPath:     "/v1/messages",
			}
		}()},
		{"chat as client", func() transcode.Mapping {
			key, _ := transcode.NewRouteKey(http.MethodPost, "/v1/chat/completions")
			return transcode.Mapping{
				ClientRoute:      key,
				ClientProtocol:   "chat-completions",
				UpstreamProtocol: transcode.UpstreamResponses,
				UpstreamPath:     "/v1/responses",
			}
		}()},
		{"relative upstream path", func() transcode.Mapping {
			key, _ := transcode.NewRouteKey(http.MethodPost, "/v1/responses")
			return transcode.Mapping{
				ClientRoute:      key,
				ClientProtocol:   transcode.ClientResponses,
				UpstreamProtocol: transcode.UpstreamChatCompletions,
				UpstreamPath:     "chat/completions",
			}
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			upstreamURL, _ := url.Parse(upstream.URL)
			_, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher(nil)),
				WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
				WithMetrics(metrics.NewCollector()),
				WithTranscodeMapping(transcodeMapping(tt.mapping)),
			)
			if err == nil {
				t.Fatalf("expected validation error for %s", tt.name)
			}
		})
	}
}

// TestProxyWithTranscodeMappingDuplicateRoutes verifies duplicate
// {method,path} client routes are rejected.
func TestProxyWithTranscodeMappingDuplicateRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)

	duplicate := testMessagesResponsesMapping(t)
	other := testMessagesResponsesMapping(t)

	_, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(transcodeMapping(duplicate), transcodeMapping(other)),
	)
	if err == nil {
		t.Fatal("expected duplicate route rejection")
	}
	if !strings.Contains(err.Error(), "duplicate transcode client route") {
		t.Fatalf("error = %v", err)
	}
}

// TestProxyTranscodeStringInput verifies string Responses input works through
// the proxy end to end.
func TestProxyTranscodeStringInput(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var chat struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &chat); err != nil {
			t.Fatalf("upstream body: %v\n%s", err, body)
		}
		if len(chat.Messages) != 1 || chat.Messages[0].Role != "user" {
			t.Fatalf("messages = %+v", chat.Messages)
		}
		// The content may be a plain string or a single text block.
		var contentStr string
		if err := json.Unmarshal(chat.Messages[0].Content, &contentStr); err != nil {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(chat.Messages[0].Content, &blocks); err != nil {
				t.Fatalf("content: %v\n%s", err, chat.Messages[0].Content)
			}
			if len(blocks) != 1 || blocks[0].Text != "Hello" {
				t.Fatalf("blocks = %+v", blocks)
			}
		} else if contentStr != "Hello" {
			t.Fatalf("content string = %q", contentStr)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	p := newTranscodeProxyUpstream(t, upstream, transcodeMapping(testResponsesMapping(t)))
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"Hello"}`),
	)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestValidQueryName covers the allowed-client-query name rule (review-08
// additional 6): query syntax characters ('=', '&', '#', '?'), control
// characters, and empty names are rejected; pchar characters that field-name
// syntax would reject (e.g. '@') are accepted.
func TestValidQueryName(t *testing.T) {
	for _, name := range []string{"model", "model_name", "a@b", "a/b", "x.y", "a-b"} {
		if !validQueryName(name) {
			t.Errorf("validQueryName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "a=b", "a&b", "#x", "?x", "a%b", "a b", "a\tb", "a\nb", string(rune(0x7f))} {
		if validQueryName(name) {
			t.Errorf("validQueryName(%q) = true, want false", name)
		}
	}
}

// TestProxyTranscodeStreaming verifies a streaming transcode through the
// proxy: the upstream SSE stream is translated and the terminal is reached.
func TestProxyTranscodeStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testcorpus.ChatCompletionsStreamSSE())
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	p := newTranscodeProxyUpstream(t, upstream, transcodeMapping(testResponsesMapping(t)))
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("missing terminal: %q", body)
	}
}

// TestProxyTranscodeMessagesToResponsesStreaming verifies the Messages client
// against a Responses SSE upstream through the proxy.
func TestProxyTranscodeMessagesToResponsesStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testcorpus.ResponsesStreamSSE())
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	// The fixture carries a reasoning item and no early usage; the
	// response-side losses are approved for this conversion.
	mapping := testMessagesResponsesMapping(t)
	mapping.LossPolicy = transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureToolSchemaStrictness:   {},
		transcode.FeatureReasoningSummary:       {},
		transcode.FeatureUsageUnknown:           {},
		transcode.FeatureUsageCacheReadUnknown:  {},
		transcode.FeatureUsageCacheWriteUnknown: {},
		transcode.FeatureUsageReasoningUnknown:  {}}}
	p := newTranscodeProxyUpstream(t, upstream, transcodeMapping(mapping))
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "message_start") {
		t.Fatalf("missing message_start: %q", body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("missing message_stop: %q", body)
	}
}

// TestProxyTranscodeLimitedRoute verifies that transcoded requests acquire
// the limiter slot and release it when the stream terminates. The test
// serves two concurrent requests under a limit of one; the second must wait
// for the first to finish. A request ordinal guards the first-started signal
// so the server never double-closes the channel (review finding 20a).
func TestProxyTranscodeLimitedRoute(t *testing.T) {
	var (
		mu             sync.Mutex
		maxActive      atomic.Int32
		currentActive  atomic.Int32
		firstStarted   = make(chan struct{})
		releaseFirst   = make(chan struct{})
		startedOnce    sync.Once
		firstStartedOK = false
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active := currentActive.Add(1)
		defer currentActive.Add(-1)
		for {
			prev := maxActive.Load()
			if active <= prev || maxActive.CompareAndSwap(prev, active) {
				break
			}
		}

		mu.Lock()
		if !firstStartedOK {
			firstStartedOK = true
			mu.Unlock()
			startedOnce.Do(func() { close(firstStarted) })
			<-releaseFirst
		} else {
			mu.Unlock()
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	limiter := queue.NewLimiterWithCooldown(1, 0)
	p, err := newProxyWithLimiter(t, upstream.URL, limiter, testResponsesMapping(t))
	if err != nil {
		t.Fatal(err)
	}

	rec1 := httptest.NewRecorder()
	rec2 := httptest.NewRecorder()
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		req1 := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(`{"model":"m","input":"a"}`),
		).WithContext(context.Background())
		p.ServeHTTP(rec1, req1)
	}()

	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not start")
	}

	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		req2 := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(`{"model":"m","input":"b"}`),
		).WithContext(context.Background())
		p.ServeHTTP(rec2, req2)
	}()

	// The second request must not complete while the first holds the slot.
	select {
	case <-done2:
		t.Fatal("second request completed while the first held the single slot")
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseFirst)
	<-done1
	<-done2

	if rec1.Code != http.StatusOK {
		t.Fatalf("rec1 code = %d: %s", rec1.Code, rec1.Body.String())
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("rec2 code = %d: %s", rec2.Code, rec2.Body.String())
	}
	var resp1, resp2 struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("rec1 body: %v\n%s", err, rec1.Body.Bytes())
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("rec2 body: %v\n%s", err, rec2.Body.Bytes())
	}
	if resp1.Status != "completed" || resp2.Status != "completed" {
		t.Fatalf("statuses = %q, %q", resp1.Status, resp2.Status)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("maxActive = %d, want 1", got)
	}
}

// newProxyWithLimiter builds a proxy with the given global limiter so
// transcoded passthrough requests are bounded by it.
func newProxyWithLimiter(t *testing.T, upstreamURL string, limiter *queue.Limiter, mapping transcode.Mapping) (*Proxy, error) {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	return New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithGlobalLimiter(limiter),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(transcodeMapping(mapping)),
	)
}

// TestProxyTranscodeBreakerClientCancelNoPhantomSuccess verifies that a
// client that cancels a 2xx transcode stream mid-body cannot reset a
// pre-seeded failure streak or bump TotalSuccesses (review findings 14/20).
func TestProxyTranscodeBreakerClientCancelNoPhantomSuccess(t *testing.T) {
	upstreamStarted := make(chan struct{})
	terminalWritten := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Write only the first frame, then hold the connection open. The
		// client cancels mid-stream, before any terminal frame arrives.
		_, _ = io.WriteString(
			w,
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
		)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Hold the connection open until the client (or proxy) cancels.
		<-r.Context().Done()
		close(terminalWritten)
	}))
	t.Cleanup(upstream.Close)

	breaker, err := circuitbreaker.New(circuitbreaker.WithFailureThreshold(100))
	if err != nil {
		t.Fatal(err)
	}
	breaker.RecordFailure(500, 0, time.Time{}, 0)
	before := breaker.Stats()
	if before.ConsecutiveFailures == 0 {
		t.Fatal("pre-seeded failure streak missing")
	}

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(breaker),
		WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	).WithContext(ctx)

	writer := newReturnGuardWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(writer, req)
		writer.returned.Store(true)
	}()

	// Wait for the first downstream flush, then cancel mid-stream.
	deadline := time.Now().Add(5 * time.Second)
	for writer.flushCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if writer.flushCount() == 0 {
		t.Fatal("no downstream flush observed")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not return after cancellation")
	}

	// Allow any copy worker attempting a late operation to be observed.
	time.Sleep(20 * time.Millisecond)
	if got := writer.lateOps(); got != 0 {
		t.Fatalf("downstream operations after ServeHTTP returned: %d", got)
	}

	after := breaker.Stats()
	if after.TotalSuccesses != before.TotalSuccesses {
		t.Fatalf(
			"client abort recorded success: before=%d after=%d",
			before.TotalSuccesses,
			after.TotalSuccesses,
		)
	}
	if after.ConsecutiveFailures != before.ConsecutiveFailures {
		t.Fatalf(
			"client abort reset failure streak: before=%d after=%d",
			before.ConsecutiveFailures,
			after.ConsecutiveFailures,
		)
	}

	// The upstream context/body must be released.
	select {
	case <-upstreamStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not start")
	}
}

// TestProxyTranscodeLocalConversion502NotFailure verifies that a local
// conversion 502 is recorded as neither success nor failure on the breaker.
// The fixture is a VALID Chat response whose finish_reason is outside the
// supported subset — a known-but-unsupported feature stays local (review-k
// finding 3).
func TestProxyTranscodeLocalConversion502NotFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// The upstream returned a 200 with a valid Chat response whose
		// finish_reason the transcoder does not support, so the local
		// conversion fails.
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"weird","message":{"role":"assistant","content":"x"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	breaker, err := circuitbreaker.New(circuitbreaker.WithFailureThreshold(100))
	if err != nil {
		t.Fatal(err)
	}
	before := breaker.Stats()

	u, _ := url.Parse(upstream.URL)
	p, err := New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(breaker),
		WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}

	after := breaker.Stats()
	if after.TotalFailures != before.TotalFailures {
		t.Fatalf("local conversion recorded as failure: before=%d after=%d", before.TotalFailures, after.TotalFailures)
	}
	if after.TotalSuccesses != before.TotalSuccesses {
		t.Fatalf("local conversion recorded as success: before=%d after=%d", before.TotalSuccesses, after.TotalSuccesses)
	}
}

// returnGuardWriter mirrors the fuzz harness's returnGuardWriter.
type returnGuardWriter struct {
	mu sync.Mutex

	header   http.Header
	status   int
	body     bytes.Buffer
	returned atomic.Bool
	late     atomic.Int64
	flushes  atomic.Int64
}

func newReturnGuardWriter() *returnGuardWriter {
	return &returnGuardWriter{}
}

func (w *returnGuardWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *returnGuardWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.returned.Load() {
		w.late.Add(1)
		return
	}
	if w.status == 0 {
		w.status = status
	}
}

func (w *returnGuardWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.returned.Load() {
		w.late.Add(1)
		return 0, io.ErrClosedPipe
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func (w *returnGuardWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.returned.Load() {
		w.late.Add(1)
		return
	}
	w.flushes.Add(1)
}

func (w *returnGuardWriter) flushCount() int64 {
	return w.flushes.Load()
}

func (w *returnGuardWriter) lateOps() int64 {
	return w.late.Load()
}

// TestProxyTranscodeBreakerOutcomeWithRetries verifies gate 20 under the
// default CLI-like configuration: a retry-aware transport owns breaker
// reporting (retryHandlesBreaker), yet a cancelled transcode stream must
// still be classified from the explicit transcode outcome — never recorded
// as a success, and never as a failure.
func TestProxyTranscodeBreakerOutcomeWithRetries(t *testing.T) {
	upstreamStarted := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(
			w,
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
		)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)

	breaker, err := circuitbreaker.New(circuitbreaker.WithFailureThreshold(100))
	if err != nil {
		t.Fatal(err)
	}
	breaker.RecordFailure(500, 0, time.Time{}, 0)
	before := breaker.Stats()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(breaker),
		WithMaxRetries(1),
		WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	).WithContext(ctx)

	writer := newReturnGuardWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(writer, req)
		writer.returned.Store(true)
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not start")
	}

	// Wait for the first downstream flush, then cancel mid-stream.
	deadline := time.Now().Add(5 * time.Second)
	for writer.flushes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if writer.flushes.Load() == 0 {
		t.Fatal("no downstream flush observed")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not return after cancellation")
	}
	time.Sleep(20 * time.Millisecond)

	after := breaker.Stats()
	if after.TotalSuccesses != before.TotalSuccesses {
		t.Fatalf(
			"client abort recorded success with retry-owned breaker: before=%d after=%d",
			before.TotalSuccesses,
			after.TotalSuccesses,
		)
	}
	if after.ConsecutiveFailures != before.ConsecutiveFailures {
		t.Fatalf(
			"client abort reset failure streak with retry-owned breaker: before=%d after=%d",
			before.ConsecutiveFailures,
			after.ConsecutiveFailures,
		)
	}
}

// TestProxyTranscodeBreakerFailureCountedOnce verifies that a failed
// transcoded exchange under the default retry configuration records exactly
// one breaker failure: the retry transport reports the HTTP-level failure
// and the explicit transcode outcome must not record it a second time.
func TestProxyTranscodeBreakerFailureCountedOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", "req_1")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"server_error","code":"boom"}}`))
	}))
	t.Cleanup(upstream.Close)

	breaker, err := circuitbreaker.New(circuitbreaker.WithFailureThreshold(100))
	if err != nil {
		t.Fatal(err)
	}
	before := breaker.Stats()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(breaker),
		WithMaxRetries(1),
		WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}

	after := breaker.Stats()
	if after.TotalFailures != before.TotalFailures+1 {
		t.Fatalf(
			"TotalFailures = %d, want exactly one more than before (%d)",
			after.TotalFailures,
			before.TotalFailures,
		)
	}
	if after.ConsecutiveFailures != before.ConsecutiveFailures+1 {
		t.Fatalf(
			"ConsecutiveFailures = %d, want exactly one more than before (%d)",
			after.ConsecutiveFailures,
			before.ConsecutiveFailures,
		)
	}
}

// TestProxyTranscodeRateLimitClassification verifies the response-aware
// failure classification parity with the native path (round-4 fix): a 403
// carrying x-ratelimit-* headers is an upstream failure, and Retry-After: 0
// is not.
func TestProxyTranscodeRateLimitClassification(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		headers map[string]string
		want    int64 // failures before -> after delta
	}{
		{
			name:    "429 is a failure",
			status:  http.StatusTooManyRequests,
			headers: map[string]string{"x-request-id": "req_1"},
			want:    1,
		},
		{
			name:    "403 with x-ratelimit headers is a failure",
			status:  http.StatusForbidden,
			headers: map[string]string{"x-ratelimit-remaining": "0", "x-ratelimit-reset": "60"},
			want:    1,
		},
		{
			name:    "403 with Retry-After: 0 is not a failure",
			status:  http.StatusForbidden,
			headers: map[string]string{"Retry-After": "0"},
			want:    0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"api_error","code":"boom"}}`))
			}))
			t.Cleanup(upstream.Close)

			breaker, err := circuitbreaker.New(circuitbreaker.WithFailureThreshold(100))
			if err != nil {
				t.Fatal(err)
			}
			before := breaker.Stats()

			u, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			p, err := New(
				WithUpstream(u),
				WithMatcher(route.NewMatcher(nil)),
				WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
				WithMetrics(metrics.NewCollector()),
				WithBreaker(breaker),
				// No retries: the outcome path is the only recorder.
				WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
			)
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/responses",
				strings.NewReader(`{"model":"m","input":"x"}`),
			)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			after := breaker.Stats()
			if got := after.TotalFailures - before.TotalFailures; got != tt.want {
				t.Fatalf("failure delta = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestProxyTranscodeMessagesToChatStreaming verifies the composed
// Messages->Chat direction through the proxy: the chat stream converts to an
// Anthropic stream (message_start + message_stop) with the response-side
// losses approved (the composed created envelope carries no usage).
func TestProxyTranscodeMessagesToChatStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testcorpus.ChatCompletionsStreamSSE())
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	mapping := testMessagesResponsesMapping(t)
	mapping.UpstreamProtocol = transcode.UpstreamChatCompletions
	mapping.UpstreamPath = "/v1/chat/completions"
	mapping.LossPolicy = transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureReasoningSummary:       {},
		transcode.FeatureUsageUnknown:           {},
		transcode.FeatureUsageCacheReadUnknown:  {},
		transcode.FeatureUsageCacheWriteUnknown: {},
		transcode.FeatureUsageReasoningUnknown:  {}}}
	p := newTranscodeProxyUpstream(t, upstream, transcodeMapping(mapping))
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "message_start") {
		t.Fatalf("missing message_start: %q", body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("missing message_stop: %q", body)
	}
	if strings.Contains(body, `"type":"error"`) {
		t.Fatalf("unexpected error event: %q", body)
	}
}

// noopSigner signs without doing anything.
type noopSigner struct{}

func (noopSigner) Sign(_ context.Context, _ *http.Request) error { return nil }

// TestProxyExternalSignerRequiresReplayableBodies proves the startup
// rejection for signer configurations that cannot supply replayable request
// bodies: an external signer with retries enabled but a zero retry body cap
// fails construction (review-z commit 6).
func TestProxyExternalSignerRequiresReplayableBodies(t *testing.T) {
	mapping := testResponsesMapping(t)
	mapping.Auth = transcode.AuthPolicy{Mode: transcode.AuthExternalSigner, Signer: noopSigner{}}

	upstreamURL, _ := url.Parse("https://upstream.example")
	pat, err := route.Parse("POST /v1/responses")
	if err != nil {
		t.Fatalf("route.Parse: %v", err)
	}
	_, err = New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithMaxRetries(3),
		WithTranscodeMapping(transcodeMapping(mapping)),
	)
	if err == nil {
		t.Fatal("external signer with retries and a zero body cap accepted")
	}
	if !strings.Contains(err.Error(), "cannot replay request bodies") {
		t.Fatalf("err = %v, want the replayability error", err)
	}
}
