package transcode

// J9 regression tests (review-k finding 9, medium): stream bookkeeping is
// bounded and non-quadratic — text and refusal accumulate in builders, the
// repeated per-chunk envelope losses are recorded once per stream, and
// cumulative semantic state beyond the configured bound is rejected as
// corrupt upstream wire.

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestChatStreamAccumulationIsNonQuadratic proves text accumulation across
// many small deltas does not re-copy the whole accumulated string per delta:
// the pre-fix linear concatenation allocates ~N^2/2 x delta bytes (5 GB for
// 10k x 100 B deltas); the builder approach allocates the accumulated bytes
// plus the per-chunk events. The accumulated text is materialized exactly
// once at finish.
func TestChatStreamAccumulationIsNonQuadratic(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	delta := strings.Repeat("x", 100)
	const chunks = 10000
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < chunks; i++ {
		if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: str(delta)}, nil)); err != nil {
			t.Fatal(err)
		}
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 100<<20 {
		t.Fatalf("allocated %d bytes for 10k x 100 B deltas — quadratic accumulation", allocated)
	}

	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{}, str("stop"))); err != nil {
		t.Fatal(err)
	}
	held, ok := state.releaseTerminal()
	if !ok {
		t.Fatal("no held terminal")
	}
	completed, ok := held[len(held)-1].(ResponseCompletedEvent)
	if !ok {
		t.Fatalf("terminal = %T", held[len(held)-1])
	}
	message := completed.Response.Output[0].(*ResponsesOutputMessage)
	text := message.Content[0].(*ResponsesOutputText)
	if want := chunks * len(delta); len(text.Text) != want {
		t.Fatalf("accumulated text length = %d, want %d", len(text.Text), want)
	}
}

// TestChatStreamRepeatedLossesRecordedOnce proves the service-tier and
// log-probabilities losses fire exactly once per stream, not once per chunk.
func TestChatStreamRepeatedLossesRecordedOnce(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		LossPolicy{Allowed: map[Feature]struct{}{
			FeatureResponseServiceTier: {},
			FeatureLogprobs:            {},
		}},
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	for i := 0; i < 3; i++ {
		chunk := chatChunk(t, ChatStreamDelta{Content: str("x")}, nil)
		chunk.ServiceTier = str("auto")
		chunk.Choices[0].LogProbs = &ChatChoiceLogprobs{
			Content: []ChatTokenLogprob{},
			Refusal: []ChatTokenLogprob{},
		}
		if _, err := state.Convert(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if count := countFeature(state.report, FeatureResponseServiceTier); count != 1 {
		t.Fatalf("service tier losses = %d, want exactly one", count)
	}
	if count := countFeature(state.report, FeatureLogprobs); count != 1 {
		t.Fatalf("logprobs losses = %d, want exactly one", count)
	}
}

// TestChatStreamToolArgumentsCumulativeBound proves identity-complete tool
// argument fragments accumulate against the per-item cumulative bound and
// crossing it terminates the stream with the typed upstream wire error
// (review-k finding 9).
func TestChatStreamToolArgumentsCumulativeBound(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	fragment := strings.Repeat("x", 128*1024)
	feed := func() error {
		_, err := state.Convert(chatChunk(t, ChatStreamDelta{
			ToolCalls: []ChatToolCallDelta{{
				Index: intPtr(0),
				ID:    str("call_1"),
				Function: ChatToolCallFunction{
					Name:      str("f"),
					Arguments: fragment,
				},
			}},
		}, nil))
		return err
	}
	// 8 x 128 KiB = 1 MiB exactly: still within the bound.
	for i := 0; i < 8; i++ {
		if err := feed(); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	// The 9th fragment crosses the bound.
	if err := feed(); err == nil {
		t.Fatal("unbounded tool arguments accepted")
	} else {
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
	}
}

// TestChatStreamTextCumulativeBound proves accumulated text beyond the
// per-part cumulative bound terminates the stream with the typed upstream
// wire error.
func TestChatStreamTextCumulativeBound(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	delta := strings.Repeat("x", 128*1024)
	for i := 0; i < 8; i++ {
		if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: str(delta)}, nil)); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: str(delta)}, nil)); err == nil {
		t.Fatal("unbounded text accepted")
	} else {
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
	}
}

