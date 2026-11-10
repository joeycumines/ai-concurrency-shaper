package transcode

import (
	"encoding/json"
	"testing"
)

// zzAnthropicFeed converts one raw Responses SSE data payload through the
// direct Responses→Anthropic converter and returns the decoded Anthropic
// events.
func zzAnthropicFeed(t *testing.T, conv *responsesToAnthropicConverter, data string) []AnthropicStreamEvent {
	t.Helper()
	batch, err := conv.Convert(SSEEvent{Data: []byte(data)})
	if err != nil {
		t.Fatalf("frame %.70s: %v", data, err)
	}
	var events []AnthropicStreamEvent
	for _, frame := range batch.Events {
		var event AnthropicStreamEvent
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

const zzCreated = `{"type":"response.created","sequence_number":1,"response":{"id":"r1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`

// TestAnthropicStreamRejectsConcurrentContentBlocks proves the reported
// defect class is closed: a reasoning_summary_part.added (or a second
// message's content_part.added) arriving while any content block is still
// open is corrupt upstream wire, never two concurrent content_block_start
// events on the Messages wire.
func TestAnthropicStreamRejectsConcurrentContentBlocks(t *testing.T) {
	newConv := func() *responsesToAnthropicConverter {
		return newResponsesToAnthropicConverter(newAnthropicResponsesStreamState(
			testStreamContext(), permissiveLossPolicy(),
			ChatCapabilities{ProviderReasoningThinking: true}, "msg_1", "m", 1,
		))
	}
	msgAdded := `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"m1","type":"message","role":"assistant","status":"in_progress","content":[]}}`
	partAdded := `{"type":"response.content_part.added","sequence_number":3,"item_id":"m1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`
	textDelta := `{"type":"response.output_text.delta","sequence_number":4,"item_id":"m1","output_index":0,"content_index":0,"delta":"hi","logprobs":[]}`

	t.Run("reasoning part while text block open", func(t *testing.T) {
		conv := newConv()
		zzAnthropicFeed(t, conv, zzCreated)
		zzAnthropicFeed(t, conv, msgAdded)
		zzAnthropicFeed(t, conv, partAdded)
		zzAnthropicFeed(t, conv, textDelta)
		// A reasoning item whose summary part opens while the text block is
		// open must fail as upstream wire.
		zzAnthropicFeed(t, conv, `{"type":"response.output_item.added","sequence_number":5,"output_index":1,"item":{"id":"r1","type":"reasoning","status":"in_progress","summary":[]}}`)
		_, err := conv.Convert(SSEEvent{Data: []byte(`{"type":"response.reasoning_summary_part.added","sequence_number":6,"item_id":"r1","output_index":1,"summary_index":0,"part":{"type":"summary_text","text":""}}`)})
		assertAnthropicWireError(t, err, "while another content block is open")
	})

	t.Run("text part while thinking block open", func(t *testing.T) {
		conv := newConv()
		zzAnthropicFeed(t, conv, zzCreated)
		zzAnthropicFeed(t, conv, `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"r1","type":"reasoning","status":"in_progress","summary":[]}}`)
		zzAnthropicFeed(t, conv, `{"type":"response.reasoning_summary_part.added","sequence_number":3,"item_id":"r1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
		zzAnthropicFeed(t, conv, `{"type":"response.reasoning_summary_text.delta","sequence_number":4,"item_id":"r1","output_index":0,"summary_index":0,"delta":"think"}`)
		// A second message item's content part while the thinking block is
		// open must fail as upstream wire.
		zzAnthropicFeed(t, conv, `{"type":"response.output_item.added","sequence_number":5,"output_index":1,"item":{"id":"m1","type":"message","role":"assistant","status":"in_progress","content":[]}}`)
		_, err := conv.Convert(SSEEvent{Data: []byte(`{"type":"response.content_part.added","sequence_number":6,"item_id":"m1","output_index":1,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`)})
		assertAnthropicWireError(t, err, "while another content block is open")
	})

	t.Run("second message part while first open", func(t *testing.T) {
		conv := newConv()
		zzAnthropicFeed(t, conv, zzCreated)
		zzAnthropicFeed(t, conv, msgAdded)
		zzAnthropicFeed(t, conv, partAdded)
		zzAnthropicFeed(t, conv, textDelta)
		zzAnthropicFeed(t, conv, `{"type":"response.output_item.added","sequence_number":5,"output_index":1,"item":{"id":"m2","type":"message","role":"assistant","status":"in_progress","content":[]}}`)
		_, err := conv.Convert(SSEEvent{Data: []byte(`{"type":"response.content_part.added","sequence_number":6,"item_id":"m2","output_index":1,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`)})
		assertAnthropicWireError(t, err, "while another content block is open")
	})
}

// TestChatToAnthropicNeverEmitsConcurrentBlocks drives the composed
// chat→Responses→Anthropic path (the OpenRouter/DeepSeek reasoning +
// reasoning_content gateway shape) through reasoning/content/tool bursts and
// asserts the emitted Anthropic wire never has two concurrent open blocks.
func TestChatToAnthropicNeverEmitsConcurrentBlocks(t *testing.T) {
	conv := newChatToAnthropicConverter(
		newChatResponsesStreamState(testStreamContext(), permissiveLossPolicy(),
			ChatCapabilities{ProviderReasoningThinking: true}, "resp_1", "m", 1, nil),
		newAnthropicResponsesStreamState(testStreamContext(), permissiveLossPolicy(),
			ChatCapabilities{ProviderReasoningThinking: true}, "resp_1", "m", 1),
	)
	feed := func(deltaJSON string, finish string) []AnthropicStreamEvent {
		t.Helper()
		finishField := "null"
		if finish != "" {
			finishField = `"` + finish + `"`
		}
		batch, err := conv.Convert(SSEEvent{Data: []byte(
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":` + deltaJSON + `,"finish_reason":` + finishField + `}]}`,
		)})
		if err != nil {
			t.Fatalf("feed %s: %v", deltaJSON, err)
		}
		var events []AnthropicStreamEvent
		for _, frame := range batch.Events {
			var event AnthropicStreamEvent
			if err := json.Unmarshal(frame.Data, &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
		return events
	}
	var all []AnthropicStreamEvent
	// Combined and split bursts: reasoning+content in one delta, reasoning
	// alone, content alone, reasoning+tool_calls, tool, content, tool.
	all = append(all, feed(`{"reasoning":"step1","content":"ans"}`, "")...)
	all = append(all, feed(`{"reasoning":"step2"}`, "")...)
	all = append(all, feed(`{"content":"more"}`, "")...)
	all = append(all, feed(`{"reasoning":"step3","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}`, "")...)
	all = append(all, feed(`{"reasoning":"step4"}`, "")...)
	all = append(all, feed(`{"content":"final"}`, "")...)
	all = append(all, feed(`{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"g","arguments":"{}"}}]}`, "")...)
	// Terminal: finish_reason releases the held terminal via [DONE].
	all = append(all, feed(`{}`, "stop")...)
	batch, err := conv.Convert(SSEEvent{Data: []byte("[DONE]")})
	if err != nil {
		t.Fatalf("[DONE]: %v", err)
	}
	for _, frame := range batch.Events {
		var event AnthropicStreamEvent
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			t.Fatal(err)
		}
		all = append(all, event)
	}

	open := 0
	maxOpen := 0
	starts, stops := 0, 0
	for _, e := range all {
		switch e.Type {
		case AnthropicStreamEventTypeContentBlockStart:
			open++
			starts++
			if open > maxOpen {
				maxOpen = open
			}
		case AnthropicStreamEventTypeContentBlockStop:
			open--
			stops++
		}
	}
	if maxOpen > 1 {
		t.Fatalf("interleaved wire: max concurrent open content blocks = %d (starts=%d stops=%d)", maxOpen, starts, stops)
	}
	if open != 0 {
		t.Fatalf("the terminal must seal every block: open = %d after [DONE] (starts=%d stops=%d)", open, starts, stops)
	}
	if starts != stops {
		t.Fatalf("starts = %d, stops = %d: every started block must be stopped", starts, stops)
	}
	// The bursts render as separate sealed blocks per transition:
	// 4 thinking + 2 text + 2 tool.
	if starts != 9 {
		t.Fatalf("started blocks = %d, want 9 (4 thinking + 2 text + 2 tool)", starts)
	}
	messageStops := 0
	for _, e := range all {
		if e.Type == AnthropicStreamEventTypeMessageStop {
			messageStops++
		}
	}
	if messageStops != 1 {
		t.Fatalf("message_stop count = %d, want exactly one success terminal", messageStops)
	}
}

// TestChatToAnthropicSealedPartNotReEmittedAtItemClose reproduces the
// closeOpenMessageItem double-done defect: a part sealed inline at a
// part-type transition (text -> refusal via closeOpenPart) must NOT be
// closed a second time when the message item itself closes at a
// message->reasoning (or message->tool) transition. On the base commit the
// item close re-emitted the sealed part's done events, and the Anthropic
// state machine failed on the duplicated content_part.done.
func TestChatToAnthropicSealedPartNotReEmittedAtItemClose(t *testing.T) {
	newConv := func() *chatToAnthropicConverter {
		return newChatToAnthropicConverter(
			newChatResponsesStreamState(testStreamContext(), permissiveLossPolicy(),
				ChatCapabilities{ProviderReasoningThinking: true}, "resp_1", "m", 1, nil),
			newAnthropicResponsesStreamState(testStreamContext(), permissiveLossPolicy(),
				ChatCapabilities{ProviderReasoningThinking: true}, "resp_1", "m", 1),
		)
	}
	feed := func(t *testing.T, conv *chatToAnthropicConverter, deltaJSON string, finish string) []AnthropicStreamEvent {
		t.Helper()
		finishField := "null"
		if finish != "" {
			finishField = `"` + finish + `"`
		}
		batch, err := conv.Convert(SSEEvent{Data: []byte(
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":` + deltaJSON + `,"finish_reason":` + finishField + `}]}`,
		)})
		if err != nil {
			t.Fatalf("feed %s: %v", deltaJSON, err)
		}
		var events []AnthropicStreamEvent
		for _, frame := range batch.Events {
			var event AnthropicStreamEvent
			if err := json.Unmarshal(frame.Data, &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
		return events
	}

	countType := func(events []AnthropicStreamEvent, typ AnthropicStreamEventType) int {
		n := 0
		for _, e := range events {
			if e.Type == typ {
				n++
			}
		}
		return n
	}

	run := func(t *testing.T, conv *chatToAnthropicConverter, toolVariant bool) []AnthropicStreamEvent {
		var all []AnthropicStreamEvent
		// Open a text part and emit text.
		all = append(all, feed(t, conv, `{"content":"a"}`, "")...)
		// Transition text -> refusal: closeOpenPart seals the text part inline
		// (its done events are emitted here, exactly once).
		all = append(all, feed(t, conv, `{"refusal":"no"}`, "")...)
		if toolVariant {
			// Message -> tool transition: closeOpenMessageItem must skip the
			// already-sealed text part and close only the open refusal part.
			all = append(all, feed(t, conv, `{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}`, "")...)
		} else {
			// Message -> reasoning transition: same close path.
			all = append(all, feed(t, conv, `{"reasoning":"x"}`, "")...)
		}
		// Terminal: finish_reason plus [DONE] releases the held terminal.
		all = append(all, feed(t, conv, `{}`, "stop")...)
		batch, err := conv.Convert(SSEEvent{Data: []byte("[DONE]")})
		if err != nil {
			t.Fatalf("[DONE]: %v", err)
		}
		for _, frame := range batch.Events {
			var event AnthropicStreamEvent
			if err := json.Unmarshal(frame.Data, &event); err != nil {
				t.Fatal(err)
			}
			all = append(all, event)
		}
		return all
	}

	for _, toolVariant := range []bool{false, true} {
		name := "reasoning transition"
		if toolVariant {
			name = "tool transition"
		}
		t.Run(name, func(t *testing.T) {
			conv := newConv()
			all := run(t, conv, toolVariant)
			// Exactly one content_block_start/stop pair per block: the text
			// and refusal parts, plus the thinking block (reasoning variant)
			// or the tool_use block (tool variant).
			wantBlocks := 3
			if got := countType(all, AnthropicStreamEventTypeContentBlockStart); got != wantBlocks {
				t.Fatalf("content_block_start count = %d, want %d", got, wantBlocks)
			}
			if got := countType(all, AnthropicStreamEventTypeContentBlockStop); got != wantBlocks {
				t.Fatalf("content_block_stop count = %d, want %d (a sealed part was re-emitted at the item close)", got, wantBlocks)
			}
			// Exactly one success terminal.
			if got := countType(all, AnthropicStreamEventTypeMessageStop); got != 1 {
				t.Fatalf("message_stop count = %d, want 1", got)
			}
			if got := countType(all, AnthropicStreamEventTypeError); got != 0 {
				t.Fatalf("unexpected error events: %d", got)
			}
		})
	}
}
