package transcode

// Review-08 blockers 3+4+5 regression tests: the Responses-to-Anthropic
// stream enforces an explicit lifecycle (created first and exactly once,
// strictly increasing sequence numbers, stable response identity, item/part
// ownership and uniqueness, matching output indexes, every block closed
// before the terminal envelope — nothing synthesized), done snapshots are
// reconciled against accumulated state (text, refusal, content parts,
// output items, tool arguments), and tool arguments can never collapse to
// an empty object when the done event carried the real snapshot.

import (
	"errors"
	"strings"
	"testing"
)

// anthropicLifecycleEnvelope builds a minimal valid ResponseEnvelope.
func anthropicLifecycleEnvelope(id string) ResponseEnvelope {
	return ResponseEnvelope{
		ID:        id,
		Object:    "response",
		CreatedAt: 1710000000,
		Status:    "in_progress",
		Model:     "gpt-4.1",
		Output:    []ResponsesOutputItem{},
	}
}

// anthropicLifecycleState builds a fresh state for lifecycle tests.
func anthropicLifecycleState(t *testing.T) *anthropicResponsesStreamState {
	t.Helper()
	return newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		"msg_1",
		"claude-x",
		1710000000,
	)
}

// feedAnthropicCreated feeds response.created and must succeed.
func feedAnthropicCreated(t *testing.T, state *anthropicResponsesStreamState, sequence int64) {
	t.Helper()
	if _, err := state.Convert(ResponseCreatedEvent{
		Type:           "response.created",
		SequenceNumber: sequence,
		Response:       anthropicLifecycleEnvelope("resp_1"),
	}); err != nil {
		t.Fatalf("created: %v", err)
	}
}

func assertAnthropicWireError(t *testing.T, err error, wantSubstring string) {
	t.Helper()
	var wireErr *UpstreamWireError
	if !errors.As(err, &wireErr) {
		t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
	}
	if wireErr.Protocol != UpstreamResponses {
		t.Fatalf("protocol = %v, want responses", wireErr.Protocol)
	}
	if !strings.Contains(err.Error(), wantSubstring) {
		t.Fatalf("error = %q, want substring %q", err.Error(), wantSubstring)
	}
}

// TestResponsesStreamRejectsTerminalBeforeCreated proves completed,
// incomplete, and failed envelopes before response.created are corrupt
// upstream wire: they must never emit message_delta + message_stop without
// message_start (review-08 blocker 3).
func TestResponsesStreamRejectsTerminalBeforeCreated(t *testing.T) {
	tests := []struct {
		name  string
		event ResponsesSSEEvent
	}{
		{
			name: "completed",
			event: ResponseCompletedEvent{
				EventBase: EventBase{
					Type:           "response.completed",
					SequenceNumber: 0,
				},
				Response: anthropicLifecycleEnvelope("resp_1"),
			},
		},
		{
			name: "incomplete",
			event: ResponseIncompleteEvent{
				EventBase: EventBase{
					Type:           "response.incomplete",
					SequenceNumber: 0,
				},
				Response: anthropicLifecycleEnvelope("resp_1"),
			},
		},
		{
			name: "failed",
			event: ResponseFailedEvent{
				EventBase: EventBase{
					Type:           "response.failed",
					SequenceNumber: 0,
				},
				Response: anthropicLifecycleEnvelope("resp_1"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := anthropicLifecycleState(t)
			events, err := state.Convert(tt.event)
			assertAnthropicWireError(t, err, "response.created")
			for _, event := range events {
				if event.Type == AnthropicStreamEventTypeMessageDelta ||
					event.Type == AnthropicStreamEventTypeMessageStop {
					t.Fatalf("terminal before created emitted %s", event.Type)
				}
			}
		})
	}
}

// TestResponsesStreamRejectsMissingDoneEvents proves a terminal envelope
// with an open text part or tool block is corrupt upstream wire: the missing
// content_part.done / output_item.done is an error, never a synthesized
// content_block_stop (review-08 blocker 3).
func TestResponsesStreamRejectsMissingDoneEvents(t *testing.T) {
	t.Run("open text part at completed", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesOutputMessage{
				ID: "m1", Type: "message", Role: "assistant",
				Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 3,
			Response: ResponseEnvelope{
				ID: "resp_1", Object: "response", CreatedAt: 1710000000,
				Status: "completed", Model: "gpt-4.1", Output: []ResponsesOutputItem{},
			},
		})
		assertAnthropicWireError(t, err, "done")
	})
	t.Run("open tool block at completed", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemInProgress,
				CallID: "call_1", Name: "f", Arguments: "",
			},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Delta:       `{"x":`,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 3,
			Response: ResponseEnvelope{
				ID: "resp_1", Object: "response", CreatedAt: 1710000000,
				Status: "completed", Model: "gpt-4.1", Output: []ResponsesOutputItem{},
			},
		})
		assertAnthropicWireError(t, err, "done")
	})
}

