package transcode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func decodeJSON(t *testing.T, data string, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(data), dst); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func asUnsupportedFeatureError(t *testing.T, err error) *UnsupportedFeatureError {
	t.Helper()
	var target *UnsupportedFeatureError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *UnsupportedFeatureError: %v", err, err)
	}
	return target
}

func TestResponsesInputString(t *testing.T) {
	var input ResponsesInput
	decodeJSON(t, `"Hello"`, &input)
	if err := input.Validate(); err != nil {
		t.Fatal(err)
	}
	if input.Text == nil || *input.Text != "Hello" || input.Items != nil {
		t.Fatalf("input = %+v", input)
	}

	// Round-trip.
	encoded := mustJSON(t, input)
	var again ResponsesInput
	if err := json.Unmarshal(encoded, &again); err != nil {
		t.Fatal(err)
	}
	if again.Text == nil || *again.Text != "Hello" {
		t.Fatalf("round trip = %+v", again)
	}
}

func TestResponsesInputItems(t *testing.T) {
	data := `[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"input_text","text":"hello"}]},
		{"type":"function_call","call_id":"call_1","name":"f","arguments":"{\"x\":1}"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"},
		{"type":"reasoning","id":"rs_1","summary":[]},
		{"type":"item_reference","id":"item_1"}
	]`
	var input ResponsesInput
	decodeJSON(t, data, &input)
	if err := input.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(input.Items) != 6 {
		t.Fatalf("items = %d, want 6", len(input.Items))
	}

	var easy *ResponsesEasyInputMessage
	switch item := input.Items[0].(type) {
	case *ResponsesEasyInputMessage:
		easy = item
	default:
		t.Fatalf("item 0 type = %T", item)
	}
	if easy.Role != ResponsesInputRoleUser || easy.Content.Text == nil {
		t.Fatalf("easy = %+v", easy)
	}

	var fn *ResponsesFunctionCallInput
	switch item := input.Items[2].(type) {
	case *ResponsesFunctionCallInput:
		fn = item
	default:
		t.Fatalf("item 2 type = %T", item)
	}
	if fn.CallID != "call_1" || fn.Name != "f" || fn.Arguments != `{"x":1}` {
		t.Fatalf("fn = %+v", fn)
	}

	var reason *ResponsesReasoningInput
	switch item := input.Items[4].(type) {
	case *ResponsesReasoningInput:
		reason = item
	default:
		t.Fatalf("item 4 type = %T", item)
	}
	if reason.Summary == nil {
		t.Fatal("reasoning summary must be non-nil")
	}
}

func TestResponsesInputUnsupportedItemType(t *testing.T) {
	var input ResponsesInput
	err := json.Unmarshal([]byte(`[{"type":"web_search_call"}]`), &input)
	if err == nil {
		t.Fatal("expected error for web_search_call")
	}
	ue := asUnsupportedFeatureError(t, err)
	if ue.Feature != "web_search_call" {
		t.Fatalf("feature = %q", ue.Feature)
	}
}

func TestResponsesInputInvalid(t *testing.T) {
	tests := []string{
		`123`,
		`null`,
		`[null]`,
		`[{"role":"user"}]`, // no content
		`[{"type":"function_call","call_id":"c"}]`,                                      // no name/arguments
		`[{"type":"function_call","call_id":"c","name":"f","arguments":"nope"}]`,        // bad arguments JSON
		`[{"type":"function_call_output","call_id":"c"}]`,                               // no output
		`[{"type":"reasoning","id":"r","summary":null}]`,                                // nil summary
		`[{"type":"message","id":"m","role":"user","status":"completed","content":[]}]`, // easy vs previous
	}
	for _, data := range tests {
		var input ResponsesInput
		if err := json.Unmarshal([]byte(data), &input); err == nil {
			if err := input.Validate(); err == nil {
				t.Errorf("accepted invalid input %s", data)
			}
		}
	}
}

func TestResponsesInputContentPartUnsupported(t *testing.T) {
	var parts ResponsesInputContentParts
	err := json.Unmarshal([]byte(`[{"type":"input_audio","audio":"x"}]`), &parts)
	if err == nil {
		t.Fatal("expected error for input_audio")
	}
	ue := asUnsupportedFeatureError(t, err)
	if ue.Feature != "input_audio" {
		t.Fatalf("feature = %q", ue.Feature)
	}
}

