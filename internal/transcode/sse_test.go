package transcode

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func readAllEvents(t *testing.T, input string) ([]SSEEvent, error) {
	t.Helper()
	reader := bufio.NewReader(strings.NewReader(input))
	var events []SSEEvent
	for {
		event, err := readSSEEvent(reader)
		if err != nil && !errors.Is(err, io.EOF) {
			return events, err
		}
		// The parser delivers a pending frame at end of stream together
		// with io.EOF; consume it before honoring EOF.
		if event.Data != nil {
			events = append(events, event)
		}
		if errors.Is(err, io.EOF) {
			return events, nil
		}
	}
}

func TestReadSSEEventBasic(t *testing.T) {
	input := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"
	events, err := readAllEvents(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].Event != "response.created" {
		t.Fatalf("event name = %q", events[0].Event)
	}
	if string(events[0].Data) != `{"type":"response.created"}` {
		t.Fatalf("data = %q", events[0].Data)
	}
}

func TestReadSSEEventDataLinesJoined(t *testing.T) {
	input := "data: line1\ndata: line2\n\n"
	events, err := readAllEvents(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if string(events[0].Data) != "line1\nline2" {
		t.Fatalf("data = %q", events[0].Data)
	}
	if events[0].Event != "" {
		t.Fatalf("event = %q, want empty", events[0].Event)
	}
}

func TestReadSSEEventMultiFrame(t *testing.T) {
	input := "event: a\ndata: 1\n\nevent: b\ndata: 2\n\n"
	events, err := readAllEvents(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].Event != "a" || string(events[0].Data) != "1" {
		t.Fatalf("event 0 = %+v", events[0])
	}
	if events[1].Event != "b" || string(events[1].Data) != "2" {
		t.Fatalf("event 1 = %+v", events[1])
	}
}

func TestReadSSEEventCommentsAndMeta(t *testing.T) {
	input := ": comment\nretry: 1000\nid: x\ndata: {}\n\n"
	events, err := readAllEvents(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if string(events[0].Data) != "{}" {
		t.Fatalf("data = %q", events[0].Data)
	}
}

func TestReadSSEEventCRLF(t *testing.T) {
	input := "event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"
	events, err := readAllEvents(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event != "message_stop" {
		t.Fatalf("events = %+v", events)
	}
}

func TestReadSSEEventEmptyDataFrame(t *testing.T) {
	// Frames without data payloads are skipped.
	input := "event: ping\n\n"
	events, err := readAllEvents(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %d", len(events))
	}
}

func TestReadSSEEventEOFWithPendingFrame(t *testing.T) {
	input := "data: {\"x\":1}"
	events, err := readAllEvents(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || string(events[0].Data) != `{"x":1}` {
		t.Fatalf("events = %+v", events)
	}
}

func TestReadSSEEventOversizedLineSkipped(t *testing.T) {
	// A line exceeding maxSSELineBytes drains the frame; the following valid
	// frame still parses.
	var builder strings.Builder
	builder.WriteString("data: ")
	builder.WriteString(strings.Repeat("x", maxSSELineBytes))
	builder.WriteString("\ndata: extra\n\ndata: ok\n\n")
	events, err := readAllEvents(t, builder.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || string(events[0].Data) != "ok" {
		t.Fatalf("events = %+v", events)
	}
}

func TestValidateEventNameMatchesJSONType(t *testing.T) {
	event := SSEEvent{
		Event: "response.completed",
		Data:  []byte(`{"type":"response.completed"}`),
	}
	if err := validateEventNameMatchesJSONType(event); err != nil {
		t.Fatal(err)
	}
	event = SSEEvent{
		Event: "response.completed",
		Data:  []byte(`{"type":"response.failed"}`),
	}
	if err := validateEventNameMatchesJSONType(event); err == nil {
		t.Fatal("expected name/type mismatch")
	}
	// Empty event name is fine (Anthropic streams).
	event = SSEEvent{Data: []byte(`{"type":"message_stop"}`)}
	if err := validateEventNameMatchesJSONType(event); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFrameBytes(t *testing.T) {
	var buf bytes.Buffer
	writeFrameBytes(&buf, frameEvent{Type: "response.created", Data: []byte(`{}`)})
	want := "event: response.created\ndata: {}\n\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
	buf.Reset()
	writeFrameBytes(&buf, frameEvent{Data: []byte(`{"x":1}`)})
	if buf.String() != "data: {\"x\":1}\n\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestFixedChunkReaderBoundary(t *testing.T) {
	input := "data: abc\ndata: def\n\ndata: ghi\n\n"
	source := &fixedChunkReader{data: []byte(input), chunkSize: 3}
	reader := bufio.NewReaderSize(source, 7)
	var events []SSEEvent
	for {
		before := source.consumed
		event, err := readSSEEvent(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if source.consumed == before {
			t.Fatal("no progress")
		}
		events = append(events, event)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d", len(events))
	}
	if string(events[0].Data) != "abc\ndef" || string(events[1].Data) != "ghi" {
		t.Fatalf("events = %+v", events)
	}
}

// fixedChunkReader serves data in fixed-size chunks to exercise parser
// boundary handling. It mirrors the fuzz harness's chunked reader.
type fixedChunkReader struct {
	data      []byte
	chunkSize int
	consumed  int
}

func (r *fixedChunkReader) Read(p []byte) (int, error) {
	if r.consumed >= len(r.data) {
		return 0, io.EOF
	}
	n := r.chunkSize
	if n > len(p) {
		n = len(p)
	}
	if remaining := len(r.data) - r.consumed; n > remaining {
		n = remaining
	}
	copy(p[:n], r.data[r.consumed:r.consumed+n])
	r.consumed += n
	return n, nil
}
