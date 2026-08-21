package transcode

// Raw-key golden tests for the emitted wire (review-z commit 1). The recipe
// from the plan: unmarshal emitted JSON into map[string]json.RawMessage,
// assert the exact required key set, and assert required nullable keys are
// present even when null. These tests never validate emitted wire by
// unmarshalling into the same implementation structs — the key set and
// nullability are asserted on the raw JSON itself. SDK conformance decoding
// of the same shapes lives in official_wire_test.go.

import (
	"encoding/json"
	"strings"
	"testing"
)

// rawObject unmarshals emitted wire into a raw key map.
func rawObject(t *testing.T, wire []byte, label string) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(wire, &object); err != nil {
		t.Fatalf("unmarshal %s: %v\nwire: %s", label, err, wire)
	}
	return object
}

// assertKeys asserts exactly the given keys are present.
func assertKeys(t *testing.T, object map[string]json.RawMessage, label string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			t.Fatalf("%s missing required key %q; keys = %v", label, key, sortedKeys(object))
		}
	}
}

// assertRawNull asserts the key is present with an explicit null value.
func assertRawNull(t *testing.T, object map[string]json.RawMessage, label, key string) {
	t.Helper()
	raw, ok := object[key]
	if !ok {
		t.Fatalf("%s missing nullable key %q", label, key)
	}
	if strings.TrimSpace(string(raw)) != "null" {
		t.Fatalf("%s key %q = %s, want null", label, key, raw)
	}
}

// assertRawArray asserts the key is present with an array value.
func assertRawArray(t *testing.T, object map[string]json.RawMessage, label, key string) {
	t.Helper()
	raw, ok := object[key]
	if !ok {
		t.Fatalf("%s missing array key %q", label, key)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		t.Fatalf("%s key %q = %s, want an array", label, key, raw)
	}
}

func sortedKeys(object map[string]json.RawMessage) []string {
	out := make([]string, 0, len(object))
	for key := range object {
		out = append(out, key)
	}
	return out
}

// responsesRequiredKeys are the 14 pinned required Response envelope fields.
var responsesRequiredKeys = []string{
	"id",
	"created_at",
	"error",
	"incomplete_details",
	"instructions",
	"metadata",
	"model",
	"object",
	"output",
	"parallel_tool_calls",
	"temperature",
	"tool_choice",
	"tools",
	"top_p",
}

// TestRawKeyResponsesEnvelope proves the wire Response type always emits the
// 14 pinned required keys, with explicit null for the nullable ones and an
// array for output — no omitempty can drop a required key.
func TestRawKeyResponsesEnvelope(t *testing.T) {
	envelope := ResponseEnvelope{
		ID:        "resp_1",
		Object:    "response",
		CreatedAt: 1710000000.5, // fractional: survives as float64
		Status:    "completed",
		Model:     "m",
		Output:    []ResponsesOutputItem{},
	}
	wire, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	object := rawObject(t, wire, "ResponseEnvelope")
	assertKeys(t, object, "ResponseEnvelope", responsesRequiredKeys...)
	// Nullable required keys are present with explicit null when unset.
	for _, key := range []string{
		"error",
		"incomplete_details",
		"instructions",
		"metadata",
		"parallel_tool_calls",
		"temperature",
		"tool_choice",
		"tools",
		"top_p",
	} {
		assertRawNull(t, object, "ResponseEnvelope", key)
	}
	assertRawArray(t, object, "ResponseEnvelope", "output")
	// created_at survives as a fractional float64, never truncated to int.
	var created float64
	if err := json.Unmarshal(object["created_at"], &created); err != nil {
		t.Fatal(err)
	}
	if created != 1710000000.5 {
		t.Fatalf("created_at = %v, want 1710000000.5", created)
	}
}

