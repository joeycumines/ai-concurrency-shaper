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

// Package circuitbreaker implements a conservative circuit breaker for a
// reverse proxy that bounds downstream concurrency.
//
// The breaker uses a three-state machine (CLOSED → OPEN → HALF_OPEN) with a
// sliding-window failure counter and a "phantom concurrency" penalty that
// holds proxy slots after failures to prevent exceeding downstream rate
// limits.
//
// Failure classification is intentionally conservative for status-only callers:
// all 5xx status codes, 429 Too Many Requests, 403 Forbidden, and transport
// errors (no response) are counted as failures. Response-aware callers should
// use IsFailureStatusWithHeaders so ordinary authentication 403s can pass
// through while rate-limit-signaled temporary bans still trip the breaker.
// Only 2xx responses count as successes. 3xx and most non-429 4xx responses
// are ignored — they neither hurt nor help the breaker.
package circuitbreaker

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// State represents the current state of the circuit breaker.
type State int

const (
	Closed   State = iota // Normal operation — requests pass through.
	Open                  // Circuit tripped — requests rejected immediately.
	HalfOpen              // Probing — one request allowed to test recovery.
)

func (s State) String() string {
	switch s {
	case Closed:
		return "CLOSED"
	case Open:
		return "OPEN"
	case HalfOpen:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}

// ErrCircuitOpen is returned by Allow when the circuit is in the OPEN state.
var ErrCircuitOpen = errors.New("circuit breaker open")

// clockSkewTolerance bounds the acceptable clock difference between the
// proxy and an upstream that omits a Date header. Without it, freshly issued
// HTTP-date Retry-After values can appear slightly expired due to network
// transit time, causing temporary bans to be misclassified as permanent auth
// errors. This tolerance is applied only to the Date-less fallback path; when
// a Date header is present, RFC 9110 §10.2.3 eliminates the skew.
const clockSkewTolerance = 1 * time.Second

// --- Option Interface ---

// Option configures a Breaker.
type Option interface {
	applyBreakerOption(cfg *breakerConfig) error
}

// --- Unexported Config Struct ---

type breakerConfig struct {
	failureThreshold int
	window           time.Duration
	openTimeout      time.Duration
	maxOpenTimeout   time.Duration
	basePenalty      time.Duration
	maxPenalty       time.Duration
}

func (c *breakerConfig) applyDefaults() {
	if c.failureThreshold <= 0 {
		c.failureThreshold = 5
	}
	if c.window <= 0 {
		c.window = 30 * time.Second
	}
	if c.openTimeout <= 0 {
		c.openTimeout = 10 * time.Second
	}
	if c.maxOpenTimeout <= 0 {
		c.maxOpenTimeout = 120 * time.Second
	}
	if c.maxOpenTimeout < c.openTimeout {
		c.maxOpenTimeout = c.openTimeout
	}
	if c.basePenalty <= 0 {
		c.basePenalty = 2 * time.Second
	}
	if c.maxPenalty <= 0 {
		c.maxPenalty = 60 * time.Second
	}
	if c.maxPenalty < c.basePenalty {
		c.maxPenalty = c.basePenalty
	}
}

// --- Concrete Options ---

// FailureThresholdOption sets the number of qualifying failures within the
// window that triggers the circuit to open.
type FailureThresholdOption struct {
	value int
}

// WithFailureThreshold returns an option that sets the failure threshold.
// Must be > 0.
func WithFailureThreshold(n int) *FailureThresholdOption {
	return &FailureThresholdOption{value: n}
}

func (o *FailureThresholdOption) applyBreakerOption(cfg *breakerConfig) error {
	if o.value <= 0 {
		return fmt.Errorf("circuitbreaker: failure threshold must be > 0, got %d", o.value)
	}
	cfg.failureThreshold = o.value
	return nil
}

// WindowOption sets the sliding time window for counting failures.
type WindowOption struct {
	value time.Duration
}

// WithWindow returns an option that sets the failure counting window.
// Must be > 0.
func WithWindow(d time.Duration) *WindowOption {
	return &WindowOption{value: d}
}

func (o *WindowOption) applyBreakerOption(cfg *breakerConfig) error {
	if o.value <= 0 {
		return fmt.Errorf("circuitbreaker: window must be > 0, got %v", o.value)
	}
	cfg.window = o.value
	return nil
}

// OpenTimeoutOption sets how long the circuit stays OPEN before probing.
type OpenTimeoutOption struct {
	value time.Duration
}

// WithOpenTimeout returns an option that sets the open timeout.
// Must be > 0.
func WithOpenTimeout(d time.Duration) *OpenTimeoutOption {
	return &OpenTimeoutOption{value: d}
}

func (o *OpenTimeoutOption) applyBreakerOption(cfg *breakerConfig) error {
	if o.value <= 0 {
		return fmt.Errorf("circuitbreaker: open timeout must be > 0, got %v", o.value)
	}
	cfg.openTimeout = o.value
	return nil
}

// MaxOpenTimeoutOption sets the maximum open timeout after exponential backoff.
type MaxOpenTimeoutOption struct {
	value time.Duration
}

// WithMaxOpenTimeout returns an option that sets the max open timeout.
// Must be > 0.
func WithMaxOpenTimeout(d time.Duration) *MaxOpenTimeoutOption {
	return &MaxOpenTimeoutOption{value: d}
}

func (o *MaxOpenTimeoutOption) applyBreakerOption(cfg *breakerConfig) error {
	if o.value <= 0 {
		return fmt.Errorf("circuitbreaker: max open timeout must be > 0, got %v", o.value)
	}
	cfg.maxOpenTimeout = o.value
	return nil
}

// BasePenaltyOption sets the base phantom concurrency slot hold time.
type BasePenaltyOption struct {
	value time.Duration
}

// WithBasePenalty returns an option that sets the base penalty duration.
// Must be > 0.
func WithBasePenalty(d time.Duration) *BasePenaltyOption {
	return &BasePenaltyOption{value: d}
}

func (o *BasePenaltyOption) applyBreakerOption(cfg *breakerConfig) error {
	if o.value <= 0 {
		return fmt.Errorf("circuitbreaker: base penalty must be > 0, got %v", o.value)
	}
	cfg.basePenalty = o.value
	return nil
}

// MaxPenaltyOption caps the phantom concurrency penalty.
type MaxPenaltyOption struct {
	value time.Duration
}

// WithMaxPenalty returns an option that sets the max penalty duration.
// Must be > 0.
func WithMaxPenalty(d time.Duration) *MaxPenaltyOption {
	return &MaxPenaltyOption{value: d}
}

func (o *MaxPenaltyOption) applyBreakerOption(cfg *breakerConfig) error {
	if o.value <= 0 {
		return fmt.Errorf("circuitbreaker: max penalty must be > 0, got %v", o.value)
	}
	cfg.maxPenalty = o.value
	return nil
}

// --- Compile-Time Compliance Checks ---

var (
	_ Option = (*FailureThresholdOption)(nil)
	_ Option = (*WindowOption)(nil)
	_ Option = (*OpenTimeoutOption)(nil)
	_ Option = (*MaxOpenTimeoutOption)(nil)
	_ Option = (*BasePenaltyOption)(nil)
	_ Option = (*MaxPenaltyOption)(nil)
)

// --- Factory ---

// Breaker is a conservative circuit breaker with phantom concurrency penalty.
// All methods are safe for concurrent use.
type Breaker struct {
	cfg breakerConfig

	mu sync.Mutex

	state           State
	failures        []time.Time // ring of failure timestamps within Window
	consecutive     int64       // consecutive failures (reset on success)
	totalFailures   int64
	totalSuccesses  int64
	lastFailure     time.Time
	lastStateChange time.Time

	// OPEN state tracking.
	openSince       time.Time
	backoffMultiple int // exponent for exponential backoff on HALF_OPEN → OPEN

	// Tracks the last observed Retry-After for penalty calculation.
	lastRetryAfter time.Duration

	// HALF_OPEN probe tracking: allows exactly one request through.
	probeInFlight  bool
	probeStartedAt time.Time // when the current probe was dispatched
	probeEpoch     uint64    // incremented each time a probe is dispatched; used to discard stale probe results
}

// New creates a Breaker with the given options. Zero-valued fields receive
// sensible defaults. Returns an error if any option validation fails.
func New(opts ...Option) (*Breaker, error) {
	cfg := breakerConfig{} // zero values → defaults applied below
	for _, o := range opts {
		if err := o.applyBreakerOption(&cfg); err != nil {
			return nil, err
		}
	}
	cfg.applyDefaults()
	now := time.Now()
	return &Breaker{
		cfg:             cfg,
		state:           Closed,
		lastStateChange: now,
		failures:        make([]time.Time, 0, cfg.failureThreshold*2),
	}, nil
}

// Allow checks whether a request should be admitted.
// Returns (epoch, nil) if the circuit is CLOSED or HALF_OPEN (probe allowed).
// The epoch is non-zero only for HALF_OPEN probes; callers must pass it back
// to RecordFailure or RecordSuccess so stale probe results are discarded.
// Returns (0, ErrCircuitOpen) if the circuit is OPEN.
func (b *Breaker) Allow() (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return 0, nil
	case Open:
		// Check if enough time has passed to transition to HALF_OPEN.
		if time.Since(b.openSince) >= b.openTimeout() {
			b.setState(HalfOpen)
			b.probeInFlight = true
			b.probeStartedAt = time.Now()
			b.probeEpoch++
			return b.probeEpoch, nil
		}
		return 0, ErrCircuitOpen
	case HalfOpen:
		// Allow exactly one probe request.
		if !b.probeInFlight {
			b.probeInFlight = true
			b.probeStartedAt = time.Now()
			b.probeEpoch++
			return b.probeEpoch, nil
		}
		// If the probe has been in flight longer than the open timeout,
		// it is likely stuck (e.g., the client disconnected without
		// triggering RecordFailure or RecordSuccess, or the probe got
		// a status that neither path reports). Allow a new probe to
		// prevent permanent HALF_OPEN deadlock. The epoch is incremented
		// so stale results from the old probe are discarded.
		if time.Since(b.probeStartedAt) > b.openTimeout() {
			b.probeStartedAt = time.Now()
			b.probeEpoch++
			return b.probeEpoch, nil
		}
		return 0, ErrCircuitOpen
	default:
		return 0, ErrCircuitOpen
	}
}

