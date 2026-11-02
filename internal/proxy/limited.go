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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/retry"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func (p *Proxy) serveLimited(w http.ResponseWriter, r *http.Request, flightID uint64) {
	ctx := r.Context()
	var upstreamAbortFailure bool
	var retryAttempt *retry.BreakerAttempt
	copyState := &copyErrorState{}
	ctx = withCopyErrorState(ctx, copyState)
	r = r.WithContext(ctx)

	// result is the immutable per-exchange classification produced after the
	// handler finishes. It is assigned before the slot-release defer can run
	// and consumed by the defer, the breaker resolution, and the completion
	// counters — never recomputed.
	var result exchangeResult
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	// Reject a request the breaker would refuse before reading its body. The
	// peek changes no breaker state: admission itself (and a HALF_OPEN probe
	// epoch) is taken by the single Allow below, after the body inspection,
	// so neither a slow upload nor a queue-depth rejection can claim,
	// supersede, or cancel a recovery probe.
	if p.breaker != nil && !p.breaker.WouldAllow() {
		p.m.IncCircuitRejected()
		http.Error(w, "circuit open", http.StatusServiceUnavailable)
		return
	}

	// Bounded-queue admission: when a depth limit is configured,
	// a request arriving while the effective limiter already holds the
	// bound in waiters fails fast with 429 + Retry-After — the protocol
	// signal AI clients and the official SDKs already handle — instead of
	// queueing silently and amplifying into client-side retries. The check
	// mirrors acquireSlot's limiter selection (first-matching pattern's
	// route limiter, else the default pool, then the global limiter) so the
	// bound applies to the queue the request would actually join. The
	// waiters count is a point-in-time snapshot: a burst may overshoot the
	// bound slightly, which is the documented soft-bound semantics.
	if p.queueDepthLimit > 0 {
		var effective *queue.Limiter
		if pat := p.matcher.FindMatch(r.Method, r.URL.Path); pat != nil {
			key := pat.Group
			if key == "" {
				key = pat.Raw
			}
			if lim, ok := p.routeLimiters[key]; ok {
				effective = lim
			}
		}
		if effective == nil {
			effective = p.limiter
		}
		if effective != nil {
			if waiters := effective.Stats().Waiters; waiters >= int64(p.queueDepthLimit) {
				p.m.IncQueueRejected()
				writeQueueRejected(w, p.queueDepthLimit, waiters)
				return
			}
		}
		if p.globalLimiter != nil {
			if waiters := p.globalLimiter.Stats().Waiters; waiters >= int64(p.queueDepthLimit) {
				p.m.IncQueueRejected()
				writeQueueRejected(w, p.queueDepthLimit, waiters)
				return
			}
		}
	}

	// SSE queue comments (opt-in): commit the streaming
	// representation BEFORE admission and keep the client's connection
	// visibly alive while it waits. Eligibility requires the client's
	// Accept header to select text/event-stream — the same signal the
	// transcode layer uses as its default stream intent — so an exchange
	// locked to the streaming representation is one that asked for it.
	// The comments stop before the downstream flow writes the real
	// response; while queued, the ticker goroutine is the only writer.
	//
	// Writing response bytes while the request body is still unconsumed
	// makes the HTTP/1.1 server drain-and-close that body before the first
	// flush (net/http writeHeader — the behavior behind Issue 15527) unless
	// the handler enabled full duplex; the post-admission upstream send
	// would then fail with "invalid Read on closed Body" (wire regression
	// 2026-09-08, pinned by TestProxy_QueueCommentsPreserveRequestBody). A
	// body-carrying request therefore requires a full-duplex writer; one
	// that cannot (recorders, exotic embedders) falls back to the
	// documented silent-queuing semantics instead of corrupting the body.
	//
	// Only the eligibility is decided here; the commit itself happens after
	// the breaker admits the request, so a breaker rejection is always a
	// plain 503 and never a committed stream with a failed close.
	rec, recIsRecorder := w.(*statusRecorder)
	var stopComments func()
	commitComments := false
	if p.queueComments > 0 {
		if recIsRecorder && queueCommentsEligible(r) && transcode.AcceptIsEventStream(r.Header.Get("Accept")) {
			if canCommitCommentsWithUnreadBody(r) || http.NewResponseController(rec).EnableFullDuplex() == nil {
				commitComments = !bodyBlocksStreamCommit(w, r, ctx)
			}
		}
	}
	if ctx.Err() != nil {
		// The admission deadline (or the client) ended the request during
		// the body inspection: answer exactly as an expired queue wait,
		// without consulting the breaker. Admission ended here, so the
		// journal's queue wait ends here too.
		if recIsRecorder && rec.entry != nil {
			rec.entry.Timing.QueueEnd = time.Now()
		}
		p.writeAdmissionFailure(w, ctx, nil, rec)
		return
	}

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

	if commitComments {
		stopComments = p.startQueueComments(rec)
		// Tell a downstream transcode handler the streaming
		// representation is committed: its error writers then
		// failed-close with dialect-legal SSE error events
		// instead of raw JSON bodies the committed stream
		// cannot carry.
		r = r.WithContext(transcode.WithCommittedStreamContext(r.Context()))
	}

	p.m.IncQueued()
	// Record the start of the queue phase precisely at the moment
	// the request begins waiting for a limiter slot, overriding
	// the coarse ServeHTTP-entry timestamp so QueueDuration
	// measures only actual queue wait time.
	if rec, ok := w.(*statusRecorder); ok && rec.entry != nil {
		rec.entry.Timing.QueueStart = time.Now()
	}
	release, slotLimiter, err := p.acquireSlot(ctx, r.Method, r.URL.Path)

	if err != nil {
		p.m.DecQueued()
		// Record the moment the queue wait ended so that QueueDuration
		// reflects the actual time spent waiting, not zero.
		if recIsRecorder && rec.entry != nil {
			rec.entry.Timing.QueueEnd = time.Now()
		}
		if p.breaker != nil {
			p.breaker.CancelProbe(breakerEpoch)
		}
		p.writeAdmissionFailure(w, ctx, stopComments, rec)
		return
	}
	// The comments keep flowing through the GLOBAL limiter wait below —
	// the request is not fully admitted until both the route limiter and
	// the global limiter have granted a slot. Stopping here would leave a
	// client behind a full global queue with stopped comments and resumed
	// silence (exactly the UX the mode exists to prevent). The global
	// error path stops them; the success path stops them after the
	// global acquire.

	// Phantom concurrency penalty: hold the slot after a qualifying
	// UPSTREAM failure to prevent exceeding downstream concurrency limits.
	// The penalty is NOT tied to the client connection context — a
	// malicious or impatient client could otherwise bypass the hold by
	// disconnecting immediately after receiving the error response.
	// The penalty must NOT be applied to self-induced errors (circuit-open
	// rejection, queue timeout) — only to failures that originated from
	// the upstream service.
	//
	// Client-initiated cancellations are suppressed ONLY when the status
	// code is ambiguous (0 = no response, 502 = proxy-generated error).
	// When a client disconnects, the reverse proxy may abort without
	// calling WriteHeader, leaving rec.status=0, or it may write 502 via
	// its error handler. Both are transport/proxy artifacts — NOT upstream
	// failures. Since IsFailureStatus(0)==true and IsFailureStatus(502)==true,
	// the phantom penalty would fire erroneously without the guard.
	//
	// Crucially, if the upstream DID return a definitive failure (5xx, 429,
	// or rate-limit-signaled 403) before the client disconnected, rec.status
	// will be that upstream code
	// (not 0 or 502) and the penalty MUST be applied — the upstream is
	// genuinely failing, and bypassing the penalty would allow an attacker
	// to rapidly recycle slots and hammer a degraded downstream. This
	// mirrors the breaker reporting logic which uses the same
	// isTransportOrProxyError guard.
	//
	// The slot is released asynchronously so the HTTP handler goroutine
	// and TCP connection are freed immediately after the response is sent,
	// rather than blocking for the penalty duration (up to MaxPenalty).
	var reachedUpstream bool
	attemptState := &upstreamAttemptState{}
	var localPanic bool // set by inner recovery when a local panic (not upstream failure) occurs
	var globalRelease func()
	releaseSlots := func() {
		if globalRelease != nil {
			globalRelease()
		}
		release()
	}
	defer func() {
		// A local panic (e.g., nil pointer in proxy code) is NOT an upstream
		// failure — do NOT apply phantom penalty, failure hold, or cancel
		// cooldown. The 502 written by the inner recovery reflects a local
		// bug, not a downstream error, so holding the slot would penalize
		// legitimate traffic for a proxy-internal problem.
		if localPanic {
			releaseSlots()
			return
		}
		rec, recOK := w.(*statusRecorder)

		// Adaptive headroom: a 429 from the upstream is evidence that the
		// provider observed more concurrent load than the proxy intended.
		// Temporarily reduce the effective limit of the limiter that owns this
		// slot by one, creating breathing room for teardown/accounting races.
		reachedUpstream = result.upstreamAttempted
		if p.adaptiveHeadroom && recOK && reachedUpstream && slotLimiter != nil && result.upstreamStatus == http.StatusTooManyRequests {
			slotLimiter.AdaptiveReduce(p.adaptiveHeadroomWindow)
		}

		ctxErr := r.Context().Err()
		isClientCancel := isContextCancellation(ctxErr) || (recOK && rec.downstreamWriteFailed())
		// Only suppress phantom penalty/failureHold when the client cancelled
		// AND the outcome is ambiguous (transport error or proxy-generated 502).
		// If the upstream returned a definitive 5xx/429 before the client
		// disconnected, the penalty must apply — the upstream is genuinely
		// failing. See the comment block above for the full rationale.
		finalUnclean := isClientCancel || (recOK && rec.aborted)
		retryRecordedFailure := retryAttemptFailureForSlot(retryAttempt, retryHistoricalFailureCanDriveSlot(rec, finalUnclean))
		suppressFailureForClientAbort := result.suppressibleAbort && !retryRecordedFailure
		// Single anchor for classification decisions in this defer: the
		// immutable per-exchange result, plus the slot-specific retry-recorded
		// failure fact.
		upstreamFailure := result.upstreamFailure || retryRecordedFailure
		if p.breaker != nil && result.upstreamAttempted && !suppressFailureForClientAbort && upstreamFailure {
			time.AfterFunc(p.breaker.PenaltyDuration(), releaseSlots)
			return
		}
		if p.failureHold > 0 && result.upstreamAttempted && !suppressFailureForClientAbort && upstreamFailure {
			// Standalone failure hold: holds the slot after an upstream failure.
			// This applies only when the circuit breaker is disabled (the
			// phantom penalty branch above returns first). Uses the same
			// suppressible-client-abort guard as the phantom penalty branch:
			// client cancels with ambiguous status codes are suppressed;
			// definitive upstream/proxy transport failures are not.
			time.AfterFunc(p.failureHold, releaseSlots)
			return
		}
		// The slot must stay held while the upstream finishes accounting for a
		// connection the proxy closed underneath it. Two shapes qualify: the
		// client left after an upstream attempt started, or the upstream
		// produced a response the proxy could not deliver and then tore it down
		// (upstreamHandoffAbandoned). Unlike phantom penalty and failure hold,
		// this branch does NOT use the isTransportOrProxyError guard — it MUST
		// fire even when rec.status == 0 (upstream still processing,
		// WriteHeader not yet called). When the upstream hasn't responded yet,
		// it is still actively computing the abandoned request, so releasing
		// the slot immediately would allow an attacker to exhaust upstream
		// concurrency by rapidly opening and dropping connections. Correctness
		// here must not depend on the limiter's post-release cooldown: an
		// operator may set -release-cooldown 0, and then an immediately
		// retryable failure would collide with the session the upstream is
		// still tearing down.
		handoffAbandoned := recOK && rec.upstreamHandoffAbandoned()
		if (isClientCancel || handoffAbandoned) && result.upstreamAttempted && p.cancelCooldown > 0 {
			time.AfterFunc(p.cancelCooldown, releaseSlots)
			return
		}
		releaseSlots()
	}()

	if p.globalLimiter != nil {
		var err error
		globalRelease, err = p.globalLimiter.Acquire(ctx)
		if err != nil {
			p.m.DecQueued()
			// Record the moment the global limiter wait ended so that
			// QueueDuration reflects the full queue time, not zero.
			if recIsRecorder && rec.entry != nil {
				rec.entry.Timing.QueueEnd = time.Now()
			}
			if p.breaker != nil {
				p.breaker.CancelProbe(breakerEpoch)
			}
			p.writeAdmissionFailure(w, ctx, stopComments, rec)
			return
		}
	}

	// All admission complete (per-route + global limiter): stop the queue
	// comments and take over the ResponseWriter before the downstream flow
	// writes the real response (the ticker goroutine must never interleave
	// with it).
	if stopComments != nil {
		stopComments()
	}

	// Record the moment the slot was acquired — this is the end of queuing.
	// QueueEnd is set AFTER both the per-route limiter and the global
	// limiter so that global queue latency is NOT absorbed into TTFB.
	if rec, ok := w.(*statusRecorder); ok && rec.entry != nil {
		rec.entry.Timing.QueueEnd = time.Now()
	}

	p.m.IncActive()
	defer p.m.DecActive()

	// Mark started before decrementing the queued gauge so a concurrent
	// snapshot never observes aggregate 0 with a per-route waiter still
	// present: the transient is an overcount (1 vs 0), never an undercount.
	p.m.MarkInFlightStarted(flightID)
	p.m.DecQueued()

	// Capture the time the upstream request begins so we can pass it to
	// RecordFailure as startedAt. This enables stale-request protection:
	// if the circuit cycles through OPEN→HALF_OPEN while the request is
	// in flight, a failure from a request started before the OPEN period
	// is ignored and does not falsely trip HALF_OPEN→OPEN.
	proxyStart := time.Now()
	r = r.WithContext(withUpstreamAttemptState(r.Context(), attemptState))
	p.installSwitchingProtocolsProbeHooks(w, retryAttempt, proxyStart, breakerEpoch)

	// Inner panic recovery: catch panics from the inner transport
	// (e.g., httputil.ReverseProxy or the retry transport) so that
	// the phantom penalty defer in this function sees the correct
	// status code (502, not 0). Without this, the phantom penalty
	// defer runs during panic unwinding BEFORE the outer recovery
	// in ServeHTTP writes the 502, causing it to evaluate
	// IsFailureStatus(0) == true for a LOCAL panic — incorrectly
	// applying the penalty as if the upstream failed.
	// By catching the panic here, we:
	//   1. Write 502 to the statusRecorder (if no terminal status written yet)
	//   2. Set localPanic = true so the phantom penalty defer and breaker
	//      reporting skip their failure-path logic (a local panic is not
	//      an upstream failure)
	//   3. Return normally — the outer ServeHTTP continues to metrics+journal
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
				slog.Error("proxy panic in serveLimited", "panic", rv)
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
	// an abort. A transcoded exchange contributes explicit outcome
	// provenance: a client-aborted 2xx stream is neither success nor
	// failure; a local conversion failure is not an upstream failure.
	if p.breaker != nil && !localPanic {
		p.resolveBreakerResult(result, retryAttempt, proxyStart, breakerEpoch)
	}
	// The clean-completion counter publishes in finalize (after the journal
	// entry), mirroring the passthrough path.
	if rec, ok := w.(*statusRecorder); ok {
		rec.admissionCompleted = true
	}
}

