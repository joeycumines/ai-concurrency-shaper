package transcode

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
)

var nextItemID atomic.Uint64

// newItemID returns a fresh synthetic item identifier with the given prefix,
// e.g. "msg_1". Identifiers are unique per process and stable per conversion
// order, which keeps generated payloads deterministic in tests.
func newItemID(prefix string) string {
	return prefix + strconv.FormatUint(nextItemID.Add(1), 10)
}

// nonEmptyPtr returns a pointer to s, or nil when s is empty.
func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// =============================================================================
// Role and type helpers
// =============================================================================

func responsesRoleChatRole(role *ResponsesMessageRoleType) ChatMessageRole {
	if role == nil {
		return ChatMessageRoleUser
	}
	switch *role {
	case ResponsesMessageRoleDeveloper, ResponsesMessageRoleSystem:
		// Deliberate normalization mandated by the plan: the chat completions
		// direction carries the developer role as system. Models that require
		// a literal developer role (o1-family) are served by the
		// chat-completions-as-upstream path with this documented loss.
		return ChatMessageRoleSystem
	case ResponsesMessageRoleAssistant:
		return ChatMessageRoleAssistant
	default:
		return ChatMessageRoleUser
	}
}

func chatRoleResponsesRole(role ChatMessageRole) *ResponsesMessageRoleType {
	switch role {
	case ChatMessageRoleAssistant:
		return new(ResponsesMessageRoleAssistant)
	case ChatMessageRoleSystem:
		return new(ResponsesMessageRoleSystem)
	case ChatMessageRoleDeveloper:
		return new(ResponsesMessageRoleDeveloper)
	default:
		return new(ResponsesMessageRoleUser)
	}
}

func anthropicRoleResponsesRole(role AnthropicMessageRole) ResponsesMessageRoleType {
	if role == AnthropicMessageRoleAssistant {
		return ResponsesMessageRoleAssistant
	}
	return ResponsesMessageRoleUser
}

// toolInputArguments returns the JSON argument string for a tool use input:
// the raw input when non-empty, otherwise an empty object (a tool call
// without arguments is still a valid call with an empty payload).
func toolInputArguments(input json.RawMessage) string {
	if len(input) > 0 && strings.TrimSpace(string(input)) != "" {
		return string(input)
	}
	return "{}"
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// trimmedReasoning returns s with trailing newlines removed, or nil when s is
// nil or empty after trimming.
func trimmedReasoning(s *string) *string {
	if s == nil {
		return nil
	}
	trimmed := strings.TrimRight(*s, "\n")
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
