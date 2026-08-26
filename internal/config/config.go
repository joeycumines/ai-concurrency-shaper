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

// Package config models the sectioned, provider-centric configuration of
// ai-concurrency-shaper.
//
// The concept model is Provider → Account → Access. A Provider is an upstream
// LLM system (identity + connection + upstream-behavior tuning). Accounts are a
// reserved, future layer for per-account limits under a provider-scoped shared
// ceiling. Access (the -prefix mount and route patterns) is an orthogonal
// concern: how a provider is exposed on the shared HTTP server.
//
// Stage 1 exercises exactly one provider per mount with a single anonymous
// account, but the model (and the parser's section registry below) keeps the
// three layers distinct.
package config

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// Scope identifies a configuration section type. Section scopes are the parser's
// registry of what a flag may do and where it may appear. scopeAccount exists
// so the concept is materialized and reserved, even though account sections are
// rejected before use.
type Scope int

const (
	scopeServer Scope = iota
	scopeProvider
	scopeAccount // reserved: not yet user-facing
)

func (s Scope) String() string {
	switch s {
	case scopeServer:
		return "server"
	case scopeProvider:
		return "provider"
	case scopeAccount:
		return "account"
	default:
		return fmt.Sprintf("scope(%d)", int(s))
	}
}

// Config is the root of the parsed configuration: server-wide settings plus one
// Provider per --provider section (or a single implicit provider in legacy mode).
//
// Config is safe to read after Parse and ResolveAndValidate complete; it is not
// safe to mutate concurrently.
type Config struct {
	Server    Server
	Providers []*Provider
}

// Server holds the server/global section settings (legacy -bind/-tui/-version).
type Server struct {
	Bind    string
	TUI     bool
	Version bool
	// MetricsBind is the dedicated listen address for the Prometheus
	// /metrics endpoint (-metrics-bind). Empty disables the endpoint.
	MetricsBind string
	// Help is set by -h/-help at server scope (or legacy top level): the
	// caller prints usage and exits 0 instead of running the proxy.
	Help bool
}

// Provider is one upstream LLM system plus its access mount and upstream-behavior
// tuning. All upstream-behavior knobs default at provider scope; a future
// Account layer inherits them with per-account overrides.
type Provider struct {
	// Name is the display/identity label. Empty derives from the upstream host
	// when multiple providers are configured. A single unnamed provider renders
	// as "⚡ shaper" in the TUI.
	Name string
	// Upstream is the base URL of the provider (scheme + host required).
	Upstream string
	// Prefix is the access mount path; "" = bare root. With more than one
	// provider every provider needs a distinct, non-overlapping prefix.
	Prefix string
	// Limits holds repeatable -limit route patterns for this provider.
	Limits []string
	// Help is set by -h/-help inside a --provider section: the caller prints
	// usage and exits 0 instead of running the proxy.
	Help bool

	// ---- Upstream-behavior tuning (provider scope) ----

	Concurrency            int
	GlobalConcurrency      int
	LimitAll               bool
	QueueTimeout           time.Duration
	RetryMax               int
	RetryMaxBodyMB         int64
	RetryWaitMin           time.Duration
	RetryWaitMax           time.Duration
	RetryMinDelay          time.Duration
	RetrySkipOn429         bool
	ReleaseCooldown        time.Duration
	CancelCooldown         time.Duration
	FailureHold            time.Duration
	AdaptiveHeadroom       bool
	AdaptiveHeadroomWindow time.Duration
	DisableKeepAlives      bool

	// ---- Upstream authentication (provider scope) ----

	// AuthSource names where the upstream credential comes from:
	// "env:VAR", "file:PATH", or the literal "none" (strip-only hygiene).
	// Empty disables upstream authentication entirely: requests are
	// forwarded verbatim, exactly as they always have been.
	AuthSource string
	// AuthMode selects how the credential is attached upstream:
	// "auto" (default when a source is set), "none", "bearer", "x-api-key",
	// "api-key", or "header:<NAME>". Auto derives from the upstream host.
	AuthMode string
	// AuthHeader names the injection target when AuthMode is header.
	AuthHeader string
	// AnthropicVersion is applied as the Anthropic-Version header when the
	// resolved mode is x-api-key.
	AnthropicVersion string

	// ---- Circuit breaker (provider scope) ----

	CBEnabled     bool
	CBThreshold   int
	CBWindow      time.Duration
	CBOpenTimeout time.Duration
	CBMaxOpen     time.Duration
	CBPenalty     time.Duration
	CBMaxPenalty  time.Duration

	// -- resolved state (populated by ResolveAndValidate) --

	upstream       *url.URL
	patterns       []route.Pattern
	matcher        *route.Matcher
	defaultLimiter *queue.Limiter
	globalLimiter  *queue.Limiter
	routeLimiters  map[string]*queue.Limiter
	breaker        *circuitbreaker.Breaker
	maxIdlePerHost int
	authPolicy     *auth.AuthPolicy
}

