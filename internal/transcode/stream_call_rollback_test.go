package transcode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// errorFrameSequence returns the sequence_number of a rendered Responses
// error frame.
func errorFrameSequence(t *testing.T, frame frameEvent) int64 {
	t.Helper()
	var event struct {
		SequenceNumber int64 `json:"sequence_number"`
	}
	if err := json.Unmarshal(frame.Data, &event); err != nil {
		t.Fatalf("error frame %s: %v", frame.Data, err)
	}
	return event.SequenceNumber
}

// assertEOFLatched feeds frames to the converter, then proves that a
// truncated stream's FinalizeEOF is a failure and that a second FinalizeEOF
// returns that same failure instance.
func assertEOFLatched(t *testing.T, c frameConverter, frames ...SSEEvent) {
	t.Helper()
	for _, frame := range frames {
		if _, err := c.Convert(frame); err != nil {
			t.Fatalf("frame %s: %v", frame.Data, err)
		}
	}
	batch, first := c.FinalizeEOF()
	if first == nil || batch.Terminal {
		t.Fatalf("FinalizeEOF: batch %+v, err %v; want a truncation failure", batch, first)
	}
	batch, second := c.FinalizeEOF()
	assertLatchedBatch(t, "second FinalizeEOF", batch, second, first)
}

