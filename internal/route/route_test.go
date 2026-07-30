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

package route

import (
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
		limit   int
	}{
		{"POST /v1/messages", false, 0},
		{"POST /v1/messages:4", false, 4},
		{"POST /v1/messages:0", false, 0}, // explicit zero limit
		{"  POST   /v1/messages  ", false, 0},
		{"/onlypath", true, 0},
		{"POST", true, 0},
		{"POST  ", true, 0},
		{"GET /", false, 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			p, err := Parse(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.input, err)
			}
			if tt.limit > 0 && p.Limit != tt.limit {
				t.Errorf("Parse(%q).Limit = %d, want %d", tt.input, p.Limit, tt.limit)
			}
		})
	}
}

func TestParseWithLimit(t *testing.T) {
	p, err := Parse("POST /v1/messages:8")
	if err != nil {
		t.Fatal(err)
	}
	if p.Limit != 8 {
		t.Errorf("Limit = %d, want 8", p.Limit)
	}
	if p.Method != "POST" {
		t.Errorf("Method = %q, want POST", p.Method)
	}
	if p.Raw == "" {
		t.Error("Raw should be set")
	}
}

func TestParse_AtInPath(t *testing.T) {
	p, err := Parse("POST /users/@me/profile:3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Limit != 3 {
		t.Errorf("Limit = %d, want 3", p.Limit)
	}
	if p.Group != "" {
		t.Errorf("Group = %q, want empty", p.Group)
	}
	wantSegments := []string{"users", "@me", "profile"}
	if len(p.Segments) != len(wantSegments) {
		t.Fatalf("Segments = %v, want %v", p.Segments, wantSegments)
	}
	for i, s := range wantSegments {
		if p.Segments[i] != s {
			t.Errorf("Segments[%d] = %q, want %q", i, p.Segments[i], s)
		}
	}
	if !p.Match("POST", "/api/users/@me/profile") {
		t.Error("should match /api/users/@me/profile")
	}
	if p.Match("POST", "/users/me/profile") {
		t.Error("should not match /users/me/profile")
	}
}

func TestParse_AtGroup(t *testing.T) {
	p, err := Parse("POST /users/me:3@users")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Limit != 3 {
		t.Errorf("Limit = %d, want 3", p.Limit)
	}
	if p.Group != "users" {
		t.Errorf("Group = %q, want users", p.Group)
	}
	if len(p.Segments) != 2 || p.Segments[0] != "users" || p.Segments[1] != "me" {
		t.Errorf("Segments = %v, want [users me]", p.Segments)
	}
}

func TestParse_ZeroLimit(t *testing.T) {
	p, err := Parse("POST /v1/chat/completions:0")
	if err != nil {
		t.Fatal(err)
	}
	if p.Limit != 0 {
		t.Errorf("expected Limit=0, got %d", p.Limit)
	}
}

func TestPattern_Match(t *testing.T) {
	p, _ := Parse("POST /v1/messages")
	if !p.Match("POST", "/v1/messages") {
		t.Error("should match")
	}
	if p.Match("GET", "/v1/messages") {
		t.Error("should not match different method")
	}
	// Suffix semantics: a sub-resource of the matched path is not limited.
	if p.Match("POST", "/v1/messages/count_tokens") {
		t.Error("should not match sub-resource")
	}
}

func TestMatcher(t *testing.T) {
	m := NewMatcher(nil)
	m.AddPattern(MustParse("POST /v1/messages"))
	if !m.IsLimited("POST", "/v1/messages") {
		t.Error("expected limited")
	}
	if m.IsLimited("GET", "/v1/messages") {
		t.Error("GET should not be limited")
	}
}

func MustParse(s string) Pattern {
	p, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return p
}

func TestMatcher_Multiple(t *testing.T) {
	m := NewMatcher(nil)
	m.AddPattern(MustParse("POST /v1/messages"))
	m.AddPattern(MustParse("POST /v1/responses"))
	if !m.IsLimited("POST", "/v1/responses") {
		t.Error("should be limited")
	}
	if m.IsLimited("GET", "/health") {
		t.Error("should not match")
	}
}

