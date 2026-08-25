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

// Package auth implements per-provider upstream authentication: it strips
// every client-supplied HTTP credential and protocol header from a request
// before it crosses a provider boundary, then attaches exactly one configured
// upstream credential.
//
// The threat model is a multi-provider gateway: different upstreams speak
// different auth schemes (Anthropic x-api-key plus anthropic-version, OpenAI
// bearer, Azure api-key, arbitrary custom headers), and verbatim forwarding
// would leak one provider's credential to another. Strip-then-inject makes
// cross-provider leakage of those credentials structurally impossible
// whenever a policy is applied; with no policy the caller performs no
// mutation at all.
//
// One credential class is deliberately exempt: Cookie values are forwarded
// verbatim (stripping them would break cookie-authenticated upstreams) and
// are instead display-redacted by RedactSensitiveHeaders so they never reach
// journal entries or the TUI. See displayOnlyCredentialNames in redact.go.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// AuthMode is the configured target authentication mode.
type AuthMode string

// AuthMode values.
const (
	// AuthAuto asks for derivation from the upstream host. It must be
	// resolved to a concrete mode via ResolveMode before a policy is built;
	// Validate rejects it so an unresolved mode can never reach requests.
	AuthAuto AuthMode = "auto"
	// AuthNone strips every credential and protocol header (Cookie exempt;
	// see StripCredentials) without injecting anything: credential hygiene
	// without custody.
	AuthNone AuthMode = "none"
	// AuthBearer injects "Authorization: Bearer <secret>" (OpenAI-compatible).
	AuthBearer AuthMode = "bearer"
	// AuthXAPIKey injects "X-Api-Key: <secret>" plus "Anthropic-Version"
	// (Anthropic Messages).
	AuthXAPIKey AuthMode = "x-api-key"
	// AuthAPIKey injects "Api-Key: <secret>" (Azure key-based auth).
	AuthAPIKey AuthMode = "api-key"
	// AuthCustomHeader injects the secret under Policy.CustomHeader
	// (e.g. Gemini's X-Goog-Api-Key).
	AuthCustomHeader AuthMode = "header"
)

// AuthPolicy describes how an upstream request is authenticated. The zero
// value is not a valid policy; construct one with a concrete resolved Mode,
// and for every mode except none a non-nil Secret.
//
// A policy carries no mutable state after construction and is safe for
// concurrent use by multiple requests.
type AuthPolicy struct {
	// Mode is the resolved authentication mode. It must never hold AuthAuto:
	// resolve once at configuration time via ResolveMode.
	Mode AuthMode

	// Secret supplies the upstream credential. It is consulted on every
	// authenticated request; configurations that resolve their source once
	// at startup wrap the resolved value in a staticSecretSource.
	Secret SecretSource

	// CustomHeader names the injection target when Mode is header.
	CustomHeader string

	// AnthropicVersion is set as the Anthropic-Version header when Mode is
	// x-api-key.
	AnthropicVersion string
}

// Validate checks the policy is internally consistent and complete enough to
// authenticate requests. It runs before any socket binds so a misconfiguration
// fails at startup rather than as a per-request 5xx.
func (p *AuthPolicy) Validate() error {
	switch p.Mode {
	case AuthNone, AuthBearer, AuthXAPIKey, AuthAPIKey:
	case AuthCustomHeader:
		if p.CustomHeader == "" {
			return errors.New("custom auth header is empty")
		}
		if !validHeaderName(p.CustomHeader) {
			return fmt.Errorf("custom auth header %q is not a valid HTTP header name", p.CustomHeader)
		}
	case AuthAuto:
		return fmt.Errorf("auth mode %q must be resolved before use (see ResolveMode)", AuthAuto)
	default:
		return fmt.Errorf("unknown auth mode %q", p.Mode)
	}

	// Every mode except none needs a way to obtain the secret. A missing
	// source would otherwise pass startup and fail every request.
	if p.Mode != AuthNone && p.Secret == nil {
		return fmt.Errorf("auth mode %q requires a secret source", p.Mode)
	}

	if p.Mode == AuthXAPIKey && strings.TrimSpace(p.AnthropicVersion) == "" {
		return errors.New("x-api-key authentication requires an anthropic-version")
	}
	return nil
}

// ResolveMode resolves the configured mode against the upstream host: auto
// derives from documented host conventions (Anthropic hosts speak x-api-key,
// everything else defaults to bearer) and any other mode passes through.
func ResolveMode(mode AuthMode, host string) (AuthMode, error) {
	if mode != AuthAuto {
		return mode, nil
	}
	if host == "" {
		return "", errors.New(`auto auth requires an upstream host`)
	}
	return DeriveMode(host), nil
}

// DeriveMode returns the default auth mode for an upstream host:
// api.anthropic.com and *.anthropic.com speak x-api-key, everything else
// defaults to bearer. The rule is deliberately narrow and deterministic;
// providers outside it configure an explicit mode.
func DeriveMode(host string) AuthMode {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "api.anthropic.com" || strings.HasSuffix(h, ".anthropic.com") {
		return AuthXAPIKey
	}
	return AuthBearer
}

// validHeaderName reports whether name is a non-empty HTTP/1.1 header field
// name: every byte must be an RFC 7230 tchar. The strictness is a security
// requirement, not style: the name is injected into upstream requests, so
// spaces, colons, or control characters would smuggle extra headers or
// response-splitting payloads past this process.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// ApplyUpstreamAuthentication strips every client credential and protocol
// header from req (Cookie exempt — see StripCredentials) and attaches exactly
// the policy's upstream credential. It must run after the outbound URL and
// Host are final and before the request is sent - inside
// httputil.ReverseProxy's Rewrite hook, where stdlib has already removed
// hop-by-hop headers from the outbound clone, so headers set here cannot be
// stripped by a client Connection list.
//
// Apply never returns an error for validated policies whose Secret was
// resolved at startup: stripping is unconditional and injection only fails if
// the secret source itself fails at call time.
func ApplyUpstreamAuthentication(ctx context.Context, req *http.Request, policy *AuthPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}

	StripCredentials(req.Header)

	if policy.Mode == AuthNone {
		return nil
	}

	secret, err := resolveSecret(ctx, policy.Secret)
	if err != nil {
		return err
	}

	switch policy.Mode {
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+secret)
	case AuthXAPIKey:
		req.Header.Set("X-Api-Key", secret)
		req.Header.Set("Anthropic-Version", policy.AnthropicVersion)
	case AuthAPIKey:
		req.Header.Set("Api-Key", secret)
	case AuthCustomHeader:
		req.Header.Set(policy.CustomHeader, secret)
	default:
		// Unreachable: Validate rejects unknown modes above.
		return fmt.Errorf("unsupported auth mode %q", policy.Mode)
	}
	return nil
}

// resolveSecret obtains the upstream secret from the source and enforces that
// it is present and non-blank: an empty credential must fail loudly, never
// emit an empty Bearer scheme or blank header upstream.
func resolveSecret(ctx context.Context, source SecretSource) (string, error) {
	if source == nil {
		return "", errors.New("no configured upstream secret source")
	}
	secret, err := source.Secret(ctx)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return "", errors.New("configured upstream secret is empty")
	}
	return trimmed, nil
}
