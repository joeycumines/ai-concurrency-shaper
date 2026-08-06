package transcode

import (
	"encoding/json"
	"fmt"
	"testing"
)

func FuzzChatToResponsesStateMachine(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{0, 6, 7, 7, 8, 5})
	f.Add([]byte{1, 1, 5, 9, 9})
	f.Add([]byte{0, 2, 6, 3, 8, 5, 4})

	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 4096 {
			t.Skip()
		}

		var (
			state = newChatResponsesStreamState(
				testStreamContext(),
				StrictLossPolicy(),
				"resp_1",
				"gpt-4.1",
				1710000000,
				nil,
			)
			events []ResponsesSSEEvent
		)

		for index, operation := range operations {
			chunk := fuzzChatChunk(operation, index)
			batch, err := state.Convert(chunk)
			if err != nil {
				// Rejecting invalid source order is acceptable. Emitting an
				// invalid target trace is not.
				break
			}
			events = append(events, batch...)
		}

		final, err := state.FinalizeEOF()
		if err == nil {
			events = append(events, final...)
		}

		assertValidResponsesTrace(t, events)
	})
}

func fuzzChatChunk(operation byte, index int) ChatStreamResponse {
	base := ChatStreamResponse{
		ID:      "chatcmpl-fuzz",
		Model:   "model",
		Created: 1,
		Choices: []ChatChoice{{
			Index: 0,
			Delta: &ChatStreamDelta{},
		}},
	}
	delta := base.Choices[0].Delta

	switch operation % 10 {
	case 0:
		delta.Role = stringPtr("assistant")

	case 1:
		delta.Content = stringPtr(fmt.Sprintf("text-%d", index))

	case 2:
		delta.Refusal = stringPtr(fmt.Sprintf("refusal-%d", index))

	case 3:
		delta.Reasoning = stringPtr(fmt.Sprintf("reason-%d", index))

	case 4:
		base.Choices[0].FinishReason = stringPtr("stop")

	case 5:
		base.Choices[0].FinishReason = stringPtr("length")

	case 6:
		callIndex := index % 3
		delta.ToolCalls = []ChatAssistantMessageToolCall{{
			Index: intPtr(callIndex),
			ID:    stringPtr(fmt.Sprintf("call-%d", callIndex)),
			Function: ChatAssistantMessageToolCallFunction{
				Name:      stringPtr(fmt.Sprintf("tool_%d", callIndex)),
				Arguments: `{"x":`,
			},
		}}

	case 7:
		callIndex := index % 3
		delta.ToolCalls = []ChatAssistantMessageToolCall{{
			Index: intPtr(callIndex),
			Function: ChatAssistantMessageToolCallFunction{
				Arguments: fmt.Sprintf("%d}", index),
			},
		}}

	case 8:
		base.Choices[0].FinishReason = stringPtr("tool_calls")

	case 9:
		base.Choices = nil
		base.Usage = &ChatLLMUsage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		}
	}

	return base
}

// assertValidResponsesTrace checks the invariants of a Responses SSE trace:
// event validity, name/type equality, contiguous sequence numbers, item
// add-before-done discipline, content part open/close discipline, and a
// single terminal (success or error) at the end.
func assertValidResponsesTrace(
	t *testing.T,
	events []ResponsesSSEEvent,
) {
	t.Helper()

	addedItems := make(map[string]struct{})
	doneItems := make(map[string]struct{})
	openContent := make(map[string]struct{})

	var (
		terminalSeen bool
		errorSeen    bool
	)

	for index, event := range events {
		if err := event.Validate(); err != nil {
			t.Fatalf("event %d %T invalid: %v", index, event, err)
		}

		data, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("event %d marshal: %v", index, err)
		}

		var common struct {
			Type           string `json:"type"`
			SequenceNumber int64  `json:"sequence_number"`
		}
		if err := json.Unmarshal(data, &common); err != nil {
			t.Fatal(err)
		}

		if common.Type != event.EventType() {
			t.Fatalf(
				"event %d name/type mismatch: %q / %q",
				index,
				event.EventType(),
				common.Type,
			)
		}
		if common.SequenceNumber != int64(index) {
			t.Fatalf(
				"event %d sequence = %d",
				index,
				common.SequenceNumber,
			)
		}
		if terminalSeen || errorSeen {
			t.Fatalf("event %d emitted after terminal/error", index)
		}

		switch value := event.(type) {
		case ResponseOutputItemAddedEvent:
			id := outputItemID(value.Item)
			if id == "" {
				t.Fatalf("event %d added item without id", index)
			}
			if _, duplicate := addedItems[id]; duplicate {
				t.Fatalf("event %d added duplicate item %q", index, id)
			}
			addedItems[id] = struct{}{}

		case ResponseContentPartAddedEvent:
			if _, ok := addedItems[value.ItemID]; !ok {
				t.Fatalf("content part before item %q", value.ItemID)
			}
			openContent[contentKey(
				value.ItemID,
				value.ContentIndex,
			)] = struct{}{}

		case ResponseTextDeltaEvent:
			if _, ok := openContent[contentKey(
				value.ItemID,
				value.ContentIndex,
			)]; !ok {
				t.Fatalf("text delta before content start")
			}

		case ResponseContentPartDoneEvent:
			key := contentKey(value.ItemID, value.ContentIndex)
			if _, ok := openContent[key]; !ok {
				t.Fatalf("content done without open content")
			}
			delete(openContent, key)

		case ResponseOutputItemDoneEvent:
			id := outputItemID(value.Item)
			if _, ok := addedItems[id]; !ok {
				t.Fatalf("item %q done before added", id)
			}
			if _, duplicate := doneItems[id]; duplicate {
				t.Fatalf("item %q done twice", id)
			}
			doneItems[id] = struct{}{}

		case ResponseCompletedEvent,
			ResponseIncompleteEvent,
			ResponseFailedEvent:
			terminalSeen = true

		case ResponseErrorEvent:
			errorSeen = true
		}
	}

	if terminalSeen && errorSeen {
		t.Fatal("trace contains both success terminal and error terminal")
	}
}

