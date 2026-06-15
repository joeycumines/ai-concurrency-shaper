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

// Package route provides fuzzy HTTP route pattern matching for concurrency-limited
// LLM API endpoints. Patterns match by consecutive path segments, so
// "POST /chat/completions" matches /v1/chat/completions, /api/v2/chat/completions,
// /openai/deployments/my-dep/chat/completions, etc.
package route

import (
	"fmt"
	"strconv"
	"strings"
)

// Pattern is a parsed route pattern.
type Pattern struct {
	Method   string
	Segments []string
	Raw      string
	Limit    int    // per-route concurrency limit; 0 = use default pool
	Group    string // non-empty when patterns share a limiter
}

// Parse parses a pattern string.
//
// Supported forms:
//
//	"POST /v1/messages"             - limited, uses default pool
//	"POST /v1/messages:4"          - limit 4
//	"POST /v1/messages:4@anthropic" - limit 4, shares "anthropic" pool
//	"POST /v1/messages:@anthropic"  - shares "anthropic" pool, default limit
func Parse(s string) (Pattern, error) {
	s = strings.TrimSpace(s)
	before, after, ok := strings.Cut(s, " ")
	if !ok {
		return Pattern{}, fmt.Errorf("route: invalid pattern %q", s)
	}
	method := strings.ToUpper(before)
	rest := strings.TrimSpace(after)
	if method == "" {
		return Pattern{}, fmt.Errorf("route: empty method in %q", s)
	}

	path, limit, group := parsePathLimitGroup(rest)

	if !strings.HasPrefix(path, "/") {
		return Pattern{}, fmt.Errorf("route: path must start with / in %q", s)
	}

	return Pattern{
		Method:   method,
		Segments: splitSegments(path),
		Raw:      s,
		Limit:    limit,
		Group:    group,
	}, nil
}

func parsePathLimitGroup(s string) (path string, limit int, group string) {
	if at := strings.LastIndex(s, "@"); at >= 0 {
		group = s[at+1:]
		s = s[:at]
	}
	if idx := strings.LastIndex(s, ":"); idx >= 0 {
		candidate := s[idx+1:]
		if n, err := strconv.Atoi(candidate); err == nil && n > 0 {
			limit = n
			s = s[:idx]
		}
	}
	path = s
	return
}

// Match returns true if the request method+path matches this pattern.
// Matching is fuzzy: the pattern segments must appear consecutively anywhere
// in the request path, regardless of prefix. This means "POST /chat/completions"
// matches /v1/chat/completions, /api/v2/chat/completions, etc.
func (p Pattern) Match(method, path string) bool {
	if !strings.EqualFold(method, p.Method) {
		return false
	}
	if len(p.Segments) == 0 {
		return true
	}
	return containsConsecutive(splitSegments(path), p.Segments)
}

func (p Pattern) String() string { return p.Raw }

func splitSegments(path string) []string {
	var out []string
	for seg := range strings.SplitSeq(path, "/") {
		if seg != "" {
			// Split on colon for Gemini-style endpoints like
			// /v1/models/gemini-pro:generateContent
			for sub := range strings.SplitSeq(seg, ":") {
				if sub != "" {
					out = append(out, sub)
				}
			}
		}
	}
	return out
}

func containsConsecutive(haystack, needle []string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j, v := range needle {
			if !strings.EqualFold(haystack[i+j], v) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// Matcher holds a collection of patterns.
type Matcher struct {
	patterns []Pattern
}

func NewMatcher(patterns []Pattern) *Matcher {
	cp := make([]Pattern, len(patterns))
	copy(cp, patterns)
	return &Matcher{patterns: cp}
}

func (m *Matcher) AddPattern(p Pattern) { m.patterns = append(m.patterns, p) }

// IsLimited returns true if the request matches any known LLM endpoint pattern.
func (m *Matcher) IsLimited(method, path string) bool {
	for _, p := range m.patterns {
		if p.Match(method, path) {
			return true
		}
	}
	return false
}

// FindMatch returns the first pattern that matches, or nil if none match.
// Used by the proxy to look up per-route limiters.
func (m *Matcher) FindMatch(method, path string) *Pattern {
	for i := range m.patterns {
		if m.patterns[i].Match(method, path) {
			return &m.patterns[i]
		}
	}
	return nil
}

func (m *Matcher) Patterns() []Pattern {
	cp := make([]Pattern, len(m.patterns))
	copy(cp, m.patterns)
	return cp
}

// DefaultPatterns returns the built-in set of patterns that match all known
// LLM API endpoints across major providers. These use fuzzy segment matching,
// so they work regardless of path prefix (e.g., /v1/chat/completions,
// /api/v1/chat/completions, /openai/deployments/x/chat/completions all match).
//
// When no -limit flags are provided, the proxy uses these defaults automatically.
// Every pattern is POST-only (GET endpoints like /v1/models are lightweight and
// pass through without limiting).
func DefaultPatterns() []Pattern {
	specs := []string{
		// OpenAI Chat/Completions
		"POST /chat/completions",
		"POST /completions",

		// OpenAI Responses API
		"POST /responses",

		// OpenAI Embeddings
		"POST /embeddings",

		// OpenAI Images
		"POST /images/generations",
		"POST /images/edits",
		"POST /images/variations",

		// OpenAI Audio
		"POST /audio/speech",
		"POST /audio/transcriptions",
		"POST /audio/translations",

		// OpenAI Moderation
		"POST /moderations",

		// OpenAI Assistants/Threads/Runs
		"POST /runs",
		"POST /threads",

		// OpenAI Batches
		"POST /batches",

		// OpenAI Realtime Sessions
		"POST /realtime/sessions",
		"POST /realtime/transcript_sessions",

		// Anthropic Messages
		"POST /messages",

		// Anthropic Batches (also matches OpenAI /v1/messages/batches)
		// This is intentionally separate from the "POST /batches" pattern
		// because "messages/batches" is a distinct 2-segment match.
		"POST /messages/batches",

		// Ollama
		"POST /api/generate",
		"POST /api/chat",
		"POST /api/embeddings",

		// Google Gemini-style (generateContent, streamGenerateContent as suffixes)
		"POST /generateContent",
		"POST /streamGenerateContent",
	}

	var out []Pattern
	for _, s := range specs {
		p, err := Parse(s)
		if err != nil {
			panic("route: invalid default pattern: " + s + ": " + err.Error())
		}
		out = append(out, p)
	}
	return out
}
