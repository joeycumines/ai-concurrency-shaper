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

// committedStreamFixture: capacity-1 limited route; the upstream handler
// blocks the FIRST request on the gate (holding the slot) and serves later
// requests immediately, so a second streaming request commits the queue
// comments and waits for a slot.
type committedStreamFixture struct {
	p       *Proxy
	journal *journal.Journal
	gate    chan struct{}
	gateC   sync.Once
}

func newCommittedStreamFixture(t *testing.T, queueTimeout time.Duration) *committedStreamFixture {
	t.Helper()
	f := &committedStreamFixture{gate: make(chan struct{})}
	f.journal = journal.New(16, 1<<16)
	var mu sync.Mutex
	first := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()
		if isFirst {
			<-f.gate
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.gateC.Do(func() { close(f.gate) })
		upstream.Close()
	})
	pat, _ := route.Parse("POST /v1/messages:1")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithRouteLimiters(map[string]*queue.Limiter{
			pat.Raw: queue.NewLimiterWithCooldown(1, 0),
		}),
		WithMetrics(metrics.NewCollector()),
		WithJournal(f.journal),
		WithQueueComments(10*time.Millisecond),
		WithQueueTimeout(queueTimeout),
	)
	if err != nil {
		t.Fatal(err)
	}
	f.p = p
	return f
}

// holdSlot sends the slot-holding first request and waits until it is
// demonstrably active upstream.
func (f *committedStreamFixture) holdSlot(t *testing.T, met *metrics.Collector) {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("Accept", "application/json")
		f.p.ServeHTTP(rec, req)
		done <- rec.Code
	}()
	waitSnapshot(t, met, func(s metrics.Snapshot) bool { return s.Active == 1 })
	// Release the gate before waiting on the holder so the holder's
	// goroutine can finish even when the test fails early: cleanups run
	// last-in-first-out, so this cleanup precedes the fixture's own.
	t.Cleanup(func() {
		f.gateC.Do(func() { close(f.gate) })
		<-done
	})
}

// waitJournalEntry polls the journal until it holds at least want entries.
func waitJournalEntry(t *testing.T, j *journal.Journal) *journal.Entry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if entries := j.Entries(); len(entries) > 0 {
			return entries[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("no journal entry")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A committed -queue-comments stream whose queue wait expires must journal
// the wait as queue time: the commit's own 200 headers are written while the
// request is still waiting, so the journal's ResponseHeaders must never
// precede QueueStart, and QueueEnd must be set when admission ends.
func TestProxy_CommittedStreamQueueTimeoutJournalsQueuePhase(t *testing.T) {
	f := newCommittedStreamFixture(t, 150*time.Millisecond)
	f.holdSlot(t, f.p.Metrics())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Accept", "text/event-stream")
	f.p.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "queue-wait failed: timeout") {
		t.Fatalf("body = %q, want the failed-close comment", body)
	}

	entry := waitJournalEntry(t, f.journal)
	// The slot-holding first request journals first; select the queued
	// request's entry by ID.
	entries := f.journal.Entries()
	for _, e := range entries {
		if e.ID > entry.ID {
			entry = e
		}
	}
	if entry.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("queued request StatusCode = %d, want the 504 the failed close recorded (entries: %d)", entry.StatusCode, len(entries))
	}
	if entry.Timing.QueueStart.IsZero() {
		t.Fatal("QueueStart unset")
	}
	if entry.Timing.QueueEnd.IsZero() {
		t.Fatal("QueueEnd unset: the admission wait was journalled as zero queue time")
	}
	if qd := entry.Timing.QueueDuration(); qd < 75*time.Millisecond {
		t.Fatalf("QueueDuration = %v, want about the 150ms queue timeout", qd)
	}
	if entry.Timing.ResponseHeaders.Before(entry.Timing.QueueStart) {
		t.Fatalf("ResponseHeaders %v precedes QueueStart %v: the commit's headers were journalled before the queue phase began",
			entry.Timing.ResponseHeaders, entry.Timing.QueueStart)
	}
	if entry.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("StatusCode = %d, want the 504 the failed close recorded", entry.StatusCode)
	}
}

// A committed stream whose exchange completes normally keeps today's journal
// facts: the first post-commit status is the upstream's, stamped with its own
// ResponseHeaders time at or after the queue phase.
func TestProxy_CommittedStreamNormalExchangeJournalsUpstreamStatus(t *testing.T) {
	f := newCommittedStreamFixture(t, 10*time.Second)
	f.holdSlot(t, f.p.Metrics())

	secondDone := make(chan int, 1)
	rec := httptest.NewRecorder()
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("Accept", "text/event-stream")
		f.p.ServeHTTP(rec, req)
		secondDone <- rec.Code
	}()
	waitSnapshot(t, f.p.Metrics(), func(s metrics.Snapshot) bool { return s.Queued == 1 })

	f.gateC.Do(func() { close(f.gate) })
	if code := <-secondDone; code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", code, rec.Body.String())
	}

	entries := f.journal.Entries()
	deadline := time.Now().Add(2 * time.Second)
	for len(entries) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("expected two journal entries, got %d", len(entries))
		}
		time.Sleep(5 * time.Millisecond)
		entries = f.journal.Entries()
	}
	entry := entries[len(entries)-1]
	if entries[0].ID > entry.ID {
		entry = entries[0]
	}
	for _, e := range entries {
		if e.ID > entry.ID {
			entry = e
		}
	}
	if entry.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want the upstream 200", entry.StatusCode)
	}
	if entry.Timing.ResponseHeaders.Before(entry.Timing.QueueStart) {
		t.Fatalf("ResponseHeaders %v precedes QueueStart %v", entry.Timing.ResponseHeaders, entry.Timing.QueueStart)
	}
	if !entry.Timing.QueueEnd.IsZero() && entry.Timing.ResponseHeaders.Before(entry.Timing.QueueEnd) {
		t.Fatalf("ResponseHeaders %v precedes QueueEnd %v", entry.Timing.ResponseHeaders, entry.Timing.QueueEnd)
	}
}
