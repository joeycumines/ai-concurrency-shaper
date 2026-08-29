package transcode

// J11 regression tests: the strict Responses envelope aligned with the
// pinned contract (openai-go v1.12.0) — every pinned envelope field decodes
// as a typed shadow, the outbound instructions is always the create-request
// string, and the envelope controls enter the explicit loss/reject decision
// (review-j findings 13 and 17).

import (
	"strings"
	"testing"
)

// currentOfficialEnvelope returns a Responses response carrying every pinned
// envelope extra: background, max_tool_calls, prompt, prompt_cache_key, and
// safety_identifier.
func currentOfficialEnvelope() string {
	return `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"background":false,"max_tool_calls":5,"prompt":{"id":"pt_1","version":"v2","variables":{"name":"Tokyo"}},"prompt_cache_key":"cache_1","safety_identifier":"red_team","usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}}`
}

// TestResponsesEnvelopePinnedControlsStrictDecode proves a current official
// response carrying every pinned envelope extra strict-decodes with no
// unknown-field failure, flags the controls, and enters the explicit
// loss/reject decision (review-j finding 13).
func TestResponsesEnvelopePinnedControlsStrictDecode(t *testing.T) {
	response, err := DecodeResponsesResponse([]byte(currentOfficialEnvelope()))
	if err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if len(response.Source.ResponsesControls) != 5 {
		t.Fatalf("controls = %v, want all five", response.Source.ResponsesControls)
	}

	strict := testExchangeContext()
	strict.RequestedClientModel = "m"
	if _, _, err := RenderMessagesResponse(response, strict); err == nil {
		t.Fatal("strict policy accepted the pinned envelope controls")
	}

	permissive := testExchangeContext()
	permissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureResponsesControls:      {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	permissive.RequestedClientModel = "m"
	rendered, report, err := RenderMessagesResponse(response, permissive)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "hi") {
		t.Fatalf("content must still render: %s", rendered)
	}
	if !reportHasFeature(report, FeatureResponsesControls) {
		t.Fatalf("report lacks the controls loss: %+v", report)
	}
}

