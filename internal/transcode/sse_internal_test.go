package transcode

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestReadSSEFrameOversizedLine verifies that a data line beyond the size cap
// is skipped without buffering it (anti-OOM) and without dropping the rest of
// the stream.
func TestReadSSEFrameOversizedLine(t *testing.T) {
	big := "data: " + strings.Repeat("x", maxSSELineBytes+4096)
	stream := big + "\n\ndata: {\"ok\":true}\n\n"
	br := bufio.NewReader(strings.NewReader(stream))
	var frames [][]byte
	for {
		frame, err := readSSEFrame(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		frames = append(frames, frame)
	}
	if len(frames) != 1 || string(frames[0]) != `{"ok":true}` {
		t.Errorf("frames = %q, want only the small frame", frames)
	}
}

// TestReadSSELineOversized verifies the line reader bounds a single line
// and returns the errSSELineOversized sentinel so callers can distinguish
// oversized lines from real I/O failures.
func TestReadSSELineOversized(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(strings.Repeat("y", maxSSELineBytes+8192) + "\nrest\n"))
	line, err := readSSELine(br)
	if !errors.Is(err, errSSELineOversized) || line != "" {
		t.Errorf("oversized line = %q, err = %v, want empty with errSSELineOversized", line, err)
	}
	line, err = readSSELine(br)
	if err != nil || line != "rest" {
		t.Errorf("following line = %q, err = %v, want rest", line, err)
	}
}

// TestReadSSEFrameOversizedFrame verifies a frame assembled from many short
// data lines beyond the size cap is drained and skipped without exhausting
// memory, and the following event still parses.
func TestReadSSEFrameOversizedFrame(t *testing.T) {
	var b strings.Builder
	// ~2 MiB of short data lines without a blank line: one oversized frame.
	for range maxSSELineBytes/16 + 1 {
		b.WriteString("data: xxxxxxxxxxxxxxx\n")
	}
	b.WriteString("\n")
	b.WriteString("data: {\"ok\":true}\n\n")

	br := bufio.NewReader(strings.NewReader(b.String()))
	frame, err := readSSEFrame(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if string(frame) != `{"ok":true}` {
		t.Errorf("frame = %q, want the small frame", frame)
	}
	_, err = readSSEFrame(br)
	if err != io.EOF {
		t.Errorf("after frame: err = %v, want EOF", err)
	}
}

// TestReadSSELineOversizedSentinel verifies that readSSELine returns the
// errSSELineOversized sentinel for lines exceeding the cap, not a generic
// error, so callers can distinguish oversized lines from real I/O failures.
func TestReadSSELineOversizedSentinel(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(strings.Repeat("z", maxSSELineBytes+1) + "\n"))
	_, err := readSSELine(br)
	if !errors.Is(err, errSSELineOversized) {
		t.Errorf("expected errSSELineOversized, got %v", err)
	}
}

// TestReadSSELinePreAppendBound verifies that the pre-append bound check in
// readSSELine catches an oversized line before the append, so the buffer
// never exceeds maxSSELineBytes + a small reader overshoot even when the
// reader supplies a fragment that pushes the accumulated line past the cap.
func TestReadSSELinePreAppendBound(t *testing.T) {
	// Build a line that is just under the cap, then append a fragment that
	// pushes it over. The pre-append check must catch this before the append.
	linePrefix := strings.Repeat("a", maxSSELineBytes-10)
	stream := linePrefix + "xxxxxxxxxxx\nnext\n"
	br := bufio.NewReader(strings.NewReader(stream))
	line, err := readSSELine(br)
	if !errors.Is(err, errSSELineOversized) || line != "" {
		t.Errorf("oversized line = %q, err = %v, want empty with errSSELineOversized", line, err)
	}
	// The next line must still be readable.
	line, err = readSSELine(br)
	if err != nil || line != "next" {
		t.Errorf("following line = %q, err = %v, want next", line, err)
	}
}

// TestReadSSEFrameOversizedData verifies that a frame whose accumulated
// data payload exceeds maxSSELineBytes (many valid data lines summing past
// the cap) is drained and skipped, and the next event is still parsed.
func TestReadSSEFrameOversizedData(t *testing.T) {
	var b strings.Builder
	// Enough 16-byte payload lines to exceed the cap.
	for range maxSSELineBytes/16 + 1 {
		b.WriteString("data: xxxxxxxxxxxxxxxx\n")
	}
	b.WriteString("\n")
	b.WriteString("data: {\"ok\":true}\n\n")

	br := bufio.NewReader(strings.NewReader(b.String()))
	frame, err := readSSEFrame(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if string(frame) != `{"ok":true}` {
		t.Errorf("frame = %q, want the small frame after oversized drain", frame)
	}
	_, err = readSSEFrame(br)
	if err != io.EOF {
		t.Errorf("after frame: err = %v, want EOF", err)
	}
}