// TestResponsesStreamToolArgumentsCumulativeBound proves the Responses→
// Anthropic direction bounds its accumulated tool arguments the same way.
func TestResponsesStreamToolArgumentsCumulativeBound(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		"msg_1",
		"m",
		1,
	)
	if _, err := state.Convert(ResponseCreatedEvent{
		EventBase: EventBase{Type: "response.created", SequenceNumber: 0},
		Response: ResponseEnvelope{
			ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
			Output: []ResponsesOutputItem{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseOutputItemAddedEvent{
		EventBase:   EventBase{Type: "response.output_item.added", SequenceNumber: 1},
		OutputIndex: 0,
		Item: &ResponsesFunctionCallOutputItem{
			ID:        "fc_1",
			Type:      "function_call",
			Status:    ResponsesItemInProgress,
			CallID:    "call_1",
			Name:      "f",
			Arguments: "",
		},
	}); err != nil {
		t.Fatal(err)
	}
	fragment := strings.Repeat("x", 128*1024)
	feed := func(sequence int64) error {
		_, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			EventBase:   EventBase{Type: "response.function_call_arguments.delta", SequenceNumber: sequence},
			ItemID:      "fc_1",
			OutputIndex: 0,
			Delta:       fragment,
		})
		return err
	}
	for i := int64(0); i < 8; i++ {
		if err := feed(2 + i); err != nil {
			t.Fatalf("delta %d: %v", i, err)
		}
	}
	if err := feed(99); err == nil {
		t.Fatal("unbounded tool arguments accepted")
	} else {
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
	}
}