func outputItemID(item ResponsesOutputItem) string {
	switch value := item.(type) {
	case *ResponsesOutputMessage:
		return value.ID
	case *ResponsesFunctionCallOutputItem:
		return value.ID
	case *ResponsesReasoningOutputItem:
		return value.ID
	default:
		return ""
	}
}

func contentKey(itemID string, contentIndex int64) string {
	return fmt.Sprintf("%s/%d", itemID, contentIndex)
}

// FuzzResponsesToAnthropicStateMachine drives the Responses-to-Anthropic
// stream state machine with arbitrary typed events (decoded from fuzz JSON)
// and asserts the Anthropic trace invariants from review-i section 11.3.
func FuzzResponsesToAnthropicStateMachine(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[],"parallel_tool_calls":true,"tools":[],"tool_choice":"auto"}}`),
		[]byte(`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}}`),
		[]byte(`{"type":"response.content_part.added","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`),
		[]byte(`{"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"hi","logprobs":[]}`),
		[]byte(`{"type":"response.content_part.done","sequence_number":4,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hi","annotations":[]}}`),
		[]byte(`{"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}}`),
		[]byte(`{"type":"response.completed","sequence_number":6,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[],"parallel_tool_calls":true,"tools":[],"tool_choice":"auto"}}`),
		[]byte(`{"type":"error","sequence_number":7,"code":"api_error","message":"boom"}`),
		[]byte(`{"type":"response.failed","sequence_number":8,"response":{"id":"resp_1","object":"response","created_at":1,"status":"failed","model":"m","output":[],"parallel_tool_calls":true,"tools":[],"tool_choice":"auto","error":{"code":"x","message":"boom"}}}`),
		[]byte(`{"type":"response.reasoning_summary_part.added","sequence_number":9,"item_id":"rs_1","output_index":1,"summary_index":0,"part":{"type":"summary_text","text":""}}`),
		[]byte(`{"type":"response.function_call_arguments.delta","sequence_number":10,"item_id":"fc_1","output_index":0,"delta":"{\"x\":1}"}`),
		[]byte(`{"type":"response.incomplete","sequence_number":11,"response":{"id":"resp_1","object":"response","created_at":1,"status":"incomplete","model":"m","output":[],"parallel_tool_calls":true,"tools":[],"tool_choice":"auto","incomplete_details":{"reason":"max_output_tokens"}}}`),
		[]byte(`{"type":"unknown_event","sequence_number":12}`),
	}

	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		const maxInput = 1 << 20
		if len(raw) > maxInput {
			t.Skip()
		}

		state := newAnthropicResponsesStreamState(
			testStreamContext(),
			"resp_1",
			"gpt-4.1",
			1710000000,
		)

		var trace []AnthropicStreamEvent
		convertErr := false

		// Split the fuzz input into frames (a stream of events) rather than
		// one blob so multi-event traces are exercised.
		segments := splitFuzzFrames(raw)
		for _, segment := range segments {
			event, err := decodeResponsesSSEEvent(segment)
			if err != nil {
				// Unknown or malformed source events are rejected at decode;
				// the state machine must not see them (they never consume a
				// block index).
				continue
			}
			batch, err := state.Convert(event)
			if err != nil {
				convertErr = true
				break
			}
			trace = append(trace, batch...)
		}

		final, err := state.FinalizeEOF()
		if err == nil {
			trace = append(trace, final...)
		} else if !convertErr {
			// FinalizeEOF reports truncation only when no terminal condition
			// was reached; a conversion error already broke the loop.
			t.Logf("FinalizeEOF: %v", err)
		}

		assertValidAnthropicTrace(t, trace)
	})
}

