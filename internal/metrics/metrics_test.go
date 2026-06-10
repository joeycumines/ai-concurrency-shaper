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

package metrics

import (
	"sync"
	"testing"
	"time"
)

func TestCollector_BasicCounters(t *testing.T) {
	c := NewCollector()

	c.IncActive()
	c.IncActive()
	c.IncQueued()
	c.IncProxied()
	c.IncPassThrough()
	c.IncTimeout()

	s := c.Snapshot()
	if s.Active != 2 {
		t.Errorf("Active: got %d, want 2", s.Active)
	}
	if s.Queued != 1 {
		t.Errorf("Queued: got %d, want 1", s.Queued)
	}
	if s.TotalProxied != 1 {
		t.Errorf("TotalProxied: got %d, want 1", s.TotalProxied)
	}
	if s.TotalPassThrough != 1 {
		t.Errorf("TotalPassThrough: got %d, want 1", s.TotalPassThrough)
	}
	if s.TotalTimeout != 1 {
		t.Errorf("TotalTimeout: got %d, want 1", s.TotalTimeout)
	}

	c.DecActive()
	s = c.Snapshot()
	if s.Active != 1 {
		t.Errorf("Active after dec: got %d, want 1", s.Active)
	}

	c.DecQueued()
	s = c.Snapshot()
	if s.Queued != 0 {
		t.Errorf("Queued after dec: got %d, want 0", s.Queued)
	}
}

func TestCollector_RecordStatus(t *testing.T) {
	c := NewCollector()

	c.RecordStatus(200)
	c.RecordStatus(201)
	c.RecordStatus(404)
	c.RecordStatus(500)
	c.RecordStatus(503)

	s := c.Snapshot()
	if s.StatusCounts[2] != 2 {
		t.Errorf("2xx: got %d, want 2", s.StatusCounts[2])
	}
	if s.StatusCounts[4] != 1 {
		t.Errorf("4xx: got %d, want 1", s.StatusCounts[4])
	}
	if s.StatusCounts[5] != 2 {
		t.Errorf("5xx: got %d, want 2", s.StatusCounts[5])
	}
}

func TestCollector_RecordStatusOutOfBounds(t *testing.T) {
	c := NewCollector()

	// code 0 is ignored (no WriteHeader was called)
	c.RecordStatus(0)

	c.RecordStatus(99)
	c.RecordStatus(600)
	c.RecordStatus(10000)

	s := c.Snapshot()
	if s.StatusCounts[0] != 3 { // 99, 600, 10000
		t.Errorf("bucket 0: got %d, want 3", s.StatusCounts[0])
	}
	if s.StatusCounts[1] != 0 { // 1xx — no legitimate 1xx recorded
		t.Errorf("bucket 1: got %d, want 0", s.StatusCounts[1])
	}
}

func TestCollector_Concurrent(t *testing.T) {
	c := NewCollector()
	const n = 5000
	const g = 50

	var wg sync.WaitGroup
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < n; j++ {
				c.IncActive()
				c.IncProxied()
				c.RecordStatus(200)
				c.DecActive()
			}
		}()
	}
	wg.Wait()

	s := c.Snapshot()
	expected := int64(g * n)
	if s.TotalProxied != expected {
		t.Errorf("TotalProxied: got %d, want %d", s.TotalProxied, expected)
	}
	if s.Active != 0 {
		t.Errorf("Active: got %d, want 0", s.Active)
	}
}

func TestCollector_RequestLog(t *testing.T) {
	c := NewCollector()

	// Record some requests.
	c.RecordRequest("POST", "/v1/messages", 200, 150*time.Millisecond, true)
	c.RecordRequest("GET", "/health", 200, 2*time.Millisecond, false)
	c.RecordRequest("POST", "/v1/messages", 504, 30*time.Second, true)
	c.RecordRequest("POST", "/v1/chat/completions", 429, 5*time.Millisecond, true)

	entries := c.LogEntries()
	if len(entries) != 4 {
		t.Fatalf("expected 4 log entries, got %d", len(entries))
	}

	// Verify chronological order.
	if entries[0].Method != "POST" || entries[0].Path != "/v1/messages" {
		t.Errorf("first entry wrong: %v", entries[0])
	}
	if entries[2].Status != 504 {
		t.Errorf("third entry status: got %d, want 504", entries[2].Status)
	}

	// Verify snapshot includes log.
	s := c.Snapshot()
	if len(s.LogEntries) != 4 {
		t.Errorf("snapshot log: got %d, want 4", len(s.LogEntries))
	}
}

