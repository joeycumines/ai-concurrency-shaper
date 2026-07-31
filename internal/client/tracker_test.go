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

package client

import (
	"sync"
	"testing"
	"time"
)

func TestClientTracker_RecordFailure_Crushed(t *testing.T) {
	track := NewClientTracker(5, 30*time.Second)
	originator := Originator{IsLocal: true, PID: 1234}

	// Record 6 failures within the window.
	for range 6 {
		track.RecordFailure(originator)
	}

	state, ok := track.GetState(originator)
	if !ok {
		t.Fatal("expected state to exist after recording failures")
	}
	if !state.IsCrushed {
		t.Errorf("expected IsCrushed=true after 6 failures with threshold=5, got false")
	}
	if state.FailureCount != 6 {
		t.Errorf("expected FailureCount=6, got %d", state.FailureCount)
	}
}

func TestClientTracker_RecordFailure_NotCrushed(t *testing.T) {
	track := NewClientTracker(5, 30*time.Second)
	originator := Originator{IsLocal: true, PID: 1234}

	// Record 4 failures within the window.
	for range 4 {
		track.RecordFailure(originator)
	}

	state, ok := track.GetState(originator)
	if !ok {
		t.Fatal("expected state to exist")
	}
	if state.IsCrushed {
		t.Errorf("expected IsCrushed=false after 4 failures with threshold=5")
	}
	if state.FailureCount != 4 {
		t.Errorf("expected FailureCount=4, got %d", state.FailureCount)
	}
}

func TestClientTracker_RecordSuccess_DoesNotClearFailures(t *testing.T) {
	track := NewClientTracker(5, 30*time.Second)
	originator := Originator{IsLocal: true, PID: 1234}

	// Record 6 failures -> crushed.
	for range 6 {
		track.RecordFailure(originator)
	}

	// Record a success — should NOT clear failures.
	track.RecordSuccess(originator)

	state, ok := track.GetState(originator)
	if !ok {
		t.Fatal("expected state to exist")
	}
	if !state.IsCrushed {
		t.Error("expected IsCrushed to remain true after success")
	}
	if state.FailureCount != 6 {
		t.Errorf("expected FailureCount=6 after success (not cleared), got %d", state.FailureCount)
	}
}

func TestClientTracker_GetState_Unknown(t *testing.T) {
	track := NewClientTracker(5, 30*time.Second)
	originator := Originator{IsLocal: true, PID: 1234}

	state, ok := track.GetState(originator)
	if ok {
		t.Errorf("expected no state for unknown originator, got %+v", state)
	}
	if state != nil {
		t.Errorf("expected nil state for unknown originator, got %+v", state)
	}
}

func TestClientTracker_Fingerprint(t *testing.T) {
	track := NewClientTracker(5, 30*time.Second)

	local := Originator{IsLocal: true, PID: 1234, IP: "127.0.0.1"}
	fp := track.Fingerprint(local)
	if fp == "" {
		t.Error("expected non-empty fingerprint for local originator")
	}
	// Local with PID should use pid prefix (not ip prefix).
	// Fingerprint uses strconv.Itoa to produce decimal PID strings.

	remote := Originator{IsLocal: false, IP: "203.0.113.50"}
	fp = track.Fingerprint(remote)
	if fp != "ip:203.0.113.50" {
		t.Errorf("expected fingerprint ip:203.0.113.50, got %q", fp)
	}
}

func TestClientTracker_IsCrushed(t *testing.T) {
	track := NewClientTracker(3, 30*time.Second)
	originator := Originator{IsLocal: true, PID: 99}

	if track.IsCrushed(originator) {
		t.Error("expected IsCrushed=false before any failures recorded")
	}

	for range 4 {
		track.RecordFailure(originator)
	}

	if !track.IsCrushed(originator) {
		t.Error("expected IsCrushed=true after exceeding threshold")
	}
}

func TestClientTracker_FailureCount(t *testing.T) {
	track := NewClientTracker(10, 30*time.Second)
	originator := Originator{IsLocal: true, PID: 42}

	if track.FailureCount(originator) != 0 {
		t.Error("expected FailureCount=0 for unknown originator")
	}

	track.RecordFailure(originator)
	track.RecordFailure(originator)

	if track.FailureCount(originator) != 2 {
		t.Errorf("expected FailureCount=2, got %d", track.FailureCount(originator))
	}
}

func TestClientTracker_Concurrency(t *testing.T) {
	track := NewClientTracker(10, 30*time.Second)
	originator := Originator{IsLocal: true, PID: 777}

	var wg sync.WaitGroup
	wg.Add(100)
	for range 100 {
		go func() {
			defer wg.Done()
			track.RecordFailure(originator)
			track.GetState(originator)
			track.IsCrushed(originator)
			track.FailureCount(originator)
		}()
	}
	wg.Wait()

	state, ok := track.GetState(originator)
	if !ok {
		t.Fatal("expected state to exist after concurrent recording")
	}
	if state.FailureCount != 100 {
		t.Errorf("expected FailureCount=100 after 100 concurrent recordings, got %d", state.FailureCount)
	}
}

func TestClientTracker_Prune(t *testing.T) {
	track := NewClientTracker(5, 100*time.Millisecond)
	originator := Originator{IsLocal: true, PID: 1}

	track.RecordFailure(originator)

	// Wait for the failure to fall outside the window.
	time.Sleep(150 * time.Millisecond)

	track.Prune()

	state, ok := track.GetState(originator)
	if ok {
		t.Errorf("expected state to be pruned, but still exists with FailureCount=%d", state.FailureCount)
	}
}

func TestClientTracker_Stats(t *testing.T) {
	track := NewClientTracker(5, 30*time.Second)
	originator := Originator{IsLocal: true, PID: 1}

	track.RecordFailure(originator)

	stats := track.Stats()
	if len(stats) == 0 {
		t.Error("expected non-empty stats after recording a failure")
	}
}

func TestClientTracker_SortedFingerprints(t *testing.T) {
	track := NewClientTracker(5, 30*time.Second)

	// Record more failures for one originator than another.
	o1 := Originator{IsLocal: true, PID: 1}
	o2 := Originator{IsLocal: true, PID: 2}

	for range 8 {
		track.RecordFailure(o1)
	}
	for range 3 {
		track.RecordFailure(o2)
	}

	fps := track.SortedFingerprints()
	if len(fps) < 2 {
		t.Fatalf("expected at least 2 fingerprints, got %d", len(fps))
	}
	// Both should be in the list.
	seen := make(map[string]bool)
	for _, fp := range fps {
		seen[fp] = true
	}
	fp1 := track.Fingerprint(o1)
	fp2 := track.Fingerprint(o2)
	if !seen[fp1] {
		t.Errorf("expected fingerprint %q in sorted results", fp1)
	}
	if !seen[fp2] {
		t.Errorf("expected fingerprint %q in sorted results", fp2)
	}
}

func TestNewClientTracker_Defaults(t *testing.T) {
	// Test that zero values are replaced with defaults.
	track := NewClientTracker(0, 0)

	if track.failureThreshold != 10 {
		t.Errorf("expected default threshold 10, got %d", track.failureThreshold)
	}
	if track.failureWindow != 30*time.Second {
		t.Errorf("expected default window 30s, got %v", track.failureWindow)
	}
}
