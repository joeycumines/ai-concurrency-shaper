package proxy

// QUEUE-1-B: opt-in SSE queue comments. When enabled and the request's
// Accept header selects text/event-stream, the proxy commits 200 + SSE
// headers BEFORE admission and writes ": queue-wait elapsed=N" comment
// lines while the request waits for a slot — keeping the client's
// connection visibly alive instead of silently blocking. The exchange is
// locked to the streaming representation: later failures surface as stream
// failures, never as HTTP error statuses.

import (
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
)

// queueCommentsFixture: capacity-1 limited route; the upstream handler
// blocks the FIRST request on the gate (holding the slot) and serves
// subsequent requests immediately, so a second request queues.
type queueCommentsFixture struct {
	p     *Proxy
	met   *metrics.Collector
	gate  chan struct{}
	gateC sync.Once
}

func newQueueCommentsFixture(t *testing.T, comments time.Duration, timeout time.Duration) *queueCommentsFixture {
	t.Helper()
	f := &queueCommentsFixture{gate: make(chan struct{})}
	var mu sync.Mutex
	first := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()
		if isFirst {
			<-f.gate
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("held"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.gateC.Do(func() { close(f.gate) })
		upstream.Close()
	})
	pat, _ := route.Parse("POST /messages:1")
	f.met = metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(f.met),
		WithQueueComments(comments),
		WithQueueTimeout(timeout),
	)
	if err != nil {
		t.Fatal(err)
	}
	f.p = p
	return f
}

func TestProxy_QueueCommentsEmittedWhileQueued(t *testing.T) {
	f := newQueueCommentsFixture(t, 20*time.Millisecond, 10*time.Second)

	// First request: acquires the only slot and blocks upstream.
	firstDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("Accept", "application/json")
		f.p.ServeHTTP(rec, req)
		firstDone <- rec.Code
	}()
	time.Sleep(100 * time.Millisecond)

	// Second request: queues with comments (streaming Accept).
	secondRec := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	secondReq.Header.Set("Accept", "text/event-stream")
	secondDone := make(chan int, 1)
	go func() {
		f.p.ServeHTTP(secondRec, secondReq)
		secondDone <- secondRec.Code
	}()
	time.Sleep(250 * time.Millisecond)

	// Release the gate: the first request completes, the slot frees, the
	// queued request is admitted and its real response flows.
	f.gateC.Do(func() { close(f.gate) })
	if code := <-firstDone; code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	if code := <-secondDone; code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", code)
	}

	body := secondRec.Body.String()
	if ct := secondRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream (committed pre-admission)", ct)
	}
	if got := strings.Count(body, ": queue-wait elapsed="); got < 2 {
		t.Fatalf("queue-wait comments = %d, want >= 2 (initial + ticks) in: %q", got, body)
	}
	// The real response flows AFTER the comments (the ticker stops before
	// the downstream writes).
	commentEnd := strings.LastIndex(body, ": queue-wait elapsed=")
	okIdx := strings.Index(body, "ok")
	if okIdx < 0 || okIdx < commentEnd {
		t.Fatalf("upstream body must follow the comments: %q", body)
	}
	// No comments after the real response body.
	if strings.Contains(body[okIdx:], ": queue-wait") {
		t.Fatalf("comments leaked after the real response: %q", body)
	}
}

func TestProxy_QueueCommentsDisabledByDefault(t *testing.T) {
	f := newQueueCommentsFixture(t, 0, 10*time.Second)

	firstDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("Accept", "text/event-stream")
		f.p.ServeHTTP(rec, req)
		firstDone <- rec.Code
	}()
	time.Sleep(100 * time.Millisecond)

	secondRec := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	secondReq.Header.Set("Accept", "text/event-stream")
	secondDone := make(chan int, 1)
	go func() {
		f.p.ServeHTTP(secondRec, secondReq)
		secondDone <- secondRec.Code
	}()
	time.Sleep(250 * time.Millisecond)

	f.gateC.Do(func() { close(f.gate) })
	<-firstDone
	if code := <-secondDone; code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", code)
	}
	if body := secondRec.Body.String(); strings.Contains(body, ": queue-wait") {
		t.Fatalf("comments must be disabled by default: %q", body)
	}
}

func TestProxy_QueueCommentsNonStreamAcceptSkipped(t *testing.T) {
	f := newQueueCommentsFixture(t, 20*time.Millisecond, 10*time.Second)

	firstDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("Accept", "application/json")
		f.p.ServeHTTP(rec, req)
		firstDone <- rec.Code
	}()
	time.Sleep(100 * time.Millisecond)

	// Non-streaming Accept: queues SILENTLY (no early commit — a JSON
	// response cannot be preceded by SSE comments).
	secondRec := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	secondReq.Header.Set("Accept", "application/json")
	secondDone := make(chan int, 1)
	go func() {
		f.p.ServeHTTP(secondRec, secondReq)
		secondDone <- secondRec.Code
	}()
	time.Sleep(250 * time.Millisecond)

	f.gateC.Do(func() { close(f.gate) })
	<-firstDone
	if code := <-secondDone; code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", code)
	}
	if body := secondRec.Body.String(); strings.Contains(body, ": queue-wait") {
		t.Fatalf("non-streaming Accept must not receive comments: %q", body)
	}
	if ct := secondRec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("non-streaming Accept must not get SSE headers: %q", ct)
	}
}

func TestProxy_QueueCommentsTimeoutAborts(t *testing.T) {
	// The gate never opens during the test: the queued request times out
	// with comments flowing.
	f := newQueueCommentsFixture(t, 20*time.Millisecond, 150*time.Millisecond)

	firstDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("Accept", "application/json")
		f.p.ServeHTTP(rec, req)
		firstDone <- rec.Code
	}()
	time.Sleep(50 * time.Millisecond)

	secondRec := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	secondReq.Header.Set("Accept", "text/event-stream")
	f.p.ServeHTTP(secondRec, secondReq)

	body := secondRec.Body.String()
	if !strings.Contains(body, ": queue-wait failed: timeout") {
		t.Fatalf("timed-out queued request must end with the failure comment: %q", body)
	}
	if got := strings.Count(body, ": queue-wait elapsed="); got < 1 {
		t.Fatalf("comments must flow before the timeout: %q", body)
	}
	snap := f.met.Snapshot()
	if snap.TotalAborted != 1 {
		t.Fatalf("TotalAborted = %d, want 1 (the committed stream failed)", snap.TotalAborted)
	}
	if snap.TotalProxied != 0 {
		t.Fatalf("TotalProxied = %d, want 0 (no clean completion)", snap.TotalProxied)
	}
}
