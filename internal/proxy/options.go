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
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// Option configures a Proxy.
type Option interface {
	applyProxyOption(cfg *proxyConfig) error
}

type proxyConfig struct {
	upstream     *url.URL
	matcher      *route.Matcher
	limiter      *queue.Limiter
	metrics      *metrics.Collector
	queueTimeout time.Duration
	// queueDepthLimit bounds how many requests may WAIT for a slot at once
	// (0 = unbounded, the default). A limited request arriving when the
	// waiters count already reaches the bound fails fast with 429 +
	// Retry-After instead of queueing — the protocol signal AI clients and
	// the official SDKs already handle (prevents silent
	// blocking from amplifying into client retries).
	queueDepthLimit int
	// queueComments is the SSE queue-comment interval for streaming
	// requests (0 = disabled, the default). When enabled and the request's
	// Accept header selects text/event-stream, the proxy commits 200 +
	// SSE headers BEFORE admission and writes ": queue-wait elapsed=N"
	// comment lines every interval while the request waits for a slot —
	// keeping the client's connection visibly alive instead of silently
	// blocking (deterministically
	// testable with the locally-installed Claude Code, since the upstream
	// is only contacted after admission).
	queueComments          time.Duration
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
	limitAll               bool
	transcodeMappings      []TranscodeMapping
	nativeRoutes           []NativeRoute
	authPolicy             *auth.AuthPolicy
	modelCatalog           *transcode.CatalogConfig
	opencode               transcode.OpencodePreset
}

// TranscodeMapping configures one transcoded route. The embedded
// transcode.Mapping is validated by WithTranscodeMapping and proxy.New.
type TranscodeMapping struct {
	transcode.Mapping

	// BodyLimits bounds request/response bodies on this route. Zero values
	// fall back to the proxy defaults.
	BodyLimits transcode.BodyLimits

	// FlowLogDir, when non-empty, receives one JSON record per transcoded
	// exchange capturing the full flow. The directory must already exist;
	// New validates it before any handler is constructed. Records are
	// unredacted (they include headers and bodies), so the directory is
	// secret-bearing.
	FlowLogDir string

	// Continuity, when non-nil, is the opt-in bounded per-conversation
	// store (OFF by default: nil keeps the existing observable
	// previous_response_id loss). It is shared by every transcoded route
	// of one provider; ContinuityKey scopes retained ids to the provider
	// mapping that emitted them. The store pointer is shared, never
	// cloned: chains accumulate across exchanges by design.
	Continuity    *transcode.ContinuityStore
	ContinuityKey string
}

// TranscodeOption configures transcoding route mappings.
type TranscodeOption struct {
	mappings []TranscodeMapping
}

// WithTranscodeMapping returns an option that registers transcoding route
// mappings. Unsupported format pairs are rejected.
func WithTranscodeMapping(mappings ...TranscodeMapping) *TranscodeOption {
	return &TranscodeOption{mappings: mappings}
}

func (o *TranscodeOption) applyProxyOption(cfg *proxyConfig) error {
	for i := range o.mappings {
		m := &o.mappings[i]
		if err := m.Validate(); err != nil {
			return fmt.Errorf("proxy: invalid transcode mapping: %w", err)
		}
	}
	cfg.transcodeMappings = append(cfg.transcodeMappings, o.mappings...)
	return nil
}

var _ Option = (*TranscodeOption)(nil)

// OpencodePresetOption enables first-party opencode header emission on
// the transparent engine. The zero preset is disabled and changes nothing.
type OpencodePresetOption struct {
	preset transcode.OpencodePreset
}

// WithOpencodePreset returns an option that enables first-party header
// emission with the given preset.
func WithOpencodePreset(preset transcode.OpencodePreset) *OpencodePresetOption {
	return &OpencodePresetOption{preset: preset}
}

func (o *OpencodePresetOption) applyProxyOption(cfg *proxyConfig) error {
	cfg.opencode = o.preset
	return nil
}

