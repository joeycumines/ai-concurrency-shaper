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

package queue

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNewLimiter_ValidatesLimit(t *testing.T) {
	t.Run("limit 1 works", func(t *testing.T) {
		l := NewLimiter(1)
		if l == nil {
			t.Fatal("expected non-nil limiter")
		}
	})

	t.Run("limit 0 panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic for limit < 1")
			}
		}()
		NewLimiter(0)
	})

	t.Run("negative limit panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic for limit < 1")
			}
		}()
		NewLimiter(-5)
	})
}

func TestAcquire_ImmediateWhenSlotsAvailable(t *testing.T) {
	l := NewLimiter(2)
	ctx := context.Background()

	rel, err := l.Acquire(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rel == nil {
		t.Fatal("expected non-nil release func")
	}

	stats := l.Stats()
	if stats.Active != 1 {
		t.Errorf("expected 1 active, got %d", stats.Active)
	}

	rel()
	stats = l.Stats()
	if stats.Active != 0 {
		t.Errorf("expected 0 active after release, got %d", stats.Active)
	}
}

func TestAcquire_BlocksWhenExhausted(t *testing.T) {
	l := NewLimiter(1)
	ctx := context.Background()

	// Exhaust the single slot.
	rel, err := l.Acquire(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Next acquire should block.
	done := make(chan struct{})
	var acquireErr error
	var acquireRel func()

	go func() {
		acquireRel, acquireErr = l.Acquire(ctx)
		close(done)
	}()

	// Give the goroutine time to enter the wait.
	time.Sleep(50 * time.Millisecond)

	stats := l.Stats()
	if stats.Waiters != 1 {
		t.Errorf("expected 1 waiter, got %d", stats.Waiters)
	}

	// Release the slot — the blocked acquire should unblock.
	rel()
	<-done

	if acquireErr != nil {
		t.Fatalf("unexpected error from blocked acquire: %v", acquireErr)
	}
	if acquireRel == nil {
		t.Fatal("expected non-nil release func from blocked acquire")
	}
	acquireRel()
}

func TestAcquire_ContextCancel(t *testing.T) {
	l := NewLimiter(1)
	ctx := context.Background()

	// Exhaust the slot.
	rel, err := l.Acquire(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer rel()

	// Cancel the context immediately.
	ctx2, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = l.Acquire(ctx2)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	stats := l.Stats()
	if stats.TotalTimeout != 1 {
		t.Errorf("expected 1 timeout, got %d", stats.TotalTimeout)
	}
}

func TestAcquire_ContextTimeout(t *testing.T) {
	l := NewLimiter(1)
	ctx := context.Background()

	// Exhaust the slot.
	rel, err := l.Acquire(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer rel()

	// Use a short timeout.
	ctx2, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = l.Acquire(ctx2)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestRelease_Idempotent(t *testing.T) {
	l := NewLimiter(1)
	ctx := context.Background()

	rel, err := l.Acquire(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Calling release multiple times should not panic or over-fill.
	rel()
	rel()
	rel()

	stats := l.Stats()
	if stats.TotalRel != 1 {
		t.Errorf("expected 1 total release, got %d", stats.TotalRel)
	}
}

func TestWaitersEventuallyServed(t *testing.T) {
	l := NewLimiter(1)
	ctx := context.Background()

	// Exhaust the slot.
	rel1, err := l.Acquire(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Queue up 3 waiters.
	done := make(chan int, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		id := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := l.Acquire(ctx)
			if err != nil {
				t.Errorf("waiter %d: unexpected error: %v", id, err)
				return
			}
			done <- id
			rel()
		}()
	}

	// Give waiters time to queue.
	time.Sleep(50 * time.Millisecond)

	// Release the initial slot; each waiter releases as it acquires.
	rel1()

	// Collect completion order (not necessarily FIFO — Go channels don't
	// guarantee strict FIFO across goroutines under contention).
	seen := make(map[int]bool)
	for i := 0; i < 3; i++ {
		id := <-done
		if seen[id] {
			t.Errorf("waiter %d completed twice", id)
		}
		seen[id] = true
	}
	wg.Wait()

	// All 3 should have completed.
	if len(seen) != 3 {
		t.Errorf("expected 3 completions, got %d", len(seen))
	}
}

func TestConcurrentAcquireRelease(t *testing.T) {
	l := NewLimiter(4)
	ctx := context.Background()
	const goroutines = 100
	const iterations = 50

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				rel, err := l.Acquire(ctx)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				rel()
			}
		}()
	}
	wg.Wait()

	stats := l.Stats()
	expected := int64(goroutines * iterations)
	if stats.TotalAcq != expected {
		t.Errorf("expected %d total acquired, got %d", expected, stats.TotalAcq)
	}
	if stats.TotalRel != expected {
		t.Errorf("expected %d total released, got %d", expected, stats.TotalRel)
	}
	if stats.Active != 0 {
		t.Errorf("expected 0 active, got %d", stats.Active)
	}
}
