package transcode

// Review-z commit 3 tests: the reusable Responses stream lifecycle FSM
// (every rejection class has a transition test), the seven-dimension total
// budget (the empty-part attack cannot allocate beyond it), and the atomic
// terminal batch (an oversized batch is never partially delivered).

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fsmCreated builds a response.created event for the FSM tests.
func fsmCreated(seq int64) ResponseCreatedEvent {
	return ResponseCreatedEvent{
		EventBase: EventBase{Type: "response.created", SequenceNumber: seq},
		Response:  fsmEnvelope("resp_1", "in_progress"),
	}
}

// fsmEnvelope builds a minimal valid envelope.
func fsmEnvelope(id, status string) ResponseEnvelope {
	return ResponseEnvelope{
		ID:        id,
		Object:    "response",
		CreatedAt: 1,
		Status:    status,
		Model:     "m",
		Output:    []ResponsesOutputItem{},
	}
}

// fsmMessageAdded builds an output_item.added event for a message item.
func fsmMessageAdded(seq, index int64, id string) ResponseOutputItemAddedEvent {
	return ResponseOutputItemAddedEvent{
		EventBase:   EventBase{Type: "response.output_item.added", SequenceNumber: seq},
		OutputIndex: index,
		Item: &ResponsesOutputMessage{
			ID: id, Type: "message", Role: "assistant", Status: ResponsesItemInProgress,
			Content: ResponsesOutputContentParts{},
		},
	}
}

// fsmCallAdded builds an output_item.added event for a function call.
func fsmCallAdded(seq, index int64, id, callID string) ResponseOutputItemAddedEvent {
	return ResponseOutputItemAddedEvent{
		EventBase:   EventBase{Type: "response.output_item.added", SequenceNumber: seq},
		OutputIndex: index,
		Item: &ResponsesFunctionCallOutputItem{
			ID: id, Type: "function_call", Status: ResponsesItemInProgress,
			CallID: callID, Name: "f", Arguments: "",
		},
	}
}

// fsmTextPartAdded builds a content_part.added event for an output_text part.
func fsmTextPartAdded(seq, index, contentIndex int64, itemID string) ResponseContentPartAddedEvent {
	return ResponseContentPartAddedEvent{
		EventBase:    EventBase{Type: "response.content_part.added", SequenceNumber: seq},
		ItemID:       itemID,
		OutputIndex:  index,
		ContentIndex: contentIndex,
		Part: &ResponsesStreamOutputTextPart{
			Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{},
		},
	}
}

// fsmTextDelta builds an output_text.delta event.
func fsmTextDelta(seq, index, contentIndex int64, itemID, delta string) ResponseTextDeltaEvent {
	return ResponseTextDeltaEvent{
		EventBase:    EventBase{Type: "response.output_text.delta", SequenceNumber: seq},
		ItemID:       itemID,
		OutputIndex:  index,
		ContentIndex: contentIndex,
		Delta:        delta,
		Logprobs:     []ResponsesTextLogprob{},
	}
}

// fsmTextDone builds an output_text.done event.
func fsmTextDone(seq, index, contentIndex int64, itemID, text string) ResponseTextDoneEvent {
	return ResponseTextDoneEvent{
		EventBase:    EventBase{Type: "response.output_text.done", SequenceNumber: seq},
		ItemID:       itemID,
		OutputIndex:  index,
		ContentIndex: contentIndex,
		Text:         text,
		Logprobs:     []ResponsesTextLogprob{},
	}
}

// fsmTextPartDone builds a content_part.done event.
func fsmTextPartDone(seq, index, contentIndex int64, itemID string) ResponseContentPartDoneEvent {
	return ResponseContentPartDoneEvent{
		EventBase:    EventBase{Type: "response.content_part.done", SequenceNumber: seq},
		ItemID:       itemID,
		OutputIndex:  index,
		ContentIndex: contentIndex,
		Part: &ResponsesStreamOutputTextPart{
			Type: "output_text", Text: "hi", Annotations: []ResponsesAnnotation{},
		},
	}
}

