package transcode_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// Anthropic <-> Responses conversion tests.

func TestConvertAnthropicRequestResponsesRequest(t *testing.T) {
	f := mustAnthropic(t)
	out := transcode.ConvertAnthropicRequestResponsesRequest(&f.Request)

	if out.Model != f.Request.Model {
		t.Errorf("model = %q, want %q", out.Model, f.Request.Model)
	}
	if out.MaxOutputTokens == nil || *out.MaxOutputTokens != f.Request.MaxTokens {
		t.Errorf("max_output_tokens = %v, want %d", out.MaxOutputTokens, f.Request.MaxTokens)
	}
	if out.Temperature == nil || *out.Temperature != 0.7 {
		t.Errorf("temperature = %v", out.Temperature)
	}
	if out.ToolChoice == nil || out.ToolChoice.Str == nil || *out.ToolChoice.Str != "auto" {
		t.Errorf("tool_choice = %+v, want auto", out.ToolChoice)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name == nil || *out.Tools[0].Name != "get_weather" ||
		out.Tools[0].Parameters == nil {
		t.Errorf("tools = %+v", out.Tools)
	}
	if len(out.Input) != 2 {
		t.Fatalf("input items = %d, want 2 (system, user)", len(out.Input))
	}
	system := out.Input[0]
	if system.Role == nil || *system.Role != transcode.ResponsesMessageRoleSystem ||
		system.Content == nil || system.Content.ContentStr == nil {
		t.Errorf("system item = %+v", system)
	}
	user := out.Input[1]
	if user.Role == nil || *user.Role != transcode.ResponsesMessageRoleUser ||
		user.Content == nil || len(user.Content.ContentBlocks) != 1 ||
		user.Content.ContentBlocks[0].Type != transcode.ResponsesMessageContentBlockTypeInputText {
		t.Errorf("user item = %+v", user)
	}

	// Thinking, tool use, and tool result blocks.
	req := transcode.AnthropicMessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []transcode.AnthropicMessage{{
			Role: transcode.AnthropicMessageRoleAssistant,
			Content: transcode.AnthropicContent{ContentBlocks: []transcode.AnthropicContentBlock{
				{Type: transcode.AnthropicContentBlockTypeThinking, Thinking: new("let me think"), Signature: new("sig_1")},
				{Type: transcode.AnthropicContentBlockTypeToolUse, ID: new("toolu_1"), Name: new("get_weather"), Input: json.RawMessage(`{"location":"Tokyo"}`)},
			}},
		}, {
			Role: transcode.AnthropicMessageRoleUser,
			Content: transcode.AnthropicContent{ContentBlocks: []transcode.AnthropicContentBlock{{
				Type:      transcode.AnthropicContentBlockTypeToolResult,
				ToolUseID: new("toolu_1"),
				Content:   &transcode.AnthropicContent{ContentStr: new("sunny")},
			}}},
		}},
	}
	converted := transcode.ConvertAnthropicRequestResponsesRequest(&req)
	if len(converted.Input) != 3 {
		t.Fatalf("input items = %d, want 3 (reasoning, function_call, function_call_output)", len(converted.Input))
	}
	reasoning := converted.Input[0]
	if reasoning.Type == nil || *reasoning.Type != transcode.ResponsesMessageTypeReasoning ||
		reasoning.Content == nil || len(reasoning.Content.ContentBlocks) != 1 ||
		reasoning.Content.ContentBlocks[0].Text == nil || *reasoning.Content.ContentBlocks[0].Text != "let me think" ||
		reasoning.Content.ContentBlocks[0].Signature == nil || *reasoning.Content.ContentBlocks[0].Signature != "sig_1" {
		t.Errorf("reasoning item = %+v", reasoning)
	}
	call := converted.Input[1]
	if call.Type == nil || *call.Type != transcode.ResponsesMessageTypeFunctionCall ||
		call.CallID == nil || *call.CallID != "toolu_1" ||
		call.Name == nil || *call.Name != "get_weather" ||
		call.Arguments == nil || *call.Arguments != `{"location":"Tokyo"}` {
		t.Errorf("function_call item = %+v", call)
	}
	result := converted.Input[2]
	if result.Type == nil || *result.Type != transcode.ResponsesMessageTypeFunctionCallOutput ||
		result.CallID == nil || *result.CallID != "toolu_1" ||
		result.Output == nil || result.Output.Str == nil || *result.Output.Str != "sunny" {
		t.Errorf("function_call_output item = %+v", result)
	}
}

