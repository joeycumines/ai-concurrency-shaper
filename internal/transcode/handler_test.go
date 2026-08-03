package transcode_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// newTestHandler builds a TranscodeHandler whose round trip runs through the
// given upstream server.
func newTestHandler(t *testing.T, upstream *httptest.Server, clientFormat, upstreamFormat transcode.Format) *transcode.TranscodeHandler {
	t.Helper()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	return transcode.NewTranscodeHandler(transcode.HandlerConfig{
		ClientPath:     "/v1/client",
		UpstreamPath:   "/v1/upstream",
		ClientFormat:   clientFormat,
		UpstreamFormat: upstreamFormat,
		Upstream:       upstreamURL,
	}, func(req *http.Request) (*http.Response, error) {
		return http.DefaultClient.Do(req)
	})
}

// flushRecorder records flush calls.
type flushRecorder struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	flushes int
}

func (r *flushRecorder) Flush() {
	r.mu.Lock()
	r.flushes++
	r.mu.Unlock()
}

func (r *flushRecorder) flushCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushes
}

// TestTranscodeHandlerNonStreaming verifies the non-streaming responses-to-
// chat round trip: the upstream receives a converted chat request on the
// rewritten path, and the client receives a converted responses response.
func TestTranscodeHandlerNonStreaming(t *testing.T) {
	chat := mustChatCompletions(t)
	responses := mustResponses(t)

	var sawUpstreamPath, sawUpstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sawUpstreamPath = r.URL.Path
		sawUpstreamBody = string(body)
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("upstream content-type = %q", r.Header.Get("Content-Type"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testcorpus.ChatCompletionsResponseJSON())
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if sawUpstreamPath != "/v1/upstream" {
		t.Errorf("upstream path = %q, want /v1/upstream", sawUpstreamPath)
	}
	var upstreamChat transcode.ChatRequest
	if err := json.Unmarshal([]byte(sawUpstreamBody), &upstreamChat); err != nil {
		t.Fatalf("unmarshal upstream chat request: %v", err)
	}
	if upstreamChat.Model != responses.Request.Model {
		t.Errorf("upstream chat model = %q, want %q", upstreamChat.Model, responses.Request.Model)
	}
	if len(upstreamChat.Messages) == 0 || upstreamChat.Messages[0].Role != transcode.ChatMessageRoleSystem {
		t.Errorf("upstream chat messages = %+v, want leading system message", upstreamChat.Messages)
	}

	var out transcode.ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal client responses response: %v", err)
	}
	if out.ID != chat.Response.ID || out.Model != chat.Response.Model {
		t.Errorf("responses envelope = %+v", out)
	}
	if out.Status == nil || *out.Status != "completed" {
		t.Errorf("status = %v, want completed", out.Status)
	}
	if len(out.Output) != 2 {
		t.Errorf("output items = %d, want 2", len(out.Output))
	}
	if out.Usage == nil || out.Usage.InputTokens != 42 || out.Usage.OutputTokens != 18 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

// TestTranscodeHandlerStreaming verifies the streaming responses-to-chat
// round trip: each upstream chat chunk is translated into responses SSE
// events and flushed immediately.
func TestTranscodeHandlerStreaming(t *testing.T) {
	chat := mustChatCompletions(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chat.Stream))
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("cache-control = %q, want no-cache", rec.Header().Get("Cache-Control"))
	}

	frames := testcorpus.ParseSSEFrames(rec.Body.Bytes())
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
	if len(frames) != len(wantTypes) {
		t.Fatalf("frames = %d, want %d: %q", len(frames), len(wantTypes), frames)
	}
	for i, want := range wantTypes {
		var event transcode.ResponsesStreamResponse
		if err := json.Unmarshal([]byte(frames[i]), &event); err != nil {
			t.Fatalf("unmarshal frame %d: %v", i, err)
		}
		if event.Type != want {
			t.Errorf("frame %d type = %q, want %q", i, event.Type, want)
		}
	}
	var terminal transcode.ResponsesStreamResponse
	if err := json.Unmarshal([]byte(frames[len(frames)-1]), &terminal); err != nil {
		t.Fatalf("unmarshal terminal frame: %v", err)
	}
	if terminal.Response == nil || terminal.Response.Usage == nil || terminal.Response.Usage.TotalTokens != 60 {
		t.Errorf("terminal = %+v", terminal.Response)
	}

	// Every upstream event chunk must have been flushed downstream.
	if got := rec.flushCount(); got < 5 {
		t.Errorf("flushes = %d, want at least 5 (one per upstream chunk)", got)
	}
}

