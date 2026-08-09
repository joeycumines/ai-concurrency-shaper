package transcode

// Review-08 blocker 8 regression tests: the upstream-failure classification
// doctrine — a 2xx upstream response with the wrong representation, a
// transport or body failure racing a client cancellation, and a failed
// non-2xx error-body transfer are all definitive upstream failures, never
// local errors or suppressed client aborts.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// failingReadCloser returns an error on every read.
type failingReadCloser struct {
	err error
}

func (f *failingReadCloser) Read([]byte) (int, error) { return 0, f.err }
func (f *failingReadCloser) Close() error             { return nil }

// TestWrongUpstreamMediaTypeIsUpstreamFailure proves a 2xx upstream response
// carrying the wrong representation for the negotiated stream mode is an
// UPSTREAM failure — the breaker must see a definitive upstream defect (probe
// failed, failure hold applied), never a local conversion error (review-08
// blocker 8).
func TestWrongUpstreamMediaTypeIsUpstreamFailure(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        []byte
		stream      bool
	}{
		{
			name:        "sse for non-streaming request",
			contentType: "text/event-stream",
			body:        testcorpus.ChatCompletionsStreamSSE(),
			stream:      false,
		},
		{
			name:        "json for streaming request",
			contentType: "application/json",
			body:        testcorpus.ChatCompletionsResponseJSON(),
			stream:      true,
		},
		{
			name:        "unrecognized content type",
			contentType: "application/octet-stream",
			body:        []byte("junk"),
			stream:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapping := responsesMapping(t)
			handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{tt.contentType}},
					Body:       io.NopCloser(bytes.NewReader(tt.body)),
				}, nil
			})
			body := `{"model":"m","input":"x"}`
			if tt.stream {
				body = `{"model":"m","input":"x","stream":true}`
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			outcome := <-outcomes
			if outcome.Provenance != ProvenanceUpstreamBodyError {
				t.Fatalf("provenance = %v, want upstream_body_error", outcome.Provenance)
			}
			if !outcome.UpstreamFailure {
				t.Fatal("wrong media type must be an upstream failure")
			}
		})
	}
}

// TestTransportFailureWinsCancellationRace proves a RoundTrip failure that is
// NOT cancellation-derived wins over a concurrent client cancellation: the
// breaker sees the real upstream defect, never a suppressed client abort
// (review-08 blocker 8).
func TestTransportFailureWinsCancellationRace(t *testing.T) {
	mapping := responsesMapping(t)
	handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: lookup upstream.example: no such host")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	outcome := <-outcomes
	if outcome.Provenance != ProvenanceUpstreamTransportError {
		t.Fatalf("provenance = %v, want upstream_transport_error", outcome.Provenance)
	}
	if !outcome.UpstreamFailure {
		t.Fatal("transport failure must be an upstream failure")
	}

	// A cancellation-derived RoundTrip error stays a client abort.
	handler2, abortedOutcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("round trip: %w", context.Canceled)
	})
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	).WithContext(ctx)
	rec2 := httptest.NewRecorder()
	handler2.ServeHTTP(rec2, req2)
	aborted := <-abortedOutcomes
	if aborted.Provenance != ProvenanceClientAbort || !aborted.ClientAborted {
		t.Fatalf("cancellation-derived error must stay a client abort: %+v", aborted)
	}
}

// TestSuccessfulBodyReadFailureWinsCancellationRace proves an upstream body
// read failure racing a client cancellation is an upstream body failure, not
// a suppressed client abort (review-08 blocker 8).
func TestSuccessfulBodyReadFailureWinsCancellationRace(t *testing.T) {
	mapping := responsesMapping(t)
	handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(&failingReadCloser{err: errors.New("connection reset by peer")}),
		}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	outcome := <-outcomes
	if outcome.Provenance != ProvenanceUpstreamBodyError {
		t.Fatalf("provenance = %v, want upstream_body_error", outcome.Provenance)
	}
	if !outcome.UpstreamFailure {
		t.Fatal("body read failure must be an upstream failure")
	}
}

