package transcode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testStreamContext() *ExchangeContext {
	return &ExchangeContext{
		IDs:        NewExchangeIDs(),
		LossPolicy: StrictLossPolicy(),
	}
}

func chatChunk(t *testing.T, delta ChatStreamDelta, finish *string) ChatStreamResponse {
	t.Helper()
	return ChatStreamResponse{
		ID:      "chatcmpl-1",
		Object:  "chat.completion.chunk",
		Created: 1710000000,
		Model:   "gpt-4.1",
		Choices: []ChatChoice{{
			Index:        0,
			Delta:        &delta,
			FinishReason: finish,
		}},
	}
}

func str(v string) *string { return &v }

func TestChatToResponsesTextStream(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)

	// Role + first content.
	events, err := state.Convert(chatChunk(t, ChatStreamDelta{
		Role:    str("assistant"),
		Content: str("Hello"),
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	// response.created + response.in_progress + output_item.added +
	// content_part.added + text delta
	if len(events) != 5 {
		t.Fatalf("events = %d: %T %T %T %T %T", len(events), events[0], events[1], events[2], events[3], events[4])
	}
	if _, ok := events[0].(ResponseCreatedEvent); !ok {
		t.Fatalf("event 0 = %T", events[0])
	}
	if _, ok := events[1].(ResponseInProgressEvent); !ok {
		t.Fatalf("event 1 = %T", events[1])
	}
	if _, ok := events[2].(ResponseOutputItemAddedEvent); !ok {
		t.Fatalf("event 2 = %T (want output_item.added)", events[2])
	}
	if _, ok := events[3].(ResponseContentPartAddedEvent); !ok {
		t.Fatalf("event 3 = %T", events[3])
	}
	delta, ok := events[4].(ResponseTextDeltaEvent)
	if !ok {
		t.Fatalf("event 4 = %T", events[4])
	}
	if delta.Delta != "Hello" || delta.Logprobs == nil {
		t.Fatalf("delta = %+v", delta)
	}
	if delta.SequenceNumber != 4 {
		t.Fatalf("sequence = %d, want 4", delta.SequenceNumber)
	}

	// Second content delta.
	events, err = state.Convert(chatChunk(t, ChatStreamDelta{Content: str(" world")}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	delta2, ok := events[0].(ResponseTextDeltaEvent)
	if !ok || delta2.Delta != " world" {
		t.Fatalf("delta2 = %+v", events[0])
	}

	// Finish.
	events, err = state.Convert(chatChunk(t, ChatStreamDelta{}, str("stop")))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("finish events = %d, want held", len(events))
	}
	if !state.sawFinish {
		t.Fatal("sawFinish not set")
	}
	// The terminal is held until [DONE] or EOF.
	held, ok := state.releaseTerminal()
	if !ok {
		t.Fatal("no held terminal")
	}
	// text done + content part done + output item done + completed
	if len(held) != 4 {
		t.Fatalf("held = %d: %s", len(held), eventTypes(held))
	}
	completed, ok := held[len(held)-1].(ResponseCompletedEvent)
	if !ok {
		t.Fatalf("last held = %T", held[len(held)-1])
	}
	if completed.Response.Status != "completed" {
		t.Fatalf("status = %q", completed.Response.Status)
	}
	if len(completed.Response.Output) != 1 {
		t.Fatalf("output = %d", len(completed.Response.Output))
	}
	message, ok := completed.Response.Output[0].(*ResponsesOutputMessage)
	if !ok {
		t.Fatalf("output[0] = %T", completed.Response.Output[0])
	}
	if message.Status != ResponsesItemCompleted {
		t.Fatalf("message status = %q", message.Status)
	}
	text, ok := message.Content[0].(*ResponsesOutputText)
	if !ok {
		t.Fatalf("content[0] = %T", message.Content[0])
	}
	if text.Text != "Hello world" || text.Annotations == nil {
		t.Fatalf("text = %+v", text)
	}
}

func TestChatToResponsesIncomplete(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: str("x")}, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{}, str("length"))); err != nil {
		t.Fatal(err)
	}
	held, ok := state.releaseTerminal()
	if !ok {
		t.Fatal("no held terminal")
	}
	last, ok := held[len(held)-1].(ResponseIncompleteEvent)
	if !ok {
		t.Fatalf("last = %T", held[len(held)-1])
	}
	if last.Response.Status != "incomplete" {
		t.Fatalf("status = %q", last.Response.Status)
	}
}

