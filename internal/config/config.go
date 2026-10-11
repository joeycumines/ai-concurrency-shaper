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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/url"
	"strings"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/rcclient"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
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

	// modelTable holds the parsed and validated -model-table entries, frozen
	// by resolveModelTable during ResolveAndValidate. Empty means the table is
	// not configured and every provider keeps its legacy model resolution.
	modelTable []modelTableEntry
	// modelTableByProvider holds, per effective provider name, the frozen
	// entries that name that provider in input order.
	modelTableByProvider map[string][]modelTableEntry
	// catalogSuites holds the validated -catalog-suite mounts.
	catalogSuites []CatalogSuiteConfig
}

// Server holds the server/global section settings (legacy -bind/-tui/-version).
type Server struct {
	Bind    string
	TUI     bool
	Version bool
	// MetricsBind is the dedicated listen address for the Prometheus
	// /metrics endpoint (-metrics-bind). Empty disables the endpoint.
	MetricsBind string
	// ModelTable holds the raw -model-table entries: each occurrence names one
	// global model identity as surrogate@provider=wire[;facts]. The table is a
	// single global namespace; ResolveAndValidate parses and freezes it.
	ModelTable []string
	// CatalogSuites holds the raw -catalog-suite entries mounting virtual catalog suites.
	CatalogSuites []string
	// Help is set by -h/-help at server scope (or legacy top level): the
	// caller prints usage and exits 0 instead of running the proxy.
	Help bool

	// ---- Embedded RC server (server scope) ----

	// RCBind is the listen address for the embedded RemoteControl.NetConn
	// gRPC server (-rc-server-bind). Empty disables the embedded server.
	RCBind string
	// RCTLS enables TLS on the embedded RC server listener.
	RCTLS bool
	// RCCA is the CA for client certificate verification on the embedded RC
	// server (file:|base64:). When set, mTLS is enforced.
	RCCA string
	// RCCert is the server certificate for the embedded RC server (file:|base64:).
	RCCert string
	// RCKey is the server private key for the embedded RC server (file:|base64:).
	RCKey string

	// rcServerConfig is the resolved embedded RC server configuration,
	// populated by ResolveAndValidate. Nil when -rc-server-bind is empty.
	rcServerConfig *rcclient.ServerConfig
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

	Concurrency       int
	GlobalConcurrency int
	LimitAll          bool
	QueueTimeout      time.Duration
	QueueDepth        int
	QueueComments     time.Duration
	RetryMax          int
	// RetryMaxBodyMB is the max request body size in MiB retained for replay by
	// the proxy retry transport. It propagates as the default RetryReplayBytes
	// for any transcoded route mapping where RetryReplayBytes is unset (zero).
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
	// Credentials from "file:PATH" are resolved once at startup and wrapped
	// as static secrets across both passthrough and transcoded routes; file
	// rotation requires a restart today.
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

	// ---- Transcoding (provider scope) ----

	TranscodeRoutes            []string
	TranscodeResponsesChat     bool
	TranscodeMessagesChat      bool
	TranscodeMessagesResponses bool
	// NativeRouteFlags holds the raw -native-route values; resolved into
	// NativeRoute declarations by resolveTranscode.
	NativeRouteFlags []string
	// Opencode enables first-party header emission for this provider.
	Opencode bool
	// OpencodeUserAgent and OpencodeClient override the pinned
	// first-party values when the client did not supply its own.
	OpencodeUserAgent         string
	OpencodeClient            string
	TranscodeStrictDefaults   bool
	TranscodeAllowLosses      []string
	TranscodeChatCapabilities []string
	TranscodeAllowClientQuery []string
	TranscodeModelMap         []string
	TranscodeProfiles         []string
	TranscodeMaxRequestMB     int64
	TranscodeFlowLogDir       string
	TranscodeMaxResponseMB    int64
	TranscodeContinuity       bool
	TranscodeContinuityCap    int
	TranscodeContinuityTTL    time.Duration
	TranscodeAuth             string
	TranscodeAuthSource       string
	TranscodeAuthHeader       string
	TranscodeAnthropicVersion string

	// ---- Circuit breaker (provider scope) ----

	CBEnabled     bool
	CBThreshold   int
	CBWindow      time.Duration
	CBOpenTimeout time.Duration
	CBMaxOpen     time.Duration
	CBPenalty     time.Duration
	CBMaxPenalty  time.Duration

	// ---- Upstream connection TLS trust (provider scope) ----
	//
	// These configure the TLS handshake the PROXY performs itself over the
	// connection it opens: direct dialing, and the RC tunnel when the RC
	// server only moves bytes (-rc-server without -rc-terminate-tls). They
	// are rejected under -rc-terminate-tls, where the handshake happens at
	// the RC server and only the -rc-target-* options apply.
	//
	// UpstreamCA is a CA for verifying the upstream certificate
	// (file:|base64:). UpstreamInsecure skips verification. UpstreamServerName
	// overrides the expected server name.
	UpstreamCA         string
	UpstreamInsecure   bool
	UpstreamServerName string

	// ---- RemoteControl.NetConn dial target (provider scope) ----

	// RCServer is the RC server address (host:port) for this provider's
	// upstream dialing (-rc-server). Empty disables RC dialing.
	RCServer string
	// RCChannelTLS enables TLS on the gRPC channel to the RC server.
	RCChannelTLS bool
	// RCChannelCert/RCChannelKey are the client certificate and key for
	// mTLS on the gRPC channel (file:|base64:).
	RCChannelCert string
	RCChannelKey  string
	// RCChannelCA is the CA for verifying the RC server's certificate (file:|base64:).
	RCChannelCA string
	// RCChannelServerName overrides the TLS SNI for the gRPC channel.
	RCChannelServerName string
	// RCChannelInsecure disables TLS certificate verification on the gRPC channel.
	RCChannelInsecure bool
	// RCDialTimeout bounds the remote dial operation.
	RCDialTimeout time.Duration
	// RCTerminateTLS requests TLS termination at the RC server. The
	// RCTarget* fields below configure that handshake and are rejected
	// unless RCTerminateTLS is set.
	RCTerminateTLS bool
	// RCTargetServerName is the SNI for the remote TLS handshake.
	RCTargetServerName string
	// RCTargetALPN is the ALPN protocol list for the remote TLS handshake.
	RCTargetALPN []string
	// RCTargetMinVersion/RCTargetMaxVersion constrain the remote TLS version.
	RCTargetMinVersion string
	RCTargetMaxVersion string
	// RCTargetClientHello is the decomposed wire-level ClientHelloSpec for
	// the upstream handshake, as protojson (file:PATH, base64:DATA, or
	// inline JSON). Empty leaves the engine default; the server fails
	// closed on any dimension it cannot honour exactly.
	RCTargetClientHello string
	// RCTargetInsecure disables certificate verification on the remote TLS handshake.
	RCTargetInsecure bool
	// RCTargetCA/RCTargetCert/RCTargetKey are the CA and client credentials
	// for the remote TLS handshake (file:|base64:).
	RCTargetCA   string
	RCTargetCert string
	RCTargetKey  string
	// RCProxyHop is the repeatable proxy hop specification for egress
	// chaining ahead of the target dial: TYPE@ADDRESS[;user=U][;pass=P][;header=H].
	RCProxyHop []string
	// RCFlowControl advertises flow-control support on the stream.
	RCFlowControl bool
	// RCOpportunisticTLS advertises opportunistic TLS upgrade support.
	RCOpportunisticTLS bool
	// RCWindowSize is the flow-control window size (0 = server default).
	// Requires RCFlowControl.
	RCWindowSize int

	// -- resolved state (populated by ResolveAndValidate) --

	upstream          *url.URL
	patterns          []route.Pattern
	matcher           *route.Matcher
	defaultLimiter    *queue.Limiter
	globalLimiter     *queue.Limiter
	routeLimiters     map[string]*queue.Limiter
	breaker           *circuitbreaker.Breaker
	maxIdlePerHost    int
	authPolicy        *auth.AuthPolicy
	transcodeMappings []proxy.TranscodeMapping
	nativeRoutes      []proxy.NativeRoute
	opencodePreset    transcode.OpencodePreset
	// rcDialer is the resolved RC dialer for this provider, or nil when RC
	// dialing is not configured.
	rcDialer *rcclient.Dialer
	// modelCatalog is this provider's frozen catalog snapshot, built from its
	// model-table subset and resolved mappings. Nil when no table names it.
	modelCatalog *transcode.CatalogConfig
	// continuityStore is the shared continuity store for this provider's
	// transcoded routes. Nil when continuity is not enabled.
	continuityStore *transcode.ContinuityStore
}

