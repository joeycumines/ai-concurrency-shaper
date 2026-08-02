package transcode_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// mustChatCompletions loads the chat completions fixture set, failing the test
// on any load error.
func mustChatCompletions(t *testing.T) testcorpus.ChatCompletionsFixtureSet {
	t.Helper()
	f, err := testcorpus.OpenAIChatCompletionsFixtures()
	if err != nil {
		t.Fatalf("load chat completions fixtures: %v", err)
	}
	return f
}

// mustResponses loads the responses fixture set, failing the test on any load
// error.
func mustResponses(t *testing.T) testcorpus.ResponsesFixtureSet {
	t.Helper()
	f, err := testcorpus.OpenAIResponsesFixtures()
	if err != nil {
		t.Fatalf("load responses fixtures: %v", err)
	}
	return f
}

// mustAnthropic loads the Anthropic fixture set, failing the test on any load
// error.
func mustAnthropic(t *testing.T) testcorpus.AnthropicMessagesFixtureSet {
	t.Helper()
	f, err := testcorpus.AnthropicMessagesFixtures()
	if err != nil {
		t.Fatalf("load anthropic fixtures: %v", err)
	}
	return f
}

// hasRole reports whether any chat message carries the given role.
func hasRole(messages []transcode.ChatMessage, role transcode.ChatMessageRole) bool {
	for i := range messages {
		if messages[i].Role == role {
			return true
		}
	}
	return false
}

