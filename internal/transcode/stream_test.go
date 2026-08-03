package transcode_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// Streaming conversion tests.

func TestConvertChatResponseResponsesStreamResponse(t *testing.T) {
	f := mustChatCompletions(t)
	frames := testcorpus.ParseSSEFrames([]byte(f.Stream))
	var state transcode.ChatResponsesStreamState
	var events []transcode.ResponsesStreamResponse
	for _, frame := range frames {
		if frame == "[DONE]" {
			continue
		}
		var chunk transcode.ChatStreamResponse
		if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
			t.Fatalf("unmarshal chat stream frame: %v", err)
		}
		events = append(events, state.ConvertChatResponseResponsesStreamResponse(&chunk)...)
	}
	// The terminal event is held back for a trailing usage chunk and released
	// at upstream EOF.
	events = append(events, state.Terminal()...)

	wantTypes := []transcode.ResponsesStreamResponseType{
		transcode.ResponsesStreamResponseTypeCreated,
		transcode.ResponsesStreamResponseTypeInProgress,
		transcode.ResponsesStreamResponseTypeOutputItemAdded,           // reasoning item
		transcode.ResponsesStreamResponseTypeReasoningSummaryPartAdded, // summary part
		transcode.ResponsesStreamResponseTypeReasoningSummaryTextDelta,
		transcode.ResponsesStreamResponseTypeReasoningSummaryTextDone,
		transcode.ResponsesStreamResponseTypeReasoningSummaryPartDone,
		transcode.ResponsesStreamResponseTypeOutputItemDone,  // reasoning item
		transcode.ResponsesStreamResponseTypeOutputItemAdded, // message item
		transcode.ResponsesStreamResponseTypeContentPartAdded,
		transcode.ResponsesStreamResponseTypeOutputTextDelta,
		transcode.ResponsesStreamResponseTypeOutputTextDelta,
		transcode.ResponsesStreamResponseTypeOutputTextDone,
		transcode.ResponsesStreamResponseTypeContentPartDone,
		transcode.ResponsesStreamResponseTypeOutputItemDone, // message item
		transcode.ResponsesStreamResponseTypeCompleted,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, events[i].Type, want)
		}
		if events[i].SequenceNumber != i {
			t.Errorf("event %d sequence = %d, want %d", i, events[i].SequenceNumber, i)
		}
	}

	reasoningAdded := events[2]
	if reasoningAdded.OutputIndex == nil || *reasoningAdded.OutputIndex != 0 ||
		reasoningAdded.Item == nil || reasoningAdded.Item.Type == nil || *reasoningAdded.Item.Type != transcode.ResponsesMessageTypeReasoning {
		t.Errorf("reasoning output_item.added = %+v", reasoningAdded)
	}
	reasoningPart := events[3]
	if reasoningPart.SummaryIndex == nil || *reasoningPart.SummaryIndex != 0 ||
		reasoningPart.ItemID == nil {
		t.Errorf("reasoning summary part added = %+v", reasoningPart)
	}
	reasoningDelta := events[4]
	if reasoningDelta.Delta == nil || !strings.Contains(*reasoningDelta.Delta, "weather") {
		t.Errorf("reasoning delta = %v", reasoningDelta.Delta)
	}
	textAdded := events[8]
	if textAdded.OutputIndex == nil || *textAdded.OutputIndex != 1 ||
		textAdded.Item == nil || textAdded.Item.Type == nil || *textAdded.Item.Type != transcode.ResponsesMessageTypeMessage {
		t.Errorf("text output_item.added = %+v", textAdded)
	}
	contentDelta := events[10]
	if contentDelta.Delta == nil || *contentDelta.Delta != "The weather in Tokyo is " {
		t.Errorf("first content delta = %v", contentDelta.Delta)
	}
	if events[11].Delta == nil || *events[11].Delta != "21°C and sunny." {
		t.Errorf("second content delta = %v", events[11].Delta)
	}

	terminal := events[len(events)-1]
	if terminal.Response == nil {
		t.Fatalf("terminal event has no response: %+v", terminal)
	}
	if terminal.Response.Status == nil || *terminal.Response.Status != "completed" {
		t.Errorf("terminal status = %v, want completed", terminal.Response.Status)
	}
	if len(terminal.Response.Output) != 2 {
		t.Fatalf("terminal output items = %d, want 2", len(terminal.Response.Output))
	}
	if terminal.Response.Output[0].Type == nil || *terminal.Response.Output[0].Type != transcode.ResponsesMessageTypeReasoning {
		t.Errorf("terminal output[0] = %+v, want reasoning", terminal.Response.Output[0])
	}
	if terminal.Response.Output[1].Type == nil || *terminal.Response.Output[1].Type != transcode.ResponsesMessageTypeMessage ||
		terminal.Response.Output[1].Content == nil || len(terminal.Response.Output[1].Content.ContentBlocks) != 1 ||
		terminal.Response.Output[1].Content.ContentBlocks[0].Text == nil ||
		*terminal.Response.Output[1].Content.ContentBlocks[0].Text != "The weather in Tokyo is 21°C and sunny." {
		t.Errorf("terminal output[1] = %+v", terminal.Response.Output[1])
	}
	if terminal.Response.Usage == nil || terminal.Response.Usage.InputTokens != 42 ||
		terminal.Response.Usage.OutputTokens != 18 || terminal.Response.Usage.TotalTokens != 60 {
		t.Errorf("terminal usage = %+v", terminal.Response.Usage)
	}
	if terminal.Response.Usage.OutputTokensDetails == nil || terminal.Response.Usage.OutputTokensDetails.ReasoningTokens != 12 {
		t.Errorf("terminal reasoning tokens = %+v", terminal.Response.Usage.OutputTokensDetails)
	}

	// Chunks after the terminal event are ignored.
	if tail := state.ConvertChatResponseResponsesStreamResponse(&transcode.ChatStreamResponse{Choices: []transcode.ChatChoice{{Delta: &transcode.ChatStreamDelta{Content: new("extra")}}}}); tail != nil {
		t.Errorf("post-terminal events = %+v, want none", tail)
	}
}

