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

package config

import (
	"context"
	"fmt"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
)

// UnprotectedProviderCount returns how many configured providers have no
// upstream auth policy (AuthPolicy() == nil). It is meaningful only after
// ResolveAndValidate; multi-provider fleets use it to decide whether to emit
// the forwarded-verbatim startup notice.
func (c *Config) UnprotectedProviderCount() int {
	n := 0
	for _, p := range c.Providers {
		if p.AuthPolicy() == nil {
			n++
		}
	}
	return n
}

// validateAuth checks the raw -auth-* flag values against the documented
// grammar without touching the environment or filesystem (no side effects):
// every provider's grammar surfaces before any credential is resolved,
// because validateBasic runs for all providers before any resolve.
//
// Grammar:
//
//	-auth-source: "" (disabled) | none | env:VAR | file:PATH
//	-auth-mode:   "" (auto when a source is set) | auto | none | bearer |
//	              x-api-key | api-key | header:<NAME>
//
// Empty source and empty mode together mean authentication is disabled: the
// provider forwards requests verbatim with no stripping or injection.
func (p *Provider) validateAuth() error {
	sourceKind, sourceArg, err := parseAuthSource(p.AuthSource)
	if err != nil {
		return err
	}

	modeName, modeArg, err := splitAuthMode(p.AuthMode)
	if err != nil {
		return err
	}

	switch modeName {
	case "", "auto":
	case "none":
		if sourceKind == "env" || sourceKind == "file" {
			return fmt.Errorf("-auth-mode none does not use -auth-source")
		}
	case "bearer", "x-api-key", "api-key":
		if sourceKind == "" {
			return fmt.Errorf("-auth-mode %q requires -auth-source", p.AuthMode)
		}
	case "header":
		if sourceKind == "" {
			return fmt.Errorf("-auth-mode header requires -auth-source")
		}
		if _, err := resolveAuthHeader(p.AuthMode, modeArg, p.AuthHeader); err != nil {
			return err
		}
	default:
		return fmt.Errorf("-auth-mode %q must be auto|none|bearer|x-api-key|api-key|header:NAME", p.AuthMode)
	}

	switch sourceKind {
	case "":
	case "none":
		// Strip-only shorthand; only bare/auto/none modes make sense.
		if modeName != "" && modeName != "auto" && modeName != "none" {
			return fmt.Errorf("-auth-source none cannot be combined with -auth-mode %q", p.AuthMode)
		}
	case "env":
		if !validEnvVarName(sourceArg) {
			return fmt.Errorf("-auth-source %q: environment variable name must match [A-Za-z_][A-Za-z0-9_]*", p.AuthSource)
		}
	case "file":
		// Non-empty path guaranteed by parseAuthSource.
	}

	return nil
}

