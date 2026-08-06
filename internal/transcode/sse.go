package transcode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// SSE processing model:
// https://html.spec.whatwg.org/multipage/server-sent-events.html
// https://platform.claude.com/docs/en/build-with-claude/streaming

// maxSSELineBytes bounds a single SSE line. Data payloads are typically a few
// hundred bytes; anything longer is treated as malformed and skipped, which
// keeps a misbehaving upstream from exhausting memory with an unbounded line.
const maxSSELineBytes = 1 << 20

// maxSSEFrameBytes bounds the total data payload of one SSE event.
const maxSSEFrameBytes = 1 << 20

// errSSELineOversized is returned by readSSELine when a line exceeds
// maxSSELineBytes. The caller must discard the active frame and drain
// to the next genuine blank line.
var errSSELineOversized = errors.New("SSE line exceeds maximum size")

// SSEEvent is one parsed SSE event: the event name (empty when not
// specified) and the joined data payload.
type SSEEvent struct {
	Event string
	Data  []byte
}

// readSSEEvent reads the next SSE event from r with the package default
// bounds.
func readSSEEvent(r *bufio.Reader) (SSEEvent, error) {
	return readSSEEventLimited(r, maxSSELineBytes, maxSSEFrameBytes)
}

// readSSEEventLimited reads the next SSE event from r. A frame is terminated
// by a blank line; consecutive data: lines are joined with a newline. The
// event name, id:, retry:, and comment lines are captured but do not
// terminate the stream. Malformed frames are drained and skipped. EOF with a
// pending frame returns that frame together with io.EOF (the data+EOF
// convention), so a successful read always consumes input.
//
// Bounds: every line is capped at lineMax and every frame payload at
// frameMax; exceeding either discards the active frame and drains to the
// next blank line.
func readSSEEventLimited(r *bufio.Reader, lineMax, frameMax int) (SSEEvent, error) {
	var event SSEEvent
	var data []byte
	for {
		line, err := readSSELine(r, lineMax)
		if errors.Is(err, errSSELineOversized) {
			// The active frame contained an oversized line: discard it
			// entirely and drain to the next genuine blank line so the
			// following event is parsed intact.
			if len(data) > 0 {
				data = nil
			}
			drained, drainErr := drainOversizedFrame(r, lineMax)
			if drainErr != nil {
				return SSEEvent{}, drainErr
			}
			if drained {
				continue
			}
		}
		if line == "" && err != nil {
			// EOF (or read error) after the last line: flush the pending
			// frame together with the error, never as a no-progress nil
			// return.
			if errors.Is(err, io.EOF) {
				if len(data) > 0 {
					event.Data = data
					return event, io.EOF
				}
				return SSEEvent{}, io.EOF
			}
			return SSEEvent{}, err
		}
		if line == "" {
			if len(data) > 0 {
				event.Data = data
				return event, nil
			}
			continue
		}

		switch {
		case strings.HasPrefix(line, "event:"):
			event.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			if len(event.Event) > lineMax {
				return SSEEvent{}, errSSELineOversized
			}

		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, payload...)
			if len(data) > frameMax {
				// Oversized frame: drain it and skip, then resume with the
				// next event. The drain consumes through the terminating
				// blank line (or to EOF); the loop continues reading the
				// following frame.
				_, drainErr := drainOversizedFrame(r, lineMax)
				if drainErr != nil {
					return SSEEvent{}, drainErr
				}
				data = nil
				continue
			}

		default:
			// id:, retry:, comments, and malformed lines are ignored.
		}
	}
}

// drainOversizedFrame consumes the remainder of an oversized line or frame up
// to the next genuine blank line. It reports whether a blank line was found
// (true) or the stream ended (false, with io.EOF or an error).
func drainOversizedFrame(r *bufio.Reader, lineMax int) (bool, error) {
	for {
		line, err := readSSELine(r, lineMax)
		if errors.Is(err, errSSELineOversized) {
			continue
		}
		if line == "" && err != nil {
			if errors.Is(err, io.EOF) {
				return false, io.EOF
			}
			return false, err
		}
		if line == "" {
			return true, nil
		}
	}
}

// readSSELine reads one line, trimming the trailing CR/LF, without growing
// past maxSSELineBytes: oversized lines are consumed and reported as
// errSSELineOversized so the frame parser can discard the active frame and
// drain to the next genuine blank line.
func readSSELine(r *bufio.Reader, lineMax int) (string, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		// Check the bound BEFORE appending so the buffer never exceeds
		// maxSSELineBytes + a small overshoot from the reader's internal
		// buffer.
		if len(line)+len(frag) > lineMax {
			// Consume the rest of the oversized line, then report the
			// sentinel error.
			for err == bufio.ErrBufferFull {
				_, err = r.ReadSlice('\n')
			}
			return "", errSSELineOversized
		}
		line = append(line, frag...)
		if err != bufio.ErrBufferFull {
			if errors.Is(err, io.EOF) && len(line) == 0 {
				return "", io.EOF
			}
			return strings.TrimRight(string(line), "\r\n"), err
		}
	}
}

// validateEventNameMatchesJSONType verifies that the SSE event name equals the
// JSON "type" tag of the data payload. Responses streams require this
// equality; Anthropic streams carry a type tag without an event name.
func validateEventNameMatchesJSONType(event SSEEvent) error {
	if event.Event == "" {
		return nil
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(event.Data, &probe); err != nil {
		return fmt.Errorf("SSE data is not JSON: %w", err)
	}
	if event.Event != probe.Type {
		return fmt.Errorf(
			"SSE event name %q does not match JSON type %q",
			event.Event,
			probe.Type,
		)
	}
	return nil
}

// frameEvent is one converted SSE frame ready for downstream writing. Type is
// the SSE event name (and JSON type tag); Data is the JSON payload.
type frameEvent struct {
	Type string
	Data []byte
}

// writeFrameBytes writes one complete SSE frame ("event: ...\ndata: ...\n\n")
// into buf. The frame is written in a single buffer so downstream writers
// flush exactly one complete event per Write call.
func writeFrameBytes(buf *bytes.Buffer, event frameEvent) {
	if event.Type != "" {
		_, _ = buf.WriteString("event: " + event.Type + "\n")
	}
	_, _ = buf.WriteString("data: ")
	_, _ = buf.Write(event.Data)
	_, _ = buf.WriteString("\n\n")
}
