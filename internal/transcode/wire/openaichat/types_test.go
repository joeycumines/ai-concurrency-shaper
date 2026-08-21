package openaichat

// Unit tests for the Chat wire types' union and validation surface that the
// transcode suite does not reach (Chat is upstream-only: the request types
// are rendered, never decoded, so their union validation and marshal paths
// need direct coverage here).

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolChoiceValidateAndMarshal(t *testing.T) {
	auto := "auto"
	if err := (ToolChoice{Str: &auto}).Validate(); err != nil {
		t.Fatal(err)
	}
	named := &ToolChoiceStruct{
		Type:     "function",
		Function: &ToolChoiceFunction{Name: "f"},
	}
	choice := ToolChoice{Struct: named}
	if err := choice.Validate(); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(choice)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{"type":"function","function":{"name":"f"}}` {
		t.Fatalf("marshal = %s", wire)
	}

	// Both arms selected is a contradictory union.
	both := ToolChoice{Str: &auto, Struct: named}
	if err := both.Validate(); err == nil {
		t.Fatal("both variants accepted")
	}
	// A named choice without a function name is invalid.
	if err := (ToolChoice{Struct: &ToolChoiceStruct{Type: "function"}}).Validate(); err == nil {
		t.Fatal("function-less named choice accepted")
	}
	// Unknown string values are rejected.
	bad := "always"
	if err := (ToolChoice{Str: &bad}).Validate(); err == nil {
		t.Fatal("invalid choice string accepted")
	}
	// No variant selected is invalid.
	if err := (ToolChoice{}).Validate(); err == nil {
		t.Fatal("empty choice accepted")
	}
	// Marshal validates: an invalid choice fails to marshal.
	if _, err := json.Marshal(ToolChoice{}); err == nil {
		t.Fatal("invalid choice marshaled")
	}
}

func TestStopValidateAndMarshal(t *testing.T) {
	a, b := "A", "B"
	stop := Stop{Strs: []string{a, b}}
	if err := stop.Validate(); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(stop)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `["A","B"]` {
		t.Fatalf("marshal = %s", wire)
	}

	// Both variants selected is a contradictory union.
	if err := (Stop{Str: &a, Strs: []string{b}}).Validate(); err == nil {
		t.Fatal("both variants accepted")
	}
	// No variant selected is invalid.
	if err := (Stop{}).Validate(); err == nil {
		t.Fatal("empty stop accepted")
	}
	// The official contract bounds the array at four sequences.
	if err := (Stop{Strs: []string{"1", "2", "3", "4", "5"}}).Validate(); err == nil {
		t.Fatal("five-sequence stop accepted")
	}
	if _, err := json.Marshal(Stop{}); err == nil {
		t.Fatal("invalid stop marshaled")
	}

	// Unmarshal rejects null and unknown shapes.
	var decoded Stop
	if err := json.Unmarshal([]byte("null"), &decoded); err == nil {
		t.Fatal("null stop accepted")
	}
	if err := json.Unmarshal([]byte(`["A","B"]`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Str != nil || len(decoded.Strs) != 2 {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestMessageValidateRoleConditional(t *testing.T) {
	// Assistant-only fields on a tool-role message are a contradictory
	// union, reported as a typed decode rejection.
	message := Message{
		Role:                 MessageRoleTool,
		ChatAssistantMessage: &ChatAssistantMessage{Refusal: new("no")},
	}
	err := message.Validate()
	if err == nil {
		t.Fatal("tool message with assistant fields accepted")
	}
	if !strings.Contains(err.Error(), "contradictory_union") {
		t.Fatalf("err = %v, want a typed contradictory-union rejection", err)
	}

	// Tool-only fields on a user-role message are likewise contradictory.
	user := Message{
		Role:            MessageRoleUser,
		ChatToolMessage: &ChatToolMessage{ToolCallID: new("t1")},
	}
	if err := user.Validate(); err == nil {
		t.Fatal("user message with tool_call_id accepted")
	}
}

func TestToolValidate(t *testing.T) {
	if err := (Tool{Type: ToolTypeFunction, Function: &ToolFunction{Name: "f"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Tool{Type: "web_search"}).Validate(); err == nil {
		t.Fatal("non-function tool accepted")
	}
	if err := (Tool{Type: ToolTypeFunction}).Validate(); err == nil {
		t.Fatal("function-less tool accepted")
	}
	if err := (Tool{Type: ToolTypeFunction, Function: &ToolFunction{}}).Validate(); err == nil {
		t.Fatal("name-less tool accepted")
	}
}

func TestContentBlockValidateBranches(t *testing.T) {
	text := "hi"
	if err := (ContentBlock{Type: ContentBlockTypeText, Text: &text}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ContentBlock{Type: ContentBlockTypeText}).Validate(); err == nil {
		t.Fatal("text-less text block accepted")
	}
	if err := (ContentBlock{Type: ContentBlockTypeImage, ImageURL: &InputImage{URL: "u"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ContentBlock{Type: ContentBlockTypeImage}).Validate(); err == nil {
		t.Fatal("url-less image block accepted")
	}
	if err := (ContentBlock{Type: "bogus"}).Validate(); err == nil {
		t.Fatal("unknown block type accepted")
	}
	// The union is exclusive: both content variants is contradictory.
	if err := (MessageContent{ContentStr: &text, ContentBlocks: []ContentBlock{{Type: ContentBlockTypeText, Text: &text}}}).Validate(); err == nil {
		t.Fatal("both content variants accepted")
	}
	if err := (MessageContent{}).Validate(); err == nil {
		t.Fatal("empty content accepted")
	}
	if _, err := json.Marshal(MessageContent{}); err == nil {
		t.Fatal("invalid content marshaled")
	}
}

func TestMessageValidateBranches(t *testing.T) {
	text := "hi"
	content := MessageContent{ContentStr: &text}
	if err := (Message{Role: MessageRoleUser, Content: &content}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Message{Content: &content}).Validate(); err == nil {
		t.Fatal("role-less message accepted")
	}
	if err := (Message{Role: MessageRoleTool}).Validate(); err == nil {
		t.Fatal("tool message without tool_call_id accepted")
	}
	// A tool call without an id is invalid.
	msg := Message{
		Role: MessageRoleAssistant,
		ChatAssistantMessage: &ChatAssistantMessage{
			ToolCalls: []MessageToolCall{{Type: "function", Function: ToolCallFunction{Name: new("f"), Arguments: "{}"}}},
		},
	}
	if err := msg.Validate(); err == nil {
		t.Fatal("id-less tool call accepted")
	}
	// A tool call whose type is not function is invalid.
	msg = Message{
		Role: MessageRoleAssistant,
		ChatAssistantMessage: &ChatAssistantMessage{
			ToolCalls: []MessageToolCall{{Type: "bogus", ID: new("t"), Function: ToolCallFunction{Name: new("f"), Arguments: "{}"}}},
		},
	}
	if err := msg.Validate(); err == nil {
		t.Fatal("non-function tool call accepted")
	}
}
