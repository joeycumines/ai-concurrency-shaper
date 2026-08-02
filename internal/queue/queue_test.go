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
		l := NewLimiterWithCooldown(1, 0)
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
		NewLimiterWithCooldown(0, 0)
	})

	t.Run("negative limit panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic for limit < 1")
			}
		}()
		NewLimiterWithCooldown(-5, 0)
	})
}

func TestAcquire_ImmediateWhenSlotsAvailable(t *testing.T) {
	l := NewLimiterWithCooldown(2, 0)
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
	l := NewLimiterWithCooldown(1, 0)
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
	l := NewLimiterWithCooldown(1, 0)
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
	l := NewLimiterWithCooldown(1, 0)
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
	l := NewLimiterWithCooldown(1, 0)
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
	l := NewLimiterWithCooldown(1, 0)
	ctx := context.Background()

	// Exhaust the slot.
	rel1, err := l.Acquire(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Queue up 3 waiters.
	done := make(chan int, 3)
	var wg sync.WaitGroup
	for i := range 3 {
		id := i
		wg.Go(func() {
			rel, err := l.Acquire(ctx)
			if err != nil {
				t.Errorf("waiter %d: unexpected error: %v", id, err)
				return
			}
			done <- id
			rel()
		})
	}

	// Give waiters time to queue.
	time.Sleep(50 * time.Millisecond)

	// Release the initial slot; each waiter releases as it acquires.
	rel1()

	// Collect completion order (not necessarily FIFO — Go channels don't
	// guarantee strict FIFO across goroutines under contention).
	seen := make(map[int]bool)
	for range 3 {
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
	l := NewLimiterWithCooldown(4, 0)
	ctx := context.Background()
	const goroutines = 100
	const iterations = 50

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range iterations {
				rel, err := l.Acquire(ctx)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				rel()
			}
		})
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

func TestLimiter_PostReleaseCooldown(t *testing.T) {
	// Verify that a limiter with cooldown delays the return of tokens
	// after release. This creates a "dead zone" between slot release
	// and re-admission, preventing KILL-02 (slot release race).
	l := NewLimiterWithCooldown(1, 200*time.Millisecond)

	release, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Release the slot — with cooldown, the token should not be
	// immediately available.
	release()

	// Try to acquire immediately — should block because the token
	// is held for the cooldown duration.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = l.Acquire(ctx)
	if err == nil {
		t.Error("expected timeout acquiring slot during cooldown, got nil error")
	}

	// After the cooldown expires (~200ms), the slot should be available.
	time.Sleep(200 * time.Millisecond)
	release2, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire after cooldown: %v", err)
	}
	release2()
}

func TestLimiter_PostReleaseCooldownZero(t *testing.T) {
	// Verify that cooldown=0 behaves identically to NewLimiter (immediate release).
	l := NewLimiterWithCooldown(1, 0)

	release, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	// Should be immediately available.
	release2, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire after immediate release: %v", err)
	}
	release2()
}

