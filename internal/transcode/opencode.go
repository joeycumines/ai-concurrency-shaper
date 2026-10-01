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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strings"
)

// First-party opencode header names. The pinned client emits these as two
// alternative sets: an opencode-family provider gets the x-opencode-* names,
// while every other provider gets the session-affinity pair instead. The
// preset reproduces the opencode branch on every outbound request, and treats
// the affinity pair as names it removes.
const (
	HeaderOpencodeSession = "X-Opencode-Session"
	HeaderOpencodeClient  = "X-Opencode-Client"
	HeaderOpencodeProject = "X-Opencode-Project"
	HeaderOpencodeRequest = "X-Opencode-Request"
	HeaderSessionAffinity = "X-Session-Affinity"
	HeaderSessionID       = "X-Session-Id"
	HeaderParentSessionID = "X-Parent-Session-Id"
)

// presetHeaderNames is the single registration point for every header
// ApplyHeaders writes or removes. PresetManagedHeaderName is built from it and
// the behavioural test reads it, so the guard against a credential being
// silently destroyed and the preset's disposition table cannot drift apart on
// the side a test can observe.
//
// KNOWN LIMITS, stated because they have already bitten. Go cannot reflect over
// the writes in ApplyHeaders, so a header added to the preset WITHOUT being
// listed here is invisible to every test: adding one means adding it here, and
// it lives beside the constants for that reason rather than being restated in
// the validation package, where it drifted once already. The drift check in the
// test catches a listed name the preset no longer touches; it cannot catch a
// name removed from this list, because a shorter list simply asserts less.
var presetHeaderNames = []string{
	"User-Agent",
	HeaderOpencodeSession, HeaderOpencodeClient, HeaderOpencodeProject,
	HeaderOpencodeRequest, HeaderSessionAffinity, HeaderSessionID,
	HeaderParentSessionID,
}

const (
	// DefaultOpencodeUserAgent is the pinned first-party User-Agent for
	// the released-stable CLI shape `opencode/<version>`, observed
	// 2026-09-28 against npm opencode-ai@1.18.33 (the published stable
	// release) and its request-preparation source. The in-development
	// 2.x line emits `opencode/<channel>/<version>/<name>`; the exact
	// string is version-specific, so an operator matching a specific
	// install overrides it with -opencode-user-agent. Re-pin per the
	// recorded rule: read the installed release's own User-Agent and
	// update this default.
	DefaultOpencodeUserAgent = "opencode/1.18.33"
	// DefaultOpencodeClient is the first-party client attribution.
	DefaultOpencodeClient = "cli"
)

// conversationKeyDomain separates derived session keys from every other
// hash in the system.
const conversationKeyDomain = "opencode-conversation-v1"

// OpencodePreset configures first-party opencode header emission for one
// provider mount. Disabled (zero value) is a no-op: nothing is set, read,
// or derived. Enabled, every outbound request carries the full header set
// the upstream would see from a real opencode client, so a client that
// named itself cannot identify itself through the impersonation markers.
// Header values are never logged.
type OpencodePreset struct {
	Enabled bool
	// UserAgent and Client are the first-party values the preset asserts
	// upstream. They default to the pinned values when empty and are
	// overridden only by these flags, never by the inbound request.
	UserAgent string
	Client    string
	// Provider scopes derived session keys to the mount. Set from the
	// provider's effective name at resolve time.
	Provider string
}

// userAgent resolves the outbound User-Agent. The preset exists to make the
// upstream see a first-party client, so its value is authoritative: a client
// that sent its own User-Agent would otherwise identify itself verbatim to
// opencode. The configured value wins over the pinned default, and inbound is
// never forwarded.
func (p OpencodePreset) userAgent() string {
	if v := strings.TrimSpace(p.UserAgent); v != "" {
		return v
	}
	return DefaultOpencodeUserAgent
}

// client resolves the outbound client attribution the same way: the preset
// asserts the first-party value rather than echoing a client that named
// itself.
func (p OpencodePreset) client() string {
	if v := strings.TrimSpace(p.Client); v != "" {
		return v
	}
	return DefaultOpencodeClient
}

