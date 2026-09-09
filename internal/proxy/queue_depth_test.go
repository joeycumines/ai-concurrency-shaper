package proxy

// QUEUE-1: bounded-queue admission. With a queue-depth limit configured, a
// limited request arriving while the effective limiter already holds the
// bound in waiters fails fast with 429 Too Many Requests + Retry-After
// (the protocol signal AI clients and the official SDKs already handle)
// instead of queueing silently and amplifying into client-side retries.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

func TestProxy_QueueDepthLimitRejectsWith429(t *testing.T) {
	// depth 1: the first request waits (the upstream is gated), the second
	// must be rejected with 429 + Retry-After instead of queueing.
	gate := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		w.WriteHeader(http.StatusOK)
	}))
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(upstream.Close)

	pat, _ := route.Parse("POST /messages:1")
	met := metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithQueueDepthLimit(1),
		WithQueueTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Request 1 acquires the only slot and blocks upstream.
	firstDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		firstDone <- rec.Code
	}()
	time.Sleep(100 * time.Millisecond)

	// Request 2 queues (waiters = 1, at the bound).
	secondDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		secondDone <- rec.Code
	}()
	time.Sleep(100 * time.Millisecond)

	// Request 3 arrives while waiters == 1 >= depth 1: rejected with 429.
	thirdRec := httptest.NewRecorder()
	p.ServeHTTP(thirdRec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if thirdRec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%q, want 429 when the queue depth bound is reached", thirdRec.Code, thirdRec.Body.String())
	}
	if ra := thirdRec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("the 429 must carry Retry-After (the signal the official SDKs honor)")
	} else if _, err := strconv.Atoi(ra); err != nil {
		t.Fatalf("Retry-After = %q, want an integer seconds value", ra)
	}
	if ct := thirdRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(thirdRec.Body.String(), `"type":"rate_limit_error"`) {
		t.Fatalf("body = %q, want the rate_limit_error envelope", thirdRec.Body.String())
	}
	if got := met.Snapshot().TotalQueueRejected; got != 1 {
		t.Fatalf("TotalQueueRejected = %d, want 1", got)
	}

	// Release the gate: the queued request completes; the rejected one never
	// queued at all. (Closed exactly once, after every consumer finished.)
	close(gate)
	if code := <-firstDone; code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	if code := <-secondDone; code != http.StatusOK {
		t.Fatalf("second (queued) request status = %d, want 200", code)
	}
}

func TestProxy_QueueDepthZeroIsUnbounded(t *testing.T) {
	// The default (0) keeps the documented blocking semantics: requests
	// queue regardless of depth.
	gate := make(chan struct{})
	var gateClose sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		w.WriteHeader(http.StatusOK)
	}))
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateClose.Do(func() { close(gate) }); upstream.Close() })

	pat, _ := route.Parse("POST /messages:1")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		firstDone <- rec.Code
	}()
	time.Sleep(100 * time.Millisecond)

	// Three more requests all queue (no depth bound).
	var wg sync.WaitGroup
	codes := make(chan int, 3)
	for range 3 {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
			codes <- rec.Code
		})
	}
	time.Sleep(150 * time.Millisecond)

	gateClose.Do(func() { close(gate) })
	if code := <-firstDone; code != http.StatusOK {
		t.Fatalf("first = %d, want 200", code)
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("queued request = %d, want 200 (depth 0 is unbounded)", code)
		}
	}
}

