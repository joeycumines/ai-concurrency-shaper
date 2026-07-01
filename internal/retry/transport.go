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

// Package retry implements a retrying HTTP transport.
package retry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
)

// CheckRetry decides whether a failed attempt should be retried.
type CheckRetry func(resp *http.Response, err error) bool

// DefaultCheckRetry retries on 5xx, 429, and transport errors.
var DefaultCheckRetry CheckRetry = func(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
}

var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// maxPoolBufCap is the maximum buffer capacity (in bytes) that will be
// returned to bufPool. Buffers that grew beyond this size (e.g., from
// handling requests close to MaxBodyBytes) are discarded and left for
// GC instead of being pinned in the pool indefinitely. Without this cap,
// sustained large-body traffic causes sync.Pool to cache many oversized
// buffers, creating a permanent memory bloat vector.
const maxPoolBufCap = 64 << 10 // 64 KB

// releaseBuf resets b and returns it to bufPool if its capacity is within
// the acceptable range. Oversized buffers are discarded to prevent the pool
// from becoming a reservoir of large allocations.
func releaseBuf(b *bytes.Buffer) {
	if b == nil {
		return
	}
	if b.Cap() > maxPoolBufCap {
		return // discard — let GC reclaim the oversized buffer
	}
	b.Reset()
	bufPool.Put(b)
}

// BreakerEpochKey is the context key for the circuit breaker epoch obtained
// by the proxy's pre-check Allow() call. When retries are enabled, the proxy
// calls Breaker.Allow() before dispatching the request to the retry transport.
// The retry transport does NOT call Allow() on the first attempt (attempt 0) —
// it relies on the proxy's pre-check. Without this context handoff, the retry
// transport's breakerEpoch would be 0 for the first attempt, causing
// RecordFailure(..., epoch=0) to bypass the stale-probe guard in
// circuitbreaker.RecordFailure (which skips epoch checks when epoch == 0).
// The proxy injects the epoch via r.WithContext(context.WithValue(..., BreakerEpochKey, epoch)),
// and the retry transport extracts it before the retry loop.
//
// breakerEpochKeyType is an unexported distinct type to prevent context key
// collisions with struct{}{} or any other package's keys, satisfying go vet.
type breakerEpochKeyType struct{}

var BreakerEpochKey = breakerEpochKeyType{}

type deferBreakerSuccessKeyType struct{}

var deferBreakerSuccessKey = deferBreakerSuccessKeyType{}

type breakerAttemptKeyType struct{}

var breakerAttemptKey = breakerAttemptKeyType{}

// BreakerAttempt records breaker metadata for the transport attempt that
// produced the response returned to the caller, plus cumulative request-level
// failure facts observed by earlier attempts in the same logical RoundTrip.
type BreakerAttempt struct {
	StartedAt             time.Time
	Epoch                 uint64
	FailureRecorded       bool
	HeaderFailureRecorded bool
	// AnyFailureRecorded and AnyHeaderFailureRecorded are intentionally
	// cumulative metadata. recordBreakerAttempt resets only current-attempt
	// fields before each attempt, while these fields let trusted callers
	// distinguish "the returned attempt failed" from "some earlier attempt in
	// this logical request already recorded breaker failure" for slot-release
	// policy and diagnostics.
	AnyFailureRecorded       bool
	AnyHeaderFailureRecorded bool
}

// WithDeferredBreakerSuccess marks a request whose final 2xx breaker success
// must be recorded by the caller after the response body is fully consumed.
// The retry transport still records failures immediately; only header-level
// 2xx success is deferred because a reverse proxy can discover body-copy
// failures after RoundTrip has already returned a successful response.
func WithDeferredBreakerSuccess(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferBreakerSuccessKey, true)
}