// TestStreamTotalStateBound proves the exchange-level stream budgets: the
// total accumulated semantic bytes across all items/parts/tools, the output
// item count, and the content parts per item are all hard bounds — crossing
// any of them terminates the exchange with the typed upstream wire error
// (review-08 blocker 7).
func TestStreamTotalStateBound(t *testing.T) {
	t.Run("total accumulated bytes across parts", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"m",
			1,
			nil,
		)
		// Each part stays under the per-part bound; the sum crosses the
		// exchange total.
		delta := strings.Repeat("x", 900*1024)
		content := ChatStreamDelta{Content: str(delta)}
		refusal := ChatStreamDelta{Refusal: str(delta)}
		for i := 0; i < 5; i++ {
			var err error
			if i%2 == 0 {
				_, err = state.Convert(chatChunk(t, content, nil))
			} else {
				_, err = state.Convert(chatChunk(t, refusal, nil))
			}
			if i < 4 {
				if err != nil {
					t.Fatalf("chunk %d: %v", i, err)
				}
				continue
			}
			var wireErr *UpstreamWireError
			if !errors.As(err, &wireErr) {
				t.Fatalf("err = %T %v, want *UpstreamWireError for the exchange total", err, err)
			}
			if !strings.Contains(err.Error(), "exchange") {
				t.Fatalf("error = %q, want the exchange-total violation", err.Error())
			}
		}
	})

	t.Run("output item count", func(t *testing.T) {
		state := newAnthropicResponsesStreamState(
			testStreamContext(),
			j6PermissivePolicy(),
			"msg_1",
			"m",
			1,
		)
		feedAnthropicCreated(t, state, 0)
		for i := 0; i < maxStreamOutputItems; i++ {
			if _, err := state.Convert(ResponseOutputItemAddedEvent{
				EventBase:   EventBase{Type: "response.output_item.added", SequenceNumber: int64(i + 1)},
				OutputIndex: int64(i),
				Item: &ResponsesReasoningOutputItem{
					ID: "r" + strconv.Itoa(i), Type: "reasoning", Status: ResponsesItemInProgress,
					Summary: []ResponsesReasoningSummary{},
				},
			}); err != nil {
				t.Fatalf("item %d: %v", i, err)
			}
		}
		_, err := state.Convert(ResponseOutputItemAddedEvent{
			EventBase:   EventBase{Type: "response.output_item.added", SequenceNumber: maxStreamOutputItems + 1},
			OutputIndex: maxStreamOutputItems,
			Item: &ResponsesReasoningOutputItem{
				ID: "overflow", Type: "reasoning", Status: ResponsesItemInProgress,
				Summary: []ResponsesReasoningSummary{},
			},
		})
		assertAnthropicWireError(t, err, "items")
	})

	t.Run("content parts per item", func(t *testing.T) {
		state := newAnthropicResponsesStreamState(
			testStreamContext(),
			j6PermissivePolicy(),
			"msg_1",
			"m",
			1,
		)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			EventBase:   EventBase{Type: "response.output_item.added", SequenceNumber: 1},
			OutputIndex: 0,
			Item: &ResponsesOutputMessage{
				ID: "m1", Type: "message", Role: "assistant",
				Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
			},
		}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < maxStreamPartsPerItem; i++ {
			if _, err := state.Convert(ResponseContentPartAddedEvent{
				EventBase:    EventBase{Type: "response.content_part.added", SequenceNumber: int64(i + 2)},
				ItemID:       "m1",
				OutputIndex:  0,
				ContentIndex: int64(i),
				Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
			}); err != nil {
				t.Fatalf("part %d: %v", i, err)
			}
		}
		_, err := state.Convert(ResponseContentPartAddedEvent{
			EventBase:    EventBase{Type: "response.content_part.added", SequenceNumber: maxStreamPartsPerItem + 2},
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: maxStreamPartsPerItem,
			Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
		})
		assertAnthropicWireError(t, err, "parts")
	})
}

// TestGeneratedFrameBoundAfterJSONEscaping proves generated downstream
// frames are bounded AFTER marshaling: a payload whose JSON escaping
// amplifies it beyond the frame bound is rejected with the typed SSE frame
// error (review-08 blocker 7).
func TestGeneratedFrameBoundAfterJSONEscaping(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	// 200 KiB of '<' escapes to \u003c (6 bytes each) — the marshaled delta
	// frame exceeds the 1 MiB frame bound while the input frame is well
	// within it.
	delta := strings.Repeat("<", 200*1024)
	_, err := converter.Convert(SSEEvent{Data: []byte(
		"{\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + delta + "\"},\"finish_reason\":null}]}",
	)})
	var boundErr *SSEBoundError
	if !errors.As(err, &boundErr) {
		t.Fatalf("err = %T %v, want *SSEBoundError", err, err)
	}
	if boundErr.Line {
		t.Fatal("bound error must be a frame violation, not a line violation")
	}
}

