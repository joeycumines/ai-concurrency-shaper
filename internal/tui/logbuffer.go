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
	"bytes"
	"io"
	"sync"
)

// LogBufferCapacity is the default line capacity of a LogBuffer.
const LogBufferCapacity = 2048

// logBufLine is a single captured line with a globally unique sequence number.
// The sequence number lets a poller read only the lines written since its last
// read without deduplicating by snapshot identity.
type logBufLine struct {
	seq  uint64
	text string
}

// LogBuffer is a thread-safe, bounded capture buffer for program log output. It
// is the sink that main() installs for the global log/slog writers when the TUI
// is enabled, so that (a) logs never leak to stderr and (b) the Logs tab
// has a bounded, pollable source of lines. Lines beyond capacity evict the oldest.
// Bounded means line count: an individual line — and thus the transient
// allocation publishing it — is as large as its input write.
//
// Once RedirectTo hands the buffer a live target (main.go does this the moment
// the TUI exits), Writes stream straight through to that target instead: after
// the dashboard is gone there is no poller left to drain the ring, so teardown
// telemetry must not be parked here to die with the process. The flip also
// hands over every line no poller has ever delivered, plus any pending
// fragment, so nothing undelivered is stranded in the dead buffer.
type LogBuffer struct {
	mu       sync.Mutex
	lines    []logBufLine
	head     int
	count    int
	capacity int
	seq      uint64
	pending  []byte

	// polled is the delivery high-water mark: the highest sequence number any
	// ReadNew call has returned to a poller. Lines above it were never seen by
	// the TUI and are exactly what RedirectTo forwards at teardown.
	polled uint64

	passthrough io.Writer
}

// NewLogBuffer returns an empty LogBuffer with the given line capacity.
func NewLogBuffer(capacity int) *LogBuffer {
	if capacity < 1 {
		capacity = 1
	}
	return &LogBuffer{lines: make([]logBufLine, capacity), capacity: capacity}
}

// maxPendingLine caps the unterminated fragment a LogBuffer retains between
// Writes (64 KiB). Both real producers (stdlib log, slog TextHandler)
// terminate every write with a newline, so the fragment stays empty in
// practice; the cap only bounds memory if some writer streams newline-free
// bytes without end.
const maxPendingLine = 64 << 10

// Write captures complete newline-terminated lines from p, assembling a logical
// line that is split across multiple Write calls (io.Writer has no line-boundary
// guarantees, so a trailing unterminated fragment is carried forward until a
// newline arrives or Flush is called rather than being published as a phony
// line). The retained fragment never exceeds maxPendingLine bytes: a writer
// that streams past the cap without a newline has its accumulated fragment
// published at the cap boundary with Flush semantics, so retained memory stays
// bounded while no bytes are dropped or reordered. It always consumes all of p
// and reports that: the caller's byte count is the full length of the input,
// never a partial write.
func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.passthrough != nil {
		return b.passthrough.Write(p)
	}
	total := len(p)
	// Prepend any fragment left over from a prior write so a line split across
	// Write boundaries is reassembled before splitting again.
	if b.pending != nil {
		data := make([]byte, 0, len(b.pending)+len(p))
		data = append(data, b.pending...)
		data = append(data, p...)
		b.pending = nil
		p = data
	}
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx >= 0 {
			seg := p[:idx]
			p = p[idx+1:]
			// Blank segments are deliberately not published: real producers
			// (stdlib log, slog TextHandler) never emit bare empty lines and
			// they would only spend ring capacity — pinned by
			// TestLogBuffer_EmptySegmentsSkipped.
			if len(seg) > 0 {
				b.publish(seg)
			}
			continue
		}
		// Unterminated tail. Retain it — as a copy (io.Writer implementations
		// must not retain p, and the caller is free to reuse its buffer after
		// Write returns) — unless it exceeds the cap, in which case publish the
		// overflow now rather than let retained memory grow without bound.
		if len(p) > maxPendingLine {
			b.publish(p[:maxPendingLine])
			p = p[maxPendingLine:]
			continue
		}
		b.pending = append([]byte(nil), p...)
		break
	}
	return total, nil
}

// publish appends one complete line to the ring, evicting the oldest line when
// full. Callers must hold b.mu.
func (b *LogBuffer) publish(text []byte) {
	b.seq++
	line := logBufLine{seq: b.seq, text: string(text)}
	if b.count < b.capacity {
		b.lines[(b.head+b.count)%b.capacity] = line
		b.count++
	} else {
		b.lines[b.head] = line
		b.head = (b.head + 1) % b.capacity
	}
}

// Flush publishes any unterminated fragment retained from prior Write calls as
// a single final line. It is idempotent and a no-op when no fragment is pending.
func (b *LogBuffer) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		return
	}
	b.publish(b.pending)
	b.pending = nil
}

// RedirectTo switches the buffer into live passthrough mode: from this call on,
// Write delegates directly to w (under the same lock, so producers cannot tear
// a line across the switch), bypassing the bounded ring entirely. Before the
// flip it hands w everything no poller has delivered: every retained line whose
// sequence number exceeds the ReadNew high-water mark, then any pending
// fragment, so nothing undelivered is stranded in the dead buffer. Forwarded
// content is dropped from the buffer — repeated redirects, or a restore-and-flip
// cycle, can therefore never emit a line twice, and lines a poller already
// delivered are never re-emitted. A nil w restores ordinary buffering without
// forwarding anything. This is what main.go invokes the moment the TUI exits:
// with the dashboard gone there is no poller to drain the ring, so
// graceful-shutdown logging must stream to stderr instead of being parked here
// until process exit.
func (b *LogBuffer) RedirectTo(w io.Writer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if w == nil {
		b.passthrough = nil
		return
	}
	b.passthrough = w
	var out bytes.Buffer
	for i := 0; i < b.count; i++ {
		if line := b.lines[(b.head+i)%b.capacity]; line.seq > b.polled {
			out.WriteString(line.text)
			out.WriteByte('\n')
		}
	}
	out.Write(b.pending)
	// Best-effort flush mirroring the io.Writer contract elsewhere in this
	// type: a failing sink cannot be meaningfully handled under this lock,
	// and the handed-off content is dropped either way.
	_, _ = w.Write(out.Bytes()) //nolint:errcheck
	b.pending = nil
	b.head, b.count = 0, 0
}

// Revision returns the sequence number of the most recently written line (0 when
// none have been written). It is monotonic across overwrites.
func (b *LogBuffer) Revision() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// ReadNew returns every retained line whose sequence number exceeds after, in
// write order, together with the revision the caller must resume from. Both are
// computed under a single lock, so a line written in the middle of a poll can
// never be returned twice: this poll either returns it (and the caller resumes
// past it) or misses it (and the next poll returns it, since it is still
// retained). Lines evicted by the bounded capacity are simply not returned; the
// revision still advances past them so an evicted line is never re-read.
//
// Each call also advances the buffer's polled high-water mark to the returned
// revision, recording what has been delivered; RedirectTo relies on that mark
// to forward only lines no poller ever saw.
func (b *LogBuffer) ReadNew(after uint64) ([]string, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.polled = b.seq
	if after >= b.seq {
		return nil, b.seq
	}
	var out []string
	for i := 0; i < b.count; i++ {
		if line := b.lines[(b.head+i)%b.capacity]; line.seq > after {
			out = append(out, line.text)
		}
	}
	return out, b.seq
}
