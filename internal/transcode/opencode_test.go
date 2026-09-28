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

package transcode

import (
	"net/http"
	"strings"
	"testing"
)

func enabledPreset() OpencodePreset {
	return OpencodePreset{Enabled: true, Provider: "zen"}
}

// TestOpencodeSessionPrecedence proves the outbound session prefers
// inbound identity headers in order and falls back to the caller key.
func TestOpencodeSessionPrecedence(t *testing.T) {
	p := enabledPreset()
	cases := []struct {
		name     string
		headers  map[string]string
		fallback string
		want     string
	}{
		{"direct wins", map[string]string{
			HeaderOpencodeSession: "sess",
			HeaderSessionAffinity: "aff",
			"X-Session-Id":        "sid",
		}, "fb", "sess"},
		{"affinity second", map[string]string{
			HeaderSessionAffinity: "aff",
			"X-Session-Id":        "sid",
		}, "fb", "aff"},
		{"session-id third", map[string]string{"X-Session-Id": "sid"}, "fb", "sid"},
		{"fallback", nil, "fb", "fb"},
		{"blank inbound ignored", map[string]string{HeaderOpencodeSession: "  "}, "fb", "fb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := http.Header{}
			for k, v := range tc.headers {
				in.Set(k, v)
			}
			if got := p.ResolveSession(in, tc.fallback); got != tc.want {
				t.Fatalf("ResolveSession = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOpencodeApplyHeaders proves the full first-party shape: inbound
// identity preserved, defaults fill gaps, identity/project/parent forward
// only when present, and disabled is a no-op.
func TestOpencodeApplyHeaders(t *testing.T) {
	in := http.Header{}
	in.Set("User-Agent", "opencode/prod/9.9.9/opencode")
	in.Set(HeaderOpencodeRequest, "user-1")
	out := http.Header{}
	OpencodePreset{Enabled: true, Provider: "zen"}.ApplyHeaders(out, in, "sess-1")

	if got := out.Get("User-Agent"); got != "opencode/prod/9.9.9/opencode" {
		t.Fatalf("User-Agent = %q, want inbound preserved", got)
	}
	for _, key := range []string{HeaderOpencodeSession, HeaderSessionAffinity, HeaderSessionID} {
		if got := out.Get(key); got != "sess-1" {
			t.Fatalf("%s = %q, want sess-1", key, got)
		}
	}
	if got := out.Get(HeaderOpencodeClient); got != DefaultOpencodeClient {
		t.Fatalf("client = %q, want default %q", got, DefaultOpencodeClient)
	}
	if got := out.Get(HeaderOpencodeRequest); got != "user-1" {
		t.Fatalf("request = %q, want forwarded", got)
	}
	if got := out.Get(HeaderOpencodeProject); got != "" {
		t.Fatalf("project = %q, want absent (never fabricated)", got)
	}

	// Defaults fill gaps.
	out2 := http.Header{}
	OpencodePreset{Enabled: true, Provider: "zen"}.ApplyHeaders(out2, http.Header{}, "s")
	if got := out2.Get("User-Agent"); got != DefaultOpencodeUserAgent {
		t.Fatalf("User-Agent = %q, want default", got)
	}

	// Disabled changes nothing.
	out3 := http.Header{}
	OpencodePreset{}.ApplyHeaders(out3, in, "s")
	if len(out3) != 0 {
		t.Fatalf("disabled preset wrote %v", out3)
	}
}

// TestOpencodeDerivationStability proves conversation keys are stable for
// one history, distinct across conversations and providers, and that the
// client key ignores the connection port.
func TestOpencodeDerivationStability(t *testing.T) {
	a := DeriveConversationKey("zen", "m", "hello")
	if a != DeriveConversationKey("zen", "m", "hello") {
		t.Fatal("conversation key unstable")
	}
	if b := DeriveConversationKey("zen", "m", "other"); a == b {
		t.Fatal("different text must give a different key")
	}
	if b := DeriveConversationKey("go", "m", "hello"); a == b {
		t.Fatal("different providers must give different keys")
	}
	c := DeriveClientKey("zen", "10.0.0.5:43210")
	if c != DeriveClientKey("zen", "10.0.0.5:9999") {
		t.Fatal("client key must ignore the ephemeral port")
	}
	if c == DeriveClientKey("zen", "10.0.0.6:43210") {
		t.Fatal("different clients must give different keys")
	}
	if len(a) != 32 || len(c) != 32 {
		t.Fatalf("keys must be 32 hex chars, got %q %q", a, c)
	}
}

// TestFirstUserText proves tolerant extraction across dialects and the
// empty result on malformed input.
func TestFirstUserText(t *testing.T) {
	messages := `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":"what is up"}]}]}`
	if got := FirstUserText(NativeMessages, []byte(messages)); got != "what is up" {
		t.Fatalf("messages text = %q", got)
	}
	chat := `{"model":"m","messages":[{"role":"user","content":"plain hi"}]}`
	if got := FirstUserText(NativeChat, []byte(chat)); got != "plain hi" {
		t.Fatalf("chat text = %q", got)
	}
	responses := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"do this"}]}]}`
	if got := FirstUserText(NativeResponses, []byte(responses)); got != "do this" {
		t.Fatalf("responses text = %q", got)
	}
	if got := FirstUserText(NativeMessages, []byte(`{"model":`)); got != "" {
		t.Fatalf("malformed = %q, want empty", got)
	}
	if got := FirstUserText(NativeMessages, []byte(`{"model":"m","messages":[]}`)); got != "" {
		t.Fatalf("no user turn = %q, want empty", got)
	}
	if !strings.HasPrefix(string(DefaultOpencodeUserAgent), "opencode/") {
		t.Fatalf("default UA = %q, want the first-party shape", DefaultOpencodeUserAgent)
	}
}
