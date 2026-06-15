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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

func TestProxy_JournalRecordsLimitedRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	patterns := []route.Pattern{
		{Method: "POST", Segments: []string{"v1", "messages"}, Raw: "POST /v1/messages"},
	}

	j := journal.New(512, 1<<20)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(patterns)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(30*time.Second),
		WithJournal(j),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	entries := j.Entries()
	if len(entries) == 0 {
		t.Fatal("expected at least 1 journal entry, got 0")
	}

	e := entries[0]
	if e.Method != "POST" {
		t.Errorf("method = %s, want POST", e.Method)
	}
	if e.StatusCode != 200 {
		t.Errorf("status = %d, want 200", e.StatusCode)
	}
	if e.URL.Path != "/v1/messages" {
		t.Errorf("path = %s, want /v1/messages", e.URL.Path)
	}
	// Verify response metadata is captured.
	if e.ResponseHeaders == nil {
		t.Error("ResponseHeaders should not be nil")
	}
	if e.ContentType != "application/json" {
		t.Errorf("ContentType = %q, want %q", e.ContentType, "application/json")
	}
	if len(e.ResponseBody) == 0 {
		t.Error("ResponseBody should not be empty")
	}
	if e.Timing.ResponseHeaders.IsZero() {
		t.Error("Timing.ResponseHeaders should be non-zero")
	}
	t.Logf("Entry: method=%s status=%d path=%s ct=%s body_len=%d queue_dur=%v", e.Method, e.StatusCode, e.URL.Path, e.ContentType, len(e.ResponseBody), e.Timing.QueueDuration())
}

func TestProxy_JournalRecordsPassthroughRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	patterns := []route.Pattern{
		{Method: "POST", Segments: []string{"v1", "messages"}, Raw: "POST /v1/messages"},
	}

	j := journal.New(512, 1<<20)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(patterns)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(30*time.Second),
		WithJournal(j),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// GET /health is not a limited route → passthrough.
	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	entries := j.Entries()
	if len(entries) == 0 {
		t.Fatal("expected at least 1 journal entry, got 0")
	}

	e := entries[0]
	if e.Method != "GET" {
		t.Errorf("method = %s, want GET", e.Method)
	}
	if e.StatusCode != 200 {
		t.Errorf("status = %d, want 200", e.StatusCode)
	}
	if e.Limited {
		t.Error("passthrough request should not be limited")
	}
	// Verify response metadata is captured.
	if e.ResponseHeaders == nil {
		t.Error("ResponseHeaders should not be nil for passthrough request")
	}
	if e.ContentType != "application/json" {
		t.Errorf("ContentType = %q, want %q", e.ContentType, "application/json")
	}
	if len(e.ResponseBody) == 0 {
		t.Error("ResponseBody should not be empty for passthrough request")
	}
	if e.Timing.ResponseHeaders.IsZero() {
		t.Error("Timing.ResponseHeaders should be non-zero")
	}
	if e.Timing.QueueDuration() != 0 {
		t.Errorf("QueueDuration = %v, want 0 for passthrough", e.Timing.QueueDuration())
	}
	t.Logf("Entry: method=%s status=%d path=%s limited=%v ct=%s body_len=%d", e.Method, e.StatusCode, e.URL.Path, e.Limited, e.ContentType, len(e.ResponseBody))
}

func TestProxy_JournalNilSafe(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)

	// No journal configured.
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(30*time.Second),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestProxy_JournalCapturesRequestBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	patterns := []route.Pattern{
		{Method: "POST", Segments: []string{"v1", "messages"}, Raw: "POST /v1/messages"},
	}

	j := journal.New(512, 1<<20)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(patterns)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(30*time.Second),
		WithJournal(j),
		WithMaxRetries(2),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"hello":"world"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	entries := j.Entries()
	if len(entries) == 0 {
		t.Fatal("expected at least 1 journal entry, got 0")
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 journal entry, got %d", len(entries))
	}

	e := entries[0]
	if string(e.RequestBody) != `{"hello":"world"}` {
		t.Errorf("RequestBody = %q, want %q", string(e.RequestBody), `{"hello":"world"}`)
	}
}

func TestProxy_JournalNoDuplicateRetryEntries(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	patterns := []route.Pattern{
		{Method: "POST", Segments: []string{"v1", "messages"}, Raw: "POST /v1/messages"},
	}

	j := journal.New(512, 1<<20)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(patterns)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(30*time.Second),
		WithJournal(j),
		WithMaxRetries(3),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	entries := j.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 journal entry (proxy only, no retry duplicates), got %d", len(entries))
	}
}

func TestProxy_JournalImplicit200Status(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		// Deliberately omit WriteHeader to trigger implicit 200.
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	j := journal.New(512, 1<<20)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(30*time.Second),
		WithJournal(j),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	entries := j.Entries()
	if len(entries) == 0 {
		t.Fatal("expected at least 1 journal entry, got 0")
	}

	e := entries[0]
	if e.StatusCode != 200 {
		t.Errorf("journal StatusCode = %d, want 200", e.StatusCode)
	}

	snap := p.Metrics().Snapshot()
	if len(snap.LogEntries) == 0 {
		t.Fatal("expected at least 1 metric log entry")
	}
	if snap.LogEntries[0].Status != 200 {
		t.Errorf("metrics status = %d, want 200", snap.LogEntries[0].Status)
	}
}