// ResolveSession returns the outbound session value: the first present
// inbound identity header (x-opencode-session, x-session-affinity,
// X-Session-Id), else the caller-derived stable fallback. Callers always
// supply a synthesized fallback, so the preset always emits. The affinity
// names are accepted here as inbound tolerance only — a client that uses them
// still gets its value carried, but outbound it travels as
// x-opencode-session, which is the only session name the first-party client
// sends to an opencode provider.
func (p OpencodePreset) ResolveSession(in http.Header, fallback string) string {
	for _, key := range []string{HeaderOpencodeSession, HeaderSessionAffinity, HeaderSessionID} {
		if v := strings.TrimSpace(in.Get(key)); v != "" {
			return v
		}
	}
	return fallback
}

// ApplyHeaders sets the full first-party header set on the outbound
// request. It runs after authentication so the mock never clobbers
// credentials and authentication never strips the mock.
//
// The first-party client emits one of TWO header sets, chosen by whether the
// model's provider id starts with "opencode": an opencode provider gets
// x-opencode-session (plus x-opencode-client and the opencode User-Agent),
// while every other provider gets x-session-affinity and X-Session-Id
// instead. This preset exists to impersonate the client on an opencode mount,
// so it emits that branch and only that branch. Emitting both would produce a
// combination no real client ever sends, which is itself a signature the
// upstream could key on.
//
// Three dispositions:
//   - Preset-authoritative (overwrite inbound): the impersonation markers
//     User-Agent and x-opencode-client, plus the session header, which carries
//     the value the caller resolved (the client's own when it sent one, else a
//     derived key) so the gateway always sees one stable value.
//   - Forward-only (never fabricated): x-opencode-request,
//     x-opencode-project, x-parent-session-id — they name the client's own
//     conversation and identity, so they pass through when present and are
//     removed when absent.
//   - Removed: x-session-affinity and X-Session-Id (the non-opencode branch,
//     which a real client would not send here) are deleted unconditionally, and
//     the headers a different client SDK uses to identify itself are deleted
//     when the client sent them — which would otherwise reveal the actual
//     client. The foreign-SDK strip is scoped to the inbound set on purpose:
//     only headers the client actually sent are removed, never a header the
//     pipeline applied. (The unconditional affinity deletion is safe for the
//     same reason: choosing a preset-managed name as the credential header is
//     refused at config time by PresetManagedHeaderName.) Protocol headers the
//     dialect requires (content-type, accept, anthropic-version,
//     anthropic-beta) are untouched.
func (p OpencodePreset) ApplyHeaders(out, in http.Header, session string) {
	if !p.Enabled {
		return
	}
	out.Set("User-Agent", p.userAgent())
	out.Set(HeaderOpencodeClient, p.client())
	for name := range out {
		// Only what the CLIENT sent identifies the client. A header the
		// pipeline applied — a custom authentication header above all — is
		// absent from the inbound set, so scoping the strip to the inbound
		// set keeps a configured credential intact. Note this scopes the
		// PRESET: credential stripping runs earlier and separately, and it
		// removes anthropic-version and anthropic-beta on any mount with a
		// configured credential, on the transparent path too. (Choosing a managed
		// name as the credential header is refused at config time by
		// PresetManagedHeaderName.) The inbound/outbound maps are both
		// canonicalized by net/http and by Set/Del, so the ranged key and
		// the Del target agree; revisit if a third call site ever hands
		// over a map written with raw map keys.
		if !foreignClientHeader(name) {
			continue
		}
		if _, fromClient := in[http.CanonicalHeaderKey(name)]; fromClient {
			out.Del(name)
		}
	}
	if session != "" {
		out.Set(HeaderOpencodeSession, session)
	} else {
		out.Del(HeaderOpencodeSession)
	}
	// x-session-affinity and X-Session-Id belong to the client's NON-opencode
	// branch, so a real client never sends them to an opencode provider.
	// Deleting rather than setting them also stops an inbound copy (a client
	// that happens to use those names) from passing through and reintroducing
	// the union. Their values are still honoured when resolving the session.
	out.Del(HeaderSessionAffinity)
	out.Del(HeaderSessionID)
	for _, key := range []string{HeaderOpencodeRequest, HeaderOpencodeProject, HeaderParentSessionID} {
		if v := strings.TrimSpace(in.Get(key)); v != "" {
			out.Set(key, v)
		} else {
			out.Del(key)
		}
	}
}

