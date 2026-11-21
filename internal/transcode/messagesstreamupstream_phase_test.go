package transcode

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

// anthropicListingBlockSSE is one mcp_tool_listing block: an ignored block
// whose payload is provider state, not conversation content.
const anthropicListingBlockSSE = "" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"mcp_tool_listing","mcp_server_name":"search","tools":[{"name":"web_search","input_schema":{"type":"object"}}]}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n"

// anthropicUsageOnlyDeltaSSE is a message_delta that carries only usage
// metadata: no stop reason. It still ends the content phase.
func anthropicUsageOnlyDeltaSSE(outputTokens int) string {
	return "" +
		"event: message_delta\n" +
		fmt.Sprintf(`data: {"type":"message_delta","delta":{"stop_reason":null,"stop_sequence":null},"usage":{"output_tokens":%d}}`, outputTokens) + "\n\n"
}

// assertFailedStream runs the SSE body through the production
// Gemini-client-over-Messages handler and asserts the exchange failed: no
// frame carries a finish reason and the last frame is an error.
func assertFailedStream(t *testing.T, sse string) {
	t.Helper()
	rec := serveGeminiStreamBody(t,
		geminiMessagesStreamHandler(t, sse, nil),
		geminiMessagesStreamTarget,
		geminiMessagesBody,
	)
	frames := geminiStreamFrames(t, rec.Body.String())
	if len(frames) == 0 {
		t.Fatalf("no frames; body %q", rec.Body.String())
	}
	for _, frame := range frames {
		if geminiFrameFinish(frame) != "" {
			t.Fatalf("failed stream reported success: %v", frame)
		}
	}
	if _, ok := frames[len(frames)-1]["error"]; !ok {
		t.Fatalf("failed stream did not end in an error frame: %v", frames[len(frames)-1])
	}
}

