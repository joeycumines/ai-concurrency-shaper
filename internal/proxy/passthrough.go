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
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/retry"
)

func (p *Proxy) servePassthrough(w http.ResponseWriter, r *http.Request, flightID uint64) {
	var localPanic bool
	var upstreamAbortFailure bool
	var retryAttempt *retry.BreakerAttempt
	copyState := &copyErrorState{}
	attemptState := &upstreamAttemptState{}
	ctx := withCopyErrorState(r.Context(), copyState)
	ctx = withUpstreamAttemptState(ctx, attemptState)
	r = r.WithContext(ctx)

	// result is the immutable per-exchange classification produced after the
	// handler finishes. It is assigned before the slot-release defer can run
	// and consumed by the defer, the breaker resolution, and the completion
	// counters — never recomputed.
	var result exchangeResult

	// Reject immediately if the circuit breaker is OPEN, BEFORE acquiring
	// any resources. This mirrors the pre-check in serveLimited (which checks
	// the breaker before queueing). Without this, passthrough requests
	// acquire a global-limiter slot before being rejected — wasteful and
	// inconsistent with serveLimited's ordering. When the breaker is OPEN,
	// the request gets an immediate 503 without consuming any concurrency
	// slots, which is the correct behavior: the upstream is known to be
	// unhealthy, so there is no point queueing.
	var breakerEpoch uint64
	if p.breaker != nil {
		var err error
		breakerEpoch, err = p.breaker.Allow()
		if err != nil {
			p.m.IncCircuitRejected()
			http.Error(w, "circuit open", http.StatusServiceUnavailable)
			return
		}
		// Inject the breaker epoch into the request context so breaker reporting
		// uses the same stale-probe guard for the first retry-transport attempt.
		ctx := context.WithValue(r.Context(), retry.BreakerEpochKey, breakerEpoch)
		ctx = p.withRetryAttemptContext(ctx, &retryAttempt)
		r = r.WithContext(ctx)
	} else if p.retryTracksAttempts {
		r = r.WithContext(p.withRetryAttemptContext(r.Context(), &retryAttempt))
	}

	if p.globalLimiter != nil && !p.matcher.IsUnlimited(r.Method, r.URL.Path) {
		// Track queue metrics for passthrough requests waiting in the
		// global limiter so the TUI reports queue depth accurately.
		p.m.IncQueued()

		// Record the start of the queue phase precisely at the moment
		// the request begins waiting for the global limiter, overriding
		// the coarse ServeHTTP-entry timestamp so QueueDuration
		// measures only actual queue wait time.
		if rec, ok := w.(*statusRecorder); ok && rec.entry != nil {
			rec.entry.Timing.QueueStart = time.Now()
		}

		// Apply the configured queue timeout so passthrough requests
		// do not block indefinitely when the global limiter is saturated.
		// This mirrors the timeout wrapping in serveLimited and ensures
		// QueueTimeoutOption is honored for ALL request paths.
		ctx := r.Context()
		if p.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, p.timeout)
			defer cancel()
		}

		release, err := p.globalLimiter.Acquire(ctx)
		p.m.DecQueued()

		if err != nil {
			// Record the moment the global limiter wait ended so that
			// QueueDuration reflects the actual queue time, not zero.
			if rec, ok := w.(*statusRecorder); ok && rec.entry != nil {
				rec.entry.Timing.QueueEnd = time.Now()
			}
			if p.breaker != nil {
				p.breaker.CancelProbe(breakerEpoch)
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				p.m.IncTimeout()
				http.Error(w, "queue timeout", http.StatusGatewayTimeout)
			} else {
				p.m.IncCancelled()
				http.Error(w, "request canceled", http.StatusServiceUnavailable)
			}
			return
		}
		// Slot-release defer: applies phantom penalty, cancel-cooldown,
		// or failure hold to the global-limiter slot, mirroring
		// serveLimited's phantom penalty / failure hold / cancel-cooldown
		// branches. The isTransportOrProxyError guard is identical to
		// serveLimited's — see the comment block there for the full
		// rationale.
		defer func() {
			// A local panic is NOT an upstream failure — release immediately
			// without penalty or cooldown (mirrors serveLimited's localPanic guard).
			if localPanic {
				release()
				return
			}
			rec, recOK := w.(*statusRecorder)
			ctxErr := r.Context().Err()
			isClientCancel := isContextCancellation(ctxErr) || (recOK && rec.downstreamWriteFailed())
			finalUnclean := isClientCancel || (recOK && rec.aborted)
			retryRecordedFailure := retryAttemptFailureForSlot(retryAttempt, retryHistoricalFailureCanDriveSlot(rec, finalUnclean))
			suppressFailureForClientAbort := result.suppressibleAbort && !retryRecordedFailure
			// Single anchor for classification decisions in this defer:
			// the immutable per-exchange result, plus the slot-specific
			// retry-recorded failure fact.
			upstreamFailure := result.upstreamFailure || retryRecordedFailure
			// Phantom penalty: hold the global-limiter slot after an upstream failure
			// when the circuit breaker is enabled. PenaltyDuration is always
			// positive under the validated options, so no penalty-zero
			// fall-through exists here.
			if p.breaker != nil && result.upstreamAttempted && !suppressFailureForClientAbort && upstreamFailure {
				time.AfterFunc(p.breaker.PenaltyDuration(), release)
				return
			}
			if p.failureHold > 0 && result.upstreamAttempted && !suppressFailureForClientAbort && upstreamFailure {
				// Standalone failure hold: holds the global-limiter slot after an
				// upstream failure. This applies only when the circuit breaker is
				// disabled. Mirrors serveLimited.
				time.AfterFunc(p.failureHold, release)
				return
			}
			if isClientCancel && result.upstreamAttempted && p.cancelCooldown > 0 {
				// Client disconnected after an upstream transport attempt started. Hold
				// the slot briefly to prevent N+1 observed concurrency from
				// downstream accounting lag. Unlike
				// phantom penalty and failure hold, the cancelCooldown does NOT
				// use the isTransportOrProxyError guard — it MUST fire even when
				// rec.status == 0 (upstream still processing). See
				// serveLimited's cancel-cooldown comment for the full rationale.
				time.AfterFunc(p.cancelCooldown, release)
				return
			}
			release()
		}()

		// Record the end of the queue phase so QueueDuration reflects
		// global limiter wait time, not zero.
		if rec, ok := w.(*statusRecorder); ok && rec.entry != nil {
			rec.entry.Timing.QueueEnd = time.Now()
		}

		p.m.IncActive()
		defer p.m.DecActive()
	}

	p.m.MarkInFlightStarted(flightID)

	// Capture the time the upstream request begins so we can pass it to
	// RecordFailure as startedAt. This enables stale-request protection:
	// if the circuit cycles through OPEN→HALF_OPEN while the request is
	// in flight, a failure from a request started before the OPEN period
	// is ignored and does not falsely trip HALF_OPEN→OPEN.
	proxyStart := time.Now()
	p.installSwitchingProtocolsProbeHooks(w, retryAttempt, proxyStart, breakerEpoch)

	// Inner panic recovery: catch panics from the inner transport
	// so that the slot-release defer and breaker reporting see the correct
	// status code. A local panic is NOT an upstream failure — the
	// localPanic flag prevents phantom penalty, failure hold, and
	// breaker recording from treating a proxy-internal crash as a
	// downstream error.
	func() {
		defer func() {
			if rv := recover(); rv != nil {
				if rv == http.ErrAbortHandler {
					if p.handleAbortHandler(w, r, retryAttempt, proxyStart, breakerEpoch) {
						upstreamAbortFailure = true
					} else {
						p.cancelSuppressedAbortProbe(retryAttempt, proxyStart, breakerEpoch)
					}
					// Classify before re-panicking so the slot-release defer
					// still sees the real exchange result during unwinding.
					rec, _ := w.(*statusRecorder)
					result = p.classifyExchange(rec, r, retryAttempt, attemptState.started.Load(), upstreamAbortFailure, breakerEpoch, time.Now())
					panic(rv)
				}
				slog.Error("proxy panic in servePassthrough", "panic", rv)
				if rec, ok := w.(*statusRecorder); ok {
					rec.recoverLocalPanic()
				}
				localPanic = true
			}
		}()
		nr, handler := p.lookupRouteTargets(r)
		if nr != nil {
			p.dispatchAfterNativeRoute(w, r, nr, handler)
		} else if handler != nil {
			p.serveTranscodeHandler(w, r, handler)
		} else {
			p.inner.ServeHTTP(w, r)
		}
	}()
	if p.handleSuppressedAbort(w, r, attemptState.started.Load(), retryAttempt, proxyStart, breakerEpoch) {
		upstreamAbortFailure = true
	}
	// One immutable per-exchange classification, produced after the handler
	// finishes. It drives the slot-release defer, breaker resolution,
	// adaptive action, completion accounting, and journal finalization and
	// is never recomputed.
	rec, recOK := w.(*statusRecorder)
	result = p.classifyExchange(rec, r, retryAttempt, attemptState.started.Load(), upstreamAbortFailure, breakerEpoch, time.Now())
	if !attemptState.started.Load() {
		if p.breaker != nil {
			p.breaker.CancelProbe(breakerEpoch)
		}
		return
	}
	if recOK && rec.aborted {
		// An aborted exchange is never a clean completion: it does not
		// reach the completion counters or the journal's clean-completion
		// path. The breaker is resolved from the classification: a
		// definitive upstream failure retained in the result is recorded
		// (mirroring the native abort path); everything else cancels the
		// probe. A LOCAL panic is not an upstream failure: its
		// proxy-generated 502 must never fail the breaker (the
		// localPanic guard preserves that).
		if !localPanic {
			p.resolveAbortedExchange(result, retryAttempt, proxyStart, breakerEpoch, upstreamAbortFailure)
		} else {
			p.cancelSuppressedAbortProbe(retryAttempt, proxyStart, breakerEpoch)
		}
		return
	}

	// Feed failure/success signals to the circuit breaker from the
	// immutable exchange result. Without retries, the proxy reports the
	// whole exchange. With retry-enabled breaker reporting, the retry
	// transport reports failures immediately (attempt.FailureRecorded) but
	// defers 2xx success via the request context; the proxy records that
	// success only after ReverseProxy has copied the response body without
	// an abort.
	if p.breaker != nil && !localPanic {
		p.resolveBreakerResult(result, retryAttempt, proxyStart, breakerEpoch)
	}
	// The clean-completion counter publishes in finalize (after the journal
	// entry). The admission flag is set only after
	// the breaker logic above: a panic in it marks the exchange aborted and
	// skips the counter — the completion counter is never double-counted.
	if rec != nil {
		rec.admissionCompleted = true
	}
}