func (p *Proxy) acquireSlot(ctx context.Context, method, path string) (release func(), limiter *queue.Limiter, err error) {
	if pat := p.matcher.FindMatch(method, path); pat != nil {
		key := pat.Group
		if key == "" {
			key = pat.Raw
		}
		if lim, ok := p.routeLimiters[key]; ok {
			rel, err := lim.Acquire(ctx)
			return rel, lim, err
		}
	}
	rel, err := p.limiter.Acquire(ctx)
	return rel, p.limiter, err
}

// queueCommentsEligible reports whether the early SSE commit may replace this
// request's response. The predicate is about the response, not about which
// methods matter: the commit replaces the upstream's whole response with a
// 200 event stream, so it is only sane when that response can carry a body
// and no handler will refuse the request first.
//
//   - HEAD: net/http discards HEAD body writes, so the commit does not fail —
//     it silently turns the exchange into a bodiless 200 event stream and
//     swallows the real status, headers, and Retry-After the client asked for.
//   - CONNECT: a 2xx CONNECT response cannot carry a body, and
//     httputil.ReverseProxy rejects CONNECT locally before any upstream
//     attempt, so the commit hides a real error behind a synthetic 200.
//   - Upgrade: the native and transcode handlers refuse every upgrade request
//     with a dialect 400, and ReverseProxy needs the connection itself for
//     the 101 handshake. A committed 200 turns the refusal into a 200 error
//     stream, or preempts the handshake and writes it into a committed
//     message.
//
// The upgrade test is transcode.IsUpgradeRequest, the same classifier both
// handlers refuse with, so the gate and the handlers cannot disagree. That is
// deliberately broader than the switching-protocols check used for 101
// validation (which follows net/http's handshake semantics): a request this
// gate lets through only loses comment bytes, while one the handlers refuse
// after a commit costs the client a truthful status.
//
// This never decides admission: serveLimited consults it after the breaker and
// queue-depth checks, purely to choose the response representation.
func queueCommentsEligible(r *http.Request) bool {
	if r == nil || r.Method == http.MethodHead || r.Method == http.MethodConnect {
		return false
	}
	return !transcode.IsUpgradeRequest(r)
}

