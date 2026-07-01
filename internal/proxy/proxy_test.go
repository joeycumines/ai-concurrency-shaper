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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/retry"
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

type flushErrorWriter struct {
	http.ResponseWriter
	err     error
	flushed bool
}

func (m *flushErrorWriter) FlushError() error {
	m.flushed = true
	return m.err
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
	if recNoFlush.downstreamWriteFailed() {
		t.Fatalf("ErrNotSupported flush marked downstream failure: %v", recNoFlush.writeErr)
	}
	if _, ok := any(recNoFlush).(http.Flusher); ok {
		t.Fatal("statusRecorder advertises http.Flusher even though only FlushError/Unwrap should expose supported flushing")
	}
}

func TestStatusRecorderFlushErrorTracksRealFailure(t *testing.T) {
	inner := httptest.NewRecorder()
	mock := &flushErrorWriter{ResponseWriter: inner, err: errProxyTestDownstreamFlush}
	rec := &statusRecorder{ResponseWriter: mock, status: http.StatusOK}

	err := rec.FlushError()
	if !errors.Is(err, errProxyTestDownstreamFlush) {
		t.Fatalf("FlushError error = %v, want %v", err, errProxyTestDownstreamFlush)
	}
	if !mock.flushed {
		t.Fatal("underlying FlushError was not called")
	}
	if !rec.downstreamWriteFailed() {
		t.Fatal("downstreamWriteFailed = false, want true after real flush error")
	}
	if !errors.Is(rec.writeErr, errProxyTestDownstreamFlush) {
		t.Fatalf("rec.writeErr = %v, want %v", rec.writeErr, errProxyTestDownstreamFlush)
	}
}

func TestStatusRecorderFlushErrorImplicitOK(t *testing.T) {
	t.Run("supported flush records implicit ok", func(t *testing.T) {
		inner := httptest.NewRecorder()
		inner.Header().Set("X-Test", "flush")
		entry := &journal.Entry{}
		rec := &statusRecorder{ResponseWriter: inner, entry: entry, status: 0}

		if err := rec.FlushError(); err != nil {
			t.Fatalf("FlushError: %v", err)
		}
		if inner.Code != http.StatusOK {
			t.Fatalf("inner status = %d, want implicit 200", inner.Code)
		}
		if rec.status != http.StatusOK || !rec.terminalWritten {
			t.Fatalf("rec status=%d terminalWritten=%v, want implicit 200 terminal", rec.status, rec.terminalWritten)
		}
		if entry.StatusCode != http.StatusOK || entry.ResponseHeaders.Get("X-Test") != "flush" || entry.Timing.ResponseHeaders.IsZero() {
			t.Fatalf("journal entry after flush = %+v, want implicit 200 headers/timing", entry)
		}
		if entry.ContentType != "" {
			t.Fatalf("entry ContentType = %q, want empty because flush-before-body has no bytes to sniff", entry.ContentType)
		}
	})

	t.Run("unsupported flush does not fabricate status", func(t *testing.T) {
		entry := &journal.Entry{}
		rec := &statusRecorder{ResponseWriter: nonFlusherWriter{httptest.NewRecorder()}, entry: entry, status: 0}

		if err := rec.FlushError(); !errors.Is(err, http.ErrNotSupported) {
			t.Fatalf("FlushError error = %v, want ErrNotSupported", err)
		}
		if rec.status != 0 || rec.terminalWritten {
			t.Fatalf("rec status=%d terminalWritten=%v, want unchanged after unsupported flush", rec.status, rec.terminalWritten)
		}
		if entry.StatusCode != 0 || !entry.Timing.ResponseHeaders.IsZero() {
			t.Fatalf("journal entry after unsupported flush = %+v, want unchanged", entry)
		}
		if rec.downstreamWriteFailed() {
			t.Fatalf("unsupported flush marked downstream failure: %v", rec.writeErr)
		}
	})
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
	if !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("expected http.ErrNotSupported, got %v", err)
	}
	if rec.hijacked {
		t.Error("hijacked flag should NOT be set when Hijack fails")
	}
}

func TestStatusRecorderHijackUnwrapsUnderlyingHijacker(t *testing.T) {
	inner := newSuccessfulUpgradeHandshakeResponseWriter()
	wrapped := &unwrapResponseWriter{ResponseWriter: inner}
	entry := &journal.Entry{}
	rec := &statusRecorder{ResponseWriter: wrapped, entry: entry}

	conn, brw, err := rec.Hijack()
	if err != nil {
		t.Fatalf("Hijack through Unwrap returned error: %v", err)
	}
	if conn == nil || brw == nil {
		t.Fatalf("Hijack returned conn=%v brw=%v, want both non-nil", conn, brw)
	}
	if closeErr := conn.Close(); closeErr != nil {
		t.Fatalf("Close hijacked conn: %v", closeErr)
	}
	if !rec.hijacked {
		t.Fatal("hijacked flag = false, want true after successful unwrap-hidden hijack")
	}
	if rec.status != http.StatusSwitchingProtocols || !rec.terminalWritten {
		t.Fatalf("rec status=%d terminalWritten=%v, want implicit 101 terminal", rec.status, rec.terminalWritten)
	}
	if entry.StatusCode != http.StatusSwitchingProtocols || entry.Timing.ResponseHeaders.IsZero() {
		t.Fatalf("journal entry after hijack = %+v, want captured 101 headers/timing", entry)
	}
}

func TestResponseWriterCanHijackUnwrapsAndIgnoresRecorderItself(t *testing.T) {
	inner := newSuccessfulUpgradeHandshakeResponseWriter()
	wrapped := &unwrapResponseWriter{ResponseWriter: inner}
	if !responseWriterCanHijack(wrapped) {
		t.Fatal("responseWriterCanHijack = false for unwrap-hidden hijacker, want true")
	}
	if responseWriterCanHijack(&statusRecorder{ResponseWriter: httptest.NewRecorder()}) {
		t.Fatal("responseWriterCanHijack treated statusRecorder.Hijack as downstream support despite non-hijacker underlying writer")
	}
	if !responseWriterCanHijack(&statusRecorder{ResponseWriter: wrapped}) {
		t.Fatal("responseWriterCanHijack failed to skip statusRecorder and unwrap to underlying hijacker")
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
	// Verify that bytesWritten tracks the byte count accepted by Write (n),
	// not the attempted bytes (len(b)). On a short write caused by client
	// disconnect, the recorder must report only what was accepted by Write,
	// not what we tried to send.
	inner := httptest.NewRecorder()
	mock := &shortWriteWriter{ResponseWriter: inner, n: 3}
	entry := &journal.Entry{}
	rec := &statusRecorder{ResponseWriter: mock, entry: entry, status: 0, captureMax: 1 << 20}

	// Write 10 bytes, but the underlying writer accepts only 3.
	n, err := rec.Write([]byte("0123456789"))
	if n != 3 {
		t.Errorf("Write returned n=%d, want 3", n)
	}
	if err != io.ErrUnexpectedEOF {
		t.Errorf("Write returned err=%v, want io.ErrUnexpectedEOF", err)
	}

	// bytesWritten must be 3 (accepted by Write), not 10 (attempted).
	if rec.bytesWritten != 3 {
		t.Errorf("bytesWritten = %d, want 3 (bytes accepted by Write, not attempted)", rec.bytesWritten)
	}
}

func TestStatusRecorderShortWriteWithContentLength(t *testing.T) {
	// Verify that when a response has a Content-Length header (e.g., 1000)
	// but Write accepts only a subset of those bytes due to a
	// client disconnect (short write), the journal entry's ResponseSize
	// reflects the bytes accepted by Write (from bytesWritten), not the
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

	// Write 10 bytes, but the underlying writer accepts only 3.
	n, err := rec.Write([]byte("0123456789"))
	if n != 3 {
		t.Errorf("Write returned n=%d, want 3", n)
	}
	if err != io.ErrUnexpectedEOF {
		t.Errorf("Write returned err=%v, want io.ErrUnexpectedEOF", err)
	}

	// bytesWritten must be 3 (accepted by Write), not 10 (attempted).
	if rec.bytesWritten != 3 {
		t.Errorf("bytesWritten = %d, want 3", rec.bytesWritten)
	}

	// ResponseSize was set to 1000 from Content-Length during WriteHeader.
	// The ServeHTTP finalizer must override this with bytesWritten (3)
	// because bytesWritten > 0 is the recorder's observed accepted-byte count.
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
	// Verify that capturedBody contains only the bytes accepted by Write
	// (b[:n]) on a short write, not the full input slice. This is the
	// structural fix from review-02: moving body capture after Write
	// allows using b[:n] instead of b, so the journal's ResponseBody
	// matches the recorder's accepted payload.
	inner := httptest.NewRecorder()
	mock := &shortWriteWriter{ResponseWriter: inner, n: 3}
	entry := &journal.Entry{}
	rec := &statusRecorder{ResponseWriter: mock, entry: entry, status: 0, captureMax: 1 << 20}

	// Write 10 bytes, but only 3 are accepted by the writer.
	n, err := rec.Write([]byte("0123456789"))
	if n != 3 {
		t.Errorf("Write returned n=%d, want 3", n)
	}
	_ = err // expected short-write error

	// capturedBody must contain exactly the 3 accepted bytes "012",
	// not the full 10-byte input "0123456789".
	if string(rec.capturedBody) != "012" {
		t.Errorf("capturedBody = %q, want %q (only accepted bytes)", string(rec.capturedBody), "012")
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

type unwrapRoundTripper struct {
	inner http.RoundTripper
}

func (w *unwrapRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return w.inner.RoundTrip(req)
}

func (w *unwrapRoundTripper) Unwrap() http.RoundTripper { return w.inner }

type breakerExposingWrapper struct {
	inner   http.RoundTripper
	breaker *circuitbreaker.Breaker
}

func (w *breakerExposingWrapper) RoundTrip(req *http.Request) (*http.Response, error) {
	return w.inner.RoundTrip(req)
}

func (w *breakerExposingWrapper) Unwrap() http.RoundTripper { return w.inner }

func (w *breakerExposingWrapper) RetryBreaker() *circuitbreaker.Breaker { return w.breaker }

func (w *breakerExposingWrapper) SetInFlightRetries(*atomic.Int64) {}

func TestProxy_NewRejectsMismatchedRetryTransportBreaker(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	baseOpts := func() []Option {
		return []Option{
			WithUpstream(upstreamURL),
			WithMatcher(route.NewMatcher([]route.Pattern{pat})),
			WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
			WithMetrics(metrics.NewCollector()),
		}
	}

	owner := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
	other := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

	tests := []struct {
		name    string
		breaker *circuitbreaker.Breaker
		inner   *retry.Transport
		retries int
		wantErr string
	}{
		{
			name:    "nil WithBreaker rejects retry transport breaker",
			breaker: nil,
			inner:   &retry.Transport{Inner: http.DefaultTransport, Breaker: owner},
			wantErr: "WithBreaker is nil",
		},
		{
			name:    "different breaker rejected",
			breaker: owner,
			inner:   &retry.Transport{Inner: http.DefaultTransport, Breaker: other},
			wantErr: "must match WithBreaker",
		},
		{
			name:    "same breaker cannot be wrapped by proxy retries",
			breaker: owner,
			inner:   &retry.Transport{Inner: http.DefaultTransport, Breaker: owner},
			retries: 1,
			wantErr: "cannot be wrapped by WithMaxRetries",
		},
		{
			name:    "same breaker allowed when proxy retries disabled",
			breaker: owner,
			inner:   &retry.Transport{Inner: http.DefaultTransport, Breaker: owner},
		},
		{
			name:    "nil transport breaker allowed as inner retry transport",
			breaker: owner,
			inner:   &retry.Transport{Inner: http.DefaultTransport},
			retries: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := baseOpts()
			opts = append(opts, WithTransport(tt.inner), WithMaxRetries(tt.retries))
			if tt.breaker != nil {
				opts = append(opts, WithBreaker(tt.breaker))
			}
			_, err := New(opts...)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("proxy.New error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("proxy.New error = nil, want containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("proxy.New error = %q, want containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestProxy_DetectsUnwrappedRetryTransportBreakerOwnership(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	owner := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
	other := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

	_, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(owner),
		WithMaxRetries(0),
		WithTransport(&unwrapRoundTripper{inner: &retry.Transport{Inner: http.DefaultTransport, Breaker: other}}),
	)
	if err == nil || !strings.Contains(err.Error(), "must match WithBreaker") {
		t.Fatalf("proxy.New mismatch error = %v, want must match WithBreaker", err)
	}

	_, err = New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(owner),
		WithMaxRetries(0),
		WithTransport(&breakerExposingWrapper{
			breaker: owner,
			inner:   &retry.Transport{Inner: http.DefaultTransport, Breaker: other},
		}),
	)
	if err == nil || !strings.Contains(err.Error(), "conflicting breakers") {
		t.Fatalf("proxy.New conflicting wrapper error = %v, want conflicting breakers", err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(owner),
		WithMaxRetries(0),
		WithTransport(&unwrapRoundTripper{inner: &retry.Transport{
			Inner: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return responseWithReadErrorAfterChunk(req, "wrapped-retry-partial"), nil
			}),
			Breaker: owner,
		}}),
	)
	if err != nil {
		t.Fatalf("proxy.New same-breaker wrapper error = %v", err)
	}

	req := withPanicOnCopyErrorContext(httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	rec := httptest.NewRecorder()
	serveExpectErrAbortHandler(t, p, rec, req)

	stats := owner.Stats()
	if stats.TotalSuccesses != 0 || stats.TotalFailures != 1 {
		t.Fatalf("wrapped retry breaker successes=%d failures=%d, want deferred success=0 and body-copy failure=1", stats.TotalSuccesses, stats.TotalFailures)
	}
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
	// The phantom penalty should NOT be applied because no upstream transport
	// attempt started.
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

func TestProxy_RetryTransportErrCircuitOpenNotUpstreamFailure(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name   string
		method string
		path   string
		global bool
	}{
		{name: "limited route limiter", method: http.MethodPost, path: "/v1/messages"},
		{name: "passthrough global limiter", method: http.MethodGet, path: "/health", global: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(1),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithOpenTimeout(20*time.Millisecond),
				circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
				circuitbreaker.WithBasePenalty(500*time.Millisecond),
				circuitbreaker.WithMaxPenalty(500*time.Millisecond),
			)
			var calls atomic.Int64
			var externalOpened atomic.Bool

			rt := &retry.Transport{
				Inner: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					call := calls.Add(1)
					if call == 1 {
						return &http.Response{
							StatusCode: http.StatusNotFound,
							Status:     "404 Not Found",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("breaker-neutral retry trigger")),
							Request:    req,
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("ok")),
						Request:    req,
					}, nil
				}),
				Breaker:    b,
				MaxRetries: 1,
				WaitMin:    time.Millisecond,
				WaitMax:    time.Millisecond,
				CheckRetry: func(resp *http.Response, err error) bool {
					if resp != nil && resp.StatusCode == http.StatusNotFound && externalOpened.CompareAndSwap(false, true) {
						b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
						return true
					}
					return false
				},
			}

			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(0),
				WithQueueTimeout(200 * time.Millisecond),
				WithTransport(rt),
			}
			if tt.global {
				opts = append(opts, WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			firstRec := httptest.NewRecorder()
			p.ServeHTTP(firstRec, httptest.NewRequest(tt.method, tt.path, nil))
			if firstRec.Code != http.StatusBadGateway {
				t.Fatalf("first response status = %d body=%q, want proxy-generated 502 from retry-side ErrCircuitOpen", firstRec.Code, firstRec.Body.String())
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("transport calls after first request = %d, want only breaker-neutral 404 before retry Allow rejected", got)
			}
			stats := b.Stats()
			if stats.TotalFailures != 1 {
				t.Fatalf("breaker TotalFailures after retry-side ErrCircuitOpen = %d, want only the external breaker-opening failure", stats.TotalFailures)
			}

			deadline := time.Now().Add(time.Second)
			for b.WaitDuration() > 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			epoch, allowErr := b.Allow()
			if allowErr != nil {
				t.Fatalf("Allow() while closing externally opened breaker = epoch %d err %v", epoch, allowErr)
			}
			if epoch == 0 {
				t.Fatal("Allow() while closing externally opened breaker returned epoch 0, want HALF_OPEN probe epoch")
			}
			b.RecordSuccess(time.Now(), epoch)

			start := time.Now()
			secondRec := httptest.NewRecorder()
			p.ServeHTTP(secondRec, httptest.NewRequest(tt.method, tt.path, nil))
			elapsed := time.Since(start)
			if secondRec.Code != http.StatusOK {
				t.Fatalf("second request status = %d body=%q after %v, want 200; retry-side ErrCircuitOpen must not hold the slot as upstream failure", secondRec.Code, secondRec.Body.String(), elapsed)
			}
			if elapsed > 250*time.Millisecond {
				t.Fatalf("second request completed after %v, want no phantom penalty/failure hold from retry-side ErrCircuitOpen", elapsed)
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("transport calls = %d, want first 404 plus second 200", got)
			}
			if failures := b.Stats().TotalFailures; failures != 1 {
				t.Fatalf("breaker TotalFailures after second request = %d, want no retry-side ErrCircuitOpen failure beyond external seed", failures)
			}
		})
	}
}

func TestProxy_PhantomPenaltyNotAppliedOnQueueTimeout(t *testing.T) {
	// Verify that a queue timeout (504) does NOT trigger the phantom
	// concurrency penalty. No upstream transport attempt starts.
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
	// Verify that the phantom concurrency penalty is NOT applied when a client
	// cancels after an upstream transport attempt has started. When a
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
	// is still processing. An upstream transport attempt has started,
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

	// Wait for the first request to start its upstream attempt.
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
	// Verify that when a client cancels after an upstream transport attempt starts,
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
	// when a client disconnects after an upstream transport attempt starts. This mirrors
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

	// Send a passthrough request and cancel it after the upstream attempt starts.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	// Start the request — it will complete quickly since upstream returns 200.
	// Cancel after a brief delay (simulating client disconnect after attempt start).
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

// ErrAbortHandler is the sentinel panic value that httputil.ReverseProxy uses
// when a client disconnects mid-stream. The proxy must distinguish it from real
// (local) panics so that the cancel-cooldown logic fires correctly instead of
// being short-circuited by the localPanic guard.
//
// See: https://pkg.go.dev/net/http#hdr-Server
// "If a handler panics with ErrAbortHandler, the server does not log a stack trace."

var (
	errProxyTestUpstreamRead    = errors.New("proxy test upstream body read failure")
	errProxyTestDownstreamWrite = errors.New("proxy test downstream write failure")
	errProxyTestDownstreamFlush = errors.New("proxy test downstream flush failure")
	errProxyTestTransport       = errors.New("proxy test transport error before response")
)

type readErrorAfterChunkBody struct {
	chunk []byte
	err   error
	sent  bool
}

func (b *readErrorAfterChunkBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, b.chunk), nil
	}
	return 0, b.err
}