// UpstreamURL returns the parsed upstream URL.
func (p *Provider) UpstreamURL() *url.URL { return p.upstream }

// Patterns returns the provider's parsed route patterns.
func (p *Provider) Patterns() []route.Pattern {
	out := make([]route.Pattern, len(p.patterns))
	copy(out, p.patterns)
	return out
}

// Matcher returns the provider's route matcher.
func (p *Provider) Matcher() *route.Matcher { return p.matcher }

// DefaultLimiter returns the provider's default concurrency limiter.
func (p *Provider) DefaultLimiter() *queue.Limiter { return p.defaultLimiter }

// GlobalLimiter returns the provider's global limiter, or nil when disabled.
func (p *Provider) GlobalLimiter() *queue.Limiter { return p.globalLimiter }

// RouteLimiters returns the provider's per-route/group limiters.
func (p *Provider) RouteLimiters() map[string]*queue.Limiter { return p.routeLimiters }

// Breaker returns the provider's circuit breaker, or nil when disabled.
func (p *Provider) Breaker() *circuitbreaker.Breaker { return p.breaker }

// MaxIdleConnsPerHost returns the transport's per-host idle connection pool
// size derived from the provider's limiters.
func (p *Provider) MaxIdleConnsPerHost() int { return p.maxIdlePerHost }

// AuthPolicy returns the provider's resolved upstream authentication policy,
// or nil when no -auth-source/-auth-mode was configured: nil means requests
// are forwarded verbatim with no stripping or injection.
func (p *Provider) AuthPolicy() *auth.AuthPolicy { return p.authPolicy }

// Account is reserved for the future multi-account layer: per-account keys and
// limits cooperating under a provider-scoped shared ceiling. Declared as a type
// now so the concept is materialized, but stage 1 never populates it and the
// CLI rejects --account sections.
type Account struct {
	Name   string
	Config Provider // inherits provider defaults, overrides per-account (reserved)
}

// ResolveAndValidate normalizes the parsed configuration and constructs each
// provider's runtime objects: route limiters, the default/global limiters, the
// route matcher, the circuit breaker, and the transport's per-host idle
// connection count. It must run before any socket binds, so every failure here is
// reported before the server starts.
//
// Parse handles mode legality (legacy vs sectioned). This function owns the
// semantic rules:
//
//  1. No providers → error.
//  2. Per-provider -upstream required; URL must parse with an http(s) scheme.
//  3. Value validation: concurrency >= 1, cooldowns/durations >= 0 (the
//     queue limiter constructor panics on invalid input, and stdlib flag does not
//     reject negative durations). Every duration/size value that reaches the
//     limiter, the proxy, or the circuit breaker is rejected here first, so the
//     panicking constructors are unreachable with invalid input; breaker values
//     are only checked (strictly positive) when the breaker is enabled.
//  4. -limit patterns parse via route.Parse.
//  5. With >1 providers: every provider needs a non-empty -prefix; prefixes
//     normalize to trailing-slash-free paths and must not overlap; unnamed
//     providers get derived names that must be unique.
func (c *Config) ResolveAndValidate() error {
	if len(c.Providers) == 0 {
		return errors.New("no providers configured")
	}

	multi := len(c.Providers) > 1

	for i, p := range c.Providers {
		if err := p.validateBasic(i, multi); err != nil {
			return err
		}
	}

	if multi {
		if err := validateMulti(c.Providers); err != nil {
			return err
		}
	}

	for i, p := range c.Providers {
		if err := p.resolve(i, multi); err != nil {
			return err
		}
	}
	return nil
}

