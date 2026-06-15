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

// Package metrics collects and exposes live proxy metrics via atomics.
//
// A Collector is safe for concurrent use from many goroutines. It tracks
// active requests, queued/waiting requests, total acquisitions, releases,
// timeouts, pass-through counts, and HTTP status code distributions.
//
// It also maintains:
//   - A ring buffer of completed requests for the TUI access log panel.
//   - A live registry of in-flight requests for the TUI in-flight panel.
//   - Per-route aggregate stats and a short-term throughput window.
package metrics

import (
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	statusBuckets    = 6
	maxLogEntries    = 512
	maxInFlight      = 256
	throughputWindow = 10 * time.Second
	throughputSlots  = 100
)

// InFlightEntry tracks a single request that is currently being processed.
type InFlightEntry struct {
	ID        uint64
	Method    string
	Path      string
	Limited   bool
	QueueTime time.Time // when the request entered the queue (zero if passthrough)
	StartTime time.Time // when the request started being proxied
}

// Age returns how long this request has been in-flight (being proxied).
func (e InFlightEntry) Age() time.Duration {
	if e.StartTime.IsZero() {
		return 0
	}
	return time.Since(e.StartTime)
}

// TotalAge returns how long since the request was first received (including queue).
func (e InFlightEntry) TotalAge() time.Duration {
	t := e.QueueTime
	if t.IsZero() {
		t = e.StartTime
	}
	if t.IsZero() {
		return 0
	}
	return time.Since(t)
}

// RequestLogEntry is a single completed request record for the TUI log.
type RequestLogEntry struct {
	Time     time.Time
	Method   string
	Path     string
	Status   int
	Duration time.Duration
	Limited  bool
}

// routeStats tracks per-route aggregate counters.
type routeStats struct {
	total    atomic.Int64
	timeouts atomic.Int64
	statuses [statusBuckets]atomic.Int64
}

// Collector holds atomic counters for proxy metrics.
type Collector struct {
	active             atomic.Int64
	queued             atomic.Int64
	totalProxied       atomic.Int64
	totalPass          atomic.Int64
	totalTimeout       atomic.Int64
	totalCancelled     atomic.Int64
	totalCircuitReject atomic.Int64

	retriesInFlight atomic.Int64

	statusCounts [statusBuckets]atomic.Int64

	// Per-route stats.
	mu     sync.RWMutex
	routes map[string]*routeStats

	// Ring buffer of completed requests.
	logMu    sync.Mutex
	logBuf   [maxLogEntries]RequestLogEntry
	logHead  int
	logCount int

	// Live in-flight registry.
	flightMu   sync.Mutex
	flightSeq  uint64
	flightByID map[uint64]*InFlightEntry

	// Throughput window.
	tpMu       sync.Mutex
	tpGran     time.Duration
	tpSlots    int
	tpCounts   []int
	tpHead     int
	tpLastTick time.Time
	tpStart    time.Time // when the throughput window began (set in NewCollector and Reset)
}

// NewCollector creates a ready-to-use Collector.
func NewCollector() *Collector {
	now := time.Now()
	return &Collector{
		routes:     make(map[string]*routeStats),
		flightByID: make(map[uint64]*InFlightEntry),
		tpGran:     throughputWindow / throughputSlots,
		tpSlots:    throughputSlots,
		tpCounts:   make([]int, throughputSlots),
		tpLastTick: now,
		tpStart:    now,
	}
}

// IncActive increments the active-request counter.
func (c *Collector) IncActive() { c.active.Add(1) }

// DecActive decrements the active-request counter.
func (c *Collector) DecActive() { c.active.Add(-1) }

// IncQueued increments the queued/waiters counter.
func (c *Collector) IncQueued() { c.queued.Add(1) }

// DecQueued decrements the queued/waiters counter.
func (c *Collector) DecQueued() { c.queued.Add(-1) }

// IncProxied increments the total-proxied-through-limiter counter.
func (c *Collector) IncProxied() { c.totalProxied.Add(1) }

// IncPassThrough increments the total-passed-through-directly counter.
func (c *Collector) IncPassThrough() { c.totalPass.Add(1) }

// IncTimeout increments the total-timeout counter (queue deadline exceeded).
func (c *Collector) IncTimeout() { c.totalTimeout.Add(1) }

// IncCancelled increments the total-cancelled counter (client disconnected while queued).
func (c *Collector) IncCancelled() { c.totalCancelled.Add(1) }