// buildAuthPolicy constructs the provider's runtime auth policy. It resolves
// the credential ONCE so a missing or empty secret fails here — before any
// socket binds — instead of failing every request later. The resolved value
// is wrapped in a static source; nothing about it is ever logged.
//
// Callers prefix returned errors with the section context.
func (p *Provider) buildAuthPolicy() error {
	p.authPolicy = nil

	if p.AuthSource == "" && p.AuthMode == "" {
		return nil
	}

	sourceKind, sourceArg, err := parseAuthSource(p.AuthSource)
	if err != nil {
		return err
	}

	modeName, modeArg, _ := splitAuthMode(p.AuthMode)

	if sourceKind == "none" || modeName == "none" {
		policy := &auth.AuthPolicy{Mode: auth.AuthNone}
		if err := policy.Validate(); err != nil {
			return err
		}
		p.authPolicy = policy
		return nil
	}

	// An unset mode with a configured source means auto: derive from the
	// upstream host. (validateAuth has already rejected combinations that
	// make no sense, so this is the only normalization needed here.)
	if modeName == "" {
		modeName = string(auth.AuthAuto)
	}

	resolved, err := auth.ResolveMode(auth.AuthMode(modeName), p.upstream.Hostname())
	if err != nil {
		return fmt.Errorf("-auth-mode %s: %w", p.AuthMode, err)
	}

	headerName := ""
	if modeName == "header" {
		var herr error
		headerName, herr = resolveAuthHeader(p.AuthMode, modeArg, p.AuthHeader)
		if herr != nil {
			return herr
		}
	}

	var source auth.SecretSource
	switch sourceKind {
	case "env":
		source = auth.NewEnvSecretSource(sourceArg)
	case "file":
		source = auth.NewFileSecretSource(sourceArg)
	default:
		return fmt.Errorf("-auth-mode %q requires -auth-source", p.AuthMode)
	}

	value, err := source.Secret(context.Background())
	if err != nil {
		return fmt.Errorf("-auth-source %s: %w", p.AuthSource, err)
	}

	policy := &auth.AuthPolicy{
		Mode:             resolved,
		Secret:           auth.NewStaticSecretSource(strings.TrimSpace(value)),
		CustomHeader:     headerName,
		AnthropicVersion: p.AnthropicVersion,
	}
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("%w (mode %q, header %q, anthropic-version %q)", err, resolved, headerName, p.AnthropicVersion)
	}
	p.authPolicy = policy
	return nil
}

// resolveAuthHeader merges the two spellings of the custom auth header name:
// "-auth-mode header:NAME" and "-auth-header NAME". Giving neither is an
// error; giving both is legal only when they agree.
func resolveAuthHeader(rawMode, modeArg, flagValue string) (string, error) {
	if modeArg != "" && flagValue != "" && modeArg != flagValue {
		return "", fmt.Errorf("-auth-header %q conflicts with -auth-mode %q", flagValue, rawMode)
	}
	if modeArg != "" {
		return modeArg, nil
	}
	if flagValue != "" {
		return flagValue, nil
	}
	return "", fmt.Errorf("-auth-mode header requires -auth-header NAME")
}

// parseAuthSource splits the raw -auth-source value into its kind and
// argument: "" (disabled), "none", "env:VAR", or "file:PATH".
func parseAuthSource(raw string) (kind, arg string, err error) {
	switch {
	case raw == "":
		return "", "", nil
	case raw == "none":
		return "none", "", nil
	}

	rest, ok := strings.CutPrefix(raw, "env:")
	if ok {
		if rest == "" {
			return "", "", fmt.Errorf("-auth-source %q: environment variable name must match [A-Za-z_][A-Za-z0-9_]*", raw)
		}
		return "env", rest, nil
	}

	rest, ok = strings.CutPrefix(raw, "file:")
	if ok {
		if rest == "" {
			return "", "", fmt.Errorf("-auth-source %q: file path must not be empty", raw)
		}
		return "file", rest, nil
	}

	return "", "", fmt.Errorf("-auth-source %q must be env:VAR, file:PATH, or none", raw)
}

// splitAuthMode splits the raw -auth-mode value into its name and argument
// ("header:NAME" yields "header","NAME"; anything else yields name,"").
func splitAuthMode(raw string) (name, arg string, err error) {
	if raw == "" {
		return "", "", nil
	}
	if rest, ok := strings.CutPrefix(raw, "header:"); ok {
		return "header", rest, nil
	}
	switch auth.AuthMode(raw) {
	case auth.AuthAuto, auth.AuthNone, auth.AuthBearer, auth.AuthXAPIKey, auth.AuthAPIKey, auth.AuthCustomHeader:
		return raw, "", nil
	default:
		return "", "", fmt.Errorf("-auth-mode %q must be auto|none|bearer|x-api-key|api-key|header:NAME", raw)
	}
}

// validEnvVarName reports whether s is a plausible environment variable name:
// letters, digits, underscores, not starting with a digit. It exists so a
// typo like "env:MY KEY" fails at startup with a precise message instead of
// silently resolving to an unset lookup.
func validEnvVarName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