// TestConvertResponsesResponseAnthropicResponse verifies the responses
// response to anthropic response conversion: output items become content
// blocks, status maps to stop reason, and usage maps across.
func TestConvertResponsesResponseAnthropicResponse(t *testing.T) {
	f := mustResponses(t)
	out := transcode.ConvertResponsesResponseAnthropicResponse(&f.Response)

	if out.ID != f.Response.ID || out.Type != "message" || out.Role != "assistant" || out.Model != f.Response.Model {
		t.Errorf("envelope = %+v", out)
	}
	if len(out.Content) != 3 {
		t.Fatalf("content blocks = %d, want 3 (thinking, tool_use, text)", len(out.Content))
	}
	if out.Content[0].Type != transcode.AnthropicContentBlockTypeThinking ||
		out.Content[0].Thinking == nil || !strings.Contains(*out.Content[0].Thinking, "get_weather") {
		t.Errorf("first block = %+v, want thinking", out.Content[0])
	}
	if out.Content[1].Type != transcode.AnthropicContentBlockTypeToolUse ||
		out.Content[1].ID == nil || *out.Content[1].ID != "call_abc123" ||
		out.Content[1].Name == nil || *out.Content[1].Name != "get_weather" ||
		string(out.Content[1].Input) != `{"location":"Tokyo"}` {
		t.Errorf("second block = %+v, want tool_use", out.Content[1])
	}
	if out.Content[2].Type != transcode.AnthropicContentBlockTypeText ||
		out.Content[2].Text == nil || !strings.Contains(*out.Content[2].Text, "sunny") {
		t.Errorf("third block = %+v, want text", out.Content[2])
	}
	if out.StopReason != transcode.AnthropicStopReasonToolUse {
		t.Errorf("stop_reason = %q, want tool_use", out.StopReason)
	}
	if out.Usage == nil || out.Usage.InputTokens != 45 || out.Usage.OutputTokens != 25 {
		t.Fatalf("usage = %+v", out.Usage)
	}
	if out.Usage.OutputTokensDetails == nil || out.Usage.OutputTokensDetails.ThinkingTokens != 12 {
		t.Errorf("thinking tokens = %+v, want 12", out.Usage.OutputTokensDetails)
	}

	// Cached input tokens are subtracted from the input count.
	usage := transcode.ResponsesResponseUsage{
		InputTokens:        50,
		InputTokensDetails: &transcode.ResponsesResponseInputTokens{CachedTokens: 10},
		OutputTokens:       20,
	}
	anthropicUsage := transcode.ConvertResponsesResponseAnthropicResponse(&transcode.ResponsesResponse{Usage: &usage}).Usage
	if anthropicUsage == nil || anthropicUsage.InputTokens != 40 || anthropicUsage.CacheReadInputTokens != 10 {
		t.Errorf("anthropic usage = %+v, want input 40 cache_read 10", anthropicUsage)
	}
}

// TestConvertChatResponseResponsesStreamResponse feeds the chat completions
// stream fixture through the stateful accumulator and verifies the responses
// event sequence, including reasoning and text item lifecycles, the terminal
// event, and usage mapping.
func TestConversionRoundTripConversions(t *testing.T) {
	responses := mustResponses(t)
	anthropic := mustAnthropic(t)

	// Anthropic request -> responses request -> anthropic request.
	responsesReq := transcode.ConvertAnthropicRequestResponsesRequest(&anthropic.Request)
	backAnthropic := transcode.ConvertResponsesRequestChatRequest(responsesReq)
	if backAnthropic.Model != anthropic.Request.Model {
		t.Errorf("anthropic round trip model = %q, want %q", backAnthropic.Model, anthropic.Request.Model)
	}
	if len(backAnthropic.Messages) < 2 || backAnthropic.Messages[0].Role != transcode.ChatMessageRoleSystem {
		t.Errorf("anthropic round trip messages = %+v", backAnthropic.Messages)
	}

	// Responses response -> anthropic response -> responses response.
	anthropicResp := transcode.ConvertResponsesResponseAnthropicResponse(&responses.Response)
	if anthropicResp.StopReason != transcode.AnthropicStopReasonToolUse || len(anthropicResp.Content) != 3 {
		t.Errorf("responses response round trip content = %+v", anthropicResp)
	}
}