var _ Option = (*OpencodePresetOption)(nil)

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
	if o.value.Scheme != "http" && o.value.Scheme != "https" {
		return errors.New("proxy: upstream URL scheme must be http or https")
	}
	// Hostname() (not Host) is checked: "http://:8080" has a non-empty Host
	// but no hostname, and would fail per-request instead of at startup.
	if o.value.Hostname() == "" {
		return errors.New("proxy: upstream URL must include a hostname")
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

// WithQueueDepthLimit returns an option that bounds how many requests may
// wait for a slot at once. 0 (the default) is unbounded. A limited request
// arriving when the waiters count already reaches the bound fails fast with
// 429 Too Many Requests + Retry-After instead of queueing: the protocol
// signal AI clients and the official SDKs already handle, preventing silent
// blocking from amplifying into client-side retries.
func WithQueueDepthLimit(n int) *QueueDepthLimitOption {
	return &QueueDepthLimitOption{value: n}
}

// QueueDepthLimitOption sets WithQueueDepthLimit.
type QueueDepthLimitOption struct{ value int }

func (o *QueueDepthLimitOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value < 0 {
		return fmt.Errorf("proxy: queue depth limit must be >= 0, got %d", o.value)
	}
	cfg.queueDepthLimit = o.value
	return nil
}

// WithQueueComments returns an option that enables SSE queue comments for
// streaming requests: when the interval is positive and the request's Accept
// header selects text/event-stream, the proxy commits 200 + SSE headers
// BEFORE admission and writes ": queue-wait elapsed=N" comment lines every
// interval while the request waits for a slot. 0 (the default) disables the
// mode. Opt-in: once comments are committed the exchange is locked to the
// streaming representation — later failures surface as stream failures, not
// HTTP error statuses (the documented trade-off of the mode).
func WithQueueComments(interval time.Duration) *QueueCommentsOption {
	return &QueueCommentsOption{value: interval}
}

// QueueCommentsOption sets WithQueueComments.
type QueueCommentsOption struct{ value time.Duration }

func (o *QueueCommentsOption) applyProxyOption(cfg *proxyConfig) error {
	if o.value < 0 {
		return fmt.Errorf("proxy: queue comment interval must be >= 0, got %v", o.value)
	}
	cfg.queueComments = o.value
	return nil
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
//
// If t is, or transparently wraps, a *retry.Transport with a non-nil breaker,
// it must expose that transport directly, via Unwrap() http.RoundTripper, or
// via RetryBreaker/SetInFlightRetries methods compatible with *retry.Transport.
// Those methods are a trusted wrapper SPI: by exposing them, a wrapper promises
// to honor retry.WithDeferredBreakerSuccess and retry.WithBreakerAttempt exactly
// like retry.Transport. The constructor can validate only visible transports and
// trusted SPI methods. An opaque wrapper that hides a breaker-bearing retry
// transport is treated as an ordinary transport; that shape is unsupported and
// may split breaker accounting because there is no safe way to detect it.
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
// the retry arrives. Zero means no floor.
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

// CancelCooldownOption sets a brief slot hold after a client disconnect once
// an upstream transport attempt has started.
type CancelCooldownOption struct {
	value time.Duration
}

// WithCancelCooldown returns an option that sets the client-cancel cooldown.
// When a client disconnects after an upstream transport attempt has started,
// the slot is held for this duration before re-admission.
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
// an upstream failure (5xx, 429). This hold applies only when the circuit
// breaker is disabled: an enabled breaker's phantom penalty, which is always
// at least its positive base penalty, is the failure-path hold instead. The
// hold is released asynchronously so the HTTP handler returns immediately.
// Zero means no hold (disabled).
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

// LimitAllOption makes the default limiter bound every request, not just the
// requests that match a limited route.
type LimitAllOption struct {
	value bool
}

// WithLimitAll returns an option that, when enabled, routes every request
// through the default concurrency limiter (the same one set by WithLimiter).
// Requests that would otherwise pass through are treated as limited: they
// acquire a slot from the default limiter, count as proxied, and are subject to
// the queue timeout. Off by default.
func WithLimitAll(enabled bool) *LimitAllOption {
	return &LimitAllOption{value: enabled}
}

func (o *LimitAllOption) applyProxyOption(cfg *proxyConfig) error {
	cfg.limitAll = o.value
	return nil
}

// AuthPolicyOption attaches an upstream authentication policy.
type AuthPolicyOption struct {
	value *auth.AuthPolicy
}

// WithAuthPolicy returns an option that sets the upstream authentication
// policy. When set, every proxied request has client credential and protocol
// headers stripped and the policy's upstream credential attached inside the
// Rewrite hook; when nil (the default) requests are forwarded verbatim with no
// mutation at all.
func WithAuthPolicy(policy *auth.AuthPolicy) *AuthPolicyOption {
	return &AuthPolicyOption{value: policy}
}

func (o *AuthPolicyOption) applyProxyOption(cfg *proxyConfig) error {
	cfg.authPolicy = o.value
	return nil
}

var (
	_ Option = (*UpstreamOption)(nil)
	_ Option = (*MatcherOption)(nil)
	_ Option = (*LimiterOption)(nil)
	_ Option = (*MetricsOption)(nil)
	_ Option = (*QueueTimeoutOption)(nil)
	_ Option = (*QueueDepthLimitOption)(nil)
	_ Option = (*QueueCommentsOption)(nil)
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
	_ Option = (*LimitAllOption)(nil)
	_ Option = (*AuthPolicyOption)(nil)
)
