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
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestParse_LegacyMode_Fields exercises the section-transition behavior: with no
// --provider marker every flag is server scope and lands on the single implicit
// provider.
func TestParse_LegacyMode_Fields(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-bind", ":8080",
		"-tui",
		"-concurrency", "7",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Server.Bind; got != ":8080" {
		t.Errorf("Server.Bind = %q, want %q", got, ":8080")
	}
	if !cfg.Server.TUI {
		t.Error("Server.TUI = false, want true")
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("len(Providers) = %d, want 1", len(cfg.Providers))
	}
	p := cfg.Providers[0]
	if p.Upstream != "https://x" {
		t.Errorf("Provider.Upstream = %q, want %q", p.Upstream, "https://x")
	}
	if p.Prefix != "" {
		t.Errorf("Provider.Prefix = %q, want empty (bare root)", p.Prefix)
	}
	if p.Concurrency != 7 {
		t.Errorf("Provider.Concurrency = %d, want 7", p.Concurrency)
	}
}

// TestParse_LegacyMode_Equals forms: single- and double-dash with -name=value.
func TestParse_LegacyMode_Equals(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream=https://x",
		"--bind=:9999",
		"--concurrency", "3",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Server.Bind; got != ":9999" {
		t.Errorf("Server.Bind = %q, want %q", got, ":9999")
	}
	if got := cfg.Providers[0].Concurrency; got != 3 {
		t.Errorf("Concurrency = %d, want 3", got)
	}
}

