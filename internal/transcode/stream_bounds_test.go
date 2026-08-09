package transcode

// J9 regression tests (review-k finding 9, medium): stream bookkeeping is
// bounded and non-quadratic — text and refusal accumulate in builders, the
// repeated per-chunk envelope losses are recorded once per stream, and
// cumulative semantic state beyond the configured bound is rejected as
// corrupt upstream wire.

import (
	"errors"
	"runtime"
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
			FeatureServiceTier: {},
			FeatureLogprobs:    {},
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
	if count := countFeature(state.report, FeatureServiceTier); count != 1 {
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
		responsesEventBase: responsesEventBase{Type: "response.created", SequenceNumber: 0},
		Response: ResponseEnvelope{
			ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
			Output: []ResponsesOutputItem{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseOutputItemAddedEvent{
		responsesEventBase: responsesEventBase{Type: "response.output_item.added", SequenceNumber: 1},
		OutputIndex:        0,
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
			responsesEventBase: responsesEventBase{Type: "response.function_call_arguments.delta", SequenceNumber: sequence},
			ItemID:             "fc_1",
			OutputIndex:        0,
			Delta:              fragment,
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