// TestConverterFinalizeEOFFailureLatches proves an EOF failure is latched the
// way a frame failure is: a second FinalizeEOF returns the same failure
// instance and never a clean terminal.
func TestConverterFinalizeEOFFailureLatches(t *testing.T) {
	t.Run("marshal bound at release", func(t *testing.T) {
		// The echoed metadata renders at about 48 MiB, so the created and
		// in-progress events fit the generated-frame bound (about 49 MiB)
		// while the terminal envelope, which also carries the 1 MiB of
		// escaped output text, does not.
		echo := &ResponsesRequestEcho{
			ToolChoice: ResponsesToolChoice{Str: new("auto")},
			Metadata:   map[string]string{"pad": strings.Repeat("<", 8<<20)},
		}
		state := newChatResponsesStreamState(
			testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "resp_1", "gpt-4.1", 1710000000, echo,
		)
		c := newChatToResponsesConverter(state)
		if _, err := c.Convert(chatFrame(t, ChatStreamDelta{Content: new(strings.Repeat("<", 1<<20))}, nil)); err != nil {
			t.Fatalf("text delta: %v", err)
		}
		if _, err := c.Convert(chatFrame(t, ChatStreamDelta{}, new("stop"))); err != nil {
			t.Fatalf("finish chunk: %v", err)
		}
		_, first := c.FinalizeEOF()
		if _, ok := errors.AsType[*SSEBoundError](first); !ok {
			t.Fatalf("FinalizeEOF: err = %v, want the generated-frame bound", first)
		}
		batch, second := c.FinalizeEOF()
		assertLatchedBatch(t, "FinalizeEOF after a marshal failure", batch, second, first)
	})

	t.Run("chat to responses", func(t *testing.T) {
		assertEOFLatched(t, newChatToResponsesConverter(newChatResponsesStreamState(
			testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "resp_1", "gpt-4.1", 1710000000, nil,
		)), chatFrame(t, ChatStreamDelta{Content: new("hi")}, nil))
	})

	t.Run("chat to anthropic", func(t *testing.T) {
		assertEOFLatched(t, newChatToAnthropicConverter(
			newChatResponsesStreamState(testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "resp_1", "gpt-4.1", 1710000000, nil),
			newAnthropicResponsesStreamState(testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "msg_1", "claude-x", 1710000000),
		), chatFrame(t, ChatStreamDelta{Content: new("hi")}, nil))
	})

	t.Run("responses to anthropic", func(t *testing.T) {
		assertEOFLatched(t, newResponsesToAnthropicConverter(newAnthropicResponsesStreamState(
			testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "msg_1", "claude-x", 1710000000,
		)), responsesFrame(t, fsmCreated(1)))
	})

	t.Run("gemini to responses", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		assertEOFLatched(t, newGeminiToResponsesConverter(newGeminiResponsesStreamState(
			ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_1", "m", 1, nil,
		)), SSEEvent{Data: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"index":0}]}`)})
	})

	t.Run("gemini to anthropic", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		assertEOFLatched(t, newGeminiToAnthropicConverter(
			newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_1", "m", 1, nil),
			newAnthropicResponsesStreamState(ctx, permissiveLossPolicy(), ChatCapabilities{}, "msg_1", "m", 1),
		), SSEEvent{Data: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"index":0}]}`)})
	})

	t.Run("chat to gemini", func(t *testing.T) {
		ctx := testStreamContext()
		ctx.LossPolicy = usageOnlyPolicy()
		assertEOFLatched(t, newChatToGeminiConverter(
			newChatResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_1", "gpt-4.1", 1710000000, nil),
			newGeminiStreamClientState(ctx, UpstreamChatCompletions, ctx.LossPolicy, "resp_1", "gemini-x"),
		), chatFrame(t, ChatStreamDelta{Content: new("hi")}, nil))
	})

	t.Run("responses to gemini", func(t *testing.T) {
		ctx := testStreamContext()
		assertEOFLatched(t, newResponsesToGeminiConverter(newGeminiStreamClientState(
			ctx, UpstreamResponses, permissiveLossPolicy(), "resp_1", "gemini-x",
		)), responsesFrame(t, fsmCreated(1)))
	})

	t.Run("messages to gemini", func(t *testing.T) {
		upstream := newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", usageOnlyPolicy())
		gemini := newGeminiStreamClientState(testStreamContext(), UpstreamMessages, usageOnlyPolicy(), "resp_1", "claude-x")
		assertEOFLatched(t, newMessagesToGeminiConverter(upstream, gemini), SSEEvent{
			Event: "message_start",
			Data:  []byte(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`),
		})
	})
}

// TestFailedConverterCallLeavesNoSequenceOrNote proves a failed call rolls
// back what it advanced: the Responses sequence (so an error event continues
// from the last delivered event) and the report entries it recorded.
func TestFailedConverterCallLeavesNoSequenceOrNote(t *testing.T) {
	t.Run("gemini first chunk refused on role", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		c := newGeminiToResponsesConverter(newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_1", "m", 1, nil))
		_, err := c.Convert(SSEEvent{Data: []byte(`{"candidates":[{"content":{"role":"user","parts":[{"text":"hi"}]},"index":0}]}`)})
		if err == nil {
			t.Fatal("role user: nil error")
		}
		frame, ok := c.ErrorEvent(err)
		if !ok {
			t.Fatal("ErrorEvent: no frame")
		}
		if seq := errorFrameSequence(t, frame); seq != 0 {
			t.Fatalf("error event sequence_number = %d, want 0: no event of the refused chunk was delivered", seq)
		}
	})

	t.Run("chat first chunk refused on logprobs", func(t *testing.T) {
		chat := newChatResponsesStreamState(
			testStreamContext(), LossPolicy{Allowed: map[Feature]struct{}{}}, ChatCapabilities{}, "resp_1", "gpt-4.1", 1710000000, nil,
		)
		c := newChatToResponsesConverter(chat)
		_, err := c.Convert(SSEEvent{Data: []byte(
			`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null,"logprobs":{"content":[]}}]}`,
		)})
		if err == nil {
			t.Fatal("logprobs under a strict policy: nil error")
		}
		frame, ok := c.ErrorEvent(err)
		if !ok {
			t.Fatal("ErrorEvent: no frame")
		}
		if seq := errorFrameSequence(t, frame); seq != 0 {
			t.Fatalf("error event sequence_number = %d, want 0: the refused chunk's created events were never delivered", seq)
		}
	})

	t.Run("refused chunk leaves no report entry", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		c := newGeminiToResponsesConverter(newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_1", "m", 1, nil))
		// The id-less call records a call-id note; the functionResponse part
		// then refuses the whole chunk.
		_, err := c.Convert(SSEEvent{Data: []byte(
			`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}},{"functionResponse":{"name":"f","response":{}}}]},"index":0}]}`,
		)})
		if err == nil {
			t.Fatal("functionResponse part: nil error")
		}
		if report := c.ConversionReport(); len(report.Losses) != 0 || report.Dropped != 0 {
			t.Fatalf("report after a refused chunk = %+v, want no entries", report)
		}
	})
}

// TestContinuityRefusesLatchedExchange proves continuity is never retained
// for an exchange whose state or converter latched a failure after the
// terminal was released.
func TestContinuityRefusesLatchedExchange(t *testing.T) {
	t.Run("chat state", func(t *testing.T) {
		s := newChatResponsesStreamState(
			testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "resp_1", "gpt-4.1", 1710000000, nil,
		)
		if _, err := s.Convert(chatChunk(t, ChatStreamDelta{Content: new("hi")}, nil)); err != nil {
			t.Fatalf("text delta: %v", err)
		}
		if _, err := s.Convert(chatChunk(t, ChatStreamDelta{}, new("stop"))); err != nil {
			t.Fatalf("finish chunk: %v", err)
		}
		if _, ok := s.releaseTerminal(); !ok {
			t.Fatal("releaseTerminal: not released")
		}
		if _, err := s.Convert(chatChunk(t, ChatStreamDelta{Content: new("late")}, nil)); err == nil {
			t.Fatal("content after finish: nil error")
		}
		if _, _, _, _, ok := s.continuityTurns(nil, 0); ok {
			t.Fatal("continuityTurns retained a latched exchange")
		}
	})

	t.Run("gemini state", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_1", "m", 1, nil)
		if _, err := s.Convert(geminiChunk(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"index":0}]}`)); err != nil {
			t.Fatalf("text chunk: %v", err)
		}
		if _, err := s.Convert(geminiChunk(t, geminiFinishChunk)); err != nil {
			t.Fatalf("finish chunk: %v", err)
		}
		if _, err := s.FinalizeEOF(); err != nil {
			t.Fatalf("FinalizeEOF: %v", err)
		}
		if _, err := s.Convert(geminiChunk(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"late"}]},"index":0}]}`)); err == nil {
			t.Fatal("content after the terminal: nil error")
		}
		if _, _, _, _, ok := s.continuityTurns(nil, 0); ok {
			t.Fatal("continuityTurns retained a latched exchange")
		}
	})

	t.Run("chat to responses converter after a failed trailing frame", func(t *testing.T) {
		c := newChatToResponsesConverter(newChatResponsesStreamState(
			testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "resp_1", "gpt-4.1", 1710000000, nil,
		))
		if _, err := c.Convert(chatFrame(t, ChatStreamDelta{Content: new("hi")}, nil)); err != nil {
			t.Fatalf("text delta: %v", err)
		}
		if _, err := c.Convert(chatFrame(t, ChatStreamDelta{}, new("stop"))); err != nil {
			t.Fatalf("finish chunk: %v", err)
		}
		if batch, err := c.Convert(SSEEvent{Data: []byte("[DONE]")}); err != nil || !batch.Terminal {
			t.Fatalf("[DONE]: batch %+v, err %v; want the terminal", batch, err)
		}
		if _, err := c.Convert(SSEEvent{Data: []byte(`{"id":`)}); err == nil {
			t.Fatal("corrupt trailing frame: nil error")
		}
		if _, _, _, _, ok := c.continuityTurns(nil, 0); ok {
			t.Fatal("continuityTurns retained a latched exchange")
		}
	})
}

// TestMessagesToGeminiEOFRequiresCommittedTerminal proves a messages stream
// whose message_stop reached the upstream state, but whose Gemini terminal was
// never committed, is not a clean EOF.
func TestMessagesToGeminiEOFRequiresCommittedTerminal(t *testing.T) {
	upstream := newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", permissiveLossPolicy())
	gemini := newGeminiStreamClientState(testStreamContext(), UpstreamMessages, permissiveLossPolicy(), "resp_1", "claude-x")
	c := newMessagesToGeminiConverter(upstream, gemini)
	// The upstream state is driven directly, so the Gemini client never sees
	// the events that would commit its terminal.
	for _, frame := range []SSEEvent{
		{Event: "message_start", Data: []byte(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)},
		{Event: "message_delta", Data: []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`)},
		{Event: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
	} {
		event, err := decodeAnthropicStreamFrame(frame)
		if err != nil {
			t.Fatalf("decode %s: %v", frame.Event, err)
		}
		if _, err := upstream.Convert(event); err != nil {
			t.Fatalf("upstream %s: %v", frame.Event, err)
		}
	}
	if !upstream.stopped {
		t.Fatal("upstream state did not stop after message_stop")
	}
	batch, err := c.FinalizeEOF()
	if batch.Terminal || err == nil {
		t.Fatalf("FinalizeEOF: batch %+v, err %v; want a truncation failure", batch, err)
	}
}

