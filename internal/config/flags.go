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
	defaultBind                  = ":8080"
	defaultConcurrency           = 4
	defaultQueueTimeout          = 30 * time.Second
	defaultRetryMax              = -1
	defaultRetryMaxBodyMB        = 5
	defaultRetryWaitMin          = 500 * time.Millisecond
	defaultRetryWaitMax          = 30 * time.Second
	defaultRetryMinDelay         = 1 * time.Second
	defaultRetrySkipOn429        = true
	defaultReleaseCooldown       = 200 * time.Millisecond
	defaultCancelCooldown        = 200 * time.Millisecond
	defaultFailureHold           = 2 * time.Second
	defaultAdaptiveHeadroom      = false
	defaultAdaptiveHeadroomWin   = 30 * time.Second
	defaultDisableKeepAlives     = false
	defaultCBEnabled             = true
	defaultCBThreshold           = 5
	defaultCBWindow              = 30 * time.Second
	defaultCBOpenTimeout         = 10 * time.Second
	defaultCBMaxOpen             = 120 * time.Second
	defaultCBPenalty             = 2 * time.Second
	defaultCBMaxPenalty          = 60 * time.Second
	defaultAnthropicVersionValue = "2023-06-01"
)

// registerServerFlags registers the server/global-section flags (legacy
// -bind/-tui/-version) bounded to s.
func registerServerFlags(r *registrar, s *Server) {
	r.scope = scopeServer
	r.stringVar(&s.Bind, "bind", defaultBind, "listen address")
	r.boolVar(&s.TUI, "tui", false, "enable terminal dashboard")
	r.boolVar(&s.Version, "version", false, "print version and exit")
	r.stringVar(&s.MetricsBind, "metrics-bind", "", "dedicated listen address for the Prometheus /metrics endpoint (empty = disabled; server scope)")
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

	// Retry.
	r.intVar(&p.RetryMax, "retry", defaultRetryMax, "max retries for limited requests (negative = unlimited)")
	r.int64Var(&p.RetryMaxBodyMB, "retry-max-body-mb", defaultRetryMaxBodyMB, "max request body size retained for retry/anatomy, in MiB")
	r.durationVar(&p.RetryWaitMin, "retry-wait-min", defaultRetryWaitMin, "minimum retry backoff")
	r.durationVar(&p.RetryWaitMax, "retry-wait-max", defaultRetryWaitMax, "maximum retry backoff")
	r.durationVar(&p.RetryMinDelay, "retry-min-delay", defaultRetryMinDelay, "minimum delay before retrying (0 = use backoff only)")
	r.boolVar(&p.RetrySkipOn429, "retry-skip-429", defaultRetrySkipOn429, "skip retrying 429 responses to prevent concurrency amplification")

	// Concurrency protection.
	r.durationVar(&p.ReleaseCooldown, "release-cooldown", defaultReleaseCooldown, "delay after slot release before re-admission (0 = immediate)")
	r.durationVar(&p.CancelCooldown, "cancel-cooldown", defaultCancelCooldown, "hold slot after client cancel once an upstream attempt started (0 = immediate)")
	r.durationVar(&p.FailureHold, "failure-hold", defaultFailureHold, "hold slot after upstream failure even without circuit breaker (0 = disabled)")

	// Adaptive headroom.
	r.boolVar(&p.AdaptiveHeadroom, "adaptive-headroom", defaultAdaptiveHeadroom, "reduce effective concurrency by one slot after a 429, restoring after a quiet window")
	r.durationVar(&p.AdaptiveHeadroomWindow, "adaptive-headroom-window", defaultAdaptiveHeadroomWin, "duration to hold the one-slot 429 headroom")

	// Transport tuning.
	r.boolVar(&p.DisableKeepAlives, "upstream-disable-keep-alives", defaultDisableKeepAlives, "disable HTTP keep-alives to upstream")

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
	r.boolVar(&p.TranscodeStrictDefaults, "transcode-strict-defaults", false, "disable default loss approvals, capabilities, and query forwarding")
	r.stringListVar(&p.TranscodeAllowLosses, "transcode-allow-loss", "approved non-portable feature or !name to deny (repeatable)")
	r.stringListVar(&p.TranscodeChatCapabilities, "transcode-chat-capability", "enable chat upstream capability or !name to deny (repeatable)")
	r.stringListVar(&p.TranscodeAllowClientQuery, "transcode-allow-client-query", "forward client query parameter or !name to deny (repeatable)")
	r.stringListVar(&p.TranscodeModelMap, "transcode-model", "map client model to upstream model: client=upstream (repeatable)")
	r.int64Var(&p.TranscodeMaxRequestMB, "transcode-max-request-mb", 0, "max request body size retained for transcoding, in MiB")
	r.int64Var(&p.TranscodeMaxResponseMB, "transcode-max-response-mb", 0, "max response body size retained for transcoding, in MiB")
	r.stringVar(&p.TranscodeAuth, "transcode-auth", "", "upstream auth mode for transcode: auto | none | bearer | x-api-key | api-key | header")
	r.stringVar(&p.TranscodeAuthSource, "transcode-auth-source", "", "upstream auth secret source for transcode: inbound | env:VAR | file:PATH")
	r.stringVar(&p.TranscodeAuthHeader, "transcode-auth-header", "", "custom upstream auth header name for transcode")
	r.stringVar(&p.TranscodeAnthropicVersion, "transcode-anthropic-version", defaultAnthropicVersionValue, "anthropic-version header value for transcode x-api-key")

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
