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
	"sort"
	"sync"
	"time"
)

// ClientState holds the current failure tracking state for a single originator.
type ClientState struct {
	// FailureCount is the number of failures recorded in the current window.
	FailureCount int
	// FailureTimestamps is a sorted list of timestamps of failures
	// within the current window. Used for windowed counting.
	FailureTimestamps []time.Time
	// LastFailure is the time of the most recent failure.
	LastFailure time.Time
	// IsCrushed is true when the client has exceeded the failure threshold
	// within the failure window.
	IsCrushed bool
}

// ClientTracker tracks per-originator failure patterns and identifies
// "crushed" clients that should receive special handling (e.g., retry-on-403).
// The tracker is safe for concurrent use by multiple goroutines.
type ClientTracker struct {
	mu               sync.RWMutex
	clients          map[string]*ClientState
	failureThreshold int
	failureWindow    time.Duration
}

// NewClientTracker creates a ClientTracker with the given failure
// threshold and window. A client is considered crushed when it exceeds
// failureThreshold failures within the failureWindow duration.
func NewClientTracker(failureThreshold int, failureWindow time.Duration) *ClientTracker {
	if failureThreshold <= 0 {
		failureThreshold = 10
	}
	if failureWindow <= 0 {
		failureWindow = 30 * time.Second
	}
	return &ClientTracker{
		clients:          make(map[string]*ClientState),
		failureThreshold: failureThreshold,
		failureWindow:    failureWindow,
	}
}

// Fingerprint returns a stable key for the originator used for tracking.
// Local processes are keyed by PID; remote clients are keyed by IP.
func (t *ClientTracker) Fingerprint(o Originator) string {
	return o.Fingerprint()
}

// RecordFailure records a failure for the given originator.
// It updates the sliding failure window and checks if the client
// has become crushed.
func (t *ClientTracker) RecordFailure(o Originator) {
	fp := t.Fingerprint(o)
	t.mu.Lock()
	defer t.mu.Unlock()
	state, exists := t.clients[fp]
	if !exists {
		state = &ClientState{}
		t.clients[fp] = state
	}
	now := time.Now()
	state.FailureTimestamps = append(state.FailureTimestamps, now)
	state.LastFailure = now
	state.FailureCount = len(state.FailureTimestamps)
	// Prune timestamps outside the window.
	cutoff := now.Add(-t.failureWindow)
	pruned := state.FailureTimestamps[:0]
	for _, ts := range state.FailureTimestamps {
		if ts.After(cutoff) {
			pruned = append(pruned, ts)
		}
	}
	state.FailureTimestamps = pruned
	state.FailureCount = len(state.FailureTimestamps)
	state.IsCrushed = state.FailureCount >= t.failureThreshold
}

// RecordSuccess records a successful request for the given originator.
// This does not reset the failure count (crushed status persists until
// the window clears naturally).
func (t *ClientTracker) RecordSuccess(o Originator) {
	// Success does not clear failure state — crushed status persists
	// until the window clears. This ensures that a client that was
	// crushed remains eligible for special handling until the failure
	// pattern genuinely subsides.
}

// GetState returns the current ClientState for the given originator.
// Returns nil, false if no state has been recorded for this originator.
// The returned state has expired timestamps pruned so that
// FailureCount reflects only failures within the active window.
func (t *ClientTracker) GetState(o Originator) (*ClientState, bool) {
	fp := t.Fingerprint(o)
	t.mu.RLock()
	defer t.mu.RUnlock()
	state, exists := t.clients[fp]
	if !exists {
		return nil, false
	}
	// Prune timestamps outside the window so the count is accurate
	// even if RecordFailure hasn't been called recently.
	// Allocate a new slice to avoid mutating the shared
	// backing array of state.FailureTimestamps under RLock.
	now := time.Now()
	cutoff := now.Add(-t.failureWindow)
	var pruned []time.Time
	for _, ts := range state.FailureTimestamps {
		if ts.After(cutoff) {
			pruned = append(pruned, ts)
		}
	}
	copied := *state
	copied.FailureTimestamps = make([]time.Time, len(pruned))
	copy(copied.FailureTimestamps, pruned)
	copied.FailureCount = len(pruned)
	copied.IsCrushed = copied.FailureCount >= t.failureThreshold
	return &copied, true
}

// IsCrushed returns true if the originator has exceeded the failure
// threshold within the failure window. Returns false if no failures
// have been recorded for this originator.
func (t *ClientTracker) IsCrushed(o Originator) bool {
	state, ok := t.GetState(o)
	return ok && state.IsCrushed
}

// FailureCount returns the current failure count for the originator
// within the sliding window. Returns 0 if no failures have been recorded.
func (t *ClientTracker) FailureCount(o Originator) int {
	state, ok := t.GetState(o)
	if !ok {
		return 0
	}
	return state.FailureCount
}

// Prune removes stale client entries whose last failure is older than
// the failure window. This prevents unbounded memory growth when the
// proxy handles many unique clients over time.
func (t *ClientTracker) Prune() {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := time.Now().Add(-t.failureWindow)
	for fp, state := range t.clients {
		if state.LastFailure.Before(cutoff) {
			delete(t.clients, fp)
		}
	}
}

// Stats returns a snapshot of the tracker's current state.
// The returned map is a copy and safe for concurrent use.
func (t *ClientTracker) Stats() map[string]ClientState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	stats := make(map[string]ClientState, len(t.clients))
	for fp, state := range t.clients {
		copied := *state
		copied.FailureTimestamps = make([]time.Time, len(state.FailureTimestamps))
		copy(copied.FailureTimestamps, state.FailureTimestamps)
		stats[fp] = copied
	}
	return stats
}

// SortedFingerprints returns the tracked originator fingerprints
// sorted by failure count descending. Useful for diagnostics.
func (t *ClientTracker) SortedFingerprints() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	type entry struct {
		fp    string
		count int
	}
	entries := make([]entry, 0, len(t.clients))
	for fp, state := range t.clients {
		entries = append(entries, entry{fp, state.FailureCount})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].count > entries[j].count
	})
	result := make([]string, 0, len(entries))
	for _, e := range entries {
		result = append(result, e.fp)
	}
	return result
}
