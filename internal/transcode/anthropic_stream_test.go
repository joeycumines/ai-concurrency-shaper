package transcode

// J6 regression tests: Responses→Messages stream identity, reasoning loss,
// stop nullability, and usage (review-j findings 7, 8, 9).

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestAnthropicStreamPartBlockIdentity proves two message items with the same
// content_index 0 target their own Anthropic blocks — no aliasing
// (review-j finding 7).
func TestAnthropicStreamPartBlockIdentity(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		"msg_1",
		"claude-x",
		1,
	)

	addPart := func(itemID string, outputIndex, sequence int64) {
		t.Helper()
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			responsesEventBase: responsesEventBase{
				Type:           "response.content_part.added",
				SequenceNumber: sequence,
			},
			ItemID:       itemID,
			OutputIndex:  outputIndex,
			ContentIndex: 0,
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
			responsesEventBase: responsesEventBase{
				Type:           "response.output_item.added",
				SequenceNumber: sequence,
			},
			OutputIndex: outputIndex,
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
	addPart("item_b", 1, 4)

	// A delta for item_a content_index 0 must target item_a's block (0),
	// not item_b's (1).
	events, err := state.Convert(ResponseTextDeltaEvent{
		responsesEventBase: responsesEventBase{
			Type:           "response.output_text.delta",
			SequenceNumber: 5,
		},
		ItemID:       "item_a",
		OutputIndex:  0,
		ContentIndex: 0,
		Delta:        "hello",
		Logprobs:     []ResponsesTextLogprob{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Index == nil || *events[0].Index != 0 {
		t.Fatalf("item_a delta targets %v, want block 0", events)
	}

	events, err = state.Convert(ResponseTextDeltaEvent{
		responsesEventBase: responsesEventBase{
			Type:           "response.output_text.delta",
			SequenceNumber: 6,
		},
		ItemID:       "item_b",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        "world",
		Logprobs:     []ResponsesTextLogprob{},
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
// single recorded loss (review-j finding 7).
func TestAnthropicStreamReasoningLossExactlyOnce(t *testing.T) {
	// Strict: the first reasoning event rejects the stream. The policy
	// approves the unavoidable usage-timing loss (cache_creation is never
	// part of the pinned Responses contract) so the created envelope can be
	// fed, but rejects reasoning.
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		LossPolicy{Allowed: map[Feature]struct{}{
			FeatureUsageTiming: {},
		}},
		"msg_1",
		"claude-x",
		1,
	)
	if _, err := state.Convert(ResponseCreatedEvent{
		responsesEventBase: responsesEventBase{Type: "response.created", SequenceNumber: 0},
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
		responsesEventBase: responsesEventBase{
			Type:           "response.reasoning_summary_part.added",
			SequenceNumber: 1,
		},
		ItemID:       "rs_1",
		OutputIndex:  0,
		SummaryIndex: 0,
		Part:         ResponsesSummaryTextPart{Type: "summary_text", Text: ""},
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
		j6PermissivePolicy(),
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
		responsesEventBase: responsesEventBase{Type: "response.created", SequenceNumber: 0},
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
	base := func(typ string) responsesEventBase {
		seq := sequence
		sequence++
		return responsesEventBase{Type: typ, SequenceNumber: seq}
	}
	feed(ResponseReasoningSummaryPartAddedEvent{
		responsesEventBase: base("response.reasoning_summary_part.added"),
		ItemID:             "rs_1",
		OutputIndex:        0,
		SummaryIndex:       0,
		Part:               ResponsesSummaryTextPart{Type: "summary_text", Text: ""},
	})
	feed(ResponseReasoningSummaryTextDeltaEvent{
		responsesEventBase: base("response.reasoning_summary_text.delta"),
		ItemID:             "rs_1",
		OutputIndex:        0,
		SummaryIndex:       0,
		Delta:              "reasoning",
	})
	feed(ResponseReasoningSummaryTextDoneEvent{
		responsesEventBase: base("response.reasoning_summary_text.done"),
		ItemID:             "rs_1",
		OutputIndex:        0,
		SummaryIndex:       0,
		Text:               "reasoning",
	})
	feed(ResponseReasoningSummaryPartDoneEvent{
		responsesEventBase: base("response.reasoning_summary_part.done"),
		ItemID:             "rs_1",
		OutputIndex:        0,
		SummaryIndex:       0,
		Part:               ResponsesSummaryTextPart{Type: "summary_text", Text: "reasoning"},
	})
	if count := countFeature(state.report, FeatureReasoningSummary); count != 1 {
		t.Fatalf("reasoning losses = %d, want exactly one", count)
	}
}

// TestAnthropicStreamMessageStartNullStopFields proves message_start
// serializes stop_reason: null and stop_sequence: null, and the stop reason
// appears only in message_delta (review-j finding 8).
func TestAnthropicStreamMessageStartNullStopFields(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		"msg_1",
		"claude-x",
		1,
	)
	events, err := state.Convert(ResponseCreatedEvent{
		responsesEventBase: responsesEventBase{
			Type:           "response.created",
			SequenceNumber: 1,
		},
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
		responsesEventBase: responsesEventBase{
			Type:           "response.completed",
			SequenceNumber: 2,
		},
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
// checked nonnegative arithmetic (review-j finding 9).
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
	converted, err := responsesUsageToAnthropicUsage(usage)
	if err != nil {
		t.Fatal(err)
	}
	// Anthropic total: 40 (uncached) + 5 (cache read) = 45 — the source
	// total, never 45 + 5.
	if converted.InputTokens != 40 || converted.CacheReadInputTokens != 5 {
		t.Fatalf("usage = %+v, want input 40 cache_read 5", converted)
	}
	if converted.OutputTokens != 25 || converted.OutputTokensDetails.ThinkingTokens != 12 {
		t.Fatalf("usage = %+v", converted)
	}

	// Checked arithmetic: cached exceeding the total is an error, as are
	// negative source values (a negative cached value must not mask a
	// negative total).
	bad := &ResponsesUsage{
		InputTokens: 3,
		TotalTokens: 3,
		InputTokensDetails: &UsageInputTokensDetails{
			CachedTokens: 10,
		},
	}
	if _, err := responsesUsageToAnthropicUsage(bad); err == nil {
		t.Fatal("arithmetically inconsistent usage accepted")
	}
	bad = &ResponsesUsage{
		InputTokens: -5,
		TotalTokens: -5,
		InputTokensDetails: &UsageInputTokensDetails{
			CachedTokens: -10,
		},
	}
	if _, err := responsesUsageToAnthropicUsage(bad); err == nil {
		t.Fatal("negative source usage accepted")
	}

	// Nil usage: unknown, never fabricated zeros.
	converted, err = responsesUsageToAnthropicUsage(nil)
	if err != nil {
		t.Fatal(err)
	}
	if converted != nil {
		t.Fatalf("nil usage converted to %+v", converted)
	}
}

// TestAnthropicUsageStreamNonStreamAgree proves the stream and non-stream
// conversions produce identical Anthropic usage for the same source
// (review-j finding 9).
func TestAnthropicUsageStreamNonStreamAgree(t *testing.T) {
	source := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":45,"input_tokens_details":{"cached_tokens":5},"output_tokens":25,"output_tokens_details":{"reasoning_tokens":12},"total_tokens":70}}`

	// Non-stream path.
	response, err := DecodeResponsesResponse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.LossPolicy = j6PermissivePolicy()
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
		j6PermissivePolicy(),
		"msg_1",
		"claude-x",
		1,
	)
	if _, err := state.Convert(ResponseCreatedEvent{
		responsesEventBase: responsesEventBase{
			Type:           "response.created",
			SequenceNumber: 0,
		},
		Response: ResponseEnvelope{
			ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
			Output: []ResponsesOutputItem{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	events, err := state.Convert(ResponseCompletedEvent{
		responsesEventBase: responsesEventBase{
			Type:           "response.completed",
			SequenceNumber: 1,
		},
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
// zeros (review-j finding 9).
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
	permissiveContext.LossPolicy = j6PermissivePolicy()
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