// TestResponsesStreamRejectsSequenceRegression proves sequence numbers must
// be strictly increasing and unique across the stream (review-08 blocker 3).
func TestResponsesStreamRejectsSequenceRegression(t *testing.T) {
	tests := []struct {
		name string
		feed func(t *testing.T, state *anthropicResponsesStreamState)
	}{
		{
			name: "duplicate sequence",
			feed: func(t *testing.T, state *anthropicResponsesStreamState) {
				feedAnthropicCreated(t, state, 0)
				if _, err := state.Convert(ResponseInProgressEvent{
					Type: "response.in_progress", SequenceNumber: 0,
					Response: anthropicLifecycleEnvelope("resp_1"),
				}); err == nil {
					t.Fatal("duplicate sequence accepted")
				} else {
					assertAnthropicWireError(t, err, "sequence")
				}
			},
		},
		{
			name: "regressing sequence",
			feed: func(t *testing.T, state *anthropicResponsesStreamState) {
				feedAnthropicCreated(t, state, 5)
				if _, err := state.Convert(ResponseInProgressEvent{
					Type: "response.in_progress", SequenceNumber: 3,
					Response: anthropicLifecycleEnvelope("resp_1"),
				}); err == nil {
					t.Fatal("regressing sequence accepted")
				} else {
					assertAnthropicWireError(t, err, "sequence")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.feed(t, anthropicLifecycleState(t))
		})
	}
}

// TestResponsesStreamRejectsIdentityAndOwnershipViolations proves the
// lifecycle ownership rules: nothing before response.created, stable
// response identity across envelopes, unique item and part identities, an
// item that owns a part before any delta, and matching output indexes
// (review-08 blocker 3).
func TestResponsesStreamRejectsIdentityAndOwnershipViolations(t *testing.T) {
	t.Run("event before created", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		_, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 0,
			OutputIndex: 0,
			Item: &ResponsesOutputMessage{
				ID: "m1", Type: "message", Role: "assistant",
				Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
			},
		})
		assertAnthropicWireError(t, err, "response.created")
	})
	t.Run("in_progress before created", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		_, err := state.Convert(ResponseInProgressEvent{
			Type: "response.in_progress", SequenceNumber: 0,
			Response: anthropicLifecycleEnvelope("resp_1"),
		})
		assertAnthropicWireError(t, err, "response.created")
	})
	t.Run("unstable response id", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		envelope := anthropicLifecycleEnvelope("resp_2")
		_, err := state.Convert(ResponseInProgressEvent{
			Type: "response.in_progress", SequenceNumber: 1,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "resp_1")
	})
	t.Run("unstable response model", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Model = "other-model"
		_, err := state.Convert(ResponseInProgressEvent{
			Type: "response.in_progress", SequenceNumber: 1,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "model")
	})
	t.Run("duplicate item identity", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		item := &ResponsesOutputMessage{
			ID: "m1", Type: "message", Role: "assistant",
			Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
		}
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        item,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 2,
			OutputIndex: 1,
			Item:        item,
		})
		assertAnthropicWireError(t, err, "duplicate")
	})
	t.Run("content part for unknown item", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		_, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 1,
			ItemID:       "ghost",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
		})
		assertAnthropicWireError(t, err, "item")
	})
	t.Run("duplicate content part", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesOutputMessage{
				ID: "m1", Type: "message", Role: "assistant",
				Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
			},
		}); err != nil {
			t.Fatal(err)
		}
		part := &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         part,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         part,
		})
		assertAnthropicWireError(t, err, "duplicate")
	})
	t.Run("mismatched output index", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesOutputMessage{
				ID: "m1", Type: "message", Role: "assistant",
				Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  1,
			ContentIndex: 0,
			Delta:        "hi",
			Logprobs:     []ResponsesTextLogprob{},
		})
		assertAnthropicWireError(t, err, "output index")
	})
}

// anthropicMessageItem builds a message output item.
func anthropicMessageItem(id string, parts ...ResponsesOutputContentPart) *ResponsesOutputMessage {
	return &ResponsesOutputMessage{
		ID:      id,
		Type:    "message",
		Role:    "assistant",
		Status:  ResponsesItemCompleted,
		Content: parts,
	}
}

// TestResponsesStreamReconcilesDoneSnapshots proves every done event is
// reconciled against the accumulated state: text.done and refusal.done
// against the accumulated text, content_part.done against the opened part,
// output_item.done against the observed item, and the terminal envelope
// against the incremental output (review-08 blockers 3 and 4).
func TestResponsesStreamReconcilesDoneSnapshots(t *testing.T) {
	t.Run("text done mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        anthropicMessageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "hel",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "lo",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 5,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "help",
			Logprobs:     []ResponsesTextLogprob{},
		})
		assertAnthropicWireError(t, err, "does not match")
	})

	t.Run("refusal done mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        anthropicMessageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamRefusalPart{Type: "refusal", Refusal: ""},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseRefusalDeltaEvent{
			Type: "response.refusal.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "no",
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseRefusalDoneEvent{
			Type: "response.refusal.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Refusal:      "yes",
		})
		assertAnthropicWireError(t, err, "refusal")
	})

	t.Run("content part done type mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        anthropicMessageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 5,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamRefusalPart{Type: "refusal", Refusal: "hi"},
		})
		assertAnthropicWireError(t, err, "output_text")
	})

	t.Run("output item done message content mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        anthropicMessageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 5,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "hi", Annotations: []ResponsesAnnotation{}},
		}); err != nil {
			t.Fatal(err)
		}
		done := anthropicMessageItem("m1",
			&ResponsesOutputText{Type: "output_text", Text: "bye", Annotations: []ResponsesAnnotation{}},
		)
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 6,
			OutputIndex: 0,
			Item:        done,
		})
		assertAnthropicWireError(t, err, "does not match")
	})

	t.Run("function arguments done mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemInProgress,
				CallID: "call_1", Name: "f", Arguments: "",
			},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Delta:       `{"x":`,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: 3,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Arguments:   `{"y":1}`,
		})
		assertAnthropicWireError(t, err, "arguments")
	})

	t.Run("output item done function identity mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemInProgress,
				CallID: "call_1", Name: "f", Arguments: "",
			},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Delta:       `{"x":1}`,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: 3,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Arguments:   `{"x":1}`,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 4,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemCompleted,
				CallID: "call_2", Name: "f", Arguments: `{"x":1}`,
			},
		})
		assertAnthropicWireError(t, err, "call_1")
	})

	t.Run("output item done function status mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemInProgress,
				CallID: "call_1", Name: "f", Arguments: "",
			},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Delta:       `{"x":1}`,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: 3,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Arguments:   `{"x":1}`,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 4,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemInProgress,
				CallID: "call_1", Name: "f", Arguments: `{"x":1}`,
			},
		})
		assertAnthropicWireError(t, err, "completed")
	})

	t.Run("terminal envelope output mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        anthropicMessageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 5,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "hi", Annotations: []ResponsesAnnotation{}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 6,
			OutputIndex: 0,
			Item: anthropicMessageItem("m1",
				&ResponsesOutputText{Type: "output_text", Text: "hi", Annotations: []ResponsesAnnotation{}},
			),
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Output = []ResponsesOutputItem{anthropicMessageItem("m1",
			&ResponsesOutputText{Type: "output_text", Text: "bye", Annotations: []ResponsesAnnotation{}},
		)}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 7,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "does not match")
	})
}

