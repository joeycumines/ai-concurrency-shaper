package transcode_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// Chat Completions <-> Responses conversion tests.

func TestConvertResponsesRequestChatRequest(t *testing.T) {
	f := mustResponses(t)
	req := f.Request
	req.Input = append(req.Input,
		transcode.ResponsesMessage{
			ID:   new("rs_1"),
			Type: new(transcode.ResponsesMessageTypeReasoning),
			Role: new(transcode.ResponsesMessageRoleAssistant),
			ResponsesReasoning: &transcode.ResponsesReasoning{
				Summary: []transcode.ResponsesReasoningSummary{{Type: "summary_text", Text: "think step one"}},
			},
		},
		transcode.ResponsesMessage{
			ID:   new("fc_1"),
			Type: new(transcode.ResponsesMessageTypeFunctionCall),
			Role: new(transcode.ResponsesMessageRoleAssistant),
			ResponsesToolMessage: &transcode.ResponsesToolMessage{
				CallID:    new("call_abc123"),
				Name:      new("get_weather"),
				Arguments: new(`{"location":"Tokyo"}`),
			},
		},
		transcode.ResponsesMessage{
			ID:   new("fo_1"),
			Type: new(transcode.ResponsesMessageTypeFunctionCallOutput),
			ResponsesToolMessage: &transcode.ResponsesToolMessage{
				CallID: new("call_abc123"),
				Output: &transcode.ResponsesToolMessageOutput{Str: new(`{"temperature":21}`)},
			},
		},
	)
	chat := transcode.ConvertResponsesRequestChatRequest(&req)

	if chat.Model != req.Model {
		t.Errorf("model = %q, want %q", chat.Model, req.Model)
	}
	if len(chat.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (system, user, assistant, tool)", len(chat.Messages))
	}
	if chat.Messages[0].Role != transcode.ChatMessageRoleSystem || chat.Messages[0].Content.ContentStr == nil || *chat.Messages[0].Content.ContentStr != *req.Instructions {
		t.Errorf("first message = %+v, want system message with instructions", chat.Messages[0])
	}
	user := chat.Messages[1]
	if user.Role != transcode.ChatMessageRoleUser || user.Content.ContentStr == nil {
		t.Errorf("second message = %+v, want user message", user)
	}
	assistant := chat.Messages[2]
	if assistant.Role != transcode.ChatMessageRoleAssistant || assistant.ChatAssistantMessage == nil {
		t.Fatalf("third message = %+v, want assistant message", assistant)
	}
	if assistant.Reasoning == nil || !strings.Contains(*assistant.Reasoning, "think step one") {
		t.Errorf("assistant reasoning = %v, want attached summary", assistant.Reasoning)
	}
	if len(assistant.ReasoningDetails) != 1 || assistant.ReasoningDetails[0].Type != transcode.ChatReasoningDetailsTypeSummary {
		t.Errorf("assistant reasoning_details = %+v, want one summary detail", assistant.ReasoningDetails)
	}
	if len(assistant.ToolCalls) != 1 || derefStr(assistant.ToolCalls[0].ID) != "call_abc123" ||
		derefStr(assistant.ToolCalls[0].Function.Name) != "get_weather" ||
		assistant.ToolCalls[0].Function.Arguments != `{"location":"Tokyo"}` {
		t.Errorf("assistant tool calls = %+v", assistant.ToolCalls)
	}
	tool := chat.Messages[3]
	if tool.Role != transcode.ChatMessageRoleTool || tool.ToolCallID == nil || *tool.ToolCallID != "call_abc123" ||
		tool.Content == nil || tool.Content.ContentStr == nil || *tool.Content.ContentStr != `{"temperature":21}` {
		t.Errorf("tool message = %+v", tool)
	}
	if chat.MaxCompletionTokens == nil || *chat.MaxCompletionTokens != *req.MaxOutputTokens {
		t.Errorf("max_completion_tokens = %v, want %v", chat.MaxCompletionTokens, req.MaxOutputTokens)
	}
	if chat.ToolChoice == nil || chat.ToolChoice.Str == nil || *chat.ToolChoice.Str != "auto" {
		t.Errorf("tool_choice = %+v, want auto", chat.ToolChoice)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function == nil || chat.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tools = %+v, want one function tool", chat.Tools)
	}
	if chat.Reasoning == nil || chat.Reasoning.Effort == nil || *chat.Reasoning.Effort != "medium" {
		t.Errorf("reasoning = %+v, want effort medium", chat.Reasoning)
	}
	if chat.Temperature == nil || *chat.Temperature != 0.7 || chat.TopP == nil {
		t.Errorf("temperature/top_p = %v/%v", chat.Temperature, chat.TopP)
	}
	if chat.User == nil || *chat.User != "user_123" {
		t.Errorf("user = %v, want user_123", chat.User)
	}
	if chat.Metadata["session_id"] != "sess_123" {
		t.Errorf("metadata = %+v", chat.Metadata)
	}
}

