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
	"maps"
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

// TestOpencodeApplyHeaders proves the full first-party shape for an opencode
// provider: the impersonation markers and the session are preset-authoritative
// (a client that named itself must not identify itself upstream), the session
// travels only under the opencode-branch name, identity/project/parent forward
// only when present, and disabled is a no-op.
func TestOpencodeApplyHeaders(t *testing.T) {
	in := http.Header{}
	in.Set("User-Agent", "claude-cli/2.1.0 (external, cli)")
	in.Set(HeaderOpencodeClient, "some-other-tool")
	in.Set(HeaderOpencodeRequest, "user-1")
	out := http.Header{}
	OpencodePreset{Enabled: true, Provider: "zen"}.ApplyHeaders(out, in, "sess-1")

	if got := out.Get("User-Agent"); got != DefaultOpencodeUserAgent {
		t.Fatalf("User-Agent = %q, want the first-party value %q", got, DefaultOpencodeUserAgent)
	}
	if got := out.Get(HeaderOpencodeClient); got != DefaultOpencodeClient {
		t.Fatalf("client = %q, want the first-party value %q", got, DefaultOpencodeClient)
	}

	// A foreign client SDK's fingerprint headers are removed from the
	// outbound set when the CLIENT sent them (which is how the surrounding
	// pipeline seeds it), while protocol headers survive.
	inForeign := http.Header{}
	inForeign.Set("X-Stainless-Lang", "js")
	inForeign.Set("X-Stainless-Package", "anthropic")
	inForeign.Set("X-App", "cli")
	outForeign := http.Header{}
	maps.Copy(outForeign, inForeign)
	outForeign.Set("Anthropic-Version", "2023-06-01")
	outForeign.Set("Content-Type", "application/json")
	OpencodePreset{Enabled: true, Provider: "zen"}.ApplyHeaders(outForeign, inForeign, "s")
	for _, key := range []string{"X-Stainless-Lang", "X-Stainless-Package", "X-App"} {
		if got := outForeign.Get(key); got != "" {
			t.Fatalf("%s = %q, want removed", key, got)
		}
	}
	for _, key := range []string{"Anthropic-Version", "Content-Type"} {
		if got := outForeign.Get(key); got == "" {
			t.Fatalf("%s removed, want the protocol header preserved", key)
		}
	}

	// A header the pipeline applied — a custom authentication header is the
	// real case — is NOT in the inbound set and must survive the strip,
	// even when its name is one the preset removes from client traffic.
	// Regression: the strip once ran over the whole outbound map, so
	// `-auth-mode header:X-App -opencode` silently lost the credential.
	outAuth := http.Header{}
	outAuth.Set("X-App", "configured-credential")
	outAuth.Set("X-Stainless-Token", "configured-credential")
	OpencodePreset{Enabled: true, Provider: "zen"}.ApplyHeaders(outAuth, http.Header{}, "s")
	for _, key := range []string{"X-App", "X-Stainless-Token"} {
		if got := outAuth.Get(key); got != "configured-credential" {
			t.Fatalf("%s = %q, want the configured credential preserved", key, got)
		}
	}

	// An explicit override is the way to assert a different first-party
	// install's value.
	outOverride := http.Header{}
	OpencodePreset{Enabled: true, Provider: "zen", UserAgent: "opencode/2.0.3", Client: "tui"}.
		ApplyHeaders(outOverride, in, "sess-1")
	if got := outOverride.Get("User-Agent"); got != "opencode/2.0.3" {
		t.Fatalf("override User-Agent = %q", got)
	}
	if got := outOverride.Get(HeaderOpencodeClient); got != "tui" {
		t.Fatalf("override client = %q", got)
	}
	if got := out.Get(HeaderOpencodeSession); got != "sess-1" {
		t.Fatalf("%s = %q, want sess-1", HeaderOpencodeSession, got)
	}
	// The pinned first-party client emits x-session-affinity and X-Session-Id
	// only for providers whose id does NOT start with "opencode". On an
	// opencode mount it sends x-opencode-session instead, so the preset must
	// not emit the pair: doing so is a header combination no real client ever
	// sends.
	for _, key := range []string{HeaderSessionAffinity, HeaderSessionID} {
		if got := out.Get(key); got != "" {
			t.Fatalf("%s = %q, want absent: it belongs to the non-opencode branch", key, got)
		}
	}
	if got := out.Get(HeaderOpencodeRequest); got != "user-1" {
		t.Fatalf("request = %q, want forwarded", got)
	}
	if got := out.Get(HeaderOpencodeProject); got != "" {
		t.Fatalf("project = %q, want absent (never fabricated)", got)
	}

	// A client that uses the non-opencode session names must not have them
	// forwarded: deleting them (rather than merely not setting them) is what
	// stops an inbound copy from reintroducing the union.
	inAffinity := http.Header{}
	inAffinity.Set(HeaderSessionAffinity, "aff")
	inAffinity.Set(HeaderSessionID, "sid")
	outAffinity := http.Header{}
	maps.Copy(outAffinity, inAffinity)
	OpencodePreset{Enabled: true, Provider: "zen"}.ApplyHeaders(outAffinity, inAffinity, "sess-1")
	for _, key := range []string{HeaderSessionAffinity, HeaderSessionID} {
		if got := outAffinity.Get(key); got != "" {
			t.Fatalf("%s = %q, want removed: the client sent it but a real opencode client would not", key, got)
		}
	}
	// The value is still carried, under the name the opencode branch uses.
	if got := outAffinity.Get(HeaderOpencodeSession); got != "sess-1" {
		t.Fatalf("session = %q, want the resolved value carried as %s", got, HeaderOpencodeSession)
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

// TestPresetManagedCoversEveryHeaderThePresetTouches pins the credential
// invariant in every direction, and reads the preset's own registration list
// so the two cannot drift apart. The failure it exists to prevent is silent:
// the preset runs after authentication, so a name it writes or removes
// destroys the secret auth just attached, and the client sees a bare 401 with
// nothing logged. Each direction is made non-vacuous on purpose — an earlier
// version of this test modelled the client as having sent nothing, so the
// foreign-SDK strip never fired and those candidates could not have failed.
func TestPresetManagedCoversEveryHeaderThePresetTouches(t *testing.T) {
	const sentinel = "sentinel-value"
	preset := OpencodePreset{Enabled: true, Provider: "zen"}

	// Every registered preset header must be preset-managed, must be one the
	// preset really touches (so the list cannot rot), and must be recognised
	// in any spelling.
	for _, name := range presetHeaderNames {
		out := http.Header{}
		out.Set(name, sentinel)
		preset.ApplyHeaders(out, http.Header{}, "session-value")
		if out.Get(name) == sentinel {
			t.Errorf("presetHeaderNames lists %q but the preset does not touch it: the list has drifted", name)
		}
		if !PresetManagedHeaderName(name) {
			t.Errorf("PresetManagedHeaderName(%q) = false, want true: the preset manages it", name)
		}
		if !PresetManagedHeaderName(strings.ToLower(name)) {
			t.Errorf("PresetManagedHeaderName(%q) = false, want true (case-insensitive)", strings.ToLower(name))
		}
	}

	// The foreign-SDK strip only fires on what the CLIENT sent, so the client
	// must be modelled as having sent it or this half is vacuous.
	for _, name := range []string{"X-App", "X-Stainless-Lang", "X-Stainless-Token"} {
		in := http.Header{}
		in.Set(name, sentinel)
		out := http.Header{}
		out.Set(name, sentinel)
		preset.ApplyHeaders(out, in, "session-value")
		if out.Get(name) == sentinel {
			t.Errorf("%q survived: the client sent it and a real opencode client would not", name)
		}
		if !PresetManagedHeaderName(name) {
			t.Errorf("PresetManagedHeaderName(%q) = false, want true", name)
		}
	}

	// Names the preset does not manage stay usable for a credential, so the
	// rule cannot harden into a blanket ban.
	for _, name := range []string{
		"X-Custom-Cred", "X-Request-Id", "Idempotency-Key", "Content-Type",
		"Accept", "Anthropic-Version", "X-Forwarded-For", "Cookie",
	} {
		if PresetManagedHeaderName(name) {
			t.Errorf("PresetManagedHeaderName(%q) = true, want false: the preset does not manage it", name)
		}
	}

	// A disabled preset touches nothing, so those names remain legal.
	disabled := OpencodePreset{Provider: "zen"}
	for _, name := range append(append([]string{}, presetHeaderNames...), "X-App") {
		out := http.Header{}
		out.Set(name, sentinel)
		disabled.ApplyHeaders(out, http.Header{}, "session-value")
		if out.Get(name) != sentinel {
			t.Errorf("disabled preset modified %q", name)
		}
	}
}