// foreignClientHeader reports whether a header identifies a client SDK other
// than opencode. The official Anthropic/OpenAI SDKs stamp x-stainless-*
// fingerprints, and the Anthropic console stamps x-app; a real opencode
// client (built on the AI SDK) sends neither, so leaving them would let the
// upstream distinguish the actual client from opencode.
func foreignClientHeader(name string) bool {
	lower := strings.ToLower(name)
	return (strings.HasPrefix(lower, "x-stainless-") || lower == "x-app")
}

// DeriveConversationKey returns a stable opaque session key from the
// request's own stable fields: provider scope, model, and first user text.
// Full histories resend every turn, so the first user turn identifies the
// conversation across tool calls without tracking state. Volatile and
// tool-call content never enters the hash.
func DeriveConversationKey(provider, model, firstUserText string) string {
	return deriveSessionKey(provider, model, firstUserText)
}

// DeriveClientKey returns a stable opaque session key from the client
// address for paths that cannot inspect the body. The port is stripped:
// it varies per connection while the client stays the same.
func DeriveClientKey(provider, remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return deriveSessionKey(provider, strings.TrimSpace(host))
}

// deriveSessionKey is the one key-construction stanza: sha256 over the
// domain separator plus the caller's fields joined on NUL, hex-encoded and
// cut to 32 characters. Every session key goes through it, so the
// domain-separation invariant lives in exactly one place.
func deriveSessionKey(parts ...string) string {
	fields := append([]string{conversationKeyDomain}, parts...)
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return hex.EncodeToString(sum[:])[:32]
}

// clientProtocolToNative maps a transcode client protocol to the document
// dialect used for first-user-text extraction.
func clientProtocolToNative(protocol ClientProtocol) NativeProtocol {
	if protocol == ClientMessages {
		return NativeMessages
	}
	return NativeResponses
}

// FirstUserText extracts the first user-role text from a client document
// for session-key derivation. It is tolerant by design: any shape mismatch
// yields "", and callers fall back to the client-derived key. Text is
// capped so adversarial bodies cannot inflate hashing work.
func FirstUserText(protocol NativeProtocol, body []byte) string {
	const maxText = 8192
	var doc struct {
		Messages []json.RawMessage `json:"messages"`
		Input    json.RawMessage   `json:"input"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ""
	}
	switch protocol {
	case NativeResponses:
		if text := firstResponsesUserText(doc.Input); text != "" {
			return truncateRunes(text, maxText)
		}
		return truncateRunes(firstChatUserText(doc.Messages), maxText)
	default:
		return truncateRunes(firstChatUserText(doc.Messages), maxText)
	}
}

// firstChatUserText reads messages-shaped arrays (messages and chat
// dialects): the first role=user entry, string content verbatim or the
// concatenation of its text parts.
func firstChatUserText(messages []json.RawMessage) string {
	for _, raw := range messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil || m.Role != "user" {
			continue
		}
		if out := firstStringOrTextParts(m.Content, "text"); out != "" {
			return out
		}
	}
	return ""
}

// firstResponsesUserText reads responses-shaped input: a plain string, or
// an array whose first user message carries input_text parts.
func firstResponsesUserText(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(input, &text); err == nil {
		return text
	}
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(input, &items); err != nil {
		return ""
	}
	for _, item := range items {
		if item.Type != "" && item.Type != "message" {
			continue
		}
		if item.Role != "" && item.Role != "user" {
			continue
		}
		if out := firstStringOrTextParts(item.Content, "input_text", "text"); out != "" {
			return out
		}
	}
	return ""
}

// firstStringOrTextParts reads one message's content as a plain string
// verbatim, else as an array of parts whose type is in acceptedTypes,
// returning the concatenation of their text. A blank result (string or
// parts) is reported as "" so the caller keeps scanning later messages.
func firstStringOrTextParts(content json.RawMessage, acceptedTypes ...string) string {
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		if strings.TrimSpace(text) != "" {
			return text
		}
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		if slices.Contains(acceptedTypes, part.Type) {
			b.WriteString(part.Text)
		}
	}
	if out := b.String(); strings.TrimSpace(out) != "" {
		return out
	}
	return ""
}

// truncateRunes caps text at n runes for hashing.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
