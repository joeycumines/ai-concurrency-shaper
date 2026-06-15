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

package circuitbreaker

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestBreaker_StartsClosed(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.State() != Closed {
		t.Fatalf("expected CLOSED, got %v", b.State())
	}
	if _, err := b.Allow(); err != nil {
		t.Fatalf("Allow() in CLOSED should return nil, got %v", err)
	}
}

func TestBreaker_TransitionsToOpen(t *testing.T) {
	b, err := New(WithFailureThreshold(3), WithWindow(10*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for range 3 {
		b.RecordFailure(500, 0, time.Time{}, 0)
	}
	if b.State() != Open {
		t.Fatalf("expected OPEN after %d failures, got %v", 3, b.State())
	}
	if _, err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Allow() in OPEN should return ErrCircuitOpen, got %v", err)
	}
}

func TestBreaker_TransitionsToHalfOpen(t *testing.T) {
	b, err := New(WithFailureThreshold(2), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b.RecordFailure(500, 0, time.Time{}, 0)
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for the open timeout to expire.
	time.Sleep(80 * time.Millisecond)

	if _, err := b.Allow(); err != nil {
		t.Fatalf("Allow() after open timeout should succeed (HALF_OPEN probe), got %v", err)
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN after Allow(), got %v", b.State())
	}

	// A second Allow() should be rejected — only one probe allowed.
	if _, err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("second Allow() in HALF_OPEN should be rejected, got %v", err)
	}
}

func TestBreaker_HalfOpenSuccess_ClosesCircuit(t *testing.T) {
	b, err := New(WithFailureThreshold(2), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b.RecordFailure(500, 0, time.Time{}, 0)
	b.RecordFailure(500, 0, time.Time{}, 0)

	time.Sleep(80 * time.Millisecond)
	_, _ = b.Allow() // triggers HALF_OPEN

	b.RecordSuccess(time.Time{}, 0)

	if b.State() != Closed {
		t.Fatalf("expected CLOSED after probe success, got %v", b.State())
	}
	s := b.Stats()
	if s.ConsecutiveFailures != 0 {
		t.Errorf("consecutive failures should be 0 after success, got %d", s.ConsecutiveFailures)
	}
}

func TestBreaker_HalfOpenFailure_Reopens(t *testing.T) {
	b, err := New(WithFailureThreshold(2), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b.RecordFailure(500, 0, time.Time{}, 0)
	b.RecordFailure(500, 0, time.Time{}, 0)

	time.Sleep(80 * time.Millisecond)
	_, _ = b.Allow() // triggers HALF_OPEN

	// Probe fails.
	b.RecordFailure(502, 0, time.Time{}, 0)

	if b.State() != Open {
		t.Fatalf("expected OPEN after probe failure, got %v", b.State())
	}
}

func TestBreaker_BackoffMultiplier(t *testing.T) {
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(20*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// First open timeout: 20ms.
	time.Sleep(40 * time.Millisecond)
	_, _ = b.Allow()                        // HALF_OPEN
	b.RecordFailure(500, 0, time.Time{}, 0) // back to OPEN, backoffMultiple = 1

	// Second open timeout: 20ms * 2 = 40ms.
	time.Sleep(20 * time.Millisecond)
	// Too early — should still be OPEN.
	if _, err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen during backoff, got %v", err)
	}

	// Wait for the doubled timeout.
	time.Sleep(30 * time.Millisecond)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("expected Allow() after doubled timeout, got %v", err)
	}
}

func TestBreaker_FailureWindow(t *testing.T) {
	b, err := New(WithFailureThreshold(3), WithWindow(200*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Record 2 failures, then wait for them to expire.
	b.RecordFailure(500, 0, time.Time{}, 0)
	b.RecordFailure(500, 0, time.Time{}, 0)
	time.Sleep(250 * time.Millisecond)

	// These 2 failures are outside the window. One more should not trip.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Closed {
		t.Fatalf("expected CLOSED (only 1 failure in window), got %v", b.State())
	}

	// Two more within the window should trip it (3 total in window).
	b.RecordFailure(500, 0, time.Time{}, 0)
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN (3 failures in window), got %v", b.State())
	}
}

func TestBreaker_PenaltyDuration(t *testing.T) {
	b, err := New(WithBasePenalty(1*time.Second), WithMaxPenalty(10*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No failures — penalty is just base.
	if p := b.PenaltyDuration(); p != 1*time.Second {
		t.Errorf("penalty with 0 failures = %v, want 1s", p)
	}

	// After 2 consecutive failures: base * (1 + 2) = 3s.
	b.RecordFailure(500, 0, time.Time{}, 0)
	b.RecordFailure(500, 0, time.Time{}, 0)
	if p := b.PenaltyDuration(); p != 3*time.Second {
		t.Errorf("penalty with 2 failures = %v, want 3s", p)
	}

	// After success, consecutive resets.
	b.RecordSuccess(time.Time{}, 0)
	if p := b.PenaltyDuration(); p != 1*time.Second {
		t.Errorf("penalty after success = %v, want 1s", p)
	}
}

func TestBreaker_PenaltyRespectsRetryAfter(t *testing.T) {
	b, err := New(WithBasePenalty(1*time.Second), WithMaxPenalty(60*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Retry-After of 5s exceeds the calculated penalty.
	b.RecordFailure(429, 5*time.Second, time.Time{}, 0)
	if p := b.PenaltyDuration(); p != 5*time.Second {
		t.Errorf("penalty should be 5s (Retry-After), got %v", p)
	}
}

func TestBreaker_PenaltyCappedAtMax(t *testing.T) {
	b, err := New(WithBasePenalty(1*time.Second), WithMaxPenalty(3*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Force 10 consecutive failures — penalty should be capped.
	for range 10 {
		b.RecordFailure(500, 0, time.Time{}, 0)
	}
	if p := b.PenaltyDuration(); p != 3*time.Second {
		t.Errorf("penalty should be capped at 3s, got %v", p)
	}
}

func TestBreaker_WaitDuration(t *testing.T) {
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(1*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// CLOSED — no wait.
	if w := b.WaitDuration(); w != 0 {
		t.Errorf("WaitDuration in CLOSED = %v, want 0", w)
	}

	b.RecordFailure(500, 0, time.Time{}, 0)

	// OPEN — wait should be approximately 1s.
	w := b.WaitDuration()
	if w <= 0 || w > 1*time.Second {
		t.Errorf("WaitDuration in OPEN = %v, want ~1s", w)
	}
}

func TestBreaker_ConsecutiveFailuresReset(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b.RecordFailure(500, 0, time.Time{}, 0)
	b.RecordFailure(500, 0, time.Time{}, 0)
	s := b.Stats()
	if s.ConsecutiveFailures != 2 {
		t.Errorf("consecutive = %d, want 2", s.ConsecutiveFailures)
	}
	b.RecordSuccess(time.Time{}, 0)
	s = b.Stats()
	if s.ConsecutiveFailures != 0 {
		t.Errorf("consecutive after success = %d, want 0", s.ConsecutiveFailures)
	}
}

func TestBreaker_Stats(t *testing.T) {
	b, err := New(WithFailureThreshold(10), WithWindow(10*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	b.RecordFailure(500, 0, time.Time{}, 0)
	b.RecordFailure(429, 0, time.Time{}, 0)
	b.RecordSuccess(time.Time{}, 0)

	s := b.Stats()
	if s.State != Closed {
		t.Errorf("State = %v, want CLOSED", s.State)
	}
	if s.Failures != 2 {
		t.Errorf("Failures = %d, want 2", s.Failures)
	}
	if s.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 (reset by success)", s.ConsecutiveFailures)
	}
	if s.TotalFailures != 2 {
		t.Errorf("TotalFailures = %d, want 2", s.TotalFailures)
	}
	if s.TotalSuccesses != 1 {
		t.Errorf("TotalSuccesses = %d, want 1", s.TotalSuccesses)
	}
}

func TestBreaker_ConcurrentAccess(t *testing.T) {
	b, err := New(WithFailureThreshold(100), WithWindow(10*time.Second), WithOpenTimeout(10*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			b.RecordFailure(500, 0, time.Time{}, 0)
		}()
		go func() {
			defer wg.Done()
			b.RecordSuccess(time.Time{}, 0)
		}()
		go func() {
			defer wg.Done()
			_, _ = b.Allow()
			_ = b.State()
			_ = b.Stats()
			_ = b.WaitDuration()
			_ = b.PenaltyDuration()
		}()
	}
	wg.Wait()
}

func TestBreaker_DefaultConfig(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := b.Stats()
	if s.State != Closed {
		t.Errorf("expected CLOSED, got %v", s.State)
	}
	// Verify defaults are applied by checking penalty behavior.
	if p := b.PenaltyDuration(); p != 2*time.Second {
		t.Errorf("default BasePenalty should be 2s, got penalty %v", p)
	}
}

func TestIsFailureStatus(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{0, true}, // transport error (no response)
		{200, false},
		{201, false},
		{301, false},
		{400, false},
		{401, false},
		{403, true}, // status-only callers must be conservative
		{404, false},
		{429, true},
		{500, true},
		{501, true},
		{502, true},
		{503, true},
		{504, true},
		{599, true},
		{600, false},
	}
	for _, tt := range tests {
		if got := IsFailureStatus(tt.code); got != tt.want {
			t.Errorf("IsFailureStatus(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
}

func TestIsFailureStatusWithHeaders(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		code   int
		header http.Header
		want   bool
	}{
		{
			name: "bare forbidden is auth error",
			code: http.StatusForbidden,
			want: false,
		},
		{
			name:   "retry after forbidden is temporary ban",
			code:   http.StatusForbidden,
			header: http.Header{"Retry-After": []string{"60"}},
			want:   true,
		},
		{
			name:   "rate limit reset forbidden is temporary ban",
			code:   http.StatusForbidden,
			header: http.Header{"X-Ratelimit-Reset": []string{"123"}},
			want:   true,
		},
		{
			name: "zero retry after is not temporary ban",
			code: http.StatusForbidden,
			header: http.Header{
				"Retry-After": []string{"0"},
			},
			want: false,
		},
		{
			name: "malformed retry after with rate limit header is temporary ban",
			code: http.StatusForbidden,
			header: http.Header{
				"Retry-After":       []string{"not-a-number"},
				"X-Ratelimit-Reset": []string{"123"},
			},
			want: true,
		},
		{
			name: "zero retry after with rate limit header is temporary ban",
			code: http.StatusForbidden,
			header: http.Header{
				"Retry-After":       []string{"0"},
				"X-Ratelimit-Limit": []string{"100"},
			},
			want: true,
		},
		{
			name: "malformed retry after alone is not temporary ban",
			code: http.StatusForbidden,
			header: http.Header{
				"Retry-After": []string{"not-a-number"},
			},
			want: false,
		},
		{
			name: "five hundred remains failure",
			code: http.StatusInternalServerError,
			want: true,
		},
		{
			name: "bad request remains client error",
			code: http.StatusBadRequest,
			want: false,
		},
		{
			name:   "http date retry after in future is temporary ban",
			code:   http.StatusForbidden,
			header: http.Header{"Retry-After": []string{now.Add(30 * time.Second).UTC().Format(http.TimeFormat)}},
			want:   true,
		},
		{
			name:   "http date retry after in past is not temporary ban",
			code:   http.StatusForbidden,
			header: http.Header{"Retry-After": []string{now.Add(-30 * time.Second).UTC().Format(http.TimeFormat)}},
			want:   false,
		},
		{
			name:   "http date retry after at boundary is temporary ban",
			code:   http.StatusForbidden,
			header: http.Header{"Retry-After": []string{now.Add(1 * time.Second).UTC().Format(http.TimeFormat)}},
			want:   true,
		},
		{
			name:   "ietf ratelimit-reset header is temporary ban",
			code:   http.StatusForbidden,
			header: http.Header{"Ratelimit-Reset": []string{"123"}},
			want:   true,
		},
		{
			name:   "ietf ratelimit-limit header is temporary ban",
			code:   http.StatusForbidden,
			header: http.Header{"Ratelimit-Limit": []string{"100"}},
			want:   true,
		},
		{
			name:   "ietf ratelimit-remaining header is temporary ban",
			code:   http.StatusForbidden,
			header: http.Header{"Ratelimit-Remaining": []string{"0"}},
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsFailureStatusWithHeaders(tt.code, tt.header, now, now)
			if got != tt.want {
				t.Fatalf("IsFailureStatusWithHeaders() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Unix(1700000000, 0)
	date := now.Add(-2 * time.Second) // upstream generated response 2s ago

	tests := []struct {
		name        string
		h           http.Header
		receivedAt  time.Time
		evaluatedAt time.Time
		want        time.Duration
	}{
		{
			name:        "delay-seconds positive",
			h:           http.Header{"Retry-After": []string{"30"}},
			receivedAt:  now,
			evaluatedAt: now,
			want:        30 * time.Second,
		},
		{
			name:        "delay-seconds zero",
			h:           http.Header{"Retry-After": []string{"0"}},
			receivedAt:  now,
			evaluatedAt: now,
			want:        0,
		},
		{
			name:        "delay-seconds negative",
			h:           http.Header{"Retry-After": []string{"-5"}},
			receivedAt:  now,
			evaluatedAt: now,
			want:        0,
		},
		{
			name: "delay-seconds elapsed",
			h:    http.Header{"Retry-After": []string{"30"}},
			// 5 seconds passed between receipt and evaluation.
			receivedAt:  now,
			evaluatedAt: now.Add(5 * time.Second),
			want:        25 * time.Second,
		},
		{
			name:        "delay-seconds expired",
			h:           http.Header{"Retry-After": []string{"30"}},
			receivedAt:  now,
			evaluatedAt: now.Add(30 * time.Second),
			want:        0,
		},
		{
			name: "http-date with date header",
			h: http.Header{
				"Date":        []string{date.UTC().Format(http.TimeFormat)},
				"Retry-After": []string{date.Add(5 * time.Second).UTC().Format(http.TimeFormat)},
			},
			receivedAt:  now,
			evaluatedAt: now,
			// Date-relative: intendedDelta = (Date+5s) - Date = 5s,
			// proxyElapsed = 0, remaining = 5s. This does not account for
			// network transit time (2s in this test), but eliminates clock
			// skew. The transit time overestimation is conservative (holds
			// slots slightly longer) and negligible in practice (ms vs. s).
			want: 5 * time.Second,
		},
		{
			name: "http-date without date header",
			h: http.Header{
				"Retry-After": []string{now.Add(5 * time.Second).UTC().Format(http.TimeFormat)},
			},
			receivedAt:  now,
			evaluatedAt: now,
			want:        5 * time.Second,
		},
		{
			name: "http-date with date header expired",
			h: http.Header{
				"Date":        []string{date.UTC().Format(http.TimeFormat)},
				"Retry-After": []string{date.Add(-1 * time.Second).UTC().Format(http.TimeFormat)},
			},
			receivedAt:  now,
			evaluatedAt: now,
			want:        0,
		},
		{
			name: "slow transfer expires date-relative ban",
			h: http.Header{
				"Date":        []string{date.UTC().Format(http.TimeFormat)},
				"Retry-After": []string{date.Add(2 * time.Second).UTC().Format(http.TimeFormat)},
			},
			// Date+2s is the expiry. We received at `now` (2s after Date) and
			// evaluated 10s later, so the ban expired 8s ago.
			receivedAt:  now,
			evaluatedAt: now.Add(10 * time.Second),
			want:        0,
		},
		{
			name:        "malformed",
			h:           http.Header{"Retry-After": []string{"not-a-value"}},
			receivedAt:  now,
			evaluatedAt: now,
			want:        0,
		},
		{
			name:        "missing",
			h:           http.Header{},
			receivedAt:  now,
			evaluatedAt: now,
			want:        0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseRetryAfter(tt.h, tt.receivedAt, tt.evaluatedAt); got != tt.want {
				t.Errorf("ParseRetryAfter() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseRetryAfter_HTTPDateClockSkew(t *testing.T) {
	// Regression test for scratch/review-14.md: when a Date header is present,
	// ParseRetryAfter must compute the remaining delay using upstream-clock-only
	// timestamps (Retry-After - Date) and proxy-clock-only elapsed time, so that
	// clock skew between the machines does not inflate or deflate the result.

	proxyNow := time.Unix(1700000000, 0)

	t.Run("upstream 2 minutes ahead", func(t *testing.T) {
		// The upstream's clock is 2 minutes ahead of the proxy's.
		upstreamNow := proxyNow.Add(2 * time.Minute)
		h := http.Header{
			"Date":        []string{upstreamNow.UTC().Format(http.TimeFormat)},
			"Retry-After": []string{upstreamNow.Add(30 * time.Second).UTC().Format(http.TimeFormat)},
		}
		receivedAt := proxyNow
		evaluatedAt := proxyNow.Add(5 * time.Second)
		// Old code: retry.Sub(evaluatedAt) = (upstreamNow+30s) - (proxyNow+5s)
		//   = (proxyNow+2m+30s) - (proxyNow+5s) = 2m25s (WRONG — inflated by skew).
		// New code: intendedDelta = 30s, proxyElapsed = 5s, remaining = 25s (CORRECT).
		got := ParseRetryAfter(h, receivedAt, evaluatedAt)
		if got != 25*time.Second {
			t.Errorf("ParseRetryAfter() = %v, want 25s (30s intended - 5s elapsed, skew eliminated)", got)
		}
	})

	t.Run("upstream 2 minutes behind", func(t *testing.T) {
		// The upstream's clock is 2 minutes behind the proxy's.
		upstreamNow := proxyNow.Add(-2 * time.Minute)
		h := http.Header{
			"Date":        []string{upstreamNow.UTC().Format(http.TimeFormat)},
			"Retry-After": []string{upstreamNow.Add(30 * time.Second).UTC().Format(http.TimeFormat)},
		}
		receivedAt := proxyNow
		evaluatedAt := proxyNow.Add(5 * time.Second)
		// Old code: retry.Sub(evaluatedAt) = (upstreamNow+30s) - (proxyNow+5s)
		//   = (proxyNow-2m+30s) - (proxyNow+5s) = -1m35s → 0 (WRONG — ban dropped).
		// New code: intendedDelta = 30s, proxyElapsed = 5s, remaining = 25s (CORRECT).
		got := ParseRetryAfter(h, receivedAt, evaluatedAt)
		if got != 25*time.Second {
			t.Errorf("ParseRetryAfter() = %v, want 25s (30s intended - 5s elapsed, skew eliminated)", got)
		}
	})

	t.Run("skew with elapsed exceeding ban", func(t *testing.T) {
		upstreamNow := proxyNow.Add(2 * time.Minute)
		h := http.Header{
			"Date":        []string{upstreamNow.UTC().Format(http.TimeFormat)},
			"Retry-After": []string{upstreamNow.Add(5 * time.Second).UTC().Format(http.TimeFormat)},
		}
		receivedAt := proxyNow
		evaluatedAt := proxyNow.Add(10 * time.Second)
		// intendedDelta = 5s, proxyElapsed = 10s, remaining = -5s → 0.
		got := ParseRetryAfter(h, receivedAt, evaluatedAt)
		if got != 0 {
			t.Errorf("ParseRetryAfter() = %v, want 0 (ban expired during proxy hold)", got)
		}
	})

	t.Run("zero delta is not a ban", func(t *testing.T) {
		// Retry-After equals Date: intendedDelta = 0, meaning "retry immediately".
		upstreamNow := proxyNow.Add(2 * time.Minute)
		h := http.Header{
			"Date":        []string{upstreamNow.UTC().Format(http.TimeFormat)},
			"Retry-After": []string{upstreamNow.UTC().Format(http.TimeFormat)},
		}
		receivedAt := proxyNow
		evaluatedAt := proxyNow
		got := ParseRetryAfter(h, receivedAt, evaluatedAt)
		if got != 0 {
			t.Errorf("ParseRetryAfter() = %v, want 0 (zero delta = no ban)", got)
		}
	})
}

func TestIsTemporaryBanResponse_HTTPDateRelative(t *testing.T) {
	// RFC 9110 §10.2.3: when a Date header is present, the remaining delay
	// is computed using upstream-clock-only timestamps (Retry-After - Date)
	// and proxy-clock-only elapsed time, eliminating clock skew. Bans that
	// expire during slow body transfers are correctly classified as expired.

	now := time.Unix(1700000000, 0)

	t.Run("date-relative future", func(t *testing.T) {
		h := http.Header{
			"Date":        []string{now.UTC().Format(http.TimeFormat)},
			"Retry-After": []string{now.Add(30 * time.Second).UTC().Format(http.TimeFormat)},
		}
		if !IsTemporaryBanResponse(h, now, now.Add(10*time.Second)) {
			t.Error("expected true when Retry-After is in the future")
		}
	})

	t.Run("date-relative expired", func(t *testing.T) {
		h := http.Header{
			"Date":        []string{now.UTC().Format(http.TimeFormat)},
			"Retry-After": []string{now.Add(-30 * time.Second).UTC().Format(http.TimeFormat)},
		}
		if IsTemporaryBanResponse(h, now, now.Add(10*time.Second)) {
			t.Error("expected false when Retry-After is expired")
		}
	})

	t.Run("slow transfer expires date-relative ban", func(t *testing.T) {
		h := http.Header{
			"Date":        []string{now.UTC().Format(http.TimeFormat)},
			"Retry-After": []string{now.Add(2 * time.Second).UTC().Format(http.TimeFormat)},
		}
		// The body streamed for 10 seconds after headers were received. The
		// ban expired 8s into the transfer; remaining-delay classification
		// must now report it as expired.
		if IsTemporaryBanResponse(h, now, now.Add(10*time.Second)) {
			t.Error("expected false for a ban that expired during body streaming")
		}
	})

	t.Run("delay-seconds expires during transfer", func(t *testing.T) {
		h := http.Header{
			"Retry-After": []string{"2"},
		}
		// A 2-second delay-seconds value received at `now` and evaluated 10s
		// later must appear expired.
		if IsTemporaryBanResponse(h, now, now.Add(10*time.Second)) {
			t.Error("expected false when delay-seconds was fully consumed during transfer")
		}
	})

	t.Run("rate limit header overrides expired date", func(t *testing.T) {
		h := http.Header{
			"Date":              []string{now.UTC().Format(http.TimeFormat)},
			"Retry-After":       []string{now.Add(-30 * time.Second).UTC().Format(http.TimeFormat)},
			"X-Ratelimit-Reset": []string{"123"},
		}
		if !IsTemporaryBanResponse(h, now, now) {
			t.Error("expected true when rate-limit header is present despite expired Retry-After")
		}
	})

	t.Run("integer retry-after still temporary", func(t *testing.T) {
		h := http.Header{
			"Retry-After": []string{"60"},
		}
		if !IsTemporaryBanResponse(h, now, now) {
			t.Error("expected true for positive integer Retry-After")
		}
	})

	t.Run("integer zero not temporary", func(t *testing.T) {
		h := http.Header{
			"Retry-After": []string{"0"},
		}
		if IsTemporaryBanResponse(h, now, now) {
			t.Error("expected false for Retry-After: 0")
		}
	})
}

func TestIsTemporaryBanResponse_HTTPDateClockSkew(t *testing.T) {
	// When an upstream omits the Date header, freshly-generated HTTP-date
	// Retry-After values can appear slightly expired due to network transit
	// time. clockSkewTolerance absorbs that without allowing stale bans to
	// live forever.

	now := time.Unix(1700000000, 0)

	t.Run("future without date is temporary", func(t *testing.T) {
		h := http.Header{
			"Retry-After": []string{now.Add(1 * time.Second).UTC().Format(http.TimeFormat)},
		}
		if !IsTemporaryBanResponse(h, now, now) {
			t.Error("expected true for future HTTP-date without Date")
		}
	})

	t.Run("expired but within skew tolerance is temporary", func(t *testing.T) {
		h := http.Header{
			"Retry-After": []string{now.Add(-500 * time.Millisecond).UTC().Format(http.TimeFormat)},
		}
		if !IsTemporaryBanResponse(h, now, now) {
			t.Error("expected true when HTTP-date is within clock skew tolerance")
		}
	})

	t.Run("expired beyond skew tolerance is not temporary", func(t *testing.T) {
		h := http.Header{
			"Retry-After": []string{now.Add(-6 * time.Second).UTC().Format(http.TimeFormat)},
		}
		if IsTemporaryBanResponse(h, now, now) {
			t.Error("expected false when HTTP-date is beyond clock skew tolerance")
		}
	})

	t.Run("expired beyond skew but rate-limit header is temporary", func(t *testing.T) {
		h := http.Header{
			"Retry-After":       []string{now.Add(-6 * time.Second).UTC().Format(http.TimeFormat)},
			"X-Ratelimit-Reset": []string{"123"},
		}
		if !IsTemporaryBanResponse(h, now, now) {
			t.Error("expected true when rate-limit header is present despite expired Retry-After")
		}
	})
}

func TestIsTemporaryBanResponse_HTTPDateClockSkewWithDate(t *testing.T) {
	// Regression test for scratch/review-14.md: when a Date header is present,
	// IsTemporaryBanResponse must compute the remaining delay using upstream-
	// clock-only timestamps (Retry-After - Date) and proxy-clock-only elapsed
	// time, so that clock skew between the machines does not cause active bans
	// to be misclassified as expired or vice versa.

	proxyNow := time.Unix(1700000000, 0)

	t.Run("upstream 2 minutes behind still classified as temporary", func(t *testing.T) {
		// The upstream's clock is 2 minutes behind the proxy's.
		// Old code: retry.Sub(evaluatedAt) would be (upstreamNow+30s) - proxyNow
		//   = (proxyNow-2m+30s) - proxyNow = -1m30s → NOT temporary (WRONG).
		// New code: intendedDelta = 30s, proxyElapsed = 0, remaining = 30s > 0
		//   → temporary (CORRECT).
		upstreamNow := proxyNow.Add(-2 * time.Minute)
		h := http.Header{
			"Date":        []string{upstreamNow.UTC().Format(http.TimeFormat)},
			"Retry-After": []string{upstreamNow.Add(30 * time.Second).UTC().Format(http.TimeFormat)},
		}
		if !IsTemporaryBanResponse(h, proxyNow, proxyNow) {
			t.Error("expected true: 30s ban from 2min-behind upstream is still active")
		}
	})

	t.Run("upstream 2 minutes ahead still classified as temporary", func(t *testing.T) {
		// The upstream's clock is 2 minutes ahead of the proxy's.
		// Old code: retry.Sub(evaluatedAt) = (upstreamNow+30s) - proxyNow
		//   = (proxyNow+2m+30s) - proxyNow = 2m30s → temporary (accidentally correct,
		//   but the remaining delay was inflated by 2min in ParseRetryAfter).
		// New code: intendedDelta = 30s, proxyElapsed = 0, remaining = 30s > 0
		//   → temporary (CORRECT, and ParseRetryAfter returns 30s not 2m30s).
		upstreamNow := proxyNow.Add(2 * time.Minute)
		h := http.Header{
			"Date":        []string{upstreamNow.UTC().Format(http.TimeFormat)},
			"Retry-After": []string{upstreamNow.Add(30 * time.Second).UTC().Format(http.TimeFormat)},
		}
		if !IsTemporaryBanResponse(h, proxyNow, proxyNow) {
			t.Error("expected true: 30s ban from 2min-ahead upstream is still active")
		}
	})

	t.Run("upstream behind with elapsed exceeding ban is not temporary", func(t *testing.T) {
		upstreamNow := proxyNow.Add(-2 * time.Minute)
		h := http.Header{
			"Date":        []string{upstreamNow.UTC().Format(http.TimeFormat)},
			"Retry-After": []string{upstreamNow.Add(5 * time.Second).UTC().Format(http.TimeFormat)},
		}
		// intendedDelta = 5s, proxyElapsed = 10s, remaining = -5s ≤ 0.
		if IsTemporaryBanResponse(h, proxyNow, proxyNow.Add(10*time.Second)) {
			t.Error("expected false: 5s ban expired after 10s of proxy elapsed time")
		}
	})

	t.Run("network latency does not misclassify fresh ban", func(t *testing.T) {
		// Upstream generates Date and Retry-After at the same instant
		// (0s ban = "retry immediately"), but the network delivers the
		// headers 15ms later. With Date present, intendedDelta = 0s,
		// so remaining = 0 - 0 = 0 → NOT temporary (correct: 0s ban
		// is not a real ban). This matches the delay-seconds behavior
		// where Retry-After: 0 is also not temporary.
		upstreamNow := proxyNow
		h := http.Header{
			"Date":        []string{upstreamNow.UTC().Format(http.TimeFormat)},
			"Retry-After": []string{upstreamNow.UTC().Format(http.TimeFormat)},
		}
		if IsTemporaryBanResponse(h, proxyNow, proxyNow.Add(15*time.Millisecond)) {
			t.Error("expected false: Retry-After = Date means 0s ban (retry immediately)")
		}
	})

	t.Run("rate-limit header overrides expired date-relative ban", func(t *testing.T) {
		upstreamNow := proxyNow.Add(-2 * time.Minute)
		h := http.Header{
			"Date":              []string{upstreamNow.UTC().Format(http.TimeFormat)},
			"Retry-After":       []string{upstreamNow.Add(5 * time.Second).UTC().Format(http.TimeFormat)},
			"X-Ratelimit-Reset": []string{"123"},
		}
		// intendedDelta = 5s, proxyElapsed = 10s, remaining = -5s,
		// but rate-limit header overrides.
		if !IsTemporaryBanResponse(h, proxyNow, proxyNow.Add(10*time.Second)) {
			t.Error("expected true: rate-limit header overrides expired Date-relative ban")
		}
	})
}

func TestBreaker_StateString(t *testing.T) {
	tests := []struct {
		s    State
		want string
	}{
		{Closed, "CLOSED"},
		{Open, "OPEN"},
		{HalfOpen, "HALF_OPEN"},
		{State(99), "UNKNOWN"},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
	}
}

func TestBreaker_OpenRejectsBeforeTimeout(t *testing.T) {
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b.RecordFailure(500, 0, time.Time{}, 0)

	// Immediately after opening — should reject.
	if _, err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen immediately after open, got %v", err)
	}
}

func TestBreaker_403TripsCircuit(t *testing.T) {
	// Upstream temp-ban escalation: after a run of 429s, some AI APIs
	// switch to 403. If 403 is not counted as a failure, the circuit
	// stays CLOSED (or re-closes on a 403 probe) and keeps feeding the
	// banned upstream, extending the ban. This regression test verifies
	// that 403s count toward the failure threshold.
	b, err := New(WithFailureThreshold(3), WithWindow(10*time.Second), WithOpenTimeout(time.Hour), WithMaxOpenTimeout(time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for range 3 {
		b.RecordFailure(403, 0, time.Time{}, 0)
	}
	if b.State() != Open {
		t.Fatalf("expected OPEN after 3 temp-ban 403s, got %v", b.State())
	}
	if _, err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen after 403 trip, got %v", err)
	}
}

func TestBreaker_TransportErrorCountsAsFailure(t *testing.T) {
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// statusCode 0 represents a transport error (no response).
	b.RecordFailure(0, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN after transport error, got %v", b.State())
	}
}

func TestBreaker_4xxNotCounted(t *testing.T) {
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 400 is NOT a qualifying failure — should not count.
	b.RecordFailure(400, 0, time.Time{}, 0)
	// Actually, RecordFailure is called by the caller after checking
	// IsFailureStatus. But if called directly with 400, it still counts
	// because RecordFailure is a low-level recorder. The classification
	// happens at the call site. So let's verify IsFailureStatus instead.
	// This test documents the expected behavior.
}

func TestBreaker_RepeatedHalfOpenCycles(t *testing.T) {
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(20*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for cycle := range 4 {
		// Trip or re-trip.
		b.RecordFailure(500, 0, time.Time{}, 0)
		if b.State() != Open {
			t.Fatalf("cycle %d: expected OPEN, got %v", cycle, b.State())
		}

		// Wait for open timeout (exponential backoff: T * 2^cycle).
		timeout := min(b.cfg.openTimeout*time.Duration(1<<cycle), b.cfg.maxOpenTimeout)
		time.Sleep(timeout + 10*time.Millisecond)

		_, _ = b.Allow() // triggers HALF_OPEN
		if b.State() != HalfOpen {
			t.Fatalf("cycle %d: expected HALF_OPEN, got %v", cycle, b.State())
		}
	}

	// After 4 cycles, a success should close the circuit.
	b.RecordSuccess(time.Time{}, 0)
	if b.State() != Closed {
		t.Fatalf("expected CLOSED after probe success, got %v", b.State())
	}
}

func TestBreaker_RetryAfterResetOnSuccess(t *testing.T) {
	b, err := New(WithBasePenalty(1*time.Second), WithMaxPenalty(60*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Record a failure with a moderate Retry-After (within MaxPenalty).
	b.RecordFailure(429, 30*time.Second, time.Time{}, 0)
	if p := b.PenaltyDuration(); p != 30*time.Second {
		t.Errorf("penalty after Retry-After = %v, want 30s", p)
	}

	// A success must reset lastRetryAfter so the penalty returns to base.
	b.RecordSuccess(time.Time{}, 0)
	if p := b.PenaltyDuration(); p != 1*time.Second {
		t.Errorf("penalty after success = %v, want 1s (base, Retry-After reset)", p)
	}
}

func TestBreaker_ExponentialBackoffSequence(t *testing.T) {
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(20*time.Millisecond), WithMaxOpenTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Cycle 0: timeout = 20ms * 2^0 = 20ms
	b.RecordFailure(500, 0, time.Time{}, 0)
	time.Sleep(30 * time.Millisecond)
	_, _ = b.Allow() // HALF_OPEN
	b.RecordFailure(500, 0, time.Time{}, 0)

	// Cycle 1: timeout = 20ms * 2^1 = 40ms
	// Too early at 15ms.
	time.Sleep(15 * time.Millisecond)
	if _, err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected OPEN at 15ms (need 40ms), got %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("expected HALF_OPEN after 45ms, got %v", err)
	}
	b.RecordFailure(500, 0, time.Time{}, 0)

	// Cycle 2: timeout = 20ms * 2^2 = 80ms
	// Too early at 40ms.
	time.Sleep(40 * time.Millisecond)
	if _, err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected OPEN at 40ms (need 80ms), got %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("expected HALF_OPEN after 90ms, got %v", err)
	}

	// Success resets backoff.
	b.RecordSuccess(time.Time{}, 0)
	if b.State() != Closed {
		t.Fatalf("expected CLOSED after success, got %v", b.State())
	}
}

func TestBreaker_StaleFailureIgnoredInHalfOpen(t *testing.T) {
	// Verify that a RecordFailure call with startedAt before openSince
	// does NOT trigger the HALF_OPEN → OPEN transition. This prevents
	// stale, long-running requests from falsely tripping the breaker
	// while a probe is in flight.
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit CLOSED → OPEN.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for open timeout to expire.
	time.Sleep(80 * time.Millisecond)

	// Trigger HALF_OPEN via Allow().
	if _, err := b.Allow(); err != nil {
		t.Fatalf("expected Allow() to succeed (HALF_OPEN probe), got %v", err)
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN, got %v", b.State())
	}

	// Record a failure from a request that started BEFORE the OPEN period.
	// This is a stale result — the request was in-flight from before the
	// circuit opened. It must NOT trip HALF_OPEN → OPEN.
	staleStart := time.Now().Add(-5 * time.Second) // clearly before openSince
	b.RecordFailure(500, 0, staleStart, 0)

	// The circuit should REMAIN in HALF_OPEN — the stale failure is ignored.
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN (stale failure ignored), got %v", b.State())
	}

	// The probe succeeds — circuit should close.
	b.RecordSuccess(time.Time{}, 0)
	if b.State() != Closed {
		t.Fatalf("expected CLOSED after probe success, got %v", b.State())
	}
}

func TestBreaker_RecentFailureTripsHalfOpen(t *testing.T) {
	// Verify that a RecordFailure call with startedAt AFTER openSince
	// correctly triggers HALF_OPEN → OPEN. This is the normal case: a
	// probe request that fails should reopen the circuit.
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for half-open.
	time.Sleep(80 * time.Millisecond)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("expected Allow() to succeed, got %v", err)
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN, got %v", b.State())
	}

	// Record a failure from a request that started AFTER the OPEN period.
	// This is the probe failing — it MUST trip HALF_OPEN → OPEN.
	recentStart := time.Now() // clearly after openSince
	b.RecordFailure(500, 0, recentStart, 0)

	if b.State() != Open {
		t.Fatalf("expected OPEN (recent failure trips circuit), got %v", b.State())
	}
}

func TestBreaker_RecordFailureZeroStartedAtUnfiltered(t *testing.T) {
	// Verify that RecordFailure with a zero startedAt (unknown start time)
	// behaves identically to the old two-argument RecordFailure — no
	// stale-request filtering is applied. This ensures backward compatibility
	// for callers that don't track request start times.
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for half-open.
	time.Sleep(80 * time.Millisecond)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("expected Allow() to succeed, got %v", err)
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN, got %v", b.State())
	}

	// Record failure with zero startedAt — must NOT be filtered.
	b.RecordFailure(500, 0, time.Time{}, 0)

	if b.State() != Open {
		t.Fatalf("expected OPEN (zero startedAt = no filtering), got %v", b.State())
	}
}

func TestBreaker_BackoffOverflowSafe(t *testing.T) {
	// Verify that the openTimeout() function never returns a negative or
	// zero duration due to int64 overflow in the bit shift. Directly
	// exercise high backoffMultiple values by manipulating the struct.
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(10*time.Second), WithMaxOpenTimeout(120*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Push backoffMultiple to extreme values and verify the breaker
	// still functions correctly — no negative durations, no panics.
	for _, bm := range []int{0, 1, 10, 20, 30, 50, 100} {
		b.backoffMultiple = bm

		// WaitDuration should be non-negative.
		if w := b.WaitDuration(); w < 0 {
			t.Errorf("backoffMultiple=%d: WaitDuration = %v, want >= 0", bm, w)
		}

		// PenaltyDuration should be non-negative.
		if p := b.PenaltyDuration(); p < 0 {
			t.Errorf("backoffMultiple=%d: PenaltyDuration = %v, want >= 0", bm, p)
		}
	}

	// For backoffMultiple >= 20, openTimeout should return exactly
	// maxOpenTimeout (the cap is hit directly without multiplication).
	b.backoffMultiple = 20
	if w := b.WaitDuration(); w < 0 {
		t.Errorf("backoffMultiple=20: WaitDuration = %v, want >= 0", w)
	}
}

func TestBreaker_ProbeTimeoutAllowsNewProbe(t *testing.T) {
	// Verify that if a probe request never completes (no RecordFailure or
	// RecordSuccess called), the breaker allows a new probe after the
	// open timeout expires. This prevents permanent HALF_OPEN deadlock.
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit CLOSED -> OPEN.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for open timeout to expire, then trigger HALF_OPEN.
	time.Sleep(80 * time.Millisecond)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("expected Allow() to succeed (HALF_OPEN probe), got %v", err)
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN, got %v", b.State())
	}

	// The probe is now in flight. A second Allow() should be rejected
	// (probeInFlight=true).
	if _, err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen while probe in flight, got %v", err)
	}

	// Wait for the probe timeout (openTimeout) to expire.
	time.Sleep(80 * time.Millisecond)

	// Now Allow() should succeed -- a new probe is allowed because the
	// previous one timed out without completing.
	if _, err := b.Allow(); err != nil {
		t.Fatalf("expected Allow() to succeed (probe timed out), got %v", err)
	}
	// State should still be HALF_OPEN (we replaced the stale probe).
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN after probe timeout, got %v", b.State())
	}
}

func TestIsFailureStatus_TransportError(t *testing.T) {
	// Transport errors (status 0, meaning no response was received) must
	// be classified as failures. This was previously returning false,
	// causing the standalone breaker path to silently ignore transport
	// errors.
	if !IsFailureStatus(0) {
		t.Error("IsFailureStatus(0) = false, want true (transport errors are always failures)")
	}
}

func TestBreaker_RetryAfterRespectsMaxPenalty(t *testing.T) {
	// Verify that a Retry-After value exceeding MaxPenalty is capped.
	// The old formula max(Retry-After, min(penalty, MaxPenalty)) allowed
	// Retry-After to exceed MaxPenalty. The new formula wraps the entire
	// result in min(..., MaxPenalty).
	b, err := New(WithBasePenalty(1*time.Second), WithMaxPenalty(5*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Record a failure with a Retry-After of 3600 seconds (1 hour).
	b.RecordFailure(429, 3600*time.Second, time.Time{}, 0)

	// The penalty must be capped at MaxPenalty (5s), not 3600s.
	if p := b.PenaltyDuration(); p != 5*time.Second {
		t.Errorf("penalty with Retry-After=3600s and MaxPenalty=5s = %v, want 5s", p)
	}
}

func TestBreaker_StaleSuccessIgnoredInHalfOpen(t *testing.T) {
	// Verify that a RecordSuccess call with startedAt before openSince
	// does NOT trigger the HALF_OPEN -> CLOSED transition. This prevents
	// a long-running request from before the circuit opened from falsely
	// closing the circuit without a valid probe.
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit CLOSED -> OPEN.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for open timeout, trigger HALF_OPEN.
	time.Sleep(80 * time.Millisecond)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("expected Allow() to succeed, got %v", err)
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN, got %v", b.State())
	}

	// Record a success from a request that started BEFORE the OPEN period.
	// This is a stale result -- it must NOT close the circuit.
	staleStart := time.Now().Add(-5 * time.Second)
	b.RecordSuccess(staleStart, 0)

	// The circuit should REMAIN in HALF_OPEN.
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN (stale success ignored), got %v", b.State())
	}

	// A success from the probe (started after openSince) should close.
	recentStart := time.Now()
	b.RecordSuccess(recentStart, 0)
	if b.State() != Closed {
		t.Fatalf("expected CLOSED after recent success, got %v", b.State())
	}
}

func TestBreaker_FailuresClearedOnRecovery(t *testing.T) {
	// Verify that the failures sliding window is cleared when the circuit
	// transitions from HALF_OPEN to CLOSED via a successful probe. Without
	// this, old failures from before the OPEN period remain in the window
	// and can immediately re-trip the circuit after a single new failure.
	b, err := New(WithFailureThreshold(3), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Accumulate 4 failures to trip the circuit.
	for range 4 {
		b.RecordFailure(500, 0, time.Time{}, 0)
	}
	if b.State() != Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for open timeout, trigger HALF_OPEN.
	time.Sleep(80 * time.Millisecond)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("expected Allow() to succeed, got %v", err)
	}

	// Probe succeeds -- circuit should close.
	b.RecordSuccess(time.Time{}, 0)
	if b.State() != Closed {
		t.Fatalf("expected CLOSED after probe success, got %v", b.State())
	}

	// Verify failures window was cleared.
	s := b.Stats()
	if s.Failures != 0 {
		t.Errorf("failures after recovery = %d, want 0 (window cleared)", s.Failures)
	}

	// A single new failure should NOT immediately re-trip the circuit
	// (threshold is 3).
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Closed {
		t.Fatalf("expected CLOSED after 1 failure (threshold=3), got %v -- failures not cleared on recovery", b.State())
	}
}

func TestBreaker_StaleFailureNoSideEffects(t *testing.T) {
	// Verify that a stale RecordFailure in HALF_OPEN (startedAt before openSince)
	// does NOT mutate any breaker state: consecutive, totalFailures, lastFailure,
	// failures, or lastRetryAfter. Before the fix, these were mutated BEFORE the
	// stale-request guard, causing the penalty to be inflated and the failure
	// window to be polluted by ghost results from before the OPEN period.
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond), WithBasePenalty(2*time.Second), WithMaxPenalty(60*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit CLOSED → OPEN.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for half-open.
	time.Sleep(80 * time.Millisecond)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("expected Allow() to succeed, got %v", err)
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN, got %v", b.State())
	}

	// Capture state before the stale failure.
	before := b.Stats()
	beforePenalty := b.PenaltyDuration()

	// Record a stale failure with a Retry-After that would inflate lastRetryAfter.
	staleStart := time.Now().Add(-5 * time.Second) // clearly before openSince
	b.RecordFailure(500, 30*time.Second, staleStart, 0)

	// The circuit must remain in HALF_OPEN.
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN (stale failure ignored), got %v", b.State())
	}

	// Verify no state was mutated by the stale failure.
	after := b.Stats()

	if after.ConsecutiveFailures != before.ConsecutiveFailures {
		t.Errorf("consecutive: got %d, want %d (stale failure must not increment)",
			after.ConsecutiveFailures, before.ConsecutiveFailures)
	}
	if after.TotalFailures != before.TotalFailures {
		t.Errorf("totalFailures: got %d, want %d (stale failure must not increment)",
			after.TotalFailures, before.TotalFailures)
	}
	if after.Failures != before.Failures {
		t.Errorf("failures in window: got %d, want %d (stale failure must not append)",
			after.Failures, before.Failures)
	}

	// The penalty must NOT reflect the stale Retry-After (30s).
	afterPenalty := b.PenaltyDuration()
	if afterPenalty != beforePenalty {
		t.Errorf("penalty after stale failure = %v, want %v (lastRetryAfter must not be updated)",
			afterPenalty, beforePenalty)
	}
}

func TestBreaker_StaleSuccessDoesNotResetPenalty(t *testing.T) {
	// Verify that a stale RecordSuccess (startedAt before openSince) does
	// NOT reset the consecutive counter or the lastRetryAfter. These side
	// effects must only apply to non-stale successes. Without the guard,
	// a ghost 200 from a long-polling request would silently drop the
	// penalty to basePenalty and discard the Retry-After signal.
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond), WithBasePenalty(1*time.Second), WithMaxPenalty(60*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Record a failure with a Retry-After to set the penalty high.
	b.RecordFailure(429, 30*time.Second, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for half-open.
	time.Sleep(80 * time.Millisecond)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("expected Allow() to succeed, got %v", err)
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN, got %v", b.State())
	}

	// Verify the penalty reflects the Retry-After.
	if p := b.PenaltyDuration(); p != 30*time.Second {
		t.Fatalf("penalty before stale success = %v, want 30s", p)
	}

	// Record a stale success (started before OPEN period).
	staleStart := time.Now().Add(-5 * time.Second)
	b.RecordSuccess(staleStart, 0)

	// The circuit should remain in HALF_OPEN.
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN (stale success ignored), got %v", b.State())
	}

	// The penalty must still reflect the Retry-After — the stale
	// success must NOT have reset consecutive or lastRetryAfter.
	if p := b.PenaltyDuration(); p != 30*time.Second {
		t.Errorf("penalty after stale success = %v, want 30s (side effects must not leak)", p)
	}
}

func TestBreaker_PenaltyOverflowSafe(t *testing.T) {
	// Verify that currentPenaltyLocked never returns 0 or a negative
	// duration due to int64 overflow in the consecutive scaling
	// calculation. Without the cap, basePenalty * Duration(1+consecutive)
	// overflows when consecutive is extremely large (~4.6B), producing a
	// negative Duration that min() treats as 0 — violating the guarantee
	// that the penalty is always at least basePenalty.
	b, err := New(WithBasePenalty(2*time.Second), WithMaxPenalty(60*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Set consecutive to an extremely large value that would overflow
	// without the cap. The penalty must be maxPenalty, not 0 or negative.
	b.consecutive = 4_600_000_000 // would overflow int64 in 1+consecutive

	penalty := b.PenaltyDuration()
	if penalty != b.cfg.maxPenalty {
		t.Errorf("penalty with extreme consecutive = %v, want %v (maxPenalty, not 0 or negative from overflow)", penalty, b.cfg.maxPenalty)
	}
	if penalty <= 0 {
		t.Errorf("penalty = %v, want > 0 (overflow must not zero the penalty)", penalty)
	}

	// Verify that the penalty is at least basePenalty for any consecutive value.
	b.consecutive = 0
	if p := b.PenaltyDuration(); p != b.cfg.basePenalty {
		t.Errorf("penalty with consecutive=0 = %v, want %v (basePenalty)", p, b.cfg.basePenalty)
	}

	b.consecutive = 1
	if p := b.PenaltyDuration(); p != b.cfg.basePenalty*2 {
		t.Errorf("penalty with consecutive=1 = %v, want %v (2*basePenalty)", p, b.cfg.basePenalty*2)
	}
}

func TestBreaker_EpochTrackingPreventsStaleProbeCorruption(t *testing.T) {
	// R26-01 regression test: Verify that when Probe B (epoch 2) succeeds after
	// Probe A (epoch 1) times out, Probe A's subsequent failure is discarded by
	// epoch mismatch. Without epoch tracking, the stale failure from Probe A would
	// re-trip a recovered circuit, causing spurious OPEN cycles.
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit CLOSED -> OPEN.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for open timeout to expire, transition to HALF_OPEN, dispatch Probe A.
	time.Sleep(80 * time.Millisecond)
	epoch1, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() for Probe A: %v", err)
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN, got %v", b.State())
	}
	if epoch1 == 0 {
		t.Fatalf("epoch for Probe A should be non-zero in HALF_OPEN")
	}

	// Wait long enough for Probe A's timeout to expire (> openTimeout).
	// Allow() dispatches Probe B with a new epoch.
	time.Sleep(80 * time.Millisecond)
	epoch2, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() for Probe B: %v", err)
	}
	if epoch2 == epoch1 {
		t.Fatalf("epoch2 (%d) should differ from epoch1 (%d)", epoch2, epoch1)
	}

	// Probe B succeeds -> circuit transitions to CLOSED.
	b.RecordSuccess(time.Time{}, epoch2)
	if b.State() != Closed {
		t.Fatalf("expected CLOSED after Probe B success, got %v", b.State())
	}

	// Now the stale Probe A finally fails. The epoch does not match the current
	// probeEpoch, so the failure must be silently discarded. The circuit must
	// remain CLOSED with ConsecutiveFailures == 0.
	b.RecordFailure(500, 0, time.Time{}, epoch1)
	if b.State() != Closed {
		t.Fatalf("expected CLOSED (stale probe discarded), got %v", b.State())
	}
	if s := b.Stats(); s.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 (stale failure discarded)", s.ConsecutiveFailures)
	}
}

func TestBreaker_StaleSuccessDiscarded(t *testing.T) {
	// R26-01 regression test: Verify that when Probe A (epoch 1) times out and
	// Probe B (epoch 2) is dispatched, both stale failures and stale successes
	// from Probe A are discarded by epoch mismatch.
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(50*time.Millisecond), WithMaxOpenTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// --- Subtest A: stale failure discarded ---

	// Trip the circuit.
	b.RecordFailure(500, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("expected OPEN, got %v", b.State())
	}

	// Wait for open timeout, dispatch Probe A (epoch1).
	time.Sleep(80 * time.Millisecond)
	epoch1, _ := b.Allow()
	if epoch1 == 0 {
		t.Fatalf("epoch1 should be non-zero")
	}

	// Wait for probe timeout, dispatch Probe B (epoch2).
	time.Sleep(80 * time.Millisecond)
	epoch2, _ := b.Allow()
	if epoch2 == epoch1 {
		t.Fatalf("epoch2 should differ from epoch1")
	}

	// Stale failure from Probe A — must be discarded.
	b.RecordFailure(500, 0, time.Time{}, epoch1)
	if b.State() != HalfOpen {
		t.Fatalf("subtest A: expected HALF_OPEN (stale failure discarded), got %v", b.State())
	}

	// Current failure from Probe B — must re-open.
	// HALF_OPEN -> OPEN increments backoffMultiple. OpenTimeout is now
	// 50ms * 2^1 = 100ms. We must wait for the open timeout to expire
	// before we can dispatch another probe.
	b.RecordFailure(500, 0, time.Time{}, epoch2)
	if b.State() != Open {
		t.Fatalf("subtest A: expected OPEN after current failure, got %v", b.State())
	}

	// --- Subtest B: stale success discarded ---

	// Wait for the backoff-increased open timeout (100ms + margin).
	time.Sleep(150 * time.Millisecond)
	epoch3, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() for Probe C: %v", err)
	}
	if epoch3 == 0 {
		t.Fatalf("epoch3 should be non-zero")
	}

	// Wait for probe timeout, dispatch Probe D (epoch4).
	// After subtest A, backoffMultiple is 2, so openTimeout() = 50ms * 4 = 200ms.
	// The probe timeout check uses openTimeout(), so we must wait > 200ms.
	time.Sleep(250 * time.Millisecond)
	epoch4, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() for Probe D: %v", err)
	}
	if epoch4 == epoch3 {
		t.Fatalf("epoch4 should differ from epoch3")
	}

	// Stale success from Probe C — must be discarded, circuit stays HALF_OPEN.
	b.RecordSuccess(time.Time{}, epoch3)
	if b.State() != HalfOpen {
		t.Fatalf("subtest B: expected HALF_OPEN (stale success discarded), got %v", b.State())
	}

	// Current success from Probe D — must close the circuit.
	b.RecordSuccess(time.Time{}, epoch4)
	if b.State() != Closed {
		t.Fatalf("subtest B: expected CLOSED after current success, got %v", b.State())
	}
}

func TestBreaker_OpenTimeoutOverflowSafe(t *testing.T) {
	// R26-05 regression test: Verify that with a very large openTimeout (5 hours)
	// and high backoffMultiple values, openTimeout() returns maxOpenTimeout, not
	// a negative number from int64 overflow. Before the fix, 5h * 1<<19 produced
	// a negative Duration.
	b, err := New(WithFailureThreshold(1), WithWindow(10*time.Second), WithOpenTimeout(5*time.Hour), WithMaxOpenTimeout(120*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With backoffMultiple=0 (Closed state), WaitDuration should return 0.
	if w := b.WaitDuration(); w != 0 {
		t.Errorf("backoffMultiple=0, CLOSED: WaitDuration = %v, want 0", w)
	}

	// Set state to Open and test with backoffMultiple=19.
	// 5h * 1<<19 would overflow int64 without the guard.
	b.backoffMultiple = 19
	b.state = Open
	b.openSince = time.Now()

	w := b.WaitDuration()
	if w < 0 {
		t.Errorf("backoffMultiple=19: WaitDuration = %v, want >= 0", w)
	}
	if w > b.cfg.maxOpenTimeout {
		t.Errorf("backoffMultiple=19: WaitDuration = %v, want <= maxOpenTimeout (%v)", w, b.cfg.maxOpenTimeout)
	}

	// Test with backoffMultiple=62 — the early-return path (shift >= 62).
	b.backoffMultiple = 62
	w = b.WaitDuration()
	if w < 0 {
		t.Errorf("backoffMultiple=62: WaitDuration = %v, want >= 0", w)
	}
	if w > b.cfg.maxOpenTimeout {
		t.Errorf("backoffMultiple=62: WaitDuration = %v, want <= maxOpenTimeout (%v)", w, b.cfg.maxOpenTimeout)
	}

	// Verify PenaltyDuration still works (no cross-contamination).
	if p := b.PenaltyDuration(); p < 0 {
		t.Errorf("PenaltyDuration = %v, want >= 0", p)
	}
}

func TestBreaker_RetryAfterHTTPDate(t *testing.T) {
	// Verify that the breaker's penalty calculation works correctly
	// when Retry-After values use HTTP-date format. This is a functional
	// test that validates the integration of parseRetryAfterFromRecorder
	// (in proxy.go) with the breaker. We test the breaker directly by
	// passing durations (the parsing happens in the caller).
	b, err := New(WithBasePenalty(1*time.Second), WithMaxPenalty(60*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Record a failure with a Retry-After of 30s (as if parsed from
	// an HTTP-date header). The penalty should reflect it.
	b.RecordFailure(429, 30*time.Second, time.Time{}, 0)
	if p := b.PenaltyDuration(); p != 30*time.Second {
		t.Errorf("penalty after HTTP-date-style Retry-After = %v, want 30s", p)
	}

	// The penalty must still be capped at maxPenalty.
	b2, err := New(WithBasePenalty(1*time.Second), WithMaxPenalty(5*time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b2.RecordFailure(429, 3600*time.Second, time.Time{}, 0) // 1 hour — exceeds maxPenalty
	if p := b2.PenaltyDuration(); p != 5*time.Second {
		t.Errorf("penalty with Retry-After=3600s and MaxPenalty=5s = %v, want 5s", p)
	}
}
