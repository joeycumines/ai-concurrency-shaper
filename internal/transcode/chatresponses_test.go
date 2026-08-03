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

// TestConvertChatResponseResponsesResponseStop verifies that a chat response
// with finish_reason "stop" produces a completed Responses response.
func TestConvertChatResponseResponsesResponseStop(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-1",
		Object:  "chat.completion",
		Created: 1710000000,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{{
			Index: 0,
			Message: &transcode.ChatMessage{
				Role:                 transcode.ChatMessageRoleAssistant,
				ChatAssistantMessage: &transcode.ChatAssistantMessage{},
				Content:              &transcode.ChatMessageContent{ContentStr: new("Hello, world!")},
			},
			FinishReason: new("stop"),
		}},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status == nil || *out.Status != "completed" {
		t.Errorf("status = %v, want completed", out.Status)
	}
	if out.IncompleteDetails != nil {
		t.Errorf("incomplete_details = %s, want nil", out.IncompleteDetails)
	}
	if len(out.Output) == 0 {
		t.Fatal("output = empty, want at least one item")
	}
	foundText := false
	for i := range out.Output {
		item := &out.Output[i]
		if item.Content != nil {
			for j := range item.Content.ContentBlocks {
				block := &item.Content.ContentBlocks[j]
				if block.Type == transcode.ResponsesMessageContentBlockTypeOutputText && block.Text != nil {
					foundText = true
					if *block.Text != "Hello, world!" {
						t.Errorf("output text = %q, want Hello, world!", *block.Text)
					}
				}
			}
		}
	}
	if !foundText {
		t.Error("no text output item found")
	}
}

// TestConvertChatResponseResponsesResponseToolCalls verifies that a chat
// response with finish_reason "tool_calls" produces a completed Responses
// response with function call output items.
func TestConvertChatResponseResponsesResponseToolCalls(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-2",
		Object:  "chat.completion",
		Created: 1710000001,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{{
			Index: 0,
			Message: &transcode.ChatMessage{
				Role: transcode.ChatMessageRoleAssistant,
				ChatAssistantMessage: &transcode.ChatAssistantMessage{
					ToolCalls: []transcode.ChatAssistantMessageToolCall{{
						Type: new("function"),
						ID:   new("call_abc123"),
						Function: transcode.ChatAssistantMessageToolCallFunction{
							Name:      new("get_weather"),
							Arguments: `{"location":"Tokyo"}`,
						},
					}},
				},
			},
			FinishReason: new("tool_calls"),
		}},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status == nil || *out.Status != "completed" {
		t.Errorf("status = %v, want completed", out.Status)
	}
	if len(out.Output) == 0 {
		t.Fatal("output = empty, want function call item")
	}
	foundFC := false
	for i := range out.Output {
		item := &out.Output[i]
		if item.Type != nil && *item.Type == transcode.ResponsesMessageTypeFunctionCall {
			foundFC = true
			if item.ResponsesToolMessage == nil || item.ResponsesToolMessage.CallID == nil ||
				*item.ResponsesToolMessage.CallID != "call_abc123" {
				t.Errorf("function call item = %+v, want call_id call_abc123", item)
			}
			if item.ResponsesToolMessage == nil || item.ResponsesToolMessage.Name == nil ||
				*item.ResponsesToolMessage.Name != "get_weather" {
				t.Errorf("function call item name = %v, want get_weather", item.ResponsesToolMessage)
			}
			if item.ResponsesToolMessage == nil || item.ResponsesToolMessage.Arguments == nil ||
				*item.ResponsesToolMessage.Arguments != `{"location":"Tokyo"}` {
				t.Errorf("function call item arguments = %v, want {\"location\":\"Tokyo\"}", item.ResponsesToolMessage)
			}
		}
	}
	if !foundFC {
		t.Error("no function call output item found")
	}
}

