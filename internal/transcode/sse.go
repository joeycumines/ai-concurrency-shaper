package transcode

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// maxSSELineBytes bounds a single SSE line. Data payloads are typically a few
// hundred bytes; anything longer is treated as malformed and skipped, which
// keeps a misbehaving upstream from exhausting memory with an unbounded line.
const maxSSELineBytes = 1 << 20

// errSSELineOversized is returned by readSSELine when a line exceeds
// maxSSELineBytes. The caller must discard the active frame and drain
// to the next genuine blank line.
var errSSELineOversized = errors.New("SSE line exceeds maximum size")

// readSSEFrame reads the data payload of the next SSE event from r. A frame
// is terminated by a blank line; consecutive data: lines are joined with a
// newline. event:, id:, retry:, comment, and malformed lines are skipped
// without terminating the stream. The final frame at EOF is returned without
// a trailing blank line. Empty events (no data payload) are skipped. Frames
// whose payload exceeds maxSSELineBytes are drained and skipped, keeping a
// misbehaving upstream from exhausting memory with endless data lines.
//
// EOF with a pending frame returns (data, nil); the final frame is never
// dropped.
func readSSEFrame(r *bufio.Reader) ([]byte, error) {
	var data []byte
	for {
		line, err := readSSELine(r)
		if errors.Is(err, errSSELineOversized) {
			// The active frame contained an oversized line: discard it
			// entirely and drain to the next genuine blank line so the
			// following event is parsed intact. Further oversized-line
			// sentinels during the drain are ignored.
			if len(data) > 0 {
				data = nil
			}
			for {
				line, err = readSSELine(r)
				if errors.Is(err, errSSELineOversized) {
					continue
				}
				if line == "" && err != nil {
					if err == io.EOF {
						return nil, io.EOF
					}
					return nil, err
				}
				if line == "" {
					// Genuine blank line: the oversized frame is
					// fully discarded. Resume parsing the next
					// event.
					break
				}
				// Non-empty line within the oversized frame:
				// discard it and keep draining.
			}
			if line == "" {
				continue
			}
		}
		if line == "" && err != nil {
			// EOF (or read error) after the last line: flush the pending frame.
			if err == io.EOF && len(data) > 0 {
				return data, nil
			}
			return nil, err
		}
		if line == "" {
			if len(data) > 0 {
				return data, nil
			}
			continue
		}
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			payload := after
			if len(payload) > 0 && payload[0] == ' ' {
				payload = payload[1:]
			}
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, payload...)
			if len(data) > maxSSELineBytes {
				// Oversized frame: drain it and skip, then resume with the
				// next event.
				for {
					line, err := readSSELine(r)
					if errors.Is(err, errSSELineOversized) {
						continue
					}
					if line == "" && err != nil {
						if err == io.EOF {
							return nil, io.EOF
						}
						return nil, err
					}
					if line == "" {
						break
					}
				}
				data = nil
			}
		}
		// All other lines are ignored.
	}
}

// readSSELine reads one line, trimming the trailing CR/LF, without
// growing past maxSSELineBytes: oversized lines are consumed and
// reported as errSSELineOversized so the frame parser can discard
// the active frame and drain to the next genuine blank line.
func readSSELine(r *bufio.Reader) (string, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		// Check the bound BEFORE appending so the buffer never
		// exceeds maxSSELineBytes + a small overshoot from the
		// reader's internal buffer.
		if len(line)+len(frag) > maxSSELineBytes {
			// Consume the rest of the oversized line, then report
			// the sentinel error.
			for err == bufio.ErrBufferFull {
				_, err = r.ReadSlice('\n')
			}
			return "", errSSELineOversized
		}
		line = append(line, frag...)
		if err != bufio.ErrBufferFull {
			if err == io.EOF && len(line) == 0 {
				return "", io.EOF
			}
			return strings.TrimRight(string(line), "\r\n"), err
		}
	}
}
