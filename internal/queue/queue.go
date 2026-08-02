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

// Package queue provides a single blocking concurrency limiter with
// best-effort wait semantics.
//
// A Limiter is configured with a maximum number of concurrent slots.
// Callers block on Acquire until a slot is available (or the context is
// canceled). Waiters are served in best-effort order; Go does not
// guarantee strict FIFO, but every waiter is eventually served
// (starvation-free).
//
// The limiter tracks queue depth, total acquisitions, total releases, and
// total timeouts for metrics export. All operations are goroutine-safe.
package queue

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// defaultMaxWithheld is the default maximum number of slots that can be
// withheld by adaptive headroom. The mitigation is intentionally bounded to a
// single slot: providers that temp-ban for "more than N" concurrent requests
// typically only need one extra connection of headroom to absorb
// teardown/accounting races. Callers that need a larger headroom range (e.g.
// the AIMD autoscaler) can raise it via SetMaxWithheld.
const defaultMaxWithheld = 1

// Limiter is a concurrency-bounded blocking queue.
type Limiter struct {
	// slots is a buffered channel whose capacity equals the concurrency
	// limit. Holding a token from this channel means the caller owns a
	// slot.
	slots chan struct{}

	// cooldown is an optional delay applied after each slot release.
	// When > 0, the token is not returned to the channel immediately on
	// release; instead, time.AfterFunc(cooldown, returnToken) schedules
	// the return. This creates a "dead zone" after every slot release,
	// ensuring the downstream service has time to complete its accounting
	// before the next request arrives. This mitigates KILL-02 (slot
	// release race under load).
	cooldown time.Duration

	// waiters tracks how many goroutines are currently blocked in Acquire.
	waiters atomic.Int64

	// Stats (all atomically accessed).
	totalAcquired atomic.Int64
	totalReleased atomic.Int64
	totalTimeout  atomic.Int64

	// adaptiveMu guards all adaptive-headroom fields.
	adaptiveMu sync.Mutex
	// withheld is the number of slots temporarily removed from circulation.
	// It is bounded by maxWithheld.
	withheld int
	// maxWithheld is the ceiling on how many slots may be withheld at once.
	// It defaults to defaultMaxWithheld (1) and can be raised via
	// SetMaxWithheld to support dynamic concurrency controllers that need a
	// wider adjustment range.
	maxWithheld int
	// pendingAbsorbs counts how many withholds were unable to remove an idle
	// token up front and are instead waiting to absorb future slot releases.
	// Each Acquire release checks this counter: if positive, the release is
	// swallowed (the token never returns to the channel) and the counter
	// decrements. Unlike a single boolean, a counter correctly tracks multiple
	// simultaneous pending withholds (e.g. an AIMD multiplicative decrease that
	// removes several slots at once while all slots are in use). It is always
	// <= withheld.
	pendingAbsorbs int
	// adaptiveTimer schedules restoration of a withheld slot. It is reset
	// (stopped and restarted) on every successful reduction so that a stream
	// of 429s keeps the headroom active.
	adaptiveTimer *time.Timer
	// timerEpoch invalidates stale timer callbacks when a reduction is
	// refreshed at the same moment an older timer has already fired.
	timerEpoch uint64
}

// NewLimiterWithCooldown creates a Limiter with the given concurrency limit
// and post-release cooldown. When cooldown > 0, releasing a slot does not
// immediately return the token to the channel; instead, the token is returned
// after the cooldown duration via time.AfterFunc. This creates a buffer between
// slot release and re-admission, preventing the downstream from observing N+1
// concurrent requests due to accounting lag (KILL-02).
func NewLimiterWithCooldown(limit int, cooldown time.Duration) *Limiter {
	if limit < 1 {
		panic("queue: concurrency limit must be >= 1")
	}
	if cooldown < 0 {
		panic("queue: cooldown must be >= 0")
	}
	l := &Limiter{
		slots:       make(chan struct{}, limit),
		cooldown:    cooldown,
		maxWithheld: defaultMaxWithheld,
	}
	// Pre-fill so the channel already contains `limit` tokens.
	for range limit {
		l.slots <- struct{}{}
	}
	return l
}

// Acquire blocks until a concurrency slot is available or the context is
// canceled.
//
// The returned release function must be called exactly once to return the
// slot. The release func is safe to call from any goroutine and is
// idempotent — calling it more than once is a no-op (extra calls are
// silently dropped).
//
// If the context is canceled while waiting, Acquire returns an error and
// no slot is held.
func (l *Limiter) Acquire(ctx context.Context) (release func(), err error) {
	l.waiters.Add(1)
	defer l.waiters.Add(-1)

	select {
	case <-ctx.Done():
		l.totalTimeout.Add(1)
		return nil, ctx.Err()
	case <-l.slots:
		l.totalAcquired.Add(1)
		released := new(sync.Once)
		return func() {
			released.Do(func() {
				doRelease := func() {
					l.slots <- struct{}{}
					l.totalReleased.Add(1)
				}
				if l.cooldown > 0 {
					time.AfterFunc(l.cooldown, func() {
						if !l.absorbNextRelease() {
							doRelease()
						}
					})
				} else if !l.absorbNextRelease() {
					doRelease()
				}
			})
		}, nil
	}
}