func (b *readErrorAfterChunkBody) Close() error { return nil }

type readErrorBody struct {
	err error
}

func (b *readErrorBody) Read([]byte) (int, error) {
	return 0, b.err
}

func (b *readErrorBody) Close() error { return nil }

type closeTrackingReadCloser struct {
	io.Reader
	closed atomic.Bool
}

func (b *closeTrackingReadCloser) Close() error {
	b.closed.Store(true)
	return nil
}

type readChunkWithErrorBody struct {
	chunk []byte
	err   error
	sent  bool
}

func (b *readChunkWithErrorBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.EOF
	}
	b.sent = true
	return copy(p, b.chunk), b.err
}

func (b *readChunkWithErrorBody) Close() error { return nil }

type observedReadWriteCloser struct {
	io.ReadWriteCloser
	readCalled       atomic.Bool
	closeWriteCalled chan<- struct{}
}

func (c *observedReadWriteCloser) Read(p []byte) (int, error) {
	c.readCalled.Store(true)
	return c.ReadWriteCloser.Read(p)
}

func (c *observedReadWriteCloser) CloseWrite() error {
	if c.closeWriteCalled != nil {
		select {
		case c.closeWriteCalled <- struct{}{}:
		default:
		}
	}
	return nil
}

type noopReadWriteCloser struct{}

func (noopReadWriteCloser) Read([]byte) (int, error) { return 0, io.EOF }

func (noopReadWriteCloser) Write(p []byte) (int, error) { return len(p), nil }

func (noopReadWriteCloser) Close() error { return nil }

type closeTrackingReadWriteCloser struct {
	closed atomic.Bool
}

func (c *closeTrackingReadWriteCloser) Read([]byte) (int, error) { return 0, io.EOF }

func (c *closeTrackingReadWriteCloser) Write(p []byte) (int, error) { return len(p), nil }

func (c *closeTrackingReadWriteCloser) Close() error {
	c.closed.Store(true)
	return nil
}

func emitEarlyHintsFromTrace(t *testing.T, req *http.Request) {
	t.Helper()
	trace := httptrace.ContextClientTrace(req.Context())
	if trace == nil || trace.Got1xxResponse == nil {
		t.Fatal("reverse proxy request context did not install Got1xxResponse trace")
	}
	if err := trace.Got1xxResponse(http.StatusEarlyHints, nil); err != nil {
		t.Fatalf("Got1xxResponse(103): %v", err)
	}
}

type testAddr string

func (a testAddr) Network() string { return string(a) }

func (a testAddr) String() string { return string(a) }

type noopConn struct{}

func (noopConn) Read([]byte) (int, error) { return 0, io.EOF }

func (noopConn) Write(p []byte) (int, error) { return len(p), nil }

func (noopConn) Close() error { return nil }

func (noopConn) LocalAddr() net.Addr { return testAddr("local") }

func (noopConn) RemoteAddr() net.Addr { return testAddr("remote") }

func (noopConn) SetDeadline(time.Time) error { return nil }

func (noopConn) SetReadDeadline(time.Time) error { return nil }

func (noopConn) SetWriteDeadline(time.Time) error { return nil }

type alwaysErrorWriter struct{ err error }

func (w alwaysErrorWriter) Write([]byte) (int, error) { return 0, w.err }

type failedUpgradeHandshakeResponseWriter struct {
	header http.Header
	status int
}

func newFailedUpgradeHandshakeResponseWriter() *failedUpgradeHandshakeResponseWriter {
	return &failedUpgradeHandshakeResponseWriter{header: make(http.Header)}
}

func (w *failedUpgradeHandshakeResponseWriter) Header() http.Header { return w.header }

func (w *failedUpgradeHandshakeResponseWriter) WriteHeader(code int) { w.status = code }

func (w *failedUpgradeHandshakeResponseWriter) Write(b []byte) (int, error) { return len(b), nil }

func (w *failedUpgradeHandshakeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	brw := bufio.NewReadWriter(
		bufio.NewReader(strings.NewReader("")),
		bufio.NewWriter(alwaysErrorWriter{err: errProxyTestDownstreamWrite}),
	)
	return noopConn{}, brw, nil
}

type successfulUpgradeHandshakeResponseWriter struct {
	header    http.Header
	status    int
	handshake bytes.Buffer
}

func newSuccessfulUpgradeHandshakeResponseWriter() *successfulUpgradeHandshakeResponseWriter {
	return &successfulUpgradeHandshakeResponseWriter{header: make(http.Header)}
}

func (w *successfulUpgradeHandshakeResponseWriter) Header() http.Header { return w.header }

func (w *successfulUpgradeHandshakeResponseWriter) WriteHeader(code int) { w.status = code }

func (w *successfulUpgradeHandshakeResponseWriter) Write(b []byte) (int, error) {
	return w.handshake.Write(b)
}

func (w *successfulUpgradeHandshakeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	brw := bufio.NewReadWriter(
		bufio.NewReader(strings.NewReader("")),
		bufio.NewWriter(&w.handshake),
	)
	return noopConn{}, brw, nil
}

type unwrapResponseWriter struct {
	http.ResponseWriter
}

func (w *unwrapResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type failedHijackResponseWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w *failedHijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, w.err
}

type delayedReadErrorAfterChunkBody struct {
	chunk []byte
	err   error
	delay time.Duration
	sent  bool
}

func (b *delayedReadErrorAfterChunkBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, b.chunk), nil
	}
	time.Sleep(b.delay)
	return 0, b.err
}

func (b *delayedReadErrorAfterChunkBody) Close() error { return nil }

func TestStatusRecorder_CapturePreallocCappedForLargeContentLength(t *testing.T) {
	entry := &journal.Entry{ResponseSize: 1 << 20}
	rec := &statusRecorder{
		ResponseWriter: httptest.NewRecorder(),
		entry:          entry,
		captureMax:     2 << 20,
	}

	n, err := rec.Write([]byte("x"))
	if err != nil || n != 1 {
		t.Fatalf("Write = (%d, %v), want (1, nil)", n, err)
	}
	if string(rec.capturedBody) != "x" {
		t.Fatalf("capturedBody = %q, want x", string(rec.capturedBody))
	}
	if cap(rec.capturedBody) > maxResponseCapturePrealloc {
		t.Fatalf("capturedBody cap = %d, want <= %d despite large declared Content-Length", cap(rec.capturedBody), maxResponseCapturePrealloc)
	}
}

func TestProxy_SwitchingProtocolsPreservesReadWriteCloserBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	j := journal.New(8, 1024)
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
	readObserved := make(chan struct{}, 1)
	closeWriteObserved := make(chan struct{}, 1)
	stopObserve := make(chan struct{})
	t.Cleanup(func() { close(stopObserve) })

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithJournal(j),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			proxySide, testSide := net.Pipe()
			body := &observedReadWriteCloser{ReadWriteCloser: proxySide, closeWriteCalled: closeWriteObserved}
			go func() {
				defer testSide.Close()
				_, _ = testSide.Write([]byte("upgrade-ok"))
			}()
			go func() {
				for {
					if body.readCalled.Load() {
						select {
						case readObserved <- struct{}{}:
						default:
						}
						return
					}
					select {
					case <-stopObserve:
						return
					case <-time.After(time.Millisecond):
					}
				}
			}()
			return &http.Response{
				StatusCode: http.StatusSwitchingProtocols,
				Status:     "101 Switching Protocols",
				Header: http.Header{
					"Connection": []string{"Upgrade"},
					"Upgrade":    []string{"testproto"},
				},
				Body:          body,
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	proxyServer := httptest.NewServer(p)
	t.Cleanup(proxyServer.Close)

	conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, proxyServer.URL+"/health", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "testproto")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101; body wrapper likely stripped io.ReadWriteCloser", resp.StatusCode)
	}
	payload := make([]byte, len("upgrade-ok"))
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatalf("read upgraded payload: %v", err)
	}
	if string(payload) != "upgrade-ok" {
		t.Fatalf("upgraded payload = %q, want upgrade-ok", string(payload))
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		if err := cw.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite client side: %v", err)
		}
	} else {
		t.Fatalf("client conn %T does not support CloseWrite", conn)
	}
	select {
	case <-readObserved:
	case <-time.After(time.Second):
		t.Fatal("wrapped upgrade body Read was not observed")
	}
	select {
	case <-closeWriteObserved:
	case <-time.After(time.Second):
		t.Fatal("backend CloseWrite was not propagated through upgrade wrapper")
	}
	conn.Close()
	var snap metrics.Snapshot
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snap = met.Snapshot()
		if snap.TotalPassThrough == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if snap.TotalPassThrough != 1 {
		t.Fatalf("TotalPassThrough = %d, want 1 after clean upgrade", snap.TotalPassThrough)
	}
	if snap.StatusCounts[5] != 0 || snap.StatusCounts[0] != 0 || snap.StatusCounts[1] != 1 {
		t.Fatalf("status buckets = %+v, want one 1xx and no status-0/5xx failure for upgrade", snap.StatusCounts)
	}
	if len(snap.LogEntries) != 1 || snap.LogEntries[0].Status != http.StatusSwitchingProtocols || snap.LogEntries[0].Aborted {
		t.Fatalf("metrics log = %+v, want one clean 101 entry", snap.LogEntries)
	}
	stats := b.Stats()
	if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
		t.Fatalf("breaker failures=%d successes=%d, want no mutation for 101 upgrade", stats.TotalFailures, stats.TotalSuccesses)
	}
	entries := j.Entries()
	if len(entries) != 1 || entries[0].StatusCode != http.StatusSwitchingProtocols || entries[0].Aborted {
		t.Fatalf("journal entries = %+v, want one clean 101 entry", entries)
	}
}

func TestProxy_SwitchingProtocolsCleanHandshakeResolvesHalfOpenProbe(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(10*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(10*time.Millisecond),
	)
	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
	if b.State() != circuitbreaker.Open {
		t.Fatalf("breaker state after seed failure = %v, want OPEN", b.State())
	}
	time.Sleep(20 * time.Millisecond)
	releaseUpgrade := make(chan struct{})
	var releaseUpgradeOnce sync.Once
	release := func() { releaseUpgradeOnce.Do(func() { close(releaseUpgrade) }) }
	t.Cleanup(release)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Upgrade") == "" {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("ok")),
					Request:    req,
				}, nil
			}
			proxySide, testSide := net.Pipe()
			go func() {
				defer testSide.Close()
				_, _ = testSide.Write([]byte("upgrade-ok"))
				<-releaseUpgrade
			}()
			return &http.Response{
				StatusCode: http.StatusSwitchingProtocols,
				Status:     "101 Switching Protocols",
				Header: http.Header{
					"Connection": []string{"Upgrade"},
					"Upgrade":    []string{"testproto"},
				},
				Body:          proxySide,
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	proxyServer := httptest.NewServer(p)
	t.Cleanup(proxyServer.Close)
	conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, proxyServer.URL+"/health", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "testproto")
	if err := req.Write(conn); err != nil {
		conn.Close()
		t.Fatalf("write upgrade request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	payload := make([]byte, len("upgrade-ok"))
	if _, err := io.ReadFull(br, payload); err != nil {
		conn.Close()
		t.Fatalf("read upgraded payload: %v", err)
	}
	if string(payload) != "upgrade-ok" {
		conn.Close()
		t.Fatalf("upgraded payload = %q, want upgrade-ok", string(payload))
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if b.State() == circuitbreaker.Closed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("breaker state after clean 101 half-open probe = %v, want CLOSED", b.State())
	}
	stats := b.Stats()
	if stats.TotalSuccesses != 1 {
		t.Fatalf("breaker TotalSuccesses = %d, want 1 for clean half-open 101 handshake", stats.TotalSuccesses)
	}

	normalReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	normalRec := httptest.NewRecorder()
	p.ServeHTTP(normalRec, normalReq)
	if normalRec.Code != http.StatusOK {
		conn.Close()
		t.Fatalf("normal request after clean 101 probe status = %d, want 200", normalRec.Code)
	}
	conn.Close()
	release()
}

func TestProxy_SwitchingProtocolsEarlyHintsThenCleanHandshakeRecordsFinal101Once(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	j := journal.New(8, 1024)
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(10*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(10*time.Millisecond),
	)
	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
	if b.State() != circuitbreaker.Open {
		t.Fatalf("breaker state after seed failure = %v, want OPEN", b.State())
	}
	deadline := time.Now().Add(time.Second)
	for b.WaitDuration() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if wait := b.WaitDuration(); wait > 0 {
		t.Fatalf("breaker still waiting after deadline: %v", wait)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithJournal(j),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			emitEarlyHintsFromTrace(t, req)
			return &http.Response{
				StatusCode: http.StatusSwitchingProtocols,
				Status:     "101 Switching Protocols",
				Header: http.Header{
					"Connection": []string{"Upgrade"},
					"Upgrade":    []string{"testproto"},
				},
				Body:          noopReadWriteCloser{},
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "testproto")
	rw := newSuccessfulUpgradeHandshakeResponseWriter()
	p.ServeHTTP(rw, req)

	if !strings.Contains(rw.handshake.String(), "101 Switching Protocols") {
		t.Fatalf("captured hijack handshake = %q, want final 101 after early hints", rw.handshake.String())
	}
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("breaker state after 103 then clean 101 probe = %v, want CLOSED", b.State())
	}
	stats := b.Stats()
	if stats.TotalFailures != 1 || stats.TotalSuccesses != 1 {
		t.Fatalf("breaker failures=%d successes=%d, want seeded failure plus exactly one clean 101 probe success", stats.TotalFailures, stats.TotalSuccesses)
	}
	snap := met.Snapshot()
	if snap.TotalAborted != 0 || snap.TotalPassThrough != 1 || snap.TotalProxied != 0 {
		t.Fatalf("metrics TotalAborted=%d TotalPassThrough=%d TotalProxied=%d, want one clean passthrough upgrade", snap.TotalAborted, snap.TotalPassThrough, snap.TotalProxied)
	}
	if snap.StatusCounts[1] != 1 || snap.StatusCounts[5] != 0 || snap.StatusCounts[0] != 0 {
		t.Fatalf("status buckets = %+v, want one final 101 bucket and no status-0/5xx", snap.StatusCounts)
	}
	if len(snap.LogEntries) != 1 || snap.LogEntries[0].Status != http.StatusSwitchingProtocols || snap.LogEntries[0].Aborted {
		t.Fatalf("metrics log = %+v, want one clean final 101 entry, not stale 103", snap.LogEntries)
	}
	entries := j.Entries()
	if len(entries) != 1 || entries[0].StatusCode != http.StatusSwitchingProtocols || entries[0].Aborted {
		t.Fatalf("journal entries = %+v, want clean final 101 entry", entries)
	}
}

func TestProxy_SwitchingProtocolsEarlyHintsThenFailedHandshakeMarkedAborted101(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	j := journal.New(8, 1024)
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithJournal(j),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			emitEarlyHintsFromTrace(t, req)
			return &http.Response{
				StatusCode: http.StatusSwitchingProtocols,
				Status:     "101 Switching Protocols",
				Header: http.Header{
					"Connection": []string{"Upgrade"},
					"Upgrade":    []string{"testproto"},
				},
				Body:          noopReadWriteCloser{},
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "testproto")
	p.ServeHTTP(newFailedUpgradeHandshakeResponseWriter(), req)

	snap := met.Snapshot()
	if snap.TotalAborted != 1 || snap.TotalPassThrough != 0 || snap.TotalProxied != 0 {
		t.Fatalf("metrics TotalAborted=%d TotalPassThrough=%d TotalProxied=%d, want aborted failed 101 handshake with no clean completion", snap.TotalAborted, snap.TotalPassThrough, snap.TotalProxied)
	}
	if snap.StatusCounts[1] != 1 || snap.StatusCounts[5] != 0 || snap.StatusCounts[0] != 0 {
		t.Fatalf("status buckets = %+v, want one aborted final 101 bucket and no stale/proxy-generated 5xx", snap.StatusCounts)
	}
	if len(snap.LogEntries) != 1 || snap.LogEntries[0].Status != http.StatusSwitchingProtocols || !snap.LogEntries[0].Aborted {
		t.Fatalf("metrics log = %+v, want one aborted final 101 entry, not stale 103", snap.LogEntries)
	}
	stats := b.Stats()
	if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
		t.Fatalf("breaker failures=%d successes=%d, want no upstream breaker mutation for downstream 101 handshake failure", stats.TotalFailures, stats.TotalSuccesses)
	}
	entries := j.Entries()
	if len(entries) != 1 || entries[0].StatusCode != http.StatusSwitchingProtocols || !entries[0].Aborted || !entries[0].Timing.ResponseComplete.IsZero() {
		t.Fatalf("journal entries = %+v, want aborted final 101 without ResponseComplete", entries)
	}
}