// TestResponsesToolArgumentsCannotBecomeEmptyObject proves the review's
// corruption sequence — added opens the call, no argument deltas, the done
// item carries {"x":1} — delivers the done snapshot to the client as an
// input_json_delta, never an empty object (review-08 blocker 4).
func TestResponsesToolArgumentsCannotBecomeEmptyObject(t *testing.T) {
	state := anthropicLifecycleState(t)
	feedAnthropicCreated(t, state, 0)
	addedEvents, err := state.Convert(ResponseOutputItemAddedEvent{
		Type: "response.output_item.added", SequenceNumber: 1,
		OutputIndex: 0,
		Item: &ResponsesFunctionCallOutputItem{
			ID: "fc_1", Type: "function_call", Status: ResponsesItemInProgress,
			CallID: "call_1", Name: "f", Arguments: "",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := state.Convert(ResponseOutputItemDoneEvent{
		Type: "response.output_item.done", SequenceNumber: 2,
		OutputIndex: 0,
		Item: &ResponsesFunctionCallOutputItem{
			ID: "fc_1", Type: "function_call", Status: ResponsesItemCompleted,
			CallID: "call_1", Name: "f", Arguments: `{"x":1}`,
		},
	})
	if err != nil {
		t.Fatalf("done with real arguments rejected: %v", err)
	}
	events = append(addedEvents, events...)
	var sawStart, sawDelta, sawStop bool
	var deltaJSON string
	for _, event := range events {
		switch event.Type {
		case AnthropicStreamEventTypeContentBlockStart:
			sawStart = true
		case AnthropicStreamEventTypeContentBlockDelta:
			if event.Delta != nil && event.Delta.Type == AnthropicStreamDeltaTypeInputJSONDelta {
				sawDelta = true
				deltaJSON = *event.Delta.PartialJSON
			}
		case AnthropicStreamEventTypeContentBlockStop:
			sawStop = true
		}
	}
	if !sawStart || !sawStop {
		t.Fatalf("missing block lifecycle: %+v", events)
	}
	if !sawDelta {
		t.Fatalf("done snapshot must be delivered as input_json_delta: %+v", events)
	}
	if deltaJSON != `{"x":1}` {
		t.Fatalf("tool input = %q, want the done snapshot {\"x\":1}", deltaJSON)
	}
}

// TestResponsesStreamOfficialLifecycleStillConverts proves the official
// lifecycle — created, item added, content part added, deltas, done events,
// completed — converts cleanly with the terminal envelope reconciliation.
func TestResponsesStreamOfficialLifecycleStillConverts(t *testing.T) {
	state := anthropicLifecycleState(t)
	feedAnthropicCreated(t, state, 0)
	if _, err := state.Convert(ResponseOutputItemAddedEvent{
		Type: "response.output_item.added", SequenceNumber: 1,
		OutputIndex: 0,
		Item:        anthropicMessageItem("m1"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseContentPartAddedEvent{
		Type: "response.content_part.added", SequenceNumber: 2,
		ItemID:       "m1",
		OutputIndex:  0,
		ContentIndex: 0,
		Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseTextDeltaEvent{
		Type: "response.output_text.delta", SequenceNumber: 3,
		ItemID:       "m1",
		OutputIndex:  0,
		ContentIndex: 0,
		Delta:        "hi",
		Logprobs:     []ResponsesTextLogprob{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseTextDoneEvent{
		Type: "response.output_text.done", SequenceNumber: 4,
		ItemID:       "m1",
		OutputIndex:  0,
		ContentIndex: 0,
		Text:         "hi",
		Logprobs:     []ResponsesTextLogprob{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseContentPartDoneEvent{
		Type: "response.content_part.done", SequenceNumber: 5,
		ItemID:       "m1",
		OutputIndex:  0,
		ContentIndex: 0,
		Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "hi", Annotations: []ResponsesAnnotation{}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseOutputItemDoneEvent{
		Type: "response.output_item.done", SequenceNumber: 6,
		OutputIndex: 0,
		Item: anthropicMessageItem("m1",
			&ResponsesOutputText{Type: "output_text", Text: "hi", Annotations: []ResponsesAnnotation{}},
		),
	}); err != nil {
		t.Fatal(err)
	}
	envelope := anthropicLifecycleEnvelope("resp_1")
	envelope.Status = "completed"
	envelope.Output = []ResponsesOutputItem{anthropicMessageItem("m1",
		&ResponsesOutputText{Type: "output_text", Text: "hi", Annotations: []ResponsesAnnotation{}},
	)}
	events, err := state.Convert(ResponseCompletedEvent{
		Type: "response.completed", SequenceNumber: 7,
		Response: envelope,
	})
	if err != nil {
		t.Fatalf("official lifecycle rejected: %v", err)
	}
	var sawDelta, sawStop bool
	for _, event := range events {
		switch event.Type {
		case AnthropicStreamEventTypeMessageDelta:
			sawDelta = true
		case AnthropicStreamEventTypeMessageStop:
			sawStop = true
		}
	}
	if !sawDelta || !sawStop {
		t.Fatalf("missing terminal events: %+v", events)
	}
	if _, err := state.FinalizeEOF(); err != nil {
		t.Fatalf("finalize after terminal = %v, want clean", err)
	}
}

// TestResponsesStreamLifecycleErrorMatrix closes the remaining error-branch
// coverage of the lifecycle and reconciliation machinery: every violation
// class is corrupt upstream wire (review-08 blockers 3 and 4).
func TestResponsesStreamLifecycleErrorMatrix(t *testing.T) {
	textPart := func() *ResponsesStreamOutputTextPart {
		return &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}}
	}
	refusalPart := func() *ResponsesStreamRefusalPart {
		return &ResponsesStreamRefusalPart{Type: "refusal", Refusal: ""}
	}
	messageItem := func(id string) *ResponsesOutputMessage {
		return &ResponsesOutputMessage{
			ID: id, Type: "message", Role: "assistant",
			Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
		}
	}
	fcItem := func(id, callID, name, arguments string) *ResponsesFunctionCallOutputItem {
		return &ResponsesFunctionCallOutputItem{
			ID: id, Type: "function_call", Status: ResponsesItemInProgress,
			CallID: callID, Name: name, Arguments: arguments,
		}
	}

	t.Run("created_at identity drift", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.CreatedAt = 2
		_, err := state.Convert(ResponseInProgressEvent{
			Type: "response.in_progress", SequenceNumber: 1,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "created_at")
	})

	t.Run("completed identity drift", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		envelope := anthropicLifecycleEnvelope("resp_2")
		envelope.Status = "completed"
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 1,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "resp_1")
	})

	t.Run("duplicate function item", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		item := fcItem("fc_1", "call_1", "f", "")
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        item,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 2,
			OutputIndex: 1,
			Item:        item,
		})
		assertAnthropicWireError(t, err, "duplicate")
	})

	t.Run("item done for unknown item", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("ghost"),
		})
		assertAnthropicWireError(t, err, "unknown item")
	})

	t.Run("item done type mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 2,
			OutputIndex: 0,
			Item: &ResponsesReasoningOutputItem{
				ID: "m1", Type: "reasoning", Status: ResponsesItemCompleted,
				Summary: []ResponsesReasoningSummary{},
			},
		})
		assertAnthropicWireError(t, err, "type")
	})

	t.Run("item done output index mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 2,
			OutputIndex: 1,
			Item:        messageItem("m1"),
		})
		assertAnthropicWireError(t, err, "output index")
	})

	t.Run("item done refusal content mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         refusalPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseRefusalDeltaEvent{
			Type: "response.refusal.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "no",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseRefusalDoneEvent{
			Type: "response.refusal.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Refusal:      "no",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 5,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         refusalPart(),
		}); err != nil {
			t.Fatal(err)
		}
		done := anthropicMessageItem("m1",
			&ResponsesOutputRefusal{Type: "refusal", Refusal: "yes"},
		)
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 6,
			OutputIndex: 0,
			Item:        done,
		})
		assertAnthropicWireError(t, err, "does not match")
	})

	t.Run("item done extra content part", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 5,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		done := anthropicMessageItem("m1",
			&ResponsesOutputText{Type: "output_text", Text: "hi", Annotations: []ResponsesAnnotation{}},
			&ResponsesOutputText{Type: "output_text", Text: "extra", Annotations: []ResponsesAnnotation{}},
		)
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 6,
			OutputIndex: 0,
			Item:        done,
		})
		assertAnthropicWireError(t, err, "never opened")
	})

	t.Run("arguments snapshot conflict", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        fcItem("fc_1", "call_1", "f", ""),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Delta:       `{"x":1}`,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: 3,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Arguments:   `{"x":2}`,
		})
		assertAnthropicWireError(t, err, "do not match")
	})

	t.Run("arguments deltas not a prefix", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        fcItem("fc_1", "call_1", "f", ""),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Delta:       `not json`,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: 3,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Arguments:   `{"x":1}`,
		})
		assertAnthropicWireError(t, err, "do not match")
	})

	t.Run("arguments delta unknown item", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		_, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 1,
			ItemID:      "ghost",
			OutputIndex: 0,
			Delta:       `{}`,
		})
		assertAnthropicWireError(t, err, "unknown item")
	})

	t.Run("arguments delta output index mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        fcItem("fc_1", "call_1", "f", ""),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 1,
			Delta:       `{}`,
		})
		assertAnthropicWireError(t, err, "output index")
	})

	t.Run("arguments done unknown item", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		_, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: 1,
			ItemID:      "ghost",
			OutputIndex: 0,
			Arguments:   `{}`,
		})
		assertAnthropicWireError(t, err, "unknown item")
	})

	t.Run("text done no open block", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		_, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 1,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "hi",
			Logprobs:     []ResponsesTextLogprob{},
		})
		assertAnthropicWireError(t, err, "no open content block")
	})

	t.Run("text done for refusal part", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         refusalPart(),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "hi",
			Logprobs:     []ResponsesTextLogprob{},
		})
		assertAnthropicWireError(t, err, "refusal")
	})

	t.Run("refusal done no open block", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		_, err := state.Convert(ResponseRefusalDoneEvent{
			Type: "response.refusal.done", SequenceNumber: 1,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Refusal:      "no",
		})
		assertAnthropicWireError(t, err, "no open content block")
	})

	t.Run("refusal done for text part", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseRefusalDoneEvent{
			Type: "response.refusal.done", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Refusal:      "no",
		})
		assertAnthropicWireError(t, err, "output_text")
	})

	t.Run("refusal delta no open block", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		_, err := state.Convert(ResponseRefusalDeltaEvent{
			Type: "response.refusal.delta", SequenceNumber: 1,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "no",
		})
		assertAnthropicWireError(t, err, "no open content block")
	})

	t.Run("content part done no open block", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		_, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 1,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		})
		assertAnthropicWireError(t, err, "no open block")
	})

	t.Run("incomplete with open part", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "incomplete"
		envelope.IncompleteDetails = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
		_, err := state.Convert(ResponseIncompleteEvent{
			Type: "response.incomplete", SequenceNumber: 3,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "done")
	})

	t.Run("failed before created", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "failed"
		envelope.Error = &ResponsesEnvelopeError{Code: "server_error", Message: "boom"}
		_, err := state.Convert(ResponseFailedEvent{
			Type: "response.failed", SequenceNumber: 0,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "response.created")
	})

	t.Run("terminal envelope item type mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 2,
			OutputIndex: 0,
			Item:        anthropicMessageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Output = []ResponsesOutputItem{fcItem("m1", "call_1", "f", "{}")}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 3,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "type")
	})

	t.Run("terminal envelope function call not closed", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        fcItem("fc_1", "call_1", "f", ""),
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Output = []ResponsesOutputItem{fcItem("fc_1", "call_1", "f", "{}")}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 2,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "done")
	})

	t.Run("terminal envelope function call identity mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        fcItem("fc_1", "call_1", "f", ""),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Delta:       `{}`,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: 3,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Arguments:   `{}`,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 4,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemCompleted,
				CallID: "call_1", Name: "f", Arguments: `{}`,
			},
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Output = []ResponsesOutputItem{fcItem("fc_1", "call_2", "f", "{}")}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 5,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "identity")
	})

	t.Run("terminal envelope function call arguments mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        fcItem("fc_1", "call_1", "f", ""),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Delta:       `{"x":1}`,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: 3,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Arguments:   `{"x":1}`,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 4,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemCompleted,
				CallID: "call_1", Name: "f", Arguments: `{"x":1}`,
			},
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Output = []ResponsesOutputItem{fcItem("fc_1", "call_1", "f", `{"x":2}`)}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 5,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "arguments")
	})
}