func TestConvertAnthropicRequestResponsesRequestBlocks(t *testing.T) {
	req := transcode.AnthropicMessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		System: &transcode.AnthropicContent{ContentBlocks: []transcode.AnthropicContentBlock{
			{Type: transcode.AnthropicContentBlockTypeText, Text: new("part one")},
			{Type: transcode.AnthropicContentBlockTypeText, Text: new("part two")},
		}},
		Messages: []transcode.AnthropicMessage{{
			Role: transcode.AnthropicMessageRoleUser,
			Content: transcode.AnthropicContent{ContentBlocks: []transcode.AnthropicContentBlock{
				{Type: transcode.AnthropicContentBlockTypeImage, Source: &transcode.AnthropicSource{Type: "url", URL: new("https://example.com/i.png")}},
				{Type: transcode.AnthropicContentBlockTypeImage, Source: &transcode.AnthropicSource{Type: "base64", MediaType: new("image/png"), Data: new("aGVsbG8=")}},
			}},
		}},
		ToolChoice: &transcode.AnthropicToolChoice{Type: "tool", Name: "get_weather"},
	}
	out := transcode.ConvertAnthropicRequestResponsesRequest(&req)

	if len(out.Input) != 3 {
		t.Fatalf("input items = %d, want 3 (system, user images, user images)", len(out.Input))
	}
	system := out.Input[0]
	if system.Content == nil || len(system.Content.ContentBlocks) != 2 {
		t.Errorf("system item = %+v, want two text blocks", system)
	}
	imageItem := out.Input[1]
	if imageItem.Content == nil || len(imageItem.Content.ContentBlocks) != 1 ||
		imageItem.Content.ContentBlocks[0].Type != transcode.ResponsesMessageContentBlockTypeInputImage ||
		imageItem.Content.ContentBlocks[0].ImageURL == nil || *imageItem.Content.ContentBlocks[0].ImageURL != "https://example.com/i.png" {
		t.Errorf("image url item = %+v", imageItem)
	}
	dataItem := out.Input[2]
	if dataItem.Content == nil || len(dataItem.Content.ContentBlocks) != 1 ||
		dataItem.Content.ContentBlocks[0].ImageData == nil ||
		!strings.Contains(string(dataItem.Content.ContentBlocks[0].ImageData), "aGVsbG8=") {
		t.Errorf("image data item = %+v", dataItem)
	}
	if out.ToolChoice == nil || out.ToolChoice.Struct == nil || out.ToolChoice.Struct.Name == nil || *out.ToolChoice.Struct.Name != "get_weather" {
		t.Errorf("tool_choice = %+v, want named function", out.ToolChoice)
	}

	// A tool result carrying block content maps to a blocks-form output; an
	// unknown tool choice type falls back to auto.
	result := transcode.AnthropicMessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []transcode.AnthropicMessage{{
			Role: transcode.AnthropicMessageRoleUser,
			Content: transcode.AnthropicContent{ContentBlocks: []transcode.AnthropicContentBlock{{
				Type:      transcode.AnthropicContentBlockTypeToolResult,
				ToolUseID: new("toolu_1"),
				Content:   &transcode.AnthropicContent{ContentStr: new("sunny")},
			}}},
		}},
		ToolChoice: &transcode.AnthropicToolChoice{Type: "bogus"},
	}
	resultOut := transcode.ConvertAnthropicRequestResponsesRequest(&result)
	if len(resultOut.Input) != 1 || resultOut.Input[0].Type == nil || *resultOut.Input[0].Type != transcode.ResponsesMessageTypeFunctionCallOutput ||
		resultOut.Input[0].Output == nil || resultOut.Input[0].Output.Str == nil || *resultOut.Input[0].Output.Str != "sunny" {
		t.Errorf("tool result = %+v", resultOut.Input)
	}
	if resultOut.ToolChoice == nil || resultOut.ToolChoice.Str == nil || *resultOut.ToolChoice.Str != "auto" {
		t.Errorf("tool_choice = %+v, want auto fallback", resultOut.ToolChoice)
	}
}

