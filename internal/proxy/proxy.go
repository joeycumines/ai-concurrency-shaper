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
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/retry"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// --- Option Interface ---

// Option configures a Proxy.
type Option interface {
	applyProxyOption(cfg *proxyConfig) error
}

// --- Unexported Config Struct ---

type proxyConfig struct {
	upstream               *url.URL
	matcher                *route.Matcher
	limiter                *queue.Limiter
	metrics                *metrics.Collector
	queueTimeout           time.Duration
	globalLimiter          *queue.Limiter
	routeLimiters          map[string]*queue.Limiter
	maxRetries             int
	maxBodyBytes           int64
	retryWaitMin           time.Duration
	retryWaitMax           time.Duration
	retryMinDelay          time.Duration
	retrySkipOn429         bool
	cancelCooldown         time.Duration
	failureHold            time.Duration
	transport              http.RoundTripper
	journal                *journal.Journal
	breaker                *circuitbreaker.Breaker
	adaptiveHeadroom       bool
	adaptiveHeadroomWindow time.Duration
}

// --- Concrete Options ---

// UpstreamOption sets the upstream URL to proxy to.
type UpstreamOption struct {
	value *url.URL
}

// WithUpstream returns an option that sets the upstream URL. Required.
func WithUpstream(u *url.URL) *UpstreamOption {
	return &UpstreamOption{value: u}
}

func (o *UpstreamOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value == nil {
		return errors.New("proxy: upstream must not be nil")
	}
	if o.value.Scheme == "" {
		return errors.New("proxy: upstream URL must include scheme (http or https)")
	}
	cfg.upstream = o.value
	return nil
}

// MatcherOption sets the route matcher for determining limited routes.
type MatcherOption struct {
	value *route.Matcher
}

// WithMatcher returns an option that sets the route matcher. Required.
func WithMatcher(m *route.Matcher) *MatcherOption {
	return &MatcherOption{value: m}
}

func (o *MatcherOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value == nil {
		return errors.New("proxy: matcher must not be nil")
	}
	cfg.matcher = o.value
	return nil
}

// LimiterOption sets the concurrency limiter for limited routes.
type LimiterOption struct {
	value *queue.Limiter
}

// WithLimiter returns an option that sets the default concurrency limiter. Required.
func WithLimiter(l *queue.Limiter) *LimiterOption {
	return &LimiterOption{value: l}
}

func (o *LimiterOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value == nil {
		return errors.New("proxy: limiter must not be nil")
	}
	cfg.limiter = o.value
	return nil
}

// MetricsOption sets the metrics collector.
type MetricsOption struct {
	value *metrics.Collector
}

// WithMetrics returns an option that sets the metrics collector. Required.
func WithMetrics(m *metrics.Collector) *MetricsOption {
	return &MetricsOption{value: m}
}

func (o *MetricsOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value == nil {
		return errors.New("proxy: metrics must not be nil")
	}
	cfg.metrics = o.value
	return nil
}

// QueueTimeoutOption sets the maximum wait time for a concurrency slot.
type QueueTimeoutOption struct {
	value time.Duration
}

// WithQueueTimeout returns an option that sets the queue timeout.
// Zero means use the request context deadline.
func WithQueueTimeout(d time.Duration) *QueueTimeoutOption {
	return &QueueTimeoutOption{value: d}
}

func (o *QueueTimeoutOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value < 0 {
		return fmt.Errorf("proxy: queue timeout must be >= 0, got %v", o.value)
	}
	cfg.queueTimeout = o.value
	return nil
}

// GlobalLimiterOption sets an optional global concurrency limiter.
type GlobalLimiterOption struct {
	value *queue.Limiter
}

// WithGlobalLimiter returns an option that sets the global concurrency limiter.
// Nil means no global limit.
func WithGlobalLimiter(l *queue.Limiter) *GlobalLimiterOption {
	return &GlobalLimiterOption{value: l}
}

func (o *GlobalLimiterOption) applyProxyOption(cfg *proxyConfig) error {
	cfg.globalLimiter = o.value
	return nil
}

// RouteLimitersOption sets per-route concurrency limiters.
type RouteLimitersOption struct {
	value map[string]*queue.Limiter
}

// WithRouteLimiters returns an option that sets per-route limiters.
func WithRouteLimiters(m map[string]*queue.Limiter) *RouteLimitersOption {
	return &RouteLimitersOption{value: m}
}

