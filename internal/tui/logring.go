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

package tui

import (
	"strings"
	"sync"
)

// logRingCapacity is the default line capacity of a logRing.
const logRingCapacity = 2048

// logRingItem is one retained log line together with its ring sequence
// number. The sequence is assigned under the same lock that manages eviction,
// so callers see exactly one identity per accepted line.
type logRingItem struct {
	seq  uint64
	text string
}

// logRing is a thread-safe ring buffer of log lines. It assigns a unique,
// monotonically increasing sequence number to every accepted line so detail
// views can pin an item across position shifts and duplicate text.
type logRing struct {
	mu       sync.Mutex
	lines    []logRingItem
	head     int
	count    int
	capacity int
	seq      uint64
}

func newLogRing(capacity int) *logRing {
	if capacity < 1 {
		capacity = 1
	}
	return &logRing{lines: make([]logRingItem, capacity), capacity: capacity}
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	text := string(p)
	for text != "" {
		idx := strings.IndexByte(text, '\n')
		var line string
		if idx < 0 {
			line = text
			text = ""
		} else {
			line = text[:idx]
			text = text[idx+1:]
		}
		// Blank lines are deliberately skipped here too, mirroring
		// LogBuffer.Write; pinned by TestLogRing_WriteEmptyLinesSkipped.
		if line == "" {
			continue
		}
		r.seq++
		item := logRingItem{seq: r.seq, text: line}
		r.lines[(r.head+r.count)%r.capacity] = item
		if r.count < r.capacity {
			r.count++
		} else {
			r.head = (r.head + 1) % r.capacity
		}
	}
	return len(p), nil
}

func (r *logRing) snapshot() []logRingItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count == 0 {
		return nil
	}
	out := make([]logRingItem, r.count)
	for i := 0; i < r.count; i++ {
		out[i] = r.lines[(r.head+i)%r.capacity]
	}
	return out
}

// Len returns the number of retained lines, guarded by the same lock that
// protects Write, so reading the total cannot race a concurrent writer.
func (r *logRing) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// logWriter wraps a logRing as an io.Writer.
type logWriter struct{ ring *logRing }

func (w *logWriter) Write(p []byte) (int, error) { return w.ring.Write(p) }