func TestParse_Invalid(t *testing.T) {
	tests := []string{
		"POST",
		"",
		" /path",
	}
	for _, input := range tests {
		_, err := Parse(input)
		if err == nil {
			t.Errorf("expected error for %q", input)
		}
	}
}

func TestParse_LimitSuffix(t *testing.T) {
	p, err := Parse("GET /api/v2:5")
	if err != nil {
		t.Fatal(err)
	}
	if p.Limit != 5 {
		t.Errorf("Limit = %d, want 5", p.Limit)
	}
	if p.Method != "GET" {
		t.Errorf("Method = %q, want GET", p.Method)
	}
}

func TestFuzzy_Basic(t *testing.T) {
	p := MustParse("POST /chat/completions")

	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{"POST", "/v1/chat/completions", true},
		{"POST", "/openai/deployments/gpt-4/chat/completions", true},
		{"POST", "/api/v2/chat/completions", true},
		{"POST", "/chat/completions", true},
		{"GET", "/v1/chat/completions", false},
		{"POST", "/v1/chat", false},
		{"POST", "/v1/models", false},
	}

	for _, tc := range cases {
		got := p.Match(tc.method, tc.path)
		if got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestFuzzy_AllProviders(t *testing.T) {
	m := NewMatcher(DefaultPatterns())

	limited := []struct {
		method string
		path   string
		desc   string
	}{
		// OpenAI
		{"POST", "/v1/chat/completions", "OpenAI chat completions"},
		{"POST", "/v1/completions", "OpenAI legacy completions"},
		{"POST", "/v1/responses", "OpenAI responses API"},
		{"POST", "/v1/embeddings", "OpenAI embeddings"},
		{"POST", "/v1/images/generations", "OpenAI image gen"},
		{"POST", "/v1/images/edits", "OpenAI image edit"},
		{"POST", "/v1/images/variations", "OpenAI image variation"},
		{"POST", "/v1/audio/speech", "OpenAI TTS"},
		{"POST", "/v1/audio/transcriptions", "OpenAI whisper"},
		{"POST", "/v1/audio/translations", "OpenAI translation"},
		{"POST", "/v1/moderations", "OpenAI moderation"},
		{"POST", "/v1/runs", "OpenAI assistant run"},
		{"POST", "/v1/threads", "OpenAI thread create"},
		{"POST", "/v1/batches", "OpenAI batch"},
		{"POST", "/v1/realtime/sessions", "OpenAI realtime session"},
		{"POST", "/v1/realtime/transcript_sessions", "OpenAI transcript session"},

		// Anthropic
		{"POST", "/v1/messages", "Anthropic messages"},
		{"POST", "/v1/messages/batches", "Anthropic message batches"},

		// OpenAI / Azure Assistants
		{"POST", "/v1/threads/thread_1/runs/run_1/submit_tool_outputs", "OpenAI submit tool outputs"},
		{"POST", "/api/threads/thread_1/runs/run_1/submit_tool_outputs", "Azure submit tool outputs"},

		// Ollama
		{"POST", "/api/generate", "Ollama generate"},
		{"POST", "/api/chat", "Ollama chat"},
		{"POST", "/api/embeddings", "Ollama embeddings"},

		// Google Gemini
		{"POST", "/v1/models/gemini-pro:generateContent", "Gemini generate"},
		{"POST", "/v1/models/gemini-pro:streamGenerateContent", "Gemini stream"},

		// Azure OpenAI
		{"POST", "/openai/deployments/my-deployment/chat/completions", "Azure chat"},
		{"POST", "/openai/deployments/my-deployment/embeddings", "Azure embeddings"},

		// Arbitrary prefix
		{"POST", "/proxy/api/v1/chat/completions", "proxied OpenAI"},
		{"POST", "/gateway/v2/responses", "gateway responses"},

		// Session endpoints
		{"POST", "/v1/realtime/sessions", "realtime session create"},
		{"POST", "/v1/realtime/transcript_sessions", "transcript session create"},
	}

	for _, tc := range limited {
		if !m.IsLimited(tc.method, tc.path) {
			t.Errorf("should be limited: %s (%s %s)", tc.desc, tc.method, tc.path)
		}
	}
}

func TestFuzzy_LightweightNotLimited(t *testing.T) {
	m := NewMatcher(DefaultPatterns())

	passthrough := []struct {
		method string
		path   string
		desc   string
	}{
		{"GET", "/v1/models", "list models"},
		{"GET", "/v1/models/gpt-4", "get model"},
		{"GET", "/health", "health check"},
		{"GET", "/status", "status check"},
		{"GET", "/v1/responses/resp_123", "poll response"},
		{"GET", "/v1/batches/batch_123", "get batch status"},
		{"GET", "/v1/threads/thread_123", "get thread"},
		{"GET", "/v1/threads/thread_123/messages", "list thread messages"},
		{"HEAD", "/v1/models", "HEAD models"},
		{"OPTIONS", "/v1/chat/completions", "CORS preflight"},
		// Sub-paths of limited endpoints are now unlimited unless they
		// have their own default pattern.
		{"POST", "/v1/messages/count_tokens", "Anthropic count_tokens"},
		{"POST", "/v1/batches/batch_1/cancel", "OpenAI batch cancel"},
		{"POST", "/v1/messages/batches/mb_1/cancel", "Anthropic batch cancel"},
		{"POST", "/v1/responses/resp_1/cancel", "OpenAI response cancel"},
		{"POST", "/v1/threads/thread_1/runs/run_1/cancel", "OpenAI run cancel"},
		{"POST", "/v1/chat/completions/cmpl_1", "OpenAI chat completion sub-resource"},
	}

	for _, tc := range passthrough {
		if m.IsLimited(tc.method, tc.path) {
			t.Errorf("should NOT be limited: %s (%s %s)", tc.desc, tc.method, tc.path)
		}
	}
}

func TestDefaultPatterns_NonZero(t *testing.T) {
	patterns := DefaultPatterns()
	if len(patterns) == 0 {
		t.Fatal("DefaultPatterns should return at least one pattern")
	}
	for _, p := range patterns {
		if p.Method != "POST" {
			t.Errorf("default pattern %q should be POST, got %s", p.Raw, p.Method)
		}
	}
}

func TestMatcher_FindMatch(t *testing.T) {
	m := NewMatcher(DefaultPatterns())

	pat := m.FindMatch("POST", "/v1/chat/completions")
	if pat == nil {
		t.Fatal("expected to find match for /v1/chat/completions")
	}
	if pat.Method != "POST" {
		t.Errorf("method = %q, want POST", pat.Method)
	}
	if pat.Raw != "POST /chat/completions" {
		t.Errorf("raw = %q, want POST /chat/completions", pat.Raw)
	}

	pat = m.FindMatch("GET", "/v1/models")
	if pat != nil {
		t.Errorf("expected nil for GET /v1/models, got %v", pat)
	}

	pat = m.FindMatch("POST", "/openai/deployments/gpt4/chat/completions")
	if pat == nil {
		t.Error("expected to find match for Azure-style path")
	}

	// /v1/messages/batches now matches the more specific POST /messages/batches
	// pattern because longer patterns are placed before shorter ones in DefaultPatterns.
	pat = m.FindMatch("POST", "/v1/messages/batches")
	if pat == nil {
		t.Fatal("expected to find match for /v1/messages/batches")
	}
	if pat.Raw != "POST /messages/batches" {
		t.Errorf("expected specific pattern POST /messages/batches, got %q", pat.Raw)
	}
}

func TestFuzzy_ColonDesyncBypass(t *testing.T) {
	m := NewMatcher(DefaultPatterns())

	cases := []struct {
		method string
		path   string
		want   bool
	}{
		// A ':' segment should not enable '..' to pop a real segment, so these
		// remain limited (proxy and upstream agree on the normalized path).
		{"POST", "/v1/messages/:/..", true},
		{"POST", "/v1/messages/batches/:/..", true},

		// A literal ':' segment is still just a directory name upstream; the
		// proxy must resolve it the same way before matching.
		{"POST", "/v1/messages/:foo/..", true},

		// Without enough preceding real segments, '..' resolves to a shorter
		// path and the request should NOT match the protected endpoint.
		{"POST", "/messages/..", false},
		{"POST", "/v1/messages/../../", false},

		// DoS vector: upstream treats '..:..' as a literal segment and returns
		// 404, but the old proxy code normalized it and consumed a token.
		{"POST", "/v1/messages/batches/fake1/fake2/..:..", false},
		{"POST", "/v1/messages/batches/fake1/fake2/..:..:..", false},
		{"POST", "/v1/messages/foo/..:..", false},

		// Gemini-style colon suffixes must still match after the structural fix.
		{"POST", "/v1/models/gemini-pro:generateContent", true},
		{"POST", "/v1/models/gemini-pro:streamGenerateContent", true},
	}

	for _, tc := range cases {
		got := m.IsLimited(tc.method, tc.path)
		if got != tc.want {
			t.Errorf("IsLimited(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestFuzzy_SuffixSemantics(t *testing.T) {
	p := MustParse("POST /v1/chat")
	if p.Match("POST", "/v1/chat/completions") {
		t.Error("prefix pattern should not match sub-resource")
	}

	m := NewMatcher(DefaultPatterns())
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		// Still limited (anchoring must not break these).
		{"POST", "/v1/messages/", true},                                       // trailing slash
		{"POST", "/v1//messages", true},                                       // empty segment
		{"POST", "/v1/threads/thread_1/runs", true},                           // sub-resource that IS the endpoint
		{"POST", "/v1/threads/thread_1/runs/run_1/submit_tool_outputs", true}, // after fix

		// Newly unlimited (lock in intended narrowing).
		{"POST", "/v1/batches/batch_1/cancel", false},
		{"POST", "/v1/chat/completions/cmpl_1", false},
		{"POST", "/v1/messages/batches/mb_1/cancel", false},

		// Degenerate.
		{"POST", "", false},
		{"POST", "/", false},

		// Dot-segment normalization: should behave like /v1/messages and remain limited,
		// and count_tokens should stay unlimited.
		{"POST", "/v1/messages/.", true},
		{"POST", "/v1/messages/foo/../count_tokens", false},
	}

	for _, tc := range cases {
		got := m.IsLimited(tc.method, tc.path)
		if got != tc.want {
			t.Errorf("IsLimited(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestDefaultPatterns_AllParse(t *testing.T) {
	patterns := DefaultPatterns()
	for _, p := range patterns {
		if p.Method == "" {
			t.Errorf("empty method in pattern: %q", p.Raw)
		}
		if len(p.Segments) == 0 {
			t.Errorf("no segments in pattern: %q", p.Raw)
		}
		if p.Raw == "" {
			t.Error("empty Raw in pattern")
		}
	}
}

func TestMatchesSuffixSegments(t *testing.T) {
	tests := []struct {
		haystack []string
		needle   []string
		want     bool
	}{
		{[]string{"v1", "chat", "completions"}, []string{"chat", "completions"}, true},
		{[]string{"v1", "chat", "completions"}, []string{"v1", "chat"}, false}, // only suffix matches
		{[]string{"v1", "chat", "completions"}, []string{"v1", "completions"}, false},
		{[]string{"v1", "messages"}, []string{"messages"}, true},
		{[]string{"api", "v1", "messages"}, []string{"messages"}, true},
		{[]string{"messages"}, []string{"messages"}, true},
		{[]string{}, []string{"messages"}, false},
		{[]string{"messages"}, []string{}, true},
		{[]string{}, []string{}, true},
	}

	for _, tt := range tests {
		got := matchesSuffixSegments(tt.haystack, tt.needle)
		if got != tt.want {
			t.Errorf("matchesSuffixSegments(%v, %v) = %v, want %v", tt.haystack, tt.needle, got, tt.want)
		}
	}
}

func TestFuzzy_CaseInsensitive(t *testing.T) {
	p := MustParse("POST /chat/completions")
	if !p.Match("post", "/v1/chat/completions") {
		t.Error("should match with lowercase method")
	}
	if !p.Match("POST", "/v1/Chat/Completions") {
		t.Error("should match with mixed-case path segments")
	}
}