// TestGeminiToAnthropicEOFChecksLatchesInFrameOrder proves that when both
// composed states latched, FinalizeEOF reports the same failure Convert would:
// the Gemini state's, which convertFrame checks first.
func TestGeminiToAnthropicEOFChecksLatchesInFrameOrder(t *testing.T) {
	ctx := geminiStreamTestContext()
	gemini := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_a", "m", 1, nil)
	anthropic := newAnthropicResponsesStreamState(ctx, permissiveLossPolicy(), ChatCapabilities{}, "msg_a", "m", 1)
	c := newGeminiToAnthropicConverter(gemini, anthropic)
	_, geminiFailure := gemini.Convert(geminiChunk(t, `{"candidates":[{"content":{"role":"user","parts":[{"text":"hi"}]},"index":0}]}`))
	if geminiFailure == nil {
		t.Fatal("gemini state: nil error for a refused role")
	}
	if _, err := anthropic.Convert(fsmCreated(1)); err != nil {
		t.Fatalf("anthropic created: %v", err)
	}
	if _, err := anthropic.Convert(fsmComplete(2)); err != nil {
		t.Fatalf("anthropic completed: %v", err)
	}
	if _, anthropicFailure := anthropic.Convert(fsmComplete(3)); anthropicFailure == nil {
		t.Fatal("anthropic state: nil error for an event after the terminal")
	}
	// FinalizeEOF is called without a prior Convert, so no converter latch
	// masks the composed states' order.
	batch, err := c.FinalizeEOF()
	assertLatchedBatch(t, "FinalizeEOF with both states latched", batch, err, geminiFailure)
}

