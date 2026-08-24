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
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"
)

// fileSpec is the JSON schema of the -config file: a providers array only.
// Server settings (bind, TUI, version, and the config path itself) stay on
// the command line so there is exactly one precedence rule to remember.
//
// Every string value may reference environment variables as ${VAR}; missing
// variables fail the load (fail-closed). Credential VALUES never belong in
// the file — auth_source carries an env:VAR reference instead.
type fileSpec struct {
	Providers []fileProvider `json:"providers"`
}

// fileProvider mirrors one provider's flags with snake_case names. Pointer
// fields distinguish "absent" (apply the CLI default) from an explicit value;
// this matters for defaults that are non-zero, such as retry_skip_429=true.
type fileProvider struct {
	Name                   string   `json:"name"`
	Upstream               string   `json:"upstream"`
	Prefix                 string   `json:"prefix"`
	Limits                 []string `json:"limits"`
	LimitAll               *bool    `json:"limit_all"`
	Concurrency            *int     `json:"concurrency"`
	GlobalConcurrency      *int     `json:"global_concurrency"`
	QueueTimeout           *string  `json:"queue_timeout"`
	RetryMax               *int     `json:"retry_max"`
	RetryMaxBodyMB         *int64   `json:"retry_max_body_mb"`
	RetryWaitMin           *string  `json:"retry_wait_min"`
	RetryWaitMax           *string  `json:"retry_wait_max"`
	RetryMinDelay          *string  `json:"retry_min_delay"`
	RetrySkipOn429         *bool    `json:"retry_skip_429"`
	ReleaseCooldown        *string  `json:"release_cooldown"`
	CancelCooldown         *string  `json:"cancel_cooldown"`
	FailureHold            *string  `json:"failure_hold"`
	AdaptiveHeadroom       *bool    `json:"adaptive_headroom"`
	AdaptiveHeadroomWindow *string  `json:"adaptive_headroom_window"`
	DisableKeepAlives      *bool    `json:"disable_keep_alives"`
	CBEnabled              *bool    `json:"circuit_breaker"`
	CBThreshold            *int     `json:"cb_threshold"`
	CBWindow               *string  `json:"cb_window"`
	CBOpenTimeout          *string  `json:"cb_open_timeout"`
	CBMaxOpenTimeout       *string  `json:"cb_max_open_timeout"`
	CBPenalty              *string  `json:"cb_penalty"`
	CBMaxPenalty           *string  `json:"cb_max_penalty"`
	AuthMode               string   `json:"auth_mode"`
	AuthSource             string   `json:"auth_source"`
	AuthHeader             string   `json:"auth_header"`
	AnthropicVersion       string   `json:"anthropic_version"`
}

// LoadFile reads a providers-only JSON configuration and merges it into c.
// The file's providers come FIRST in resolution order; CLI --provider
// sections are appended after them, and every entry — loaded or parsed —
// flows through the same ResolveAndValidate rules (prefix overlap, name
// uniqueness, value backstops, auth grammar).
//
// When the command line used legacy form (no --provider section, no top-level
// provider configuration), the implicit empty provider is replaced by the
// file's list; mixing -config with top-level provider flags is rejected as
// ambiguous.
func (c *Config) LoadFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("-config %s: %w", path, err)
	}

	var spec fileSpec
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return fmt.Errorf("-config %s: %w", path, err)
	}
	if dec.More() {
		return fmt.Errorf("-config %s: unexpected content after the JSON document", path)
	}
	if len(spec.Providers) == 0 {
		return fmt.Errorf("-config %s: providers array must not be empty", path)
	}

	loaded := make([]*Provider, 0, len(spec.Providers))
	for i, fp := range spec.Providers {
		p, err := fp.provider()
		if err != nil {
			return fmt.Errorf("-config %s: provider %d: %w", path, i+1, err)
		}
		loaded = append(loaded, p)
	}

	if len(c.Providers) == 1 && c.Providers[0].isUnconfigured() {
		c.Providers = loaded
		return nil
	}
	if !c.sectioned {
		return fmt.Errorf("-config cannot be combined with top-level provider flags; wrap the CLI provider in a --provider section")
	}
	c.Providers = append(loaded, c.Providers...)
	return nil
}

// isUnconfigured reports whether a legacy-form implicit provider carries no
// operator configuration: identical to a freshly defaulted provider. Any
// deviation — even a lone tuning flag such as -concurrency 8 — means the
// operator mixed forms deliberately, which LoadFile rejects instead of
// silently dropping their settings.
//
// The comparison runs against a throwaway registration of the provider flags
// parsed from zero arguments, so it automatically covers every future flag.
func (p *Provider) isUnconfigured() bool {
	ref := &Provider{}
	fs := newFlagSet("file-defaults")
	registerProviderFlags(&registrar{fs: fs, meta: map[string]flagMeta{}}, ref)
	return reflect.DeepEqual(ref, p)
}

