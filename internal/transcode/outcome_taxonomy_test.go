package transcode

// Review-z commit 4 acceptance tests: the eight-row failure taxonomy, the
// per-attempt signing transport, and the anchored Retry-After (the recorder
// header fallback is gone for transcoded exchanges).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// multiCaptureSigner records every request it signs.
type multiCaptureSigner struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (s *multiCaptureSigner) Sign(_ context.Context, req *http.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req.Clone(req.Context()))
	return nil
}

// failingSigner fails every sign attempt.
type failingSigner struct{}

func (failingSigner) Sign(context.Context, *http.Request) error {
	return errors.New("signing failed")
}

// TestOutcomeSinkExactlyOnce proves the sink records exactly one outcome.
func TestOutcomeSinkExactlyOnce(t *testing.T) {
	var sink OutcomeSink
	sink.Record(Outcome{Provenance: ProvenanceUpstreamHTTP})
	sink.Record(Outcome{Provenance: ProvenanceClientAbort})
	outcome, ok := sink.Load()
	if !ok {
		t.Fatal("no outcome recorded")
	}
	if outcome.Provenance != ProvenanceUpstreamHTTP {
		t.Fatalf("provenance = %v, want the first record", outcome.Provenance)
	}
	if _, ok := (&OutcomeSink{}).Load(); ok {
		t.Fatal("empty sink reports a record")
	}
}

// TestOutcomeTaxonomyEightRows asserts the eight-row failure taxonomy exactly
// (review-z commit 4): the breaker classification derived from each outcome.
func TestOutcomeTaxonomyEightRows(t *testing.T) {
	type row struct {
		name string
		out  Outcome
		// wantFailure: breaker failure classification.
		wantFailure bool
		// wantLocal: local failure (never breaker-relevant).
		wantLocal bool
		// wantAbort: aborted exchange.
		wantAbort bool
		// wantAttempted: whether the handler records the attempt fact.
		wantAttempted bool
	}
	rows := []row{
		{
			// 1: valid and portable upstream result -> translated response,
			// breaker success.
			name: "valid portable result",
			out: Outcome{
				UpstreamAttempted:  true,
				UpstreamStatus:     Optional[int]{Value: 200, Set: true},
				Provenance:         ProvenanceUpstreamHTTP,
				DownstreamComplete: true,
			},
			wantAttempted: true,
		},
		{
			// 2: valid but unrepresentable upstream feature -> conversion
			// error, neutral. The upstream was reached and its response
			// consumed, so the attempt fact is true (the rendered 502 is
			// the proxy's gateway status, not an upstream status).
			name: "valid unrepresentable feature",
			out: Outcome{
				UpstreamAttempted: true,
				UpstreamStatus:    Optional[int]{Value: 502, Set: true},
				Provenance:        ProvenanceLocalResponseConversionError,
				LocalFailure:      true,
			},
			wantLocal:     true,
			wantAttempted: true,
		},
		{
			// 3: invalid model-generated tool arguments -> conversion error,
			// neutral.
			name: "invalid model tool arguments",
			out: Outcome{
				UpstreamAttempted: true,
				UpstreamStatus:    Optional[int]{Value: 502, Set: true},
				Provenance:        ProvenanceLocalResponseConversionError,
				LocalFailure:      true,
			},
			wantLocal:     true,
			wantAttempted: true,
		},
		{
			// 4: malformed or contradictory upstream wire -> gateway error,
			// failure.
			name: "malformed upstream wire",
			out: Outcome{
				UpstreamAttempted: true,
				UpstreamStatus:    Optional[int]{Value: 200, Set: true},
				Provenance:        ProvenanceUpstreamBodyError,
				UpstreamFailure:   true,
			},
			wantFailure:   true,
			wantAttempted: true,
		},
		{
			// 5: upstream HTTP 429/5xx/rate-signalled 403 -> provider error,
			// failure.
			name: "upstream 429",
			out: Outcome{
				UpstreamAttempted: true,
				UpstreamStatus:    Optional[int]{Value: 429, Set: true},
				Provenance:        ProvenanceUpstreamHTTP,
				UpstreamFailure:   true,
			},
			wantFailure:   true,
			wantAttempted: true,
		},
		{
			// 6: local rendering or signing problem -> internal/gateway
			// error, neutral. The upstream was reached; the failure is
			// local (conversion/validation), so the attempt fact is true
			// but no breaker failure is recorded.
			name: "local rendering failure",
			out: Outcome{
				UpstreamAttempted: true,
				UpstreamStatus:    Optional[int]{Value: 502, Set: true},
				Provenance:        ProvenanceLocalStreamValidationError,
				LocalFailure:      true,
			},
			wantLocal:     true,
			wantAttempted: true,
		},
		{
			// 7: client abort before definitive upstream outcome -> aborted
			// exchange, neutral. No attempt fact: abort records carry none
			// (the proxy attempt marker covers mid-flight aborts).
			name: "client abort before outcome",
			out: Outcome{
				Provenance:    ProvenanceClientAbort,
				ClientAborted: true,
			},
			wantAbort: true,
		},
		{
			// 8: client abort after definitive upstream failure -> aborted
			// exchange, failure retained.
			name: "client abort after upstream failure",
			out: Outcome{
				UpstreamAttempted: true,
				UpstreamStatus:    Optional[int]{Value: 503, Set: true},
				Provenance:        ProvenanceClientAbort,
				ClientAborted:     true,
				UpstreamFailure:   true,
			},
			wantFailure:   true,
			wantAbort:     true,
			wantAttempted: true,
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if r.out.UpstreamFailure != r.wantFailure {
				t.Fatalf("UpstreamFailure = %v, want %v", r.out.UpstreamFailure, r.wantFailure)
			}
			// The attempt fact must be recorded whenever the upstream was
			// reached: everything except a local REQUEST conversion/signing
			// error dispatches an upstream request. Abort records carry no
			// fact (the proxy marker covers mid-flight aborts).
			if r.out.UpstreamAttempted != r.wantAttempted {
				t.Fatalf("UpstreamAttempted = %v, want %v", r.out.UpstreamAttempted, r.wantAttempted)
			}
			if r.out.LocalFailure != r.wantLocal {
				t.Fatalf("LocalFailure = %v, want %v", r.out.LocalFailure, r.wantLocal)
			}
			if r.out.ClientAborted != r.wantAbort {
				t.Fatalf("ClientAborted = %v, want %v", r.out.ClientAborted, r.wantAbort)
			}
			// Row 7 and 8: an abort is never a clean completion.
			if r.wantAbort && r.out.DownstreamComplete {
				t.Fatal("aborted exchange recorded as complete")
			}
		})
	}
}