// fsmItemDone builds an output_item.done event for a message item.
func fsmItemDone(seq, index int64, id string) ResponseOutputItemDoneEvent {
	return ResponseOutputItemDoneEvent{
		EventBase:   EventBase{Type: "response.output_item.done", SequenceNumber: seq},
		OutputIndex: index,
		Item: &ResponsesOutputMessage{
			ID: id, Type: "message", Role: "assistant", Status: ResponsesItemCompleted,
			Content: ResponsesOutputContentParts{},
		},
	}
}

// fsmComplete builds a response.completed event.
func fsmComplete(seq int64) ResponseCompletedEvent {
	return ResponseCompletedEvent{
		EventBase: EventBase{Type: "response.completed", SequenceNumber: seq},
		Response:  fsmEnvelope("resp_1", "completed"),
	}
}

// assertFSMWire asserts a typed upstream-wire rejection.
func assertFSMWire(t *testing.T, err error) {
	t.Helper()
	var wireErr *UpstreamWireError
	if !errors.As(err, &wireErr) {
		t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
	}
}

// TestResponsesStreamFSMTransitionTable covers every rejection class of the
// transition table with one test per class (review-z commit 3).
func TestResponsesStreamFSMTransitionTable(t *testing.T) {
	// A full legal lifecycle passes.
	t.Run("legal lifecycle", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		seq := int64(0)
		events := []ResponsesSSEEvent{
			fsmCreated(seq),
		}
		seq++
		events = append(events, fsmMessageAdded(seq, 0, "m1"))
		seq++
		events = append(events, fsmTextPartAdded(seq, 0, 0, "m1"))
		seq++
		events = append(events, fsmTextDelta(seq, 0, 0, "m1", "hi"))
		seq++
		events = append(events, fsmTextDone(seq, 0, 0, "m1", "hi"))
		seq++
		events = append(events, fsmTextPartDone(seq, 0, 0, "m1"))
		seq++
		events = append(events, fsmItemDone(seq, 0, "m1"))
		seq++
		events = append(events, fsmComplete(seq))
		for _, event := range events {
			if err := fsm.Validate(event); err != nil {
				t.Fatalf("legal lifecycle rejected at %s: %v", event.EventType(), err)
			}
		}
	})

	// 1: any event before response.created, except a terminal error event.
	t.Run("event before created", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		assertFSMWire(t, fsm.Validate(fsmMessageAdded(0, 0, "m1")))
	})
	t.Run("error event before created allowed", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		if err := fsm.Validate(ResponseErrorEvent{
			EventBase: EventBase{Type: "error", SequenceNumber: 0},
			Code:      "c", Message: "m",
		}); err != nil {
			t.Fatalf("error event before created rejected: %v", err)
		}
	})

	// 2: duplicate response.created.
	t.Run("duplicate created", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		if err := fsm.Validate(fsmCreated(0)); err != nil {
			t.Fatal(err)
		}
		assertFSMWire(t, fsm.Validate(fsmCreated(1)))
	})

	// 3: duplicate or decreasing sequence numbers.
	t.Run("decreasing sequence", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		if err := fsm.Validate(fsmCreated(1)); err != nil {
			t.Fatal(err)
		}
		assertFSMWire(t, fsm.Validate(fsmCreated(1)))
		assertFSMWire(t, fsm.Validate(fsmMessageAdded(0, 0, "m1")))
	})

	// 4: duplicate item IDs.
	t.Run("duplicate item id", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0), fsmMessageAdded(1, 0, "m1"))
		assertFSMWire(t, fsm.Validate(fsmMessageAdded(2, 1, "m1")))
	})

	// 5: duplicate output indexes.
	t.Run("duplicate output index", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0), fsmMessageAdded(1, 0, "m1"))
		assertFSMWire(t, fsm.Validate(fsmMessageAdded(2, 0, "m2")))
	})

	// 6: part events for unknown, non-message, or completed items.
	t.Run("part for unknown item", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0))
		assertFSMWire(t, fsm.Validate(fsmTextPartAdded(1, 0, 0, "ghost")))
	})
	t.Run("part for completed item", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm,
			fsmCreated(0), fsmMessageAdded(1, 0, "m1"), fsmItemDone(2, 0, "m1"),
		)
		assertFSMWire(t, fsm.Validate(fsmTextPartAdded(3, 0, 0, "m1")))
	})

	// 7: duplicate content indexes within an item.
	t.Run("duplicate content index", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm,
			fsmCreated(0), fsmMessageAdded(1, 0, "m1"), fsmTextPartAdded(2, 0, 0, "m1"),
		)
		assertFSMWire(t, fsm.Validate(fsmTextPartAdded(3, 0, 0, "m1")))
	})

	// 8: text events targeting refusal parts and vice versa.
	t.Run("text delta targets refusal part", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm,
			fsmCreated(0), fsmMessageAdded(1, 0, "m1"),
			ResponseContentPartAddedEvent{
				EventBase:    EventBase{Type: "response.content_part.added", SequenceNumber: 2},
				ItemID:       "m1",
				OutputIndex:  0,
				ContentIndex: 0,
				Part:         &ResponsesStreamRefusalPart{Type: "refusal", Refusal: ""},
			},
		)
		assertFSMWire(t, fsm.Validate(fsmTextDelta(3, 0, 0, "m1", "x")))
	})

	// 9: duplicate output_text.done, refusal.done, or arguments-done events.
	t.Run("duplicate text done", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm,
			fsmCreated(0), fsmMessageAdded(1, 0, "m1"), fsmTextPartAdded(2, 0, 0, "m1"),
			fsmTextDelta(3, 0, 0, "m1", "hi"), fsmTextDone(4, 0, 0, "m1", "hi"),
		)
		assertFSMWire(t, fsm.Validate(fsmTextDone(5, 0, 0, "m1", "hi")))
	})

	// 10: deltas after the corresponding done event.
	t.Run("delta after done", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm,
			fsmCreated(0), fsmMessageAdded(1, 0, "m1"), fsmTextPartAdded(2, 0, 0, "m1"),
			fsmTextDelta(3, 0, 0, "m1", "hi"), fsmTextDone(4, 0, 0, "m1", "hi"),
		)
		assertFSMWire(t, fsm.Validate(fsmTextDelta(5, 0, 0, "m1", "more")))
	})

	// 11: content_part.done before value-done.
	t.Run("part done before value done", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm,
			fsmCreated(0), fsmMessageAdded(1, 0, "m1"), fsmTextPartAdded(2, 0, 0, "m1"),
		)
		assertFSMWire(t, fsm.Validate(fsmTextPartDone(3, 0, 0, "m1")))
	})

	// 12: output_item.done while any child part/call lifecycle is open.
	t.Run("item done with open part", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm,
			fsmCreated(0), fsmMessageAdded(1, 0, "m1"), fsmTextPartAdded(2, 0, 0, "m1"),
		)
		assertFSMWire(t, fsm.Validate(fsmItemDone(3, 0, "m1")))
	})
	t.Run("item done with streaming call", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0), fsmCallAdded(1, 0, "fc_1", "call_1"))
		delta := ResponseFunctionCallArgumentsDeltaEvent{
			EventBase: EventBase{Type: "response.function_call_arguments.delta", SequenceNumber: 2},
			ItemID:    "fc_1", OutputIndex: 0, Delta: `{`,
		}
		mustValidateFSM(t, fsm, delta)
		assertFSMWire(t, fsm.Validate(ResponseOutputItemDoneEvent{
			EventBase:   EventBase{Type: "response.output_item.done", SequenceNumber: 3},
			OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputItem{
				ID: "fc_1", Type: "function_call", Status: ResponsesItemCompleted,
				CallID: "call_1", Name: "f", Arguments: `{}`,
			},
		}))
	})

	// 14: terminal success while any item is open.
	t.Run("terminal with open item", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0), fsmMessageAdded(1, 0, "m1"))
		assertFSMWire(t, fsm.Validate(fsmComplete(2)))
	})

	// 15: terminal envelope identity drift.
	t.Run("terminal identity drift", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0))
		drifted := fsmComplete(1)
		drifted.Response.ID = "resp_other"
		assertFSMWire(t, fsm.Validate(drifted))
	})

	// 16: terminal snapshots that disagree with accumulated content.
	t.Run("terminal snapshot disagreement", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm,
			fsmCreated(0), fsmMessageAdded(1, 0, "m1"), fsmTextPartAdded(2, 0, 0, "m1"),
			fsmTextDelta(3, 0, 0, "m1", "hello"),
		)
		// The done snapshot text disagrees with the accumulated text.
		assertFSMWire(t, fsm.Validate(fsmTextDone(4, 0, 0, "m1", "goodbye")))
	})

	// 17: any event after a terminal event.
	t.Run("event after terminal", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0), fsmComplete(1))
		assertFSMWire(t, fsm.Validate(fsmMessageAdded(2, 0, "m1")))
	})
}