// TestConvertChatResponseResponsesStreamResponseToolCall verifies the tool
// call lifecycle of the chat stream conversion: output_item.added with
// in_progress status, arguments deltas, and the terminal done events.
func TestConvertChatResponseResponsesStreamResponseToolCall(t *testing.T) {
	var state transcode.ChatResponsesStreamState
	feed := func(delta transcode.ChatStreamDelta, finish *string) []transcode.ResponsesStreamResponse {
		return state.ConvertChatResponseResponsesStreamResponse(&transcode.ChatStreamResponse{
			ID:      "chatcmpl-1",
			Model:   "gpt-4.1",
			Created: 1710000000,
			Choices: []transcode.ChatChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
		})
	}
	var events []transcode.ResponsesStreamResponse
	events = append(events, feed(transcode.ChatStreamDelta{Role: new("assistant"), Content: new("")}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{ToolCalls: []transcode.ChatAssistantMessageToolCall{{
		ID:       new("call_1"),
		Type:     new("function"),
		Function: transcode.ChatAssistantMessageToolCallFunction{Name: new("get_weather"), Arguments: `{"loc`},
	}}}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{ToolCalls: []transcode.ChatAssistantMessageToolCall{{
		Index:    new(0),
		Function: transcode.ChatAssistantMessageToolCallFunction{Arguments: `ation":"Tokyo"}`},
	}}}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{}, new("tool_calls"))...)
	events = append(events, state.Terminal()...)

	wantTypes := []transcode.ResponsesStreamResponseType{
		transcode.ResponsesStreamResponseTypeCreated,
		transcode.ResponsesStreamResponseTypeInProgress,
		transcode.ResponsesStreamResponseTypeOutputItemAdded,
		transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDelta,
		transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDelta,
		transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDone,
		transcode.ResponsesStreamResponseTypeOutputItemDone,
		transcode.ResponsesStreamResponseTypeCompleted,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, events[i].Type, want)
		}
	}
	added := events[2]
	if added.Item == nil || added.Item.Type == nil || *added.Item.Type != transcode.ResponsesMessageTypeFunctionCall ||
		added.Item.CallID == nil || *added.Item.CallID != "call_1" ||
		added.Item.Name == nil || *added.Item.Name != "get_weather" {
		t.Errorf("function_call added = %+v", added.Item)
	}
	if events[3].Arguments == nil || *events[3].Arguments != `{"loc` {
		t.Errorf("first arguments delta = %v", events[3].Arguments)
	}
	if events[4].Arguments == nil || *events[4].Arguments != `ation":"Tokyo"}` {
		t.Errorf("second arguments delta = %v", events[4].Arguments)
	}
	done := events[5]
	if done.Arguments == nil || *done.Arguments != `{"location":"Tokyo"}` {
		t.Errorf("arguments done = %v", done.Arguments)
	}
	itemDone := events[6]
	if itemDone.Item == nil || itemDone.Item.Status == nil || *itemDone.Item.Status != "completed" ||
		itemDone.Item.Arguments == nil || *itemDone.Item.Arguments != `{"location":"Tokyo"}` {
		t.Errorf("item done = %+v", itemDone.Item)
	}
	if events[7].Response == nil || events[7].Response.Status == nil || *events[7].Response.Status != "completed" {
		t.Errorf("terminal = %+v", events[7].Response)
	}
}