// canCommitCommentsWithUnreadBody reports whether the early SSE commit can
// proceed without enabling full duplex — i.e. whether the request carries
// no body bytes the post-admission upstream send still needs. ContentLength
// 0 covers both absent and empty bodies; a negative (chunked/unknown) or
// positive length must assume bytes are pending and require full duplex.
func canCommitCommentsWithUnreadBody(r *http.Request) bool {
	return r.Body == nil || r.ContentLength == 0
}

type bodyReadCloser struct {
	io.Reader
	io.Closer
}

// writeAdmissionFailure answers a request whose admission context ended
// before it was admitted: the queue timeout (504) or a client cancellation
// (503). Once the queue comments committed the streaming representation
// (stopComments != nil), the failure surfaces as a failed close (error
// comment + aborted exchange), never as a misleading new HTTP status or a
// non-SSE body inside the committed stream.
func (p *Proxy) writeAdmissionFailure(w http.ResponseWriter, ctx context.Context, stopComments func(), rec *statusRecorder) {
	reason, status, text := "canceled", http.StatusServiceUnavailable, "request canceled"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		reason, status, text = "timeout", http.StatusGatewayTimeout, "queue timeout"
		p.m.IncTimeout()
	} else {
		p.m.IncCancelled()
	}
	if stopComments == nil {
		http.Error(w, text, status)
		return
	}
	stopComments()
	rec.aborted = true
	fmt.Fprintf(w, ": queue-wait failed: %s\n\n", reason)
	_ = rec.FlushError()
	// Record the admission failure in the journal the way a committed
	// upstream failure status is recorded: the client saw the committed 200
	// and the failed close, but the journal must carry the reason (504
	// timeout, 503 cancel) rather than a status no exchange ever observed.
	rec.recordCommittedFailure(status)
}

