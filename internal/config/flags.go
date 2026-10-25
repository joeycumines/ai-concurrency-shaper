// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY, without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package config

import (
	"flag"
	"io"
	"strings"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// flagMeta records where a flag is legal and whether it is a boolean, for the
// token walker: it needs to know both to recognize -name=value forms and to
// decide whether a flag consumes the next argument.
type flagMeta struct {
	scope  Scope
	isBool bool
	// isHelp marks the -h/-help pair so Parse can short-circuit a help
	// request before any validation runs.
	isHelp bool
}

// registrar owns a flag.FlagSet plus the shared metadata map. Every flag
// registration writes its metadata in the same call that registers it, so the
// walker's view of "what flags exist and how they parse" can never diverge
// from the per-section FlagSets.
type registrar struct {
	fs    *flag.FlagSet
	meta  map[string]flagMeta
	scope Scope
}

func (r *registrar) stringVar(p *string, name, value, usage string) {
	r.fs.StringVar(p, name, value, usage)
	r.meta[name] = flagMeta{scope: r.scope}
}

func (r *registrar) boolVar(p *bool, name string, value bool, usage string) {
	r.fs.BoolVar(p, name, value, usage)
	r.meta[name] = flagMeta{scope: r.scope, isBool: true}
}

func (r *registrar) intVar(p *int, name string, value int, usage string) {
	r.fs.IntVar(p, name, value, usage)
	r.meta[name] = flagMeta{scope: r.scope}
}

func (r *registrar) int64Var(p *int64, name string, value int64, usage string) {
	r.fs.Int64Var(p, name, value, usage)
	r.meta[name] = flagMeta{scope: r.scope}
}

func (r *registrar) durationVar(p *time.Duration, name string, value time.Duration, usage string) {
	r.fs.DurationVar(p, name, value, usage)
	r.meta[name] = flagMeta{scope: r.scope}
}

// stringListVar registers a repeatable string flag (flag.Var), the same shape as
// the legacy -limit handling in main.go.
func (r *registrar) stringListVar(p *[]string, name string, usage string) {
	r.fs.Var(&stringList{slice: p}, name, usage)
	r.meta[name] = flagMeta{scope: r.scope}
}

// stringList implements flag.Value for repeatable string flags.
type stringList struct{ slice *[]string }

func (s *stringList) String() string {
	if s == nil || s.slice == nil {
		return ""
	}
	return strings.Join(*s.slice, ", ")
}

func (s *stringList) Set(v string) error {
	*s.slice = append(*s.slice, v)
	return nil
}

// Provider flag defaults. They live here as named constants rather than
// inline literals so each default reads as a single named value at its
// registration site and cannot drift between flag sets.
const (
	defaultBind                   = ":8080"
	defaultConcurrency            = 4
	defaultQueueTimeout           = 30 * time.Second
	defaultRetryMax               = -1
	defaultRetryMaxBodyMB         = 5
	defaultRetryWaitMin           = 500 * time.Millisecond
	defaultRetryWaitMax           = 30 * time.Second
	defaultRetryMinDelay          = 1 * time.Second
	defaultRetrySkipOn429         = true
	defaultReleaseCooldown        = 200 * time.Millisecond
	defaultCancelCooldown         = 200 * time.Millisecond
	defaultFailureHold            = 2 * time.Second
	defaultAdaptiveHeadroom       = false
	defaultAdaptiveHeadroomWin    = 30 * time.Second
	defaultDisableKeepAlives      = false
	defaultCBEnabled              = true
	defaultCBThreshold            = 5
	defaultCBWindow               = 30 * time.Second
	defaultCBOpenTimeout          = 10 * time.Second
	defaultCBMaxOpen              = 120 * time.Second
	defaultCBPenalty              = 2 * time.Second
	defaultCBMaxPenalty           = 60 * time.Second
	defaultAnthropicVersionValue  = auth.DefaultAnthropicVersion
	defaultTranscodeMaxRequestMB  = 10
	defaultTranscodeMaxResponseMB = 10
)

// registerServerFlags registers the server/global-section flags (legacy
// -bind/-tui/-version) bounded to s.
func registerServerFlags(r *registrar, s *Server) {
	r.scope = scopeServer
	r.stringVar(&s.Bind, "bind", defaultBind, "listen address")
	r.boolVar(&s.TUI, "tui", false, "enable terminal dashboard")
	r.boolVar(&s.Version, "version", false, "print version and exit")
	r.stringVar(&s.MetricsBind, "metrics-bind", "", "dedicated listen address for the Prometheus /metrics endpoint (empty = disabled; server scope)")
	r.stringListVar(&s.ModelTable, "model-table", "global model table entry: surrogate@provider=wire[;facts] (repeatable; server scope)")
	r.stringListVar(&s.CatalogSuites, "catalog-suite", "repeatable catalog suite mount: [name@]prefix[=models][;options] (repeatable; server scope)")

	// Embedded RC server (server scope).
	r.stringVar(&s.RCBind, "rc-server-bind", "", "listen address for the embedded RemoteControl.NetConn gRPC server (empty = disabled)")
	r.boolVar(&s.RCTLS, "rc-server-tls", false, "enable TLS on the embedded RC server listener")
	r.stringVar(&s.RCCA, "rc-server-ca", "", "CA for client certificate verification on the embedded RC server, enforcing mTLS (file:PATH or base64:DATA)")
	r.stringVar(&s.RCCert, "rc-server-cert", "", "server certificate for the embedded RC server (file:PATH or base64:DATA; requires -rc-server-tls and -rc-server-key)")
	r.stringVar(&s.RCKey, "rc-server-key", "", "server private key for the embedded RC server (file:PATH or base64:DATA; requires -rc-server-tls and -rc-server-cert)")

	registerHelp(r, &s.Help)
}

// registerHelp registers -h and -help at the registrar's current scope. Both
// spellings bind the same target so either form prints usage and exits 0.
//
// At provider scope the target is the provider's own Help field, so a
// --provider section can request help exactly like the server section can.
func registerHelp(r *registrar, p *bool) {
	// Legacy mode registers the server and provider scopes into ONE FlagSet;
	// the second registration of -h/-help must reuse the first target instead
	// of panicking with "flag redefined". The server binding wins there,
	// which is fine: both print the same usage.
	if r.fs.Lookup("h") != nil {
		return
	}
	r.boolVar(p, "h", false, "print usage and exit")
	r.boolVar(p, "help", false, "print usage and exit")
	r.meta["h"] = flagMeta{scope: r.scope, isBool: true, isHelp: true}
	r.meta["help"] = flagMeta{scope: r.scope, isBool: true, isHelp: true}
}

// registerProviderFlags registers every provider-scoped flag (all upstream-behavior
// tuning plus -upstream/-prefix/-name) bounded to p. In legacy mode these
// same flags are server-scope and bind to the single implicit provider.
func registerProviderFlags(r *registrar, p *Provider) {
	r.scope = scopeProvider

	r.stringVar(&p.Upstream, "upstream", "", "upstream base URL (required)")
	r.stringVar(&p.Prefix, "prefix", "", "provider mount prefix, stripped before forwarding (required for multiple providers)")
	r.stringVar(&p.Name, "name", "", "provider display name (default: derived from upstream host)")
	registerHelp(r, &p.Help)

	// Route limiting.
	r.stringListVar(&p.Limits, "limit", "route pattern to limit, matched by trailing path segments (repeatable)")
	r.intVar(&p.Concurrency, "concurrency", defaultConcurrency, "max concurrent limited requests")
	r.intVar(&p.GlobalConcurrency, "global-concurrency", 0, "global concurrency limit (0 = disabled)")
	r.boolVar(&p.LimitAll, "limit-all", false, "limit all requests, not just matching routes")
	r.durationVar(&p.QueueTimeout, "queue-timeout", defaultQueueTimeout, "max time a request waits in the queue (0 = wait indefinitely)")
	r.intVar(&p.QueueDepth, "queue-depth", 0, "max requests waiting for a slot at once (0 = unbounded; exceeding fails fast with 429 + Retry-After)")
	r.durationVar(&p.QueueComments, "queue-comments", 0, "SSE queue-comment interval for streaming requests (0 = disabled; e.g. 15s commits the stream early and emits ': queue-wait' comments while queued)")

	// Retry.
	r.intVar(&p.RetryMax, "retry", defaultRetryMax, "max retries for limited requests (negative = unlimited)")
	r.int64Var(&p.RetryMaxBodyMB, "retry-max-body-mb", defaultRetryMaxBodyMB, "max request body size retained for retry/anatomy, in MiB")
	r.durationVar(&p.RetryWaitMin, "retry-wait-min", defaultRetryWaitMin, "minimum retry backoff")
	r.durationVar(&p.RetryWaitMax, "retry-wait-max", defaultRetryWaitMax, "maximum retry backoff")
	r.durationVar(&p.RetryMinDelay, "retry-min-delay", defaultRetryMinDelay, "minimum delay before retrying (0 = use backoff only)")
	r.boolVar(&p.RetrySkipOn429, "retry-skip-429", defaultRetrySkipOn429, "skip retrying 429 responses to prevent concurrency amplification")

	// Concurrency protection.
	r.durationVar(&p.ReleaseCooldown, "release-cooldown", defaultReleaseCooldown, "delay after slot release before re-admission; caps throughput at limit/cooldown req/s (0 = immediate)")
	r.durationVar(&p.CancelCooldown, "cancel-cooldown", defaultCancelCooldown, "hold slot after client cancel once an upstream attempt started (0 = immediate)")
	r.durationVar(&p.FailureHold, "failure-hold", defaultFailureHold, "hold slot after upstream failure even without circuit breaker (0 = disabled)")

	// Adaptive headroom.
	r.boolVar(&p.AdaptiveHeadroom, "adaptive-headroom", defaultAdaptiveHeadroom, "reduce effective concurrency by one slot after a 429, restoring after a quiet window")
	r.durationVar(&p.AdaptiveHeadroomWindow, "adaptive-headroom-window", defaultAdaptiveHeadroomWin, "duration to hold the one-slot 429 headroom")

	// Transport tuning.
	r.boolVar(&p.DisableKeepAlives, "upstream-disable-keep-alives", defaultDisableKeepAlives, "disable HTTP keep-alives to upstream")

	// Upstream TLS trust: the handshake the proxy performs itself (direct
	// dialing, or the RC tunnel without -rc-terminate-tls). Rejected under
	// -rc-terminate-tls, where only the -rc-target-* options apply, and on
	// plaintext http upstreams, where the transport never handshakes.
	r.stringVar(&p.UpstreamCA, "upstream-ca", "", "CA for verifying the upstream certificate on the proxy's own TLS handshake (file:PATH or base64:DATA; rejected with -rc-terminate-tls and on plaintext http upstreams)")
	r.boolVar(&p.UpstreamInsecure, "upstream-insecure", false, "skip upstream TLS certificate verification on the proxy's own handshake (rejected with -rc-terminate-tls and on plaintext http upstreams)")
	r.stringVar(&p.UpstreamServerName, "upstream-server-name", "", "TLS SNI override for the proxy's own upstream handshake (rejected with -rc-terminate-tls and on plaintext http upstreams)")

	// Upstream authentication.
	r.stringVar(&p.AuthMode, "auth-mode", "", "upstream auth mode: auto | none | bearer | x-api-key | api-key | header:NAME (default: auto-derived from the upstream host when -auth-source is set)")
	r.stringVar(&p.AuthSource, "auth-source", "", "upstream credential source: env:VAR | file:PATH | none (empty disables upstream auth entirely; requests are forwarded verbatim)")
	r.stringVar(&p.AuthHeader, "auth-header", "", "custom upstream auth header name (required by -auth-mode header:<NAME>)")
	r.stringVar(&p.AnthropicVersion, "anthropic-version", defaultAnthropicVersionValue, "anthropic-version header value applied when the auth mode resolves to x-api-key")

	// Transcoding (provider scope).
	r.stringListVar(&p.TranscodeRoutes, "transcode-route", "repeatable route mapping: clientProtocol@clientPath=upstreamProtocol@upstreamPath")
	r.boolVar(&p.TranscodeResponsesChat, "transcode-responses-chat", false, "preset: map POST /v1/responses to upstream /v1/chat/completions")
	r.boolVar(&p.TranscodeMessagesChat, "transcode-messages-chat", false, "preset: map POST /v1/messages to upstream /v1/chat/completions")
	r.boolVar(&p.TranscodeMessagesResponses, "transcode-messages-responses", false, "preset: map POST /v1/messages to upstream /v1/responses")
	r.stringListVar(&p.NativeRouteFlags, "native-route", "repeatable natively served route: protocol@path with protocol responses|messages|chat (model identifier rewritten, body otherwise forwarded verbatim)")
	r.boolVar(&p.Opencode, "opencode", false, "emit the first-party opencode request shape upstream (User-Agent, session affinity, client attribution)")
	r.stringVar(&p.OpencodeUserAgent, "opencode-user-agent", transcode.DefaultOpencodeUserAgent, "User-Agent the -opencode preset asserts upstream (the impersonation value)")
	r.stringVar(&p.OpencodeClient, "opencode-client", transcode.DefaultOpencodeClient, "x-opencode-client the -opencode preset asserts upstream")
	r.boolVar(&p.TranscodeStrictDefaults, "transcode-strict-defaults", false, "disable default loss approvals, capabilities, and query forwarding")
	r.stringListVar(&p.TranscodeAllowLosses, "transcode-allow-loss", "approved non-portable feature or !name to deny (repeatable)")
	r.stringListVar(&p.TranscodeChatCapabilities, "transcode-chat-capability", "enable chat upstream capability or !name to deny (repeatable)")
	r.stringListVar(&p.TranscodeAllowClientQuery, "transcode-allow-client-query", "forward client query parameter or !name to deny (repeatable)")
	r.stringListVar(&p.TranscodeModelMap, "transcode-model", "map client model to upstream model: client=upstream (repeatable)")
	r.stringListVar(&p.TranscodeProfiles, "transcode-profile", "map profile name to upstream model and reasoning tier: name=model:tier (repeatable; tier optional, omit for model-only mapping)")
	r.int64Var(&p.TranscodeMaxRequestMB, "transcode-max-request-mb", defaultTranscodeMaxRequestMB, "max request body size retained for transcoding, in MiB")
	r.int64Var(&p.TranscodeMaxResponseMB, "transcode-max-response-mb", defaultTranscodeMaxResponseMB, "max response body size retained for transcoding, in MiB")
	r.stringVar(&p.TranscodeFlowLogDir, "transcode-flowlog-dir", "", "existing directory that receives one JSON record per transcoded exchange, capturing the full flow (client request, converted upstream request, upstream response, downstream response) unredacted; empty disables the recorder")
	r.boolVar(&p.TranscodeContinuity, "transcode-continuity", false, "opt-in per-conversation continuity store: retain the canonical conversation behind each emitted Responses id so a follow-up previous_response_id reconstructs against a stateless chat upstream (off by default; a miss degrades to the existing observable loss)")
	r.intVar(&p.TranscodeContinuityCap, "transcode-continuity-capacity", 0, "max retained conversation chains for the continuity store (0 = default 1024; negative rejected)")
	r.durationVar(&p.TranscodeContinuityTTL, "transcode-continuity-ttl", 0, "max age of a retained conversation chain (0 = default 30m; negative rejected)")
	r.stringVar(&p.TranscodeAuth, "transcode-auth", "", "upstream auth mode for transcode: auto | none | bearer | x-api-key | api-key | header (unset inherits the provider auth policy)")
	r.stringVar(&p.TranscodeAuthSource, "transcode-auth-source", "", "upstream auth secret source for transcode: inbound | env:VAR | file:PATH | provider")
	r.stringVar(&p.TranscodeAuthHeader, "transcode-auth-header", "", "custom upstream auth header name for transcode")
	r.stringVar(&p.TranscodeAnthropicVersion, "transcode-anthropic-version", defaultAnthropicVersionValue, "anthropic-version header value for transcode x-api-key")

	// Upstream transport and Sesame endpoint options.
	r.stringVar(&p.Transport, "transport", "", "upstream transport mode: direct | sesame (default: direct unless an RC dial target, -rc-server or -sesame-endpoint, is configured)")
	r.stringVar(&p.SesameEndpoint, "sesame-endpoint", "", "sesame endpoint to dial through via RemoteControl (host:port or http(s)://host:port)")
	r.stringVar(&p.SesameTokenEnv, "sesame-token-env", "", "environment variable name providing the sesame endpoint bearer token (requires a TLS sesame channel)")
	r.stringVar(&p.SesameCACert, "sesame-ca-cert", "", "CA certificate for sesame endpoint mTLS verification (file:PATH, base64:DATA, or a bare path)")
	r.stringVar(&p.SesameClientCert, "sesame-client-cert", "", "client certificate for sesame endpoint mTLS (file:PATH, base64:DATA, or a bare path)")
	r.stringVar(&p.SesameClientKey, "sesame-client-key", "", "client private key for sesame endpoint mTLS (file:PATH, base64:DATA, or a bare path)")
	r.stringVar(&p.SesameServerName, "sesame-server-name", "", "TLS SNI override for sesame endpoint channel")

	// RemoteControl.NetConn dial target.
	r.stringVar(&p.RCServer, "rc-server", "", "RemoteControl.NetConn gRPC server address (host:port) this provider's upstream connections dial through (empty = direct dialing)")
	r.boolVar(&p.RCChannelTLS, "rc-channel-tls", false, "enable TLS on the gRPC channel to the RC server")
	r.stringVar(&p.RCChannelCert, "rc-channel-cert", "", "client certificate for mTLS on the gRPC channel to the RC server (file:PATH or base64:DATA; requires -rc-channel-tls and -rc-channel-key)")
	r.stringVar(&p.RCChannelKey, "rc-channel-key", "", "client private key for mTLS on the gRPC channel to the RC server (file:PATH or base64:DATA; requires -rc-channel-tls and -rc-channel-cert)")
	r.stringVar(&p.RCChannelCA, "rc-channel-ca", "", "CA for verifying the RC server certificate on the gRPC channel (file:PATH or base64:DATA; requires -rc-channel-tls)")
	r.stringVar(&p.RCChannelServerName, "rc-channel-server-name", "", "TLS SNI override for the gRPC channel to the RC server (requires -rc-channel-tls)")
	r.boolVar(&p.RCChannelInsecure, "rc-channel-insecure", false, "skip TLS certificate verification on the gRPC channel to the RC server (requires -rc-channel-tls)")
	r.durationVar(&p.RCDialTimeout, "rc-dial-timeout", 0, "timeout for the remote dial through the RC server (0 = no timeout)")
	r.boolVar(&p.RCTerminateTLS, "rc-terminate-tls", false, "terminate upstream TLS at the RC server instead of inside the proxy (enables the -rc-target-* options; rejected on a plaintext http upstream, which has no handshake to terminate)")
	r.stringVar(&p.RCTargetServerName, "rc-target-server-name", "", "TLS SNI for the upstream handshake terminated at the RC server (requires -rc-terminate-tls)")
	r.stringListVar(&p.RCTargetALPN, "rc-target-alpn", "ALPN protocol offered during the upstream TLS handshake terminated at the RC server (repeatable; requires -rc-terminate-tls; h2 is refused because the terminated connection carries HTTP/1.1 only)")
	r.stringVar(&p.RCTargetMinVersion, "rc-target-min-version", "", "minimum upstream TLS version: 1.0, 1.1, 1.2, or 1.3 (requires -rc-terminate-tls)")
	r.stringVar(&p.RCTargetMaxVersion, "rc-target-max-version", "", "maximum upstream TLS version: 1.0, 1.1, 1.2, or 1.3 (requires -rc-terminate-tls)")
	r.stringVar(&p.RCTargetClientHello, "rc-target-client-hello", "", "decomposed wire-level ClientHelloSpec for the upstream handshake as protojson (file:PATH, base64:DATA, or inline JSON; requires -rc-terminate-tls; empty = engine default; the server fails closed on any dimension it cannot honour exactly)")
	r.boolVar(&p.RCTargetInsecure, "rc-target-insecure", false, "skip upstream TLS certificate verification for the handshake terminated at the RC server (requires -rc-terminate-tls)")
	r.stringVar(&p.RCTargetCA, "rc-target-ca", "", "CA for verifying the upstream certificate during the handshake terminated at the RC server (file:PATH or base64:DATA; requires -rc-terminate-tls)")
	r.stringVar(&p.RCTargetCert, "rc-target-cert", "", "client certificate for upstream mTLS terminated at the RC server (file:PATH or base64:DATA; requires -rc-terminate-tls and -rc-target-key)")
	r.stringVar(&p.RCTargetKey, "rc-target-key", "", "client private key for upstream mTLS terminated at the RC server (file:PATH or base64:DATA; requires -rc-terminate-tls and -rc-target-cert)")
	r.stringListVar(&p.RCProxyHop, "rc-proxy-hop", "egress proxy hop ahead of the upstream dial: http-connect|socks5@host:port[;user=U][;pass=P][;header=H] (repeatable)")
	r.boolVar(&p.RCFlowControl, "rc-flow-control", false, "advertise flow-control support on the RC stream (switches to the in-stream control path)")
	r.boolVar(&p.RCOpportunisticTLS, "rc-opportunistic-tls", false, "advertise opportunistic TLS upgrade support on the RC stream")
	r.intVar(&p.RCWindowSize, "rc-window-size", 0, "flow-control window size for the RC stream (0 = server default; requires -rc-flow-control)")

	// Circuit breaker.
	r.boolVar(&p.CBEnabled, "circuit-breaker", defaultCBEnabled, "enable the circuit breaker")
	r.intVar(&p.CBThreshold, "cb-threshold", defaultCBThreshold, "failures within window to trip circuit breaker")
	r.durationVar(&p.CBWindow, "cb-window", defaultCBWindow, "circuit breaker failure counting window")
	r.durationVar(&p.CBOpenTimeout, "cb-open-timeout", defaultCBOpenTimeout, "time before circuit breaker probes (half-open)")
	r.durationVar(&p.CBMaxOpen, "cb-max-open-timeout", defaultCBMaxOpen, "max circuit breaker open timeout after backoff")
	r.durationVar(&p.CBPenalty, "cb-penalty", defaultCBPenalty, "base phantom concurrency hold time")
	r.durationVar(&p.CBMaxPenalty, "cb-max-penalty", defaultCBMaxPenalty, "max phantom concurrency hold time")
}

// newFlagSet returns a FlagSet configured for our controlled error reporting:
// parsing failures are returned as errors, never written to stderr.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// flagMetadata returns the name → metadata map for every known flag.
// It is built once by registering against throwaway targets; the walker uses it
// to recognize flags and classify booleans.
//
// The provider scope registers first so that the server registration of
// -h/-help wins the shared map slot: help is legal at EVERY scope, and a
// server-scope entry keeps the sectioned parser's mixed-mode check from
// rejecting a bare -h at server scope.
func flagMetadata() map[string]flagMeta {
	meta := make(map[string]flagMeta)
	registerProviderFlags(&registrar{fs: newFlagSet("meta"), meta: meta}, &Provider{})
	registerServerFlags(&registrar{fs: newFlagSet("meta"), meta: meta}, &Server{})
	return meta
}