func TestLimiter_AdaptiveReducesEffectiveLimit(t *testing.T) {
	l := NewLimiterWithCooldown(4, 0)
	ctx := context.Background()

	// Acquire all 4 slots.
	rels := make([]func(), 4)
	for i := range 4 {
		rel, err := l.Acquire(ctx)
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		rels[i] = rel
	}

	// A 5th acquire must block.
	blocked := make(chan func())
	go func() {
		rel, err := l.Acquire(ctx)
		if err != nil {
			t.Errorf("blocked acquire: %v", err)
			close(blocked)
			return
		}
		blocked <- rel
	}()

	// Give the goroutine time to enter the wait.
	time.Sleep(50 * time.Millisecond)
	if l.Stats().Waiters != 1 {
		t.Fatalf("expected 1 waiter, got %d", l.Stats().Waiters)
	}

	// Reduce after a 429. This should not unblock the waiter until a slot is released.
	if !l.AdaptiveReduce(100 * time.Millisecond) {
		t.Fatal("AdaptiveReduce should succeed with limit 4")
	}
	if l.Withheld() != 1 {
		t.Fatalf("expected Withheld=1, got %d", l.Withheld())
	}

	// Releasing one slot should absorb the headroom, not serve the waiter.
	rels[0]()
	time.Sleep(50 * time.Millisecond)
	if l.Stats().Waiters != 1 {
		t.Fatalf("waiter should still be blocked when slot is absorbed: waiters=%d", l.Stats().Waiters)
	}

	// Release the remaining 3 slots. Now the waiter should proceed.
	for i := 1; i < 4; i++ {
		rels[i]()
	}
	rel := <-blocked

	// Release the final slot and wait for the adaptive window to expire.
	rel()
	time.Sleep(200 * time.Millisecond)

	if l.Withheld() != 0 {
		t.Fatalf("expected Withheld=0 after restoration, got %d", l.Withheld())
	}

	// We should be able to acquire 4 slots again.
	for i := range 4 {
		r, err := l.Acquire(ctx)
		if err != nil {
			t.Fatalf("re-acquire %d: %v", i, err)
		}
		defer r()
	}
}

func TestLimiter_AdaptiveReduceExtendsWindow(t *testing.T) {
	l := NewLimiterWithCooldown(2, 0)
	window := 300 * time.Millisecond

	if !l.AdaptiveReduce(window) {
		t.Fatal("first AdaptiveReduce should succeed")
	}

	// Halfway through the window, reduce again.
	time.Sleep(window / 2)
	if !l.AdaptiveReduce(window) {
		t.Fatal("second AdaptiveReduce should succeed and extend the window")
	}

	// Wait until slightly after the original window would have expired.
	time.Sleep(window/2 + 100*time.Millisecond)
	if !l.AdaptiveActive() {
		t.Fatal("adaptive headroom should still be active because the window was extended")
	}

	// Wait for the extended window to expire.
	time.Sleep(window + 100*time.Millisecond)
	if l.AdaptiveActive() {
		t.Fatal("adaptive headroom should have expired")
	}
}

func TestLimiter_AdaptiveReduceRespectsMinimum(t *testing.T) {
	l := NewLimiterWithCooldown(1, 0)
	if l.AdaptiveReduce(time.Second) {
		t.Fatal("AdaptiveReduce should refuse to reduce limit below 1")
	}
}

func TestLimiter_AdaptiveReduceNoWindow(t *testing.T) {
	l := NewLimiterWithCooldown(2, 0)
	if l.AdaptiveReduce(0) {
		t.Fatal("AdaptiveReduce with zero window should be a no-op")
	}
}

func TestLimiter_AdaptiveStats(t *testing.T) {
	l := NewLimiterWithCooldown(3, 0)
	if l.Stats().Withheld != 0 {
		t.Fatalf("expected Withheld=0, got %d", l.Stats().Withheld)
	}
	l.AdaptiveReduce(100 * time.Millisecond)
	if l.Stats().Withheld != 1 {
		t.Fatalf("expected Withheld=1, got %d", l.Stats().Withheld)
	}
	if l.Stats().Active != 0 {
		t.Fatalf("expected Active=0, got %d", l.Stats().Active)
	}
}

func TestLimiter_AdaptiveStatsPendingAbsorbReportsActualActive(t *testing.T) {
	l := NewLimiterWithCooldown(4, 0)
	ctx := context.Background()

	rels := make([]func(), 4)
	for i := range 4 {
		rel, err := l.Acquire(ctx)
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		rels[i] = rel
	}

	if !l.AdaptiveReduce(time.Minute) {
		t.Fatal("AdaptiveReduce should succeed")
	}
	if l.pendingAbsorbs != 1 {
		t.Fatalf("expected 1 pending absorption while all slots are held, got %d", l.pendingAbsorbs)
	}
	if got := l.Stats().Active; got != 4 {
		t.Fatalf("Active while pendingAbsorbs is pending = %d, want 4", got)
	}

	rels[0]()
	if l.pendingAbsorbs != 0 {
		t.Fatalf("expected first release to be absorbed, pendingAbsorbs = %d", l.pendingAbsorbs)
	}
	if got := l.Stats().Active; got != 3 {
		t.Fatalf("Active after one absorbed release = %d, want 3", got)
	}

	for _, rel := range rels[1:] {
		rel()
	}
}

