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
	"fmt"
	"slices"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// CatalogSuiteConfig defines one mounted catalog suite: discovery and completion
// routes mounted at a specific path prefix, routing models to providers.
type CatalogSuiteConfig struct {
	Name     string
	Prefix   string
	Models   []string // empty means all models from model table
	Format   transcode.CatalogShape
	Provider string // optional filter by provider name
	Strict   bool
	Raw      string
}

// parseCatalogSuite parses one -catalog-suite value:
//
//	[name@]prefix[=models][;options]
//
// where options are semicolon-separated key=value pairs:
//
//	format=codex|anthropic|openai|auto
//	provider=provider-name
//	strict=true|false
func parseCatalogSuite(raw string) (CatalogSuiteConfig, error) {
	if raw == "" {
		return CatalogSuiteConfig{}, fmt.Errorf("empty -catalog-suite")
	}

	left, rest, hasOptions := strings.Cut(raw, ";")
	mount, modelsStr, hasModels := strings.Cut(left, "=")

	var name, prefix string
	if before, after, ok := strings.Cut(mount, "@"); ok {
		name = before
		prefix = after
	} else {
		prefix = mount
	}

	prefix, err := cleanCatalogPrefix(prefix)
	if err != nil {
		return CatalogSuiteConfig{}, fmt.Errorf("invalid -catalog-suite %q: %w", raw, err)
	}
	if name == "" {
		trimmed := strings.Trim(strings.ReplaceAll(prefix, "/", "-"), "-")
		if trimmed == "" {
			name = "catalog"
		} else {
			name = trimmed
		}
	}

	cfg := CatalogSuiteConfig{
		Name:   name,
		Prefix: prefix,
		Strict: true,
		Raw:    raw,
	}

	if hasModels && modelsStr != "" && modelsStr != "*" {
		var models []string
		for _, m := range strings.FieldsFunc(modelsStr, func(r rune) bool { return r == '+' || r == ',' }) {
			m = strings.TrimSpace(m)
			if m != "" {
				if !validModelTableIdent(m, modelTableMaxSurrogateLen) {
					return CatalogSuiteConfig{}, fmt.Errorf("invalid -catalog-suite %q: invalid model surrogate %q", raw, m)
				}
				models = append(models, m)
			}
		}
		cfg.Models = models
	}

	if hasOptions {
		for segment := range strings.SplitSeq(rest, ";") {
			if segment == "" {
				continue
			}
			k, v, hasV := strings.Cut(segment, "=")
			switch k {
			case "format":
				if !hasV || v == "" {
					return CatalogSuiteConfig{}, fmt.Errorf("invalid -catalog-suite %q: format requires value", raw)
				}
				switch strings.ToLower(strings.TrimSpace(v)) {
				case "codex", "responses":
					cfg.Format = transcode.CatalogShapeCodex
				case "anthropic", "messages":
					cfg.Format = transcode.CatalogShapeAnthropic
				case "openai", "chat", "chat-completions":
					cfg.Format = transcode.CatalogShapeOpenAI
				case "auto":
					cfg.Format = ""
				default:
					return CatalogSuiteConfig{}, fmt.Errorf("invalid -catalog-suite %q: unknown format %q (want codex, anthropic, openai, or auto)", raw, v)
				}
			case "provider":
				if !hasV || v == "" {
					return CatalogSuiteConfig{}, fmt.Errorf("invalid -catalog-suite %q: provider requires value", raw)
				}
				cfg.Provider = strings.TrimSpace(v)
			case "strict":
				if hasV && (v == "false" || v == "0") {
					cfg.Strict = false
				} else {
					cfg.Strict = true
				}
			default:
				return CatalogSuiteConfig{}, fmt.Errorf("invalid -catalog-suite %q: unknown option %q", raw, k)
			}
		}
	}

	return cfg, nil
}

func cleanCatalogPrefix(prefix string) (string, error) {
	if prefix == "" || prefix == "/" {
		return "", nil
	}
	if !strings.HasPrefix(prefix, "/") {
		return "", fmt.Errorf("prefix must start with /, got %q", prefix)
	}
	return strings.TrimSuffix(prefix, "/"), nil
}

// resolveCatalogSuites parses, validates, and checks prefix non-overlap for all
// configured -catalog-suite flags.
func (c *Config) resolveCatalogSuites() error {
	if len(c.Server.CatalogSuites) == 0 {
		return nil
	}

	suites := make([]CatalogSuiteConfig, 0, len(c.Server.CatalogSuites))
	for _, raw := range c.Server.CatalogSuites {
		suite, err := parseCatalogSuite(raw)
		if err != nil {
			return err
		}
		// Check overlap against earlier catalog suites
		for _, prev := range suites {
			if segmentsOverlap(suite.Prefix, prev.Prefix) {
				return fmt.Errorf("catalog suite prefix %q in %q overlaps with catalog suite prefix %q", suite.Prefix, suite.Raw, prev.Prefix)
			}
		}
		// Check overlap against configured providers
		for _, p := range c.Providers {
			if segmentsOverlap(suite.Prefix, p.Prefix) {
				return fmt.Errorf("catalog suite prefix %q in %q overlaps with provider %q prefix %q", suite.Prefix, suite.Raw, effectiveName(p), p.Prefix)
			}
		}
		suites = append(suites, suite)
	}

	c.catalogSuites = suites
	return nil
}

// CatalogSuites returns a deep copy of the configured catalog suites.
func (c *Config) CatalogSuites() []CatalogSuiteConfig {
	out := make([]CatalogSuiteConfig, len(c.catalogSuites))
	for i, s := range c.catalogSuites {
		s.Models = slices.Clone(s.Models)
		out[i] = s
	}
	return out
}