// TestStreamBoundaryHelpers closes the remaining exchange-budget branches:
// the conversion-report entry cap, the terminal-batch bound in the
// converting reader, the chat-side tool-call and part-per-item counts, the
// bounded error text, and the Anthropic-direction frame bound
// (review-08 blocker 7).
func TestStreamBoundaryHelpers(t *testing.T) {
	t.Run("conversion report entry cap", func(t *testing.T) {
		report := ConversionReport{}
		policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureResponseServiceTier: {}}}
		for i := 0; i < maxStreamConversionReportEntries; i++ {
			if err := report.Lose(policy, FeatureResponseServiceTier, "x", "y"); err != nil {
				t.Fatalf("entry %d: %v", i, err)
			}
		}
		err := report.Lose(policy, FeatureResponseServiceTier, "x", "y")
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError (upstream classification)", err, err)
		}
	})

	t.Run("terminal batch bound", func(t *testing.T) {
		reader := newConvertingReader(NewSSEReaderWithLimits(strings.NewReader(""), 0, 0), &fixedConverter{})
		frames := (maxStreamTerminalBatchBytes / (maxSSEFrameBytes - 64)) + 2
		batch := convertedBatch{}
		for i := 0; i < frames; i++ {
			batch.Events = append(batch.Events, frameEvent{
				Type: "x",
				Data: bytes.Repeat([]byte("a"), maxSSEFrameBytes-64),
			})
		}
		err := reader.appendBatch(batch)
		var boundErr *SSEBoundError
		if !errors.As(err, &boundErr) {
			t.Fatalf("err = %T %v, want *SSEBoundError", err, err)
		}
	})

	t.Run("chat tool call count", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"m",
			1,
			nil,
		)
		for i := 0; i < maxStreamToolCalls; i++ {
			chunk := chatChunk(t, ChatStreamDelta{ToolCalls: []ChatToolCallDelta{{
				Index: intPtr(i),
				ID:    str(fmt.Sprintf("call_%d", i)),
				Type:  str("function"),
				Function: ChatToolCallFunction{
					Name:      str("f"),
					Arguments: "{}",
				},
			}}}, nil)
			if _, err := state.Convert(chunk); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
		chunk := chatChunk(t, ChatStreamDelta{ToolCalls: []ChatToolCallDelta{{
			Index:    intPtr(maxStreamToolCalls),
			ID:       str("overflow"),
			Type:     str("function"),
			Function: ChatToolCallFunction{Name: str("f"), Arguments: "{}"},
		}}}, nil)
		_, err := state.Convert(chunk)
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
	})

	t.Run("chat parts per item", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"m",
			1,
			nil,
		)
		for i := 0; i < maxStreamPartsPerItem; i++ {
			var delta ChatStreamDelta
			if i%2 == 0 {
				delta = ChatStreamDelta{Content: str("x")}
			} else {
				delta = ChatStreamDelta{Refusal: str("x")}
			}
			if _, err := state.Convert(chatChunk(t, delta, nil)); err != nil {
				t.Fatalf("part %d: %v", i, err)
			}
		}
		_, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: str("x")}, nil))
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
	})

	t.Run("bounded error text", func(t *testing.T) {
		long := strings.Repeat("x", maxStreamErrorTextBytes*2)
		bounded := boundedErrorMessage(errors.New(long))
		if len(bounded) != maxStreamErrorTextBytes+len("…") {
			t.Fatalf("bounded length = %d", len(bounded))
		}
		if !strings.HasSuffix(bounded, "…") {
			t.Fatalf("bounded = %q, want ellipsis", bounded)
		}
		if got := boundedErrorMessage(errors.New("short")); got != "short" {
			t.Fatalf("short error = %q", got)
		}
	})

	t.Run("anthropic frame bound after escaping", func(t *testing.T) {
		state := newAnthropicResponsesStreamState(
			testStreamContext(),
			j6PermissivePolicy(),
			"msg_1",
			"m",
			1,
		)
		converter := newResponsesToAnthropicConverter(state)
		feed := func(eventType, data string) error {
			_, err := converter.Convert(SSEEvent{Event: eventType, Data: []byte(data)})
			return err
		}
		if err := feed("response.created",
			`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[]}}`); err != nil {
			t.Fatal(err)
		}
		if err := feed("response.output_item.added",
			`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"m1","type":"message","role":"assistant","status":"in_progress","content":[]}}`); err != nil {
			t.Fatal(err)
		}
		if err := feed("response.content_part.added",
			`{"type":"response.content_part.added","sequence_number":2,"item_id":"m1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`); err != nil {
			t.Fatal(err)
		}
		delta := strings.Repeat("<", 200*1024)
		err := feed("response.output_text.delta", fmt.Sprintf(
			`{"type":"response.output_text.delta","sequence_number":3,"item_id":"m1","output_index":0,"content_index":0,"delta":%q,"logprobs":[]}`,
			delta,
		))
		var boundErr *SSEBoundError
		if !errors.As(err, &boundErr) {
			t.Fatalf("err = %T %v, want *SSEBoundError", err, err)
		}
	})
}

