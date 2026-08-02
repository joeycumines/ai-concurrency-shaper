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

package autoscale

import (
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
)

func TestNew_ValidatesLimiter(t *testing.T) {
	_, err := New()
	if err == nil {
		t.Fatal("expected error for nil limiter")
	}
}

func TestNew_ValidatesMin(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)
	_, err := New(WithLimiter(l), WithMin(0))
	if err == nil {
		t.Fatal("expected error for min < 1")
	}
}

func TestNew_ValidatesMaxGEMin(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)
	_, err := New(WithLimiter(l), WithMin(4), WithMax(2))
	if err == nil {
		t.Fatal("expected error for max < min")
	}
}

func TestNew_ValidatesInitialInRange(t *testing.T) {
	t.Run("initial below min", func(t *testing.T) {
		l := queue.NewLimiterWithCooldown(8, 0)
		_, err := New(WithLimiter(l), WithMin(2), WithMax(8), WithInitial(1))
		if err == nil {
			t.Fatal("expected error for initial < min")
		}
	})

	t.Run("initial above max", func(t *testing.T) {
		l := queue.NewLimiterWithCooldown(8, 0)
		_, err := New(WithLimiter(l), WithMin(2), WithMax(8), WithInitial(9))
		if err == nil {
			t.Fatal("expected error for initial > max")
		}
	})
}

func TestNew_ValidatesDecreaseRatio(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)

	t.Run("zero ratio", func(t *testing.T) {
		_, err := New(WithLimiter(l), WithMax(8), WithDecreaseRatio(0))
		if err == nil {
			t.Fatal("expected error for ratio <= 0")
		}
	})

	t.Run("negative ratio", func(t *testing.T) {
		_, err := New(WithLimiter(l), WithMax(8), WithDecreaseRatio(-0.5))
		if err == nil {
			t.Fatal("expected error for ratio <= 0")
		}
	})

	t.Run("ratio above 1", func(t *testing.T) {
		_, err := New(WithLimiter(l), WithMax(8), WithDecreaseRatio(1.5))
		if err == nil {
			t.Fatal("expected error for ratio > 1")
		}
	})
}

func TestNew_AppliesDefaults(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)
	a, err := New(WithLimiter(l), WithMax(8))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := a.Stats()
	if s.Min != 1 {
		t.Errorf("default Min = %d, want 1", s.Min)
	}
	if s.IncreaseStep != 1 {
		t.Errorf("default IncreaseStep = %d, want 1", s.IncreaseStep)
	}
	if s.DecreaseRatio != 0.5 {
		t.Errorf("default DecreaseRatio = %v, want 0.5", s.DecreaseRatio)
	}
	if s.IncreaseCooldown != 0 {
		t.Errorf("default IncreaseCooldown = %v, want 0", s.IncreaseCooldown)
	}
	if s.DecreaseOn5xx != false {
		t.Errorf("default DecreaseOn5xx = %v, want false", s.DecreaseOn5xx)
	}
	// initial defaults to max when not specified.
	if s.Target != 8 {
		t.Errorf("default Target = %d, want 8 (defaults to max)", s.Target)
	}
}

func TestNew_InitialWithholding(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)
	a, err := New(WithLimiter(l), WithMin(1), WithMax(8), WithInitial(4))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff := l.EffectiveLimit(); eff != 4 {
		t.Fatalf("EffectiveLimit = %d, want 4", eff)
	}
	if w := l.Withheld(); w != 4 {
		t.Fatalf("Withheld = %d, want 4", w)
	}
	if a.Target() != 4 {
		t.Fatalf("Target = %d, want 4", a.Target())
	}
}

func TestAutoscaler_OnSuccessIncreasesTarget(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)
	a, err := New(WithLimiter(l), WithMin(1), WithMax(8), WithInitial(4), WithIncreaseStep(2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff := l.EffectiveLimit(); eff != 4 {
		t.Fatalf("EffectiveLimit before increase = %d, want 4", eff)
	}

	a.OnSuccess()

	if a.Target() != 6 {
		t.Fatalf("Target after success = %d, want 6", a.Target())
	}
	if eff := l.EffectiveLimit(); eff != 6 {
		t.Fatalf("EffectiveLimit after increase = %d, want 6", eff)
	}
}

func TestAutoscaler_OnSuccessRespectsMax(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)
	a, err := New(WithLimiter(l), WithMin(1), WithMax(8), WithInitial(7), WithIncreaseStep(5))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	a.OnSuccess()

	if a.Target() != 8 {
		t.Fatalf("Target = %d, want 8 (capped at max)", a.Target())
	}
	if eff := l.EffectiveLimit(); eff != 8 {
		t.Fatalf("EffectiveLimit = %d, want 8", eff)
	}

	// Another success should not exceed max.
	a.OnSuccess()
	if a.Target() != 8 {
		t.Fatalf("Target after second success = %d, want 8", a.Target())
	}
}