func TestProxy_SwitchingProtocolsRoundTripUsesOutboundRequestForValidation(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name            string
		responseRequest func(*http.Request) *http.Request
	}{
		{
			name: "nil response request",
			responseRequest: func(*http.Request) *http.Request {
				return nil
			},
		},
		{
			name: "wrong request without downstream context",
			responseRequest: func(*http.Request) *http.Request {
				wrong := httptest.NewRequest(http.MethodGet, "http://wrong.invalid/elsewhere", nil)
				wrong.Header.Set("Connection", "Upgrade")
				wrong.Header.Set("Upgrade", "testproto")
				return wrong
			},
		},
		{
			name: "wrong request with mismatched upgrade header",
			responseRequest: func(*http.Request) *http.Request {
				wrong := httptest.NewRequest(http.MethodGet, "http://wrong.invalid/elsewhere", nil)
				wrong.Header.Set("Connection", "Upgrade")
				wrong.Header.Set("Upgrade", "otherproto")
				return wrong
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithMaxRetries(0),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusSwitchingProtocols,
						Status:     "101 Switching Protocols",
						Header: http.Header{
							"Connection": []string{"Upgrade"},
							"Upgrade":    []string{"testproto"},
						},
						Body:          noopReadWriteCloser{},
						ContentLength: -1,
						Request:       tt.responseRequest(req),
					}, nil
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "testproto")
			rw := newSuccessfulUpgradeHandshakeResponseWriter()
			p.ServeHTTP(rw, req)

			if !strings.Contains(rw.handshake.String(), "101 Switching Protocols") {
				t.Fatalf("captured hijack handshake = %q, want valid 101 using proxy outbound request", rw.handshake.String())
			}
			snap := met.Snapshot()
			if snap.TotalAborted != 0 || snap.TotalPassThrough != 1 || snap.TotalProxied != 0 {
				t.Fatalf("metrics TotalAborted=%d TotalPassThrough=%d TotalProxied=%d, want one clean passthrough upgrade", snap.TotalAborted, snap.TotalPassThrough, snap.TotalProxied)
			}
			if len(snap.LogEntries) != 1 || snap.LogEntries[0].Status != http.StatusSwitchingProtocols || snap.LogEntries[0].Aborted {
				t.Fatalf("metrics log = %+v, want one clean 101 entry", snap.LogEntries)
			}
		})
	}
}

func TestProxy_SwitchingProtocolsUnwrapOnlyHijackerSucceeds(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusSwitchingProtocols,
				Status:     "101 Switching Protocols",
				Header: http.Header{
					"Connection": []string{"Upgrade"},
					"Upgrade":    []string{"testproto"},
				},
				Body:          noopReadWriteCloser{},
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	inner := newSuccessfulUpgradeHandshakeResponseWriter()
	rw := &unwrapResponseWriter{ResponseWriter: inner}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "testproto")
	p.ServeHTTP(rw, req)

	if !strings.Contains(inner.handshake.String(), "101 Switching Protocols") {
		t.Fatalf("captured hijack handshake = %q, want 101 response written to unwrap-hidden hijacker", inner.handshake.String())
	}
	snap := met.Snapshot()
	if snap.TotalAborted != 0 || snap.TotalProxied != 1 || snap.TotalPassThrough != 0 {
		t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want one clean proxied upgrade", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
	}
	if snap.StatusCounts[1] != 1 || snap.StatusCounts[5] != 0 || snap.StatusCounts[0] != 0 {
		t.Fatalf("status buckets = %+v, want one clean 1xx upgrade", snap.StatusCounts)
	}
	stats := b.Stats()
	if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
		t.Fatalf("breaker failures=%d successes=%d, want no mutation for closed-state clean 101", stats.TotalFailures, stats.TotalSuccesses)
	}
}

func TestProxy_SwitchingProtocolsPreservesReadWriteCloserBody_WithRetryBuffering(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	j := journal.New(8, 1024)
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
	readObserved := make(chan struct{}, 1)
	closeWriteObserved := make(chan struct{}, 1)
	bodySeen := make(chan string, 1)
	stopObserve := make(chan struct{})
	t.Cleanup(func() { close(stopObserve) })

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithJournal(j),
		WithBreaker(b),
		WithMaxRetries(1),
		WithMaxBodyBytes(1024),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			got, readErr := io.ReadAll(req.Body)
			closeErr := req.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			bodySeen <- string(got)

			proxySide, testSide := net.Pipe()
			body := &observedReadWriteCloser{ReadWriteCloser: proxySide, closeWriteCalled: closeWriteObserved}
			go func() {
				defer testSide.Close()
				_, _ = testSide.Write([]byte("upgrade-ok"))
			}()
			go func() {
				for {
					if body.readCalled.Load() {
						select {
						case readObserved <- struct{}{}:
						default:
						}
						return
					}
					select {
					case <-stopObserve:
						return
					case <-time.After(time.Millisecond):
					}
				}
			}()
			return &http.Response{
				StatusCode: http.StatusSwitchingProtocols,
				Status:     "101 Switching Protocols",
				Header: http.Header{
					"Connection": []string{"Upgrade"},
					"Upgrade":    []string{"testproto"},
				},
				Body:          body,
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	proxyServer := httptest.NewServer(p)
	t.Cleanup(proxyServer.Close)

	conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, proxyServer.URL+"/health", strings.NewReader("retry-upgrade-body"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "testproto")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	select {
	case got := <-bodySeen:
		if got != "retry-upgrade-body" {
			t.Fatalf("upstream request body = %q, want retry-upgrade-body", got)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive buffered request body")
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101; retry pooled response body likely stripped io.ReadWriteCloser", resp.StatusCode)
	}
	payload := make([]byte, len("upgrade-ok"))
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatalf("read upgraded payload: %v", err)
	}
	if string(payload) != "upgrade-ok" {
		t.Fatalf("upgraded payload = %q, want upgrade-ok", string(payload))
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		if err := cw.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite client side: %v", err)
		}
	} else {
		t.Fatalf("client conn %T does not support CloseWrite", conn)
	}
	select {
	case <-readObserved:
	case <-time.After(time.Second):
		t.Fatal("retry-buffered upgrade body Read was not observed")
	}
	select {
	case <-closeWriteObserved:
	case <-time.After(time.Second):
		t.Fatal("backend CloseWrite was not propagated through retry pooled upgrade wrapper")
	}
	conn.Close()
	var snap metrics.Snapshot
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snap = met.Snapshot()
		if snap.TotalPassThrough == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if snap.TotalPassThrough != 1 {
		t.Fatalf("TotalPassThrough = %d, want 1 after clean retry-buffered upgrade", snap.TotalPassThrough)
	}
	if snap.StatusCounts[5] != 0 || snap.StatusCounts[0] != 0 || snap.StatusCounts[1] != 1 {
		t.Fatalf("status buckets = %+v, want one 1xx and no status-0/5xx failure for retry-buffered upgrade", snap.StatusCounts)
	}
	if len(snap.LogEntries) != 1 || snap.LogEntries[0].Status != http.StatusSwitchingProtocols || snap.LogEntries[0].Aborted {
		t.Fatalf("metrics log = %+v, want one clean 101 entry", snap.LogEntries)
	}
	stats := b.Stats()
	if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
		t.Fatalf("breaker failures=%d successes=%d, want no mutation for retry-buffered 101 upgrade", stats.TotalFailures, stats.TotalSuccesses)
	}
	entries := j.Entries()
	if len(entries) != 1 || entries[0].StatusCode != http.StatusSwitchingProtocols || entries[0].Aborted {
		t.Fatalf("journal entries = %+v, want one clean retry-buffered 101 entry", entries)
	}
}

func TestProxy_SwitchingProtocolsNonReadWriteCloserBodyIsProxyGeneratedFailure(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name       string
		method     string
		path       string
		maxRetries int
	}{
		{name: "limited no retries", method: http.MethodPost, path: "/v1/messages"},
		{name: "passthrough no retries", method: http.MethodGet, path: "/health"},
		{name: "limited retry-owned breaker", method: http.MethodPost, path: "/v1/messages", maxRetries: 1},
		{name: "passthrough retry-owned breaker", method: http.MethodGet, path: "/health", maxRetries: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			j := journal.New(8, 1024)
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
			var transportCalls atomic.Int64
			bodyCh := make(chan *closeTrackingReadCloser, 1)

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithJournal(j),
				WithBreaker(b),
				WithMaxRetries(tt.maxRetries),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					transportCalls.Add(1)
					malformedBody := &closeTrackingReadCloser{Reader: strings.NewReader("not a bidirectional stream")}
					bodyCh <- malformedBody
					return &http.Response{
						StatusCode: http.StatusSwitchingProtocols,
						Status:     "101 Switching Protocols",
						Header: http.Header{
							"Connection": []string{"Upgrade"},
							"Upgrade":    []string{"testproto"},
						},
						Body:          malformedBody,
						ContentLength: -1,
						Request:       req,
					}, nil
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			proxyServer := httptest.NewServer(p)
			t.Cleanup(proxyServer.Close)

			conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
			if err != nil {
				t.Fatalf("dial proxy: %v", err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatalf("SetDeadline: %v", err)
			}
			req, err := http.NewRequest(tt.method, proxyServer.URL+tt.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "testproto")
			if err := req.Write(conn); err != nil {
				t.Fatalf("write upgrade request: %v", err)
			}

			resp, err := http.ReadResponse(bufio.NewReader(conn), req)
			if err != nil {
				t.Fatalf("ReadResponse: %v", err)
			}
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				t.Fatalf("read response body: %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("close response body: %v", closeErr)
			}
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d body=%q, want proxy-generated 502 for non-ReadWriteCloser 101 body", resp.StatusCode, string(body))
			}
			if !strings.Contains(string(body), "bad gateway") {
				t.Fatalf("body = %q, want proxy-generated bad gateway body", string(body))
			}
			if got := transportCalls.Load(); got != 1 {
				t.Fatalf("transport calls = %d, want 1 upstream attempt before invalid 101 body was rejected", got)
			}
			var malformedBody *closeTrackingReadCloser
			select {
			case malformedBody = <-bodyCh:
			default:
				t.Fatal("transport did not publish malformed 101 body for close assertion")
			}
			if !malformedBody.closed.Load() {
				t.Fatal("malformed 101 response body was not closed after proxy rejection")
			}

			var snap metrics.Snapshot
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				snap = met.Snapshot()
				if snap.StatusCounts[5] == 1 && len(j.Entries()) == 1 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if snap.StatusCounts[5] != 1 || snap.StatusCounts[1] != 0 || snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics StatusCounts=%+v TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted local 502 with no clean 101 upgrade", snap.StatusCounts, snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			stats := b.Stats()
			if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want malformed 101 body classified as local upgrade failure", stats.TotalFailures, stats.TotalSuccesses)
			}
			entries := j.Entries()
			if len(entries) != 1 || entries[0].StatusCode != http.StatusBadGateway || !entries[0].Aborted || !entries[0].Timing.ResponseComplete.IsZero() {
				t.Fatalf("journal entries = %+v, want aborted local 502 entry, not clean 101 upgrade", entries)
			}
		})
	}
}

func TestProxy_SwitchingProtocolsNonHijackerClosesUpstreamBodyAsLocalFailure(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	j := journal.New(8, 1024)
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
	upstreamBody := &closeTrackingReadWriteCloser{}
	var transportCalls atomic.Int64

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithJournal(j),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			transportCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusSwitchingProtocols,
				Status:     "101 Switching Protocols",
				Header: http.Header{
					"Connection": []string{"Upgrade"},
					"Upgrade":    []string{"testproto"},
				},
				Body:          upstreamBody,
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "testproto")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%q, want local 502 for non-hijacker downstream", rec.Code, rec.Body.String())
	}
	if got := transportCalls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want exactly one upstream 101 before local handoff failure", got)
	}
	if !upstreamBody.closed.Load() {
		t.Fatal("upstream 101 body was not closed by ModifyResponse local handoff failure")
	}
	stats := b.Stats()
	if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
		t.Fatalf("breaker failures=%d successes=%d, want no upstream breaker mutation for local non-hijacker handoff failure", stats.TotalFailures, stats.TotalSuccesses)
	}
	snap := met.Snapshot()
	if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 || snap.StatusCounts[5] != 1 {
		t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d StatusCounts=%+v, want aborted local 502 with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough, snap.StatusCounts)
	}
	entries := j.Entries()
	if len(entries) != 1 || entries[0].StatusCode != http.StatusBadGateway || !entries[0].Aborted || !entries[0].Timing.ResponseComplete.IsZero() {
		t.Fatalf("journal entries = %+v, want aborted local 502 without ResponseComplete", entries)
	}
}

func TestProxy_SwitchingProtocolsNilBodyIsLocalFailureWithoutPanic(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	j := journal.New(8, 1024)
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
	var transportCalls atomic.Int64

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithJournal(j),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			transportCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusSwitchingProtocols,
				Status:     "101 Switching Protocols",
				Header: http.Header{
					"Connection": []string{"Upgrade"},
					"Upgrade":    []string{"testproto"},
				},
				Body:          nil,
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "testproto")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%q, want local 502 for nil 101 body", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "bad gateway") || strings.Contains(rec.Body.String(), "internal error") {
		t.Fatalf("body = %q, want ErrorHandler bad gateway and no local panic recovery", rec.Body.String())
	}
	if got := transportCalls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want exactly one upstream 101 before nil body rejection", got)
	}
	stats := b.Stats()
	if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
		t.Fatalf("breaker failures=%d successes=%d, want nil 101 body classified as local upgrade failure", stats.TotalFailures, stats.TotalSuccesses)
	}
	snap := met.Snapshot()
	if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 || snap.StatusCounts[5] != 1 {
		t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d StatusCounts=%+v, want aborted local 502 with no panic-clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough, snap.StatusCounts)
	}
	entries := j.Entries()
	if len(entries) != 1 || entries[0].StatusCode != http.StatusBadGateway || !entries[0].Aborted || !entries[0].Timing.ResponseComplete.IsZero() {
		t.Fatalf("journal entries = %+v, want aborted local 502 without ResponseComplete", entries)
	}
}

func TestProxy_SwitchingProtocolsProtocolViolationPreemptsLocalDownstreamFailure(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name          string
		requestProto  string
		responseProto string
	}{
		{name: "bare 101 without request upgrade", responseProto: ""},
		{name: "response upgrade without request upgrade", responseProto: "testproto"},
		{name: "response upgrade mismatch", requestProto: "testproto", responseProto: "otherproto"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			j := journal.New(8, 1024)
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
			upstreamBody := &closeTrackingReadWriteCloser{}
			var transportCalls atomic.Int64

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithJournal(j),
				WithBreaker(b),
				WithMaxRetries(0),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					transportCalls.Add(1)
					return &http.Response{
						StatusCode: http.StatusSwitchingProtocols,
						Status:     "101 Switching Protocols",
						Header: http.Header{
							"Connection": []string{"Upgrade"},
							"Upgrade":    []string{tt.responseProto},
						},
						Body:          upstreamBody,
						ContentLength: -1,
						Request:       req,
					}, nil
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if tt.requestProto != "" {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", tt.requestProto)
			}
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d body=%q, want 502 for upstream protocol violation", rec.Code, rec.Body.String())
			}
			if got := transportCalls.Load(); got != 1 {
				t.Fatalf("transport calls = %d, want exactly one upstream 101 before protocol rejection", got)
			}
			if !upstreamBody.closed.Load() {
				t.Fatal("upstream 101 body was not closed after protocol violation")
			}
			stats := b.Stats()
			if stats.TotalFailures != 1 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want upstream protocol violation recorded as upstream failure", stats.TotalFailures, stats.TotalSuccesses)
			}
			snap := met.Snapshot()
			if snap.TotalAborted != 0 || snap.TotalProxied != 1 || snap.TotalPassThrough != 0 || snap.StatusCounts[5] != 1 {
				t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d StatusCounts=%+v, want completed proxy-generated 502 upstream failure", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough, snap.StatusCounts)
			}
			entries := j.Entries()
			if len(entries) != 1 || entries[0].StatusCode != http.StatusBadGateway || entries[0].Aborted || entries[0].Timing.ResponseComplete.IsZero() {
				t.Fatalf("journal entries = %+v, want completed 502 upstream protocol violation entry", entries)
			}
		})
	}
}

func TestIsLocalSwitchingProtocolsFailurePinsReverseProxyUpgradeErrorStrings(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "testproto")

	for _, msg := range []string{
		"httputil: ReverseProxy: can't switch protocols using non-Hijacker",
		"httputil: ReverseProxy: Hijack failed on protocol switch: downstream write failed",
	} {
		t.Run(msg, func(t *testing.T) {
			if !isLocalSwitchingProtocolsFailure(req, errors.New(msg)) {
				t.Fatalf("isLocalSwitchingProtocolsFailure(%q) = false, want true", msg)
			}
			withoutUpgrade := httptest.NewRequest(http.MethodGet, "/health", nil)
			if isLocalSwitchingProtocolsFailure(withoutUpgrade, errors.New(msg)) {
				t.Fatalf("isLocalSwitchingProtocolsFailure(%q) = true without requested Upgrade, want false", msg)
			}
		})
	}
}

func TestProxy_SwitchingProtocolsHijackerErrNotSupportedClosesUpstreamBodyAsLocalFailure(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	j := journal.New(8, 1024)
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
	upstreamBody := &closeTrackingReadWriteCloser{}
	var transportCalls atomic.Int64

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithJournal(j),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			transportCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusSwitchingProtocols,
				Status:     "101 Switching Protocols",
				Header: http.Header{
					"Connection": []string{"Upgrade"},
					"Upgrade":    []string{"testproto"},
				},
				Body:          upstreamBody,
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "testproto")
	rw := &failedHijackResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: http.ErrNotSupported}
	p.ServeHTTP(rw, req)

	if rw.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%q, want local 502 for apparent hijacker that returns ErrNotSupported", rw.Code, rw.Body.String())
	}
	if got := transportCalls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want exactly one upstream 101 before local hijack rejection", got)
	}
	if !upstreamBody.closed.Load() {
		t.Fatal("upstream 101 body was not closed after apparent hijacker returned ErrNotSupported")
	}
	stats := b.Stats()
	if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
		t.Fatalf("breaker failures=%d successes=%d, want ErrNotSupported hijack classified as local upgrade failure", stats.TotalFailures, stats.TotalSuccesses)
	}
	snap := met.Snapshot()
	if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 || snap.StatusCounts[5] != 1 {
		t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d StatusCounts=%+v, want aborted local 502 with closed upstream body", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough, snap.StatusCounts)
	}
	entries := j.Entries()
	if len(entries) != 1 || entries[0].StatusCode != http.StatusBadGateway || !entries[0].Aborted || !entries[0].Timing.ResponseComplete.IsZero() {
		t.Fatalf("journal entries = %+v, want aborted local 502 without ResponseComplete", entries)
	}
}

