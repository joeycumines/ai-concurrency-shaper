package transcode

// Regression tests: Responses→Messages stream identity, reasoning loss,
// stop nullability, and usage.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// TestAnthropicStreamPartBlockIdentity proves two message items with the same
// content_index 0 target their own Anthropic blocks — no aliasing
func TestAnthropicStreamPartBlockIdentity(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		permissiveLossPolicy(),
		ChatCapabilities{},
		"msg_1",
		"claude-x",
		1,
	)

	addPart := func(itemID string, outputIndex, sequence int64) {
		t.Helper()
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type:           "response.content_part.added",
			SequenceNumber: sequence,
			ItemID:         itemID,
			OutputIndex:    outputIndex,
			ContentIndex:   0,
			Part: &ResponsesStreamOutputTextPart{
				Type:        "output_text",
				Text:        "",
				Annotations: []ResponsesAnnotation{},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The lifecycle: created, then both message items, then their parts.
	feedAnthropicCreated(t, state, 0)
	addItem := func(itemID string, outputIndex, sequence int64) {
		t.Helper()
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type:           "response.output_item.added",
			SequenceNumber: sequence,
			OutputIndex:    outputIndex,
			Item: &ResponsesOutputMessage{
				ID:      itemID,
				Type:    "message",
				Role:    "assistant",
				Status:  ResponsesItemInProgress,
				Content: ResponsesOutputContentParts{},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	addItem("item_a", 0, 1)
	addItem("item_b", 1, 2)
	addPart("item_a", 0, 3)

	// A delta for item_a content_index 0 must target item_a's block (0).
	events, err := state.Convert(ResponseTextDeltaEvent{
		Type:           "response.output_text.delta",
		SequenceNumber: 4,
		ItemID:         "item_a",
		OutputIndex:    0,
		ContentIndex:   0,
		Delta:          "hello",
		Logprobs:       []ResponsesTextLogprob{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Index == nil || *events[0].Index != 0 {
		t.Fatalf("item_a delta targets %v, want block 0", events)
	}

	// While item_a's block is still open, a second part (item_b's) must be
	// rejected: the Messages dialect carries exactly one open content block.
	_, err = state.Convert(ResponseContentPartAddedEvent{
		Type:           "response.content_part.added",
		SequenceNumber: 5,
		ItemID:         "item_b",
		OutputIndex:    1,
		ContentIndex:   0,
		Part: &ResponsesStreamOutputTextPart{
			Type:        "output_text",
			Text:        "",
			Annotations: []ResponsesAnnotation{},
		},
	})
	assertAnthropicWireError(t, err, "while another content block is open")

	// The state latches the failure, so the continuation replays the same
	// prefix on a fresh state (the converter-level rollback path is covered
	// by the composed-converter tests). After item_a's part closes, item_b's
	// part opens its own block (1), and its deltas target it — never item_a's
	// closed block.
	state2 := newAnthropicResponsesStreamState(
		testStreamContext(),
		permissiveLossPolicy(),
		ChatCapabilities{},
		"msg_1",
		"claude-x",
		1,
	)
	feedAnthropicCreated(t, state2, 0)
	addItem2 := func(itemID string, outputIndex, sequence int64) {
		t.Helper()
		if _, err := state2.Convert(ResponseOutputItemAddedEvent{
			Type:           "response.output_item.added",
			SequenceNumber: sequence,
			OutputIndex:    outputIndex,
			Item: &ResponsesOutputMessage{
				ID:      itemID,
				Type:    "message",
				Role:    "assistant",
				Status:  ResponsesItemInProgress,
				Content: ResponsesOutputContentParts{},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	addItem2("item_a", 0, 1)
	addItem2("item_b", 1, 2)
	if _, err := state2.Convert(ResponseContentPartAddedEvent{
		Type:           "response.content_part.added",
		SequenceNumber: 3,
		ItemID:         "item_a",
		OutputIndex:    0,
		ContentIndex:   0,
		Part:           &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state2.Convert(ResponseTextDeltaEvent{
		Type:           "response.output_text.delta",
		SequenceNumber: 4,
		ItemID:         "item_a",
		OutputIndex:    0,
		ContentIndex:   0,
		Delta:          "hello",
		Logprobs:       []ResponsesTextLogprob{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state2.Convert(ResponseTextDoneEvent{
		Type:           "response.output_text.done",
		SequenceNumber: 5,
		ItemID:         "item_a",
		OutputIndex:    0,
		ContentIndex:   0,
		Text:           "hello",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state2.Convert(ResponseContentPartDoneEvent{
		Type:           "response.content_part.done",
		SequenceNumber: 6,
		ItemID:         "item_a",
		OutputIndex:    0,
		ContentIndex:   0,
		Part:           &ResponsesStreamOutputTextPart{Type: "output_text", Text: "hello", Annotations: []ResponsesAnnotation{}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state2.Convert(ResponseContentPartAddedEvent{
		Type:           "response.content_part.added",
		SequenceNumber: 7,
		ItemID:         "item_b",
		OutputIndex:    1,
		ContentIndex:   0,
		Part:           &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
	}); err != nil {
		t.Fatal(err)
	}
	events, err = state2.Convert(ResponseTextDeltaEvent{
		Type:           "response.output_text.delta",
		SequenceNumber: 8,
		ItemID:         "item_b",
		OutputIndex:    1,
		ContentIndex:   0,
		Delta:          "world",
		Logprobs:       []ResponsesTextLogprob{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Index == nil || *events[0].Index != 1 {
		t.Fatalf("item_b delta targets %v, want block 1", events)
	}
}

// TestAnthropicStreamReasoningLossExactlyOnce proves reasoning events enter
// the loss decision exactly once: strict rejects, permissive converts with a
// single recorded loss.
func TestAnthropicStreamReasoningLossExactlyOnce(t *testing.T) {
	// Strict: the first reasoning event rejects the stream. The policy
	// approves the unavoidable usage-timing loss (cache_creation is never
	// part of the pinned Responses contract) so the created envelope can be
	// fed, but rejects reasoning.
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		LossPolicy{Allowed: map[Feature]struct{}{
			FeatureUsageCacheReadUnknown:  {},
			FeatureUsageCacheWriteUnknown: {},
			FeatureUsageReasoningUnknown:  {},
			FeatureUsageUnknown:           {},
		}},
		ChatCapabilities{},
		"msg_1",
		"claude-x",
		1,
	)
	if _, err := state.Convert(ResponseCreatedEvent{
		Type: "response.created", SequenceNumber: 0,
		Response: ResponseEnvelope{
			ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
			Output: []ResponsesOutputItem{},
			Usage: &ResponsesUsage{
				InputTokens: 0, OutputTokens: 0, TotalTokens: 0,
				InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
				OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := state.Convert(ResponseReasoningSummaryPartAddedEvent{
		Type:           "response.reasoning_summary_part.added",
		SequenceNumber: 1,
		ItemID:         "rs_1",
		OutputIndex:    0,
		SummaryIndex:   0,
		Part:           ResponsesSummaryTextPart{Type: "summary_text", Text: ""},
	})
	if err == nil {
		t.Fatal("strict policy accepted a reasoning event")
	}
	var unsupported *UnsupportedFeatureError
	if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureReasoningSummary) {
		t.Fatalf("error = %v, want reasoning_summary unsupported feature", err)
	}

	// Permissive: exactly one loss recorded across all four event types.
	state = newAnthropicResponsesStreamState(
		testStreamContext(),
		permissiveLossPolicy(),
		ChatCapabilities{},
		"msg_1",
		"claude-x",
		1,
	)
	sequence := int64(0)
	feed := func(event ResponsesSSEEvent) {
		t.Helper()
		if _, err := state.Convert(event); err != nil {
			t.Fatal(err)
		}
	}
	feed(ResponseCreatedEvent{
		Type: "response.created", SequenceNumber: 0,
		Response: ResponseEnvelope{
			ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
			Output: []ResponsesOutputItem{},
			// The created envelope carries usage so the early-usage loss is
			// not part of this test's single-loss accounting.
			Usage: &ResponsesUsage{
				InputTokens: 0, OutputTokens: 0, TotalTokens: 0,
				InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
				OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
			},
		},
	})
	sequence = 1
	base := func(typ string) EventBase {
		seq := sequence
		sequence++
		return EventBase{Type: typ, SequenceNumber: seq}
	}
	feed(ResponseReasoningSummaryPartAddedEvent{
		EventBase:    base("response.reasoning_summary_part.added"),
		ItemID:       "rs_1",
		OutputIndex:  0,
		SummaryIndex: 0,
		Part:         ResponsesSummaryTextPart{Type: "summary_text", Text: ""},
	})
	feed(ResponseReasoningSummaryTextDeltaEvent{
		EventBase:    base("response.reasoning_summary_text.delta"),
		ItemID:       "rs_1",
		OutputIndex:  0,
		SummaryIndex: 0,
		Delta:        "reasoning",
	})
	feed(ResponseReasoningSummaryTextDoneEvent{
		EventBase:    base("response.reasoning_summary_text.done"),
		ItemID:       "rs_1",
		OutputIndex:  0,
		SummaryIndex: 0,
		Text:         "reasoning",
	})
	feed(ResponseReasoningSummaryPartDoneEvent{
		EventBase:    base("response.reasoning_summary_part.done"),
		ItemID:       "rs_1",
		OutputIndex:  0,
		SummaryIndex: 0,
		Part:         ResponsesSummaryTextPart{Type: "summary_text", Text: "reasoning"},
	})
	if count := countFeature(state.report, FeatureReasoningSummary); count != 1 {
		t.Fatalf("reasoning losses = %d, want exactly one", count)
	}
}

// TestAnthropicStreamMessageStartNullStopFields proves message_start
// serializes stop_reason: null and stop_sequence: null, and the stop reason
// appears only in message_delta.
func TestAnthropicStreamMessageStartNullStopFields(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		permissiveLossPolicy(),
		ChatCapabilities{},
		"msg_1",
		"claude-x",
		1,
	)
	events, err := state.Convert(ResponseCreatedEvent{
		Type:           "response.created",
		SequenceNumber: 1,
		Response: ResponseEnvelope{
			ID:        "resp_1",
			Object:    "response",
			CreatedAt: 1,
			Status:    "in_progress",
			Model:     "m",
			Output:    []ResponsesOutputItem{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != AnthropicStreamEventTypeMessageStart {
		t.Fatalf("events = %+v", events)
	}
	data, err := json.Marshal(events[0])
	if err != nil {
		t.Fatal(err)
	}
	raw := string(data)
	if !strings.Contains(raw, `"stop_reason":null`) {
		t.Fatalf("message_start must serialize stop_reason null: %s", raw)
	}
	if !strings.Contains(raw, `"stop_sequence":null`) {
		t.Fatalf("message_start must serialize stop_sequence null: %s", raw)
	}

	// The terminal message_delta carries the real stop reason. No output
	// items were observed, so the terminal envelope carries none.
	if _, err := state.Convert(ResponseCompletedEvent{
		Type:           "response.completed",
		SequenceNumber: 2,
		Response: ResponseEnvelope{
			ID:        "resp_1",
			Object:    "response",
			CreatedAt: 1,
			Status:    "completed",
			Model:     "m",
			Output:    []ResponsesOutputItem{},
			Usage: &ResponsesUsage{
				InputTokens:         10,
				OutputTokens:        5,
				TotalTokens:         15,
				InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
				OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	terminal, err := state.terminalEvents(CanonicalStopEndTurn)
	if err != nil {
		t.Fatal(err)
	}
	var delta *AnthropicStreamEvent
	for _, event := range terminal {
		if event.Type == AnthropicStreamEventTypeMessageDelta {
			delta = &event
		}
	}
	if delta == nil || delta.Delta == nil || delta.Delta.StopReason == nil ||
		*delta.Delta.StopReason != AnthropicStopReasonEndTurn {
		t.Fatalf("message_delta stop reason = %+v", delta)
	}
}

// TestAnthropicUsageUncachedArithmetic proves the Responses→Anthropic usage
// conversion: uncached input = total - cache read, never a double count, with
// checked nonnegative arithmetic.
func TestAnthropicUsageUncachedArithmetic(t *testing.T) {
	usage := &ResponsesUsage{
		InputTokens:  45,
		OutputTokens: 25,
		TotalTokens:  70,
		InputTokensDetails: &UsageInputTokensDetails{
			CachedTokens: 5,
		},
		OutputTokensDetails: &UsageOutputTokensDetails{
			ReasoningTokens: 12,
		},
	}
	converted, clamp, err := responsesUsageToAnthropicUsage(usage, responsesUsagePresence(usage))
	if err != nil {
		t.Fatal(err)
	}
	if !clamp.empty() {
		t.Fatalf("consistent usage clamped: %+v", clamp)
	}
	// Anthropic total: 40 (uncached) + 5 (cache read) = 45 — the source
	// total, never 45 + 5.
	if converted.InputTokens != 40 || converted.CacheReadInputTokens != 5 {
		t.Fatalf("usage = %+v, want input 40 cache_read 5", converted)
	}
	if converted.OutputTokens != 25 || converted.OutputTokensDetails.ThinkingTokens != 12 {
		t.Fatalf("usage = %+v", converted)
	}

	// An arithmetically inconsistent source is CLAMPED, never a failed
	// exchange: the cached breakdown is bounded by
	// the input total and the Anthropic identity (uncached + cache-read +
	// cache-creation = source total) holds.
	bad := &ResponsesUsage{
		InputTokens: 3,
		TotalTokens: 3,
		InputTokensDetails: &UsageInputTokensDetails{
			CachedTokens: 10,
		},
	}
	converted, clamp, err = responsesUsageToAnthropicUsage(bad, responsesUsagePresence(bad))
	if err != nil {
		t.Fatalf("cache-exceeds-input must clamp, not reject: %v", err)
	}
	if !clamp.cacheExceedsInput || clamp.negativeCounts {
		t.Fatalf("clamp = %+v, want cache-exceeds-input only", clamp)
	}
	if converted.InputTokens != 0 || converted.CacheReadInputTokens != 3 || converted.CacheCreationInputTokens != 0 {
		t.Fatalf("clamped usage = %+v, want input 0 cache_read 3 cache_creation 0", converted)
	}
	bad = &ResponsesUsage{
		InputTokens: -5,
		TotalTokens: -5,
		InputTokensDetails: &UsageInputTokensDetails{
			CachedTokens: -10,
		},
	}
	converted, clamp, err = responsesUsageToAnthropicUsage(bad, responsesUsagePresence(bad))
	if err != nil {
		t.Fatalf("negative counts must clamp, not reject: %v", err)
	}
	if !clamp.negativeCounts {
		t.Fatalf("clamp = %+v, want negative counts recorded", clamp)
	}
	if converted.InputTokens != 0 || converted.CacheReadInputTokens != 0 || converted.OutputTokens != 0 {
		t.Fatalf("clamped usage = %+v, want all-zero counts", converted)
	}

	// Nil usage: unknown, never fabricated zeros.
	converted, clamp, err = responsesUsageToAnthropicUsage(nil, responsesUsagePresence(nil))
	if err != nil {
		t.Fatal(err)
	}
	if converted != nil || !clamp.empty() {
		t.Fatalf("nil usage converted to %+v (clamp %+v)", converted, clamp)
	}
}

// TestAnthropicUsageStreamNonStreamAgree proves the stream and non-stream
// conversions produce identical Anthropic usage for the same source
func TestAnthropicUsageStreamNonStreamAgree(t *testing.T) {
	source := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":45,"input_tokens_details":{"cached_tokens":5},"output_tokens":25,"output_tokens_details":{"reasoning_tokens":12},"total_tokens":70}}`

	// Non-stream path.
	response, err := DecodeResponsesResponse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.LossPolicy = permissiveLossPolicy()
	context.RequestedClientModel = "m"
	rendered, _, err := RenderMessagesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	var nonStream AnthropicMessageResponse
	if err := json.Unmarshal(rendered, &nonStream); err != nil {
		t.Fatal(err)
	}
	if nonStream.Usage == nil {
		t.Fatal("non-stream usage is nil")
	}

	// Stream path.
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		permissiveLossPolicy(),
		ChatCapabilities{},
		"msg_1",
		"claude-x",
		1,
	)
	if _, err := state.Convert(ResponseCreatedEvent{
		Type:           "response.created",
		SequenceNumber: 0,
		Response: ResponseEnvelope{
			ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
			Output: []ResponsesOutputItem{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	events, err := state.Convert(ResponseCompletedEvent{
		Type:           "response.completed",
		SequenceNumber: 1,
		Response: ResponseEnvelope{
			ID:        "resp_1",
			Object:    "response",
			CreatedAt: 1,
			Status:    "completed",
			Model:     "m",
			Output:    []ResponsesOutputItem{},
			Usage: &ResponsesUsage{
				InputTokens:  45,
				OutputTokens: 25,
				TotalTokens:  70,
				InputTokensDetails: &UsageInputTokensDetails{
					CachedTokens: 5,
				},
				OutputTokensDetails: &UsageOutputTokensDetails{
					ReasoningTokens: 12,
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = events
	if state.usage == nil {
		t.Fatal("stream usage is nil")
	}

	nonStreamJSON, _ := json.Marshal(nonStream.Usage)
	streamJSON, _ := json.Marshal(state.usage)
	if string(nonStreamJSON) != string(streamJSON) {
		t.Fatalf("stream and non-stream usage disagree: %s vs %s", nonStreamJSON, streamJSON)
	}
}

// TestAnthropicNonStreamUnknownUsageLoss proves the non-stream Messages
// render with unknown usage enters the loss decision instead of fabricating
// zeros.
func TestAnthropicNonStreamUnknownUsageLoss(t *testing.T) {
	source := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}]}`
	response, err := DecodeResponsesResponse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if !response.Usage.Unknown() {
		t.Fatal("usage should be unknown")
	}
	strictContext := testExchangeContext()
	strictContext.RequestedClientModel = "m"
	if _, _, err := RenderMessagesResponse(response, strictContext); err == nil {
		t.Fatal("strict policy accepted unknown usage")
	}
	permissiveContext := testExchangeContext()
	permissiveContext.LossPolicy = permissiveLossPolicy()
	permissiveContext.RequestedClientModel = "m"
	rendered, _, err := RenderMessagesResponse(response, permissiveContext)
	if err != nil {
		t.Fatal(err)
	}
	var message AnthropicMessageResponse
	if err := json.Unmarshal(rendered, &message); err != nil {
		t.Fatal(err)
	}
	if message.Usage == nil {
		t.Fatal("required Messages usage omitted")
	}
}

// anthropicThinkingLifecycleState builds a fresh state with the
// provider_reasoning_thinking capability so reasoning events render as
// native thinking blocks.
func anthropicThinkingLifecycleState(t *testing.T) *anthropicResponsesStreamState {
	t.Helper()
	return newAnthropicResponsesStreamState(
		testStreamContext(),
		permissiveLossPolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"msg_1",
		"claude-x",
		1710000000,
	)
}

// reasoningTextDelta must charge the event budget once (convertEvent already
// did) and bound the accumulated thinking text per item.
// reasoningTextDelta must charge the event budget once (convertEvent already
// did) and bound the accumulated thinking text per item.
func TestAnthropicStreamReasoningDeltaChargedOnceAndBounded(t *testing.T) {
	state := anthropicThinkingLifecycleState(t)
	feedAnthropicCreated(t, state, 0)
	if _, err := state.Convert(ResponseOutputItemAddedEvent{
		Type: "response.output_item.added", SequenceNumber: 1, OutputIndex: 0,
		Item: &ResponsesReasoningOutputItem{ID: "rs_1", Type: "reasoning", Status: ResponsesItemInProgress},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseReasoningSummaryPartAddedEvent{
		Type: "response.reasoning_summary_part.added", SequenceNumber: 2,
		ItemID: "rs_1", OutputIndex: 0, SummaryIndex: 0,
		Part: openairesponses.SummaryTextPart{Type: "summary_text", Text: ""},
	}); err != nil {
		t.Fatal(err)
	}
	// Cumulative 64KiB deltas stay under the 4MiB exchange total for 16
	// deltas, so only the per-item 1MiB bound can fire — and it must name
	// thinking text.
	chunk := strings.Repeat("y", 64<<10)
	var err error
	for seq := int64(3); err == nil && seq < 3+17; seq++ {
		_, err = state.Convert(ResponseReasoningSummaryTextDeltaEvent{
			Type: "response.reasoning_summary_text.delta", SequenceNumber: seq,
			ItemID: "rs_1", OutputIndex: 0, SummaryIndex: 0, Delta: chunk,
		})
	}
	if err == nil {
		t.Fatal("unbounded thinking text accepted (17 x 64KiB > 1MiB per-item bound)")
	}
	if !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("error = %q, want it to name thinking text (per-item accumulated bound)", err.Error())
	}
	var wireErr *UpstreamWireError
	if _, ok := asUpstreamWire(err, &wireErr); !ok {
		t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
	}

	// The event budget is charged exactly once per event: a stream of N
	// reasoning deltas must accept exactly N events before the bound, i.e.
	// the budget error must not fire at half the count. Drive a fresh state
	// with tiny deltas up to the event budget and count accepted events.
	state2 := anthropicThinkingLifecycleState(t)
	feedAnthropicCreated(t, state2, 0)
	if _, err := state2.Convert(ResponseOutputItemAddedEvent{
		Type: "response.output_item.added", SequenceNumber: 1, OutputIndex: 0,
		Item: &ResponsesReasoningOutputItem{ID: "rs_1", Type: "reasoning", Status: ResponsesItemInProgress},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state2.Convert(ResponseReasoningSummaryPartAddedEvent{
		Type: "response.reasoning_summary_part.added", SequenceNumber: 2,
		ItemID: "rs_1", OutputIndex: 0, SummaryIndex: 0,
		Part: openairesponses.SummaryTextPart{Type: "summary_text", Text: ""},
	}); err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for seq := int64(3); ; seq++ {
		_, err := state2.Convert(ResponseReasoningSummaryTextDeltaEvent{
			Type: "response.reasoning_summary_text.delta", SequenceNumber: seq,
			ItemID: "rs_1", OutputIndex: 0, SummaryIndex: 0, Delta: "z",
		})
		if err != nil {
			break
		}
		accepted++
	}
	// 3 events were consumed before the deltas (created, item added, part
	// added); the budget is maxStreamTotalEvents, so a double-charged delta
	// path accepts only about half of the remaining events.
	want := maxStreamTotalEvents - 3
	if accepted != want {
		t.Fatalf("accepted %d reasoning deltas, want %d (event budget charged once per event; double charge would halve it)", accepted, want)
	}
}

func summaryTextPart(text string) ResponsesSummaryTextPart {
	return ResponsesSummaryTextPart{Type: "summary_text", Text: text}
}

// reasoningLifecycleState builds an anthropic state with the thinking
// capability on and drives it through created -> reasoning item added ->
// summary part added -> summary text delta -> summary text done, returning
// with the thinking block open.
func reasoningLifecycleState(t *testing.T) *anthropicResponsesStreamState {
	t.Helper()
	return reasoningLifecycleStateAt(t, 0, "think")
}

// reasoningLifecycleStateAt is reasoningLifecycleState with a chosen summary
// index and text, for probing non-conforming upstreams.
func reasoningLifecycleStateAt(t *testing.T, summaryIndex int64, text string) *anthropicResponsesStreamState {
	t.Helper()
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		permissiveLossPolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"msg_1",
		"claude-x",
		1,
	)
	created := ResponseCreatedEvent{
		Type: "response.created", SequenceNumber: 0,
		Response: ResponseEnvelope{
			ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
			Output: []ResponsesOutputItem{},
			Usage: &ResponsesUsage{
				InputTokens: 0, OutputTokens: 0, TotalTokens: 0,
				InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
				OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
			},
		},
	}
	if _, err := state.Convert(created); err != nil {
		t.Fatalf("created: %v", err)
	}
	if _, err := state.Convert(ResponseOutputItemAddedEvent{
		Type: "response.output_item.added", SequenceNumber: 1,
		OutputIndex: 0,
		Item:        &ResponsesReasoningOutputItem{ID: "rs_1", Type: "reasoning", Status: ResponsesItemInProgress, Summary: []ResponsesReasoningSummary{}},
	}); err != nil {
		t.Fatalf("reasoning item added: %v", err)
	}
	if _, err := state.Convert(ResponseReasoningSummaryPartAddedEvent{
		Type: "response.reasoning_summary_part.added", SequenceNumber: 2,
		ItemID:       "rs_1",
		OutputIndex:  0,
		SummaryIndex: summaryIndex,
		Part:         summaryTextPart(""),
	}); err != nil {
		t.Fatalf("summary part added: %v", err)
	}
	if _, err := state.Convert(ResponseReasoningSummaryTextDeltaEvent{
		Type: "response.reasoning_summary_text.delta", SequenceNumber: 3,
		ItemID:       "rs_1",
		OutputIndex:  0,
		SummaryIndex: summaryIndex,
		Delta:        text,
	}); err != nil {
		t.Fatalf("summary text delta: %v", err)
	}
	if _, err := state.Convert(ResponseReasoningSummaryTextDoneEvent{
		Type: "response.reasoning_summary_text.done", SequenceNumber: 4,
		ItemID:       "rs_1",
		OutputIndex:  0,
		SummaryIndex: summaryIndex,
		Text:         text,
	}); err != nil {
		t.Fatalf("summary text done: %v", err)
	}
	return state
}

// TestReasoningPartDoneRequiresOpenThinkingItem verifies the
// openThinkingItem guard: a part-done for an item other than the open
// thinking block's own item is corrupt wire, not a seal of the wrong block,
// and the open block survives the rejection.
func TestReasoningPartDoneRequiresOpenThinkingItem(t *testing.T) {
	state := reasoningLifecycleState(t)
	// Call the render-side method directly with a foreign item id: the
	// openThinkingItem guard must reject it as corrupt wire, leave the open
	// block untouched, and produce a typed upstream wire error.
	_, err := state.reasoningPartDone(ResponseReasoningSummaryPartDoneEvent{
		Type: "response.reasoning_summary_part.done", SequenceNumber: 5,
		ItemID:       "rs_other",
		OutputIndex:  0,
		SummaryIndex: 0,
		Part:         summaryTextPart("think"),
	})
	assertAnthropicWireError(t, err, "does not target the open thinking block's item")
	if state.reasoningBlockIndex == nil {
		t.Fatal("the open thinking block must survive the rejected part-done")
	}
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
	}
}

// TestReasoningPartDoneChargesStateEntries verifies each recorded
// thinkingDoneSummaries map and entry is charged against the state-entries
// budget (the fix for the uncharged done-summary bookkeeping).
func TestReasoningPartDoneChargesStateEntries(t *testing.T) {
	state := reasoningLifecycleState(t)
	before := state.budget.stateEntries
	if _, err := state.Convert(ResponseReasoningSummaryPartDoneEvent{
		Type: "response.reasoning_summary_part.done", SequenceNumber: 5,
		ItemID:       "rs_1",
		OutputIndex:  0,
		SummaryIndex: 0,
		Part:         summaryTextPart("think"),
	}); err != nil {
		t.Fatalf("part done: %v", err)
	}
	if got := state.budget.stateEntries - before; got != 2 {
		t.Fatalf("stateEntries charged = %d, want 2 (one map + one entry)", got)
	}
	if state.thinkingDoneSummaries["rs_1"][0] != "think" {
		t.Fatalf("done summary not recorded: %+v", state.thinkingDoneSummaries)
	}
}

// TestTerminalReasoningSummaryCountCompleteness verifies the terminal
// reconciliation's completeness check: every observed part-done summary must
// be covered by the envelope's summary list.
func TestTerminalReasoningSummaryCountCompleteness(t *testing.T) {
	state := reasoningLifecycleState(t)
	if _, err := state.Convert(ResponseReasoningSummaryPartDoneEvent{
		Type: "response.reasoning_summary_part.done", SequenceNumber: 5,
		ItemID:       "rs_1",
		OutputIndex:  0,
		SummaryIndex: 0,
		Part:         summaryTextPart("think"),
	}); err != nil {
		t.Fatalf("part done: %v", err)
	}
	// The item done carries the summary so the item reconciles, but the
	// completed ENVELOPE omits it: the observed part-done count (1) exceeds
	// the envelope's summary count (0).
	if _, err := state.Convert(ResponseOutputItemDoneEvent{
		Type: "response.output_item.done", SequenceNumber: 6,
		OutputIndex: 0,
		Item: &ResponsesReasoningOutputItem{ID: "rs_1", Type: "reasoning", Status: ResponsesItemCompleted,
			Summary: []ResponsesReasoningSummary{{Type: "summary_text", Text: "think"}}},
	}); err != nil {
		t.Fatalf("item done: %v", err)
	}
	envelope := ResponseEnvelope{
		ID: "resp_1", Object: "response", CreatedAt: 1, Status: "completed", Model: "m",
		Output: []ResponsesOutputItem{
			&ResponsesReasoningOutputItem{ID: "rs_1", Type: "reasoning", Status: ResponsesItemCompleted, Summary: []ResponsesReasoningSummary{}},
		},
		Usage: &ResponsesUsage{
			InputTokens: 0, OutputTokens: 0, TotalTokens: 0,
			InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
			OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
		},
	}
	_, err := state.Convert(ResponseCompletedEvent{
		Type: "response.completed", SequenceNumber: 7,
		Response: envelope,
	})
	assertAnthropicWireError(t, err, "does not cover the observed part-done summary 0")

	// Coverage is by index, not by count: an observed part-done at index 1
	// with an envelope carrying one summary leaves the observed summary out
	// even though the counts are equal.
	shifted := reasoningLifecycleStateAt(t, 1, "other")
	if _, err := shifted.Convert(ResponseReasoningSummaryPartDoneEvent{
		Type: "response.reasoning_summary_part.done", SequenceNumber: 5,
		ItemID:       "rs_1",
		OutputIndex:  0,
		SummaryIndex: 1,
		Part:         summaryTextPart("other"),
	}); err != nil {
		t.Fatalf("shifted part done: %v", err)
	}
	if _, err := shifted.Convert(ResponseOutputItemDoneEvent{
		Type: "response.output_item.done", SequenceNumber: 6,
		OutputIndex: 0,
		Item: &ResponsesReasoningOutputItem{ID: "rs_1", Type: "reasoning", Status: ResponsesItemCompleted,
			Summary: []ResponsesReasoningSummary{{Type: "summary_text", Text: "other"}}},
	}); err != nil {
		t.Fatalf("shifted item done: %v", err)
	}
	shiftedEnvelope := ResponseEnvelope{
		ID: "resp_1", Object: "response", CreatedAt: 1, Status: "completed", Model: "m",
		Output: []ResponsesOutputItem{
			&ResponsesReasoningOutputItem{ID: "rs_1", Type: "reasoning", Status: ResponsesItemCompleted,
				Summary: []ResponsesReasoningSummary{{Type: "summary_text", Text: "other"}}},
		},
		Usage: &ResponsesUsage{
			InputTokens: 0, OutputTokens: 0, TotalTokens: 0,
			InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
			OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
		},
	}
	_, err = shifted.Convert(ResponseCompletedEvent{
		Type: "response.completed", SequenceNumber: 7,
		Response: shiftedEnvelope,
	})
	assertAnthropicWireError(t, err, "does not cover the observed part-done summary 1")
}