func TestAutoscaler_OnSuccessRespectsCooldown(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)
	a, err := New(WithLimiter(l), WithMin(1), WithMax(8), WithInitial(4), WithIncreaseStep(1), WithIncreaseCooldown(100*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// First success should increase.
	a.OnSuccess()
	if a.Target() != 5 {
		t.Fatalf("Target after first success = %d, want 5", a.Target())
	}

	// Second success immediately after should be blocked by cooldown.
	a.OnSuccess()
	if a.Target() != 5 {
		t.Fatalf("Target after cooldown-blocked success = %d, want 5", a.Target())
	}

	// After cooldown elapses, success should increase again.
	time.Sleep(120 * time.Millisecond)
	a.OnSuccess()
	if a.Target() != 6 {
		t.Fatalf("Target after cooldown = %d, want 6", a.Target())
	}
}

func TestAutoscaler_OnFailure429DecreasesTarget(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)
	a, err := New(WithLimiter(l), WithMin(1), WithMax(8), WithInitial(8), WithDecreaseRatio(0.5))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	a.OnFailure(429, 0)

	// 8 * 0.5 = 4, floor(4) = 4.
	if a.Target() != 4 {
		t.Fatalf("Target after 429 = %d, want 4", a.Target())
	}
	if eff := l.EffectiveLimit(); eff != 4 {
		t.Fatalf("EffectiveLimit after 429 = %d, want 4", eff)
	}
}

func TestAutoscaler_OnFailure5xxOnlyWhenEnabled(t *testing.T) {
	t.Run("5xx ignored by default", func(t *testing.T) {
		l := queue.NewLimiterWithCooldown(8, 0)
		a, err := New(WithLimiter(l), WithMin(1), WithMax(8), WithInitial(8))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		a.OnFailure(500, 0)

		if a.Target() != 8 {
			t.Fatalf("Target after 5xx (default) = %d, want 8", a.Target())
		}
		if eff := l.EffectiveLimit(); eff != 8 {
			t.Fatalf("EffectiveLimit after 5xx (default) = %d, want 8", eff)
		}
	})

	t.Run("5xx decreases when enabled", func(t *testing.T) {
		l := queue.NewLimiterWithCooldown(8, 0)
		a, err := New(WithLimiter(l), WithMin(1), WithMax(8), WithInitial(8), WithDecreaseRatio(0.5), WithDecreaseOn5xx(true))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		a.OnFailure(503, 0)

		if a.Target() != 4 {
			t.Fatalf("Target after 5xx (enabled) = %d, want 4", a.Target())
		}
		if eff := l.EffectiveLimit(); eff != 4 {
			t.Fatalf("EffectiveLimit after 5xx (enabled) = %d, want 4", eff)
		}
	})
}

func TestAutoscaler_OnFailureNeverBelowMin(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)
	a, err := New(WithLimiter(l), WithMin(3), WithMax(8), WithInitial(8), WithDecreaseRatio(0.5))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Repeated 429s: 8 → 4 → 2(min floored) → 3(min floored)
	a.OnFailure(429, 0)
	if a.Target() != 4 {
		t.Fatalf("Target after 1st 429 = %d, want 4", a.Target())
	}
	a.OnFailure(429, 0)
	// 4 * 0.5 = 2, but min is 3, so floored to 3.
	if a.Target() != 3 {
		t.Fatalf("Target after 2nd 429 = %d, want 3 (floored at min)", a.Target())
	}
	a.OnFailure(429, 0)
	if a.Target() != 3 {
		t.Fatalf("Target after 3rd 429 = %d, want 3 (at min)", a.Target())
	}
	if eff := l.EffectiveLimit(); eff != 3 {
		t.Fatalf("EffectiveLimit = %d, want 3 (min)", eff)
	}
}

func TestAutoscaler_RetryAfterIgnoredForMagnitude(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)
	a, err := New(WithLimiter(l), WithMin(1), WithMax(8), WithInitial(8), WithDecreaseRatio(0.5))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// A large Retry-After must not cause a larger decrease than the ratio.
	a.OnFailure(429, 3600*time.Second)

	if a.Target() != 4 {
		t.Fatalf("Target with large Retry-After = %d, want 4 (ratio governs, not Retry-After)", a.Target())
	}
}