// validateBasic validates a single provider's identity/connection values and
// normalizes its prefix. It does not construct runtime objects. When multi is
// false (the single legacy provider) errors are not prefixed with a section
// label, keeping the error text byte-identical to the legacy binary.
func (p *Provider) validateBasic(index int, multi bool) error {
	// When multi is false (the single legacy provider) the context prefix is
	// empty so error text stays byte-identical to the legacy binary.
	ctx := func() string {
		if !multi {
			return ""
		}
		return fmt.Sprintf("provider section %d: ", index+1)
	}

	if p.Upstream == "" {
		return fmt.Errorf("%s-upstream is required", ctx())
	}
	u, err := url.Parse(p.Upstream)
	if err != nil {
		return fmt.Errorf("%sinvalid -upstream URL: %w", ctx(), err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("%s-upstream URL must include scheme (http or https)", ctx())
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s-upstream URL scheme must be http or https, got %q", ctx(), u.Scheme)
	}
	p.upstream = u

	if p.Concurrency < 1 {
		return fmt.Errorf("%s-concurrency must be >= 1, got %d", ctx(), p.Concurrency)
	}
	if p.GlobalConcurrency < 0 {
		return fmt.Errorf("%s-global-concurrency must be >= 0, got %d", ctx(), p.GlobalConcurrency)
	}
	if p.ReleaseCooldown < 0 {
		return fmt.Errorf("%s-release-cooldown must be >= 0, got %v", ctx(), p.ReleaseCooldown)
	}

	// Fail-closed backstops for every remaining value flag, in flag-definition
	// order (see registerProviderFlags). queue.NewLimiterWithCooldown panics on
	// invalid input and stdlib flag accepts negative durations, so each value is
	// rejected here before resolve() can reach any panicking constructor. The
	// messages mirror the downstream proxy/circuitbreaker validation that this
	// replaces as the first line of defense.
	if p.QueueTimeout < 0 {
		return fmt.Errorf("%s-queue-timeout must be >= 0, got %v", ctx(), p.QueueTimeout)
	}
	if p.RetryMaxBodyMB < 0 {
		return fmt.Errorf("%s-retry-max-body-mb must be >= 0, got %d", ctx(), p.RetryMaxBodyMB)
	}
	if p.RetryWaitMin < 0 {
		return fmt.Errorf("%s-retry-wait-min must be >= 0, got %v", ctx(), p.RetryWaitMin)
	}
	if p.RetryWaitMax < 0 {
		return fmt.Errorf("%s-retry-wait-max must be >= 0, got %v", ctx(), p.RetryWaitMax)
	}
	if p.RetryMinDelay < 0 {
		return fmt.Errorf("%s-retry-min-delay must be >= 0, got %v", ctx(), p.RetryMinDelay)
	}
	if p.CancelCooldown < 0 {
		return fmt.Errorf("%s-cancel-cooldown must be >= 0, got %v", ctx(), p.CancelCooldown)
	}
	if p.FailureHold < 0 {
		return fmt.Errorf("%s-failure-hold must be >= 0, got %v", ctx(), p.FailureHold)
	}
	if p.AdaptiveHeadroomWindow < 0 {
		return fmt.Errorf("%s-adaptive-headroom-window must be >= 0, got %v", ctx(), p.AdaptiveHeadroomWindow)
	}
	if p.CBEnabled {
		// The breaker rejects zero/negative values, but only when enabled —
		// the disabled-breaker zero state is legal.
		if p.CBThreshold < 1 {
			return fmt.Errorf("%s-cb-threshold must be > 0, got %d", ctx(), p.CBThreshold)
		}
		if p.CBWindow <= 0 {
			return fmt.Errorf("%s-cb-window must be > 0, got %v", ctx(), p.CBWindow)
		}
		if p.CBOpenTimeout <= 0 {
			return fmt.Errorf("%s-cb-open-timeout must be > 0, got %v", ctx(), p.CBOpenTimeout)
		}
		if p.CBMaxOpen <= 0 {
			return fmt.Errorf("%s-cb-max-open-timeout must be > 0, got %v", ctx(), p.CBMaxOpen)
		}
		if p.CBPenalty <= 0 {
			return fmt.Errorf("%s-cb-penalty must be > 0, got %v", ctx(), p.CBPenalty)
		}
		if p.CBMaxPenalty <= 0 {
			return fmt.Errorf("%s-cb-max-penalty must be > 0, got %v", ctx(), p.CBMaxPenalty)
		}
	}

	if err := p.validateAuth(); err != nil {
		return fmt.Errorf("%s%w", ctx(), err)
	}

	for _, s := range p.Limits {
		if _, err := route.Parse(s); err != nil {
			return fmt.Errorf("%sinvalid -limit %q: %w", ctx(), s, err)
		}
	}

	if err := normalizePrefix(p); err != nil {
		return fmt.Errorf("%s%w", ctx(), err)
	}
	return nil
}

// validateMulti enforces the multi-provider access rules: distinct non-empty,
// non-overlapping prefixes, and unique derived names.
func validateMulti(providers []*Provider) error {
	for i, p := range providers {
		if p.Prefix == "" {
			if p.Name == "" {
				return fmt.Errorf("provider section %d: requires -prefix when multiple providers are configured", i+1)
			}
			return fmt.Errorf("provider %q requires -prefix when multiple providers are configured", p.Name)
		}
	}

	// Derive names first so the overlap error below can name each provider
	// even when -name was not given (its derived label appears, not the raw
	// upstream host). Derived names must be unique; explicit names collide
	// with anything.
	seen := make(map[string]int, len(providers))
	for i, p := range providers {
		if p.Name == "" {
			p.Name = deriveName(p.upstream.Hostname())
		}
		if p.Name == "" {
			return fmt.Errorf("provider section %d: unable to derive a name from upstream %q", i+1, p.upstream.Host)
		}
		if prev, dup := seen[p.Name]; dup {
			return fmt.Errorf("duplicate provider name %q (providers %d and %d)", p.Name, prev+1, i+1)
		}
		seen[p.Name] = i
	}

	// Overlap: reject any pair where one prefix is a segment-wise prefix of
	// the other (they would alias each other on the shared dispatcher).
	for a := range providers {
		for b := a + 1; b < len(providers); b++ {
			if segmentsOverlap(providers[a].Prefix, providers[b].Prefix) {
				return fmt.Errorf(
					"provider prefixes overlap: %q (%s) and %q (%s)",
					providers[a].Prefix, nameLabel(providers[a]),
					providers[b].Prefix, nameLabel(providers[b]),
				)
			}
		}
	}
	return nil
}

// resolve constructs the provider's runtime objects. It runs after validateBasic
// and validateMulti, so the values are known-good.
func (p *Provider) resolve(index int, multi bool) error {
	ctx := func() string {
		if !multi {
			return ""
		}
		return fmt.Sprintf("provider section %d: ", index+1)
	}

	patterns, err := p.parsePatterns()
	if err != nil {
		return fmt.Errorf("%s%w", ctx(), err)
	}
	p.patterns = patterns
	p.matcher = route.NewMatcher(patterns)

	p.defaultLimiter = queue.NewLimiterWithCooldown(p.Concurrency, p.ReleaseCooldown)

	p.routeLimiters = routeLimiters(p.Limits, p.ReleaseCooldown)

	if p.GlobalConcurrency > 0 {
		p.globalLimiter = queue.NewLimiterWithCooldown(p.GlobalConcurrency, p.ReleaseCooldown)
	}

	if p.CBEnabled {
		p.breaker, err = circuitbreaker.New(
			circuitbreaker.WithFailureThreshold(p.CBThreshold),
			circuitbreaker.WithWindow(p.CBWindow),
			circuitbreaker.WithOpenTimeout(p.CBOpenTimeout),
			circuitbreaker.WithMaxOpenTimeout(p.CBMaxOpen),
			circuitbreaker.WithBasePenalty(p.CBPenalty),
			circuitbreaker.WithMaxPenalty(p.CBMaxPenalty),
		)
		if err != nil {
			return fmt.Errorf("%scircuit breaker config: %w", ctx(), err)
		}
	}

	if err := p.buildAuthPolicy(); err != nil {
		return fmt.Errorf("%s%w", ctx(), err)
	}

	p.maxIdlePerHost = proxy.MaxIdleConnsPerHost(p.GlobalConcurrency, p.Concurrency, patterns, p.routeLimiters, p.LimitAll)
	return nil
}

// parsePatterns parses -limit patterns, falling back to the built-in defaults
// when none were configured.
func (p *Provider) parsePatterns() ([]route.Pattern, error) {
	if len(p.Limits) == 0 {
		return route.DefaultPatterns(), nil
	}
	out := make([]route.Pattern, 0, len(p.Limits))
	for _, s := range p.Limits {
		pattern, err := route.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("invalid -limit %q: %w", s, err)
		}
		out = append(out, pattern)
	}
	return out, nil
}