// TestConvertResponsesRequestChatRequestContentBlocks verifies the block-form
// conversions of the responses request direction: multi-block content stays
// block-form, image blocks become image_url, refusal items map to refusal
// messages, and a tool choice naming an existing tool survives.
func TestConvertResponsesResponseAnthropicResponseBlocks(t *testing.T) {
	resp := transcode.ResponsesResponse{
		ID:    "resp_1",
		Model: "gpt-4.1",
		Output: []transcode.ResponsesMessage{
			{
				ID:                 new("rs_1"),
				Type:               new(transcode.ResponsesMessageTypeReasoning),
				ResponsesReasoning: &transcode.ResponsesReasoning{EncryptedContent: new("encrypted-bytes")},
			},
			{
				ID:   new("fc_1"),
				Type: new(transcode.ResponsesMessageTypeFunctionCall),
				ResponsesToolMessage: &transcode.ResponsesToolMessage{
					CallID: new("call_1"),
					Name:   new("f"),
				},
			},
		},
	}
	out := transcode.ConvertResponsesResponseAnthropicResponse(&resp)
	if len(out.Content) != 2 {
		t.Fatalf("content = %d, want 2", len(out.Content))
	}
	if out.Content[0].Type != transcode.AnthropicContentBlockTypeRedactedThinking || out.Content[0].Data == nil || *out.Content[0].Data != "encrypted-bytes" {
		t.Errorf("first block = %+v, want redacted_thinking", out.Content[0])
	}
	if out.Content[1].Type != transcode.AnthropicContentBlockTypeToolUse || string(out.Content[1].Input) != `{}` {
		t.Errorf("second block = %+v, want tool_use with empty input", out.Content[1])
	}
}

// TestConvertRemainingBranches covers the remaining conversion branches: tool
// result block content, redacted thinking, unknown finish reasons, and stream
// options passthrough.
func TestConvertRemainingBranches(t *testing.T) {
	// Tool result with block content maps to a blocks-form output.
	blocksResult := transcode.ConvertAnthropicRequestResponsesRequest(&transcode.AnthropicMessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []transcode.AnthropicMessage{{
			Role: transcode.AnthropicMessageRoleUser,
			Content: transcode.AnthropicContent{ContentBlocks: []transcode.AnthropicContentBlock{{
				Type:      transcode.AnthropicContentBlockTypeToolResult,
				ToolUseID: new("toolu_1"),
				Content: &transcode.AnthropicContent{ContentBlocks: []transcode.AnthropicContentBlock{
					{Type: transcode.AnthropicContentBlockTypeText, Text: new("a")},
					{Type: transcode.AnthropicContentBlockTypeText, Text: new("b")},
				}},
			}}},
		}},
	})
	if len(blocksResult.Input) != 1 || blocksResult.Input[0].Output == nil ||
		len(blocksResult.Input[0].Output.Blocks) != 2 {
		t.Errorf("tool result blocks = %+v", blocksResult.Input)
	}

	// Redacted thinking becomes a reasoning item with encrypted content.
	redacted := transcode.ConvertAnthropicRequestResponsesRequest(&transcode.AnthropicMessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []transcode.AnthropicMessage{{
			Role: transcode.AnthropicMessageRoleAssistant,
			Content: transcode.AnthropicContent{ContentBlocks: []transcode.AnthropicContentBlock{{
				Type: transcode.AnthropicContentBlockTypeRedactedThinking,
				Data: new("redacted-data"),
			}}},
		}},
	})
	if len(redacted.Input) != 1 || redacted.Input[0].Type == nil ||
		*redacted.Input[0].Type != transcode.ResponsesMessageTypeReasoning ||
		redacted.Input[0].EncryptedContent == nil || *redacted.Input[0].EncryptedContent != "redacted-data" {
		t.Errorf("redacted thinking = %+v", redacted.Input)
	}

	// An unknown finish reason leaves the status unset.
	resp := mustChatCompletions(t).Response
	resp.Choices[0].FinishReason = new("content_filter")
	unknown := transcode.ConvertChatResponseResponsesResponse(&resp)
	if unknown.Status != nil {
		t.Errorf("status = %v, want nil for unknown finish reason", unknown.Status)
	}

	// Stream options pass through both request directions.
	responsesReq := transcode.ResponsesRequest{
		Model:         "gpt-4.1",
		Stream:        new(true),
		StreamOptions: &transcode.ResponsesStreamOptions{IncludeUsage: new(true)},
	}
	chatReq := transcode.ConvertResponsesRequestChatRequest(&responsesReq)
	if chatReq.Stream == nil || !*chatReq.Stream || chatReq.StreamOptions == nil || chatReq.StreamOptions.IncludeUsage == nil || !*chatReq.StreamOptions.IncludeUsage {
		t.Errorf("stream options = %+v", chatReq.StreamOptions)
	}
}