func TestAutoscaler_ConcurrentCalls(t *testing.T) {
	l := queue.NewLimiterWithCooldown(16, 0)
	a, err := New(WithLimiter(l), WithMin(1), WithMax(16), WithInitial(8), WithDecreaseRatio(0.5), WithIncreaseStep(1), WithIncreaseCooldown(0))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			a.OnSuccess()
		})
		wg.Go(func() {
			a.OnFailure(429, 0)
		})
		wg.Go(func() {
			a.OnFailure(500, 0)
		})
		wg.Go(func() {
			_ = a.Target()
			_ = a.Stats()
		})
	}
	wg.Wait()

	// Verify the autoscaler is in a consistent state.
	s := a.Stats()
	if s.Target < 1 || s.Target > 16 {
		t.Fatalf("Target = %d, want in [1, 16]", s.Target)
	}
	if s.TotalSuccesses != 100 {
		t.Errorf("TotalSuccesses = %d, want 100", s.TotalSuccesses)
	}
	if s.Total429s != 100 {
		t.Errorf("Total429s = %d, want 100", s.Total429s)
	}
	if s.Total5xxs != 100 {
		t.Errorf("Total5xxs = %d, want 100", s.Total5xxs)
	}
}

func TestAutoscaler_DecreaseThenIncreaseConverges(t *testing.T) {
	l := queue.NewLimiterWithCooldown(16, 0)
	a, err := New(WithLimiter(l), WithMin(1), WithMax(16), WithInitial(16), WithDecreaseRatio(0.5), WithIncreaseStep(1), WithIncreaseCooldown(0))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Decrease to min: 16 → 8 → 4 → 2 → 1.
	a.OnFailure(429, 0)
	if a.Target() != 8 {
		t.Fatalf("after 1st decrease: Target = %d, want 8", a.Target())
	}
	a.OnFailure(429, 0)
	if a.Target() != 4 {
		t.Fatalf("after 2nd decrease: Target = %d, want 4", a.Target())
	}
	a.OnFailure(429, 0)
	if a.Target() != 2 {
		t.Fatalf("after 3rd decrease: Target = %d, want 2", a.Target())
	}
	a.OnFailure(429, 0)
	if a.Target() != 1 {
		t.Fatalf("after 4th decrease: Target = %d, want 1", a.Target())
	}

	// Increase back to max: 1 → 2 → ... → 16.
	for i := 1; i <= 15; i++ {
		a.OnSuccess()
		want := min(1+i, 16)
		if a.Target() != want {
			t.Fatalf("increase step %d: Target = %d, want %d", i, a.Target(), want)
		}
	}
	if a.Target() != 16 {
		t.Fatalf("after full recovery: Target = %d, want 16", a.Target())
	}
	if eff := l.EffectiveLimit(); eff != 16 {
		t.Fatalf("EffectiveLimit after full recovery = %d, want 16", eff)
	}
}

func TestAutoscaler_403TreatedAsBan(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)
	a, err := New(WithLimiter(l), WithMin(1), WithMax(8), WithInitial(8), WithDecreaseRatio(0.5))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	a.OnFailure(403, 0)

	if a.Target() != 4 {
		t.Fatalf("Target after 403 = %d, want 4", a.Target())
	}
	s := a.Stats()
	if s.Total403Bans != 1 {
		t.Errorf("Total403Bans = %d, want 1", s.Total403Bans)
	}
}

func TestAutoscaler_OnFailureOtherStatusIgnored(t *testing.T) {
	l := queue.NewLimiterWithCooldown(8, 0)
	a, err := New(WithLimiter(l), WithMin(1), WithMax(8), WithInitial(4))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, code := range []int{200, 404, 301, 400, 401} {
		a.OnFailure(code, 0)
		if a.Target() != 4 {
			t.Fatalf("Target after status %d = %d, want 4 (ignored)", code, a.Target())
		}
		if eff := l.EffectiveLimit(); eff != 4 {
			t.Fatalf("EffectiveLimit after status %d = %d, want 4 (ignored)", code, eff)
		}
	}

	s := a.Stats()
	if s.Total429s != 0 {
		t.Errorf("Total429s = %d, want 0", s.Total429s)
	}
	if s.Total5xxs != 0 {
		t.Errorf("Total5xxs = %d, want 0", s.Total5xxs)
	}
	if s.Total403Bans != 0 {
		t.Errorf("Total403Bans = %d, want 0", s.Total403Bans)
	}
	if s.TotalDecreases != 0 {
		t.Errorf("TotalDecreases = %d, want 0", s.TotalDecreases)
	}
}