// TestStreamBoundaryHelpers2 closes the remaining reachable exchange-budget
// branches (review-08 blocker 7).
func TestStreamBoundaryHelpers2(t *testing.T) {
	t.Run("anthropic per-item text bound", func(t *testing.T) {
		state := newAnthropicResponsesStreamState(
			testStreamContext(),
			j6PermissivePolicy(),
			"msg_1",
			"m",
			1,
		)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			EventBase:   EventBase{Type: "response.output_item.added", SequenceNumber: 1},
			OutputIndex: 0,
			Item: &ResponsesOutputMessage{
				ID: "m1", Type: "message", Role: "assistant",
				Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			EventBase:    EventBase{Type: "response.content_part.added", SequenceNumber: 2},
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
		}); err != nil {
			t.Fatal(err)
		}
		delta := strings.Repeat("x", 128*1024)
		var err error
		for i := 0; i < 8; i++ {
			_, err = state.Convert(ResponseTextDeltaEvent{
				EventBase:    EventBase{Type: "response.output_text.delta", SequenceNumber: int64(i + 3)},
				ItemID:       "m1",
				OutputIndex:  0,
				ContentIndex: 0,
				Delta:        delta,
				Logprobs:     []ResponsesTextLogprob{},
			})
			if err != nil {
				t.Fatalf("delta %d: %v", i, err)
			}
		}
		_, err = state.Convert(ResponseTextDeltaEvent{
			EventBase:    EventBase{Type: "response.output_text.delta", SequenceNumber: 99},
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        delta,
			Logprobs:     []ResponsesTextLogprob{},
		})
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
	})

	t.Run("anthropic tool call count", func(t *testing.T) {
		state := newAnthropicResponsesStreamState(
			testStreamContext(),
			j6PermissivePolicy(),
			"msg_1",
			"m",
			1,
		)
		feedAnthropicCreated(t, state, 0)
		for i := 0; i < maxStreamToolCalls; i++ {
			if _, err := state.Convert(ResponseOutputItemAddedEvent{
				EventBase:   EventBase{Type: "response.output_item.added", SequenceNumber: int64(i + 1)},
				OutputIndex: int64(i),
				Item: &ResponsesFunctionCallOutputItem{
					ID: "fc" + strconv.Itoa(i), Type: "function_call", Status: ResponsesItemInProgress,
					CallID: "call" + strconv.Itoa(i), Name: "f", Arguments: "",
				},
			}); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
		_, err := state.Convert(ResponseOutputItemAddedEvent{
			EventBase:   EventBase{Type: "response.output_item.added", SequenceNumber: maxStreamToolCalls + 1},
			OutputIndex: maxStreamToolCalls,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "overflow", Type: "function_call", Status: ResponsesItemInProgress,
				CallID: "call_overflow", Name: "f", Arguments: "",
			},
		})
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
	})

	t.Run("append batch frame bound", func(t *testing.T) {
		reader := newConvertingReader(NewSSEReaderWithLimits(strings.NewReader(""), 0, 0), &fixedConverter{})
		err := reader.appendBatch(convertedBatch{Events: []frameEvent{{
			Type: "x",
			Data: bytes.Repeat([]byte("a"), maxSSEFrameBytes),
		}}})
		var boundErr *SSEBoundError
		if !errors.As(err, &boundErr) {
			t.Fatalf("err = %T %v, want *SSEBoundError", err, err)
		}
	})
}