// TestTranscodeHandlerStreamingMessagesChat verifies the composed messages-to-
// chat streaming conversion: chat completions chunks arrive as anthropic SSE
// events.
func TestTranscodeHandlerStreamingMessagesChat(t *testing.T) {
	chat := mustChatCompletions(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chat.Stream))
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatMessages, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.AnthropicMessagesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	frames := testcorpus.ParseSSEFrames(rec.Body.Bytes())
	if len(frames) == 0 {
		t.Fatal("no frames received")
	}
	var start transcode.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(frames[0]), &start); err != nil {
		t.Fatalf("unmarshal first frame: %v", err)
	}
	if start.Type != transcode.AnthropicStreamEventTypeMessageStart {
		t.Errorf("first event = %q, want message_start", start.Type)
	}
	var last transcode.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(frames[len(frames)-1]), &last); err != nil {
		t.Fatalf("unmarshal last frame: %v", err)
	}
	if last.Type != transcode.AnthropicStreamEventTypeMessageStop {
		t.Errorf("last event = %q, want message_stop", last.Type)
	}
	var sawTextDelta, sawThinkingDelta bool
	for _, frame := range frames {
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

// TestTranscodeHandlerMalformedSSE verifies that malformed upstream SSE lines
// are skipped without dropping the stream.
func TestTranscodeHandlerMalformedSSE(t *testing.T) {
	chat := mustChatCompletions(t)
	frames := testcorpus.ParseSSEFrames([]byte(chat.Stream))
	stream := "garbage line without colon\n" +
		"event: bogus\n" +
		"data: " + frames[0] + "\n\n" +
		"data: not-json{{{" + "\n\n" +
		"data: " + frames[len(frames)-2] + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(stream))
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	outFrames := testcorpus.ParseSSEFrames(rec.Body.Bytes())
	// The malformed data frame must be skipped; the stream must survive.
	if len(outFrames) == 0 {
		t.Fatal("no frames received; stream dropped")
	}
	if strings.Contains(rec.Body.String(), "not-json") {
		t.Error("malformed frame leaked into the client stream")
	}
	var terminal transcode.ResponsesStreamResponse
	if err := json.Unmarshal([]byte(outFrames[len(outFrames)-1]), &terminal); err != nil {
		t.Fatalf("unmarshal terminal frame: %v", err)
	}
	if terminal.Type != transcode.ResponsesStreamResponseTypeCompleted {
		t.Errorf("terminal type = %q, want response.completed", terminal.Type)
	}
}

// TestTranscodeHandlerClientCancel verifies that cancelling the client
// context mid-stream aborts the stream proxy, releases the upstream
// connection (the upstream sees its request context cancelled), and the
// handler returns promptly.
func TestTranscodeHandlerClientCancel(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Stream one chat chunk, then wait for the client to disconnect.
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		close(upstreamCancelled)
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON()))).WithContext(ctx)
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
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
		t.Error("handler did not return after cancellation")
	}
}

// TestTranscodeHandlerErrors verifies the error paths: invalid request JSON,
// invalid client payloads, and non-JSON upstream responses passing through
// unchanged.
func TestTranscodeHandlerErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>upstream error</html>"))
	}))
	t.Cleanup(upstream.Close)
	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)

	// Invalid request JSON is rejected with 400.
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON status = %d, want 400", rec.Code)
	}

	// Non-JSON upstream responses pass through unchanged.
	req = httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("passthrough status = %d, want 502", rec.Code)
	}
	if rec.Body.String() != "<html>upstream error</html>" {
		t.Errorf("passthrough body = %q", rec.Body.String())
	}
}

