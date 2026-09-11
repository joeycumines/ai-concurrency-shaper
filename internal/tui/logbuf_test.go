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
	"fmt"
	"strings"
	"sync"
	"testing"
)

// ringTexts extracts the text of a ring snapshot for assertions that only
// care about line content.
func ringTexts(r *logRing) []string {
	items := r.snapshot()
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.text
	}
	return out
}

type safeBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

// scrollbarTop returns the first terminal row that belongs to the scrollbar
// track for the current tab. It is offset past the fixed header rows so that
// the scrollbar aligns with the scrollable data area.

func TestLogRing_WriteSingleLine(t *testing.T) {
	r := newLogRing(10)
	r.Write([]byte("hello"))
	snap := ringTexts(r)
	if len(snap) != 1 || snap[0] != "hello" {
		t.Errorf("snapshot = %v, want [hello]", snap)
	}
}

func TestLogRing_WriteMultipleLines(t *testing.T) {
	r := newLogRing(10)
	r.Write([]byte("line1\nline2\nline3\n"))
	snap := ringTexts(r)
	if len(snap) != 3 {
		t.Errorf("len(snapshot) = %d, want 3", len(snap))
	}
	if snap[0] != "line1" || snap[1] != "line2" || snap[2] != "line3" {
		t.Errorf("snapshot = %v, want [line1 line2 line3]", snap)
	}
}

func TestLogRing_WriteEmptyLinesSkipped(t *testing.T) {
	r := newLogRing(10)
	r.Write([]byte("\n\nhello\n\n"))
	snap := ringTexts(r)
	if len(snap) != 1 || snap[0] != "hello" {
		t.Errorf("snapshot = %v, want [hello]", snap)
	}
}

func TestLogRing_SnapshotEmpty(t *testing.T) {
	r := newLogRing(10)
	snap := ringTexts(r)
	if len(snap) != 0 {
		t.Errorf("snapshot = %v, want empty", snap)
	}
}

func TestLogRing_CapacityOverflow(t *testing.T) {
	r := newLogRing(3)
	r.Write([]byte("a\n"))
	r.Write([]byte("b\n"))
	r.Write([]byte("c\n"))
	r.Write([]byte("d\n"))
	snap := ringTexts(r)
	if len(snap) != 3 {
		t.Fatalf("len(snapshot) = %d, want 3", len(snap))
	}
	if snap[0] != "b" || snap[1] != "c" || snap[2] != "d" {
		t.Errorf("snapshot = %v, want [b c d] (oldest evicted)", snap)
	}
}

func TestLogWriter_DelegatesToRing(t *testing.T) {
	r := newLogRing(10)
	w := &logWriter{ring: r}
	n, err := w.Write([]byte("via writer\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 11 {
		t.Errorf("n = %d, want 11", n)
	}
	snap := ringTexts(r)
	if len(snap) != 1 || snap[0] != "via writer" {
		t.Errorf("snapshot = %v, want [via writer]", snap)
	}
}

func TestLogRing_ConcurrentWrite(t *testing.T) {
	r := newLogRing(100)
	done := make(chan struct{})
	for i := range 10 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := range 100 {
				r.Write(fmt.Appendf(nil, "g%d-line%d\n", i, j))
			}
		}(i)
	}
	for range 10 {
		<-done
	}
	snap := r.snapshot()
	if len(snap) == 0 {
		t.Error("expected some log lines after concurrent writes")
	}
}

// ─── TUI-08: visibleLogLines / renderLogs ───

func TestLogBuffer_ReadNew_Sequential(t *testing.T) {
	b := NewLogBuffer(8)
	if got := b.Revision(); got != 0 {
		t.Fatalf("Revision = %d, want 0", got)
	}
	b.Write([]byte("one\ntwo\n"))
	b.Write([]byte("three\n"))

	lines, rev := b.ReadNew(0)
	if rev != 3 {
		t.Fatalf("ReadNew cursor = %d, want 3", rev)
	}
	if len(lines) != 3 || lines[0] != "one" || lines[2] != "three" {
		t.Fatalf("ReadNew(0) = %v, want [one two three]", lines)
	}

	// Polling from the returned cursor yields only the new line.
	b.Write([]byte("four\n"))
	next, rev2 := b.ReadNew(rev)
	if len(next) != 1 || next[0] != "four" {
		t.Fatalf("ReadNew(after) = %v, want [four]", next)
	}
	if rev2 != 4 {
		t.Fatalf("ReadNew cursor = %d, want 4", rev2)
	}
	// Nothing further: empty result, cursor remains.
	if got, cur := b.ReadNew(b.Revision()); got != nil || cur != 4 {
		t.Fatalf("ReadNew(current) = %v,%d, want nil,4", got, cur)
	}
}

