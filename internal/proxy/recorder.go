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
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

const maxResponseCapturePrealloc = 64 << 10

// statusRecorder wraps a ResponseWriter to capture the status code,
// response headers, and response body for the journal.
type statusRecorder struct {
	http.ResponseWriter
	status              int
	entry               *journal.Entry
	capturedBody        []byte
	captureMax          int64
	captureDone         bool
	bytesWritten        int64 // total bytes written through Write (may exceed capturedBody)
	hijacked            bool
	terminalWritten     bool      // true once a terminal status (>=200) has been recorded
	responseAt          time.Time // headers received; anchor for Retry-After evaluation
	writeFailed         bool
	writeErr            error
	proxyGeneratedError bool
	transportErr        error
	// upstreamAttempted records whether the upstream round trip actually
	// started before transportErr was observed. It is the fact that separates
	// a genuine upstream transport failure from a proxy-local defect that also
	// happens to arrive as a transport error (an unsupported CONNECT, a signer
	// failure): transportErr alone cannot tell them apart, and "local failure"
	// means "the upstream was not the cause".
	upstreamAttempted bool
	aborted           bool
	// admissionCompleted is set at the end of the admission path
	// (serveLimited/servePassthrough): finalize's clean-completion counter
	// fires only when the exchange actually completed the admission path,
	// not on the queue-timeout/cancel/circuit-rejection early returns.
	admissionCompleted bool
	// commentsCommitted marks the SSE queue-comment mode: the
	// streaming representation (200 + text/event-stream) was committed
	// BEFORE admission, so the exchange is locked to the streaming
	// representation — later WriteHeader calls cannot change what the
	// client already received, and admission failures surface as stream
	// error frames, never as HTTP error statuses.
	commentsCommitted      bool
	upstreamStatusOverride int
	// commitSSE marks the WriteHeader call that IS the queue-comment commit
	// (the representation's own 200, written before admission and before the
	// queue phase begins). WriteHeader skips the journal stamp for it: it is
	// not an observed upstream status, and stamping it would journal headers
	// that precede QueueStart on exchanges that end without a later real
	// status. The commit still sets terminalWritten, so the client-visible
	// committed-200 semantics are unchanged.
	commitSSE bool
	// committedOKObserved records a 2xx written after the queue-comment
	// commit. committedResponseStatus retains the actual upstream status
	// while the client-visible status remains the already-committed 200.
	committedOKObserved     bool
	committedResponseStatus int
	transcodeStream         bool
	// dialectErrorFramed marks a committed stream whose failure is being
	// delivered as a client-dialect error terminal by its owner. The
	// recorder must then not append its own ": queue-wait failed: upstream"
	// close comment: the failure is local (or already framed), and blaming
	// the upstream would misreport what happened.
	dialectErrorFramed  bool
	localUpgradeFailure bool

	switchingProtocolsProbeResolved      atomic.Bool
	onSwitchingProtocolsHandshakeSuccess func()
	onSwitchingProtocolsHandshakeFailure func()

	// transcodeOutcome is the explicit outcome provenance recorded by a
	// transcoded exchange. When set, breaker accounting uses it instead of
	// status-code guessing.
	transcodeOutcome *transcode.Outcome
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) FlushError() error {
	err := http.NewResponseController(r.ResponseWriter).Flush()
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		r.writeFailed = true
		r.writeErr = err
	}
	if err == nil {
		r.recordImplicitOK(nil)
	}
	return err
}

// SuppressFailedCloseComment marks the committed stream as framed by its
// owner: a dialect error terminal follows, so the recorder must not add its
// own failed-close comment. It is called by the dialect error writer just
// before the terminal, while the exchange is still running.
func (r *statusRecorder) SuppressFailedCloseComment() { r.dialectErrorFramed = true }

// recoverLocalPanic records a recovered local panic. Any panic aborts the
// exchange: it never reaches the completion counters or a clean journal
// finalization. The failure is proxy-generated only when no upstream status
// was observed — a panic after a delivered upstream status (including a 200
// the commit silenced) leaves that status's classification alone. Before any
// status the client gets a 502. After the SSE queue-comment commit the status
// is fixed, so the stream is
// closed with a local failed-close comment (unless its owner already framed
// a dialect error) and the 502 is recorded the way a committed upstream
// failure status is: without it the client saw the queue comments and then a
// silent clean EOF, and the journal kept the commit's 200.
func (r *statusRecorder) recoverLocalPanic() {
	r.aborted = true
	switch {
	case !r.terminalWritten:
		r.proxyGeneratedError = true
		http.Error(r, "internal error", http.StatusBadGateway)
	case r.commentsCommitted && r.upstreamStatusOverride == 0 && !r.committedOKObserved:
		r.proxyGeneratedError = true
		if !r.dialectErrorFramed {
			fmt.Fprintf(r, ": queue-wait failed: internal\n\n")
			_ = r.FlushError()
		}
		r.recordCommittedFailure(http.StatusBadGateway)
	}
}

