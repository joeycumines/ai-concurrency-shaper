package transcode

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"sync"
)

// Required copy primitive and lifecycle contract:
// https://github.com/joeycumines/sesame/blob/v0.1.1/stream/proxy.go
//
// SSE contracts:
// https://platform.claude.com/docs/en/build-with-claude/streaming
// https://platform.openai.com/docs/api-reference/responses-streaming

// SSEReader parses one SSE event at a time from an upstream stream, applying
// the line and frame bounds from the parser.
type SSEReader struct {
	br *bufio.Reader
}

// NewSSEReader wraps r in a bounded SSE event reader.
func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{br: bufio.NewReader(r)}
}

// Next returns the next SSE event, or io.EOF at the end of the stream.
func (r *SSEReader) Next() (SSEEvent, error) {
	return readSSEEvent(r.br)
}

// convertedBatch is one conversion output: the frames to write downstream and
// whether the source stream reached its terminal condition.
type convertedBatch struct {
	Events   []frameEvent
	Terminal bool
}

// frameConverter converts one upstream SSE event into downstream frames.
// FinalizeEOF is called when the upstream stream ends and must either emit a
// legitimate held terminal event or return a truncation/conversion error. It
// must never fabricate success when the source stream ended before a terminal
// condition.
type frameConverter interface {
	Convert(frame SSEEvent) (convertedBatch, error)

	FinalizeEOF() (convertedBatch, error)

	// ErrorEvent returns the client-dialect error event frame for a
	// conversion error, or ok=false when no error event can be emitted.
	// When the stream has already started with 200, the convertingReader
	// appends this frame before surfacing the error so the client receives
	// an explicit error terminal rather than a silent clean EOF.
	ErrorEvent(err error) (frameEvent, bool)
}

// convertingReader adapts a frameConverter into an io.Reader that emits
// downstream SSE frames. It provides the terminal-state semantics the handler
// relies on:
//
//   - a converter-declared terminal batch drains completely, then the reader
//     returns io.EOF immediately (stopAfterDrain) even when the upstream
//     keeps the connection open after [DONE];
//   - malformed upstream frames are skipped only while the remaining stream
//     can still produce a valid lifecycle; a conversion error is surfaced
//     through Read, never silently turned into success;
//   - Err, SawTerminal, and SawErrorEvent expose the conversion state to the
//     outcome classifier.
type convertingReader struct {
	mu sync.Mutex

	source *SSEReader
	conv   frameConverter
	buf    bytes.Buffer

	stopAfterDrain bool
	err            error
	sawTerminal    bool
	sawErrorEvent  bool

	// sawUpstreamErrorFrame is set when the error frame came from real
	// upstream data (converted), not from a local truncation/conversion
	// error. Genuine upstream error frames are definitive upstream outcomes
	// even when the client cancels concurrently.
	sawUpstreamErrorFrame bool
}

// newConvertingReader wraps the source reader and converter.
func newConvertingReader(source *SSEReader, conv frameConverter) *convertingReader {
	return &convertingReader{source: source, conv: conv}
}

// Read returns the next chunk of downstream SSE bytes.
func (r *convertingReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for r.buf.Len() == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.stopAfterDrain {
			r.err = io.EOF
			return 0, io.EOF
		}

		sourceEvent, err := r.source.Next()
		if err != nil && !errors.Is(err, io.EOF) {
			r.appendErrorEvent(err)
			r.err = err
			// Drain the appended error frame before surfacing the error.
			continue
		}

		// The parser delivers a pending frame at end of stream together with
		// io.EOF (data+EOF convention). Convert it before finalizing so the
		// final upstream event is never dropped.
		if sourceEvent.Data != nil {
			batch, err := r.conv.Convert(sourceEvent)
			if err != nil {
				r.appendErrorEvent(err)
				r.err = err
				// Drain the appended error frame before surfacing the error.
				continue
			}
			r.appendBatch(batch)
			if batch.Terminal {
				r.stopAfterDrain = true
			}
		}

		if errors.Is(err, io.EOF) {
			batch, finalErr := r.conv.FinalizeEOF()
			if finalErr != nil {
				r.appendErrorEvent(finalErr)
				r.err = finalErr
				// Drain the appended error frame before surfacing the error.
				continue
			}
			r.appendBatch(batch)
			r.stopAfterDrain = true
			continue
		}
	}

	return r.buf.Read(p)
}

// appendErrorEvent appends the converter's client-dialect error event frame
// so a failed stream terminates explicitly instead of a silent clean EOF.
func (r *convertingReader) appendErrorEvent(err error) {
	frame, ok := r.conv.ErrorEvent(err)
	if !ok {
		return
	}
	writeFrameBytes(&r.buf, frame)
	r.sawErrorEvent = true
}

// appendBatch buffers the converted frames and records the terminal/error
// state.
func (r *convertingReader) appendBatch(batch convertedBatch) {
	for _, event := range batch.Events {
		writeFrameBytes(&r.buf, event)
		if event.Type == "error" {
			r.sawErrorEvent = true
			// A frame emitted by the converter is real upstream data; a
			// locally generated truncation error event is not.
			r.sawUpstreamErrorFrame = true
		}
	}
	if batch.Terminal {
		r.sawTerminal = true
	}
}

// Err returns the first conversion or read error, or io.EOF once the stream
// has terminated cleanly.
func (r *convertingReader) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// SawTerminal reports whether a success or failure terminal condition was
// reached.
func (r *convertingReader) SawTerminal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sawTerminal
}

// SawErrorEvent reports whether an error event was emitted downstream.
func (r *convertingReader) SawErrorEvent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sawErrorEvent
}

// SawUpstreamErrorFrame reports whether the error event originated from real
// upstream data rather than a local truncation or conversion error.
func (r *convertingReader) SawUpstreamErrorFrame() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sawUpstreamErrorFrame
}
