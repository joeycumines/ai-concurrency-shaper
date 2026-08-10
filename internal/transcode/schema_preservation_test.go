package transcode

// J2 regression tests (review-k finding 2, blocker): schema-bearing JSON
// (Anthropic input_schema, Chat function parameters, Chat json_schema
// response_format, Responses tool parameters and text.format schema) crosses
// every wire and canonical representation as validated json.RawMessage, so
// numbers above 2^53, decimals, and exponent forms survive byte-exact —
// never decoded and remarshaled through a map[string]any.

import (
	"bytes"
	"encoding/json"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// schemaNumbers is a schema whose numeric literals exercise every corruption
// class: integers above 2^53, negative integers below -2^53, a decimal, and
// an exponent form. Written compactly so byte-exact substring assertions are
// unambiguous.
const schemaNumbers = `{"type":"object","properties":{"location":{"type":"string"}},"maximum":9007199254740993,"minimum":-9007199254740995,"enum":[9007199254740993,1.5e100,0.0000001,-9007199254740995]}`

// assertSchemaBytesExact asserts the emitted wire contains the schema
// literals verbatim and never the float64-corrupted forms.
func assertSchemaBytesExact(t *testing.T, emitted []byte) {
	t.Helper()
	for _, literal := range []string{
		`"maximum":9007199254740993`,
		`"minimum":-9007199254740995`,
		`"enum":[9007199254740993,1.5e100,0.0000001,-9007199254740995]`,
	} {
		if !bytes.Contains(emitted, []byte(literal)) {
			t.Errorf("emitted body missing %s verbatim:\n%s", literal, emitted)
		}
	}
	for _, corrupted := range []string{
		"9007199254740992",  // 9007199254740993 as float64
		"-9007199254740996", // -9007199254740995 as float64
		"1.5e+100",          // 1.5e100 normalized through float64
		"1e-7",              // 0.0000001 normalized through float64
	} {
		if bytes.Contains(emitted, []byte(corrupted)) {
			t.Errorf("emitted body contains float64-corrupted literal %s:\n%s", corrupted, emitted)
		}
	}
}

// TestSchemaBigNumbersMessagesToChat proves input_schema numbers survive
// byte-exact through Messages→Chat (request direction).
func TestSchemaBigNumbersMessagesToChat(t *testing.T) {
	body := []byte(`{
		"model":"claude-x",
		"max_tokens":100,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"get_weather",
			"description":"d",
			"input_schema":` + schemaNumbers + `
		}]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Request.Tools) != 1 {
		t.Fatalf("tools = %d", len(result.Request.Tools))
	}
	if string(result.Request.Tools[0].JSONSchema) != schemaNumbers {
		t.Fatalf("canonical schema = %s, want the raw bytes", result.Request.Tools[0].JSONSchema)
	}
	rendered, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaBytesExact(t, rendered)

	// The rendered wire type carries the same raw bytes.
	var chat ChatRequest
	if err := strictDecode(rendered, &chat); err != nil {
		t.Fatalf("rendered chat: %v\n%s", err, rendered)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function == nil {
		t.Fatalf("tools = %+v", chat.Tools)
	}
	if string(chat.Tools[0].Function.Parameters) != schemaNumbers {
		t.Fatalf("rendered parameters = %s, want the raw bytes", chat.Tools[0].Function.Parameters)
	}
}

// TestSchemaBigNumbersMessagesToResponses proves input_schema numbers survive
// byte-exact through Messages→Responses (request direction).
func TestSchemaBigNumbersMessagesToResponses(t *testing.T) {
	body := []byte(`{
		"model":"claude-x",
		"max_tokens":100,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"get_weather",
			"description":"d",
			"input_schema":` + schemaNumbers + `
		}]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	// Under strict policy a Messages tool (no strictness semantic) cannot be
	// rendered as a Responses function tool: the conversion is rejected
	// client-dialect before any upstream request (review-z commit 1).
	if _, _, err := RenderResponsesRequest(
		result.Request,
		testExchangeContext(),
	); err == nil {
		t.Fatal("expected strict-policy rejection for missing tool strictness")
	}
	// Under the tool_schema_strictness permission the render emits explicit
	// strict:false — the non-tightening value — never an omitted strict.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolSchemaStrictness: {},
	}}
	rendered, _, err := RenderResponsesRequest(result.Request, context)
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaBytesExact(t, rendered)
	var envelope openairesponses.Request
	if err := strictDecode(rendered, &envelope); err != nil {
		t.Fatalf("rendered responses: %v\n%s", err, rendered)
	}
	if len(envelope.Tools) != 1 {
		t.Fatalf("tools = %d", len(envelope.Tools))
	}
	if !envelope.Tools[0].Strict.Present || envelope.Tools[0].Strict.Null ||
		envelope.Tools[0].Strict.Value {
		t.Fatalf("rendered strict = %+v, want explicit false", envelope.Tools[0].Strict)
	}
	if string(envelope.Tools[0].Parameters) != schemaNumbers {
		t.Fatalf("rendered parameters = %s, want the raw bytes", envelope.Tools[0].Parameters)
	}
}

