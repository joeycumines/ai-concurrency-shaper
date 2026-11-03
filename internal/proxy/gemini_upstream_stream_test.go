package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// geminiSSEUpstreamStub serves scripted SSE bodies for Gemini
// streamGenerateContent requests. The script is a list of frames; each frame
// is written as one data: line followed by a blank line, and the handler then
// flushes. A frame equal to "HANG" makes the handler block until the request
// context is cancelled, so a test can observe client-cancellation semantics
// without waiting on an open body.
type geminiSSEUpstreamStub struct {
	server *httptest.Server
	mu     sync.Mutex
	paths  []string
	bodies []string
	script []string
	// hangAt, when >= 0, is the request index whose handler blocks until the
	// client context ends.
	hangAt int
	// released records that a hung handler observed its context end.
	released chan struct{}
}

func newGeminiSSEUpstreamStub(t *testing.T, script []string, hangAt int) *geminiSSEUpstreamStub {
	t.Helper()
	stub := &geminiSSEUpstreamStub{script: script, hangAt: hangAt, released: make(chan struct{}, 1)}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		stub.mu.Lock()
		n := len(stub.paths)
		stub.paths = append(stub.paths, r.URL.Path+"?"+r.URL.RawQuery)
		stub.bodies = append(stub.bodies, string(body))
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		for _, frame := range stub.script {
			fmt.Fprintf(w, "data: %s\n\n", frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if n == stub.hangAt {
			// Block until the upstream request context ends (client cancel
			// propagates through the proxy), then record the release.
			<-r.Context().Done()
			select {
			case stub.released <- struct{}{}:
			default:
			}
			return
		}
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *geminiSSEUpstreamStub) url(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(s.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (s *geminiSSEUpstreamStub) last() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.paths) - 1
	return s.paths[n], s.bodies[n]
}

// readSSE reads the client SSE stream to EOF and returns the frames.
func readSSE(t *testing.T, resp *http.Response) []string {
	t.Helper()
	defer resp.Body.Close()
	var frames []string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			frames = append(frames, strings.TrimPrefix(line, "data: "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read SSE: %v", err)
	}
	return frames
}

func countFrame(frames []string, eventType string) int {
	n := 0
	for _, f := range frames {
		if strings.Contains(f, `"type":"`+eventType+`"`) {
			n++
		}
	}
	return n
}

// TestGeminiUpstreamStreamResponsesSuccess drives an R2G streaming exchange:
// text chunks, a finish chunk, then a trailing usage-only chunk, then EOF.
// The client must see exactly one success terminal carrying the trailing
// usage, and the upstream path must be the streamGenerateContent action.
func TestGeminiUpstreamStreamResponsesSuccess(t *testing.T) {
	stub := newGeminiSSEUpstreamStub(t, []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"index":0}]}`,
		`{"candidates":[{"content":{"parts":[{"text":", world"}]},"index":0}]}`,
		`{"candidates":[{"finishReason":"STOP","index":0}]}`,
		`{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}`,
	}, -1)
	p := newGeminiUpstreamProxy(t, stub.url(t), responsesGeminiMapping())
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses",
		strings.NewReader(`{"model":"gemini-2.5-flash","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want SSE", ct)
	}
	frames := readSSE(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, frames = %v", resp.StatusCode, frames)
	}
	if n := countFrame(frames, "response.completed"); n != 1 {
		t.Fatalf("response.completed count = %d, want 1; frames = %v", n, frames)
	}
	if n := countFrame(frames, "response.output_text.delta"); n != 2 {
		t.Fatalf("delta count = %d, want 2; frames = %v", n, frames)
	}
	var terminal string
	for _, f := range frames {
		if strings.Contains(f, `"type":"response.completed"`) {
			terminal = f
		}
	}
	if !strings.Contains(terminal, `"total_tokens":7`) {
		t.Fatalf("terminal lacks trailing usage: %s", terminal)
	}
	path, _ := stub.last()
	if !strings.Contains(path, ":streamGenerateContent") || !strings.Contains(path, "alt=sse") {
		t.Fatalf("upstream path = %q, want streamGenerateContent?alt=sse", path)
	}
}