// TranscodeMappings returns the resolved transcode route mappings for this provider,
// deep-cloning internal maps to prevent caller mutations.
func (p *Provider) TranscodeMappings() []proxy.TranscodeMapping {
	out := make([]proxy.TranscodeMapping, len(p.transcodeMappings))
	for i, m := range p.transcodeMappings {
		out[i] = cloneTranscodeMapping(m)
	}
	return out
}

func cloneTranscodeMapping(m proxy.TranscodeMapping) proxy.TranscodeMapping {
	cloned := m
	if m.Mapping.ModelMap.Exact != nil {
		exact := make(map[string]transcode.ModelMapping, len(m.Mapping.ModelMap.Exact))
		maps.Copy(exact, m.Mapping.ModelMap.Exact)
		cloned.Mapping.ModelMap.Exact = exact
	}
	if m.Mapping.ProfileMap.Profiles != nil {
		profiles := make(map[string]transcode.ProfileMapping, len(m.Mapping.ProfileMap.Profiles))
		maps.Copy(profiles, m.Mapping.ProfileMap.Profiles)
		cloned.Mapping.ProfileMap.Profiles = profiles
	}
	if m.Mapping.LossPolicy.Allowed != nil {
		allowed := make(map[transcode.Feature]struct{}, len(m.Mapping.LossPolicy.Allowed))
		maps.Copy(allowed, m.Mapping.LossPolicy.Allowed)
		cloned.Mapping.LossPolicy.Allowed = allowed
	}
	if m.Mapping.AllowedClientQuery != nil {
		query := make(map[string]struct{}, len(m.Mapping.AllowedClientQuery))
		maps.Copy(query, m.Mapping.AllowedClientQuery)
		cloned.Mapping.AllowedClientQuery = query
	}
	return cloned
}