// WithBreakerAttempt stores a mutable attempt recorder in the request context.
// It is used with WithDeferredBreakerSuccess so the caller can report a final
// body-copy success/failure with the exact startedAt/epoch of the retry attempt
// that produced the returned response.
func WithBreakerAttempt(ctx context.Context, attempt *BreakerAttempt) context.Context {
	return context.WithValue(ctx, breakerAttemptKey, attempt)
}

func deferBreakerSuccess(req *http.Request) bool {
	deferSuccess, _ := req.Context().Value(deferBreakerSuccessKey).(bool)
	return deferSuccess
}

func recordBreakerAttempt(req *http.Request, startedAt time.Time, epoch uint64) {
	attempt, _ := req.Context().Value(breakerAttemptKey).(*BreakerAttempt)
	if attempt == nil {
		return
	}
	attempt.StartedAt = startedAt
	attempt.Epoch = epoch
	attempt.FailureRecorded = false
	attempt.HeaderFailureRecorded = false
}

func markBreakerAttemptFailure(req *http.Request, headerFailure bool) {
	attempt, _ := req.Context().Value(breakerAttemptKey).(*BreakerAttempt)
	if attempt == nil {
		return
	}
	attempt.FailureRecorded = true
	attempt.AnyFailureRecorded = true
	if headerFailure {
		attempt.HeaderFailureRecorded = true
		attempt.AnyHeaderFailureRecorded = true
	}
}

func isContextCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isRequestContextCancellation(req *http.Request) bool {
	return isContextCancellation(req.Context().Err())
}

func suppressBreakerFailureForError(req *http.Request, err error) bool {
	return isRequestContextCancellation(req) && isContextCancellation(err)
}

// Transport is an http.RoundTripper that retries transient failures.
type Transport struct {
	Inner        http.RoundTripper
	MaxRetries   int
	MaxBodyBytes int64
	CheckRetry   CheckRetry
	WaitMin      time.Duration
	WaitMax      time.Duration

	// MinRetryDelay is a floor for the retry wait duration. When > 0, the
	// wait before each retry attempt is max(calcWait, MinRetryDelay). This
	// gives the downstream service time to complete its accounting before
	// the retry arrives (KILL-05 mitigation). MinRetryDelay applies after
	// the Retry-After header is considered: the final wait is
	// max(calcWait, Retry-After, MinRetryDelay). This means MinRetryDelay
	// can override a shorter Retry-After value — this is intentional, as
	// the operator knows their downstream's accounting window better than
	// the upstream's rate-limit header.
	MinRetryDelay time.Duration

	// InFlightRetries tracks the number of RoundTrip calls currently in retry
	// mode. When non-nil, it is incremented exactly once when the RoundTrip
	// enters retry mode (attempt == 1) and decremented exactly once via defer
	// when RoundTrip returns. This provides visibility into retry pressure for
	// the TUI and enables future admission control (KILL-01/03 mitigation).
	InFlightRetries *atomic.Int64

	// Breaker is an optional circuit breaker. When set, failed attempts are
	// reported to the breaker, and retries are aborted if the circuit is OPEN.
	Breaker *circuitbreaker.Breaker
}

// RetryBreaker exposes the breaker owned by this retry transport. Wrappers may
// expose the same method as a trusted compatibility SPI; doing so promises they
// honor WithDeferredBreakerSuccess and WithBreakerAttempt with retry.Transport
// semantics, not merely that they can return a breaker pointer. Opaque wrappers
// that expose neither this SPI nor Unwrap cannot be detected by the proxy and
// are treated as ordinary transports.
func (t *Transport) RetryBreaker() *circuitbreaker.Breaker {
	if t == nil {
		return nil
	}
	return t.Breaker
}

