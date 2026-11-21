package control

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
)

func fixture(t *testing.T) (*Controller, *queue.Limiter) {
	t.Helper()
	c := New()
	l := queue.NewLimiterWithCooldown(3, 0)
	if err := c.RegisterProvider("primary", map[string]*queue.Limiter{"default": l}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, l
}
func apply(t *testing.T, c *Controller, cmd Command) Result {
	t.Helper()
	p, err := c.Preview(cmd)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Apply(p.Token)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func waitState(t *testing.T, fn func() bool) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("state transition timed out")
}

func TestPreviewFrozenRevisionAndIdempotentApply(t *testing.T) {
	c, l := fixture(t)
	p, err := c.Preview(Command{Action: Resize, Provider: "primary", Pool: "default", Limit: 7})
	if err != nil {
		t.Fatal(err)
	}
	if l.Limit() != 3 {
		t.Fatal("preview mutated pool")
	}
	p.Command.Limit = 99
	p.Consequences[0] = "altered"
	r, err := c.Apply(p.Token)
	if err != nil {
		t.Fatal(err)
	}
	if l.Limit() != 7 || r.Command.Limit != 7 {
		t.Fatalf("caller modified frozen command: %+v", r)
	}
	again, err := c.Apply(p.Token)
	if err != nil || again != r {
		t.Fatalf("idempotence got%+v err%v", again, err)
	}
	if len(c.Snapshot().Audit) != 1 {
		t.Fatal("duplicate audit")
	}
	stale, err := c.Preview(Command{Action: Pause, Provider: "primary"})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, Command{Action: Drain, Provider: "primary"})
	if _, err = c.Apply(stale.Token); !errors.Is(err, ErrStale) {
		t.Fatalf("stale apply %v", err)
	}
	if c.Snapshot().Providers[0].State != Draining {
		t.Fatal("stale action changed state")
	}
}

func TestPauseContextDrainAndResume(t *testing.T) {
	c, _ := fixture(t)
	apply(t, c, Command{Action: Pause, Provider: "primary"})
	waiting := make(chan struct{})
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		done <- c.GateObserved(ctx, "primary", func(v bool) {
			if v {
				close(waiting)
			}
		})
	}()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-done:
		t.Fatalf("pause passed: %v", err)
	default:
	}
	apply(t, c, Command{Action: Drain, Provider: "primary"})
	if err := <-done; !errors.Is(err, ErrDraining) {
		t.Fatalf("paused arrival after drain=%v", err)
	}
	if err := c.Gate(ctx, "primary"); !errors.Is(err, ErrDraining) {
		t.Fatal(err)
	}
	apply(t, c, Command{Action: Resume, Provider: "primary"})
	if err := c.Gate(ctx, "primary"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := c.Gate(cancelled, "primary"); !errors.Is(err, context.Canceled) {
		t.Fatalf("resume ignored cancellation: %v", err)
	}
}

func TestTTLRestoresCurrentBaselineAndIgnoresSupersededTimer(t *testing.T) {
	c, l := fixture(t)
	apply(t, c, Command{Action: Resize, Provider: "primary", Pool: "default", Limit: 5})
	apply(t, c, Command{Action: Resize, Provider: "primary", Pool: "default", Limit: 1, TTL: 20 * time.Millisecond})
	waitState(t, func() bool { return l.Limit() == 5 })
	apply(t, c, Command{Action: Resize, Provider: "primary", Pool: "default", Limit: 2, TTL: 50 * time.Millisecond})
	apply(t, c, Command{Action: Resize, Provider: "primary", Pool: "default", Limit: 6})
	time.Sleep(70 * time.Millisecond)
	if l.Limit() != 6 {
		t.Fatal("superseded TTL undid new baseline")
	}
	apply(t, c, Command{Action: Pause, Provider: "primary", TTL: 20 * time.Millisecond})
	waitState(t, func() bool { return c.Snapshot().Providers[0].State == Running })
	s := c.Snapshot()
	if s.Providers[0].Pools[0].Baseline != 6 {
		t.Fatal("baseline mismatch")
	}
	found := 0
	for _, a := range s.Audit {
		if a.Kind == "expired" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("expiry audit count %d", found)
	}
}

