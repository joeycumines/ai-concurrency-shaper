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

// Package queue provides a resizable FIFO concurrency limiter. Existing leases
// survive resizing, including their post-release cooldown. Shrinking creates
// admission debt: no new lease can start until occupied slots fall below the
// effective limit. This is deliberately independent of client cancellation.
package queue

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

const maxAdaptiveSlots = 1

var ErrStaleRevision = errors.New("queue: configured limit changed")

// Limiter owns leases rather than replacing a channel on resize. Every state
// transition is serialized, so a resize cannot mint or lose an outstanding
// lease and FIFO waiters cannot be overtaken by new arrivals.
type Limiter struct {
	revision                                   uint64
	mu                                         sync.Mutex
	limit                                      int
	cooldown                                   time.Duration
	occupied, active, cooling                  int64
	totalAcquired, totalReleased, totalTimeout int64
	pending                                    list.List
	withheld                                   int
	absorbNext                                 bool
	adaptiveTimer                              *time.Timer
	timerEpoch                                 uint64
	observer                                   func(time.Time, LimiterStats)
}

type waiter struct {
	ctx     context.Context
	ready   chan struct{}
	element *list.Element
	granted bool
}

func NewLimiterWithCooldown(limit int, cooldown time.Duration) *Limiter {
	if limit < 1 {
		panic("queue: concurrency limit must be >= 1")
	}
	if cooldown < 0 {
		panic("queue: cooldown must be >= 0")
	}
	return &Limiter{limit: limit, cooldown: cooldown}
}

// Acquire waits in FIFO order. A cancellation racing a grant gives its lease
// back before returning the error; the lease never reached an upstream and
// therefore incurs no post-release cooldown. Successful releases are idempotent.
func (l *Limiter) Acquire(ctx context.Context) (func(), error) {
	return l.AcquireObserved(ctx, nil)
}

// AcquireObserved brackets an actual blocked wait. The callback is never
// invoked for an immediately available lease, so admission histograms do not
// fabricate waiting requests. It must not call this limiter or perform I/O.
func (l *Limiter) AcquireObserved(ctx context.Context, waiting func(bool)) (func(), error) {
	l.mu.Lock()
	if err := ctx.Err(); err != nil {
		l.totalTimeout++
		l.notifyLocked()
		l.mu.Unlock()
		return nil, err
	}
	if l.pending.Len() == 0 && l.occupied < int64(l.effectiveLocked()) {
		l.occupied++
		l.active++
		l.totalAcquired++
		l.notifyLocked()
		l.mu.Unlock()
		return l.releaseFunc(), nil
	}
	w := &waiter{ctx: ctx, ready: make(chan struct{})}
	w.element = l.pending.PushBack(w)
	if waiting != nil {
		waiting(true)
		defer waiting(false)
	}
	l.notifyLocked()
	l.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-w.ready:
	}
	l.mu.Lock()
	if err := ctx.Err(); err != nil {
		if w.element != nil {
			l.pending.Remove(w.element)
			w.element = nil
		}
		if w.granted {
			l.active--
			l.occupied--
			l.totalReleased++
		}
		l.totalTimeout++
		l.grantLocked()
		l.notifyLocked()
		l.mu.Unlock()
		return nil, err
	}
	// A non-cancelled waiter can only wake after grantLocked reserved a lease.
	l.mu.Unlock()
	return l.releaseFunc(), nil
}

func (l *Limiter) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.active--
			if l.cooldown > 0 {
				l.cooling++
				l.notifyLocked()
				l.mu.Unlock()
				time.AfterFunc(l.cooldown, l.finishCooldown)
				return
			}
			l.releaseOccupiedLocked()
			l.mu.Unlock()
		})
	}
}

func (l *Limiter) finishCooldown() {
	l.mu.Lock()
	l.cooling--
	l.releaseOccupiedLocked()
	l.mu.Unlock()
}

func (l *Limiter) releaseOccupiedLocked() {
	l.occupied--
	// Preserve the historical release counter: a release absorbed by adaptive
	// headroom is published when that withheld slot returns to circulation.
	if l.absorbNext {
		l.absorbNext = false
	} else {
		l.totalReleased++
	}
	l.grantLocked()
	l.notifyLocked()
}

