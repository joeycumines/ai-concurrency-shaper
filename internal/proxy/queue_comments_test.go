package proxy

// Opt-in SSE queue comments. When enabled and the request's
// Accept header selects text/event-stream, the proxy commits 200 + SSE
// headers BEFORE admission and writes ": queue-wait elapsed=N" comment
// lines while the request waits for a slot — keeping the client's
// connection visibly alive instead of silently blocking. The exchange is
// locked to the streaming representation: later failures surface as stream
// failures, never as HTTP error statuses.

import (
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
	// Deterministic readiness: the first request must demonstrably hold the
	// slot before the streaming request fires, so the streaming request is
	// the one that queues. A fixed sleep lets it win the slot under load,
	// which would leave it with no queue comments and false-fail.
	waitSnapshot(t, f.met, func(s metrics.Snapshot) bool { return s.Active == 1 })

	// Second request: queues with comments (streaming Accept).
	secondRec := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	secondReq.Header.Set("Accept", "text/event-stream")
	secondDone := make(chan int, 1)
	go func() {
		f.p.ServeHTTP(secondRec, secondReq)
		secondDone <- secondRec.Code
	}()
	// Deterministic readiness: the streaming request must demonstrably have
	// been queued for at least two 20ms comment ticker intervals before the
	// gate closes, so the body carries the admission comment plus at least
	// one ticker comment. A fixed sleep lets the request queue late under
	// load and be released before the first tick.
	waitSnapshot(t, f.met, func(s metrics.Snapshot) bool {
		return s.Queued == 1 && s.OldestQueuedAge >= 40*time.Millisecond
	})

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
	// Deterministic readiness: the first request must demonstrably hold the
	// slot before the streaming request is served, so the streaming request
	// queues and times out with comments. A fixed sleep lets it take the
	// slot under load, where it blocks upstream (the gate never opens) and
	// times out with no comments, false-failing the comment assertions.
	waitSnapshot(t, f.met, func(s metrics.Snapshot) bool { return s.Active == 1 })

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

// queueCommentsEchoUpstream starts an upstream that echoes the request body
// back as "echo:<body>", recording the received body for assertions.
func queueCommentsEchoUpstream(t *testing.T) (*httptest.Server, *string, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	got := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream body read: %v", err)
			return
		}
		mu.Lock()
		got = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("echo:" + string(b)))
	}))
	t.Cleanup(upstream.Close)
	return upstream, &got, &mu
}

// TestProxy_QueueCommentsPreserveRequestBody pins the wire-smoke regression
// (2026-09-08): committing the streaming representation BEFORE admission
// writes response bytes while the request body is still unread, and the
// real HTTP server then closes that body — the post-admission upstream send
// failed with "http: invalid Read on closed Body". The proxy buffers the
// body before the early commit; this test drives the proxy through a REAL
// http server (httptest.NewServer) with a real request body and asserts the
// body reaches the upstream intact.
func TestProxy_QueueCommentsPreserveRequestBody(t *testing.T) {
	upstream, got, mu := queueCommentsEchoUpstream(t)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	pat, _ := route.Parse("POST /messages:1")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueComments(20*time.Millisecond),
		WithQueueTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	const reqBody = `{"model":"m","max_tokens":10}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %q)", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), ": queue-wait elapsed=0") {
		t.Fatalf("early-commit comment marker missing: %q", respBody)
	}
	if !strings.Contains(string(respBody), "echo:"+reqBody) {
		t.Fatalf("request body did not survive the early commit (no upstream echo): %q", respBody)
	}
	mu.Lock()
	defer mu.Unlock()
	if *got != reqBody {
		t.Fatalf("upstream received body %q, want %q", *got, reqBody)
	}
}

// nonDuplexResponseWriter hides Unwrap/EnableFullDuplex behind an embedded
// recorder, simulating a downstream writer that cannot support full-duplex
// streaming.
type nonDuplexResponseWriter struct{ *httptest.ResponseRecorder }

// TestProxy_QueueCommentsBodyWithoutFullDuplexSilent pins the fallback: a
// body-carrying request whose downstream writer cannot enable full duplex
// must NOT lock the exchange to the streaming representation — comments are
// skipped and the exchange completes through the normal silent path with
// the body intact.
func TestProxy_QueueCommentsBodyWithoutFullDuplexSilent(t *testing.T) {
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

	const reqBody = `{"model":"m","max_tokens":10}`
	secondRec := nonDuplexResponseWriter{httptest.NewRecorder()}
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	secondReq.Header.Set("Accept", "text/event-stream")
	secondDone := make(chan int, 1)
	go func() {
		f.p.ServeHTTP(secondRec, secondReq)
		secondDone <- secondRec.Code
	}()
	time.Sleep(250 * time.Millisecond)

	f.gateC.Do(func() { close(f.gate) })
	if code := <-firstDone; code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	if code := <-secondDone; code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", code)
	}
	if body := secondRec.Body.String(); strings.Contains(body, ": queue-wait") {
		t.Fatalf("body-carrying request without full duplex must fall back to silent queuing: %q", body)
	}
	if body := secondRec.Body.String(); !strings.Contains(body, "ok") {
		t.Fatalf("queued request must complete normally after admission: %q", body)
	}
}

// TestProxy_QueueCommentsTransportFailureFailedClose pins the post-commit
// transport-failure contract: when the upstream send fails after the queue
// comments committed the streaming representation, the client receives an
// explicit failed-close comment (never silent EOF, never a non-SSE error
// body), and the exchange counts as aborted.
func TestProxy_QueueCommentsTransportFailureFailedClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	upstream.Close()

	pat, _ := route.Parse("POST /messages:1")
	met := metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithQueueComments(20*time.Millisecond),
		WithQueueTimeout(10*time.Second),
		WithMaxRetries(0),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (committed by the early SSE commit)", resp.StatusCode)
	}
	if !strings.Contains(string(respBody), ": queue-wait elapsed=0") {
		t.Fatalf("early-commit comment marker missing: %q", respBody)
	}
	if !strings.Contains(string(respBody), ": queue-wait failed: upstream") {
		t.Fatalf("transport failure must emit an explicit failed-close comment: %q", respBody)
	}
	snap := met.Snapshot()
	if snap.TotalAborted != 1 {
		t.Fatalf("TotalAborted = %d, want 1 (the committed stream failed)", snap.TotalAborted)
	}
	if snap.TotalProxied != 0 {
		t.Fatalf("TotalProxied = %d, want 0 (no clean completion)", snap.TotalProxied)
	}
}