// TestResponsesToolArgumentsSemanticEquality proves the arguments
// reconciliation accepts a snapshot that is semantically equal but not a
// byte-prefix of the accumulated buffer (e.g. re-serialized whitespace):
// review-08 blocker 4 requires byte-or-semantic reconciliation.
func TestResponsesToolArgumentsSemanticEquality(t *testing.T) {
	state := anthropicLifecycleState(t)
	feedAnthropicCreated(t, state, 0)
	if _, err := state.Convert(ResponseOutputItemAddedEvent{
		Type: "response.output_item.added", SequenceNumber: 1,
		OutputIndex: 0,
		Item: &ResponsesFunctionCallOutputItem{
			ID: "fc_1", Type: "function_call", Status: ResponsesItemInProgress,
			CallID: "call_1", Name: "f", Arguments: "",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
		Type: "response.function_call_arguments.delta", SequenceNumber: 2,
		ItemID:      "fc_1",
		OutputIndex: 0,
		Delta:       `{"x": 1}`,
	}); err != nil {
		t.Fatal(err)
	}
	events, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
		Type: "response.function_call_arguments.done", SequenceNumber: 3,
		ItemID:      "fc_1",
		OutputIndex: 0,
		Arguments:   `{"x":1}`,
	})
	if err != nil {
		t.Fatalf("semantically equal snapshot rejected: %v", err)
	}
	// The buffer already covered the input: no suffix delta is emitted.
	for _, event := range events {
		if event.Delta != nil && event.Delta.Type == AnthropicStreamDeltaTypeInputJSONDelta {
			t.Fatalf("unexpected suffix delta: %+v", events)
		}
	}
}

