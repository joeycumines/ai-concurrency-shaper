package transcode

// J7 regression tests (review-k finding 7, medium): a recorder-detected
// short write can never be a clean completion.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// shortWriteResponseWriter violates the io.Writer contract by returning
// (n < len(b), nil) from Write.
type shortWriteResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *shortWriteResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *shortWriteResponseWriter) WriteHeader(status int) { w.status = status }

func (w *shortWriteResponseWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	n := len(b) - 1
	w.body.Write(b[:n])
	return n, nil
}

// TestHandlerShortWriteNotCleanCompletion proves jsonResponse treats a short
// write with a nil error as io.ErrShortWrite: the recorded outcome is a
// downstream write error and never a clean completion (review-k finding 7).
func TestHandlerShortWriteNotCleanCompletion(t *testing.T) {
	mapping := responsesMapping(t)
	var outcomes []Outcome
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
			ModelMap:   ModelMap{AllowIdentity: true},
			LossPolicy: StrictLossPolicy(),
			AuthPolicy: AuthPolicy{Mode: AuthNone},
		},
		func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`,
				)),
			}, nil
		},
		func(o Outcome) { outcomes = append(outcomes, o) },
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	writer := &shortWriteResponseWriter{}
	handler.ServeHTTP(writer, req)
	if writer.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", writer.status)
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d, want 1", len(outcomes))
	}
	if outcomes[0].DownstreamComplete {
		t.Fatal("short write recorded as a clean completion")
	}
	if outcomes[0].Provenance != ProvenanceDownstreamWriteError {
		t.Fatalf("provenance = %s, want downstream_write_error", outcomes[0].Provenance)
	}
}