// TestConvertResponsesRequestChatRequestDeveloperNormalization verifies that
// developer-role input items normalize to system role, and that non-function
// tools and unknown tool choices are sanitized away.
func TestConvertResponsesRequestChatRequestDeveloperNormalization(t *testing.T) {
	req := transcode.ResponsesRequest{
		Model: "gpt-4.1",
		Input: []transcode.ResponsesMessage{{
			ID:      new("msg_1"),
			Type:    new(transcode.ResponsesMessageTypeMessage),
			Role:    new(transcode.ResponsesMessageRoleDeveloper),
			Content: &transcode.ResponsesMessageContent{ContentStr: new("be concise")},
		}},
		Tools: []transcode.ResponsesTool{
			{Type: "web_search", Name: new("web")},
			{Type: transcode.ResponsesToolTypeFunction, Name: new("  ")},
			{Type: transcode.ResponsesToolTypeFunction, Name: new("get_weather")},
		},
		ToolChoice: &transcode.ResponsesToolChoice{Struct: &transcode.ResponsesToolChoiceStruct{Type: "function", Name: new("unknown_tool")}},
	}
	chat := transcode.ConvertResponsesRequestChatRequest(&req)
	if len(chat.Messages) != 1 || chat.Messages[0].Role != transcode.ChatMessageRoleSystem {
		t.Errorf("messages = %+v, want one system message", chat.Messages)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function == nil || chat.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tools = %+v, want only the get_weather function tool", chat.Tools)
	}
	if chat.ToolChoice != nil {
		t.Errorf("tool_choice = %+v, want nil for unknown tool name", chat.ToolChoice)
	}
}

// TestConvertResponsesRequestChatRequestContentBlocks verifies the block-form
// conversions of the anthropic direction: block-form system, image blocks,
// tool result block content, and tool choice variants.
func TestConvertResponsesRequestChatRequestContentBlocks(t *testing.T) {
	req := transcode.ResponsesRequest{
		Model: "gpt-4.1",
		Input: []transcode.ResponsesMessage{
			{
				ID:   new("msg_1"),
				Type: new(transcode.ResponsesMessageTypeMessage),
				Role: new(transcode.ResponsesMessageRoleUser),
				Content: &transcode.ResponsesMessageContent{ContentBlocks: []transcode.ResponsesMessageContentBlock{
					{Type: transcode.ResponsesMessageContentBlockTypeInputText, Text: new("one")},
					{Type: transcode.ResponsesMessageContentBlockTypeInputText, Text: new("two")},
					{Type: transcode.ResponsesMessageContentBlockTypeInputImage, ImageURL: new("https://example.com/i.png")},
				}},
			},
			{
				ID:      new("msg_2"),
				Type:    new(transcode.ResponsesMessageTypeRefusal),
				Role:    new(transcode.ResponsesMessageRoleAssistant),
				Content: &transcode.ResponsesMessageContent{ContentStr: new("I refuse")},
			},
		},
		Tools: []transcode.ResponsesTool{{Type: transcode.ResponsesToolTypeFunction, Name: new("get_weather")}},
		ToolChoice: &transcode.ResponsesToolChoice{Struct: &transcode.ResponsesToolChoiceStruct{
			Type: "function", Name: new("get_weather"),
		}},
	}
	out := transcode.ConvertResponsesRequestChatRequest(&req)

	if len(out.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(out.Messages))
	}
	blocks := out.Messages[0].Content.ContentBlocks
	if len(blocks) != 3 {
		t.Fatalf("content blocks = %d, want 3", len(blocks))
	}
	if blocks[0].Type != transcode.ChatContentBlockTypeText || blocks[0].Text == nil || *blocks[0].Text != "one" {
		t.Errorf("block 0 = %+v", blocks[0])
	}
	if blocks[2].Type != transcode.ChatContentBlockTypeImage || blocks[2].ImageURL == nil || blocks[2].ImageURL.URL != "https://example.com/i.png" {
		t.Errorf("block 2 = %+v", blocks[2])
	}
	refusal := out.Messages[1]
	if refusal.Role != transcode.ChatMessageRoleAssistant || refusal.Refusal == nil || *refusal.Refusal != "I refuse" {
		t.Errorf("refusal message = %+v", refusal)
	}
	if out.ToolChoice == nil || out.ToolChoice.Struct == nil || out.ToolChoice.Struct.Function == nil || out.ToolChoice.Struct.Function.Name != "get_weather" {
		t.Errorf("tool_choice = %+v, want named function choice", out.ToolChoice)
	}
}