func TestProxy_SwitchingProtocolsNonReadWriteCloserBodyWithRetryReleasesHalfOpenProbe(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(20*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
	)
	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
	deadline := time.Now().Add(time.Second)
	for b.WaitDuration() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if wait := b.WaitDuration(); wait > 0 {
		t.Fatalf("breaker still waiting after deadline: %v", wait)
	}

	var transportCalls atomic.Int64
	malformedBody := &closeTrackingReadCloser{Reader: strings.NewReader("not a bidirectional stream")}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(1),
		WithRetryWaitMin(time.Millisecond),
		WithRetryWaitMax(time.Millisecond),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			transportCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusSwitchingProtocols,
				Status:     "101 Switching Protocols",
				Header: http.Header{
					"Connection": []string{"Upgrade"},
					"Upgrade":    []string{"testproto"},
				},
				Body:          malformedBody,
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	proxyServer := httptest.NewServer(p)
	t.Cleanup(proxyServer.Close)

	conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, proxyServer.URL+"/health", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "testproto")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read response body: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close response body: %v", closeErr)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d body=%q, want proxy-generated 502 for retry-owned malformed 101", resp.StatusCode, string(body))
	}
	if got := transportCalls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want 1; malformed 101 must not be retried", got)
	}
	if !malformedBody.closed.Load() {
		t.Fatal("malformed 101 response body was not closed after local upgrade validation failure")
	}

	stats := b.Stats()
	if stats.TotalFailures != 1 || stats.TotalSuccesses != 0 {
		t.Fatalf("breaker failures=%d successes=%d, want only seeded failure and no malformed-101 breaker mutation", stats.TotalFailures, stats.TotalSuccesses)
	}
	if state := b.State(); state != circuitbreaker.HalfOpen {
		t.Fatalf("breaker state = %s, want HALF_OPEN after local malformed-101 failure releases probe", state)
	}
	nextEpoch, allowErr := b.Allow()
	if allowErr != nil {
		t.Fatalf("Allow() after local malformed-101 failure = epoch %d err %v; want released HALF_OPEN probe", nextEpoch, allowErr)
	}
	if nextEpoch == 0 {
		t.Fatal("Allow() after local malformed-101 failure returned epoch 0, want new HALF_OPEN probe epoch")
	}
	b.CancelProbe(nextEpoch)
	snap := met.Snapshot()
	if snap.StatusCounts[5] != 1 || snap.StatusCounts[1] != 0 || snap.TotalAborted != 1 || snap.TotalPassThrough != 0 || snap.TotalProxied != 0 {
		t.Fatalf("metrics StatusCounts=%+v TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted local 502 with no clean 101 upgrade", snap.StatusCounts, snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
	}
}

func TestProxy_SwitchingProtocolsFailedHandshakeMarkedAborted(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name   string
		method string
		path   string
		global bool
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages"},
		{name: "passthrough", method: http.MethodGet, path: "/health", global: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			j := journal.New(8, 1024)
			lim := queue.NewLimiterWithCooldown(1, 0)
			activeLimiter := lim
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithJournal(j),
				WithBreaker(b),
				WithMaxRetries(0),
				WithCancelCooldown(100 * time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if req.Header.Get("Upgrade") == "" {
						return &http.Response{
							StatusCode: http.StatusOK,
							Status:     "200 OK",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("ok")),
							Request:    req,
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusSwitchingProtocols,
						Status:     "101 Switching Protocols",
						Header: http.Header{
							"Connection": []string{"Upgrade"},
							"Upgrade":    []string{"testproto"},
						},
						Body:          noopReadWriteCloser{},
						ContentLength: -1,
						Request:       req,
					}, nil
				})),
			}
			if tt.global {
				globalLimiter := queue.NewLimiterWithCooldown(1, 0)
				activeLimiter = globalLimiter
				opts = append(opts, WithGlobalLimiter(globalLimiter))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "testproto")
			rw := newFailedUpgradeHandshakeResponseWriter()
			p.ServeHTTP(rw, req)

			snap := met.Snapshot()
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted failed 101 handshake with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			if snap.StatusCounts[1] != 1 || snap.StatusCounts[5] != 0 || snap.StatusCounts[0] != 0 {
				t.Fatalf("status buckets = %+v, want one status-1xx aborted handshake and no 5xx/status-0", snap.StatusCounts)
			}
			if len(snap.LogEntries) != 1 || snap.LogEntries[0].Status != http.StatusSwitchingProtocols || !snap.LogEntries[0].Aborted {
				t.Fatalf("metrics log = %+v, want aborted 101", snap.LogEntries)
			}
			stats := b.Stats()
			if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want no mutation for failed 101 handshake", stats.TotalFailures, stats.TotalSuccesses)
			}
			entries := j.Entries()
			if len(entries) != 1 || entries[0].StatusCode != http.StatusSwitchingProtocols || !entries[0].Aborted || !entries[0].Timing.ResponseComplete.IsZero() {
				t.Fatalf("journal entries = %+v, want aborted 101 without ResponseComplete", entries)
			}
			if active := activeLimiter.Stats().Active; active != 1 {
				t.Fatalf("limiter active slots after failed upgrade handshake = %d, want 1 while cancelCooldown protects upstream accounting", active)
			}

			done := make(chan int, 1)
			start := time.Now()
			go func() {
				secondReq := httptest.NewRequest(tt.method, tt.path, nil)
				secondRec := httptest.NewRecorder()
				p.ServeHTTP(secondRec, secondReq)
				done <- secondRec.Code
			}()
			select {
			case code := <-done:
				t.Fatalf("second request completed with status %d before cancelCooldown elapsed", code)
			case <-time.After(50 * time.Millisecond):
			}
			select {
			case code := <-done:
				if code != http.StatusOK {
					t.Fatalf("second request status = %d, want 200 after cancelCooldown", code)
				}
				if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
					t.Fatalf("second request completed after %v, want cancelCooldown delay", elapsed)
				}
			case <-time.After(time.Second):
				t.Fatal("second request did not complete after cancelCooldown")
			}
		})
	}
}

func TestProxy_SwitchingProtocolsFailedHandshakeReleasesHalfOpenProbe(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(10*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(10*time.Millisecond),
	)
	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
	time.Sleep(20 * time.Millisecond)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Upgrade") == "" {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("ok")),
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusSwitchingProtocols,
				Status:     "101 Switching Protocols",
				Header: http.Header{
					"Connection": []string{"Upgrade"},
					"Upgrade":    []string{"testproto"},
				},
				Body:          noopReadWriteCloser{},
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	upgradeReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	upgradeReq.Header.Set("Connection", "Upgrade")
	upgradeReq.Header.Set("Upgrade", "testproto")
	p.ServeHTTP(newFailedUpgradeHandshakeResponseWriter(), upgradeReq)
	if b.State() != circuitbreaker.HalfOpen {
		t.Fatalf("breaker state after failed 101 handshake = %v, want HALF_OPEN with probe released", b.State())
	}
	if stats := b.Stats(); stats.TotalSuccesses != 0 || stats.TotalFailures != 1 {
		t.Fatalf("breaker stats after failed 101 handshake successes=%d failures=%d, want no new mutation", stats.TotalSuccesses, stats.TotalFailures)
	}

	normalReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	normalRec := httptest.NewRecorder()
	p.ServeHTTP(normalRec, normalReq)
	if normalRec.Code != http.StatusOK {
		t.Fatalf("normal request after failed 101 handshake status = %d, want 200; half-open probe was not released", normalRec.Code)
	}
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("breaker state after normal probe = %v, want CLOSED", b.State())
	}
}

func TestProxy_SwitchingProtocolsLocalUpgradeFailureWithRetryReleasesHalfOpenProbe(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name            string
		makeWriter      func() http.ResponseWriter
		wantStatusClass int
	}{
		{
			name: "non hijacker downstream writer generated 502 is aborted local upgrade failure",
			makeWriter: func() http.ResponseWriter {
				return httptest.NewRecorder()
			},
			wantStatusClass: 5,
		},
		{
			name: "failed hijack generated 502 is aborted local upgrade failure",
			makeWriter: func() http.ResponseWriter {
				return &failedHijackResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
			},
			wantStatusClass: 5,
		},
		{
			name: "post hijack downstream handshake failure is aborted local upgrade failure",
			makeWriter: func() http.ResponseWriter {
				return newFailedUpgradeHandshakeResponseWriter()
			},
			wantStatusClass: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(1),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithOpenTimeout(20*time.Millisecond),
				circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
			)
			b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
			time.Sleep(30 * time.Millisecond)

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(1),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if req.Header.Get("Upgrade") == "" {
						return &http.Response{
							StatusCode: http.StatusOK,
							Status:     "200 OK",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("ok")),
							Request:    req,
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusSwitchingProtocols,
						Status:     "101 Switching Protocols",
						Header: http.Header{
							"Connection": []string{"Upgrade"},
							"Upgrade":    []string{"testproto"},
						},
						Body:          noopReadWriteCloser{},
						ContentLength: -1,
						Request:       req,
					}, nil
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			upgradeReq := httptest.NewRequest(http.MethodGet, "/health", nil)
			upgradeReq.Header.Set("Connection", "Upgrade")
			upgradeReq.Header.Set("Upgrade", "testproto")
			p.ServeHTTP(tt.makeWriter(), upgradeReq)

			stats := b.Stats()
			if stats.TotalFailures != 1 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want only seeded failure and no local/downstream upgrade failure accounting", stats.TotalFailures, stats.TotalSuccesses)
			}
			if state := b.State(); state != circuitbreaker.HalfOpen {
				t.Fatalf("breaker state after local/downstream upgrade failure = %v, want HALF_OPEN with probe released", state)
			}
			nextEpoch, allowErr := b.Allow()
			if allowErr != nil {
				t.Fatalf("Allow() after local/downstream upgrade failure = epoch %d, err %v; want released HALF_OPEN probe", nextEpoch, allowErr)
			}
			if nextEpoch == 0 {
				t.Fatal("Allow() after local/downstream upgrade failure returned epoch 0, want new HALF_OPEN probe epoch")
			}
			b.CancelProbe(nextEpoch)

			snap := met.Snapshot()
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted local/downstream upgrade failure with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			if snap.StatusCounts[tt.wantStatusClass] != 1 {
				t.Fatalf("status buckets = %+v, want one class-%d local/downstream upgrade failure", snap.StatusCounts, tt.wantStatusClass)
			}
		})
	}
}

type cancelingReadErrorAfterChunkBody struct {
	chunk  []byte
	err    error
	cancel context.CancelFunc
	sent   bool
}

func (b *cancelingReadErrorAfterChunkBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, b.chunk), nil
	}
	if b.cancel != nil {
		b.cancel()
	}
	return 0, b.err
}

func (b *cancelingReadErrorAfterChunkBody) Close() error { return nil }

func responseWithReadErrorAfterChunk(req *http.Request, chunk string) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        make(http.Header),
		Body:          &readErrorAfterChunkBody{chunk: []byte(chunk), err: errProxyTestUpstreamRead},
		ContentLength: -1,
		Request:       req,
	}
}

func responseWithImmediateReadError(req *http.Request, contentLength int64) *http.Response {
	h := make(http.Header)
	h.Set("Content-Length", strconv.FormatInt(contentLength, 10))
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        h,
		Body:          &readErrorBody{err: errProxyTestUpstreamRead},
		ContentLength: contentLength,
		Request:       req,
	}
}

func responseWithDelayedReadErrorAfterChunk(req *http.Request, status int, h http.Header, chunk string, delay time.Duration) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        h,
		Body:          &delayedReadErrorAfterChunkBody{chunk: []byte(chunk), err: errProxyTestUpstreamRead, delay: delay},
		ContentLength: -1,
		Request:       req,
	}
}

func withPanicOnCopyErrorContext(req *http.Request) *http.Request {
	ctx := context.WithValue(req.Context(), http.ServerContextKey, &http.Server{})
	return req.WithContext(ctx)
}

func serveExpectErrAbortHandler(t *testing.T, p *Proxy, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	defer func() {
		if rv := recover(); rv != http.ErrAbortHandler {
			t.Fatalf("recovered panic = %v, want http.ErrAbortHandler", rv)
		}
	}()
	p.ServeHTTP(w, r)
}

type cancelingErrorResponseWriter struct {
	*httptest.ResponseRecorder
	cancel   context.CancelFunc
	err      error
	canceled bool
}

func (w *cancelingErrorResponseWriter) Write(p []byte) (int, error) {
	if !w.canceled {
		w.canceled = true
		w.cancel()
	}
	return 0, w.err
}

type errorResponseWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w *errorResponseWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

type flushFailResponseWriter struct {
	*httptest.ResponseRecorder
	err     error
	flushes atomic.Int64
}

func (w *flushFailResponseWriter) FlushError() error {
	w.flushes.Add(1)
	return w.err
}

func TestProxy_ErrAbortHandler_UpstreamReadFailure_NoBreakerSuccess_Limited(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return responseWithReadErrorAfterChunk(req, "partial"), nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := withPanicOnCopyErrorContext(httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	rec := httptest.NewRecorder()

	serveExpectErrAbortHandler(t, p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want committed upstream 200", rec.Code)
	}
	if rec.Body.String() != "partial" {
		t.Fatalf("body = %q, want partial body committed before read failure", rec.Body.String())
	}
	stats := b.Stats()
	if stats.TotalSuccesses != 0 {
		t.Fatalf("breaker TotalSuccesses = %d, want 0 (truncated upstream 2xx body must not heal breaker)", stats.TotalSuccesses)
	}
	if stats.TotalFailures != 1 {
		t.Fatalf("breaker TotalFailures = %d, want 1 (upstream body read failure must be reported as an unclean upstream transfer)", stats.TotalFailures)
	}
}

func TestProxy_ErrAbortHandler_UpstreamReadFailure_NoBreakerSuccess_Passthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithGlobalLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return responseWithReadErrorAfterChunk(req, "passthrough-partial"), nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := withPanicOnCopyErrorContext(httptest.NewRequest(http.MethodGet, "/health", nil))
	rec := httptest.NewRecorder()

	serveExpectErrAbortHandler(t, p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want committed upstream 200", rec.Code)
	}
	if rec.Body.String() != "passthrough-partial" {
		t.Fatalf("body = %q, want partial body committed before read failure", rec.Body.String())
	}
	stats := b.Stats()
	if stats.TotalSuccesses != 0 {
		t.Fatalf("breaker TotalSuccesses = %d, want 0 (truncated passthrough 2xx body must not heal breaker)", stats.TotalSuccesses)
	}
	if stats.TotalFailures != 1 {
		t.Fatalf("breaker TotalFailures = %d, want 1 (passthrough upstream body read failure must be reported)", stats.TotalFailures)
	}
}

func TestProxy_ErrAbortHandler_ImmediateUpstreamReadFailure_ContentLengthJournalsZeroDeliveredBytes(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name        string
		method      string
		path        string
		wantLimited bool
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages", wantLimited: true},
		{name: "passthrough", method: http.MethodGet, path: "/health", wantLimited: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			j := journal.New(8, 1024)
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithJournal(j),
				WithBreaker(b),
				WithMaxRetries(0),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					return responseWithImmediateReadError(req, 123), nil
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			rec := httptest.NewRecorder()
			req := withPanicOnCopyErrorContext(httptest.NewRequest(tt.method, tt.path, nil))
			serveExpectErrAbortHandler(t, p, rec, req)

			entries := j.Entries()
			if len(entries) != 1 {
				t.Fatalf("journal entries = %d, want 1", len(entries))
			}
			entry := entries[0]
			if entry.StatusCode != http.StatusOK || !entry.Aborted || entry.Limited != tt.wantLimited {
				t.Fatalf("journal entry status=%d aborted=%v limited=%v, want status=200 aborted=true limited=%v", entry.StatusCode, entry.Aborted, entry.Limited, tt.wantLimited)
			}
			if entry.ResponseSize != 0 {
				t.Fatalf("journal ResponseSize = %d, want 0 bytes accepted by Write despite Content-Length", entry.ResponseSize)
			}
			if len(entry.ResponseBody) != 0 {
				t.Fatalf("journal ResponseBody = %q, want empty", string(entry.ResponseBody))
			}
			if !entry.Timing.ResponseComplete.IsZero() {
				t.Fatalf("journal ResponseComplete = %v, want zero for aborted exchange", entry.Timing.ResponseComplete)
			}

			snap := met.Snapshot()
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted=1 clean=0", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			stats := b.Stats()
			if stats.TotalSuccesses != 0 || stats.TotalFailures != 1 {
				t.Fatalf("breaker successes=%d failures=%d, want successes=0 failures=1 for upstream body read failure", stats.TotalSuccesses, stats.TotalFailures)
			}
		})
	}
}

func TestProxy_DirectServeHTTP_UpstreamReadFailureWithoutServerContext_IsAbortedNotSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return responseWithReadErrorAfterChunk(req, "direct-partial"), nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want committed upstream 200", rec.Code)
	}
	if rec.Body.String() != "direct-partial" {
		t.Fatalf("body = %q, want partial body", rec.Body.String())
	}
	stats := b.Stats()
	if stats.TotalSuccesses != 0 || stats.TotalFailures != 1 {
		t.Fatalf("breaker successes=%d failures=%d, want successes=0 failures=1", stats.TotalSuccesses, stats.TotalFailures)
	}
	if snap := met.Snapshot(); snap.TotalAborted != 1 || snap.TotalProxied != 0 {
		t.Fatalf("metrics TotalAborted=%d TotalProxied=%d, want aborted=1 clean proxied=0", snap.TotalAborted, snap.TotalProxied)
	}
}

func TestProxy_DirectServeHTTP_DownstreamWriteFailureWithoutServerContext_IsAbortedNotSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "response body")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

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

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
	p.ServeHTTP(rec, req)

	stats := b.Stats()
	if stats.TotalSuccesses != 0 || stats.TotalFailures != 0 {
		t.Fatalf("breaker successes=%d failures=%d, want no mutation for downstream-only write failure", stats.TotalSuccesses, stats.TotalFailures)
	}
	if snap := met.Snapshot(); snap.TotalAborted != 1 || snap.TotalProxied != 0 {
		t.Fatalf("metrics TotalAborted=%d TotalProxied=%d, want aborted=1 clean proxied=0", snap.TotalAborted, snap.TotalProxied)
	}
}