// TestGeminiUpstreamStreamResponsesTruncated proves an upstream EOF without a
// finish chunk produces exactly one error terminal, never a success.
func TestGeminiUpstreamStreamResponsesTruncated(t *testing.T) {
	stub := newGeminiSSEUpstreamStub(t, []string{
		`{"candidates":[{"content":{"parts":[{"text":"partial"}]},"index":0}]}`,
	}, -1)
	p := newGeminiUpstreamProxy(t, stub.url(t), responsesGeminiMapping())
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/responses",
		strings.NewReader(`{"model":"gemini-2.5-flash","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	frames := readSSE(t, resp)
	if n := countFrame(frames, "response.completed"); n != 0 {
		t.Fatalf("success terminals = %d, want 0; frames = %v", n, frames)
	}
	if n := countFrame(frames, "error"); n != 1 {
		t.Fatalf("error terminals = %d, want 1; frames = %v", n, frames)
	}
}

// TestGeminiUpstreamStreamMessagesCancel proves client cancellation mid-stream:
// the client sees no terminal, the upstream request context ends, and the
// exchange completes without a success terminal.
func TestGeminiUpstreamStreamMessagesCancel(t *testing.T) {
	// The script's frames are written, then the handler hangs until the
	// request context ends, so the client receives one frame and can cancel.
	stub := newGeminiSSEUpstreamStub(t, []string{
		`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"index":0}]}`,
	}, 0)
	p := newGeminiUpstreamProxy(t, stub.url(t), messagesGeminiMapping())
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/messages?beta=true",
		strings.NewReader(`{"model":"gemini-2.5-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "client-key")
	done := make(chan []string, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- nil
			return
		}
		defer resp.Body.Close()
		var frames []string
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				frames = append(frames, strings.TrimPrefix(line, "data: "))
			}
			// Cancel after the first data frame arrives.
			if len(frames) == 1 {
				cancel()
			}
		}
		done <- frames
	}()

	select {
	case <-stub.released:
	case <-time.After(10 * time.Second):
		t.Fatal("upstream handler never observed the cancelled context")
	}
	select {
	case frames := <-done:
		if frames == nil {
			t.Fatal("client request errored")
		}
		for _, f := range frames {
			if strings.Contains(f, `"type":"message_stop"`) || strings.Contains(f, `"type":"message_delta"`) {
				t.Fatalf("client saw a terminal after cancel: %v", frames)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client response body never closed after cancel")
	}
}

// TestGeminiUpstreamStreamMessagesSuccess drives an M2G streaming exchange and
// checks the Anthropic SSE event sequence.
func TestGeminiUpstreamStreamMessagesSuccess(t *testing.T) {
	stub := newGeminiSSEUpstreamStub(t, []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi there"}]},"index":0}]}`,
		`{"candidates":[{"finishReason":"STOP","index":0}]}`,
		`{"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`,
	}, -1)
	p := newGeminiUpstreamProxy(t, stub.url(t), messagesGeminiMapping())
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages?beta=true",
		strings.NewReader(`{"model":"gemini-2.5-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "client-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	frames := readSSE(t, resp)
	if n := countFrame(frames, "message_stop"); n != 1 {
		t.Fatalf("message_stop = %d, want 1; frames = %v", n, frames)
	}
	if n := countFrame(frames, "message_start"); n != 1 {
		t.Fatalf("message_start = %d, want 1; frames = %v", n, frames)
	}
	if !strings.Contains(strings.Join(frames, "\n"), `"text":"hi there"`) {
		t.Fatalf("text delta missing: %v", frames)
	}
	var sawUsage bool
	for _, f := range frames {
		if strings.Contains(f, `"message_delta"`) && strings.Contains(f, `"output_tokens":2`) {
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Fatalf("message_delta lacks trailing usage: %v", frames)
	}
}

// TestGeminiUpstreamStreamErrorFrame proves an in-stream Gemini error frame
// becomes exactly one client error event and no success terminal.
func TestGeminiUpstreamStreamErrorFrame(t *testing.T) {
	stub := newGeminiSSEUpstreamStub(t, []string{
		`{"candidates":[{"content":{"parts":[{"text":"partial"}]},"index":0}]}`,
		`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota exceeded"}}`,
	}, -1)
	p := newGeminiUpstreamProxy(t, stub.url(t), responsesGeminiMapping())
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/responses",
		strings.NewReader(`{"model":"gemini-2.5-flash","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	frames := readSSE(t, resp)
	if n := countFrame(frames, "response.completed"); n != 0 {
		t.Fatalf("success terminals = %d; frames = %v", n, frames)
	}
	if n := countFrame(frames, "error"); n != 1 {
		t.Fatalf("error terminals = %d, want 1; frames = %v", n, frames)
	}
	if !strings.Contains(strings.Join(frames, "\n"), "quota exceeded") {
		t.Fatalf("error frame lacks the upstream message: %v", frames)
	}
}
