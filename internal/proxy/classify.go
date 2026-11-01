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
	"net/http"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/retry"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func isUpstreamFailureStatus(rec *statusRecorder, now time.Time, ctxErr error) bool {
	if rec == nil {
		// Without a recorder there is no upstream observation to classify,
		// and a local defect must never open the breaker.
		return false
	}
	// A proxy-generated error WITHOUT a real transport error behind it never
	// saw the upstream: it is a local defect, not an upstream health signal
	// (panic 502, queue rejection, the native path's local 5xx). A 502 that
	// wraps a genuine transport error (rec.transportErr) still classifies as
	// upstream — hasRealProxyTransportError names exactly that shape.
	if rec.proxyGeneratedError && !rec.hasRealProxyTransportError(ctxErr) {
		return false
	}
	if rec.localUpgradeFailure || rec.retryCircuitOpen() {
		return false
	}
	status := rec.observedStatus()
	// When the client cancelled the context and the upstream returned a
	// clean 2xx response, this is a suppressible client abort — the
	// upstream succeeded and the client just disconnected. Classifying
	// it as an upstream failure would incorrectly trip the breaker.
	if ctxErr != nil && status >= 200 && status < 300 {
		return false
	}
	return circuitbreaker.IsFailureStatusWithHeaders(status, responseHeaders(rec), rec.responseAt, now)
}

// exchangeResult is the immutable per-exchange classification produced after
// the handler finishes. One value drives breaker resolution, slot holds,
// adaptive action, completion counters, and journal finalization for both
// admission paths. It is never recomputed or reinterpreted.
type exchangeResult struct {
	// upstreamAttempted reports whether an upstream transport attempt
	// started (the transport-level fact, tracked for both admission paths).
	upstreamAttempted bool

	// upstreamStatus is the definitive upstream response status (0 when no
	// response was received).
	upstreamStatus int

	// upstreamFailure reports a definitive upstream failure: an HTTP
	// failure status, a transport error, a truncated upstream body, or an
	// abort caused by an upstream body error.
	upstreamFailure bool

	// upstreamSuccess reports a definitive upstream success (2xx, or a
	// HALF_OPEN probe resolved by a non-failure status). A cancelled 2xx is
	// neither success nor failure.
	upstreamSuccess bool

	// retryAfter is the upstream-signaled Retry-After (0 when absent). For
	// transcoded exchanges it comes from the ORIGINAL upstream response,
	// never from the rendered client error.
	retryAfter time.Duration

	// clientAborted reports that the client disconnected before the
	// exchange completed (context cancellation or a failed downstream
	// write).
	clientAborted bool

	// localFailure reports a proxy-local failure (conversion error,
	// proxy-generated error): not upstream-health input.
	localFailure bool

	// switchingProtocolsResolved reports that a 101 switching-protocols
	// response had its probe resolved by the handshake hooks; the hooks
	// already recorded the outcome and the breaker section must not
	// re-record it.
	switchingProtocolsResolved bool

	// suppressibleAbort reports a client abort with no definitive upstream
	// failure signal: the exchange must not drive a phantom hold or a
	// breaker failure.
	suppressibleAbort bool
}

// classifyExchange produces the immutable per-exchange result. Transcode
// exchanges classify from the explicit outcome provenance (plus recorder
// write state); native exchanges normalize the existing recorder facts into
// the same structure with their exact legacy semantics.
func (p *Proxy) classifyExchange(
	rec *statusRecorder,
	r *http.Request,
	retryAttempt *retry.BreakerAttempt,
	upstreamAttempted bool,
	upstreamAbortFailure bool,
	breakerEpoch uint64,
	now time.Time,
) exchangeResult {
	if rec != nil && rec.transcodeOutcome != nil {
		return classifyTranscodeExchange(rec, upstreamAttempted)
	}
	return classifyNativeExchange(rec, r, retryAttempt, upstreamAttempted, upstreamAbortFailure, breakerEpoch, now)
}