// TestConvertResponsesStreamResponseAnthropicStreamEvent feeds the responses
// stream fixture through the stateful accumulator and verifies the anthropic
// event sequence: message_start, thinking and text block lifecycles,
// message_delta with stop reason, and message_stop.
func TestConvertResponsesStreamResponseAnthropicStreamEvent(t *testing.T) {
	f := mustResponses(t)
	frames := testcorpus.ParseSSEFrames([]byte(f.Stream))
	var state transcode.AnthropicResponsesStreamState
	var events []transcode.AnthropicStreamEvent
	for _, frame := range frames {
		var event transcode.ResponsesStreamResponse
		if err := json.Unmarshal([]byte(frame), &event); err != nil {
			t.Fatalf("unmarshal responses stream frame: %v", err)
		}
		events = append(events, state.ConvertResponsesStreamResponseAnthropicStreamEvent(&event)...)
	}

	wantTypes := []transcode.AnthropicStreamEventType{
		transcode.AnthropicStreamEventTypeMessageStart,
		transcode.AnthropicStreamEventTypeContentBlockStart,
		transcode.AnthropicStreamEventTypeContentBlockDelta,
		transcode.AnthropicStreamEventTypeContentBlockStop,
		transcode.AnthropicStreamEventTypeContentBlockStart,
		transcode.AnthropicStreamEventTypeContentBlockDelta,
		transcode.AnthropicStreamEventTypeContentBlockDelta,
		transcode.AnthropicStreamEventTypeContentBlockStop,
		transcode.AnthropicStreamEventTypeMessageDelta,
		transcode.AnthropicStreamEventTypeMessageStop,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, events[i].Type, want)
		}
	}

	start := events[0]
	if start.Message == nil || start.Message.ID != "resp_8xyz" || start.Message.Type != "message" ||
		start.Message.Role != "assistant" || start.Message.Model != "gpt-4.1" {
		t.Errorf("message_start = %+v", start.Message)
	}
	if start.Message.Usage == nil || start.Message.Usage.InputTokens != 0 {
		t.Errorf("message_start usage = %+v, want zero usage", start.Message.Usage)
	}
	if events[1].ContentBlock == nil || events[1].ContentBlock.Type != transcode.AnthropicContentBlockTypeThinking ||
		events[1].Index == nil || *events[1].Index != 0 {
		t.Errorf("thinking block start = %+v", events[1])
	}
	if events[2].Delta == nil || events[2].Delta.Type != transcode.AnthropicStreamDeltaTypeThinkingDelta ||
		events[2].Delta.Thinking == nil || !strings.Contains(*events[2].Delta.Thinking, "get_weather") {
		t.Errorf("thinking delta = %+v", events[2].Delta)
	}
	if events[4].ContentBlock == nil || events[4].ContentBlock.Type != transcode.AnthropicContentBlockTypeText ||
		events[4].Index == nil || *events[4].Index != 1 {
		t.Errorf("text block start = %+v", events[4])
	}
	if events[5].Delta == nil || events[5].Delta.Type != transcode.AnthropicStreamDeltaTypeTextDelta ||
		events[5].Delta.Text == nil || *events[5].Delta.Text != "The weather in Tokyo is " {
		t.Errorf("first text delta = %+v", events[5].Delta)
	}
	delta := events[8]
	if delta.Delta == nil || delta.Delta.StopReason == nil || *delta.Delta.StopReason != transcode.AnthropicStopReasonEndTurn {
		t.Errorf("message_delta = %+v, want stop_reason end_turn", delta.Delta)
	}
	if delta.Usage == nil || delta.Usage.InputTokens != 45 || delta.Usage.OutputTokens != 25 {
		t.Errorf("message_delta usage = %+v", delta.Usage)
	}
	if delta.Usage.OutputTokensDetails == nil || delta.Usage.OutputTokensDetails.ThinkingTokens != 12 {
		t.Errorf("message_delta thinking tokens = %+v", delta.Usage.OutputTokensDetails)
	}

	// Events after the terminal message_stop are dropped.
	if tail := state.ConvertResponsesStreamResponseAnthropicStreamEvent(&transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeOutputTextDelta, Delta: new("x"), ItemID: new("msg_1")}); tail != nil {
		t.Errorf("post-terminal events = %+v, want none", tail)
	}
}

// TestConvertResponsesStreamResponseAnthropicStreamEventToolUse verifies the
// tool use lifecycle: content_block_start with the call id and name,
// input_json deltas for arguments, content_block_stop, and a message_delta
// stop reason of tool_use.
func TestConvertResponsesStreamResponseAnthropicStreamEventToolUse(t *testing.T) {
	var state transcode.AnthropicResponsesStreamState
	feed := func(event transcode.ResponsesStreamResponse) []transcode.AnthropicStreamEvent {
		return state.ConvertResponsesStreamResponseAnthropicStreamEvent(&event)
	}
	var events []transcode.AnthropicStreamEvent
	events = append(events, feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeCreated, Response: &transcode.ResponsesResponse{ID: "resp_1", Model: "gpt-4.1"}})...)
	events = append(events, feed(transcode.ResponsesStreamResponse{
		Type: transcode.ResponsesStreamResponseTypeOutputItemAdded,
		Item: &transcode.ResponsesMessage{
			ID: new("fc_1"), Type: new(transcode.ResponsesMessageTypeFunctionCall),
			ResponsesToolMessage: &transcode.ResponsesToolMessage{CallID: new("call_1"), Name: new("get_weather")},
		},
	})...)
	events = append(events, feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDelta, ItemID: new("fc_1"), Arguments: new(`{"location"`)})...)
	events = append(events, feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDelta, ItemID: new("fc_1"), Arguments: new(`:"Tokyo"}`)})...)
	events = append(events, feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeOutputItemDone, ItemID: new("fc_1")})...)
	events = append(events, feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeCompleted, Response: &transcode.ResponsesResponse{Status: new("completed")}})...)

	wantTypes := []transcode.AnthropicStreamEventType{
		transcode.AnthropicStreamEventTypeMessageStart,
		transcode.AnthropicStreamEventTypeContentBlockStart,
		transcode.AnthropicStreamEventTypeContentBlockDelta,
		transcode.AnthropicStreamEventTypeContentBlockDelta,
		transcode.AnthropicStreamEventTypeContentBlockStop,
		transcode.AnthropicStreamEventTypeMessageDelta,
		transcode.AnthropicStreamEventTypeMessageStop,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, events[i].Type, want)
		}
	}
	toolUse := events[1]
	if toolUse.ContentBlock == nil || toolUse.ContentBlock.Type != transcode.AnthropicContentBlockTypeToolUse ||
		toolUse.ContentBlock.ID == nil || *toolUse.ContentBlock.ID != "call_1" ||
		toolUse.ContentBlock.Name == nil || *toolUse.ContentBlock.Name != "get_weather" {
		t.Errorf("tool_use start = %+v", toolUse.ContentBlock)
	}
	if events[2].Delta == nil || events[2].Delta.Type != transcode.AnthropicStreamDeltaTypeInputJSONDelta ||
		events[2].Delta.PartialJSON == nil || *events[2].Delta.PartialJSON != `{"location"` {
		t.Errorf("first input_json delta = %+v", events[2].Delta)
	}
	if events[3].Delta == nil || events[3].Delta.PartialJSON == nil || *events[3].Delta.PartialJSON != `:"Tokyo"}` {
		t.Errorf("second input_json delta = %+v", events[3].Delta)
	}
	if events[5].Delta == nil || events[5].Delta.StopReason == nil || *events[5].Delta.StopReason != transcode.AnthropicStopReasonToolUse {
		t.Errorf("message_delta stop_reason = %+v, want tool_use", events[5].Delta)
	}
}

