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

// Package proxy implements a concurrency-bounded reverse proxy.
package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// --- Factory ---

// Proxy is a concurrency-bounded reverse proxy.
type Proxy struct {
	inner   *httputil.ReverseProxy
	matcher *route.Matcher
	limiter *queue.Limiter
	m       *metrics.Collector
	timeout time.Duration
	// queueDepthLimit bounds concurrent waiters (0 = unbounded). See
	// WithQueueDepthLimit.
	queueDepthLimit int
	// queueComments is the SSE queue-comment interval (0 = disabled). See
	// WithQueueComments.
	queueComments  time.Duration
	globalLimiter  *queue.Limiter
	routeLimiters  map[string]*queue.Limiter
	transport      http.RoundTripper
	journal        *journal.Journal
	breaker        *circuitbreaker.Breaker
	cancelCooldown time.Duration
	failureHold    time.Duration

	// retryHandlesBreaker is true when the transport is a retry.Transport
	// with a non-nil Breaker. In that case, the retry transport reports
	// every attempt's outcome to the breaker, so the proxy must NOT
	// duplicate failure/success reporting. When false (breaker without
	// retries), the proxy is the sole reporter.
	retryHandlesBreaker bool

	// retryTracksAttempts is true when the transport is retry-aware and honors
	// retry.WithBreakerAttempt. It is broader than retryHandlesBreaker: even
	// with no breaker, the proxy needs retry-attempt history to apply slot
	// failure-hold after an earlier definitive upstream failure is hidden by a
	// later ambiguous cancellation.
	retryTracksAttempts bool

	// adaptiveHeadroom enables one-slot concurrency reduction on 429s.
	adaptiveHeadroom bool

	// adaptiveHeadroomWindow is how long the one-slot reduction lasts.
	adaptiveHeadroomWindow time.Duration

	// limitAll routes every request through the default limiter, not just
	// requests matching a limited route.
	limitAll bool

	// transcodeHandlers serves transcoded routes, one per mapping.
	transcodeHandlers []*transcode.TranscodeHandler

	// nativeRoutes owns the natively served route declarations; the map
	// below points into this slice.
	nativeRoutes []NativeRoute

	// transcodeHandlerMap provides O(1) lookup of transcode handlers by
	// method+path route key, built once at construction.
	transcodeHandlerMap map[transcode.RouteKey]http.Handler

	// nativeRouteMap provides O(1) lookup of natively served routes by
	// method+path route key, built once at construction. A native miss
	// falls through to the transcode lookup.
	nativeRouteMap map[transcode.RouteKey]*NativeRoute

	// opencode, when enabled, emits the first-party header set on the
	// transparent engine's outbound requests (after authentication).
	opencode transcode.OpencodePreset

	// catalog, when non-nil, answers this mount's GET /v1/models discovery
	// request locally from the frozen model-table snapshot. Catalog behavior
	// lives in catalog.go; only the central Proxy lifecycle hook remains here.
	catalog *transcode.CatalogHandler

	// authPolicy, when non-nil, strips client credential/protocol headers
	// and attaches the upstream credential inside the Rewrite hook. nil
	// forwards requests verbatim.
	authPolicy *auth.AuthPolicy
}

// Metrics returns the shared metrics collector.
func (p *Proxy) Metrics() *metrics.Collector {
	return p.m
}

// Journal returns the shared request journal.
func (p *Proxy) Journal() *journal.Journal {
	return p.journal
}

// HandlerForRouteKey returns the transcode handler mapped to key, or nil.
// A route with multiple mappings returns a dispatcher that selects the
// target by the request's model dialect (Via); callers invoking it directly
// on a raw request pay one bounded body pre-read and get dispatcher
// semantics, including the native-miss context contract.
func (p *Proxy) HandlerForRouteKey(key transcode.RouteKey) http.Handler {
	return p.transcodeHandlerMap[key]
}

