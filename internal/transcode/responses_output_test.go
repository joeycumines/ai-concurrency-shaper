package transcode

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestResponsesOutputMessageContent(t *testing.T) {
	data := `[
		{"type":"output_text","text":"hello","annotations":[]},
		{"type":"refusal","refusal":"I cannot help"}
	]`
	var parts ResponsesOutputContentParts
	if err := json.Unmarshal([]byte(data), &parts); err != nil {
		t.Fatal(err)
	}
	if err := parts.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	text, ok := parts[0].(*ResponsesOutputText)
	if !ok {
		t.Fatalf("part 0 = %T", parts[0])
	}
	if text.Annotations == nil {
		t.Fatal("annotations must be present")
	}
	refusal, ok := parts[1].(*ResponsesOutputRefusal)
	if !ok {
		t.Fatalf("part 1 = %T", parts[1])
	}
	if refusal.Refusal != "I cannot help" {
		t.Fatalf("refusal = %q", refusal.Refusal)
	}
}

func TestResponsesOutputTextRequiresAnnotations(t *testing.T) {
	var part ResponsesOutputText
	decodeJSON(t, `{"type":"output_text","text":"x"}`, &part)
	if err := part.Validate(); err == nil {
		t.Fatal("expected error for missing annotations")
	}
	part.Annotations = []ResponsesAnnotation{}
	if err := part.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesOutputContentUnsupportedPart(t *testing.T) {
	var parts ResponsesOutputContentParts
	err := json.Unmarshal([]byte(`[{"type":"output_audio","audio":"x"}]`), &parts)
	if err == nil {
		t.Fatal("expected error for output_audio")
	}
	asUnsupportedFeatureError(t, err)
}

func TestResponsesOutputMessage(t *testing.T) {
	data := `{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"status":"completed",
		"content":[{"type":"output_text","text":"hi","annotations":[]}]
	}`
	item, err := DecodeResponsesOutputItem([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	message, ok := item.(*ResponsesOutputMessage)
	if !ok {
		t.Fatalf("item = %T", item)
	}
	if message.ID != "msg_1" || message.Status != ResponsesItemCompleted {
		t.Fatalf("message = %+v", message)
	}
}

func TestResponsesOutputFunctionCall(t *testing.T) {
	data := `{
		"id":"fc_1",
		"type":"function_call",
		"status":"completed",
		"call_id":"call_1",
		"name":"f",
		"arguments":"{\"x\":1}"
	}`
	item, err := DecodeResponsesOutputItem([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	call, ok := item.(*ResponsesFunctionCallOutputItem)
	if !ok {
		t.Fatalf("item = %T", item)
	}
	if call.Name != "f" || call.CallID != "call_1" {
		t.Fatalf("call = %+v", call)
	}
}

func TestResponsesOutputReasoning(t *testing.T) {
	data := `{
		"id":"rs_1",
		"type":"reasoning",
		"status":"completed",
		"summary":[{"type":"summary_text","text":"thinking"}]
	}`
	item, err := DecodeResponsesOutputItem([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	reason, ok := item.(*ResponsesReasoningOutputItem)
	if !ok {
		t.Fatalf("item = %T", item)
	}
	if len(reason.Summary) != 1 || reason.Summary[0].Text != "thinking" {
		t.Fatalf("reason = %+v", reason)
	}
}

func TestResponsesOutputUnsupportedItem(t *testing.T) {
	_, err := DecodeResponsesOutputItem([]byte(`{"type":"web_search_call"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	ue := asUnsupportedFeatureError(t, err)
	if ue.Feature != "web_search_call" || ue.Path != "output[].type" {
		t.Fatalf("ue = %+v", ue)
	}
}

func TestResponsesOutputInvalid(t *testing.T) {
	tests := []string{
		`{"type":"message","role":"assistant","status":"completed","content":[]}`,      // no id
		`{"type":"message","id":"m","role":"user","status":"completed","content":[]}`,  // wrong role
		`{"type":"message","id":"m","role":"assistant","status":"bogus","content":[]}`, // bad status
		`{"type":"function_call","id":"f","status":"completed","call_id":"c","name":"n","arguments":"not json"}`,
	}
	for _, data := range tests {
		_, err := DecodeResponsesOutputItem([]byte(data))
		if err == nil {
			t.Errorf("accepted invalid output %s", data)
		}
	}
}

func TestResponsesEnvelopeRoundTrip(t *testing.T) {
	item := &ResponsesOutputMessage{
		ID:     "msg_1",
		Type:   "message",
		Role:   "assistant",
		Status: ResponsesItemCompleted,
		Content: ResponsesOutputContentParts{
			&ResponsesOutputText{Type: "output_text", Text: "hi", Annotations: []ResponsesAnnotation{}},
		},
	}
	envelope := ResponseEnvelope{
		ID:        "resp_1",
		Object:    "response",
		CreatedAt: 1710000000,
		Status:    "completed",
		Model:     "gpt-4.1",
		Output:    []ResponsesOutputItem{item},
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var again ResponseEnvelope
	if err := json.Unmarshal(data, &again); err != nil {
		t.Fatalf("decode own encoding: %v\n%s", err, data)
	}
	if err := again.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(again.Output) != 1 {
		t.Fatalf("output = %d", len(again.Output))
	}
}

func TestResponsesEnvelopeValidate(t *testing.T) {
	base := ResponseEnvelope{
		ID:        "resp_1",
		Object:    "response",
		CreatedAt: 1,
		Status:    "completed",
		Model:     "m",
		Output:    []ResponsesOutputItem{},
	}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ResponseEnvelope{Object: "response", Model: "m", Output: []ResponsesOutputItem{}}).Validate(); err == nil {
		t.Fatal("expected error for missing id")
	}
	if err := (ResponseEnvelope{ID: "r", Model: "m", Output: []ResponsesOutputItem{}}).Validate(); err == nil {
		t.Fatal("expected error for missing object")
	}
	if err := (ResponseEnvelope{ID: "r", Object: "response", Output: []ResponsesOutputItem{}}).Validate(); err == nil {
		t.Fatal("expected error for missing model")
	}
	if err := (ResponseEnvelope{ID: "r", Object: "response", Model: "m"}).Validate(); err == nil {
		t.Fatal("expected error for nil output")
	}
}

func TestResponsesToolChoice(t *testing.T) {
	var choice ResponsesToolChoice
	decodeJSON(t, `"auto"`, &choice)
	if err := choice.Validate(); err != nil {
		t.Fatal(err)
	}
	if choice.Str == nil || *choice.Str != "auto" {
		t.Fatalf("choice = %+v", choice)
	}
	decodeJSON(t, `{"type":"function","name":"f"}`, &choice)
	if err := choice.Validate(); err != nil {
		t.Fatal(err)
	}
	if choice.Named == nil || choice.Named.Name != "f" {
		t.Fatalf("choice = %+v", choice)
	}
	// Invalid string values are rejected at decode time.
	if err := json.Unmarshal([]byte(`"always"`), &choice); err == nil {
		t.Fatal("expected error for invalid tool choice string")
	}
	// Round trip.
	decodeJSON(t, `{"type":"function","name":"f"}`, &choice)
	encoded := mustJSON(t, choice)
	var again ResponsesToolChoice
	if err := json.Unmarshal(encoded, &again); err != nil {
		t.Fatal(err)
	}
	if again.Named == nil || again.Named.Name != "f" {
		t.Fatalf("round trip = %+v", again)
	}
}

func TestResponsesToolValidate(t *testing.T) {
	tool := ResponsesTool{Type: "function", Name: "f", Parameters: json.RawMessage(`{"type":"object"}`)}
	if err := tool.Validate(); err != nil {
		t.Fatal(err)
	}
	tool = ResponsesTool{Type: "web_search", Name: "ws"}
	if err := tool.Validate(); err == nil {
		t.Fatal("expected error for non-function tool")
	}
	tool = ResponsesTool{Type: "function"}
	if err := tool.Validate(); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestCanonicalizeResponsesToolChoice(t *testing.T) {
	choice := ResponsesToolChoice{Str: stringPtr("auto")}
	out, err := canonicalizeResponsesToolChoice(choice)
	if err != nil {
		t.Fatal(err)
	}
	if out.Mode != "auto" {
		t.Fatalf("mode = %q", out.Mode)
	}
	choice = ResponsesToolChoice{Named: &ResponsesToolChoiceNamed{Type: "function", Name: "f"}}
	out, err = canonicalizeResponsesToolChoice(choice)
	if err != nil {
		t.Fatal(err)
	}
	if out.Mode != "named" || out.Name != "f" {
		t.Fatalf("out = %+v", out)
	}
	// "required" is part of the official contract and canonicalizes as-is.
	choice = ResponsesToolChoice{Str: stringPtr("required")}
	requiredOut, err := canonicalizeResponsesToolChoice(choice)
	if err != nil {
		t.Fatalf("required tool choice rejected: %v", err)
	}
	if requiredOut.Mode != "required" {
		t.Fatalf("mode = %q, want required", requiredOut.Mode)
	}
}

func TestSplitImageDataURL(t *testing.T) {
	mediaType, data, err := splitImageDataURL("data:image/png;base64,AAAA")
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != "image/png" || data != "AAAA" {
		t.Fatalf("%q %q", mediaType, data)
	}
	mediaType, data, err = splitImageDataURL("https://x/y.png")
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != "" || data != "" {
		t.Fatalf("plain URL: %q %q", mediaType, data)
	}
	if _, _, err := splitImageDataURL("data:image/svg+xml;base64,AAAA"); err == nil {
		t.Fatal("expected unsupported media type rejection")
	}
	if _, _, err := splitImageDataURL("data:image/png;base64,"); err == nil {
		t.Fatal("expected empty data rejection")
	}
}

func TestExchangeIDs(t *testing.T) {
	// IDs within one exchange share the random prefix and carry a
	// monotonic counter (review-08 blocker 6); the prefix shape is 16
	// lowercase hex characters.
	ids := NewExchangeIDs()
	first := ids.New("msg_")
	second := ids.New("fc_")
	var prefix string
	if n, err := fmt.Sscanf(first, "msg_%16s_1", &prefix); n != 1 || err != nil {
		t.Fatalf("first = %q, want the msg_<16 hex>_1 shape", first)
	}
	if want := "fc_" + prefix + "_2"; second != want {
		t.Fatalf("second = %q, want %q (same prefix, next counter)", second, want)
	}
	if ids.New("msg_") == first {
		t.Fatal("third ID equals the first")
	}
}