func TestResponsesInputImageValidation(t *testing.T) {
	var part ResponsesInputImage
	part = ResponsesInputImage{Type: "input_image", Detail: "high", ImageURL: "https://x/y.png"}
	if err := part.Validate(); err != nil {
		t.Fatal(err)
	}
	part = ResponsesInputImage{Type: "input_image", Detail: "high", FileID: "file_1"}
	if err := part.Validate(); err != nil {
		t.Fatal(err)
	}
	part = ResponsesInputImage{Type: "input_image", Detail: "high"} // neither
	if err := part.Validate(); err == nil {
		t.Fatal("expected error for missing image_url and file_id")
	}
	part = ResponsesInputImage{Type: "input_image", Detail: "weird", ImageURL: "x"}
	if err := part.Validate(); err == nil {
		t.Fatal("expected error for invalid detail")
	}
}

func TestResponsesInputFileValidation(t *testing.T) {
	part := ResponsesInputFile{Type: "input_file", FileID: "file_1"}
	if err := part.Validate(); err != nil {
		t.Fatal(err)
	}
	part = ResponsesInputFile{Type: "input_file"}
	if err := part.Validate(); err == nil {
		t.Fatal("expected error for no file source")
	}
	part = ResponsesInputFile{Type: "input_file", FileID: "a", FileURL: "b"}
	if err := part.Validate(); err == nil {
		t.Fatal("expected error for two file sources")
	}
}

func TestResponsesInputRoundTripStable(t *testing.T) {
	data := []byte(`[
		{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","detail":"auto","image_url":"https://x/y.png"}]},
		{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"{\"ok\":true}"}
	]`)
	var first ResponsesInput
	if err := json.Unmarshal(data, &first); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var second ResponsesInput
	if err := json.Unmarshal(encoded, &second); err != nil {
		t.Fatalf("cannot decode own encoding: %v\n%s", err, encoded)
	}
	if err := second.Validate(); err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(mustJSON(t, second)) {
		t.Fatalf("unstable round trip:\n%s\n%s", encoded, mustJSON(t, second))
	}
}

func TestResponsesInputEasyMessagePhaseValidation(t *testing.T) {
	bad := `{"role":"assistant","content":"x","phase":"wrong"}`
	item := &ResponsesEasyInputMessage{}
	if err := json.Unmarshal([]byte(bad), item); err == nil {
		if err := item.Validate(); err == nil {
			t.Fatal("expected error for invalid phase")
		}
	}
}