// TestErrorBodyReadFailureIsUpstreamFailure proves a non-2xx response whose
// error body fails to read or exceeds its bound is an upstream body failure
// regardless of the status: a truncated 400 is never a healthy non-failure
// (review-08 blocker 8).
func TestErrorBodyReadFailureIsUpstreamFailure(t *testing.T) {
	tests := []struct {
		name string
		body io.ReadCloser
	}{
		{
			name: "read failure",
			body: io.NopCloser(&failingReadCloser{err: errors.New("connection reset by peer")}),
		},
		{
			name: "bound violation",
			body: io.NopCloser(strings.NewReader(strings.Repeat("x", 2<<20))),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapping := responsesMapping(t)
			handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       tt.body,
				}, nil
			})
			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/responses",
				strings.NewReader(`{"model":"m","input":"x"}`),
			)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			outcome := <-outcomes
			if outcome.Provenance != ProvenanceUpstreamBodyError {
				t.Fatalf("provenance = %v, want upstream_body_error", outcome.Provenance)
			}
			if !outcome.UpstreamFailure {
				t.Fatal("failed error-body transfer must be an upstream failure")
			}
		})
	}
}

// TestRequestBodyCancellationDerivedAbort proves a cancellation-derived
// request-body read error with a cancelled context stays a client abort
// (the suppression gate's positive direction, review-08 blocker 8).
func TestRequestBodyCancellationDerivedAbort(t *testing.T) {
	mapping := responsesMapping(t)
	handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		t.Fatal("round trip must not be called")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	).WithContext(ctx)
	req.Body = io.NopCloser(&failingReadCloser{err: context.Canceled})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	outcome := <-outcomes
	if outcome.Provenance != ProvenanceClientAbort || !outcome.ClientAborted {
		t.Fatalf("cancellation-derived body error must stay a client abort: %+v", outcome)
	}
}

// TestWriteUpstreamBodyErrorWriteFailure proves a failed downstream write
// while rendering an upstream body error changes the provenance exactly like
// the other error writers, retaining the upstream failure fact (review-08
// blocker 8).
func TestWriteUpstreamBodyErrorWriteFailure(t *testing.T) {
	apiErr := CanonicalAPIError{
		Status:  http.StatusBadGateway,
		Type:    "api_error",
		Code:    "upstream_body_error",
		Message: "read upstream error body: boom",
	}
	t.Run("without cancellation", func(t *testing.T) {
		handler, outcomes := outcomeCaptureHandler(t, responsesMapping(t), func(req *http.Request) (*http.Response, error) {
			t.Fatal("round trip must not be called")
			return nil, nil
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
		resp := &http.Response{StatusCode: http.StatusTooManyRequests}
		handler.writeUpstreamBodyError(req, &failingResponseWriter{}, resp, time.Now(), apiErr)
		outcome := <-outcomes
		if outcome.Provenance != ProvenanceDownstreamWriteError {
			t.Fatalf("provenance = %v, want downstream_write_error", outcome.Provenance)
		}
		if !outcome.UpstreamFailure {
			t.Fatal("upstream failure fact must be retained")
		}
	})
	t.Run("with cancellation", func(t *testing.T) {
		handler, outcomes := outcomeCaptureHandler(t, responsesMapping(t), func(req *http.Request) (*http.Response, error) {
			t.Fatal("round trip must not be called")
			return nil, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)).WithContext(ctx)
		resp := &http.Response{StatusCode: http.StatusTooManyRequests}
		handler.writeUpstreamBodyError(req, &failingResponseWriter{}, resp, time.Now(), apiErr)
		outcome := <-outcomes
		if outcome.Provenance != ProvenanceClientAbort || !outcome.ClientAborted {
			t.Fatalf("provenance = %v, want client abort", outcome.Provenance)
		}
		if !outcome.UpstreamFailure {
			t.Fatal("upstream failure fact must be retained")
		}
	})
}

// TestErrorBodyFailureKeepsRetryAfter proves a failed error-body transfer
// retains the upstream Retry-After hold signal from the headers (review-08
// blocker 8).
func TestErrorBodyFailureKeepsRetryAfter(t *testing.T) {
	mapping := responsesMapping(t)
	handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		header := http.Header{"Content-Type": []string{"application/json"}}
		header.Set("Retry-After", "10")
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     header,
			Body:       io.NopCloser(&failingReadCloser{err: errors.New("connection reset by peer")}),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	outcome := <-outcomes
	if outcome.Provenance != ProvenanceUpstreamBodyError {
		t.Fatalf("provenance = %v, want upstream_body_error", outcome.Provenance)
	}
	if !outcome.UpstreamFailure {
		t.Fatal("failed error-body transfer must be an upstream failure")
	}
	if outcome.RetryAfter <= 9*time.Second || outcome.RetryAfter > 10*time.Second {
		t.Fatalf("RetryAfter = %v, want ~10s for a fast body", outcome.RetryAfter)
	}
}

// TestResponseWriteCancellationDerivedAbort proves a cancellation-derived
// response write error with a cancelled context stays a client abort (the
// write-site suppression gate's positive direction, review-08 blocker 8).
func TestResponseWriteCancellationDerivedAbort(t *testing.T) {
	mapping := responsesMapping(t)
	handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(testcorpus.ChatCompletionsResponseJSON())),
		}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	).WithContext(ctx)
	rec := &writeErrorRecorder{err: context.Canceled}
	handler.ServeHTTP(rec, req)
	outcome := <-outcomes
	if outcome.Provenance != ProvenanceClientAbort || !outcome.ClientAborted {
		t.Fatalf("cancellation-derived write error must stay a client abort: %+v", outcome)
	}
}

