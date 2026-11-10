package transcode

import (
	"encoding/json"
	"errors"
	"testing"
)

// assertLatchedBatch fails unless err is the latched failure and the batch
// carries no terminal and no events.
func assertLatchedBatch(t *testing.T, what string, batch convertedBatch, err, latched error) {
	t.Helper()
	if !errors.Is(err, latched) {
		t.Fatalf("%s: err = %v, want the latched %v", what, err, latched)
	}
	if batch.Terminal || len(batch.Events) != 0 {
		t.Fatalf("%s released %+v", what, batch)
	}
}

// chatFrame marshals one chat chunk into an SSE frame.
func chatFrame(t *testing.T, delta ChatStreamDelta, finish *string) SSEEvent {
	t.Helper()
	data, err := json.Marshal(chatChunk(t, delta, finish))
	if err != nil {
		t.Fatal(err)
	}
	return SSEEvent{Data: data}
}

// responsesFrame marshals one typed Responses event into a named SSE frame.
func responsesFrame(t *testing.T, event ResponsesSSEEvent) SSEEvent {
	t.Helper()
	data, err := MarshalResponsesEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	return SSEEvent{Event: event.EventType(), Data: data}
}

// TestStreamAdapterFailedFrameStaysFailed proves the chat->responses,
// chat->anthropic, and responses->anthropic converters latch the first
// failure after the stream reached its terminal condition: [DONE] and EOF
// keep reporting that failure and never release a clean terminal.
func TestStreamAdapterFailedFrameStaysFailed(t *testing.T) {
	newChat := func() *chatResponsesStreamState {
		return newChatResponsesStreamState(
			testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "resp_1", "gpt-4.1", 1710000000, nil,
		)
	}
	newAnthropic := func() *anthropicResponsesStreamState {
		return newAnthropicResponsesStreamState(
			testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "msg_1", "claude-x", 1710000000,
		)
	}
	type chatConverter interface {
		Convert(SSEEvent) (convertedBatch, error)
		FinalizeEOF() (convertedBatch, error)
	}
	chatCases := map[string]func() chatConverter{
		"chat to responses": func() chatConverter { return newChatToResponsesConverter(newChat()) },
		"chat to anthropic": func() chatConverter { return newChatToAnthropicConverter(newChat(), newAnthropic()) },
	}
	trailing := map[string]func(t *testing.T) SSEEvent{
		"corrupt frame": func(*testing.T) SSEEvent { return SSEEvent{Data: []byte(`{"choices":[ oops`)} },
		// The chat state itself refuses substantive content after the finish.
		"content after finish": func(t *testing.T) SSEEvent {
			return chatFrame(t, ChatStreamDelta{Content: new("late")}, nil)
		},
	}
	for name, newConverter := range chatCases {
		for trailingName, frame := range trailing {
			t.Run(name+"/"+trailingName, func(t *testing.T) {
				c := newConverter()
				if _, err := c.Convert(chatFrame(t, ChatStreamDelta{Content: new("hi")}, nil)); err != nil {
					t.Fatalf("text delta: %v", err)
				}
				if _, err := c.Convert(chatFrame(t, ChatStreamDelta{}, new("stop"))); err != nil {
					t.Fatalf("finish chunk: %v", err)
				}
				_, latched := c.Convert(frame(t))
				if latched == nil {
					t.Fatal("trailing frame after finish: nil error")
				}
				batch, err := c.Convert(chatFrame(t, ChatStreamDelta{}, new("stop")))
				assertLatchedBatch(t, "frame after a failed frame", batch, err, latched)
				batch, err = c.Convert(SSEEvent{Data: []byte("[DONE]")})
				assertLatchedBatch(t, "[DONE] after a failed frame", batch, err, latched)
				batch, err = c.FinalizeEOF()
				assertLatchedBatch(t, "FinalizeEOF after a failed frame", batch, err, latched)
			})
		}
	}

	responsesTrailing := map[string]SSEEvent{
		"corrupt frame":         {Data: []byte(`{"type": oops`)},
		"event after terminal":  {}, // filled per run: a well-formed event the state refuses
		"mismatched event name": {Event: "response.in_progress"},
	}
	for trailingName, frame := range responsesTrailing {
		t.Run("responses to anthropic/"+trailingName, func(t *testing.T) {
			c := newResponsesToAnthropicConverter(newAnthropic())
			if _, err := c.Convert(responsesFrame(t, fsmCreated(1))); err != nil {
				t.Fatalf("created: %v", err)
			}
			batch, err := c.Convert(responsesFrame(t, fsmComplete(2)))
			if err != nil || !batch.Terminal {
				t.Fatalf("completed: batch %+v, err %v; want the terminal", batch, err)
			}
			switch trailingName {
			case "event after terminal":
				frame = responsesFrame(t, fsmComplete(3))
			case "mismatched event name":
				frame.Data = responsesFrame(t, fsmComplete(3)).Data
			}
			_, latched := c.Convert(frame)
			if latched == nil {
				t.Fatal("trailing frame after the terminal: nil error")
			}
			batch, err = c.Convert(responsesFrame(t, fsmComplete(4)))
			assertLatchedBatch(t, "frame after a failed frame", batch, err, latched)
			batch, err = c.Convert(SSEEvent{Data: []byte("[DONE]")})
			assertLatchedBatch(t, "[DONE] after a failed frame", batch, err, latched)
			batch, err = c.FinalizeEOF()
			assertLatchedBatch(t, "FinalizeEOF after a failed frame", batch, err, latched)
		})
	}
}

