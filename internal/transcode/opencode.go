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
	"strings"
)

// First-party opencode header names. The pinned client sends the whole set
// together; the preset reproduces that shape on every outbound request.
const (
	HeaderOpencodeSession = "X-Opencode-Session"
	HeaderOpencodeClient  = "X-Opencode-Client"
	HeaderOpencodeProject = "X-Opencode-Project"
	HeaderOpencodeRequest = "X-Opencode-Request"
	HeaderSessionAffinity = "X-Session-Affinity"
	HeaderSessionID       = "X-Session-Id"
	HeaderParentSessionID = "X-Parent-Session-Id"
)

const (
	// DefaultOpencodeUserAgent is the pinned first-party User-Agent,
	// observed 2026-09-28 (local CLI 2.0.3, channel allowlist
	// dev/beta/next/prod). Re-pin per the recorded rule: verify against
	// the latest stable release, update the default and its test pin.
	DefaultOpencodeUserAgent = "opencode/prod/2.0.3/opencode"
	// DefaultOpencodeClient is the first-party client attribution.
	DefaultOpencodeClient = "cli"
)

// conversationKeyDomain separates derived session keys from every other
// hash in the system.
const conversationKeyDomain = "opencode-conversation-v1"

// OpencodePreset configures first-party opencode header emission for one
// provider mount. Disabled (zero value) is a no-op: nothing is set, read,
// or derived. Enabled, every outbound request carries the full header set
// with values resolved inbound-first: genuine client values are preserved,
// preset defaults fill only the gaps. Header values are never logged.
type OpencodePreset struct {
	Enabled bool
	// UserAgent and Client default to the pinned first-party values when
	// empty. Inbound values always win over both.
	UserAgent string
	Client    string
	// Provider scopes derived session keys to the mount. Set from the
	// provider's effective name at resolve time.
	Provider string
}

// userAgent resolves the effective User-Agent: inbound first, then the
// configured value, then the pinned default.
func (p OpencodePreset) userAgent(in http.Header) string {
	if v := strings.TrimSpace(in.Get("User-Agent")); v != "" {
		return v
	}
	if strings.TrimSpace(p.UserAgent) != "" {
		return strings.TrimSpace(p.UserAgent)
	}
	return DefaultOpencodeUserAgent
}

// client resolves the effective client attribution the same way.
func (p OpencodePreset) client(in http.Header) string {
	if v := strings.TrimSpace(in.Get(HeaderOpencodeClient)); v != "" {
		return v
	}
	if strings.TrimSpace(p.Client) != "" {
		return strings.TrimSpace(p.Client)
	}
	return DefaultOpencodeClient
}

// ResolveSession returns the outbound session value: the first present
// inbound identity header (x-opencode-session, x-session-affinity,
// X-Session-Id), else the caller-derived stable fallback. Callers always
// supply a synthesized fallback, so the preset always emits.
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
// credentials and authentication never strips the mock. Identity and
// project headers forward only when the client sent them: they name the
// client's own conversation and are never fabricated.
func (p OpencodePreset) ApplyHeaders(out, in http.Header, session string) {
	if !p.Enabled {
		return
	}
	out.Set("User-Agent", p.userAgent(in))
	out.Set(HeaderOpencodeClient, p.client(in))
	if session != "" {
		out.Set(HeaderOpencodeSession, session)
		out.Set(HeaderSessionAffinity, session)
		out.Set(HeaderSessionID, session)
	} else {
		out.Del(HeaderOpencodeSession)
		out.Del(HeaderSessionAffinity)
		out.Del(HeaderSessionID)
	}
	for _, key := range []string{HeaderOpencodeRequest, HeaderOpencodeProject, HeaderParentSessionID} {
		if v := strings.TrimSpace(in.Get(key)); v != "" {
			out.Set(key, v)
		} else {
			out.Del(key)
		}
	}
}

// DeriveConversationKey returns a stable opaque session key from the
// request's own stable fields: provider scope, model, and first user text.
// Full histories resend every turn, so the first user turn identifies the
// conversation across tool calls without tracking state. Volatile and
// tool-call content never enters the hash.
func DeriveConversationKey(provider, model, firstUserText string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		conversationKeyDomain, provider, model, firstUserText,
	}, "\x00")))
	return hex.EncodeToString(sum[:])[:32]
}

// DeriveClientKey returns a stable opaque session key from the client
// address for paths that cannot inspect the body. The port is stripped:
// it varies per connection while the client stays the same.
func DeriveClientKey(provider, remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		conversationKeyDomain, provider, strings.TrimSpace(host),
	}, "\x00")))
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
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil {
			if strings.TrimSpace(text) != "" {
				return text
			}
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			continue
		}
		var b strings.Builder
		for _, part := range parts {
			if part.Type == "text" {
				b.WriteString(part.Text)
			}
		}
		if out := b.String(); strings.TrimSpace(out) != "" {
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
		var s string
		if err := json.Unmarshal(item.Content, &s); err == nil {
			if strings.TrimSpace(s) != "" {
				return s
			}
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(item.Content, &parts); err != nil {
			continue
		}
		var b strings.Builder
		for _, part := range parts {
			if part.Type == "input_text" || part.Type == "text" {
				b.WriteString(part.Text)
			}
		}
		if out := b.String(); strings.TrimSpace(out) != "" {
			return out
		}
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