func (o *RouteLimitersOption) applyProxyOption(cfg *proxyConfig) error {
	cfg.routeLimiters = o.value
	return nil
}

// MaxRetriesOption sets the maximum retry attempts.
type MaxRetriesOption struct {
	value int
}

// WithMaxRetries returns an option that sets the max retry count.
// -1 means unlimited, 0 means disabled.
func WithMaxRetries(n int) *MaxRetriesOption {
	return &MaxRetriesOption{value: n}
}

func (o *MaxRetriesOption) applyProxyOption(cfg *proxyConfig) error {
	cfg.maxRetries = o.value
	return nil
}

// MaxBodyBytesOption sets the max request body size eligible for retry.
type MaxBodyBytesOption struct {
	value int64
}

// WithMaxBodyBytes returns an option that sets the max retry body size.
func WithMaxBodyBytes(n int64) *MaxBodyBytesOption {
	return &MaxBodyBytesOption{value: n}
}

func (o *MaxBodyBytesOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value < 0 {
		return fmt.Errorf("proxy: max body bytes must be >= 0, got %d", o.value)
	}
	cfg.maxBodyBytes = o.value
	return nil
}

// RetryWaitMinOption sets the minimum retry wait duration.
type RetryWaitMinOption struct {
	value time.Duration
}

// WithRetryWaitMin returns an option that sets the minimum retry wait.
func WithRetryWaitMin(d time.Duration) *RetryWaitMinOption {
	return &RetryWaitMinOption{value: d}
}

func (o *RetryWaitMinOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value < 0 {
		return fmt.Errorf("proxy: retry wait min must be >= 0, got %v", o.value)
	}
	cfg.retryWaitMin = o.value
	return nil
}

// RetryWaitMaxOption sets the maximum retry wait duration.
type RetryWaitMaxOption struct {
	value time.Duration
}

// WithRetryWaitMax returns an option that sets the maximum retry wait.
func WithRetryWaitMax(d time.Duration) *RetryWaitMaxOption {
	return &RetryWaitMaxOption{value: d}
}

func (o *RetryWaitMaxOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value < 0 {
		return fmt.Errorf("proxy: retry wait max must be >= 0, got %v", o.value)
	}
	cfg.retryWaitMax = o.value
	return nil
}

// TransportOption sets the base HTTP transport.
type TransportOption struct {
	value http.RoundTripper
}

// WithTransport returns an option that sets the base HTTP transport.
// Nil means http.DefaultTransport.
func WithTransport(t http.RoundTripper) *TransportOption {
	return &TransportOption{value: t}
}

func (o *TransportOption) applyProxyOption(cfg *proxyConfig) error {
	cfg.transport = o.value
	return nil
}

// JournalOption sets the request journal.
type JournalOption struct {
	value *journal.Journal
}

// WithJournal returns an option that sets the request journal.
// Nil means no journaling.
func WithJournal(j *journal.Journal) *JournalOption {
	return &JournalOption{value: j}
}

func (o *JournalOption) applyProxyOption(cfg *proxyConfig) error {
	cfg.journal = o.value
	return nil
}

// BreakerOption sets the circuit breaker.
type BreakerOption struct {
	value *circuitbreaker.Breaker
}

// WithBreaker returns an option that sets the circuit breaker.
// Nil means no breaker.
//
// When retries are enabled (MaxRetries != 0), the retry transport reports
// ALL request outcomes to the breaker — including passthrough requests
// that bypass the proxy's concurrency limiter. This means passthrough
// failures can influence circuit state. This is intentional: if downstream
// is unhealthy for any traffic, limited traffic should also back off.
func WithBreaker(b *circuitbreaker.Breaker) *BreakerOption {
	return &BreakerOption{value: b}
}

func (o *BreakerOption) applyProxyOption(cfg *proxyConfig) error {
	cfg.breaker = o.value
	return nil
}

// RetryMinDelayOption sets a floor for the retry wait duration.
type RetryMinDelayOption struct {
	value time.Duration
}

// WithRetryMinDelay returns an option that sets the minimum retry delay.
// This gives the downstream service time to complete its accounting before
// the retry arrives (KILL-05 mitigation). Zero means no floor.
func WithRetryMinDelay(d time.Duration) *RetryMinDelayOption {
	return &RetryMinDelayOption{value: d}
}