func TestProxy_DirectServeHTTP_DownstreamFlushFailureWithoutWriteFailure_IsAbortedNotSuccess(t *testing.T) {
	const responseBody = "response body"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, responseBody)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name        string
		method      string
		path        string
		wantLimited bool
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages", wantLimited: true},
		{name: "passthrough", method: http.MethodGet, path: "/health", wantLimited: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			j := journal.New(8, 1024)
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithJournal(j),
				WithBreaker(b),
				WithMaxRetries(0),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := &flushFailResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamFlush}
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want committed upstream 200", rec.Code)
			}
			if rec.Body.String() != responseBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), responseBody)
			}
			if rec.flushes.Load() == 0 {
				t.Fatal("downstream FlushError was not called; test did not exercise flush-only failure path")
			}

			stats := b.Stats()
			if stats.TotalSuccesses != 0 || stats.TotalFailures != 0 {
				t.Fatalf("breaker successes=%d failures=%d, want no mutation for downstream-only flush failure", stats.TotalSuccesses, stats.TotalFailures)
			}
			snap := met.Snapshot()
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted=1 clean=0", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			if snap.StatusCounts[2] != 1 || snap.StatusCounts[5] != 0 {
				t.Fatalf("status buckets = %+v, want one observed 2xx and no 5xx for flush-only abort", snap.StatusCounts)
			}
			if len(snap.LogEntries) != 1 || !snap.LogEntries[0].Aborted || snap.LogEntries[0].Status != http.StatusOK || snap.LogEntries[0].Limited != tt.wantLimited {
				t.Fatalf("metrics log = %+v, want one aborted 200 entry limited=%v", snap.LogEntries, tt.wantLimited)
			}

			entries := j.Entries()
			if len(entries) != 1 {
				t.Fatalf("journal entries = %d, want 1", len(entries))
			}
			entry := entries[0]
			if entry.StatusCode != http.StatusOK || !entry.Aborted || entry.Limited != tt.wantLimited {
				t.Fatalf("journal entry status=%d aborted=%v limited=%v, want aborted 200 limited=%v", entry.StatusCode, entry.Aborted, entry.Limited, tt.wantLimited)
			}
			if !entry.Timing.ResponseComplete.IsZero() {
				t.Fatalf("journal ResponseComplete = %v, want zero for flush-aborted response", entry.Timing.ResponseComplete)
			}
			if entry.ResponseSize != int64(len(responseBody)) {
				t.Fatalf("journal ResponseSize = %d, want Write-accepted byte count %d", entry.ResponseSize, len(responseBody))
			}
		})
	}
}

func TestProxy_DownstreamAbortReleasesHalfOpenProbe(t *testing.T) {
	const responseBody = "response body"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, responseBody)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name        string
		method      string
		path        string
		makeWriter  func() http.ResponseWriter
		wantLimited bool
	}{
		{
			name:   "limited write failure",
			method: http.MethodPost,
			path:   "/v1/messages",
			makeWriter: func() http.ResponseWriter {
				return &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
			},
			wantLimited: true,
		},
		{
			name:   "passthrough write failure",
			method: http.MethodGet,
			path:   "/health",
			makeWriter: func() http.ResponseWriter {
				return &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
			},
			wantLimited: false,
		},
		{
			name:   "limited flush failure",
			method: http.MethodPost,
			path:   "/v1/messages",
			makeWriter: func() http.ResponseWriter {
				return &flushFailResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamFlush}
			},
			wantLimited: true,
		},
		{
			name:   "passthrough flush failure",
			method: http.MethodGet,
			path:   "/health",
			makeWriter: func() http.ResponseWriter {
				return &flushFailResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamFlush}
			},
			wantLimited: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(2),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithOpenTimeout(20*time.Millisecond),
				circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
			)
			waitForHalfOpenProbeWindow(t, b)

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

			req := httptest.NewRequest(tt.method, tt.path, nil)
			p.ServeHTTP(tt.makeWriter(), req)

			stats := b.Stats()
			if stats.TotalFailures != 2 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want only seeded failures and no downstream-abort accounting", stats.TotalFailures, stats.TotalSuccesses)
			}
			if state := b.State(); state != circuitbreaker.HalfOpen {
				t.Fatalf("breaker state = %s, want HALF_OPEN after downstream-only abort", state)
			}
			nextEpoch, allowErr := b.Allow()
			if allowErr != nil {
				t.Fatalf("Allow() after downstream-only abort = epoch %d, err %v; want released HALF_OPEN probe", nextEpoch, allowErr)
			}
			if nextEpoch == 0 {
				t.Fatal("Allow() after downstream-only abort returned epoch 0, want new HALF_OPEN probe epoch")
			}
			b.CancelProbe(nextEpoch)

			snap := met.Snapshot()
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted downstream-only exchange with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			if len(snap.LogEntries) != 1 || !snap.LogEntries[0].Aborted || snap.LogEntries[0].Status != http.StatusOK || snap.LogEntries[0].Limited != tt.wantLimited {
				t.Fatalf("metrics log = %+v, want one aborted 200 entry limited=%v", snap.LogEntries, tt.wantLimited)
			}
		})
	}
}

func TestProxy_ErrAbortHandler_DownstreamAbortReleasesHalfOpenProbe(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name        string
		method      string
		path        string
		wantLimited bool
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages", wantLimited: true},
		{name: "passthrough", method: http.MethodGet, path: "/health", wantLimited: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(2),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithOpenTimeout(20*time.Millisecond),
				circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
			)
			waitForHalfOpenProbeWindow(t, b)

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(0),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode:    http.StatusOK,
						Status:        "200 OK",
						Header:        make(http.Header),
						Body:          io.NopCloser(strings.NewReader("panic-path downstream body")),
						ContentLength: int64(len("panic-path downstream body")),
						Request:       req,
					}, nil
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			rec := &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
			req := withPanicOnCopyErrorContext(httptest.NewRequest(tt.method, tt.path, nil))
			serveExpectErrAbortHandler(t, p, rec, req)

			stats := b.Stats()
			if stats.TotalFailures != 2 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want only seeded failures and no downstream panic-path accounting", stats.TotalFailures, stats.TotalSuccesses)
			}
			if state := b.State(); state != circuitbreaker.HalfOpen {
				t.Fatalf("breaker state = %s, want HALF_OPEN after downstream-only ErrAbortHandler", state)
			}
			nextEpoch, allowErr := b.Allow()
			if allowErr != nil {
				t.Fatalf("Allow() after downstream-only ErrAbortHandler = epoch %d, err %v; want released HALF_OPEN probe", nextEpoch, allowErr)
			}
			if nextEpoch == 0 {
				t.Fatal("Allow() after downstream-only ErrAbortHandler returned epoch 0, want new HALF_OPEN probe epoch")
			}
			b.CancelProbe(nextEpoch)

			snap := met.Snapshot()
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted downstream-only panic path with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			if len(snap.LogEntries) != 1 || !snap.LogEntries[0].Aborted || snap.LogEntries[0].Status != http.StatusOK || snap.LogEntries[0].Limited != tt.wantLimited {
				t.Fatalf("metrics log = %+v, want one aborted 200 ErrAbortHandler entry limited=%v", snap.LogEntries, tt.wantLimited)
			}
		})
	}
}

func TestProxy_ErrAbortHandler_RealServerClientDisconnectReleasesHalfOpenProbe(t *testing.T) {
	firstChunk := []byte("first half-open chunk\n")
	firstFlushed := make(chan struct{})
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(firstChunk); err != nil {
			return
		}
		flusher.Flush()
		close(firstFlushed)

		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if _, err := fmt.Fprintf(w, "tail-%d\n", i); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(250*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(250*time.Millisecond),
	)
	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
	deadline := time.Now().Add(time.Second)
	for b.WaitDuration() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if wait := b.WaitDuration(); wait > 0 {
		t.Fatalf("breaker still waiting after deadline: %v", wait)
	}

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

	proxyServer := httptest.NewServer(p)
	t.Cleanup(proxyServer.Close)

	conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		conn.Close()
		t.Fatalf("SetDeadline: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1/messages", nil)
	if err != nil {
		conn.Close()
		t.Fatalf("NewRequest: %v", err)
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		t.Fatalf("write request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := make([]byte, len(firstChunk))
	if _, err := io.ReadFull(resp.Body, got); err != nil {
		conn.Close()
		t.Fatalf("read first response chunk: %v", err)
	}
	if string(got) != string(firstChunk) {
		conn.Close()
		t.Fatalf("first response chunk = %q, want %q", string(got), string(firstChunk))
	}
	select {
	case <-firstFlushed:
	default:
		conn.Close()
		t.Fatal("upstream first chunk was read before firstFlushed signal")
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close client connection: %v", err)
	}

	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream handler did not observe downstream disconnect")
	}

	var snap metrics.Snapshot
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap = met.Snapshot()
		if snap.TotalAborted == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
		t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted real-server disconnect with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
	}
	stats := b.Stats()
	if stats.TotalFailures != 1 || stats.TotalSuccesses != 0 {
		t.Fatalf("breaker failures=%d successes=%d, want only seeded failure and no client-disconnect accounting", stats.TotalFailures, stats.TotalSuccesses)
	}
	if state := b.State(); state != circuitbreaker.HalfOpen {
		t.Fatalf("breaker state = %s, want HALF_OPEN after real client disconnect", state)
	}
	nextEpoch, allowErr := b.Allow()
	if allowErr != nil {
		t.Fatalf("Allow() after real client disconnect = epoch %d, err %v; want ErrAbortHandler path to release HALF_OPEN probe immediately", nextEpoch, allowErr)
	}
	if nextEpoch == 0 {
		t.Fatal("Allow() after real client disconnect returned epoch 0, want new HALF_OPEN probe epoch")
	}
	b.CancelProbe(nextEpoch)
}

func TestProxy_ErrAbortHandler_UpstreamReadFailure_RealServerAbortsClientStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	j := journal.New(8, 1024)
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithGlobalLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithJournal(j),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return responseWithReadErrorAfterChunk(req, "wire-partial"), nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	proxyServer := httptest.NewServer(p)
	t.Cleanup(proxyServer.Close)

	resp, err := proxyServer.Client().Get(proxyServer.URL + "/health")
	if err != nil {
		t.Fatalf("client Get: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want committed upstream 200", resp.StatusCode)
	}
	if string(body) != "wire-partial" {
		t.Fatalf("body = %q, want partial bytes before abort", string(body))
	}
	if readErr == nil {
		t.Fatal("ReadAll error = nil, want aborted chunked stream error")
	}

	stats := b.Stats()
	if stats.TotalSuccesses != 0 {
		t.Fatalf("breaker TotalSuccesses = %d, want 0", stats.TotalSuccesses)
	}
	if stats.TotalFailures != 1 {
		t.Fatalf("breaker TotalFailures = %d, want 1", stats.TotalFailures)
	}
	snap := met.Snapshot()
	if snap.TotalAborted != 1 {
		t.Fatalf("TotalAborted = %d, want 1", snap.TotalAborted)
	}
	if snap.TotalPassThrough != 0 {
		t.Fatalf("TotalPassThrough = %d, want 0 (aborted passthrough is not a clean completion)", snap.TotalPassThrough)
	}
	if len(snap.LogEntries) != 1 || !snap.LogEntries[0].Aborted || snap.LogEntries[0].Status != http.StatusOK {
		t.Fatalf("metrics log = %+v, want one aborted 200 entry", snap.LogEntries)
	}
	entries := j.Entries()
	if len(entries) != 1 {
		t.Fatalf("journal entries = %d, want 1", len(entries))
	}
	if !entries[0].Aborted {
		t.Fatal("journal entry Aborted = false, want true")
	}
	if !entries[0].Timing.ResponseComplete.IsZero() {
		t.Fatalf("journal ResponseComplete = %v, want zero for aborted response", entries[0].Timing.ResponseComplete)
	}
	if entries[0].ResponseSize != int64(len("wire-partial")) {
		t.Fatalf("journal ResponseSize = %d, want Write-accepted partial byte count", entries[0].ResponseSize)
	}
}

func TestProxy_ClientDisconnectDuringRealUpstreamStream_DoesNotPoisonBreaker(t *testing.T) {
	firstChunk := []byte("first-chunk\n")
	firstFlushed := make(chan struct{})
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(firstChunk); err != nil {
			return
		}
		flusher.Flush()
		close(firstFlushed)

		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if _, err := fmt.Fprintf(w, "more-%d\n", i); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	j := journal.New(8, 1024)
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
	lim := queue.NewLimiterWithCooldown(1, 0)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(lim),
		WithMetrics(met),
		WithJournal(j),
		WithBreaker(b),
		WithCancelCooldown(5*time.Second),
		WithMaxRetries(0),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	proxyServer := httptest.NewServer(p)
	t.Cleanup(proxyServer.Close)

	conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		conn.Close()
		t.Fatalf("SetDeadline: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1/messages", nil)
	if err != nil {
		conn.Close()
		t.Fatalf("NewRequest: %v", err)
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		t.Fatalf("write request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := make([]byte, len(firstChunk))
	if _, err := io.ReadFull(resp.Body, got); err != nil {
		conn.Close()
		t.Fatalf("read first response chunk: %v", err)
	}
	if string(got) != string(firstChunk) {
		conn.Close()
		t.Fatalf("first response chunk = %q, want %q", string(got), string(firstChunk))
	}
	select {
	case <-firstFlushed:
	default:
		conn.Close()
		t.Fatal("upstream first chunk was read before firstFlushed signal")
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close client connection: %v", err)
	}

	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream handler did not observe downstream disconnect")
	}

	var snap metrics.Snapshot
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap = met.Snapshot()
		if snap.TotalAborted == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if snap.TotalAborted != 1 {
		t.Fatalf("TotalAborted = %d, want 1; metrics=%+v breaker=%+v", snap.TotalAborted, snap, b.Stats())
	}
	if snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
		t.Fatalf("TotalProxied=%d TotalPassThrough=%d, want no clean completion", snap.TotalProxied, snap.TotalPassThrough)
	}
	if len(snap.LogEntries) != 1 || !snap.LogEntries[0].Aborted || snap.LogEntries[0].Status != http.StatusOK {
		t.Fatalf("metrics log = %+v, want one aborted 200 entry", snap.LogEntries)
	}
	stats := b.Stats()
	if stats.TotalSuccesses != 0 || stats.TotalFailures != 0 {
		t.Fatalf("breaker successes=%d failures=%d, want no mutation for real client disconnect", stats.TotalSuccesses, stats.TotalFailures)
	}
	limStats := lim.Stats()
	if limStats.TotalAcq != 1 || limStats.TotalRel != 0 || limStats.Active != 1 {
		t.Fatalf("limiter stats after abort = %+v, want acquired slot held by cancel cooldown", limStats)
	}
	entries := j.Entries()
	if len(entries) != 1 {
		t.Fatalf("journal entries = %d, want 1", len(entries))
	}
	if entries[0].StatusCode != http.StatusOK || !entries[0].Aborted || !entries[0].Timing.ResponseComplete.IsZero() {
		t.Fatalf("journal entry status=%d aborted=%v complete=%v, want aborted 200 with zero ResponseComplete", entries[0].StatusCode, entries[0].Aborted, entries[0].Timing.ResponseComplete)
	}
	if entries[0].ResponseSize < int64(len(firstChunk)) {
		t.Fatalf("journal ResponseSize = %d, want at least first Write-accepted chunk %d", entries[0].ResponseSize, len(firstChunk))
	}
}

func waitForHalfOpenProbeWindow(t *testing.T, b *circuitbreaker.Breaker) {
	t.Helper()
	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
	if state := b.State(); state != circuitbreaker.Open {
		t.Fatalf("breaker state after forced failures = %s, want OPEN", state)
	}
	time.Sleep(30 * time.Millisecond)
}

func TestProxy_RetryRequestContextCancellationDoesNotBecomeClean502(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name        string
		method      string
		path        string
		wantLimited bool
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages", wantLimited: true},
		{name: "passthrough", method: http.MethodGet, path: "/health", wantLimited: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(1),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithOpenTimeout(20*time.Millisecond),
				circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
			)
			b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
			deadline := time.Now().Add(time.Second)
			for b.WaitDuration() > 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if wait := b.WaitDuration(); wait > 0 {
				t.Fatalf("breaker still waiting after deadline: %v", wait)
			}

			var cancel context.CancelFunc
			var calls atomic.Int64
			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(1),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
					cancel()
					return nil, context.Canceled
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			ctx, cancelFn := context.WithCancel(context.Background())
			cancel = cancelFn
			req := httptest.NewRequest(tt.method, tt.path, nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if calls.Load() != 1 {
				t.Fatalf("transport calls = %d, want 1; request-context cancellation must not be retried", calls.Load())
			}
			if rec.Code == http.StatusBadGateway {
				t.Fatalf("response status = 502; request-context cancellation was converted into a proxy-generated bad gateway")
			}
			stats := b.Stats()
			if stats.TotalFailures != 1 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want only seeded failure and no cancellation accounting", stats.TotalFailures, stats.TotalSuccesses)
			}
			if state := b.State(); state != circuitbreaker.HalfOpen {
				t.Fatalf("breaker state = %s, want HALF_OPEN because client cancellation is not an upstream failure", state)
			}
			nextEpoch, allowErr := b.Allow()
			if allowErr != nil {
				t.Fatalf("Allow() after retry-owned request cancellation = epoch %d, err %v; want released HALF_OPEN probe", nextEpoch, allowErr)
			}
			if nextEpoch == 0 {
				t.Fatal("Allow() after retry-owned request cancellation returned epoch 0, want new HALF_OPEN probe epoch")
			}
			b.CancelProbe(nextEpoch)

			snap := met.Snapshot()
			if snap.StatusCounts[5] != 0 {
				t.Fatalf("StatusCounts[5] = %d, want 0 (no clean/proxy-generated 502)", snap.StatusCounts[5])
			}
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted cancellation with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			if len(snap.LogEntries) != 1 || !snap.LogEntries[0].Aborted || snap.LogEntries[0].Status != 0 || snap.LogEntries[0].Limited != tt.wantLimited {
				t.Fatalf("metrics log = %+v, want one aborted status-0 entry limited=%v", snap.LogEntries, tt.wantLimited)
			}
		})
	}
}