// TestParse_GNUForms exercises one-or-two-dash GNU flag forms and explicit
// boolean values.
func TestParse_GNUForms(t *testing.T) {
	cfg, err := Parse([]string{
		"--bind", ":7777",
		"-tui",
		"--circuit-breaker=false",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.Bind != ":7777" {
		t.Errorf("Server.Bind = %q, want %q", cfg.Server.Bind, ":7777")
	}
	if !cfg.Server.TUI {
		t.Error("Server.TUI = false, want true")
	}
	if cfg.Providers[0].CBEnabled {
		t.Error("CBEnabled = true, want false (from --circuit-breaker=false)")
	}
}

// TestParse_RepeatableLimit confirms -limit accumulates within the single provider in
// legacy mode.
func TestParse_RepeatableLimit(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-limit", "POST /v1/messages:2",
		"-limit", "POST /completions:4",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"POST /v1/messages:2", "POST /completions:4"}
	if got := cfg.Providers[0].Limits; !reflect.DeepEqual(got, want) {
		t.Errorf("Limits = %q, want %q", got, want)
	}
}

// TestParse_Terminator checks that -- ends flag parsing and that flags still parse
// before it.
func TestParse_Terminator(t *testing.T) {
	cfg, err := Parse([]string{"-upstream", "https://x", "--"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("len(Providers) = %d, want 1", len(cfg.Providers))
	}
	if cfg.Providers[0].Upstream != "https://x" {
		t.Errorf("Upstream = %q, want %q", cfg.Providers[0].Upstream, "https://x")
	}

	if _, err := Parse([]string{"-upstream", "https://x", "--", "orphan"}); err == nil {
		t.Error("Parse with positional after --: nil error, want error")
	}
}

// TestParse_SectionTransition confirms flags before the first --provider are server
// scope and those after are provider scope.
func TestParse_SectionTransition(t *testing.T) {
	cfg, err := Parse([]string{
		"--bind", ":8080",
		"--provider=acme",
		"-upstream", "https://acme",
		"-concurrency", "3",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.Bind != ":8080" {
		t.Errorf("Server.Bind = %q, want %q", cfg.Server.Bind, ":8080")
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("len(Providers) = %d, want 1", len(cfg.Providers))
	}
	p := cfg.Providers[0]
	if p.Name != "acme" {
		t.Errorf("Name = %q, want %q", p.Name, "acme")
	}
	if p.Concurrency != 3 {
		t.Errorf("Concurrency = %d, want 3", p.Concurrency)
	}
}

// TestParse_ProviderEqualsName verifies the --provider=name form supplies the name
// hint while a bare --provider supplies none.
func TestParse_ProviderEqualsName(t *testing.T) {
	cfg, err := Parse([]string{
		"--provider=acme",
		"-upstream", "https://a",
		"--provider",
		"-upstream", "https://b",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("len(Providers) = %d, want 2", len(cfg.Providers))
	}
	if cfg.Providers[0].Name != "acme" {
		t.Errorf("Providers[0].Name = %q, want %q", cfg.Providers[0].Name, "acme")
	}
	if cfg.Providers[1].Name != "" {
		t.Errorf("Providers[1].Name = %q, want empty", cfg.Providers[1].Name)
	}
}

// TestParse_UnknownFlag checks that an unregistered flag errors with a section label.
func TestParse_UnknownFlag(t *testing.T) {
	_, err := Parse([]string{"-bogus", "x"})
	if err == nil {
		t.Fatal("Parse with unknown flag: nil error, want error")
	}
}

// TestParse_MixedModeProviderFlagAtServerScope rejects a provider-scoped flag
// appearing in the server pre-marker region.
func TestParse_MixedModeProviderFlagAtServerScope(t *testing.T) {
	_, err := Parse([]string{
		"-upstream", "https://a",
		"--provider=acme",
		"-upstream", "https://b",
	})
	if err == nil {
		t.Fatal("Parse with provider flag at server scope: nil error, want error")
	}

	want := "usage error: provider options are not allowed at server scope when --provider sections are used"
	if err.Error() != want {
		t.Errorf("mixed-mode error = %q, want %q", err, want)
	}
}

// TestParse_AccountError checks the --account marker error text exactly.
func TestParse_AccountError(t *testing.T) {
	_, err := Parse([]string{"--provider", "-upstream", "https://x", "--account", "-concurrency", "2"})
	if err == nil {
		t.Fatal("Parse with --account: nil error, want error")
	}
	want := "account sections are not yet supported"
	if err.Error() != want {
		t.Errorf("account error = %q, want %q", err, want)
	}
}

// TestParse_ValueConsumption helper scenario: a value-taking flag in a provider
// section must consume the next argument, never treating it as a marker.
func TestParse_ValueConsumption(t *testing.T) {
	cfg, err := Parse([]string{
		"--provider=acme",
		"-upstream",
		"https://x",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Providers[0].Upstream != "https://x" {
		t.Errorf("Upstream = %q, want %q", cfg.Providers[0].Upstream, "https://x")
	}
}

// TestParse_HelpSentinel_ServerScope: -h at server scope (or legacy top
// level) short-circuits with ErrHelp before any parsing or validation.
func TestParse_HelpSentinel_ServerScope(t *testing.T) {
	for _, args := range [][]string{
		{"-h"},
		{"--help"},
		{"-h", "-upstream", "missing-no-matter"},
		{"-bind", ":9090", "-h"},
		{"-h=true"},
	} {
		cfg, err := Parse(args)
		if !errors.Is(err, ErrHelp) {
			t.Errorf("Parse(%q) err = %v, want ErrHelp", args, err)
		}
		if cfg != nil {
			t.Errorf("Parse(%q) cfg = %v, want nil", args, cfg)
		}
	}
}

// TestParse_HelpSentinel_ProviderScope: -h inside a --provider section also
// help-exits, even when the section is otherwise invalid (no -upstream).
func TestParse_HelpSentinel_ProviderScope(t *testing.T) {
	for _, args := range [][]string{
		{"--provider=acme", "-h"},
		{"--provider", "--help"},
		{"--bind", ":8080", "--provider=acme", "-upstream", "https://x", "-h"},
	} {
		_, err := Parse(args)
		if !errors.Is(err, ErrHelp) {
			t.Errorf("Parse(%q) err = %v, want ErrHelp", args, err)
		}
	}
}

// TestParse_HelpNegationNotHelp: -h=false must NOT trigger help; parsing
// proceeds normally (the missing -upstream is then a semantic validation
// error in ResolveAndValidate — exit 1 territory, not a usage error).
func TestParse_HelpNegationNotHelp(t *testing.T) {
	cfg, err := Parse([]string{"-h=false"})
	if err != nil {
		t.Fatalf("Parse(-h=false): %v", err)
	}
	if cfg == nil || cfg.Server.Help {
		t.Fatalf("Server.Help must stay false for -h=false, got %+v", cfg)
	}
	err = cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: want error (no upstream), got nil")
	}
	if errors.Is(err, ErrUsage) {
		t.Errorf("-h=false is a valid invocation shape; err %v must not be a usage error", err)
	}
}

// TestParse_UnknownFlagIsUsageError: the unknown-flag error wraps ErrUsage so
// main can exit 2, and it names the offending token.
func TestParse_UnknownFlagIsUsageError(t *testing.T) {
	_, err := Parse([]string{"-bogus", "x"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("unknown flag err = %v, want ErrUsage wrapper", err)
	}
	if !strings.Contains(err.Error(), `"‑bogus"`) && !strings.Contains(err.Error(), "-bogus") {
		t.Errorf("unknown flag error should name the token: %v", err)
	}

	// Inside a provider section too.
	_, err = Parse([]string{"--provider=a", "-nope"})
	if !errors.Is(err, ErrUsage) {
		t.Errorf("unknown flag in provider section err = %v, want ErrUsage", err)
	}
}

// TestParse_UsageErrorSites: every command-line-shape failure wraps ErrUsage.
func TestParse_UsageErrorSites(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"missing flag argument", []string{"-bind"}},
		{"argument after --", []string{"-upstream", "https://x", "--", "orphan"}},
		{"mixed scope", []string{"-upstream", "https://a", "--provider=acme", "-upstream", "https://b"}},
	}
	for _, tc := range cases {
		_, err := Parse(tc.args)
		if !errors.Is(err, ErrUsage) {
			t.Errorf("%s: err = %v, want ErrUsage", tc.name, err)
		}
	}
}

// TestParse_ValidationErrorIsNotUsageError: a well-formed command line with a
// bad VALUE is a semantic failure (exit 1), not a usage error (exit 2).
func TestParse_ValidationErrorIsNotUsageError(t *testing.T) {
	cfg, err := Parse([]string{"-upstream", ":::"})
	if err != nil {
		t.Fatalf("Parse is structural; url validity is ResolveAndValidate's job: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err == nil {
		t.Fatal("ResolveAndValidate: want error for invalid URL")
	} else if errors.Is(err, ErrUsage) {
		t.Errorf("invalid value must not be a usage error: %v", err)
	}
}

// TestPrintUsage_CoversAllFlagsAndSections: the rendered usage must contain
// every server flag, the --provider marker, and every provider flag — built
// from the same registrar so it cannot drift.
func TestPrintUsage_CoversAllFlagsAndSections(t *testing.T) {
	var buf bytes.Buffer
	PrintUsage(&buf)
	s := buf.String()

	for _, want := range []string{
		"Usage: ai-concurrency-shaper",
		"Server flags:",
		"--provider[=name]",
		"Provider flags",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("usage missing %q", want)
		}
	}

	// Every registered flag at both scopes must appear. flag.PrintDefaults
	// emits each flag as a line "  -name" followed by a space or tab and the
	// type/usage, or a bare newline for multi-char booleans.
	for name, m := range flagMetadata() {
		if !strings.Contains(s, "\n  -"+name+" ") && !strings.Contains(s, "\n  -"+name+"\t") && !strings.Contains(s, "\n  -"+name+"\n") {
			t.Errorf("usage missing flag %q (scope %s)", name, m.scope)
		}
	}

	// Spot-check defaults render.
	for _, want := range []string{
		`(default ":8080")`,
		"(default 4)",
		"(default 30s)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("usage missing default %q", want)
		}
	}
}