func (o *RetryMinDelayOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value < 0 {
		return fmt.Errorf("proxy: retry min delay must be >= 0, got %v", o.value)
	}
	cfg.retryMinDelay = o.value
	return nil
}

// CancelCooldownOption sets a brief slot hold after a client disconnect
// that reached upstream.
type CancelCooldownOption struct {
	value time.Duration
}

// WithCancelCooldown returns an option that sets the client-cancel cooldown.
// When a client disconnects after the request reached upstream, the slot
// is held for this duration before re-admission (KILL-04 mitigation).
// Zero means no cooldown (immediate release on client cancel).
func WithCancelCooldown(d time.Duration) *CancelCooldownOption {
	return &CancelCooldownOption{value: d}
}

func (o *CancelCooldownOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value < 0 {
		return fmt.Errorf("proxy: cancel cooldown must be >= 0, got %v", o.value)
	}
	cfg.cancelCooldown = o.value
	return nil
}

// RetrySkipOn429Option configures whether 429 responses should be skipped
// by the retry transport to prevent amplification loops.
type RetrySkipOn429Option struct {
	value bool
}

// WithRetrySkipOn429 returns an option that controls whether 429 (Too Many
// Requests) responses are retried. When true, the retry transport does NOT
// retry 429s, preventing the positive feedback loop where retries amplify
// the concurrency issue that caused the 429 in the first place.
func WithRetrySkipOn429(skip bool) *RetrySkipOn429Option {
	return &RetrySkipOn429Option{value: skip}
}

func (o *RetrySkipOn429Option) applyProxyOption(cfg *proxyConfig) error {
	cfg.retrySkipOn429 = o.value
	return nil
}

// FailureHoldOption sets a standalone slot hold duration after upstream failure.
type FailureHoldOption struct {
	value time.Duration
}

// WithFailureHold returns an option that sets the slot hold duration after
// an upstream failure (5xx, 429). This hold applies when the circuit breaker
// is disabled or when the breaker's phantom penalty is zero. When the breaker
// is enabled with a non-zero penalty, the phantom penalty handles failure-path
// holds instead. The hold is released asynchronously so the HTTP handler
// returns immediately. Zero means no hold (disabled).
func WithFailureHold(d time.Duration) *FailureHoldOption {
	return &FailureHoldOption{value: d}
}

func (o *FailureHoldOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value < 0 {
		return fmt.Errorf("proxy: failure hold must be >= 0, got %v", o.value)
	}
	cfg.failureHold = o.value
	return nil
}

// AdaptiveHeadroomOption enables dynamic slot withholding after a 429.
type AdaptiveHeadroomOption struct {
	value bool
}

// WithAdaptiveHeadroom returns an option that enables adaptive concurrency
// headroom. When enabled, a 429 response on a limited route temporarily
// reduces the effective limit of that route's limiter by one slot, creating
// headroom for provider-side accounting/teardown races.
func WithAdaptiveHeadroom(enabled bool) *AdaptiveHeadroomOption {
	return &AdaptiveHeadroomOption{value: enabled}
}

func (o *AdaptiveHeadroomOption) applyProxyOption(cfg *proxyConfig) error {
	cfg.adaptiveHeadroom = o.value
	return nil
}

// AdaptiveHeadroomWindowOption sets how long the one-slot 429 headroom lasts.
type AdaptiveHeadroomWindowOption struct {
	value time.Duration
}

// WithAdaptiveHeadroomWindow returns an option that sets the adaptive headroom
// recovery window. Each new 429 on the affected route resets this timer.
func WithAdaptiveHeadroomWindow(d time.Duration) *AdaptiveHeadroomWindowOption {
	return &AdaptiveHeadroomWindowOption{value: d}
}

func (o *AdaptiveHeadroomWindowOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value < 0 {
		return fmt.Errorf("proxy: adaptive headroom window must be >= 0, got %v", o.value)
	}
	cfg.adaptiveHeadroomWindow = o.value
	return nil
}

// --- Compile-Time Compliance Checks ---

