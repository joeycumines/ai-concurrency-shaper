package proxy

// The unlimited admission class. Under -limit-all, a route
// declared ":unlimited" never acquires a slot, so cheap auxiliary calls
// (token counting) cannot queue behind long-running completions — the
// operator-reported unresponsive-agent failure mode.

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

func TestProxy_UnlimitedClassPassesUnderLimitAll(t *testing.T) {
	// Two gated completions hold the only two limited slots.
	release1 := make(chan struct{})
	release2 := make(chan struct{})
	completions := sync.WaitGroup{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages":
			// Each completion blocks until released.
			if r.Header.Get("X-Probe") == "1" {
				<-release1
			} else {
				<-release2
			}
			completions.Done()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		default:
			// count_tokens answers immediately.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"input_tokens":42}`))
		}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	limited, _ := route.Parse("POST /messages:2")
	unlimited, _ := route.Parse("POST /messages/count_tokens:unlimited")
	met := metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{unlimited, limited})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(met),
		WithLimitAll(true),
		WithQueueTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Two completions occupy both limited slots (they block server-side).
	completions.Add(2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			req.Header.Set("X-Probe", map[bool]string{true: "1", false: "0"}[i == 0])
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
		})
	}
	// Wait until both slots are held: a third limited request must time out.
	// A fixed sleep lets the third request win a slot under load and
	// complete instead of queueing, false-failing the saturation
	// precondition.
	waitSnapshot(t, met, func(s metrics.Snapshot) bool { return s.Active == 2 })

	blocked := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		blocked <- rec.Code
	}()
	select {
	case code := <-blocked:
		t.Fatalf("third limited request completed with %d while both slots were held; want it queued", code)
	case <-time.After(300 * time.Millisecond):
		// Still queued: the limited pool is saturated, as expected.
	}

	// The unlimited count_tokens request passes WITHOUT queueing, even
	// though -limit-all is on and both slots are held.
	start := time.Now()
	countRec := httptest.NewRecorder()
	countReq := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	p.ServeHTTP(countRec, countReq)
	elapsed := time.Since(start)
	if countRec.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d, want 200 through the unlimited class", countRec.Code)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("count_tokens took %v — it queued behind the saturated limited pool", elapsed)
	}

	close(release1)
	close(release2)
	wg.Wait()
	completions.Wait()
	<-blocked // the queued third completion completes after a slot frees

	snap := met.Snapshot()
	if snap.TotalPassThrough < 1 {
		t.Fatalf("TotalPassThrough = %d, want >= 1 (the unlimited request is not shaped)", snap.TotalPassThrough)
	}
}

func TestProxy_UnlimitedClassPrecedenceFirstMatchWins(t *testing.T) {
	// A limited pattern on the SAME path declared BEFORE the unlimited one
	// wins (first-MATCHING-wins): the request IS limited.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	limited, _ := route.Parse("POST /messages/count_tokens:1")
	unlimited, _ := route.Parse("POST /messages/count_tokens:unlimited")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{limited, unlimited})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithLimitAll(true),
	)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// The limited pattern won: the exchange counted as proxied, not
	// passthrough.
	if got := p.Metrics().Snapshot().TotalProxied; got != 1 {
		t.Fatalf("TotalProxied = %d, want 1 (the first-matching limited pattern governs)", got)
	}
}

func TestProxy_UnlimitedBypassesGlobalLimiter(t *testing.T) {
	// The unlimited class never acquires a slot, including the global
	// limiter: with the single global slot held by a blocking limited
	// request, an unlimited request still answers immediately instead of
	// queueing behind it.
	release := make(chan struct{})
	var closeRelease sync.Once
	safeClose := func() { closeRelease.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			<-release
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(safeClose)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	limited, _ := route.Parse("POST /messages:1")
	unlimited, _ := route.Parse("POST /messages/count_tokens:unlimited")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{unlimited, limited})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithLimitAll(true),
		WithQueueTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	holderDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		holderDone <- rec.Code
	}()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil))
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("unlimited request took %v behind the held global slot, want immediate bypass", elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unlimited status = %d, want 200", rec.Code)
	}
	safeClose()
	if code := <-holderDone; code != http.StatusOK {
		t.Fatalf("holder status = %d, want 200", code)
	}
}
