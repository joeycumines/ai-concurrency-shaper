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
	"net/http"
	"testing"
	"time"
)

// TestWouldAllowAgreesWithAllowWithoutMutating pins that WouldAllow predicts
// Allow's verdict in every state while leaving the state machine untouched:
// an elapsed OPEN period stays OPEN, an in-flight probe keeps its epoch, and a
// superseded probe that is still valid can still close the breaker.
func TestWouldAllowAgreesWithAllowWithoutMutating(t *testing.T) {
	const openTimeout = 20 * time.Millisecond
	newOpen := func(t *testing.T) *Breaker {
		t.Helper()
		b, err := New(
			WithFailureThreshold(1),
			WithOpenTimeout(openTimeout),
			WithMaxOpenTimeout(openTimeout),
		)
		if err != nil {
			t.Fatal(err)
		}
		b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
		if b.State() != Open {
			t.Fatalf("state = %s, want OPEN", b.State())
		}
		return b
	}

	t.Run("closed", func(t *testing.T) {
		b, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if !b.WouldAllow() {
			t.Fatal("WouldAllow = false for CLOSED")
		}
	})

	t.Run("open before timeout", func(t *testing.T) {
		b := newOpen(t)
		if b.WouldAllow() {
			t.Fatal("WouldAllow = true before the open timeout elapsed")
		}
	})

	t.Run("open after timeout stays open", func(t *testing.T) {
		b := newOpen(t)
		time.Sleep(openTimeout + 10*time.Millisecond)
		if !b.WouldAllow() {
			t.Fatal("WouldAllow = false after the open timeout elapsed")
		}
		if got := b.State(); got != Open {
			t.Fatalf("state after WouldAllow = %s, want OPEN (no transition)", got)
		}
		if _, err := b.Allow(); err != nil {
			t.Fatalf("Allow after WouldAllow = %v, want a probe", err)
		}
	})

	t.Run("probe in flight", func(t *testing.T) {
		b := newOpen(t)
		time.Sleep(openTimeout + 10*time.Millisecond)
		epoch, err := b.Allow()
		if err != nil || epoch == 0 {
			t.Fatalf("Allow = (%d, %v), want a probe", epoch, err)
		}
		if b.WouldAllow() {
			t.Fatal("WouldAllow = true while the probe is in flight")
		}
	})

	t.Run("stale probe success still closes", func(t *testing.T) {
		b := newOpen(t)
		time.Sleep(openTimeout + 10*time.Millisecond)
		epoch, err := b.Allow()
		if err != nil || epoch == 0 {
			t.Fatalf("Allow = (%d, %v), want a probe", epoch, err)
		}
		time.Sleep(openTimeout + 10*time.Millisecond)
		if !b.WouldAllow() {
			t.Fatal("WouldAllow = false for a probe older than the open timeout")
		}
		b.RecordSuccess(time.Now(), epoch)
		if got := b.State(); got != Closed {
			t.Fatalf("state = %s, want CLOSED: peeking must not invalidate the in-flight probe", got)
		}
	})
}