// routeLimiters builds the per-route/per-group limiters exactly as the legacy
// wiring in main.go does: one limiter per distinct -limit pattern (or group),
// keyed by the pattern's group name when present and by its raw string otherwise.
// Patterns with Limit == 0 fall through to the default pool and get no limiter.
//
// validateBasic already parsed every entry of limitSpecs, so route.Parse cannot
// fail here. A group-conflict warning mirrors the legacy behavior (the first
// limiter wins).
func routeLimiters(limitSpecs []string, releaseCooldown time.Duration) map[string]*queue.Limiter {
	limiters := make(map[string]*queue.Limiter)

	for _, s := range limitSpecs {
		pattern, _ := route.Parse(s) // validated by validateBasic
		if pattern.Limit == 0 {
			continue // falls through to the default pool
		}
		key := pattern.Raw
		if pattern.Group != "" {
			key = pattern.Group
		}
		if existing, ok := limiters[key]; ok {
			// Group conflict warning: legacy behavior keeps the first limiter.
			if existing.Limit() != pattern.Limit {
				log.Printf("WARNING: route %q specifies group %q with limit %d, but group already has limit %d. Using %d.\n", s, pattern.Group, pattern.Limit, existing.Limit(), existing.Limit())
			}
			continue
		}
		limiters[key] = queue.NewLimiterWithCooldown(pattern.Limit, releaseCooldown)
	}
	return limiters
}