var (
	_ Option = (*UpstreamOption)(nil)
	_ Option = (*MatcherOption)(nil)
	_ Option = (*LimiterOption)(nil)
	_ Option = (*MetricsOption)(nil)
	_ Option = (*QueueTimeoutOption)(nil)
	_ Option = (*GlobalLimiterOption)(nil)
	_ Option = (*RouteLimitersOption)(nil)
	_ Option = (*MaxRetriesOption)(nil)
	_ Option = (*MaxBodyBytesOption)(nil)
	_ Option = (*RetryWaitMinOption)(nil)
	_ Option = (*RetryWaitMaxOption)(nil)
	_ Option = (*TransportOption)(nil)
	_ Option = (*JournalOption)(nil)
	_ Option = (*BreakerOption)(nil)
	_ Option = (*RetryMinDelayOption)(nil)
	_ Option = (*CancelCooldownOption)(nil)
	_ Option = (*RetrySkipOn429Option)(nil)
	_ Option = (*FailureHoldOption)(nil)
	_ Option = (*AdaptiveHeadroomOption)(nil)
	_ Option = (*AdaptiveHeadroomWindowOption)(nil)
)

// --- Factory ---

// Proxy is a concurrency-bounded reverse proxy.
type Proxy struct {
	inner          *httputil.ReverseProxy
	matcher        *route.Matcher
	limiter        *queue.Limiter
	m              *metrics.Collector
	timeout        time.Duration
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

	// adaptiveHeadroom enables one-slot concurrency reduction on 429s.
	adaptiveHeadroom bool

	// adaptiveHeadroomWindow is how long the one-slot reduction lasts.
	adaptiveHeadroomWindow time.Duration
}

