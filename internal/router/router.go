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

// Package router dispatches HTTP requests to one of several providers by a
// literal path-prefix mount. Each provider is mounted at a distinct prefix
// (e.g. /acme) and the request path is stripped of the matching prefix before
// it is forwarded — a "true mount": /acme/v1/messages reaches the acme
// upstream as /v1/messages. When exactly one provider is mounted at the bare
// root ("") the router is a transparent pass-through, preserving the
// single-provider behavior of the proxy.
//
// The dispatcher only rewrites r.URL.Path (and clears RawPath) before
// delegating; it never inspects or munges response bodies, and it does not
// interfere with Upgrade/Hijack — those are owned by the underlying
// httputil.ReverseProxy.
package router

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Provider mounts a single proxy (or any http.Handler) at a path prefix.
type Provider struct {
	// Name is the display/identity label ("" for a bare single provider).
	Name string
	// Prefix is the mount path; "" means bare root (transparent pass-through).
	Prefix string
	// Proxy is the handler requests matching Prefix are delegated to.
	Proxy http.Handler
}

// Handler dispatches incoming HTTP requests by path prefix to the appropriate
// provider's proxy.
type Handler struct {
	providers []Provider
	// bare points to the single bare-root provider when exactly one exists,
	// bypassing the slice scan on the hot path.
	bare *Provider
}

// New constructs a prefix-matching router over the given providers.
//
// Semantics:
//
//   - A single provider with Prefix == "" or "/" is a bare-root mount: it receives
//     every request after normalizing the path to match prefixed mount semantics.
//   - Multiple providers with Prefix == "" are rejected (they would collide on the
//     bare root).
//   - When any provider has a non-empty Prefix, every provider must have a
//     non-empty prefix (mixing bare and prefixed providers is illegal).
//   - Overlapping prefixes (where one prefix is a segment-wise prefix of another,
//     such as /acme and /acme/v1) are rejected at construction.
//   - Every provider's Proxy handler must not be nil.
//
// Prefix values are matched literally (split on '/' only, not ':') matching the
// segmentation rules of the CLI validator.
func New(providers []Provider) (*Handler, error) {
	if len(providers) == 0 {
		return nil, errors.New("router: at least one provider is required")
	}

	h := &Handler{
		providers: make([]Provider, len(providers)),
	}
	copy(h.providers, providers)

	// Validate handlers and prefixes.
	bare := -1
	for i := range h.providers {
		p := &h.providers[i]
		if p.Proxy == nil {
			if p.Name != "" {
				return nil, fmt.Errorf("router: provider %q has nil Proxy", p.Name)
			}
			return nil, fmt.Errorf("router: provider %d (%q) has nil Proxy", i, p.Prefix)
		}
		p.Prefix = normalizePrefix(p.Prefix)
		if p.Prefix == "" {
			if bare >= 0 {
				return nil, errors.New("router: only one bare root (empty prefix) provider is allowed")
			}
			bare = i
		}
	}

	// Mixing bare and prefixed mounts is rejected: if there are >1 providers,
	// every provider must have a non-empty prefix.
	if len(h.providers) > 1 && bare >= 0 {
		return nil, fmt.Errorf("router: provider %q has empty prefix, but multiple providers are configured (every provider requires a distinct prefix)", h.providers[bare].Name)
	}

	// Overlap check (only relevant with >1 providers).
	for a := range h.providers {
		for b := a + 1; b < len(h.providers); b++ {
			if prefixesOverlap(h.providers[a].Prefix, h.providers[b].Prefix) {
				return nil, fmt.Errorf(
					"router: provider prefixes overlap: %q (%s) and %q (%s)",
					h.providers[a].Prefix, h.providers[a].Name,
					h.providers[b].Prefix, h.providers[b].Name,
				)
			}
		}
	}

	if bare >= 0 {
		h.bare = &h.providers[bare]
	}
	return h, nil
}

// ServeHTTP routes the request to the provider whose mount prefix matches the
// request path, stripping the prefix before delegating. With a single bare
// provider (h.bare set) it delegates after normalizing the path through the
// same traversal-resolved joinSegments / RawPath clearing as prefixed mounts,
// ensuring route key dispatch parity. No match yields 404.
//
// Inbound requests are shallow-cloned before mutating URL path fields, ensuring
// the caller's request object and URL are never mutated (review-16 finding 1).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.bare != nil {
		r2 := new(http.Request)
		*r2 = *r
		if r.URL != nil {
			r2.URL = new(url.URL)
			*r2.URL = *r.URL
			r2.URL.Path = joinSegments(segments(r.URL.Path))
			r2.URL.RawPath = ""
		}
		h.bare.Proxy.ServeHTTP(w, r2)
		return
	}

	segs := segments(r.URL.Path)
	for i := range h.providers {
		p := &h.providers[i]
		pre := segments(p.Prefix)
		if len(segs) < len(pre) {
			continue
		}
		match := true
		for k := range pre {
			if segs[k] != pre[k] {
				match = false
				break
			}
		}
		if !match {
			continue
		}

		// Rebuild the path from the remaining segments. Clearing RawPath lets
		// the downstream reverse proxy re-encode from the decoded Path.
		r2 := new(http.Request)
		*r2 = *r
		if r.URL != nil {
			r2.URL = new(url.URL)
			*r2.URL = *r.URL
			r2.URL.Path = joinSegments(segs[len(pre):])
			r2.URL.RawPath = ""
		}
		p.Proxy.ServeHTTP(w, r2)
		return
	}

	http.NotFound(w, r)
}

// segments resolves a path into its traversal-resolved /-separated segments.
// Unlike route.splitSegments it does NOT additionally split on ':' — a prefix
// mount must not mangle endpoints that contain a colon (e.g. Gemini's
// /v1/models/gemini-pro:generateContent).
func segments(path string) []string {
	var out []string
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, seg)
	}
	return out
}

// joinSegments rebuilds a "/"-separated path from segments, always with a
// leading slash; an empty input maps to "/".
func joinSegments(segs []string) string {
	if len(segs) == 0 {
		return "/"
	}
	var b strings.Builder
	b.Grow(len(segs) * 2)
	for _, s := range segs {
		b.WriteByte('/')
		b.WriteString(s)
	}
	return b.String()
}

// normalizePrefix normalizes a mount prefix: "/" becomes "", a trailing slash
// is removed.
func normalizePrefix(prefix string) string {
	if prefix == "" || prefix == "/" {
		return ""
	}
	return strings.TrimSuffix(prefix, "/")
}

// prefixesOverlap reports whether two normalized prefixes have a segment-wise
// prefix relationship (either is a prefix of the other), meaning they would
// route the same request. An empty prefix (bare root) overlaps everything.
func prefixesOverlap(a, b string) bool {
	as := segments(a)
	bs := segments(b)
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