// TestConvertChatResponseResponsesResponseLength verifies that a chat
// response with finish_reason "length" produces an incomplete Responses
// response with max_output_tokens incomplete details.
func TestConvertChatResponseResponsesResponseLength(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-3",
		Object:  "chat.completion",
		Created: 1710000002,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{{
			Index: 0,
			Message: &transcode.ChatMessage{
				Role:                 transcode.ChatMessageRoleAssistant,
				ChatAssistantMessage: &transcode.ChatAssistantMessage{},
				Content:              &transcode.ChatMessageContent{ContentStr: new("The answer is")},
			},
			FinishReason: new("length"),
		}},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status == nil || *out.Status != "incomplete" {
		t.Errorf("status = %v, want incomplete", out.Status)
	}
	if out.IncompleteDetails == nil {
		t.Fatal("incomplete_details = nil, want max_output_tokens reason")
	}
	if string(out.IncompleteDetails) != `{"reason":"max_output_tokens"}` {
		t.Errorf("incomplete_details = %s, want max_output_tokens", out.IncompleteDetails)
	}
}

// TestConvertChatResponseResponsesResponseContentFilter verifies that a
// chat response with finish_reason "content_filter" produces an incomplete
// Responses response with a content_filter reason, matching the streaming
// behavior established by TestConvertChatResponseResponsesStreamResponseContentFilter.
func TestConvertChatResponseResponsesResponseContentFilter(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-4",
		Object:  "chat.completion",
		Created: 1710000003,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{{
			Index: 0,
			Message: &transcode.ChatMessage{
				Role:                 transcode.ChatMessageRoleAssistant,
				ChatAssistantMessage: &transcode.ChatAssistantMessage{},
				Content:              &transcode.ChatMessageContent{ContentStr: new("I cannot answer that.")},
			},
			FinishReason: new("content_filter"),
		}},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status == nil || *out.Status != "incomplete" {
		t.Errorf("status = %v, want incomplete", out.Status)
	}
	if out.IncompleteDetails == nil {
		t.Fatal("incomplete_details = nil, want content_filter reason")
	}
	if string(out.IncompleteDetails) != `{"reason":"content_filter"}` {
		t.Errorf("incomplete_details = %s, want content_filter", out.IncompleteDetails)
	}
}

// TestConvertChatResponseResponsesResponseMultipleChoicesMixedFinishReasons
// verifies that when multiple choices have mixed finish reasons, incomplete
// wins over completed (length beats stop).
func TestConvertChatResponseResponsesResponseMultipleChoicesMixedFinishReasons(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-5",
		Object:  "chat.completion",
		Created: 1710000004,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{
			{
				Index: 0,
				Message: &transcode.ChatMessage{
					Role:                 transcode.ChatMessageRoleAssistant,
					ChatAssistantMessage: &transcode.ChatAssistantMessage{},
					Content:              &transcode.ChatMessageContent{ContentStr: new("First answer")},
				},
				FinishReason: new("stop"),
			},
			{
				Index: 1,
				Message: &transcode.ChatMessage{
					Role:                 transcode.ChatMessageRoleAssistant,
					ChatAssistantMessage: &transcode.ChatAssistantMessage{},
					Content:              &transcode.ChatMessageContent{ContentStr: new("Second answer")},
				},
				FinishReason: new("length"),
			},
		},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status == nil || *out.Status != "incomplete" {
		t.Errorf("status = %v, want incomplete (length beats stop)", out.Status)
	}
	if out.IncompleteDetails == nil || string(out.IncompleteDetails) != `{"reason":"max_output_tokens"}` {
		t.Errorf("incomplete_details = %s, want max_output_tokens", out.IncompleteDetails)
	}
	if len(out.Output) != 2 {
		t.Errorf("output items = %d, want 2", len(out.Output))
	}
}