// TestConversionRoundTripConversions runs conversions in both directions and
// verifies the inverse of each field that survives the round trip.
func TestConvertStreamEventEdgeCases(t *testing.T) {
	var state transcode.AnthropicResponsesStreamState
	feed := func(event transcode.ResponsesStreamResponse) []transcode.AnthropicStreamEvent {
		return state.ConvertResponsesStreamResponseAnthropicStreamEvent(&event)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypePing}); len(events) != 1 || events[0].Type != transcode.AnthropicStreamEventTypePing {
		t.Errorf("ping = %+v", events)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeError}); len(events) != 1 || events[0].Type != transcode.AnthropicStreamEventTypeError {
		t.Errorf("error = %+v", events)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeInProgress}); events != nil {
		t.Errorf("in_progress = %+v, want skipped", events)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: "bogus"}); events != nil {
		t.Errorf("unknown type = %+v, want skipped", events)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeCreated}); events != nil {
		t.Errorf("created without response = %+v, want skipped", events)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeOutputItemAdded}); events != nil {
		t.Errorf("added without item = %+v, want skipped", events)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeOutputItemAdded, Item: &transcode.ResponsesMessage{}}); events != nil {
		t.Errorf("added without item type = %+v, want skipped", events)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeContentPartAdded}); events != nil {
		t.Errorf("content_part.added = %+v, want skipped", events)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeOutputTextDelta, ItemID: new("nope"), Delta: new("x")}); events != nil {
		t.Errorf("delta with unknown item id = %+v, want skipped", events)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeOutputItemDone}); events != nil {
		t.Errorf("done without id = %+v, want skipped", events)
	}
	first := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeCompleted, Response: &transcode.ResponsesResponse{Status: new("completed")}})
	if len(first) != 2 || first[0].Type != transcode.AnthropicStreamEventTypeMessageDelta || first[1].Type != transcode.AnthropicStreamEventTypeMessageStop {
		t.Fatalf("completed = %+v", first)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeCompleted}); events != nil {
		t.Errorf("duplicate completed = %+v, want skipped", events)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypeOutputTextDelta, ItemID: new("x"), Delta: new("y")}); events != nil {
		t.Errorf("post-terminal delta = %+v, want skipped", events)
	}
	if events := feed(transcode.ResponsesStreamResponse{Type: transcode.ResponsesStreamResponseTypePing}); len(events) != 1 || events[0].Type != transcode.AnthropicStreamEventTypePing {
		t.Errorf("post-terminal ping = %+v", events)
	}
	if events := feed(transcode.ResponsesStreamResponse{}); events != nil {
		t.Errorf("zero event = %+v, want skipped", events)
	}

	// Nil conversion inputs produce empty results.
	if out := transcode.ConvertResponsesRequestChatRequest(nil); out.Model != "" || out.Messages != nil {
		t.Errorf("nil responses request = %+v", out)
	}
	if out := transcode.ConvertChatResponseResponsesResponse(nil); out.Object != "" {
		t.Errorf("nil chat response = %+v", out)
	}
	if out := transcode.ConvertAnthropicRequestResponsesRequest(nil); out.Model != "" {
		t.Errorf("nil anthropic request = %+v", out)
	}
	if out := transcode.ConvertResponsesResponseAnthropicResponse(nil); out.Role != "" {
		t.Errorf("nil responses response = %+v", out)
	}
	var chatState transcode.ChatResponsesStreamState
	if events := chatState.ConvertChatResponseResponsesStreamResponse(nil); events != nil {
		t.Errorf("nil chat chunk = %+v", events)
	}
	var anthropicState transcode.AnthropicResponsesStreamState
	if events := anthropicState.ConvertResponsesStreamResponseAnthropicStreamEvent(nil); events != nil {
		t.Errorf("nil responses event = %+v", events)
	}
}