// TestSupportedFormatPair pins the supported client-to-upstream format pairs.
func TestSupportedFormatPair(t *testing.T) {
	tests := []struct {
		client, upstream transcode.Format
		want             bool
	}{
		{transcode.FormatResponses, transcode.FormatChatCompletions, true},
		{transcode.FormatMessages, transcode.FormatResponses, true},
		{transcode.FormatMessages, transcode.FormatChatCompletions, true},
		{transcode.FormatChatCompletions, transcode.FormatResponses, false},
		{transcode.FormatResponses, transcode.FormatMessages, false},
		{transcode.FormatChatCompletions, transcode.FormatMessages, false},
		{"bogus", transcode.FormatResponses, false},
	}
	for _, tt := range tests {
		if got := transcode.SupportedFormatPair(tt.client, tt.upstream); got != tt.want {
			t.Errorf("SupportedFormatPair(%q, %q) = %v, want %v", tt.client, tt.upstream, got, tt.want)
		}
	}
}

// TestTranscodeHandlerMessagesResponsesNonStreaming verifies the non-streaming
// messages-to-responses round trip.
func TestTranscodeHandlerMessagesResponsesNonStreaming(t *testing.T) {
	responses := mustResponses(t)
	anthropic := mustAnthropic(t)

	var sawUpstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sawUpstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testcorpus.ResponsesResponseJSON())
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatMessages, transcode.FormatResponses)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.AnthropicMessagesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var upstreamResponses transcode.ResponsesRequest
	if err := json.Unmarshal([]byte(sawUpstreamBody), &upstreamResponses); err != nil {
		t.Fatalf("unmarshal upstream responses request: %v", err)
	}
	if upstreamResponses.Model != anthropic.Request.Model {
		t.Errorf("upstream responses model = %q", upstreamResponses.Model)
	}
	var out transcode.AnthropicMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal client anthropic response: %v", err)
	}
	if out.ID != responses.Response.ID || out.Model != responses.Response.Model {
		t.Errorf("anthropic envelope = %+v", out)
	}
	if out.StopReason != transcode.AnthropicStopReasonToolUse {
		t.Errorf("stop_reason = %q, want tool_use", out.StopReason)
	}
	if len(out.Content) != 3 {
		t.Errorf("content blocks = %d, want 3", len(out.Content))
	}
}

// TestTranscodeHandlerMessagesChatNonStreaming verifies the composed
// messages-to-chat non-streaming round trip (messages -> responses -> chat).
func TestTranscodeHandlerMessagesChatNonStreaming(t *testing.T) {
	chat := mustChatCompletions(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testcorpus.ChatCompletionsResponseJSON())
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatMessages, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.AnthropicMessagesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

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
	// Cached input tokens move into the cache read field: 42 prompt tokens
	// with 5 cached become 37 input + 5 cache read.
	if out.Usage == nil || out.Usage.InputTokens != 37 || out.Usage.CacheReadInputTokens != 5 || out.Usage.OutputTokens != 18 {
		t.Errorf("usage = %+v, want input 37 cache_read 5 output 18", out.Usage)
	}
}

// TestTranscodeHandlerUpstreamErrors verifies the upstream failure paths:
// conversion failures and transport errors surface as 502 responses.
func TestTranscodeHandlerUpstreamErrors(t *testing.T) {
	// Upstream returns JSON that cannot be unmarshaled into the upstream
	// schema, failing the response conversion.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	t.Cleanup(upstream.Close)
	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("conversion failure status = %d, want 502", rec.Code)
	}

	// A failing round trip surfaces as 502.
	handler = transcode.NewTranscodeHandler(transcode.HandlerConfig{
		ClientPath:     "/v1/client",
		UpstreamPath:   "/v1/upstream",
		ClientFormat:   transcode.FormatResponses,
		UpstreamFormat: transcode.FormatChatCompletions,
		Upstream:       mustParseURL(t, "http://127.0.0.1:1"),
	}, func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	req = httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("transport failure status = %d, want 502", rec.Code)
	}
}

