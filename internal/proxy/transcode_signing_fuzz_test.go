package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// FuzzTranscodeSigningAndRetry drives the per-attempt signing + retry
// interplay (review-z commit 4/6) and asserts:
//
//   - the signer runs EXACTLY once per actual upstream attempt;
//   - a signer failure never contacts the upstream, never retries, and
//     records zero breaker failures;
//   - a body over the retry cap is attempted exactly once (no replay);
//   - the signer always observes the finalized Content-Length;
//   - retries after 5xx failures record exactly the failed attempts.
func FuzzTranscodeSigningAndRetry(f *testing.F) {
	validRequest := `{"model":"m","input":"x"}`
	validResponse := `{"object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`
	f.Add([]byte(validRequest), uint8(0), false, false)
	f.Add([]byte(validRequest), uint8(2), false, false)
	f.Add([]byte(validRequest), uint8(0), true, false)
	f.Add([]byte(validRequest), uint8(0), false, true)
	f.Add([]byte(`{"model":"m"`), uint8(0), false, false)
	f.Add([]byte(`not json`), uint8(3), true, true)

	f.Fuzz(func(
		t *testing.T,
		body []byte,
		failFirstN uint8,
		signerFails bool,
		bodyTooLarge bool,
	) {
		failFirstN %= 4

		var attempts atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := attempts.Add(1)
			if int32(failFirstN) >= n {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, validResponse)
		}))
		defer upstream.Close()

		var signerCalls atomic.Int32
		signer := fuzzSigner{
			fails: signerFails,
			calls: &signerCalls,
		}

		breaker, err := circuitbreaker.New(
			circuitbreaker.WithFailureThreshold(100),
			circuitbreaker.WithBasePenalty(1*time.Millisecond),
			circuitbreaker.WithMaxPenalty(10*time.Millisecond),
		)
		if err != nil {
			t.Fatal(err)
		}

		upstreamURL, _ := url.Parse(upstream.URL)
		pat, err := route.Parse("POST /v1/responses")
		if err != nil {
			t.Fatal(err)
		}
		mapping := testResponsesMapping(t)
		mapping.Auth = transcode.AuthPolicy{
			Mode:   transcode.AuthExternalSigner,
			Signer: signer,
		}
		p, err := New(
			WithUpstream(upstreamURL),
			WithMatcher(route.NewMatcher([]route.Pattern{pat})),
			WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
			WithMetrics(metrics.NewCollector()),
			WithBreaker(breaker),
			WithMaxRetries(3),
			WithMaxBodyBytes(1<<10),
			WithTranscodeMapping(transcodeMapping(mapping)),
		)
		if err != nil {
			t.Fatal(err)
		}

		requestBody := string(body)
		if bodyTooLarge {
			requestBody = `{"model":"m","input":"` + strings.Repeat("x", 4096) + `"}`
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)

		signed := signerCalls.Load()
		upstreamAttempts := attempts.Load()

		switch {
		case signerFails:
			// A pre-dispatch rejection (e.g. malformed request JSON) never
			// runs the signer; once the signer DID run, the exchange is
			// local: the upstream is never contacted and the breaker
			// records nothing.
			if signed == 1 {
				if upstreamAttempts != 0 {
					t.Fatalf("signer failure reached the upstream (%d attempts)", upstreamAttempts)
				}
				if stats := breaker.Stats(); stats.TotalFailures != 0 {
					t.Fatalf("signer failure recorded %d breaker failures", stats.TotalFailures)
				}
			} else if signed != 0 || upstreamAttempts != 0 {
				t.Fatalf("signer calls = %d, upstream attempts = %d; want only the pre-dispatch reject", signed, upstreamAttempts)
			}
			return
		}

		// Successful signing: one signer call per actual attempt.
		if signed != upstreamAttempts {
			t.Fatalf("signer calls = %d, upstream attempts = %d (must sign every attempt)", signed, upstreamAttempts)
		}
		if signed == 0 && rec.Code == http.StatusOK {
			t.Fatalf("a successful exchange was never signed: %d", rec.Code)
		}
		if bodyTooLarge && upstreamAttempts > 1 {
			t.Fatalf("body over the retry cap was replayed (%d attempts)", upstreamAttempts)
		}
		wantAttempts := int32(failFirstN) + 1
		if !bodyTooLarge && upstreamAttempts > wantAttempts {
			t.Fatalf("attempts = %d, want <= %d (bounded retries)", upstreamAttempts, wantAttempts)
		}
		if stats := breaker.Stats(); int64(upstreamAttempts-1) != stats.TotalFailures && rec.Code == http.StatusOK {
			t.Fatalf("breaker failures = %d, want %d for the failed attempts", stats.TotalFailures, upstreamAttempts-1)
		}
	})
}

// fuzzSigner counts its invocations and optionally fails.
type fuzzSigner struct {
	fails bool
	calls *atomic.Int32
}

func (s fuzzSigner) Sign(_ context.Context, req *http.Request) error {
	s.calls.Add(1)
	if req.ContentLength < 0 {
		// The retry layer must always finalize Content-Length before the
		// signer runs (review-z commit 4).
		return &transcode.SigningError{Cause: context.DeadlineExceeded}
	}
	if s.fails {
		return &transcode.SigningError{Cause: context.DeadlineExceeded}
	}
	return nil
}
