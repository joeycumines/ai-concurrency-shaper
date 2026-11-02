// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package proxy

// End-to-end tests that the transport-attempt fact, not a call-site guard,
// decides local-versus-upstream classification on every path, and that the
// per-attempt marker does not leak across reauth/retry resends.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// TestClassifyNativeExchangePreAttemptCancelIsNeitherFailureNorSuccess pins
// that a client cancellation before any upstream attempt is not an upstream
// failure (observedStatus 0 is IsFailureStatus(0)=true, but the attempt fact
// must win) and not a success either.
func TestClassifyNativeExchangePreAttemptCancelIsNeitherFailureNorSuccess(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(cancelled)
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	cancel()

	result := classifyNativeExchange(rec, req, nil, false, false, 1, time.Now())
	if result.upstreamFailure {
		t.Fatal("a pre-attempt client cancel is classified as an upstream failure")
	}
	if result.upstreamSuccess {
		t.Fatal("a pre-attempt client cancel is classified as an upstream success")
	}
	if !result.clientAborted {
		t.Fatal("the cancelled context must mark the exchange client-aborted")
	}
}

// TestClassifyNativeExchangePreAttemptRefusalIsLocal pins that a local 4xx
// refusal rendered before any upstream attempt (the native dialect error
// path) is a local failure and never an upstream success, even under a probe
// epoch — and that feeding the result to the resolution path cancels the
// probe instead of closing it.
func TestClassifyNativeExchangePreAttemptRefusalIsLocal(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			rec := &statusRecorder{
				ResponseWriter:      httptest.NewRecorder(),
				status:              status,
				proxyGeneratedError: true, // writeNativeDialectError marks every local refusal
			}
			result := classifyNativeExchange(rec, req, nil, false, false, 1, time.Now())
			if !result.localFailure {
				t.Fatalf("a pre-attempt local %d refusal is not classified as a local failure", status)
			}
			if result.upstreamSuccess {
				t.Fatalf("a pre-attempt local %d refusal is classified as an upstream success", status)
			}
			if result.upstreamFailure {
				t.Fatalf("a pre-attempt local %d refusal is classified as an upstream failure", status)
			}

			b := halfOpenBreaker(t)
			epoch := takeProbe(t, b)
			p := newTestProxy(b)
			p.resolveBreakerResult(result, nil, time.Now(), epoch)
			if stats := b.Stats(); stats.State == circuitbreaker.Closed || stats.TotalSuccesses != 0 {
				t.Fatalf("probe resolved as success on a local %d refusal: state=%s successes=%d", status, stats.State, stats.TotalSuccesses)
			}
		})
	}
}

// TestClassifyNativeExchangeUnmarkedPreAttemptRefusalIsLocal pins the
// classifier-level guarantee even when the recorder fact was never set (a
// caller passing upstreamAttempted=false directly): a positive status with no
// attempt is a local answer, never an upstream outcome.
func TestClassifyNativeExchangeUnmarkedPreAttemptRefusalIsLocal(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := &statusRecorder{
		ResponseWriter: httptest.NewRecorder(),
		status:         http.StatusBadRequest, // no proxyGeneratedError marker at all
	}
	result := classifyNativeExchange(rec, req, nil, false, false, 1, time.Now())
	if !result.localFailure {
		t.Fatal("a pre-attempt status without any marker is not classified as a local failure")
	}
	if result.upstreamSuccess || result.upstreamFailure {
		t.Fatalf("a pre-attempt exchange reports upstream success=%v failure=%v, want neither", result.upstreamSuccess, result.upstreamFailure)
	}
}

// failingResendSigner signs successfully on the first attempt and fails on
// every later one, simulating a signer that breaks when the reauth layer
// resends with a fresh credential.
type failingResendSigner struct{ calls atomic.Int64 }

func (s *failingResendSigner) Sign(context.Context, *http.Request) error {
	if s.calls.Add(1) > 1 {
		return errors.New("signing: refreshed credential rejected locally")
	}
	return nil
}

// rotatingSecretSource is an auth.DynamicSecretSource whose value changes
// from stale to fresh after the first Invalidate, so the reauth resend path
// engages without any external process.
type rotatingSecretSource struct {
	mu      sync.Mutex
	current string
}