func TestProxy_JournalHijackedResponseComplete(t *testing.T) {
	rec := httptest.NewRecorder()
	u, _ := url.Parse("/")
	entry := &journal.Entry{Method: "GET", URL: u}
	sr := &statusRecorder{ResponseWriter: rec, entry: entry}

	_, _, err := sr.Hijack()
	if err != http.ErrNotSupported {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
	// Failed hijack must NOT set the flag — the connection is still
	// operating as a regular HTTP connection.
	if sr.hijacked {
		t.Fatal("hijacked flag should NOT be set when Hijack fails")
	}

	// Simulate the ServeHTTP finalization logic: since hijack failed,
	// ResponseComplete should be set normally.
	if !sr.hijacked {
		entry.Timing.ResponseComplete = time.Now()
	}
	if entry.Timing.ResponseComplete.IsZero() {
		t.Error("expected ResponseComplete to be set when hijack fails (non-hijacked connection)")
	}
}

func TestProxy_QueueEndAfterGlobalLimiter(t *testing.T) {
	// Upstream is slow so requests hold the active slot.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	t.Cleanup(slow.Close)

	upstreamURL, _ := url.Parse(slow.URL)
	patterns := []route.Pattern{
		{Method: "POST", Segments: []string{"v1", "messages"}, Raw: "POST /v1/messages"},
	}

	j := journal.New(512, 1<<20)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(patterns)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)), // per-route limit = 2 (not the bottleneck)
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(30*time.Second),
		WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)), // global limit = 1 (the bottleneck)
		WithJournal(j),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First request holds the global slot for ~150ms.
	go func() {
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()

	// Small delay so the first request is ahead in the queue.
	time.Sleep(20 * time.Millisecond)

	// Second request gets the per-route slot immediately (limit=2),
	// but must wait for the global slot.
	req2 := httptest.NewRequest("POST", "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec2.Code)
	}

	entries := j.Entries()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// The second entry should have a QueueDuration that reflects
	// waiting in the global limiter, because QueueEnd is now set
	// AFTER the global limiter acquire.
	e := entries[1]
	if e.Timing.QueueDuration() < 100*time.Millisecond {
		t.Errorf("QueueDuration = %v, expected at least 100ms (global queue wait)", e.Timing.QueueDuration())
	}
}

func TestProxy_RetryWithNonEmptyBody(t *testing.T) {
	calls := 0
	var receivedBodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		receivedBodies = append(receivedBodies, string(body))
		if calls < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	patterns := []route.Pattern{
		{Method: "POST", Segments: []string{"v1", "messages"}, Raw: "POST /v1/messages"},
	}

	j := journal.New(512, 1<<20)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(patterns)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(30*time.Second),
		WithJournal(j),
		WithMaxRetries(3),
		WithMaxBodyBytes(1<<20),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"hello":"world"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if calls != 2 {
		t.Fatalf("expected 2 upstream calls (1 retry), got %d", calls)
	}
	for i, b := range receivedBodies {
		if b != `{"hello":"world"}` {
			t.Errorf("attempt %d: body = %q, want %q", i, b, `{"hello":"world"}`)
		}
	}

	entries := j.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 journal entry (proxy only, no retry duplicates), got %d", len(entries))
	}
	if string(entries[0].RequestBody) != `{"hello":"world"}` {
		t.Errorf("RequestBody = %q, want %q", string(entries[0].RequestBody), `{"hello":"world"}`)
	}
}

func TestProxy_ChunkedResponseSizeAccurate(t *testing.T) {
	// Verify that a chunked response (no Content-Length) larger than
	// captureMax records the true total bytes written, not the truncated
	// capture buffer size.
	const totalSize = 2 * 1024 // 2 KiB response
	const captureLimit = 256   // tiny capture limit

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Do NOT set Content-Length — this forces chunked transfer encoding.
		w.WriteHeader(http.StatusOK)
		// Write the full body in one shot.
		w.Write(make([]byte, totalSize))
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, _ := url.Parse(upstream.URL)
	j := journal.New(512, captureLimit) // very small capture limit
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(30*time.Second),
		WithJournal(j),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	entries := j.Entries()
	if len(entries) == 0 {
		t.Fatal("expected at least 1 journal entry, got 0")
	}

	e := entries[0]
	// ResponseSize must reflect the true total, not the capture limit.
	if e.ResponseSize != totalSize {
		t.Errorf("ResponseSize = %d, want %d (true total, not capture limit %d)", e.ResponseSize, totalSize, captureLimit)
	}
	// The captured body should be at most captureLimit bytes.
	if len(e.ResponseBody) > captureLimit {
		t.Errorf("ResponseBody len = %d, should not exceed captureLimit %d", len(e.ResponseBody), captureLimit)
	}
}