// TestSchemaBigNumbersResponsesToChat proves tool parameters numbers survive
// byte-exact through Responses→Chat (request direction).
func TestSchemaBigNumbersResponsesToChat(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tools":[{
			"type":"function",
			"name":"get_weather",
			"description":"d",
			"strict":true,
			"parameters":` + schemaNumbers + `
		}]
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Request.Tools) != 1 {
		t.Fatalf("tools = %d", len(result.Request.Tools))
	}
	if string(result.Request.Tools[0].JSONSchema) != schemaNumbers {
		t.Fatalf("canonical schema = %s, want the raw bytes", result.Request.Tools[0].JSONSchema)
	}
	rendered, _, err := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaBytesExact(t, rendered)
}

// TestSchemaBigNumbersResponsesToResponses proves tool parameters numbers
// survive byte-exact through the Responses renderer (request direction).
func TestSchemaBigNumbersResponsesToResponses(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tools":[{
			"type":"function",
			"name":"get_weather",
			"description":"d",
			"strict":true,
			"parameters":` + schemaNumbers + `
		}]
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	rendered, _, err := RenderResponsesRequest(result.Request, testExchangeContext())
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaBytesExact(t, rendered)
}

// TestSchemaBigNumbersStructuredOutput proves text.format.json_schema numbers
// survive byte-exact through Responses→Chat response_format and the
// Responses text.format echo.
func TestSchemaBigNumbersStructuredOutput(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"text":{"format":{"type":"json_schema","name":"s","schema":` + schemaNumbers + `}}
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if result.Request.StructuredOutput == nil {
		t.Fatal("no structured output decoded")
	}
	if string(result.Request.StructuredOutput.Schema) != schemaNumbers {
		t.Fatalf("canonical schema = %s, want the raw bytes", result.Request.StructuredOutput.Schema)
	}

	// Responses target: the text.format schema is echoed byte-exact.
	rendered, _, err := RenderResponsesRequest(result.Request, testExchangeContext())
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaBytesExact(t, rendered)
	var envelope openairesponses.Request
	if err := strictDecode(rendered, &envelope); err != nil {
		t.Fatalf("rendered responses: %v\n%s", err, rendered)
	}
	if envelope.Text == nil || envelope.Text.Format == nil ||
		string(envelope.Text.Format.Schema) != schemaNumbers {
		t.Fatalf("rendered format = %+v, want the raw schema", envelope.Text)
	}

	// Chat target: response_format.json_schema.schema is byte-exact.
	chatBody, _, err := RenderChatRequest(
		result.Request,
		testExchangeContext(),
		ChatCapabilities{StructuredOutputs: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaBytesExact(t, chatBody)
	var chat ChatRequest
	if err := strictDecode(chatBody, &chat); err != nil {
		t.Fatalf("rendered chat: %v\n%s", err, chatBody)
	}
	if chat.ResponseFormat == nil || chat.ResponseFormat.JSONSchema == nil ||
		string(chat.ResponseFormat.JSONSchema.Schema) != schemaNumbers {
		t.Fatalf("rendered response format = %+v, want the raw schema", chat.ResponseFormat)
	}
}

// TestSchemaMalformedRejected proves a schema that is not exactly one JSON
// object is rejected at the canonical-IR boundary with a conversion error,
// for every wire source and schema field.
func TestSchemaMalformedRejected(t *testing.T) {
	nonObjects := []string{
		`[1,2]`,
		`"str"`,
		`42`,
		`null`,
	}

	for _, value := range nonObjects {
		t.Run("messages-input-schema-"+value, func(t *testing.T) {
			body := []byte(`{
				"model":"m",
				"max_tokens":100,
				"messages":[{"role":"user","content":"hi"}],
				"tools":[{"name":"f","input_schema":` + value + `}]
			}`)
			if _, err := DecodeMessagesRequest(body, StrictLossPolicy()); err == nil {
				t.Fatalf("input_schema %s accepted", value)
			}
		})

		t.Run("responses-parameters-"+value, func(t *testing.T) {
			body := []byte(`{
				"model":"m",
				"input":"hi",
				"tools":[{"type":"function","name":"f","parameters":` + value + `}]
			}`)
			if _, _, err := DecodeResponsesRequest(body, StrictLossPolicy()); err == nil {
				t.Fatalf("parameters %s accepted", value)
			}
		})

		t.Run("responses-format-schema-"+value, func(t *testing.T) {
			body := []byte(`{
				"model":"m",
				"input":"hi",
				"text":{"format":{"type":"json_schema","name":"s","schema":` + value + `}}
			}`)
			if _, _, err := DecodeResponsesRequest(body, StrictLossPolicy()); err == nil {
				t.Fatalf("format schema %s accepted", value)
			}
		})
	}

	// The shared validator itself rejects trailing garbage and empty input.
	if _, err := decodeJSONObject(`{"a":1} {"b":2}`); err == nil {
		t.Fatal("trailing garbage accepted")
	}
	if _, err := decodeJSONObject(``); err == nil {
		t.Fatal("empty accepted")
	}
}

// TestSchemaOfficialFixturesStillDecode proves the committed official-shaped
// fixtures (which carry schemas) decode and render unchanged after the
// RawMessage conversion.
func TestSchemaOfficialFixturesStillDecode(t *testing.T) {
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureTopK: {}}}
	result, err := DecodeMessagesRequest(testcorpus.AnthropicMessagesRequestJSON(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Request.Tools) != 1 {
		t.Fatalf("fixture tools = %d, want 1", len(result.Request.Tools))
	}
	if len(result.Request.Tools[0].JSONSchema) == 0 {
		t.Fatal("fixture tool lost its schema")
	}
	// The Messages source tool carries no strictness semantic: approve the
	// tool_schema_strictness loss so rendering proceeds with explicit
	// strict:false (the fixture's schema must still survive byte-exact).
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolSchemaStrictness: {},
	}}
	if _, _, err := RenderResponsesRequest(result.Request, context); err != nil {
		t.Fatal(err)
	}

	result2, _, err := DecodeResponsesRequest(testcorpus.ResponsesRequestJSON(), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(result2.Request.Tools) != 1 || len(result2.Request.Tools[0].JSONSchema) == 0 {
		t.Fatalf("responses fixture tools = %+v", result2.Request.Tools)
	}
	if _, _, err := RenderChatRequest(result2.Request, testExchangeContext(), ChatCapabilities{ParallelToolCalls: true}); err != nil {
		t.Fatal(err)
	}
}