// mustValidateFSM asserts every event validates.
func mustValidateFSM(t *testing.T, fsm *responsesStreamFSM, events ...ResponsesSSEEvent) {
	t.Helper()
	for _, event := range events {
		if err := fsm.Validate(event); err != nil {
			t.Fatalf("event %s rejected: %v", event.EventType(), err)
		}
	}
}

// TestResponsesStreamFSMBudgetBounds proves the total budget bounds an
// exchange: the empty-part attack (many zero-byte parts) cannot allocate
// beyond the budget, and the event/item/call dimensions reject overflow.
func TestResponsesStreamFSMBudgetBounds(t *testing.T) {
	// The empty-part attack: zero-byte semantic content, unbounded part
	// count. The parts dimension caps it.
	t.Run("empty part attack", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0), fsmMessageAdded(1, 0, "m1"))
		seq := int64(2)
		added := 0
		for {
			event := fsmTextPartAdded(seq, 0, int64(added), "m1")
			seq++
			if err := fsm.Validate(event); err != nil {
				assertFSMWire(t, err)
				break
			}
			added++
			if added > maxStreamTotalParts+10 {
				t.Fatal("empty parts never hit the total budget")
			}
		}
		if added >= maxStreamTotalParts {
			t.Fatalf("parts allocated = %d, want the budget to bind below %d", added, maxStreamTotalParts)
		}
	})

	// The item dimension caps a flood of items.
	t.Run("item flood", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0))
		for i := 0; i < maxStreamOutputItems; i++ {
			if err := fsm.Validate(fsmMessageAdded(int64(i+1), int64(i), "m"+json.Number(string(rune('0'+i%10))).String()+string(rune('a'+i/10)))); err != nil {
				t.Fatalf("item %d rejected early: %v", i, err)
			}
		}
		assertFSMWire(t, fsm.Validate(fsmMessageAdded(maxStreamOutputItems+1, maxStreamOutputItems, "overflow")))
	})

	// The event dimension caps a flood of events.
	t.Run("event flood", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0))
		rejected := false
		for i := 1; i < maxStreamTotalEvents+10; i++ {
			err := fsm.Validate(ResponseInProgressEvent{
				EventBase: EventBase{Type: "response.in_progress", SequenceNumber: int64(i)},
				Response:  fsmEnvelope("resp_1", "in_progress"),
			})
			if err != nil {
				assertFSMWire(t, err)
				rejected = true
				break
			}
		}
		if !rejected {
			t.Fatal("event flood never hit the total budget")
		}
	})

	// The semantic-bytes dimension caps accumulated text.
	t.Run("semantic byte flood", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0), fsmMessageAdded(1, 0, "m1"), fsmTextPartAdded(2, 0, 0, "m1"))
		seq := int64(3)
		const chunk = 4096
		rejected := false
		for {
			if err := fsm.Validate(fsmTextDelta(seq, 0, 0, "m1", strings.Repeat("x", chunk))); err != nil {
				assertFSMWire(t, err)
				rejected = true
				break
			}
			seq++
		}
		if !rejected {
			t.Fatal("semantic byte flood never hit the budget")
		}
	})
}