// TestTranscodeHandlerClientPath verifies the handler reports its route path.
func TestTranscodeHandlerClientPath(t *testing.T) {
	handler := transcode.NewTranscodeHandler(transcode.HandlerConfig{ClientPath: "/v1/x"}, nil)
	if handler.ClientPath() != "/v1/x" {
		t.Errorf("ClientPath = %q, want /v1/x", handler.ClientPath())
	}
}

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	return u
}

// failingWriter fails every write, simulating a downstream that disconnects
// mid-frame.
type failingWriter struct {
	header http.Header
	status int
}

func (w *failingWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *failingWriter) WriteHeader(status int) { w.status = status }
func (w *failingWriter) Write(p []byte) (int, error) {
	return 0, http.ErrBodyNotAllowed
}

// TestTranscodeHandlerDownstreamWriteFailure verifies that a downstream write
// failure mid-stream does not hang the handler (regression: the handler used
// to wait on a converter completion signal that only fires on the read path).
func TestTranscodeHandlerDownstreamWriteFailure(t *testing.T) {
	chat := mustChatCompletions(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for range 100 {
			_, _ = w.Write([]byte(chat.Stream))
		}
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(&failingWriter{}, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after downstream write failure")
	}
}

// TestTranscodeHandlerUpstreamErrorPassthrough verifies that non-success
// upstream responses pass through unconverted, preserving the provider error
// payload.
func TestTranscodeHandlerUpstreamErrorPassthrough(t *testing.T) {
	errorBody := `{"error":{"message":"model does not exist","type":"invalid_request_error","code":"model_not_found"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(errorBody))
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if rec.Body.String() != errorBody {
		t.Errorf("body = %s, want the raw upstream error payload", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
}

// TestTranscodeHandlerHeaderSanitization verifies that hop-by-hop and
// content-negotiation headers from the client are not forwarded upstream: a
// client Accept-Encoding would make the upstream compress the response
// (breaking JSON/SSE conversion), and connection-level headers could confuse
// the transport.
func TestTranscodeHandlerHeaderSanitization(t *testing.T) {
	var sawHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testcorpus.ChatCompletionsResponseJSON())
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	req.Header.Set("Accept-Encoding", "gzip, br")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Transfer-Encoding", "chunked")
	req.Header.Set("X-Forwarded-For", "203.0.113.1")
	req.Header.Set("X-Api-Key", "secret-value")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	for _, h := range []string{"Connection", "Transfer-Encoding", "X-Forwarded-For"} {
		if got := sawHeaders.Get(h); got != "" {
			t.Errorf("upstream saw %s = %q, want absent", h, got)
		}
	}
	// The client's Accept-Encoding must not leak: the transport adds its own
	// (and transparently decodes the response), so the client's exact value
	// must be absent.
	if got := sawHeaders.Get("Accept-Encoding"); got == "gzip, br" {
		t.Errorf("upstream saw the client accept-encoding %q", got)
	}
	// Auth and custom headers must still pass through.
	if got := sawHeaders.Get("X-Api-Key"); got != "secret-value" {
		t.Errorf("upstream x-api-key = %q, want secret-value", got)
	}
	if ct := sawHeaders.Get("Content-Type"); ct != "application/json" {
		t.Errorf("upstream content-type = %q, want application/json", ct)
	}
}

// TestTranscodeHandlerStreamPreservesHeaders verifies that upstream entity
// headers (e.g. CORS) are copied onto the streaming response alongside the
// SSE headers.
func TestTranscodeHandlerStreamPreservesHeaders(t *testing.T) {
	chat, err := testcorpus.OpenAIChatCompletionsFixtures()
	if err != nil {
		t.Fatalf("load chat fixtures: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Access-Control-Allow-Origin", "https://example.com")
		w.Header().Set("X-Trace-Id", "trace-1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chat.Stream))
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("access-control-allow-origin = %q, want https://example.com", got)
	}
	if got := rec.Header().Get("X-Trace-Id"); got != "trace-1" {
		t.Errorf("x-trace-id = %q, want trace-1", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
}

// TestTranscodeHandlerGzipUpstream verifies the Accept-Encoding sanitization
// regression: a client that asks for compression must not force a compressed
// upstream response through the converter. The handler strips the client
// header, the transport negotiates its own encoding, and transparently
// decodes the gzipped body before conversion.
func TestTranscodeHandlerGzipUpstream(t *testing.T) {
	chat, err := testcorpus.OpenAIChatCompletionsFixtures()
	if err != nil {
		t.Fatalf("load chat fixtures: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusOK)
			gz := gzip.NewWriter(w)
			_, _ = gz.Write(mustChatCompletionsJSON(t))
			_ = gz.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(mustChatCompletionsJSON(t))
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	req.Header.Set("Accept-Encoding", "gzip, br")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var out transcode.ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal client responses response: %v", err)
	}
	if out.ID != chat.Response.ID {
		t.Errorf("responses id = %q, want %q", out.ID, chat.Response.ID)
	}
}

// mustChatCompletionsJSON returns the raw chat completions response fixture.
func mustChatCompletionsJSON(t *testing.T) []byte {
	t.Helper()
	f := mustChatCompletions(t)
	b, err := json.Marshal(&f.Response)
	if err != nil {
		t.Fatalf("marshal chat response: %v", err)
	}
	return b
}

// TestTranscodeHandlerClientCancelRoundTripError verifies that a round trip
// failure with a cancelled client context aborts the exchange (panic with
// http.ErrAbortHandler) instead of writing a 502 — a 502 would be classified
// by the proxy as an upstream failure, tripping the breaker and phantom
// penalty for a failure that never happened.
func TestTranscodeHandlerClientCancelRoundTripError(t *testing.T) {
	handler := transcode.NewTranscodeHandler(transcode.HandlerConfig{
		ClientPath:     "/v1/client",
		UpstreamPath:   "/v1/upstream",
		ClientFormat:   transcode.FormatResponses,
		UpstreamFormat: transcode.FormatChatCompletions,
		Upstream:       mustParseURL(t, "http://127.0.0.1:1"),
	}, func(*http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON()))).WithContext(ctx)
	rec := httptest.NewRecorder()
	defer func() {
		if rv := recover(); rv != http.ErrAbortHandler {
			t.Errorf("panic = %v, want http.ErrAbortHandler", rv)
		}
	}()
	handler.ServeHTTP(rec, req)
	t.Error("ServeHTTP did not abort on a cancelled client context")
}

// TestTranscodeHandlerRoundTripErrorNotClientCancel verifies that a round
// trip failure WITHOUT a cancelled client context still surfaces as 502.
func TestTranscodeHandlerRoundTripErrorNotClientCancel(t *testing.T) {
	handler := transcode.NewTranscodeHandler(transcode.HandlerConfig{
		ClientPath:     "/v1/client",
		UpstreamPath:   "/v1/upstream",
		ClientFormat:   transcode.FormatResponses,
		UpstreamFormat: transcode.FormatChatCompletions,
		Upstream:       mustParseURL(t, "http://127.0.0.1:1"),
	}, func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// TestTranscodeHandlerRoundTripErrorClosesBody verifies that a RoundTripper
// returning both a response and an error does not leak the response body: the
// handler closes it before surfacing the 502.
func TestTranscodeHandlerRoundTripErrorClosesBody(t *testing.T) {
	closed := make(chan struct{})
	handler := transcode.NewTranscodeHandler(transcode.HandlerConfig{
		ClientPath:     "/v1/client",
		UpstreamPath:   "/v1/upstream",
		ClientFormat:   transcode.FormatResponses,
		UpstreamFormat: transcode.FormatChatCompletions,
		Upstream:       mustParseURL(t, "http://127.0.0.1:1"),
	}, func(*http.Request) (*http.Response, error) {
		body := io.NopCloser(strings.NewReader("ignored"))
		rc := &closeNotifier{ReadCloser: body, closed: closed}
		return &http.Response{StatusCode: http.StatusOK, Body: rc}, errors.New("boom")
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Error("response body was not closed after a round trip error")
	}
}

// closeNotifier closes a channel when Close is called.
type closeNotifier struct {
	io.ReadCloser
	closed chan struct{}
}

func (c *closeNotifier) Close() error {
	close(c.closed)
	return c.ReadCloser.Close()
}

// TestTranscodeHandlerMaxBodyBytes verifies the request body read is bounded:
// an oversized payload is rejected with 413 instead of being buffered whole.
func TestTranscodeHandlerMaxBodyBytes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testcorpus.ChatCompletionsResponseJSON())
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	handler := transcode.NewTranscodeHandler(transcode.HandlerConfig{
		ClientPath:     "/v1/client",
		UpstreamPath:   "/v1/upstream",
		ClientFormat:   transcode.FormatResponses,
		UpstreamFormat: transcode.FormatChatCompletions,
		Upstream:       upstreamURL,
		MaxBodyBytes:   64,
	}, func(req *http.Request) (*http.Response, error) {
		return http.DefaultClient.Do(req)
	})

	big := `{"model":"gpt-4.1","input":[{"role":"user","content":"` + strings.Repeat("x", 200) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(big))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

// TestTranscodeHandlerUpstreamBasePath verifies the configured upstream base
// path is joined with the mapped route path (e.g. -upstream .../api with
// route /v1/chat/completions reaches .../api/v1/chat/completions).
func TestTranscodeHandlerUpstreamBasePath(t *testing.T) {
	var sawPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testcorpus.ChatCompletionsResponseJSON())
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL + "/api")
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	handler := transcode.NewTranscodeHandler(transcode.HandlerConfig{
		ClientPath:     "/v1/client",
		UpstreamPath:   "/v1/chat/completions",
		ClientFormat:   transcode.FormatResponses,
		UpstreamFormat: transcode.FormatChatCompletions,
		Upstream:       upstreamURL,
	}, func(req *http.Request) (*http.Response, error) {
		return http.DefaultClient.Do(req)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if sawPath != "/api/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /api/v1/chat/completions", sawPath)
	}
}

// TestTranscodeHandlerEventLines verifies streamed frames carry SSE event
// lines (event: <type>) so clients that dispatch on the event name can route
// frames correctly.
func TestTranscodeHandlerEventLines(t *testing.T) {
	chat, err := testcorpus.OpenAIChatCompletionsFixtures()
	if err != nil {
		t.Fatalf("load chat fixtures: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chat.Stream))
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: response.created\n") {
		t.Errorf("missing event line for response.created:\n%s", body)
	}
	if !strings.Contains(body, "event: response.completed\n") {
		t.Errorf("missing event line for response.completed:\n%s", body)
	}
	// The data lines still carry the payload.
	if !strings.Contains(body, "data: {\"type\":\"response.created\"") {
		t.Errorf("missing data line:\n%s", body)
	}
}

// TestTranscodeHandlerEntityHeadersStripped verifies encoding and validator
// headers of the upstream entity are not copied onto the converted response.
func TestTranscodeHandlerEntityHeadersStripped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("ETag", "\"abc123\"")
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(mustChatCompletionsJSON(t))
		_ = gz.Close()
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON())))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	// The transport transparently decodes the gzip body; the validator header
	// of the upstream entity must not reach the converted response.
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("content-encoding = %q, want absent", got)
	}
	if got := rec.Header().Get("ETag"); got != "" {
		t.Errorf("etag = %q, want absent", got)
	}
}

// TestTranscodeHandlerJSONResponseClientCancel verifies that an upstream
// response read failure with a cancelled client context aborts the exchange
// (panic with http.ErrAbortHandler) instead of writing a 502 the proxy would
// classify as an upstream failure.
func TestTranscodeHandlerJSONResponseClientCancel(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Stream an incomplete JSON document until the client cancels.
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = w.Write([]byte(`{"partial":`))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandler(t, upstream, transcode.FormatResponses, transcode.FormatChatCompletions)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/client", strings.NewReader(string(testcorpus.ResponsesRequestJSON()))).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	var panicVal any
	go func() {
		defer func() {
			panicVal = recover()
			close(done)
		}()
		handler.ServeHTTP(rec, req)
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request never started")
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after cancellation")
	}
	if panicVal != http.ErrAbortHandler {
		t.Errorf("panic = %v, want http.ErrAbortHandler", panicVal)
	}
}