// NativeRoutes returns the resolved natively served routes for this
// provider, deep-cloning internal maps to prevent caller mutations.
func (p *Provider) NativeRoutes() []proxy.NativeRoute {
	out := make([]proxy.NativeRoute, len(p.nativeRoutes))
	for i, n := range p.nativeRoutes {
		cloned := n
		if n.ModelMap.Exact != nil {
			exact := make(map[string]transcode.ModelMapping, len(n.ModelMap.Exact))
			maps.Copy(exact, n.ModelMap.Exact)
			cloned.ModelMap.Exact = exact
		}
		out[i] = cloned
	}
	return out
}

// OpencodePreset returns the resolved first-party header preset for this
// provider. The zero preset is disabled.
func (p *Provider) OpencodePreset() transcode.OpencodePreset {
	return p.opencodePreset
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

// RouteLimiters returns a copy of the provider's per-route/group limiters,
// so caller mutation cannot change live provider behavior.
func (p *Provider) RouteLimiters() map[string]*queue.Limiter {
	if p.routeLimiters == nil {
		return nil
	}
	out := make(map[string]*queue.Limiter, len(p.routeLimiters))
	maps.Copy(out, p.routeLimiters)
	return out
}

// Breaker returns the provider's circuit breaker, or nil when disabled.
func (p *Provider) Breaker() *circuitbreaker.Breaker { return p.breaker }

// MaxIdleConnsPerHost returns the transport's per-host idle connection pool
// size derived from the provider's limiters.
func (p *Provider) MaxIdleConnsPerHost() int { return p.maxIdlePerHost }

// AuthPolicy returns the provider's resolved upstream authentication policy,
// or nil when no -auth-source/-auth-mode was configured: nil means requests
// are forwarded verbatim with no stripping or injection.
func (p *Provider) AuthPolicy() *auth.AuthPolicy { return p.authPolicy }

// RCDialer returns the provider's resolved RemoteControl.NetConn dialer, or
// nil when -rc-server was not configured: nil means upstream connections
// dial directly through the default transport.
func (p *Provider) RCDialer() *rcclient.Dialer { return p.rcDialer }

// UpstreamTLSConfig builds the TLS configuration for the proxy's own
// upstream handshake (direct dialing, or the RC tunnel where the RC server
// only moves bytes). It returns nil when no trust flag is set, keeping the
// previous behavior of verifying against the system roots. The CA reference
// is decoded at startup — once discarded during resolve validation, once
// rebuilt when the transport is constructed — so a CA that fails to parse
// as PEM is a startup error, never a per-request surprise.
func (p *Provider) UpstreamTLSConfig() (*tls.Config, error) {
	if p.UpstreamCA == "" && !p.UpstreamInsecure && p.UpstreamServerName == "" {
		return nil, nil
	}
	cfg := &tls.Config{
		ServerName:         p.UpstreamServerName,
		InsecureSkipVerify: p.UpstreamInsecure,
	}
	ca, err := rcclient.DecodeMaterial(p.UpstreamCA)
	if err != nil {
		return nil, fmt.Errorf("upstream CA: %w", err)
	}
	if len(ca) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("upstream CA: invalid PEM")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// RCServerConfig returns the resolved embedded RC server configuration, or
// nil when -rc-server-bind was not configured.
func (s *Server) RCServerConfig() *rcclient.ServerConfig { return s.rcServerConfig }

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
	if len(c.Providers) == 0 && len(c.Server.CatalogSuites) == 0 {
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

	if err := c.resolveModelTable(); err != nil {
		return err
	}

	if err := c.resolveCatalogSuites(); err != nil {
		return err
	}

	if err := c.Server.resolveRC(); err != nil {
		return err
	}

	for i, p := range c.Providers {
		if err := p.resolve(i, multi, c.modelTableByProvider[effectiveName(p)]); err != nil {
			return err
		}
	}
	return nil
}

// EffectiveName is the provider name the global model table references: the
// validated Name when set, otherwise the same host derivation validateMulti
// applies, so the single-provider legacy mode has a stable table key too.
func (p *Provider) EffectiveName() string {
	if p.Name != "" {
		return p.Name
	}
	if p.upstream == nil {
		return ""
	}
	return deriveName(p.upstream.Hostname())
}

func effectiveName(p *Provider) string {
	return p.EffectiveName()
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
	if p.QueueDepth < 0 {
		return fmt.Errorf("%s-queue-depth must be >= 0, got %d", ctx(), p.QueueDepth)
	}
	if p.QueueComments < 0 {
		return fmt.Errorf("%s-queue-comments must be >= 0, got %v", ctx(), p.QueueComments)
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

	// The local-handshake trust flags configure the handshake the proxy
	// performs itself. Under remote termination the proxy never handshakes,
	// so accepting them would silently ignore the operator's CA — refuse
	// here in validation, before any material is decoded, so the error names
	// the meaningless combination rather than a missing file.
	if p.RCTerminateTLS && (p.UpstreamCA != "" || p.UpstreamInsecure || p.UpstreamServerName != "") {
		return fmt.Errorf("%supstream TLS trust options (-upstream-ca, -upstream-insecure, -upstream-server-name) do not apply with -rc-terminate-tls; use the -rc-target-* options", ctx())
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
// and validateMulti, so the values are known-good. modelTable is this provider's
// frozen subset of the global -model-table (nil when the table is not configured).
func (p *Provider) resolve(index int, multi bool, modelTable []modelTableEntry) error {
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

	if _, err := p.UpstreamTLSConfig(); err != nil {
		return fmt.Errorf("%s%w", ctx(), err)
	}

	if err := p.resolveTranscode(modelTable); err != nil {
		return fmt.Errorf("%s%w", ctx(), err)
	}

	if err := p.resolveRC(); err != nil {
		return fmt.Errorf("%s%w", ctx(), err)
	}

	p.resolveModelCatalog(modelTable)

	p.maxIdlePerHost = proxy.MaxIdleConnsPerHost(p.GlobalConcurrency, p.Concurrency, patterns, p.routeLimiters, p.LimitAll)
	return nil
}

// resolveRC builds the provider's RemoteControl.NetConn dialer when
// -rc-server is configured. Validation is fail-closed: any RC dial option
// without -rc-server, or an invalid combination, is rejected here at
// startup. The underlying gRPC channel is lazy, so an unreachable RC server
// does not fail startup; dial errors surface at request time, matching the
// transport's existing semantics.
func (p *Provider) resolveRC() error {
	if p.RCServer == "" {
		if p.rcFlagSet() {
			return errors.New("rc dial options require -rc-server")
		}
		return nil
	}
	if p.RCDialTimeout < 0 {
		return errors.New("rc dial timeout must be >= 0")
	}
	channel := rcclient.ChannelConfig{
		Server:     p.RCServer,
		TLS:        p.RCChannelTLS,
		Cert:       p.RCChannelCert,
		Key:        p.RCChannelKey,
		CA:         p.RCChannelCA,
		ServerName: p.RCChannelServerName,
		Insecure:   p.RCChannelInsecure,
	}
	clientHello, err := rcclient.ParseClientHello(p.RCTargetClientHello)
	if err != nil {
		return err
	}
	dial := rcclient.DialConfig{
		Timeout:          p.RCDialTimeout,
		TerminateTLS:     p.RCTerminateTLS,
		TargetServerName: p.RCTargetServerName,
		ALPN:             p.RCTargetALPN,
		MinVersion:       p.RCTargetMinVersion,
		MaxVersion:       p.RCTargetMaxVersion,
		ClientHello:      clientHello,
		TargetInsecure:   p.RCTargetInsecure,
		TargetCA:         p.RCTargetCA,
		TargetCert:       p.RCTargetCert,
		TargetKey:        p.RCTargetKey,
		ProxyHops:        p.RCProxyHop,
		FlowControl:      p.RCFlowControl,
		OpportunisticTLS: p.RCOpportunisticTLS,
		WindowSize:       p.RCWindowSize,
	}
	dialer, err := rcclient.NewDialer(channel, dial)
	if err != nil {
		return err
	}
	// Close the dialer from any prior resolution so a repeated
	// ResolveAndValidate never orphans its gRPC channel.
	if p.rcDialer != nil {
		_ = p.rcDialer.Close()
	}
	p.rcDialer = dialer
	return nil
}

// rcFlagSet reports whether any RC dial option other than -rc-server itself
// was configured, so a stray option without -rc-server is rejected rather
// than silently ignored.
func (p *Provider) rcFlagSet() bool {
	return p.RCChannelTLS || p.RCChannelCert != "" || p.RCChannelKey != "" ||
		p.RCChannelCA != "" || p.RCChannelServerName != "" || p.RCChannelInsecure ||
		p.RCDialTimeout != 0 || p.RCTerminateTLS || p.RCTargetServerName != "" ||
		len(p.RCTargetALPN) != 0 || p.RCTargetMinVersion != "" || p.RCTargetMaxVersion != "" ||
		p.RCTargetClientHello != "" || p.RCTargetInsecure ||
		p.RCTargetCA != "" || p.RCTargetCert != "" || p.RCTargetKey != "" ||
		len(p.RCProxyHop) != 0 || p.RCFlowControl || p.RCOpportunisticTLS || p.RCWindowSize != 0
}

// resolveRC validates the embedded RC server configuration when
// -rc-server-bind is set. Without it, stray server options are rejected
// rather than ignored. No socket is bound here; run() owns the listener.
func (s *Server) resolveRC() error {
	if s.RCBind == "" {
		if s.RCTLS || s.RCCA != "" || s.RCCert != "" || s.RCKey != "" {
			return errors.New("rc-server TLS options require -rc-server-bind")
		}
		return nil
	}
	cfg := rcclient.ServerConfig{
		Bind: s.RCBind,
		TLS:  s.RCTLS,
		CA:   s.RCCA,
		Cert: s.RCCert,
		Key:  s.RCKey,
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.rcServerConfig = &cfg
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
		if pattern.Unlimited {
			continue // exempt class: no limiter, never acquires a slot
		}
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