// TestConvertAnthropicToolChoiceAnyRequired verifies that Anthropic's "any"
// tool choice maps to the responses API's "required" (passing "any" through
// would be rejected by an OpenAI upstream).
func TestConvertAnthropicToolChoiceAnyRequired(t *testing.T) {
	req := transcode.AnthropicMessageRequest{
		Model:      "claude-3-5-sonnet",
		MaxTokens:  100,
		Messages:   []transcode.AnthropicMessage{{Role: transcode.AnthropicMessageRoleUser, Content: transcode.AnthropicContent{ContentStr: new("hi")}}},
		ToolChoice: &transcode.AnthropicToolChoice{Type: "any"},
	}
	out := transcode.ConvertAnthropicRequestResponsesRequest(&req)
	if out.ToolChoice == nil || out.ToolChoice.Str == nil || *out.ToolChoice.Str != "required" {
		t.Errorf("tool choice = %+v, want required", out.ToolChoice)
	}

	// auto and none pass through unchanged.
	for _, tc := range []string{"auto", "none"} {
		req := transcode.AnthropicMessageRequest{
			Model:      "claude-3-5-sonnet",
			MaxTokens:  100,
			Messages:   []transcode.AnthropicMessage{{Role: transcode.AnthropicMessageRoleUser, Content: transcode.AnthropicContent{ContentStr: new("hi")}}},
			ToolChoice: &transcode.AnthropicToolChoice{Type: tc},
		}
		out := transcode.ConvertAnthropicRequestResponsesRequest(&req)
		if out.ToolChoice == nil || out.ToolChoice.Str == nil || *out.ToolChoice.Str != tc {
			t.Errorf("tool choice = %+v, want %q", out.ToolChoice, tc)
		}
	}
}

