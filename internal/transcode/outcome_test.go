package transcode

// J2 regression tests for the extended Outcome dimensions (review-j finding
// 2): a failed downstream write changes the recorded provenance instead of
// being logged and ignored, and writeUpstreamHTTPError records RetryAfter
// from the ORIGINAL upstream response so rate-signalled 403s keep their hold
// signal.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// failingResponseWriter fails every Write, simulating a disconnected
// downstream client.
type failingResponseWriter struct {
	header http.Header
	status int
}

func (w *failingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingResponseWriter) WriteHeader(status int) { w.status = status }

func (w *failingResponseWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// outcomeCaptureHandler builds a handler whose outcome hook records into the
// returned channel.
func outcomeCaptureHandler(t *testing.T, mapping Mapping, roundTrip RoundTrip) (*TranscodeHandler, chan Outcome) {
	t.Helper()
	outcomes := make(chan Outcome, 1)
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
			ModelMap:           ModelMap{AllowIdentity: true},
			LossPolicy:         StrictLossPolicy(),
			AuthPolicy:         AuthPolicy{Mode: AuthNone},
			ChatCapabilities:   ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true},
			AllowedClientQuery: map[string]struct{}{},
		},
		roundTrip,
		func(o Outcome) { outcomes <- o },
	)
	return handler, outcomes
}

// TestWriteDialectHTTPErrorDownstreamWriteFailureChangesOutcome proves that a
// failed downstream write changes the recorded provenance to
// DownstreamWriteError (or ClientAbort when the context is cancelled) with
// DownstreamComplete=false, instead of being logged and ignored (review-j
// finding 2).
func TestWriteDialectHTTPErrorDownstreamWriteFailureChangesOutcome(t *testing.T) {
	handler, outcomes := outcomeCaptureHandler(t, responsesMapping(t), func(req *http.Request) (*http.Response, error) {
		t.Fatal("round trip must not be called")
		return nil, nil
	})

	apiErr := CanonicalAPIError{
		Status:  http.StatusBadGateway,
		Type:    "api_error",
		Code:    "response_conversion_error",
		Message: "local conversion failure",
	}

	// Without a cancelled context: a downstream write error.
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	handler.writeDialectHTTPError(req, &failingResponseWriter{}, apiErr, ProvenanceLocalResponseConversionError)
	outcome := <-outcomes
	if outcome.Provenance != ProvenanceDownstreamWriteError {
		t.Fatalf("provenance = %v, want downstream_write_error", outcome.Provenance)
	}
	if outcome.DownstreamComplete {
		t.Fatal("DownstreamComplete = true for a failed write")
	}
	if outcome.UpstreamFailure {
		t.Fatal("local conversion error recorded as upstream failure")
	}

	// With a cancelled context: a client abort.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)).WithContext(ctx)
	handler.writeDialectHTTPError(req, &failingResponseWriter{}, apiErr, ProvenanceLocalResponseConversionError)
	outcome = <-outcomes
	if outcome.Provenance != ProvenanceClientAbort {
		t.Fatalf("provenance = %v, want client_abort", outcome.Provenance)
	}
	if !outcome.ClientAborted {
		t.Fatal("ClientAborted = false for a cancelled-context write failure")
	}
	if outcome.DownstreamComplete {
		t.Fatal("DownstreamComplete = true for an aborted write")
	}
}