// absorbNextRelease consumes the next slot release for a pending withhold when
// the withhold was unable to remove an idle token up front. It returns true
// once per pending absorb; after all pending absorbs are consumed, releases
// flow normally until a slot is restored by the timer or RestoreSlot.
func (l *Limiter) absorbNextRelease() bool {
	l.adaptiveMu.Lock()
	defer l.adaptiveMu.Unlock()
	if l.pendingAbsorbs <= 0 {
		return false
	}
	l.pendingAbsorbs--
	return true
}

// AdaptiveReduce temporarily removes one slot from circulation when the
// downstream signals it is observing too much concurrency. The slot is
// restored after the provided window elapses without another 429. Calling
// AdaptiveReduce again while already reduced extends the recovery window.
//
// It returns false and does nothing if window <= 0 or if the configured
// limit is already 1 (cannot reduce below one slot). It returns true while
// a reduction is active or is being refreshed, even when the limiter was
// already reduced by maxWithheld.
func (l *Limiter) AdaptiveReduce(window time.Duration) bool {
	if window <= 0 {
		return false
	}
	l.adaptiveMu.Lock()
	defer l.adaptiveMu.Unlock()

	limit := cap(l.slots)

	// Refresh an existing reduction first, before the effective-limit guard
	// would otherwise refuse because limit-withheld == 1.
	if l.withheld >= l.maxWithheld {
		l.timerEpoch++
		epoch := l.timerEpoch
		l.stopAdaptiveTimerLocked()
		l.adaptiveTimer = time.AfterFunc(window, func() { l.restoreAdaptiveSlot(epoch) })
		return true
	}

	if limit-l.withheld <= 1 {
		// Never reduce the effective limit below 1.
		return false
	}

	l.withheld++
	l.pendingAbsorbs++
	l.timerEpoch++
	epoch := l.timerEpoch
	l.stopAdaptiveTimerLocked()
	l.adaptiveTimer = time.AfterFunc(window, func() { l.restoreAdaptiveSlot(epoch) })
	// If a token is currently idle, remove it from circulation immediately so
	// the reduction takes effect even before the next release.
	select {
	case <-l.slots:
		l.pendingAbsorbs--
	default:
	}
	return true
}

// stopAdaptiveTimerLocked stops the existing adaptive timer. The caller must
// hold l.adaptiveMu.
func (l *Limiter) stopAdaptiveTimerLocked() {
	if l.adaptiveTimer != nil {
		l.adaptiveTimer.Stop()
	}
}

// restoreAdaptiveSlot returns one withheld slot to the pool. It is scheduled
// by the adaptive timer, inserts the token back into the slots channel when
// one was actually removed, and decrements the withheld count.
func (l *Limiter) restoreAdaptiveSlot(epoch uint64) {
	l.adaptiveMu.Lock()
	if epoch != l.timerEpoch {
		l.adaptiveMu.Unlock()
		return
	}
	if l.withheld <= 0 {
		// No token was actually removed from circulation yet, so there is
		// nothing to restore. Clear any pending absorbs so future releases
		// flow normally.
		l.pendingAbsorbs = 0
		l.adaptiveMu.Unlock()
		return
	}
	l.withheld--
	if l.pendingAbsorbs > 0 {
		// The reduction was pending on a future release that never arrived
		// within the window. Clear one pending absorb and do not insert a
		// token because no slot was ever taken out of circulation.
		l.pendingAbsorbs--
		l.adaptiveMu.Unlock()
		return
	}
	l.adaptiveMu.Unlock()

	l.slots <- struct{}{}
	l.totalReleased.Add(1)
}

// AdaptiveActive reports whether adaptive headroom is currently active. It
// returns true while a token has been removed from circulation or while a
// reduction is pending (waiting to absorb the next slot release).
func (l *Limiter) AdaptiveActive() bool {
	l.adaptiveMu.Lock()
	defer l.adaptiveMu.Unlock()
	return l.withheld > 0 || l.pendingAbsorbs > 0
}

// Withheld returns the number of slots currently withheld from circulation.
func (l *Limiter) Withheld() int {
	l.adaptiveMu.Lock()
	defer l.adaptiveMu.Unlock()
	return l.withheld
}

// SetMaxWithheld sets the maximum number of slots that may be withheld from
// circulation simultaneously by WithholdSlot, AdaptiveReduce, and RestoreSlot.
// The default is 1 (sufficient for the single-slot adaptive headroom
// mitigation). Dynamic concurrency controllers that need a wider adjustment
// range (e.g. an AIMD autoscaler) should call this before withholding slots.
// Setting a value lower than the current withheld count does not retroactively
// restore slots; it only prevents new withholds until the count drops below the
// new ceiling.
func (l *Limiter) SetMaxWithheld(n int) {
	l.adaptiveMu.Lock()
	defer l.adaptiveMu.Unlock()
	if n < 0 {
		n = 0
	}
	l.maxWithheld = n
}