// New creates a Proxy from the given options.
// Returns an error if required options are missing or validation fails.
func New(opts ...Option) (*Proxy, error) {
	var cfg proxyConfig
	for _, o := range opts {
		if err := o.applyProxyOption(&cfg); err != nil {
			return nil, err
		}
	}

	// Validate required fields.
	if cfg.upstream == nil {
		return nil, errors.New("proxy: upstream is required")
	}
	if cfg.matcher == nil {
		return nil, errors.New("proxy: matcher is required")
	}
	if cfg.limiter == nil {
		return nil, errors.New("proxy: limiter is required")
	}
	if cfg.metrics == nil {
		return nil, errors.New("proxy: metrics is required")
	}

	// Build retry transport when retries are enabled.
	var transport http.RoundTripper = cfg.transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if cfg.maxRetries != 0 {
		var checkRetry retry.CheckRetry
		if cfg.retrySkipOn429 {
			checkRetry = func(resp *http.Response, err error) bool {
				if err != nil {
					return true
				}
				if resp == nil {
					return false
				}
				// Skip 429 — retrying rate-limited responses amplifies
				// the concurrency issue that caused the 429.
				return resp.StatusCode >= 500
			}
		}
		transport = &retry.Transport{
			Inner:         transport,
			MaxRetries:    cfg.maxRetries,
			MaxBodyBytes:  cfg.maxBodyBytes,
			WaitMin:       cfg.retryWaitMin,
			WaitMax:       cfg.retryWaitMax,
			MinRetryDelay: cfg.retryMinDelay,
			Breaker:       cfg.breaker,
			CheckRetry:    checkRetry,
		}
	}

	// Track whether the retry transport handles breaker reporting.
	var retryHandlesBreaker bool
	if rt, ok := transport.(*retry.Transport); ok {
		if rt.Breaker != nil {
			retryHandlesBreaker = true
		}
		// Wire the in-flight retry counter for TUI visibility (KILL-01/03).
		// The counter is the metrics collector's atomic, so the TUI sees
		// retry pressure through the snapshot cycle.
		rt.InFlightRetries = cfg.metrics.RetriesInFlightCounter()
	}

	p := &Proxy{
		matcher:                cfg.matcher,
		limiter:                cfg.limiter,
		m:                      cfg.metrics,
		timeout:                cfg.queueTimeout,
		globalLimiter:          cfg.globalLimiter,
		routeLimiters:          cfg.routeLimiters,
		journal:                cfg.journal,
		breaker:                cfg.breaker,
		cancelCooldown:         cfg.cancelCooldown,
		failureHold:            cfg.failureHold,
		retryHandlesBreaker:    retryHandlesBreaker,
		adaptiveHeadroom:       cfg.adaptiveHeadroom,
		adaptiveHeadroomWindow: cfg.adaptiveHeadroomWindow,
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = cfg.upstream.Scheme
			req.URL.Host = cfg.upstream.Host
			req.URL.Path = cfg.upstream.Path + req.URL.Path
			req.Host = cfg.upstream.Host
			req.Header.Del("X-Forwarded-For")
			req.Header.Del("X-Forwarded-Host")
			req.Header.Del("X-Forwarded-Proto")
		},
		Transport: p,
	}
	p.inner = rp
	p.transport = transport
	return p, nil
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

	start := time.Now()

	// Recover from panics in the inner transport (e.g., httputil.ReverseProxy
	// or the retry transport). Without this, a panic kills the goroutine and
	// the client gets a connection close with no response. The recover()
	// must be registered AFTER DeregisterInFlight so it runs first during
	// panic unwinding (defers are LIFO) — it catches the panic, writes a
	// 502 through the statusRecorder so metrics and journal capture the error,
	// and the remaining defers (DeregisterInFlight, etc.) run normally.
	var recPtr *statusRecorder
	var metRecorded bool // set to true when normal-flow RecordStatus runs
	defer func() {
		if rv := recover(); rv != nil {
			log.Printf("proxy panic: %v", rv)
			if recPtr != nil {
				http.Error(recPtr, "internal error", http.StatusBadGateway)
				if !metRecorded {
					p.m.RecordStatus(recPtr.status)
					p.m.RecordRequest(r.Method, r.URL.Path, recPtr.status, time.Since(start), limited)
					if limited {
						p.m.IncProxied()
					} else {
						p.m.IncPassThrough()
					}
				}
			} else {
				// No statusRecorder — write directly to the raw ResponseWriter.
				// This path should not happen in practice (recPtr is always set
				// below before any work begins), but handle it defensively.
				http.Error(w, "internal error", http.StatusBadGateway)
				if !metRecorded {
					p.m.RecordStatus(http.StatusBadGateway)
					p.m.RecordRequest(r.Method, r.URL.Path, http.StatusBadGateway, time.Since(start), limited)
					if limited {
						p.m.IncProxied()
					} else {
						p.m.IncPassThrough()
					}
				}
			}
		}
	}()

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
	recPtr = rec // wire to panic recovery so 502 is captured in metrics

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
	metRecorded = true
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
	var passthroughReachedUpstream bool
	var localPanic bool

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
		// Inject the breaker epoch into the request context so the retry
		// transport can use it for the first attempt's RecordFailure/
		// RecordSuccess calls. Without this, the retry transport's
		// breakerEpoch would be 0 for attempt 0, bypassing the stale-probe
		// guard in circuitbreaker.RecordFailure (review-06 Finding 1).
		r = r.WithContext(context.WithValue(r.Context(), retry.BreakerEpochKey, breakerEpoch))
	}

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
			p.m.IncCancelled()
			http.Error(w, "request canceled", http.StatusServiceUnavailable)
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
			isClientCancel := ctxErr != nil && (errors.Is(ctxErr, context.Canceled) || errors.Is(ctxErr, context.DeadlineExceeded))
			isTransportOrProxyError := recOK && (rec.status == 0 || rec.status == http.StatusBadGateway)
			// Single anchor for classification decisions in this defer.
			now := time.Now()
			// Phantom penalty: hold the global-limiter slot after an upstream failure
			// when the circuit breaker is enabled and the penalty is non-zero. If the
			// penalty is zero (currently unreachable — basePenalty is always > 0 — but
			// defensive), fall through to check failureHold.
			if p.breaker != nil && recOK && passthroughReachedUpstream && !(isClientCancel && isTransportOrProxyError) && isUpstreamFailureStatus(rec, now) {
				penalty := p.breaker.PenaltyDuration()
				if penalty > 0 {
					time.AfterFunc(penalty, release)
					return
				}
				// Penalty is zero — fall through to check failureHold below.
			}
			if isClientCancel && passthroughReachedUpstream && p.cancelCooldown > 0 {
				// Client disconnected after the request reached upstream. Hold
				// the slot briefly to prevent N+1 observed concurrency from
				// downstream accounting lag (KILL-04 mitigation). Unlike
				// phantom penalty and failure hold, the cancelCooldown does NOT
				// use the isTransportOrProxyError guard — it MUST fire even when
				// rec.status == 0 (upstream still processing). See
				// serveLimited's cancel-cooldown comment for the full rationale.
				time.AfterFunc(p.cancelCooldown, release)
				return
			}
			if p.failureHold > 0 && recOK && passthroughReachedUpstream && !(isClientCancel && isTransportOrProxyError) && isUpstreamFailureStatus(rec, now) {
				// Standalone failure hold: holds the global-limiter slot after an
				// upstream failure. This applies when the circuit breaker is disabled
				// or when the breaker penalty is zero. Mirrors serveLimited.
				time.AfterFunc(p.failureHold, release)
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
	passthroughReachedUpstream = true

	// Inner panic recovery: catch panics from the inner transport
	// so that the slot-release defer and breaker reporting see the correct
	// status code. A local panic is NOT an upstream failure — the
	// localPanic flag prevents phantom penalty, failure hold, and
	// breaker recording from treating a proxy-internal crash as a
	// downstream error.
	func() {
		defer func() {
			if rv := recover(); rv != nil {
				log.Printf("proxy panic in servePassthrough: %v", rv)
				if rec, ok := w.(*statusRecorder); ok && !rec.terminalWritten {
					http.Error(rec, "internal error", http.StatusBadGateway)
				}
				localPanic = true
			}
		}()
		p.inner.ServeHTTP(w, r)
	}()

	// Feed failure/success signals to the circuit breaker when the
	// retry transport is NOT handling it (i.e., breaker configured
	// without retries). When the retry transport IS active with a
	// breaker, it reports every attempt's outcome — the proxy must
	// not duplicate that or it would double-count failures.
	if p.breaker != nil && !p.retryHandlesBreaker && !localPanic {
		rec, recOK := w.(*statusRecorder)
		if recOK {
			// Client-initiated context cancellation (e.g., the user
			// closed their browser tab) is NOT an upstream failure —
			// do not feed it to the breaker. An attacker could
			// otherwise trip the breaker by initiating and
			// immediately dropping connections. This mirrors the
			// isClientCancel guard in the retry transport. Note:
			// unlike the retry transport which checks the RoundTrip
			// error directly, the proxy uses r.Context().Err() because
			// httputil.ReverseProxy.ServeHTTP writes to the
			// ResponseWriter instead of returning a Go error.
			ctxErr := r.Context().Err()
			isClientCancel := ctxErr != nil && (errors.Is(ctxErr, context.Canceled) || errors.Is(ctxErr, context.DeadlineExceeded))
			// Only skip recording for transport errors (status 0) and
			// proxy-generated 502 errors when the client cancelled. The
			// proxy's error handler writes 502 on context cancellation —
			// this is NOT an upstream failure. Any other 5xx (500, 503,
			// 504, 429) came from the upstream and MUST be reported
			// regardless of whether the client disconnected mid-response.
			isTransportOrProxyError := rec.status == 0 || rec.status == http.StatusBadGateway
			// Evaluate classification and Retry-After under a single time
			// anchor so a slow response body cannot create a split-brain state.
			now := time.Now()
			if !(isClientCancel && isTransportOrProxyError) && isUpstreamFailureStatus(rec, now) {
				p.breaker.RecordFailure(rec.status, parseRetryAfterFromRecorder(rec, now), proxyStart, breakerEpoch)
			} else if rec.status >= 200 && rec.status < 300 {
				p.breaker.RecordSuccess(proxyStart, breakerEpoch)
			}
		}
	}
	// Count the passthrough request only after it has actually been
	// forwarded AND the breaker logic has run. This placement mirrors
	// serveLimited's IncProxied() at the end of that function. If a
	// panic occurs in the breaker logic above, IncPassThrough is NOT
	// called — the outer ServeHTTP recovery handles metrics recording,
	// calling IncPassThrough exactly once (via the !metRecorded guard).
	// Placing this BEFORE the breaker logic would cause a double-count
	// on panic: once here, once in the outer recovery.
	p.m.IncPassThrough()
}

