package transcode

// J7 regression tests: the loss-policy semantic matrix and response-side
// reporting (review-j finding 10).

import (
	"strings"
	"testing"
)

// TestToolResultIsErrorStrictRejects proves a tool result marked as an error
// is rejected by default when rendering to Responses and Chat (review-j
// finding 10).
func TestToolResultIsErrorStrictRejects(t *testing.T) {
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","is_error":true,"content":"boom"}]}]}`
	result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()

	if _, _, err := RenderResponsesRequest(result.Request, context); err == nil {
		t.Fatal("strict policy accepted an error tool result into Responses")
	}
	if _, _, err := RenderChatRequest(result.Request, context, ChatCapabilities{ParallelToolCalls: true}); err == nil {
		t.Fatal("strict policy accepted an error tool result into Chat")
	}

	// The request decodes the is_error flag.
	found := false
	for _, turn := range result.Request.Turns {
		for _, part := range turn.Parts {
			if fr, ok := part.(CanonicalFunctionResult); ok && fr.IsError {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("is_error flag lost at decode")
	}
}

// TestToolResultIsErrorPermissiveEncoding proves the permissive policy
// encodes the error status into visible content with the named
// error_status_prefix encoding, and the loss is reported (review-j finding
// 10).
func TestToolResultIsErrorPermissiveEncoding(t *testing.T) {
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","is_error":true,"content":"boom"}]}]}`
	result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolResultErrorStatus: {},
	}}

	rendered, report, err := RenderResponsesRequest(result.Request, context)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "[tool_result_error]") {
		t.Fatalf("responses render lacks the error_status_prefix encoding: %s", rendered)
	}
	if !reportHasFeature(report, FeatureToolResultErrorStatus) {
		t.Fatalf("responses render report lacks the tool_result_error loss: %+v", report)
	}

	rendered, report, err = RenderChatRequest(result.Request, context, ChatCapabilities{ParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "[tool_result_error]") {
		t.Fatalf("chat render lacks the error_status_prefix encoding: %s", rendered)
	}
	if !reportHasFeature(report, FeatureToolResultErrorStatus) {
		t.Fatalf("chat render report lacks the tool_result_error loss: %+v", report)
	}
}

// TestReasoningSummaryRequestLossGate proves the Responses reasoning.summary
// style entering a Chat request is a loss/reject decision (review-j finding
// 10).
func TestReasoningSummaryRequestLossGate(t *testing.T) {
	body := `{"model":"m","input":"x","reasoning":{"effort":"medium","summary":"auto"}}`
	result, echo, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if echo == nil || echo.Reasoning == nil || echo.Reasoning.Summary == nil {
		t.Fatal("summary not captured in the echo")
	}

	strict := testExchangeContext()
	strict.OriginalResponsesRequest = echo
	if _, _, err := RenderChatRequest(result.Request, strict, ChatCapabilities{ReasoningEffort: true}); err == nil {
		t.Fatal("strict policy accepted reasoning.summary into Chat")
	}

	permissive := testExchangeContext()
	permissive.OriginalResponsesRequest = echo
	permissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureReasoningSummary:      {},
		FeatureProviderReasoningText: {},
	}}
	rendered, report, err := RenderChatRequest(result.Request, permissive, ChatCapabilities{ReasoningEffort: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), `"reasoning_effort":"medium"`) {
		t.Fatalf("effort must still render: %s", rendered)
	}
	if !reportHasFeature(report, FeatureReasoningSummary) {
		t.Fatalf("report lacks the reasoning_summary loss: %+v", report)
	}
}

