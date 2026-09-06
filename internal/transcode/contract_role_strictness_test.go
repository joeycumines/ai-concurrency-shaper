package transcode

// Contract-role strictness regression tests (2026-09-06). The transcoder
// applies strictness by DIRECTION and by CONTRACT ROLE, not blanket:
//
//   - The CLIENT contract (OpenAI Responses + Anthropic Messages client
//     requests) is authoritative/pinned — an unknown field is a client-dialect
//     error (a consequence of the lossless-transcoding invariant).
//   - The UPSTREAM provider contract (Chat Completions + Responses response
//     envelope) is subject to change — an unknown field is a provider
//     extension and is TOLERATED (never a failure), never forwarded.
//   - The content-block UNIONS stay strict: a text block carrying image_url,
//     or an unknown content-block type, is still rejected.
//
// See AGENTS.md 'Contract-role strictness and the directional loss model'.

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestContractRoleUnknownFieldToleratedOnUpstreamResponse proves an arbitrary
// unknown provider-extension field on the upstream response envelope is
// TOLERATED — it never fails the request and never reaches the client.
func TestContractRoleUnknownFieldToleratedOnUpstreamResponse(t *testing.T) {
	// An arbitrary, unmodeled field at both the chat envelope and usage.
	body := `{"id":"c","object":"chat.completion","created":1,"model":"m","provider_meta_x":"v","choices":[{"index":0,"logprobs":null,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"provider_meta_y":3}}`
	response, _, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatalf("upstream response with unknown provider fields rejected: %v", err)
	}
	if len(response.Items) != 1 {
		t.Fatalf("response = %+v", response)
	}

	// The unknown extensions must never reach the client dialect. Render with
	// a policy that approves the usage breakdown losses (the body usage
	// carries no cache/reasoning breakdown, which the Messages dialect
	// requires).
	allowed, err := ParseLossFeatures(
		"usage_unknown",
		"usage_cache_read_unknown",
		"usage_cache_write_unknown",
		"usage_reasoning_unknown",
	)
	if err != nil {
		t.Fatalf("parse loss features: %v", err)
	}
	ctx := &ExchangeContext{
		IDs:        NewExchangeIDs(),
		LossPolicy: LossPolicy{Allowed: allowed},
	}
	rendered, _, err := RenderMessagesResponse(response, ctx)
	if err != nil {
		t.Fatalf("render client dialect: %v", err)
	}
	for _, leaked := range []string{"provider_meta_x", "provider_meta_y"} {
		if bytes.Contains(rendered, []byte(leaked)) {
			t.Fatalf("%s leaked into client output: %s", leaked, rendered)
		}
	}

	// The same contract role applies to the OpenAI Responses upstream
	// response envelope.
	if _, err := DecodeResponsesResponse([]byte(
		`{"id":"r","object":"response","created_at":1,"status":"completed","model":"m","output":[],"provider_meta_z":true}`,
	)); err != nil {
		t.Fatalf("responses upstream response with unknown provider field rejected: %v", err)
	}
}

