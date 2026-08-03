package transcode

import (
	"bufio"
	"io"
	"strings"
)

// maxSSELineBytes bounds a single SSE line. Data payloads are typically a few
// hundred bytes; anything longer is treated as malformed and skipped, which
// keeps a misbehaving upstream from exhausting memory with an unbounded line.
const maxSSELineBytes = 1 << 20

// readSSEFrame reads the data payload of the next SSE event from r. A frame
// is terminated by a blank line; consecutive data: lines are joined with a
// newline. event:, id:, retry:, comment, and malformed lines are skipped
// without terminating the stream. The final frame at EOF is returned without
// a trailing blank line. Empty events (no data payload) are skipped. Frames
// whose payload exceeds maxSSELineBytes are drained and skipped, keeping a
// misbehaving upstream from exhausting memory with endless data lines.
func readSSEFrame(r *bufio.Reader) ([]byte, error) {
	var data []byte
	for {
		line, err := readSSELine(r)
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
			payload = strings.TrimPrefix(payload, " ")
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, payload...)
			if len(data) > maxSSELineBytes {
				// Oversized frame: drain it and skip, then resume with the
				// next event.
				for {
					line, err := readSSELine(r)
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

// readSSELine reads one line, trimming the trailing CR/LF, without growing
// past maxSSELineBytes: oversized lines are consumed and reported as empty so
// the frame parser skips them without buffering the payload.
func readSSELine(r *bufio.Reader) (string, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		line = append(line, frag...)
		if err != bufio.ErrBufferFull {
			if err == io.EOF && len(line) == 0 {
				return "", io.EOF
			}
			return strings.TrimRight(string(line), "\r\n"), err
		}
		if len(line) > maxSSELineBytes {
			// Consume the rest of the oversized line, then report it as
			// malformed (empty).
			for err == bufio.ErrBufferFull {
				_, err = r.ReadSlice('\n')
			}
			return "", nil
		}
	}
}