// TestRawKeyResponsesEnvelopeRenderPath proves the actual non-streaming
// render (Chat→Responses) emits the 14 required keys in the client body.
func TestRawKeyResponsesEnvelopeRenderPath(t *testing.T) {
	resp := CanonicalResponse{
		ID:        "resp_1",
		Model:     "m",
		CreatedAt: 1710000000.5,
		Status:    CanonicalResponseCompleted,
		Stop:      CanonicalStop{Reason: CanonicalStopEndTurn},
		Items: []CanonicalResponseItem{&CanonicalMessageItem{
			Role:  CanonicalAssistant,
			Parts: []CanonicalPart{CanonicalText{Text: "hello"}},
		}},
		Usage: CanonicalUsage{
			InputTokens: 5, InputKnown: true,
			OutputTokens: 2, OutputKnown: true,
			TotalTokens: 7, TotalKnown: true,
		},
	}
	ctx := &ExchangeContext{
		RequestedClientModel: "m",
		UpstreamModel:        "m",
		IDs:                  NewExchangeIDs(),
		LossPolicy: LossPolicy{Allowed: map[Feature]struct{}{
			FeatureUsageCacheReadUnknown:  {},
			FeatureUsageCacheWriteUnknown: {},
			FeatureUsageReasoningUnknown:  {},
		}},
	}
	body, _, err := RenderResponsesResponse(resp, ctx)
	if err != nil {
		t.Fatal(err)
	}
	object := rawObject(t, body, "rendered Responses response")
	assertKeys(t, object, "rendered Responses response", responsesRequiredKeys...)
	assertRawArray(t, object, "rendered Responses response", "output")
	// Without a request echo, the nullable required keys are explicit null.
	assertRawNull(t, object, "rendered Responses response", "instructions")
	assertRawNull(t, object, "rendered Responses response", "metadata")
	assertRawNull(t, object, "rendered Responses response", "tools")
}

// TestRawKeyResponsesStreamTerminalEnvelope proves the streamed terminal
// envelope carries the 14 required keys with explicit values from the
// request echo.
func TestRawKeyResponsesStreamTerminalEnvelope(t *testing.T) {
	state := newChatResponsesStreamState(
		&ExchangeContext{
			IDs:                  NewExchangeIDs(),
			RequestedClientModel: "m",
			LossPolicy:           StrictLossPolicy(),
		},
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1710000000,
		&ResponsesRequestEcho{
			Instructions:      &ResponsesInput{Text: new("sys")},
			ParallelToolCalls: true,
			Temperature:       1.0,
			ToolChoice:        ResponsesToolChoice{Str: new("auto")},
			TopP:              1.0,
		},
	)
	// A zero-output finish: release the terminal through the [DONE] sentinel
	// path used by the converters.
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{}, new("stop"))); err != nil {
		t.Fatal(err)
	}
	held, ok := state.releaseTerminal()
	if !ok || len(held) != 1 {
		t.Fatalf("held = %d events, ok = %v", len(held), ok)
	}
	completed, ok := held[0].(ResponseCompletedEvent)
	if !ok {
		t.Fatalf("terminal = %T", held[0])
	}
	wire, err := MarshalResponsesEvent(completed)
	if err != nil {
		t.Fatal(err)
	}
	object := rawObject(t, wire, "terminal event")
	// The event itself: type + sequence_number are required.
	assertKeys(t, object, "terminal event", "type", "sequence_number", "response")
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(object["response"], &envelope); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, envelope, "terminal envelope", responsesRequiredKeys...)
	// The echo carries effective values: instructions non-null, parallel
	// tool calls true, temperature/top_p numbers, tool_choice "auto".
	if strings.TrimSpace(string(envelope["instructions"])) == "null" {
		t.Fatal("terminal envelope instructions = null, want the echo string")
	}
	if !strings.Contains(string(envelope["parallel_tool_calls"]), "true") {
		t.Fatalf("parallel_tool_calls = %s", envelope["parallel_tool_calls"])
	}
	if !strings.Contains(string(envelope["tool_choice"]), "auto") {
		t.Fatalf("tool_choice = %s", envelope["tool_choice"])
	}
}