func (p *Proxy) serveLimited(w http.ResponseWriter, r *http.Request, flightID uint64) {
	ctx := r.Context()
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	// Reject immediately if the circuit breaker is OPEN.
	var breakerEpoch uint64
	if p.breaker != nil {
		var err error
		breakerEpoch, err = p.breaker.Allow()
		if err != nil {
			p.m.IncCircuitRejected()
			http.Error(w, "circuit open", http.StatusServiceUnavailable)
			return
		}
		// Inject the breaker epoch into the request context so the retry
		// transport can use it for the first attempt's RecordFailure/
		// RecordSuccess calls. Without this, the retry transport's
		// breakerEpoch would be 0 for attempt 0, bypassing the stale-probe
		// guard in circuitbreaker.RecordFailure (review-06 Finding 1).
		r = r.WithContext(context.WithValue(r.Context(), retry.BreakerEpochKey, breakerEpoch))
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
	var localPanic bool // set by inner recovery when a local panic (not upstream failure) occurs
	defer func() {
		// A local panic (e.g., nil pointer in proxy code) is NOT an upstream
		// failure — do NOT apply phantom penalty, failure hold, or cancel
		// cooldown. The 502 written by the inner recovery reflects a local
		// bug, not a downstream error, so holding the slot would penalize
		// legitimate traffic for a proxy-internal problem.
		if localPanic {
			release()
			return
		}
		rec, recOK := w.(*statusRecorder)

		// Adaptive headroom: a 429 from the upstream is evidence that the
		// provider observed more concurrent load than the proxy intended.
		// Temporarily reduce the effective limit of the limiter that owns this
		// slot by one, creating breathing room for teardown/accounting races.
		if p.adaptiveHeadroom && recOK && reachedUpstream && slotLimiter != nil && rec.status == http.StatusTooManyRequests {
			slotLimiter.AdaptiveReduce(p.adaptiveHeadroomWindow)
		}

		ctxErr := r.Context().Err()
		isClientCancel := ctxErr != nil && (errors.Is(ctxErr, context.Canceled) || errors.Is(ctxErr, context.DeadlineExceeded))
		// Only suppress phantom penalty/failureHold when the client cancelled
		// AND the status is ambiguous (transport error or proxy-generated 502).
		// If the upstream returned a definitive 5xx/429 before the client
		// disconnected, the penalty must apply — the upstream is genuinely
		// failing. See the comment block above for the full rationale.
		isTransportOrProxyError := recOK && (rec.status == 0 || rec.status == http.StatusBadGateway)
		// Single anchor for classification decisions in this defer.
		now := time.Now()
		if p.breaker != nil && recOK && reachedUpstream && !(isClientCancel && isTransportOrProxyError) && isUpstreamFailureStatus(rec, now) {
			penalty := p.breaker.PenaltyDuration()
			if penalty > 0 {
				time.AfterFunc(penalty, release)
				return
			}
			// Penalty is zero — fall through to check failureHold below.
		}
		if isClientCancel && reachedUpstream && p.cancelCooldown > 0 {
			// Client disconnected after the request reached upstream. Hold the
			// slot briefly to prevent N+1 observed concurrency from downstream
			// accounting lag (KILL-04 mitigation). Unlike phantom penalty and
			// failure hold, the cancelCooldown does NOT use the
			// isTransportOrProxyError guard — it MUST fire even when
			// rec.status == 0 (upstream still processing, WriteHeader not yet
			// called). When the upstream hasn't responded yet, the upstream is
			// still actively computing the abandoned request, so releasing the
			// slot immediately would allow an attacker to exhaust upstream
			// concurrency by rapidly opening and dropping connections. The
			// isTransportOrProxyError guard IS correct for phantom penalty and
			// failure hold (which punish upstream failures, not accounting lag),
			// but the cooldown exists specifically to cover the lag case.
			time.AfterFunc(p.cancelCooldown, release)
			return
		}
		if p.failureHold > 0 && recOK && reachedUpstream && !(isClientCancel && isTransportOrProxyError) && isUpstreamFailureStatus(rec, now) {
			// Standalone failure hold: holds the slot after an upstream failure.
			// This applies when the circuit breaker is disabled or when the
			// breaker penalty is zero. Uses the same isTransportOrProxyError
			// guard as the phantom penalty branch — client cancels with
			// ambiguous status codes are suppressed; definitive upstream
			// failures are not.
			time.AfterFunc(p.failureHold, release)
			return
		}
		release()
	}()

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

	// Capture the time the upstream request begins so we can pass it to
	// RecordFailure as startedAt. This enables stale-request protection:
	// if the circuit cycles through OPEN→HALF_OPEN while the request is
	// in flight, a failure from a request started before the OPEN period
	// is ignored and does not falsely trip HALF_OPEN→OPEN.
	proxyStart := time.Now()
	reachedUpstream = true

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
				log.Printf("proxy panic in serveLimited: %v", rv)
				if rec, ok := w.(*statusRecorder); ok && !rec.terminalWritten {
					http.Error(rec, "internal error", http.StatusBadGateway)
				}
				localPanic = true
			}
		}()
		p.inner.ServeHTTP(w, r)
	}()

	// Feed failure/success signals to the circuit breaker when the
	// retry transport is NOT handling it (i.e., breaker configured
	// without retries). When the retry transport IS active with a
	// breaker, it reports every attempt's outcome — the proxy must
	// not duplicate that or it would double-count failures.
	if p.breaker != nil && !p.retryHandlesBreaker && !localPanic {
		rec, recOK := w.(*statusRecorder)
		if recOK {
			// Client-initiated context cancellation (e.g., the user
			// closed their browser tab) is NOT an upstream failure —
			// do not feed it to the breaker. An attacker could
			// otherwise trip the breaker by initiating and
			// immediately dropping connections. This mirrors the
			// isClientCancel guard in the retry transport. Note:
			// unlike the retry transport which checks the RoundTrip
			// error directly, the proxy uses r.Context().Err() because
			// httputil.ReverseProxy.ServeHTTP writes to the
			// ResponseWriter instead of returning a Go error.
			ctxErr := r.Context().Err()
			// Both Canceled (explicit disconnect) and DeadlineExceeded
			// (client-imposed timeout) are client-initiated. The retry
			// transport also checks both — since it never calls
			// context.WithTimeout, all DeadlineExceeded errors originate
			// from the client context or the proxy's queue timeout, not
			// from per-attempt deadlines controlled by the transport.
			isClientCancel := ctxErr != nil && (errors.Is(ctxErr, context.Canceled) || errors.Is(ctxErr, context.DeadlineExceeded))
			// Only skip recording for transport errors (status 0) and
			// proxy-generated 502 errors when the client cancelled. The
			// proxy's error handler writes 502 on context cancellation —
			// this is NOT an upstream failure. Any other 5xx came from
			// the upstream and MUST be reported regardless of client state.
			isTransportOrProxyError := rec.status == 0 || rec.status == http.StatusBadGateway
			// Evaluate classification and Retry-After under a single time
			// anchor so a slow response body cannot create a split-brain state
			// where the breaker sees a temporary ban but the penalty is zero.
			now := time.Now()
			if !(isClientCancel && isTransportOrProxyError) && isUpstreamFailureStatus(rec, now) {
				p.breaker.RecordFailure(rec.status, parseRetryAfterFromRecorder(rec, now), proxyStart, breakerEpoch)
			} else if rec.status >= 200 && rec.status < 300 {
				p.breaker.RecordSuccess(proxyStart, breakerEpoch)
			}
		}
	}

	p.m.IncProxied()
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

