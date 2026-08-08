package transcode

// J5 regression tests (review-k finding 5, high): tagged unions decode
// per-arm — each type admits exactly its own fields with DisallowUnknownFields,
// so contradictory arms are rejected at decode instead of having their data
// silently discarded.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAnthropicContentBlockPerArmRejectsMixedArms proves every anthropic
// content block arm rejects fields belonging to another arm, including the
// review's exact counterexample (a text block carrying a source).
func TestAnthropicContentBlockPerArmRejectsMixedArms(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "text with source",
			body: `{"type":"text","text":"visible","source":{"type":"url","url":"https://example.test/image.png"}}`,
		},
		{
			name: "text with tool use id",
			body: `{"type":"text","text":"visible","tool_use_id":"t1"}`,
		},
		{
			name: "image with text",
			body: `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"},"text":"t"}`,
		},
		{
			name: "image with tool fields",
			body: `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"},"id":"t1","name":"f","input":{}}`,
		},
		{
			name: "document with name",
			body: `{"type":"document","source":{"type":"url","url":"https://example.test/d.pdf"},"name":"f"}`,
		},
		{
			name: "tool use with text",
			body: `{"type":"tool_use","id":"t1","name":"f","input":{},"text":"x"}`,
		},
		{
			name: "tool use with source",
			body: `{"type":"tool_use","id":"t1","name":"f","input":{},"source":{"type":"url","url":"https://example.test/x.png"}}`,
		},
		{
			name: "tool result with input",
			body: `{"type":"tool_result","tool_use_id":"t1","content":"ok","input":{}}`,
		},
		{
			name: "tool result with text",
			body: `{"type":"tool_result","tool_use_id":"t1","content":"ok","text":"x"}`,
		},
		{
			name: "thinking with data",
			body: `{"type":"thinking","thinking":"x","signature":"s","data":"d"}`,
		},
		{
			name: "thinking with source",
			body: `{"type":"thinking","thinking":"x","signature":"s","source":{"type":"url","url":"https://example.test/x.png"}}`,
		},
		{
			name: "redacted thinking with thinking",
			body: `{"type":"redacted_thinking","data":"d","thinking":"x"}`,
		},
		{
			name: "unknown type",
			body: `{"type":"bogus","text":"x"}`,
		},
		{
			name: "malformed JSON",
			body: `{"type":"text","text":`,
		},
		// Required-field enforcement inside each arm (the per-arm decode
		// accepts the arm's fields; Validate rejects missing required ones).
		{"text without text", `{"type":"text"}`},
		{"image without source", `{"type":"image"}`},
		{"document without source", `{"type":"document"}`},
		{"tool use without id", `{"type":"tool_use","name":"f","input":{}}`},
		{"tool use without name", `{"type":"tool_use","id":"t1","input":{}}`},
		{"tool use without input", `{"type":"tool_use","id":"t1","name":"f"}`},
		{"tool use non-object input", `{"type":"tool_use","id":"t1","name":"f","input":[1,2]}`},
		{"tool result without tool use id", `{"type":"tool_result","content":"ok"}`},
		{"tool result without content", `{"type":"tool_result","tool_use_id":"t1"}`},
		{"thinking without signature", `{"type":"thinking","thinking":"x"}`},
		{"thinking without thinking", `{"type":"thinking","signature":"s"}`},
		{"redacted thinking without data", `{"type":"redacted_thinking"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var block AnthropicContentBlock
			if err := json.Unmarshal([]byte(tt.body), &block); err == nil {
				t.Fatalf("mixed-arm block accepted: %s", tt.body)
			}
		})
	}
}

// TestAnthropicContentBlockPerArmPositives proves each arm decodes with its
// own fields, validates, and round-trips through its own encoding — with the
// thinking values preserved for the byte-for-byte artifact path.
func TestAnthropicContentBlockPerArmPositives(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"text", `{"type":"text","text":"hello"}`},
		{"image", `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}`},
		{"document", `{"type":"document","source":{"type":"url","url":"https://example.test/d.pdf"}}`},
		{"tool use", `{"type":"tool_use","id":"t1","name":"f","input":{"x":1}}`},
		{"tool result", `{"type":"tool_result","tool_use_id":"t1","content":"ok"}`},
		{"tool result error", `{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"bad"}`},
		{"thinking", `{"type":"thinking","thinking":"hmm","signature":"sig123"}`},
		{"redacted thinking", `{"type":"redacted_thinking","data":"redacted"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var block AnthropicContentBlock
			if err := json.Unmarshal([]byte(tt.body), &block); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := block.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			encoded, err := json.Marshal(block)
			if err != nil {
				t.Fatal(err)
			}
			var second AnthropicContentBlock
			if err := json.Unmarshal(encoded, &second); err != nil {
				t.Fatalf("re-decode: %v\nencoded=%s", err, encoded)
			}
			if !semanticJSONEqual(encoded, mustMarshalJSON(t, second)) {
				t.Fatalf("unstable round trip:\nfirst=%s\nsecond=%s", encoded, mustMarshalJSON(t, second))
			}
			// The thinking values survive byte-for-byte for the artifact
			// path: the re-marshal carries the same field values.
			if strings.Contains(tt.body, `"type":"thinking"`) {
				if !strings.Contains(string(encoded), `"thinking":"hmm"`) ||
					!strings.Contains(string(encoded), `"signature":"sig123"`) {
					t.Fatalf("thinking values lost: %s", encoded)
				}
			}
		})
	}
}