// classifyTranscodeExchange classifies a transcoded exchange from the
// explicit outcome provenance. Upstream failure, client abort, downstream
// completion, and Retry-After are independent dimensions of the outcome.
func classifyTranscodeExchange(
	rec *statusRecorder,
	upstreamAttempted bool,
) exchangeResult {
	outcome := rec.transcodeOutcome
	// The attempt marker is inside SigningTransport, so it only fires after
	// signing succeeds and the configured round trip is about to begin. The
	// outcome fact still covers exchanges whose handler recorded an attempt
	// but whose transport never returned a response.
	attempted := outcome.UpstreamAttempted ||
		(upstreamAttempted && outcome.Provenance != transcode.ProvenanceLocalRequestConversionError)
	result := exchangeResult{
		upstreamAttempted: attempted,
		upstreamFailure:   outcome.UpstreamFailure,
		clientAborted:     outcome.ClientAborted || outcome.Provenance == transcode.ProvenanceClientAbort,
	}
	if outcome.UpstreamStatus.Set {
		result.upstreamStatus = outcome.UpstreamStatus.Value
	}
	switch outcome.Provenance {
	case transcode.ProvenanceLocalRequestConversionError,
		transcode.ProvenanceLocalResponseConversionError,
		transcode.ProvenanceLocalStreamValidationError,
		transcode.ProvenanceDownstreamWriteError:
		result.localFailure = true
	}
	result.localFailure = result.localFailure || outcome.LocalFailure
	// A client-aborted exchange is never breaker SUCCESS, even when the
	// upstream answered 2xx: a client is not a health probe, and crediting a
	// partial stream could reset a real failure streak. This mirrors the
	// native path's !clientAborted guard so classification is self-contained
	// and does not depend on the recorder's independent rec.aborted coupling
	// in serveTranscodeHandler. A local failure (including a downstream write
	// that failed mid-stream) is excluded for the same reason: the upstream's
	// 2xx is retained as an observed fact, but the exchange did not succeed.
	// Success also needs the whole response delivered: a delivery the recorder
	// saw fail, or one not completed, leaves the upstream's status observed but
	// does not make the exchange a success.
	result.upstreamSuccess = !result.upstreamFailure &&
		!result.localFailure &&
		!result.clientAborted &&
		!rec.downstreamWriteFailed() &&
		outcome.DownstreamComplete &&
		outcome.UpstreamStatus.Set &&
		outcome.UpstreamStatus.Value >= http.StatusOK &&
		outcome.UpstreamStatus.Value < http.StatusMultipleChoices
	// Retry-After is anchored at the ORIGINAL header receipt in the
	// outcome; the translated downstream header is NEVER re-parsed with a
	// fresh receipt timestamp.
	if outcome.RetryAfter.Set {
		result.retryAfter = outcome.RetryAfter.Value
	}
	// A client abort suppresses the phantom hold and breaker failure only
	// when the outcome carries no definitive upstream failure signal —
	// mirroring the native suppressible-abort guard, where a definitive
	// upstream status (429, 5xx, or a rate-signalled 403 classified from
	// the ORIGINAL upstream headers) is recorded even if the client
	// disconnects while the translated error is written.
	result.suppressibleAbort = result.clientAborted && !outcome.UpstreamFailure
	return result
}

// classifyNativeExchange normalizes the existing recorder facts into the
// exchange result with their exact legacy semantics. breakerEpoch is the
// probe epoch from Allow(); it is passed to isBreakerSuccessStatus so a
// resolved 101 switching-protocols handshake is not double-recorded.
func classifyNativeExchange(
	rec *statusRecorder,
	r *http.Request,
	retryAttempt *retry.BreakerAttempt,
	upstreamAttempted bool,
	upstreamAbortFailure bool,
	breakerEpoch uint64,
	now time.Time,
) exchangeResult {
	result := exchangeResult{
		upstreamAttempted: upstreamAttempted,
		clientAborted: isContextCancellation(r.Context().Err()) ||
			(rec != nil && rec.downstreamWriteFailed()),
	}
	if rec == nil {
		return result
	}
	ctxErr := r.Context().Err()
	epoch := breakerEpoch
	if retryAttempt != nil && retryAttempt.Epoch != 0 {
		epoch = retryAttempt.Epoch
	}
	result.upstreamStatus = rec.observedStatus()
	result.upstreamFailure = isUpstreamFailureStatus(rec, now, ctxErr) || upstreamAbortFailure
	result.switchingProtocolsResolved = rec.status == http.StatusSwitchingProtocols &&
		rec.switchingProtocolsProbeResolved.Load()
	// A cancelled 2xx is neither success nor failure: the upstream completed
	// but the client abandoned the response. Recording it as a success could
	// reset an existing failure streak.
	result.upstreamSuccess = !result.upstreamFailure && !result.clientAborted &&
		isBreakerSuccessStatus(rec, now, epoch, ctxErr)
	result.retryAfter = parseRetryAfterFromRecorder(rec, now)
	// proxyGeneratedError marks an error this proxy produced — the 502 panic
	// handler, queue rejection, and the native path's own 5xx local errors.
	// When it wraps a genuine transport error the upstream was attempted and
	// failed, so that exchange is an upstream failure, not a local one: the
	// proxy merely reported what the transport observed. A local upgrade
	// handoff failure is the converse — the upstream may have answered, but
	// the proxy could not deliver the upgrade, so it never speaks to upstream
	// health and is classified as a local failure.
	result.localFailure = (rec.proxyGeneratedError && !rec.hasRealProxyTransportError(ctxErr)) ||
		rec.localUpgradeFailure
	result.suppressibleAbort = rec.suppressibleClientAbort(ctxErr)
	return result
}