// SetInFlightRetries wires the retry in-flight counter used by proxy metrics.
// Wrapper implementations are trusted to delegate to retry-compatible retry
// execution, matching the RetryBreaker SPI contract above. They must not claim
// compatibility while hiding a different breaker-bearing retry transport.
func (t *Transport) SetInFlightRetries(counter *atomic.Int64) {
	if t == nil {
		return
	}
	t.InFlightRetries = counter
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	inner := t.Inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	shouldRetry := t.CheckRetry
	if shouldRetry == nil {
		shouldRetry = DefaultCheckRetry
	}

	// Extract the breaker epoch from the request context, if set by the
	// proxy's pre-check Allow() call. This ensures the first attempt's
	// RecordFailure/RecordSuccess uses the correct epoch instead of 0,
	// preventing stale-probe bypass (review-06 Finding 1).
	var breakerEpoch uint64
	if v, ok := req.Context().Value(BreakerEpochKey).(uint64); ok {
		breakerEpoch = v
	}

	deferSuccess := deferBreakerSuccess(req)

	var bodyBuf *bytes.Buffer
	if req.Body != nil && t.MaxBodyBytes > 0 {
		buf := bufPool.Get().(*bytes.Buffer)
		buf.Reset()
		if _, err := io.Copy(buf, io.LimitReader(req.Body, t.MaxBodyBytes+1)); err != nil {
			req.Body.Close()
			releaseBuf(buf)
			return nil, err
		}
		if int64(buf.Len()) > t.MaxBodyBytes {
			// Body too large to buffer; reconstruct with prefix + remaining stream.
			// Do NOT close req.Body here — the MultiReader must be able to read
			// the remaining bytes from it after exhausting buf. The original body
			// is tracked via origBody so pooledBody.Close() can clean it up after
			// the downstream consumer finishes reading.
			origBody := req.Body
			req.Body = &pooledBody{
				ReadCloser: io.NopCloser(io.MultiReader(buf, origBody)),
				buf:        nil, // buf is inside MultiReader — do NOT recycle in Close or after RoundTrip
				origBody:   origBody,
			}
			// Report the outcome to the circuit breaker. The body-too-large
			// path cannot retry (no buffered body to replay), but it MUST
			// still report outcomes so that large payloads cannot evade the
			// breaker entirely. Without this, an attacker could bypass the
			// circuit by sending payloads exceeding MaxBodyBytes.
			attemptStart := time.Now()
			recordBreakerAttempt(req, attemptStart, breakerEpoch)
			resp, err := inner.RoundTrip(req)
			receivedAt := time.Now()
			// Do NOT recycle buf back to the pool here. The http.RoundTripper
			// contract does not guarantee the request body is fully consumed by
			// the time RoundTrip returns — the transport may close the body in a
			// separate goroutine. Since buf is wrapped inside an io.MultiReader
			// that the pooledBody references, recycling buf while the MultiReader
			// may still be reading from it would cause data corruption across
			// unrelated requests that reuse the pooled buffer. Instead, buf is
			// set to nil in the pooledBody above so Close() skips pool recycling,
			// and buf is GC'd naturally when the MultiReader finishes and all
			// references are dropped. This is the body-too-large edge case —
			// the allocation cost is acceptable.
			_ = buf // buf lives inside the MultiReader; let GC handle it
			// Guard against a buggy inner transport returning (nil, nil),
			// which would cause the breaker classification below to panic
			// on resp.Header. The retry loop applies this same guard when
			// it evaluates isFailureStatusWithHeaders.
			if resp == nil && err == nil {
				err = errors.New("nil response without error from inner transport")
			}
			// Client-initiated context cancellation and deadline expiry are NOT
			// upstream failures. The transport does not set its own per-attempt
			// deadlines (context.WithTimeout is never called), so all
			// DeadlineExceeded errors originate from the client context or the
			// proxy's queue timeout. Capture a single evaluation anchor so
			// classification and Retry-After extraction cannot disagree due to GC
			// pauses or scheduling jitter between time.Now() calls. Use the
			// explicit receivedAt so the intent is self-documenting.
			now := time.Now()
			statusFailure := resp != nil && circuitbreaker.IsFailureStatusWithHeaders(resp.StatusCode, resp.Header, receivedAt, now)
			errorFailure := err != nil && !suppressBreakerFailureForError(req, err)
			if statusFailure || errorFailure {
				markBreakerAttemptFailure(req, statusFailure)
				if t.Breaker != nil {
					var ra time.Duration
					if resp != nil {
						ra = circuitbreaker.ParseRetryAfter(resp.Header, receivedAt, now)
					}
					code := statusCode(resp, err)
					if statusFailure {
						code = resp.StatusCode
					}
					t.Breaker.RecordFailure(code, ra, attemptStart, breakerEpoch)
				}
			} else if t.Breaker != nil && err == nil && !deferSuccess && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				t.Breaker.RecordSuccess(attemptStart, breakerEpoch)
			}
			if err != nil && suppressBreakerFailureForError(req, err) {
				return t.finishRequestCancellation(req, resp, nil, breakerEpoch)
			}
			return resp, err
		}
		req.Body.Close()
		bodyBuf = buf
	}

	var lastResp *http.Response
	var lastReceivedAt time.Time // when the previous attempt's response headers arrived
	var attemptStart time.Time

	// Track in-flight retry state for TUI visibility (KILL-01/03).
	// The counter is incremented exactly once when the RoundTrip enters
	// retry mode (attempt == 1) and decremented exactly once when the
	// function exits via defer. This prevents the counter leak where
	// incrementing on every attempt but decrementing only on the final
	// return left a permanent +N-1 residue per multi-retry request.
	var retryActive bool
	if t.InFlightRetries != nil {
		defer func() {
			if retryActive {
				t.InFlightRetries.Add(-1)
			}
		}()
	}

	for attempt := 0; ; attempt++ {
		// A cancellation that happens after a retryable response but before the
		// next attempt is just as terminal as a cancellation returned by
		// RoundTrip. Check before Breaker.Allow so a HALF_OPEN failure from the
		// previous response cannot turn an abandoned request into ErrCircuitOpen.
		if attempt > 0 && isRequestContextCancellation(req) {
			return t.finishRequestCancellation(req, nil, bodyBuf, breakerEpoch)
		}

		// Enter retry mode on the first retry attempt (attempt == 1).
		// Only increment once — the counter represents "how many RoundTrip
		// calls are currently in retry mode", not "how many retry attempts
		// are in flight across all RoundTrips".
		if attempt == 1 && t.InFlightRetries != nil {
			retryActive = true
			t.InFlightRetries.Add(1)
		}

		// Before each retry, check the circuit breaker.
		if attempt > 0 && t.Breaker != nil {
			var allowErr error
			breakerEpoch, allowErr = t.Breaker.Allow()
			if allowErr != nil {
				// Circuit is OPEN — abort retries.
				if bodyBuf != nil {
					releaseBuf(bodyBuf)
				}
				return nil, circuitbreaker.ErrCircuitOpen
			}
		}

		if bodyBuf != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBuf.Bytes()))
			req.ContentLength = int64(bodyBuf.Len())
			if req.Header != nil {
				req.Header.Set("Content-Length", strconv.Itoa(bodyBuf.Len()))
			}
		}

		if attempt > 0 {
			// Capture the evaluation anchor for the retry wait. Use the
			// persisted receipt timestamp (lastReceivedAt) from when the
			// previous attempt's headers arrived so that ParseRetryAfter
			// correctly subtracts body-drain time from the ban duration.
			// Without this, the transport over-sleeps by the full drain
			// duration: e.g., Retry-After: 10 with a 4s drain sleeps the
			// full 10s instead of the remaining 6s.
			retryNow := time.Now()
			wait := calcWait(attempt, t.WaitMin, t.WaitMax)
			if lastResp != nil {
				if ra := circuitbreaker.ParseRetryAfter(lastResp.Header, lastReceivedAt, retryNow); ra > 0 && ra > wait {
					wait = ra
				}
			}
			// Enforce minimum retry delay floor for downstream accounting
			// (KILL-05 mitigation). This gives the downstream time to
			// complete its cleanup before the retry arrives. Retry-After
			// values already override this when larger.
			if t.MinRetryDelay > 0 && wait < t.MinRetryDelay {
				wait = t.MinRetryDelay
			}
			timer := time.NewTimer(wait)
			select {
			case <-req.Context().Done():
				timer.Stop()
				return t.finishRequestCancellation(req, nil, bodyBuf, breakerEpoch)
			case <-timer.C:
			}
		}

		attemptStart = time.Now()
		recordBreakerAttempt(req, attemptStart, breakerEpoch)
		resp, err := inner.RoundTrip(req)
		// Capture when the response headers arrived. This anchors the
		// receipt time for ParseRetryAfter so body-drain latency is
		// correctly subtracted from the remaining ban duration.
		receivedAt := time.Now()

		// Guard against a buggy inner transport returning (nil, nil),
		// which would cause httputil.ReverseProxy to panic. The standard
		// http.DefaultTransport never does this, but custom RoundTripper
		// implementations (metrics wrappers, logging wrappers, test mocks)
		// may accidentally return (nil, nil). Replace with a synthetic
		// transport error so the retry logic and breaker reporting work
		// correctly — statusCode(resp, err) returns 0 (transport error),
		// shouldRetry returns true (err != nil), and the breaker records
		// a failure.
		if resp == nil && err == nil {
			err = errors.New("nil response without error from inner transport")
		}

		// Report the outcome to the circuit breaker when present, and always mark
		// retry-attempt metadata when the caller installed a BreakerAttempt. The
		// metadata is used for slot-release policy even when breaker reporting is
		// disabled.
		// Client-initiated context cancellation and deadline expiry are NOT upstream
		// failures. The transport does not set its own per-attempt deadlines
		// (context.WithTimeout is never called), so all DeadlineExceeded errors
		// originate from the client context or the proxy's queue timeout. Capture a
		// single evaluation anchor so classification and Retry-After extraction
		// cannot disagree due to GC pauses or scheduling jitter between time.Now()
		// calls. Use the explicit receivedAt (from when RoundTrip returned) so the
		// intent is self-documenting and any scheduling delay is handled correctly.
		now := time.Now()
		statusFailure := resp != nil && circuitbreaker.IsFailureStatusWithHeaders(resp.StatusCode, resp.Header, receivedAt, now)
		errorFailure := err != nil && !suppressBreakerFailureForError(req, err)
		if statusFailure || errorFailure {
			markBreakerAttemptFailure(req, statusFailure)
			if t.Breaker != nil {
				var ra time.Duration
				if resp != nil {
					ra = circuitbreaker.ParseRetryAfter(resp.Header, receivedAt, now)
				}
				code := statusCode(resp, err)
				if statusFailure {
					code = resp.StatusCode
				}
				t.Breaker.RecordFailure(code, ra, attemptStart, breakerEpoch)
			}
		} else if t.Breaker != nil && err == nil && !deferSuccess && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			t.Breaker.RecordSuccess(attemptStart, breakerEpoch)
		}

		// A request-context cancellation error from RoundTrip is terminal for this
		// RoundTrip. The retry policy intentionally treats generic transport errors as
		// retryable, but a cancellation/deadline from req.Context() means the caller
		// has abandoned the request. Retrying it can convert a clean client abort into
		// ErrCircuitOpen in HALF_OPEN because the original probe remains in flight
		// while attempt > 0 calls Breaker.Allow(). The breaker accounting above
		// already suppresses the cancellation as a non-upstream failure while still
		// preserving definitive response-status failures, so return the context error
		// without entering the retry path.
		if err != nil && suppressBreakerFailureForError(req, err) {
			return t.finishRequestCancellation(req, resp, bodyBuf, breakerEpoch)
		}

		mustRetry := shouldRetry(resp, err)
		atLimit := t.MaxRetries >= 0 && attempt >= t.MaxRetries

		// When a request has a body but MaxBodyBytes <= 0, the body
		// was not buffered (bodyBuf is nil). After the first attempt
		// consumes and closes the body, subsequent retries would pass
		// the already-closed body to inner.RoundTrip, causing an
		// immediate "http: request body closed" error. This error is
		// itself retryable (err != nil → shouldRetry returns true),
		// creating a rapid futile retry loop that feeds false status-0
		// failures to the circuit breaker. Prevent this by checking
		// that the body is either absent or buffered before retrying.
		canRetry := req.Body == nil || req.Body == http.NoBody || bodyBuf != nil
		if !mustRetry || atLimit || !canRetry {
			// Preserve terminal responses even if req.Context() was canceled before
			// RoundTrip returned. At this point the response will not be retried: it is
			// either non-retryable, the retry limit is exhausted, or the request body is
			// not replayable. Returning the definitive response lets the caller observe
			// the upstream outcome after breaker accounting has already recorded any
			// failure/success. Retryable responses still check cancellation before
			// draining below so an abandoned request never waits on connection reuse.
			finalizeBufferedResponseBody(resp, bodyBuf)
			return resp, err
		}
		if isRequestContextCancellation(req) {
			return t.finishRequestCancellation(req, resp, bodyBuf, breakerEpoch)
		}

		// Drain the body so the connection can be reused. Bound the
		// drain to 4KB to prevent a tarpit (malicious upstream
		// returning 5xx with an infinite streaming body) from blocking
		// the retry goroutine indefinitely — a trivial DoS vector.
		// The drain is also time-bounded (5 seconds) to prevent a
		// slow-drip tarpit (1 byte/minute) from blocking the goroutine.
		if resp != nil && resp.Body != nil {
			drainBody(resp.Body, 4096)
			resp.Body.Close()
		}
		lastResp = resp
		lastReceivedAt = receivedAt
	}
}