// TestChatContentBlockPerArmRejectsMixedArms proves a Chat text block
// carrying image_url (and vice versa) is rejected at decode.
func TestChatContentBlockPerArmRejectsMixedArms(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"text with image url", `{"type":"text","text":"x","image_url":{"url":"https://example.test/x.png"}}`},
		{"image url with text", `{"type":"image_url","image_url":{"url":"https://example.test/x.png"},"text":"x"}`},
		{"unknown type", `{"type":"bogus","text":"x"}`},
		{"malformed JSON", `{"type":"text","text":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var block ChatContentBlock
			if err := json.Unmarshal([]byte(tt.body), &block); err == nil {
				t.Fatalf("mixed-arm chat block accepted: %s", tt.body)
			}
		})
	}
}

// TestChatContentBlockPerArmPositives proves the Chat arms decode and
// round-trip.
func TestChatContentBlockPerArmPositives(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"text", `{"type":"text","text":"hello"}`},
		{"image url", `{"type":"image_url","image_url":{"url":"https://example.test/x.png"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var block ChatContentBlock
			if err := json.Unmarshal([]byte(tt.body), &block); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := block.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			encoded, err := json.Marshal(block)
			if err != nil {
				t.Fatal(err)
			}
			var second ChatContentBlock
			if err := json.Unmarshal(encoded, &second); err != nil {
				t.Fatalf("re-decode: %v\nencoded=%s", err, encoded)
			}
			if !semanticJSONEqual(encoded, mustMarshalJSON(t, second)) {
				t.Fatalf("unstable round trip:\nfirst=%s\nsecond=%s", encoded, mustMarshalJSON(t, second))
			}
		})
	}
}

