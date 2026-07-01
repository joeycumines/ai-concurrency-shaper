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
	for range g {
		wg.Go(func() {
			for range n {
				c.IncActive()
				c.IncProxied()
				c.RecordStatus(200)
				c.DecActive()
			}
		})
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
	for range maxLogEntries + 100 {
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
	for range 10 {
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

func TestCollector_ThroughputAccuracy(t *testing.T) {
	// R33-02 regression test: Verify that Throughput() returns approximately
	// correct RPS — not 100-200x inflated. Before the fix, Throughput()
	// divided the total count across all 100 ring buffer slots by
	// time.Since(tpLastTick), where tpLastTick was updated every ~100ms
	// during buffer advancement. This divided a 10-second window's total by
	// a fraction of a second, producing ~100-200x inflated RPS.
	c := NewCollector()

	// Record 10 requests with ~10ms spacing, so total elapsed ~100ms.
	// Expected RPS ≈ 10 / 0.1 ≈ 100 (order of magnitude).
	for range 10 {
		c.RecordRequest("POST", "/v1/messages", 200, time.Millisecond, true)
		time.Sleep(10 * time.Millisecond)
	}

	rps := c.Throughput()

	// The RPS should be in a plausible range: between 20 and 500.
	// Before the fix, it would be ~2000+ (100 requests / 0.05s).
	if rps < 20 {
		t.Errorf("throughput too low: %f RPS (expected ~100)", rps)
	}
	if rps > 500 {
		t.Errorf("throughput too high: %f RPS — likely still using tpLastTick as denominator (expected ~100)", rps)
	}
}

func TestCollector_ThroughputRampUp(t *testing.T) {
	// Verify that throughput is non-zero during the first window duration
	// (before the ring buffer is fully filled). The tpStart field ensures
	// we divide by actual elapsed time, not by the full window.
	c := NewCollector()

	c.RecordRequest("POST", "/v1/messages", 200, time.Millisecond, true)
	// Immediately query — should be > 0 (1 request / ~0s ≈ large, but at least > 0).
	rps := c.Throughput()
	if rps <= 0 {
		t.Errorf("throughput during ramp-up should be > 0, got %f", rps)
	}
}

func TestCollector_ThroughputAfterReset(t *testing.T) {
	// Verify that after Reset(), throughput starts fresh from 0.
	c := NewCollector()

	// Record some requests to establish a baseline.
	for range 5 {
		c.RecordRequest("POST", "/v1/messages", 200, time.Millisecond, true)
	}

	before := c.Throughput()
	if before <= 0 {
		t.Fatalf("throughput before reset should be > 0, got %f", before)
	}

	c.Reset()

	// Immediately after reset, there are no requests in the window.
	after := c.Throughput()
	if after != 0 {
		t.Errorf("throughput immediately after Reset() should be 0, got %f", after)
	}

	// Record new requests — throughput should ramp up from 0.
	c.RecordRequest("POST", "/v1/messages", 200, time.Millisecond, true)
	afterNew := c.Throughput()
	if afterNew <= 0 {
		t.Errorf("throughput after Reset() + new request should be > 0, got %f", afterNew)
	}
}

func TestCollector_ThroughputSteadyState(t *testing.T) {
	// Verify that after the window is fully filled (>10s of traffic),
	// throughput is stable and accurate. We simulate this by recording
	// enough requests to fill the ring buffer and then checking the value.
	c := NewCollector()

	// Record 20 requests with 50ms spacing = ~1s total elapsed.
	// Expected RPS ≈ 20 / 1.0 ≈ 20.
	for range 20 {
		c.RecordRequest("POST", "/v1/messages", 200, time.Millisecond, true)
		time.Sleep(50 * time.Millisecond)
	}

	rps := c.Throughput()

	// Should be roughly 20 RPS, allow wide margin (5-80) for CI timing jitter.
	if rps < 5 {
		t.Errorf("steady-state throughput too low: %f RPS (expected ~20)", rps)
	}
	if rps > 80 {
		t.Errorf("steady-state throughput too high: %f RPS (expected ~20)", rps)
	}
}

func TestCollector_ThroughputDecaysAfterTrafficStops(t *testing.T) {
	// R34-04 regression test: Verify that Throughput() drops toward zero after
	// traffic stops. Before the fix, Throughput() and ThroughputSparkline()
	// did not advance the ring buffer window at read time, so stale non-zero
	// RPS was reported indefinitely after the last request.
	// Use a short window (1s / 10 slots at 100ms) for fast test execution.
	c := NewCollector()
	c.tpSlots = 10
	c.tpGran = 100 * time.Millisecond
	c.tpCounts = make([]int, 10)
	c.tpHead = 0
	now := time.Now()
	c.tpLastTick = now
	c.tpStart = now

	// Record some requests to populate the window.
	for range 5 {
		c.RecordRequest("POST", "/v1/messages", 200, time.Millisecond, true)
		time.Sleep(20 * time.Millisecond)
	}

	rpsDuringTraffic := c.Throughput()
	if rpsDuringTraffic <= 0 {
		t.Fatalf("expected positive throughput during traffic, got %f", rpsDuringTraffic)
	}

	// Wait for the window to fully expire (>1s for our 1s window).
	time.Sleep(1200 * time.Millisecond)

	rpsAfterStop := c.Throughput()
	if rpsAfterStop > 5 {
		t.Errorf("after traffic stopped and window expired, throughput should be ~0, got %f (frozen metrics bug)", rpsAfterStop)
	}
}

func TestCollector_ThroughputSparklineDecaysAfterTrafficStops(t *testing.T) {
	// R34-04 regression test: Same as TestCollector_ThroughputDecaysAfterTrafficStops
	// but for the sparkline path.
	c := NewCollector()
	c.tpSlots = 10
	c.tpGran = 100 * time.Millisecond
	c.tpCounts = make([]int, 10)
	c.tpHead = 0
	now := time.Now()
	c.tpLastTick = now
	c.tpStart = now

	for range 5 {
		c.RecordRequest("POST", "/v1/messages", 200, time.Millisecond, true)
		time.Sleep(20 * time.Millisecond)
	}

	sparkDuring := c.ThroughputSparkline(5)
	totalDuring := 0
	for _, v := range sparkDuring {
		totalDuring += v
	}
	if totalDuring <= 0 {
		t.Fatalf("expected non-zero sparkline during traffic, got total=%d", totalDuring)
	}

	time.Sleep(1200 * time.Millisecond)

	sparkAfter := c.ThroughputSparkline(5)
	totalAfter := 0
	for _, v := range sparkAfter {
		totalAfter += v
	}
	if totalAfter > 0 {
		t.Errorf("after traffic stopped and window expired, sparkline should be all zeros, got total=%d (frozen metrics bug)", totalAfter)
	}
}

func TestCollector_ThroughputNoIdleTimeDebtAfterLongPause(t *testing.T) {
	c := NewCollector()
	c.tpMu.Lock()
	c.tpSlots = 4
	c.tpGran = 10 * time.Millisecond
	c.tpCounts = make([]int, c.tpSlots)
	c.tpHead = 0
	longAgo := time.Now().Add(-time.Hour)
	c.tpLastTick = longAgo
	c.tpStart = longAgo
	c.tpMu.Unlock()

	c.RecordRequest("POST", "/v1/messages", 200, time.Millisecond, true)
	c.RecordRequest("POST", "/v1/messages", 200, time.Millisecond, true)

	spark := c.ThroughputSparkline(4)
	total := 0
	for _, v := range spark {
		total += v
	}
	if total != 2 {
		t.Fatalf("sparkline total after resumed traffic = %d, want 2 (idle time debt should not wipe each new request)", total)
	}
	if rps := c.Throughput(); rps <= 0 {
		t.Fatalf("throughput after resumed traffic = %f, want positive", rps)
	}
}

func TestCollector_ThroughputNoDriftOverTime(t *testing.T) {
	// R34-05 regression test: Verify that additive tpLastTick advancement
	// prevents cumulative drift. Before the fix, tpLastTick = now discarded
	// the fractional remainder on each advancement (e.g., 190ms elapsed with
	// 100ms granules → adv=1, 90ms lost). Over many iterations this caused
	// the ring buffer's internal clock to lag behind real time, trapping
	// old requests in the window and inflating RPS.
	// With additive advancement (tpLastTick.Add(duration(adv)*tpGran)), the
	// remainder carries forward, keeping tpLastTick synchronized with real time.
	c := NewCollector()

	// Record ~50 requests with ~10ms spacing = ~500ms of traffic.
	// Expected RPS ≈ 50 / 0.5 ≈ 100.
	for range 50 {
		c.RecordRequest("POST", "/v1/messages", 200, time.Millisecond, true)
		time.Sleep(10 * time.Millisecond)
	}

	rps := c.Throughput()

	// With additive advancement, RPS should be in a plausible range.
	// With the old tpLastTick = now drift, RPS would be inflated because
	// old counts linger in the window longer than they should.
	// Allow generous margin (10-300) for CI timing jitter.
	if rps < 10 {
		t.Errorf("throughput too low after sustained traffic: %f RPS", rps)
	}
	if rps > 300 {
		t.Errorf("throughput too high after sustained traffic: %f RPS — possible tpLastTick drift (expected ~100)", rps)
	}

	// Verify tpLastTick hasn't drifted far from real time.
	// After ~500ms of traffic with 100ms granules, tpLastTick should be
	// within a few granules of now.
	c.tpMu.Lock()
	drift := time.Since(c.tpLastTick)
	c.tpMu.Unlock()

	// The drift should be less than 2 granules (200ms). With the old
	// tpLastTick = now bug, drift would accumulate to many granules.
	if drift > 2*c.tpGran {
		t.Errorf("tpLastTick drift = %v, expected < %v — additive advancement may not be working", drift, 2*c.tpGran)
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
	if s.TotalCircuitRejected != 0 {
		t.Errorf("TotalCircuitRejected: got %d, want 0", s.TotalCircuitRejected)
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

func TestCollector_ResetDoesNotZeroRetriesInFlight(t *testing.T) {
	// Verify that Reset() does NOT zero retriesInFlight. Like active and queued,
	// retriesInFlight is a runtime-derived value shared with the live retry
	// transport. Zeroing it mid-retry causes the defer's Add(-1) to drive the
	// counter negative.
	c := NewCollector()

	c.IncRetryInFlight()
	c.IncRetryInFlight()
	if got := c.Snapshot().RetriesInFlight; got != 2 {
		t.Fatalf("before Reset: RetriesInFlight = %d, want 2", got)
	}

	c.Reset()

	if got := c.Snapshot().RetriesInFlight; got != 2 {
		t.Errorf("after Reset: RetriesInFlight = %d, want 2 (not reset — runtime-derived value)", got)
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
	for range inFlight {
		c.IncActive()
	}

	var wg sync.WaitGroup
	// Start goroutines that will DecActive after a tiny delay.
	for range inFlight {
		wg.Go(func() {
			time.Sleep(time.Millisecond)
			c.DecActive()
		})
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
