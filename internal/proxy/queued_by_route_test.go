package proxy

// UNRESP-3: per-route queue observability. The aggregate queued count hides
// WHICH route is starved; the snapshot now breaks the queue down per route
// (key "METHOD /path", matching RouteStats) with the oldest queued age per
// route, so the operator can see "which route is waiting" without packet
// capture.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

func TestProxy_QueuedByRoutePerRouteCounts(t *testing.T) {
	// Two independent route limiters (capacity 1 each); the upstream blocks
	// until the test releases it, so each route's first request holds its
	// slot and its second request queues.
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

	patA, _ := route.Parse("POST /messages:1")
	patB, _ := route.Parse("POST /chat/completions:1")
	met := metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{patA, patB})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithRouteLimiters(map[string]*queue.Limiter{
			patA.Raw: queue.NewLimiterWithCooldown(1, 0),
			patB.Raw: queue.NewLimiterWithCooldown(1, 0),
		}),
		WithMetrics(met),
		WithQueueTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	send := func(path string) {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
	}

	// First request per route: acquires each route's only slot (blocked
	// upstream).
	go send("/v1/messages")
	go send("/v1/chat/completions")
	time.Sleep(150 * time.Millisecond)

	// Second request per route: queues on its OWN route limiter.
	var wg sync.WaitGroup
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			send(path)
		}(path)
	}
	time.Sleep(150 * time.Millisecond)

	snap := met.Snapshot()
	if got := snap.QueuedByRoute["POST /v1/messages"]; got != 1 {
		t.Fatalf("QueuedByRoute[POST /v1/messages] = %d, want 1 (snapshot: %v)", got, snap.QueuedByRoute)
	}
	if got := snap.QueuedByRoute["POST /v1/chat/completions"]; got != 1 {
		t.Fatalf("QueuedByRoute[POST /v1/chat/completions] = %d, want 1 (snapshot: %v)", got, snap.QueuedByRoute)
	}
	for route, count := range snap.QueuedByRoute {
		if count > 0 {
			if age, ok := snap.OldestQueuedAgeByRoute[route]; !ok || age <= 0 {
				t.Fatalf("route %q has %d waiters but no oldest-queued age: %v", route, count, snap.OldestQueuedAgeByRoute)
			}
		}
	}
	if total := int64(0); false {
		_ = total
	}
	sum := int64(0)
	for _, count := range snap.QueuedByRoute {
		sum += count
	}
	if sum != snap.Queued {
		t.Fatalf("per-route sum %d != aggregate queued %d", sum, snap.Queued)
	}

	gateClose.Do(func() { close(gate) })
	wg.Wait()
}

func TestProxy_QueuedByRouteEmptyWhenIdle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	snap := met.Snapshot()
	if len(snap.QueuedByRoute) != 0 {
		t.Fatalf("QueuedByRoute = %v, want empty when nothing is queued", snap.QueuedByRoute)
	}
	if len(snap.OldestQueuedAgeByRoute) != 0 {
		t.Fatalf("OldestQueuedAgeByRoute = %v, want empty", snap.OldestQueuedAgeByRoute)
	}
}

func TestProxy_QueuedGaugesAgreeDuringGlobalWait(t *testing.T) {
	// A limited request waiting on the global limiter (route slot free,
	// global slot held) must appear in both the aggregate queued gauge
	// and the per-route view: a snapshot reporting shaper_queued=0 with a
	// per-route waiter present breaks the TUI headline invariant.
	gate := make(chan struct{})
	var gateClose sync.Once
	safeClose := func() { gateClose.Do(func() { close(gate) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(safeClose)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	pat, _ := route.Parse("POST /messages:4")
	met := metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithQueueTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	holderDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		holderDone <- rec.Code
	}()
	time.Sleep(100 * time.Millisecond)

	waiterDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		waiterDone <- rec.Code
	}()
	time.Sleep(200 * time.Millisecond)

	snap := met.Snapshot()
	if snap.Queued < 1 {
		t.Fatalf("aggregate queued = %d, want >= 1 while a limited request waits on the global limiter", snap.Queued)
	}
	if got := snap.QueuedByRoute["POST /v1/messages"]; got != 1 {
		t.Fatalf("QueuedByRoute[POST /v1/messages] = %d, want 1 during the global wait", got)
	}
	if age := snap.OldestQueuedAgeByRoute["POST /v1/messages"]; age <= 0 {
		t.Fatalf("oldest queued age = %v, want > 0 during the global wait", age)
	}
	safeClose()
	if code := <-holderDone; code != http.StatusOK {
		t.Fatalf("holder status = %d, want 200", code)
	}
	if code := <-waiterDone; code != http.StatusOK {
		t.Fatalf("waiter status = %d, want 200", code)
	}
}