// TestConvertChatResponseResponsesResponseMultipleChoicesContentFilterWins
// verifies that content_filter also wins over completed when mixed with stop.
func TestConvertChatResponseResponsesResponseMultipleChoicesContentFilterWins(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-6",
		Object:  "chat.completion",
		Created: 1710000005,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{
			{
				Index: 0,
				Message: &transcode.ChatMessage{
					Role:                 transcode.ChatMessageRoleAssistant,
					ChatAssistantMessage: &transcode.ChatAssistantMessage{},
					Content:              &transcode.ChatMessageContent{ContentStr: new("Safe answer")},
				},
				FinishReason: new("stop"),
			},
			{
				Index: 1,
				Message: &transcode.ChatMessage{
					Role:                 transcode.ChatMessageRoleAssistant,
					ChatAssistantMessage: &transcode.ChatAssistantMessage{},
					Content:              &transcode.ChatMessageContent{ContentStr: new("Filtered answer")},
				},
				FinishReason: new("content_filter"),
			},
		},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status == nil || *out.Status != "incomplete" {
		t.Errorf("status = %v, want incomplete (content_filter beats stop)", out.Status)
	}
	if out.IncompleteDetails == nil || string(out.IncompleteDetails) != `{"reason":"content_filter"}` {
		t.Errorf("incomplete_details = %s, want content_filter", out.IncompleteDetails)
	}
}

// TestConvertChatResponseResponsesResponseLengthContentFilter verifies that
// when length and content_filter finish reasons appear across different
// choices, content_filter takes precedence.
func TestConvertChatResponseResponsesResponseLengthContentFilter(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-14",
		Object:  "chat.completion",
		Created: 1710000014,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{
			{
				Index: 0,
				Message: &transcode.ChatMessage{
					Role:                 transcode.ChatMessageRoleAssistant,
					ChatAssistantMessage: &transcode.ChatAssistantMessage{},
					Content:              &transcode.ChatMessageContent{ContentStr: new("First answer")},
				},
				FinishReason: new("length"),
			},
			{
				Index: 1,
				Message: &transcode.ChatMessage{
					Role:                 transcode.ChatMessageRoleAssistant,
					ChatAssistantMessage: &transcode.ChatAssistantMessage{},
					Content:              &transcode.ChatMessageContent{ContentStr: new("Filtered answer")},
				},
				FinishReason: new("content_filter"),
			},
		},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status == nil || *out.Status != "incomplete" {
		t.Errorf("status = %v, want incomplete", out.Status)
	}
	if out.IncompleteDetails == nil || string(out.IncompleteDetails) != `{"reason":"content_filter"}` {
		t.Errorf("incomplete_details = %s, want content_filter (content_filter wins over length)", out.IncompleteDetails)
	}
}

// TestConvertChatResponseResponsesResponseFunctionCallFinishReason verifies
// that the legacy "function_call" finish reason maps to completed, same as
// "tool_calls".
func TestConvertChatResponseResponsesResponseFunctionCallFinishReason(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-15",
		Object:  "chat.completion",
		Created: 1710000015,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{{
			Index: 0,
			Message: &transcode.ChatMessage{
				Role:                 transcode.ChatMessageRoleAssistant,
				ChatAssistantMessage: &transcode.ChatAssistantMessage{},
				Content:              &transcode.ChatMessageContent{ContentStr: new("Done")},
			},
			FinishReason: new("function_call"),
		}},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status == nil || *out.Status != "completed" {
		t.Errorf("status = %v, want completed for function_call finish reason", out.Status)
	}
}

// TestConvertChatResponseResponsesResponseEmptyChoices verifies that a
// response with no choices produces a response with no output items and
// no status set.
func TestConvertChatResponseResponsesResponseEmptyChoices(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-7",
		Object:  "chat.completion",
		Created: 1710000006,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status != nil {
		t.Errorf("status = %v, want nil for empty choices", out.Status)
	}
	if len(out.Output) != 0 {
		t.Errorf("output items = %d, want 0", len(out.Output))
	}
}