func TestLimiter_AdaptiveStatsDrainedIdleReportsActualActive(t *testing.T) {
	l := NewLimiterWithCooldown(4, 0)
	ctx := context.Background()

	rels := make([]func(), 3)
	for i := range 3 {
		rel, err := l.Acquire(ctx)
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		rels[i] = rel
	}

	if !l.AdaptiveReduce(time.Minute) {
		t.Fatal("AdaptiveReduce should succeed")
	}
	if l.pendingAbsorbs != 0 {
		t.Fatalf("idle token should be drained immediately, pendingAbsorbs = %d", l.pendingAbsorbs)
	}
	if got := l.Stats().Active; got != 3 {
		t.Fatalf("Active after draining idle token = %d, want 3", got)
	}

	for _, rel := range rels {
		rel()
	}
}

func TestLimiter_AdaptiveStaleTimerRestoreIgnored(t *testing.T) {
	l := NewLimiterWithCooldown(2, 0)
	if !l.AdaptiveReduce(time.Minute) {
		t.Fatal("AdaptiveReduce should succeed")
	}

	l.restoreAdaptiveSlot(0)

	if !l.AdaptiveActive() {
		t.Fatal("stale restore must not clear active adaptive headroom")
	}
	if got := l.Withheld(); got != 1 {
		t.Fatalf("Withheld after stale restore = %d, want 1", got)
	}
}

func TestLimiter_AdaptiveReduceWithCooldownAbsorbsRelease(t *testing.T) {
	l := NewLimiterWithCooldown(2, 200*time.Millisecond)
	rel1, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	rel2, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// 429 while both slots are held. No idle token can be drained.
	if !l.AdaptiveReduce(100 * time.Millisecond) {
		t.Fatal("AdaptiveReduce should succeed")
	}
	if l.Withheld() != 1 {
		t.Fatalf("expected Withheld=1, got %d", l.Withheld())
	}

	// Release the first slot. With cooldown, the token returns after the
	// cooldown unless adaptive headroom absorbs it first.
	rel1()

	// Immediately try to acquire another slot. The effective limit is now 1,
	// so this must block/timeout even after the cooldown fires.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = l.Acquire(ctx)
	if err == nil {
		t.Fatal("expected acquire to fail while adaptive headroom is active")
	}

	// Release the second slot and wait for the cooldown + window to expire.
	rel2()
	time.Sleep(300 * time.Millisecond)

	// The slot should be restored and the effective limit back to 2.
	if l.AdaptiveActive() {
		t.Fatal("adaptive headroom should have expired")
	}
	if l.EffectiveLimit() != 2 {
		t.Fatalf("expected effective limit 2, got %d", l.EffectiveLimit())
	}
}

func TestLimiter_AdaptiveReduceWithCooldownDrainsIdle(t *testing.T) {
	l := NewLimiterWithCooldown(2, 200*time.Millisecond)
	rel1, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// One slot is idle when the 429 arrives. AdaptiveReduce should drain it.
	if !l.AdaptiveReduce(100 * time.Millisecond) {
		t.Fatal("AdaptiveReduce should succeed")
	}
	if l.EffectiveLimit() != 1 {
		t.Fatalf("expected effective limit 1, got %d", l.EffectiveLimit())
	}

	// A new acquire must block because the idle slot was drained.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = l.Acquire(ctx)
	if err == nil {
		t.Fatal("expected acquire to fail after idle slot drained")
	}
	cancel()

	rel1()
	time.Sleep(200 * time.Millisecond)

	if l.AdaptiveActive() {
		t.Fatal("adaptive headroom should have expired")
	}
	if l.EffectiveLimit() != 2 {
		t.Fatalf("expected effective limit 2 after recovery, got %d", l.EffectiveLimit())
	}
}