// Catalog returns the mount's catalog handler, or nil when no catalog was configured.
func (p *Proxy) Catalog() *transcode.CatalogHandler {
	return p.catalog
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The unlimited admission class exempts a request whose FIRST matching
	// pattern declares :unlimited — including under -limit-all. The same
	// first-match lookup drives limiter selection (acquireSlot/FindMatch),
	// so classification and admission can never disagree. The catalog is a
	// local answer with its own unlimited class: it is never limited, never
	// queued, and never gated by the circuit breaker.
	catalog := p.catalogServes(r)
	limited := (p.limitAll || p.matcher.IsLimited(r.Method, r.URL.Path)) &&
		!p.matcher.IsUnlimited(r.Method, r.URL.Path) && !catalog

	flightID := p.m.RegisterInFlight(r.Method, r.URL.Path, limited)
	defer p.m.DeregisterInFlight(flightID)

	start := time.Now()

	// Recover from panics in the inner transport (e.g., httputil.ReverseProxy
	// or the retry transport). Without this, a panic kills the goroutine and
	// the client gets a connection close with no response. The recover()
	// must be registered AFTER DeregisterInFlight so it runs first during
	// panic unwinding (defers are LIFO) — it catches the panic, writes a
	// 502 through the statusRecorder so metrics and journal capture the error,
	// and the remaining defers (DeregisterInFlight, etc.) run normally.
	var recPtr *statusRecorder
	var entry *journal.Entry
	var reqBodyBuf *journal.CaptureBuf
	var finalized bool
	finalize := func(aborted bool) {
		if finalized {
			return
		}
		finalized = true

		status := 0
		if recPtr != nil {
			status = recPtr.status
		}

		// Finalize and record the journal entry BEFORE any counter publishes
		// (the same counter-before-journal shape
		// c9ff856 fixed for the aborted pair): a consumer that observes a
		// status bucket, the request ring, TotalProxied/TotalPassThrough, or
		// TotalAborted must find the journal entry already present. Aborted
		// responses deliberately leave ResponseComplete unset: headers/body
		// may have been partially accepted by the downstream ResponseWriter,
		// but the exchange did not complete cleanly. The Aborted flag is the
		// explicit outcome signal for the TUI/network log.
		if entry != nil {
			entry.Aborted = aborted
			if recPtr != nil && recPtr.hijacked && recPtr.status == http.StatusSwitchingProtocols {
				entry.StatusCode = http.StatusSwitchingProtocols
				entry.ResponseHeaders = recPtr.ResponseWriter.Header().Clone()
				entry.ContentType = recPtr.ResponseWriter.Header().Get("Content-Type")
			}
			if recPtr != nil && !recPtr.hijacked && !aborted {
				entry.Timing.ResponseComplete = time.Now()
			}
			// An exchange with no queue wait (passthrough, or a request ended
			// before admission began) gets QueueEnd equal to QueueStart, so
			// QueueDuration returns 0.
			if entry.Timing.QueueEnd.IsZero() {
				entry.Timing.QueueEnd = entry.Timing.QueueStart
			}
			if reqBodyBuf != nil {
				// One snapshot: the transport write loop can still be
				// appending (and can mark EOF) while finalize runs, so the
				// bytes and the flags must describe the same instant.
				body, truncated, complete := reqBodyBuf.Snapshot()
				entry.RequestBody = body
				// A request that declared no body (ContentLength 0) is never
				// read by the transport, so the capture cannot reach EOF: an
				// empty capture is complete, not truncated.
				entry.RequestBodyTruncated = r.ContentLength != 0 && (truncated || !complete)
			}
			if recPtr != nil {
				entry.ResponseBody = recPtr.capturedBody
				// Use bytesWritten as the recorder's observed response size.
				// Content-Length reflects the intended size; bytesWritten is how
				// many bytes the downstream ResponseWriter accepted before
				// completion/abort. A later flush failure can still mean those
				// bytes were buffered rather than delivered to the network peer.
				if aborted {
					entry.ResponseSize = recPtr.bytesWritten
				} else if recPtr.bytesWritten > 0 {
					entry.ResponseSize = recPtr.bytesWritten
				}
			}
			p.journal.Record(entry)
		}

		p.m.RecordStatus(status)
		if aborted {
			p.m.RecordAbortedRequest(r.Method, r.URL.Path, status, time.Since(start), limited)
		} else {
			p.m.RecordRequest(r.Method, r.URL.Path, status, time.Since(start), limited)
			// The clean-completion counter publishes here — after the journal
			// entry — instead of at the end of the handler body, so the
			// counter-then-journal window cannot reopen. The
			// admission-completed gate keeps the queue-timeout, cancel, and
			// circuit-rejection early returns out (they never counted as
			// clean completions), and aborted exchanges never reach this
			// branch.
			if recPtr != nil && recPtr.admissionCompleted {
				p.completionCounter(limited)
			}
		}
	}
	defer func() {
		if rv := recover(); rv != nil {
			if rv == http.ErrAbortHandler {
				finalize(true)
				panic(rv)
			}
			slog.Error("proxy panic", "panic", rv)
			if recPtr != nil {
				// Any recovered panic is an aborted exchange: the exchange never
				// completed cleanly, the clean-completion counters are skipped,
				// and the journal records Aborted with ResponseComplete unset.
				recPtr.recoverLocalPanic()
				finalize(true)
			} else {
				// No statusRecorder — write directly to the raw ResponseWriter.
				// This path should not happen in practice (recPtr is always set
				// below before any work begins), but handle it defensively.
				http.Error(w, "internal error", http.StatusBadGateway)
				p.m.RecordStatus(http.StatusBadGateway)
				p.m.RecordRequest(r.Method, r.URL.Path, http.StatusBadGateway, time.Since(start), limited)
				if limited {
					p.m.IncProxied()
				} else {
					p.m.IncPassThrough()
				}
			}
		}
	}()

	// Create journal entry for this request. Per AGENTS.md TUI Redaction
	// Constraint, the journal and TUI may show raw credential headers and URLs
	// — TUI output is not captured and is visible only to the local operator.
	if p.journal != nil {
		entry = &journal.Entry{
			Method:         r.Method,
			URL:            r.URL,
			RequestHeaders: r.Header.Clone(),
			Limited:        limited,
			Timing: journal.Timing{
				QueueStart: time.Now(),
			},
		}
	}

	// Wrap the response writer to capture response details.
	captureMax := int64(1 << 20) // fallback
	if p.journal != nil {
		captureMax = p.journal.MaxBodyBytes()
	}
	rec := &statusRecorder{
		ResponseWriter: w,
		status:         0,
		entry:          entry,
		captureMax:     captureMax,
	}
	recPtr = rec // wire to panic recovery so 502 is captured in metrics

	// Preserve whether the original downstream writer can support an HTTP/1
	// protocol upgrade. statusRecorder itself implements Hijack for
	// ReverseProxy compatibility, so later 101 validation must consult this
	// original-writer fact rather than asking the recorder.
	ctx := withSwitchingProtocolsResponseBodyState(r.Context())
	ctx = withDownstreamUpgradeSupport(ctx, responseWriterCanHijack(w))
	r = r.WithContext(ctx)

	// Wrap request body to capture inbound data for the journal.
	if p.journal != nil && r.Body != nil {
		var reqBodyReader *journal.CapturingReader
		reqBodyReader, reqBodyBuf = journal.TeeReadCloser(r.Body, captureMax)
		r.Body = reqBodyReader
	}

	// Dispatch to the admission paths. Transcoded routes are dispatched
	// inside serveLimited/servePassthrough (via lookupRouteTargets) so they
	// are bounded exactly like ordinary requests: limited routes acquire the
	// per-route and global limiter slots, and passthrough routes honor the
	// global limiter.
	if limited {
		p.serveLimited(rec, r, flightID)
	} else if catalog {
		p.serveCatalog(rec, r)
	} else {
		p.servePassthrough(rec, r, flightID)
	}

	finalize(recPtr != nil && recPtr.aborted)
}

