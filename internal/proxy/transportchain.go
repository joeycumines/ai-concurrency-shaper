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

import (
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/retry"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// buildUpstreamTransport assembles the engine's upstream round-trip chain,
// outermost first: the retry transport (when retries are enabled), the
// reauthentication layer, attempt marking, request signing, and finally the
// configured (or default) transport.
func buildUpstreamTransport(cfg *proxyConfig) http.RoundTripper {
	var transport http.RoundTripper = cfg.transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if cfg.maxRetries != 0 {
		var checkRetry retry.CheckRetry
		if cfg.retrySkipOn429 {
			checkRetry = func(resp *http.Response, err error) bool {
				if err != nil {
					// Local non-retryable defects (e.g. signing failures)
					// are never retried.
					return !retry.IsNonRetryable(err)
				}
				if resp == nil {
					return false
				}
				// Skip 429 — retrying rate-limited responses amplifies
				// the concurrency issue that caused the 429.
				return resp.StatusCode >= 500
			}
		}
		// Disposition (grounded moot): a deterministic
		// transcode decode failure (a 200 body outside the supported wire
		// subset) can NEVER be retried here, because this policy sees only
		// RoundTrip errors and HTTP status — the body fails AFTER the
		// response returns, inside the TranscodeHandler. The proxy therefore
		// re-sends a poisonous body zero times; the field-observed repeat
		// 502s were client-driven retries. Pinned by
		// TestProxyTranscodePoisonUsage200IsNeverRetried
		// (transcode_classification_test.go).
		// The signing transport sits inside the retry chain so EVERY
		// attempt is signed after the retry layer rebuilt the body and
		// finalized Content-Length. Requests without a
		// signer in their context pass through untouched.
		// The reauthentication layer sits inside the retry chain too, so its
		// one re-send after a rejected rotating credential replays the body the
		// retry layer buffered, and outside attempt marking so the re-send
		// counts as an attempt.
		transport = &retry.Transport{
			Inner: &reauthTransport{inner: withAttemptMarkingTransport(&transcode.SigningTransport{
				Inner: transport,
			})},
			MaxRetries:    cfg.maxRetries,
			MaxBodyBytes:  cfg.maxBodyBytes,
			WaitMin:       cfg.retryWaitMin,
			WaitMax:       cfg.retryWaitMax,
			MinRetryDelay: cfg.retryMinDelay,
			Breaker:       cfg.breaker,
			CheckRetry:    checkRetry,
		}
	} else {
		transport = &reauthTransport{inner: withAttemptMarkingTransport(&transcode.SigningTransport{
			Inner: transport,
		})}
	}
	return transport
}

type retryBreakerTransport interface {
	RetryBreaker() *circuitbreaker.Breaker
}

type retryInFlightTransport interface {
	SetInFlightRetries(*atomic.Int64)
}

type roundTripperUnwrapper interface {
	Unwrap() http.RoundTripper
}

type retryTransportDetection struct {
	found              bool
	breaker            *circuitbreaker.Breaker
	conflictingBreaker bool
	setInFlightRetries func(*atomic.Int64)
}

type attemptMarkingTransport struct {
	inner http.RoundTripper
}

func (t *attemptMarkingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if state := upstreamAttemptStateFromContext(req.Context()); state != nil {
		state.started.Store(true)
	}
	inner := t.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	return inner.RoundTrip(req)
}

func (t *attemptMarkingTransport) Unwrap() http.RoundTripper {
	if t == nil {
		return nil
	}
	return t.inner
}

func withAttemptMarkingTransport(transport http.RoundTripper) http.RoundTripper {
	if transport == nil {
		transport = http.DefaultTransport
	}
	if withAttemptMarkingRetryTransport(transport, 0) {
		return transport
	}
	return &attemptMarkingTransport{inner: transport}
}

func withAttemptMarkingRetryTransport(transport http.RoundTripper, depth int) bool {
	if transport == nil || depth > 16 {
		return false
	}
	if _, ok := transport.(*attemptMarkingTransport); ok {
		return true
	}
	if rt, ok := transport.(*retry.Transport); ok {
		rt.Inner = withAttemptMarkingTransport(rt.Inner)
		return true
	}
	if uw, ok := transport.(roundTripperUnwrapper); ok {
		return withAttemptMarkingRetryTransport(uw.Unwrap(), depth+1)
	}
	return false
}

func detectRetryTransport(transport http.RoundTripper) retryTransportDetection {
	return detectRetryTransportDepth(transport, 0)
}

func detectRetryTransportDepth(transport http.RoundTripper, depth int) retryTransportDetection {
	if transport == nil || depth > 16 {
		return retryTransportDetection{}
	}

	var out retryTransportDetection
	if rt, ok := transport.(retryBreakerTransport); ok {
		out.found = true
		out.breaker = rt.RetryBreaker()
	}
	if rt, ok := transport.(retryInFlightTransport); ok {
		out.found = true
		out.setInFlightRetries = rt.SetInFlightRetries
	}

	if uw, ok := transport.(roundTripperUnwrapper); ok {
		inner := detectRetryTransportDepth(uw.Unwrap(), depth+1)
		if !out.found {
			return inner
		}
		if out.breaker == nil {
			out.breaker = inner.breaker
		} else if inner.breaker != nil && inner.breaker != out.breaker {
			out.conflictingBreaker = true
		}
		out.conflictingBreaker = out.conflictingBreaker || inner.conflictingBreaker
		if out.setInFlightRetries == nil {
			out.setInFlightRetries = inner.setInFlightRetries
		}
	}
	return out
}

func validateRetryTransportBreaker(transport http.RoundTripper, breaker *circuitbreaker.Breaker, maxRetries int) error {
	retryInfo := detectRetryTransport(transport)
	if !retryInfo.found || retryInfo.breaker == nil {
		if retryInfo.conflictingBreaker {
			return errors.New("proxy: supplied retry transport wrappers expose conflicting breakers")
		}
		return nil
	}
	if retryInfo.conflictingBreaker {
		return errors.New("proxy: supplied retry transport wrappers expose conflicting breakers")
	}
	if breaker == nil {
		return errors.New("proxy: supplied retry transport has a breaker but WithBreaker is nil; WithBreaker owns proxy breaker integration")
	}
	if retryInfo.breaker != breaker {
		return errors.New("proxy: supplied retry transport breaker must match WithBreaker breaker")
	}
	if maxRetries != 0 {
		return errors.New("proxy: supplied retry transport with a breaker cannot be wrapped by WithMaxRetries; set the transport breaker to nil or disable proxy retries")
	}
	return nil
}
