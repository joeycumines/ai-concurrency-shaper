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

package auth

import (
	"net/http"
	"strings"
)

// credentialHeaderNames are the canonical header names StripCredentials
// removes at every provider boundary. The list matches the proven sibling
// implementation exactly: an LLM gateway never has a reason to forward these
// across providers, and each name is re-injectable via an explicit mode or a
// custom header.
var credentialHeaderNames = []string{
	"Authorization",
	"Proxy-Authorization",
	"X-Api-Key",
	"Api-Key",
	"X-Goog-Api-Key",
}

// protocolHeaderNames carry provider protocol state rather than credentials,
// but they are still stripped at every boundary: an Anthropic version or beta
// selection must never bleed into another provider's request.
var protocolHeaderNames = []string{
	"Anthropic-Version",
	"Anthropic-Beta",
}

// displayOnlyCredentialNames carry values too sensitive to render but NOT
// safe to strip unconditionally (stripping would break legitimate
// cookie-authenticated upstreams). They are redacted from journal/TUI display
// only.
var displayOnlyCredentialNames = []string{
	"Cookie",
}

// isCloudSignaturePrefix reports whether a canonical header name belongs to a
// cloud-provider signature family (AWS X-Amz-*, Google X-Goog-*). Such
// headers sign one specific request for one specific provider and are removed
// before the target credential runs.
func isCloudSignaturePrefix(canonical string) bool {
	return strings.HasPrefix(canonical, "X-Amz-") ||
		strings.HasPrefix(canonical, "X-Goog-")
}

// sensitive reports whether a header NAME carries values too sensitive to
// display. Protocol headers are not sensitive: values like "2023-06-01" are
// configuration, not credentials.
func sensitive(canonical string) bool {
	for _, name := range credentialHeaderNames {
		if name == canonical {
			return true
		}
	}
	for _, name := range displayOnlyCredentialNames {
		if name == canonical {
			return true
		}
	}
	return isCloudSignaturePrefix(canonical)
}

// StripCredentials removes every client-supplied credential and provider
// protocol header from h so no credential crosses a provider boundary: the
// named credential and protocol families plus case-insensitive X-Amz-*/X-Goog-*
// prefixed keys. It mutates h in place.
func StripCredentials(h http.Header) {
	for _, name := range credentialHeaderNames {
		h.Del(name)
	}
	for _, name := range protocolHeaderNames {
		h.Del(name)
	}
	// http.Header range yields the stored spelling, which may be
	// non-canonical for hand-built headers; classify on the canonical form so
	// "x-amz-date" and "X-Amz-Date" behave identically. Del itself is
	// case-insensitive.
	for name := range h {
		if isCloudSignaturePrefix(http.CanonicalHeaderKey(name)) {
			h.Del(name)
		}
	}
}

// RedactSensitiveHeaders replaces the VALUES of every credential-carrying
// header with "[REDACTED]", preserving key names, ordering, and multi-value
// structure so journal entries and TUI panels stay useful for debugging
// without disclosing secrets. Non-sensitive headers pass through untouched;
// protocol headers (e.g. Anthropic-Version) stay visible because they carry
// configuration, not credentials. It mutates h in place and returns it.
func RedactSensitiveHeaders(h http.Header) http.Header {
	const placeholder = "[REDACTED]"
	for name, values := range h {
		if !sensitive(http.CanonicalHeaderKey(name)) {
			continue
		}
		redacted := make([]string, len(values))
		for i := range values {
			redacted[i] = placeholder
		}
		h[name] = redacted
	}
	return h
}
