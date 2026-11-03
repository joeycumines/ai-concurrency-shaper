package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

const geminiStreamChatFirstChunk = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gemini-x","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}]}` + "\n\n"

const geminiStreamChatLastChunks = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gemini-x","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gemini-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// geminiStreamUpstream is a chat upstream that records what it received and
// whether its request context was cancelled by the proxy.
type geminiStreamUpstream struct {
	mu        sync.Mutex
	seen      []string
	bodies    []string
	cancelled chan struct{}
	// hold, when set, keeps the stream open after its first chunk until the
	// request context is cancelled or the hold timer fires.
	hold bool
}

func newGeminiStreamUpstream(hold bool) *geminiStreamUpstream {
	return &geminiStreamUpstream{cancelled: make(chan struct{}), hold: hold}
}

func (u *geminiStreamUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.seen = append(u.seen, r.Method+" "+r.URL.Path)
	u.bodies = append(u.bodies, string(body))
	// Only the first exchange holds, so later exchanges complete normally.
	hold := u.hold && len(u.seen) == 1
	u.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, geminiStreamChatFirstChunk)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if hold {
		select {
		case <-r.Context().Done():
			close(u.cancelled)
		case <-time.After(10 * time.Second):
		}
		return
	}
	_, _ = io.WriteString(w, geminiStreamChatLastChunks)
}

func (u *geminiStreamUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seen...)
}

// newGeminiStreamProxy serves a streamGenerateContent route through a real
// HTTP server, so client disconnects reach the proxy as request cancellation.
func newGeminiStreamProxy(t *testing.T, upstream *geminiStreamUpstream, concurrency int, extra ...Option) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	opts := append([]Option{
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(route.DefaultPatterns())),
		WithLimiter(queue.NewLimiterWithCooldown(concurrency, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(TranscodeMapping{
			ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1beta/models/{model}:generateContent"},
			ClientProtocol:   transcode.ClientGemini,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
			LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
				transcode.FeatureUsageUnknown:           {},
				transcode.FeatureUsageCacheReadUnknown:  {},
				transcode.FeatureUsageCacheWriteUnknown: {},
				transcode.FeatureUsageReasoningUnknown:  {},
			}}}),
	}, extra...)
	p, err := New(opts...)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return front
}

const geminiStreamRequestBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`

func TestGeminiStreamOverHTTPCarriesSSEToClient(t *testing.T) {
	upstream := newGeminiStreamUpstream(false)
	front := newGeminiStreamProxy(t, upstream, 2)

	resp, err := http.Post(front.URL+"/v1beta/models/gemini-x:streamGenerateContent?alt=sse",
		"application/json", strings.NewReader(geminiStreamRequestBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type %q, want text/event-stream", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, `"text":"hel"`) || !strings.Contains(text, `"text":"lo"`) {
		t.Fatalf("streamed text missing from %s", text)
	}
	if !strings.Contains(text, `"finishReason":"STOP"`) {
		t.Fatalf("no success terminal in %s", text)
	}
	if strings.Contains(text, "event:") || strings.Contains(text, "\n:") || strings.HasPrefix(text, ":") {
		t.Fatalf("stream carries an event or comment line: %s", text)
	}
	if got := upstream.requests(); len(got) != 1 || got[0] != "POST /v1/chat/completions" {
		t.Fatalf("upstream saw %v, want one chat completions request", got)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if !strings.Contains(upstream.bodies[0], `"stream":true`) {
		t.Fatalf("upstream request is not a streaming request: %s", upstream.bodies[0])
	}
}

func TestGeminiStreamWithoutAltNeverReachesUpstream(t *testing.T) {
	upstream := newGeminiStreamUpstream(false)
	front := newGeminiStreamProxy(t, upstream, 2)

	resp, err := http.Post(front.URL+"/v1beta/models/gemini-x:streamGenerateContent",
		"application/json", strings.NewReader(geminiStreamRequestBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if got := upstream.requests(); len(got) != 0 {
		t.Fatalf("refused stream reached the upstream: %v", got)
	}
}

// TestGeminiStreamClientCancelReleasesUpstreamAndSlot proves that a client who
// leaves mid-stream cancels the upstream exchange and frees the only limiter
// slot, so the next request is admitted.
func TestGeminiStreamClientCancelReleasesUpstreamAndSlot(t *testing.T) {
	const cancelCooldown = 300 * time.Millisecond
	upstream := newGeminiStreamUpstream(true)
	front := newGeminiStreamProxy(t, upstream, 1, WithCancelCooldown(cancelCooldown))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		front.URL+"/v1beta/models/gemini-x:streamGenerateContent?alt=sse",
		strings.NewReader(geminiStreamRequestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading first frame: %v", err)
	}
	if !strings.Contains(first, `"text":"hel"`) {
		t.Fatalf("first frame %q", first)
	}
	cancel()
	_ = resp.Body.Close()

	select {
	case <-upstream.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request context was not cancelled after the client left")
	}
	cancelledAt := time.Now()

	// The next request is admitted only after the abandoned exchange released
	// its slot, which happens no earlier than the cancel cooldown after the
	// upstream observed the cancellation. The limiter has a single slot.
	next, err := http.Post(front.URL+"/v1beta/models/gemini-x:streamGenerateContent?alt=sse",
		"application/json", strings.NewReader(geminiStreamRequestBody))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Body.Close()
	body, err := io.ReadAll(next.Body)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(cancelledAt); elapsed < cancelCooldown {
		t.Fatalf("slot was reused %v after the cancel, before the %v cancel cooldown", elapsed, cancelCooldown)
	}
	if !strings.Contains(string(body), `"finishReason":"STOP"`) {
		t.Fatalf("follow-up request did not complete: %s", body)
	}
}