// TestAnthropicToolChoiceNameRejected proves a name is only valid with type
// "tool": auto/none/any carrying a name are contradictory union arms,
// rejected instead of silently dropping the name.
func TestAnthropicToolChoiceNameRejected(t *testing.T) {
	var report ConversionReport
	for _, mode := range []string{"auto", "none", "any"} {
		if _, err := canonicalizeAnthropicToolChoice(
			AnthropicToolChoice{Type: mode, Name: "f"},
			&report,
			StrictLossPolicy(),
		); err == nil {
			t.Fatalf("tool_choice %s with name accepted", mode)
		}
	}
	// Type tool requires a name and accepts one.
	if _, err := canonicalizeAnthropicToolChoice(
		AnthropicToolChoice{Type: "tool", Name: "f"},
		&report,
		StrictLossPolicy(),
	); err != nil {
		t.Fatalf("tool with name rejected: %v", err)
	}
	if _, err := canonicalizeAnthropicToolChoice(
		AnthropicToolChoice{Type: "tool"},
		&report,
		StrictLossPolicy(),
	); err == nil {
		t.Fatal("tool without name accepted")
	}
	// The plain modes still map without a name.
	for _, tt := range []struct {
		in   string
		want string
	}{
		{"auto", "auto"},
		{"none", "none"},
		{"any", "required"},
	} {
		choice, err := canonicalizeAnthropicToolChoice(
			AnthropicToolChoice{Type: tt.in},
			&report,
			StrictLossPolicy(),
		)
		if err != nil {
			t.Fatalf("%s: %v", tt.in, err)
		}
		if choice.Mode != tt.want {
			t.Fatalf("%s mode = %q, want %q", tt.in, choice.Mode, tt.want)
		}
	}
	// An unknown type is rejected, never defaulted.
	if _, err := canonicalizeAnthropicToolChoice(
		AnthropicToolChoice{Type: "bogus"},
		&report,
		StrictLossPolicy(),
	); err == nil {
		t.Fatal("unknown tool_choice type accepted")
	}
}

// TestChatMessageRoleConditionalValidate proves assistant-only fields
// (refusal, tool_calls, reasoning) are rejected on other roles and
// tool_call_id is rejected on every role but tool — never silently
// discarded or relabeled.
func TestChatMessageRoleConditionalValidate(t *testing.T) {
	rejected := []struct {
		name string
		msg  ChatMessage
	}{
		{
			name: "user with refusal",
			msg: ChatMessage{
				Role: ChatMessageRoleUser,
				ChatAssistantMessage: &ChatAssistantMessage{
					Refusal: str("no"),
				},
			},
		},
		{
			name: "user with tool calls",
			msg: ChatMessage{
				Role: ChatMessageRoleUser,
				ChatAssistantMessage: &ChatAssistantMessage{
					ToolCalls: []ChatMessageToolCall{{
						ID:   str("call_1"),
						Type: str("function"),
						Function: ChatToolCallFunction{
							Name:      str("f"),
							Arguments: "{}",
						},
					}},
				},
			},
		},
		{
			name: "user with reasoning",
			msg: ChatMessage{
				Role: ChatMessageRoleUser,
				ChatAssistantMessage: &ChatAssistantMessage{
					Reasoning: str("think"),
				},
			},
		},
		{
			name: "assistant with tool call id",
			msg: ChatMessage{
				Role:            ChatMessageRoleAssistant,
				ChatToolMessage: &ChatToolMessage{ToolCallID: str("t1")},
			},
		},
		{
			name: "system with refusal",
			msg: ChatMessage{
				Role: ChatMessageRoleSystem,
				ChatAssistantMessage: &ChatAssistantMessage{
					Refusal: str("no"),
				},
			},
		},
		{
			name: "tool without tool call id",
			msg: ChatMessage{
				Role: ChatMessageRoleTool,
			},
		},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.msg.Validate(); err == nil {
				t.Fatalf("contradictory message accepted: %+v", tt.msg)
			}
		})
	}

	accepted := []ChatMessage{
		{Role: ChatMessageRoleAssistant, ChatAssistantMessage: &ChatAssistantMessage{Refusal: str("no")}},
		{Role: ChatMessageRoleAssistant, ChatAssistantMessage: &ChatAssistantMessage{
			ToolCalls: []ChatMessageToolCall{{
				ID:   str("call_1"),
				Type: str("function"),
				Function: ChatToolCallFunction{
					Name:      str("f"),
					Arguments: "{}",
				},
			}},
		}},
		{Role: ChatMessageRoleTool, ChatToolMessage: &ChatToolMessage{ToolCallID: str("t1")}},
		{Role: ChatMessageRoleUser, Content: &ChatMessageContent{ContentStr: str("hi")}},
	}
	for i, msg := range accepted {
		if err := msg.Validate(); err != nil {
			t.Fatalf("valid message %d rejected: %v", i, err)
		}
	}
}
