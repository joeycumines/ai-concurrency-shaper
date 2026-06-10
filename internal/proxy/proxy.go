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
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/retry"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// Proxy is a concurrency-bounded reverse proxy.
type Proxy struct {
	inner         *httputil.ReverseProxy
	matcher       *route.Matcher
	limiter       *queue.Limiter
	m             *metrics.Collector
	timeout       time.Duration
	globalLimiter *queue.Limiter
	routeLimiters map[string]*queue.Limiter
	transport     http.RoundTripper
	journal       *journal.Journal
}

// Config holds the parameters for constructing a Proxy.
type Config struct {
	Upstream      *url.URL
	Matcher       *route.Matcher
	Limiter       *queue.Limiter
	Metrics       *metrics.Collector
	QueueTimeout  time.Duration
	GlobalLimiter *queue.Limiter
	RouteLimiters map[string]*queue.Limiter

	// Retry configuration.  When MaxRetries != 0 the proxy wraps
	// each upstream request in a retry.Transport.
	MaxRetries   int
	MaxBodyBytes int64

	// Transport is the base http.RoundTripper for outbound requests.
	// If nil, http.DefaultTransport is used.
	Transport http.RoundTripper

	// Journal is the shared request journal for devtools and retry.
	// When set, request/response pairs are recorded for inspection.
	Journal *journal.Journal
}

// New creates a Proxy from the given config.
func New(cfg Config) *Proxy {
	p := &Proxy{
		matcher:       cfg.Matcher,
		limiter:       cfg.Limiter,
		m:             cfg.Metrics,
		timeout:       cfg.QueueTimeout,
		globalLimiter: cfg.GlobalLimiter,
		routeLimiters: cfg.RouteLimiters,
		journal:       cfg.Journal,
	}

	// Build retry transport when retries are enabled.
	var transport http.RoundTripper = cfg.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if cfg.MaxRetries != 0 {
		transport = &retry.Transport{
			Inner:        transport,
			MaxRetries:   cfg.MaxRetries,
			MaxBodyBytes: cfg.MaxBodyBytes,
		}
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = cfg.Upstream.Scheme
			req.URL.Host = cfg.Upstream.Host
			req.URL.Path = cfg.Upstream.Path + req.URL.Path
			req.Host = cfg.Upstream.Host
			req.Header.Del("X-Forwarded-For")
			req.Header.Del("X-Forwarded-Host")
			req.Header.Del("X-Forwarded-Proto")
		},
		Transport: p,
	}
	p.inner = rp
	p.transport = transport
	return p
}

// Metrics returns the shared metrics collector.
func (p *Proxy) Metrics() *metrics.Collector {
	return p.m
}

// Journal returns the shared request journal.
func (p *Proxy) Journal() *journal.Journal {
	return p.journal
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	limited := p.matcher.IsLimited(r.Method, r.URL.Path)

	flightID := p.m.RegisterInFlight(r.Method, r.URL.Path, limited)
	defer p.m.DeregisterInFlight(flightID)

	// Create journal entry for this request.
	var entry *journal.Entry
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

	start := time.Now()

	// Wrap request body to capture inbound data for the journal.
	var reqBodyBuf *journal.CaptureBuf
	if p.journal != nil && r.Body != nil {
		var reqBodyReader *journal.CapturingReader
		reqBodyReader, reqBodyBuf = journal.TeeReadCloser(r.Body, captureMax)
		r.Body = reqBodyReader
	}

	if limited {
		p.serveLimited(rec, r, flightID)
	} else {
		p.servePassthrough(rec, r, flightID)
	}

	p.m.RecordStatus(rec.status)
	p.m.RecordRequest(r.Method, r.URL.Path, rec.status, time.Since(start), limited)

	// Finalize and record the journal entry.
	if entry != nil {
		if !rec.hijacked {
			entry.Timing.ResponseComplete = time.Now()
		}
		// For passthrough requests, there is no queue phase —
		// set QueueEnd equal to QueueStart so QueueDuration returns 0.
		if entry.Timing.QueueEnd.IsZero() {
			entry.Timing.QueueEnd = entry.Timing.QueueStart
		}
		if reqBodyBuf != nil {
			entry.RequestBody = reqBodyBuf.Bytes()
		}
		entry.ResponseBody = rec.capturedBody
		// Use bytesWritten as the ground truth for the actual response
		// size. Content-Length (stored in ResponseSize during WriteHeader)
		// reflects the *intended* size, but on a short write (client
		// disconnect mid-transfer) bytesWritten < Content-Length.
		// When bytesWritten is positive it always overrides because it
		// reflects the actual bytes delivered to the client.
		if rec.bytesWritten > 0 {
			entry.ResponseSize = rec.bytesWritten
		}
		p.journal.Record(entry)
	}
}