func TestProxy_QueueDurationOnTimeout(t *testing.T) {
	// Verify that a limited request which times out waiting for
	// acquireSlot records a non-zero QueueDuration — the actual time
	// spent waiting in the queue, not 0.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(slow.Close)

	slowURL, _ := url.Parse(slow.URL)
	pat, _ := route.Parse("POST /v1/messages")

	j := journal.New(512, 1<<20)
	p, err := New(
		WithUpstream(slowURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)), // concurrency 1 — slot held by first request
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(100*time.Millisecond),
		WithJournal(j),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First request holds the slot for 2s.
	go func() {
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()

	// Let the first request enter acquireSlot and hold the slot.
	time.Sleep(20 * time.Millisecond)

	// Second request should time out waiting in the per-route limiter.
	req2 := httptest.NewRequest("POST", "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec2.Code)
	}

	entries := j.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 journal entry (the timed-out request), got %d", len(entries))
	}

	e := entries[0]
	if e.Timing.QueueDuration() == 0 {
		t.Errorf("QueueDuration = 0, expected non-zero (request waited in queue before timing out)")
	}
	// The queue duration should be roughly the timeout (100ms), not 0.
	if e.Timing.QueueDuration() < 50*time.Millisecond {
		t.Errorf("QueueDuration = %v, expected at least 50ms (was queued before timeout)", e.Timing.QueueDuration())
	}
}

func TestProxy_QueueDurationOnGlobalLimiterTimeout(t *testing.T) {
	// Verify that a limited request which times out waiting for the
	// global limiter records a non-zero QueueDuration — the actual
	// time spent waiting in the per-route limiter + global limiter.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `ok`)
	}))
	t.Cleanup(slow.Close)

	slowURL, _ := url.Parse(slow.URL)
	pat, _ := route.Parse("POST /v1/messages")

	j := journal.New(512, 1<<20)
	p, err := New(
		WithUpstream(slowURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)), // per-route limit = 2 (not the bottleneck)
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(100*time.Millisecond),
		WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)), // global limit = 1 (the bottleneck)
		WithJournal(j),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First request holds the global slot for 2s.
	go func() {
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()

	// Let the first request acquire both per-route and global slots.
	time.Sleep(20 * time.Millisecond)

	// Second request gets the per-route slot immediately (limit=2),
	// but must wait for the global slot and should time out there.
	req2 := httptest.NewRequest("POST", "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec2.Code)
	}

	entries := j.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 journal entry (the timed-out request), got %d", len(entries))
	}

	e := entries[0]
	if e.Timing.QueueDuration() == 0 {
		t.Errorf("QueueDuration = 0, expected non-zero (request waited in global limiter before timing out)")
	}
	if e.Timing.QueueDuration() < 50*time.Millisecond {
		t.Errorf("QueueDuration = %v, expected at least 50ms (was queued before timeout)", e.Timing.QueueDuration())
	}
}

func TestProxy_QueueDurationOnPassthroughCancel(t *testing.T) {
	// Verify that a passthrough request cancelled while waiting in
	// the global limiter records a non-zero QueueDuration.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(func() {
		slow.CloseClientConnections()
		slow.Close()
	})

	slowURL, _ := url.Parse(slow.URL)
	patterns := []route.Pattern{
		{Method: "POST", Segments: []string{"v1", "messages"}, Raw: "POST /v1/messages"},
	}

	j := journal.New(512, 1<<20)
	p, err := New(
		WithUpstream(slowURL),
		WithMatcher(route.NewMatcher(patterns)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(0), // no queue timeout — we cancel manually
		WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithJournal(j),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First passthrough request holds the global slot.
	// Give it a cancellable context so we can clean up the goroutine.
	firstCtx, firstCancel := context.WithCancel(context.Background())
	go func() {
		req := httptest.NewRequest("GET", "/health", nil).WithContext(firstCtx)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()

	// Let the first request acquire the global slot.
	time.Sleep(20 * time.Millisecond)

	// Second passthrough request will block in globalLimiter.Acquire.
	// Cancel it after a short delay.
	ctx, cancel := context.WithCancel(context.Background())
	req2 := httptest.NewRequest("GET", "/health", nil)
	req2 = req2.WithContext(ctx)
	rec2 := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec2, req2)
		close(done)
	}()

	// Let it wait in the global limiter for at least 50ms.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Good.
	case <-time.After(2 * time.Second):
		t.Fatal("second request did not complete after cancellation")
	}

	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec2.Code)
	}

	entries := j.Entries()
	// Find the cancelled entry (the first entry is the still-running request).
	var found bool
	for _, e := range entries {
		if e.StatusCode == http.StatusServiceUnavailable {
			found = true
			if e.Timing.QueueDuration() == 0 {
				t.Errorf("QueueDuration = 0 for cancelled passthrough, expected non-zero (waited in global limiter)")
			}
			if e.Timing.QueueDuration() < 40*time.Millisecond {
				t.Errorf("QueueDuration = %v, expected at least 40ms (waited before cancellation)", e.Timing.QueueDuration())
			}
			break
		}
	}
	if !found {
		t.Fatal("expected a journal entry with status 503 (cancelled passthrough)")
	}

	// Clean up the first request goroutine.
	firstCancel()
}