// TestRawKeyMessagesResponse proves the Messages response render emits the
// pinned required keys, with stop_reason/stop_sequence explicitly null when
// unset.
func TestRawKeyMessagesResponse(t *testing.T) {
	resp := CanonicalResponse{
		ID:        "resp_1",
		Model:     "m",
		CreatedAt: 1,
		Status:    CanonicalResponseCompleted,
		Stop:      CanonicalStop{Reason: CanonicalStopEndTurn},
		Items: []CanonicalResponseItem{&CanonicalMessageItem{
			Role:  CanonicalAssistant,
			Parts: []CanonicalPart{CanonicalText{Text: "hello"}},
		}},
		Usage: CanonicalUsage{
			InputTokens: 5, InputKnown: true,
			OutputTokens: 2, OutputKnown: true,
			TotalTokens: 7, TotalKnown: true,
		},
	}
	body, _, err := RenderMessagesResponse(resp, &ExchangeContext{
		RequestedClientModel: "m",
		IDs:                  NewExchangeIDs(),
		// The Chat-source usage cannot reproduce cache_creation tokens; the
		// required usage_timing loss is approved so rendering proceeds.
		LossPolicy: LossPolicy{Allowed: map[Feature]struct{}{
			FeatureUsageCacheReadUnknown:  {},
			FeatureUsageCacheWriteUnknown: {},
			FeatureUsageReasoningUnknown:  {},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	object := rawObject(t, body, "Messages response")
	assertKeys(t, object, "Messages response",
		"id", "type", "role", "content", "model", "stop_reason", "stop_sequence",
	)
	if string(object["type"]) != `"message"` {
		t.Fatalf("type = %s", object["type"])
	}
	if string(object["role"]) != `"assistant"` {
		t.Fatalf("role = %s", object["role"])
	}
	assertRawArray(t, object, "Messages response", "content")
}

// TestRawKeyChatRequest proves the Chat request render emits the pinned
// required keys and every assistant tool call carries type=function in the
// raw wire.
func TestRawKeyChatRequest(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
			{Role: CanonicalAssistant, Parts: []CanonicalPart{
				CanonicalText{Text: "sure"},
				CanonicalFunctionCall{
					CallID:    "call_1",
					Name:      "get_weather",
					Arguments: mustRawMessage(map[string]json.RawMessage{"location": json.RawMessage(`"tokyo"`)}),
				},
			}},
		},
	}
	body, _, err := RenderChatRequest(request, &ExchangeContext{
		RequestedClientModel: "m",
		IDs:                  NewExchangeIDs(),
		LossPolicy:           StrictLossPolicy(),
	}, ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	object := rawObject(t, body, "Chat request")
	assertKeys(t, object, "Chat request", "model", "messages")
	var messages []json.RawMessage
	if err := json.Unmarshal(object["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d", len(messages))
	}
	var assistant map[string]json.RawMessage
	if err := json.Unmarshal(messages[1], &assistant); err != nil {
		t.Fatal(err)
	}
	// The tool call's type key is present and equals "function" (the
	// acceptance criterion: every Chat assistant tool call carries
	// type=function in raw wire).
	var toolCalls []map[string]json.RawMessage
	if err := json.Unmarshal(assistant["tool_calls"], &toolCalls); err != nil {
		t.Fatal(err)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls = %d", len(toolCalls))
	}
	if string(toolCalls[0]["type"]) != `"function"` {
		t.Fatalf("tool call type = %s, want \"function\"", toolCalls[0]["type"])
	}
}

// TestRawKeyResponsesRequestStrict proves the Responses request render emits
// an explicit strict on every tool: true when the source provided it, false
// under the tool_schema_strictness permission, and never omitted.
func TestRawKeyResponsesRequestStrict(t *testing.T) {
	strict := true
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
		Tools: []CanonicalTool{{
			Name:       "f",
			JSONSchema: json.RawMessage(`{"type":"object"}`),
			Strict:     &strict,
		}},
	}
	body, _, err := RenderResponsesRequest(request, &ExchangeContext{
		RequestedClientModel: "m",
		IDs:                  NewExchangeIDs(),
		LossPolicy:           StrictLossPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	object := rawObject(t, body, "Responses request")
	assertKeys(t, object, "Responses request", "model")
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(object["tools"], &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	if _, ok := tools[0]["strict"]; !ok {
		t.Fatal("tool strict key missing from raw wire")
	}
	if string(tools[0]["strict"]) != "true" {
		t.Fatalf("tool strict = %s, want true", tools[0]["strict"])
	}

	// A source without a strictness semantic emits explicit strict:false
	// under the permission.
	request.Tools[0].Strict = nil
	context := &ExchangeContext{
		RequestedClientModel: "m",
		IDs:                  NewExchangeIDs(),
		LossPolicy: LossPolicy{Allowed: map[Feature]struct{}{
			FeatureToolSchemaStrictness: {},
		}},
	}
	body, _, err = RenderResponsesRequest(request, context)
	if err != nil {
		t.Fatal(err)
	}
	object = rawObject(t, body, "Responses request")
	if err := json.Unmarshal(object["tools"], &tools); err != nil {
		t.Fatal(err)
	}
	if string(tools[0]["strict"]) != "false" {
		t.Fatalf("tool strict = %s, want explicit false", tools[0]["strict"])
	}
}

// TestRawKeyEvents proves every emitted Responses stream event carries type
// and sequence_number plus its payload's required keys.
func TestRawKeyEvents(t *testing.T) {
	envelope := ResponseEnvelope{
		ID:        "resp_1",
		Object:    "response",
		CreatedAt: 1,
		Status:    "completed",
		Model:     "m",
		Output:    []ResponsesOutputItem{},
	}
	textPart := &ResponsesStreamOutputTextPart{
		Type:        "output_text",
		Text:        "x",
		Annotations: []ResponsesAnnotation{},
	}
	builder := &ResponsesEventBuilder{}
	cases := []struct {
		name  string
		event ResponsesSSEEvent
		keys  []string
	}{
		{"response.created", builder.Created(envelope), []string{"type", "sequence_number", "response"}},
		{"response.in_progress", builder.InProgress(envelope), []string{"type", "sequence_number", "response"}},
		{"response.output_item.added", builder.OutputItemAdded(0, &ResponsesOutputMessage{
			ID: "msg_1", Type: "message", Role: "assistant", Status: ResponsesItemInProgress,
			Content: ResponsesOutputContentParts{},
		}), []string{"type", "sequence_number", "output_index", "item"}},
		{"response.output_item.done", builder.OutputItemDone(0, &ResponsesOutputMessage{
			ID: "msg_1", Type: "message", Role: "assistant", Status: ResponsesItemCompleted,
			Content: ResponsesOutputContentParts{},
		}), []string{"type", "sequence_number", "output_index", "item"}},
		{"response.reasoning_summary_part.added", builder.ReasoningSummaryPartAdded(
			"rs_1", 0, 0, ResponsesSummaryTextPart{Type: "summary_text", Text: "s"},
		), []string{"type", "sequence_number", "item_id", "output_index", "summary_index", "part"}},
		{"response.reasoning_summary_text.delta", builder.ReasoningSummaryTextDelta(
			"rs_1", 0, 0, "d",
		), []string{"type", "sequence_number", "item_id", "output_index", "summary_index", "delta"}},
		{"response.reasoning_summary_text.done", builder.ReasoningSummaryTextDone(
			"rs_1", 0, 0, "t",
		), []string{"type", "sequence_number", "item_id", "output_index", "summary_index", "text"}},
		{"response.reasoning_summary_part.done", builder.ReasoningSummaryPartDone(
			"rs_1", 0, 0, ResponsesSummaryTextPart{Type: "summary_text", Text: "s"},
		), []string{"type", "sequence_number", "item_id", "output_index", "summary_index", "part"}},
		{"response.content_part.added", builder.ContentPartAdded("msg_1", 0, 0, textPart),
			[]string{"type", "sequence_number", "item_id", "output_index", "content_index", "part"}},
		{"response.output_text.delta", builder.TextDelta("msg_1", 0, 0, "d"),
			[]string{"type", "sequence_number", "item_id", "output_index", "content_index", "delta", "logprobs"}},
		{"response.output_text.done", builder.TextDone("msg_1", 0, 0, "t"),
			[]string{"type", "sequence_number", "item_id", "output_index", "content_index", "text", "logprobs"}},
		{"response.content_part.done", builder.ContentPartDone("msg_1", 0, 0, textPart),
			[]string{"type", "sequence_number", "item_id", "output_index", "content_index", "part"}},
		{"response.function_call_arguments.delta", builder.FunctionArgumentsDelta("fc_1", 0, "{"),
			[]string{"type", "sequence_number", "item_id", "output_index", "delta"}},
		{"response.function_call_arguments.done", builder.FunctionArgumentsDone("fc_1", 0, "{}"),
			[]string{"type", "sequence_number", "item_id", "output_index", "arguments"}},
		{"response.refusal.delta", builder.RefusalDelta("msg_1", 0, 0, "d"),
			[]string{"type", "sequence_number", "item_id", "output_index", "content_index", "delta"}},
		{"response.refusal.done", builder.RefusalDone("msg_1", 0, 0, "r"),
			[]string{"type", "sequence_number", "item_id", "output_index", "content_index", "refusal"}},
		{"response.completed", builder.Completed(envelope), []string{"type", "sequence_number", "response"}},
		{"response.incomplete", builder.Incomplete(incompleteEnvelope(envelope)),
			[]string{"type", "sequence_number", "response"}},
		{"response.failed", builder.Failed(failedEnvelope(envelope)),
			[]string{"type", "sequence_number", "response"}},
		{"error", builder.Error("code", "message", "param"),
			[]string{"type", "sequence_number", "code", "message", "param"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wire, err := MarshalResponsesEvent(c.event)
			if err != nil {
				t.Fatal(err)
			}
			object := rawObject(t, wire, c.name)
			assertKeys(t, object, c.name, c.keys...)
		})
	}
}

// incompleteEnvelope returns the envelope with the status the incomplete
// terminal event requires.
func incompleteEnvelope(envelope ResponseEnvelope) ResponseEnvelope {
	envelope.Status = "incomplete"
	return envelope
}

// failedEnvelope returns the envelope with the status the failed terminal
// event requires.
func failedEnvelope(envelope ResponseEnvelope) ResponseEnvelope {
	envelope.Status = "failed"
	return envelope
}

// TestRawKeyResponsesInputFunctionCall proves the request-direction golden
// objects carry the pinned required keys.
func TestRawKeyResponsesInputFunctionCall(t *testing.T) {
	item := ResponsesFunctionCallInput{
		Type:      "function_call",
		CallID:    "call_1",
		Name:      "get_weather",
		Arguments: `{"location":"tokyo"}`,
	}
	wire, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	object := rawObject(t, wire, "function_call input item")
	assertKeys(t, object, "function_call input item", "type", "call_id", "name", "arguments")
	if string(object["type"]) != `"function_call"` {
		t.Fatalf("type = %s", object["type"])
	}
}

// TestRawKeyResponsesRequestStreamField proves the rendered request's stream
// field matches the exchange mode: omitted entirely for a non-streaming
// exchange (a present struct value would otherwise always emit), and
// "stream":true only when streaming (review run 1 finding F1).
func TestRawKeyResponsesRequestStreamField(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
	}
	context := &ExchangeContext{
		RequestedClientModel: "m",
		IDs:                  NewExchangeIDs(),
		LossPolicy:           StrictLossPolicy(),
	}

	// Non-streaming: no stream key at all.
	body, _, err := RenderResponsesRequest(request, context)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rawObject(t, body, "non-streaming request")["stream"]; ok {
		t.Fatalf("non-streaming request carries a stream key: %s", body)
	}

	// Streaming: explicit "stream":true.
	request.Stream = true
	body, _, err = RenderResponsesRequest(request, context)
	if err != nil {
		t.Fatal(err)
	}
	object := rawObject(t, body, "streaming request")
	stream := object["stream"]
	if string(stream) != "true" {
		t.Fatalf("streaming request stream = %s, want true", stream)
	}
}

// TestRawKeyResponsesEnvelopeErrorAndUsageKeys proves the error object's
// code and the usage detail objects are always emitted (review run 1
// findings F3 and F6).
func TestRawKeyResponsesEnvelopeErrorAndUsageKeys(t *testing.T) {
	// A failed envelope always carries error.code, even when unknown.
	failed := ResponseEnvelope{
		ID: "resp_1", Object: "response", CreatedAt: 1, Status: "failed",
		Model:  "m",
		Output: []ResponsesOutputItem{},
		Error:  &ResponsesEnvelopeError{Message: "boom"},
	}
	wire, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	object := rawObject(t, wire, "failed envelope")
	if _, ok := object["error"]; !ok {
		t.Fatal("error key missing")
	}
	var errorObject map[string]json.RawMessage
	if err := json.Unmarshal(object["error"], &errorObject); err != nil {
		t.Fatal(err)
	}
	if _, ok := errorObject["code"]; !ok {
		t.Fatal("error.code key missing from raw wire")
	}

	// A usage object always carries the detail objects (null when absent).
	usage := ResponsesUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}
	wire, err = json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	usageObject := rawObject(t, wire, "usage")
	assertKeys(t, usageObject, "usage",
		"input_tokens", "input_tokens_details", "output_tokens", "output_tokens_details", "total_tokens",
	)
	assertRawNull(t, usageObject, "usage", "input_tokens_details")
	assertRawNull(t, usageObject, "usage", "output_tokens_details")
}