// IncRetryInFlight increments the active retry counter.
func (c *Collector) IncRetryInFlight() { c.retriesInFlight.Add(1) }

// DecRetryInFlight decrements the active retry counter.
func (c *Collector) DecRetryInFlight() { c.retriesInFlight.Add(-1) }

// RetriesInFlightCounter returns a pointer to the atomic counter tracking
// in-flight retries. This enables the retry transport to increment/decrement
// the counter directly without going through the Inc/Dec methods.
func (c *Collector) RetriesInFlightCounter() *atomic.Int64 { return &c.retriesInFlight }

// IncCircuitRejected increments the counter for requests rejected because
// the circuit breaker was OPEN. These are immediate pre-queue rejections,
// distinct from queue timeouts.
func (c *Collector) IncCircuitRejected() { c.totalCircuitReject.Add(1) }

// RecordStatus records an HTTP status code.
// A code of 0 is ignored (it indicates the response was never written to).
func (c *Collector) RecordStatus(code int) {
	if code == 0 {
		return
	}
	bucket := code / 100
	if bucket < 0 || bucket >= statusBuckets {
		bucket = 0
	}
	c.statusCounts[bucket].Add(1)
}

// RegisterInFlight registers a new in-flight request and returns an ID
// that must be passed to DeregisterInFlight when the request completes.
func (c *Collector) RegisterInFlight(method, path string, limited bool) uint64 {
	c.flightMu.Lock()
	defer c.flightMu.Unlock()
	c.flightSeq++
	id := c.flightSeq
	now := time.Now()
	c.flightByID[id] = &InFlightEntry{
		ID:        id,
		Method:    method,
		Path:      path,
		Limited:   limited,
		QueueTime: now,
		StartTime: now,
	}
	return id
}

// MarkInFlightStarted updates the start time for a request that has been
// queued and is now being proxied. This separates queue time from proxy time.
func (c *Collector) MarkInFlightStarted(id uint64) {
	c.flightMu.Lock()
	defer c.flightMu.Unlock()
	if e, ok := c.flightByID[id]; ok {
		e.StartTime = time.Now()
	}
}

// DeregisterInFlight removes an in-flight request from the live registry.
func (c *Collector) DeregisterInFlight(id uint64) {
	c.flightMu.Lock()
	defer c.flightMu.Unlock()
	delete(c.flightByID, id)
}

// InFlightSnapshot returns a snapshot of all currently in-flight requests,
// sorted by start time (oldest first).
func (c *Collector) InFlightSnapshot() []InFlightEntry {
	c.flightMu.Lock()
	defer c.flightMu.Unlock()
	out := make([]InFlightEntry, 0, len(c.flightByID))
	for _, e := range c.flightByID {
		out = append(out, *e)
	}
	// Sort by start time, oldest first.
	// Entries with zero StartTime (still queued) sort to the end.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		aZero, bZero := a.StartTime.IsZero(), b.StartTime.IsZero()
		if aZero != bZero {
			return !aZero // non-zero sorts before zero
		}
		if aZero {
			return false
		}
		return a.StartTime.Before(b.StartTime)
	})
	return out
}

// InFlightCount returns the number of currently in-flight requests.
func (c *Collector) InFlightCount() int {
	c.flightMu.Lock()
	defer c.flightMu.Unlock()
	return len(c.flightByID)
}

// RecordRequest records a completed request in the ring buffer and updates
// per-route stats and the throughput window.
func (c *Collector) RecordRequest(method, path string, status int, duration time.Duration, limited bool) {
	key := method + " " + path

	// Update per-route stats.
	c.mu.RLock()
	rs, ok := c.routes[key]
	c.mu.RUnlock()
	if !ok {
		c.mu.Lock()
		rs, ok = c.routes[key]
		if !ok {
			rs = &routeStats{}
			c.routes[key] = rs
		}
		c.mu.Unlock()
	}
	rs.total.Add(1)
	if status == http.StatusGatewayTimeout || status == http.StatusServiceUnavailable {
		rs.timeouts.Add(1)
	}
	if status >= 0 && status/100 < statusBuckets {
		rs.statuses[status/100].Add(1)
	}

	// Record in ring buffer.
	entry := RequestLogEntry{
		Time:     time.Now(),
		Method:   method,
		Path:     path,
		Status:   status,
		Duration: duration,
		Limited:  limited,
	}
	c.logMu.Lock()
	c.logBuf[c.logHead] = entry
	c.logHead = (c.logHead + 1) % maxLogEntries
	if c.logCount < maxLogEntries {
		c.logCount++
	}
	c.logMu.Unlock()

	// Record in throughput window.
	c.tpMu.Lock()
	c.advanceThroughputWindow()
	c.tpCounts[c.tpHead]++
	c.tpMu.Unlock()
}