// TestConvertResponsesResponseAnthropicResponseMultiBlock verifies that
// multi-block message and reasoning items are fully preserved (no truncation
// to the first block) and reasoning signatures round-trip.
func TestConvertResponsesResponseAnthropicResponseMultiBlock(t *testing.T) {
	resp := transcode.ResponsesResponse{
		ID:     "resp_1",
		Model:  "gpt-4.1",
		Status: new("completed"),
		Output: []transcode.ResponsesMessage{
			{
				ID:   new("rs_1"),
				Type: new(transcode.ResponsesMessageTypeReasoning),
				Role: new(transcode.ResponsesMessageRoleAssistant),
				Content: &transcode.ResponsesMessageContent{ContentBlocks: []transcode.ResponsesMessageContentBlock{
					{Type: transcode.ResponsesMessageContentBlockTypeReasoning, Text: new("think one"), Signature: new("sig_1")},
					{Type: transcode.ResponsesMessageContentBlockTypeReasoning, Text: new("think two"), Signature: new("sig_2")},
				}},
			},
			{
				ID:   new("msg_1"),
				Type: new(transcode.ResponsesMessageTypeMessage),
				Role: new(transcode.ResponsesMessageRoleAssistant),
				Content: &transcode.ResponsesMessageContent{ContentBlocks: []transcode.ResponsesMessageContentBlock{
					{Type: transcode.ResponsesMessageContentBlockTypeOutputText, Text: new("first part")},
					{Type: transcode.ResponsesMessageContentBlockTypeOutputText, Text: new("second part")},
				}},
			},
		},
	}
	out := transcode.ConvertResponsesResponseAnthropicResponse(&resp)
	if len(out.Content) != 4 {
		t.Fatalf("content blocks = %d, want 4: %+v", len(out.Content), out.Content)
	}
	if out.Content[0].Type != transcode.AnthropicContentBlockTypeThinking ||
		out.Content[0].Thinking == nil || *out.Content[0].Thinking != "think one" ||
		out.Content[0].Signature == nil || *out.Content[0].Signature != "sig_1" {
		t.Errorf("block 0 = %+v, want thinking with signature sig_1", out.Content[0])
	}
	if out.Content[1].Type != transcode.AnthropicContentBlockTypeThinking ||
		out.Content[1].Thinking == nil || *out.Content[1].Thinking != "think two" ||
		out.Content[1].Signature == nil || *out.Content[1].Signature != "sig_2" {
		t.Errorf("block 1 = %+v, want thinking with signature sig_2", out.Content[1])
	}
	if out.Content[2].Type != transcode.AnthropicContentBlockTypeText ||
		out.Content[2].Text == nil || *out.Content[2].Text != "first part" {
		t.Errorf("block 2 = %+v, want text", out.Content[2])
	}
	if out.Content[3].Type != transcode.AnthropicContentBlockTypeText ||
		out.Content[3].Text == nil || *out.Content[3].Text != "second part" {
		t.Errorf("block 3 = %+v, want text", out.Content[3])
	}
}

// TestConvertResponsesResponseAnthropicResponseToolUseID verifies a tool use
// item without a call id gets a synthesized id (Anthropic requires one) and
// an item without a name is skipped.
func TestConvertResponsesResponseAnthropicResponseToolUseID(t *testing.T) {
	noID := transcode.ResponsesResponse{
		ID:     "resp_1",
		Model:  "gpt-4.1",
		Status: new("completed"),
		Output: []transcode.ResponsesMessage{{
			ID:   new("fc_1"),
			Type: new(transcode.ResponsesMessageTypeFunctionCall),
			Role: new(transcode.ResponsesMessageRoleAssistant),
			ResponsesToolMessage: &transcode.ResponsesToolMessage{
				Name:      new("get_weather"),
				Arguments: new(`{"location":"Tokyo"}`),
			},
		}},
	}
	out := transcode.ConvertResponsesResponseAnthropicResponse(&noID)
	if len(out.Content) != 1 {
		t.Fatalf("content = %+v, want one tool use", out.Content)
	}
	block := out.Content[0]
	if block.Type != transcode.AnthropicContentBlockTypeToolUse {
		t.Fatalf("block type = %q, want tool_use", block.Type)
	}
	if block.ID == nil || *block.ID == "" {
		t.Errorf("tool use id = %v, want synthesized", block.ID)
	}
	if block.Name == nil || *block.Name != "get_weather" {
		t.Errorf("tool use name = %v, want get_weather", block.Name)
	}

	// Without a name the block cannot be represented: it is skipped.
	noName := transcode.ResponsesResponse{
		ID:     "resp_2",
		Model:  "gpt-4.1",
		Status: new("completed"),
		Output: []transcode.ResponsesMessage{{
			ID:   new("fc_2"),
			Type: new(transcode.ResponsesMessageTypeFunctionCall),
			Role: new(transcode.ResponsesMessageRoleAssistant),
			ResponsesToolMessage: &transcode.ResponsesToolMessage{
				CallID:    new("call_2"),
				Arguments: new(`{}`),
			},
		}},
	}
	out = transcode.ConvertResponsesResponseAnthropicResponse(&noName)
	if len(out.Content) != 0 {
		t.Errorf("content = %+v, want none (tool use without a name)", out.Content)
	}
}