// TestContractRoleUnknownFieldRejectedOnClientRequest proves an unknown field
// on a CLIENT request is still a client-dialect error (the lossless-transcoding
// invariant) for BOTH the OpenAI Responses and Anthropic Messages dialects.
func TestContractRoleUnknownFieldRejectedOnClientRequest(t *testing.T) {
	// OpenAI Responses client request with an unknown field.
	_, _, err := DecodeResponsesRequest(
		[]byte(`{"model":"m","input":"hi","bogus":1}`),
		StrictLossPolicy(),
	)
	if err == nil {
		t.Fatal("responses client request accepted an unknown field; want a client-dialect error")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v, want it to name the offending field", err)
	}

	// Anthropic Messages client request with an unknown field.
	_, err = DecodeMessagesRequest(
		[]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"x"}],"bogus":1}`),
		StrictLossPolicy(),
	)
	if err == nil {
		t.Fatal("messages client request accepted an unknown field; want a client-dialect error")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v, want it to name the offending field", err)
	}
}

// TestContractRoleUnionArmStrictnessPreserved proves the content-block union
// strictness survives the tolerant upstream envelope: a text block carrying
// image_url (cross-arm) and an unknown content-block type are still rejected.
func TestContractRoleUnionArmStrictnessPreserved(t *testing.T) {
	// Cross-arm leakage: a text block carrying image_url.
	crossArm := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"text","text":"x","image_url":{"url":"https://example.test/x.png"}}]}}]}`
	if _, _, err := DecodeChatResponseWithPolicy([]byte(crossArm), ChatCapabilities{}, StrictLossPolicy()); err == nil {
		t.Fatal("text content block carrying image_url accepted; want rejection")
	}

	// Unknown content-block type.
	unknownType := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"thinking","thinking":"x"}]}}]}`
	if _, _, err := DecodeChatResponseWithPolicy([]byte(unknownType), ChatCapabilities{}, StrictLossPolicy()); err == nil {
		t.Fatal("unknown content-block type accepted; want rejection")
	}
}

// TestContractRoleResponsesSSEEnvelopeTolerantUnionStrict proves the
// contract-role split on the OpenAI Responses SSE event path: a bogus field on
// the event ENVELOPE is tolerated (a provider extension), while an unknown
// content-part type inside the item is still rejected (union strictness).
func TestContractRoleResponsesSSEEnvelopeTolerantUnionStrict(t *testing.T) {
	// Envelope tolerance: an output_item.added event carrying a bogus
	// envelope field decodes.
	ok := `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"m_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]},"bogus":1}`
	if _, err := decodeResponsesSSEEvent([]byte(ok)); err != nil {
		t.Fatalf("output_item.added with bogus envelope field rejected: %v", err)
	}

	// Union strictness: an unknown content-part type inside the item is still
	// rejected.
	bad := `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"m_1","status":"completed","role":"assistant","content":[{"type":"thinking","text":"x"}]}}`
	if _, err := decodeResponsesSSEEvent([]byte(bad)); err == nil {
		t.Fatal("output_item.added with unknown content-part type accepted; want rejection")
	}
}

// TestLogConversionReportDistinguishesApprovedLossesAndNotes proves the
// aggregated loss line distinguishes approved losses from Notes and carries the
// Note's detail (the startup summary covers only the policy's Allowed set).
func TestLogConversionReportDistinguishesApprovedLossesAndNotes(t *testing.T) {
	allowed, err := ParseLossFeatures("mid_conversation_system")
	if err != nil {
		t.Fatal(err)
	}
	h := &TranscodeHandler{cfg: HandlerConfig{Mapping: Mapping{LossPolicy: LossPolicy{Allowed: allowed}}}}
	report := ConversionReport{Losses: []ConversionLoss{
		{Feature: FeatureMidConversationSystem, Path: "messages[]", Detail: "consolidated", Kind: LossRecord},
		{Feature: FeatureProviderReasoningText, Path: "choices[0].message.content", Detail: "provider reasoning text", Kind: NoteRecord},
	}}
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	h.logConversionReport(report, req, "request")
	out := buf.String()
	if !strings.Contains(out, "loss(es)/note(s)") {
		t.Fatalf("header missing loss(es)/note(s): %q", out)
	}
	if !strings.Contains(out, "note: provider_reasoning_text at choices[0].message.content: provider reasoning text") {
		t.Fatalf("note detail missing: %q", out)
	}
	if !strings.Contains(out, "mid_conversation_system at messages[]") {
		t.Fatalf("approved loss missing: %q", out)
	}
	if got := strings.Count(out, "\n"); got != 1 {
		t.Fatalf("expected exactly 1 log line, got %d: %q", got, out)
	}
}