// TestWriteDialectHTTPErrorSuccessfulWriteComplete proves that a successfully
// written dialect error is a complete downstream exchange.
func TestWriteDialectHTTPErrorSuccessfulWriteComplete(t *testing.T) {
	handler, outcomes := outcomeCaptureHandler(t, responsesMapping(t), func(req *http.Request) (*http.Response, error) {
		t.Fatal("round trip must not be called")
		return nil, nil
	})
	apiErr := CanonicalAPIError{
		Status:  http.StatusBadRequest,
		Type:    "invalid_request_error",
		Code:    "invalid_request",
		Message: "bad request",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler.writeDialectHTTPError(req, rec, apiErr, ProvenanceLocalRequestConversionError)
	outcome := <-outcomes
	if !outcome.DownstreamComplete {
		t.Fatal("DownstreamComplete = false for a successful write")
	}
	if outcome.Provenance != ProvenanceLocalRequestConversionError {
		t.Fatalf("provenance = %v", outcome.Provenance)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestWriteUpstreamHTTPErrorRetainsUpstreamRetryAfter proves that the
// Retry-After from the ORIGINAL upstream response is recorded on the outcome,
// and that a downstream write failure retains the upstream facts (status,
// failure, retry-after) while changing the provenance.
func TestWriteUpstreamHTTPErrorRetainsUpstreamRetryAfter(t *testing.T) {
	handler, outcomes := outcomeCaptureHandler(t, responsesMapping(t), func(req *http.Request) (*http.Response, error) {
		t.Fatal("round trip must not be called")
		return nil, nil
	})

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"Retry-After":  {"5"},
		},
		Body: io.NopCloser(strings.NewReader(
			`{"error":{"message":"slow down","type":"rate_limit_error"}}`,
		)),
	}
	apiErr := CanonicalAPIError{
		Status:  http.StatusTooManyRequests,
		Type:    "rate_limit_error",
		Code:    "rate_limit_exceeded",
		Message: "slow down",
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	handler.writeUpstreamHTTPError(req, &failingResponseWriter{}, resp, time.Now(), apiErr)
	outcome := <-outcomes
	// The hold is anchored at header receipt and evaluated at outcome
	// construction: a fast body leaves ~5s remaining (review-08 blocker 9).
	if outcome.RetryAfter <= 4*time.Second || outcome.RetryAfter > 5*time.Second {
		t.Fatalf("RetryAfter = %v, want ~5s", outcome.RetryAfter)
	}
	if !outcome.UpstreamFailure {
		t.Fatal("429 recorded with UpstreamFailure = false")
	}
	if outcome.Provenance != ProvenanceDownstreamWriteError {
		t.Fatalf("provenance = %v, want downstream_write_error (write failed)", outcome.Provenance)
	}
	if outcome.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (retained)", outcome.Status)
	}
	if outcome.DownstreamComplete {
		t.Fatal("DownstreamComplete = true for a failed write")
	}
}

// TestWriteUpstreamHTTPErrorRateSignalled403 proves that a 403 carrying only
// a rate-limit signal is classified as an upstream failure from the ORIGINAL
// upstream headers.
func TestWriteUpstreamHTTPErrorRateSignalled403(t *testing.T) {
	handler, outcomes := outcomeCaptureHandler(t, responsesMapping(t), func(req *http.Request) (*http.Response, error) {
		t.Fatal("round trip must not be called")
		return nil, nil
	})

	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header: http.Header{
			"Content-Type":          {"application/json"},
			"X-Ratelimit-Remaining": {"0"},
		},
		Body: io.NopCloser(strings.NewReader(
			`{"error":{"message":"banned","type":"permission_error"}}`,
		)),
	}
	apiErr := CanonicalAPIError{
		Status:  http.StatusForbidden,
		Type:    "permission_error",
		Code:    "forbidden",
		Message: "banned",
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler.writeUpstreamHTTPError(req, rec, resp, time.Now(), apiErr)
	outcome := <-outcomes
	if !outcome.UpstreamFailure {
		t.Fatal("rate-signalled 403 recorded with UpstreamFailure = false")
	}
	if !outcome.DownstreamComplete {
		t.Fatal("DownstreamComplete = false for a successful write")
	}
	// The rendered client error carries no rate-limit signal: the outcome's
	// UpstreamFailure is the only evidence the proxy needs.
	for name := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(name), "x-ratelimit") {
			t.Fatalf("rendered error leaks upstream rate-limit header %q", name)
		}
	}
}

// TestWriteUpstreamHTTPErrorRateSignalled403ClientAbortRetainsFailure proves
// that a rate-signalled upstream 403 whose translated error write fails with
// a cancelled client context changes provenance to a client abort while
// RETAINING the upstream failure fact: the abort must not suppress the
// definitive failure (native parity).
func TestWriteUpstreamHTTPErrorRateSignalled403ClientAbortRetainsFailure(t *testing.T) {
	handler, outcomes := outcomeCaptureHandler(t, responsesMapping(t), func(req *http.Request) (*http.Response, error) {
		t.Fatal("round trip must not be called")
		return nil, nil
	})

	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header: http.Header{
			"Content-Type":          {"application/json"},
			"X-Ratelimit-Remaining": {"0"},
		},
		Body: io.NopCloser(strings.NewReader(
			`{"error":{"message":"banned","type":"permission_error"}}`,
		)),
	}
	apiErr := CanonicalAPIError{
		Status:  http.StatusForbidden,
		Type:    "permission_error",
		Code:    "forbidden",
		Message: "banned",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)).WithContext(ctx)
	handler.writeUpstreamHTTPError(req, &failingResponseWriter{}, resp, time.Now(), apiErr)
	outcome := <-outcomes
	if outcome.Provenance != ProvenanceClientAbort {
		t.Fatalf("provenance = %v, want client_abort", outcome.Provenance)
	}
	if !outcome.ClientAborted {
		t.Fatal("ClientAborted = false")
	}
	if !outcome.UpstreamFailure {
		t.Fatal("UpstreamFailure lost on write failure: the rate-signalled 403 must remain a definitive upstream failure")
	}
	if outcome.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (retained)", outcome.Status)
	}
	if outcome.DownstreamComplete {
		t.Fatal("DownstreamComplete = true for a failed write")
	}
}