// MaxWithheld returns the maximum number of slots that may be withheld from
// circulation at once.
func (l *Limiter) MaxWithheld() int {
	l.adaptiveMu.Lock()
	defer l.adaptiveMu.Unlock()
	return l.maxWithheld
}

// WithholdSlot removes one slot from circulation without a timer. It is the
// non-timer-based counterpart to AdaptiveReduce, designed for dynamic
// concurrency controllers that manage their own adjustment schedule (e.g. an
// AIMD autoscaler).
//
// If an idle token is available in the channel it is drained immediately. If
// all slots are currently held, the next release is absorbed instead (via
// pendingAbsorbs). The effective limit decreases by one on success.
//
// It returns false if the limiter is already at its maxWithheld ceiling or if
// reducing would bring the effective limit below 1. It returns true (even when
// the reduction is pending on a future release) if the withhold was accepted.
func (l *Limiter) WithholdSlot() bool {
	l.adaptiveMu.Lock()
	defer l.adaptiveMu.Unlock()

	if l.withheld >= l.maxWithheld {
		return false
	}
	if cap(l.slots)-l.withheld <= 1 {
		return false
	}

	l.withheld++
	l.pendingAbsorbs++
	// If a token is currently idle, remove it from circulation immediately so
	// the reduction takes effect even before the next release.
	select {
	case <-l.slots:
		l.pendingAbsorbs--
	default:
	}
	return true
}

// RestoreSlot returns one withheld slot to circulation. It is the counterpart
// to WithholdSlot. If a withhold is still pending (no token was ever removed
// from circulation), the pending absorb is cleared instead of inserting a
// token — no slot was ever taken out, so nothing needs to go back in.
//
// If the limiter has no withheld slots, RestoreSlot is a no-op.
func (l *Limiter) RestoreSlot() {
	l.adaptiveMu.Lock()
	if l.withheld <= 0 {
		// No slot was actually removed from circulation. Clear any stale
		// pending absorbs so future releases flow normally.
		l.pendingAbsorbs = 0
		l.adaptiveMu.Unlock()
		return
	}
	l.withheld--
	if l.pendingAbsorbs > 0 {
		// The reduction was pending on a future release that never arrived.
		// Clear one pending absorb and do not insert a token because no slot
		// was ever taken out of circulation.
		l.pendingAbsorbs--
		l.adaptiveMu.Unlock()
		return
	}
	l.adaptiveMu.Unlock()

	l.slots <- struct{}{}
	l.totalReleased.Add(1)
}

// Stats returns a snapshot of the limiter's current state.
func (l *Limiter) Stats() LimiterStats {
	l.adaptiveMu.Lock()
	withheld := int64(l.withheld)
	pendingAbsorbs := int64(l.pendingAbsorbs)
	l.adaptiveMu.Unlock()

	// active = capacity - idle tokens - withheld + pendingAbsorbs.
	// pendingAbsorbs counts withholds that have NOT yet removed a token from
	// the channel, so for each one there is a token that is currently in use
	// (counts against len(slots) as non-idle) but whose eventual release will
	// be swallowed. Adding them back corrects the active count.
	active := max(int64(cap(l.slots))-int64(len(l.slots))-withheld+pendingAbsorbs, 0)
	return LimiterStats{
		Active:       active,
		Waiters:      l.waiters.Load(),
		TotalAcq:     l.totalAcquired.Load(),
		TotalRel:     l.totalReleased.Load(),
		TotalTimeout: l.totalTimeout.Load(),
		Withheld:     withheld,
	}
}

// LimiterStats is a point-in-time snapshot of limiter metrics.
type LimiterStats struct {
	Active       int64
	Waiters      int64
	TotalAcq     int64
	TotalRel     int64
	TotalTimeout int64
	// Withheld is the number of slots temporarily removed from circulation
	// by adaptive headroom.
	Withheld int64
}

// Limit returns the configured concurrency limit (channel capacity).
func (l *Limiter) Limit() int {
	return cap(l.slots)
}

// EffectiveLimit returns the configured limit minus any slots currently
// withheld by adaptive headroom. It represents how many slots may be in use
// or available at this moment, but note that when pendingAbsorbs is nonzero
// the next releases will also be absorbed, so the value may briefly over-report
// active capacity until those releases occur.
func (l *Limiter) EffectiveLimit() int {
	l.adaptiveMu.Lock()
	defer l.adaptiveMu.Unlock()
	return cap(l.slots) - l.withheld
}

// Cooldown returns the configured post-release cooldown duration.
func (l *Limiter) Cooldown() time.Duration {
	return l.cooldown
}