// resolveAbortedExchange resolves the breaker for an aborted exchange. An
// aborted exchange never reaches the completion counters or the journal's
// clean-completion path; the breaker is resolved from the classification — a
// definitive upstream failure retained in the result is recorded (mirroring
// the native abort path), everything else cancels the probe.
func (p *Proxy) resolveAbortedExchange(
	result exchangeResult,
	attempt *retry.BreakerAttempt,
	startedAt time.Time,
	epoch uint64,
	upstreamAbortFailure bool,
) {
	if p.breaker == nil {
		return
	}
	if upstreamAbortFailure {
		// handleAbortHandler already recorded the failure and marked the
		// attempt; the probe resolution stands.
		return
	}
	if result.upstreamFailure && !result.suppressibleAbort {
		p.recordFailureOnce(result, attempt, startedAt, epoch)
		return
	}
	attemptStart, attemptEpoch := retryAttemptOrDefault(attempt, startedAt, epoch)
	// An aborted exchange resolves a probe only from a definitive non-2xx status:
	// an aborted 2xx transfer was not delivered in full, so it is neither
	// success nor failure.
	if attemptEpoch != 0 && !result.clientAborted && result.upstreamStatus >= http.StatusMultipleChoices &&
		!result.upstreamFailure && !result.localFailure && !result.suppressibleAbort &&
		!result.switchingProtocolsResolved {
		p.breaker.RecordSuccess(attemptStart, attemptEpoch)
		return
	}
	p.cancelSuppressedAbortProbe(attempt, startedAt, epoch)
}

// resolveBreakerResult feeds the immutable exchange result into the circuit
// breaker. Only definitive upstream outcomes mutate breaker health:
//
//   - upstream failures (HTTP >= 500, 429, rate-signalled 403, transport and
//     body errors) are failures;
//   - upstream successes (2xx, completed streams, resolved HALF_OPEN probes)
//     are successes;
//   - client aborts, local failures, and suppressed aborts are neither
//     success nor failure — a cancelled 2xx stream must not reset a failure
//     streak, and a local conversion error is not an upstream failure.
//
// The retry transport records HTTP-level failures immediately
// (attempt.FailureRecorded); that resolution is authoritative and is never
// duplicated here. Success is always recorded here: the retry transport
// defers 2xx success to the proxy.
func (p *Proxy) resolveBreakerResult(
	result exchangeResult,
	attempt *retry.BreakerAttempt,
	startedAt time.Time,
	epoch uint64,
) {
	if p.breaker == nil {
		return
	}
	attemptStart, attemptEpoch := retryAttemptOrDefault(attempt, startedAt, epoch)
	switch {
	case result.upstreamFailure && !result.suppressibleAbort:
		p.recordFailureOnce(result, attempt, startedAt, epoch)
	case result.upstreamSuccess && !result.suppressibleAbort:
		p.breaker.RecordSuccess(attemptStart, attemptEpoch)
	case attemptEpoch != 0 && result.upstreamStatus > 0 &&
		!result.upstreamFailure && !result.localFailure && !result.suppressibleAbort &&
		!result.switchingProtocolsResolved:
		// A HALF_OPEN probe must be resolved by every definitive upstream
		// response; non-failure statuses prove the upstream answered even if
		// they are not ordinary CLOSED-state 2xx successes. A resolved 101
		// handshake is excluded: the switching-protocols hooks already
		// recorded its outcome.
		p.breaker.RecordSuccess(attemptStart, attemptEpoch)
	default:
		if attempt != nil && attempt.FailureRecorded {
			return
		}
		p.breaker.CancelProbe(attemptEpoch)
	}
}