// TestConvertAnthropicRequestResponsesRequestParallelToolUse verifies
// disable_parallel_tool_use maps to the responses parallel_tool_calls
// control.
func TestConvertAnthropicRequestResponsesRequestParallelToolUse(t *testing.T) {
	disabled := transcode.AnthropicMessageRequest{
		Model:      "claude-3-5-sonnet",
		MaxTokens:  100,
		Messages:   []transcode.AnthropicMessage{{Role: transcode.AnthropicMessageRoleUser, Content: transcode.AnthropicContent{ContentStr: new("hi")}}},
		ToolChoice: &transcode.AnthropicToolChoice{Type: "auto", DisableParallelToolUse: new(true)},
	}
	out := transcode.ConvertAnthropicRequestResponsesRequest(&disabled)
	if out.ParallelToolCalls == nil || *out.ParallelToolCalls {
		t.Errorf("parallel_tool_calls = %v, want false (disabled)", out.ParallelToolCalls)
	}

	enabled := transcode.AnthropicMessageRequest{
		Model:      "claude-3-5-sonnet",
		MaxTokens:  100,
		Messages:   []transcode.AnthropicMessage{{Role: transcode.AnthropicMessageRoleUser, Content: transcode.AnthropicContent{ContentStr: new("hi")}}},
		ToolChoice: &transcode.AnthropicToolChoice{Type: "auto", DisableParallelToolUse: new(false)},
	}
	out = transcode.ConvertAnthropicRequestResponsesRequest(&enabled)
	if out.ParallelToolCalls == nil || !*out.ParallelToolCalls {
		t.Errorf("parallel_tool_calls = %v, want true", out.ParallelToolCalls)
	}

	unset := transcode.AnthropicMessageRequest{
		Model:     "claude-3-5-sonnet",
		MaxTokens: 100,
		Messages:  []transcode.AnthropicMessage{{Role: transcode.AnthropicMessageRoleUser, Content: transcode.AnthropicContent{ContentStr: new("hi")}}},
	}
	out = transcode.ConvertAnthropicRequestResponsesRequest(&unset)
	if out.ParallelToolCalls != nil {
		t.Errorf("parallel_tool_calls = %v, want nil", out.ParallelToolCalls)
	}
}

// TestConvertAnthropicRequestResponsesRequestEmptyToolInput verifies a tool
// use with an empty input becomes an empty-object arguments payload, not a
// missing arguments field.
func TestConvertAnthropicRequestResponsesRequestEmptyToolInput(t *testing.T) {
	req := transcode.AnthropicMessageRequest{
		Model:     "claude-3-5-sonnet",
		MaxTokens: 100,
		Messages: []transcode.AnthropicMessage{{
			Role: transcode.AnthropicMessageRoleAssistant,
			Content: transcode.AnthropicContent{ContentBlocks: []transcode.AnthropicContentBlock{{
				Type:      transcode.AnthropicContentBlockTypeToolUse,
				ID:        new("toolu_1"),
				Name:      new("get_weather"),
				ToolUseID: new("call_1"),
			}}},
		}},
	}
	out := transcode.ConvertAnthropicRequestResponsesRequest(&req)
	if len(out.Input) != 1 {
		t.Fatalf("input = %+v", out.Input)
	}
	item := out.Input[0]
	if item.ResponsesToolMessage == nil || item.ResponsesToolMessage.Arguments == nil {
		t.Fatalf("tool message = %+v, want arguments", item.ResponsesToolMessage)
	}
	if *item.ResponsesToolMessage.Arguments != "{}" {
		t.Errorf("arguments = %q, want {}", *item.ResponsesToolMessage.Arguments)
	}
}