func TestPreviewExpiryBoundsAndCancellation(t *testing.T) {
	c, _ := fixture(t)
	p, err := c.Preview(Command{Action: Pause, Provider: "primary"})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.prepared[p.Token].preview.ExpiresAt = time.Now().Add(-time.Second)
	c.mu.Unlock()
	if _, err = c.Apply(p.Token); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	if c.Snapshot().Providers[0].State != Running {
		t.Fatal("expired preview changed state")
	}
	for _, cmd := range []Command{{Action: Resize, Provider: "primary", Pool: "missing", Limit: 1}, {Action: Resize, Provider: "primary", Pool: "default", Limit: 0}, {Action: Pause, Provider: "primary", TTL: MaxTTL + 1}, {Action: Cancel, Provider: "primary", RequestID: "x"}} {
		if _, err = c.Preview(cmd); err == nil {
			t.Fatalf("accepted invalid command %+v", cmd)
		}
	}
	var cancelled string
	c.SetCanceller(func(id string) bool { cancelled = id; return id == "exact-live-id" })
	cp, err := c.Preview(Command{Action: Cancel, Provider: "primary", RequestID: "exact-live-id"})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled != "" {
		t.Fatal("preview cancelled request")
	}
	if _, err = c.Apply(cp.Token); err != nil || cancelled != "exact-live-id" {
		t.Fatalf("cancel id=%q err=%v", cancelled, err)
	}
	for range HistoryLimit + 20 {
		apply(t, c, Command{Action: Resume, Provider: "primary"})
	}
	if s := c.Snapshot(); len(s.Audit) != HistoryLimit {
		t.Fatalf("audit length=%d", len(s.Audit))
	}
	c.mu.Lock()
	if len(c.prepared) > HistoryLimit || len(c.order) > HistoryLimit {
		t.Error("unbounded previews")
	}
	c.mu.Unlock()
}

func TestConcurrentPreviewApplySnapshotAndClose(t *testing.T) {
	c, _ := fixture(t)
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for i := range 40 {
				cmd := Command{Action: Resize, Provider: "primary", Pool: "default", Limit: 1 + (i+worker)%6}
				p, err := c.Preview(cmd)
				if err == nil {
					_, err = c.Apply(p.Token)
					if err != nil && !errors.Is(err, ErrStale) {
						t.Errorf("Apply: %v", err)
					}
				}
				s := c.Snapshot()
				if len(s.Providers) != 1 {
					t.Error("torn snapshot")
				}
			}
		})
	}
	wg.Wait()
	apply(t, c, Command{Action: Pause, Provider: "primary"})
	done := make(chan error, 1)
	go func() { done <- c.Gate(context.Background(), "primary") }()
	c.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close stranded paused arrival")
	}
}

func TestIndependentPoolResizeInvalidatesPreviewAndTTL(t *testing.T) {
	c, l := fixture(t)
	preview, err := c.Preview(Command{Action: Resize, Provider: "primary", Pool: "default", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Resize(4); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Apply(preview.Token); !errors.Is(err, ErrStale) {
		t.Fatalf("independent resize did not invalidate preview: %v", err)
	}
	if l.Limit() != 4 {
		t.Fatal("stale preview changed independently resized pool")
	}
	apply(t, c, Command{Action: Resize, Provider: "primary", Pool: "default", Limit: 2, TTL: 20 * time.Millisecond})
	if err := l.Resize(6); err != nil {
		t.Fatal(err)
	}
	waitState(t, func() bool { return c.Snapshot().Providers[0].Pools[0].ExpiresAt.IsZero() })
	if l.Limit() != 6 {
		t.Fatal("TTL overwrote an independent newer resize")
	}
	audit := c.Snapshot().Audit
	if last := audit[len(audit)-1]; last.Kind != "superseded" || last.Applied {
		t.Fatalf("expiry claimed a restoration it did not perform: %+v", last)
	}
}

func TestCancelBeforeProviderResolutionUsesExactID(t *testing.T) {
	c := New()
	defer c.Close()
	var got string
	c.SetCanceller(func(id string) bool { got = id; return true })
	apply(t, c, Command{Action: Cancel, Provider: "__unresolved__", RequestID: "live-before-suite-resolution"})
	if got != "live-before-suite-resolution" {
		t.Fatalf("cancelled %q", got)
	}
	if len(c.Snapshot().Providers) != 0 {
		t.Fatal("cancel invented a provider")
	}
}
