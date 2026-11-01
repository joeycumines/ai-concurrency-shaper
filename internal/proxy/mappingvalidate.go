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

package proxy

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// Validate checks the immutable transcode route configuration so a
// misconfigured route fails at proxy.New, never on the first request:
// the mapping (route, direction, auth, model map),
// the body limits, and the allowed client query keys.
func (m TranscodeMapping) Validate() error {
	if err := m.Mapping.Validate(); err != nil {
		return err
	}
	if err := m.BodyLimits.Validate(); err != nil {
		return fmt.Errorf("body limits: %w", err)
	}
	if m.FlowLogDir != "" {
		info, err := os.Stat(m.FlowLogDir)
		if err != nil {
			return fmt.Errorf("flow log directory: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("flow log directory %q is not a directory", m.FlowLogDir)
		}
	}
	for key := range m.AllowedClientQuery {
		if !validQueryName(key) {
			return fmt.Errorf(
				"allowed client query key %q is not a valid query name",
				key,
			)
		}
	}
	return nil
}

// validQueryName reports whether s is a valid client query parameter name on
// a transcoded route: non-empty, no control characters, and none of the
// characters that delimit or encode query syntax.
// HTTP field-name syntax is not query-name syntax: query names may contain
// characters such as '@' or '/' that field names reject, while '=' '&' '#'
// and '?' would alter the query structure.
func validQueryName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x21 || c == 0x7f || strings.ContainsRune("=%&#?", rune(c)) {
			return false
		}
	}
	return true
}

// cloneModelMap deep-copies the mutable model map so caller mutation after
// New cannot change live behavior.
func cloneModelMap(m transcode.ModelMap) transcode.ModelMap {
	if m.Exact != nil {
		cloned := make(map[string]transcode.ModelMapping, len(m.Exact))
		maps.Copy(cloned, m.Exact)
		m.Exact = cloned
	}
	return m
}

// cloneLossPolicy deep-copies the mutable loss policy.
func cloneLossPolicy(p transcode.LossPolicy) transcode.LossPolicy {
	if p.Allowed != nil {
		cloned := make(map[transcode.Feature]struct{}, len(p.Allowed))
		for feature := range p.Allowed {
			cloned[feature] = struct{}{}
		}
		p.Allowed = cloned
	}
	return p
}

// cloneStringSet deep-copies a string set.
func cloneStringSet(s map[string]struct{}) map[string]struct{} {
	if s == nil {
		return nil
	}
	cloned := make(map[string]struct{}, len(s))
	for key := range s {
		cloned[key] = struct{}{}
	}
	return cloned
}

// cloneURL copies the upstream URL value so caller mutation of the original
// after New cannot change the live target.
func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	cloned := *u
	return &cloned
}

// cloneLimiterMap copies the route-limiters map (the limiter pointers stay
// shared: they are live synchronization objects) so caller mutation of the
// original map after New cannot change live routing.
func cloneLimiterMap(m map[string]*queue.Limiter) map[string]*queue.Limiter {
	if m == nil {
		return nil
	}
	cloned := make(map[string]*queue.Limiter, len(m))
	maps.Copy(cloned, m)
	return cloned
}

// cloneMatcher deep-copies the caller's matcher — including every
// Pattern.Segments slice — so caller mutation of the original after New
// cannot change live route matching.
func cloneMatcher(m *route.Matcher) *route.Matcher {
	if m == nil {
		return nil
	}
	patterns := m.Patterns()
	cloned := make([]route.Pattern, len(patterns))
	for i, p := range patterns {
		cloned[i] = p
		if p.Segments != nil {
			cloned[i].Segments = append([]string(nil), p.Segments...)
		}
	}
	return route.NewMatcher(cloned)
}

// MaxIdleConnsPerHost sizes the upstream transport's per-host idle connection
// pool to fit the requested limiters.
//
// routeLimiters is keyed exactly as the proxy expects: by group name, or by the
// raw pattern when the pattern has no group. The returned value counts every route
// limiter's capacity, plus the default pool whenever it is actually used (a pattern
// with no group and no limit; a group-less unlimited pattern; a grouped pattern
// whose group resolves to no limiter; or -limit-all), capped by the global
// concurrency pool when one exists not to exceed the global concurrency. A floor of
// 20 keeps the shared transport healthy.
//
// This is the connection pool for admission, not a hard cap: the proxy stays
// transparent and never varies from the limiters.
func MaxIdleConnsPerHost(globalConcurrency, concurrency int, patterns []route.Pattern, routeLimiters map[string]*queue.Limiter, limitAll bool) int {
	routePoolMax := 0
	for _, lim := range routeLimiters {
		routePoolMax += lim.Limit()
	}

	defaultPoolUsed := limitAll
	for _, p := range patterns {
		key := p.Group
		if key == "" {
			key = p.Raw
		}
		if p.Limit == 0 {
			if p.Group == "" {
				defaultPoolUsed = true
				continue
			}
			if _, ok := routeLimiters[key]; !ok {
				defaultPoolUsed = true
			}
		}
	}
	if defaultPoolUsed {
		routePoolMax += concurrency
	}
	if globalConcurrency > 0 && routePoolMax > globalConcurrency {
		routePoolMax = globalConcurrency
	}
	if routePoolMax < 20 {
		return 20
	}
	return routePoolMax
}