// TestLogConversionReportNoteUnderAllowedKeyProvesKindWins proves a Note is
// labeled a note even when its feature key IS in the policy's Allowed set
// (classification is by the recorded Kind, not by policy membership).
func TestLogConversionReportNoteUnderAllowedKeyProvesKindWins(t *testing.T) {
	allowed, err := ParseLossFeatures("responses_controls")
	if err != nil {
		t.Fatal(err)
	}
	h := &TranscodeHandler{cfg: HandlerConfig{Mapping: Mapping{LossPolicy: LossPolicy{Allowed: allowed}}}}
	report := ConversionReport{Losses: []ConversionLoss{
		{Feature: FeatureResponsesControls, Path: "include", Detail: "include is noted", Kind: NoteRecord},
	}}
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	h.logConversionReport(report, req, "request")
	out := buf.String()
	if !strings.Contains(out, "note: responses_controls at include: include is noted") {
		t.Fatalf("note under an allowed key mislabeled: %q", out)
	}
	if strings.Contains(out, "approved loss") {
		t.Fatalf("a Note under an allowed key labeled as an approved loss: %q", out)
	}
}

// TestContractRoleNonStreamDeltaArmRejected proves the streaming-only delta arm
// is a STRUCTURAL rejection on the non-streaming chat response (GAP-012
// parity), so its content can never be silently dropped.
func TestContractRoleNonStreamDeltaArmRejected(t *testing.T) {
	body := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","delta":{"content":"SHOULD NOT BE DROPPED"},"message":{"role":"assistant","content":"ok"}}]}`
	if _, _, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, StrictLossPolicy()); err == nil {
		t.Fatal("non-streaming chat response with a delta arm accepted; want rejection")
	}
	// A normal non-streaming response still decodes.
	if _, _, err := DecodeChatResponseWithPolicy([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`), ChatCapabilities{}, StrictLossPolicy()); err != nil {
		t.Fatalf("normal non-streaming response rejected: %v", err)
	}
}

// TestContractRoleLegacyFunctionCallRejected proves the legacy non-tool_calls
// function_call spelling is a STRUCTURAL rejection on both the non-streaming
// and streaming chat surfaces (a KNOWN official field, not a provider
// extension), so it is never silently dropped (which would leave the client
// with a tool_use stop reason and no tool call).
func TestContractRoleLegacyFunctionCallRejected(t *testing.T) {
	// Non-streaming message.function_call.
	nonStream := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"function_call","message":{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{}"}}}]}`
	if _, _, err := DecodeChatResponseWithPolicy([]byte(nonStream), ChatCapabilities{}, StrictLossPolicy()); err == nil {
		t.Fatal("non-streaming message.function_call accepted; want rejection")
	}
	// Streaming delta.function_call.
	stream := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"function_call":{"name":"f","arguments":"{}"}},"finish_reason":null}]}`
	if _, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(stream)}); err == nil {
		t.Fatal("streaming delta.function_call accepted; want rejection")
	}
}

// TestStartupLossProfileSummary proves the per-route startup summary logs the
// route's allowed set, and 'none (strict)' for a strict route.
func TestStartupLossProfileSummary(t *testing.T) {
	// Non-empty allowed set.
	m := messagesMapping(t, UpstreamChatCompletions)
	allowed, err := ParseLossFeatures("mid_conversation_system", "usage_unknown")
	if err != nil {
		t.Fatal(err)
	}
	m.LossPolicy = LossPolicy{Allowed: allowed}
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	_ = testHandler(t, m, func(*http.Request) (*http.Response, error) { return nil, nil })
	out := buf.String()
	if !strings.Contains(out, "loss policy approves:") {
		t.Fatalf("startup summary missing: %q", out)
	}
	if !strings.Contains(out, "mid_conversation_system") || !strings.Contains(out, "usage_unknown") {
		t.Fatalf("startup summary missing allowed keys: %q", out)
	}

	// Strict route logs 'none (strict)'.
	buf.Reset()
	m2 := messagesMapping(t, UpstreamChatCompletions)
	m2.LossPolicy = StrictLossPolicy()
	_ = testHandler(t, m2, func(*http.Request) (*http.Response, error) { return nil, nil })
	out2 := buf.String()
	if !strings.Contains(out2, "loss policy approves: none (strict)") {
		t.Fatalf("strict route summary missing 'none (strict)': %q", out2)
	}
}