// bodyBlocksStreamCommit reports whether a request body forbids the early
// queue-comment SSE commit. An explicit body "stream": false takes precedence
// over the client's Accept header, so it must not be locked into the streaming
// representation; neither may a body whose intent cannot be determined (too
// large to inspect, unreadable, or not JSON). A body of unknown length
// (chunked, or HTTP/2 without Content-Length) is inspected like any other:
// the bounded read decides whether it fits.
//
// The inspection is bounded and ends with probeContext. The consumed bytes are
// always replayed to the handler, followed by the original read error if the
// read failed, so the handler observes the body exactly as sent.
//
// Cancellation must interrupt a read blocked on a stalled upload. Closing an
// HTTP/1.1 server body cannot: (*http.body).Read holds the body mutex across
// the socket read and Close waits for the same mutex. The read deadline of
// the underlying connection can, so it is moved to now; Close is the fallback
// for writers without deadline support.
func bodyBlocksStreamCommit(w http.ResponseWriter, r *http.Request, probeContext context.Context) bool {
	if r == nil || r.Method != http.MethodPost || r.Body == nil || r.ContentLength == 0 {
		return false
	}
	const limit = int64(10 << 20)
	if r.ContentLength > limit {
		return true
	}
	origBody := r.Body
	stopInterrupt := context.AfterFunc(probeContext, func() {
		if http.NewResponseController(w).SetReadDeadline(time.Now()) != nil {
			_ = origBody.Close()
		}
	})
	buf, err := io.ReadAll(io.LimitReader(origBody, limit+1))
	if !stopInterrupt() && err == nil {
		// The interrupt fired after the read finished: the request is
		// over, and origBody may be closed concurrently, so it must not be
		// read again.
		err = probeContext.Err()
	}
	if err != nil {
		r.Body = &bodyReadCloser{
			Reader: &errorBodyReader{rest: bytes.NewReader(buf), err: err},
			Closer: origBody,
		}
		return true
	}
	r.Body = &bodyReadCloser{
		Reader: io.MultiReader(bytes.NewReader(buf), origBody),
		Closer: origBody,
	}
	if int64(len(buf)) > limit {
		return true
	}
	var probe struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(buf, &probe); err != nil {
		return true
	}
	return probe.Stream != nil && !*probe.Stream
}

