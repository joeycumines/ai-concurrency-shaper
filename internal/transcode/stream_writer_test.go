package transcode

// Review-08 blocker 11 regression tests: writeAll is single-shot — a partial
// write with a nil error is io.ErrShortWrite, never retried into a false
// success, so a short write can never be recorded as a clean completion.

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// partialWriter returns one byte per Write with a nil error, violating the
// io.Writer contract in exactly the way the review describes. It implements
// http.ResponseWriter so the dialect error path can be exercised.
type partialWriter struct {
	header http.Header
}

func (w *partialWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *partialWriter) WriteHeader(int) {}

func (w *partialWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return 1, nil
}

// TestWriteAllRejectsShortWrite proves writeAll never turns repeated partial
// writes into success: a (1, nil)-per-call writer yields io.ErrShortWrite,
// and a compliant writer succeeds (review-08 blocker 11).
func TestWriteAllRejectsShortWrite(t *testing.T) {
	if err := writeAll(&partialWriter{}, []byte("hello")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err = %v, want io.ErrShortWrite", err)
	}
	if err := writeAll(io.Discard, []byte("hello")); err != nil {
		t.Fatalf("full write failed: %v", err)
	}
}

// TestWriteDialectHTTPErrorShortWriteNotClean proves a short write through
// WriteDialectHTTPError records DownstreamComplete=false: the translated
// error was never fully delivered (review-08 blocker 11).
func TestWriteDialectHTTPErrorShortWriteNotClean(t *testing.T) {
	handler, outcomes := outcomeCaptureHandler(t, responsesMapping(t), func(req *http.Request) (*http.Response, error) {
		t.Fatal("round trip must not be called")
		return nil, nil
	})
	apiErr := CanonicalAPIError{
		Status:  http.StatusBadGateway,
		Type:    "api_error",
		Code:    "response_conversion_error",
		Message: "boom",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	handler.writeDialectHTTPError(req, &partialWriter{}, apiErr, ProvenanceLocalResponseConversionError)
	outcome := <-outcomes
	if outcome.DownstreamComplete {
		t.Fatal("short write recorded as a clean completion")
	}
	if outcome.Provenance != ProvenanceDownstreamWriteError {
		t.Fatalf("provenance = %v, want downstream_write_error", outcome.Provenance)
	}
}
