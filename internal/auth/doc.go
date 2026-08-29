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
// verbatim (stripping them would break cookie-authenticated upstreams) and,
// per the TUI Redaction Constraint in AGENTS.md, are displayed raw — journal
// and TUI output is not captured and is visible only to the local operator.
// The only scrub applied on a captured path is sanitizeTransportError's use
// of RedactSensitiveURL for transport-error logs.
package auth