func isUpstreamFailureStatus(rec *statusRecorder, now time.Time) bool {
	return circuitbreaker.IsFailureStatusWithHeaders(rec.status, responseHeaders(rec), rec.responseAt, now)
}

func responseHeaders(rec *statusRecorder) http.Header {
	if rec.entry != nil && rec.entry.ResponseHeaders != nil {
		return rec.entry.ResponseHeaders
	}
	return rec.ResponseWriter.Header()
}

// parseRetryAfterFromRecorder extracts the remaining Retry-After duration
// from the statusRecorder's captured response headers. Returns 0 if absent or
// invalid. Supports both delay-seconds (integer) and HTTP-date formats per
// RFC 9110 §10.2.3. receivedAt is the header receipt time captured in rec.responseAt;
// evaluatedAt is the time of evaluation, passed in by the caller together with
// classification so both decisions share one anchor.
func parseRetryAfterFromRecorder(rec *statusRecorder, now time.Time) time.Duration {
	return circuitbreaker.ParseRetryAfter(responseHeaders(rec), rec.responseAt, now)
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
	terminalWritten bool      // true once a terminal status (>=200) has been recorded
	responseAt      time.Time // headers received; anchor for Retry-After evaluation
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
	// If WriteHeader was never called for a terminal status, the Go runtime
	// will trigger an implicit WriteHeader(StatusOK) inside
	// ResponseWriter.Write — which bypasses our override. 1xx informational
	// responses also call WriteHeader, so we must distinguish terminal
	// (>=200) from informational (<200) status codes. Capture
	// headers/status here on the first terminal write.
	if !r.terminalWritten {
		r.status = http.StatusOK
		now := time.Now()
		r.responseAt = now
		if r.entry != nil {
			r.entry.StatusCode = http.StatusOK
			r.entry.ResponseHeaders = r.ResponseWriter.Header().Clone()
			r.entry.Timing.ResponseHeaders = now
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
