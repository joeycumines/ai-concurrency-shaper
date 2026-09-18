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

func TestRenderChatMultiAgentPriming(t *testing.T) {
	// Case 1: MultiAgentPriming: false (default)
	req := CanonicalRequest{
		ClientModel: "test-model",
		Turns: []CanonicalTurn{
			{
				Role: CanonicalSystem,
				Parts: []CanonicalPart{
					CanonicalText{Text: "You are an AI assistant."},
				},
			},
			{
				Role: CanonicalUser,
				Parts: []CanonicalPart{
					CanonicalText{Text: "Help me write code."},
				},
			},
		},
	}
	ctx := testExchangeContext()

	// Off by default
	renderedBytes, report, err := RenderChatRequest(req, ctx, ChatCapabilities{})
	if err != nil {
		t.Fatalf("RenderChatRequest failed: %v", err)
	}
	var chatReq ChatRequest
	if err := json.Unmarshal(renderedBytes, &chatReq); err != nil {
		t.Fatalf("unmarshal rendered chat request: %v", err)
	}
	if len(chatReq.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(chatReq.Messages))
	}
	for _, loss := range report.Losses {
		if loss.Feature == FeatureMultiAgentPriming {
			t.Fatalf("unexpected multi_agent_priming Note when disabled: %+v", loss)
		}
	}
	// Verify content was not modified
	if *chatReq.Messages[0].Content.ContentBlocks[0].Text != "You are an AI assistant." {
		t.Fatalf("expected untouched system message, got %q", *chatReq.Messages[0].Content.ContentBlocks[0].Text)
	}

	// Case 2: MultiAgentPriming: true with existing system message
	renderedBytes, report, err = RenderChatRequest(req, ctx, ChatCapabilities{
		MultiAgentPriming: true,
	})
	if err != nil {
		t.Fatalf("RenderChatRequest with MultiAgentPriming failed: %v", err)
	}
	if err := json.Unmarshal(renderedBytes, &chatReq); err != nil {
		t.Fatalf("unmarshal rendered chat request: %v", err)
	}
	// Note must be recorded
	foundNote := false
	for _, loss := range report.Losses {
		if loss.Feature == FeatureMultiAgentPriming && loss.Kind == NoteRecord {
			foundNote = true
			break
		}
	}
	if !foundNote {
		t.Fatalf("expected FeatureMultiAgentPriming Note in report, got: %+v", report.Losses)
	}
	// Check that leading system turn has the original text preserved AND the reminder appended
	sysBlocks := chatReq.Messages[0].Content.ContentBlocks
	if len(sysBlocks) != 2 {
		t.Fatalf("expected 2 content blocks in leading system turn, got %d", len(sysBlocks))
	}
	if *sysBlocks[0].Text != "You are an AI assistant." {
		t.Fatalf("client instructions corrupted: got %q", *sysBlocks[0].Text)
	}
	if *sysBlocks[1].Text != MultiAgentPrimingReminderText {
		t.Fatalf("reminder text mismatch: got %q", *sysBlocks[1].Text)
	}

	// Case 3: MultiAgentPriming: true without existing system message (dialog only)
	reqNoSys := CanonicalRequest{
		ClientModel: "test-model",
		Turns: []CanonicalTurn{
			{
				Role: CanonicalUser,
				Parts: []CanonicalPart{
					CanonicalText{Text: "Help me write code."},
				},
			},
		},
	}
	renderedBytes, report, err = RenderChatRequest(reqNoSys, ctx, ChatCapabilities{
		MultiAgentPriming: true,
	})
	if err != nil {
		t.Fatalf("RenderChatRequest with MultiAgentPriming without sys message failed: %v", err)
	}
	if err := json.Unmarshal(renderedBytes, &chatReq); err != nil {
		t.Fatalf("unmarshal rendered chat request: %v", err)
	}
	if len(chatReq.Messages) != 2 {
		t.Fatalf("expected 2 messages (prepended system + user), got %d", len(chatReq.Messages))
	}
	if chatReq.Messages[0].Role != ChatMessageRoleSystem {
		t.Fatalf("expected leading message to be system, got %s", chatReq.Messages[0].Role)
	}
	if *chatReq.Messages[0].Content.ContentBlocks[0].Text != MultiAgentPrimingReminderText {
		t.Fatalf("expected reminder text, got %q", *chatReq.Messages[0].Content.ContentBlocks[0].Text)
	}
	if chatReq.Messages[1].Role != ChatMessageRoleUser {
		t.Fatalf("expected second message to be user, got %s", chatReq.Messages[1].Role)
	}

	// Case 4: MultiAgentPriming: true with empty conversation must still be rejected
	reqEmpty := CanonicalRequest{
		ClientModel: "test-model",
		Turns:       nil,
	}
	_, _, err = RenderChatRequest(reqEmpty, ctx, ChatCapabilities{
		MultiAgentPriming: true,
	})
	if err == nil {
		t.Fatal("expected empty conversation to be rejected even with MultiAgentPriming: true")
	}
	if !strings.Contains(err.Error(), "the source request has no Chat-representable messages") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestApplyMultiAgentPriming_NonMutatingStringPreservationAndDeveloperRole(t *testing.T) {
	// 1. String preservation & non-mutation:
	origStr := "System prompt string"
	origContent := &ChatMessageContent{
		ContentStr: &origStr,
	}
	messages := []ChatMessage{
		{
			Role:    ChatMessageRoleSystem,
			Content: origContent,
		},
	}
	var report ConversionReport
	primed, err := applyMultiAgentPriming(messages, &report)
	if err != nil {
		t.Fatalf("applyMultiAgentPriming failed: %v", err)
	}

	// Verify input was NOT mutated
	if messages[0].Content != origContent {
		t.Errorf("input messages[0].Content pointer changed")
	}
	if origContent.ContentStr == nil || *origContent.ContentStr != "System prompt string" {
		t.Errorf("original ContentStr was mutated: %v", origContent.ContentStr)
	}
	if origContent.ContentBlocks != nil {
		t.Errorf("original ContentBlocks was mutated: %v", origContent.ContentBlocks)
	}

	// Verify output preserved string format
	if primed[0].Content.ContentStr == nil {
		t.Fatalf("primed[0].Content.ContentStr is nil (should have preserved string format)")
	}
	wantStr := "System prompt string\n\n" + MultiAgentPrimingReminderText
	if *primed[0].Content.ContentStr != wantStr {
		t.Errorf("primed[0].Content.ContentStr = %q, want %q", *primed[0].Content.ContentStr, wantStr)
	}
	if primed[0].Content.ContentBlocks != nil {
		t.Errorf("primed[0].Content.ContentBlocks should be nil for string system prompt")
	}

	// 2. Developer role preservation:
	devStr := "Developer prompt string"
	devMessages := []ChatMessage{
		{
			Role:    ChatMessageRoleDeveloper,
			Content: &ChatMessageContent{ContentStr: &devStr},
		},
		{
			Role:    ChatMessageRoleUser,
			Content: &ChatMessageContent{ContentStr: &origStr},
		},
	}
	var reportDev ConversionReport
	primedDev, err := applyMultiAgentPriming(devMessages, &reportDev)
	if err != nil {
		t.Fatalf("applyMultiAgentPriming on developer role failed: %v", err)
	}
	if len(primedDev) != 2 {
		t.Fatalf("expected 2 messages, got %d (should not have prepended a system message)", len(primedDev))
	}
	if primedDev[0].Role != ChatMessageRoleDeveloper {
		t.Errorf("primedDev[0].Role = %s, want developer", primedDev[0].Role)
	}
	// Note description should state appended to leading developer turn
	foundDevNote := false
	for _, l := range reportDev.Losses {
		if l.Feature == FeatureMultiAgentPriming && strings.Contains(l.Detail, "developer turn") {
			foundDevNote = true
			break
		}
	}
	if !foundDevNote {
		t.Errorf("expected developer turn note in report, got: %+v", reportDev.Losses)
	}

	// 3. Prepending note accuracy when no system/developer turn exists:
	userMessages := []ChatMessage{
		{
			Role:    ChatMessageRoleUser,
			Content: &ChatMessageContent{ContentStr: &origStr},
		},
	}
	var reportUser ConversionReport
	primedUser, err := applyMultiAgentPriming(userMessages, &reportUser)
	if err != nil {
		t.Fatalf("applyMultiAgentPriming on user message failed: %v", err)
	}
	if len(primedUser) != 2 {
		t.Fatalf("expected 2 messages (prepended system + user), got %d", len(primedUser))
	}
	foundPrependNote := false
	for _, l := range reportUser.Losses {
		if l.Feature == FeatureMultiAgentPriming && strings.Contains(l.Detail, "prepended") && l.Path == "messages[0]" {
			foundPrependNote = true
			break
		}
	}
	if !foundPrependNote {
		t.Errorf("expected prepended note with path messages[0], got: %+v", reportUser.Losses)
	}

	// 4. Empty slice case:
	var reportEmpty ConversionReport
	primedEmpty, err := applyMultiAgentPriming(nil, &reportEmpty)
	if err != nil {
		t.Fatalf("applyMultiAgentPriming on nil slice failed: %v", err)
	}
	if len(primedEmpty) != 1 || primedEmpty[0].Role != ChatMessageRoleSystem {
		t.Fatalf("expected 1 prepended system message, got %d", len(primedEmpty))
	}
}