// TestConvertingReaderStagingAtomicity proves an oversized terminal batch is
// never partially delivered: the reader emits exactly one error terminal and
// never a success terminal followed by an error (review-z commit 3). Each
// individual frame passes the generatedFrameMax; only the STAGED terminal
// batch (the released item-closing events plus the terminal envelope) exceeds
// the generatedBatchMax.
func TestConvertingReaderStagingAtomicity(t *testing.T) {
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
	// Many small content chunks: the released terminal batch repeats every
	// accumulated delta, so the batch bound binds while each frame stays far
	// below the frame bound.
	var frames strings.Builder
	for i := 0; i < 200; i++ {
		frames.WriteString("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" +
			strings.Repeat("x", 64) +
			"\"},\"finish_reason\":null}]}\n\n")
	}
	frames.WriteString("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	frames.WriteString("data: [DONE]\n\n")
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(frames.String()), 0, 0),
		converter,
		1<<20, // generatedFrameMax: every frame passes
		4096,  // generatedBatchMax: the staged terminal batch exceeds it
		0,
	)
	output := make([]byte, 1<<20)
	var body string
	var err error
	for {
		n, readErr := reader.Read(output)
		body += string(output[:n])
		if readErr != nil {
			err = readErr
			break
		}
		if n == 0 {
			t.Fatal("reader made no progress")
		}
	}
	if err == nil {
		t.Fatal("expected the staged batch to fail")
	}
	var boundErr *SSEBoundError
	if !errors.As(err, &boundErr) {
		t.Fatalf("err = %T %v, want *SSEBoundError", err, err)
	}
	if strings.Contains(body, `"type":"response.completed"`) {
		t.Fatal("a success terminal escaped before the size failure")
	}
	if reader.SawTerminal() {
		t.Fatal("reader reported a terminal for a failed batch")
	}
	// The error event may be appended (the reader surfaces the conversion
	// failure as an error terminal) — but never after a success terminal.
	if reader.SawErrorEvent() {
		if strings.Contains(body, `"type":"response.completed"`) {
			t.Fatal("error event after a success terminal")
		}
	}
}