// TestConvertResponsesRequestChatRequestImageData verifies a base64
// input_image payload maps to a chat image_url data URI rather than being
// dropped.
func TestConvertResponsesRequestChatRequestImageData(t *testing.T) {
	req := transcode.ResponsesRequest{
		Model: "gpt-4.1",
		Input: []transcode.ResponsesMessage{{
			ID:   new("msg_1"),
			Type: new(transcode.ResponsesMessageTypeMessage),
			Role: new(transcode.ResponsesMessageRoleUser),
			Content: &transcode.ResponsesMessageContent{ContentBlocks: []transcode.ResponsesMessageContentBlock{{
				Type:      transcode.ResponsesMessageContentBlockTypeInputImage,
				ImageData: json.RawMessage(`{"type":"base64","data":"aGVsbG8=","media_type":"image/png"}`),
			}}},
		}},
	}
	out := transcode.ConvertResponsesRequestChatRequest(&req)
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %+v", out.Messages)
	}
	blocks := out.Messages[0].Content.ContentBlocks
	if len(blocks) != 1 || blocks[0].Type != transcode.ChatContentBlockTypeImage ||
		blocks[0].ImageURL == nil || blocks[0].ImageURL.URL != "data:image/png;base64,aGVsbG8=" {
		t.Errorf("image block = %+v, want data URI", blocks)
	}

	// A URL image still maps to the URL form.
	urlReq := transcode.ResponsesRequest{
		Model: "gpt-4.1",
		Input: []transcode.ResponsesMessage{{
			ID:   new("msg_2"),
			Type: new(transcode.ResponsesMessageTypeMessage),
			Role: new(transcode.ResponsesMessageRoleUser),
			Content: &transcode.ResponsesMessageContent{ContentBlocks: []transcode.ResponsesMessageContentBlock{{
				Type:     transcode.ResponsesMessageContentBlockTypeInputImage,
				ImageURL: new("https://example.com/img.png"),
			}}},
		}},
	}
	out = transcode.ConvertResponsesRequestChatRequest(&urlReq)
	blocks = out.Messages[0].Content.ContentBlocks
	if len(blocks) != 1 || blocks[0].ImageURL == nil || blocks[0].ImageURL.URL != "https://example.com/img.png" {
		t.Errorf("url image block = %+v", blocks)
	}
}

// TestConvertChatResponseResponsesResponseNilInput verifies that passing a
// nil ChatResponse returns an empty (non-nil) response rather than panicking.
func TestConvertChatResponseResponsesResponseNilInput(t *testing.T) {
	out := transcode.ConvertChatResponseResponsesResponse(nil)
	if out.Object != "" {
		t.Errorf("nil chat response = %+v, want zero-value", out)
	}
}
