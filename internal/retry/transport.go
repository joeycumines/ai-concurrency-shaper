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
			if t.Breaker != nil {
				// Client-initiated context cancellation and deadline
				// expiry are NOT upstream failures — do not feed them to
				// the breaker. The transport does not set its own per-
				// attempt deadlines (context.WithTimeout is never called),
				// so all DeadlineExceeded errors originate from the client
				// context or the proxy's queue timeout. An attacker could
				// otherwise trip the breaker by sending requests with tight
				// client deadlines that expire before the upstream responds.
				isClientCancel := err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
				// Capture a single evaluation anchor so classification and
				// Retry-After extraction cannot disagree due to GC pauses or
				// scheduling jitter between time.Now() calls. Use the explicit
				// receivedAt so the intent is self-documenting.
				now := time.Now()
				if !isClientCancel && (err != nil || circuitbreaker.IsFailureStatusWithHeaders(statusCode(resp, err), resp.Header, receivedAt, now)) {
					var ra time.Duration
					if resp != nil {
						ra = circuitbreaker.ParseRetryAfter(resp.Header, receivedAt, now)
					}
					t.Breaker.RecordFailure(statusCode(resp, err), ra, attemptStart, breakerEpoch)
				} else if resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
					t.Breaker.RecordSuccess(attemptStart, breakerEpoch)
				}
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
				if bodyBuf != nil {
					releaseBuf(bodyBuf)
				}
				return nil, req.Context().Err()
			case <-timer.C:
			}
		}

		attemptStart = time.Now()
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

		mustRetry := shouldRetry(resp, err)
		atLimit := t.MaxRetries >= 0 && attempt >= t.MaxRetries

		// Report the outcome to the circuit breaker.
		if t.Breaker != nil {
			// Client-initiated context cancellation and deadline
			// expiry are NOT upstream failures — do not feed them to
			// the breaker. The transport does not set its own per-
			// attempt deadlines (context.WithTimeout is never called),
			// so all DeadlineExceeded errors originate from the client
			// context or the proxy's queue timeout. An attacker could
			// otherwise trip the breaker by sending requests with tight
			// client deadlines that expire before the upstream responds.
			isClientCancel := err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
			// Capture a single evaluation anchor so classification and
			// Retry-After extraction cannot disagree due to GC pauses or
			// scheduling jitter between time.Now() calls. Use the explicit
			// receivedAt (from when RoundTrip returned) so the intent is
			// self-documenting and any scheduling delay is handled correctly.
			now := time.Now()
			if !isClientCancel && (err != nil || circuitbreaker.IsFailureStatusWithHeaders(statusCode(resp, err), resp.Header, receivedAt, now)) {
				var ra time.Duration
				if resp != nil {
					ra = circuitbreaker.ParseRetryAfter(resp.Header, receivedAt, now)
				}
				t.Breaker.RecordFailure(statusCode(resp, err), ra, attemptStart, breakerEpoch)
			} else if resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				t.Breaker.RecordSuccess(attemptStart, breakerEpoch)
			}
		}

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
			if bodyBuf != nil {
				if resp != nil && resp.Body != nil {
					resp.Body = &pooledBody{ReadCloser: resp.Body, buf: bodyBuf}
				} else {
					releaseBuf(bodyBuf)
				}
			}
			return resp, err
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