// advanceThroughputWindow advances the throughput ring buffer to the current
// time, zeroing expired slots. Must be called with c.tpMu held. This is
// called from RecordRequest (to position the write head), and from
// Throughput/ThroughputSparkline (to ensure the window is current at read
// time even when no traffic is arriving — preventing frozen non-zero RPS
// after traffic stops).
func (c *Collector) advanceThroughputWindow() {
	now := time.Now()
	elapsed := now.Sub(c.tpLastTick)
	if elapsed < 0 {
		return
	}
	adv := int(elapsed / c.tpGran)
	if adv <= 0 {
		return
	}
	if adv > c.tpSlots {
		adv = c.tpSlots
	}
	for i := 0; i < adv; i++ {
		c.tpHead = (c.tpHead + 1) % c.tpSlots
		c.tpCounts[c.tpHead] = 0
	}
	// Use additive advancement instead of assigning now directly.
	// When elapsed is not a perfect multiple of tpGran, the remainder
	// would be discarded by tpLastTick = now, causing the ring buffer's
	// internal clock to drift slower than real time and inflating RPS.
	// By advancing by the exact discrete multiple, the remainder carries
	// forward to the next call.
	c.tpLastTick = c.tpLastTick.Add(time.Duration(adv) * c.tpGran)
}

// LogEntries returns recent log entries in chronological order (oldest first).
func (c *Collector) LogEntries() []RequestLogEntry {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	n := c.logCount
	if n == 0 {
		return nil
	}
	out := make([]RequestLogEntry, n)
	start := (c.logHead - n + maxLogEntries) % maxLogEntries
	for i := range n {
		out[i] = c.logBuf[(start+i)%maxLogEntries]
	}
	return out
}

// Throughput returns requests per second over the sliding window.
// The ring buffer is advanced to the current time before reading, so
// stale non-zero RPS is never reported after traffic stops.
// During the first window-duration of the collector's life (or after a
// Reset), throughput is computed over the actual elapsed time since the
// window started, so it is not under-reported during ramp-up. Once the
// window is fully filled, the denominator is the full window duration,
// producing accurate steady-state RPS.
func (c *Collector) Throughput() float64 {
	c.tpMu.Lock()
	defer c.tpMu.Unlock()
	c.advanceThroughputWindow()
	var total int
	for _, v := range c.tpCounts {
		total += v
	}
	window := float64(c.tpSlots) * c.tpGran.Seconds()
	if window <= 0 {
		return 0
	}
	// Use the actual elapsed time since the window started (tpStart),
	// capped at the full window size. Before the fix, this used
	// time.Since(c.tpLastTick) which measures only the time since the
	// last ring-buffer slot advancement (~100ms), dividing the total
	// count across all 100 slots by a fraction of a second and
	// producing ~100-200x inflated RPS.
	elapsed := time.Since(c.tpStart)
	if elapsed <= 0 {
		if total > 0 {
			// Edge case: clock hasn't advanced — attribute all counts
			// to the minimum granule to avoid returning infinity.
			return float64(total) / c.tpGran.Seconds()
		}
		return 0
	}
	effectiveWindow := elapsed.Seconds()
	if effectiveWindow > window {
		effectiveWindow = window
	}
	if effectiveWindow <= 0 {
		return 0
	}
	return float64(total) / effectiveWindow
}

// ThroughputSparkline returns per-slot counts for a sparkline visualization.
// The ring buffer is advanced to the current time before reading, so
// stale counts are never reported after traffic stops.
func (c *Collector) ThroughputSparkline(width int) []int {
	c.tpMu.Lock()
	defer c.tpMu.Unlock()
	c.advanceThroughputWindow()
	if width <= 0 || width > c.tpSlots {
		width = c.tpSlots
	}
	out := make([]int, width)
	step := float64(c.tpSlots) / float64(width)
	for i := 0; i < width; i++ {
		var sum int
		start := int(float64(i) * step)
		end := min(int(float64(i+1)*step), c.tpSlots)
		for j := start; j < end; j++ {
			idx := (c.tpHead + 1 + j) % c.tpSlots
			sum += c.tpCounts[idx]
		}
		out[i] = sum
	}
	return out
}