// TestSchemaWireTypesAreRawMessage is a compile-time guard: the schema wire
// fields must stay json.RawMessage so number corruption cannot re-enter
// through a map round trip.
func TestSchemaWireTypesAreRawMessage(t *testing.T) {
	var tool ChatToolFunction
	if _, ok := any(tool.Parameters).(json.RawMessage); !ok {
		t.Fatalf("ChatToolFunction.Parameters = %T, want json.RawMessage", tool.Parameters)
	}
	var format ChatJSONSchemaFormat
	if _, ok := any(format.Schema).(json.RawMessage); !ok {
		t.Fatalf("ChatJSONSchemaFormat.Schema = %T, want json.RawMessage", format.Schema)
	}
	var anthropicTool AnthropicTool
	if _, ok := any(anthropicTool.InputSchema).(json.RawMessage); !ok {
		t.Fatalf("AnthropicTool.InputSchema = %T, want json.RawMessage", anthropicTool.InputSchema)
	}
	var responsesTool ResponsesTool
	if _, ok := any(responsesTool.Parameters).(json.RawMessage); !ok {
		t.Fatalf("ResponsesTool.Parameters = %T, want json.RawMessage", responsesTool.Parameters)
	}
	var envelopeFormat ResponsesEnvelopeTextFormat
	if _, ok := any(envelopeFormat.Schema).(json.RawMessage); !ok {
		t.Fatalf("ResponsesEnvelopeTextFormat.Schema = %T, want json.RawMessage", envelopeFormat.Schema)
	}
}