// completionCounter records the exchange in the clean-completion counter
// selected by the admission path. finalize calls it after the journal entry
// is published, so a consumer that observes the
// counter finds the journal entry already present.
func (p *Proxy) completionCounter(limited bool) {
	if limited {
		p.m.IncProxied()
	} else {
		p.m.IncPassThrough()
	}
}

// serveTranscodeHandler runs a transcode handler with the per-request outcome
// sink wired. After the handler returns, the recorded outcome provenance is
// attached to the statusRecorder so breaker accounting and slot protection
// use it instead of status-code guessing. Recorder completion state is set
// from the outcome before breaker or metrics processing: an exchange whose
// downstream response was not fully written (client abort or failed write) is
// marked aborted so it is never counted as a clean completion.
func (p *Proxy) serveTranscodeHandler(w http.ResponseWriter, r *http.Request, handler http.Handler) {
	if rec, ok := w.(*statusRecorder); ok {
		rec.transcodeStream = true
	}
	ctx, sink := transcode.WithOutcomeSink(r.Context())
	handler.ServeHTTP(w, r.WithContext(ctx))
	// The handler records EXACTLY ONE outcome synchronously (its defer
	// guarantees one when no path recorded it): there is no non-blocking
	// branch that silently loses provenance, and a missing outcome is an
	// internal invariant violation.
	outcome, recorded := sink.Load()
	if !recorded {
		panic("transcode handler returned without recording an outcome")
	}
	if outcome.CircuitRejected {
		p.m.IncCircuitRejected()
	}
	if rec, ok := w.(*statusRecorder); ok {
		outcomeCopy := outcome
		rec.transcodeOutcome = &outcomeCopy
		if outcome.CircuitRejected {
			rec.transportErr = circuitbreaker.ErrCircuitOpen
		}
		// Completion is monotonic: the recorder's independent
		// write-failure observation (a short write or write error on
		// the raw ResponseWriter) can never be overwritten by an
		// outcome that claims a clean completion.
		rec.aborted = rec.aborted ||
			!outcomeCopy.DownstreamComplete ||
			rec.downstreamWriteFailed()
	}
}