// recordCommittedFailure records the failure status of a stream whose 200 the
// queue comments already committed: the client cannot see it, but
// classification and the journal entry read it, exactly as they read a
// committed upstream failure status from WriteHeader, including the
// committed headers the client received. Callers write any failed-close
// comment first, since Write swallows bytes once an override is recorded.
func (r *statusRecorder) recordCommittedFailure(code int) {
	r.aborted = true
	r.upstreamStatusOverride = code
	r.stampCommittedStatus(code)
}

// stampCommittedStatus records code, observed after the queue-comment commit,
// as the exchange's response in the journal entry, with the committed
// headers the client received and the time it was observed.
func (r *statusRecorder) stampCommittedStatus(code int) {
	now := time.Now()
	r.responseAt = now
	if r.entry != nil {
		r.entry.StatusCode = code
		r.entry.ResponseHeaders = r.ResponseWriter.Header().Clone()
		r.entry.Timing.ResponseHeaders = now
		r.entry.ContentType = r.ResponseWriter.Header().Get("Content-Type")
	}
}

func (r *statusRecorder) downstreamWriteFailed() bool {
	return r != nil && r.writeFailed
}

func (r *statusRecorder) resolveSwitchingProtocolsProbe() bool {
	return r != nil && r.switchingProtocolsProbeResolved.CompareAndSwap(false, true)
}

// observedStatus is the response status the exchange actually observed, the
// one every classification reads. Without the queue-comment commit it is the
// recorded status. After the commit, status is fixed at the commit's own 200,
// which says nothing about the upstream: the observed status is then a
// recorded failure status (the upstream's, or a proxy-generated one), else
// the upstream's silenced 200, else 0 — no status observed, exactly the state
// an uncommitted exchange reports before WriteHeader.
func (r *statusRecorder) observedStatus() int {
	switch {
	case r == nil:
		return 0
	case !r.commentsCommitted:
		return r.status
	case r.upstreamStatusOverride > 0:
		return r.upstreamStatusOverride
	case r.committedOKObserved:
		if r.committedResponseStatus > 0 {
			return r.committedResponseStatus
		}
		return http.StatusOK
	default:
		return 0
	}
}

func (r *statusRecorder) proxyTransportOrGeneratedError() bool {
	if r == nil {
		return false
	}
	return r.observedStatus() == 0 || r.proxyGeneratedError
}

// hasRealProxyTransportError reports whether the exchange failed in the
// upstream transport after the round trip started. Every term is load-bearing:
// a local upgrade handoff failure and a breaker rejection are local facts, an
// error that never reached the transport (no attempt started) is a local
// defect, and a transport error that is only the client's own cancellation is
// an abandonment rather than an upstream health signal.
func (r *statusRecorder) hasRealProxyTransportError(ctxErr error) bool {
	return r != nil &&
		!r.localUpgradeFailure &&
		!r.retryCircuitOpen() &&
		r.upstreamAttempted &&
		r.proxyGeneratedError &&
		r.transportErr != nil &&
		!(isContextCancellation(r.transportErr) && isContextCancellation(ctxErr))
}

func (r *statusRecorder) retryCircuitOpen() bool {
	return r != nil && errors.Is(r.transportErr, circuitbreaker.ErrCircuitOpen)
}

// upstreamHandoffAbandoned reports whether the upstream produced a response
// the proxy did not deliver and then closed that connection. The upstream has
// been told to tear a session down and may still be accounting for it, so the
// slot has to cover that lag exactly as it does for a client that leaves after
// an upstream attempt started.
//
// One concept for both admission paths: serveLimited and servePassthrough both
// consult this predicate, so a future case extends it here rather than being
// added to one defer and forgotten in the other.
func (r *statusRecorder) upstreamHandoffAbandoned() bool {
	// A local upgrade handoff failure is the only shape today: the upstream
	// answered 101, the proxy closed that body because it could not deliver the
	// upgrade downstream, and the client was told to retry.
	return r != nil && r.localUpgradeFailure
}

func (r *statusRecorder) suppressibleClientAbort(ctxErr error) bool {
	if r == nil || !r.proxyTransportOrGeneratedError() {
		return false
	}
	if r.hasRealProxyTransportError(ctxErr) {
		return false
	}
	if r.downstreamWriteFailed() {
		return true
	}
	if !isContextCancellation(ctxErr) {
		return false
	}
	return r.transportErr == nil || isContextCancellation(r.transportErr)
}