// TestConvertStreamTerminalVariants pins the boundary behaviors of the chat
// stream conversion: a length terminal carries incomplete details, reasoning
// deltas after text has opened are ignored, and tool call chunks referencing
// a prior call by index keep accumulating arguments.
func TestConvertStreamTerminalVariants(t *testing.T) {
	var state transcode.ChatResponsesStreamState
	feed := func(delta transcode.ChatStreamDelta, finish *string) []transcode.ResponsesStreamResponse {
		return state.ConvertChatResponseResponsesStreamResponse(&transcode.ChatStreamResponse{
			ID:      "chatcmpl-1",
			Model:   "gpt-4.1",
			Created: 1710000000,
			Choices: []transcode.ChatChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
		})
	}
	var events []transcode.ResponsesStreamResponse
	events = append(events, feed(transcode.ChatStreamDelta{Role: new("assistant")}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{Content: new("partial")}, nil)...)
	// Reasoning deltas after text has opened must not reopen a reasoning item.
	events = append(events, feed(transcode.ChatStreamDelta{Reasoning: new("late thinking")}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{}, new("length"))...)
	events = append(events, state.Terminal()...)

	var sawLateReasoning bool
	for i := range events {
		if events[i].Type == transcode.ResponsesStreamResponseTypeReasoningSummaryTextDelta {
			sawLateReasoning = true
		}
	}
	if sawLateReasoning {
		t.Error("reasoning delta after text: want it ignored")
	}
	terminal := events[len(events)-1]
	if terminal.Type != transcode.ResponsesStreamResponseTypeIncomplete {
		t.Fatalf("terminal type = %q, want response.incomplete", terminal.Type)
	}
	if terminal.Response == nil || terminal.Response.Status == nil || *terminal.Response.Status != "incomplete" {
		t.Fatalf("terminal response = %+v, want status incomplete", terminal.Response)
	}
	if string(terminal.Response.IncompleteDetails) != `{"reason":"max_output_tokens"}` {
		t.Errorf("incomplete_details = %s", terminal.Response.IncompleteDetails)
	}
	if len(terminal.Response.Output) != 1 || terminal.Response.Output[0].Status == nil || *terminal.Response.Output[0].Status != "incomplete" {
		t.Errorf("terminal output = %+v, want text item with incomplete status", terminal.Response.Output)
	}

	// A tool call chunk identifying the call by index only continues it.
	var toolState transcode.ChatResponsesStreamState
	feedTool := func(delta transcode.ChatStreamDelta, finish *string) []transcode.ResponsesStreamResponse {
		return toolState.ConvertChatResponseResponsesStreamResponse(&transcode.ChatStreamResponse{
			ID:      "chatcmpl-2",
			Model:   "gpt-4.1",
			Created: 1710000001,
			Choices: []transcode.ChatChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
		})
	}
	var toolEvents []transcode.ResponsesStreamResponse
	toolEvents = append(toolEvents, feedTool(transcode.ChatStreamDelta{Role: new("assistant")}, nil)...)
	toolEvents = append(toolEvents, feedTool(transcode.ChatStreamDelta{ToolCalls: []transcode.ChatAssistantMessageToolCall{{
		ID:       new("call_9"),
		Index:    new(0),
		Function: transcode.ChatAssistantMessageToolCallFunction{Name: new("f"), Arguments: `{"a"`},
	}}}, nil)...)
	toolEvents = append(toolEvents, feedTool(transcode.ChatStreamDelta{ToolCalls: []transcode.ChatAssistantMessageToolCall{{
		Index:    new(0),
		Function: transcode.ChatAssistantMessageToolCallFunction{Arguments: `:1}`},
	}}}, nil)...)
	toolEvents = append(toolEvents, feedTool(transcode.ChatStreamDelta{}, new("tool_calls"))...)
	toolEvents = append(toolEvents, toolState.Terminal()...)
	argsDone := toolEvents[len(toolEvents)-3]
	if argsDone.Type != transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDone || argsDone.Arguments == nil || *argsDone.Arguments != `{"a":1}` {
		t.Errorf("arguments done = %+v, want accumulated {\"a\":1}", argsDone)
	}
}

// TestConvertChatResponseResponsesStreamResponseTrailingUsage verifies that a
// trailing usage-only chunk (empty choices) attaches its usage to the
// terminal event.
func TestConvertChatResponseResponsesStreamResponseTrailingUsage(t *testing.T) {
	var state transcode.ChatResponsesStreamState
	feed := func(chunk transcode.ChatStreamResponse) []transcode.ResponsesStreamResponse {
		return state.ConvertChatResponseResponsesStreamResponse(&chunk)
	}
	var events []transcode.ResponsesStreamResponse
	events = append(events, feed(transcode.ChatStreamResponse{
		ID: "chatcmpl-1", Model: "gpt-4.1", Created: 1,
		Choices: []transcode.ChatChoice{{Index: 0, Delta: &transcode.ChatStreamDelta{Role: new("assistant")}}},
	})...)
	events = append(events, feed(transcode.ChatStreamResponse{
		ID: "chatcmpl-1", Model: "gpt-4.1", Created: 1,
		Choices: []transcode.ChatChoice{{Index: 0, Delta: &transcode.ChatStreamDelta{Content: new("hi")}}},
	})...)
	events = append(events, feed(transcode.ChatStreamResponse{
		ID: "chatcmpl-1", Model: "gpt-4.1", Created: 1,
		Choices: []transcode.ChatChoice{{Index: 0, Delta: &transcode.ChatStreamDelta{}, FinishReason: new("stop")}},
	})...)
	// No terminal event yet: held back for the trailing usage chunk.
	if len(events) != 8 {
		t.Fatalf("events before usage chunk = %d, want 8", len(events))
	}
	events = append(events, feed(transcode.ChatStreamResponse{
		ID: "chatcmpl-1", Model: "gpt-4.1", Created: 1,
		Usage: &transcode.ChatLLMUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	})...)
	terminal := events[len(events)-1]
	if terminal.Type != transcode.ResponsesStreamResponseTypeCompleted {
		t.Fatalf("terminal type = %q", terminal.Type)
	}
	if terminal.Response == nil || terminal.Response.Usage == nil || terminal.Response.Usage.TotalTokens != 15 {
		t.Errorf("terminal usage = %+v, want total 15 from the trailing chunk", terminal.Response)
	}
	// The state is finished; further chunks are ignored and Terminal is empty.
	if tail := state.Terminal(); tail != nil {
		t.Errorf("post-terminal events = %+v, want none", tail)
	}
}