// TestResponsesStreamLifecycleErrorMatrix2 closes the remaining error
// branches of the lifecycle machinery (review-08 blockers 3 and 4).
func TestResponsesStreamLifecycleErrorMatrix2(t *testing.T) {
	textPart := func() *ResponsesStreamOutputTextPart {
		return &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}}
	}
	refusalPart := func() *ResponsesStreamRefusalPart {
		return &ResponsesStreamRefusalPart{Type: "refusal", Refusal: ""}
	}
	messageItem := func(id string) *ResponsesOutputMessage {
		return &ResponsesOutputMessage{
			ID: id, Type: "message", Role: "assistant",
			Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
		}
	}
	fcItem := func(id, callID, name, arguments string) *ResponsesFunctionCallOutputItem {
		return &ResponsesFunctionCallOutputItem{
			ID: id, Type: "function_call", Status: ResponsesItemInProgress,
			CallID: callID, Name: name, Arguments: arguments,
		}
	}

	t.Run("created with inconsistent usage", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Usage = &ResponsesUsage{
			InputTokens: 3, TotalTokens: 3,
			InputTokensDetails: &UsageInputTokensDetails{CachedTokens: 10},
		}
		_, err := state.Convert(ResponseCreatedEvent{
			Type: "response.created", SequenceNumber: 0,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "usage")
	})

	t.Run("unsupported item type on add", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		_, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputResultItem{
				ID: "r1", Type: "function_call_output", Status: ResponsesItemCompleted,
				CallID: "call_1", Output: ResponsesFunctionOutput{Text: new("ok")},
			},
		})
		if _, ok := errors.AsType[*UnsupportedFeatureError](err); !ok {
			t.Fatalf("err = %T %v, want *UnsupportedFeatureError", err, err)
		}
	})

	t.Run("content part added output index mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  1,
			ContentIndex: 0,
			Part:         textPart(),
		})
		assertAnthropicWireError(t, err, "output index")
	})

	t.Run("content part done output index mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  1,
			ContentIndex: 0,
			Part:         textPart(),
		})
		assertAnthropicWireError(t, err, "output index")
	})

	t.Run("refusal delta output index mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         refusalPart(),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseRefusalDeltaEvent{
			Type: "response.refusal.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  1,
			ContentIndex: 0,
			Delta:        "no",
		})
		assertAnthropicWireError(t, err, "output index")
	})

	t.Run("refusal done output index mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         refusalPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseRefusalDeltaEvent{
			Type: "response.refusal.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "no",
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseRefusalDoneEvent{
			Type: "response.refusal.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  1,
			ContentIndex: 0,
			Refusal:      "no",
		})
		assertAnthropicWireError(t, err, "output index")
	})

	t.Run("item done name mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        fcItem("fc_1", "call_1", "f", ""),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Delta:       `{}`,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: 3,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Arguments:   `{}`,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 4,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemCompleted,
				CallID: "call_1", Name: "g", Arguments: `{}`,
			},
		})
		assertAnthropicWireError(t, err, "name")
	})

	t.Run("item done without call identity", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		item := fcItem("fc_1", "", "", "")
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        item,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 2,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemCompleted,
				CallID: "call_1", Name: "f", Arguments: `{}`,
			},
		})
		assertAnthropicWireError(t, err, "identity")
	})

	t.Run("item done non-object arguments", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        fcItem("fc_1", "call_1", "f", ""),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 2,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemCompleted,
				CallID: "call_1", Name: "f", Arguments: `[1,2]`,
			},
		})
		// Non-object model-generated arguments cannot be represented as
		// tool_use.input: a LOCAL unrepresentable output, never corrupt
		// upstream wire (review-z commit 2).
		if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
			t.Fatalf("err = %T %v, want *UnrepresentableError", err, err)
		}
	})

	t.Run("item done message part type mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 5,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		done := anthropicMessageItem("m1",
			&ResponsesOutputRefusal{Type: "refusal", Refusal: "hi"},
		)
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 6,
			OutputIndex: 0,
			Item:        done,
		})
		assertAnthropicWireError(t, err, "output_text")
	})

	t.Run("arguments conflict different key count", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        fcItem("fc_1", "call_1", "f", ""),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Delta:       `{"x":1}`,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: 3,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Arguments:   `{"x":1,"y":2}`,
		})
		assertAnthropicWireError(t, err, "do not match")
	})

	t.Run("terminal envelope part never observed", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 2,
			OutputIndex: 0,
			Item:        anthropicMessageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Output = []ResponsesOutputItem{anthropicMessageItem("m1",
			&ResponsesOutputText{Type: "output_text", Text: "hi", Annotations: []ResponsesAnnotation{}},
		)}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 3,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "never observed")
	})

	t.Run("terminal envelope part type mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 5,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 6,
			OutputIndex: 0,
			Item: anthropicMessageItem("m1",
				&ResponsesOutputText{Type: "output_text", Text: "hi", Annotations: []ResponsesAnnotation{}},
			),
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Output = []ResponsesOutputItem{anthropicMessageItem("m1",
			&ResponsesOutputRefusal{Type: "refusal", Refusal: "hi"},
		)}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 7,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "want")
	})

	t.Run("terminal envelope refusal mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         refusalPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseRefusalDeltaEvent{
			Type: "response.refusal.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "no",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseRefusalDoneEvent{
			Type: "response.refusal.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Refusal:      "no",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 5,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         refusalPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 6,
			OutputIndex: 0,
			Item: anthropicMessageItem("m1",
				&ResponsesOutputRefusal{Type: "refusal", Refusal: "no"},
			),
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Output = []ResponsesOutputItem{anthropicMessageItem("m1",
			&ResponsesOutputRefusal{Type: "refusal", Refusal: "yes"},
		)}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 7,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "does not match")
	})

	t.Run("failed with empty error message", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "failed"
		envelope.Error = &ResponsesEnvelopeError{Code: "", Message: ""}
		events, err := state.Convert(ResponseFailedEvent{
			Type: "response.failed", SequenceNumber: 1,
			Response: envelope,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Error == nil ||
			events[0].Error.Message != "upstream response failed" {
			t.Fatalf("failed events = %+v", events)
		}
	})
}

