package transcode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestDecodeResponsesRequestNullEncryptedContent pins the CODEX-NULL fix
// (operator-observed 2026-09-08): codex-tui sends reasoning input items with
// encrypted_content and reasoning_text.signature explicitly null (JSON null,
// not absent). The strict client decode rejected the null on the modeled
// string fields and 400'd the exchange. Null on a provider-opaque optional
// pass-through field means "absent": the decode must accept it and the
// pass-through must omit the key.
func TestDecodeResponsesRequestNullEncryptedContent(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"t","signature":null}],"encrypted_content":null},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"stream":false}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("explicit null on optional pass-through fields must decode: %v", err)
	}
	// The user turn must survive behind the reasoning item.
	userTurns := 0
	for _, turn := range result.Request.Turns {
		if turn.Role == CanonicalUser {
			userTurns++
		}
	}
	if userTurns == 0 {
		t.Fatal("user turn missing after the reasoning item")
	}
	// The reasoning artifact re-marshal must omit the null keys entirely.
	found := false
	for _, raw := range result.Request.Artifacts.ResponsesReasoningItems {
		if strings.Contains(string(raw), `"encrypted_content"`) || strings.Contains(string(raw), `"signature"`) {
			t.Fatalf("null optional keys must be omitted from the pass-through artifact: %s", raw)
		}
		if strings.Contains(string(raw), `"type":"reasoning"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("reasoning artifact item missing")
	}
}

// TestResponsesStructuredOutputToChatDefaultCapabilities pins the fix for
// the observed codex-tui 400 (operator-observed 2026-09-11): a Responses
// request carrying text.format with type json_schema was rejected by
// RenderChatRequest because ChatCapabilities.StructuredOutputs defaulted to
// false and FeatureStructuredOutput was not in the default loss policy. The
// fix enables StructuredOutputs by default so json_schema maps losslessly to
// Chat response_format.
func TestResponsesStructuredOutputToChatDefaultCapabilities(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"text":{"format":{"type":"json_schema","name":"test_schema","schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]},"strict":true}},"stream":false}`)

	// Decode under strict policy — json_schema decode is lossless, no
	// policy decision needed at this stage.
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode responses request with text.format json_schema: %v", err)
	}
	if result.Request.StructuredOutput == nil {
		t.Fatal("expected StructuredOutput to be set from text.format json_schema")
	}
	if result.Request.StructuredOutput.Name != "test_schema" {
		t.Fatalf("structured output name = %q, want %q", result.Request.StructuredOutput.Name, "test_schema")
	}

	// Render to Chat with default capabilities (StructuredOutputs: true).
	context := testExchangeContext()
	defaultCaps := ChatCapabilities{
		ParallelToolCalls:         true,
		ProviderReasoningThinking: true,
		StructuredOutputs:         true,
	}
	rendered, report, err := RenderChatRequest(result.Request, context, defaultCaps)
	if err != nil {
		t.Fatalf("render chat request with default capabilities must not reject structured_output: %v", err)
	}

	// Verify no structured_output loss was recorded.
	for _, loss := range report.Losses {
		if loss.Feature == FeatureStructuredOutput {
			t.Fatalf("unexpected structured_output loss under default capabilities: %+v", loss)
		}
	}

	// Verify the rendered body contains response_format json_schema.
	var parsed struct {
		ResponseFormat *struct {
			Type       string `json:"type"`
			JSONSchema *struct {
				Name   string          `json:"name"`
				Schema json.RawMessage `json:"schema"`
				Strict *bool           `json:"strict"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(rendered, &parsed); err != nil {
		t.Fatalf("unmarshal rendered chat request: %v", err)
	}
	if parsed.ResponseFormat == nil {
		t.Fatal("rendered chat request missing response_format")
	}
	if parsed.ResponseFormat.Type != "json_schema" {
		t.Fatalf("response_format type = %q, want json_schema", parsed.ResponseFormat.Type)
	}
	if parsed.ResponseFormat.JSONSchema == nil {
		t.Fatal("response_format missing json_schema")
	}
	if parsed.ResponseFormat.JSONSchema.Name != "test_schema" {
		t.Fatalf("json_schema name = %q, want test_schema", parsed.ResponseFormat.JSONSchema.Name)
	}
	if parsed.ResponseFormat.JSONSchema.Strict == nil || !*parsed.ResponseFormat.JSONSchema.Strict {
		t.Fatal("json_schema strict should be true")
	}
	// Verify schema bytes are preserved byte-exact.
	var schemaObj map[string]json.RawMessage
	if err := json.Unmarshal(parsed.ResponseFormat.JSONSchema.Schema, &schemaObj); err != nil {
		t.Fatalf("schema is not a valid JSON object: %v", err)
	}
	if _, ok := schemaObj["properties"]; !ok {
		t.Fatal("schema missing properties key")
	}
}

// TestResponsesJSONObjectStructuredOutputIsLossGated proves that
// text.format.type json_object still requires the structured_output loss
// permission at decode time, even with StructuredOutputs capability enabled.
func TestResponsesJSONObjectStructuredOutputIsLossGated(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"text":{"format":{"type":"json_object"}},"stream":false}`)

	// Under strict policy, json_object must be rejected.
	_, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err == nil {
		t.Fatal("json_object under strict policy must be rejected")
	}
	var ufe *UnsupportedFeatureError
	if !errors.As(err, &ufe) || ufe.Feature != "structured_output" {
		t.Fatalf("expected UnsupportedFeatureError for structured_output, got: %v", err)
	}

	// With the loss allowed, it must succeed (the format is dropped
	// observably; the render path sees no StructuredOutput).
	allowed, err := ParseLossFeatures("structured_output")
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := DecodeResponsesRequest(body, LossPolicy{Allowed: allowed})
	if err != nil {
		t.Fatalf("json_object with structured_output loss allowed: %v", err)
	}
	if result.Request.StructuredOutput != nil {
		t.Fatal("json_object must not produce a CanonicalStructuredOutput")
	}
	found := false
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureStructuredOutput && loss.Kind == LossRecord {
			found = true
		}
	}
	if !found {
		t.Fatal("expected structured_output loss record for json_object")
	}
}