func TestCollector_RingBufferWrap(t *testing.T) {
	c := NewCollector()

	// Overflow the ring buffer.
	for i := 0; i < maxLogEntries+100; i++ {
		c.RecordRequest("GET", "/test", 200, time.Millisecond, false)
	}

	entries := c.LogEntries()
	if len(entries) != maxLogEntries {
		t.Errorf("expected %d entries, got %d", maxLogEntries, len(entries))
	}
}

func TestCollector_RouteStats(t *testing.T) {
	c := NewCollector()

	c.RecordRequest("POST", "/v1/messages", 200, 100*time.Millisecond, true)
	c.RecordRequest("POST", "/v1/messages", 200, 200*time.Millisecond, true)
	c.RecordRequest("POST", "/v1/messages", 504, 30*time.Second, true)
	c.RecordRequest("POST", "/v1/chat/completions", 200, 50*time.Millisecond, true)

	rs := c.RouteStats()
	msgStats, ok := rs["POST /v1/messages"]
	if !ok {
		t.Fatal("missing POST /v1/messages route stat")
	}
	if msgStats.Total != 3 {
		t.Errorf("Total: got %d, want 3", msgStats.Total)
	}
	if msgStats.Statuses[2] != 2 {
		t.Errorf("2xx: got %d, want 2", msgStats.Statuses[2])
	}
	if msgStats.Statuses[5] != 1 {
		t.Errorf("5xx: got %d, want 1", msgStats.Statuses[5])
	}

	chatStats := rs["POST /v1/chat/completions"]
	if chatStats.Total != 1 {
		t.Errorf("chat Total: got %d, want 1", chatStats.Total)
	}
}

func TestCollector_Throughput(t *testing.T) {
	c := NewCollector()

	// Record some requests.
	for i := 0; i < 10; i++ {
		c.RecordRequest("POST", "/v1/messages", 200, 10*time.Millisecond, true)
		time.Sleep(5 * time.Millisecond)
	}

	rps := c.Throughput()
	if rps <= 0 {
		t.Errorf("expected positive throughput, got %f", rps)
	}

	spark := c.ThroughputSparkline(20)
	if len(spark) != 20 {
		t.Errorf("sparkline length: got %d, want 20", len(spark))
	}
}

func TestCollector_Reset(t *testing.T) {
	c := NewCollector()

	c.IncActive()
	c.IncProxied()
	c.IncPassThrough()
	c.IncTimeout()
	c.IncCancelled()
	c.RecordStatus(200)
	c.RecordRequest("POST", "/v1/messages", 200, 10*time.Millisecond, true)

	c.Reset()

	s := c.Snapshot()
	// active/queued are runtime state and are NOT reset (resetting them
	// while goroutines are mid-request would cause atomic underflow).
	if s.TotalProxied != 0 {
		t.Errorf("TotalProxied: got %d, want 0", s.TotalProxied)
	}
	if s.TotalPassThrough != 0 {
		t.Errorf("TotalPassThrough: got %d, want 0", s.TotalPassThrough)
	}
	if s.TotalTimeout != 0 {
		t.Errorf("TotalTimeout: got %d, want 0", s.TotalTimeout)
	}
	if s.TotalCancelled != 0 {
		t.Errorf("TotalCancelled: got %d, want 0", s.TotalCancelled)
	}
	if len(s.LogEntries) != 0 {
		t.Errorf("LogEntries: got %d, want 0", len(s.LogEntries))
	}
	if len(c.RouteStats()) != 0 {
		t.Error("RouteStats not empty after reset")
	}
	if len(s.InFlight) != 0 {
		t.Errorf("InFlight: got %d, want 0", len(s.InFlight))
	}
}

func TestNewCollector(t *testing.T) {
	c := NewCollector()
	if c == nil {
		t.Fatal("expected non-nil collector")
	}
	s := c.Snapshot()
	if s.Active != 0 || s.Queued != 0 {
		t.Error("expected all-zero snapshot for new collector")
	}
}