func TestChatToResponsesFunctionCallStream(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)

	// First fragment: index + id + name. The first chunk also carries
	// response.created + response.in_progress.
	events, err := state.Convert(chatChunk(t, ChatStreamDelta{ToolCalls: []ChatAssistantMessageToolCall{{
		Index: intPtr(0),
		ID:    str("call_1"),
		Function: ChatAssistantMessageToolCallFunction{
			Name:      str("get_weather"),
			Arguments: `{"loc`,
		},
	}}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	// created + in_progress + output_item.added + arguments delta
	if len(events) != 4 {
		t.Fatalf("events = %d: %s", len(events), eventTypes(events))
	}
	added, ok := events[2].(ResponseOutputItemAddedEvent)
	if !ok {
		t.Fatalf("event 2 = %T", events[2])
	}
	call, ok := added.Item.(*ResponsesFunctionCallOutputItem)
	if !ok {
		t.Fatalf("item = %T", added.Item)
	}
	if call.CallID != "call_1" || call.Name != "get_weather" || call.Status != ResponsesItemInProgress {
		t.Fatalf("call = %+v", call)
	}
	argDelta, ok := events[3].(ResponseFunctionCallArgumentsDeltaEvent)
	if !ok {
		t.Fatalf("event 3 = %T", events[3])
	}
	if argDelta.Delta != `{"loc` {
		t.Fatalf("delta = %q", argDelta.Delta)
	}
	if _, hasArgs := events[1].(ResponseFunctionCallArgumentsDoneEvent); hasArgs {
		t.Fatal("unexpected done")
	}

	// Second fragment: arguments continuation only.
	events, err = state.Convert(chatChunk(t, ChatStreamDelta{ToolCalls: []ChatAssistantMessageToolCall{{
		Index: intPtr(0),
		Function: ChatAssistantMessageToolCallFunction{
			Arguments: `ation":"Tokyo"}`,
		},
	}}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d: %s", len(events), eventTypes(events))
	}
	argDelta2, ok := events[0].(ResponseFunctionCallArgumentsDeltaEvent)
	if !ok {
		t.Fatalf("event = %T", events[0])
	}
	if argDelta2.Delta != `ation":"Tokyo"}` {
		t.Fatalf("delta2 = %q", argDelta2.Delta)
	}

	// Finish with tool_calls.
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{}, str("tool_calls"))); err != nil {
		t.Fatal(err)
	}
	held, ok := state.releaseTerminal()
	if !ok {
		t.Fatal("no held terminal")
	}
	// function args done + output item done + completed
	if len(held) != 3 {
		t.Fatalf("held = %d: %s", len(held), eventTypes(held))
	}
	done, ok := held[0].(ResponseFunctionCallArgumentsDoneEvent)
	if !ok {
		t.Fatalf("held[0] = %T", held[0])
	}
	if done.Name != "get_weather" || done.Arguments != `{"location":"Tokyo"}` {
		t.Fatalf("done = %+v", done)
	}
	completed, ok := held[2].(ResponseCompletedEvent)
	if !ok {
		t.Fatalf("held[2] = %T", held[2])
	}
	if len(completed.Response.Output) != 1 {
		t.Fatalf("output = %d", len(completed.Response.Output))
	}
	finalCall, ok := completed.Response.Output[0].(*ResponsesFunctionCallOutputItem)
	if !ok {
		t.Fatalf("output[0] = %T", completed.Response.Output[0])
	}
	if finalCall.Arguments != `{"location":"Tokyo"}` || finalCall.Status != ResponsesItemCompleted {
		t.Fatalf("final = %+v", finalCall)
	}
}

func TestChatToResponsesEmptyToolArguments(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)
	// Tool call with no arguments at all.
	_, err := state.Convert(chatChunk(t, ChatStreamDelta{ToolCalls: []ChatAssistantMessageToolCall{{
		Index:    intPtr(0),
		ID:       str("call_1"),
		Function: ChatAssistantMessageToolCallFunction{Name: str("f")},
	}}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{}, str("tool_calls"))); err != nil {
		t.Fatal(err)
	}
	held, _ := state.releaseTerminal()
	done, ok := held[0].(ResponseFunctionCallArgumentsDoneEvent)
	if !ok {
		t.Fatalf("held[0] = %T", held[0])
	}
	if done.Arguments != "{}" {
		t.Fatalf("empty arguments = %q, want {}", done.Arguments)
	}
}

func TestChatToResponsesBufferedToolStartUntilIdentity(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)
	// Name arrives first, id later: output_item.added must be withheld. The
	// first chunk still carries response.created + response.in_progress.
	events, err := state.Convert(chatChunk(t, ChatStreamDelta{ToolCalls: []ChatAssistantMessageToolCall{{
		Index:    intPtr(0),
		Function: ChatAssistantMessageToolCallFunction{Name: str("f"), Arguments: `{"x":`},
	}}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events before identity = %d: %s", len(events), eventTypes(events))
	}
	if _, ok := events[0].(ResponseCreatedEvent); !ok {
		t.Fatalf("event 0 = %T", events[0])
	}
	if _, ok := events[1].(ResponseInProgressEvent); !ok {
		t.Fatalf("event 1 = %T", events[1])
	}
	// id arrives now. The buffered arguments replay as one delta.
	events, err = state.Convert(chatChunk(t, ChatStreamDelta{ToolCalls: []ChatAssistantMessageToolCall{{
		Index: intPtr(0),
		ID:    str("call_1"),
		Function: ChatAssistantMessageToolCallFunction{
			Arguments: `1}`,
		},
	}}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	// output_item.added + replayed arguments delta
	if len(events) != 2 {
		t.Fatalf("events after identity = %d: %s", len(events), eventTypes(events))
	}
	added, ok := events[0].(ResponseOutputItemAddedEvent)
	if !ok {
		t.Fatalf("event 0 = %T", events[0])
	}
	call, ok := added.Item.(*ResponsesFunctionCallOutputItem)
	if !ok {
		t.Fatalf("item = %T", added.Item)
	}
	if call.CallID != "call_1" || call.Name != "f" {
		t.Fatalf("call = %+v", call)
	}
	replayed, ok := events[1].(ResponseFunctionCallArgumentsDeltaEvent)
	if !ok {
		t.Fatalf("event 1 = %T", events[1])
	}
	if replayed.Delta != `{"x":1}` {
		t.Fatalf("replayed delta = %q", replayed.Delta)
	}
}

func TestChatToResponsesProviderReasoningDropped(t *testing.T) {
	// Under strict policy, provider reasoning is a documented loss or a
	// rejection. The default mapping uses the strict policy, so the stream
	// fails rather than silently dropping.
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)
	_, err := state.Convert(chatChunk(t, ChatStreamDelta{Reasoning: str("think")}, nil))
	if err == nil {
		t.Fatal("expected reasoning rejection under strict policy")
	}
	var target *UnsupportedFeatureError
	if !errors.As(err, &target) {
		t.Fatalf("error = %T", err)
	}
	if target.Feature != string(FeatureProviderReasoning) {
		t.Fatalf("feature = %q", target.Feature)
	}

	// With the loss approved, the reasoning delta is dropped and the text
	// continues.
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureProviderReasoning: {}}}
	state = newChatResponsesStreamState(
		testStreamContext(),
		policy,
		"resp_1",
		"m",
		1,
		nil,
	)
	events, err := state.Convert(chatChunk(t, ChatStreamDelta{
		Content:   str("answer"),
		Reasoning: str("think"),
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("events = %d (created, in_progress, output_item.added, content added, text delta)", len(events))
	}
	for _, event := range events {
		if _, ok := event.(ResponseReasoningSummaryPartAddedEvent); ok {
			t.Fatal("reasoning must never be synthesized")
		}
	}
}

func TestChatToResponsesRefusal(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)
	events, err := state.Convert(chatChunk(t, ChatStreamDelta{Refusal: str("I cannot")}, nil))
	if err != nil {
		t.Fatal(err)
	}
	// created + in_progress + output_item.added + content_part.added +
	// refusal.delta
	if len(events) != 5 {
		t.Fatalf("events = %d: %s", len(events), eventTypes(events))
	}
	if _, ok := events[2].(ResponseOutputItemAddedEvent); !ok {
		t.Fatalf("event 2 = %T (want output_item.added)", events[2])
	}
	if _, ok := events[3].(ResponseContentPartAddedEvent); !ok {
		t.Fatalf("event 3 = %T", events[3])
	}
	if _, ok := events[4].(ResponseRefusalDeltaEvent); !ok {
		t.Fatalf("event 4 = %T", events[4])
	}
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{}, str("stop"))); err != nil {
		t.Fatal(err)
	}
	held, _ := state.releaseTerminal()
	var sawRefusalDone bool
	for _, event := range held {
		switch event.(type) {
		case ResponseRefusalDoneEvent:
			sawRefusalDone = true
		case ResponseCompletedEvent:
			// The refusal is part of the completed message content.
			completed := event.(ResponseCompletedEvent)
			message, ok := completed.Response.Output[0].(*ResponsesOutputMessage)
			if !ok || len(message.Content) != 1 {
				t.Fatalf("output = %+v", completed.Response.Output)
			}
			if _, ok := message.Content[0].(*ResponsesOutputRefusal); !ok {
				t.Fatalf("content[0] = %T", message.Content[0])
			}
		}
	}
	if !sawRefusalDone {
		t.Fatalf("missing refusal.done: %s", eventTypes(held))
	}
}

func TestChatToResponsesMultipleChoicesRejected(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)
	chunk := ChatStreamResponse{
		ID: "x", Object: "chat.completion.chunk", Created: 1, Model: "m",
		Choices: []ChatChoice{
			{Index: 0, Delta: &ChatStreamDelta{Content: str("a")}},
			{Index: 1, Delta: &ChatStreamDelta{Content: str("b")}},
		},
	}
	if _, err := state.Convert(chunk); err == nil {
		t.Fatal("expected multiple-choice rejection")
	}
}

func TestResponsesToAnthropicBasic(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		"msg_1",
		"m",
		1,
	)

	builder := &ResponsesEventBuilder{}
	envelope := ResponseEnvelope{
		ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
		Output: []ResponsesOutputItem{},
	}
	events, err := state.Convert(builder.Created(envelope))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	start := events[0]
	if start.Type != AnthropicStreamEventTypeMessageStart {
		t.Fatalf("event = %+v", start)
	}
	if start.Message == nil || start.Message.ID != "msg_1" || start.Message.Role != "assistant" {
		t.Fatalf("message = %+v", start.Message)
	}

	// output_item.added message + content_part.added + text delta.
	messageItem := &ResponsesOutputMessage{
		ID: "msg_2", Type: "message", Role: "assistant", Status: ResponsesItemInProgress,
		Content: ResponsesOutputContentParts{},
	}
	events, err = state.Convert(builder.OutputItemAdded(0, messageItem))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("item added events = %d", len(events))
	}
	events, err = state.Convert(builder.ContentPartAdded(
		"msg_2", 0, 0,
		&ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("content part events = %d", len(events))
	}
	blockStart := events[0]
	if blockStart.Type != AnthropicStreamEventTypeContentBlockStart || *blockStart.Index != 0 {
		t.Fatalf("block start = %+v", blockStart)
	}
	if blockStart.ContentBlock == nil || blockStart.ContentBlock.Type != AnthropicContentBlockTypeText {
		t.Fatalf("block = %+v", blockStart.ContentBlock)
	}

	events, err = state.Convert(builder.TextDelta("msg_2", 0, 0, "Hi"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("delta events = %d", len(events))
	}
	deltaEvent := events[0]
	if deltaEvent.Delta == nil || deltaEvent.Delta.Type != AnthropicStreamDeltaTypeTextDelta ||
		deltaEvent.Delta.Text == nil || *deltaEvent.Delta.Text != "Hi" {
		t.Fatalf("delta = %+v", deltaEvent)
	}

	events, err = state.Convert(builder.TextDone("msg_2", 0, 0, "Hi"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("text done events = %d", len(events))
	}

	events, err = state.Convert(builder.ContentPartDone(
		"msg_2", 0, 0,
		&ResponsesStreamOutputTextPart{Type: "output_text", Text: "Hi", Annotations: []ResponsesAnnotation{}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("content done events = %d", len(events))
	}
	blockStop := events[0]
	if blockStop.Type != AnthropicStreamEventTypeContentBlockStop {
		t.Fatalf("block stop = %+v", blockStop)
	}

	// The terminal is emitted immediately on completed: the Responses
	// protocol has no [DONE] sentinel, so holding it would block the stream
	// when the upstream keeps the connection open.
	completedEnvelope := ResponseEnvelope{
		ID: "resp_1", Object: "response", CreatedAt: 1, Status: "completed", Model: "m",
		Output: []ResponsesOutputItem{messageItem},
	}
	events, err = state.Convert(builder.Completed(completedEnvelope))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("completed events = %d, want message_delta + message_stop", len(events))
	}
	if events[0].Type != AnthropicStreamEventTypeMessageDelta {
		t.Fatalf("events[0] = %+v", events[0])
	}
	if events[0].Delta == nil || events[0].Delta.StopReason == nil || *events[0].Delta.StopReason != AnthropicStopReasonEndTurn {
		t.Fatalf("delta = %+v", events[0].Delta)
	}
	if events[1].Type != AnthropicStreamEventTypeMessageStop {
		t.Fatalf("events[1] = %+v", events[1])
	}
	if !state.sawTerminal {
		t.Fatal("terminal not seen")
	}
}

func TestResponsesToAnthropicFunctionCall(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		"msg_1",
		"m",
		1,
	)
	builder := &ResponsesEventBuilder{}
	envelope := ResponseEnvelope{
		ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
		Output: []ResponsesOutputItem{},
	}
	if _, err := state.Convert(builder.Created(envelope)); err != nil {
		t.Fatal(err)
	}

	callItem := &ResponsesFunctionCallOutputItem{
		ID: "fc_1", Type: "function_call", Status: ResponsesItemInProgress,
		CallID: "call_1", Name: "f", Arguments: "{}",
	}
	events, err := state.Convert(builder.OutputItemAdded(0, callItem))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	start := events[0]
	if start.Type != AnthropicStreamEventTypeContentBlockStart {
		t.Fatalf("start = %+v", start)
	}
	if start.ContentBlock == nil || start.ContentBlock.Type != AnthropicContentBlockTypeToolUse ||
		start.ContentBlock.ID == nil || *start.ContentBlock.ID != "call_1" ||
		start.ContentBlock.Name == nil || *start.ContentBlock.Name != "f" {
		t.Fatalf("tool block = %+v", start.ContentBlock)
	}

	events, err = state.Convert(builder.FunctionArgumentsDelta("fc_1", 0, `{"x":`))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("delta events = %d", len(events))
	}
	argDelta := events[0]
	if argDelta.Delta == nil || argDelta.Delta.Type != AnthropicStreamDeltaTypeInputJSONDelta ||
		argDelta.Delta.PartialJSON == nil || *argDelta.Delta.PartialJSON != `{"x":` {
		t.Fatalf("arg delta = %+v", argDelta)
	}

	events, err = state.Convert(builder.FunctionArgumentsDone("fc_1", 0, "f", `{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("done events = %d", len(events))
	}

	events, err = state.Convert(builder.OutputItemDone(0, &ResponsesFunctionCallOutputItem{
		ID: "fc_1", Type: "function_call", Status: ResponsesItemCompleted,
		CallID: "call_1", Name: "f", Arguments: `{"x":1}`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("item done events = %d", len(events))
	}
	stop := events[0]
	if stop.Type != AnthropicStreamEventTypeContentBlockStop {
		t.Fatalf("stop = %+v", stop)
	}
}

func TestResponsesToAnthropicFailedNeverEndTurn(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		"msg_1",
		"m",
		1,
	)
	builder := &ResponsesEventBuilder{}
	envelope := ResponseEnvelope{
		ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
		Output: []ResponsesOutputItem{},
	}
	if _, err := state.Convert(builder.Created(envelope)); err != nil {
		t.Fatal(err)
	}

	failedEnvelope := ResponseEnvelope{
		ID: "resp_1", Object: "response", CreatedAt: 1, Status: "failed", Model: "m",
		Output: []ResponsesOutputItem{},
		Error:  &ResponsesEnvelopeError{Code: "server_error", Message: "boom"},
	}
	events, err := state.Convert(builder.Failed(failedEnvelope))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	errorEvent := events[0]
	if errorEvent.Type != AnthropicStreamEventTypeError {
		t.Fatalf("event = %+v", errorEvent)
	}
	if errorEvent.Error == nil || errorEvent.Error.Message != "boom" || errorEvent.Error.Type == "" {
		t.Fatalf("error = %+v", errorEvent.Error)
	}
	if !state.sawErrorEvent {
		t.Fatal("sawErrorEvent not set")
	}
}

func TestResponsesToAnthropicErrorEventNested(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		"msg_1",
		"m",
		1,
	)
	builder := &ResponsesEventBuilder{}
	events, err := state.Convert(builder.Error("rate_limit_exceeded", "slow down", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	errorEvent := events[0]
	if errorEvent.Type != AnthropicStreamEventTypeError {
		t.Fatalf("event = %+v", errorEvent)
	}
	if errorEvent.Error == nil || errorEvent.Error.Type != "rate_limit_error" {
		t.Fatalf("error = %+v", errorEvent.Error)
	}
}

func TestResponsesToAnthropicReasoningNeverThinking(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		"msg_1",
		"m",
		1,
	)
	builder := &ResponsesEventBuilder{}
	envelope := ResponseEnvelope{
		ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
		Output: []ResponsesOutputItem{},
	}
	if _, err := state.Convert(builder.Created(envelope)); err != nil {
		t.Fatal(err)
	}
	events, err := state.Convert(builder.ReasoningSummaryPartAdded(
		"rs_1", 0, 0, ResponsesSummaryTextPart{Type: "summary_text"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("reasoning events = %d (must be dropped)", len(events))
	}
	events, err = state.Convert(builder.ReasoningSummaryTextDelta("rs_1", 0, 0, "think"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("reasoning delta events = %d", len(events))
	}
}

func TestChatToAnthropicCompositionInMemory(t *testing.T) {
	chat := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)
	anthropic := newAnthropicResponsesStreamState(
		testStreamContext(),
		"msg_1",
		"m",
		1,
	)
	converter := newChatToAnthropicConverter(chat, anthropic)

	// The chat chunk flows through both state machines without intermediate
	// JSON.
	batch, err := converter.Convert(SSEEvent{Data: []byte(`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	// message_start + content_block_start + text_delta
	if len(batch.Events) != 3 {
		t.Fatalf("events = %d", len(batch.Events))
	}
	var decoded []AnthropicStreamEvent
	for _, frame := range batch.Events {
		var event AnthropicStreamEvent
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, event)
	}
	if decoded[0].Type != AnthropicStreamEventTypeMessageStart {
		t.Fatalf("event 0 = %+v", decoded[0])
	}
	if decoded[1].Type != AnthropicStreamEventTypeContentBlockStart {
		t.Fatalf("event 1 = %+v", decoded[1])
	}
	if decoded[2].Type != AnthropicStreamEventTypeContentBlockDelta ||
		decoded[2].Delta == nil || decoded[2].Delta.Text == nil || *decoded[2].Delta.Text != "hi" {
		t.Fatalf("event 2 = %+v", decoded[2])
	}

	// Finish chunk holds the Chat terminal, which flows into the Anthropic
	// state machine.
	batch, err = converter.Convert(SSEEvent{Data: []byte(`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 0 {
		t.Fatalf("finish events = %d (both held)", len(batch.Events))
	}

	// [DONE] releases both held terminals.
	batch, err = converter.Convert(SSEEvent{Data: []byte("[DONE]")})
	if err != nil {
		t.Fatal(err)
	}
	if !batch.Terminal {
		t.Fatal("batch not terminal")
	}
	// content_block_stop + message_delta + message_stop
	if len(batch.Events) != 3 {
		t.Fatalf("done events = %d", len(batch.Events))
	}
	decoded = nil
	for _, frame := range batch.Events {
		var event AnthropicStreamEvent
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, event)
	}
	if decoded[len(decoded)-1].Type != AnthropicStreamEventTypeMessageStop {
		t.Fatalf("last = %+v", decoded[len(decoded)-1])
	}
	if decoded[len(decoded)-2].Type != AnthropicStreamEventTypeMessageDelta {
		t.Fatalf("second-last = %+v", decoded[len(decoded)-2])
	}
}

func TestChatToAnthropicFailedStream(t *testing.T) {
	// A truncated chat stream (EOF before finish) must produce an error via
	// FinalizeEOF, never a fabricated success.
	chat := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)
	anthropic := newAnthropicResponsesStreamState(
		testStreamContext(),
		"msg_1",
		"m",
		1,
	)
	converter := newChatToAnthropicConverter(chat, anthropic)
	if _, err := converter.Convert(SSEEvent{Data: []byte(`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := converter.FinalizeEOF(); err == nil {
		t.Fatal("expected truncation error on EOF without terminal")
	}
}

func TestDecodeResponsesSSEEvent(t *testing.T) {
	data := `{"type":"response.completed","sequence_number":5,"response":{"id":"r","object":"response","created_at":1,"status":"completed","model":"m","output":[]}}`
	event, err := decodeResponsesSSEEvent([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if event.EventType() != "response.completed" {
		t.Fatalf("type = %q", event.EventType())
	}
	if _, err := decodeResponsesSSEEvent([]byte(`{"type":"bogus"}`)); err == nil {
		t.Fatal("expected unknown type rejection")
	}
}

func TestValidateFinalToolInput(t *testing.T) {
	if err := validateFinalToolInput(`{"x":1}`); err != nil {
		t.Fatal(err)
	}
	if err := validateFinalToolInput(`[1,2]`); err == nil {
		t.Fatal("expected non-object rejection")
	}
	if err := validateFinalToolInput(`{"x":`); err == nil {
		t.Fatal("expected partial JSON rejection")
	}
}

func eventTypes(events []ResponsesSSEEvent) string {
	var parts []string
	for _, event := range events {
		parts = append(parts, event.EventType())
	}
	return strings.Join(parts, ",")
}

// TestChatToAnthropicInterleavedContentAndRefusal verifies that interleaved
// text and refusal deltas in the composed chat->anthropic direction target
// their own content blocks. The chat state machine keeps both parts open
// until finish, so deltas must resolve their block by part index, never by
// the lowest open block.
func TestChatToAnthropicInterleavedContentAndRefusal(t *testing.T) {
	converter := newChatToAnthropicConverter(
		newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			"resp_1",
			"m",
			1,
			nil,
		),
		newAnthropicResponsesStreamState(testStreamContext(), "resp_1", "m", 1),
	)

	feed := func(deltaJSON string) []AnthropicStreamEvent {
		t.Helper()
		batch, err := converter.Convert(SSEEvent{Data: []byte(
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":` + deltaJSON + `,"finish_reason":null}]}`,
		)})
		if err != nil {
			t.Fatal(err)
		}
		var events []AnthropicStreamEvent
		for _, frame := range batch.Events {
			var event AnthropicStreamEvent
			if err := json.Unmarshal(frame.Data, &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
		return events
	}

	// Text part opens, then refusal part opens, then both receive deltas.
	events := feed(`{"content":"hello"}`)
	events = append(events, feed(`{"refusal":"no way"}`)...)
	events = append(events, feed(`{"content":" world"}`)...)
	events = append(events, feed(`{"refusal":"!"}`)...)

	// Collect per-block text. Each contiguous text/refusal run opens its own
	// content part (the Responses protocol has no part reuse), so every
	// delta must land in the block its part opened — never merged into the
	// lowest open block.
	blockText := map[int]string{}
	for _, event := range events {
		if event.Type == AnthropicStreamEventTypeContentBlockDelta &&
			event.Delta != nil && event.Delta.Text != nil && event.Index != nil {
			blockText[*event.Index] += *event.Delta.Text
		}
	}
	want := map[int]string{
		0: "hello",
		1: "no way",
		2: " world",
		3: "!",
	}
	if len(blockText) != len(want) {
		t.Fatalf("blocks with text = %v, want %v", blockText, want)
	}
	for index, text := range want {
		if blockText[index] != text {
			t.Fatalf("block %d text = %q, want %q (deltas misrouted)", index, blockText[index], text)
		}
	}

	// The finish must close both blocks and emit the terminal.
	batch, err := converter.Convert(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 0 {
		t.Fatalf("finish events = %d", len(batch.Events))
	}
	batch, err = converter.Convert(SSEEvent{Data: []byte("[DONE]")})
	if err != nil {
		t.Fatal(err)
	}
	if !batch.Terminal {
		t.Fatal("batch not terminal")
	}
	var stops []AnthropicStreamEvent
	for _, frame := range batch.Events {
		var event AnthropicStreamEvent
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			t.Fatal(err)
		}
		stops = append(stops, event)
	}
	blockStops := 0
	for _, event := range stops {
		if event.Type == AnthropicStreamEventTypeContentBlockStop {
			blockStops++
		}
	}
	if blockStops != 4 {
		t.Fatalf("content block stops = %d, want 4", blockStops)
	}
}

// TestChatToResponsesUnstartedToolCallRejected verifies that a tool-call
// fragment which never receives an id or name makes the finish fail with a
// conversion error instead of being silently dropped behind a successful
// completion.
func TestChatToResponsesUnstartedToolCallRejected(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)
	// A fragment with only arguments (no index attribution, no id/name).
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{
		ToolCalls: []ChatAssistantMessageToolCall{{
			Function: ChatAssistantMessageToolCallFunction{Arguments: `{"x":1}`},
		}},
	}, nil)); err != nil {
		t.Fatal(err)
	}
	_, err := state.Convert(chatChunk(t, ChatStreamDelta{}, str("tool_calls")))
	if err == nil {
		t.Fatal("finish accepted an unstarted tool call")
	}

	// Two index-less fragments with different ids must not merge.
	state2 := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)
	chunk := ChatStreamResponse{
		ID: "c", Model: "m", Created: 1,
		Choices: []ChatChoice{{Index: 0, Delta: &ChatStreamDelta{
			ToolCalls: []ChatAssistantMessageToolCall{
				{ID: str("call_a"), Function: ChatAssistantMessageToolCallFunction{Name: str("f_a"), Arguments: `{}`}},
				{ID: str("call_b"), Function: ChatAssistantMessageToolCallFunction{Name: str("f_b"), Arguments: `{}`}},
			},
		}}},
	}
	events, err := state2.Convert(chunk)
	if err != nil {
		t.Fatal(err)
	}
	var callIDs []string
	for _, event := range events {
		if added, ok := event.(ResponseOutputItemAddedEvent); ok {
			if fc, ok := added.Item.(*ResponsesFunctionCallOutputItem); ok {
				callIDs = append(callIDs, fc.CallID)
			}
		}
	}
	if len(callIDs) != 2 || callIDs[0] == callIDs[1] {
		t.Fatalf("index-less parallel calls merged: %v", callIDs)
	}
}

// TestChatToResponsesToolCallIndexCollision verifies that a fragment whose
// index resolves to a pending call with a different id is rejected instead of
// silently merging identities.
func TestChatToResponsesToolCallIndexCollision(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{
		ToolCalls: []ChatAssistantMessageToolCall{{
			Index:    intPtr(0),
			ID:       str("call_a"),
			Function: ChatAssistantMessageToolCallFunction{Name: str("f_a"), Arguments: `{}`},
		}},
	}, nil)); err != nil {
		t.Fatal(err)
	}
	_, err := state.Convert(chatChunk(t, ChatStreamDelta{
		ToolCalls: []ChatAssistantMessageToolCall{{
			Index:    intPtr(0),
			ID:       str("call_b"),
			Function: ChatAssistantMessageToolCallFunction{Name: str("f_b"), Arguments: `{}`},
		}},
	}, nil))
	if err == nil {
		t.Fatal("conflicting index/id fragment accepted")
	}
}

// TestChatToResponsesAmbiguousFragmentRejected verifies that an index-less,
// id-less continuation fragment is rejected when several pending tool calls
// make the attribution ambiguous.
func TestChatToResponsesAmbiguousFragmentRejected(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"m",
		1,
		nil,
	)
	// Two identified calls at distinct indexes.
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{
		ToolCalls: []ChatAssistantMessageToolCall{{
			Index:    intPtr(0),
			ID:       str("call_a"),
			Function: ChatAssistantMessageToolCallFunction{Name: str("f_a"), Arguments: `{}`},
		}},
	}, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{
		ToolCalls: []ChatAssistantMessageToolCall{{
			Index:    intPtr(1),
			ID:       str("call_b"),
			Function: ChatAssistantMessageToolCallFunction{Name: str("f_b"), Arguments: `{}`},
		}},
	}, nil)); err != nil {
		t.Fatal(err)
	}
	// An index-less, id-less fragment cannot be attributed to either.
	_, err := state.Convert(chatChunk(t, ChatStreamDelta{
		ToolCalls: []ChatAssistantMessageToolCall{{
			Function: ChatAssistantMessageToolCallFunction{Arguments: `{"x":1}`},
		}},
	}, nil))
	if err == nil {
		t.Fatal("ambiguous tool call fragment accepted")
	}
}