func (t *Transport) finishRequestCancellation(req *http.Request, resp *http.Response, buf *bytes.Buffer, breakerEpoch uint64) (*http.Response, error) {
	if t != nil && t.Breaker != nil {
		t.Breaker.CancelProbe(breakerEpoch)
	}
	closeResponseAndReleaseBuffer(resp, buf)
	err := req.Context().Err()
	if err == nil {
		err = context.Canceled
	}
	return nil, err
}

func closeResponseAndReleaseBuffer(resp *http.Response, buf *bytes.Buffer) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if buf != nil {
		releaseBuf(buf)
	}
}

func finalizeBufferedResponseBody(resp *http.Response, buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	if resp != nil && resp.Body != nil {
		resp.Body = wrapBufferedResponseBody(resp, buf)
		return
	}
	releaseBuf(buf)
}

// statusCode extracts the HTTP status code from a response/error pair.
// Returns 0 for transport errors (no response).
func statusCode(resp *http.Response, err error) int {
	if err != nil || resp == nil {
		return 0
	}
	return resp.StatusCode
}

func calcWait(attempt int, wMin, wMax time.Duration) time.Duration {
	if wMin <= 0 {
		wMin = 500 * time.Millisecond
	}
	if wMax <= 0 {
		wMax = 30 * time.Second
	}
	base := wMin
	for i := 1; i < attempt; i++ {
		base *= 2
		if base >= wMax {
			base = wMax
			break
		}
	}
	// ±25% jitter (rand.Int64N uses the global auto-seeded source, which is
	// safe for concurrent use and does not require manual seeding since Go 1.22).
	if j := int64(float64(base) * 0.25); j > 0 {
		base += time.Duration(rand.Int64N(2*int64(j)+1) - j)
	}
	if base < wMin {
		return wMin
	}
	if base > wMax {
		return wMax
	}
	return base
}