func (p *Proxy) servePassthrough(w http.ResponseWriter, r *http.Request, flightID uint64) {
	p.m.IncPassThrough()

	if p.globalLimiter != nil {
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

		release, err := p.globalLimiter.Acquire(r.Context())
		p.m.DecQueued()

		if err != nil {
			// Record the moment the global limiter wait ended so that
			// QueueDuration reflects the actual queue time, not zero.
			if rec, ok := w.(*statusRecorder); ok && rec.entry != nil {
				rec.entry.Timing.QueueEnd = time.Now()
			}
			p.m.IncCancelled()
			http.Error(w, "request canceled", http.StatusServiceUnavailable)
			return
		}
		defer release()

		// Record the end of the queue phase so QueueDuration reflects
		// global limiter wait time, not zero.
		if rec, ok := w.(*statusRecorder); ok && rec.entry != nil {
			rec.entry.Timing.QueueEnd = time.Now()
		}

		p.m.IncActive()
		defer p.m.DecActive()
	}

	p.m.MarkInFlightStarted(flightID)
	p.inner.ServeHTTP(w, r)
}

func (p *Proxy) serveLimited(w http.ResponseWriter, r *http.Request, flightID uint64) {
	ctx := r.Context()
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	p.m.IncQueued()
	// Record the start of the queue phase precisely at the moment
	// the request begins waiting for a limiter slot, overriding
	// the coarse ServeHTTP-entry timestamp so QueueDuration
	// measures only actual queue wait time.
	if rec, ok := w.(*statusRecorder); ok && rec.entry != nil {
		rec.entry.Timing.QueueStart = time.Now()
	}
	release, err := p.acquireSlot(ctx, r.Method, r.URL.Path)
	p.m.DecQueued()

	if err != nil {
		// Record the moment the queue wait ended so that QueueDuration
		// reflects the actual time spent waiting, not zero.
		if rec, ok := w.(*statusRecorder); ok && rec.entry != nil {
			rec.entry.Timing.QueueEnd = time.Now()
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
	defer release()

	if p.globalLimiter != nil {
		globalRelease, err := p.globalLimiter.Acquire(ctx)
		if err != nil {
			// Record the moment the global limiter wait ended so that
			// QueueDuration reflects the full queue time, not zero.
			if rec, ok := w.(*statusRecorder); ok && rec.entry != nil {
				rec.entry.Timing.QueueEnd = time.Now()
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
		defer globalRelease()
	}

	// Record the moment the slot was acquired — this is the end of queuing.
	// QueueEnd is set AFTER both the per-route limiter and the global
	// limiter so that global queue latency is NOT absorbed into TTFB.
	if rec, ok := w.(*statusRecorder); ok && rec.entry != nil {
		rec.entry.Timing.QueueEnd = time.Now()
	}

	p.m.IncActive()
	defer p.m.DecActive()

	p.m.MarkInFlightStarted(flightID)

	p.inner.ServeHTTP(w, r)
	p.m.IncProxied()
}

func (p *Proxy) acquireSlot(ctx context.Context, method, path string) (func(), error) {
	if pat := p.matcher.FindMatch(method, path); pat != nil {
		key := pat.Group
		if key == "" {
			key = pat.Raw
		}
		if lim, ok := p.routeLimiters[key]; ok {
			return lim.Acquire(ctx)
		}
	}
	return p.limiter.Acquire(ctx)
}

// RoundTrip implements http.RoundTripper, delegating to the retry-aware
// transport set up during construction.
func (p *Proxy) RoundTrip(r *http.Request) (*http.Response, error) {
	return p.transport.RoundTrip(r)
}

// statusRecorder wraps a ResponseWriter to capture the status code,
// response headers, and response body for the journal.
type statusRecorder struct {
	http.ResponseWriter
	status          int
	entry           *journal.Entry
	capturedBody    []byte
	captureMax      int64
	captureDone     bool
	bytesWritten    int64 // total bytes written through Write (may exceed capturedBody)
	hijacked        bool
	terminalWritten bool // true once a terminal status (>=200) has been recorded
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) WriteHeader(code int) {
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

	r.status = code
	if r.entry != nil {
		r.entry.StatusCode = code
		r.entry.ResponseHeaders = r.ResponseWriter.Header().Clone()
		r.entry.Timing.ResponseHeaders = time.Now()
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
	// If WriteHeader was never called for a terminal status, the Go runtime
	// will trigger an implicit WriteHeader(StatusOK) inside
	// ResponseWriter.Write — which bypasses our override. 1xx informational
	// responses also call WriteHeader, so we must distinguish terminal
	// (>=200) from informational (<200) status codes. Capture
	// headers/status here on the first terminal write.
	if !r.terminalWritten {
		r.status = http.StatusOK
		if r.entry != nil {
			r.entry.StatusCode = http.StatusOK
			r.entry.ResponseHeaders = r.ResponseWriter.Header().Clone()
			r.entry.Timing.ResponseHeaders = time.Now()
			r.entry.ContentType = r.ResponseWriter.Header().Get("Content-Type")
			// If the handler never set Content-Type, Go runs MIME sniffing
			// during ResponseWriter.Write. Clone captured the headers *before*
			// sniffing, so detect the type from the body bytes ourselves.
			if r.entry.ContentType == "" {
				r.entry.ContentType = http.DetectContentType(b)
				// Sync the MIME-sniffed type into the cloned headers so
				// the TUI detail overlay shows it in both the Type field
				// and the Headers list.
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

	// Perform the actual write before capturing the body so we can
	// record only the bytes that were actually delivered (b[:n]).
	// The MIME-sniffing logic above reads b without mutation, so
	// there is no conflict with moving capture after the write.
	n, err := r.ResponseWriter.Write(b)
	// Track total bytes actually written (not attempted) so that
	// ResponseSize reflects the true payload delivered to the client.
	// On a short write (n < len(b)), typically caused by client
	// disconnect mid-transfer, only the bytes delivered are counted.
	r.bytesWritten += int64(n)

	// Capture only the bytes that were actually delivered to the
	// client. On a short write (n < len(b)), b[:n] reflects only
	// what made it through, keeping capturedBody consistent with
	// both bytesWritten and ResponseSize.
	if r.entry != nil && !r.captureDone && n > 0 {
		delivered := b[:n]
		if r.capturedBody == nil {
			// Preallocate only when the response size is known and small.
			if r.entry.ResponseSize > 0 && r.entry.ResponseSize < r.captureMax {
				r.capturedBody = make([]byte, 0, r.entry.ResponseSize)
			}
			// For unknown/large sizes, leave r.capturedBody nil so append
			// grows the backing array on demand instead of preallocating.
		}
		remaining := r.captureMax - int64(len(r.capturedBody))
		if remaining > 0 {
			if int64(len(delivered)) > remaining {
				r.capturedBody = append(r.capturedBody, delivered[:remaining]...)
				r.captureDone = true
			} else {
				r.capturedBody = append(r.capturedBody, delivered...)
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

// Hijack forwards the Hijack call if the underlying writer supports
// http.Hijacker. This future-proofs the proxy for WebSocket upgrades.
// The hijacked flag is only set on successful hijack to avoid
// corrupting the journal's ResponseComplete timing on failed attempts.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		conn, brw, err := h.Hijack()
		if err == nil {
			r.hijacked = true
		}
		return conn, brw, err
	}
	return nil, nil, http.ErrNotSupported
}