func TestLimiter_SetMaxWithheld_DefaultsToOne(t *testing.T) {
	l := NewLimiterWithCooldown(4, 0)
	if got := l.MaxWithheld(); got != 1 {
		t.Fatalf("expected default MaxWithheld=1, got %d", got)
	}
}

func TestLimiter_SetMaxWithheld_Configurable(t *testing.T) {
	l := NewLimiterWithCooldown(8, 0)
	l.SetMaxWithheld(4)
	if got := l.MaxWithheld(); got != 4 {
		t.Fatalf("expected MaxWithheld=4, got %d", got)
	}
}

func TestLimiter_WithholdSlotReducesEffectiveLimit(t *testing.T) {
	l := NewLimiterWithCooldown(4, 0)
	l.SetMaxWithheld(3)

	if !l.WithholdSlot() {
		t.Fatal("WithholdSlot should succeed")
	}
	if l.EffectiveLimit() != 3 {
		t.Fatalf("expected effective limit 3, got %d", l.EffectiveLimit())
	}
	if l.Withheld() != 1 {
		t.Fatalf("expected Withheld=1, got %d", l.Withheld())
	}

	if !l.WithholdSlot() {
		t.Fatal("second WithholdSlot should succeed")
	}
	if l.EffectiveLimit() != 2 {
		t.Fatalf("expected effective limit 2, got %d", l.EffectiveLimit())
	}
}

func TestLimiter_WithholdSlotRespectsMaxWithheld(t *testing.T) {
	l := NewLimiterWithCooldown(4, 0)
	l.SetMaxWithheld(2)

	if !l.WithholdSlot() {
		t.Fatal("first WithholdSlot should succeed")
	}
	if !l.WithholdSlot() {
		t.Fatal("second WithholdSlot should succeed")
	}
	// At maxWithheld=2, third should fail.
	if l.WithholdSlot() {
		t.Fatal("third WithholdSlot should fail at maxWithheld=2")
	}
	if l.EffectiveLimit() != 2 {
		t.Fatalf("expected effective limit 2, got %d", l.EffectiveLimit())
	}
}

func TestLimiter_WithholdSlotRespectsMinimum(t *testing.T) {
	l := NewLimiterWithCooldown(2, 0)
	l.SetMaxWithheld(2)

	// Withhold one (effective limit drops to 1).
	if !l.WithholdSlot() {
		t.Fatal("first WithholdSlot should succeed")
	}
	// Withholding a second would reduce effective limit to 0 — must refuse.
	if l.WithholdSlot() {
		t.Fatal("WithholdSlot should refuse to reduce effective limit below 1")
	}
	if l.EffectiveLimit() != 1 {
		t.Fatalf("expected effective limit 1, got %d", l.EffectiveLimit())
	}
}

func TestLimiter_WithholdSlotDefaultMaxIsOne(t *testing.T) {
	l := NewLimiterWithCooldown(4, 0)
	// Default maxWithheld is 1.
	if !l.WithholdSlot() {
		t.Fatal("first WithholdSlot should succeed")
	}
	if l.WithholdSlot() {
		t.Fatal("second WithholdSlot should fail with default maxWithheld=1")
	}
}