// TestChatStreamReasoningReportRecordedOnce proves the provider-reasoning
// report entry (loss without the capability, note with it) is recorded
// exactly once per stream, never once per delta (review-08 blocker 7).
func TestChatStreamReasoningReportRecordedOnce(t *testing.T) {
	t.Run("loss without capability", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			LossPolicy{Allowed: map[Feature]struct{}{FeatureProviderReasoningText: {}}},
			ChatCapabilities{},
			"resp_1",
			"m",
			1,
			nil,
		)
		for i := 0; i < 3; i++ {
			if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Reasoning: str("think")}, nil)); err != nil {
				t.Fatalf("delta %d: %v", i, err)
			}
		}
		if count := countFeature(state.report, FeatureProviderReasoningText); count != 1 {
			t.Fatalf("reasoning losses = %d, want exactly one", count)
		}
	})
	t.Run("note with capability", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{ProviderReasoningText: true},
			"resp_1",
			"m",
			1,
			nil,
		)
		for i := 0; i < 3; i++ {
			if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Reasoning: str("think")}, nil)); err != nil {
				t.Fatalf("delta %d: %v", i, err)
			}
		}
		if count := countFeature(state.report, FeatureProviderReasoningText); count != 1 {
			t.Fatalf("reasoning notes = %d, want exactly one", count)
		}
	})
}

// TestStreamToolSnapshotBytesCounted proves the done-snapshot bytes written
// into the accumulated tool-argument buffer count against the exchange total
// — the snapshot-completion path must not bypass the budget (review-08
// blocker 7).
func TestStreamToolSnapshotBytesCounted(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		"msg_1",
		"m",
		1,
	)
	feedAnthropicCreated(t, state, 0)
	snapshot := `{"x":"` + strings.Repeat("a", 500*1024) + `"}`
	// 8 calls × 500 KiB snapshots = 4 MiB: exactly at the exchange total.
	for i := 0; i < 8; i++ {
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			EventBase:   EventBase{Type: "response.output_item.added", SequenceNumber: int64(i*3 + 1)},
			OutputIndex: int64(i),
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc" + strconv.Itoa(i), Type: "function_call", Status: ResponsesItemInProgress,
				CallID: "call" + strconv.Itoa(i), Name: "f", Arguments: "",
			},
		}); err != nil {
			t.Fatalf("added %d: %v", i, err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			EventBase:   EventBase{Type: "response.function_call_arguments.done", SequenceNumber: int64(i*3 + 2)},
			ItemID:      "fc" + strconv.Itoa(i),
			OutputIndex: int64(i),
			Arguments:   snapshot,
		}); err != nil {
			t.Fatalf("args done %d: %v", i, err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			EventBase:   EventBase{Type: "response.output_item.done", SequenceNumber: int64(i*3 + 3)},
			OutputIndex: int64(i),
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc" + strconv.Itoa(i), Type: "function_call", Status: ResponsesItemCompleted,
				CallID: "call" + strconv.Itoa(i), Name: "f", Arguments: snapshot,
			},
		}); err != nil {
			t.Fatalf("item done %d: %v", i, err)
		}
	}
	// The 9th call's snapshot crosses the exchange total.
	if _, err := state.Convert(ResponseOutputItemAddedEvent{
		EventBase:   EventBase{Type: "response.output_item.added", SequenceNumber: 25},
		OutputIndex: 8,
		Item: &ResponsesFunctionCallOutputItem{
			ID: "fc8", Type: "function_call", Status: ResponsesItemInProgress,
			CallID: "call8", Name: "f", Arguments: "",
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
		EventBase:   EventBase{Type: "response.function_call_arguments.done", SequenceNumber: 26},
		ItemID:      "fc8",
		OutputIndex: 8,
		Arguments:   snapshot,
	})
	var wireErr *UpstreamWireError
	if !errors.As(err, &wireErr) {
		t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
	}
	if !strings.Contains(err.Error(), "exchange total") {
		t.Fatalf("error = %q, want the exchange-total violation", err.Error())
	}
}
