package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

func TestProxy_LimitedFailureHoldAlsoHoldsGlobalLimiter(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	pat, _ := route.Parse("POST /messages:1")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithMaxRetries(0),
		WithFailureHold(200*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}

	limited := httptest.NewRecorder()
	p.ServeHTTP(limited, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if limited.Code != http.StatusInternalServerError {
		t.Fatalf("limited response status = %d, want 500", limited.Code)
	}

	start := time.Now()
	passthrough := httptest.NewRecorder()
	p.ServeHTTP(passthrough, httptest.NewRequest(http.MethodGet, "/health", nil))
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("passthrough acquired global slot after %v; want the limited failure hold", elapsed)
	}
	if passthrough.Code != http.StatusOK {
		t.Fatalf("passthrough response status = %d, want 200", passthrough.Code)
	}
}