// provider expands environment references, applies the shared CLI defaults
// for absent fields, and returns the configured Provider.
func (fp fileProvider) provider() (*Provider, error) {
	name, err := expandStrict(fp.Name)
	if err != nil {
		return nil, err
	}
	upstream, err := expandStrict(fp.Upstream)
	if err != nil {
		return nil, err
	}
	prefix, err := expandStrict(fp.Prefix)
	if err != nil {
		return nil, err
	}
	authSource, err := expandStrict(fp.AuthSource)
	if err != nil {
		return nil, err
	}
	authMode, err := expandStrict(fp.AuthMode)
	if err != nil {
		return nil, err
	}
	authHeader, err := expandStrict(fp.AuthHeader)
	if err != nil {
		return nil, err
	}
	anthropicVersion := defaultAnthropicVersionValue
	if fp.AnthropicVersion != "" {
		v, err := expandStrict(fp.AnthropicVersion)
		if err != nil {
			return nil, err
		}
		anthropicVersion = v
	}

	limits := make([]string, len(fp.Limits))
	for i, l := range fp.Limits {
		expanded, err := expandStrict(l)
		if err != nil {
			return nil, err
		}
		limits[i] = expanded
	}

	p := &Provider{
		Name:                   name,
		Upstream:               upstream,
		Prefix:                 prefix,
		Limits:                 limits,
		Concurrency:            defaultConcurrency,
		QueueTimeout:           defaultQueueTimeout,
		RetryMax:               defaultRetryMax,
		RetryMaxBodyMB:         defaultRetryMaxBodyMB,
		RetryWaitMin:           defaultRetryWaitMin,
		RetryWaitMax:           defaultRetryWaitMax,
		RetryMinDelay:          defaultRetryMinDelay,
		RetrySkipOn429:         defaultRetrySkipOn429,
		ReleaseCooldown:        defaultReleaseCooldown,
		CancelCooldown:         defaultCancelCooldown,
		FailureHold:            defaultFailureHold,
		AdaptiveHeadroomWindow: defaultAdaptiveHeadroomWin,
		CBEnabled:              defaultCBEnabled,
		CBThreshold:            defaultCBThreshold,
		CBWindow:               defaultCBWindow,
		CBOpenTimeout:          defaultCBOpenTimeout,
		CBMaxOpen:              defaultCBMaxOpen,
		CBPenalty:              defaultCBPenalty,
		CBMaxPenalty:           defaultCBMaxPenalty,
		AnthropicVersion:       anthropicVersion,
		AuthSource:             authSource,
		AuthMode:               authMode,
		AuthHeader:             authHeader,
	}

	if fp.LimitAll != nil {
		p.LimitAll = *fp.LimitAll
	}
	if fp.Concurrency != nil {
		p.Concurrency = *fp.Concurrency
	}
	if fp.GlobalConcurrency != nil {
		p.GlobalConcurrency = *fp.GlobalConcurrency
	}
	if fp.RetryMax != nil {
		p.RetryMax = *fp.RetryMax
	}
	if fp.RetryMaxBodyMB != nil {
		p.RetryMaxBodyMB = *fp.RetryMaxBodyMB
	}
	if fp.RetrySkipOn429 != nil {
		p.RetrySkipOn429 = *fp.RetrySkipOn429
	}
	if fp.AdaptiveHeadroom != nil {
		p.AdaptiveHeadroom = *fp.AdaptiveHeadroom
	}
	if fp.DisableKeepAlives != nil {
		p.DisableKeepAlives = *fp.DisableKeepAlives
	}
	if fp.CBEnabled != nil {
		p.CBEnabled = *fp.CBEnabled
	}
	if fp.CBThreshold != nil {
		p.CBThreshold = *fp.CBThreshold
	}

	durations := []struct {
		json  *string
		field *time.Duration
	}{
		{fp.QueueTimeout, &p.QueueTimeout},
		{fp.RetryWaitMin, &p.RetryWaitMin},
		{fp.RetryWaitMax, &p.RetryWaitMax},
		{fp.RetryMinDelay, &p.RetryMinDelay},
		{fp.ReleaseCooldown, &p.ReleaseCooldown},
		{fp.CancelCooldown, &p.CancelCooldown},
		{fp.FailureHold, &p.FailureHold},
		{fp.AdaptiveHeadroomWindow, &p.AdaptiveHeadroomWindow},
		{fp.CBWindow, &p.CBWindow},
		{fp.CBOpenTimeout, &p.CBOpenTimeout},
		{fp.CBMaxOpenTimeout, &p.CBMaxOpen},
		{fp.CBPenalty, &p.CBPenalty},
		{fp.CBMaxPenalty, &p.CBMaxPenalty},
	}
	for _, d := range durations {
		if d.json == nil {
			continue
		}
		text, err := expandStrict(*d.json)
		if err != nil {
			return nil, err
		}
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return nil, fmt.Errorf("invalid duration %q: %w", text, err)
		}
		*d.field = parsed
	}

	return p, nil
}

// expandStrict expands ${VAR} references via os.Expand, failing when any
// referenced variable is unset. Unlike silent empty substitution, this keeps
// a typo'd reference a startup error instead of an empty value.
func expandStrict(s string) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var missing []string
	out := os.Expand(s, func(key string) string {
		value, ok := os.LookupEnv(key)
		if !ok {
			missing = append(missing, key)
			return ""
		}
		return value
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable %q referenced by %q is not set", missing[0], s)
	}
	return out, nil
}