func (r *statusRecorder) WriteHeader(code int) {
	// Once the queue-comment mode committed the streaming
	// representation, the client has already received 200 + SSE headers —
	// later WriteHeader calls cannot change that. A 200 dup is silenced
	// (the handler re-commits on its streaming path); a non-200 (e.g. a
	// representation-mismatch 502) marks the exchange aborted — the client
	// sees the comments then a failed close, never a misleading new status.
	if r.commentsCommitted {
		if code >= 200 && code < 300 && r.terminalWritten &&
			!r.committedOKObserved && r.upstreamStatusOverride == 0 {
			// terminalWritten is first set by the commit's own WriteHeader,
			// so the first status after it is the upstream/handler
			// response; a later one is superfluous, exactly as net/http
			// ignores a second WriteHeader on an uncommitted exchange.
			r.committedOKObserved = true
			r.committedResponseStatus = code
			r.stampCommittedStatus(code)
		}
		if code >= 300 {
			// The first post-commit status is the one the exchange observed. A
			// later failure is superfluous, as net/http ignores a second
			// WriteHeader: it still marks the exchange aborted, but it neither
			// replaces the recorded status and timing nor writes a second close.
			r.aborted = true
			if r.upstreamStatusOverride == 0 && !r.committedOKObserved {
				r.upstreamStatusOverride = code
				r.stampCommittedStatus(code)
				if !r.transcodeStream && !r.dialectErrorFramed {
					fmt.Fprintf(r.ResponseWriter, ": queue-wait failed: upstream\n\n")
					_ = r.FlushError()
				}
			}
		}
		return
	}
	// Ignore duplicate terminal statuses in the journal, matching net/http
	// behavior. Once a terminal status (>=200) is locked, subsequent
	// terminal WriteHeader calls are forwarded to the underlying writer
	// (to trigger the stdlib's dup-WriteHeader warning) but must not
	// mutate the journal entry — the client already received the first
	// status code. Informational 1xx responses are still allowed to
	// update the state until a terminal status locks it.
	if code >= 200 && r.terminalWritten {
		r.ResponseWriter.WriteHeader(code)
		return
	}
	if r.commitSSE {
		// The queue-comment commit's own 200: written before admission,
		// before the queue phase begins. It sets terminalWritten so the
		// committed-stream guards hold, but stamps nothing: it is the
		// streaming representation, not an observed upstream status, and
		// stamping it would journal ResponseHeaders before QueueStart on
		// exchanges that end without a later real status (an expired queue
		// wait, or a client abort before the handler wrote anything). It
		// is still the status the client received, so status keeps it:
		// the status buckets and request log count the client-visible
		// status, while classification reads observedStatus.
		r.status = code
		r.ResponseWriter.WriteHeader(code)
		if code >= 200 {
			r.terminalWritten = true
		}
		return
	}
	r.status = code
	now := time.Now()
	r.responseAt = now
	if r.entry != nil {
		r.entry.StatusCode = code
		r.entry.ResponseHeaders = r.ResponseWriter.Header().Clone()
		r.entry.Timing.ResponseHeaders = now
		r.entry.ContentType = r.ResponseWriter.Header().Get("Content-Type")
		if cl := r.ResponseWriter.Header().Get("Content-Length"); cl != "" {
			if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
				r.entry.ResponseSize = n
			}
		}
	}
	r.ResponseWriter.WriteHeader(code)
	if code >= 200 {
		r.terminalWritten = true
	}
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.commentsCommitted && r.upstreamStatusOverride > 0 && !r.transcodeStream && !r.dialectErrorFramed {
		if r.entry != nil && !r.captureDone && len(b) > 0 {
			remaining := r.captureMax - int64(len(r.capturedBody))
			if remaining > 0 {
				if int64(len(b)) > remaining {
					r.capturedBody = append(r.capturedBody, b[:remaining]...)
					r.captureDone = true
				} else {
					r.capturedBody = append(r.capturedBody, b...)
				}
			}
		}
		r.bytesWritten += int64(len(b))
		return len(b), nil
	}

	// If WriteHeader was never called for a terminal status, the Go runtime
	// will trigger an implicit WriteHeader(StatusOK) inside
	// ResponseWriter.Write — which bypasses our override. 1xx informational
	// responses also call WriteHeader, so we must distinguish terminal
	// (>=200) from informational (<200) status codes. Capture
	// headers/status here on the first terminal write.
	r.recordImplicitOK(b)

	// Perform the actual write before capturing the body so we can
	// record only the bytes accepted by ResponseWriter.Write (b[:n]).
	// The MIME-sniffing logic above reads b without mutation, so
	// there is no conflict with moving capture after the write.
	n, err := r.ResponseWriter.Write(b)
	if err != nil || n != len(b) {
		r.writeFailed = true
		if err != nil {
			r.writeErr = err
		} else {
			r.writeErr = io.ErrShortWrite
		}
	}
	// Track total bytes accepted by Write (not attempted) so ResponseSize
	// reflects what the ResponseWriter accepted. On a short write (n < len(b)),
	// typically caused by client disconnect mid-transfer, only the accepted
	// bytes are counted. A successful Write followed by a failed FlushError can
	// still mean the bytes did not reach the network peer.
	r.bytesWritten += int64(n)

	// Capture only the bytes accepted by ResponseWriter.Write. On a short write
	// (n < len(b)), b[:n] reflects only what the writer accepted, keeping
	// capturedBody consistent with both bytesWritten and ResponseSize.
	if r.entry != nil && !r.captureDone && n > 0 {
		accepted := b[:n]
		if r.capturedBody == nil {
			// Preallocate only when the response size is known and small. A hostile
			// upstream can declare a large Content-Length and then abort after one
			// byte; cap the initial allocation so journal capture grows with bytes
			// actually accepted by Write rather than upstream promises.
			if r.entry.ResponseSize > 0 && r.entry.ResponseSize < r.captureMax && r.entry.ResponseSize <= maxResponseCapturePrealloc {
				r.capturedBody = make([]byte, 0, r.entry.ResponseSize)
			}
			// For unknown/large sizes, leave r.capturedBody nil so append
			// grows the backing array on demand instead of preallocating.
		}
		remaining := r.captureMax - int64(len(r.capturedBody))
		if remaining > 0 {
			if int64(len(accepted)) > remaining {
				r.capturedBody = append(r.capturedBody, accepted[:remaining]...)
				r.captureDone = true
			} else {
				r.capturedBody = append(r.capturedBody, accepted...)
			}
		} else {
			r.captureDone = true
		}
	}
	// Defensive: if the handler called WriteHeader(200) explicitly
	// before Write, the implicit-200 block above is skipped. Some
	// non-standard ResponseWriter wrappers may set Content-Type on
	// the underlying writer during Write even after explicit
	// WriteHeader. Capture it here as a safety net. Also sync the
	// cloned headers so the TUI detail overlay shows the Content-Type
	// in both the Type field and the Headers list.
	if r.entry != nil && r.entry.ContentType == "" {
		if ct := r.ResponseWriter.Header().Get("Content-Type"); ct != "" {
			r.entry.ContentType = ct
			if r.entry.ResponseHeaders != nil {
				r.entry.ResponseHeaders.Set("Content-Type", ct)
			}
		}
	}
	return n, err
}

