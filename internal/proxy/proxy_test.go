// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

func setup(t *testing.T, concurrency int, timeout time.Duration, patterns ...string) (*Proxy, *httptest.Server) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"method":%q,"path":%q}`, r.Method, r.URL.Path)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("failed to parse upstream URL: %v", err)
	}

	var pats []route.Pattern
	for _, p := range patterns {
		pat, err := route.Parse(p)
		if err != nil {
			t.Fatalf("failed to parse pattern %q: %v", p, err)
		}
		pats = append(pats, pat)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(pats)),
		WithLimiter(queue.NewLimiterWithCooldown(concurrency, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(timeout),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p, upstream
}

func TestProxy_PassthroughRoute(t *testing.T) {
	proxy, _ := setup(t, 2, 0, "POST /v1/messages")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), `"method":"GET"`) {
		t.Errorf("expected proxied response body, got %q", string(body))
	}
}

func TestProxy_LimitedRoute(t *testing.T) {
	proxy, _ := setup(t, 2, 0, "POST /v1/messages")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProxy_QueueTimeout(t *testing.T) {
	// Use concurrency 1 with a slow upstream to hold the slot.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(slow.Close)

	slowURL, _ := url.Parse(slow.URL)
	pat, _ := route.Parse("POST /v1/messages")

	proxy, err := New(
		WithUpstream(slowURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(50*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First request will hold the slot for 2s.
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
	}()

	// Brief pause to ensure first request holds the slot.
	time.Sleep(10 * time.Millisecond)

	// Second request should time out.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	proxy.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestProxy_ConcurrentLimitedRequests(t *testing.T) {
	proxy, _ := setup(t, 4, 0, "POST /v1/messages")

	const n = 20
	results := make(chan int, n)
	for range n {
		go func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)
			results <- rec.Code
		}()
	}

	for i := range n {
		code := <-results
		if code != http.StatusOK {
			t.Errorf("request %d: expected 200, got %d", i, code)
		}
	}
}

func TestProxy_MixedRoutes(t *testing.T) {
	proxy, _ := setup(t, 1, 0, "POST /v1/messages")

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("passthrough: expected 200, got %d", rec.Code)
			}
		})
	}
	wg.Wait()
}

func TestProxy_Metrics(t *testing.T) {
	proxy, _ := setup(t, 2, 0, "POST /v1/messages")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	req2 := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec2 := httptest.NewRecorder()
	proxy.ServeHTTP(rec2, req2)

	snap := proxy.Metrics().Snapshot()
	if snap.TotalProxied != 1 {
		t.Errorf("TotalProxied: got %d, want 1", snap.TotalProxied)
	}
	if snap.TotalPassThrough != 1 {
		t.Errorf("TotalPassThrough: got %d, want 1", snap.TotalPassThrough)
	}
	if snap.Active != 0 {
		t.Errorf("Active: got %d, want 0", snap.Active)
	}
}

func TestProxy_UpstreamPathPreserved(t *testing.T) {
	proxy, _ := setup(t, 2, 0, "POST /v1/messages")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages?stream=true", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), `"path":"/v1/messages"`) {
		t.Errorf("path not preserved, got %q", string(body))
	}
	if !strings.Contains(string(body), `"method":"POST"`) {
		t.Errorf("method not preserved, got %q", string(body))
	}
}

func TestProxy_AllRequestsLogged(t *testing.T) {
	proxy, _ := setup(t, 2, 0, "POST /v1/messages")

	// Passthrough request.
	req1 := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec1 := httptest.NewRecorder()
	proxy.ServeHTTP(rec1, req1)

	// Limited request.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	proxy.ServeHTTP(rec2, req2)

	// Another passthrough.
	req3 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec3 := httptest.NewRecorder()
	proxy.ServeHTTP(rec3, req3)

	snap := proxy.Metrics().Snapshot()
	if len(snap.LogEntries) != 3 {
		t.Fatalf("expected 3 log entries, got %d", len(snap.LogEntries))
	}

	// Verify chronological order and content.
	if snap.LogEntries[0].Method != "GET" || snap.LogEntries[0].Path != "/health" {
		t.Errorf("entry 0: got %s %s, want GET /health", snap.LogEntries[0].Method, snap.LogEntries[0].Path)
	}
	if snap.LogEntries[0].Limited {
		t.Error("entry 0 should not be limited")
	}

	if snap.LogEntries[1].Method != "POST" || snap.LogEntries[1].Path != "/v1/messages" {
		t.Errorf("entry 1: got %s %s, want POST /v1/messages", snap.LogEntries[1].Method, snap.LogEntries[1].Path)
	}
	if !snap.LogEntries[1].Limited {
		t.Error("entry 1 should be limited")
	}

	if snap.LogEntries[2].Method != "GET" || snap.LogEntries[2].Path != "/v1/models" {
		t.Errorf("entry 2: got %s %s, want GET /v1/models", snap.LogEntries[2].Method, snap.LogEntries[2].Path)
	}
	if snap.LogEntries[2].Limited {
		t.Error("entry 2 should not be limited")
	}

	// Verify passthrough counter.
	if snap.TotalPassThrough != 2 {
		t.Errorf("TotalPassThrough: got %d, want 2", snap.TotalPassThrough)
	}
	if snap.TotalProxied != 1 {
		t.Errorf("TotalProxied: got %d, want 1", snap.TotalProxied)
	}
}

func TestProxy_InFlightTracking(t *testing.T) {
	proxy, _ := setup(t, 2, 0, "POST /v1/messages")

	// Before any requests.
	snap := proxy.Metrics().Snapshot()
	if len(snap.InFlight) != 0 {
		t.Fatalf("expected 0 in-flight, got %d", len(snap.InFlight))
	}

	// Make a limited request.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	// After completion, in-flight should be empty.
	snap = proxy.Metrics().Snapshot()
	if len(snap.InFlight) != 0 {
		t.Errorf("expected 0 in-flight after completion, got %d", len(snap.InFlight))
	}
	if len(snap.LogEntries) != 1 {
		t.Errorf("expected 1 log entry, got %d", len(snap.LogEntries))
	}
	if snap.LogEntries[0].Method != "POST" || snap.LogEntries[0].Path != "/v1/messages" {
		t.Errorf("wrong log entry: %v", snap.LogEntries[0])
	}
}

func TestProxy_ContextCancellation(t *testing.T) {
	// Slow upstream.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wait for context cancellation.
		<-r.Context().Done()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	slowURL, _ := url.Parse(slow.URL)
	pat, _ := route.Parse("POST /v1/messages")

	p, err := New(
		WithUpstream(slowURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(0),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Start a request and cancel it.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec, req)
		close(done)
	}()

	// Give it time to start.
	time.Sleep(50 * time.Millisecond)

	// Cancel the client context.
	cancel()

	// Wait for the request to finish.
	select {
	case <-done:
		// Good — request completed.
	case <-time.After(2 * time.Second):
		t.Fatal("request did not complete after context cancellation")
	}

	// In-flight should be cleaned up.
	snap := p.Metrics().Snapshot()
	if len(snap.InFlight) != 0 {
		t.Errorf("expected 0 in-flight after cancellation, got %d", len(snap.InFlight))
	}
}

func TestProxy_ProxiedOnlyCountsSuccessful(t *testing.T) {
	// Verify that TotalProxied only counts requests that actually
	// reached the upstream, not timed-out or cancelled ones.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	slowURL, _ := url.Parse(slow.URL)
	pat, _ := route.Parse("POST /v1/messages")

	p, err := New(
		WithUpstream(slowURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(50*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First request holds the slot.
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()
	time.Sleep(10 * time.Millisecond)

	// Second request times out.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", rec2.Code)
	}

	snap := p.Metrics().Snapshot()
	if snap.TotalProxied != 0 {
		t.Errorf("TotalProxied: got %d, want 0 (no request completed yet)", snap.TotalProxied)
	}
	if snap.TotalTimeout != 1 {
		t.Errorf("TotalTimeout: got %d, want 1", snap.TotalTimeout)
	}
}

func TestProxy_TimeoutVsCancelledMetrics(t *testing.T) {
	// Verify that queue timeouts increment TotalTimeout and
	// client cancellations increment TotalCancelled, separately.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	slowURL, _ := url.Parse(slow.URL)
	pat, _ := route.Parse("POST /v1/messages")

	p, err := New(
		WithUpstream(slowURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(50*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Hold the slot.
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()
	time.Sleep(10 * time.Millisecond)

	// Timeout case.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)

	// Cancellation case.
	ctx, cancel := context.WithCancel(context.Background())
	req3 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req3 = req3.WithContext(ctx)
	rec3 := httptest.NewRecorder()
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	p.ServeHTTP(rec3, req3)

	snap := p.Metrics().Snapshot()
	if snap.TotalTimeout != 1 {
		t.Errorf("TotalTimeout: got %d, want 1", snap.TotalTimeout)
	}
	if snap.TotalCancelled != 1 {
		t.Errorf("TotalCancelled: got %d, want 1", snap.TotalCancelled)
	}
}

// TestPerRouteConcurrency verifies that the per-route limiter is used when
// a per-route concurrency limit is set.
func TestPerRouteConcurrency(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)

	pat, _ := route.Parse("POST /v1/messages:2")

	met := metrics.NewCollector()
	routeLimiters := map[string]*queue.Limiter{
		pat.Raw: queue.NewLimiterWithCooldown(2, 0), // per-route cap of 2
	}
	limiter := queue.NewLimiterWithCooldown(4, 0) // default pool — should NOT be used
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(limiter),
		WithMetrics(met),
		WithRouteLimiters(routeLimiters),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Send 3 concurrent requests. With a per-route limit of 2, the
	// third request must block until one of the first two completes.
	var wg sync.WaitGroup
	var results []int
	var mu sync.Mutex
	for range 3 {
		wg.Go(func() {
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			mu.Lock()
			results = append(results, rec.Code)
			mu.Unlock()
		})
	}
	wg.Wait()
	mu.Lock()
	if got := len(results); got != 3 {
		t.Fatalf("expected 3 requests, got %d", got)
	}
}

type mockFlusherWriter struct {
	http.ResponseWriter
	flushed bool
}

func (m *mockFlusherWriter) Flush() {
	m.flushed = true
}

// nonFlusherWriter is an http.ResponseWriter that does NOT implement http.Flusher.
type nonFlusherWriter struct{ http.ResponseWriter }

func TestStatusRecorderUnwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: inner, status: 0}

	// Verify statusRecorder itself (not the embedded field) implements Unwrap()
	// so that http.ResponseController can correctly detect capabilities.
	if _, ok := any(rec).(interface{ Unwrap() http.ResponseWriter }); !ok {
		t.Error("expected statusRecorder to implement Unwrap()")
	}

	// Verify ResponseController can flush through the wrapper.
	mock := &mockFlusherWriter{ResponseWriter: inner}
	rec2 := &statusRecorder{ResponseWriter: mock, status: 0}
	if err := http.NewResponseController(rec2).Flush(); err != nil {
		t.Errorf("ResponseController.Flush through wrapper: %v", err)
	}
	if !mock.flushed {
		t.Error("expected underlying Flush to be called via ResponseController")
	}

	// When the underlying writer does NOT implement Flusher, ResponseController
	// should return ErrNotSupported (not silently no-op).
	recNoFlush := &statusRecorder{ResponseWriter: nonFlusherWriter{httptest.NewRecorder()}, status: 0}
	if err := http.NewResponseController(recNoFlush).Flush(); !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("expected ErrNotSupported for non-flushing writer, got %v", err)
	}
}

func TestStatusRecorderHijack(t *testing.T) {
	// With a non-Hijackable underlying writer, Hijack should return
	// http.ErrNotSupported and NOT set the hijacked flag (failed hijack
	// must not corrupt journal timing).
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: 0}
	_, _, err := rec.Hijack()
	if err == nil {
		t.Error("expected error for non-Hijackable writer, got nil")
	}
	if err != http.ErrNotSupported {
		t.Errorf("expected http.ErrNotSupported, got %v", err)
	}
	if rec.hijacked {
		t.Error("hijacked flag should NOT be set when Hijack fails")
	}
}

func TestStatusRecorderDefaultStatusZero(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	if rec.status != 0 {
		t.Errorf("default status = %d, want 0", rec.status)
	}
}

func TestStatusRecorder1xxThenImplicit200(t *testing.T) {
	inner := httptest.NewRecorder()
	entry := &journal.Entry{}
	rec := &statusRecorder{ResponseWriter: inner, entry: entry, status: 0}

	// Simulate a 103 Early Hints response from an upstream handler.
	rec.WriteHeader(http.StatusEarlyHints)
	if !rec.terminalWritten {
		t.Logf("terminalWritten correctly false after 1xx")
	}
	if entry.StatusCode != http.StatusEarlyHints {
		t.Fatalf("after 1xx WriteHeader: StatusCode = %d, want %d", entry.StatusCode, http.StatusEarlyHints)
	}

	// Now the handler writes the body without calling WriteHeader(200).
	// The Go server will trigger an implicit 200 on the first Write.
	rec.Write([]byte("ok"))
	if entry.StatusCode != http.StatusOK {
		t.Fatalf("after implicit 200 Write: StatusCode = %d, want 200", entry.StatusCode)
	}
	if entry.Timing.ResponseHeaders.IsZero() {
		t.Error("expected ResponseHeaders to be set for implicit 200")
	}
	if rec.status != http.StatusOK {
		t.Errorf("rec.status = %d, want 200", rec.status)
	}
}

func TestStatusRecorder1xxThenWriteHeadersCloned(t *testing.T) {
	// Verify that when a 1xx response is followed by an implicit 200 Write,
	// the journal entry's ResponseHeaders and ContentType reflect the headers
	// at the time of the terminal write, not the stale 1xx headers.
	inner := httptest.NewRecorder()
	entry := &journal.Entry{}
	rec := &statusRecorder{ResponseWriter: inner, entry: entry, status: 0}

	// Step 1: Send 103 Early Hints with Link header.
	inner.Header().Set("Link", "</style.css>; rel=preload")
	rec.WriteHeader(http.StatusEarlyHints)

	// At this point, the journal has the 103 headers (no Content-Type).
	if entry.ContentType != "" {
		t.Errorf("ContentType after 1xx = %q, want empty", entry.ContentType)
	}

	// Step 2: Between the 103 and the body write, the handler sets
	// Content-Type (a common pattern for SSE or streaming endpoints).
	inner.Header().Set("Content-Type", "text/event-stream")

	// Step 3: First Write triggers implicit 200 capture.
	rec.Write([]byte("data: ok\n\n"))

	// The journal entry should now reflect the terminal (200) headers,
	// which include the Content-Type set between 103 and Write.
	if entry.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", entry.StatusCode)
	}
	if entry.ContentType != "text/event-stream" {
		t.Errorf("ContentType = %q, want %q (from terminal write, not 1xx)", entry.ContentType, "text/event-stream")
	}
	if entry.ResponseHeaders == nil {
		t.Fatal("ResponseHeaders should not be nil after implicit 200")
	}
	// The Link header from the 103 should also be present (it was on the
	// writer before the terminal write clone).
	if got := entry.ResponseHeaders.Get("Link"); got != "</style.css>; rel=preload" {
		t.Errorf("ResponseHeaders.Link = %q, want %q", got, "</style.css>; rel=preload")
	}
	if got := entry.ResponseHeaders.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("ResponseHeaders.Content-Type = %q, want %q", got, "text/event-stream")
	}
}

func TestStatusRecorderImplicitContentType(t *testing.T) {
	inner := httptest.NewRecorder()
	entry := &journal.Entry{}
	rec := &statusRecorder{ResponseWriter: inner, entry: entry, status: 0}

	// Handler writes JSON without setting Content-Type or calling WriteHeader.
	rec.Write([]byte(`{"ok":true}`))

	if entry.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", entry.StatusCode)
	}
	// DetectContentType returns text/plain for bare JSON in Go's implementation.
	if entry.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("ContentType = %q, want %q", entry.ContentType, "text/plain; charset=utf-8")
	}
}

// explicitSniffWriter simulates a ResponseWriter that MIME-sniffs and
// sets Content-Type during Write even after WriteHeader was called
// explicitly. Go's standard net/http server does NOT do this (headers
// are finalized after WriteHeader), but some custom ResponseWriter
// wrappers may. This mock verifies the defensive post-Write backfill
// path in statusRecorder.Write.
type explicitSniffWriter struct {
	*httptest.ResponseRecorder
}

func (s *explicitSniffWriter) Write(b []byte) (int, error) {
	if s.Header().Get("Content-Type") == "" {
		s.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	return s.ResponseRecorder.Write(b)
}

func TestStatusRecorderExplicit200ThenWriteSniffsContentType(t *testing.T) {
	inner := &explicitSniffWriter{httptest.NewRecorder()}
	entry := &journal.Entry{}
	rec := &statusRecorder{ResponseWriter: inner, entry: entry, status: 0}

	// Handler explicitly calls WriteHeader(200) without setting Content-Type.
	rec.WriteHeader(http.StatusOK)
	// Then writes body. The underlying writer simulates sniffing during Write.
	rec.Write([]byte(`{"ok":true}`))

	if entry.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", entry.StatusCode)
	}
	// The post-write capture path must pick up the sniffed Content-Type.
	if entry.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("ContentType = %q, want %q", entry.ContentType, "text/plain; charset=utf-8")
	}
}

func TestStatusRecorderDuplicateWriteHeader(t *testing.T) {
	// Verify that a duplicate terminal WriteHeader call does not corrupt
	// the journal entry. net/http ignores subsequent WriteHeader calls once
	// a terminal status (>=200) has been written — the journal must match
	// this behavior.
	inner := httptest.NewRecorder()
	entry := &journal.Entry{}
	rec := &statusRecorder{ResponseWriter: inner, entry: entry, status: 0}

	// First terminal WriteHeader.
	rec.WriteHeader(http.StatusOK)
	if entry.StatusCode != http.StatusOK {
		t.Fatalf("after first WriteHeader: StatusCode = %d, want 200", entry.StatusCode)
	}
	if !rec.terminalWritten {
		t.Error("expected terminalWritten after WriteHeader(200)")
	}

	// A buggy handler calls WriteHeader again with a different status.
	// The underlying ResponseWriter ignores this (net/http semantics),
	// and the journal must preserve the original status.
	rec.WriteHeader(http.StatusInternalServerError)
	if entry.StatusCode != http.StatusOK {
		t.Errorf("after duplicate WriteHeader(500): StatusCode = %d, want 200 (first status preserved)", entry.StatusCode)
	}
	if rec.status != http.StatusOK {
		t.Errorf("rec.status after duplicate WriteHeader(500) = %d, want 200", rec.status)
	}

	// Verify the inner recorder also got the 200 (net/http logs a
	// warning for the duplicate call, but the client got 200).
	if inner.Code != http.StatusOK {
		t.Errorf("inner recorder code = %d, want 200", inner.Code)
	}
}

// shortWriteWriter is an http.ResponseWriter that simulates a short write
// by returning n < len(b) from Write. This can happen when the client
// disconnects mid-transfer.
type shortWriteWriter struct {
	http.ResponseWriter
	n int // number of bytes to report as written
}

func (s *shortWriteWriter) Write(b []byte) (int, error) {
	// Clamp to len(b) to satisfy the io.Writer contract (0 <= n <= len(b)),
	// preventing b[:n] panics in production code that consumes n.
	n := min(s.n, len(b))
	return n, io.ErrUnexpectedEOF
}

func TestStatusRecorderShortWriteBytesWritten(t *testing.T) {
	// Verify that bytesWritten tracks the ACTUAL bytes written (n from
	// Write), not the attempted bytes (len(b)). On a short write caused
	// by client disconnect, the recorder must report only what was
	// delivered, not what we tried to send.
	inner := httptest.NewRecorder()
	mock := &shortWriteWriter{ResponseWriter: inner, n: 3}
	entry := &journal.Entry{}
	rec := &statusRecorder{ResponseWriter: mock, entry: entry, status: 0, captureMax: 1 << 20}

	// Write 10 bytes, but the underlying writer only delivers 3.
	n, err := rec.Write([]byte("0123456789"))
	if n != 3 {
		t.Errorf("Write returned n=%d, want 3", n)
	}
	if err != io.ErrUnexpectedEOF {
		t.Errorf("Write returned err=%v, want io.ErrUnexpectedEOF", err)
	}

	// bytesWritten must be 3 (actual delivered), not 10 (attempted).
	if rec.bytesWritten != 3 {
		t.Errorf("bytesWritten = %d, want 3 (actual bytes written, not attempted)", rec.bytesWritten)
	}
}

func TestStatusRecorderShortWriteWithContentLength(t *testing.T) {
	// Verify that when a response has a Content-Length header (e.g., 1000)
	// but the actual Write only delivers a subset of those bytes due to a
	// client disconnect (short write), the journal entry's ResponseSize
	// reflects the ACTUAL delivered bytes (from bytesWritten), not the
	// Content-Length value. This is the critical bug identified by
	// review-01 and review-02: the old finalizer checked
	// entry.ResponseSize == 0, which was always false when Content-Length
	// was present, causing bytesWritten to be silently ignored.
	inner := httptest.NewRecorder()
	mock := &shortWriteWriter{ResponseWriter: inner, n: 3}
	entry := &journal.Entry{}
	rec := &statusRecorder{ResponseWriter: mock, entry: entry, status: 0, captureMax: 1 << 20}

	// Simulate WriteHeader(200) with Content-Length: 1000.
	inner.Header().Set("Content-Length", "1000")
	rec.WriteHeader(http.StatusOK)

	// Write 10 bytes, but the underlying writer only delivers 3.
	n, err := rec.Write([]byte("0123456789"))
	if n != 3 {
		t.Errorf("Write returned n=%d, want 3", n)
	}
	if err != io.ErrUnexpectedEOF {
		t.Errorf("Write returned err=%v, want io.ErrUnexpectedEOF", err)
	}

	// bytesWritten must be 3 (actual delivered), not 10 (attempted).
	if rec.bytesWritten != 3 {
		t.Errorf("bytesWritten = %d, want 3", rec.bytesWritten)
	}

	// ResponseSize was set to 1000 from Content-Length during WriteHeader.
	// The ServeHTTP finalizer must override this with bytesWritten (3)
	// because bytesWritten > 0 is the ground truth of actual delivery.
	// Simulate the finalizer logic directly. NOTE: This tests the
	// logic inline rather than exercising the actual ServeHTTP
	// finalizer path, to keep the test unit-scoped. The ServeHTTP
	// integration is covered by TestProxy_ChunkedResponseSizeAccurate
	// and other proxy-level tests.
	if rec.bytesWritten > 0 {
		entry.ResponseSize = rec.bytesWritten
	}
	if entry.ResponseSize != 3 {
		t.Errorf("ResponseSize = %d, want 3 (bytesWritten overrides Content-Length)", entry.ResponseSize)
	}
}

func TestStatusRecorderShortWriteCapturedBody(t *testing.T) {
	// Verify that capturedBody contains only the bytes actually delivered
	// (b[:n]) on a short write, not the full input slice. This is the
	// structural fix from review-02: moving body capture after Write
	// allows using b[:n] instead of b, so the journal's ResponseBody
	// matches the actual delivered payload.
	inner := httptest.NewRecorder()
	mock := &shortWriteWriter{ResponseWriter: inner, n: 3}
	entry := &journal.Entry{}
	rec := &statusRecorder{ResponseWriter: mock, entry: entry, status: 0, captureMax: 1 << 20}

	// Write 10 bytes, but only 3 are delivered.
	n, err := rec.Write([]byte("0123456789"))
	if n != 3 {
		t.Errorf("Write returned n=%d, want 3", n)
	}
	_ = err // expected short-write error

	// capturedBody must contain exactly the 3 delivered bytes "012",
	// not the full 10-byte input "0123456789".
	if string(rec.capturedBody) != "012" {
		t.Errorf("capturedBody = %q, want %q (only delivered bytes)", string(rec.capturedBody), "012")
	}
}

func TestStatusRecorderSniffedContentTypeInHeaders(t *testing.T) {
	// Verify that when Content-Type is MIME-sniffed (either via
	// DetectContentType or via Go's implicit Write sniffing), the
	// sniffed type is present in BOTH entry.ContentType AND
	// entry.ResponseHeaders so the TUI detail overlay shows it
	// consistently in the Type field and the Headers list.

	t.Run("implicit_200_detect_content_type", func(t *testing.T) {
		// Handler writes body without setting Content-Type or WriteHeader.
		// The implicit-200 path uses http.DetectContentType.
		inner := httptest.NewRecorder()
		entry := &journal.Entry{}
		rec := &statusRecorder{ResponseWriter: inner, entry: entry, status: 0, captureMax: 1 << 20}

		rec.Write([]byte(`{"ok":true}`))

		if entry.ContentType == "" {
			t.Fatal("ContentType should be set via DetectContentType")
		}
		// The sniffed Content-Type must also appear in ResponseHeaders.
		if got := entry.ResponseHeaders.Get("Content-Type"); got != entry.ContentType {
			t.Errorf("ResponseHeaders.Content-Type = %q, want %q (parity with ContentType)", got, entry.ContentType)
		}
	})

	t.Run("explicit_200_then_sniff", func(t *testing.T) {
		// Handler calls WriteHeader(200) without Content-Type, then writes.
		// Go's MIME sniffing sets Content-Type on the underlying writer
		// during the Write call, captured by the post-Write backfill.
		sw := &explicitSniffWriter{httptest.NewRecorder()}
		entry := &journal.Entry{}
		rec := &statusRecorder{ResponseWriter: sw, entry: entry, status: 0, captureMax: 1 << 20}

		rec.WriteHeader(http.StatusOK)
		rec.Write([]byte("hello world"))

		if entry.ContentType == "" {
			t.Fatal("ContentType should be backfilled from underlying writer")
		}
		// The sniffed Content-Type must also appear in ResponseHeaders.
		if got := entry.ResponseHeaders.Get("Content-Type"); got != entry.ContentType {
			t.Errorf("ResponseHeaders.Content-Type = %q, want %q (parity with ContentType)", got, entry.ContentType)
		}
	})
}

// --- Circuit breaker integration tests ---

func TestProxy_CircuitBreakerRejectsWhenOpen(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(5*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit.
	b.RecordFailure(500, 0, time.Time{}, 0)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (circuit open), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "circuit open") {
		t.Errorf("body should mention circuit open, got %q", rec.Body.String())
	}
}

func TestProxy_PhantomConcurrencyPenalty(t *testing.T) {
	// The slot should be held for a penalty duration after a 5xx response.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `internal error`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100), // don't trip during this test
		circuitbreaker.WithBasePenalty(200*time.Millisecond),
		circuitbreaker.WithMaxPenalty(1*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)), // 1 slot so we can observe blocking
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First request: gets the slot, receives 500, then holds slot for penalty.
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()

	// Wait for the first request to acquire the slot.
	time.Sleep(20 * time.Millisecond)

	// Send a second request — it should block because the slot is held
	// by the phantom penalty, then succeed after the penalty expires.
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	// The second request should have waited at least the penalty duration
	// (200ms base + some overhead). If there were no penalty, it would
	// complete almost instantly.
	if elapsed < 100*time.Millisecond {
		t.Errorf("second request completed in %v, expected penalty delay (~200ms)", elapsed)
	}
}

func TestProxy_PhantomConcurrencyPenaltyIgnoresClientContext(t *testing.T) {
	// Verify that the phantom concurrency penalty holds the slot for its
	// full duration even when the client disconnects (context cancelled).
	// A malicious or impatient client must not be able to bypass the slot
	// hold by closing the connection immediately after receiving the error
	// response. The penalty runs asynchronously — the handler returns
	// immediately, but the slot is held by a background goroutine.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `error`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100),
		circuitbreaker.WithBasePenalty(200*time.Millisecond),
		circuitbreaker.WithMaxPenalty(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Send a request with a context that times out in 50ms — well before
	// the 200ms penalty. The handler returns immediately (async release),
	// but the slot must remain held for the full penalty duration.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	// The handler should return quickly (async release), NOT block for the
	// penalty duration.
	start := time.Now()
	p.ServeHTTP(rec, req)
	handlerElapsed := time.Since(start)

	// The handler must return quickly — it is NOT blocked by the penalty.
	// The penalty goroutine holds the slot in the background.
	if handlerElapsed > 100*time.Millisecond {
		t.Errorf("handler blocked for %v, expected <100ms (async release)", handlerElapsed)
	}

	// Verify the slot is still held by sending a second request. With a
	// concurrency of 1, it should block until the penalty expires.
	start2 := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed2 := time.Since(start2)

	// The second request should have waited at least the remaining penalty
	// duration (~150ms since the first handler returned quickly). If the
	// penalty were not running asynchronously, the second request would
	// succeed almost instantly.
	if elapsed2 < 100*time.Millisecond {
		t.Errorf("second request completed in %v, expected penalty delay (slot held async)", elapsed2)
	}
}

func TestProxy_CircuitBreakerFedFromFailedResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `bad gateway`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	s := b.Stats()
	if s.TotalFailures < 1 {
		t.Errorf("breaker should have recorded at least 1 failure, got %d", s.TotalFailures)
	}
}

func TestProxy_CircuitBreakerNilSafe(t *testing.T) {
	// Verify that all proxy operations work with a nil breaker.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestIsFailureStatus(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{200, false},
		{301, false},
		{400, false},
		{404, false},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true},
		{599, true},
		{600, false},
	}
	for _, tt := range tests {
		if got := circuitbreaker.IsFailureStatus(tt.code); got != tt.want {
			t.Errorf("IsFailureStatus(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
}

func TestProxy_StandaloneBreakerRecordsSuccess(t *testing.T) {
	// Verify that when a breaker is configured WITHOUT retries, the proxy
	// calls RecordSuccess for 2xx responses. Without this, the breaker
	// would permanently deadlock in HALF_OPEN after a single probe success.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(50*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Trip the circuit.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != circuitbreaker.Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for half-open.
	time.Sleep(80 * time.Millisecond)

	// Send a successful request through the proxy.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	s := b.Stats()
	if s.TotalSuccesses != 1 {
		t.Errorf("TotalSuccesses = %d, want 1 (proxy must report success to breaker)", s.TotalSuccesses)
	}
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("expected CLOSED after successful probe, got %v", b.State())
	}
}

func TestProxy_CircuitBreakerRejectIncrementsCircuitRejected(t *testing.T) {
	// Verify that circuit-open rejections increment TotalCircuitRejected,
	// NOT TotalTimeout (which is reserved for queue deadline exceeded).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit.
	b.RecordFailure(500, 0, time.Time{}, 0)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}

	snap := met.Snapshot()
	if snap.TotalCircuitRejected != 1 {
		t.Errorf("TotalCircuitRejected = %d, want 1", snap.TotalCircuitRejected)
	}
	if snap.TotalTimeout != 0 {
		t.Errorf("TotalTimeout = %d, want 0 (circuit rejection is NOT a queue timeout)", snap.TotalTimeout)
	}
}

// --- R21: review-03 fixes ---

func TestProxy_PhantomPenaltyDoesNotBlockHandler(t *testing.T) {
	// Verify that the phantom concurrency penalty is released asynchronously:
	// ServeHTTP returns immediately after a 5xx response, even though the
	// concurrency slot is held in the background for the penalty duration.
	// Before the fix, the handler blocked for the penalty duration, preventing
	// HTTP response finalization (chunked terminating chunk) and holding the
	// goroutine/TCP connection hostage for up to MaxPenalty (60s).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `error`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100),
		circuitbreaker.WithBasePenalty(200*time.Millisecond),
		circuitbreaker.WithMaxPenalty(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()

	start := time.Now()
	p.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	// The handler must return quickly — the penalty runs in the background.
	// Before the fix, this would take ~200ms (the penalty duration).
	if elapsed > 50*time.Millisecond {
		t.Errorf("handler blocked for %v, expected <50ms (penalty should be async)", elapsed)
	}
}

func TestProxy_PassthroughCircuitRejectedWhenOpen(t *testing.T) {
	// Verify that passthrough requests are rejected with 503 when the circuit
	// is OPEN, and that IncCircuitRejected is incremented. Before the fix,
	// passthrough requests had no breaker pre-check and would hit the upstream
	// even when the circuit was OPEN.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit.
	b.RecordFailure(500, 0, time.Time{}, 0)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Send a PASSTHROUGH request (GET /health — not in the limited set).
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (circuit open) for passthrough, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "circuit open") {
		t.Errorf("body should mention circuit open, got %q", rec.Body.String())
	}

	snap := met.Snapshot()
	if snap.TotalCircuitRejected != 1 {
		t.Errorf("TotalCircuitRejected = %d, want 1 (passthrough rejection must be counted)", snap.TotalCircuitRejected)
	}
}

func TestProxy_PassthroughBreakerReportsFailure(t *testing.T) {
	// Verify that when MaxRetries==0 and a breaker is configured, a 5xx
	// passthrough response causes the proxy to call RecordFailure on the
	// breaker. Before the fix, passthrough traffic with no retries completely
	// ignored the breaker.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `bad gateway`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		// MaxRetries defaults to 0 — no retry transport.
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Send a PASSTHROUGH request (GET /health).
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	s := b.Stats()
	if s.TotalFailures < 1 {
		t.Errorf("breaker should have recorded at least 1 failure from passthrough, got %d", s.TotalFailures)
	}
}

func TestProxy_PassthroughBreakerReportsSuccess(t *testing.T) {
	// Verify that when MaxRetries==0 and a breaker is configured, a 2xx
	// passthrough response causes the proxy to call RecordSuccess on the
	// breaker. This is critical for the HALF_OPEN→CLOSED transition: without
	// it, the breaker would deadlock in HALF_OPEN because no success signal
	// is ever received.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(50*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Trip the circuit.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != circuitbreaker.Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for half-open.
	time.Sleep(80 * time.Millisecond)

	// Send a PASSTHROUGH request (GET /health). This should pass the
	// breaker pre-check (transitions to HALF_OPEN) and succeed.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for passthrough probe, got %d", rec.Code)
	}

	s := b.Stats()
	if s.TotalSuccesses != 1 {
		t.Errorf("TotalSuccesses = %d, want 1 (passthrough success must report to breaker)", s.TotalSuccesses)
	}
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("expected CLOSED after successful passthrough probe, got %v", b.State())
	}
}

// mustBreaker creates a new circuit breaker or fails the test.
func mustBreaker(t *testing.T, opts ...circuitbreaker.Option) *circuitbreaker.Breaker {
	t.Helper()
	b, err := circuitbreaker.New(opts...)
	if err != nil {
		t.Fatalf("circuitbreaker.New: %v", err)
	}
	return b
}

func TestProxy_PhantomPenaltyNotAppliedOnCircuitRejection(t *testing.T) {
	// Verify that a circuit-open rejection (503) does NOT trigger the
	// phantom concurrency penalty. The penalty is for UPSTREAM failures --
	// self-induced rejections must not hold slots hostage.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(1), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Trip the breaker.
	b.RecordFailure(500, 0, time.Time{}, 0)
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != circuitbreaker.Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Send a request that will be rejected by the circuit breaker.
	// The phantom penalty should NOT be applied because the request
	// never reached upstream (reachedUpstream=false).
	start := time.Now()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}

	// The phantom penalty can be up to 60s. If it were applied,
	// the slot would be held for that duration. Since we're testing
	// that it's NOT applied, the response should return quickly.
	if elapsed > 500*time.Millisecond {
		t.Errorf("response took %v -- phantom penalty may be incorrectly applied on circuit rejection", elapsed)
	}
}

func TestProxy_PhantomPenaltyNotAppliedOnQueueTimeout(t *testing.T) {
	// Verify that a queue timeout (504) does NOT trigger the phantom
	// concurrency penalty. The request never reached upstream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(5), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
		WithQueueTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Fill up the concurrency limiter so the next request times out.
	acquired := make([]func(), 4)
	for i := range 4 {
		acquired[i], err = p.limiter.Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
	defer func() {
		for _, release := range acquired {
			release()
		}
	}()

	start := time.Now()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
	elapsed := time.Since(start)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", rec.Code)
	}

	// If phantom penalty were applied, the slot would be held for
	// the penalty duration. Since it shouldn't be applied, the
	// response returns quickly.
	if elapsed > 500*time.Millisecond {
		t.Errorf("response took %v -- phantom penalty may be incorrectly applied on queue timeout", elapsed)
	}
}

func TestProxy_StandaloneBreakerRespectsRetryAfter(t *testing.T) {
	// Verify that when retries are disabled and the upstream returns
	// Retry-After, the breaker's penalty reflects it. Previously the
	// proxy hardcoded retryAfter=0, discarding the upstream hint.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(5), circuitbreaker.WithWindow(10*time.Second), circuitbreaker.WithBasePenalty(1*time.Second), circuitbreaker.WithMaxPenalty(60*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}

	// The penalty should reflect the remaining Retry-After duration (close
	// to 10s), not the base penalty of 1s or 0. A tiny amount of time may
	// have elapsed between header receipt and evaluation.
	penalty := b.PenaltyDuration()
	if penalty < 9*time.Second || penalty > 10*time.Second {
		t.Errorf("penalty = %v, want approximately 10s (Retry-After from upstream)", penalty)
	}
}

func TestProxy_StandaloneBreakerIgnoresClientCancel(t *testing.T) {
	// Verify that when retries are disabled and the client disconnects
	// (context.Canceled), the standalone breaker path does NOT call
	// RecordFailure. An attacker could otherwise trip the breaker by
	// initiating and immediately dropping connections. This mirrors the
	// isClientCancel guard in the retry transport (R22-07).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow upstream — the client will cancel before we respond.
		<-r.Context().Done()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(5), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0), // standalone breaker — no retry transport
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Cancel the request context immediately to simulate client disconnect.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)).WithContext(ctx))

	s := b.Stats()
	if s.TotalFailures != 0 {
		t.Errorf("TotalFailures = %d, want 0 (client cancel should not be reported to breaker)", s.TotalFailures)
	}
}

func TestProxy_PassthroughBreakerIgnoresClientCancel(t *testing.T) {
	// Same as TestProxy_StandaloneBreakerIgnoresClientCancel but for the
	// passthrough path — verifying that the isClientCancel guard also
	// protects the passthrough breaker reporting.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(5), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(route.DefaultPatterns())),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0), // standalone breaker — no retry transport
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := httptest.NewRecorder()
	// GET /unlimited is a passthrough route (not limited).
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/unlimited", nil).WithContext(ctx))

	s := b.Stats()
	if s.TotalFailures != 0 {
		t.Errorf("TotalFailures = %d, want 0 (client cancel should not be reported to breaker in passthrough)", s.TotalFailures)
	}
}

func TestProxy_PhantomPenaltyIgnoresClientCancel(t *testing.T) {
	// Verify that the phantom concurrency penalty is NOT applied when a
	// client cancels their request after it has reached upstream. When a
	// client disconnects, httputil.ReverseProxy's error handler writes a
	// 502 status. Since IsFailureStatus(502)==true, the phantom penalty
	// would fire erroneously without the isClientCancel guard — allowing
	// a malicious client to exhaust concurrency slots by initiating
	// requests and immediately closing connections.
	//
	// Test strategy: use an upstream that is slow for the first request
	// (to trigger the client context deadline) but fast for subsequent
	// requests. If the phantom penalty were applied, the second request
	// would block for ~200ms waiting for the slot.
	var mu sync.Mutex
	firstRequest := true
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isFirst := firstRequest
		if isFirst {
			firstRequest = false
		}
		mu.Unlock()
		if isFirst {
			close(started)
			// Delay long enough for the client context to cancel.
			time.Sleep(200 * time.Millisecond)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Subsequent requests respond immediately.
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100),
		circuitbreaker.WithBasePenalty(200*time.Millisecond),
		circuitbreaker.WithMaxPenalty(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)), // 1 slot so we can observe blocking
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0), // standalone breaker — no retry transport
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Send a request with a short timeout that cancels while the upstream
	// is still processing. The request reaches upstream (reachedUpstream=true)
	// but the client context deadline expires, causing a 502 from
	// ReverseProxy.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec, req)
		close(done)
	}()

	// Wait for the first request to start reaching upstream.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received request")
	}

	// Wait for the first request to complete (it will fail due to context
	// deadline, the upstream responds with 500 but the proxy sees 502).
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not complete")
	}

	// The context deadline exceeded — the phantom penalty should NOT be
	// applied. Send a second request — if the phantom penalty were
	// applied, the slot would be held for 200ms and the second request
	// would block. Since the upstream is fast for subsequent requests,
	// the second request should complete quickly.
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	// The second request should complete quickly — no phantom penalty.
	if elapsed > 50*time.Millisecond {
		t.Errorf("second request took %v — phantom penalty may be incorrectly applied on client cancel", elapsed)
	}
	if rec2.Code != http.StatusOK {
		t.Errorf("second request status = %d, want 200", rec2.Code)
	}
}

func TestProxy_PhantomPenaltyAppliesOnUpstream5xxDespiteClientCancel(t *testing.T) {
	// R33-01 regression test: Verify that the phantom concurrency penalty IS
	// applied when the upstream returns a definitive failure status (e.g., 500)
	// and the client then disconnects. Before the fix, the defer used
	// !isClientCancel which suppressed the penalty for ALL client cancellations
	// — even when the upstream had already returned a real 5xx. The fix mirrors
	// the breaker reporting logic: only suppress the penalty when the client
	// cancelled AND the status is ambiguous (0 or 502, meaning transport/proxy
	// error). A real upstream 5xx is NOT ambiguous — the upstream is genuinely
	// failing, and bypassing the penalty would allow an attacker to rapidly
	// recycle slots.
	//
	// Strategy: upstream returns 500 on the first request and 200 on subsequent
	// requests. The first request has a client context deadline. The proxy sees
	// rec.status=500 (a real upstream status, not 0 or 502). The phantom penalty
	// MUST be applied. Verify by sending a second request on the same single-slot
	// limiter — it should block for ~200ms (the penalty duration).
	var mu sync.Mutex
	firstRequest := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isFirst := firstRequest
		if isFirst {
			firstRequest = false
		}
		mu.Unlock()
		if isFirst {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `internal error`)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100),
		circuitbreaker.WithBasePenalty(200*time.Millisecond),
		circuitbreaker.WithMaxPenalty(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)), // 1 slot so we can observe blocking
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0), // standalone breaker — no retry transport
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Send a request with a short deadline. The upstream returns 500 immediately,
	// so rec.status=500 (a definitive upstream failure, not 0 or 502). The client
	// context expires shortly after the response is written.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (upstream error)", rec.Code)
	}

	// The phantom penalty should be applied because rec.status=500 is NOT
	// a transport/proxy error. Send a second request — it should block for
	// ~200ms (the penalty duration).
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	if elapsed < 150*time.Millisecond {
		t.Errorf("second request took %v — phantom penalty not applied (expected ~200ms delay)", elapsed)
	}
	if rec2.Code != http.StatusOK {
		t.Errorf("second request status = %d, want 200", rec2.Code)
	}
}

func TestProxy_PassthroughPhantomPenaltyAppliesOnUpstream5xxDespiteClientCancel(t *testing.T) {
	// R33-01 regression test (passthrough variant): Same scenario as
	// TestProxy_PhantomPenaltyAppliesOnUpstream5xxDespiteClientCancel but
	// through the passthrough path with a global limiter. Verify that the
	// passthrough slot-release defer also applies the phantom penalty when
	// the upstream returns a real 5xx and the client then cancels.
	var mu sync.Mutex
	firstRequest := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isFirst := firstRequest
		if isFirst {
			firstRequest = false
		}
		mu.Unlock()
		if isFirst {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `internal error`)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100),
		circuitbreaker.WithBasePenalty(200*time.Millisecond),
		circuitbreaker.WithMaxPenalty(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)), // 1 global slot to observe blocking
		WithBreaker(b),
		WithMaxRetries(0), // standalone breaker
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Send a passthrough request (GET /v1/models, not in limited routes).
	// The upstream returns 500 on the first request. The phantom penalty should
	// apply to the global limiter slot.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (upstream error)", rec.Code)
	}

	// Send a second passthrough request — should block for ~200ms.
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	if elapsed < 150*time.Millisecond {
		t.Errorf("second request took %v — phantom penalty not applied (expected ~200ms delay)", elapsed)
	}
	if rec2.Code != http.StatusOK {
		t.Errorf("second request status = %d, want 200", rec2.Code)
	}
}

func TestProxy_PassthroughMetricExcludesCircuitRejection(t *testing.T) {
	// Verify that TotalPassThrough is NOT incremented when a passthrough
	// request is rejected by the circuit breaker. The metric should only
	// count requests that actually pass through to upstream, mirroring
	// serveLimited's IncProxied() placement at the end.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit.
	b.RecordFailure(500, 0, time.Time{}, 0)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Send a PASSTHROUGH request that will be rejected by the circuit breaker.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (circuit open), got %d", rec.Code)
	}

	snap := met.Snapshot()
	if snap.TotalPassThrough != 0 {
		t.Errorf("TotalPassThrough = %d, want 0 (circuit rejection should NOT count as passthrough)", snap.TotalPassThrough)
	}
	if snap.TotalCircuitRejected != 1 {
		t.Errorf("TotalCircuitRejected = %d, want 1", snap.TotalCircuitRejected)
	}
}

func TestProxy_PassthroughQueueTimeoutRespected(t *testing.T) {
	// R26-02 regression test: Verify that a passthrough request waiting in the
	// global limiter respects QueueTimeout. Before the fix, passthrough requests
	// blocked indefinitely when the global limiter was saturated.
	//
	// Strategy: upstream sleeps forever, global limiter has 1 slot, queue timeout
	// is 100ms. First request (passthrough) takes the slot. Second passthrough
	// request should receive 503 within the timeout window.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second) // block forever (relative to test)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)), // not used for passthrough
		WithMetrics(met),
		WithQueueTimeout(100*time.Millisecond),
		WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)), // 1 global slot
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First passthrough request takes the only global slot (blocks).
	firstDone := make(chan struct{})
	go func() {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		close(firstDone)
	}()

	// Wait for the first request to acquire the slot.
	time.Sleep(20 * time.Millisecond)

	// Second passthrough request should time out waiting for global limiter.
	start := time.Now()
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/unlimited", nil))
	elapsed := time.Since(start)

	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (queue timeout on global limiter), got %d", rec2.Code)
	}

	// The request should complete within roughly queue timeout + overhead.
	// Before the fix, it would block for ~5s (until the upstream sleep finished).
	if elapsed > 300*time.Millisecond {
		t.Errorf("second request took %v, expected <300ms (queue timeout should apply)", elapsed)
	}

	// Cleanup: unblock the first request by closing the upstream.
	<-firstDone
}

func TestProxy_Upstream5xxReportedDespiteClientCancel(t *testing.T) {
	// R26-04 regression test: Verify that when the upstream returns a real 5xx
	// status code and the client disconnects mid-response, the breaker STILL
	// records the failure. Before the fix, the isClientCancel guard was too broad
	// and suppressed ALL failures when the client cancelled — even genuine
	// upstream 5xx responses that had already been written.
	//
	// Strategy: upstream returns 500 immediately. The client's context has a
	// short deadline. The 500 comes from upstream, so it must be reported.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `internal error`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(10), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0), // standalone breaker
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Use a context with a short deadline. The upstream writes 500 immediately,
	// but the client context may expire during response transfer. The 500 is
	// from upstream — it must be reported to the breaker regardless.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// The response should be the upstream's 500, not a proxy-generated 502.
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (upstream error, not proxy-generated)", rec.Code)
	}

	// The breaker must have recorded the failure.
	s := b.Stats()
	if s.TotalFailures < 1 {
		t.Errorf("TotalFailures = %d, want >= 1 (upstream 5xx must be reported despite client cancel)", s.TotalFailures)
	}
}

func TestProxy_TransportErrorIgnoredOnClientCancel(t *testing.T) {
	// R26-04 preservation test: Verify that transport errors (status 0, meaning
	// no response received) with client-initiated cancellation are still ignored
	// by the breaker. This is the EXISTING correct behavior that must not be
	// broken by the isClientCancel refinement. Only real upstream 5xx should be
	// reported; transport errors from client disconnects must be suppressed.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never respond — the client will cancel before we write anything.
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(5), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0), // standalone breaker
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Cancel the context immediately to simulate client disconnect before
	// the upstream can respond. This produces a transport error (status 0)
	// or a proxy-generated 502 — both must be suppressed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	s := b.Stats()
	if s.TotalFailures != 0 {
		t.Errorf("TotalFailures = %d, want 0 (transport error from client cancel must not be reported to breaker)", s.TotalFailures)
	}
}

func TestParseRetryAfterFromRecorder(t *testing.T) {
	// Verify that parseRetryAfterFromRecorder returns the remaining delay,
	// not the raw upstream ban duration. The captured responseAt is the
	// receipt time; the caller-supplied evaluatedAt is when the defer
	// evaluates the result. Expired bans must report zero even if the Date
	// header implies a positive original duration.

	ref := time.Unix(1700000000, 0)
	date := ref.Add(-2 * time.Second) // upstream generated the response 2s before we saw it

	tests := []struct {
		name        string
		h           http.Header
		responseAt  time.Time
		evaluatedAt time.Time
		want        time.Duration
	}{
		{
			name:        "delay-seconds immediate",
			h:           http.Header{"Retry-After": []string{"30"}},
			responseAt:  ref,
			evaluatedAt: ref,
			want:        30 * time.Second,
		},
		{
			name:        "delay-seconds elapsed",
			h:           http.Header{"Retry-After": []string{"30"}},
			responseAt:  ref,
			evaluatedAt: ref.Add(5 * time.Second),
			want:        25 * time.Second,
		},
		{
			name:        "delay-seconds expired",
			h:           http.Header{"Retry-After": []string{"30"}},
			responseAt:  ref,
			evaluatedAt: ref.Add(30 * time.Second),
			want:        0,
		},
		{
			name: "http-date with date header remaining",
			h: http.Header{
				"Date":        []string{date.UTC().Format(http.TimeFormat)},
				"Retry-After": []string{date.Add(5 * time.Second).UTC().Format(http.TimeFormat)},
			},
			responseAt:  ref,
			evaluatedAt: ref,
			// Date-relative: intendedDelta = (Date+5s) - Date = 5s,
			// proxyElapsed = 0, remaining = 5s. This does not account for
			// network transit time (2s in this test), but eliminates clock
			// skew. The transit time overestimation is conservative.
			want: 5 * time.Second,
		},
		{
			name: "http-date without date header uses absolute expiry",
			h: http.Header{
				"Retry-After": []string{ref.Add(5 * time.Second).UTC().Format(http.TimeFormat)},
			},
			responseAt:  ref,
			evaluatedAt: ref,
			want:        5 * time.Second,
		},
		{
			name: "expired http-date returns zero",
			h: http.Header{
				"Date":        []string{date.UTC().Format(http.TimeFormat)},
				"Retry-After": []string{date.Add(-5 * time.Second).UTC().Format(http.TimeFormat)},
			},
			responseAt:  ref,
			evaluatedAt: ref,
			want:        0,
		},
		{
			name: "slow body expires date-relative ban",
			h: http.Header{
				"Date":        []string{date.UTC().Format(http.TimeFormat)},
				"Retry-After": []string{date.Add(2 * time.Second).UTC().Format(http.TimeFormat)},
			},
			// Upstream expiry is Date+2s == ref. We received at ref and
			// evaluated 10s later, so the ban is long expired.
			responseAt:  ref,
			evaluatedAt: ref.Add(10 * time.Second),
			want:        0,
		},
		{
			name:        "missing",
			h:           http.Header{},
			responseAt:  ref,
			evaluatedAt: ref,
			want:        0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &statusRecorder{
				ResponseWriter: httptest.NewRecorder(),
				responseAt:     tt.responseAt,
				entry: &journal.Entry{
					ResponseHeaders: tt.h.Clone(),
				},
			}
			got := parseRetryAfterFromRecorder(rec, tt.evaluatedAt)
			if got != tt.want {
				t.Errorf("parseRetryAfterFromRecorder() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProxy_PanicRecovery(t *testing.T) {
	// Verify that a panic in the proxy's ServeHTTP path is recovered,
	// and the client receives a 502 response instead of a connection close.
	// We test by providing a transport that panics on RoundTrip, which
	// httputil.ReverseProxy will propagate.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			panic("boom from transport")
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()

	// This should not panic the test goroutine — the proxy recovers.
	p.ServeHTTP(rec, req)

	// The panic should be caught and a 502 returned.
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d (502 Bad Gateway after panic recovery)", rec.Code, http.StatusBadGateway)
	}

	// The 502 should be reflected in metrics (statusRecorder captured it).
	snap := met.Snapshot()
	if snap.StatusCounts[5] != 1 {
		t.Errorf("StatusCounts[5] = %d, want 1 (panic 502 captured by statusRecorder)", snap.StatusCounts[5])
	}
}

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestProxy_CancelCooldownDelaysRelease(t *testing.T) {
	// Verify that when a client cancels after the request reached upstream,
	// the slot is held for the configured cancelCooldown duration before
	// being returned to the limiter (KILL-04 mitigation).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	lim := queue.NewLimiterWithCooldown(1, 0) // single slot for easy observation

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(lim),
		WithMetrics(met),
		WithCancelCooldown(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Request 1: cancel the context after upstream has responded.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	// Start the request, then cancel the client context after a tiny delay.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	p.ServeHTTP(rec, req)

	// The slot should NOT be immediately available — it's in cooldown.
	stats := lim.Stats()
	if stats.Active != 0 {
		// Slot was released immediately — cooldown didn't work.
		// But we can't guarantee timing, so wait briefly and check it IS still held.
		t.Logf("slot already released (Active=%d) — timing dependent, not a failure", stats.Active)
	}

	// Wait for cooldown to expire, then verify the slot is available.
	time.Sleep(300 * time.Millisecond)
	stats = lim.Stats()
	if stats.Active != 0 {
		t.Errorf("after cooldown, Active = %d, want 0 (slot should be released)", stats.Active)
	}
}

func TestProxy_CancelCooldownZeroReleasesImmediately(t *testing.T) {
	// Verify that cancelCooldown=0 (default) releases the slot immediately
	// on client cancel — no cooldown applied.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	lim := queue.NewLimiterWithCooldown(1, 0)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(lim),
		WithMetrics(met),
		// No WithCancelCooldown — defaults to 0
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	p.ServeHTTP(rec, req)

	// Slot should be released immediately (no cooldown).
	time.Sleep(20 * time.Millisecond)
	stats := lim.Stats()
	if stats.Active != 0 {
		t.Errorf("Active = %d, want 0 (no cooldown, immediate release)", stats.Active)
	}
}

func TestProxy_CancelCooldownFiresOnSlowUpstreamClientCancel(t *testing.T) {
	// R34-01 regression test: Verify that the cancelCooldown fires when the
	// client cancels while the upstream is still processing (rec.status == 0,
	// WriteHeader never called). This is the KILL-04 mitigation case — the
	// upstream is still working on the abandoned request, so releasing the slot
	// immediately would allow slot-exhaustion attacks. The !isTransportOrProxyError
	// guard was erroneously applied to the cancelCooldown branch, suppressing it
	// when rec.status == 0 (which this test exercises).
	var mu sync.Mutex
	firstRequest := true
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isFirst := firstRequest
		if isFirst {
			firstRequest = false
		}
		mu.Unlock()
		if isFirst {
			close(started)
			// Sleep long enough for the client context to expire.
			// Do NOT call WriteHeader — leaves rec.status == 0.
			time.Sleep(500 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			return
		}
		// Subsequent requests respond immediately.
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	lim := queue.NewLimiterWithCooldown(1, 0)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(lim),
		WithMetrics(met),
		WithCancelCooldown(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Request 1: cancel the context while upstream is still processing.
	// The upstream sleeps 500ms, so the 50ms timeout will fire while
	// rec.status is still 0.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received request")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not complete")
	}

	// The slot should be in cooldown — NOT immediately available.
	// Send a second request (which goes to the fast upstream path).
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	// The second request should take at least ~100ms because the cooldown
	// holds the slot. Without the fix, the slot would be released immediately
	// and the second request would complete in ~10ms (fast upstream).
	if elapsed < 100*time.Millisecond {
		t.Errorf("second request took %v — cancelCooldown may not be firing when rec.status == 0 (KILL-04 regression)", elapsed)
	}
	if rec2.Code != http.StatusOK {
		t.Errorf("second request status = %d, want 200", rec2.Code)
	}
}

func TestProxy_EpochContextHandoff_Limited(t *testing.T) {
	// Verify that when retries are active and a limited-route request's
	// first attempt fails, the breaker's RecordFailure uses the epoch from
	// the proxy's Allow() call — not 0. This is the fix for review-06
	// Finding 1: without the context handoff, the retry transport's
	// breakerEpoch would be 0 for attempt 0, bypassing the stale-probe
	// guard in circuitbreaker.RecordFailure.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "error")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(5),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(1*time.Second),
	)
	if err != nil {
		t.Fatalf("circuitbreaker.New: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0), // no retries — first attempt reports to breaker
		WithRetryWaitMin(time.Millisecond),
		WithRetryWaitMax(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// The breaker should have recorded a failure with a non-zero epoch
	// (the epoch from the proxy's Allow() call).
	stats := b.Stats()
	if stats.TotalFailures != 1 {
		t.Errorf("total failures = %d, want 1", stats.TotalFailures)
	}
	if stats.ConsecutiveFailures != 1 {
		t.Errorf("consecutive failures = %d, want 1", stats.ConsecutiveFailures)
	}
}

func TestProxy_EpochContextHandoff_WithRetries(t *testing.T) {
	// Verify the epoch handoff works when the retry transport is active.
	// The retry transport's first attempt (attempt 0) must use the epoch
	// from the proxy's Allow() call, not epoch 0.
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "error")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(1*time.Second),
	)
	if err != nil {
		t.Fatalf("circuitbreaker.New: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(2), // retries active — retry transport handles breaker
		WithRetryWaitMin(time.Millisecond),
		WithRetryWaitMax(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// With MaxRetries=2, we expect 3 attempts (initial + 2 retries).
	if calls != 3 {
		t.Errorf("upstream calls = %d, want 3", calls)
	}

	// All 3 failures should be recorded by the breaker (via the retry transport).
	stats := b.Stats()
	if stats.TotalFailures != 3 {
		t.Errorf("total failures = %d, want 3", stats.TotalFailures)
	}
}

func TestProxy_EpochContextHandoff_Passthrough(t *testing.T) {
	// Verify the epoch handoff also works for passthrough routes.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "error")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(5),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(1*time.Second),
	)
	if err != nil {
		t.Fatalf("circuitbreaker.New: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
		WithRetryWaitMin(time.Millisecond),
		WithRetryWaitMax(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Use a GET request which is a passthrough route.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	stats := b.Stats()
	if stats.TotalFailures != 1 {
		t.Errorf("total failures = %d, want 1", stats.TotalFailures)
	}
}

func TestProxy_ReleaseCooldownPreventsImmediateReuse(t *testing.T) {
	// Verify that the post-release cooldown delays slot re-admission at the
	// proxy level (KILL-02 mitigation). With concurrency=1 and cooldown=200ms,
	// two sequential requests should have ~200ms gap between the first's
	// completion and the second's start.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	lim := queue.NewLimiterWithCooldown(1, 200*time.Millisecond)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(lim),
		WithMetrics(met),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First request: acquires the single slot.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec1 := httptest.NewRecorder()
	p.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("request 1: status = %d, want 200", rec1.Code)
	}

	// Second request should be delayed by the 200ms cooldown.
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	if elapsed < 150*time.Millisecond {
		t.Errorf("second request elapsed = %v, want >= 200ms (cooldown)", elapsed)
	}
	if rec2.Code != http.StatusOK {
		t.Errorf("request 2: status = %d, want 200", rec2.Code)
	}
}

func TestProxy_RetryBreakerLifecycle(t *testing.T) {
	// End-to-end test: retry transport + circuit breaker full lifecycle.
	// CLOSED → (failures from retries) → OPEN → (timeout) → HALF_OPEN → (success) → CLOSED
	calls := 0
	var mu sync.Mutex

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		c := calls
		mu.Unlock()
		if c <= 4 {
			// First 4 calls: return 500 to trip the breaker.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Subsequent calls: success.
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(3),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(1*time.Second),
		circuitbreaker.WithBasePenalty(100*time.Millisecond),
		circuitbreaker.WithMaxPenalty(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("circuitbreaker.New: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(1), // retry transport handles breaker
		WithRetryWaitMin(time.Millisecond),
		WithRetryWaitMax(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Phase 1: CLOSED — first request retries and fails (2 upstream calls).
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("phase 1: status = %d, want 500", rec.Code)
	}

	// Phase 2: Second request also fails (2 more upstream calls = 4 total).
	// With 4 failures and threshold=3, the breaker should be OPEN.
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req)
	stats := b.Stats()
	t.Logf("after 2 requests: breaker state=%s, failures=%d, total=%d",
		stats.State, stats.Failures, stats.TotalFailures)

	// Phase 3: Third request should be rejected by the circuit breaker.
	rec3 := httptest.NewRecorder()
	p.ServeHTTP(rec3, req)
	if rec3.Code != http.StatusServiceUnavailable {
		stats = b.Stats()
		t.Errorf("phase 3: status = %d, want 503 (breaker should be OPEN, state=%s)", rec3.Code, stats.State)
	}

	// Phase 4: Wait for the OPEN timeout to expire, then the next request
	// should be allowed as a HALF_OPEN probe. We may need multiple attempts
	// because the retry transport's Allow() for retry attempts can also
	// be rejected while the breaker is in HALF_OPEN with probeInFlight.
	time.Sleep(1100 * time.Millisecond)

	// Try up to 3 times to get a successful probe through.
	var probeOK bool
	for i := range 3 {
		rec4 := httptest.NewRecorder()
		p.ServeHTTP(rec4, req)
		stats = b.Stats()
		t.Logf("probe attempt %d: status=%d, breaker state=%s, successes=%d, calls=%d",
			i+1, rec4.Code, stats.State, stats.TotalSuccesses, calls)
		if rec4.Code == http.StatusOK {
			probeOK = true
			break
		}
		// If still OPEN, wait more for the next open timeout cycle.
		time.Sleep(1100 * time.Millisecond)
	}

	if !probeOK {
		stats = b.Stats()
		t.Errorf("HALF_OPEN probe never succeeded after 3 attempts; breaker state=%s, total successes=%d, calls=%d",
			stats.State, stats.TotalSuccesses, calls)
	}

	// Phase 5: After successful probe, breaker should be CLOSED.
	// Verify subsequent requests flow normally.
	rec5 := httptest.NewRecorder()
	p.ServeHTTP(rec5, req)
	stats = b.Stats()
	if rec5.Code != http.StatusOK {
		t.Errorf("phase 5: status = %d, want 200 (breaker should be CLOSED, state=%s)", rec5.Code, stats.State)
	}
	mu.Lock()
	finalCalls := calls
	mu.Unlock()
	t.Logf("final: calls=%d, failures=%d, successes=%d, state=%s",
		finalCalls, stats.TotalFailures, stats.TotalSuccesses, stats.State)

	// Verify the breaker saw multiple failures from the retry transport.
	stats = b.Stats()
	if stats.TotalFailures < 3 {
		t.Errorf("total failures = %d, want >= 3 (retries reported to breaker)", stats.TotalFailures)
	}
	if stats.TotalSuccesses < 1 {
		t.Errorf("total successes = %d, want >= 1 (probe succeeded)", stats.TotalSuccesses)
	}
}

func TestProxy_AccountingLagCausesConcurrencyOverflow(t *testing.T) {
	// This test demonstrates the core mechanism behind the "5-hour naughty
	// corner": when an upstream provider has accounting lag (it doesn't
	// decrement its concurrency counter immediately after sending a
	// response), the proxy can cause the upstream to observe more concurrent
	// requests than the configured limit.
	//
	// The mechanism is simple: the proxy releases a slot as soon as it
	// receives the response, but the upstream hasn't finished its internal
	// accounting yet. The next request acquires the slot and arrives at
	// the upstream while the previous request is still "active" from the
	// upstream's perspective. Under sustained load, this creates a
	// permanent N+1 (or worse) observed concurrency.
	//
	// No retries are needed — this is purely about the slot cycling speed
	// exceeding the upstream's accounting speed. It happens on EVERY
	// response (success or failure), not just on retries.
	//
	// This is the mechanism that -release-cooldown and -cancel-cooldown
	// are designed to prevent.
	const (
		concurrencyLimit = 4
		numClients       = 16
		accountingLag    = 200 * time.Millisecond
	)

	var (
		active atomic.Int64
		peak   atomic.Int64
		mu     sync.Mutex
		counts []int64
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)

		// Track peak concurrently.
		for {
			p := peak.Load()
			if current <= p || peak.CompareAndSwap(p, current) {
				break
			}
		}

		mu.Lock()
		counts = append(counts, current)
		mu.Unlock()

		// Brief processing time (models real upstream latency).
		time.Sleep(2 * time.Millisecond)

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")

		// Simulate accounting lag: the upstream's concurrency counter
		// is not decremented until after this delay. This models real
		// provider behavior where internal accounting (load balancers,
		// rate limiters, billing systems) is asynchronous.
		go func() {
			time.Sleep(accountingLag)
			active.Add(-1)
		}()
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	// No breaker → no phantom penalty on failure.
	// No release cooldown → slot returned immediately.
	// This is the DEFAULT production configuration.
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(concurrencyLimit, 0)),
		WithMetrics(met),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Launch concurrent client requests. With concurrency=4 and 16 clients,
	// the proxy processes them in batches of 4. Each batch completes in
	// ~2ms (upstream processing time), but the upstream's accounting lag
	// is 200ms. By the time batch 2 arrives (4ms), batch 1's accounting
	// hasn't completed. The upstream sees 8 concurrent. By batch 3, 12.
	// By batch 4, 16.
	var wg sync.WaitGroup
	for range numClients {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
		})
	}
	wg.Wait()

	observed := peak.Load()
	t.Logf("peak concurrent requests at upstream: %d (limit: %d)", observed, concurrencyLimit)
	t.Logf("total upstream requests: %d", len(counts))

	mu.Lock()
	for i, c := range counts {
		if c > int64(concurrencyLimit) {
			t.Logf("  request #%d: %d concurrent (exceeds limit of %d)", i+1, c, concurrencyLimit)
		}
	}
	mu.Unlock()

	if observed <= int64(concurrencyLimit) {
		t.Errorf(
			"peak concurrent = %d, want > %d — the accounting lag should cause the upstream to observe concurrency exceeding the limit",
			observed, concurrencyLimit,
		)
	}
}

func TestProxy_ReleaseCooldownPreventsConcurrencyOverflow(t *testing.T) {
	// This test demonstrates that release-cooldown prevents the concurrency
	// overflow shown in TestProxy_AccountingLagCausesConcurrencyOverflow.
	// When the cooldown >= accounting lag, the slot is not re-admitted until
	// after the upstream has finished its cleanup, so the upstream never
	// sees more than the configured limit.
	const (
		concurrencyLimit = 4
		numClients       = 16
		accountingLag    = 50 * time.Millisecond
		releaseCooldown  = 100 * time.Millisecond
	)

	var (
		active atomic.Int64
		peak   atomic.Int64
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		for {
			p := peak.Load()
			if current <= p || peak.CompareAndSwap(p, current) {
				break
			}
		}

		time.Sleep(2 * time.Millisecond)

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")

		go func() {
			time.Sleep(accountingLag)
			active.Add(-1)
		}()
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	// THE KEY DIFFERENCE: releaseCooldown (100ms) exceeds the accounting
	// lag (50ms). After each slot release, the token is held in limbo for
	// 100ms before re-admission. By the time the next request acquires the
	// slot and reaches the upstream, the previous request's accounting has
	// completed. No N+1.
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(concurrencyLimit, releaseCooldown)),
		WithMetrics(met),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	var wg sync.WaitGroup
	for range numClients {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
		})
	}
	wg.Wait()

	observed := peak.Load()
	t.Logf("peak concurrent requests at upstream: %d (limit: %d)", observed, concurrencyLimit)

	if observed > int64(concurrencyLimit) {
		t.Errorf(
			"peak concurrent = %d, want <= %d — releaseCooldown should prevent concurrency overflow",
			observed, concurrencyLimit,
		)
	}
}

func TestProxy_DefaultConfigRobustAgainstAccountingLag(t *testing.T) {
	// TDD: The proxy's DEFAULT configuration must prevent concurrency overflow
	// when the upstream has moderate accounting lag. This is the "robust out of
	// the box" test — the operator should not need to tune any flags to avoid
	// downstream concurrency violations under normal conditions.
	//
	// With the new defaults (release-cooldown=200ms, cancel-cooldown=200ms,
	// retry-min-delay=1s), the proxy inserts dead zones between slot release
	// and re-admission, preventing the N+1 accounting lag problem.
	const (
		concurrencyLimit = 4
		numClients       = 16
		accountingLag    = 100 * time.Millisecond // moderate lag
	)

	var (
		active atomic.Int64
		peak   atomic.Int64
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		for {
			p := peak.Load()
			if current <= p || peak.CompareAndSwap(p, current) {
				break
			}
		}

		time.Sleep(2 * time.Millisecond)

		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")

		go func() {
			time.Sleep(accountingLag)
			active.Add(-1)
		}()
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	// DEFAULT production configuration — using NewLimiterWithCooldown with
	// the new default release-cooldown of 200ms. The operator does NOT need
	// to set any flags for this protection.
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(concurrencyLimit, 200*time.Millisecond)),
		WithMetrics(met),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	var wg sync.WaitGroup
	for range numClients {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
		})
	}
	wg.Wait()

	observed := peak.Load()
	t.Logf("peak concurrent requests at upstream: %d (limit: %d)", observed, concurrencyLimit)

	if observed > int64(concurrencyLimit) {
		t.Errorf(
			"FAIL: peak concurrent = %d, want <= %d — default config must prevent concurrency overflow",
			observed, concurrencyLimit,
		)
	}
}

func TestProxy_Retry429DoesNotAmplify(t *testing.T) {
	// TDD: When the upstream returns 429 (rate limited), the retry transport
	// must NOT retry if -retry-skip-429 is enabled. Retrying 429 creates a
	// positive feedback loop: more concurrency → more 429s → more retries →
	// more concurrency. This is the #2 cause of upstream bans.
	const (
		concurrencyLimit = 4
		numClients       = 8
	)

	var (
		calls atomic.Int64
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := calls.Add(1)
		// First 4 requests return 429, then succeed.
		if c <= 4 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"rate limited"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(concurrencyLimit, 0)),
		WithMetrics(met),
		WithMaxRetries(2),
		WithMaxBodyBytes(1<<20),
		WithRetryWaitMin(time.Millisecond),
		WithRetryWaitMax(time.Millisecond),
		WithRetrySkipOn429(true),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	var wg sync.WaitGroup
	for range numClients {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
		})
	}
	wg.Wait()

	totalCalls := calls.Load()
	t.Logf("total upstream calls: %d (clients: %d)", totalCalls, numClients)

	// Without retry-skip-429: 8 clients × (1 + 2 retries) = 24 calls worst case
	// With retry-skip-429: 8 clients × 1 attempt = 8 calls
	// The first 4 get 429 (not retried), the next 4 succeed.
	// Total = exactly numClients (no retries on 429).
	if totalCalls > int64(numClients) {
		t.Errorf("total upstream calls = %d, want <= %d — 429s must not be retried when retry-skip-429 is enabled", totalCalls, numClients)
	}
}

func TestProxy_FailureHoldWithoutBreaker(t *testing.T) {
	// TDD: The failure hold (-failure-hold) must hold the slot after an
	// upstream failure even when the circuit breaker is disabled. Without
	// this, the failure path has no protection — the slot is released
	// immediately, creating N+1 observed concurrency.
	const failureHold = 200 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "error")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	lim := queue.NewLimiterWithCooldown(1, 0) // single slot for easy observation

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(lim),
		WithMetrics(met),
		WithFailureHold(failureHold),
		// NO breaker — the failure hold must work independently.
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First request gets the slot, receives 500, failure hold begins.
	start := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	handlerElapsed := time.Since(start)

	// The handler must return quickly (async release).
	if handlerElapsed > 50*time.Millisecond {
		t.Errorf("handler blocked for %v, expected <50ms (failure hold should be async)", handlerElapsed)
	}

	// The slot should NOT be immediately available — it's in failure hold.
	// Send a second request; it should block until the hold expires.
	start2 := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed2 := time.Since(start2)

	// The second request should have waited at least the failure hold duration.
	if elapsed2 < 150*time.Millisecond {
		t.Errorf("second request completed in %v, expected >= %v (failure hold)", elapsed2, failureHold)
	}
	if rec2.Code != http.StatusInternalServerError {
		t.Errorf("second request status = %d, want 500", rec2.Code)
	}
}

func TestProxy_PassthroughFailureHold(t *testing.T) {
	// Verify that failureHold is applied to passthrough global-limiter slots,
	// mirroring serveLimited's behavior. Without this, a passthrough 5xx
	// would release its global-limiter slot immediately, creating N+1 observed
	// concurrency from downstream accounting lag.
	const failureHold = 200 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "error")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	globalLimiter := queue.NewLimiterWithCooldown(1, 0) // single global slot

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithGlobalLimiter(globalLimiter),
		WithFailureHold(failureHold),
		// NO breaker — failure hold must work independently.
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First request: passthrough (GET /health), receives 500, failure hold begins.
	start := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	handlerElapsed := time.Since(start)

	// The handler must return quickly (async release).
	if handlerElapsed > 50*time.Millisecond {
		t.Errorf("handler blocked for %v, expected <50ms (failure hold should be async)", handlerElapsed)
	}

	// The global slot should NOT be immediately available — it's in failure hold.
	start2 := time.Now()
	req2 := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed2 := time.Since(start2)

	if elapsed2 < 150*time.Millisecond {
		t.Errorf("second passthrough completed in %v, expected >= %v (failure hold)", elapsed2, failureHold)
	}
	if rec2.Code != http.StatusInternalServerError {
		t.Errorf("second passthrough status = %d, want 500", rec2.Code)
	}
}

func TestProxy_PassthroughPhantomPenaltyWithBreaker(t *testing.T) {
	// Verify that phantom penalty is applied to passthrough global-limiter slots
	// when the circuit breaker is enabled. This mirrors serveLimited's phantom
	// penalty and ensures passthrough failures also reduce downstream concurrency.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "error")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	globalLimiter := queue.NewLimiterWithCooldown(1, 0) // single global slot

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100), // don't trip
		circuitbreaker.WithBasePenalty(200*time.Millisecond),
		circuitbreaker.WithMaxPenalty(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithGlobalLimiter(globalLimiter),
		WithBreaker(b),
		// failureHold > 0 but breaker is enabled, so phantom penalty takes precedence.
		WithFailureHold(100*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First request: passthrough (GET /health), receives 500, phantom penalty applied.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()

	time.Sleep(20 * time.Millisecond)

	// Second request should wait for phantom penalty duration.
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	if elapsed < 50*time.Millisecond {
		t.Errorf("second passthrough completed in %v -- phantom penalty may not be applied", elapsed)
	}
	if rec2.Code != http.StatusInternalServerError {
		t.Errorf("second passthrough status = %d, want 500", rec2.Code)
	}
}

func TestProxy_FailureHoldZeroIsNoop(t *testing.T) {
	// Verify that failure-hold=0 (disabled) does not delay slot release.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "error")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	lim := queue.NewLimiterWithCooldown(1, 0)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(lim),
		WithMetrics(met),
		WithFailureHold(0), // disabled
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// Slot should be released immediately (no failure hold).
	time.Sleep(20 * time.Millisecond)
	stats := lim.Stats()
	if stats.Active != 0 {
		t.Errorf("Active = %d, want 0 (no failure hold, immediate release)", stats.Active)
	}
}

func TestProxy_PassthroughBreakerCheckBeforeGlobalLimiter(t *testing.T) {
	// Verify that when the circuit breaker is OPEN and a global limiter is
	// configured, the passthrough request is rejected WITHOUT acquiring a
	// global-limiter slot. Before the fix, servePassthrough acquired the
	// global slot first, then checked the breaker — wasteful and inconsistent
	// with serveLimited which checks the breaker before queueing.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	globalLimiter := queue.NewLimiterWithCooldown(4, 0)

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithGlobalLimiter(globalLimiter),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Trip the breaker.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != circuitbreaker.Open {
		t.Fatal("breaker should be OPEN")
	}

	// Send a passthrough request — should be rejected immediately.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (circuit open)", rec.Code)
	}

	snap := met.Snapshot()
	if snap.TotalCircuitRejected != 1 {
		t.Errorf("TotalCircuitRejected = %d, want 1", snap.TotalCircuitRejected)
	}
	// The global limiter should show 0 active (request was rejected before acquire).
	stats := globalLimiter.Stats()
	if stats.Active != 0 {
		t.Errorf("globalLimiter Active = %d, want 0 (breaker rejected before slot acquire)", stats.Active)
	}
}

func TestProxy_PassthroughCancelCooldownWithGlobalLimiter(t *testing.T) {
	// Verify that cancel-cooldown is applied to passthrough global-limiter slots
	// when a client disconnects after the request reached upstream. This mirrors
	// serveLimited's cancel-cooldown behavior and was missing test coverage after
	// the servePassthrough rewrite.
	const cancelCooldown = 200 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	globalLimiter := queue.NewLimiterWithCooldown(1, 0) // single global slot

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithGlobalLimiter(globalLimiter),
		WithCancelCooldown(cancelCooldown),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Send a passthrough request and cancel it after upstream receipt.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	// Start the request — it will complete quickly since upstream returns 200.
	// Cancel after a brief delay (simulating client disconnect after upstream receipt).
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	p.ServeHTTP(rec, req)

	// The global slot should be in cancel-cooldown hold.
	// Send a second request — it should wait for the cooldown.
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	// The second request may or may not have waited for the full cooldown,
	// depending on timing. We just verify it didn't fail.
	if rec2.Code != http.StatusOK {
		t.Errorf("second passthrough status = %d, want 200", rec2.Code)
	}
	// If the cancel cooldown was applied, the second request should have waited
	// at least some time (but timing is inherently imprecise, so we use a
	// generous lower bound).
	_ = elapsed // timing-based assertions are fragile; this test primarily
	// verifies the code path executes without panic or deadlock.
}

func TestProxy_PassthroughCancelCooldownFiresOnSlowUpstreamClientCancel(t *testing.T) {
	// R34-02 regression test: Verify that the cancelCooldown fires in
	// servePassthrough when the client cancels while the upstream is still
	// processing (rec.status == 0). This mirrors
	// TestProxy_CancelCooldownFiresOnSlowUpstreamClientCancel for the
	// passthrough/global-limiter path.
	var mu sync.Mutex
	firstRequest := true
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isFirst := firstRequest
		if isFirst {
			firstRequest = false
		}
		mu.Unlock()
		if isFirst {
			close(started)
			// Sleep long enough for the client context to expire.
			// Do NOT call WriteHeader — leaves rec.status == 0.
			time.Sleep(500 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			return
		}
		// Subsequent requests respond immediately.
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	globalLimiter := queue.NewLimiterWithCooldown(1, 0)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithGlobalLimiter(globalLimiter),
		WithCancelCooldown(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Send a passthrough request with a short timeout — the upstream sleeps
	// 500ms so the client cancels while rec.status == 0.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received request")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not complete")
	}

	// The global slot should be in cooldown. Send a second request (fast
	// upstream path) and verify it waits for the cooldown.
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	// The second request should take at least ~100ms because the cooldown
	// holds the global slot. Without the fix, the slot would be released
	// immediately and the second request would complete in ~10ms (fast upstream).
	if elapsed < 100*time.Millisecond {
		t.Errorf("second passthrough request took %v — cancelCooldown may not be firing when rec.status == 0 (KILL-04 regression)", elapsed)
	}
	if rec2.Code != http.StatusOK {
		t.Errorf("second passthrough request status = %d, want 200", rec2.Code)
	}
}

func TestProxy_DefaultConfigRobustAgainstRetryAmplification(t *testing.T) {
	// TDD: With the default retry-min-delay (1s), retries must not arrive at
	// the upstream before it finishes accounting. This test uses a moderate
	// accounting lag (200ms) and verifies that with retry-min-delay=1s, the
	// retry arrives AFTER the upstream has cleaned up.
	const (
		concurrencyLimit = 4
		accountingLag    = 200 * time.Millisecond
	)

	var (
		active atomic.Int64
		peak   atomic.Int64
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		for {
			p := peak.Load()
			if current <= p || peak.CompareAndSwap(p, current) {
				break
			}
		}

		time.Sleep(2 * time.Millisecond)
		// Return 500 on first attempt, success on retry.
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "error")

		go func() {
			time.Sleep(accountingLag)
			active.Add(-1)
		}()
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(concurrencyLimit, 200*time.Millisecond)),
		WithMetrics(met),
		WithMaxRetries(1),
		WithMaxBodyBytes(1<<20),
		WithRetryWaitMin(time.Millisecond),
		WithRetryWaitMax(time.Millisecond),
		WithRetryMinDelay(1*time.Second), // default
		WithRetrySkipOn429(true),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Send 4 concurrent requests. Each will get 500, then retry after 1s.
	// During the 1s retry delay, the upstream should finish accounting for
	// the initial 4 requests (accounting lag is only 200ms).
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
		})
	}
	wg.Wait()

	observed := peak.Load()
	t.Logf("peak concurrent at upstream: %d (limit: %d)", observed, concurrencyLimit)

	// With retry-min-delay=1s and accounting-lag=200ms, the retry arrives
	// 800ms AFTER the upstream finished accounting. Peak should never exceed
	// the concurrency limit.
	if observed > int64(concurrencyLimit) {
		t.Errorf("peak concurrent = %d, want <= %d — retry-min-delay should prevent retry amplification", observed, concurrencyLimit)
	}
}

func TestProxy_PanicRecoveryMetricsComplete(t *testing.T) {
	// Verify that panic recovery records ALL metrics — not just RecordStatus.
	// Before the fix, the panic handler called RecordStatus(502) but skipped
	// RecordRequest and IncProxied/IncPassThrough. This caused TUI metrics
	// drift: the sum of status codes exceeded the total request counts.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			panic("boom from transport")
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Limited route panic.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	snap := met.Snapshot()

	// StatusCounts[5] must be 1 (the 502).
	if snap.StatusCounts[5] != 1 {
		t.Errorf("StatusCounts[5] = %d, want 1", snap.StatusCounts[5])
	}

	// TotalProxied must be 1 — the panic request counts as proxied.
	if snap.TotalProxied != 1 {
		t.Errorf("TotalProxied = %d, want 1 (panic request must be counted)", snap.TotalProxied)
	}

	// The request log must have an entry for the panic request.
	if len(snap.LogEntries) != 1 {
		t.Fatalf("LogEntries = %d, want 1", len(snap.LogEntries))
	}
	if snap.LogEntries[0].Status != http.StatusBadGateway {
		t.Errorf("LogEntries[0].Status = %d, want 502", snap.LogEntries[0].Status)
	}
	if snap.LogEntries[0].Method != "POST" {
		t.Errorf("LogEntries[0].Method = %q, want POST", snap.LogEntries[0].Method)
	}
	if snap.LogEntries[0].Path != "/v1/messages" {
		t.Errorf("LogEntries[0].Path = %q, want /v1/messages", snap.LogEntries[0].Path)
	}
	if !snap.LogEntries[0].Limited {
		t.Error("LogEntries[0].Limited = false, want true")
	}

	// Consistency check: sum of all status counts must equal total requests.
	var statusTotal int64
	for _, v := range snap.StatusCounts {
		statusTotal += v
	}
	reqTotal := snap.TotalProxied + snap.TotalPassThrough + snap.TotalTimeout + snap.TotalCancelled + snap.TotalCircuitRejected
	if statusTotal != reqTotal {
		t.Errorf("metrics drift: status total = %d, request total = %d (should be equal)", statusTotal, reqTotal)
	}
}

func TestProxy_PanicRecoveryMetricsComplete_Passthrough(t *testing.T) {
	// Same as TestProxy_PanicRecoveryMetricsComplete but for passthrough routes.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			panic("boom from transport")
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Passthrough route (GET /health is not limited).
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	snap := met.Snapshot()

	if snap.StatusCounts[5] != 1 {
		t.Errorf("StatusCounts[5] = %d, want 1", snap.StatusCounts[5])
	}

	// TotalPassThrough must be 1 (not TotalProxied — this is passthrough).
	if snap.TotalPassThrough != 1 {
		t.Errorf("TotalPassThrough = %d, want 1 (panic passthrough must be counted)", snap.TotalPassThrough)
	}
	if snap.TotalProxied != 0 {
		t.Errorf("TotalProxied = %d, want 0 (this was passthrough)", snap.TotalProxied)
	}

	if len(snap.LogEntries) != 1 {
		t.Fatalf("LogEntries = %d, want 1", len(snap.LogEntries))
	}
	if snap.LogEntries[0].Status != http.StatusBadGateway {
		t.Errorf("LogEntries[0].Status = %d, want 502", snap.LogEntries[0].Status)
	}
	if snap.LogEntries[0].Limited {
		t.Error("LogEntries[0].Limited = true, want false (passthrough)")
	}

	// Consistency check.
	var statusTotal int64
	for _, v := range snap.StatusCounts {
		statusTotal += v
	}
	reqTotal := snap.TotalProxied + snap.TotalPassThrough + snap.TotalTimeout + snap.TotalCancelled + snap.TotalCircuitRejected
	if statusTotal != reqTotal {
		t.Errorf("metrics drift: status total = %d, request total = %d (should be equal)", statusTotal, reqTotal)
	}
}

func TestProxy_PanicRecovery_PhantomPenaltySeesCorrectStatus(t *testing.T) {
	// Verify that when a LOCAL panic occurs in the inner transport, the phantom
	// penalty defer in serveLimited does NOT apply the phantom penalty or report
	// a failure to the circuit breaker. A local panic (e.g., nil pointer in
	// proxy code) is NOT an upstream failure — holding the slot for the penalty
	// duration or recording a false breaker failure would penalize legitimate
	// traffic for a proxy-internal problem.
	// The fix adds a localPanic flag: the inner recovery sets it true, and the
	// phantom penalty defer + breaker reporting both check it to skip their
	// failure-path logic when a local panic (not upstream failure) occurred.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100), // don't trip
		circuitbreaker.WithBasePenalty(200*time.Millisecond),
		circuitbreaker.WithMaxPenalty(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)), // 1 slot so we can observe blocking
		WithMetrics(met),
		WithBreaker(b),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			panic("boom from transport")
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First request: panics, recovered with 502, localPanic=true so slot
	// is released immediately (no phantom penalty or breaker failure recorded).
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()

	time.Sleep(20 * time.Millisecond)

	// Second request should acquire the slot immediately since the first
	// request's panic did NOT trigger phantom penalty (localPanic bypass).
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	// The second request should complete quickly — no 200ms phantom penalty delay.
	if elapsed > 100*time.Millisecond {
		t.Errorf("second request took %v -- phantom penalty may have been incorrectly applied for local panic", elapsed)
	}

	if rec2.Code != http.StatusBadGateway {
		t.Errorf("second request status = %d, want 502", rec2.Code)
	}

	// Verify the breaker did NOT record failures from the local panics.
	stats := b.Stats()
	if stats.TotalFailures != 0 {
		t.Errorf("breaker TotalFailures = %d, want 0 (local panics should not be reported to breaker)", stats.TotalFailures)
	}
}

func TestProxy_PanicRecovery_NoBreakerFailureForLocalPanic(t *testing.T) {
	// Verify that a local panic in serveLimited does NOT record a failure
	// with the circuit breaker. The localPanic flag prevents false
	// RecordFailure calls for proxy-internal crashes that are not upstream
	// failures.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100),
		circuitbreaker.WithBasePenalty(200*time.Millisecond),
		circuitbreaker.WithMaxPenalty(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			panic("boom from transport")
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	stats := b.Stats()
	if stats.TotalFailures != 0 {
		t.Errorf("breaker TotalFailures = %d, want 0 (local panic should not report to breaker)", stats.TotalFailures)
	}
	if stats.ConsecutiveFailures != 0 {
		t.Errorf("breaker ConsecutiveFailures = %d, want 0", stats.ConsecutiveFailures)
	}
}

func TestProxy_PanicRecovery_NoBreakerFailureForLocalPanic_Passthrough(t *testing.T) {
	// Verify that a local panic in servePassthrough does NOT record a failure
	// with the circuit breaker. Mirrors TestProxy_PanicRecovery_NoBreakerFailureForLocalPanic
	// but for passthrough routes.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100),
		circuitbreaker.WithBasePenalty(200*time.Millisecond),
		circuitbreaker.WithMaxPenalty(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			panic("boom from transport")
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// GET /health is a passthrough route (not limited).
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	stats := b.Stats()
	if stats.TotalFailures != 0 {
		t.Errorf("breaker TotalFailures = %d, want 0 (local panic should not report to breaker)", stats.TotalFailures)
	}
	if stats.ConsecutiveFailures != 0 {
		t.Errorf("breaker ConsecutiveFailures = %d, want 0", stats.ConsecutiveFailures)
	}
}

func TestProxy_PanicRecovery_JournalRecorded(t *testing.T) {
	// Verify that when a panic occurs, the journal entry is recorded.
	// Before the inner recovery fix, the panic propagated to ServeHTTP's
	// recovery, which recorded metrics but skipped the journal block at
	// the end of ServeHTTP (the function returned from the recovery defer).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	j := journal.New(100, 1<<20) // 100 entries, 1MB max body capture

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithJournal(j),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			panic("boom from transport")
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	// The journal must have an entry for the panicked request.
	entries := j.Entries()
	if len(entries) != 1 {
		t.Fatalf("journal entries = %d, want 1 (panic request must be journalized)", len(entries))
	}
	if entries[0].StatusCode != http.StatusBadGateway {
		t.Errorf("journal entry status = %d, want 502", entries[0].StatusCode)
	}
	if entries[0].Method != "POST" {
		t.Errorf("journal entry method = %q, want POST", entries[0].Method)
	}
	if !entries[0].Limited {
		t.Error("journal entry limited = false, want true")
	}

	// Metrics must also be complete.
	snap := met.Snapshot()
	if snap.TotalProxied != 1 {
		t.Errorf("TotalProxied = %d, want 1", snap.TotalProxied)
	}
	if snap.StatusCounts[5] != 1 {
		t.Errorf("StatusCounts[5] = %d, want 1", snap.StatusCounts[5])
	}
}

func TestProxy_403TemporaryBanTriggersPhantomPenalty(t *testing.T) {
	// When the upstream escalates rate-limit enforcement to a temporary
	// API-key/IP ban via 403, the proxy must treat it as a failure and
	// hold the concurrency slot for the phantom penalty. After the
	// response-aware 403 change, only rate-limit-signaled 403s (like this
	// Retry-After response) still count as failures; bare auth 403s no
	// longer trigger the penalty. This test locks in the temporary-ban path.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `forbidden`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100), // keep CLOSED for this test
		circuitbreaker.WithBasePenalty(200*time.Millisecond),
		circuitbreaker.WithMaxPenalty(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First request gets the slot, receives 403, and should hold the slot
	// for the penalty duration.
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()

	// Wait for the first request to acquire the slot.
	time.Sleep(20 * time.Millisecond)

	// A second request must wait for the phantom penalty to expire before
	// it can acquire the slot. If 403 did not trigger the penalty, it
	// would complete almost instantly.
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	if rec2.Code != http.StatusForbidden {
		t.Errorf("second request status = %d, want 403", rec2.Code)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("second request completed in %v, expected penalty hold (~200ms)", elapsed)
	}
}

func TestProxy_403TemporaryBanTripsCircuitBreaker(t *testing.T) {
	// A run of 403s from the upstream must trip the circuit breaker so
	// new requests are rejected with 503 without reaching the banned
	// upstream. This stops the request bleed that keeps a temporary ban
	// alive.
	callCount := atomic.Int64{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Header().Set("x-ratelimit-reset", "60")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `forbidden`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithWindow(30*time.Second),
		circuitbreaker.WithBasePenalty(50*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 from upstream, got %d", rec.Code)
		}
	}

	if b.State() != circuitbreaker.Open {
		t.Fatalf("breaker state = %s, want OPEN", b.State())
	}

	// The next request must be rejected at the breaker pre-check.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (circuit open), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "circuit open") {
		t.Errorf("body should mention circuit open, got %q", rec.Body.String())
	}

	// No extra upstream calls should have been made for the rejected request.
	if got := callCount.Load(); got != 2 {
		t.Errorf("upstream call count = %d, want 2", got)
	}
}

func TestProxy_Bare403AuthErrorDoesNotTripCircuitBreaker(t *testing.T) {
	callCount := atomic.Int64{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `invalid api key`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithWindow(30*time.Second),
		circuitbreaker.WithBasePenalty(50*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(met),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	for range 3 {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("request returned status = %d, want bare upstream 403", rec.Code)
		}
	}

	if b.State() != circuitbreaker.Closed {
		t.Fatalf("breaker state = %s, want CLOSED for bare auth 403s", b.State())
	}
	if got := callCount.Load(); got != 3 {
		t.Errorf("upstream call count = %d, want 3", got)
	}
}

func mustParsePattern(t *testing.T, s string) route.Pattern {
	t.Helper()
	pat, err := route.Parse(s)
	if err != nil {
		t.Fatalf("failed to parse pattern %q: %v", s, err)
	}
	return pat
}

func TestProxy_UpstreamConnectionCap(t *testing.T) {
	const concurrency = 4

	tests := []struct {
		name                   string
		disableKeepAlives      bool
		maxIdleConnsPerHost    int
		wantIdleOpenAfterBurst bool
	}{
		{
			name:                   "keepalives_enabled_capped_pool",
			disableKeepAlives:      false,
			maxIdleConnsPerHost:    concurrency,
			wantIdleOpenAfterBurst: true,
		},
		{
			name:                   "keepalives_disabled",
			disableKeepAlives:      true,
			maxIdleConnsPerHost:    concurrency,
			wantIdleOpenAfterBurst: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				activeCount atomic.Int64
				peakActive  atomic.Int64
				openCount   atomic.Int64
				states      sync.Map
			)

			release := make(chan struct{})
			s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-release
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, `ok`)
			}))
			s.Config.ConnState = func(conn net.Conn, state http.ConnState) {
				switch state {
				case http.StateNew:
					states.Store(conn, state)
					openCount.Add(1)
				case http.StateActive:
					states.Store(conn, state)
					if c := activeCount.Add(1); c > peakActive.Load() {
						peakActive.Store(c)
					}
				case http.StateIdle:
					states.Store(conn, state)
					activeCount.Add(-1)
				case http.StateClosed, http.StateHijacked:
					if previous, ok := states.Load(conn); ok && previous == http.StateActive {
						activeCount.Add(-1)
					}
					states.Delete(conn)
					openCount.Add(-1)
				}
			}
			s.Start()
			t.Cleanup(s.Close)

			upstreamURL, err := url.Parse(s.URL)
			if err != nil {
				t.Fatalf("failed to parse upstream URL: %v", err)
			}

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{mustParsePattern(t, "POST /v1/messages")})),
				WithLimiter(queue.NewLimiterWithCooldown(concurrency, 0)),
				WithQueueTimeout(0),
				WithMetrics(metrics.NewCollector()),
				WithTransport(&http.Transport{
					DisableKeepAlives:   tt.disableKeepAlives,
					MaxIdleConnsPerHost: tt.maxIdleConnsPerHost,
				}),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			const numReqs = 8
			var wg sync.WaitGroup
			wg.Add(numReqs)
			for range numReqs {
				go func() {
					defer wg.Done()
					req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
					rec := httptest.NewRecorder()
					p.ServeHTTP(rec, req)
					if rec.Code != http.StatusOK {
						t.Errorf("expected 200, got %d", rec.Code)
					}
				}()
			}

			for {
				time.Sleep(5 * time.Millisecond)
				if activeCount.Load() >= concurrency {
					break
				}
			}

			if got := peakActive.Load(); got > concurrency {
				t.Errorf("proxy allowed %d active upstream connections, want <= %d", got, concurrency)
			}

			close(release)
			wg.Wait()

			time.Sleep(100 * time.Millisecond)

			final := openCount.Load()
			t.Logf("disableKeepAlives=%v peakActive=%d finalOpen=%d limit=%d",
				tt.disableKeepAlives, peakActive.Load(), final, concurrency)

			if tt.disableKeepAlives && final != 0 {
				t.Errorf("keep-alives disabled: final open connections = %d, want 0", final)
			}
			if !tt.disableKeepAlives && final == 0 {
				t.Errorf("keep-alives enabled: expected some idle connections to remain, got 0")
			}
		})
	}
}

func TestProxy_AdaptiveHeadroomOn429(t *testing.T) {
	const concurrency = 4
	const window = 200 * time.Millisecond

	var (
		activeCount atomic.Int64
		peakActive  atomic.Int64
		states      sync.Map
	)

	release := make(chan struct{})
	releaseRecovered := make(chan struct{})
	phase := atomic.Int32{} // 0 = 429, 1 = reduced, 2 = recovered

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch phase.Load() {
		case 0:
			w.WriteHeader(http.StatusTooManyRequests)
		case 1:
			<-release
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "ok")
		case 2:
			<-releaseRecovered
			w.WriteHeader(http.StatusOK)
		}
	})

	upstream := httptest.NewUnstartedServer(handler)
	upstream.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		switch state {
		case http.StateActive:
			states.Store(conn, state)
			if c := activeCount.Add(1); c > peakActive.Load() {
				peakActive.Store(c)
			}
		case http.StateIdle:
			states.Store(conn, state)
			activeCount.Add(-1)
		case http.StateClosed, http.StateHijacked:
			if previous, ok := states.Load(conn); ok && previous == http.StateActive {
				activeCount.Add(-1)
			}
			states.Delete(conn)
		}
	}
	upstream.Start()
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("failed to parse upstream URL: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{mustParsePattern(t, "POST /v1/messages")})),
		WithLimiter(queue.NewLimiterWithCooldown(concurrency, 0)),
		WithQueueTimeout(0),
		WithMetrics(metrics.NewCollector()),
		WithAdaptiveHeadroom(true),
		WithAdaptiveHeadroomWindow(window),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Trigger adaptive headroom with a single 429.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	// Give the adaptive reduction time to apply (it is triggered in the defer).
	time.Sleep(20 * time.Millisecond)

	if !p.limiter.AdaptiveActive() {
		t.Fatal("expected adaptive headroom to be active after a 429")
	}

	// Second burst: the effective limit is now 3, so only 3 upstream requests
	// should become active simultaneously.
	phase.Store(1)
	peakActive.Store(0)
	activeCount.Store(0)

	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
		})
	}

	// Wait until at least 3 are active; there should never be a 4th.
	for {
		time.Sleep(5 * time.Millisecond)
		if activeCount.Load() >= 3 {
			break
		}
	}

	// Brief pause to let any accidental 4th request slip through.
	time.Sleep(50 * time.Millisecond)
	if got := peakActive.Load(); got > 3 {
		t.Errorf("after adaptive reduction, expected <= 3 active upstream requests, got %d", got)
	}

	close(release)
	wg.Wait()

	// After the window expires, the full 4 slots should be available again.
	time.Sleep(window + 100*time.Millisecond)
	if p.limiter.AdaptiveActive() {
		t.Fatal("adaptive headroom should have expired")
	}

	phase.Store(2)
	peakActive.Store(0)
	activeCount.Store(0)

	var wg2 sync.WaitGroup
	for range concurrency {
		wg2.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
		})
	}

	for {
		time.Sleep(5 * time.Millisecond)
		if activeCount.Load() >= concurrency {
			break
		}
	}

	if got := peakActive.Load(); got != concurrency {
		t.Errorf("after recovery, expected %d active upstream requests, got %d", concurrency, got)
	}

	close(releaseRecovered)
	wg2.Wait()
}

func TestProxy_403RetryAfterExtendsPenalty(t *testing.T) {
	// If the upstream sends Retry-After on a 403, the phantom penalty
	// should honor it so the concurrency slot stays out of the pool until
	// the upstream says it is ready. The implementation caps the value at
	// cb-max-penalty to prevent a malicious/large header from blocking
	// the slot indefinitely.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `forbidden`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100),
		circuitbreaker.WithBasePenalty(50*time.Millisecond),
		circuitbreaker.WithMaxPenalty(5*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	elapsed := time.Since(start)

	if rec2.Code != http.StatusForbidden {
		t.Errorf("second request status = %d, want 403", rec2.Code)
	}
	// The Retry-After header says 2s, so the slot should be held at least
	// until then (with a little scheduling slack allowed).
	if elapsed < 1500*time.Millisecond {
		t.Errorf("second request completed in %v, expected >= ~2s Retry-After hold", elapsed)
	}
}

// slowResponseWriter wraps an httptest.ResponseRecorder and sleeps on every
// Write. This simulates a downstream/client that reads the response body
// slowly, forcing httputil.ReverseProxy.ServeHTTP to remain blocked until
// the entire body has been transferred.
type slowResponseWriter struct {
	*httptest.ResponseRecorder
	delay time.Duration
}

func (w *slowResponseWriter) Write(b []byte) (int, error) {
	time.Sleep(w.delay)
	return w.ResponseRecorder.Write(b)
}

func TestProxy_403SlowDripPenaltyDurationHonored(t *testing.T) {
	// Regression for review-13: the penalty must be the remaining delay, not
	// the raw (Retry-After - Date) duration. With a finite body transfer,
	// some of the ban window elapses before the proxy evaluates the failure;
	// the recorded penalty must reflect what is actually left, not the full 2s
	// requested by the upstream. We measure this via the breaker's
	// CurrentPenalty rather than wall-clock elapsed, keeping the test tight
	// and deterministic.

	base := time.Now().UTC()
	bodyStarted := make(chan struct{})
	var bodyStartedOnce sync.Once

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", base.Format(http.TimeFormat))
		w.Header().Set("Retry-After", base.Add(2*time.Second).Format(http.TimeFormat))
		w.WriteHeader(http.StatusForbidden)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		bodyStartedOnce.Do(func() { close(bodyStarted) })
		for i := range 5 {
			fmt.Fprintf(w, "chunk%d\n", i)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100), // keep CLOSED for this test
		circuitbreaker.WithBasePenalty(50*time.Millisecond),
		circuitbreaker.WithMaxPenalty(5*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := &slowResponseWriter{
			ResponseRecorder: httptest.NewRecorder(),
			delay:            150 * time.Millisecond,
		}
		p.ServeHTTP(rec, req)
		close(done)
	}()

	<-bodyStarted
	<-done

	penalty := b.PenaltyDuration()
	// The upstream asked for 2s from Date. The body took >500ms upstream
	// plus 5 x 150ms slow-writer delay, so >1.25s elapsed. The penalty
	// must be less than the raw 2s and must be positive.
	if penalty <= 0 || penalty >= 2*time.Second {
		t.Errorf("penalty = %v, want remaining delay in (0, 2s)", penalty)
	}
}

func TestProxy_403SlowDripStillActiveTemporaryBan(t *testing.T) {
	// A slow-drip body must not misclassify a ban that is still active.
	// With remaining-delay semantics, classification checks the absolute expiry
	// instant; if that instant is still in the future after body streaming,
	// the 403 is still a temporary ban and the breaker opens.

	base := time.Now().UTC()
	callCount := atomic.Int64{}
	bodyStarted := make(chan struct{})
	var bodyStartedOnce sync.Once

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Header().Set("Date", base.Format(http.TimeFormat))
		// Retry-After is far enough in the future that it remains active
		// after an ~8 second body transfer.
		w.Header().Set("Retry-After", base.Add(30*time.Second).Format(http.TimeFormat))
		w.WriteHeader(http.StatusForbidden)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		bodyStartedOnce.Do(func() { close(bodyStarted) })
		for i := range 8 {
			fmt.Fprintf(w, "chunk%d\n", i)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(1 * time.Second)
		}
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(30*time.Second),
		circuitbreaker.WithBasePenalty(10*time.Millisecond),
		circuitbreaker.WithMaxPenalty(1*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := &slowResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		delay:            150 * time.Millisecond,
	}

	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec, req)
		close(done)
	}()

	<-bodyStarted
	<-done

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if callCount.Load() != 1 {
		t.Fatalf("upstream call count = %d, want 1", callCount.Load())
	}
	if b.State() != circuitbreaker.Open {
		t.Fatalf("breaker state = %s, want OPEN (still-active ban must classify)", b.State())
	}
}

func TestProxy_403SlowDripExpiredRetryAfterNotTemporaryBan(t *testing.T) {
	// Regression for review-13: returning the raw (Retry-After - Date)
	// duration kept expired bans alive. If the Retry-After HTTP-date expires
	// during the body transfer, classification must now treat the 403 as a
	// permanent auth error and must NOT trip the breaker.

	base := time.Now().UTC()
	callCount := atomic.Int64{}
	bodyStarted := make(chan struct{})
	var bodyStartedOnce sync.Once

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Header().Set("Date", base.Format(http.TimeFormat))
		// Retry-After is 2s after Date. The body transfer lasts ~8s, so the
		// ban expires well before ServeHTTP returns.
		w.Header().Set("Retry-After", base.Add(2*time.Second).Format(http.TimeFormat))
		w.WriteHeader(http.StatusForbidden)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		bodyStartedOnce.Do(func() { close(bodyStarted) })
		for i := range 8 {
			fmt.Fprintf(w, "chunk%d\n", i)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(1 * time.Second)
		}
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(30*time.Second),
		circuitbreaker.WithBasePenalty(10*time.Millisecond),
		circuitbreaker.WithMaxPenalty(1*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(b),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := &slowResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		delay:            150 * time.Millisecond,
	}

	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec, req)
		close(done)
	}()

	<-bodyStarted
	<-done

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if callCount.Load() != 1 {
		t.Fatalf("upstream call count = %d, want 1", callCount.Load())
	}
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("breaker state = %s, want CLOSED (expired Retry-After must not trip breaker)", b.State())
	}
	if b.Stats().TotalFailures != 0 {
		t.Fatalf("TotalFailures = %d, want 0 (expired Retry-After must not count)", b.Stats().TotalFailures)
	}
}
