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
)

// Limiter is a concurrency-bounded blocking queue.
type Limiter struct {
	// slots is a buffered channel whose capacity equals the concurrency
	// limit. Holding a token from this channel means the caller owns a
	// slot.
	slots chan struct{}

	// waiters tracks how many goroutines are currently blocked in Acquire.
	waiters atomic.Int64

	// Stats (all atomically accessed).
	totalAcquired atomic.Int64
	totalReleased atomic.Int64
	totalTimeout  atomic.Int64
}

// NewLimiter creates a Limiter with the given concurrency limit.
// Panics if limit < 1.
func NewLimiter(limit int) *Limiter {
	if limit < 1 {
		panic("queue: concurrency limit must be >= 1")
	}
	l := &Limiter{
		slots: make(chan struct{}, limit),
	}
	// Pre-fill so the channel already contains `limit` tokens.
	for i := 0; i < limit; i++ {
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
				l.slots <- struct{}{}
				l.totalReleased.Add(1)
			})
		}, nil
	}
}

// Stats returns a snapshot of the limiter's current state.
func (l *Limiter) Stats() LimiterStats {
	return LimiterStats{
		Active:       int64(cap(l.slots)) - int64(len(l.slots)),
		Waiters:      l.waiters.Load(),
		TotalAcq:     l.totalAcquired.Load(),
		TotalRel:     l.totalReleased.Load(),
		TotalTimeout: l.totalTimeout.Load(),
	}
}

// LimiterStats is a point-in-time snapshot of limiter metrics.
type LimiterStats struct {
	Active       int64
	Waiters      int64
	TotalAcq     int64
	TotalRel     int64
	TotalTimeout int64
}
