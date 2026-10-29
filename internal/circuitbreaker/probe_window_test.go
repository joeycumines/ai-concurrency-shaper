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

const probeWindowOpenTimeout = 20 * time.Millisecond

// probeWindowBreaker returns a breaker that has just tripped OPEN, with an
// open timeout short enough for tests to drive HALF_OPEN probes directly.
func probeWindowBreaker(t *testing.T) *Breaker {
	t.Helper()
	b, err := New(
		WithFailureThreshold(1),
		WithOpenTimeout(probeWindowOpenTimeout),
		WithMaxOpenTimeout(probeWindowOpenTimeout),
	)
	if err != nil {
		t.Fatal(err)
	}
	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
	if b.State() != Open {
		t.Fatalf("state = %s, want OPEN", b.State())
	}
	time.Sleep(probeWindowOpenTimeout + 10*time.Millisecond)
	return b
}

// awaitStaleProbe sleeps past the open timeout, so a probe admitted before
// the call counts as stale and a later Allow supersedes it.
func awaitStaleProbe() {
	time.Sleep(2 * probeWindowOpenTimeout)
}

// A probe superseded by a request that then gives up is still running
// upstream. Cancelling the superseding probe must leave the superseded probe
// able to close the breaker when it succeeds.
func TestBreakerSupersededProbeSurvivesSupersederCancellation(t *testing.T) {
	b := probeWindowBreaker(t)
	older, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() admitting the first probe: %v", err)
	}
	awaitStaleProbe()
	superseding, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() superseding the stale probe: %v", err)
	}
	b.CancelProbe(superseding)

	b.RecordSuccess(time.Now(), older)
	if got := b.State(); got != Closed {
		t.Fatalf("state after the superseded probe succeeded = %s, want CLOSED", got)
	}
}

// Once the superseding probe was cancelled, the older probe is the one still
// in flight. Its failure is genuine upstream evidence and must reopen the
// breaker, exactly as the newest probe's failure would.
func TestBreakerCancelledSupersederRestoresOlderProbeAsInFlight(t *testing.T) {
	b := probeWindowBreaker(t)
	older, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() admitting the first probe: %v", err)
	}
	awaitStaleProbe()
	superseding, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() superseding the stale probe: %v", err)
	}
	b.CancelProbe(superseding)

	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, older)
	if got := b.State(); got != Open {
		t.Fatalf("state after the restored probe failed = %s, want OPEN", got)
	}
}

// While the superseding probe is still in flight, a superseded probe's
// failure is stale and must not decide the breaker.
func TestBreakerSupersededProbeFailureIgnoredWhileSupersederLives(t *testing.T) {
	b := probeWindowBreaker(t)
	older, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() admitting the first probe: %v", err)
	}
	awaitStaleProbe()
	superseding, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() superseding the stale probe: %v", err)
	}

	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, older)
	if got := b.State(); got != HalfOpen {
		t.Fatalf("state after the superseded probe failed = %s, want HALF_OPEN", got)
	}
	b.RecordSuccess(time.Time{}, superseding)
	if got := b.State(); got != Closed {
		t.Fatalf("state after the superseding probe succeeded = %s, want CLOSED", got)
	}
}

// A cancelled probe is resolved: its own late result must stay stale even
// while an older superseded probe is still live in the same window.
func TestBreakerCancelledProbeLateResultStaysStaleWhileOlderProbeLives(t *testing.T) {
	b := probeWindowBreaker(t)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("Allow() admitting the first probe: %v", err)
	}
	awaitStaleProbe()
	superseding, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() superseding the stale probe: %v", err)
	}
	b.CancelProbe(superseding)

	before := b.Stats().TotalSuccesses
	b.RecordSuccess(time.Now(), superseding)
	if got := b.State(); got != HalfOpen {
		t.Fatalf("state after the cancelled probe's late success = %s, want HALF_OPEN", got)
	}
	if after := b.Stats().TotalSuccesses; after != before {
		t.Fatalf("TotalSuccesses changed from %d to %d on a cancelled probe's late success", before, after)
	}
}

// Several superseded probes can remain live at once. Cancelling the newest
// must leave every older live probe able to close the breaker.
func TestBreakerProbeWindowKeepsEverySupersededProbeLive(t *testing.T) {
	b := probeWindowBreaker(t)
	first, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() admitting the first probe: %v", err)
	}
	awaitStaleProbe()
	if _, err := b.Allow(); err != nil {
		t.Fatalf("Allow() superseding the first probe: %v", err)
	}
	awaitStaleProbe()
	third, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() superseding the second probe: %v", err)
	}
	b.CancelProbe(third)

	b.RecordSuccess(time.Now(), first)
	if got := b.State(); got != Closed {
		t.Fatalf("state after the oldest live probe succeeded = %s, want CLOSED", got)
	}
}

// Closing the breaker ends the window: a probe that was still live when the
// breaker closed must not reopen or otherwise act on the new CLOSED state.
func TestBreakerClosingEndsProbeWindow(t *testing.T) {
	b := probeWindowBreaker(t)
	older, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() admitting the first probe: %v", err)
	}
	awaitStaleProbe()
	superseding, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() superseding the stale probe: %v", err)
	}
	b.RecordSuccess(time.Now(), superseding)
	if got := b.State(); got != Closed {
		t.Fatalf("state after the superseding probe succeeded = %s, want CLOSED", got)
	}

	before := b.Stats().TotalSuccesses
	b.RecordSuccess(time.Now(), older)
	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, older)
	if got := b.State(); got != Closed {
		t.Fatalf("state after the old probe reported in CLOSED = %s, want CLOSED", got)
	}
	if after := b.Stats().TotalSuccesses; after != before {
		t.Fatalf("TotalSuccesses changed from %d to %d on a late result after the window closed", before, after)
	}
}