func TestLogBuffer_CapacityEvictsOldest(t *testing.T) {
	b := NewLogBuffer(3)
	for _, l := range []string{"a", "b", "c", "d"} {
		b.Write([]byte(l + "\n"))
	}
	lines, rev := b.ReadNew(0)
	if len(lines) != 3 || lines[0] != "b" || lines[2] != "d" {
		t.Fatalf("ReadNew(0) = %v, want [b c d]", lines)
	}
	if rev != 4 {
		t.Fatalf("revision = %d, want 4 (monotonic past eviction)", rev)
	}
	// Evicted lines are never re-read after the cursor advances.
	if got, _ := b.ReadNew(rev); got != nil {
		t.Fatalf("ReadNew(after eviction) = %v, want nil", got)
	}
}

func TestLogBuffer_EmptySegmentsSkipped(t *testing.T) {
	b := NewLogBuffer(8)
	b.Write([]byte("\n\nhello\n\n"))
	lines, rev := b.ReadNew(0)
	if len(lines) != 1 || lines[0] != "hello" {
		t.Fatalf("ReadNew(0) = %v, want [hello]", lines)
	}
	if rev != 1 {
		t.Fatalf("revision = %d, want 1", rev)
	}
}

// TestLogBuffer_Write_ReturnsInputLength pins the io.Writer contract: Write
// consumes all of p and must report that it did. Regression for the log-buffer
// sink returning n=0 for non-empty input.
func TestLogBuffer_Write_ReturnsInputLength(t *testing.T) {
	b := NewLogBuffer(8)
	for _, in := range [][]byte{[]byte("one\ntwo\nthree"), []byte("single"), []byte("a\nb\n")} {
		if n, err := b.Write(in); err != nil || n != len(in) {
			t.Fatalf("Write(%q) = n=%d err=%v, want n=%d err=nil", in, n, err, len(in))
		}
	}
}

// TestLogBuffer_Write_DoesNotRetainCallerSlice pins the io.Writer no-retain
// rule on the fragment path: a caller that reuses its buffer after Write must
// not corrupt the withheld partial line.
func TestLogBuffer_Write_DoesNotRetainCallerSlice(t *testing.T) {
	b := NewLogBuffer(8)
	frag := []byte("hel")
	if _, err := b.Write(frag); err != nil {
		t.Fatalf("Write = %v, want nil", err)
	}
	frag[0] = 'X' // caller reuse between writes
	b.Write([]byte("lo\n"))
	lines, _ := b.ReadNew(0)
	if len(lines) != 1 || lines[0] != "hello" {
		t.Fatalf("ReadNew(0) = %v, want [hello]", lines)
	}
}

// TestLogBuffer_ReadNew_NoDuplicateDelivery pins the single-lock cursor: a line
// written after a poll is delivered exactly once on the next poll — the TOCTOU
// between a separate Revision() and ReadNew() call would deliver it twice.
func TestLogBuffer_ReadNew_NoDuplicateDelivery(t *testing.T) {
	b := NewLogBuffer(4)
	b.Write([]byte("a\nb\nc\n"))
	_, cur := b.ReadNew(0) // cursor = 3

	b.Write([]byte("d\n")) // a write racing the previous poll
	next, cur2 := b.ReadNew(cur)
	if len(next) != 1 || next[0] != "d" {
		t.Fatalf("second poll = %v, want [d] (no duplicates)", next)
	}
	if cur2 != 4 {
		t.Fatalf("cursor = %d, want 4", cur2)
	}
	if got, _ := b.ReadNew(cur2); got != nil {
		t.Fatalf("third poll = %v, want nil", got)
	}
}

