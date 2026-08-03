package transcode

import (
	"bufio"
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

// TestReadSSELineOversized verifies the line reader bounds a single line.
func TestReadSSELineOversized(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(strings.Repeat("y", maxSSELineBytes+8192) + "\nrest\n"))
	line, err := readSSELine(br)
	if err != nil || line != "" {
		t.Errorf("oversized line = %q, err = %v, want empty", line, err)
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