// recordFailureOnce records an upstream failure unless the retry transport
// already recorded it at the HTTP level. Only transports that own breaker
// reporting set the flag meaningfully; a retry transport without a breaker
// sets FailureRecorded without recording, and the proxy must still report.
func (p *Proxy) recordFailureOnce(
	result exchangeResult,
	attempt *retry.BreakerAttempt,
	startedAt time.Time,
	epoch uint64,
) {
	if p.breaker == nil {
		return
	}
	if p.retryHandlesBreaker && attempt != nil && attempt.FailureRecorded {
		return
	}
	attemptStart, attemptEpoch := retryAttemptOrDefault(attempt, startedAt, epoch)
	p.breaker.RecordFailure(result.upstreamStatus, result.retryAfter, attemptStart, attemptEpoch)
}

func isBreakerSuccessStatus(rec *statusRecorder, now time.Time, epoch uint64, ctxErr error) bool {
	if rec == nil || rec.proxyTransportOrGeneratedError() {
		return false
	}
	if rec.status == http.StatusSwitchingProtocols && rec.switchingProtocolsProbeResolved.Load() {
		return false
	}
	status := rec.observedStatus()
	if status >= 200 && status < 300 {
		return true
	}
	// A HALF_OPEN probe must be resolved by every definitive upstream response.
	// Clean 101 upgrades are successful HTTP handshakes, and other non-failure
	// statuses (for example a bare auth 403) prove the upstream answered even if
	// they are not counted as ordinary CLOSED-state 2xx successes.
	if epoch != 0 && status > 0 && !isUpstreamFailureStatus(rec, now, ctxErr) {
		return true
	}
	return false
}

func retryAttemptOrDefault(attempt *retry.BreakerAttempt, startedAt time.Time, epoch uint64) (time.Time, uint64) {
	if attempt != nil && !attempt.StartedAt.IsZero() {
		return attempt.StartedAt, attempt.Epoch
	}
	return startedAt, epoch
}

func (p *Proxy) withRetryAttemptContext(ctx context.Context, attempt **retry.BreakerAttempt) context.Context {
	if !p.retryTracksAttempts {
		return ctx
	}
	*attempt = &retry.BreakerAttempt{}
	if p.retryHandlesBreaker {
		ctx = retry.WithDeferredBreakerSuccess(ctx)
	}
	return retry.WithBreakerAttempt(ctx, *attempt)
}

func (p *Proxy) cancelSuppressedAbortProbe(attempt *retry.BreakerAttempt, startedAt time.Time, epoch uint64) {
	if p.breaker == nil {
		return
	}
	if retryAttemptHasCurrentFailure(attempt) {
		return
	}
	_, attemptEpoch := retryAttemptOrDefault(attempt, startedAt, epoch)
	p.breaker.CancelProbe(attemptEpoch)
}

func retryAttemptHasCurrentFailure(attempt *retry.BreakerAttempt) bool {
	return attempt != nil && attempt.FailureRecorded
}

func retryAttemptFailureForSlot(attempt *retry.BreakerAttempt, historicalFailureCanDriveSlot bool) bool {
	if attempt == nil {
		return false
	}
	if attempt.FailureRecorded {
		return true
	}
	return historicalFailureCanDriveSlot && attempt.AnyFailureRecorded
}

func retryHistoricalFailureCanDriveSlot(rec *statusRecorder, finalUnclean bool) bool {
	if !finalUnclean {
		return false
	}
	if rec == nil {
		return true
	}
	if rec.localUpgradeFailure {
		return false
	}
	if rec.observedStatus() > 0 && !rec.proxyTransportOrGeneratedError() {
		return false
	}
	return true
}