func TestCollector_InFlight(t *testing.T) {
	c := NewCollector()

	// Register some in-flight requests.
	id1 := c.RegisterInFlight("POST", "/v1/messages", true)
	id2 := c.RegisterInFlight("GET", "/health", false)
	id3 := c.RegisterInFlight("POST", "/api/anthropic/v1/messages", true)

	if c.InFlightCount() != 3 {
		t.Errorf("InFlightCount: got %d, want 3", c.InFlightCount())
	}

	// Snapshot should show all three.
	snap := c.Snapshot()
	if len(snap.InFlight) != 3 {
		t.Fatalf("InFlight: got %d, want 3", len(snap.InFlight))
	}

	// Mark one as started.
	c.MarkInFlightStarted(id1)

	// Deregister one.
	c.DeregisterInFlight(id2)
	if c.InFlightCount() != 2 {
		t.Errorf("InFlightCount after dereg: got %d, want 2", c.InFlightCount())
	}

	// Snapshot should show remaining two.
	snap = c.Snapshot()
	if len(snap.InFlight) != 2 {
		t.Errorf("InFlight after dereg: got %d, want 2", len(snap.InFlight))
	}

	// Complete the rest.
	c.DeregisterInFlight(id1)
	c.DeregisterInFlight(id3)
	if c.InFlightCount() != 0 {
		t.Errorf("InFlightCount after all done: got %d, want 0", c.InFlightCount())
	}
}

func TestInFlightEntry_Age(t *testing.T) {
	e := InFlightEntry{
		StartTime: time.Now().Add(-500 * time.Millisecond),
	}
	age := e.Age()
	if age < 400*time.Millisecond || age > 600*time.Millisecond {
		t.Errorf("Age: got %v, want ~500ms", age)
	}

	// Zero start time.
	e2 := InFlightEntry{}
	if e2.Age() != 0 {
		t.Errorf("Age with zero StartTime: got %v, want 0", e2.Age())
	}
}

func TestInFlightEntry_TotalAge(t *testing.T) {
	now := time.Now()
	e := InFlightEntry{
		QueueTime: now.Add(-2 * time.Second),
		StartTime: now.Add(-1 * time.Second),
	}
	age := e.TotalAge()
	if age < 1500*time.Millisecond || age > 2500*time.Millisecond {
		t.Errorf("TotalAge: got %v, want ~2s", age)
	}
}

func TestCollector_ResetConcurrent(t *testing.T) {
	// Verify that Reset() does not underflow the active counter when
	// DecActive() is called concurrently (the original bug from GAP-005).
	c := NewCollector()

	const inFlight = 100
	for i := 0; i < inFlight; i++ {
		c.IncActive()
	}

	var wg sync.WaitGroup
	// Start goroutines that will DecActive after a tiny delay.
	for i := 0; i < inFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Millisecond)
			c.DecActive()
		}()
	}

	// Reset while DecActive calls are in-flight.
	time.Sleep(500 * time.Microsecond)
	c.Reset()

	wg.Wait()

	// After all DecActive calls complete, active should be 0.
	s := c.Snapshot()
	if s.Active != 0 {
		t.Errorf("Active after concurrent reset+dec: got %d, want 0", s.Active)
	}
	// Total counters should still be reset.
	if s.TotalProxied != 0 {
		t.Errorf("TotalProxied: got %d, want 0", s.TotalProxied)
	}
}

func TestCollector_RouteTimeoutTracking(t *testing.T) {
	c := NewCollector()

	// Record some requests with various statuses.
	c.RecordRequest("POST", "/v1/messages", 200, 100*time.Millisecond, true)
	c.RecordRequest("POST", "/v1/messages", 504, 30*time.Second, true)
	c.RecordRequest("POST", "/v1/messages", 503, 50*time.Millisecond, true)
	c.RecordRequest("POST", "/v1/messages", 200, 200*time.Millisecond, true)
	c.RecordRequest("POST", "/v1/chat/completions", 504, 10*time.Second, true)

	rs := c.RouteStats()
	msgStats := rs["POST /v1/messages"]
	if msgStats.Total != 4 {
		t.Errorf("Total: got %d, want 4", msgStats.Total)
	}
	if msgStats.Timeouts != 2 {
		t.Errorf("Timeouts: got %d, want 2 (one 504 + one 503)", msgStats.Timeouts)
	}

	chatStats := rs["POST /v1/chat/completions"]
	if chatStats.Total != 1 {
		t.Errorf("chat Total: got %d, want 1", chatStats.Total)
	}
	if chatStats.Timeouts != 1 {
		t.Errorf("chat Timeouts: got %d, want 1", chatStats.Timeouts)
	}
}