// TestResponsesEnvelopeControlsAbsentNoLoss proves an envelope without the
// pinned controls renders cleanly under the strict policy.
func TestResponsesEnvelopeControlsAbsentNoLoss(t *testing.T) {
	source := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}}`
	response, err := DecodeResponsesResponse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Source.ResponsesControls) != 0 {
		t.Fatalf("controls = %v, want none", response.Source.ResponsesControls)
	}
	// The usage breakdown is fully known (both detail objects present), but
	// cache-creation is never part of the pinned Responses contract: the
	// Messages render enters the usage-timing loss decision (review-k
	// finding 6). The controls are absent — that is the property under
	// test.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	context.RequestedClientModel = "m"
	if _, _, err := RenderMessagesResponse(response, context); err != nil {
		t.Fatalf("render with no controls: %v", err)
	}
}

// TestResponsesRequestInstructionsStringOnly proves the outbound Responses
// request always emits the pinned create-request instructions string — never
// an items array (review-j finding 13).
func TestResponsesRequestInstructionsStringOnly(t *testing.T) {
	// Text-only system: the string arm.
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"system":"You are helpful."}`
	result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	rendered, _, err := RenderResponsesRequest(result.Request, context)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), `"instructions":"You are helpful."`) {
		t.Fatalf("text-only system must render the instructions string: %s", rendered)
	}
	if strings.Contains(string(rendered), `"items"`) {
		t.Fatalf("instructions rendered as an items array: %s", rendered)
	}

	// A system prompt with an image cannot be expressed in the string:
	// strict rejects, permissive records the loss — never an array.
	imageBody := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"system":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}`
	result, err = DecodeMessagesRequest([]byte(imageBody), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	strictContext := testExchangeContext()
	if _, _, err := RenderResponsesRequest(result.Request, strictContext); err == nil {
		t.Fatal("strict policy accepted an image system prompt into the string-only instructions")
	}
	permissive := testExchangeContext()
	permissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent: {},
	}}
	rendered, report, err := RenderResponsesRequest(result.Request, permissive)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), `"items"`) {
		t.Fatalf("instructions rendered as an items array: %s", rendered)
	}
	if !reportHasFeature(report, FeatureSystemNonTextContent) {
		t.Fatalf("report lacks the image loss: %+v", report)
	}
}

// TestResponsesEchoInstructionsStringArm proves the response envelope echo
// renders the client's string instructions as the string arm of the pinned
// instructions union, and omits it when absent.
func TestResponsesEchoInstructionsStringArm(t *testing.T) {
	_, echo, err := DecodeResponsesRequest(
		[]byte(`{"model":"m","input":"x","instructions":"be helpful"}`),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	// The pinned Responses usage requires the breakdown detail objects the
	// source did not provide: the render enters the usage-timing loss
	// decision (review-k finding 6).
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	context.OriginalResponsesRequest = echo
	context.RequestedClientModel = "m"
	rendered, _, err := RenderResponsesResponse(CanonicalResponse{
		ID:        "resp_1",
		Model:     "m",
		CreatedAt: 1,
		Status:    CanonicalResponseCompleted,
		Stop:      CanonicalStop{Reason: CanonicalStopEndTurn},
		Items: []CanonicalResponseItem{&CanonicalMessageItem{
			Role:  CanonicalAssistant,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
		Usage: CanonicalUsage{
			InputTokens:  1,
			OutputTokens: 1,
			InputKnown:   true,
			OutputKnown:  true,
		},
	}, context)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), `"instructions":"be helpful"`) {
		t.Fatalf("echo must render the string arm: %s", rendered)
	}
	if strings.Contains(string(rendered), `"items"`) {
		t.Fatalf("echo rendered an items array: %s", rendered)
	}

	// No instructions on the request: the echo omits the field.
	_, echo, err = DecodeResponsesRequest(
		[]byte(`{"model":"m","input":"x"}`),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if echo.Instructions != nil {
		t.Fatalf("echo instructions = %+v, want nil", echo.Instructions)
	}
}

// TestStreamingEnvelopeControlsGated proves the streaming direction gates
// the pinned envelope controls exactly once per stream instead of silently
// dropping them (review-j finding 13).
func TestStreamingEnvelopeControlsGated(t *testing.T) {
	// Strict: a created envelope carrying controls rejects the stream.
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"msg_1",
		"claude-x",
		1,
	)
	_, err := state.Convert(ResponseCreatedEvent{
		EventBase: EventBase{Type: "response.created", SequenceNumber: 1},
		Response:  envelopeWithControls(),
	})
	if err == nil {
		t.Fatal("strict policy accepted envelope controls in a stream")
	}

	// Permissive: gated exactly once even though the created and completed
	// envelopes both carry the controls.
	state = newAnthropicResponsesStreamState(
		testStreamContext(),
		LossPolicy{Allowed: map[Feature]struct{}{
			FeatureResponsesControls:      {},
			FeatureUsageCacheReadUnknown:  {},
			FeatureUsageCacheWriteUnknown: {},
			FeatureUsageReasoningUnknown:  {},
			FeatureUsageUnknown:           {},
		}},
		"msg_1",
		"claude-x",
		1,
	)
	if _, err := state.Convert(ResponseCreatedEvent{
		EventBase: EventBase{Type: "response.created", SequenceNumber: 1},
		Response:  envelopeWithControls(),
	}); err != nil {
		t.Fatal(err)
	}
	completed := envelopeWithControls()
	completed.Status = "completed"
	if _, err := state.Convert(ResponseCompletedEvent{
		EventBase: EventBase{Type: "response.completed", SequenceNumber: 2},
		Response:  completed,
	}); err != nil {
		t.Fatal(err)
	}
	if count := countFeature(state.report, FeatureResponsesControls); count != 1 {
		t.Fatalf("controls losses = %d, want exactly one", count)
	}
}

// envelopeWithControls returns a minimal envelope carrying all five pinned
// controls.
func envelopeWithControls() ResponseEnvelope {
	return ResponseEnvelope{
		ID:               "resp_1",
		Object:           "response",
		CreatedAt:        1,
		Status:           "in_progress",
		Model:            "m",
		Output:           []ResponsesOutputItem{},
		Background:       new(false),
		MaxToolCalls:     new(int64(5)),
		Prompt:           &ResponsesEnvelopePrompt{ID: "pt_1", Version: "v2"},
		PromptCacheKey:   "cache_1",
		SafetyIdentifier: "red_team",
		Usage: &ResponsesUsage{
			InputTokens:         1,
			OutputTokens:        1,
			TotalTokens:         2,
			InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
			OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
		},
	}
}

// TestResponsesInstructionsMultiTurnAndParts covers the remaining
// instructions loss branches: multiple system turns, document parts, and the
// text-join encoding note (review-j finding 13).
func TestResponsesInstructionsMultiTurnAndParts(t *testing.T) {
	// Multiple system turns: a loss/reject decision, never an items array.
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "one"}}},
			{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "two"}}},
		},
	}
	permissive := testExchangeContext()
	permissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMultipleSystemTurns: {},
	}}
	rendered, report, err := RenderResponsesRequest(request, permissive)
	if err != nil {
		t.Fatal(err)
	}
	if !reportHasFeature(report, FeatureMultipleSystemTurns) {
		t.Fatalf("report lacks the multi-turn loss: %+v", report)
	}
	if strings.Contains(string(rendered), `"items"`) {
		t.Fatalf("multi-turn instructions rendered as items: %s", rendered)
	}

	// A document system part is loss-gated with system_non_text_content.
	request = CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role:  CanonicalSystem,
			Parts: []CanonicalPart{CanonicalDocument{URL: "https://example.com/doc.pdf"}},
		}},
	}
	permissive = testExchangeContext()
	permissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent: {},
	}}
	rendered, report, err = RenderResponsesRequest(request, permissive)
	if err != nil {
		t.Fatal(err)
	}
	if !reportHasFeature(report, FeatureSystemNonTextContent) {
		t.Fatalf("report lacks the document loss: %+v", report)
	}
	if strings.Contains(string(rendered), `"items"`) {
		t.Fatalf("document instructions rendered as items: %s", rendered)
	}

	// Multiple text parts in one system turn join with the named encoding.
	request = CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role: CanonicalSystem,
			Parts: []CanonicalPart{
				CanonicalText{Text: "one"},
				CanonicalText{Text: "two"},
			},
		}},
	}
	rendered, report, err = RenderResponsesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), `"instructions":"one\ntwo"`) {
		t.Fatalf("text parts must join into the string: %s", rendered)
	}
	if !reportHasFeature(report, FeatureMultipleSystemTurns) {
		t.Fatalf("report lacks the join encoding note: %+v", report)
	}
}

// TestStreamingEnvelopeControlsLateAppearance proves controls appearing only
// on the completed envelope (not the created one) are still gated exactly
// once — the gate must not latch on a control-free first envelope
// (review-j finding 13).
func TestStreamingEnvelopeControlsLateAppearance(t *testing.T) {
	// Strict for controls: the usage-timing loss (the required
	// cache-creation breakdown the Responses source never provides,
	// review-k finding 6) must be allowed so the created envelope passes and
	// the late controls are the only rejection.
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		LossPolicy{Allowed: map[Feature]struct{}{
			FeatureUsageCacheReadUnknown:  {},
			FeatureUsageCacheWriteUnknown: {},
			FeatureUsageReasoningUnknown:  {},
			FeatureUsageUnknown:           {},
		}},
		"msg_1",
		"claude-x",
		1,
	)
	created := ResponseEnvelope{
		ID:        "resp_1",
		Object:    "response",
		CreatedAt: 1,
		Status:    "in_progress",
		Model:     "m",
		Output:    []ResponsesOutputItem{},
		Usage: &ResponsesUsage{
			InputTokens:         1,
			OutputTokens:        1,
			TotalTokens:         2,
			InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
			OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
		},
	}
	if _, err := state.Convert(ResponseCreatedEvent{
		EventBase: EventBase{Type: "response.created", SequenceNumber: 1},
		Response:  created,
	}); err != nil {
		t.Fatal(err)
	}
	completed := envelopeWithControls()
	completed.Status = "completed"
	if _, err := state.Convert(ResponseCompletedEvent{
		EventBase: EventBase{Type: "response.completed", SequenceNumber: 2},
		Response:  completed,
	}); err == nil {
		t.Fatal("strict policy accepted late-appearing envelope controls")
	}

	// Permissive: exactly one loss despite the control-free created
	// envelope.
	state = newAnthropicResponsesStreamState(
		testStreamContext(),
		LossPolicy{Allowed: map[Feature]struct{}{
			FeatureResponsesControls:      {},
			FeatureUsageCacheReadUnknown:  {},
			FeatureUsageCacheWriteUnknown: {},
			FeatureUsageReasoningUnknown:  {},
			FeatureUsageUnknown:           {},
		}},
		"msg_1",
		"claude-x",
		1,
	)
	if _, err := state.Convert(ResponseCreatedEvent{
		EventBase: EventBase{Type: "response.created", SequenceNumber: 1},
		Response: ResponseEnvelope{
			ID:        "resp_1",
			Object:    "response",
			CreatedAt: 1,
			Status:    "in_progress",
			Model:     "m",
			Output:    []ResponsesOutputItem{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseCompletedEvent{
		EventBase: EventBase{Type: "response.completed", SequenceNumber: 2},
		Response:  completed,
	}); err != nil {
		t.Fatal(err)
	}
	if count := countFeature(state.report, FeatureResponsesControls); count != 1 {
		t.Fatalf("controls losses = %d, want exactly one", count)
	}
}