func TestResponsesFunctionOutputUnion(t *testing.T) {
	var output ResponsesFunctionOutput
	decodeJSON(t, `"plain text"`, &output)
	if output.Text == nil || *output.Text != "plain text" {
		t.Fatalf("output = %+v", output)
	}
	decodeJSON(t, `[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]`, &output)
	if output.Parts == nil || len(output.Parts) != 2 {
		t.Fatalf("output = %+v", output)
	}
	if err := output.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesInputInstructionsUnion(t *testing.T) {
	// The envelope uses *ResponsesInput for instructions; both arms must
	// decode through strict decoding.
	data := `{"instructions":"system prompt","input":"hi","model":"m"}`
	var envelope responsesRequestEnvelope
	if err := strictDecode([]byte(data), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Instructions == nil || envelope.Instructions.Text == nil {
		t.Fatalf("instructions = %+v", envelope.Instructions)
	}
}

func TestLossPolicyStrict(t *testing.T) {
	policy := StrictLossPolicy()
	var report ConversionReport
	if err := report.Lose(policy, FeatureTopK, "top_k", "no portable equivalent"); err == nil {
		t.Fatal("strict policy must reject losses")
	}
	if len(report.Losses) != 0 {
		t.Fatal("rejected loss must not be recorded")
	}

	policy = LossPolicy{Allowed: map[Feature]struct{}{FeatureTopK: {}}}
	var allowed ConversionReport
	if err := allowed.Lose(policy, FeatureTopK, "top_k", "dropped"); err != nil {
		t.Fatal(err)
	}
	if len(allowed.Losses) != 1 || allowed.Losses[0].Feature != FeatureTopK {
		t.Fatalf("losses = %+v", allowed.Losses)
	}
	if err := allowed.Lose(policy, FeatureStopSequences, "stop", "no"); err == nil {
		t.Fatal("policy must still reject unlisted features")
	}
}

func TestStrictDecodeTrailingValue(t *testing.T) {
	var value string
	if err := strictDecode([]byte(`"a" "b"`), &value); err == nil {
		t.Fatal("expected trailing-value rejection")
	}
	if err := strictDecode([]byte(`{"a":1,"b":2}`), &struct {
		A int `json:"a"`
	}{}); err == nil {
		t.Fatal("expected unknown-field rejection")
	}
}

func TestDecodeJSONObject(t *testing.T) {
	value, err := decodeJSONObject(`{"x":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(value) != 1 {
		t.Fatalf("value = %v", value)
	}
	if _, err := decodeJSONObject(`[1,2]`); err == nil {
		t.Fatal("expected non-object rejection")
	}
	if _, err := decodeJSONObject(`"str"`); err == nil {
		t.Fatal("expected non-object rejection")
	}
	if _, err := decodeJSONObject(``); err == nil {
		t.Fatal("expected empty rejection")
	}
}

func TestStringInputDecodeEndToEnd(t *testing.T) {
	// The acceptance criterion: a Responses request with "input":"Hello"
	// decodes and round-trips.
	result, echo, err := DecodeResponsesRequest(
		[]byte(`{"model":"gpt-4.1","input":"Hello"}`),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if echo == nil {
		t.Fatal("echo is nil")
	}
	if len(result.Request.Turns) != 1 {
		t.Fatalf("turns = %d", len(result.Request.Turns))
	}
	if result.Request.Turns[0].Role != CanonicalUser {
		t.Fatalf("role = %q", result.Request.Turns[0].Role)
	}
	text, ok := result.Request.Turns[0].Parts[0].(CanonicalText)
	if !ok || text.Text != "Hello" {
		t.Fatalf("part = %#v", result.Request.Turns[0].Parts[0])
	}

	rendered, _, err := RenderResponsesRequest(result.Request, &ExchangeContext{
		UpstreamModel: "gpt-4.1",
		LossPolicy:    StrictLossPolicy(),
		IDs:           NewExchangeIDs(),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var probe struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(rendered, &probe); err != nil {
		t.Fatal(err)
	}
	// The string input is rendered as a user easy message carrying the same
	// text; decode the rendered input and assert semantic equality.
	var input ResponsesInput
	if err := json.Unmarshal(probe.Input, &input); err != nil {
		t.Fatal(err)
	}
	if input.Text != nil && *input.Text == "Hello" {
		return
	}
	if len(input.Items) != 1 {
		t.Fatalf("rendered input = %s, want string \"Hello\" or one user message", probe.Input)
	}
	easy, ok := input.Items[0].(*ResponsesEasyInputMessage)
	if !ok || easy.Role != ResponsesInputRoleUser {
		t.Fatalf("rendered input item = %T %+v", input.Items[0], input.Items[0])
	}
	if easy.Content.Text != nil && *easy.Content.Text == "Hello" {
		return
	}
	if len(easy.Content.Parts) != 1 {
		t.Fatalf("rendered content = %+v", easy.Content)
	}
	textPart, ok := easy.Content.Parts[0].(*ResponsesInputText)
	if !ok || textPart.Text != "Hello" {
		t.Fatalf("rendered text part = %T %+v", easy.Content.Parts[0], easy.Content.Parts[0])
	}
}

func TestUnsupportedRequestFieldRejected(t *testing.T) {
	_, _, err := DecodeResponsesRequest(
		[]byte(`{"model":"m","input":"hi","include":["reasoning.encrypted_content"]}`),
		StrictLossPolicy(),
	)
	if err == nil {
		t.Fatal("expected include rejection")
	}
	ue := asUnsupportedFeatureError(t, err)
	if ue.Path != "include" {
		t.Fatalf("path = %q", ue.Path)
	}
	if !strings.Contains(err.Error(), "include") {
		t.Fatalf("error = %v", err)
	}
}

func TestResponsesInputImageDetailOptional(t *testing.T) {
	// detail is optional on the wire (the official SDKs omit it; the API
	// defaults to auto). An input_image without detail must decode.
	var content ResponsesInputMessageContent
	if err := json.Unmarshal([]byte(
		`[{"type":"input_image","image_url":"https://example.com/a.png"}]`,
	), &content); err != nil {
		t.Fatalf("decode without detail: %v", err)
	}
	if err := content.Validate(); err != nil {
		t.Fatalf("validate without detail: %v", err)
	}
}