// TestChatStreamComposedFailedEOFReleaseLeavesNoSentinelNote proves a
// missing-[DONE] release refused at EOF leaves no note claiming the completion
// was released.
func TestChatStreamComposedFailedEOFReleaseLeavesNoSentinelNote(t *testing.T) {
	chat := newChatResponsesStreamState(
		testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "resp_1", "gpt-4.1", 1710000000, nil,
	)
	anthropic := newAnthropicResponsesStreamState(
		testStreamContext(),
		LossPolicy{Allowed: map[Feature]struct{}{FeatureUsageUnknown: {}}},
		ChatCapabilities{},
		"msg_1",
		"claude-x",
		1710000000,
	)
	converter := newChatToAnthropicConverter(chat, anthropic)
	if _, err := converter.Convert(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
	)}); err != nil {
		t.Fatal(err)
	}
	// The bare usage on the finish chunk makes the EOF release hit the strict
	// usage refusal; there is no [DONE] sentinel.
	if _, err := converter.Convert(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
	)}); err != nil {
		t.Fatal(err)
	}
	if _, err := converter.FinalizeEOF(); err == nil {
		t.Fatal("EOF release: nil error, want the strict usage refusal")
	}
	if reportHasFeature(*converter.ConversionReport(), FeatureMissingStreamSentinel) {
		t.Fatal("the report claims the completion was released on EOF although the release was refused")
	}
}
