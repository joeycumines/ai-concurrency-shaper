package transcode

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

// Go fuzzing guidance:
// https://go.dev/doc/security/fuzz/
//
// SSE processing model:
// https://html.spec.whatwg.org/multipage/server-sent-events.html
// https://platform.claude.com/docs/en/build-with-claude/streaming

func FuzzReadSSEEvent(f *testing.F) {
	seeds := [][]byte{
		[]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"),
		[]byte("event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"),
		[]byte("data: line1\ndata: line2\n\n"),
		[]byte(": comment\nretry: 1000\nid: x\ndata: {}\n\n"),
		[]byte("data: [DONE]\n\n"),
		[]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"x\"}}\n"),
		[]byte("garbage\ndata: {}\n\n"),
	}

	for _, seed := range seeds {
		f.Add(seed, uint8(7))
	}

	f.Fuzz(func(t *testing.T, raw []byte, chunkSize uint8) {
		const maxInput = 2 << 20
		if len(raw) > maxInput {
			t.Skip()
		}

		size := int(chunkSize%64) + 1
		source := &fixedChunkReader{
			data:      append([]byte(nil), raw...),
			chunkSize: size,
		}
		reader := bufio.NewReaderSize(source, 128)

		var (
			events       int
			lastConsumed int
		)

		for events < 4096 {
			before := source.consumed
			event, err := readSSEEvent(reader)
			after := source.consumed

			// The parser must never return a nil error without producing an
			// event and without consuming input (an infinite loop). A nil
			// error WITH an event may consume nothing new: bufio reads ahead,
			// so a large source chunk can satisfy several events from the
			// buffer after a single underlying fill.
			if err == nil && after == before && len(event.Data) == 0 && event.Event == "" {
				t.Fatalf(
					"parser made no progress: consumed=%d event=%+v",
					after,
					event,
				)
			}
			if after < lastConsumed {
				t.Fatalf(
					"consumption moved backwards: %d -> %d",
					lastConsumed,
					after,
				)
			}
			lastConsumed = after

			if len(event.Event) > maxSSELineBytes {
				t.Fatalf("event name exceeded bound: %d", len(event.Event))
			}
			if len(event.Data) > maxSSEFrameBytes {
				t.Fatalf("event data exceeded bound: %d", len(event.Data))
			}

			switch {
			case errors.Is(err, io.EOF):
				return
			case err != nil:
				// Defined parser errors are acceptable. A panic, hang, or
				// unbounded result is not.
				return
			default:
				events++
			}
		}

		t.Fatalf("parser emitted too many events without termination")
	})
}

func TestSSEEventNameMatchesJSONType(t *testing.T) {
	input := []byte(
		"event: response.completed\n" +
			"data: {\"type\":\"response.failed\"}\n\n",
	)

	event, err := readSSEEvent(
		bufio.NewReader(bytes.NewReader(input)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEventNameMatchesJSONType(event); err == nil {
		t.Fatal("expected event/type mismatch")
	}
}