// writeErrorRecorder is a ResponseWriter whose Write returns a fixed error.
type writeErrorRecorder struct {
	header http.Header
	err    error
}

func (w *writeErrorRecorder) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *writeErrorRecorder) WriteHeader(int) {}
func (w *writeErrorRecorder) Write([]byte) (int, error) {
	return 0, w.err
}

// TestRequestSideRacesStayLocal proves a non-cancellation request-side
// failure racing with a client cancellation is recorded as its true LOCAL
// classification, never suppressed into a client abort (review-08 blocker
// 8).
func TestRequestSideRacesStayLocal(t *testing.T) {
	t.Run("body read", func(t *testing.T) {
		mapping := responsesMapping(t)
		handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
			t.Fatal("round trip must not be called")
			return nil, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(`{"model":"m","input":"x"}`),
		).WithContext(ctx)
		req.Body = io.NopCloser(&failingReadCloser{err: errors.New("connection reset by peer")})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		outcome := <-outcomes
		if outcome.Provenance != ProvenanceLocalRequestConversionError {
			t.Fatalf("provenance = %v, want local_request_conversion_error", outcome.Provenance)
		}
		if outcome.ClientAborted {
			t.Fatal("non-cancellation body error must not be a client abort")
		}
	})
	t.Run("convert", func(t *testing.T) {
		mapping := responsesMapping(t)
		handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
			t.Fatal("round trip must not be called")
			return nil, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(`{"model":"m","input":"x","bogus":1}`),
		).WithContext(ctx)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		outcome := <-outcomes
		if outcome.Provenance != ProvenanceLocalRequestConversionError {
			t.Fatalf("provenance = %v, want local_request_conversion_error", outcome.Provenance)
		}
		if outcome.ClientAborted {
			t.Fatal("non-cancellation conversion error must not be a client abort")
		}
	})
}

// TestSuccessfulBodyReadCancellationDerivedAbort proves a cancellation-derived
// successful-body read error with a cancelled context stays a client abort
// (the JSON body-read suppression gate's positive direction, review-08
// blocker 8).
func TestSuccessfulBodyReadCancellationDerivedAbort(t *testing.T) {
	mapping := responsesMapping(t)
	handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(&failingReadCloser{err: context.Canceled}),
		}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	outcome := <-outcomes
	if outcome.Provenance != ProvenanceClientAbort || !outcome.ClientAborted {
		t.Fatalf("cancellation-derived body error must stay a client abort: %+v", outcome)
	}
}