// TestLogBuffer_FragmentedWritesAssembleLines pins the line-assembly contract:
// io.Writer makes no line-boundary guarantee, so a logical line split across
// Write calls must still arrive as one line, and an unterminated tail must not
// be published until the newline that completes it.
func TestLogBuffer_FragmentedWritesAssembleLines(t *testing.T) {
	b := NewLogBuffer(8)
	b.Write([]byte("hel"))
	b.Write([]byte("lo\n"))
	b.Write([]byte("one\nt"))
	b.Write([]byte("wo\n"))
	lines, rev := b.ReadNew(0)
	if len(lines) != 3 || lines[0] != "hello" || lines[1] != "one" || lines[2] != "two" {
		t.Fatalf("ReadNew(0) = %v, want [hello one two]", lines)
	}
	if rev != 3 {
		t.Fatalf("revision = %d, want 3", rev)
	}
	// The unterminated fragment "tail" from the writes above has no newline, so
	// it must not have been published as a line.
	b.Write([]byte("tail"))
	if got, _ := b.ReadNew(rev); got != nil {
		t.Fatalf("ReadNew(after unterminated fragment) = %v, want nil", got)
	}
}

// TestLogBuffer_FlushEmitsPartial pins Flush: an unterminated fragment withheld
// from the line stream is published as a single final line (idempotent, no-op
// when nothing is pending).
func TestLogBuffer_FlushEmitsPartial(t *testing.T) {
	b := NewLogBuffer(8)
	b.Write([]byte("one\ntwo"))
	if got, _ := b.ReadNew(0); len(got) != 1 || got[0] != "one" {
		t.Fatalf("ReadNew(0) = %v, want [one]", got)
	}
	b.Flush()
	lines, rev := b.ReadNew(0)
	if len(lines) != 2 || lines[1] != "two" {
		t.Fatalf("ReadNew(after Flush) = %v, want [one two]", lines)
	}
	if rev != 2 {
		t.Fatalf("revision = %d, want 2", rev)
	}
	// Flush with nothing pending is a no-op: no new line, cursor stable.
	b.Flush()
	if got, cur := b.ReadNew(rev); got != nil || cur != 2 {
		t.Fatalf("ReadNew(no-op Flush) = %v,%d, want nil,2", got, cur)
	}
	// A fragment split across writes plus Flush still assembles to one line.
	b.Write([]byte("thr"))
	b.Write([]byte("ee"))
	b.Flush()
	if got, _ := b.ReadNew(rev); len(got) != 1 || got[0] != "three" {
		t.Fatalf("ReadNew(after split+Flush) = %v, want [three]", got)
	}
}

// TestLogRing_Len pins the locked total accessor renderLogs uses: Len reflects
// writes, is bounded by capacity, and is safe to call concurrently with writers
// (exercised under -race).
func TestLogRing_Len(t *testing.T) {
	r := newLogRing(4)
	if got := r.Len(); got != 0 {
		t.Fatalf("empty ring Len = %d, want 0", got)
	}
	r.Write([]byte("a\nb\n"))
	if got := r.Len(); got != 2 {
		t.Fatalf("Len after two lines = %d, want 2", got)
	}
	r.Write([]byte("c\nd\ne\n"))
	if got := r.Len(); got != 4 {
		t.Fatalf("Len past capacity = %d, want 4", got)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			r.Write([]byte("x\n"))
		}()
		go func() {
			defer wg.Done()
			_ = r.Len()
		}()
	}
	wg.Wait()
	if got := r.Len(); got != 4 {
		t.Fatalf("Len after concurrent writes = %d, want 4 (capacity-bounded)", got)
	}
}

func TestLogBuffer_PendingCapForcePublishes(t *testing.T) {
	buf := NewLogBuffer(8)
	payload := strings.Repeat("x", maxPendingLine*2+5)

	if n, err := buf.Write([]byte(payload)); n != len(payload) || err != nil {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(payload))
	}

	lines, rev := buf.ReadNew(0)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 cap-sized force-published chunks", len(lines))
	}
	for i := range lines {
		if len(lines[i]) != maxPendingLine || strings.Trim(lines[i], "x") != "" {
			t.Fatalf("line %d is not a %d-byte chunk of x's (len %d)", i, maxPendingLine, len(lines[i]))
		}
	}
	// The tail below the cap stays retained for the next write or Flush.
	if string(buf.pending) != "xxxxx" {
		t.Fatalf("pending = %q (%d bytes), want the 5-byte retained tail", buf.pending, len(buf.pending))
	}

	buf.Flush()
	lines, _ = buf.ReadNew(rev)
	if len(lines) != 1 || lines[0] != "xxxxx" {
		t.Fatalf("after Flush got %v, want [xxxxx]", lines)
	}
}