// TestConvertResponsesStreamResponseAnthropicStreamEventRefusal verifies a
// refusal delta streams as a text delta of the open block (never dropped).
func TestConvertResponsesStreamResponseAnthropicStreamEventRefusal(t *testing.T) {
	var state transcode.AnthropicResponsesStreamState
	_ = state.ConvertResponsesStreamResponseAnthropicStreamEvent(&transcode.ResponsesStreamResponse{
		Type:     transcode.ResponsesStreamResponseTypeCreated,
		Response: &transcode.ResponsesResponse{ID: "resp_1", Model: "gpt-4.1"},
	})
	_ = state.ConvertResponsesStreamResponseAnthropicStreamEvent(&transcode.ResponsesStreamResponse{
		Type: transcode.ResponsesStreamResponseTypeOutputItemAdded,
		Item: &transcode.ResponsesMessage{ID: new("msg_1"), Type: new(transcode.ResponsesMessageTypeMessage)},
	})
	events := state.ConvertResponsesStreamResponseAnthropicStreamEvent(&transcode.ResponsesStreamResponse{
		Type:   transcode.ResponsesStreamResponseTypeRefusalDelta,
		ItemID: new("msg_1"),
		Delta:  new("I cannot answer that"),
	})
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Type != transcode.AnthropicStreamEventTypeContentBlockDelta ||
		events[0].Delta == nil || events[0].Delta.Type != transcode.AnthropicStreamDeltaTypeTextDelta ||
		events[0].Delta.Text == nil || *events[0].Delta.Text != "I cannot answer that" {
		t.Errorf("refusal delta = %+v", events[0])
	}
}

// TestConvertResponsesStreamResponseAnthropicStreamEventFailed verifies a
// failed responses stream terminates the anthropic stream cleanly.
func TestConvertResponsesStreamResponseAnthropicStreamEventFailed(t *testing.T) {
	var state transcode.AnthropicResponsesStreamState
	_ = state.ConvertResponsesStreamResponseAnthropicStreamEvent(&transcode.ResponsesStreamResponse{
		Type:     transcode.ResponsesStreamResponseTypeCreated,
		Response: &transcode.ResponsesResponse{ID: "resp_1", Model: "gpt-4.1"},
	})
	events := state.ConvertResponsesStreamResponseAnthropicStreamEvent(&transcode.ResponsesStreamResponse{
		Type:     transcode.ResponsesStreamResponseTypeFailed,
		Response: &transcode.ResponsesResponse{ID: "resp_1", Status: new("failed")},
	})
	if len(events) != 2 {
		t.Fatalf("events = %+v, want message_delta + message_stop", events)
	}
	if events[0].Type != transcode.AnthropicStreamEventTypeMessageDelta || events[1].Type != transcode.AnthropicStreamEventTypeMessageStop {
		t.Errorf("events = %+v", events)
	}
}

// TestConvertResponsesStreamResponseAnthropicStreamEventUnsupportedItem
// verifies unsupported output item types do not consume a content block
// index (no gaps in later emitted indexes).
func TestConvertResponsesStreamResponseAnthropicStreamEventUnsupportedItem(t *testing.T) {
	var state transcode.AnthropicResponsesStreamState
	_ = state.ConvertResponsesStreamResponseAnthropicStreamEvent(&transcode.ResponsesStreamResponse{
		Type:     transcode.ResponsesStreamResponseTypeCreated,
		Response: &transcode.ResponsesResponse{ID: "resp_1", Model: "gpt-4.1"},
	})
	// An unsupported item type (e.g. refusal item) must produce no events.
	if events := state.ConvertResponsesStreamResponseAnthropicStreamEvent(&transcode.ResponsesStreamResponse{
		Type: transcode.ResponsesStreamResponseTypeOutputItemAdded,
		Item: &transcode.ResponsesMessage{ID: new("ref_1"), Type: new(transcode.ResponsesMessageTypeRefusal)},
	}); events != nil {
		t.Errorf("unsupported item events = %+v, want none", events)
	}
	// The next supported item starts at index 0.
	events := state.ConvertResponsesStreamResponseAnthropicStreamEvent(&transcode.ResponsesStreamResponse{
		Type: transcode.ResponsesStreamResponseTypeOutputItemAdded,
		Item: &transcode.ResponsesMessage{ID: new("msg_1"), Type: new(transcode.ResponsesMessageTypeMessage)},
	})
	if len(events) != 1 || events[0].Index == nil || *events[0].Index != 0 {
		t.Errorf("supported item events = %+v, want index 0", events)
	}
}
