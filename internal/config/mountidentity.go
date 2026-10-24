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
	"strings"
)

// This file owns the provider mount's identity resolution: the -prefix
// normalization and overlap rules that decide which mount owns a path, and
// the -name derivation that gives each mount its display identity.

// normalizePrefix normalizes a mount prefix: "/" becomes "", a trailing slash is
// removed, and the leading slash requirement is enforced.
func normalizePrefix(p *Provider) error {
	if p.Prefix == "" || p.Prefix == "/" {
		p.Prefix = ""
		return nil
	}
	if !strings.HasPrefix(p.Prefix, "/") {
		return fmt.Errorf("-prefix must start with /, got %q", p.Prefix)
	}
	// Keep internal slashes; drop at most one trailing slash.
	p.Prefix = strings.TrimSuffix(p.Prefix, "/")
	return nil
}

// prefixSegments splits a normalized prefix into traversal-resolved segments for
// overlap comparison: empty and "." segments are dropped, and ".." pops the
// previous segment. This deliberately matches the router's literal (colon-free)
// mount segmentation — the prefix is a literal mount prefix, not a route pattern.
func prefixSegments(prefix string) []string {
	var segs []string
	for s := range strings.SplitSeq(prefix, "/") {
		if s == "" || s == "." {
			continue
		}
		if s == ".." {
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
			continue
		}
		segs = append(segs, s)
	}
	return segs
}

// segmentsOverlap reports whether the two normalized prefixes have a segment-wise
// prefix relationship (either is a prefix of the other), meaning they would route
// the same request.
func segmentsOverlap(a, b string) bool {
	as := prefixSegments(a)
	bs := prefixSegments(b)
	if len(as) > len(bs) {
		as, bs = bs, as
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// deriveName derives a provider display name from its upstream host:
// strip the port, strip a leading "api."/"api-", and take the first
// dot-segment. api.anthropic.com → anthropic. The host is the port-free,
// bracket-free form from url.URL.Hostname() — IPv6 literals such as
// "[::1]:9000" derive as "::1", without brackets or port.
func deriveName(host string) string {
	h := host
	h = strings.TrimPrefix(h, "api.")
	h = strings.TrimPrefix(h, "api-")
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	return h
}

func nameLabel(p *Provider) string {
	if p.Name != "" {
		return p.Name
	}
	if p.upstream != nil {
		return p.upstream.Host
	}
	return "(unnamed)"
}