// TestResponsesStreamFSMRemainingRejectionClasses covers the transition-table
// classes not exercised by the main matrix: non-message part targets (6),
// the refusal-to-text converse (8), duplicate refusal/arguments done (9),
// arguments after item-done (13), model/createdAt identity drift (15), and
// the tool-call flood budget dimension.
func TestResponsesStreamFSMRemainingRejectionClasses(t *testing.T) {
	// 6: part events for a non-message item.
	t.Run("part for non-message item", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0), fsmCallAdded(1, 0, "fc_1", "call_1"))
		assertFSMWire(t, fsm.Validate(fsmTextPartAdded(2, 0, 0, "fc_1")))
	})

	// 8 (converse): refusal deltas targeting a text part.
	t.Run("refusal delta targets text part", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm,
			fsmCreated(0), fsmMessageAdded(1, 0, "m1"), fsmTextPartAdded(2, 0, 0, "m1"),
		)
		assertFSMWire(t, fsm.Validate(ResponseRefusalDeltaEvent{
			EventBase:    EventBase{Type: "response.refusal.delta", SequenceNumber: 3},
			ItemID:       "m1",
			OutputIndex:  0,
			ContentIndex: 0,
			Delta:        "no",
		}))
	})

	// 9: duplicate refusal.done and arguments.done.
	t.Run("duplicate refusal done", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm,
			fsmCreated(0), fsmMessageAdded(1, 0, "m1"),
			ResponseContentPartAddedEvent{
				EventBase:    EventBase{Type: "response.content_part.added", SequenceNumber: 2},
				ItemID:       "m1",
				OutputIndex:  0,
				ContentIndex: 0,
				Part:         &ResponsesStreamRefusalPart{Type: "refusal", Refusal: ""},
			},
			ResponseRefusalDeltaEvent{
				EventBase: EventBase{Type: "response.refusal.delta", SequenceNumber: 3},
				ItemID:    "m1", OutputIndex: 0, ContentIndex: 0, Delta: "no",
			},
			ResponseRefusalDoneEvent{
				EventBase: EventBase{Type: "response.refusal.done", SequenceNumber: 4},
				ItemID:    "m1", OutputIndex: 0, ContentIndex: 0, Refusal: "no",
			},
		)
		assertFSMWire(t, fsm.Validate(ResponseRefusalDoneEvent{
			EventBase: EventBase{Type: "response.refusal.done", SequenceNumber: 5},
			ItemID:    "m1", OutputIndex: 0, ContentIndex: 0, Refusal: "no",
		}))
	})
	t.Run("duplicate arguments done", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0), fsmCallAdded(1, 0, "fc_1", "call_1"))
		mustValidateFSM(t, fsm, ResponseFunctionCallArgumentsDoneEvent{
			EventBase: EventBase{Type: "response.function_call_arguments.done", SequenceNumber: 2},
			ItemID:    "fc_1", OutputIndex: 0, Arguments: `{}`,
		})
		assertFSMWire(t, fsm.Validate(ResponseFunctionCallArgumentsDoneEvent{
			EventBase: EventBase{Type: "response.function_call_arguments.done", SequenceNumber: 3},
			ItemID:    "fc_1", OutputIndex: 0, Arguments: `{}`,
		}))
	})

	// 13: arguments events after item-done.
	t.Run("arguments delta after item done", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm,
			fsmCreated(0), fsmCallAdded(1, 0, "fc_1", "call_1"),
			ResponseFunctionCallArgumentsDoneEvent{
				EventBase: EventBase{Type: "response.function_call_arguments.done", SequenceNumber: 2},
				ItemID:    "fc_1", OutputIndex: 0, Arguments: `{}`,
			},
			ResponseOutputItemDoneEvent{
				EventBase:   EventBase{Type: "response.output_item.done", SequenceNumber: 3},
				OutputIndex: 0,
				Item: &ResponsesFunctionCallOutputItem{
					ID: "fc_1", Type: "function_call", Status: ResponsesItemCompleted,
					CallID: "call_1", Name: "f", Arguments: `{}`,
				},
			},
		)
		assertFSMWire(t, fsm.Validate(ResponseFunctionCallArgumentsDeltaEvent{
			EventBase: EventBase{Type: "response.function_call_arguments.delta", SequenceNumber: 4},
			ItemID:    "fc_1", OutputIndex: 0, Delta: `{}`,
		}))
	})

	// 15: model and createdAt identity drift.
	t.Run("terminal model drift", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0))
		drifted := fsmComplete(1)
		drifted.Response.Model = "other"
		assertFSMWire(t, fsm.Validate(drifted))
	})
	t.Run("terminal createdAt drift", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0))
		drifted := fsmComplete(1)
		drifted.Response.CreatedAt = 99
		assertFSMWire(t, fsm.Validate(drifted))
	})

	// Budget: the tool-call flood binds at the toolCalls dimension.
	t.Run("tool call flood", func(t *testing.T) {
		fsm := newResponsesStreamFSM()
		mustValidateFSM(t, fsm, fsmCreated(0))
		rejected := false
		for i := 0; i < maxStreamToolCalls+10; i++ {
			err := fsm.Validate(fsmCallAdded(int64(i+1), int64(i), "fc_"+json.Number(string(rune('a'+i%26))).String()+string(rune('0'+i/26)), "c"+json.Number(string(rune('a'+i%26))).String()))
			if err != nil {
				assertFSMWire(t, err)
				rejected = true
				break
			}
		}
		if !rejected {
			t.Fatal("tool call flood never hit the budget")
		}
	})
}