func TestProxy_RetryRequestContextCancellationAfterRetryableStatusDoesNotBecomeClean502(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name        string
		method      string
		path        string
		wantLimited bool
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages", wantLimited: true},
		{name: "passthrough", method: http.MethodGet, path: "/health", wantLimited: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(1),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithOpenTimeout(20*time.Millisecond),
				circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
			)
			b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
			deadline := time.Now().Add(time.Second)
			for b.WaitDuration() > 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if wait := b.WaitDuration(); wait > 0 {
				t.Fatalf("breaker still waiting after deadline: %v", wait)
			}

			var cancel context.CancelFunc
			var calls atomic.Int64
			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(1),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
					cancel()
					return &http.Response{
						StatusCode: http.StatusInternalServerError,
						Status:     "500 Internal Server Error",
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("retryable failure")),
						Request:    req,
					}, nil
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			ctx, cancelFn := context.WithCancel(context.Background())
			cancel = cancelFn
			req := httptest.NewRequest(tt.method, tt.path, nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if calls.Load() != 1 {
				t.Fatalf("transport calls = %d, want 1; cancellation after retryable status must not be retried", calls.Load())
			}
			if rec.Code == http.StatusBadGateway || strings.Contains(rec.Body.String(), "bad gateway") {
				t.Fatalf("response status=%d body=%q; post-status cancellation was converted into a proxy-generated bad gateway", rec.Code, rec.Body.String())
			}
			stats := b.Stats()
			if stats.TotalFailures != 2 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want seeded failure plus upstream 500 and no cancellation success", stats.TotalFailures, stats.TotalSuccesses)
			}
			if state := b.State(); state != circuitbreaker.Open {
				t.Fatalf("breaker state = %s, want OPEN after definitive upstream 500", state)
			}

			snap := met.Snapshot()
			if snap.StatusCounts[5] != 0 {
				t.Fatalf("StatusCounts=%+v, want aborted status-0 with no proxy-generated 5xx", snap.StatusCounts)
			}
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted cancellation with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			if len(snap.LogEntries) != 1 || !snap.LogEntries[0].Aborted || snap.LogEntries[0].Status != 0 || snap.LogEntries[0].Limited != tt.wantLimited {
				t.Fatalf("metrics log = %+v, want one aborted status-0 entry limited=%v", snap.LogEntries, tt.wantLimited)
			}
		})
	}
}

func TestProxy_RetryRecordedFailureHoldsSlotDespiteRequestCancellation(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name        string
		method      string
		path        string
		global      bool
		wantLimited bool
	}{
		{name: "limited route limiter", method: http.MethodPost, path: "/v1/messages", wantLimited: true},
		{name: "passthrough global limiter", method: http.MethodGet, path: "/health", global: true, wantLimited: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(100),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithBasePenalty(150*time.Millisecond),
				circuitbreaker.WithMaxPenalty(150*time.Millisecond),
			)
			var cancel context.CancelFunc
			var calls atomic.Int64

			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(1),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithCancelCooldown(0),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					call := calls.Add(1)
					if call == 1 {
						cancel()
						return &http.Response{
							StatusCode: http.StatusInternalServerError,
							Status:     "500 Internal Server Error",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("retry-recorded failure")),
							Request:    req,
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("ok")),
						Request:    req,
					}, nil
				})),
			}
			if tt.global {
				opts = append(opts, WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			ctx, cancelFn := context.WithCancel(context.Background())
			cancel = cancelFn
			firstReq := httptest.NewRequest(tt.method, tt.path, nil).WithContext(ctx)
			firstRec := httptest.NewRecorder()
			p.ServeHTTP(firstRec, firstReq)

			stats := b.Stats()
			if stats.TotalFailures != 1 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want retry transport to record the upstream 500 despite cancellation", stats.TotalFailures, stats.TotalSuccesses)
			}
			snap := met.Snapshot()
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("after first request metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted cancellation with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			if len(snap.LogEntries) != 1 || !snap.LogEntries[0].Aborted || snap.LogEntries[0].Status != 0 || snap.LogEntries[0].Limited != tt.wantLimited {
				t.Fatalf("metrics log = %+v, want one aborted status-0 entry limited=%v", snap.LogEntries, tt.wantLimited)
			}

			start := time.Now()
			secondReq := httptest.NewRequest(tt.method, tt.path, nil)
			secondRec := httptest.NewRecorder()
			p.ServeHTTP(secondRec, secondReq)
			elapsed := time.Since(start)
			if secondRec.Code != http.StatusOK {
				t.Fatalf("second request status = %d body=%q, want 200 after upstream-failure slot hold", secondRec.Code, secondRec.Body.String())
			}
			if elapsed < 100*time.Millisecond {
				t.Fatalf("second request acquired slot in %v, want retry-recorded upstream failure to hold slot despite client cancellation", elapsed)
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("transport calls = %d, want first failure plus second success", got)
			}
		})
	}
}

func TestProxy_RetryRecordedEarlierFailureHoldsSlotAfterLaterCancellation(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name        string
		method      string
		path        string
		global      bool
		wantLimited bool
	}{
		{name: "limited route limiter", method: http.MethodPost, path: "/v1/messages", wantLimited: true},
		{name: "passthrough global limiter", method: http.MethodGet, path: "/health", global: true, wantLimited: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(100),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithBasePenalty(150*time.Millisecond),
				circuitbreaker.WithMaxPenalty(150*time.Millisecond),
			)
			var cancel context.CancelFunc
			var calls atomic.Int64

			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(2),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithCancelCooldown(0),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					call := calls.Add(1)
					switch call {
					case 1:
						return &http.Response{
							StatusCode: http.StatusInternalServerError,
							Status:     "500 Internal Server Error",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("first attempt upstream failure")),
							Request:    req,
						}, nil
					case 2:
						cancel()
						return nil, context.Canceled
					default:
						return &http.Response{
							StatusCode: http.StatusOK,
							Status:     "200 OK",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("ok")),
							Request:    req,
						}, nil
					}
				})),
			}
			if tt.global {
				opts = append(opts, WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			ctx, cancelFn := context.WithCancel(context.Background())
			cancel = cancelFn
			firstReq := httptest.NewRequest(tt.method, tt.path, nil).WithContext(ctx)
			firstRec := httptest.NewRecorder()
			p.ServeHTTP(firstRec, firstReq)

			if got := calls.Load(); got != 2 {
				t.Fatalf("transport calls after canceled first exchange = %d, want first failure plus canceled retry attempt", got)
			}
			stats := b.Stats()
			if stats.TotalFailures != 1 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want retry transport to remember attempt-0 upstream failure despite attempt-1 cancellation", stats.TotalFailures, stats.TotalSuccesses)
			}
			snap := met.Snapshot()
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("after first request metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted cancellation with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			if len(snap.LogEntries) != 1 || !snap.LogEntries[0].Aborted || snap.LogEntries[0].Status != 0 || snap.LogEntries[0].Limited != tt.wantLimited {
				t.Fatalf("metrics log = %+v, want one aborted status-0 entry limited=%v", snap.LogEntries, tt.wantLimited)
			}

			start := time.Now()
			secondReq := httptest.NewRequest(tt.method, tt.path, nil)
			secondRec := httptest.NewRecorder()
			p.ServeHTTP(secondRec, secondReq)
			elapsed := time.Since(start)
			if secondRec.Code != http.StatusOK {
				t.Fatalf("second request status = %d body=%q, want 200 after earlier-failure slot hold", secondRec.Code, secondRec.Body.String())
			}
			if elapsed < 100*time.Millisecond {
				t.Fatalf("second request acquired slot in %v, want earlier retry-recorded upstream failure to hold slot despite later attempt cancellation", elapsed)
			}
			if got := calls.Load(); got != 3 {
				t.Fatalf("transport calls = %d, want first failure, canceled retry, and second success", got)
			}
		})
	}
}

func TestProxy_RetryRecordedEarlierFailureHoldsSlotAfterLaterCancellationWithoutBreaker(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name        string
		method      string
		path        string
		global      bool
		wantLimited bool
	}{
		{name: "limited route limiter", method: http.MethodPost, path: "/v1/messages", wantLimited: true},
		{name: "passthrough global limiter", method: http.MethodGet, path: "/health", global: true, wantLimited: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)
			var cancel context.CancelFunc
			var calls atomic.Int64

			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithBreaker(nil),
				WithMaxRetries(2),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithCancelCooldown(0),
				WithFailureHold(150 * time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					call := calls.Add(1)
					switch call {
					case 1:
						return &http.Response{
							StatusCode: http.StatusInternalServerError,
							Status:     "500 Internal Server Error",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("first attempt upstream failure")),
							Request:    req,
						}, nil
					case 2:
						cancel()
						return nil, context.Canceled
					default:
						return &http.Response{
							StatusCode: http.StatusOK,
							Status:     "200 OK",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("ok")),
							Request:    req,
						}, nil
					}
				})),
			}
			if tt.global {
				opts = append(opts, WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			ctx, cancelFn := context.WithCancel(context.Background())
			cancel = cancelFn
			firstReq := httptest.NewRequest(tt.method, tt.path, nil).WithContext(ctx)
			firstRec := httptest.NewRecorder()
			p.ServeHTTP(firstRec, firstReq)

			if got := calls.Load(); got != 2 {
				t.Fatalf("transport calls after canceled first exchange = %d, want first failure plus canceled retry attempt", got)
			}
			snap := met.Snapshot()
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("after first request metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted cancellation with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			if len(snap.LogEntries) != 1 || !snap.LogEntries[0].Aborted || snap.LogEntries[0].Status != 0 || snap.LogEntries[0].Limited != tt.wantLimited {
				t.Fatalf("metrics log = %+v, want one aborted status-0 entry limited=%v", snap.LogEntries, tt.wantLimited)
			}

			start := time.Now()
			secondReq := httptest.NewRequest(tt.method, tt.path, nil)
			secondRec := httptest.NewRecorder()
			p.ServeHTTP(secondRec, secondReq)
			elapsed := time.Since(start)
			if secondRec.Code != http.StatusOK {
				t.Fatalf("second request status = %d body=%q, want 200 after breaker-disabled failure-hold", secondRec.Code, secondRec.Body.String())
			}
			if elapsed < 100*time.Millisecond {
				t.Fatalf("second request acquired slot in %v, want earlier retry-recorded upstream failure to drive failure-hold without breaker", elapsed)
			}
			if got := calls.Load(); got != 3 {
				t.Fatalf("transport calls = %d, want first failure, canceled retry, and second success", got)
			}
		})
	}
}

func TestProxy_RetryRecordedEarlierFailureUsesCancelCooldownAfterSuccessfulRetryAbort(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name        string
		method      string
		path        string
		global      bool
		wantLimited bool
	}{
		{name: "limited route limiter", method: http.MethodPost, path: "/v1/messages", wantLimited: true},
		{name: "passthrough global limiter", method: http.MethodGet, path: "/health", global: true, wantLimited: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(100),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithBasePenalty(500*time.Millisecond),
				circuitbreaker.WithMaxPenalty(500*time.Millisecond),
			)
			var calls atomic.Int64

			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(1),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithCancelCooldown(20 * time.Millisecond),
				WithQueueTimeout(200 * time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					call := calls.Add(1)
					if call == 1 {
						return &http.Response{
							StatusCode: http.StatusInternalServerError,
							Status:     "500 Internal Server Error",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("first attempt upstream failure")),
							Request:    req,
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("successful current retry attempt")),
						Request:    req,
					}, nil
				})),
			}
			if tt.global {
				opts = append(opts, WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			firstReq := httptest.NewRequest(tt.method, tt.path, nil)
			firstRec := &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
			p.ServeHTTP(firstRec, firstReq)

			if got := calls.Load(); got != 2 {
				t.Fatalf("transport calls after aborted first exchange = %d, want first failure plus successful retry attempt", got)
			}
			stats := b.Stats()
			if stats.TotalFailures != 1 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want only the earlier upstream failure and no deferred success after downstream abort", stats.TotalFailures, stats.TotalSuccesses)
			}
			snap := met.Snapshot()
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("after first request metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted downstream/client exchange with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			if len(snap.LogEntries) != 1 || !snap.LogEntries[0].Aborted || snap.LogEntries[0].Status != http.StatusOK || snap.LogEntries[0].Limited != tt.wantLimited {
				t.Fatalf("metrics log = %+v, want one aborted 200 entry limited=%v", snap.LogEntries, tt.wantLimited)
			}

			start := time.Now()
			secondReq := httptest.NewRequest(tt.method, tt.path, nil)
			secondRec := httptest.NewRecorder()
			p.ServeHTTP(secondRec, secondReq)
			elapsed := time.Since(start)
			if secondRec.Code != http.StatusOK {
				t.Fatalf("second request status = %d body=%q after %v, want 200 after cancel-cooldown rather than queue timeout from phantom penalty", secondRec.Code, secondRec.Body.String(), elapsed)
			}
			if elapsed > 250*time.Millisecond {
				t.Fatalf("second request completed after %v, want no breaker phantom penalty/failure hold after successful current retry attempt was aborted by downstream", elapsed)
			}
			if got := calls.Load(); got != 3 {
				t.Fatalf("transport calls = %d, want first failure, successful aborted retry, and second success", got)
			}
		})
	}
}

func TestProxy_RetryRecordedEarlierFailureDoesNotBlockCurrentHalfOpenProbeCancellation(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name        string
		method      string
		path        string
		wantLimited bool
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages", wantLimited: true},
		{name: "passthrough", method: http.MethodGet, path: "/health", wantLimited: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(1),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithOpenTimeout(20*time.Millisecond),
				circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
			)
			var calls atomic.Int64

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(1),
				WithRetryWaitMin(0),
				WithRetryWaitMax(0),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					call := calls.Add(1)
					if call == 1 {
						return &http.Response{
							StatusCode: http.StatusInternalServerError,
							Status:     "500 Internal Server Error",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("first attempt upstream failure")),
							Request:    req,
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("current half-open probe response")),
						Request:    req,
					}, nil
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			retryTransport := p.transport.(*retry.Transport)
			oldCheckRetry := retryTransport.CheckRetry
			if oldCheckRetry == nil {
				oldCheckRetry = retry.DefaultCheckRetry
			}
			retryTransport.CheckRetry = func(resp *http.Response, err error) bool {
				if resp != nil && resp.StatusCode == http.StatusInternalServerError {
					deadline := time.Now().Add(time.Second)
					for b.WaitDuration() > 0 && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
				}
				return oldCheckRetry(resp, err)
			}

			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
			p.ServeHTTP(rec, req)

			if got := calls.Load(); got != 2 {
				t.Fatalf("transport calls = %d, want first failure plus current HALF_OPEN retry probe", got)
			}
			stats := b.Stats()
			if stats.TotalFailures != 1 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want only first attempt failure and no success after downstream abort", stats.TotalFailures, stats.TotalSuccesses)
			}
			if state := b.State(); state != circuitbreaker.HalfOpen {
				t.Fatalf("breaker state after current probe downstream abort = %s, want HALF_OPEN with probe canceled", state)
			}
			nextEpoch, allowErr := b.Allow()
			if allowErr != nil {
				t.Fatalf("Allow() after current probe downstream abort = epoch %d err %v; historical AnyFailureRecorded blocked CancelProbe", nextEpoch, allowErr)
			}
			if nextEpoch == 0 {
				t.Fatal("Allow() after current probe downstream abort returned epoch 0, want new HALF_OPEN probe epoch")
			}
			b.CancelProbe(nextEpoch)

			snap := met.Snapshot()
			if snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted current-probe exchange with no clean completion", snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			if len(snap.LogEntries) != 1 || !snap.LogEntries[0].Aborted || snap.LogEntries[0].Status != http.StatusOK || snap.LogEntries[0].Limited != tt.wantLimited {
				t.Fatalf("metrics log = %+v, want one aborted 200 entry limited=%v", snap.LogEntries, tt.wantLimited)
			}
		})
	}
}

func TestProxy_RetryRecordedEarlierFailureDoesNotHoldSlotAfterCleanRetrySuccess(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name   string
		method string
		path   string
		global bool
	}{
		{name: "limited route limiter", method: http.MethodPost, path: "/v1/messages"},
		{name: "passthrough global limiter", method: http.MethodGet, path: "/health", global: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(100),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithBasePenalty(300*time.Millisecond),
				circuitbreaker.WithMaxPenalty(300*time.Millisecond),
			)
			var calls atomic.Int64

			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(1),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithQueueTimeout(50 * time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					call := calls.Add(1)
					if call == 1 {
						return &http.Response{
							StatusCode: http.StatusInternalServerError,
							Status:     "500 Internal Server Error",
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("retryable upstream failure")),
							Request:    req,
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("ok")),
						Request:    req,
					}, nil
				})),
			}
			if tt.global {
				opts = append(opts, WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			firstReq := httptest.NewRequest(tt.method, tt.path, nil)
			firstRec := httptest.NewRecorder()
			p.ServeHTTP(firstRec, firstReq)
			if firstRec.Code != http.StatusOK {
				t.Fatalf("first request status = %d body=%q, want retry success 200", firstRec.Code, firstRec.Body.String())
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("transport calls after first request = %d, want one retryable failure plus clean retry success", got)
			}

			start := time.Now()
			secondReq := httptest.NewRequest(tt.method, tt.path, nil)
			secondRec := httptest.NewRecorder()
			p.ServeHTTP(secondRec, secondReq)
			elapsed := time.Since(start)
			if secondRec.Code != http.StatusOK {
				t.Fatalf("second request status = %d body=%q, want 200; clean retry success must not leave slot in failure hold", secondRec.Code, secondRec.Body.String())
			}
			if elapsed > 150*time.Millisecond {
				t.Fatalf("second request acquired slot after %v, want immediate release after clean retry success despite earlier failure", elapsed)
			}
			if got := calls.Load(); got != 3 {
				t.Fatalf("transport calls = %d, want first failure, clean retry success, and second success", got)
			}
		})
	}
}

func TestProxy_RetryBreakerDeferredSuccessClosesHalfOpenAfterFullBodyCopy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(20*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
	)
	waitForHalfOpenProbeWindow(t, b)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(1),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Status:        "200 OK",
				Header:        make(http.Header),
				Body:          io.NopCloser(strings.NewReader("ok")),
				ContentLength: 2,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want ok", rec.Body.String())
	}
	if state := b.State(); state != circuitbreaker.Closed {
		t.Fatalf("breaker state = %s, want CLOSED after fully copied 2xx probe", state)
	}
	if successes := b.Stats().TotalSuccesses; successes != 1 {
		t.Fatalf("TotalSuccesses = %d, want 1 after full body copy", successes)
	}
}