// TestConvertChatResponseResponsesResponseNilMessageChoice verifies that
// a choice with a nil message is skipped without panicking and that the
// choice's finish reason still contributes to the response status.
func TestConvertChatResponseResponsesResponseNilMessageChoice(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-8",
		Object:  "chat.completion",
		Created: 1710000007,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{
			{Index: 0, Message: nil, FinishReason: new("length")},
		},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status == nil || *out.Status != "incomplete" {
		t.Errorf("status = %v, want incomplete (length from nil-message choice)", out.Status)
	}
	if out.IncompleteDetails == nil || string(out.IncompleteDetails) != `{"reason":"max_output_tokens"}` {
		t.Errorf("incomplete_details = %s, want max_output_tokens", out.IncompleteDetails)
	}
	if len(out.Output) != 0 {
		t.Errorf("output items = %d, want 0 for nil message", len(out.Output))
	}
}

// TestConvertChatResponseResponsesResponseUsage verifies that token usage
// is correctly mapped from chat LLM usage to responses usage.
func TestConvertChatResponseResponsesResponseUsage(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-9",
		Object:  "chat.completion",
		Created: 1710000008,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{{
			Index: 0,
			Message: &transcode.ChatMessage{
				Role:                 transcode.ChatMessageRoleAssistant,
				ChatAssistantMessage: &transcode.ChatAssistantMessage{},
				Content:              &transcode.ChatMessageContent{ContentStr: new("Answer")},
			},
			FinishReason: new("stop"),
		}},
		Usage: &transcode.ChatLLMUsage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
			PromptTokensDetails: &transcode.ChatPromptTokensDetails{
				CachedTokens: 2,
			},
			CompletionTokensDetails: &transcode.ChatCompletionTokensDetails{
				ReasoningTokens: 1,
			},
		},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Usage == nil {
		t.Fatal("usage = nil, want mapped usage")
	}
	if out.Usage.InputTokens != 10 {
		t.Errorf("input_tokens = %d, want 10", out.Usage.InputTokens)
	}
	if out.Usage.OutputTokens != 5 {
		t.Errorf("output_tokens = %d, want 5", out.Usage.OutputTokens)
	}
	if out.Usage.TotalTokens != 15 {
		t.Errorf("total_tokens = %d, want 15", out.Usage.TotalTokens)
	}
	if out.Usage.InputTokensDetails == nil || out.Usage.InputTokensDetails.CachedTokens != 2 {
		t.Errorf("input_tokens_details.cached_tokens = %v, want 2", out.Usage.InputTokensDetails)
	}
	if out.Usage.OutputTokensDetails == nil || out.Usage.OutputTokensDetails.ReasoningTokens != 1 {
		t.Errorf("output_tokens_details.reasoning_tokens = %v, want 1", out.Usage.OutputTokensDetails)
	}
}

// TestConvertChatResponseResponsesResponseRefusal verifies that a refusal
// message is correctly converted to a refusal output item.
func TestConvertChatResponseResponsesResponseRefusal(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-10",
		Object:  "chat.completion",
		Created: 1710000009,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{{
			Index: 0,
			Message: &transcode.ChatMessage{
				Role: transcode.ChatMessageRoleAssistant,
				ChatAssistantMessage: &transcode.ChatAssistantMessage{
					Refusal: new("I cannot fulfill this request."),
				},
			},
			FinishReason: new("stop"),
		}},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status == nil || *out.Status != "completed" {
		t.Errorf("status = %v, want completed", out.Status)
	}
	if len(out.Output) == 0 {
		t.Fatal("output = empty, want refusal item")
	}
	foundRefusal := false
	for i := range out.Output {
		item := &out.Output[i]
		if item.Type != nil && *item.Type == transcode.ResponsesMessageTypeRefusal {
			foundRefusal = true
			if item.Content == nil || len(item.Content.ContentBlocks) == 0 ||
				item.Content.ContentBlocks[0].Text == nil ||
				*item.Content.ContentBlocks[0].Text != "I cannot fulfill this request." {
				t.Errorf("refusal item content = %+v, want refusal text", item.Content)
			}
		}
	}
	if !foundRefusal {
		t.Error("no refusal output item found")
	}
}