// TestStreamProviderReasoningCapabilityGate proves streaming provider
// reasoning deltas are capability-gated: without the capability they enter
// the loss decision; with it they map to ordinary text with the named
// provider_reasoning_text encoding (review-j finding 10).
func TestStreamProviderReasoningCapabilityGate(t *testing.T) {
	// Without the capability: strict rejects, permissive drops with a loss.
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Reasoning: new("think")}, nil)); err == nil {
		t.Fatal("strict policy accepted a provider reasoning delta without the capability")
	}

	state = newChatResponsesStreamState(
		testStreamContext(),
		LossPolicy{Allowed: map[Feature]struct{}{FeatureProviderReasoningText: {}}},
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Reasoning: new("think")}, nil)); err != nil {
		t.Fatal(err)
	}
	if !reportHasFeature(state.report, FeatureProviderReasoningText) {
		t.Fatalf("report lacks the provider reasoning loss: %+v", state.report)
	}

	// With the capability: the delta maps to ordinary text with the named
	// encoding recorded in the report.
	state = newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{ProviderReasoningText: true},
		"resp_1",
		"m",
		1,
		nil,
	)
	events, err := state.Convert(chatChunk(t, ChatStreamDelta{Reasoning: new("think")}, nil))
	if err != nil {
		t.Fatalf("capability-gated reasoning delta rejected: %v", err)
	}
	sawText := false
	for _, event := range events {
		if delta, ok := event.(ResponseTextDeltaEvent); ok && delta.Delta == "think" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("provider reasoning delta not mapped to text: %+v", events)
	}
	if !reportHasFeature(state.report, FeatureProviderReasoningText) {
		t.Fatalf("report lacks the provider_reasoning_text encoding note: %+v", state.report)
	}
}

// TestOutputMessagePhaseLossGate proves a Responses output-message phase is
// a loss/reject decision when rendering to Messages (review-j finding 10).
func TestOutputMessagePhaseLossGate(t *testing.T) {
	source := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","phase":"commentary","content":[{"type":"output_text","text":"thinking out loud","annotations":[]}]}]}`
	response, err := DecodeResponsesResponse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if !responseHasPhase(response) {
		t.Fatal("phase flag not set at decode")
	}
	strict := testExchangeContext()
	strict.RequestedClientModel = "m"
	if _, _, err := RenderMessagesResponse(response, strict); err == nil {
		t.Fatal("strict policy accepted an output phase into Messages")
	}
	permissive := testExchangeContext()
	permissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureOutputPhase:            {},
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
	if !strings.Contains(string(rendered), "thinking out loud") {
		t.Fatalf("content must still render: %s", rendered)
	}
	if !reportHasFeature(report, FeatureOutputPhase) {
		t.Fatalf("report lacks the phase loss: %+v", report)
	}
}