// TestResponsesStreamLifecycleErrorMatrix3 closes the last reachable error
// branches of the lifecycle and reconciliation machinery (review-08 blockers
// 3 and 4).
func TestResponsesStreamLifecycleErrorMatrix3(t *testing.T) {
	textPart := func() *ResponsesStreamOutputTextPart {
		return &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}}
	}
	messageItem := func(id string) *ResponsesOutputMessage {
		return &ResponsesOutputMessage{
			ID: id, Type: "message", Role: "assistant",
			Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
		}
	}

	t.Run("item done fewer content parts", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 5,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 6,
			OutputIndex: 0,
			Item:        anthropicMessageItem("m1"),
		})
		assertAnthropicWireError(t, err, "content count")
	})

	t.Run("text delta no open block", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		_, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 1,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "hi",
			Logprobs:     []ResponsesTextLogprob{},
		})
		assertAnthropicWireError(t, err, "no open content block")
	})

	t.Run("text done output index mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  1,
			ContentIndex: 0,
			Text:         "hi",
			Logprobs:     []ResponsesTextLogprob{},
		})
		assertAnthropicWireError(t, err, "output index")
	})

	t.Run("arguments done output index mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemInProgress,
				CallID: "call_1", Name: "f", Arguments: "",
			},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 1,
			Arguments:   `{}`,
		})
		assertAnthropicWireError(t, err, "output index")
	})

	t.Run("completed phase gate rejection", func(t *testing.T) {
		// The added item carries no phase; the terminal envelope introduces
		// one, entering the phase decision at the terminal.
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
			1710000000,
		)
		feedAnthropicCreated(t, state, 0)
		item := messageItem("m1")
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        item,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 2,
			OutputIndex: 0,
			Item:        anthropicMessageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		withPhase := messageItem("m1")
		withPhase.Phase = "commentary"
		withPhase.Status = ResponsesItemCompleted
		envelope.Output = []ResponsesOutputItem{withPhase}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 3,
			Response: envelope,
		})
		if _, ok := errors.AsType[*UnsupportedFeatureError](err); !ok {
			t.Fatalf("err = %T %v, want *UnsupportedFeatureError", err, err)
		}
	})

	t.Run("completed controls loss rejection", func(t *testing.T) {
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
			1710000000,
		)
		feedAnthropicCreated(t, state, 0)
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		background := true
		envelope.Background = &background
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 1,
			Response: envelope,
		})
		if _, ok := errors.AsType[*UnsupportedFeatureError](err); !ok {
			t.Fatalf("err = %T %v, want *UnsupportedFeatureError", err, err)
		}
	})

	t.Run("completed inconsistent usage", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Usage = &ResponsesUsage{
			InputTokens: 3, TotalTokens: 3,
			InputTokensDetails: &UsageInputTokensDetails{CachedTokens: 10},
		}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 1,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "usage")
	})

	t.Run("terminal envelope item never observed", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Output = []ResponsesOutputItem{anthropicMessageItem("ghost")}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 1,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "never observed")
	})

	t.Run("terminal envelope message count mismatch", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "hi",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 5,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 6,
			OutputIndex: 0,
			Item: anthropicMessageItem("m1",
				&ResponsesOutputText{Type: "output_text", Text: "hi", Annotations: []ResponsesAnnotation{}},
			),
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Output = []ResponsesOutputItem{anthropicMessageItem("m1")}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 7,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "content count")
	})

	t.Run("terminal envelope function call non-object arguments", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemInProgress,
				CallID: "call_1", Name: "f", Arguments: "",
			},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDeltaEvent{
			Type: "response.function_call_arguments.delta", SequenceNumber: 2,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Delta:       `{}`,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseFunctionCallArgumentsDoneEvent{
			Type: "response.function_call_arguments.done", SequenceNumber: 3,
			ItemID:      "fc_1",
			OutputIndex: 0,
			Arguments:   `{}`,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 4,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemCompleted,
				CallID: "call_1", Name: "f", Arguments: `{}`,
			},
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Output = []ResponsesOutputItem{&ResponsesFunctionCallOutputItem{
			ID: "fc_1", Type: "function_call", Status: ResponsesItemCompleted,
			CallID: "call_1", Name: "f", Arguments: `[1,2]`,
		}}
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 5,
			Response: envelope,
		})
		// Non-object terminal-envelope arguments are a LOCAL unrepresentable
		// output (review-z commit 2); only snapshot-vs-accumulated identity
		// drift remains corrupt upstream wire.
		if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
			t.Fatalf("err = %T %v, want *UnrepresentableError", err, err)
		}
	})

	t.Run("failed identity drift", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		envelope := anthropicLifecycleEnvelope("resp_2")
		envelope.Status = "failed"
		envelope.Error = &ResponsesEnvelopeError{Code: "server_error", Message: "boom"}
		_, err := state.Convert(ResponseFailedEvent{
			Type: "response.failed", SequenceNumber: 1,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "resp_1")
	})
}

