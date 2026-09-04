package transcode

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

var (
	errStreamWriterSealed = errors.New("transcode stream writer is sealed")
	errTrailingSSEBytes   = errors.New("transcode stream ended with a partial SSE event")
)

// sealedSSEWriter provides two guarantees:
//
//  1. every downstream Flush follows one complete "\n\n"-terminated SSE event;
//  2. after Seal returns, no goroutine can call the underlying ResponseWriter.
//
// io.Copy is allowed to split or combine reads arbitrarily, so flushing on
// every Write call does not imply one flush per SSE event.
type sealedSSEWriter struct {
	stopping atomic.Bool

	mu       sync.Mutex
	dst      http.ResponseWriter
	pending  bytes.Buffer
	firstErr error
}

func newSealedSSEWriter(dst http.ResponseWriter) *sealedSSEWriter {
	return &sealedSSEWriter{dst: dst}
}

// Write buffers the bytes and flushes complete "\n\n"-terminated SSE events.
func (w *sealedSSEWriter) Write(p []byte) (int, error) {
	if w.stopping.Load() {
		return 0, errStreamWriterSealed
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Recheck after acquiring the mutex. A writer may have been waiting while
	// shutdown began.
	if w.stopping.Load() {
		return 0, errStreamWriterSealed
	}
	if w.firstErr != nil {
		return 0, w.firstErr
	}

	_, _ = w.pending.Write(p)

	for {
		data := w.pending.Bytes()
		end := bytes.Index(data, []byte("\n\n"))
		if end < 0 {
			break
		}
		end += 2

		frame := append([]byte(nil), data[:end]...)
		w.pending.Next(end)

		if err := writeAll(w.dst, frame); err != nil {
			w.firstErr = err
			return len(p), err
		}
		if err := flushResponse(w.dst); err != nil {
			w.firstErr = err
			return len(p), err
		}
	}

	return len(p), nil
}

// StopAccepting is non-blocking and prevents every Write that has not already
// entered its underlying ResponseWriter operation.
func (w *sealedSSEWriter) StopAccepting() {
	w.stopping.Store(true)
}

// Seal waits for an already-entered Write/Flush to finish and then guarantees
// that the underlying writer will never be touched again.
func (w *sealedSSEWriter) Seal() error {
	w.stopping.Store(true)

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.firstErr != nil {
		return w.firstErr
	}
	if w.pending.Len() != 0 {
		w.pending.Reset()
		return errTrailingSSEBytes
	}
	return nil
}

// writeAll writes p fully to w with a single Write. A partial write with a
// nil error is io.ErrShortWrite: repeated partial writes must never be
// retried into a false success, because a caller that sees nil records the
// write as complete (review-08 blocker 11).
func writeAll(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// flushResponse flushes w unless the ResponseWriter does not support flushing.
func flushResponse(w http.ResponseWriter) error {
	err := http.NewResponseController(w).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}