// TestStreamOutputMessagePhaseGate proves the streaming direction loss-gates
// a phase-bearing message exactly once per item, in both the added event and
// the terminal envelope (review-j finding 10).
func TestStreamOutputMessagePhaseGate(t *testing.T) {
	created := ResponseEnvelope{
		ID:        "resp_1",
		Object:    "response",
		CreatedAt: 1,
		Status:    "in_progress",
		Model:     "m",
		Output:    []ResponsesOutputItem{},
		// The created envelope carries usage so the early-usage loss is not
		// part of this test's phase-loss accounting.
		Usage: &ResponsesUsage{
			InputTokens: 0, OutputTokens: 0, TotalTokens: 0,
			InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
			OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
		},
	}
	// Strict: the phase-bearing added event rejects the stream. The policy
	// approves the unavoidable usage-timing loss (cache_creation is never
	// part of the pinned Responses contract) so the created envelope can be
	// fed, but rejects the phase.
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
	if _, err := state.Convert(ResponseCreatedEvent{
		EventBase: EventBase{Type: "response.created", SequenceNumber: 0},
		Response:  created,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := state.Convert(ResponseOutputItemAddedEvent{
		EventBase: EventBase{
			Type:           "response.output_item.added",
			SequenceNumber: 1,
		},
		OutputIndex: 0,
		Item: &ResponsesOutputMessage{
			ID:      "msg_1",
			Type:    "message",
			Role:    "assistant",
			Status:  ResponsesItemInProgress,
			Phase:   "commentary",
			Content: ResponsesOutputContentParts{},
		},
	})
	if err == nil {
		t.Fatal("strict policy accepted a phase-bearing message item")
	}

	// Permissive: gated exactly once even though the phase appears in both
	// the added event and the terminal envelope.
	state = newAnthropicResponsesStreamState(
		testStreamContext(),
		LossPolicy{Allowed: map[Feature]struct{}{
			FeatureOutputPhase:            {},
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
		EventBase: EventBase{Type: "response.created", SequenceNumber: 0},
		Response:  created,
	}); err != nil {
		t.Fatal(err)
	}
	envelope := ResponseEnvelope{
		ID:        "resp_1",
		Object:    "response",
		CreatedAt: 1,
		Status:    "completed",
		Model:     "m",
		Output: []ResponsesOutputItem{&ResponsesOutputMessage{
			ID:      "msg_1",
			Type:    "message",
			Role:    "assistant",
			Status:  ResponsesItemCompleted,
			Phase:   "commentary",
			Content: ResponsesOutputContentParts{},
		}},
		Usage: &ResponsesUsage{
			InputTokens:         10,
			OutputTokens:        5,
			TotalTokens:         15,
			InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
			OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
		},
	}
	if _, err := state.Convert(ResponseOutputItemAddedEvent{
		EventBase: EventBase{
			Type:           "response.output_item.added",
			SequenceNumber: 1,
		},
		OutputIndex: 0,
		Item:        envelope.Output[0],
	}); err != nil {
		t.Fatal(err)
	}
	// The item is closed by its done event before the terminal (review-08
	// blocker 3).
	if _, err := state.Convert(ResponseOutputItemDoneEvent{
		EventBase: EventBase{
			Type:           "response.output_item.done",
			SequenceNumber: 2,
		},
		OutputIndex: 0,
		Item: &ResponsesOutputMessage{
			ID:      "msg_1",
			Type:    "message",
			Role:    "assistant",
			Status:  ResponsesItemCompleted,
			Phase:   "commentary",
			Content: ResponsesOutputContentParts{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseCompletedEvent{
		EventBase: EventBase{
			Type:           "response.completed",
			SequenceNumber: 3,
		},
		Response: envelope,
	}); err != nil {
		t.Fatal(err)
	}
	if count := countFeature(state.report, FeatureOutputPhase); count != 1 {
		t.Fatalf("phase losses = %d, want exactly one per item", count)
	}
}

// TestInputMessagePhaseGate proves input-message phases are loss-gated at
// decode (review-j finding 10).
func TestInputMessagePhaseGate(t *testing.T) {
	body := `{"model":"m","input":[{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"input_text","text":"thinking"}]}]}`
	if _, _, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy()); err == nil {
		t.Fatal("strict policy accepted an input message phase")
	}
	result, _, err := DecodeResponsesRequest([]byte(body), LossPolicy{Allowed: map[Feature]struct{}{
		FeatureOutputPhase: {},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if count := countFeature(result.Report, FeatureOutputPhase); count != 1 {
		t.Fatalf("phase losses = %d, want one", count)
	}
}

// responseHasPhase reports whether any output message item carries a phase.
func responseHasPhase(response CanonicalResponse) bool {
	for _, item := range response.Items {
		if message, ok := item.(*CanonicalMessageItem); ok && message.Phase.Set {
			return true
		}
	}
	return false
}

// countFeature counts the report records for a feature.
func countFeature(report ConversionReport, feature Feature) int {
	count := 0
	for _, loss := range report.Losses {
		if loss.Feature == feature {
			count++
		}
	}
	return count
}

// TestResponseRenderReportSurfaced proves the response renderers return the
// accumulated report (conversation-state and reasoning losses flow through)
// (review-j finding 10).
func TestResponseRenderReportSurfaced(t *testing.T) {
	source := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"},{"id":"fo_1","type":"function_call_output","call_id":"call_1","output":"{\"temp\":21}"},{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]}`
	response, err := DecodeResponsesResponse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureOutputItemBoundaries:   {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	context.RequestedClientModel = "m"
	_, report, err := RenderMessagesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	if !reportHasFeature(report, FeatureOutputItemBoundaries) {
		t.Fatalf("report lacks the conversation-state loss: %+v", report)
	}
}

// reportHasFeature reports whether the report contains a loss or note for
// the feature.
func reportHasFeature(report ConversionReport, feature Feature) bool {
	for _, loss := range report.Losses {
		if loss.Feature == feature {
			return true
		}
	}
	return false
}