func (s *rotatingSecretSource) Secret(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, nil
}

func (s *rotatingSecretSource) Invalidate(rejected string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == rejected {
		s.current = "fresh"
	}
}

func (s *rotatingSecretSource) Reacquire(_ context.Context, rejected string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == rejected {
		return "", errors.New("no replacement credential available")
	}
	return s.current, nil
}

// TestProxyReauthResendSignerFailureIsLocalNotUpstream drives a full proxy
// exchange: attempt 1 reaches the (fake) upstream and is answered 401, the
// reauth layer resends with a fresh credential, and the signer fails locally
// on the resend. The exchange must classify as a local failure — the breaker
// must stay Closed (a local defect never trips it). The per-request attempt
// marker from attempt 1 must not leak into the resend's error.
func TestProxyReauthResendSignerFailureIsLocalNotUpstream(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if r.Header.Get("Authorization") == "Bearer fresh" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized) // forces the reauth resend
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	signer := &failingResendSigner{}
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(time.Hour),
		circuitbreaker.WithMaxOpenTimeout(time.Hour),
	)
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{mustRoutePattern(t, "POST /v1/messages")})),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(10*time.Second),
		// Retries enabled with a body cap: the retry transport buffers the
		// body and supplies GetBody on every attempt, which is what makes the
		// reauth resend replayable on the transparent path.
		WithMaxRetries(1),
		WithMaxBodyBytes(1<<20),
		WithRetryWaitMin(time.Millisecond),
		WithRetryWaitMax(time.Millisecond),
		WithRetryMinDelay(0),
		WithBreaker(b),
		// A rotating provider credential: the Rewrite hook attaches the
		// reauthentication marker, so reauthTransport engages on the 401.
		WithAuthPolicy(&auth.AuthPolicy{
			Mode:   auth.AuthBearer,
			Secret: &rotatingSecretSource{current: "stale"},
		}),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Attach the signer to the request context the way the transcode path
	// does (transcode.AuthExternalSigner), then serve through the proxy's
	// transparent path: the proxy's Rewrite hook applies the rotating
	// credential and attaches the reauth marker on top.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(transcode.WithRequestSigner(req.Context(), signer))

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if got := signer.calls.Load(); got < 2 {
		t.Fatalf("signer calls = %d, want at least 2 (initial attempt plus the reauth resend)", got)
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (the resend fails locally before reaching upstream)", got)
	}
	if stats := b.Stats(); stats.TotalFailures != 0 || stats.State != circuitbreaker.Closed {
		t.Fatalf("breaker recorded failures from a local signer defect: state=%s totalFailures=%d", stats.State, stats.TotalFailures)
	}
}

// TestBuildUpstreamTransportReauthResendClearsAttemptMarker pins the
// per-attempt marker directly: after attempt 1 starts, a reauth resend whose
// inner chain fails locally must leave started=false, so the terminal error
// classifies as local rather than upstream.
func TestBuildUpstreamTransportReauthResendClearsAttemptMarker(t *testing.T) {
	ctx := t.Context()
	state := &upstreamAttemptState{}
	ctx = withUpstreamAttemptState(ctx, state)

	var calls atomic.Int64
	base := transportchainRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{},
				Body:       http.NoBody,
				Request:    req,
			}, nil
		}
		return nil, errors.New("unexpected second upstream call")
	})
	transport := &reauthTransport{inner: &transcode.SigningTransport{
		Inner: &attemptMarkingTransport{inner: base},
	}}

	// A reauthentication marker whose source yields a different credential on
	// reacquisition, so the resend path runs.
	dyn := &rotatingSecretSource{current: "stale"}
	req := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/messages", strings.NewReader(`{}`))
	req = req.WithContext(ctx)
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
	auth.AttachReauthentication(req, dyn, "stale", "Authorization", "Bearer ")

	// The signer fails on the resend only.
	req = req.WithContext(transcode.WithRequestSigner(req.Context(), &failingResendSigner{}))

	_, err := transport.RoundTrip(req)
	if err == nil {
		t.Fatal("RoundTrip error = nil, want the local signer failure")
	}
	if state.started.Load() {
		t.Fatal("attempt marker leaked across the reauth resend: started=true after a local resend failure")
	}
}