// RouteStats returns a copy of per-route stats.
func (c *Collector) RouteStats() map[string]RouteStat {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]RouteStat, len(c.routes))
	for k, v := range c.routes {
		var s RouteStat
		s.Total = v.total.Load()
		s.Timeouts = v.timeouts.Load()
		for i := range v.statuses {
			s.Statuses[i] = v.statuses[i].Load()
		}
		out[k] = s
	}
	return out
}

// RouteStat is a point-in-time copy of per-route counters.
type RouteStat struct {
	Total    int64
	Timeouts int64
	Statuses [6]int64
}

// Reset zeroes cumulative counters and derived state.
// Note: active, queued, and retriesInFlight are runtime-derived values (they
// reflect actual in-flight operations) and are NOT reset — resetting them
// while goroutines are mid-request would cause atomic underflow when
// DecActive/DecQueued/DecRetryInFlight fire.
func (c *Collector) Reset() {
	c.totalProxied.Store(0)
	c.totalPass.Store(0)
	c.totalTimeout.Store(0)
	c.totalCancelled.Store(0)
	c.totalCircuitReject.Store(0)
	for i := range c.statusCounts {
		c.statusCounts[i].Store(0)
	}
	c.mu.Lock()
	c.routes = make(map[string]*routeStats)
	c.mu.Unlock()
	c.logMu.Lock()
	c.logHead = 0
	c.logCount = 0
	c.logMu.Unlock()
	c.flightMu.Lock()
	c.flightByID = make(map[uint64]*InFlightEntry)
	c.flightMu.Unlock()
	c.tpMu.Lock()
	for i := range c.tpCounts {
		c.tpCounts[i] = 0
	}
	c.tpHead = 0
	now := time.Now()
	c.tpLastTick = now
	c.tpStart = now
	c.tpMu.Unlock()
}

// Snapshot returns a consistent point-in-time copy of all metrics.
func (c *Collector) Snapshot() Snapshot {
	var s Snapshot
	s.Active = c.active.Load()
	s.Queued = c.queued.Load()
	s.TotalProxied = c.totalProxied.Load()
	s.TotalPassThrough = c.totalPass.Load()
	s.TotalTimeout = c.totalTimeout.Load()
	s.TotalCancelled = c.totalCancelled.Load()
	s.TotalCircuitRejected = c.totalCircuitReject.Load()
	for i := range c.statusCounts {
		s.StatusCounts[i] = c.statusCounts[i].Load()
	}
	s.LogEntries = c.LogEntries()
	s.Throughput = c.Throughput()
	s.Sparkline = c.ThroughputSparkline(60)
	s.RouteStats = c.RouteStats()
	s.InFlight = c.InFlightSnapshot()
	var limited, passthrough int64
	for _, e := range s.InFlight {
		if e.Limited {
			limited++
		} else {
			passthrough++
		}
	}
	s.InFlightLimited = limited
	s.InFlightPassthrough = passthrough
	s.RetriesInFlight = c.retriesInFlight.Load()

	// Calculate oldest queued age from in-flight snapshot.
	var oldestQueue time.Duration
	for _, e := range s.InFlight {
		if e.Limited && e.StartTime.IsZero() && !e.QueueTime.IsZero() {
			if age := time.Since(e.QueueTime); age > oldestQueue {
				oldestQueue = age
			}
		}
	}
	s.OldestQueuedAge = oldestQueue

	return s
}

// Snapshot is a serialised view of Collector at a point in time.
type Snapshot struct {
	Active               int64
	Queued               int64
	TotalProxied         int64
	TotalPassThrough     int64
	TotalTimeout         int64
	TotalCancelled       int64
	TotalCircuitRejected int64
	StatusCounts         [6]int64
	LogEntries           []RequestLogEntry
	Throughput           float64
	Sparkline            []int
	RouteStats           map[string]RouteStat
	InFlight             []InFlightEntry
	InFlightLimited      int64
	InFlightPassthrough  int64
	RetriesInFlight      int64
	OldestQueuedAge      time.Duration
	CircuitBreaker       *CBStats
}

// CBStats is a snapshot of circuit breaker state for the TUI.
type CBStats struct {
	State               string
	Failures            int64
	ConsecutiveFailures int64
	TotalFailures       int64
	TotalSuccesses      int64
	CurrentPenalty      time.Duration
	NextRetry           time.Time
}
