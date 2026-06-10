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
		{"POST /v1/messages:0", false, 0}, // :0 treated as no limit
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

func TestParse_NegativeLimit(t *testing.T) {
	p, err := Parse("POST /v1/messages:0")
	if err != nil {
		t.Fatal(err)
	}
	if p.Limit != 0 {
		t.Errorf("Limit = %d, want 0", p.Limit)
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

	pat = m.FindMatch("GET", "/v1/models")
	if pat != nil {
		t.Errorf("expected nil for GET /v1/models, got %v", pat)
	}

	pat = m.FindMatch("POST", "/openai/deployments/gpt4/chat/completions")
	if pat == nil {
		t.Error("expected to find match for Azure-style path")
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

func TestContainsConsecutive(t *testing.T) {
	tests := []struct {
		haystack []string
		needle   []string
		want     bool
	}{
		{[]string{"v1", "chat", "completions"}, []string{"chat", "completions"}, true},
		{[]string{"v1", "chat", "completions"}, []string{"v1", "chat"}, true},
		{[]string{"v1", "chat", "completions"}, []string{"v1", "completions"}, false},
		{[]string{"v1", "messages"}, []string{"messages"}, true},
		{[]string{"api", "v1", "messages"}, []string{"messages"}, true},
		{[]string{"messages"}, []string{"messages"}, true},
		{[]string{}, []string{"messages"}, false},
		{[]string{"messages"}, []string{}, true},
		{[]string{}, []string{}, true},
	}

	for _, tt := range tests {
		got := containsConsecutive(tt.haystack, tt.needle)
		if got != tt.want {
			t.Errorf("containsConsecutive(%v, %v) = %v, want %v", tt.haystack, tt.needle, got, tt.want)
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