// TestOpenAIChatCompletionsFixturesLoad verifies the chat completions fixture
// set: request, response, and stream all decode and exercise the documented
// feature coverage (developer role, tool calls, reasoning blocks, usage).
func TestOpenAIChatCompletionsFixturesLoad(t *testing.T) {
	f := mustChatCompletions(t)

	req := f.Request
	if req.Model != "gpt-4.1" {
		t.Errorf("request model = %q, want gpt-4.1", req.Model)
	}
	if !hasRole(req.Messages, transcode.ChatMessageRoleDeveloper) {
		t.Error("request messages: developer role message missing")
	}
	if !hasRole(req.Messages, transcode.ChatMessageRoleTool) {
		t.Error("request messages: tool role message missing")
	}
	var assistantToolCall *transcode.ChatAssistantMessageToolCall
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role == transcode.ChatMessageRoleAssistant && m.ChatAssistantMessage != nil {
			if len(m.ToolCalls) == 1 {
				assistantToolCall = &m.ToolCalls[0]
			}
		}
		if m.Role == transcode.ChatMessageRoleTool && m.ChatToolMessage != nil {
			if m.ToolCallID == nil || *m.ToolCallID != "call_abc123" {
				t.Errorf("tool message tool_call_id = %v, want call_abc123", m.ToolCallID)
			}
		}
	}
	if assistantToolCall == nil {
		t.Fatal("request messages: assistant tool call missing")
	}
	if assistantToolCall.Function.Name == nil || *assistantToolCall.Function.Name != "get_weather" {
		t.Errorf("assistant tool call function name = %v, want get_weather", assistantToolCall.Function.Name)
	}
	if assistantToolCall.Function.Arguments == "" {
		t.Error("assistant tool call arguments empty")
	}
	if len(req.Tools) != 1 || req.Tools[0].Function == nil || req.Tools[0].Function.Name != "get_weather" {
		t.Errorf("request tools = %+v, want one get_weather function tool", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Str == nil || *req.ToolChoice.Str != "auto" {
		t.Errorf("request tool_choice = %+v, want string auto", req.ToolChoice)
	}
	if req.Reasoning == nil || req.Reasoning.Effort == nil || *req.Reasoning.Effort != "medium" {
		t.Errorf("request reasoning = %+v, want effort medium", req.Reasoning)
	}
	if req.Stream == nil || *req.Stream {
		t.Errorf("request stream = %v, want false", req.Stream)
	}

	resp := f.Response
	if resp.ID != "chatcmpl_8xyz" || resp.Object != "chat.completion" {
		t.Errorf("response id/object = %q/%q, want chatcmpl_8xyz/chat.completion", resp.ID, resp.Object)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("response choices = %d, want 1", len(resp.Choices))
	}
	choice := resp.Choices[0]
	if choice.FinishReason == nil || *choice.FinishReason != "stop" {
		t.Errorf("choice finish_reason = %v, want stop", choice.FinishReason)
	}
	if choice.Message == nil || choice.Message.ChatAssistantMessage == nil {
		t.Fatal("choice message assistant payload missing")
	}
	if choice.Message.Reasoning == nil || !strings.Contains(*choice.Message.Reasoning, "weather") {
		t.Errorf("choice message reasoning = %v, want weather explanation", choice.Message.Reasoning)
	}
	if len(choice.Message.ReasoningDetails) != 1 || choice.Message.ReasoningDetails[0].Type != transcode.ChatReasoningDetailsTypeText {
		t.Errorf("choice message reasoning_details = %+v, want one reasoning.text block", choice.Message.ReasoningDetails)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 60 {
		t.Fatalf("response usage = %+v, want total_tokens 60", resp.Usage)
	}
	if resp.Usage.CompletionTokensDetails == nil || resp.Usage.CompletionTokensDetails.ReasoningTokens != 12 {
		t.Errorf("response usage reasoning_tokens = %+v, want 12", resp.Usage.CompletionTokensDetails)
	}
	if resp.Usage.PromptTokensDetails == nil || resp.Usage.PromptTokensDetails.CachedTokens != 5 {
		t.Errorf("response usage cached_tokens = %+v, want 5", resp.Usage.PromptTokensDetails)
	}

	frames := testcorpus.ParseSSEFrames([]byte(f.Stream))
	if len(frames) != 6 {
		t.Fatalf("stream frames = %d, want 6", len(frames))
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Errorf("stream final frame = %q, want [DONE]", frames[len(frames)-1])
	}
	var first transcode.ChatStreamResponse
	if err := json.Unmarshal([]byte(frames[0]), &first); err != nil {
		t.Fatalf("unmarshal first stream frame: %v", err)
	}
	if len(first.Choices) != 1 || first.Choices[0].Delta == nil || first.Choices[0].Delta.Role == nil || *first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first stream frame delta role = %+v, want assistant", first.Choices[0].Delta)
	}
	var usageFrame struct {
		Usage *transcode.ChatLLMUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(frames[4]), &usageFrame); err != nil {
		t.Fatalf("unmarshal usage stream frame: %v", err)
	}
	if usageFrame.Usage == nil || usageFrame.Usage.TotalTokens != 60 {
		t.Errorf("usage stream frame = %+v, want total_tokens 60", usageFrame.Usage)
	}
}

// TestOpenAIResponsesFixturesLoad verifies the responses fixture set: request,
// response, and stream all decode and exercise the documented event types.
func TestOpenAIResponsesFixturesLoad(t *testing.T) {
	f := mustResponses(t)

	req := f.Request
	if req.Model != "gpt-4.1" {
		t.Errorf("request model = %q, want gpt-4.1", req.Model)
	}
	if req.Instructions == nil || !strings.Contains(*req.Instructions, "weather") {
		t.Errorf("request instructions = %v, want weather instructions", req.Instructions)
	}
	if len(req.Input) != 1 || req.Input[0].Role == nil || *req.Input[0].Role != transcode.ResponsesMessageRoleUser {
		t.Fatalf("request input = %+v, want one user message", req.Input)
	}
	blocks := req.Input[0].Content.ContentBlocks
	if len(blocks) != 1 || blocks[0].Type != transcode.ResponsesMessageContentBlockTypeInputText {
		t.Errorf("request input content blocks = %+v, want one input_text block", blocks)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name == nil || *req.Tools[0].Name != "get_weather" {
		t.Errorf("request tools = %+v, want one get_weather tool", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Str == nil || *req.ToolChoice.Str != "auto" {
		t.Errorf("request tool_choice = %+v, want string auto", req.ToolChoice)
	}
	if req.Reasoning == nil || req.Reasoning.Effort == nil || *req.Reasoning.Effort != "medium" {
		t.Errorf("request reasoning = %+v, want effort medium", req.Reasoning)
	}

	resp := f.Response
	if resp.ID != "resp_8xyz" || resp.Object != "response" {
		t.Errorf("response id/object = %q/%q, want resp_8xyz/response", resp.ID, resp.Object)
	}
	if resp.Status == nil || *resp.Status != "completed" {
		t.Errorf("response status = %v, want completed", resp.Status)
	}
	if len(resp.Output) != 4 {
		t.Fatalf("response output items = %d, want 4", len(resp.Output))
	}
	var sawReasoning, sawFunctionCall, sawFunctionCallOutput, sawMessage bool
	for i := range resp.Output {
		item := &resp.Output[i]
		switch *item.Type {
		case transcode.ResponsesMessageTypeReasoning:
			sawReasoning = true
			if len(item.Summary) != 1 || item.Summary[0].Type != "summary_text" || !strings.Contains(item.Summary[0].Text, "get_weather") {
				t.Errorf("reasoning item summary = %+v, want summary_text with get_weather", item.Summary)
			}
		case transcode.ResponsesMessageTypeFunctionCall:
			sawFunctionCall = true
			if item.CallID == nil || *item.CallID != "call_abc123" {
				t.Errorf("function_call call_id = %v, want call_abc123", item.CallID)
			}
			if item.Name == nil || *item.Name != "get_weather" {
				t.Errorf("function_call name = %v, want get_weather", item.Name)
			}
			if item.Arguments == nil || !strings.Contains(*item.Arguments, "Tokyo") {
				t.Errorf("function_call arguments = %v, want Tokyo", item.Arguments)
			}
		case transcode.ResponsesMessageTypeFunctionCallOutput:
			sawFunctionCallOutput = true
			if item.CallID == nil || *item.CallID != "call_abc123" {
				t.Errorf("function_call_output call_id = %v, want call_abc123", item.CallID)
			}
			if item.Output == nil || item.Output.Str == nil || !strings.Contains(*item.Output.Str, "sunny") {
				t.Errorf("function_call_output output = %+v, want sunny string", item.Output)
			}
		case transcode.ResponsesMessageTypeMessage:
			sawMessage = true
			if item.Role == nil || *item.Role != transcode.ResponsesMessageRoleAssistant {
				t.Errorf("message role = %v, want assistant", item.Role)
			}
			if len(item.Content.ContentBlocks) != 1 || item.Content.ContentBlocks[0].Type != transcode.ResponsesMessageContentBlockTypeOutputText {
				t.Errorf("message content blocks = %+v, want one output_text block", item.Content.ContentBlocks)
			}
		}
	}
	if !sawReasoning || !sawFunctionCall || !sawFunctionCallOutput || !sawMessage {
		t.Errorf("output item coverage: reasoning=%v function_call=%v function_call_output=%v message=%v",
			sawReasoning, sawFunctionCall, sawFunctionCallOutput, sawMessage)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 70 {
		t.Fatalf("response usage = %+v, want total_tokens 70", resp.Usage)
	}
	if resp.Usage.OutputTokensDetails == nil || resp.Usage.OutputTokensDetails.ReasoningTokens != 12 {
		t.Errorf("response usage reasoning_tokens = %+v, want 12", resp.Usage.OutputTokensDetails)
	}

	frames := testcorpus.ParseSSEFrames([]byte(f.Stream))
	wantTypes := []transcode.ResponsesStreamResponseType{
		transcode.ResponsesStreamResponseTypeCreated,
		transcode.ResponsesStreamResponseTypeInProgress,
		transcode.ResponsesStreamResponseTypeOutputItemAdded,
		transcode.ResponsesStreamResponseTypeReasoningSummaryPartAdded,
		transcode.ResponsesStreamResponseTypeReasoningSummaryTextDelta,
		transcode.ResponsesStreamResponseTypeReasoningSummaryTextDone,
		transcode.ResponsesStreamResponseTypeOutputItemDone,
		transcode.ResponsesStreamResponseTypeOutputItemAdded,
		transcode.ResponsesStreamResponseTypeContentPartAdded,
		transcode.ResponsesStreamResponseTypeOutputTextDelta,
		transcode.ResponsesStreamResponseTypeOutputTextDelta,
		transcode.ResponsesStreamResponseTypeOutputTextDone,
		transcode.ResponsesStreamResponseTypeContentPartDone,
		transcode.ResponsesStreamResponseTypeOutputItemDone,
		transcode.ResponsesStreamResponseTypeCompleted,
	}
	if len(frames) != len(wantTypes) {
		t.Fatalf("stream frames = %d, want %d", len(frames), len(wantTypes))
	}
	for i, want := range wantTypes {
		var event transcode.ResponsesStreamResponse
		if err := json.Unmarshal([]byte(frames[i]), &event); err != nil {
			t.Fatalf("unmarshal stream frame %d: %v", i, err)
		}
		if event.Type != want {
			t.Errorf("stream frame %d type = %q, want %q", i, event.Type, want)
		}
	}
	var completed transcode.ResponsesStreamResponse
	if err := json.Unmarshal([]byte(frames[len(frames)-1]), &completed); err != nil {
		t.Fatalf("unmarshal completed frame: %v", err)
	}
	if completed.Response == nil || completed.Response.Usage == nil || completed.Response.Usage.TotalTokens != 70 {
		t.Errorf("completed frame usage = %+v, want total_tokens 70", completed.Response)
	}
}

// TestAnthropicMessagesFixturesLoad verifies the Anthropic fixture set:
// request, response, and stream all decode and exercise the documented event
// types.
func TestAnthropicMessagesFixturesLoad(t *testing.T) {
	f := mustAnthropic(t)

	req := f.Request
	if req.Model != "claude-sonnet-4-20250514" || req.MaxTokens != 1024 {
		t.Errorf("request model/max_tokens = %q/%d, want claude-sonnet-4-20250514/1024", req.Model, req.MaxTokens)
	}
	if req.System == nil || req.System.ContentStr == nil || !strings.Contains(*req.System.ContentStr, "weather") {
		t.Errorf("request system = %+v, want weather system prompt", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != transcode.AnthropicMessageRoleUser {
		t.Fatalf("request messages = %+v, want one user message", req.Messages)
	}
	if len(req.Messages[0].Content.ContentBlocks) != 1 || req.Messages[0].Content.ContentBlocks[0].Type != transcode.AnthropicContentBlockTypeText {
		t.Errorf("request message content = %+v, want one text block", req.Messages[0].Content)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Errorf("request tools = %+v, want one get_weather tool", req.Tools)
	}
	if req.Tools[0].InputSchema == nil {
		t.Error("request tool input_schema missing")
	}
	if req.ToolChoice == nil || req.ToolChoice.Type != "auto" {
		t.Errorf("request tool_choice = %+v, want auto", req.ToolChoice)
	}
	if req.Stream == nil || *req.Stream {
		t.Errorf("request stream = %v, want false", req.Stream)
	}

	resp := f.Response
	if resp.ID != "msg_01AbC2xY" || resp.Type != "message" || resp.Role != "assistant" {
		t.Errorf("response id/type/role = %q/%q/%q, want msg_01AbC2xY/message/assistant", resp.ID, resp.Type, resp.Role)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != transcode.AnthropicContentBlockTypeText {
		t.Fatalf("response content = %+v, want one text block", resp.Content)
	}
	if resp.Content[0].Text == nil || !strings.Contains(*resp.Content[0].Text, "sunny") {
		t.Errorf("response text = %v, want sunny weather", resp.Content[0].Text)
	}
	if resp.StopReason != transcode.AnthropicStopReasonEndTurn {
		t.Errorf("response stop_reason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage == nil || resp.Usage.InputTokens != 45 || resp.Usage.OutputTokens != 25 {
		t.Errorf("response usage = %+v, want input 45 output 25", resp.Usage)
	}

	frames := testcorpus.ParseSSEFrames([]byte(f.Stream))
	wantTypes := []transcode.AnthropicStreamEventType{
		transcode.AnthropicStreamEventTypeMessageStart,
		transcode.AnthropicStreamEventTypeContentBlockStart,
		transcode.AnthropicStreamEventTypeContentBlockDelta,
		transcode.AnthropicStreamEventTypeContentBlockDelta,
		transcode.AnthropicStreamEventTypeContentBlockStop,
		transcode.AnthropicStreamEventTypeContentBlockStart,
		transcode.AnthropicStreamEventTypeContentBlockDelta,
		transcode.AnthropicStreamEventTypeContentBlockDelta,
		transcode.AnthropicStreamEventTypeContentBlockStop,
		transcode.AnthropicStreamEventTypeMessageDelta,
		transcode.AnthropicStreamEventTypeMessageStop,
	}
	if len(frames) != len(wantTypes) {
		t.Fatalf("stream frames = %d, want %d", len(frames), len(wantTypes))
	}
	var sawThinkingDelta, sawSignatureDelta, sawTextDelta bool
	for i, want := range wantTypes {
		var event transcode.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(frames[i]), &event); err != nil {
			t.Fatalf("unmarshal stream frame %d: %v", i, err)
		}
		if event.Type != want {
			t.Errorf("stream frame %d type = %q, want %q", i, event.Type, want)
		}
		if event.Delta != nil {
			switch event.Delta.Type {
			case transcode.AnthropicStreamDeltaTypeThinkingDelta:
				sawThinkingDelta = true
			case transcode.AnthropicStreamDeltaTypeSignatureDelta:
				sawSignatureDelta = true
			case transcode.AnthropicStreamDeltaTypeTextDelta:
				sawTextDelta = true
			}
		}
	}
	if !sawThinkingDelta || !sawSignatureDelta || !sawTextDelta {
		t.Errorf("delta coverage: thinking=%v signature=%v text=%v", sawThinkingDelta, sawSignatureDelta, sawTextDelta)
	}
	var messageStart transcode.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(frames[0]), &messageStart); err != nil {
		t.Fatalf("unmarshal message_start frame: %v", err)
	}
	if messageStart.Message == nil || messageStart.Message.Usage == nil || messageStart.Message.Usage.InputTokens != 45 {
		t.Errorf("message_start usage = %+v, want input 45", messageStart.Message)
	}
	var messageDelta transcode.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(frames[len(frames)-2]), &messageDelta); err != nil {
		t.Fatalf("unmarshal message_delta frame: %v", err)
	}
	if messageDelta.Delta == nil || messageDelta.Delta.StopReason == nil || *messageDelta.Delta.StopReason != transcode.AnthropicStopReasonEndTurn {
		t.Errorf("message_delta stop_reason = %+v, want end_turn", messageDelta.Delta)
	}
	if messageDelta.Usage == nil || messageDelta.Usage.OutputTokens != 25 {
		t.Errorf("message_delta usage = %+v, want output 25", messageDelta.Usage)
	}
}

// TestFixturesRoundTrip proves the schema types round-trip the fixture payloads
// losslessly: re-encoding a decoded fixture must be semantically identical to
// the original fixture bytes, so no field is silently dropped or added by the
// schema port.
func TestFixturesRoundTrip(t *testing.T) {
	chat := mustChatCompletions(t)
	responses := mustResponses(t)
	anthropic := mustAnthropic(t)
	tests := []struct {
		name string
		got  any
		raw  []byte
	}{
		{"chat request", chat.Request, testcorpus.ChatCompletionsRequestJSON()},
		{"chat response", chat.Response, testcorpus.ChatCompletionsResponseJSON()},
		{"responses request", responses.Request, testcorpus.ResponsesRequestJSON()},
		{"responses response", responses.Response, testcorpus.ResponsesResponseJSON()},
		{"anthropic request", anthropic.Request, testcorpus.AnthropicMessagesRequestJSON()},
		{"anthropic response", anthropic.Response, testcorpus.AnthropicMessagesResponseJSON()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marshaled, err := json.Marshal(tt.got)
			if err != nil {
				t.Fatalf("marshal fixture: %v", err)
			}
			// Decode both the original fixture bytes and the re-encoded payload
			// into generic values and compare structurally.
			var want, got any
			if err := json.Unmarshal(tt.raw, &want); err != nil {
				t.Fatalf("unmarshal fixture bytes: %v", err)
			}
			if err := json.Unmarshal(marshaled, &got); err != nil {
				t.Fatalf("unmarshal marshaled fixture: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip mismatch:\nraw:  %s\nreencoded: %s", tt.raw, marshaled)
			}
		})
	}
}

// TestChatMessageContentUnion verifies the string-or-blocks union semantics.
func TestChatMessageContentUnion(t *testing.T) {
	var content transcode.ChatMessageContent
	if err := json.Unmarshal([]byte(`"plain text"`), &content); err != nil {
		t.Fatalf("unmarshal string content: %v", err)
	}
	if content.ContentStr == nil || *content.ContentStr != "plain text" || content.ContentBlocks != nil {
		t.Errorf("string content decoded as %+v", content)
	}
	out, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal string content: %v", err)
	}
	if string(out) != `"plain text"` {
		t.Errorf("string content marshaled as %s", out)
	}

	var blocks transcode.ChatMessageContent
	if err := json.Unmarshal([]byte(`[{"type":"text","text":"hello"}]`), &blocks); err != nil {
		t.Fatalf("unmarshal blocks content: %v", err)
	}
	if blocks.ContentStr != nil || len(blocks.ContentBlocks) != 1 {
		t.Errorf("blocks content decoded as %+v", blocks)
	}
	if blocks.ContentBlocks[0].Text == nil || *blocks.ContentBlocks[0].Text != "hello" {
		t.Errorf("block text = %v, want hello", blocks.ContentBlocks[0].Text)
	}
	out, err = json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal blocks content: %v", err)
	}
	if string(out) != `[{"type":"text","text":"hello"}]` {
		t.Errorf("blocks content marshaled as %s", out)
	}

	// Both forms set must fail to marshal.
	bad := transcode.ChatMessageContent{ContentStr: strptr("x"), ContentBlocks: []transcode.ChatContentBlock{{Type: transcode.ChatContentBlockTypeText}}}
	if _, err := json.Marshal(bad); err == nil {
		t.Error("marshal with both forms set: want error")
	}

	// null decodes to the zero state and marshals back to null.
	var nullContent transcode.ChatMessageContent
	if err := json.Unmarshal([]byte(`null`), &nullContent); err != nil {
		t.Fatalf("unmarshal null content: %v", err)
	}
	if nullContent.ContentStr != nil || nullContent.ContentBlocks != nil {
		t.Errorf("null content decoded as %+v", nullContent)
	}
	out, err = json.Marshal(nullContent)
	if err != nil {
		t.Fatalf("marshal null content: %v", err)
	}
	if string(out) != "null" {
		t.Errorf("null content marshaled as %s", out)
	}
}

// TestResponsesContentUnions verifies the responses content, tool output, and
// tool choice union semantics.
func TestResponsesContentUnions(t *testing.T) {
	var content transcode.ResponsesMessageContent
	if err := json.Unmarshal([]byte(`"hello"`), &content); err != nil {
		t.Fatalf("unmarshal string content: %v", err)
	}
	out, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal string content: %v", err)
	}
	if string(out) != `"hello"` {
		t.Errorf("string content marshaled as %s", out)
	}
	// Empty content must marshal as an empty string, never null.
	empty, err := json.Marshal(transcode.ResponsesMessageContent{})
	if err != nil {
		t.Fatalf("marshal empty content: %v", err)
	}
	if string(empty) != `""` {
		t.Errorf("empty content marshaled as %s", empty)
	}

	var output transcode.ResponsesToolMessageOutput
	if err := json.Unmarshal([]byte(`[{"type":"output_text","text":"result"}]`), &output); err != nil {
		t.Fatalf("unmarshal blocks output: %v", err)
	}
	if output.Str != nil || len(output.Blocks) != 1 {
		t.Errorf("blocks output decoded as %+v", output)
	}
	emptyOutput, err := json.Marshal(transcode.ResponsesToolMessageOutput{})
	if err != nil {
		t.Fatalf("marshal empty output: %v", err)
	}
	if string(emptyOutput) != `""` {
		t.Errorf("empty output marshaled as %s", emptyOutput)
	}
	out, err = json.Marshal(output)
	if err != nil {
		t.Fatalf("marshal blocks output: %v", err)
	}
	if !strings.Contains(string(out), `"output_text"`) {
		t.Errorf("blocks output marshaled as %s", out)
	}
	badOutput := transcode.ResponsesToolMessageOutput{Str: strptr("x"), Blocks: []transcode.ResponsesMessageContentBlock{{Type: transcode.ResponsesMessageContentBlockTypeOutputText}}}
	if _, err := json.Marshal(badOutput); err == nil {
		t.Error("marshal tool output with both forms set: want error")
	}
	badContent := transcode.ResponsesMessageContent{ContentStr: strptr("x"), ContentBlocks: []transcode.ResponsesMessageContentBlock{{Type: transcode.ResponsesMessageContentBlockTypeOutputText}}}
	if _, err := json.Marshal(badContent); err == nil {
		t.Error("marshal content with both forms set: want error")
	}

	var choice transcode.ResponsesToolChoice
	if err := json.Unmarshal([]byte(`"auto"`), &choice); err != nil {
		t.Fatalf("unmarshal string tool choice: %v", err)
	}
	if choice.Str == nil || *choice.Str != "auto" {
		t.Errorf("string tool choice decoded as %+v", choice)
	}
	if err := json.Unmarshal([]byte(`{"type":"function","name":"get_weather"}`), &choice); err != nil {
		t.Fatalf("unmarshal struct tool choice: %v", err)
	}
	if choice.Struct == nil || choice.Struct.Name == nil || *choice.Struct.Name != "get_weather" {
		t.Errorf("struct tool choice decoded as %+v", choice)
	}
	out, err = json.Marshal(choice)
	if err != nil {
		t.Fatalf("marshal struct tool choice: %v", err)
	}
	if !strings.Contains(string(out), `"name":"get_weather"`) {
		t.Errorf("struct tool choice marshaled as %s", out)
	}
	bad := transcode.ResponsesToolChoice{Str: strptr("auto"), Struct: &transcode.ResponsesToolChoiceStruct{Type: "function"}}
	if _, err := json.Marshal(bad); err == nil {
		t.Error("marshal with both forms set: want error")
	}
	zero, err := json.Marshal(transcode.ResponsesToolChoice{})
	if err != nil {
		t.Fatalf("marshal zero tool choice: %v", err)
	}
	if string(zero) != "null" {
		t.Errorf("zero tool choice marshaled as %s", zero)
	}
}

// TestChatToolChoiceUnion verifies the chat tool choice union semantics.
func TestChatToolChoiceUnion(t *testing.T) {
	var choice transcode.ChatToolChoice
	if err := json.Unmarshal([]byte(`{"type":"function","function":{"name":"get_weather"}}`), &choice); err != nil {
		t.Fatalf("unmarshal struct tool choice: %v", err)
	}
	if choice.Struct == nil || choice.Struct.Function == nil || choice.Struct.Function.Name != "get_weather" {
		t.Errorf("struct tool choice decoded as %+v", choice)
	}
	out, err := json.Marshal(choice)
	if err != nil {
		t.Fatalf("marshal struct tool choice: %v", err)
	}
	if !strings.Contains(string(out), `"name":"get_weather"`) {
		t.Errorf("struct tool choice marshaled as %s", out)
	}
	if err := json.Unmarshal([]byte(`"required"`), &choice); err != nil {
		t.Fatalf("unmarshal string tool choice: %v", err)
	}
	if choice.Str == nil || *choice.Str != "required" {
		t.Errorf("string tool choice decoded as %+v", choice)
	}

	// Both forms set must fail to marshal.
	bad := transcode.ChatToolChoice{Str: strptr("required"), Struct: &transcode.ChatToolChoiceStruct{Type: "function", Function: &transcode.ChatToolChoiceFunction{Name: "f"}}}
	if _, err := json.Marshal(bad); err == nil {
		t.Error("marshal with both forms set: want error")
	}

	// The zero value marshals as null.
	out, err = json.Marshal(transcode.ChatToolChoice{})
	if err != nil {
		t.Fatalf("marshal zero tool choice: %v", err)
	}
	if string(out) != "null" {
		t.Errorf("zero tool choice marshaled as %s", out)
	}
}

// TestAnthropicContentUnion verifies the Anthropic content union semantics,
// including the empty-form marshal to an empty array.
func TestAnthropicContentUnion(t *testing.T) {
	var content transcode.AnthropicContent
	if err := json.Unmarshal([]byte(`"hello"`), &content); err != nil {
		t.Fatalf("unmarshal string content: %v", err)
	}
	if content.ContentStr == nil || *content.ContentStr != "hello" {
		t.Errorf("string content decoded as %+v", content)
	}
	out, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal string content: %v", err)
	}
	if string(out) != `"hello"` {
		t.Errorf("string content marshaled as %s", out)
	}
	empty, err := json.Marshal(transcode.AnthropicContent{})
	if err != nil {
		t.Fatalf("marshal empty content: %v", err)
	}
	if string(empty) != "[]" {
		t.Errorf("empty content marshaled as %s", empty)
	}
	if err := json.Unmarshal([]byte(`[{"type":"text","text":"hi"}]`), &content); err != nil {
		t.Fatalf("unmarshal blocks content: %v", err)
	}
	if content.ContentStr != nil || len(content.ContentBlocks) != 1 || content.ContentBlocks[0].Type != transcode.AnthropicContentBlockTypeText {
		t.Errorf("blocks content decoded as %+v", content)
	}
	bad := transcode.AnthropicContent{ContentStr: strptr("x"), ContentBlocks: []transcode.AnthropicContentBlock{{Type: transcode.AnthropicContentBlockTypeText}}}
	if _, err := json.Marshal(bad); err == nil {
		t.Error("marshal with both forms set: want error")
	}
}

// TestUnionUnmarshalInvalidInputs verifies every union type rejects input
// that is neither of its forms, returning an error instead of silently
// decoding into an empty value.
func TestUnionUnmarshalInvalidInputs(t *testing.T) {
	invalid := []struct {
		name string
		data string
		got  any
	}{
		{"chat content", `123`, &transcode.ChatMessageContent{}},
		{"chat tool choice", `[1,2]`, &transcode.ChatToolChoice{}},
		{"responses content", `123`, &transcode.ResponsesMessageContent{}},
		{"tool output", `123`, &transcode.ResponsesToolMessageOutput{}},
		{"responses tool choice", `[1,2]`, &transcode.ResponsesToolChoice{}},
		{"anthropic content", `123`, &transcode.AnthropicContent{}},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tt.data), tt.got); err == nil {
				t.Errorf("unmarshal %s: want error, got nil", tt.data)
			}
		})
	}
}