// TestTruncateErrorTextPreservesIdentity proves the truncating wrapper keeps
// the original error reachable through errors.As (classification never
// changes) while the client-visible text is bounded.
func TestTruncateErrorTextPreservesIdentity(t *testing.T) {
	original := &UpstreamWireError{Protocol: UpstreamResponses, Status: 200, Cause: errors.New(strings.Repeat("x", 4096))}
	truncated := truncateErrorText(original, 64)
	if len(truncated.Error()) > 64 {
		t.Fatalf("truncated length = %d", len(truncated.Error()))
	}
	var wireErr *UpstreamWireError
	if !errors.As(truncated, &wireErr) {
		t.Fatal("errors.As lost the original error identity")
	}
	if wireErr.Protocol != UpstreamResponses {
		t.Fatal("classification lost")
	}
	if got := truncateErrorText(original, 1<<20); got != original {
		t.Fatal("short text must pass through unwrapped")
	}
}

// TestResponsesStreamFSMReasoningLifecycle proves the legal reasoning
// lifecycle passes the FSM: part added, text delta, text done, part done.
func TestResponsesStreamFSMReasoningLifecycle(t *testing.T) {
	fsm := newResponsesStreamFSM()
	mustValidateFSM(t, fsm, fsmCreated(0))
	seq := int64(1)
	mustValidateFSM(t, fsm,
		ResponseOutputItemAddedEvent{
			EventBase:   EventBase{Type: "response.output_item.added", SequenceNumber: seq},
			OutputIndex: 0,
			Item: &ResponsesReasoningOutputItem{
				ID: "rs_1", Type: "reasoning", Status: ResponsesItemInProgress,
				Summary: []ResponsesReasoningSummary{},
			},
		})
	seq++
	mustValidateFSM(t, fsm,
		ResponseReasoningSummaryPartAddedEvent{
			EventBase: EventBase{Type: "response.reasoning_summary_part.added", SequenceNumber: seq},
			ItemID:    "rs_1", OutputIndex: 0, SummaryIndex: 0,
			Part: ResponsesSummaryTextPart{Type: "summary_text", Text: ""},
		})
	seq++
	mustValidateFSM(t, fsm,
		ResponseReasoningSummaryTextDeltaEvent{
			EventBase: EventBase{Type: "response.reasoning_summary_text.delta", SequenceNumber: seq},
			ItemID:    "rs_1", OutputIndex: 0, SummaryIndex: 0, Delta: "think",
		})
	seq++
	mustValidateFSM(t, fsm,
		ResponseReasoningSummaryTextDoneEvent{
			EventBase: EventBase{Type: "response.reasoning_summary_text.done", SequenceNumber: seq},
			ItemID:    "rs_1", OutputIndex: 0, SummaryIndex: 0, Text: "think",
		})
	seq++
	mustValidateFSM(t, fsm,
		ResponseReasoningSummaryPartDoneEvent{
			EventBase: EventBase{Type: "response.reasoning_summary_part.done", SequenceNumber: seq},
			ItemID:    "rs_1", OutputIndex: 0, SummaryIndex: 0,
			Part: ResponsesSummaryTextPart{Type: "summary_text", Text: "think"},
		})
}