// TestDecodeMessagesServerToolDefinitionKeyed pins the live server-tool
// capture (2026-09-17, Claude Code 2.1.273 web-search session via
// messages->chat:
// tools:[{type:web_search_20250305, name:web_search, max_uses:8}]): the
// type-discriminated server definition must fail with the keyed
// anthropic_server_tools error under strict policy (never an unattributed
// unknown-field rejection), and drop observably under the approval.
func TestDecodeMessagesServerToolDefinitionKeyed(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":8,` +
		`"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}],` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	_, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err == nil {
		t.Fatal("expected keyed rejection under strict policy, got nil")
	}
	target := &UnsupportedFeatureError{}
	if !errors.As(err, &target) {
		t.Fatalf("err = %T: %v, want UnsupportedFeatureError carrying the key", err, err)
	}
	if target.Feature != string(FeatureAnthropicServerTools) {
		t.Fatalf("feature = %q, want %q", target.Feature, FeatureAnthropicServerTools)
	}
	permissive := LossPolicy{Allowed: map[Feature]struct{}{FeatureAnthropicServerTools: {}}}
	result, err := DecodeMessagesRequest(body, permissive)
	if err != nil {
		t.Fatalf("approved drop rejected: %v", err)
	}
	if len(result.Request.Tools) != 0 {
		t.Fatalf("server tool leaked %d tools upstream", len(result.Request.Tools))
	}
	found := false
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureAnthropicServerTools {
			found = true
		}
	}
	if !found {
		t.Fatalf("report = %+v, want the anthropic_server_tools loss", result.Report.Losses)
	}
}

// TestDecodeMessagesServerBlocksKeyed pins the per-block dispositions:
// server_tool_use drops under the key (never a synthesized function call),
// mcp_tool_use maps 1:1 onto a function call with no loss key.
func TestDecodeMessagesServerBlocksKeyed(t *testing.T) {
	server := []byte(`{"model":"m","max_tokens":8,` +
		`"messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"q":"x"}}]}]}`)
	_, err := DecodeMessagesRequest(server, StrictLossPolicy())
	target := &UnsupportedFeatureError{}
	if !errors.As(err, &target) {
		t.Fatalf("server_tool_use err = %T: %v, want keyed rejection", err, err)
	}
	if target.Feature != string(FeatureAnthropicServerTools) {
		t.Fatalf("feature = %q, want %q", target.Feature, FeatureAnthropicServerTools)
	}
	permissive := LossPolicy{Allowed: map[Feature]struct{}{FeatureAnthropicServerTools: {}}}
	res, err := DecodeMessagesRequest(server, permissive)
	if err != nil {
		t.Fatalf("approved server drop rejected: %v", err)
	}
	for _, turn := range res.Request.Turns {
		for _, part := range turn.Parts {
			if _, ok := part.(CanonicalFunctionCall); ok {
				t.Fatal("server_tool_use synthesized a function call with no upstream executor")
			}
		}
	}
	mcp := []byte(`{"model":"m","max_tokens":8,` +
		`"messages":[{"role":"assistant","content":[{"type":"mcp_tool_use","id":"call_9","name":"read","input":{"x":1}}]}]}`)
	mcpRes, err := DecodeMessagesRequest(mcp, StrictLossPolicy())
	if err != nil {
		t.Fatalf("mcp_tool_use rejected under strict policy: %v", err)
	}
	found := false
	for _, turn := range mcpRes.Request.Turns {
		for _, part := range turn.Parts {
			if call, ok := part.(CanonicalFunctionCall); ok {
				found = true
				if call.CallID != "call_9" || call.Name != "read" {
					t.Fatalf("mcp call identity = %+v, want call_9/read", call)
				}
			}
		}
	}
	if !found {
		t.Fatal("mcp_tool_use did not map to a function call")
	}
	for _, loss := range mcpRes.Report.Losses {
		if loss.Feature == FeatureAnthropicServerTools {
			t.Fatalf("mcp mapping touched the server key: %+v", loss)
		}
	}
}