// slowReadCloser delays the first read by the given duration, then returns
// the payload or error. It simulates an upstream whose error body transfer
// takes real time.
type slowReadCloser struct {
	delay   time.Duration
	data    []byte
	readErr error
	delayed bool
}

func (s *slowReadCloser) Read(p []byte) (int, error) {
	if !s.delayed {
		s.delayed = true
		time.Sleep(s.delay)
	}
	if s.readErr != nil {
		return 0, s.readErr
	}
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.data)
	s.data = s.data[n:]
	return n, nil
}

func (s *slowReadCloser) Close() error { return nil }

// TestRetryAfterUsesHeaderReceiptTime proves the recorded Outcome.RetryAfter
// and the 403 rate-signal classification measure from the moment the
// upstream headers arrived, never from the moment the error body finished
// reading: body-read time is excluded from the remaining hold (review-08
// blocker 9).
func TestRetryAfterUsesHeaderReceiptTime(t *testing.T) {
	const (
		retryAfter = 10 * time.Second
		bodyDelay  = 500 * time.Millisecond
	)
	assertAnchored := func(t *testing.T, outcome Outcome) {
		t.Helper()
		if !outcome.UpstreamFailure {
			t.Fatal("rate-signalled failure must be an upstream failure")
		}
		// The body read consumed ~500ms of the 10s hold: the recorded
		// remaining delay must exclude it.
		if outcome.RetryAfter >= retryAfter || outcome.RetryAfter < 9*time.Second {
			t.Fatalf("RetryAfter = %v, want the remaining hold after the body read (9s..10s)", outcome.RetryAfter)
		}
	}

	t.Run("429 with slow failing error body", func(t *testing.T) {
		mapping := responsesMapping(t)
		handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
			header := http.Header{"Content-Type": []string{"application/json"}}
			header.Set("Retry-After", "10")
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     header,
				Body:       io.NopCloser(&slowReadCloser{delay: bodyDelay, readErr: errors.New("connection reset by peer")}),
			}, nil
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"x"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertAnchored(t, <-outcomes)
	})

	t.Run("429 with slow successful error body", func(t *testing.T) {
		mapping := responsesMapping(t)
		handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
			header := http.Header{"Content-Type": []string{"application/json"}}
			header.Set("Retry-After", "10")
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     header,
				Body: io.NopCloser(&slowReadCloser{
					delay: bodyDelay,
					data:  []byte(`{"error":{"message":"slow"}}`),
				}),
			}, nil
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"x"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertAnchored(t, <-outcomes)
	})

	t.Run("403 rate signal with slow body", func(t *testing.T) {
		mapping := responsesMapping(t)
		handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
			header := http.Header{"Content-Type": []string{"application/json"}}
			header.Set("Retry-After", "10")
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     header,
				Body: io.NopCloser(&slowReadCloser{
					delay: bodyDelay,
					data:  []byte(`{"error":{"message":"rate limited"}}`),
				}),
			}, nil
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"x"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertAnchored(t, <-outcomes)
	})

	t.Run("fast body records the full hold", func(t *testing.T) {
		mapping := responsesMapping(t)
		handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
			header := http.Header{"Content-Type": []string{"application/json"}}
			header.Set("Retry-After", "10")
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"fast"}}`)),
			}, nil
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"x"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		outcome := <-outcomes
		if !outcome.UpstreamFailure {
			t.Fatal("429 must be an upstream failure")
		}
		// A fast body reads in microseconds: the hold is ~10s, the full
		// signaled duration.
		if outcome.RetryAfter <= 9*time.Second || outcome.RetryAfter > retryAfter {
			t.Fatalf("RetryAfter = %v, want ~10s for a fast body", outcome.RetryAfter)
		}
	})
}