func (p *Proxy) handleAbortHandler(w http.ResponseWriter, r *http.Request, attempt *retry.BreakerAttempt, startedAt time.Time, epoch uint64) bool {
	recordStartedAt, recordEpoch := retryAttemptOrDefault(attempt, startedAt, epoch)
	rec, _ := w.(*statusRecorder)
	now := time.Now()

	// Definitive upstream failure statuses are facts independent of how the copy
	// later aborted. Preserve them even if the client disconnects while the body
	// is being written.
	if rec != nil {
		if rec.hasRealProxyTransportError(r.Context().Err()) {
			p.recordAbortFailure(w, attempt, recordStartedAt, recordEpoch, r.Context().Err())
			return true
		}
		isTransportOrProxyError := rec.proxyTransportOrGeneratedError()
		if !isTransportOrProxyError && isUpstreamFailureStatus(rec, now, r.Context().Err()) {
			p.recordAbortFailure(w, attempt, recordStartedAt, recordEpoch, r.Context().Err())
			return true
		}
	}

	if upstreamErr := copyErrorStateFromContext(r.Context()).upstreamError(); upstreamErr != nil {
		// If the only upstream-body read error is the request context being
		// canceled, this is still a client abort. Non-context body read errors are
		// unclean upstream transfers and must not be hidden by a simultaneous client
		// cancellation race.
		if isContextCancellation(upstreamErr) && isContextCancellation(r.Context().Err()) {
			return false
		}
		p.recordAbortFailure(w, attempt, recordStartedAt, recordEpoch, r.Context().Err())
		return true
	}

	if rec != nil {
		// A downstream write error or short write is client-side pressure, not an
		// upstream body failure. Treat it as a client abort even if the request
		// context has not been canceled yet; net/http may observe those events in
		// either order depending on protocol and wrappers. This check intentionally
		// follows upstreamError: Go's copy loop can observe n > 0 with a read error,
		// then fail writing those same bytes. The upstream read fact must win.
		if rec.downstreamWriteFailed() {
			return false
		}
	}

	if isContextCancellation(r.Context().Err()) {
		return false
	}

	p.recordAbortFailure(w, attempt, recordStartedAt, recordEpoch, r.Context().Err())
	return true
}

func (p *Proxy) handleSuppressedAbort(w http.ResponseWriter, r *http.Request, reachedUpstream bool, attempt *retry.BreakerAttempt, startedAt time.Time, epoch uint64) bool {
	rec, _ := w.(*statusRecorder)
	if rec == nil {
		return false
	}
	if rec.transcodeOutcome != nil {
		// A transcoded exchange records explicit outcome provenance after the
		// handler returns. Its upstream-body reads are tracked in the shared
		// copy error state, so a client cancellation that interrupts the
		// stream would look like a copy failure here. The explicit outcome
		// (ClientAbort, upstream failure, success) is authoritative; the
		// legacy copy-error abort accounting must not override it.
		return false
	}
	copyFailed := copyErrorStateFromContext(r.Context()).upstreamError() != nil || rec.downstreamWriteFailed()
	transportCanceled := rec.transportErr != nil && isContextCancellation(r.Context().Err()) && isContextCancellation(rec.transportErr)
	if !copyFailed && !transportCanceled {
		return false
	}
	rec.aborted = true
	if !reachedUpstream {
		return false
	}
	return p.handleAbortHandler(w, r, attempt, startedAt, epoch)
}

func (p *Proxy) recordAbortFailure(w http.ResponseWriter, attempt *retry.BreakerAttempt, startedAt time.Time, epoch uint64, ctxErr error) {
	if p.breaker == nil {
		return
	}

	rec, _ := w.(*statusRecorder)
	now := time.Now()
	status := 0
	var retryAfter time.Duration
	// Only skip when the retry transport itself owns breaker reporting and
	// already recorded this failure. A retry transport WITHOUT a breaker
	// sets FailureRecorded without recording to any breaker; the proxy must
	// still report the definitive upstream failure.
	if p.retryHandlesBreaker && attempt != nil && attempt.FailureRecorded {
		return
	}
	if rec != nil && !rec.proxyTransportOrGeneratedError() && isUpstreamFailureStatus(rec, now, ctxErr) {
		status = rec.observedStatus()
		retryAfter = parseRetryAfterFromRecorder(rec, now)
	}

	p.breaker.RecordFailure(status, retryAfter, startedAt, epoch)
	if attempt != nil {
		attempt.FailureRecorded = true
		attempt.AnyFailureRecorded = true
	}
}