// TestResponsesStreamIncompleteErrorBranches covers the incomplete terminal's
// phase, controls, and usage error returns (review-08 blockers 3 and 4).
func TestResponsesStreamIncompleteErrorBranches(t *testing.T) {
	t.Run("incomplete phase gate rejection", func(t *testing.T) {
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
			1710000000,
		)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesOutputMessage{
				ID: "m1", Type: "message", Role: "assistant",
				Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 2,
			OutputIndex: 0,
			Item:        anthropicMessageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "incomplete"
		envelope.IncompleteDetails = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
		item := &ResponsesOutputMessage{
			ID: "m1", Type: "message", Role: "assistant",
			Status: ResponsesItemCompleted, Phase: "commentary",
			Content: ResponsesOutputContentParts{},
		}
		envelope.Output = []ResponsesOutputItem{item}
		_, err := state.Convert(ResponseIncompleteEvent{
			Type: "response.incomplete", SequenceNumber: 3,
			Response: envelope,
		})
		if _, ok := errors.AsType[*UnsupportedFeatureError](err); !ok {
			t.Fatalf("err = %T %v, want *UnsupportedFeatureError", err, err)
		}
	})

	t.Run("incomplete controls loss rejection", func(t *testing.T) {
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
			1710000000,
		)
		feedAnthropicCreated(t, state, 0)
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "incomplete"
		envelope.IncompleteDetails = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
		background := true
		envelope.Background = &background
		_, err := state.Convert(ResponseIncompleteEvent{
			Type: "response.incomplete", SequenceNumber: 1,
			Response: envelope,
		})
		if _, ok := errors.AsType[*UnsupportedFeatureError](err); !ok {
			t.Fatalf("err = %T %v, want *UnsupportedFeatureError", err, err)
		}
	})

	t.Run("incomplete inconsistent usage", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "incomplete"
		envelope.IncompleteDetails = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
		envelope.Usage = &ResponsesUsage{
			InputTokens: 3, TotalTokens: 3,
			InputTokensDetails: &UsageInputTokensDetails{CachedTokens: 10},
		}
		_, err := state.Convert(ResponseIncompleteEvent{
			Type: "response.incomplete", SequenceNumber: 1,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "usage")
	})
}

// TestResponsesStreamZeroDeltaPartReconciliation proves the coherent
// zero-delta rule for text/refusal parts: a part that accumulated nothing
// reconciles with the empty string — a consistent "" snapshot is accepted
// and a contradictory non-empty snapshot is corrupt upstream wire, in every
// done and envelope position (review-08 blocker 4).
func TestResponsesStreamZeroDeltaPartReconciliation(t *testing.T) {
	textPart := func() *ResponsesStreamOutputTextPart {
		return &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}}
	}
	messageItem := func(id string) *ResponsesOutputMessage {
		return &ResponsesOutputMessage{
			ID: id, Type: "message", Role: "assistant",
			Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
		}
	}
	// Setup: created + message item + an OPEN text part with no deltas.
	setup := func(t *testing.T) (*anthropicResponsesStreamState, responsePartKey) {
		t.Helper()
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		return state, responsePartKey{ItemID: "m1", ContentIndex: 0}
	}

	t.Run("consistent empty text done accepted", func(t *testing.T) {
		state, _ := setup(t)
		if _, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatalf("zero-delta empty text done rejected: %v", err)
		}
	})

	t.Run("contradictory item done text rejected", func(t *testing.T) {
		state, _ := setup(t)
		if _, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		done := anthropicMessageItem("m1",
			&ResponsesOutputText{Type: "output_text", Text: "phantom", Annotations: []ResponsesAnnotation{}},
		)
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 5,
			OutputIndex: 0,
			Item:        done,
		})
		assertAnthropicWireError(t, err, "does not match")
	})

	t.Run("terminal envelope consistent empty text accepted", func(t *testing.T) {
		state, _ := setup(t)
		if _, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 5,
			OutputIndex: 0,
			Item: anthropicMessageItem("m1",
				&ResponsesOutputText{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
			),
		}); err != nil {
			t.Fatal(err)
		}
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		envelope.Output = []ResponsesOutputItem{anthropicMessageItem("m1",
			&ResponsesOutputText{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
		)}
		if _, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 6,
			Response: envelope,
		}); err != nil {
			t.Fatalf("zero-delta empty terminal text rejected: %v", err)
		}
	})

	t.Run("duplicate message item done rejected", func(t *testing.T) {
		state, _ := setup(t)
		if _, err := state.Convert(ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Text:         "",
			Logprobs:     []ResponsesTextLogprob{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: 4,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		done := anthropicMessageItem("m1",
			&ResponsesOutputText{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
		)
		if _, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 5,
			OutputIndex: 0,
			Item:        done,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: 6,
			OutputIndex: 0,
			Item:        done,
		})
		assertAnthropicWireError(t, err, "duplicate")
	})
}

