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

// TestConvertChatRequestResponsesRequest verifies the chat request to
// responses request conversion: developer messages keep their role, assistant
// tool calls become function call items, tool messages become function call
// output items, and parameters carry over.
func TestConvertChatRequestResponsesRequest(t *testing.T) {
	f := mustChatCompletions(t)
	out := transcode.ConvertChatRequestResponsesRequest(&f.Request)

	if out.Model != f.Request.Model {
		t.Errorf("model = %q, want %q", out.Model, f.Request.Model)
	}
	if out.MaxOutputTokens == nil || *out.MaxOutputTokens != *f.Request.MaxCompletionTokens {
		t.Errorf("max_output_tokens = %v, want %v", out.MaxOutputTokens, f.Request.MaxCompletionTokens)
	}
	if out.ToolChoice == nil || out.ToolChoice.Str == nil || *out.ToolChoice.Str != "auto" {
		t.Errorf("tool_choice = %+v, want auto", out.ToolChoice)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name == nil || *out.Tools[0].Name != "get_weather" {
		t.Errorf("tools = %+v, want one function tool", out.Tools)
	}
	if out.Reasoning == nil || out.Reasoning.Effort == nil || *out.Reasoning.Effort != "medium" {
		t.Errorf("reasoning = %+v, want effort medium", out.Reasoning)
	}
	if len(out.Input) != 4 {
		t.Fatalf("input items = %d, want 4", len(out.Input))
	}
	developer := out.Input[0]
	if developer.Type == nil || *developer.Type != transcode.ResponsesMessageTypeMessage ||
		developer.Role == nil || *developer.Role != transcode.ResponsesMessageRoleDeveloper {
		t.Errorf("first item = %+v, want developer message", developer)
	}
	var sawFunctionCall, sawFunctionCallOutput bool
	for i := range out.Input {
		item := &out.Input[i]
		if item.Type == nil {
			continue
		}
		switch *item.Type {
		case transcode.ResponsesMessageTypeFunctionCall:
			sawFunctionCall = true
			if item.CallID == nil || *item.CallID != "call_abc123" {
				t.Errorf("function_call call_id = %v", item.CallID)
			}
			if item.Name == nil || *item.Name != "get_weather" {
				t.Errorf("function_call name = %v", item.Name)
			}
			if item.Arguments == nil || *item.Arguments != `{"location":"Tokyo"}` {
				t.Errorf("function_call arguments = %v", item.Arguments)
			}
		case transcode.ResponsesMessageTypeFunctionCallOutput:
			sawFunctionCallOutput = true
			if item.CallID == nil || *item.CallID != "call_abc123" || item.Output == nil || item.Output.Str == nil {
				t.Errorf("function_call_output = %+v", item)
			}
		}
	}
	if !sawFunctionCall || !sawFunctionCallOutput {
		t.Errorf("tool coverage: function_call=%v output=%v", sawFunctionCall, sawFunctionCallOutput)
	}
}

// TestConvertChatResponseResponsesResponse verifies the chat response to
// responses response conversion: finish reason drives status, reasoning and
// tool calls become output items, and usage maps across.
func TestConvertChatRequestResponsesRequestContentBlocks(t *testing.T) {
	req := transcode.ChatRequest{
		Model: "gpt-4.1",
		Messages: []transcode.ChatMessage{
			{Role: transcode.ChatMessageRoleDeveloper, Content: &transcode.ChatMessageContent{ContentStr: new("be concise")}},
			{Role: transcode.ChatMessageRoleUser, Content: &transcode.ChatMessageContent{ContentBlocks: []transcode.ChatContentBlock{
				{Type: transcode.ChatContentBlockTypeText, Text: new("what is this?")},
				{Type: transcode.ChatContentBlockTypeImage, ImageURL: &transcode.ChatInputImage{URL: "https://example.com/a.png"}},
				{Type: transcode.ChatContentBlockTypeRefusal, Refusal: new("no")},
			}}},
		},
		ToolChoice: &transcode.ChatToolChoice{Struct: &transcode.ChatToolChoiceStruct{
			Type:     "function",
			Function: &transcode.ChatToolChoiceFunction{Name: "get_weather"},
		}},
	}
	out := transcode.ConvertChatRequestResponsesRequest(&req)

	if len(out.Input) != 2 {
		t.Fatalf("input items = %d, want 2", len(out.Input))
	}
	if out.Input[0].Role == nil || *out.Input[0].Role != transcode.ResponsesMessageRoleDeveloper {
		t.Errorf("first item = %+v, want developer role", out.Input[0])
	}
	blocks := out.Input[1].Content.ContentBlocks
	if len(blocks) != 3 {
		t.Fatalf("user blocks = %d, want 3", len(blocks))
	}
	if blocks[0].Type != transcode.ResponsesMessageContentBlockTypeInputText || blocks[0].Text == nil {
		t.Errorf("block 0 = %+v, want input_text", blocks[0])
	}
	if blocks[1].Type != transcode.ResponsesMessageContentBlockTypeInputImage || blocks[1].ImageURL == nil || *blocks[1].ImageURL != "https://example.com/a.png" {
		t.Errorf("block 1 = %+v, want input_image with url", blocks[1])
	}
	if blocks[2].Type != transcode.ResponsesMessageContentBlockTypeRefusal || blocks[2].Text == nil {
		t.Errorf("block 2 = %+v, want refusal", blocks[2])
	}
	if out.ToolChoice == nil || out.ToolChoice.Struct == nil || out.ToolChoice.Struct.Name == nil || *out.ToolChoice.Struct.Name != "get_weather" {
		t.Errorf("tool_choice = %+v, want named function choice", out.ToolChoice)
	}

	// Assistant refusal becomes a refusal item; empty tool names are dropped.
	refusal := transcode.ChatRequest{
		Model: "gpt-4.1",
		Messages: []transcode.ChatMessage{{
			Role: transcode.ChatMessageRoleAssistant,
			ChatAssistantMessage: &transcode.ChatAssistantMessage{
				Refusal: new("I cannot answer that"),
				ToolCalls: []transcode.ChatAssistantMessageToolCall{{
					ID:       new("call_1"),
					Type:     new("function"),
					Function: transcode.ChatAssistantMessageToolCallFunction{Name: new(""), Arguments: ""},
				}},
			},
		}},
		Tools: []transcode.ChatTool{{Type: transcode.ChatToolTypeFunction, Function: &transcode.ChatToolFunction{Name: ""}}},
	}
	refusalOut := transcode.ConvertChatRequestResponsesRequest(&refusal)
	if len(refusalOut.Input) != 2 {
		t.Fatalf("refusal conversion = %+v, want refusal item and function call item", refusalOut.Input)
	}
	if refusalOut.Input[0].Type == nil || *refusalOut.Input[0].Type != transcode.ResponsesMessageTypeRefusal {
		t.Errorf("first item = %+v, want refusal", refusalOut.Input[0])
	}
	call := refusalOut.Input[1]
	if call.Type == nil || *call.Type != transcode.ResponsesMessageTypeFunctionCall ||
		call.CallID == nil || *call.CallID != "call_1" || call.Name != nil {
		t.Errorf("second item = %+v, want function call with call_id only", call)
	}
	if len(refusalOut.Tools) != 0 {
		t.Errorf("tools = %+v, want empty-name tool dropped", refusalOut.Tools)
	}
}

// TestConvertAnthropicRequestResponsesRequestBlocks verifies the block-form
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

// TestConvertResponsesResponseAnthropicResponseBlocks verifies the block-form
// conversions of the responses to anthropic response direction: encrypted
// reasoning becomes redacted_thinking, and an empty-arguments function call
// gets an empty object input.
func TestConvertContentOnlyAssistantMessage(t *testing.T) {
	chat := transcode.ChatRequest{
		Model: "gpt-4.1",
		Messages: []transcode.ChatMessage{
			{Role: transcode.ChatMessageRoleUser, Content: &transcode.ChatMessageContent{ContentStr: new("hi")}},
			{Role: transcode.ChatMessageRoleAssistant, Content: &transcode.ChatMessageContent{ContentStr: new("hello there")}},
		},
	}
	req := transcode.ConvertChatRequestResponsesRequest(&chat)
	if len(req.Input) != 2 {
		t.Fatalf("input items = %d, want 2", len(req.Input))
	}
	assistant := req.Input[1]
	if assistant.Type == nil || *assistant.Type != transcode.ResponsesMessageTypeMessage ||
		assistant.Role == nil || *assistant.Role != transcode.ResponsesMessageRoleAssistant ||
		assistant.Content == nil || len(assistant.Content.ContentBlocks) != 1 ||
		assistant.Content.ContentBlocks[0].Type != transcode.ResponsesMessageContentBlockTypeOutputText ||
		assistant.Content.ContentBlocks[0].Text == nil || *assistant.Content.ContentBlocks[0].Text != "hello there" {
		t.Errorf("assistant item = %+v", assistant)
	}

	resp := transcode.ChatResponse{
		ID:      "chatcmpl-1",
		Object:  "chat.completion",
		Created: 1,
		Model:   "gpt-4.1",
		Choices: []transcode.ChatChoice{{
			Index:        0,
			FinishReason: new("stop"),
			Message: &transcode.ChatMessage{
				Role:    transcode.ChatMessageRoleAssistant,
				Content: &transcode.ChatMessageContent{ContentStr: new("the answer")},
			},
		}},
	}
	out := transcode.ConvertChatResponseResponsesResponse(&resp)
	if len(out.Output) != 1 {
		t.Fatalf("output items = %d, want 1", len(out.Output))
	}
	if out.Output[0].Content == nil || len(out.Output[0].Content.ContentBlocks) != 1 ||
		out.Output[0].Content.ContentBlocks[0].Text == nil || *out.Output[0].Content.ContentBlocks[0].Text != "the answer" {
		t.Errorf("output item = %+v", out.Output[0])
	}
	if out.Status == nil || *out.Status != "completed" {
		t.Errorf("status = %v, want completed", out.Status)
	}
}

// TestConvertChatRequestResponsesRequestMaxTokensFallback verifies that the
// legacy max_tokens field is used when max_completion_tokens is absent, so
// client output limits are never silently discarded.
func TestConvertChatRequestResponsesRequestMaxTokensFallback(t *testing.T) {
	legacy := transcode.ChatRequest{Model: "gpt-4.1", MaxTokens: new(512)}
	out := transcode.ConvertChatRequestResponsesRequest(&legacy)
	if out.MaxOutputTokens == nil || *out.MaxOutputTokens != 512 {
		t.Errorf("max_output_tokens = %v, want 512 (from max_tokens)", out.MaxOutputTokens)
	}

	// max_completion_tokens wins when both are set.
	both := transcode.ChatRequest{Model: "gpt-4.1", MaxCompletionTokens: new(256), MaxTokens: new(512)}
	out = transcode.ConvertChatRequestResponsesRequest(&both)
	if out.MaxOutputTokens == nil || *out.MaxOutputTokens != 256 {
		t.Errorf("max_output_tokens = %v, want 256 (max_completion_tokens wins)", out.MaxOutputTokens)
	}

	// Neither set: nil, no fabricated limit.
	none := transcode.ChatRequest{Model: "gpt-4.1"}
	out = transcode.ConvertChatRequestResponsesRequest(&none)
	if out.MaxOutputTokens != nil {
		t.Errorf("max_output_tokens = %v, want nil", out.MaxOutputTokens)
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

// TestConvertChatRequestResponsesRequestMultiBlockToolResult verifies a tool
// message with multiple content blocks maps to a function call output with
// output content blocks instead of being dropped.
func TestConvertChatRequestResponsesRequestMultiBlockToolResult(t *testing.T) {
	req := transcode.ChatRequest{
		Model: "gpt-4.1",
		Messages: []transcode.ChatMessage{{
			Role:            transcode.ChatMessageRoleTool,
			ChatToolMessage: &transcode.ChatToolMessage{ToolCallID: new("call_1")},
			Content: &transcode.ChatMessageContent{ContentBlocks: []transcode.ChatContentBlock{
				{Type: transcode.ChatContentBlockTypeText, Text: new("first part")},
				{Type: transcode.ChatContentBlockTypeText, Text: new("second part")},
			}},
		}},
	}
	out := transcode.ConvertChatRequestResponsesRequest(&req)
	if len(out.Input) != 1 {
		t.Fatalf("input = %+v", out.Input)
	}
	item := out.Input[0]
	if item.ResponsesToolMessage == nil || item.ResponsesToolMessage.Output == nil {
		t.Fatalf("tool message = %+v, want output", item.ResponsesToolMessage)
	}
	output := item.ResponsesToolMessage.Output
	if output.Str != nil || len(output.Blocks) != 2 {
		t.Fatalf("output = %+v, want 2 blocks", output)
	}
	if output.Blocks[0].Text == nil || *output.Blocks[0].Text != "first part" ||
		output.Blocks[1].Text == nil || *output.Blocks[1].Text != "second part" {
		t.Errorf("output blocks = %+v", output.Blocks)
	}
}