// drainBody drains up to maxBytes from body, bounded by a 5-second timeout.
// If the deadline fires, the body is closed to abort the read, and the
// function returns immediately. The caller must still close resp.Body after
// calling drainBody (the close is a no-op if drainBody already closed it).
func drainBody(body io.ReadCloser, maxBytes int64) {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(body, maxBytes))
		close(done)
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
		// Drain completed within the deadline.
	case <-timer.C:
		// Timeout — close the body to abort the read goroutine.
		body.Close()
		// Wait for the goroutine to finish to prevent it from reading
		// from a closed body after we return.
		<-done
	}
}

// pooledBody wraps an io.ReadCloser and optionally recycles a bytes.Buffer
// back to bufPool on Close. It is safe for concurrent Read and Close calls,
// as required by the http.Request.Body contract: "Body must allow Read to
// be called concurrently with Close."
//
// The closeOnce field guarantees Close logic executes exactly once, preventing
// sync.Pool double-put. The buf field is only recycled in Close when it is
// NOT part of an active io.MultiReader — the body-too-large path sets buf=nil
// and handles pool return separately after the transport finishes reading.
type pooledBody struct {
	io.ReadCloser
	buf       *bytes.Buffer
	origBody  io.ReadCloser // original body to close on Close (nil unless body-too-large path)
	closeOnce sync.Once
}