func (l *Limiter) grantLocked() {
	for l.pending.Len() > 0 && l.occupied < int64(l.effectiveLocked()) {
		el := l.pending.Front()
		w := el.Value.(*waiter)
		l.pending.Remove(el)
		w.element = nil
		if w.ctx.Err() != nil {
			close(w.ready)
			continue
		}
		l.occupied++
		l.active++
		l.totalAcquired++
		w.granted = true
		close(w.ready)
	}
}

func (l *Limiter) effectiveLocked() int { return max(1, l.limit-l.withheld) }

// Resize changes the positive configured limit. Outstanding leases and
// cooldowns are untouched. Increasing admits queued work in FIFO order;
// shrinking reports debt until enough existing leases finish.
func (l *Limiter) Resize(limit int) error {
	_, err := l.resize(limit, nil)
	return err
}

// ResizeIfRevision atomically compares the configured-limit revision and
// resizes. It lets runtime previews and TTL restoration reject an out-of-band
// resize without racing an independent programmatic caller.
func (l *Limiter) ResizeIfRevision(limit int, expected uint64) (uint64, error) {
	return l.resize(limit, &expected)
}
func (l *Limiter) resize(limit int, expected *uint64) (uint64, error) {
	if limit < 1 || limit > 1_000_000 {
		return 0, errors.New("queue: resize limit must be between 1 and 1000000")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if expected != nil && *expected != l.revision {
		return l.revision, ErrStaleRevision
	}
	l.limit = limit
	l.revision++
	l.grantLocked()
	l.notifyLocked()
	return l.revision, nil
}

func (l *Limiter) AdaptiveReduce(window time.Duration) bool {
	if window <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.withheld < maxAdaptiveSlots {
		if l.limit <= 1 {
			return false
		}
		l.withheld = 1
		l.absorbNext = l.occupied >= int64(l.limit)
	}
	l.timerEpoch++
	epoch := l.timerEpoch
	if l.adaptiveTimer != nil {
		l.adaptiveTimer.Stop()
	}
	l.adaptiveTimer = time.AfterFunc(window, func() { l.restoreAdaptiveSlot(epoch) })
	l.notifyLocked()
	return true
}

func (l *Limiter) restoreAdaptiveSlot(epoch uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if epoch != l.timerEpoch || l.withheld == 0 {
		return
	}
	if !l.absorbNext {
		l.totalReleased++
	}
	l.withheld = 0
	l.absorbNext = false
	l.grantLocked()
	l.notifyLocked()
}

func (l *Limiter) AdaptiveActive() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.withheld > 0 }
func (l *Limiter) Withheld() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return min(l.withheld, max(0, l.limit-1))
}
func (l *Limiter) Limit() int              { l.mu.Lock(); defer l.mu.Unlock(); return l.limit }
func (l *Limiter) EffectiveLimit() int     { l.mu.Lock(); defer l.mu.Unlock(); return l.effectiveLocked() }
func (l *Limiter) Cooldown() time.Duration { return l.cooldown }

// LimiterStats is one atomic view. Active includes every caller-held lease
// (including a proxy failure hold); Cooldown starts only after release() is
// called. Occupied = Active + Cooldown, never Active + Cooldown + Withheld.
type LimiterStats struct {
	Revision     uint64
	Active       int64
	Waiters      int64
	TotalAcq     int64
	TotalRel     int64
	TotalTimeout int64
	Withheld     int64
	Occupied     int64
	Cooldown     int64
	Debt         int64
	Limit        int
	Effective    int
}

func (l *Limiter) statsLocked() LimiterStats {
	effective := l.effectiveLocked()
	return LimiterStats{Revision: l.revision, Active: l.active, Waiters: int64(l.pending.Len()), TotalAcq: l.totalAcquired,
		TotalRel: l.totalReleased, TotalTimeout: l.totalTimeout, Withheld: int64(min(l.withheld, max(0, l.limit-1))),
		Occupied: l.occupied, Cooldown: l.cooling, Debt: max(0, l.occupied-int64(effective)),
		Limit: l.limit, Effective: effective}
}
func (l *Limiter) Stats() LimiterStats { l.mu.Lock(); defer l.mu.Unlock(); return l.statsLocked() }

// SetObserver installs a bounded, memory-only transition observer and sends
// the initial state. The callback runs under the limiter lock to preserve
// event ordering; it must never call this limiter or perform I/O.
func (l *Limiter) SetObserver(fn func(time.Time, LimiterStats)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.observer = fn
	l.notifyLocked()
}
func (l *Limiter) notifyLocked() {
	if l.observer != nil {
		l.observer(time.Now(), l.statsLocked())
	}
}