// CancelProbe releases a HALF_OPEN probe admitted by Allow when the caller
// discovers that the probe was inconclusive rather than a breaker success or
// failure. Examples include no upstream attempt, downstream write/flush failure,
// local upgrade capability failure, and suppressed client aborts after response
// headers. It is intentionally a no-op for CLOSED requests (epoch 0), stale
// epochs, and probes that have already resolved through RecordSuccess or
// RecordFailure. A matching cancellation also advances the epoch so any late
// result carrying the canceled epoch is treated as stale by
// RecordSuccess/RecordFailure.
func (b *Breaker) CancelProbe(epoch uint64) {
	if epoch == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == HalfOpen && b.probeInFlight && b.probeEpoch == epoch {
		b.probeInFlight = false
		b.probeEpoch++
	}
}

// RecordFailure records a qualifying failure (5xx, 429, transport error, or
// rate-limit-signaled 403).
// statusCode is 0 for transport errors where no response was received.
// retryAfter is the Retry-After duration from the response header (0 if absent).
// startedAt is the time the request began; if non-zero and before the current
// OPEN period's start (openSince), the failure is ignored in the HALF_OPEN
// state transition — it is a stale result from a request that started before
// the circuit opened, not the probe. Pass a zero time.Time to disable filtering.
// epoch is the probe epoch returned by Allow; if non-zero and different from the
// current probeEpoch, the result is from a stale probe and is discarded.
// Pass 0 to disable epoch filtering (e.g., for non-probe requests in CLOSED state).
func (b *Breaker) RecordFailure(statusCode int, retryAfter time.Duration, startedAt time.Time, epoch uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Discard results from stale probes. When Allow() dispatches a probe
	// in HALF_OPEN, it increments probeEpoch and returns the new value.
	// If the circuit subsequently dispatches another probe (e.g., because
	// the first one timed out), the old probe's epoch no longer matches.
	// This prevents out-of-order probe resolution from corrupting the
	// state machine: a stale failure cannot re-trip a recovered circuit,
	// and a stale success cannot falsely close a re-opened circuit.
	if epoch != 0 && epoch != b.probeEpoch {
		return
	}

	// Ignore stale failures in HALF_OPEN: if the request started before the
	// current OPEN period, it is a ghost from the previous failure wave, not
	// the probe. The probe request started AFTER openSince, so its startedAt
	// will be after openSince and will correctly trigger the HALF_OPEN → OPEN
	// transition. This guard MUST precede ALL state mutations — a stale
	// failure must not inflate b.consecutive (which feeds the penalty), pollute
	// b.failures (which feeds the threshold check), or update b.lastRetryAfter
	// (which feeds the penalty floor).
	if b.state == HalfOpen &&
		!startedAt.IsZero() && !b.openSince.IsZero() && startedAt.Before(b.openSince) {
		return
	}

	now := time.Now()

	// Track the max retry-after for penalty calculation.
	if retryAfter > b.lastRetryAfter {
		b.lastRetryAfter = retryAfter
	}

	b.consecutive++
	b.totalFailures++
	b.lastFailure = now

	// Append failure timestamp and trim expired entries.
	b.failures = append(b.failures, now)
	b.trimFailures(now)

	switch b.state {
	case Closed:
		if len(b.failures) >= b.cfg.failureThreshold {
			b.setState(Open)
			b.openSince = now
		}
	case HalfOpen:
		// Probe failed — back to OPEN with increased backoff.
		b.backoffMultiple++
		b.setState(Open)
		b.openSince = now
		b.probeInFlight = false
	}
}