// TestStreamStatesLatchDirectFailures proves the chat and Anthropic render
// states latch every failure they return, so a caller driving a state
// directly cannot obtain a released terminal after a failure.
func TestStreamStatesLatchDirectFailures(t *testing.T) {
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
		_, latched := s.Convert(chatChunk(t, ChatStreamDelta{Content: new("late")}, nil))
		if latched == nil {
			t.Fatal("content after finish: nil error")
		}
		if _, err := s.Convert(chatChunk(t, ChatStreamDelta{}, new("stop"))); !errors.Is(err, latched) {
			t.Fatalf("chunk after a failure: err = %v, want the latched %v", err, latched)
		}
		events, err := s.FinalizeEOF()
		if !errors.Is(err, latched) || len(events) != 0 {
			t.Fatalf("FinalizeEOF after a failure: events %d, err = %v; want the latched %v", len(events), err, latched)
		}
	})

	t.Run("anthropic state", func(t *testing.T) {
		s := newAnthropicResponsesStreamState(
			testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "msg_1", "claude-x", 1710000000,
		)
		if _, err := s.Convert(fsmCreated(1)); err != nil {
			t.Fatalf("created: %v", err)
		}
		if _, err := s.Convert(fsmComplete(2)); err != nil {
			t.Fatalf("completed: %v", err)
		}
		_, latched := s.Convert(fsmComplete(3))
		if latched == nil {
			t.Fatal("event after the terminal: nil error")
		}
		events, err := s.FinalizeEOF()
		if !errors.Is(err, latched) || len(events) != 0 {
			t.Fatalf("FinalizeEOF after a failure: events %d, err = %v; want the latched %v", len(events), err, latched)
		}
	})
}