// TestProxy_QueueDepth429ReleasesBreakerProbe pins the review finding
// (ses_f82433a3affeYcnpN3ETKBmQxz, 2026-09-08): the bounded-queue 429
// happens after breaker.Allow(), so in HALF_OPEN the request carries the
// single recovery probe — if it returns on the queue-rejection path without
// CancelProbe, the probe stays occupied until the open timeout and every
// other request gets 503 instead of a recovery probe, delaying breaker
// recovery exactly under load. After the fix, the 429'd probe-carrying
// request releases the probe: the next arrival (queue still full) receives
// a fresh 429, not a circuit-open 503, and the breaker recovers normally
// once the queue drains.
func TestProxy_QueueDepth429ReleasesBreakerProbe(t *testing.T) {
	gate := make(chan struct{})
	var closeGate sync.Once
	safeCloseGate := func() {
		closeGate.Do(func() {
			close(gate)
		})
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-gate
		w.WriteHeader(http.StatusOK)
	}))
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(upstream.Close)
	t.Cleanup(safeCloseGate)

	// 1s open timeout: short enough that the lazy HALF_OPEN transition is
	// reachable in test time, long enough to cover the setup sleeps.
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(1*time.Second),
		circuitbreaker.WithMaxOpenTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	pat, _ := route.Parse("POST /messages:1")
	met := metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithQueueDepthLimit(1),
		WithQueueTimeout(10*time.Second),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Fill the slot and the single waiter bound while the breaker is still
	// CLOSED (only CLOSED arrivals pass Allow() to become waiters).
	fillerDone := make(chan int, 2)
	for range 2 {
		go func() {
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
			fillerDone <- rec.Code
		}()
	}
	time.Sleep(100 * time.Millisecond)

	// Trip the breaker directly: a real exchange could not reach the
	// upstream while the queue is full, and the direct record keeps the
	// fillers queued with stale epochs (their results are ignored).
	b.RecordFailure(500, 0, time.Time{}, 0)
	if state := b.State(); state != circuitbreaker.Open {
		t.Fatalf("breaker state after recorded failure = %v, want Open", state)
	}

	// Age past the open timeout, then drive the lazy OPEN→HALF_OPEN
	// transition (it only fires inside Allow(); State() is a pure read).
	// The probe this poll takes is cancelled immediately so the proxy
	// requests below carry their own.
	time.Sleep(1100 * time.Millisecond)
	halfOpen := false
	for range 200 {
		epoch, err := b.Allow()
		if err == nil {
			b.CancelProbe(epoch)
			halfOpen = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !halfOpen || b.State() != circuitbreaker.HalfOpen {
		t.Fatalf("breaker did not reach HalfOpen after the open timeout (state=%v)", b.State())
	}

	// The probe-carrying request hits the full queue: 429.
	probeRec := httptest.NewRecorder()
	p.ServeHTTP(probeRec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if probeRec.Code != http.StatusTooManyRequests {
		t.Fatalf("probe-carrying request status = %d, want 429 on the full queue", probeRec.Code)
	}

	// The 429 must NOT strand the probe: the next arrival gets a fresh
	// probe (429 again, queue still full) instead of 503 circuit-open.
	postRec := httptest.NewRecorder()
	p.ServeHTTP(postRec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if postRec.Code != http.StatusTooManyRequests {
		t.Fatalf("post-probe request status = %d body=%q, want 429 (probe released, fresh probe admitted); 503 would mean the stranded probe blocked recovery", postRec.Code, postRec.Body.String())
	}

	// Drain the queue, then prove the breaker still recovers: the next
	// request is admitted, succeeds, and closes the circuit.
	safeCloseGate()
	for range 2 {
		if code := <-fillerDone; code != http.StatusOK {
			t.Fatalf("filler request status = %d, want 200", code)
		}
	}
	finalRec := httptest.NewRecorder()
	p.ServeHTTP(finalRec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if finalRec.Code != http.StatusOK {
		t.Fatalf("post-recovery request status = %d, want 200", finalRec.Code)
	}
	if state := b.State(); state != circuitbreaker.Closed {
		t.Fatalf("breaker state after a successful recovery probe = %v, want Closed", state)
	}
}

// TestProxy_QueueDepthDoesNotRejectWhenSlotsFree verifies that when concurrency
// slots are available, in-flight slot acquisitions do not falsely inflate the
// waiters count and trigger 429 rejections under a tight queue-depth limit.
func TestProxy_QueueDepthDoesNotRejectWhenSlotsFree(t *testing.T) {
	gate := make(chan struct{})
	var closeGate sync.Once
	safeCloseGate := func() {
		closeGate.Do(func() {
			close(gate)
		})
	}
	t.Cleanup(safeCloseGate)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-gate
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	const concurrencyLimit = 64
	const queueDepthLimit = 1
	const concurrentRequests = 16

	pat, _ := route.Parse("POST /messages:64")
	met := metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(concurrencyLimit, 0)),
		WithMetrics(met),
		WithQueueDepthLimit(queueDepthLimit),
		WithQueueTimeout(5*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Fire concurrent requests simultaneously while slots are completely free.
	startBarrier := make(chan struct{})
	type reqResult struct {
		code int
		body string
	}
	results := make(chan reqResult, concurrentRequests)

	for range concurrentRequests {
		go func() {
			<-startBarrier
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			p.ServeHTTP(rec, req)
			results <- reqResult{code: rec.Code, body: rec.Body.String()}
		}()
	}

	// Release all client goroutines simultaneously.
	close(startBarrier)

	// Wait briefly to ensure requests enter ServeHTTP and acquire slots.
	time.Sleep(50 * time.Millisecond)

	// Now unblock the upstream handlers so the requests can finish.
	safeCloseGate()

	for i := range concurrentRequests {
		res := <-results
		if res.code != http.StatusOK {
			t.Fatalf("request %d returned status %d (body=%q), want 200 OK — false 429 rejection while slots free", i, res.code, res.body)
		}
	}
}