// RecordSuccess records a successful request (2xx response).
// Resets consecutive failure count and, if in HALF_OPEN, transitions to CLOSED.
// startedAt is the time the request began; if non-zero and before the current
// OPEN period's start (openSince), the success is ignored in the HALF_OPEN
// state transition — it is a stale result from a request that started before
// the circuit opened, not the probe. Pass a zero time.Time to disable filtering.
// epoch is the probe epoch returned by Allow; if non-zero and different from the
// current probeEpoch, the result is from a stale probe and is discarded.
// Pass 0 to disable epoch filtering.
func (b *Breaker) RecordSuccess(startedAt time.Time, epoch uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Discard results from stale probes (same logic as RecordFailure).
	if epoch != 0 && epoch != b.probeEpoch {
		return
	}

	b.totalSuccesses++

	switch b.state {
	case HalfOpen:
		// Ignore stale successes: if the request started before the current
		// OPEN period, it is a ghost from the previous wave, not the probe.
		// A long-polling request that began while CLOSED and returns 200
		// after the circuit has cycled through OPEN→HALF_OPEN must not
		// falsely close the circuit. The early return MUST happen before
		// resetting consecutive/lastRetryAfter to avoid leaking side
		// effects (dropping the penalty to base and discarding Retry-After).
		if !startedAt.IsZero() && !b.openSince.IsZero() && startedAt.Before(b.openSince) {
			return
		}
		// Probe succeeded — downstream is healthy again.
		b.backoffMultiple = 0
		b.consecutive = 0
		b.lastRetryAfter = 0
		b.failures = b.failures[:0] // Clear accumulated failures on recovery.
		b.setState(Closed)
		b.probeInFlight = false
	case Closed:
		// Normal success — reset consecutive and penalty state.
		b.consecutive = 0
		b.lastRetryAfter = 0
	}
}