// TestGeminiStreamMessagesUsageOnlyDeltaEndsContentPhase proves the content
// phase ends at the FIRST message_delta even when it carries no stop reason:
// a block opened before that delta (or started after it) is malformed wire,
// because later metadata cannot reopen content.
func TestGeminiStreamMessagesUsageOnlyDeltaEndsContentPhase(t *testing.T) {
	openListing := "" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"mcp_tool_listing","mcp_server_name":"search","tools":[{"name":"web_search","input_schema":{"type":"object"}}]}}` + "\n\n"

	t.Run("usage-only delta while a block is open fails", func(t *testing.T) {
		sse := anthropicStartSSE + openListing +
			anthropicUsageOnlyDeltaSSE(2) +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n" +
			anthropicStopSSE("end_turn")
		assertFailedStream(t, sse)
	})

	t.Run("content_block_start after a usage-only delta fails", func(t *testing.T) {
		sse := anthropicStartSSE + anthropicListingBlockSSE +
			anthropicUsageOnlyDeltaSSE(2) +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"late"}}` + "\n\n" +
			anthropicStopSSE("end_turn")
		assertFailedStream(t, sse)
	})

	t.Run("content_block_delta after a usage-only delta fails", func(t *testing.T) {
		sse := anthropicStartSSE +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}` + "\n\n" +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n" +
			anthropicUsageOnlyDeltaSSE(2) +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}` + "\n\n" +
			anthropicStopSSE("end_turn")
		assertFailedStream(t, sse)
	})

	t.Run("content_block_stop after a usage-only delta fails", func(t *testing.T) {
		sse := anthropicStartSSE +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
			anthropicUsageOnlyDeltaSSE(2) +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n" +
			anthropicStopSSE("end_turn")
		assertFailedStream(t, sse)
	})

	t.Run("a valid usage-only delta after closed content still succeeds", func(t *testing.T) {
		sse := anthropicStartSSE + anthropicTextBlockSSE +
			anthropicUsageOnlyDeltaSSE(2) +
			anthropicStopSSE("end_turn")
		rec := serveGeminiStreamBody(t,
			geminiMessagesStreamHandler(t, sse, nil),
			geminiMessagesStreamTarget,
			geminiMessagesBody,
		)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}
		frames := geminiStreamFrames(t, rec.Body.String())
		assertSingleTerminal(t, frames)
		if got := geminiFrameText(frames); got != "hello" {
			t.Fatalf("text %q, want hello; body %s", got, rec.Body.String())
		}
		if finish := geminiFrameFinish(frames[len(frames)-1]); finish != "STOP" {
			t.Fatalf("finish %q, want STOP; body %s", finish, rec.Body.String())
		}
	})

	t.Run("repeated metadata deltas after the phase transition complete", func(t *testing.T) {
		// Many closed blocks followed by many usage-only deltas: the closure
		// check runs once at the phase transition, not per delta, so the
		// stream completes within the event budget.
		var blocks strings.Builder
		const blockCount = 200
		for i := range blockCount {
			fmt.Fprintf(&blocks, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n", i)
			fmt.Fprintf(&blocks, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n", i)
			fmt.Fprintf(&blocks, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i)
		}
		var deltas strings.Builder
		const deltaCount = 200
		for i := range deltaCount {
			deltas.WriteString(anthropicUsageOnlyDeltaSSE(2 + i))
		}
		sse := anthropicStartSSE + blocks.String() + deltas.String() + anthropicStopSSE("end_turn")
		rec := serveGeminiStreamBody(t,
			geminiMessagesStreamHandler(t, sse, nil),
			geminiMessagesStreamTarget,
			geminiMessagesBody,
		)
		frames := geminiStreamFrames(t, rec.Body.String())
		assertSingleTerminal(t, frames)
		if finish := geminiFrameFinish(frames[len(frames)-1]); finish != "STOP" {
			t.Fatalf("finish %q, want STOP; body %s", finish, rec.Body.String())
		}
	})
}

// TestGeminiStreamMessagesStartRefusalCannotBeDiscarded proves a message_start
// that already carried a stop reason is authoritative start metadata: a later
// successful terminal under a different reason contradicts it and fails the
// stream, while a matching refusal terminal still renders as incomplete.
func TestGeminiStreamMessagesStartRefusalCannotBeDiscarded(t *testing.T) {
	startRefusal := "" +
		"event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x","stop_reason":"refusal","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n"

	t.Run("start refusal then a successful terminal fails", func(t *testing.T) {
		sse := startRefusal + anthropicTextBlockSSE + anthropicStopSSE("end_turn")
		assertFailedStream(t, sse)
	})

	t.Run("start refusal then a contradictory delta fails at the delta", func(t *testing.T) {
		sse := startRefusal +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"
		assertFailedStream(t, sse)
	})

	t.Run("start refusal with a matching refusal terminal stays incomplete", func(t *testing.T) {
		sse := startRefusal +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null},"usage":{"output_tokens":2}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"
		rec := serveGeminiStreamBody(t,
			geminiMessagesStreamHandler(t, sse, nil),
			geminiMessagesStreamTarget,
			geminiMessagesBody,
		)
		frames := geminiStreamFrames(t, rec.Body.String())
		assertSingleTerminal(t, frames)
		last := frames[len(frames)-1]
		if _, ok := last["error"]; ok {
			t.Fatalf("a genuine refusal terminal failed: %v", last)
		}
		if finish := geminiFrameFinish(last); finish != "SAFETY" {
			t.Fatalf("finish %q, want SAFETY (content_filter); body %s", finish, rec.Body.String())
		}
	})

	t.Run("start without a stop reason still completes normally", func(t *testing.T) {
		sse := anthropicStartSSE + anthropicTextBlockSSE + anthropicStopSSE("end_turn")
		rec := serveGeminiStreamBody(t,
			geminiMessagesStreamHandler(t, sse, nil),
			geminiMessagesStreamTarget,
			geminiMessagesBody,
		)
		frames := geminiStreamFrames(t, rec.Body.String())
		assertSingleTerminal(t, frames)
		if finish := geminiFrameFinish(frames[len(frames)-1]); finish != "STOP" {
			t.Fatalf("finish %q, want STOP; body %s", finish, rec.Body.String())
		}
	})
}