func TestLimiter_RestoreSlotIncreasesEffectiveLimit(t *testing.T) {
	l := NewLimiterWithCooldown(4, 0)
	l.SetMaxWithheld(3)

	// Withhold 2 slots.
	l.WithholdSlot()
	l.WithholdSlot()
	if l.EffectiveLimit() != 2 {
		t.Fatalf("expected effective limit 2, got %d", l.EffectiveLimit())
	}

	// Restore one.
	l.RestoreSlot()
	if l.EffectiveLimit() != 3 {
		t.Fatalf("expected effective limit 3 after restore, got %d", l.EffectiveLimit())
	}
	if l.Withheld() != 1 {
		t.Fatalf("expected Withheld=1 after restore, got %d", l.Withheld())
	}

	// Restore another.
	l.RestoreSlot()
	if l.EffectiveLimit() != 4 {
		t.Fatalf("expected effective limit 4 after second restore, got %d", l.EffectiveLimit())
	}
	if l.Withheld() != 0 {
		t.Fatalf("expected Withheld=0, got %d", l.Withheld())
	}
}

func TestLimiter_RestoreSlotNoopWhenNothingWithheld(t *testing.T) {
	l := NewLimiterWithCooldown(4, 0)
	// Restoring when nothing is withheld should be a no-op.
	l.RestoreSlot()
	if l.EffectiveLimit() != 4 {
		t.Fatalf("expected effective limit 4, got %d", l.EffectiveLimit())
	}
	if l.Withheld() != 0 {
		t.Fatalf("expected Withheld=0, got %d", l.Withheld())
	}
}

func TestLimiter_RestoreSlotClearsPendingAbsorb(t *testing.T) {
	ctx := context.Background()
	l := NewLimiterWithCooldown(4, 0)
	l.SetMaxWithheld(3)

	// Acquire all 4 slots so there are no idle tokens.
	rels := make([]func(), 4)
	for i := range 4 {
		rel, err := l.Acquire(ctx)
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		rels[i] = rel
	}

	// WithholdSlot when all slots are held: pendingAbsorbs is set, no token drained.
	if !l.WithholdSlot() {
		t.Fatal("WithholdSlot should succeed")
	}
	if l.pendingAbsorbs != 1 {
		t.Fatalf("expected pendingAbsorbs=1 when all slots held, got %d", l.pendingAbsorbs)
	}
	if l.EffectiveLimit() != 3 {
		t.Fatalf("expected effective limit 3, got %d", l.EffectiveLimit())
	}

	// RestoreSlot: should clear the pending absorb without inserting a token.
	l.RestoreSlot()
	if l.pendingAbsorbs != 0 {
		t.Fatalf("pendingAbsorbs should be 0 after RestoreSlot, got %d", l.pendingAbsorbs)
	}
	if l.EffectiveLimit() != 4 {
		t.Fatalf("expected effective limit 4, got %d", l.EffectiveLimit())
	}
	if l.Withheld() != 0 {
		t.Fatalf("expected Withheld=0, got %d", l.Withheld())
	}

	// Release all slots — all 4 should be available.
	for _, rel := range rels {
		rel()
	}
	for i := range 4 {
		rel, err := l.Acquire(ctx)
		if err != nil {
			t.Fatalf("re-acquire %d after restore: %v", i, err)
		}
		rel()
	}
}

func TestLimiter_WithholdThenRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	l := NewLimiterWithCooldown(6, 0)
	l.SetMaxWithheld(5)

	// Withhold 3 slots.
	for range 3 {
		if !l.WithholdSlot() {
			t.Fatal("WithholdSlot should succeed")
		}
	}
	if l.EffectiveLimit() != 3 {
		t.Fatalf("expected effective limit 3, got %d", l.EffectiveLimit())
	}

	// Verify we can acquire exactly 3 and no more.
	rels := make([]func(), 3)
	for i := range 3 {
		rel, err := l.Acquire(ctx)
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		rels[i] = rel
	}

	// 4th acquire should block.
	done := make(chan error)
	go func() {
		_, err := l.Acquire(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if l.Stats().Waiters != 1 {
		t.Fatalf("expected 1 waiter, got %d", l.Stats().Waiters)
	}

	// Restore 1 slot → effective limit becomes 4. The blocked acquire should
	// now proceed.
	l.RestoreSlot()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocked acquire after restore: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked acquire did not proceed after restore")
	}

	if l.EffectiveLimit() != 4 {
		t.Fatalf("expected effective limit 4, got %d", l.EffectiveLimit())
	}

	// Restore the remaining 2 withholds.
	l.RestoreSlot()
	l.RestoreSlot()
	if l.EffectiveLimit() != 6 {
		t.Fatalf("expected effective limit 6, got %d", l.EffectiveLimit())
	}
}