// WaitDuration returns how long a caller should wait before the next attempt.
// Returns 0 if the circuit is CLOSED.
// Returns the remaining open timeout if OPEN.
func (b *Breaker) WaitDuration() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Open:
		elapsed := time.Since(b.openSince)
		wait := b.openTimeout() - elapsed
		if wait < 0 {
			return 0
		}
		return wait
	default:
		return 0
	}
}

// PenaltyDuration returns the phantom concurrency slot hold time.
// The penalty grows with consecutive failures: BasePenalty * (1 + consecutive).
// It is also raised to match any observed Retry-After header.
// The result is capped at MaxPenalty.
func (b *Breaker) PenaltyDuration() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.currentPenaltyLocked()
}

// State returns the current circuit breaker state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Stats returns a point-in-time snapshot of the breaker's internal state.
func (b *Breaker) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()

	var nextRetry time.Time
	if b.state == Open {
		nextRetry = b.openSince.Add(b.openTimeout())
	}

	return Stats{
		State:               b.state,
		Failures:            int64(len(b.failures)),
		ConsecutiveFailures: b.consecutive,
		TotalFailures:       b.totalFailures,
		TotalSuccesses:      b.totalSuccesses,
		LastFailure:         b.lastFailure,
		LastStateChange:     b.lastStateChange,
		CurrentPenalty:      b.currentPenaltyLocked(),
		NextRetry:           nextRetry,
	}
}

// Stats is a point-in-time snapshot of the breaker's state.
type Stats struct {
	State               State
	Failures            int64
	ConsecutiveFailures int64
	TotalFailures       int64
	TotalSuccesses      int64
	LastFailure         time.Time
	LastStateChange     time.Time
	CurrentPenalty      time.Duration
	NextRetry           time.Time
}

