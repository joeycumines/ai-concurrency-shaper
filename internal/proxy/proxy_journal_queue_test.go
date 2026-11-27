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
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

func TestProxy_QueueEndAfterGlobalLimiter(t *testing.T) {
	// Upstream holds the first request long enough for the second request to
	// observe a deterministic global-limiter queue wait.
	firstEntered := make(chan struct{})
	var firstOnce sync.Once
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstOnce.Do(func() { close(firstEntered) })
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
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()

	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached upstream")
	}

	// Second request gets the per-route slot immediately (limit=2),
	// but must wait for the global slot.
	req2 := httptest.NewRequest("POST", "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	<-firstDone

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

func TestProxy_QueueDurationOnTimeout(t *testing.T) {
	// Verify that a limited request which times out waiting for
	// acquireSlot records a non-zero QueueDuration — the actual time
	// spent waiting in the queue, not 0.
	firstEntered := make(chan struct{})
	release := make(chan struct{})
	var firstOnce sync.Once
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstOnce.Do(func() { close(firstEntered) })
		<-release
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

	// First request holds the slot until released.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()
	t.Cleanup(func() {
		close(release)
		<-firstDone
	})

	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached upstream")
	}

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
	firstEntered := make(chan struct{})
	release := make(chan struct{})
	var firstOnce sync.Once
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstOnce.Do(func() { close(firstEntered) })
		<-release
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

	// First request holds the global slot until released.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()
	t.Cleanup(func() {
		close(release)
		<-firstDone
	})

	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached upstream")
	}

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
	firstEntered := make(chan struct{})
	var firstOnce sync.Once
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstOnce.Do(func() { close(firstEntered) })
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
	globalLim := queue.NewLimiterWithCooldown(1, 0)
	p, err := New(
		WithUpstream(slowURL),
		WithMatcher(route.NewMatcher(patterns)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(0), // no queue timeout — we cancel manually
		WithGlobalLimiter(globalLim),
		WithJournal(j),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// First passthrough request holds the global slot.
	// Give it a cancellable context so we can clean up the goroutine.
	firstCtx, firstCancel := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest("GET", "/health", nil).WithContext(firstCtx)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
	}()
	t.Cleanup(func() {
		firstCancel()
		<-firstDone
	})

	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached upstream")
	}

	// Second passthrough request will block in globalLimiter.Acquire.
	// Wait until it is actually queued, then hold briefly before cancelling.
	ctx, cancel := context.WithCancel(context.Background())
	req2 := httptest.NewRequest("GET", "/health", nil)
	req2 = req2.WithContext(ctx)
	rec2 := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec2, req2)
		close(done)
	}()

	waitDeadline := time.Now().Add(5 * time.Second)
	for globalLim.Stats().Waiters < 1 && time.Now().Before(waitDeadline) {
		time.Sleep(time.Millisecond)
	}
	if globalLim.Stats().Waiters < 1 {
		cancel()
		t.Fatal("second request never queued in global limiter")
	}

	// Let it wait in the global limiter for at least 50ms once queued.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Good.
	case <-time.After(5 * time.Second):
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
}