// TestLimiter_WithholdMultipleWhileAllHeld is a regression test for a deadlock
// that occurred when WithholdSlot was called several times while every slot was
// in use. The old single-boolean absorb flag could only track ONE pending
// absorb, so only one in-flight release was swallowed; the others leaked tokens
// back into the channel, inflating len(slots) past the effective limit. A later
// RestoreSlot then blocked forever on a full channel. The fix tracks pending
// absorbs with a counter, so each virtual withhold independently consumes a
// future release.
func TestLimiter_WithholdMultipleWhileAllHeld(t *testing.T) {
	ctx := context.Background()
	const limit = 8
	l := NewLimiterWithCooldown(limit, 0)
	l.SetMaxWithheld(limit - 1)

	// Acquire all slots so there are no idle tokens to drain.
	rels := make([]func(), limit)
	for i := range limit {
		rel, err := l.Acquire(ctx)
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		rels[i] = rel
	}

	// Withhold 4 slots while all are held. Each withhold registers a pending
	// absorb that must consume a future release.
	const withholds = 4
	for i := range withholds {
		if !l.WithholdSlot() {
			t.Fatalf("WithholdSlot %d should succeed", i)
		}
	}
	if l.pendingAbsorbs != withholds {
		t.Fatalf("pendingAbsorbs = %d, want %d", l.pendingAbsorbs, withholds)
	}
	if eff := l.EffectiveLimit(); eff != limit-withholds {
		t.Fatalf("EffectiveLimit = %d, want %d", eff, limit-withholds)
	}

	// Release slots one at a time. The first `withholds` releases must be
	// swallowed (absorbed) — they must NOT return tokens to the channel,
	// otherwise len(slots) would exceed the effective limit.
	for i := range withholds {
		rels[i]()
		if l.pendingAbsorbs != withholds-(i+1) {
			t.Fatalf("after release %d: pendingAbsorbs = %d, want %d", i, l.pendingAbsorbs, withholds-(i+1))
		}
		// The channel should still be empty: no tokens returned yet.
		if got := len(l.slots); got != 0 {
			t.Fatalf("after absorbed release %d: channel len = %d, want 0 (token leaked)", i, got)
		}
	}

	// Remaining releases (withholds..limit) should return tokens normally.
	for i := withholds; i < limit; i++ {
		rels[i]()
	}
	if got := len(l.slots); got != limit-withholds {
		t.Fatalf("after all releases: channel len = %d, want %d", got, limit-withholds)
	}

	// Now restore the withheld slots one at a time. Each RestoreSlot must
	// insert a real token. This is the step that DEADLOCKED before the fix:
	// the channel was over-full so the send blocked forever.
	for range withholds {
		done := make(chan struct{})
		go func() {
			l.RestoreSlot()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("RestoreSlot deadlocked (regression: multi-withhold while all slots held)")
		}
	}

	if l.Withheld() != 0 {
		t.Fatalf("Withheld = %d, want 0 after full restore", l.Withheld())
	}
	if eff := l.EffectiveLimit(); eff != limit {
		t.Fatalf("EffectiveLimit = %d, want %d", eff, limit)
	}
	// Channel must be exactly full now.
	if got := len(l.slots); got != limit {
		t.Fatalf("after full restore: channel len = %d, want %d", got, limit)
	}

	// All slots must be acquirable again.
	for range limit {
		rel, err := l.Acquire(ctx)
		if err != nil {
			t.Fatalf("re-acquire after full restore: %v", err)
		}
		rel()
	}
}