// TestConvertChatResponseResponsesStreamResponseMultiToolCall verifies that
// every tool call of a single chat delta is streamed: a chunk carrying two
// tool calls produces two output_item.added events and two argument delta
// streams, and the finish chunk closes both items.
func TestConvertChatResponseResponsesStreamResponseMultiToolCall(t *testing.T) {
	var state transcode.ChatResponsesStreamState
	feed := func(delta transcode.ChatStreamDelta, finish *string) []transcode.ResponsesStreamResponse {
		return state.ConvertChatResponseResponsesStreamResponse(&transcode.ChatStreamResponse{
			ID:      "chatcmpl-1",
			Model:   "gpt-4.1",
			Created: 1710000000,
			Choices: []transcode.ChatChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
		})
	}
	var events []transcode.ResponsesStreamResponse
	events = append(events, feed(transcode.ChatStreamDelta{Role: new("assistant"), Content: new("")}, nil)...)
	// One chunk with two concurrent tool calls.
	events = append(events, feed(transcode.ChatStreamDelta{ToolCalls: []transcode.ChatAssistantMessageToolCall{
		{Index: new(0), ID: new("call_1"), Function: transcode.ChatAssistantMessageToolCallFunction{Name: new("get_weather"), Arguments: `{"loc`}},
		{Index: new(1), ID: new("call_2"), Function: transcode.ChatAssistantMessageToolCallFunction{Name: new("get_time"), Arguments: `{"city`}},
	}}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{ToolCalls: []transcode.ChatAssistantMessageToolCall{
		{Index: new(0), Function: transcode.ChatAssistantMessageToolCallFunction{Arguments: `ation":"Tokyo"}`}},
		{Index: new(1), Function: transcode.ChatAssistantMessageToolCallFunction{Arguments: `":"Tokyo"}`}},
	}}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{}, new("tool_calls"))...)
	events = append(events, state.Terminal()...)

	wantTypes := []transcode.ResponsesStreamResponseType{
		transcode.ResponsesStreamResponseTypeCreated,
		transcode.ResponsesStreamResponseTypeInProgress,
		transcode.ResponsesStreamResponseTypeOutputItemAdded,            // call_1
		transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDelta, // call_1 part 1
		transcode.ResponsesStreamResponseTypeOutputItemAdded,            // call_2
		transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDelta, // call_2 part 1
		transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDelta, // call_1 part 2
		transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDelta, // call_2 part 2
		transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDone,  // call_1
		transcode.ResponsesStreamResponseTypeOutputItemDone,             // call_1
		transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDone,  // call_2
		transcode.ResponsesStreamResponseTypeOutputItemDone,             // call_2
		transcode.ResponsesStreamResponseTypeCompleted,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, events[i].Type, want)
		}
	}
	// Both items were added, with distinct ids and output indices.
	if events[2].Item == nil || events[2].Item.CallID == nil || *events[2].Item.CallID != "call_1" {
		t.Errorf("first added = %+v", events[2].Item)
	}
	if events[4].Item == nil || events[4].Item.CallID == nil || *events[4].Item.CallID != "call_2" {
		t.Errorf("second added = %+v", events[4].Item)
	}
	// The arguments done events carry the complete argument strings. The
	// item ids are distinct from the chat call ids (the item/call id fix),
	// so resolve the arguments by the terminal items' call ids.
	argDone := map[string]string{}
	for _, e := range events {
		if e.Type == transcode.ResponsesStreamResponseTypeFunctionCallArgumentsDone && e.Arguments != nil {
			argDone[*e.ItemID] = *e.Arguments
		}
	}
	if len(argDone) != 2 {
		t.Errorf("arguments done = %v, want 2", argDone)
	}
	// The terminal envelope contains both completed function call items.
	terminal := events[len(events)-1]
	if terminal.Response == nil || len(terminal.Response.Output) != 2 {
		t.Fatalf("terminal output = %+v", terminal.Response)
	}
	if terminal.Response.Output[0].CallID == nil || *terminal.Response.Output[0].CallID != "call_1" ||
		terminal.Response.Output[1].CallID == nil || *terminal.Response.Output[1].CallID != "call_2" {
		t.Errorf("terminal items = %+v", terminal.Response.Output)
	}
	// The item ids differ from the call ids, and each item's arguments match.
	seen := map[string]bool{}
	for _, item := range terminal.Response.Output {
		if item.ID == nil || *item.ID == *item.CallID {
			t.Errorf("item id = %v, call id = %v: ids must differ", item.ID, item.CallID)
		}
		if item.Arguments == nil {
			t.Errorf("item %v has no arguments", item.CallID)
			continue
		}
		seen[*item.Arguments] = true
	}
	if !seen[`{"location":"Tokyo"}`] || !seen[`{"city":"Tokyo"}`] {
		t.Errorf("terminal arguments = %v, want both complete argument strings", seen)
	}
}