// TestResponsesStreamZeroDeltaRefusalReconciliation covers the zero-delta
// rule for refusal parts (review-08 blocker 4).
func TestResponsesStreamZeroDeltaRefusalReconciliation(t *testing.T) {
	refusalPart := func() *ResponsesStreamRefusalPart {
		return &ResponsesStreamRefusalPart{Type: "refusal", Refusal: ""}
	}
	state := anthropicLifecycleState(t)
	feedAnthropicCreated(t, state, 0)
	if _, err := state.Convert(ResponseOutputItemAddedEvent{
		Type: "response.output_item.added", SequenceNumber: 1,
		OutputIndex: 0,
		Item: &ResponsesOutputMessage{
			ID: "m1", Type: "message", Role: "assistant",
			Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(ResponseContentPartAddedEvent{
		Type: "response.content_part.added", SequenceNumber: 2,
		ItemID:       "m1",
		OutputIndex:  0,
		ContentIndex: 0,
		Part:         refusalPart(),
	}); err != nil {
		t.Fatal(err)
	}
	// A zero-delta part reconciles with an empty done snapshot.
	if _, err := state.Convert(ResponseRefusalDoneEvent{
		Type: "response.refusal.done", SequenceNumber: 3,
		ItemID:       "m1",
		OutputIndex:  0,
		ContentIndex: 0,
		Refusal:      "",
	}); err != nil {
		t.Fatalf("zero-delta empty refusal done rejected: %v", err)
	}
	// A contradictory non-empty done snapshot is corrupt wire.
	if _, err := state.Convert(ResponseRefusalDoneEvent{
		Type: "response.refusal.done", SequenceNumber: 4,
		ItemID:       "m1",
		OutputIndex:  0,
		ContentIndex: 0,
		Refusal:      "phantom",
	}); err == nil {
		t.Fatal("contradictory refusal done accepted")
	}
}

// TestResponsesStreamItemClosureAndPartOwnership proves every observed item
// must be closed by output_item.done before the terminal, and content parts
// may only attach to message items (review-08 blocker 3).
func TestResponsesStreamItemClosureAndPartOwnership(t *testing.T) {
	t.Run("message item never done at terminal", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesOutputMessage{
				ID: "m1", Type: "message", Role: "assistant",
				Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
			},
		}); err != nil {
			t.Fatal(err)
		}
		// All parts closed, but the item done never arrives.
		envelope := anthropicLifecycleEnvelope("resp_1")
		envelope.Status = "completed"
		_, err := state.Convert(ResponseCompletedEvent{
			Type: "response.completed", SequenceNumber: 2,
			Response: envelope,
		})
		assertAnthropicWireError(t, err, "output_item.done")
	})

	t.Run("content part for function call item", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemInProgress,
				CallID: "call_1", Name: "f", Arguments: "",
			},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "fc_1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}},
		})
		assertAnthropicWireError(t, err, "non-message")
	})
}

// TestResponsesStreamDeltaPartTypeMatching proves text and refusal deltas
// must target their own part type: a misrouted delta would drift the emitted
// block from every done snapshot (review-08 blocker 4).
func TestResponsesStreamDeltaPartTypeMatching(t *testing.T) {
	refusalPart := func() *ResponsesStreamRefusalPart {
		return &ResponsesStreamRefusalPart{Type: "refusal", Refusal: ""}
	}
	textPart := func() *ResponsesStreamOutputTextPart {
		return &ResponsesStreamOutputTextPart{Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{}}
	}
	messageItem := func(id string) *ResponsesOutputMessage {
		return &ResponsesOutputMessage{
			ID: id, Type: "message", Role: "assistant",
			Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
		}
	}

	t.Run("text delta on refusal part", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         refusalPart(),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "x",
			Logprobs:     []ResponsesTextLogprob{},
		})
		assertAnthropicWireError(t, err, "refusal")
	})

	t.Run("refusal delta on text part", func(t *testing.T) {
		state := anthropicLifecycleState(t)
		feedAnthropicCreated(t, state, 0)
		if _, err := state.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: 1,
			OutputIndex: 0,
			Item:        messageItem("m1"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.Convert(ResponseContentPartAddedEvent{
			Type: "response.content_part.added", SequenceNumber: 2,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Part:         textPart(),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.Convert(ResponseRefusalDeltaEvent{
			Type: "response.refusal.delta", SequenceNumber: 3,
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "no",
		})
		assertAnthropicWireError(t, err, "output_text")
	})
}

// TestResponsesFunctionArgumentsDoneRejectsName proves an inbound
// function_call_arguments.done carrying the private superset name field is
// corrupt upstream wire (review-08 blocker 5).
func TestResponsesFunctionArgumentsDoneRejectsName(t *testing.T) {
	_, err := decodeResponsesSSEEvent([]byte(
		`{"type":"response.function_call_arguments.done","sequence_number":1,"item_id":"fc_1","output_index":0,"arguments":"{}","name":"f"}`,
	))
	var wireErr *UpstreamWireError
	if !errors.As(err, &wireErr) {
		t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
	}
	if wireErr.Protocol != UpstreamResponses {
		t.Fatalf("protocol = %v, want responses", wireErr.Protocol)
	}
	// The official shape without name still decodes.
	if _, err := decodeResponsesSSEEvent([]byte(
		`{"type":"response.function_call_arguments.done","sequence_number":1,"item_id":"fc_1","output_index":0,"arguments":"{}"}`,
	)); err != nil {
		t.Fatalf("official done rejected: %v", err)
	}
}