// TestResponsesMessageItemShapes verifies that the responses message union
// decodes each item shape (message, reasoning, function_call,
// function_call_output) into the right embedded payload.
func TestResponsesMessageItemShapes(t *testing.T) {
	var item transcode.ResponsesMessage
	if err := json.Unmarshal([]byte(`{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"think"}]}`), &item); err != nil {
		t.Fatalf("unmarshal reasoning item: %v", err)
	}
	if len(item.Summary) != 1 || item.Summary[0].Text != "think" {
		t.Errorf("reasoning item summary = %+v", item.Summary)
	}
	out, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal reasoning item: %v", err)
	}
	if !strings.Contains(string(out), `"summary":[{"type":"summary_text","text":"think"}]`) {
		t.Errorf("reasoning item marshaled as %s", out)
	}

	item = transcode.ResponsesMessage{}
	if err := json.Unmarshal([]byte(`{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"}`), &item); err != nil {
		t.Fatalf("unmarshal function_call item: %v", err)
	}
	if item.CallID == nil || *item.CallID != "call_1" || item.Name == nil || *item.Name != "f" || item.Arguments == nil {
		t.Errorf("function_call item decoded as %+v", item)
	}

	item = transcode.ResponsesMessage{}
	if err := json.Unmarshal([]byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}`), &item); err != nil {
		t.Fatalf("unmarshal message item: %v", err)
	}
	if item.Role == nil || *item.Role != transcode.ResponsesMessageRoleAssistant {
		t.Errorf("message item role = %v", item.Role)
	}
	if len(item.Content.ContentBlocks) != 1 || item.Content.ContentBlocks[0].Text == nil || *item.Content.ContentBlocks[0].Text != "hi" {
		t.Errorf("message item content = %+v", item.Content)
	}
}

// TestParseSSEFrames verifies the SSE frame parser tolerates event lines,
// CRLF, multi-line data payloads, and malformed lines.
func TestParseSSEFrames(t *testing.T) {
	input := "event: response.created\r\ndata: {\"type\":\"response.created\"}\r\n\r\n" +
		"data: line1\ndata: line2\n\n" +
		"bogus line without colon\n" +
		"data: [DONE]\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n"
	frames := testcorpus.ParseSSEFrames([]byte(input))
	want := []string{`{"type":"response.created"}`, "line1\nline2", "[DONE]", `{"type":"ping"}`}
	if len(frames) != len(want) {
		t.Fatalf("frames = %d, want %d: %q", len(frames), len(want), frames)
	}
	for i := range want {
		if frames[i] != want[i] {
			t.Errorf("frame %d = %q, want %q", i, frames[i], want[i])
		}
	}
	if got := testcorpus.ParseSSEFrames([]byte("")); len(got) != 0 {
		t.Errorf("empty input frames = %q, want none", got)
	}
}

func strptr(s string) *string { return &s }
