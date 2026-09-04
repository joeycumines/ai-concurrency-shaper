package transcode

import (
	"encoding/json"
	"strings"
	"testing"
)

func marshalEvent(t *testing.T, event ResponsesSSEEvent) map[string]json.RawMessage {
	t.Helper()
	data, err := MarshalResponsesEvent(event)
	if err != nil {
		t.Fatalf("marshal %T: %v", event, err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal %T: %v", event, err)
	}
	return out
}

func requireField(t *testing.T, m map[string]json.RawMessage, name string) json.RawMessage {
	t.Helper()
	value, ok := m[name]
	if !ok {
		t.Fatalf("missing required field %q in %s", name, string(m["type"]))
	}
	return value
}

func TestSequenceNumberZeroEmitted(t *testing.T) {
	// Sequence zero must be present, not omitted.
	builder := &ResponsesEventBuilder{}
	envelope := ResponseEnvelope{
		ID:        "resp_1",
		Object:    "response",
		CreatedAt: 1,
		Status:    "in_progress",
		Model:     "m",
		Output:    []ResponsesOutputItem{},
	}
	event := builder.InProgress(envelope)
	m := marshalEvent(t, event)
	if string(requireField(t, m, "sequence_number")) != "0" {
		t.Fatalf("sequence_number = %s, want 0", m["sequence_number"])
	}
	if string(requireField(t, m, "type")) != `"response.in_progress"` {
		t.Fatalf("type = %s", m["type"])
	}
}

func TestCreatedEventSequence(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	envelope := ResponseEnvelope{ID: "r", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m", Output: []ResponsesOutputItem{}}
	first := builder.Created(envelope)
	second := builder.Created(envelope)
	m1 := marshalEvent(t, first)
	m2 := marshalEvent(t, second)
	if string(m1["sequence_number"]) != "0" || string(m2["sequence_number"]) != "1" {
		t.Fatalf("sequence = %s, %s", m1["sequence_number"], m2["sequence_number"])
	}
}

func TestTextDeltaLogprobsRequired(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	event := builder.TextDelta("msg_1", 0, 0, "hi")
	m := marshalEvent(t, event)
	logprobs := requireField(t, m, "logprobs")
	if string(logprobs) != "[]" {
		t.Fatalf("logprobs = %s, want []", logprobs)
	}
	requireField(t, m, "item_id")
	requireField(t, m, "output_index")
	requireField(t, m, "content_index")
	requireField(t, m, "delta")
	// The delta payload is carried by delta, never arguments.
	if _, ok := m["arguments"]; ok {
		t.Fatalf("text delta must not carry arguments: %s", m)
	}
}

func TestTextDoneLogprobsRequired(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	event := builder.TextDone("msg_1", 0, 0, "hi")
	m := marshalEvent(t, event)
	if string(requireField(t, m, "logprobs")) != "[]" {
		t.Fatalf("logprobs = %s", m["logprobs"])
	}
	requireField(t, m, "text")
}

func TestFunctionArgumentsDeltaUsesDelta(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	event := builder.FunctionArgumentsDelta("fc_1", 1, `{"loc`)
	m := marshalEvent(t, event)
	if string(requireField(t, m, "delta")) != `"{\"loc"` {
		t.Fatalf("delta = %s", m["delta"])
	}
	if _, ok := m["arguments"]; ok {
		t.Fatal("delta event must not carry arguments")
	}
	if string(requireField(t, m, "type")) != `"response.function_call_arguments.delta"` {
		t.Fatalf("type = %s", m["type"])
	}
}

func TestFunctionArgumentsDoneCarriesArguments(t *testing.T) {
	// The official done event carries arguments and NO name: call identity
	// comes from the item-added lifecycle (review-08 blocker 5).
	builder := &ResponsesEventBuilder{}
	event := builder.FunctionArgumentsDone("fc_1", 1, `{"location":"Tokyo"}`)
	m := marshalEvent(t, event)
	if _, ok := m["name"]; ok {
		t.Fatalf("done event must not carry a name field: %v", m)
	}
	if string(requireField(t, m, "arguments")) != `"{\"location\":\"Tokyo\"}"` {
		t.Fatalf("arguments = %s", m["arguments"])
	}
	if string(requireField(t, m, "type")) != `"response.function_call_arguments.done"` {
		t.Fatalf("type = %s", m["type"])
	}
}

func TestFunctionArgumentsDoneEmptyArgumentsValidJSON(t *testing.T) {
	// Empty arguments are emitted as "{}", never omitted.
	builder := &ResponsesEventBuilder{}
	event := builder.FunctionArgumentsDone("fc_1", 0, "{}")
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	m := marshalEvent(t, event)
	requireField(t, m, "arguments")
}

func TestRefusalDoneExists(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	event := builder.RefusalDone("msg_1", 0, 0, "I cannot help")
	m := marshalEvent(t, event)
	if string(requireField(t, m, "type")) != `"response.refusal.done"` {
		t.Fatalf("type = %s", m["type"])
	}
	if string(requireField(t, m, "refusal")) != `"I cannot help"` {
		t.Fatalf("refusal = %s", m["refusal"])
	}
}

func TestRefusalDelta(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	event := builder.RefusalDelta("msg_1", 0, 0, "I can")
	m := marshalEvent(t, event)
	requireField(t, m, "delta")
	if string(requireField(t, m, "type")) != `"response.refusal.delta"` {
		t.Fatalf("type = %s", m["type"])
	}
}

func TestContentPartAddedCarriesPart(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	part := &ResponsesStreamOutputTextPart{
		Type:        "output_text",
		Text:        "hi",
		Annotations: []ResponsesAnnotation{},
	}
	event := builder.ContentPartAdded("msg_1", 0, 0, part)
	m := marshalEvent(t, event)
	rawPart := requireField(t, m, "part")
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(rawPart, &decoded); err != nil {
		t.Fatal(err)
	}
	requireField(t, decoded, "type")
	requireField(t, decoded, "text")
	requireField(t, decoded, "annotations")
	requireField(t, m, "item_id")
	requireField(t, m, "content_index")
}

func TestContentPartDoneCarriesPart(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	part := &ResponsesStreamRefusalPart{Type: "refusal", Refusal: "no"}
	event := builder.ContentPartDone("msg_1", 0, 1, part)
	m := marshalEvent(t, event)
	if string(requireField(t, m, "type")) != `"response.content_part.done"` {
		t.Fatalf("type = %s", m["type"])
	}
	requireField(t, m, "part")
}

func TestReasoningSummaryPartEventsCarryPart(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	part := ResponsesSummaryTextPart{Type: "summary_text"}
	added := builder.ReasoningSummaryPartAdded("rs_1", 0, 0, part)
	m := marshalEvent(t, added)
	rawPart := requireField(t, m, "part")
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(rawPart, &decoded); err != nil {
		t.Fatal(err)
	}
	requireField(t, decoded, "type")
	if string(decoded["type"]) != `"summary_text"` {
		t.Fatalf("part type = %s", decoded["type"])
	}
	done := builder.ReasoningSummaryPartDone("rs_1", 0, 0, part)
	m = marshalEvent(t, done)
	requireField(t, m, "part")
}

func TestReasoningSummaryTextEvents(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	delta := builder.ReasoningSummaryTextDelta("rs_1", 0, 0, "think")
	m := marshalEvent(t, delta)
	requireField(t, m, "delta")
	done := builder.ReasoningSummaryTextDone("rs_1", 0, 0, "thought")
	m = marshalEvent(t, done)
	requireField(t, m, "text")
}

func TestOutputItemAddedCarriesCompleteItem(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	item := &ResponsesOutputMessage{
		ID:      "msg_1",
		Type:    "message",
		Role:    "assistant",
		Status:  ResponsesItemInProgress,
		Content: ResponsesOutputContentParts{},
	}
	event := builder.OutputItemAdded(0, item)
	m := marshalEvent(t, event)
	rawItem := requireField(t, m, "item")
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(rawItem, &decoded); err != nil {
		t.Fatal(err)
	}
	requireField(t, decoded, "id")
	requireField(t, decoded, "type")
	requireField(t, decoded, "status")
	requireField(t, m, "output_index")
}

func TestOutputItemDone(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	item := &ResponsesFunctionCallOutputItem{
		ID:        "fc_1",
		Type:      "function_call",
		Status:    ResponsesItemCompleted,
		CallID:    "call_1",
		Name:      "f",
		Arguments: "{}",
	}
	event := builder.OutputItemDone(0, item)
	m := marshalEvent(t, event)
	if string(requireField(t, m, "type")) != `"response.output_item.done"` {
		t.Fatalf("type = %s", m["type"])
	}
}

func TestTerminalEvents(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	completed := builder.Completed(ResponseEnvelope{
		ID: "r", Object: "response", CreatedAt: 1, Status: "completed", Model: "m", Output: []ResponsesOutputItem{},
	})
	m := marshalEvent(t, completed)
	if string(requireField(t, m, "type")) != `"response.completed"` {
		t.Fatalf("type = %s", m["type"])
	}

	incomplete := builder.Incomplete(ResponseEnvelope{
		ID: "r", Object: "response", CreatedAt: 1, Status: "incomplete", Model: "m", Output: []ResponsesOutputItem{},
	})
	m = marshalEvent(t, incomplete)
	if string(requireField(t, m, "type")) != `"response.incomplete"` {
		t.Fatalf("type = %s", m["type"])
	}

	failed := builder.Failed(ResponseEnvelope{
		ID: "r", Object: "response", CreatedAt: 1, Status: "failed", Model: "m", Output: []ResponsesOutputItem{},
	})
	m = marshalEvent(t, failed)
	if string(requireField(t, m, "type")) != `"response.failed"` {
		t.Fatalf("type = %s", m["type"])
	}
}

func TestErrorEventRequiredFields(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	event := builder.Error("server_error", "boom", "")
	m := marshalEvent(t, event)
	if string(requireField(t, m, "type")) != `"error"` {
		t.Fatalf("type = %s", m["type"])
	}
	requireField(t, m, "code")
	requireField(t, m, "message")
	requireField(t, m, "param")
}

func TestEventValidateRejectsMismatch(t *testing.T) {
	builder := &ResponsesEventBuilder{}
	event := builder.TextDelta("msg_1", 0, 0, "x")
	event.EventBase.Type = "wrong"
	if err := event.Validate(); err == nil {
		t.Fatal("expected type mismatch rejection")
	}

	// Negative sequence numbers are rejected.
	builder = &ResponsesEventBuilder{}
	base := EventBase{Type: "error", SequenceNumber: 0}
	base.SequenceNumber = -1
	errorEvent := ResponseErrorEvent{EventBase: base, Code: "c", Message: "m"}
	if err := errorEvent.Validate(); err == nil {
		t.Fatal("expected negative sequence rejection")
	}
}

func TestEventNameMatchesJSONType(t *testing.T) {
	// Every emitted event's SSE name equals its JSON type tag.
	builder := &ResponsesEventBuilder{}
	envelope := ResponseEnvelope{ID: "r", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m", Output: []ResponsesOutputItem{}}
	events := []ResponsesSSEEvent{
		builder.Created(envelope),
		builder.InProgress(envelope),
		builder.OutputItemAdded(0, &ResponsesOutputMessage{ID: "m", Type: "message", Role: "assistant", Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{}}),
		builder.TextDelta("m", 0, 0, "x"),
		builder.FunctionArgumentsDelta("f", 0, "{}"),
		builder.FunctionArgumentsDone("f", 0, "{}"),
		builder.ContentPartAdded("m", 0, 0, &ResponsesStreamOutputTextPart{Type: "output_text", Text: "x", Annotations: []ResponsesAnnotation{}}),
		builder.RefusalDone("m", 0, 0, "no"),
		builder.Completed(ResponseEnvelope{ID: "r", Object: "response", CreatedAt: 1, Status: "completed", Model: "m", Output: []ResponsesOutputItem{}}),
	}
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &probe); err != nil {
			t.Fatal(err)
		}
		if probe.Type != event.EventType() {
			t.Fatalf("event %T: name %q != type %q", event, event.EventType(), probe.Type)
		}
		if event.EventType() == "" || !strings.HasPrefix(event.EventType(), "response.") && event.EventType() != "error" {
			t.Fatalf("event %T has odd name %q", event, event.EventType())
		}
	}
}