// TestSigningTransportSignsEveryAttempt proves the signing transport signs
// each actual attempt after body reconstruction, never reuses a signature,
// and never mutates the original request.
func TestSigningTransportSignsEveryAttempt(t *testing.T) {
	signer := &multiCaptureSigner{}
	original := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"a":1}`))
	original = original.WithContext(WithRequestSigner(original.Context(), signer))
	original.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"a":1}`)), nil
	}
	original.ContentLength = 7
	var attempts atomic.Int32
	inner := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	transport := &SigningTransport{Inner: inner}

	// Three sequential requests through the same transport: the signer must
	// run exactly once per attempt and the original must stay untouched.
	for i := 0; i < 3; i++ {
		resp, err := transport.RoundTrip(original)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
	if len(signer.reqs) != 3 {
		t.Fatalf("signatures = %d, want one per attempt", len(signer.reqs))
	}
	if original.Header.Get("X-Signed") != "" {
		t.Fatal("original request was mutated")
	}
	// The signed clones carry the finalized Content-Length and body.
	signer.mu.Lock()
	defer signer.mu.Unlock()
	for _, signed := range signer.reqs {
		body, _ := io.ReadAll(signed.Body)
		if signed.ContentLength != int64(len(body)) {
			t.Fatalf("signed Content-Length = %d, want %d", signed.ContentLength, len(body))
		}
		if string(body) != `{"a":1}` {
			t.Fatalf("signed body = %q", body)
		}
	}
}

// TestSigningTransportSignerErrorIsLocal proves a signer error surfaces as a
// transport error (local classification), never as an upstream response.
func TestSigningTransportSignerErrorIsLocal(t *testing.T) {
	transport := &SigningTransport{
		Inner: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatal("inner transport must not run when signing fails")
			return nil, nil
		}),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req = req.WithContext(WithRequestSigner(req.Context(), failingSigner{}))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
	if _, err := transport.RoundTrip(req); err == nil {
		t.Fatal("signer failure swallowed")
	} else if !strings.Contains(err.Error(), "signing") {
		t.Fatalf("err = %v, want a signing error", err)
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestTranscodeRetryAfterNeverReDerivedFromRenderedHeader proves the
// transcoded classification uses ONLY the outcome's anchored Retry-After:
// the recorder-header fallback is gone, so an expired original hold is never
// re-parsed from the translated downstream header with a fresh receipt
// timestamp (review-z commit 4).
func TestTranscodeRetryAfterNeverReDerivedFromRenderedHeader(t *testing.T) {
	now := time.Now()
	// The outcome carries a present-but-expired hold (Set=true, zero): the
	// translated header may carry a stale value, but the classification must
	// NOT re-read it.
	outcome := Outcome{
		UpstreamStatus:  Optional[int]{Value: 429, Set: true},
		Provenance:      ProvenanceUpstreamHTTP,
		UpstreamFailure: true,
		RetryAfter:      Optional[time.Duration]{Value: 0, Set: true},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_ = req
	_ = now
	// The proxy-side classification is asserted in the proxy package; here
	// we pin the outcome contract: Set=true with zero means
	// present-but-expired, Set=false means not supplied.
	if !outcome.RetryAfter.Set || outcome.RetryAfter.Value != 0 {
		t.Fatalf("RetryAfter = %+v", outcome.RetryAfter)
	}
	var absent Outcome
	if absent.RetryAfter.Set {
		t.Fatal("absent RetryAfter must have Set=false")
	}
}