func (r *statusRecorder) recordImplicitOK(sample []byte) {
	if r.terminalWritten {
		return
	}
	r.status = http.StatusOK
	now := time.Now()
	r.responseAt = now
	if r.entry != nil {
		r.entry.StatusCode = http.StatusOK
		r.entry.ResponseHeaders = r.ResponseWriter.Header().Clone()
		r.entry.Timing.ResponseHeaders = now
		r.entry.ContentType = r.ResponseWriter.Header().Get("Content-Type")
		// If the handler never set Content-Type, Go runs MIME sniffing during
		// ResponseWriter.Write. Clone captured the headers before sniffing, so
		// detect the type from the body bytes ourselves. Flush-before-body has no
		// sample to sniff, so it records only the headers that actually exist.
		if r.entry.ContentType == "" && sample != nil {
			r.entry.ContentType = http.DetectContentType(sample)
			// Sync the MIME-sniffed type into the cloned headers so the TUI detail
			// overlay shows it in both the Type field and the Headers list.
			if r.entry.ResponseHeaders != nil {
				r.entry.ResponseHeaders.Set("Content-Type", r.entry.ContentType)
			}
		}
		if cl := r.ResponseWriter.Header().Get("Content-Length"); cl != "" {
			if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
				r.entry.ResponseSize = n
			}
		}
	}
	r.terminalWritten = true
}

// Hijack forwards the Hijack call through ResponseController so wrappers that
// expose the real writer via Unwrap are honored. The hijacked flag is only set
// on successful hijack to avoid corrupting the journal's ResponseComplete
// timing on failed attempts.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err != nil {
		return conn, brw, err
	}

	r.hijacked = true
	if !r.terminalWritten {
		now := time.Now()
		r.status = http.StatusSwitchingProtocols
		r.responseAt = now
		r.terminalWritten = true
		if r.entry != nil {
			r.entry.StatusCode = http.StatusSwitchingProtocols
			r.entry.ResponseHeaders = r.ResponseWriter.Header().Clone()
			r.entry.Timing.ResponseHeaders = now
			r.entry.ContentType = r.ResponseWriter.Header().Get("Content-Type")
		}
	}
	if r.onSwitchingProtocolsHandshakeSuccess != nil && brw != nil {
		brw = bufio.NewReadWriter(brw.Reader, bufio.NewWriter(&switchingProtocolsHandshakeWriter{
			writer:    brw.Writer,
			onSuccess: r.onSwitchingProtocolsHandshakeSuccess,
		}))
	}
	return conn, brw, nil
}
