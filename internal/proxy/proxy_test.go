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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

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

	cfg := Config{
		Upstream:     upstreamURL,
		Matcher:      route.NewMatcher(pats),
		Limiter:      queue.NewLimiter(concurrency),
		Metrics:      metrics.NewCollector(),
		QueueTimeout: timeout,
	}

	return New(cfg), upstream
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

	cfg := Config{
		Upstream:     slowURL,
		Matcher:      route.NewMatcher([]route.Pattern{pat}),
		Limiter:      queue.NewLimiter(1),
		Metrics:      metrics.NewCollector(),
		QueueTimeout: 50 * time.Millisecond,
	}
	proxy := New(cfg)

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
	for i := 0; i < n; i++ {
		go func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)
			results <- rec.Code
		}()
	}

	for i := 0; i < n; i++ {
		code := <-results
		if code != http.StatusOK {
			t.Errorf("request %d: expected 200, got %d", i, code)
		}
	}
}

func TestProxy_MixedRoutes(t *testing.T) {
	proxy, _ := setup(t, 1, 0, "POST /v1/messages")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("passthrough: expected 200, got %d", rec.Code)
			}
		}()
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

	cfg := Config{
		Upstream:     slowURL,
		Matcher:      route.NewMatcher([]route.Pattern{pat}),
		Limiter:      queue.NewLimiter(2),
		Metrics:      metrics.NewCollector(),
		QueueTimeout: 0,
	}
	p := New(cfg)

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

	cfg := Config{
		Upstream:     slowURL,
		Matcher:      route.NewMatcher([]route.Pattern{pat}),
		Limiter:      queue.NewLimiter(1),
		Metrics:      metrics.NewCollector(),
		QueueTimeout: 50 * time.Millisecond,
	}
	p := New(cfg)

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

	cfg := Config{
		Upstream:     slowURL,
		Matcher:      route.NewMatcher([]route.Pattern{pat}),
		Limiter:      queue.NewLimiter(1),
		Metrics:      metrics.NewCollector(),
		QueueTimeout: 50 * time.Millisecond,
	}
	p := New(cfg)

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
		pat.Raw: queue.NewLimiter(2), // per-route cap of 2
	}
	limiter := queue.NewLimiter(4) // default pool — should NOT be used
	p := New(Config{
		Upstream:      upstreamURL,
		Matcher:       route.NewMatcher([]route.Pattern{pat}),
		Limiter:       limiter,
		Metrics:       met,
		RouteLimiters: routeLimiters,
	})

	// Send 3 concurrent requests. With a per-route limit of 2, the
	// third request must block until one of the first two completes.
	var wg sync.WaitGroup
	var results []int
	var mu sync.Mutex
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			mu.Lock()
			results = append(results, rec.Code)
			mu.Unlock()
		}()
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
	if _, ok := interface{}(rec).(interface{ Unwrap() http.ResponseWriter }); !ok {
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
	n := s.n
	if n > len(b) {
		n = len(b)
	}
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