// IsFailureStatus returns true for status codes that indicate a potential
// phantom concurrency risk or an upstream ban. This is the conservative
// failure classification:
//   - Status 0: transport error (no response received) — always a failure.
//   - All 5xx (500–599): downstream error or uncertain state.
//   - 429: rate-limited, request may still be running.
//   - 403 (Forbidden): status-only callers conservatively treat this as a
//     failure; response-aware callers require rate-limit signals first.
//
// 2xx and 3xx are clean completions. Other non-429 4xx are client errors
// (rejected before processing).
func IsFailureStatus(code int) bool {
	return code == 0 || code == 429 || code == 403 || (code >= 500 && code < 600)
}

// IsFailureStatusWithHeaders classifies a status code with optional response
// headers. receivedAt is when the response headers arrived; evaluatedAt is
// when the classification decision is made. The elapsed time between them is
// subtracted from delay-seconds Retry-After values.
func IsFailureStatusWithHeaders(code int, h http.Header, receivedAt, evaluatedAt time.Time) bool {
	if code == http.StatusForbidden {
		return IsTemporaryBanResponse(h, receivedAt, evaluatedAt)
	}
	return IsFailureStatus(code)
}

// ParseRetryAfter extracts the remaining non-negative Retry-After duration
// from headers. It supports both delay-seconds (integer) and HTTP-date formats
// per RFC 9110 §10.2.3. receivedAt anchors the start of delay-seconds counting,
// and evaluatedAt is subtracted so that callers evaluating after body streaming
// do not keep expired bans alive.
//
// For HTTP-date values, when a valid Date header is present the remaining delay
// is computed using only upstream-clock timestamps (Retry-After - Date) and only
// proxy-clock timestamps (evaluatedAt - receivedAt), eliminating clock skew
// between the two machines. When Date is absent or unparseable, the remaining
// delay falls back to retry.Sub(evaluatedAt), which is susceptible to clock skew
// but is mitigated by clockSkewTolerance in IsTemporaryBanResponse.
func ParseRetryAfter(h http.Header, receivedAt, evaluatedAt time.Time) time.Duration {
	if h == nil {
		return 0
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}

	// Delay-seconds: an integer number of seconds measured from receipt.
	if sec, err := strconv.Atoi(v); err == nil {
		if sec <= 0 {
			return 0
		}
		elapsed := max(evaluatedAt.Sub(receivedAt), 0)
		intended := time.Duration(sec) * time.Second
		if intended <= elapsed {
			return 0
		}
		return intended - elapsed
	}

	// HTTP-date: an absolute expiry time.
	retry, err := http.ParseTime(v)
	if err != nil {
		return 0
	}

	// When a valid Date header is present, compute the intended ban duration
	// from upstream-only timestamps and subtract proxy-only elapsed time.
	// This eliminates clock skew between the upstream and proxy clocks:
	//   intendedDelta = Retry-After - Date   (upstream clock only)
	//   proxyElapsed  = evaluatedAt - receivedAt  (proxy clock only)
	//   remaining     = intendedDelta - proxyElapsed
	if dateStr := h.Get("Date"); dateStr != "" {
		if upstreamDate, err := http.ParseTime(dateStr); err == nil {
			intendedDelta := retry.Sub(upstreamDate)
			if intendedDelta <= 0 {
				return 0
			}
			proxyElapsed := max(evaluatedAt.Sub(receivedAt), 0)
			remaining := intendedDelta - proxyElapsed
			if remaining <= 0 {
				return 0
			}
			return remaining
		}
	}

	// No valid Date header: fall back to direct comparison. This is
	// susceptible to clock skew but clockSkewTolerance in classification
	// absorbs minor discrepancies.
	remaining := retry.Sub(evaluatedAt)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// IsTemporaryBanResponse reports whether a response looks like a temporary
// rate-limit or ban response rather than a permanent client error. receivedAt
// is when the response headers arrived; evaluatedAt is when the classification
// decision is made.
//
// When a valid Date header is present, the intended ban duration is derived
// from upstream-only timestamps (Retry-After - Date), then the proxy's elapsed
// time since receipt is subtracted. This eliminates clock skew between the
// upstream and proxy machines entirely — no tolerance window is needed.
//
// When Date is absent or unparseable, clockSkewTolerance is applied so that
// minor transit delay or upstream clock drift does not discard a fresh
// temporary ban as expired.
func IsTemporaryBanResponse(h http.Header, receivedAt, evaluatedAt time.Time) bool {
	if h == nil {
		return false
	}

	// First check for explicit rate-limit headers. Their presence is enough
	// to classify a 403 as a temporary ban, even if a malformed Retry-After
	// header is also present.
	hasRateLimitHeader := false
	for k, values := range h {
		if len(values) == 0 {
			continue
		}
		key := strings.ToLower(k)
		if key == "x-ratelimit-reset" || key == "x-ratelimit-limit" || key == "x-ratelimit-remaining" ||
			key == "ratelimit-reset" || key == "ratelimit-limit" || key == "ratelimit-remaining" {
			hasRateLimitHeader = true
			break
		}
	}

	if v := h.Get("Retry-After"); v != "" {
		if _, err := strconv.Atoi(v); err == nil {
			// Retry-After: 0 means "retry immediately" (RFC 9110 §10.2.3) —
			// the ban is already resolved. Delay-seconds values are consumed
			// by elapsed time; if the remaining wait is zero, treat it the
			// same as Retry-After: 0.
			return ParseRetryAfter(h, receivedAt, evaluatedAt) > 0 || hasRateLimitHeader
		}
		if retry, err := http.ParseTime(v); err == nil {
			// When a valid Date header is present, compute the intended ban
			// duration from upstream-only timestamps to eliminate clock skew,
			// then subtract proxy-only elapsed time since receipt.
			if dateStr := h.Get("Date"); dateStr != "" {
				if upstreamDate, err := http.ParseTime(dateStr); err == nil {
					intendedDelta := retry.Sub(upstreamDate)
					proxyElapsed := max(evaluatedAt.Sub(receivedAt), 0)
					remaining := intendedDelta - proxyElapsed
					return remaining > 0 || hasRateLimitHeader
				}
			}
			// No valid Date: apply clock skew tolerance so network transit
			// does not classify a fresh ban as expired.
			remaining := retry.Sub(evaluatedAt)
			return remaining >= -clockSkewTolerance || hasRateLimitHeader
		}
		// Malformed Retry-After: fall through to the rate-limit header check.
		return hasRateLimitHeader
	}

	return hasRateLimitHeader
}

// --- internal helpers ---

func (b *Breaker) setState(s State) {
	b.state = s
	b.lastStateChange = time.Now()
}

func (b *Breaker) openTimeout() time.Duration {
	// Compute openTimeout * 2^backoffMultiple, capped at maxOpenTimeout.
	// Check for overflow BEFORE multiplying: if maxOpenTimeout / 2^shift
	// < openTimeout, then the raw value would exceed maxOpenTimeout (and
	// potentially overflow int64), so return maxOpenTimeout directly.
	// Without this guard, openTimeout * Duration(1<<shift) overflows for
	// large openTimeout values (e.g. 5h * 1<<19 ≈ 9.4e18 > MaxInt64),
	// producing a negative Duration that min() cannot fix (negative <
	// positive for signed ints).
	shift := b.backoffMultiple
	if shift >= 62 { // 1<<62 is already > MaxInt64/2; no point computing further
		return b.cfg.maxOpenTimeout
	}
	multiplier := time.Duration(1 << shift)
	if multiplier > 0 && b.cfg.maxOpenTimeout/multiplier < b.cfg.openTimeout {
		return b.cfg.maxOpenTimeout
	}
	return min(b.cfg.openTimeout*multiplier, b.cfg.maxOpenTimeout)
}

func (b *Breaker) currentPenaltyLocked() time.Duration {
	// The penalty is the larger of: the last observed Retry-After, or the
	// base penalty scaled by consecutive failures. The entire result is then
	// capped at MaxPenalty — even a malicious Retry-After cannot hold slots
	// longer than the configured maximum.
	//
	// Cap consecutive before multiplication to prevent int64 overflow.
	// Without this, basePenalty * Duration(1+consecutive) overflows when
	// consecutive is extremely large (~4.6B), producing a negative Duration
	// that min() treats as 0 — violating the guarantee that the penalty is
	// always at least basePenalty. The cap ensures the scaled value can
	// never exceed maxPenalty through legitimate scaling.
	c := b.consecutive
	if c > 0 {
		maxConsecutive := int64(b.cfg.maxPenalty / b.cfg.basePenalty)
		if c > maxConsecutive {
			c = maxConsecutive
		}
	}
	scaled := b.cfg.basePenalty * time.Duration(1+c)
	penalty := min(max(b.lastRetryAfter, scaled), b.cfg.maxPenalty)
	return penalty
}

// trimFailures removes entries older than the window cutoff.
func (b *Breaker) trimFailures(now time.Time) {
	cutoff := now.Add(-b.cfg.window)
	i := 0
	for i < len(b.failures) && b.failures[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		b.failures = b.failures[i:]
	}
}
