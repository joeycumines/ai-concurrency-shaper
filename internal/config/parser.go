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
	"errors"
	"fmt"
	"strings"
)

// ErrHelp is returned by Parse when -h or -help was requested at any scope.
// The caller prints the usage text (Usage) to stdout and exits 0.
var ErrHelp = errors.New("help requested")

// ErrUsage is wrapped by every parse error caused by the command line's shape
// (unknown flag, missing flag argument, mixed-scope misuse) rather than by an
// invalid value. Callers map it to the GNU usage-error exit code 2 and hint at
// -h, matching the pre-sectioned binary's flag package behavior.
var ErrUsage = errors.New("usage error")

// section is one configuration section produced by the token walker: its scope
// (server/provider/account), the provider name hint from a --provider=name
// marker, and the raw lexemes to hand to the section's FlagSet.
type section struct {
	kind Scope
	name string // provider name hint from --provider=name; never from -name
	lex  []string
}

// Parse parses a command line into a Config.
//
// Two modes are supported. Legacy mode (no --provider section marker)
// preserves the original flat invocation byte-for-byte: every flag is server scope
// and configures a single implicit provider at bare root. Sectioned mode (any
// --provider[=name]) scopes options: flags before the first marker are server
// scope, each marker opens a provider section, and any provider-scoped flag at
// server scope is rejected (mixed mode is forbidden). --account is a recognized
// but reserved marker that is always rejected.
//
// Parse performs the structural and type-level work. Semantic validation and
// runtime-object construction happen in ResolveAndValidate.
func Parse(args []string) (*Config, error) {
	sections, err := tokenize(args)
	if err != nil {
		return nil, err
	}

	cfg := &Config{}

	// Sectioned mode is true when any non-server section (provider or account)
	// marker appears.
	sectioned := false
	for _, s := range sections[1:] {
		if s.kind != scopeServer {
			sectioned = true
			break
		}
	}

	// The first section is always the server section (legacy mode folds every
	// flag into it).
	serverSec := sections[0]

	// -h/-help short-circuits before any parsing or validation: help is a
	// usage request, not a configuration. Whichever section asked, the answer
	// is the same complete usage text.
	if serverHelpRequested(sections) {
		return nil, ErrHelp
	}

	if !sectioned {
		// Legacy mode: every flag is server scope; the provider flags
		// bind to the single implicit provider.
		fs := newFlagSet("server")
		p0 := &Provider{}
		registerServerFlags(&registrar{fs: fs, meta: map[string]flagMeta{}}, &cfg.Server)
		registerProviderFlags(&registrar{fs: fs, meta: map[string]flagMeta{}}, p0)
		if err := fs.Parse(serverSec.lex); err != nil {
			return nil, err
		}
		cfg.Providers = []*Provider{p0}
		return cfg, nil
	}

	// Sectioned mode.

	// Mixed mode is forbidden: a provider-scoped flag at server scope when
	// --provider sections are used.
	for _, lex := range serverSec.lex {
		if flagMetadata()[flagTokenName(lex)].scope == scopeProvider {
			return nil, fmt.Errorf("%w: provider options are not allowed at server scope when --provider sections are used", ErrUsage)
		}
	}

	fs := newFlagSet("server section")
	registerServerFlags(&registrar{fs: fs, meta: map[string]flagMeta{}}, &cfg.Server)
	if err := fs.Parse(serverSec.lex); err != nil {
		return nil, fmt.Errorf("server section: %w", err)
	}

	for _, s := range sections[1:] {
		switch s.kind {
		case scopeAccount:
			return nil, errors.New("account sections are not yet supported")
		case scopeProvider:
			p := &Provider{}
			ps := newFlagSet(fmt.Sprintf("provider section %d", len(cfg.Providers)+1))
			registerProviderFlags(&registrar{fs: ps, meta: map[string]flagMeta{}}, p)
			if err := ps.Parse(s.lex); err != nil {
				return nil, fmt.Errorf("provider section %d: %w", len(cfg.Providers)+1, err)
			}
			if p.Name == "" {
				p.Name = s.name
			}
			cfg.Providers = append(cfg.Providers, p)
		default:
			return nil, fmt.Errorf("unsupported section kind %s", s.kind)
		}
	}

	return cfg, nil
}

// serverHelpRequested reports whether any raw lexeme in any section requests
// help (-h/-help in either dash form, with or without =true). It inspects the
// pre-parse lexemes so help works even when the section's flags would
// otherwise fail validation first (e.g. "--provider=acme -h" with no
// -upstream anywhere).
func serverHelpRequested(sections []section) bool {
	for _, s := range sections {
		for _, tok := range s.lex {
			m, ok := flagMetadata()[flagTokenName(tok)]
			if !ok || !m.isHelp {
				continue
			}
			// Reject "-h=false" style negations: only an affirmative request
			// triggers help.
			if _, after, cut := strings.Cut(tok, "="); cut && !isTrueBool(after) {
				continue
			}
			return true
		}
	}
	return false
}

// isTrueBool reports whether s is a value the flag package accepts for a
// boolean true.
func isTrueBool(s string) bool {
	switch s {
	case "1", "t", "T", "true", "TRUE", "True":
		return true
	}
	return false
}

// tokenize walks the raw arguments into sections. It understands the section
// markers (--provider[=name], --account), the -- terminator, and which
// flags consume a following value. Unknown flags and provider-scoped flags outside a
// provider section are errors.
func tokenize(args []string) ([]section, error) {
	meta := flagMetadata()

	sections := []section{{kind: scopeServer}}
	cur := 0

	terminated := false

	flush := func(name string, kind Scope) {
		sections = append(sections, section{kind: kind, name: name})
		cur = len(sections) - 1
	}

	for i := 0; i < len(args); i++ {
		tok := args[i]

		if terminated {
			return nil, fmt.Errorf("%w: unexpected argument after --: %q", ErrUsage, tok)
		}

		if tok == "--" {
			terminated = true
			continue
		}

		name := flagTokenName(tok)
		if name == "provider" && (tok == "-provider" || tok == "--provider" || strings.HasPrefix(tok, "-provider=") || strings.HasPrefix(tok, "--provider=")) {
			flush(providerMarkName(tok), scopeProvider)
			continue
		}
		if name == "account" && (tok == "-account" || tok == "--account" || strings.HasPrefix(tok, "-account=") || strings.HasPrefix(tok, "--account=")) {
			flush("", scopeAccount)
			continue
		}

		m, ok := meta[name]
		if !ok {
			return nil, fmt.Errorf("%w: unknown flag: %q", ErrUsage, tok)
		}

		sections[cur].lex = append(sections[cur].lex, tok)

		if !strings.Contains(tok, "=") && !m.isBool {
			// Value-taking flag: consume the next argument as its
			// value, exactly like the standard flag package. The
			// consumed value is never examined for markers.
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%w: flag needs an argument: %s", ErrUsage, name)
			}
			sections[cur].lex = append(sections[cur].lex, args[i+1])
			i++
		}
	}

	return sections, nil
}

// flagTokenName strips leading dashes and the =value suffix, yielding the flag
// or marker name ("--provider=acme" → "provider").
func flagTokenName(tok string) string {
	name := strings.TrimLeft(tok, "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	return name
}

// providerMarkName extracts the name hint from a --provider=name marker.
func providerMarkName(tok string) string {
	if _, after, ok := strings.Cut(tok, "="); ok {
		return after
	}
	return ""
}