// TestComposedConvertersCheckAnthropicStateLatch proves a failure fed to the
// Anthropic state of a composed converter directly, after the terminal was
// released, is reported at EOF instead of a clean terminal.
func TestComposedConvertersCheckAnthropicStateLatch(t *testing.T) {
	t.Run("chat to anthropic", func(t *testing.T) {
		chat := newChatResponsesStreamState(
			testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "resp_1", "gpt-4.1", 1710000000, nil,
		)
		anthropic := newAnthropicResponsesStreamState(
			testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "msg_1", "claude-x", 1710000000,
		)
		c := newChatToAnthropicConverter(chat, anthropic)
		if _, err := c.Convert(chatFrame(t, ChatStreamDelta{Content: new("hi")}, nil)); err != nil {
			t.Fatalf("text delta: %v", err)
		}
		if _, err := c.Convert(chatFrame(t, ChatStreamDelta{}, new("stop"))); err != nil {
			t.Fatalf("finish chunk: %v", err)
		}
		if batch, err := c.Convert(SSEEvent{Data: []byte("[DONE]")}); err != nil || !batch.Terminal {
			t.Fatalf("[DONE]: batch %+v, err %v; want the terminal", batch, err)
		}
		_, latched := anthropic.Convert(fsmComplete(1 << 40))
		if latched == nil {
			t.Fatal("event after the terminal: nil error")
		}
		batch, err := c.FinalizeEOF()
		assertLatchedBatch(t, "FinalizeEOF after an Anthropic state failure", batch, err, latched)
	})

	t.Run("gemini to anthropic", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		ctx.LossPolicy.Allowed[FeatureUsageUnknown] = struct{}{}
		gemini := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_a", "m", 1, nil)
		anthropic := newAnthropicResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "msg_a", "m", 1)
		c := newGeminiToAnthropicConverter(gemini, anthropic)
		if _, err := c.Convert(SSEEvent{Data: []byte(geminiFinishChunk)}); err != nil {
			t.Fatalf("finish chunk: %v", err)
		}
		if batch, err := c.Convert(SSEEvent{Data: []byte("[DONE]")}); err != nil || !batch.Terminal {
			t.Fatalf("[DONE]: batch %+v, err %v; want the terminal", batch, err)
		}
		_, latched := anthropic.Convert(fsmComplete(1 << 40))
		if latched == nil {
			t.Fatal("event after the terminal: nil error")
		}
		batch, err := c.Convert(SSEEvent{Data: []byte("[DONE]")})
		assertLatchedBatch(t, "[DONE] after an Anthropic state failure", batch, err, latched)
		batch, err = c.FinalizeEOF()
		assertLatchedBatch(t, "FinalizeEOF after an Anthropic state failure", batch, err, latched)
	})

	t.Run("chat to gemini checks the chat state latch", func(t *testing.T) {
		ctx := testStreamContext()
		ctx.LossPolicy = usageOnlyPolicy()
		chat := newChatResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_1", "gpt-4.1", 1710000000, nil)
		gemini := newGeminiStreamClientState(ctx, UpstreamChatCompletions, ctx.LossPolicy, "resp_1", "gemini-x")
		c := newChatToGeminiConverter(chat, gemini)
		if _, err := c.Convert(chatFrame(t, ChatStreamDelta{Content: new("hi")}, nil)); err != nil {
			t.Fatalf("text delta: %v", err)
		}
		if _, err := c.Convert(chatFrame(t, ChatStreamDelta{}, new("stop"))); err != nil {
			t.Fatalf("finish chunk: %v", err)
		}
		_, latched := chat.Convert(chatChunk(t, ChatStreamDelta{Content: new("late")}, nil))
		if latched == nil {
			t.Fatal("content after finish: nil error")
		}
		batch, err := c.Convert(SSEEvent{Data: []byte("[DONE]")})
		assertLatchedBatch(t, "[DONE] after a chat state failure", batch, err, latched)
		batch, err = c.FinalizeEOF()
		assertLatchedBatch(t, "FinalizeEOF after a chat state failure", batch, err, latched)
	})
}

// TestGeminiStreamMessagesErrorEventIsOneFailure proves an Anthropic error
// event yields one failure instance: the converter's Convert, every later
// Convert, and FinalizeEOF all report the same error.
func TestGeminiStreamMessagesErrorEventIsOneFailure(t *testing.T) {
	upstream := newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", usageOnlyPolicy())
	gemini := newGeminiStreamClientState(
		testStreamContext(), UpstreamMessages, usageOnlyPolicy(), "resp_1", "claude-x",
	)
	c := newMessagesToGeminiConverter(upstream, gemini)
	if _, err := c.Convert(SSEEvent{
		Event: "message_start",
		Data:  []byte(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`),
	}); err != nil {
		t.Fatalf("message_start: %v", err)
	}
	_, failure := c.Convert(SSEEvent{
		Event: "error",
		Data:  []byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`),
	})
	if failure == nil {
		t.Fatal("error event: nil error")
	}
	batch, err := c.Convert(SSEEvent{Event: "ping", Data: []byte(`{"type":"ping"}`)})
	assertLatchedBatch(t, "frame after the error event", batch, err, failure)
	batch, err = c.FinalizeEOF()
	assertLatchedBatch(t, "FinalizeEOF after the error event", batch, err, failure)
}
