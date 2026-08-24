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
	"io"
)

// PrintUsage writes the complete sectioned CLI usage text to w: a header line,
// the server-scope flags, the --provider[=name] section marker, and the
// provider-scope flags. It is what -h/-help prints and what the footer of an
// unknown-flag error points at.
//
// The per-flag text is produced by the standard flag package's own
// PrintDefaults, applied to throwaway FlagSets built with the very same
// registrar used at parse time — so the usage can never drift from the flags
// the parser actually accepts.
func PrintUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage: ai-concurrency-shaper [server flags] [--provider[=name] provider flags]...\n\n")
	fmt.Fprintf(w, "Server flags configure the listener. Provider flags configure one upstream\n")
	fmt.Fprintf(w, "and may appear at the top level (single implicit provider, legacy form) or\n")
	fmt.Fprintf(w, "inside a --provider section (multi-provider form).\n\n")

	fmt.Fprintln(w, "Server flags:")
	printScopeDefaults(w, scopeServer)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Provider sections:")
	fmt.Fprintln(w, "  --provider[=name]  open a provider section; name is a display hint (repeatable)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Provider flags (per section, or at top level for the single implicit provider):")
	printScopeDefaults(w, scopeProvider)
}

// printScopeDefaults builds a throwaway FlagSet for the given scope via the
// same registrar the parser uses, then lets the flag package format it. The
// throwaway targets receive the defaults but are discarded; only the FlagSet's
// flag registry (names, defaults, usage strings) is consulted.
func printScopeDefaults(w io.Writer, scope Scope) {
	fs := newFlagSet(scope.String())
	meta := map[string]flagMeta{}
	switch scope {
	case scopeServer:
		registerServerFlags(&registrar{fs: fs, meta: meta}, &Server{})
	case scopeProvider:
		registerProviderFlags(&registrar{fs: fs, meta: meta}, &Provider{})
	}
	fs.SetOutput(w)
	// Usage is a no-op on these FlagSets (newFlagSet silences it); PrintDefaults
	// is the direct path to the per-flag text we want.
	fs.PrintDefaults()
}