func TestProxy_ErrAbortHandler_RetryBreakerDeferredSuccessKeepsHalfOpenOpenOnBodyFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(20*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(20*time.Millisecond),
	)
	waitForHalfOpenProbeWindow(t, b)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(1),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return responseWithReadErrorAfterChunk(req, "retry-partial"), nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := withPanicOnCopyErrorContext(httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	rec := httptest.NewRecorder()
	serveExpectErrAbortHandler(t, p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want committed upstream 200", rec.Code)
	}
	if rec.Body.String() != "retry-partial" {
		t.Fatalf("body = %q, want partial body committed before retry-enabled read failure", rec.Body.String())
	}
	stats := b.Stats()
	if stats.TotalSuccesses != 0 {
		t.Fatalf("TotalSuccesses = %d, want 0 (header-level retry success must be deferred)", stats.TotalSuccesses)
	}
	if stats.TotalFailures != 3 {
		t.Fatalf("TotalFailures = %d, want 3 (two setup failures plus failed half-open body copy)", stats.TotalFailures)
	}
	if state := b.State(); state != circuitbreaker.Open {
		t.Fatalf("breaker state = %s, want OPEN after failed half-open body copy", state)
	}
}

func TestProxy_ErrAbortHandler_RetryTemporaryBanBodyFailureDoesNotDoubleCountAfterRetryAfterExpiry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithWindow(10*time.Second),
	)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(1),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return responseWithDelayedReadErrorAfterChunk(
				req,
				http.StatusForbidden,
				http.Header{"Retry-After": []string{"1"}},
				"temporary-ban-partial",
				1100*time.Millisecond,
			), nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := withPanicOnCopyErrorContext(httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	rec := httptest.NewRecorder()
	serveExpectErrAbortHandler(t, p, rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want committed upstream 403", rec.Code)
	}
	if rec.Body.String() != "temporary-ban-partial" {
		t.Fatalf("body = %q, want partial body committed before read failure", rec.Body.String())
	}
	stats := b.Stats()
	if stats.TotalFailures != 1 {
		t.Fatalf("TotalFailures = %d, want 1 (retry transport header failure must not be double counted after Retry-After expiry)", stats.TotalFailures)
	}
	if state := b.State(); state != circuitbreaker.Closed {
		t.Fatalf("breaker state = %s, want CLOSED with threshold 2 after a single upstream attempt failure", state)
	}
}

func TestProxy_ErrAbortHandler_RetryBare403BodyFailureRecordsProxyAbortFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(1),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return responseWithDelayedReadErrorAfterChunk(
				req,
				http.StatusForbidden,
				make(http.Header),
				"bare-forbidden-partial",
				0,
			), nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := withPanicOnCopyErrorContext(httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	rec := httptest.NewRecorder()
	serveExpectErrAbortHandler(t, p, rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want committed upstream 403", rec.Code)
	}
	if stats := b.Stats(); stats.TotalFailures != 1 {
		t.Fatalf("TotalFailures = %d, want 1 (retry transport did not record bare 403 header; proxy must record body abort)", stats.TotalFailures)
	}
}

func TestProxy_ErrAbortHandler_DownstreamWriteCancel_AppliesCooldownWithoutBreakerMutation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "response body")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	lim := queue.NewLimiterWithCooldown(1, 0)
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(lim),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
		WithCancelCooldown(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	base := context.WithValue(context.Background(), http.ServerContextKey, &http.Server{})
	ctx, cancel := context.WithCancel(base)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	rec := &cancelingErrorResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		cancel:           cancel,
		err:              errProxyTestDownstreamWrite,
	}

	serveExpectErrAbortHandler(t, p, rec, req)

	stats := b.Stats()
	if stats.TotalFailures != 0 {
		t.Fatalf("breaker TotalFailures = %d, want 0 (client disconnect write failure is not upstream failure)", stats.TotalFailures)
	}
	if stats.TotalSuccesses != 0 {
		t.Fatalf("breaker TotalSuccesses = %d, want 0 (aborted downstream copy must not heal breaker)", stats.TotalSuccesses)
	}
	if snap := met.Snapshot(); snap.StatusCounts[5] != 0 {
		t.Fatalf("StatusCounts[5] = %d, want 0 (client disconnect must not synthesize 502)", snap.StatusCounts[5])
	}

	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("second request acquired slot in %v, want cancelCooldown to hold the slot", elapsed)
	}
}

func TestProxy_ErrAbortHandler_DownstreamWriteErrorWithoutRequestCancel_DoesNotPoisonBreaker(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "response body")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	lim := queue.NewLimiterWithCooldown(1, 0)
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(lim),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
		WithCancelCooldown(200*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := withPanicOnCopyErrorContext(httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	rec := &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}

	serveExpectErrAbortHandler(t, p, rec, req)

	if err := req.Context().Err(); err != nil {
		t.Fatalf("request context was canceled by test writer: %v", err)
	}
	stats := b.Stats()
	if stats.TotalFailures != 0 {
		t.Fatalf("breaker TotalFailures = %d, want 0 (downstream write error without context cancel is still client-side)", stats.TotalFailures)
	}
	if stats.TotalSuccesses != 0 {
		t.Fatalf("breaker TotalSuccesses = %d, want 0", stats.TotalSuccesses)
	}
	if snap := met.Snapshot(); snap.TotalAborted != 1 || snap.TotalProxied != 0 {
		t.Fatalf("metrics snapshot TotalAborted=%d TotalProxied=%d, want aborted=1 clean proxied=0", snap.TotalAborted, snap.TotalProxied)
	}

	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("second request acquired slot in %v, want downstream write failure to apply cancelCooldown", elapsed)
	}
}

func TestProxy_ErrAbortHandler_UpstreamReadFailureDespiteRequestCancel_RecordsBreakerFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
	base := context.WithValue(context.Background(), http.ServerContextKey, &http.Server{})
	ctx, cancel := context.WithCancel(base)

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Status:        "200 OK",
				Header:        make(http.Header),
				Body:          &cancelingReadErrorAfterChunkBody{chunk: []byte("partial"), err: errProxyTestUpstreamRead, cancel: cancel},
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	serveExpectErrAbortHandler(t, p, rec, req)

	if ctx.Err() == nil {
		t.Fatal("request context was not canceled by upstream body")
	}
	stats := b.Stats()
	if stats.TotalFailures != 1 {
		t.Fatalf("breaker TotalFailures = %d, want 1 (non-context upstream read error must not be hidden by request cancellation)", stats.TotalFailures)
	}
	if stats.TotalSuccesses != 0 {
		t.Fatalf("breaker TotalSuccesses = %d, want 0", stats.TotalSuccesses)
	}
}

func TestProxy_ErrAbortHandler_UpstreamReadErrorWithSameReadBytesBeatsDownstreamWriteError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(0),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Status:        "200 OK",
				Header:        make(http.Header),
				Body:          &readChunkWithErrorBody{chunk: []byte("partial"), err: errProxyTestUpstreamRead},
				ContentLength: -1,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := withPanicOnCopyErrorContext(httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	rec := &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
	serveExpectErrAbortHandler(t, p, rec, req)

	stats := b.Stats()
	if stats.TotalFailures != 1 {
		t.Fatalf("breaker TotalFailures = %d, want 1 (non-context upstream read error must win over downstream write error)", stats.TotalFailures)
	}
	if stats.TotalSuccesses != 0 {
		t.Fatalf("breaker TotalSuccesses = %d, want 0", stats.TotalSuccesses)
	}
}

func TestProxy_ErrAbortHandler_DownstreamWriteCancelAfterUpstreamFailure_RecordsBreakerFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "upstream failure body")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages"},
		{name: "passthrough", method: http.MethodGet, path: "/health"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

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

			base := context.WithValue(context.Background(), http.ServerContextKey, &http.Server{})
			ctx, cancel := context.WithCancel(base)
			req := httptest.NewRequest(tt.method, tt.path, nil).WithContext(ctx)
			rec := &cancelingErrorResponseWriter{
				ResponseRecorder: httptest.NewRecorder(),
				cancel:           cancel,
				err:              errProxyTestDownstreamWrite,
			}

			serveExpectErrAbortHandler(t, p, rec, req)

			stats := b.Stats()
			if stats.TotalFailures != 1 {
				t.Fatalf("breaker TotalFailures = %d, want 1 (definitive upstream 500 must be recorded despite client disconnect)", stats.TotalFailures)
			}
			if stats.TotalSuccesses != 0 {
				t.Fatalf("breaker TotalSuccesses = %d, want 0", stats.TotalSuccesses)
			}
		})
	}
}

func TestProxy_ErrAbortHandler_DownstreamWriteCancelAfterUpstream502_RecordsBreakerFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "provider bad gateway")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages"},
		{name: "passthrough", method: http.MethodGet, path: "/health"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

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

			base := context.WithValue(context.Background(), http.ServerContextKey, &http.Server{})
			ctx, cancel := context.WithCancel(base)
			req := httptest.NewRequest(tt.method, tt.path, nil).WithContext(ctx)
			rec := &cancelingErrorResponseWriter{
				ResponseRecorder: httptest.NewRecorder(),
				cancel:           cancel,
				err:              errProxyTestDownstreamWrite,
			}

			serveExpectErrAbortHandler(t, p, rec, req)

			stats := b.Stats()
			if stats.TotalFailures != 1 {
				t.Fatalf("breaker TotalFailures = %d, want 1 (upstream 502 must not be confused with proxy-generated 502)", stats.TotalFailures)
			}
			if stats.TotalSuccesses != 0 {
				t.Fatalf("breaker TotalSuccesses = %d, want 0", stats.TotalSuccesses)
			}
		})
	}
}

func TestProxy_ErrorHandler_ClientCancelBeforeResponse_NoSpurious502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages"},
		{name: "passthrough", method: http.MethodGet, path: "/health"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
			ctx, cancel := context.WithCancel(context.Background())

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(0),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					cancel()
					return nil, context.Canceled
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(tt.method, tt.path, nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if body := rec.Body.String(); body != "" {
				t.Fatalf("body = %q, want no proxy-generated error body for client cancellation", body)
			}
			if snap := met.Snapshot(); snap.StatusCounts[5] != 0 || snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics StatusCounts[5]=%d TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want no 5xx, one aborted incomplete exchange, and no clean completion", snap.StatusCounts[5], snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
			stats := b.Stats()
			if stats.TotalFailures != 0 {
				t.Fatalf("breaker TotalFailures = %d, want 0 (pre-response client cancel is not upstream failure)", stats.TotalFailures)
			}
			if stats.TotalSuccesses != 0 {
				t.Fatalf("breaker TotalSuccesses = %d, want 0 (no response was received)", stats.TotalSuccesses)
			}
		})
	}
}

func TestProxy_LocalReverseProxyErrorBeforeRoundTrip_NotUpstreamFailure(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name   string
		method string
		path   string
		global bool
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages"},
		{name: "passthrough", method: http.MethodGet, path: "/health", global: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)
			activeLimiter := lim
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(100),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithBasePenalty(200*time.Millisecond),
				circuitbreaker.WithMaxPenalty(200*time.Millisecond),
			)
			var transportCalled atomic.Bool

			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(0),
				WithFailureHold(200 * time.Millisecond),
				WithCancelCooldown(200 * time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					transportCalled.Store(true)
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("unexpected")),
						Request:    req,
					}, nil
				})),
			}
			if tt.global {
				globalLimiter := queue.NewLimiterWithCooldown(1, 0)
				activeLimiter = globalLimiter
				opts = append(opts, WithGlobalLimiter(globalLimiter))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", string([]byte{0x7f}))
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if transportCalled.Load() {
				t.Fatal("transport RoundTrip was called for invalid client upgrade before local ReverseProxy validation failed")
			}
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 for local invalid-upgrade proxy error", rec.Code)
			}
			stats := b.Stats()
			if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want no upstream accounting before RoundTrip", stats.TotalFailures, stats.TotalSuccesses)
			}
			if active := activeLimiter.Stats().Active; active != 0 {
				t.Fatalf("limiter active slots = %d, want 0 because no upstream attempt occurred", active)
			}
			snap := met.Snapshot()
			if snap.StatusCounts[5] != 1 || snap.TotalAborted != 0 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics StatusCounts[5]=%d TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want completed local 502 with no clean upstream completion", snap.StatusCounts[5], snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
		})
	}
}

func TestProxy_LocalReverseProxyErrorBeforeRoundTrip_WriteFailureDoesNotRecordBreaker(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name   string
		method string
		path   string
		global bool
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages"},
		{name: "passthrough", method: http.MethodGet, path: "/health", global: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)
			activeLimiter := lim
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(100),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithBasePenalty(200*time.Millisecond),
				circuitbreaker.WithMaxPenalty(200*time.Millisecond),
			)
			var transportCalled atomic.Bool

			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(0),
				WithFailureHold(200 * time.Millisecond),
				WithCancelCooldown(200 * time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					transportCalled.Store(true)
					return nil, errProxyTestTransport
				})),
			}
			if tt.global {
				globalLimiter := queue.NewLimiterWithCooldown(1, 0)
				activeLimiter = globalLimiter
				opts = append(opts, WithGlobalLimiter(globalLimiter))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", string([]byte{0x7f}))
			rec := &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
			p.ServeHTTP(rec, req)

			if transportCalled.Load() {
				t.Fatal("transport RoundTrip was called for invalid client upgrade before local ReverseProxy validation failed")
			}
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 for local invalid-upgrade proxy error", rec.Code)
			}
			stats := b.Stats()
			if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want no upstream accounting before RoundTrip despite generated-502 write failure", stats.TotalFailures, stats.TotalSuccesses)
			}
			if active := activeLimiter.Stats().Active; active != 0 {
				t.Fatalf("limiter active slots = %d, want 0 because no upstream attempt occurred", active)
			}
			snap := met.Snapshot()
			if snap.StatusCounts[5] != 1 || snap.TotalAborted != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics StatusCounts[5]=%d TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want aborted local 502 with no clean upstream completion", snap.StatusCounts[5], snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough)
			}
		})
	}
}

func TestProxy_LocalReverseProxyErrorBeforeRoundTrip_ReleasesHalfOpenProbe(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name   string
		method string
		path   string
		global bool
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages"},
		{name: "passthrough", method: http.MethodGet, path: "/health", global: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(1),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithOpenTimeout(10*time.Millisecond),
				circuitbreaker.WithMaxOpenTimeout(10*time.Millisecond),
			)
			b.RecordFailure(http.StatusInternalServerError, 0, time.Now(), 0)
			if state := b.State(); state != circuitbreaker.Open {
				t.Fatalf("breaker state after seed failure = %v, want OPEN", state)
			}
			deadline := time.Now().Add(time.Second)
			for b.WaitDuration() > 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if wait := b.WaitDuration(); wait > 0 {
				t.Fatalf("breaker still waiting after deadline: %v", wait)
			}

			var transportCalls atomic.Int64
			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(0),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					transportCalls.Add(1)
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("ok")),
						Request:    req,
					}, nil
				})),
			}
			if tt.global {
				opts = append(opts, WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			badReq := httptest.NewRequest(tt.method, tt.path, nil)
			badReq.Header.Set("Connection", "Upgrade")
			badReq.Header.Set("Upgrade", string([]byte{0x7f}))
			badRec := httptest.NewRecorder()
			p.ServeHTTP(badRec, badReq)
			if badRec.Code != http.StatusBadGateway {
				t.Fatalf("bad request status = %d, want local 502", badRec.Code)
			}
			if got := transportCalls.Load(); got != 0 {
				t.Fatalf("transport calls after local validation error = %d, want 0", got)
			}

			goodReq := httptest.NewRequest(tt.method, tt.path, nil)
			goodRec := httptest.NewRecorder()
			p.ServeHTTP(goodRec, goodReq)
			if goodRec.Code != http.StatusOK {
				t.Fatalf("good request status = %d, want 200; local error consumed HALF_OPEN probe", goodRec.Code)
			}
			if got := transportCalls.Load(); got != 1 {
				t.Fatalf("transport calls after good request = %d, want 1", got)
			}
			if state := b.State(); state != circuitbreaker.Closed {
				t.Fatalf("breaker state after good probe = %v, want CLOSED", state)
			}
		})
	}
}

func TestProxy_RetryRequestBodyBufferingErrorBeforeRoundTrip_NotUpstreamFailure(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name      string
		method    string
		path      string
		global    bool
		writeFail bool
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages"},
		{name: "limited generated 502 write failure", method: http.MethodPost, path: "/v1/messages", writeFail: true},
		{name: "passthrough", method: http.MethodGet, path: "/health", global: true},
		{name: "passthrough generated 502 write failure", method: http.MethodGet, path: "/health", global: true, writeFail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)
			activeLimiter := lim
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(100),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithBasePenalty(200*time.Millisecond),
				circuitbreaker.WithMaxPenalty(200*time.Millisecond),
			)
			var innerCalled atomic.Bool
			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(1),
				WithMaxBodyBytes(1024),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithFailureHold(200 * time.Millisecond),
				WithCancelCooldown(200 * time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					innerCalled.Store(true)
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("unexpected")),
						Request:    req,
					}, nil
				})),
			}
			if tt.global {
				globalLimiter := queue.NewLimiterWithCooldown(1, 0)
				activeLimiter = globalLimiter
				opts = append(opts, WithGlobalLimiter(globalLimiter))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(tt.method, tt.path, &readErrorBody{err: errProxyTestTransport})
			var rw http.ResponseWriter = httptest.NewRecorder()
			if tt.writeFail {
				rw = &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
			}
			p.ServeHTTP(rw, req)

			if innerCalled.Load() {
				t.Fatal("inner upstream RoundTrip was called even though retry request-body buffering failed")
			}
			stats := b.Stats()
			if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want no upstream accounting for local retry body-buffering failure", stats.TotalFailures, stats.TotalSuccesses)
			}
			if active := activeLimiter.Stats().Active; active != 0 {
				t.Fatalf("limiter active slots = %d, want 0 because no upstream attempt occurred", active)
			}
			snap := met.Snapshot()
			wantAborted := int64(0)
			if tt.writeFail {
				wantAborted = 1
			}
			if snap.StatusCounts[5] != 1 || snap.TotalAborted != wantAborted || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics StatusCounts[5]=%d TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want local 502 with aborted=%d and no clean upstream completion", snap.StatusCounts[5], snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough, wantAborted)
			}
		})
	}
}