func TestLogBuffer_PendingCapCarriesAcrossWrites(t *testing.T) {
	buf := NewLogBuffer(8)
	first := strings.Repeat("a", maxPendingLine-3)
	buf.Write([]byte(first))
	if len(buf.pending) != maxPendingLine-3 {
		t.Fatalf("pending = %d bytes, want the fragment fully retained", len(buf.pending))
	}

	// Crossing the cap mid-fragment force-publishes at the exact cap boundary
	// and retains only the remainder.
	buf.Write([]byte("bcde"))
	lines, rev := buf.ReadNew(0)
	if len(lines) != 1 || len(lines[0]) != maxPendingLine {
		t.Fatalf("got %d lines (first len %d), want one cap-sized chunk", len(lines), len(lines[0]))
	}
	if want := first + "bcd"; lines[0] != want {
		t.Fatal("chunk is not the accumulated fragment truncated at the cap boundary")
	}
	if string(buf.pending) != "e" {
		t.Fatalf("pending = %q, want %q", buf.pending, "e")
	}

	// The remainder keeps assembling normally.
	buf.Write([]byte("f\n"))
	lines, _ = buf.ReadNew(rev)
	if len(lines) != 1 || lines[0] != "ef" {
		t.Fatalf("got %v, want [ef]", lines)
	}
}

func TestLogBuffer_CompleteLineLongerThanCapPublishedWhole(t *testing.T) {
	buf := NewLogBuffer(4)
	line := strings.Repeat("y", maxPendingLine*2+7)
	buf.Write([]byte(line + "\n"))
	lines, _ := buf.ReadNew(0)
	if len(lines) != 1 || lines[0] != line {
		t.Fatalf("complete lines must publish whole regardless of the pending cap (got %d lines)", len(lines))
	}
	if len(buf.pending) != 0 {
		t.Fatalf("pending = %d bytes, want none", len(buf.pending))
	}
}

// TestLogBuffer_RedirectToStreamsThrough pins review-10 #4 / review-11 #2:
// RedirectTo flips the buffer into live passthrough — the torn fragment held at
// flip time is flushed to the target first, subsequent Writes bypass the ring
// entirely, and nothing new lands in the polled buffer.
func TestLogBuffer_RedirectToStreamsThrough(t *testing.T) {
	b := NewLogBuffer(8)
	b.Write([]byte("torn"))

	var sink bytes.Buffer
	b.RedirectTo(&sink)
	if got := sink.String(); got != "torn" {
		t.Fatalf("pending fragment not flushed on redirect: %q, want %q", got, "torn")
	}
	if lines, _ := b.ReadNew(0); lines != nil {
		t.Fatalf("flushed fragment leaked into the ring: %v", lines)
	}

	b.Write([]byte("live\n"))
	if got := sink.String(); got != "tornlive\n" {
		t.Fatalf("post-redirect write did not stream through: %q, want %q", got, "tornlive\n")
	}
	if lines, _ := b.ReadNew(0); lines != nil {
		t.Fatalf("post-redirect write leaked into the ring: %v", lines)
	}
}

// TestLogBuffer_RedirectToNilRestoresBuffering pins that a nil-w redirect
// returns the buffer to ordinary capture.
func TestLogBuffer_RedirectToNilRestoresBuffering(t *testing.T) {
	b := NewLogBuffer(8)
	var sink bytes.Buffer
	b.RedirectTo(&sink)
	b.RedirectTo(nil)

	b.Write([]byte("buffered\n"))
	lines, _ := b.ReadNew(0)
	if len(lines) != 1 || lines[0] != "buffered" {
		t.Fatalf("nil redirect did not restore buffering: ring=%v sink=%q", lines, sink.String())
	}
}