// TestAnthropicUpstreamContentPhaseBoundaryDirect drives the upstream state
// directly (no Gemini composition) to pin the phase-transition semantics:
// the first message_delta ends the content phase, the closure check runs
// once, and block events after it are wire errors.
func TestAnthropicUpstreamContentPhaseBoundaryDirect(t *testing.T) {
	newStarted := func(t *testing.T) *anthropicUpstreamResponsesStreamState {
		t.Helper()
		s := newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", usageOnlyPolicy())
		if _, err := s.Convert(anthropicmessages.StreamEvent{
			Type:    anthropicmessages.StreamEventTypeMessageStart,
			Message: &anthropicmessages.Response{ID: "msg_1"},
		}); err != nil {
			t.Fatalf("message_start: %v", err)
		}
		return s
	}
	blockStart := func(index int, kind anthropicmessages.ContentBlockType) anthropicmessages.StreamEvent {
		return anthropicmessages.StreamEvent{
			Type:         anthropicmessages.StreamEventTypeContentBlockStart,
			Index:        new(index),
			ContentBlock: &anthropicmessages.ContentBlock{Type: kind},
		}
	}
	blockStop := func(index int) anthropicmessages.StreamEvent {
		return anthropicmessages.StreamEvent{
			Type:  anthropicmessages.StreamEventTypeContentBlockStop,
			Index: new(index),
		}
	}
	usageOnlyDelta := func() anthropicmessages.StreamEvent {
		return anthropicmessages.StreamEvent{
			Type:  anthropicmessages.StreamEventTypeMessageDelta,
			Usage: &anthropicmessages.Usage{OutputTokens: 2},
		}
	}
	deltaWithReason := func(reason anthropicmessages.StopReason) anthropicmessages.StreamEvent {
		return anthropicmessages.StreamEvent{
			Type:  anthropicmessages.StreamEventTypeMessageDelta,
			Delta: &anthropicmessages.StreamDelta{StopReason: new(reason)},
		}
	}

	t.Run("usage-only delta with an open block is a wire error", func(t *testing.T) {
		s := newStarted(t)
		if _, err := s.Convert(blockStart(0, anthropicmessages.ContentBlockTypeText)); err != nil {
			t.Fatalf("block start: %v", err)
		}
		_, err := s.Convert(usageOnlyDelta())
		if err == nil || !isUpstreamConversionError(err) {
			t.Fatalf("usage-only delta with open block: err = %T %v, want an upstream wire error", err, err)
		}
		if !strings.Contains(err.Error(), "content block 0 stopped") {
			t.Fatalf("diagnostic should name the unclosed block: %v", err)
		}
	})

	t.Run("block start after a usage-only delta is a wire error", func(t *testing.T) {
		s := newStarted(t)
		if _, err := s.Convert(blockStart(0, anthropicmessages.ContentBlockTypeText)); err != nil {
			t.Fatalf("block start: %v", err)
		}
		if _, err := s.Convert(blockStop(0)); err != nil {
			t.Fatalf("block stop: %v", err)
		}
		if _, err := s.Convert(usageOnlyDelta()); err != nil {
			t.Fatalf("usage-only delta: %v", err)
		}
		_, err := s.Convert(blockStart(1, anthropicmessages.ContentBlockTypeText))
		if err == nil || !isUpstreamConversionError(err) {
			t.Fatalf("block start after usage-only delta: err = %T %v, want an upstream wire error", err, err)
		}
		if !strings.Contains(err.Error(), "after message_delta") {
			t.Fatalf("diagnostic should name the phase end: %v", err)
		}
	})

	t.Run("repeated deltas after the transition do not rescan blocks", func(t *testing.T) {
		s := newStarted(t)
		for i := range 50 {
			if _, err := s.Convert(blockStart(i, anthropicmessages.ContentBlockTypeText)); err != nil {
				t.Fatalf("block start %d: %v", i, err)
			}
			if _, err := s.Convert(blockStop(i)); err != nil {
				t.Fatalf("block stop %d: %v", i, err)
			}
		}
		for i := range 50 {
			if _, err := s.Convert(usageOnlyDelta()); err != nil {
				t.Fatalf("usage-only delta %d: %v", i, err)
			}
		}
		if !s.contentPhaseEnded {
			t.Fatal("contentPhaseEnded not latched at the first message_delta")
		}
		if _, err := s.Convert(deltaWithReason(anthropicmessages.StopReasonEndTurn)); err != nil {
			t.Fatalf("final delta: %v", err)
		}
		if _, err := s.Convert(anthropicmessages.StreamEvent{Type: anthropicmessages.StreamEventTypeMessageStop}); err != nil {
			t.Fatalf("message_stop: %v", err)
		}
		if !s.stopped {
			t.Fatal("message_stop did not stop the stream")
		}
	})
}