// splitFuzzFrames splits raw bytes into zero or more JSON frames. A frame is
// every byte between two zero bytes; stray bytes without a following zero are
// discarded so each frame is independently decodable.
func splitFuzzFrames(raw []byte) [][]byte {
	var frames [][]byte
	start := 0
	for i, b := range raw {
		if b == 0 {
			if i > start {
				frames = append(frames, raw[start:i])
			}
			start = i + 1
		}
	}
	return frames
}

// assertValidAnthropicTrace checks the invariants of an Anthropic SSE trace:
// message_start first and once; block start/stop discipline; message_delta
// only after all blocks stopped; message_stop only after message_delta; error
// terminates without message_stop; no thinking blocks; tool_use arguments
// parse as JSON by block stop.
func assertValidAnthropicTrace(t *testing.T, trace []AnthropicStreamEvent) {
	t.Helper()

	var (
		messageStarted bool
		messageStopped bool
		messageDelta   bool
		errorSeen      bool

		openBlocks   = make(map[int]bool)
		startedBlock = make(map[int]bool)
		stoppedBlock = make(map[int]bool)

		openToolUse = make(map[int]bool)
		toolArgs    = make(map[int]string)
	)

	for index, event := range trace {
		if event.Type == "" {
			t.Fatalf("event %d has empty type", index)
		}
		if errorSeen {
			t.Fatalf("event %d emitted after error terminal", index)
		}

		switch event.Type {
		case AnthropicStreamEventTypeMessageStart:
			if messageStarted {
				t.Fatalf("duplicate message_start at %d", index)
			}
			if messageStopped {
				t.Fatalf("message_start after message_stop at %d", index)
			}
			messageStarted = true

		case AnthropicStreamEventTypeContentBlockStart:
			if !messageStarted || messageStopped {
				t.Fatalf("content_block_start before message_start or after stop at %d", index)
			}
			if event.Index == nil {
				t.Fatalf("content_block_start without index at %d", index)
			}
			if startedBlock[*event.Index] {
				t.Fatalf("block %d started twice at %d", *event.Index, index)
			}
			startedBlock[*event.Index] = true
			openBlocks[*event.Index] = true
			if event.ContentBlock != nil && event.ContentBlock.Type == AnthropicContentBlockTypeToolUse {
				openToolUse[*event.Index] = true
			}

		case AnthropicStreamEventTypeContentBlockDelta:
			if event.Index == nil {
				t.Fatalf("content_block_delta without index at %d", index)
			}
			if !openBlocks[*event.Index] {
				t.Fatalf("delta for non-open block %d at %d", *event.Index, index)
			}
			if event.Delta != nil && event.Delta.Type == "input_json_delta" {
				if openToolUse[*event.Index] {
					toolArgs[*event.Index] += *event.Delta.PartialJSON
				}
			}

		case AnthropicStreamEventTypeContentBlockStop:
			if event.Index == nil {
				t.Fatalf("content_block_stop without index at %d", index)
			}
			if !openBlocks[*event.Index] {
				t.Fatalf("stop for non-open block %d at %d", *event.Index, index)
			}
			delete(openBlocks, *event.Index)
			if stoppedBlock[*event.Index] {
				t.Fatalf("block %d stopped twice at %d", *event.Index, index)
			}
			stoppedBlock[*event.Index] = true
			if openToolUse[*event.Index] {
				// Function argument accumulation must parse as a JSON object
				// before the block stops.
				args := toolArgs[*event.Index]
				var probe any
				if err := json.Unmarshal([]byte(args), &probe); err != nil {
					t.Fatalf(
						"tool_use block %d arguments do not parse at stop: %q",
						*event.Index,
						args,
					)
				}
				delete(openToolUse, *event.Index)
			}

		case AnthropicStreamEventTypeMessageDelta:
			if !messageStarted || messageStopped {
				t.Fatalf("message_delta before start or after stop at %d", index)
			}
			if len(openBlocks) > 0 {
				t.Fatalf("message_delta with open blocks at %d: %v", index, openBlocks)
			}
			messageDelta = true

		case AnthropicStreamEventTypeMessageStop:
			if !messageStarted {
				t.Fatalf("message_stop before message_start at %d", index)
			}
			if !messageDelta {
				t.Fatalf("message_stop before message_delta at %d", index)
			}
			if len(openBlocks) > 0 {
				t.Fatalf("message_stop with open blocks at %d", index)
			}
			messageStopped = true

		case AnthropicStreamEventTypeError:
			errorSeen = true

		case AnthropicStreamEventTypePing:
			// Ping may appear anywhere.

		default:
			t.Fatalf("event %d has unknown type %q", index, event.Type)
		}

		// OpenAI reasoning is never synthesized as Anthropic thinking.
		if event.ContentBlock != nil && event.ContentBlock.Type == AnthropicContentBlockTypeThinking {
			t.Fatalf("event %d emitted a thinking block", index)
		}
	}

	if errorSeen && messageStopped {
		t.Fatal("trace contains both error terminal and message_stop")
	}
}
