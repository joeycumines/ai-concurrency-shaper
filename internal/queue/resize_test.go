package queue

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func awaitQueue(t *testing.T, l *Limiter, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if l.Stats().Waiters == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiters=%d want%d", l.Stats().Waiters, want)
}

func TestResizePreservesOutstandingLeasesAndFIFO(t *testing.T) {
	l := NewLimiterWithCooldown(3, 0)
	releases := make([]func(), 3)
	for i := range releases {
		r, err := l.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases[i] = r
	}
	if err := l.Resize(1); err != nil {
		t.Fatal(err)
	}
	if s := l.Stats(); s.Active != 3 || s.Occupied != 3 || s.Debt != 2 || s.Effective != 1 {
		t.Fatalf("shrink lost leases: %+v", s)
	}
	type granted struct {
		n       int
		release func()
	}
	acquired := make(chan granted, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := range 3 {
		go func() {
			r, err := l.Acquire(ctx)
			if err == nil {
				acquired <- granted{i, r}
			}
		}()
		awaitQueue(t, l, int64(i+1))
	}
	releases[0]()
	releases[1]()
	select {
	case x := <-acquired:
		x.release()
		t.Fatal("admitted while occupied equalled shrunken limit")
	default:
	}
	releases[2]()
	for i := range 3 {
		select {
		case x := <-acquired:
			if x.n != i {
				t.Errorf("FIFO order got%d want%d", x.n, i)
			}
			x.release()
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if s := l.Stats(); s.Occupied != 0 || s.Active != 0 || s.Waiters != 0 || s.TotalAcq != s.TotalRel {
		t.Fatalf("lease conservation: %+v", s)
	}
}

func TestResizeRetainsCooldownAndAdaptiveHeadroom(t *testing.T) {
	l := NewLimiterWithCooldown(2, 80*time.Millisecond)
	first, _ := l.Acquire(context.Background())
	second, _ := l.Acquire(context.Background())
	if !l.AdaptiveReduce(time.Second) {
		t.Fatal("expected adaptive reduction")
	}
	first()
	second()
	if err := l.Resize(1); err != nil {
		t.Fatal(err)
	}
	if s := l.Stats(); s.Active != 0 || s.Cooldown != 2 || s.Occupied != 2 || s.Debt != 1 || s.Effective != 1 {
		t.Fatalf("cooldown disappeared on shrink: %+v", s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if r, err := l.Acquire(ctx); err == nil {
		r()
		t.Fatal("resize bypassed cooldown")
	}
	if err := l.Resize(3); err != nil {
		t.Fatal(err)
	}
	// Effective3-1=2, both still occupied. Growing does not invent old slots.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if r, err := l.Acquire(ctx2); err == nil {
		r()
		t.Fatal("growth bypassed retained occupied cooldowns")
	}
	ctx3, cancel3 := context.WithTimeout(context.Background(), time.Second)
	defer cancel3()
	r, err := l.Acquire(ctx3)
	if err != nil {
		t.Fatal(err)
	}
	r()
}

func TestAcquireObservedMeasuresOnlyBlockedWaits(t *testing.T) {
	l := NewLimiterWithCooldown(1, 0)
	var transitions []bool
	callback := func(v bool) { transitions = append(transitions, v) }
	r, err := l.AcquireObserved(context.Background(), callback)
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 0 {
		t.Fatalf("idle acquire claimed wait: %v", transitions)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err = l.AcquireObserved(ctx, callback); err == nil {
		t.Fatal("expected timeout")
	}
	r()
	if len(transitions) != 2 || !transitions[0] || transitions[1] {
		t.Fatalf("wait brackets=%v", transitions)
	}
}

func TestResizeConcurrentCancellationConservesLeases(t *testing.T) {
	l := NewLimiterWithCooldown(4, 0)
	var wg sync.WaitGroup
	errorsCh := make(chan error, 64)
	for worker := range 16 {
		wg.Go(func() {
			for i := range 150 {
				ctx, cancel := context.WithCancel(context.Background())
				if (i+worker)%5 == 0 {
					cancel()
				}
				release, err := l.Acquire(ctx)
				if err == nil {
					if s := l.Stats(); s.Active < 1 || s.Occupied < s.Active {
						errorsCh <- fmt.Errorf("impossible state %+v", s)
					}
					release()
					release()
				}
				cancel()
			}
		})
	}
	wg.Go(func() {
		for i := range 300 {
			if err := l.Resize(1 + i%7); err != nil {
				errorsCh <- err
			}
		}
	})
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	s := l.Stats()
	if s.Active != 0 || s.Occupied != 0 || s.Cooldown != 0 || s.Waiters != 0 || s.TotalAcq != s.TotalRel {
		t.Fatalf("leak or double-release: %+v", s)
	}
}

func TestResizeRejectsInvalidLimitsWithoutMutation(t *testing.T) {
	l := NewLimiterWithCooldown(2, 0)
	for _, n := range []int{-1, 0, 1_000_001} {
		if err := l.Resize(n); err == nil {
			t.Fatalf("accepted %d", n)
		}
		if l.Limit() != 2 {
			t.Fatal("invalid resize mutated limit")
		}
	}
}

func TestResizeMinimumReportsEffectiveAdaptiveWithholding(t *testing.T) {
	l := NewLimiterWithCooldown(3, 0)
	if !l.AdaptiveReduce(time.Minute) {
		t.Fatal("adaptive reduction missing")
	}
	if err := l.Resize(1); err != nil {
		t.Fatal(err)
	}
	if s := l.Stats(); s.Effective != 1 || s.Withheld != 0 || l.Withheld() != 0 {
		t.Fatalf("clamped capacity overstated withheld slots: %+v", s)
	}
	if !l.AdaptiveActive() {
		t.Fatal("shrinking discarded active recovery window")
	}
	if err := l.Resize(3); err != nil {
		t.Fatal(err)
	}
	if s := l.Stats(); s.Effective != 2 || s.Withheld != 1 {
		t.Fatalf("growth discarded adaptive protection: %+v", s)
	}
}