// TestAnthropicUpstreamStartStopReasonContradictionDirect pins the start
// stop-reason recording at the state level: a start reason that differs from
// the final delta reason is contradictory metadata.
func TestAnthropicUpstreamStartStopReasonContradictionDirect(t *testing.T) {
	s := newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", usageOnlyPolicy())
	refusal := anthropicmessages.StopReasonRefusal
	if _, err := s.Convert(anthropicmessages.StreamEvent{
		Type: anthropicmessages.StreamEventTypeMessageStart,
		Message: &anthropicmessages.Response{
			ID:         "msg_1",
			StopReason: &refusal,
		},
	}); err != nil {
		t.Fatalf("message_start(refusal): %v", err)
	}
	if !s.sawStartStopValue || s.startStopReason == nil || *s.startStopReason != refusal {
		t.Fatalf("start stop reason not recorded: saw=%v reason=%v", s.sawStartStopValue, s.startStopReason)
	}
	_, err := s.Convert(anthropicmessages.StreamEvent{
		Type:  anthropicmessages.StreamEventTypeMessageDelta,
		Delta: &anthropicmessages.StreamDelta{StopReason: new(anthropicmessages.StopReasonEndTurn)},
	})
	if err == nil || !isUpstreamConversionError(err) {
		t.Fatalf("contradictory delta: err = %T %v, want an upstream wire error", err, err)
	}
	if !strings.Contains(err.Error(), "message_start") {
		t.Fatalf("diagnostic should cite the message_start reason: %v", err)
	}

	// A matching refusal delta still works and the terminal is incomplete.
	s2 := newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", usageOnlyPolicy())
	if _, err := s2.Convert(anthropicmessages.StreamEvent{
		Type: anthropicmessages.StreamEventTypeMessageStart,
		Message: &anthropicmessages.Response{
			ID:         "msg_1",
			StopReason: &refusal,
			Usage:      &anthropicmessages.Usage{InputTokens: 3, OutputTokens: 1},
		},
	}); err != nil {
		t.Fatalf("message_start(refusal): %v", err)
	}
	if _, err := s2.Convert(anthropicmessages.StreamEvent{
		Type:  anthropicmessages.StreamEventTypeMessageDelta,
		Delta: &anthropicmessages.StreamDelta{StopReason: &refusal},
		Usage: &anthropicmessages.Usage{OutputTokens: 2},
	}); err != nil {
		t.Fatalf("matching refusal delta: %v", err)
	}
	events, err := s2.Convert(anthropicmessages.StreamEvent{Type: anthropicmessages.StreamEventTypeMessageStop})
	if err != nil {
		t.Fatalf("message_stop after matching refusal: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("terminal events = %d, want 1", len(events))
	}
	if _, ok := events[0].(ResponseIncompleteEvent); !ok {
		t.Fatalf("terminal = %T, want ResponseIncompleteEvent", events[0])
	}
	if !errors.Is(s2.streamError, nil) && s2.streamError != nil {
		t.Fatalf("streamError latched: %v", s2.streamError)
	}
}