// TestConvertChatResponseResponsesResponseOutputItems verifies that
// assistant text, reasoning, and tool call items are all correctly
// converted in the non-streaming response direction.
func TestConvertChatResponseResponsesResponseOutputItems(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-11",
		Object:  "chat.completion",
		Created: 1710000010,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{{
			Index: 0,
			Message: &transcode.ChatMessage{
				Role: transcode.ChatMessageRoleAssistant,
				ChatAssistantMessage: &transcode.ChatAssistantMessage{
					Reasoning: new("Let me think step by step..."),
					ToolCalls: []transcode.ChatAssistantMessageToolCall{{
						Type: new("function"),
						ID:   new("call_xyz"),
						Function: transcode.ChatAssistantMessageToolCallFunction{
							Name:      new("search"),
							Arguments: `{"query":"weather"}`,
						},
					}},
				},
				Content: &transcode.ChatMessageContent{ContentStr: new("Here is the answer.")},
			},
			FinishReason: new("stop"),
		}},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status == nil || *out.Status != "completed" {
		t.Errorf("status = %v, want completed", out.Status)
	}
	if len(out.Output) != 3 {
		t.Fatalf("output items = %d, want 3 (reasoning, tool call, text)", len(out.Output))
	}
}

// TestConvertChatResponseResponsesResponseEnvelopeFields verifies that
// the envelope fields (ID, Object, CreatedAt, Model) are correctly mapped.
func TestConvertChatResponseResponsesResponseEnvelopeFields(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-12",
		Object:  "chat.completion",
		Created: 1710000011,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{{
			Index: 0,
			Message: &transcode.ChatMessage{
				Role:                 transcode.ChatMessageRoleAssistant,
				ChatAssistantMessage: &transcode.ChatAssistantMessage{},
				Content:              &transcode.ChatMessageContent{ContentStr: new("Hi")},
			},
			FinishReason: new("stop"),
		}},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.ID != resp.ID {
		t.Errorf("id = %q, want %q", out.ID, resp.ID)
	}
	if out.Object != "response" {
		t.Errorf("object = %q, want response", out.Object)
	}
	if out.CreatedAt != resp.Created {
		t.Errorf("created_at = %d, want %d", out.CreatedAt, resp.Created)
	}
	if out.Model != resp.Model {
		t.Errorf("model = %q, want %q", out.Model, resp.Model)
	}
}

// TestConvertChatResponseResponsesResponseUnmappedFinishReason verifies
// that an unmapped finish reason (e.g. "unknown_reason") does not set
// any status on the response.
func TestConvertChatResponseResponsesResponseUnmappedFinishReason(t *testing.T) {
	resp := &transcode.ChatResponse{
		ID:      "chatcmpl-13",
		Object:  "chat.completion",
		Created: 1710000012,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{{
			Index: 0,
			Message: &transcode.ChatMessage{
				Role:                 transcode.ChatMessageRoleAssistant,
				ChatAssistantMessage: &transcode.ChatAssistantMessage{},
				Content:              &transcode.ChatMessageContent{ContentStr: new("Answer")},
			},
			FinishReason: new("unknown_reason"),
		}},
	}
	out := transcode.ConvertChatResponseResponsesResponse(resp)

	if out.Status != nil {
		t.Errorf("status = %v, want nil for unmapped finish reason", out.Status)
	}
	if out.IncompleteDetails != nil {
		t.Errorf("incomplete_details = %s, want nil for unmapped finish reason", out.IncompleteDetails)
	}
}