type closeWriter interface {
	CloseWrite() error
}

func wrapBufferedResponseBody(resp *http.Response, buf *bytes.Buffer) io.ReadCloser {
	if resp != nil && resp.StatusCode == http.StatusSwitchingProtocols {
		if rwc, ok := resp.Body.(io.ReadWriteCloser); ok {
			if cw, ok := resp.Body.(closeWriter); ok {
				return &pooledReadWriteCloseWriter{ReadWriteCloser: rwc, closeWriter: cw, buf: buf}
			}
			return &pooledReadWriteBody{ReadWriteCloser: rwc, buf: buf}
		}
	}
	return &pooledBody{ReadCloser: resp.Body, buf: buf}
}

type pooledReadWriteBody struct {
	io.ReadWriteCloser
	buf       *bytes.Buffer
	closeOnce sync.Once
}

func (p *pooledReadWriteBody) Close() error {
	var err error
	p.closeOnce.Do(func() {
		if p.ReadWriteCloser != nil {
			err = p.ReadWriteCloser.Close()
		}
		if p.buf != nil {
			releaseBuf(p.buf)
		}
	})
	return err
}

type pooledReadWriteCloseWriter struct {
	io.ReadWriteCloser
	closeWriter
	buf       *bytes.Buffer
	closeOnce sync.Once
}

func (p *pooledReadWriteCloseWriter) Close() error {
	var err error
	p.closeOnce.Do(func() {
		if p.ReadWriteCloser != nil {
			err = p.ReadWriteCloser.Close()
		}
		if p.buf != nil {
			releaseBuf(p.buf)
		}
	})
	return err
}

func (p *pooledBody) Read(b []byte) (int, error) {
	return p.ReadCloser.Read(b)
}

func (p *pooledBody) Close() error {
	var err error
	p.closeOnce.Do(func() {
		var errs []error
		// Close the outer wrapper first (standard convention for layered closers).
		// For the body-too-large path, ReadCloser is io.NopCloser(io.MultiReader(buf,
		// origBody)) — closing the NopCloser is a no-op, but we close the wrapper
		// before the underlying resources it wraps.
		if p.ReadCloser != nil {
			errs = append(errs, p.ReadCloser.Close())
		}
		// Close the original body (if tracked) so the underlying stream is cleaned
		// up. This matters in the body-too-large reconstruction path where origBody
		// is the pre-MultiReader request body.
		if p.origBody != nil {
			errs = append(errs, p.origBody.Close())
		}
		if p.buf != nil {
			releaseBuf(p.buf)
		}
		err = errors.Join(errs...)
	})
	return err
}