// TestConvertChatResponseResponsesStreamResponseRefusal verifies the refusal
// item lifecycle: output_item.added and content_part.added precede the
// refusal.delta events, the finish chunk closes the item, and the terminal
// envelope carries the refusal content block.
func TestConvertChatResponseResponsesStreamResponseRefusal(t *testing.T) {
	var state transcode.ChatResponsesStreamState
	feed := func(delta transcode.ChatStreamDelta, finish *string) []transcode.ResponsesStreamResponse {
		return state.ConvertChatResponseResponsesStreamResponse(&transcode.ChatStreamResponse{
			ID:      "chatcmpl-1",
			Model:   "gpt-4.1",
			Created: 1710000000,
			Choices: []transcode.ChatChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
		})
	}
	var events []transcode.ResponsesStreamResponse
	events = append(events, feed(transcode.ChatStreamDelta{Role: new("assistant"), Content: new("")}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{Refusal: new("I cannot answer that")}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{Refusal: new(" because it is private")}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{}, new("stop"))...)
	events = append(events, state.Terminal()...)

	wantTypes := []transcode.ResponsesStreamResponseType{
		transcode.ResponsesStreamResponseTypeCreated,
		transcode.ResponsesStreamResponseTypeInProgress,
		transcode.ResponsesStreamResponseTypeOutputItemAdded,  // refusal item
		transcode.ResponsesStreamResponseTypeContentPartAdded, // refusal part
		transcode.ResponsesStreamResponseTypeRefusalDelta,
		transcode.ResponsesStreamResponseTypeRefusalDelta,
		transcode.ResponsesStreamResponseTypeContentPartDone,
		transcode.ResponsesStreamResponseTypeOutputItemDone,
		transcode.ResponsesStreamResponseTypeCompleted,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, events[i].Type, want)
		}
	}
	// The refusal deltas carry the item and content indexes.
	for i := 4; i <= 5; i++ {
		if events[i].ItemID == nil || events[i].OutputIndex == nil || *events[i].OutputIndex != 0 ||
			events[i].ContentIndex == nil || *events[i].ContentIndex != 0 {
			t.Errorf("refusal delta %d = %+v", i, events[i])
		}
	}
	// The part added event carries the refusal part type.
	partAdded := events[3]
	if partAdded.Part == nil || partAdded.Part.Type != transcode.ResponsesMessageContentBlockTypeRefusal {
		t.Errorf("part added = %+v", partAdded.Part)
	}
	// The item done event carries the accumulated refusal text.
	itemDone := events[7]
	if itemDone.Item == nil || itemDone.Item.Content == nil ||
		len(itemDone.Item.Content.ContentBlocks) != 1 ||
		itemDone.Item.Content.ContentBlocks[0].Type != transcode.ResponsesMessageContentBlockTypeRefusal ||
		itemDone.Item.Content.ContentBlocks[0].Text == nil ||
		*itemDone.Item.Content.ContentBlocks[0].Text != "I cannot answer that because it is private" {
		t.Errorf("item done = %+v", itemDone.Item)
	}
	// The terminal envelope includes the refusal item.
	terminal := events[8]
	if terminal.Response == nil || len(terminal.Response.Output) != 1 ||
		terminal.Response.Output[0].Content == nil ||
		len(terminal.Response.Output[0].Content.ContentBlocks) != 1 {
		t.Fatalf("terminal output = %+v", terminal.Response)
	}
	block := terminal.Response.Output[0].Content.ContentBlocks[0]
	if block.Type != transcode.ResponsesMessageContentBlockTypeRefusal ||
		block.Text == nil || *block.Text != "I cannot answer that because it is private" {
		t.Errorf("terminal refusal block = %+v", block)
	}
}