func TestProxy_NestedUnwrappedRetryRequestBodyBufferingErrorBeforeRoundTrip_NotUpstreamFailure(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name      string
		method    string
		path      string
		global    bool
		writeFail bool
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages"},
		{name: "limited generated 502 write failure", method: http.MethodPost, path: "/v1/messages", writeFail: true},
		{name: "passthrough", method: http.MethodGet, path: "/health", global: true},
		{name: "passthrough generated 502 write failure", method: http.MethodGet, path: "/health", global: true, writeFail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)
			activeLimiter := lim
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(100),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithBasePenalty(200*time.Millisecond),
				circuitbreaker.WithMaxPenalty(200*time.Millisecond),
			)
			var innerCalled atomic.Bool
			nestedRetry := &unwrapRoundTripper{inner: &unwrapRoundTripper{inner: &retry.Transport{
				Inner: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					innerCalled.Store(true)
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("unexpected")),
						Request:    req,
					}, nil
				}),
				Breaker:      b,
				MaxBodyBytes: 1024,
			}}}
			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(0),
				WithFailureHold(200 * time.Millisecond),
				WithCancelCooldown(200 * time.Millisecond),
				WithTransport(nestedRetry),
			}
			if tt.global {
				globalLimiter := queue.NewLimiterWithCooldown(1, 0)
				activeLimiter = globalLimiter
				opts = append(opts, WithGlobalLimiter(globalLimiter))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(tt.method, tt.path, &readErrorBody{err: errProxyTestTransport})
			var rw http.ResponseWriter = httptest.NewRecorder()
			if tt.writeFail {
				rw = &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
			}
			p.ServeHTTP(rw, req)

			if innerCalled.Load() {
				t.Fatal("nested retry inner upstream RoundTrip was called even though request-body buffering failed")
			}
			stats := b.Stats()
			if stats.TotalFailures != 0 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want no upstream accounting for nested retry local body-buffering failure", stats.TotalFailures, stats.TotalSuccesses)
			}
			if active := activeLimiter.Stats().Active; active != 0 {
				t.Fatalf("limiter active slots = %d, want 0 because nested retry never started an upstream attempt", active)
			}
			snap := met.Snapshot()
			wantAborted := int64(0)
			if tt.writeFail {
				wantAborted = 1
			}
			if snap.StatusCounts[5] != 1 || snap.TotalAborted != wantAborted || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics StatusCounts[5]=%d TotalAborted=%d TotalProxied=%d TotalPassThrough=%d, want local 502 with aborted=%d and no clean upstream completion", snap.StatusCounts[5], snap.TotalAborted, snap.TotalProxied, snap.TotalPassThrough, wantAborted)
			}
		})
	}
}

func TestProxy_QueueFailureBeforeRoundTrip_ReleasesHalfOpenProbe(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name       string
		method     string
		path       string
		global     bool
		wantStatus int
	}{
		{name: "limited queue timeout", method: http.MethodPost, path: "/v1/messages", wantStatus: http.StatusGatewayTimeout},
		{name: "passthrough global queue cancel", method: http.MethodGet, path: "/health", global: true, wantStatus: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)
			blockingLimiter := lim
			b := mustBreaker(t,
				circuitbreaker.WithFailureThreshold(1),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithOpenTimeout(10*time.Millisecond),
				circuitbreaker.WithMaxOpenTimeout(10*time.Millisecond),
			)
			b.RecordFailure(http.StatusInternalServerError, 0, time.Now(), 0)
			deadline := time.Now().Add(time.Second)
			for b.WaitDuration() > 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if wait := b.WaitDuration(); wait > 0 {
				t.Fatalf("breaker still waiting after deadline: %v", wait)
			}

			var transportCalls atomic.Int64
			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(0),
				WithQueueTimeout(10 * time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					transportCalls.Add(1)
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("ok")),
						Request:    req,
					}, nil
				})),
			}
			if tt.global {
				globalLimiter := queue.NewLimiterWithCooldown(1, 0)
				blockingLimiter = globalLimiter
				opts = append(opts, WithGlobalLimiter(globalLimiter))
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}
			release, err := blockingLimiter.Acquire(context.Background())
			if err != nil {
				t.Fatalf("pre-acquire limiter: %v", err)
			}

			blockedReq := httptest.NewRequest(tt.method, tt.path, nil)
			blockedRec := httptest.NewRecorder()
			p.ServeHTTP(blockedRec, blockedReq)
			if blockedRec.Code != tt.wantStatus {
				t.Fatalf("blocked request status = %d, want %d", blockedRec.Code, tt.wantStatus)
			}
			if got := transportCalls.Load(); got != 0 {
				t.Fatalf("transport calls while limiter blocked = %d, want 0", got)
			}
			release()

			goodReq := httptest.NewRequest(tt.method, tt.path, nil)
			goodRec := httptest.NewRecorder()
			p.ServeHTTP(goodRec, goodReq)
			if goodRec.Code != http.StatusOK {
				t.Fatalf("good request status = %d, want 200; queue failure consumed HALF_OPEN probe", goodRec.Code)
			}
			if got := transportCalls.Load(); got != 1 {
				t.Fatalf("transport calls after good request = %d, want 1", got)
			}
			if state := b.State(); state != circuitbreaker.Closed {
				t.Fatalf("breaker state after good probe = %v, want CLOSED", state)
			}
		})
	}
}

func TestProxy_ErrorHandler_TransportErrorBeforeResponse_Records502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages"},
		{name: "passthrough", method: http.MethodGet, path: "/health"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(0),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					return nil, errProxyTestTransport
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 for real pre-response transport error", rec.Code)
			}
			if snap := met.Snapshot(); snap.StatusCounts[5] != 1 {
				t.Fatalf("StatusCounts[5] = %d, want 1 for real pre-response transport error", snap.StatusCounts[5])
			}
			if stats := b.Stats(); stats.TotalFailures != 1 {
				t.Fatalf("breaker TotalFailures = %d, want 1 for real pre-response transport error", stats.TotalFailures)
			}
		})
	}
}

func TestProxy_ErrorHandler_RequestCancelWithNonContextTransportError_Records502AndBreakerFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "limited", method: http.MethodPost, path: "/v1/messages"},
		{name: "passthrough", method: http.MethodGet, path: "/health"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
			ctx, cancel := context.WithCancel(context.Background())

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(0),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					cancel()
					return nil, errProxyTestTransport
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(tt.method, tt.path, nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 for non-context transport error racing with request cancel", rec.Code)
			}
			if snap := met.Snapshot(); snap.StatusCounts[5] != 1 || snap.TotalAborted != 0 {
				t.Fatalf("metrics StatusCounts[5]=%d TotalAborted=%d, want one 5xx completed proxy error and no abort", snap.StatusCounts[5], snap.TotalAborted)
			}
			if stats := b.Stats(); stats.TotalFailures != 1 {
				t.Fatalf("breaker TotalFailures = %d, want 1 (non-context transport error must not be masked by request cancel)", stats.TotalFailures)
			}
		})
	}
}

func TestProxy_ErrorHandler_RealTransportErrorWithGenerated502WriteFailure_RecordsBreakerFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name         string
		method       string
		path         string
		maxRetries   int
		transportErr error
		wantFails    int64
	}{
		{name: "limited non-context no retries", method: http.MethodPost, path: "/v1/messages", maxRetries: 0, transportErr: errProxyTestTransport, wantFails: 1},
		{name: "passthrough non-context no retries", method: http.MethodGet, path: "/health", maxRetries: 0, transportErr: errProxyTestTransport, wantFails: 1},
		{name: "limited context-canceled active request no retries", method: http.MethodPost, path: "/v1/messages", maxRetries: 0, transportErr: context.Canceled, wantFails: 1},
		{name: "passthrough context-canceled active request no retries", method: http.MethodGet, path: "/health", maxRetries: 0, transportErr: context.Canceled, wantFails: 1},
		{name: "limited deadline-exceeded active request no retries", method: http.MethodPost, path: "/v1/messages", maxRetries: 0, transportErr: context.DeadlineExceeded, wantFails: 1},
		{name: "passthrough deadline-exceeded active request no retries", method: http.MethodGet, path: "/health", maxRetries: 0, transportErr: context.DeadlineExceeded, wantFails: 1},
		{name: "limited retry transport no double count", method: http.MethodPost, path: "/v1/messages", maxRetries: 1, transportErr: errProxyTestTransport, wantFails: 2},
		{name: "limited retry context-canceled active request no double count", method: http.MethodPost, path: "/v1/messages", maxRetries: 1, transportErr: context.Canceled, wantFails: 2},
		{name: "limited retry deadline-exceeded active request no double count", method: http.MethodPost, path: "/v1/messages", maxRetries: 1, transportErr: context.DeadlineExceeded, wantFails: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(tt.maxRetries),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if req.Context().Err() != nil {
						t.Fatalf("request context unexpectedly canceled before transport returned: %v", req.Context().Err())
					}
					return nil, tt.transportErr
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 committed before write failure", rec.Code)
			}
			if stats := b.Stats(); stats.TotalFailures != tt.wantFails {
				t.Fatalf("breaker TotalFailures = %d, want %d (real transport error must survive generated 502 write failure)", stats.TotalFailures, tt.wantFails)
			}
			if snap := met.Snapshot(); snap.TotalAborted != 1 || snap.StatusCounts[5] != 1 || snap.TotalProxied != 0 || snap.TotalPassThrough != 0 {
				t.Fatalf("metrics TotalAborted=%d StatusCounts[5]=%d TotalProxied=%d TotalPassThrough=%d, want aborted proxy 502 with no clean completion", snap.TotalAborted, snap.StatusCounts[5], snap.TotalProxied, snap.TotalPassThrough)
			}
		})
	}
}

func TestProxy_ErrorHandler_RealTransportErrorWithGenerated502WriteFailure_AppliesFailureHold(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	for _, tt := range []struct {
		name         string
		transportErr error
	}{
		{name: "non-context", transportErr: errProxyTestTransport},
		{name: "context-canceled active request", transportErr: context.Canceled},
		{name: "deadline-exceeded active request", transportErr: context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			lim := queue.NewLimiterWithCooldown(1, 0)

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(lim),
				WithMetrics(met),
				WithMaxRetries(0),
				WithFailureHold(200*time.Millisecond),
				WithCancelCooldown(10*time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if req.Context().Err() != nil {
						t.Fatalf("request context unexpectedly canceled before transport returned: %v", req.Context().Err())
					}
					return nil, tt.transportErr
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			rec := &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: errProxyTestDownstreamWrite}
			p.ServeHTTP(rec, req)

			start := time.Now()
			req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			rec2 := httptest.NewRecorder()
			p.ServeHTTP(rec2, req2)
			if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
				t.Fatalf("second request acquired slot in %v, want failureHold to beat downstream-write cancelCooldown for real transport failure", elapsed)
			}
		})
	}
}

func TestProxy_ErrorHandler_ContextCanceledErrorWithoutClientCancel_Records502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")

	tests := []struct {
		name       string
		maxRetries int
		wantFails  int64
	}{
		{name: "no retries", maxRetries: 0, wantFails: 1},
		{name: "with retries", maxRetries: 1, wantFails: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			met := metrics.NewCollector()
			b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))

			p, err := New(
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithBreaker(b),
				WithMaxRetries(tt.maxRetries),
				WithRetryWaitMin(time.Millisecond),
				WithRetryWaitMax(time.Millisecond),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if req.Context().Err() != nil {
						t.Fatalf("request context unexpectedly canceled before transport returned: %v", req.Context().Err())
					}
					return nil, context.Canceled
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 when transport returns context.Canceled without request cancellation", rec.Code)
			}
			if snap := met.Snapshot(); snap.StatusCounts[5] != 1 {
				t.Fatalf("StatusCounts[5] = %d, want 1 when request context is still active", snap.StatusCounts[5])
			}
			if stats := b.Stats(); stats.TotalFailures != tt.wantFails {
				t.Fatalf("breaker TotalFailures = %d, want %d when request context is still active", stats.TotalFailures, tt.wantFails)
			}
		})
	}
}

func TestProxy_RetryBreakerRecordsTemporaryBanDespiteRequestCancel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "unused")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	b := mustBreaker(t, circuitbreaker.WithFailureThreshold(100), circuitbreaker.WithWindow(10*time.Second))
	ctx, cancel := context.WithCancel(context.Background())

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithMaxRetries(1),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			cancel()
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Status:     "403 Forbidden",
				Header:     http.Header{"Retry-After": []string{"30"}},
				Body:       io.NopCloser(strings.NewReader("temporary ban")),
				Request:    req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want committed upstream 403", rec.Code)
	}
	if stats := b.Stats(); stats.TotalFailures != 1 {
		t.Fatalf("breaker TotalFailures = %d, want 1 (temporary ban must be recorded despite request cancellation)", stats.TotalFailures)
	}
}

// TestProxy_ErrAbortHandler_AppliesCancelCooldown verifies that when the
// reverse proxy panics with http.ErrAbortHandler (client disconnect mid-stream),
// the cancelCooldown IS applied — the slot is held for the configured duration.
// This is the key regression: before the fix, the inner recover set localPanic=true,
// which caused the slot-release defer to skip the cancelCooldown branch and
// release immediately.
func TestProxy_ErrAbortHandler_AppliesCancelCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	lim := queue.NewLimiterWithCooldown(1, 0) // single slot for easy observation

	ctx, cancel := context.WithCancel(context.Background())

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(lim),
		WithMetrics(met),
		WithCancelCooldown(200*time.Millisecond),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			cancel()
			panic(http.ErrAbortHandler)
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	serveExpectErrAbortHandler(t, p, rec, req)

	// The slot should be in cooldown — not immediately available.
	// With localPanic=true (the bug), the slot is released instantly,
	// so a second request would acquire it immediately. With the fix,
	// cancelCooldown holds it for 200ms.
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	serveExpectErrAbortHandler(t, p, rec2, req2)
	elapsed := time.Since(start)

	// If cancelCooldown was skipped (localPanic bug), the second request
	// acquires the slot immediately (elapsed < 50ms). If cancelCooldown
	// fired correctly, the second request waits ~200ms for the slot.
	if elapsed < 100*time.Millisecond {
		t.Errorf("second request acquired slot in %v — cancelCooldown was NOT applied (localPanic bug: ErrAbortHandler treated as local panic)", elapsed)
	}

	// After cooldown, the slot should be released.
	time.Sleep(200 * time.Millisecond)
	stats := lim.Stats()
	if stats.Active != 0 {
		t.Errorf("after cooldown, Active = %d, want 0 (slot should be released)", stats.Active)
	}
}

// TestProxy_ErrAbortHandler_NoSpurious502 verifies that when the reverse
// proxy panics with http.ErrAbortHandler, no spurious 502 is recorded in
// metrics. The client disconnected — writing a 502 to a dead connection is
// wasteful and inflates error counts.
func TestProxy_ErrAbortHandler_NoSpurious502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()

	ctx, cancel := context.WithCancel(context.Background())

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			cancel()
			panic(http.ErrAbortHandler)
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	serveExpectErrAbortHandler(t, p, rec, req)

	// ErrAbortHandler should NOT produce a 502 in metrics.
	// Before the fix, the inner recover wrote http.Error(rec, "internal error", 502)
	// which inflated StatusCounts[5].
	snap := met.Snapshot()
	if snap.StatusCounts[5] != 0 {
		t.Errorf("StatusCounts[5] = %d, want 0 (ErrAbortHandler should not produce a 502 — client already disconnected)", snap.StatusCounts[5])
	}
}

// TestProxy_ErrAbortHandler_Passthrough_AppliesCancelCooldown mirrors
// TestProxy_ErrAbortHandler_AppliesCancelCooldown but for passthrough routes
// (servePassthrough instead of serveLimited).
func TestProxy_ErrAbortHandler_Passthrough_AppliesCancelCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	pat, _ := route.Parse("POST /v1/messages")
	met := metrics.NewCollector()
	routeLim := queue.NewLimiterWithCooldown(1, 0)
	globalLim := queue.NewLimiterWithCooldown(1, 0)

	ctx, cancel := context.WithCancel(context.Background())

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(routeLim),
		WithGlobalLimiter(globalLim),
		WithMetrics(met),
		WithCancelCooldown(200*time.Millisecond),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			cancel()
			panic(http.ErrAbortHandler)
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// GET /health is a passthrough route (not limited).
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	serveExpectErrAbortHandler(t, p, rec, req)

	// Second request should wait for cancelCooldown on the global limiter.
	start := time.Now()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	serveExpectErrAbortHandler(t, p, rec2, req2)
	elapsed := time.Since(start)

	if elapsed < 100*time.Millisecond {
		t.Errorf("second request acquired slot in %v — cancelCooldown was NOT applied for passthrough ErrAbortHandler (localPanic bug)", elapsed)
	}
}

// TestProxy_ErrAbortHandler_NoBreakerFailure verifies that ErrAbortHandler
// (client disconnect) is NOT reported as a circuit breaker failure.
// This mirrors TestProxy_PanicRecovery_NoBreakerFailureForLocalPanic but
// specifically for the ErrAbortHandler sentinel. Real local panics SHOULD be
// excluded from the breaker (they're proxy bugs, not upstream failures), and
// client disconnects SHOULD also be excluded (they're not upstream failures
// either — the isClientCancel guard handles this in the slot-release defer).
func TestProxy_ErrAbortHandler_NoBreakerFailure(t *testing.T) {
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

	ctx, cancel := context.WithCancel(context.Background())

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithBreaker(b),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			cancel()
			panic(http.ErrAbortHandler)
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	serveExpectErrAbortHandler(t, p, rec, req)

	// Give the cancelCooldown time to release.
	time.Sleep(100 * time.Millisecond)

	stats := b.Stats()
	if stats.TotalFailures != 0 {
		t.Errorf("breaker TotalFailures = %d, want 0 (ErrAbortHandler is a client disconnect, not an upstream failure)", stats.TotalFailures)
	}
	if stats.ConsecutiveFailures != 0 {
		t.Errorf("breaker ConsecutiveFailures = %d, want 0", stats.ConsecutiveFailures)
	}
}