// lookupTranscodeHandlerKey returns the transcode handler for an
// already-built key. The key is built once per request and shared with the
// native lookup so the hot path pays route-key construction at most once.
func (p *Proxy) lookupTranscodeHandlerKey(key transcode.RouteKey) http.Handler {
	if len(p.transcodeHandlerMap) == 0 {
		return nil
	}
	return p.transcodeHandlerMap[key]
}

// lookupRouteTargets resolves the request's method+path to its native route
// and transcode handler with a single route-key construction, so the paired
// lookups on the serve path never pay key construction twice. Dispatch is
// method-scoped: mappings are POST-only, so a non-POST method on a mapped
// path matches no key and passes through transparently.
func (p *Proxy) lookupRouteTargets(r *http.Request) (*NativeRoute, http.Handler) {
	if len(p.nativeRoutes) == 0 && len(p.transcodeHandlerMap) == 0 {
		return nil, nil
	}
	key, err := transcode.NewRouteKey(r.Method, r.URL.Path)
	if err != nil {
		return nil, nil
	}
	return p.lookupNativeRouteKey(key), p.lookupTranscodeHandlerKey(key)
}

// dispatchAfterNativeRoute answers one request whose method+path sits on a
// natively served route. Native-first: a model served on its own dialect is
// rewritten and forwarded through the transparent engine; a model whose
// dialect differs falls through to the transcode mapping on the same route
// when one exists, and otherwise rides the transparent engine verbatim.
// fallback is the transcode handler already resolved for this method+path by
// lookupRouteTargets at the serve site, so the miss path reuses it instead
// of rebuilding the route key for a second lookup. nativeError requests
// were already answered by the native path, so this does nothing for them.
func (p *Proxy) dispatchAfterNativeRoute(w http.ResponseWriter, r *http.Request, nr *NativeRoute, fallback http.Handler) {
	out, action := p.nativeRouteAction(w, r, nr)
	switch action {
	case nativeServe:
		p.inner.ServeHTTP(w, out)
	case nativeMiss:
		if fallback != nil {
			p.serveTranscodeHandler(w, out, fallback)
		} else {
			p.inner.ServeHTTP(w, out)
		}
	}
}
