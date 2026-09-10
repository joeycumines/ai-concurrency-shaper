package transcode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openaichat"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
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

// asWireUnsupportedTypeError asserts the error chain carries the wire
// layer's typed unsupported-type report.
func asWireUnsupportedTypeError(t *testing.T, err error) *wire.UnsupportedTypeError {
	t.Helper()
	var target *wire.UnsupportedTypeError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *wire.UnsupportedTypeError: %v", err, err)
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
	// The wire-layer union dispatch reports the unsupported arm as the
	// typed wire.UnsupportedTypeError; the transcode request/response
	// boundaries translate it into UnsupportedFeatureError (covered by the
	// classification tests). At the direct decode level the wire type is the
	// contract.
	var input ResponsesInput
	err := json.Unmarshal([]byte(`[{"type":"web_search_call"}]`), &input)
	if err == nil {
		t.Fatal("expected error for web_search_call")
	}
	ue := asWireUnsupportedTypeError(t, err)
	if ue.Type != "web_search_call" {
		t.Fatalf("type = %q", ue.Type)
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
	// The wire-layer union dispatch reports the unsupported arm as the
	// typed wire.UnsupportedTypeError; the transcode request/response
	// boundaries translate it into UnsupportedFeatureError (covered by the
	// classification tests). At the direct decode level the wire type is the
	// contract.
	var parts ResponsesInputContentParts
	err := json.Unmarshal([]byte(`[{"type":"input_audio","audio":"x"}]`), &parts)
	if err == nil {
		t.Fatal("expected error for input_audio")
	}
	ue := asWireUnsupportedTypeError(t, err)
	if ue.Type != "input_audio" {
		t.Fatalf("type = %q", ue.Type)
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

func TestResponsesInputInstructionsString(t *testing.T) {
	// The create-request instructions is a plain string per the pinned
	// ResponseNewParams shape.
	data := `{"instructions":"system prompt","input":"hi","model":"m"}`
	var envelope openairesponses.Request
	if err := strictDecode([]byte(data), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Instructions == nil || *envelope.Instructions != "system prompt" {
		t.Fatalf("instructions = %+v", envelope.Instructions)
	}

	// An items array is not a valid create-request instructions shape.
	bad := `{"instructions":[{"type":"message","role":"system","content":[{"type":"input_text","text":"x"}]}],"input":"hi","model":"m"}`
	if err := strictDecode([]byte(bad), &envelope); err == nil {
		t.Fatal("array instructions accepted on the create request")
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
	dupVal, err := decodeJSONObject(`{"max_output_tokens": 100, "max_output_tokens": 200}`)
	if err != nil {
		t.Fatalf("expected duplicate keys accepted in decodeJSONObject: %v", err)
	}
	if len(dupVal) != 1 || string(dupVal["max_output_tokens"]) != "200" {
		t.Fatalf("unexpected dupVal: %v", dupVal)
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
	// background is a recognized-but-unsupported envelope control: rejected
	// as a typed unsupported feature, never silently dropped.
	_, _, err := DecodeResponsesRequest(
		[]byte(`{"model":"m","input":"hi","background":true}`),
		StrictLossPolicy(),
	)
	if err == nil {
		t.Fatal("expected background rejection")
	}
	ue := asUnsupportedFeatureError(t, err)
	if ue.Path != "background" {
		t.Fatalf("path = %q", ue.Path)
	}
	if !strings.Contains(err.Error(), "background") {
		t.Fatalf("error = %v", err)
	}
}

func TestIncludeAcceptedWithNote(t *testing.T) {
	// include is a best-effort response-format preference: accepted, noted,
	// never forwarded. The decoded request carries no rejection.
	result, _, err := DecodeResponsesRequest(
		[]byte(`{"model":"m","input":"hi","include":["reasoning.encrypted_content"]}`),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureResponsesControls &&
			strings.Contains(loss.Detail, "include") {
			found = true
		}
	}
	if !found {
		t.Fatalf("include note missing: %+v", result.Report.Losses)
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

// TestResponsesInputItemIdentityNoted pins the observable drop of input
// item identity: every ID-bearing input
// item type (easy message, previous output message, function call, function
// call output) accepts an id on the wire, and the decode records exactly one
// deduped previous_response_id note per exchange — never a silent drop and
// never a policy gate. A request whose items carry no ids records no note.
func TestResponsesInputItemIdentityNoted(t *testing.T) {
	countNotes := func(report ConversionReport) int {
		count := 0
		for _, loss := range report.Losses {
			if loss.Feature == FeaturePreviousResponseID && loss.Path == "input[].id" {
				count++
			}
		}
		return count
	}

	t.Run("id on every item type, one deduped note", func(t *testing.T) {
		result, _, err := DecodeResponsesRequest([]byte(`{
			"model": "m",
			"input": [
				{"id": "msg_1", "role": "user", "content": "hi"},
				{"id": "msg_2", "type": "message", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": "hello", "annotations": []}]},
				{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "f", "arguments": "{\"x\":1}"},
				{"id": "fo_1", "type": "function_call_output", "call_id": "call_1", "output": "ok"}
			]
		}`), StrictLossPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if got := countNotes(result.Report); got != 1 {
			t.Fatalf("input[].id notes = %d, want exactly 1 (deduped)", got)
		}
		// The ids never reach the canonical turns: every renderer rebuilds
		// items from turns.
		for _, turn := range result.Request.Turns {
			for _, part := range turn.Parts {
				switch value := part.(type) {
				case CanonicalFunctionCall:
					if value.CallID != "call_1" {
						t.Fatalf("call identity = %q", value.CallID)
					}
				}
			}
		}
	})

	t.Run("single id, one note", func(t *testing.T) {
		result, _, err := DecodeResponsesRequest([]byte(`{
			"model": "m",
			"input": [{"id": "msg_1", "role": "user", "content": "hi"}]
		}`), StrictLossPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if got := countNotes(result.Report); got != 1 {
			t.Fatalf("input[].id notes = %d, want 1", got)
		}
	})

	t.Run("no ids, no note", func(t *testing.T) {
		result, _, err := DecodeResponsesRequest([]byte(`{
			"model": "m",
			"input": [{"role": "user", "content": "hi"}]
		}`), StrictLossPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if got := countNotes(result.Report); got != 0 {
			t.Fatalf("input[].id notes = %d, want 0", got)
		}
	})
}

func TestResponsesInputFunctionCallDuplicateArgumentsKeys(t *testing.T) {
	// Exact scenario observed from Codex TUI (glm-5.3-flash):
	// replaying conversation history where an input item contains a function_call
	// with duplicate JSON keys in the arguments string (e.g. "max_output_tokens").
	rawBody := []byte(`{
		"model": "glm-5.3-flash",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": "Perform task"
			},
			{
				"type": "function_call",
				"id": "fc_01a07f8a",
				"call_id": "call_01a07f8a",
				"name": "generate_text",
				"arguments": "{\"max_output_tokens\": 100, \"prompt\": \"hello\", \"max_output_tokens\": 200}"
			}
		]
	}`)

	result, _, err := DecodeResponsesRequest(rawBody, StrictLossPolicy())
	if err != nil {
		t.Fatalf("DecodeResponsesRequest failed on duplicate keys in function call arguments: %v", err)
	}

	// Verify CanonicalFunctionCall arguments were normalized
	var call *CanonicalFunctionCall
	for _, turn := range result.Request.Turns {
		for _, part := range turn.Parts {
			if fc, ok := part.(CanonicalFunctionCall); ok {
				call = &fc
				break
			}
		}
	}
	if call == nil {
		t.Fatal("expected CanonicalFunctionCall in turns")
	}
	if call.CallID != "call_01a07f8a" || call.Name != "generate_text" {
		t.Fatalf("unexpected call identity: %+v", call)
	}
	// Arguments must be valid JSON with the last key value (200)
	var parsedArgs map[string]any
	if err := json.Unmarshal(call.Arguments, &parsedArgs); err != nil {
		t.Fatalf("unmarshal normalized arguments: %v", err)
	}
	if parsedArgs["max_output_tokens"] != float64(200) {
		t.Fatalf("expected max_output_tokens 200, got %v", parsedArgs["max_output_tokens"])
	}
	if parsedArgs["prompt"] != "hello" {
		t.Fatalf("expected prompt hello, got %v", parsedArgs["prompt"])
	}

	// Verify RenderChatRequest renders valid upstream chat tool_calls without error
	chatBytes, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
	if err != nil {
		t.Fatalf("RenderChatRequest failed: %v", err)
	}
	var chatReq openaichat.Request
	if err := wire.Decode(chatBytes, &chatReq); err != nil {
		t.Fatalf("rendered Chat request failed wire.Decode: %v", err)
	}

	// Proves that duplicate keys on the envelope itself are STILL strictly rejected
	envelopeDup := []byte(`{
		"model": "glm-5.3-flash",
		"model": "other-model",
		"input": "hi"
	}`)
	if _, _, err := DecodeResponsesRequest(envelopeDup, StrictLossPolicy()); err == nil {
		t.Fatal("expected envelope-level duplicate key rejection")
	} else {
		var decodeErr *wire.DecodeError
		if !errors.As(err, &decodeErr) || decodeErr.Kind != wire.DecodeDuplicateKey {
			t.Fatalf("expected DecodeDuplicateKey error, got %v", err)
		}
	}
}