// TestLogBuffer_RedirectToForwardsUnpolledRingLines pins review-14 #1: lines
// published to the ring but never read by a poller are stranded when the TUI
// dies — RedirectTo must hand every retained-but-unpolled line, plus any
// pending fragment, to the target writer before flipping passthrough. Lines a
// poller already delivered must not be re-emitted to stderr.
func TestLogBuffer_RedirectToForwardsUnpolledRingLines(t *testing.T) {
	b := NewLogBuffer(8)
	b.Write([]byte("shown-a\nshown-b\n"))
	lines, rev := b.ReadNew(0)
	if len(lines) != 2 || lines[0] != "shown-a" || lines[1] != "shown-b" {
		t.Fatalf("precondition poll = %v, want [shown-a shown-b]", lines)
	}

	// Written after the final drain: stranded by the old implementation.
	b.Write([]byte("late-c\nlate-d\ntorn"))

	var sink bytes.Buffer
	b.RedirectTo(&sink)
	if got, want := sink.String(), "late-c\nlate-d\ntorn"; got != want {
		t.Fatalf("redirect sink = %q, want %q", got, want)
	}
	// The flipped buffer retains nothing readable: everything left was either
	// delivered to the TUI (never re-emitted) or just handed to the sink.
	if got, _ := b.ReadNew(rev); got != nil {
		t.Fatalf("post-redirect ReadNew = %v, want nil", got)
	}
	// Subsequent writes stream through as before.
	b.Write([]byte("live\n"))
	if got, want := sink.String(), "late-c\nlate-d\ntornlive\n"; got != want {
		t.Fatalf("post-redirect sink = %q, want %q", got, want)
	}
}

// TestLogBuffer_RedirectToRepeatedHandoffExactlyOnce is the sequential mirror
// of TestLogBuffer_RedirectToConcurrentExactlyOneSink (review-14 #1): repeated
// redirects with buffering restored in between must forward each undelivered
// line exactly once — already-polled lines are never re-emitted, and content
// forwarded by an earlier redirect never leaks into a later one.
func TestLogBuffer_RedirectToRepeatedHandoffExactlyOnce(t *testing.T) {
	b := NewLogBuffer(64)
	var s1, s2 bytes.Buffer

	b.Write([]byte("a\nb\n"))
	lines, _ := b.ReadNew(0) // the model drains [a b]
	if len(lines) != 2 {
		t.Fatalf("precondition poll = %v, want two lines", lines)
	}
	b.Write([]byte("c\nd\n"))
	b.RedirectTo(&s1) // gets only c,d — a,b were already delivered

	b.RedirectTo(nil) // capture restored
	b.Write([]byte("e\nf\n"))
	b.RedirectTo(&s2) // gets e,f; c,d must not reappear

	for _, l := range []string{"a\n", "b\n"} {
		if strings.Contains(s1.String(), l) || strings.Contains(s2.String(), l) {
			t.Fatalf("polled line %q re-emitted to a teardown sink", strings.TrimSpace(l))
		}
	}
	if got, want := s1.String(), "c\nd\n"; got != want {
		t.Fatalf("first sink = %q, want %q", got, want)
	}
	if got, want := s2.String(), "e\nf\n"; got != want {
		t.Fatalf("second sink = %q, want %q", got, want)
	}
}

// TestLogBuffer_RedirectToConcurrentExactlyOneSink races writers against a
// toggling redirect: every line must land in exactly one of the two sinks,
// never both and never neither (exercised under -race).
func TestLogBuffer_RedirectToConcurrentExactlyOneSink(t *testing.T) {
	const writers = 8
	const perWriter = 200

	// Ring capacity covers the whole stream: eviction is legitimate ring behavior,
	// not part of this invariant, and would silently swallow buffered lines.
	b := NewLogBuffer(writers * perWriter)
	var sink safeBuffer

	stop := make(chan struct{})
	togglerDone := make(chan struct{})
	go func() {
		defer close(togglerDone)
		on := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			if on {
				b.RedirectTo(nil)
			} else {
				b.RedirectTo(&sink)
			}
			on = !on
		}
	}()

	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range perWriter {
				line := fmt.Sprintf("g%02d-line%03d\n", i, j)
				if _, err := b.Write([]byte(line)); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	<-togglerDone
	b.RedirectTo(nil)

	ring := map[string]int{}
	for _, l := range func() []string { ls, _ := b.ReadNew(0); return ls }() {
		ring[l]++
	}
	sinkLines := strings.SplitSeq(strings.TrimSuffix(sink.String(), "\n"), "\n")
	for l := range sinkLines {
		if l == "" {
			continue
		}
		ring[l]++
	}
	if len(ring) != writers*perWriter {
		t.Fatalf("sinks hold %d distinct lines, want exactly %d", len(ring), writers*perWriter)
	}
	for line, n := range ring {
		if n != 1 {
			t.Fatalf("line %q landed in sinks %d times, want exactly once", line, n)
		}
	}
}