// startQueueComments commits the SSE streaming representation (200 +
// text/event-stream) BEFORE admission and writes ": queue-wait elapsed=N"
// comment lines every p.queueComments interval, flushing each, until the
// returned stop function is called. The stop function blocks until the
// comment goroutine has finished its last write, so the caller can safely
// take over the ResponseWriter. The recorder's commentsCommitted flag locks
// the representation: later WriteHeader calls cannot change the status the
// client already received. The caller decides eligibility; see
// queueCommentsEligible and bodyBlocksStreamCommit.
func (p *Proxy) startQueueComments(rec *statusRecorder) func() {
	rec.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	rec.Header().Set("Cache-Control", "no-cache")
	// The commit's own 200 is the streaming representation, not an observed
	// upstream status: it is written while the request is still waiting for
	// admission, before the queue phase even begins. commitSSE marks it so
	// WriteHeader skips the journal stamp (see its commentsCommitted branch),
	// and an exchange that ends without a later real status - an expired
	// queue wait, or a client abort before the handler wrote anything -
	// never journals headers that precede QueueStart.
	rec.commitSSE = true
	rec.WriteHeader(http.StatusOK)
	rec.commitSSE = false
	rec.commentsCommitted = true
	start := time.Now()
	fmt.Fprintf(rec, ": queue-wait elapsed=0\n\n")
	_ = rec.FlushError()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(p.queueComments)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(rec, ": queue-wait elapsed=%d\n\n", int(time.Since(start).Seconds()))
				if rec.FlushError() != nil {
					return
				}
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

// writeQueueRejected emits the bounded-queue 429: a Retry-After header plus
// a JSON error body valid under both AI dialects' envelope conventions
// ({"error":{...}}). The status and Retry-After are the machine signal the
// official SDKs act on; the body is for human logs. The write goes through
// the statusRecorder so the journal and status buckets capture the 429.
func writeQueueRejected(w http.ResponseWriter, depthLimit int, waiters int64) {
	if rec, ok := w.(*statusRecorder); ok {
		if !rec.terminalWritten {
			rec.proxyGeneratedError = true
		}
	}
	seconds := 5
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = fmt.Fprintf(w,
		`{"error":{"type":"rate_limit_error","message":"gateway concurrency queue is full: %d requests already waiting (limit %d); retry after %d seconds"}}`,
		waiters, depthLimit, seconds)
}
