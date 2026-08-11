package transcode

// J6 regression tests (review-k finding 6, high): unknown usage breakdowns
// are never emitted as factual zeros — the Responses and Messages renderers
// loss-gate every wire-required component the source did not provide and
// emit the required zeros only after the loss is approved, streaming behaves
// identically, and known totals are preserved.

import (
	"strings"
	"testing"
)

// usageCounterexample is the review-k finding-6 fixture: all three totals
// present, no breakdown detail objects.
const usageCounterexample = `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`

// TestUsageCounterexampleResponsesLossGated proves the review-k
// counterexample renders to a Responses client only after the explicit
// usage-timing loss: the pinned Responses contract requires the breakdown
// detail objects, so strict policy rejects and under an approved loss the
// zeros are emitted with the loss recorded exactly once (review-k finding
// 6). The known totals are preserved.
func TestUsageCounterexampleResponsesLossGated(t *testing.T) {
	response, err := DecodeChatResponse([]byte(usageCounterexample), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Usage.InputKnown || !response.Usage.OutputKnown || !response.Usage.TotalKnown {
		t.Fatalf("usage = %+v, want known totals", response.Usage)
	}
	if response.Usage.CacheReadKnown || response.Usage.ReasoningKnown {
		t.Fatalf("usage = %+v, want unknown breakdowns", response.Usage)
	}
	if _, _, err := RenderResponsesResponse(response, testExchangeContext()); err == nil {
		t.Fatal("strict policy accepted unknown usage breakdowns")
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	rendered, report, err := RenderResponsesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	if count := countFeature(report, FeatureUsageCacheReadUnknown) +
		countFeature(report, FeatureUsageReasoningUnknown); count != 2 {
		t.Fatalf("usage component losses = %d, want exactly one per unknown component", count)
	}
	body := string(rendered)
	if !strings.Contains(body, `"input_tokens":10`) ||
		!strings.Contains(body, `"output_tokens":2`) ||
		!strings.Contains(body, `"total_tokens":12`) {
		t.Fatalf("known totals lost: %s", body)
	}
	if !strings.Contains(body, `"cached_tokens":0`) ||
		!strings.Contains(body, `"reasoning_tokens":0`) {
		t.Fatalf("required breakdown fields missing from the approved render: %s", body)
	}
}

// TestUsageCounterexampleMessagesLossGated proves the same source renders to
// a Messages client only after the explicit usage-timing loss: strict policy
// rejects, and under an approved loss the breakdown zeros are emitted with
// the loss recorded exactly once.
func TestUsageCounterexampleMessagesLossGated(t *testing.T) {
	response, err := DecodeChatResponse([]byte(usageCounterexample), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := RenderMessagesResponse(response, testExchangeContext()); err == nil {
		t.Fatal("strict policy accepted unknown usage breakdowns")
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	rendered, report, err := RenderMessagesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	if count := countFeature(report, FeatureUsageCacheReadUnknown) +
		countFeature(report, FeatureUsageCacheWriteUnknown) +
		countFeature(report, FeatureUsageReasoningUnknown); count != 3 {
		t.Fatalf("usage component losses = %d, want exactly one per unknown component", count)
	}
	body := string(rendered)
	if !strings.Contains(body, `"cache_creation_input_tokens":0`) ||
		!strings.Contains(body, `"cache_read_input_tokens":0`) {
		t.Fatalf("messages usage breakdown missing: %s", body)
	}
	if !strings.Contains(body, `"thinking_tokens":0`) {
		t.Fatalf("thinking breakdown missing: %s", body)
	}
	if !strings.Contains(body, `"input_tokens":10`) ||
		!strings.Contains(body, `"output_tokens":2`) {
		t.Fatalf("known totals lost: %s", body)
	}
}

// TestUsagePartialPreservesKnownTotals proves partial usage (input and total
// present, output absent) preserves the known totals and loss-gates the
// unknown output total on the Responses target.
func TestUsagePartialPreservesKnownTotals(t *testing.T) {
	body := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"total_tokens":12}}`
	response, err := DecodeChatResponse([]byte(body), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Usage.InputKnown || response.Usage.OutputKnown || !response.Usage.TotalKnown {
		t.Fatalf("usage = %+v, want input and total known, output unknown", response.Usage)
	}
	if _, _, err := RenderResponsesResponse(response, testExchangeContext()); err == nil {
		t.Fatal("strict policy accepted the unknown output total")
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	rendered, report, err := RenderResponsesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	if count := countFeature(report, FeatureUsageUnknown) +
		countFeature(report, FeatureUsageCacheReadUnknown) +
		countFeature(report, FeatureUsageReasoningUnknown); count != 3 {
		t.Fatalf("usage component losses = %d, want exactly one per unknown component", count)
	}
	emitted := string(rendered)
	if !strings.Contains(emitted, `"input_tokens":10`) ||
		!strings.Contains(emitted, `"total_tokens":12`) {
		t.Fatalf("known totals lost: %s", emitted)
	}

	// Total-only usage: both part totals unknown, the source total known.
	// The known total is preserved; the required part totals are
	// loss-gated.
	totalOnly := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"total_tokens":12}}`
	response, err = DecodeChatResponse([]byte(totalOnly), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.Unknown() {
		t.Fatal("total-only usage must not be entirely unknown")
	}
	if response.Usage.InputKnown || response.Usage.OutputKnown || !response.Usage.TotalKnown {
		t.Fatalf("usage = %+v, want only the total known", response.Usage)
	}
	if _, _, err := RenderResponsesResponse(response, testExchangeContext()); err == nil {
		t.Fatal("strict policy accepted unknown part totals")
	}
	context = testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	rendered, _, err = RenderResponsesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), `"total_tokens":12`) {
		t.Fatalf("known total lost: %s", rendered)
	}
}

// TestUsageStreamingDetailObjectsPresence proves the stream chunk usage
// converter always emits the pinned-required detail objects — the loss gate
// at the call sites sanctions the zeros (review-k finding 6) — and reflects
// provided breakdowns.
func TestUsageStreamingDetailObjectsPresence(t *testing.T) {
	usage, err := chatUsageToResponsesUsage(&ChatLLMUsage{
		PromptTokens:     10,
		CompletionTokens: 2,
		TotalTokens:      12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokensDetails == nil || usage.OutputTokensDetails == nil {
		t.Fatalf("required detail objects missing: %+v", usage)
	}
	if usage.InputTokensDetails.CachedTokens != 0 || usage.OutputTokensDetails.ReasoningTokens != 0 {
		t.Fatalf("unknown breakdowns invented: %+v", usage)
	}
	usage, err = chatUsageToResponsesUsage(&ChatLLMUsage{
		PromptTokens:     10,
		CompletionTokens: 2,
		TotalTokens:      12,
		PromptTokensDetails: &ChatPromptTokensDetails{
			CachedTokens: 3,
		},
		CompletionTokensDetails: &ChatCompletionTokensDetails{
			ReasoningTokens: 4,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokensDetails == nil || usage.InputTokensDetails.CachedTokens != 3 {
		t.Fatalf("cached = %+v", usage.InputTokensDetails)
	}
	if usage.OutputTokensDetails == nil || usage.OutputTokensDetails.ReasoningTokens != 4 {
		t.Fatalf("reasoning = %+v", usage.OutputTokensDetails)
	}
}

// TestUsageStreamingChatToResponsesLossGatedOnce proves the chat→responses
// stream gate: a chunk usage without the breakdown details rejects under the
// strict policy and converts under an approved usage_timing loss recorded
// exactly once per stream (review-k finding 6).
func TestUsageStreamingChatToResponsesLossGatedOnce(t *testing.T) {
	chunk := chatChunk(t, ChatStreamDelta{Content: str("x")}, nil)
	chunk.Usage = &ChatLLMUsage{
		PromptTokens:     10,
		CompletionTokens: 2,
		TotalTokens:      12,
	}

	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	if _, err := state.Convert(chunk); err == nil {
		t.Fatal("strict policy accepted unknown usage breakdowns")
	}

	state = newChatResponsesStreamState(
		testStreamContext(),
		LossPolicy{Allowed: map[Feature]struct{}{
			FeatureUsageCacheReadUnknown:  {},
			FeatureUsageCacheWriteUnknown: {},
			FeatureUsageReasoningUnknown:  {},
			FeatureUsageUnknown:           {},
		}},
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	if _, err := state.Convert(chunk); err != nil {
		t.Fatal(err)
	}
	tail := chatChunk(t, ChatStreamDelta{}, nil)
	tail.Usage = &ChatLLMUsage{
		PromptTokens:     11,
		CompletionTokens: 3,
		TotalTokens:      14,
	}
	if _, err := state.Convert(tail); err != nil {
		t.Fatal(err)
	}
	if count := countFeature(state.report, FeatureUsageCacheReadUnknown) +
		countFeature(state.report, FeatureUsageReasoningUnknown); count != 2 {
		t.Fatalf("usage component losses = %d, want exactly one per unknown component", count)
	}
}

// usageEnvelope returns a completed-envelope usage source with both detail
// objects present.
func usageEnvelope() *ResponsesUsage {
	return &ResponsesUsage{
		InputTokens:         10,
		OutputTokens:        2,
		TotalTokens:         12,
		InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 3},
		OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 4},
	}
}

// TestUsageStreamingResponsesToAnthropicLossGatedOnce proves the Responses→
// Anthropic stream enters the usage-timing loss for the always-unknown
// cache-creation component — strict rejects, permissive converts with the
// loss recorded exactly once across the whole stream (review-k finding 6).
func TestUsageStreamingResponsesToAnthropicLossGatedOnce(t *testing.T) {
	created := ResponseEnvelope{
		ID:        "resp_1",
		Object:    "response",
		CreatedAt: 1,
		Status:    "in_progress",
		Model:     "m",
		Output:    []ResponsesOutputItem{},
		Usage:     usageEnvelope(),
	}
	completed := created
	completed.Status = "completed"

	// Strict: the created envelope's message_start rejects.
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"msg_1",
		"m",
		1,
	)
	if _, err := state.Convert(ResponseCreatedEvent{
		EventBase: EventBase{Type: "response.created", SequenceNumber: 1},
		Response:  created,
	}); err == nil {
		t.Fatal("strict policy accepted the unknown cache-creation breakdown")
	}

	// Permissive: the loss is recorded exactly once, though both
	// message_start and the completed envelope convert usage.
	state = newAnthropicResponsesStreamState(
		testStreamContext(),
		LossPolicy{Allowed: map[Feature]struct{}{
			FeatureUsageCacheReadUnknown:  {},
			FeatureUsageCacheWriteUnknown: {},
			FeatureUsageReasoningUnknown:  {},
			FeatureUsageUnknown:           {},
		}},
		"msg_1",
		"m",
		1,
	)
	if _, err := state.Convert(ResponseCreatedEvent{
		EventBase: EventBase{Type: "response.created", SequenceNumber: 1},
		Response:  created,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseCompletedEvent{
		EventBase: EventBase{Type: "response.completed", SequenceNumber: 2},
		Response:  completed,
	}); err != nil {
		t.Fatal(err)
	}
	// The source provides both detail objects; only cache-creation tokens
	// are never part of the pinned Responses contract, so exactly one
	// component loss is recorded.
	if count := countFeature(state.report, FeatureUsageCacheWriteUnknown); count != 1 {
		t.Fatalf("usage component losses = %d, want exactly one", count)
	}
}