// normalizePrefix normalizes a mount prefix: "/" becomes "", a trailing slash is
// removed, and the leading slash requirement is enforced.
func normalizePrefix(p *Provider) error {
	if p.Prefix == "" || p.Prefix == "/" {
		p.Prefix = ""
		return nil
	}
	if !strings.HasPrefix(p.Prefix, "/") {
		return fmt.Errorf("-prefix must start with /, got %q", p.Prefix)
	}
	// Keep internal slashes; drop at most one trailing slash.
	p.Prefix = strings.TrimSuffix(p.Prefix, "/")
	return nil
}

// prefixSegments splits a normalized prefix into traversal-resolved segments for
// overlap comparison: empty and "." segments are dropped, and ".." pops the
// previous segment. This deliberately matches the router's literal (colon-free)
// mount segmentation — the prefix is a literal mount prefix, not a route pattern.
func prefixSegments(prefix string) []string {
	var segs []string
	for s := range strings.SplitSeq(prefix, "/") {
		if s == "" || s == "." {
			continue
		}
		if s == ".." {
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
			continue
		}
		segs = append(segs, s)
	}
	return segs
}

// segmentsOverlap reports whether the two normalized prefixes have a segment-wise
// prefix relationship (either is a prefix of the other), meaning they would route
// the same request.
func segmentsOverlap(a, b string) bool {
	as := prefixSegments(a)
	bs := prefixSegments(b)
	if len(as) > len(bs) {
		as, bs = bs, as
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// deriveName derives a provider display name from its upstream host:
// strip the port, strip a leading "api."/"api-", and take the first
// dot-segment. api.anthropic.com → anthropic. The host is the port-free,
// bracket-free form from url.URL.Hostname() — IPv6 literals such as
// "[::1]:9000" derive as "::1", without brackets or port.
func deriveName(host string) string {
	h := host
	h = strings.TrimPrefix(h, "api.")
	h = strings.TrimPrefix(h, "api-")
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	return h
}

func nameLabel(p *Provider) string {
	if p.Name != "" {
		return p.Name
	}
	if p.upstream != nil {
		return p.upstream.Host
	}
	return "(unnamed)"
}
