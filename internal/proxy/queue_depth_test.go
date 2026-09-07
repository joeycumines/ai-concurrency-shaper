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
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
			codes <- rec.Code
		}()
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