// TestConvertChatResponseResponsesStreamResponseCreatedAt verifies the
// response.created event carries the upstream chunk timestamp rather than a
// synthesized one.
func TestConvertChatResponseResponsesStreamResponseCreatedAt(t *testing.T) {
	var state transcode.ChatResponsesStreamState
	events := state.ConvertChatResponseResponsesStreamResponse(&transcode.ChatStreamResponse{
		ID:      "chatcmpl-1",
		Model:   "gpt-4.1",
		Created: 1710000000,
		Choices: []transcode.ChatChoice{{Index: 0, Delta: &transcode.ChatStreamDelta{Role: new("assistant"), Content: new("")}}},
	})
	if len(events) < 2 || events[0].Type != transcode.ResponsesStreamResponseTypeCreated {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Response == nil || events[0].Response.CreatedAt != 1710000000 {
		t.Errorf("created_at = %v, want 1710000000", events[0].Response)
	}
}

// TestConvertChatResponseResponsesStreamResponseStartWithoutRole verifies the
// response envelope is emitted on the first delta even when it carries no
// role (providers may omit the role or send content first).
func TestConvertChatResponseResponsesStreamResponseStartWithoutRole(t *testing.T) {
	var state transcode.ChatResponsesStreamState
	events := state.ConvertChatResponseResponsesStreamResponse(&transcode.ChatStreamResponse{
		ID:      "chatcmpl-1",
		Model:   "gpt-4.1",
		Created: 1710000000,
		Choices: []transcode.ChatChoice{{Index: 0, Delta: &transcode.ChatStreamDelta{Content: new("hi")}}},
	})
	if len(events) < 3 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Type != transcode.ResponsesStreamResponseTypeCreated || events[1].Type != transcode.ResponsesStreamResponseTypeInProgress {
		t.Errorf("first events = %q, %q, want created then in_progress", events[0].Type, events[1].Type)
	}
	if events[0].Response == nil || events[0].Response.Model != "gpt-4.1" {
		t.Errorf("created envelope = %+v", events[0].Response)
	}
}

// TestConvertChatResponseResponsesStreamResponseContentFilter verifies a
// content_filter finish reason maps to response.incomplete with the
// content_filter reason, not a completed success.
func TestConvertChatResponseResponsesStreamResponseContentFilter(t *testing.T) {
	var state transcode.ChatResponsesStreamState
	feed := func(delta transcode.ChatStreamDelta, finish *string) []transcode.ResponsesStreamResponse {
		return state.ConvertChatResponseResponsesStreamResponse(&transcode.ChatStreamResponse{
			ID:      "chatcmpl-1",
			Model:   "gpt-4.1",
			Created: 1710000000,
			Choices: []transcode.ChatChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
		})
	}
	var events []transcode.ResponsesStreamResponse
	events = append(events, feed(transcode.ChatStreamDelta{Role: new("assistant"), Content: new("")}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{}, new("content_filter"))...)
	events = append(events, state.Terminal()...)

	terminal := events[len(events)-1]
	if terminal.Type != transcode.ResponsesStreamResponseTypeIncomplete {
		t.Errorf("terminal type = %q, want response.incomplete", terminal.Type)
	}
	if terminal.Response == nil || terminal.Response.Status == nil || *terminal.Response.Status != "incomplete" {
		t.Fatalf("terminal = %+v", terminal.Response)
	}
	if !strings.Contains(string(terminal.Response.IncompleteDetails), "content_filter") {
		t.Errorf("incomplete details = %s, want content_filter", terminal.Response.IncompleteDetails)
	}
}

// TestConvertChatResponseResponsesStreamResponseToolCallNameLater verifies a
// tool call name arriving after the call was first seen is picked up.
func TestConvertChatResponseResponsesStreamResponseToolCallNameLater(t *testing.T) {
	var state transcode.ChatResponsesStreamState
	feed := func(delta transcode.ChatStreamDelta, finish *string) []transcode.ResponsesStreamResponse {
		return state.ConvertChatResponseResponsesStreamResponse(&transcode.ChatStreamResponse{
			ID:      "chatcmpl-1",
			Model:   "gpt-4.1",
			Created: 1710000000,
			Choices: []transcode.ChatChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
		})
	}
	var events []transcode.ResponsesStreamResponse
	events = append(events, feed(transcode.ChatStreamDelta{Role: new("assistant"), Content: new("")}, nil)...)
	// First delta: id only, no name.
	events = append(events, feed(transcode.ChatStreamDelta{ToolCalls: []transcode.ChatAssistantMessageToolCall{{
		Index: new(0), ID: new("call_1"), Function: transcode.ChatAssistantMessageToolCallFunction{Arguments: `{"a":1}`},
	}}}, nil)...)
	// Second delta: name arrives.
	events = append(events, feed(transcode.ChatStreamDelta{ToolCalls: []transcode.ChatAssistantMessageToolCall{{
		Index: new(0), Function: transcode.ChatAssistantMessageToolCallFunction{Name: new("get_weather")},
	}}}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{}, new("tool_calls"))...)
	events = append(events, state.Terminal()...)

	terminal := events[len(events)-1]
	if terminal.Response == nil || len(terminal.Response.Output) != 1 {
		t.Fatalf("terminal output = %+v", terminal.Response)
	}
	item := terminal.Response.Output[0]
	if item.Name == nil || *item.Name != "get_weather" {
		t.Errorf("final tool call name = %v, want get_weather", item.Name)
	}
	if item.CallID == nil || *item.CallID != "call_1" {
		t.Errorf("final tool call id = %v, want call_1", item.CallID)
	}
}

// TestConvertChatResponseResponsesStreamResponseToolCallIndexAnchored
// verifies deltas carrying only a stream index resolve to the right call even
// when the calls arrive out of order (index 1 before index 0).
func TestConvertChatResponseResponsesStreamResponseToolCallIndexAnchored(t *testing.T) {
	var state transcode.ChatResponsesStreamState
	feed := func(delta transcode.ChatStreamDelta, finish *string) []transcode.ResponsesStreamResponse {
		return state.ConvertChatResponseResponsesStreamResponse(&transcode.ChatStreamResponse{
			ID:      "chatcmpl-1",
			Model:   "gpt-4.1",
			Created: 1710000000,
			Choices: []transcode.ChatChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
		})
	}
	var events []transcode.ResponsesStreamResponse
	events = append(events, feed(transcode.ChatStreamDelta{Role: new("assistant"), Content: new("")}, nil)...)
	// Index 1 arrives first, carrying its id.
	events = append(events, feed(transcode.ChatStreamDelta{ToolCalls: []transcode.ChatAssistantMessageToolCall{{
		Index: new(1), ID: new("call_b"), Function: transcode.ChatAssistantMessageToolCallFunction{Name: new("get_time"), Arguments: `{"city`},
	}}}, nil)...)
	// Index 0 arrives second with its id.
	events = append(events, feed(transcode.ChatStreamDelta{ToolCalls: []transcode.ChatAssistantMessageToolCall{{
		Index: new(0), ID: new("call_a"), Function: transcode.ChatAssistantMessageToolCallFunction{Name: new("get_weather"), Arguments: `{"loc`},
	}}}, nil)...)
	// Continuation deltas anchored by index only.
	events = append(events, feed(transcode.ChatStreamDelta{ToolCalls: []transcode.ChatAssistantMessageToolCall{{
		Index: new(1), Function: transcode.ChatAssistantMessageToolCallFunction{Arguments: `":"Tokyo"}`},
	}}}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{ToolCalls: []transcode.ChatAssistantMessageToolCall{{
		Index: new(0), Function: transcode.ChatAssistantMessageToolCallFunction{Arguments: `ation":"Tokyo"}`},
	}}}, nil)...)
	events = append(events, feed(transcode.ChatStreamDelta{}, new("tool_calls"))...)
	events = append(events, state.Terminal()...)

	terminal := events[len(events)-1]
	if terminal.Response == nil || len(terminal.Response.Output) != 2 {
		t.Fatalf("terminal output = %+v", terminal.Response)
	}
	callArgs := map[string]string{}
	for _, item := range terminal.Response.Output {
		if item.CallID != nil && item.Arguments != nil {
			callArgs[*item.CallID] = *item.Arguments
		}
	}
	if callArgs["call_a"] != `{"location":"Tokyo"}` || callArgs["call_b"] != `{"city":"Tokyo"}` {
		t.Errorf("arguments by call = %v", callArgs)
	}
}
