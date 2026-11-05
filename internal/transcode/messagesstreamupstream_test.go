package transcode

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

// geminiMessagesStreamHandler serves a streamGenerateContent exchange over an
// Anthropic Messages upstream with the given SSE body. The upstream request
// body is stored in captured when it is non-nil, and allowed lists the losses
// the policy approves.
func geminiMessagesStreamHandler(t *testing.T, sse string, captured *string, allowed ...Feature) http.Handler {
	t.Helper()
	key, err := NewRouteKey(http.MethodPost, "/v1beta/models/{model}:generateContent")
	if err != nil {
		t.Fatal(err)
	}
	// The usage approvals match the chat-upstream harness: the Messages
	// upstream carries no reasoning breakdown unless the provider sends one.
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageUnknown:           {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	for _, feature := range allowed {
		policy.Allowed[feature] = struct{}{}
	}
	mapping := Mapping{
		ClientRoute:      key,
		ClientProtocol:   ClientGemini,
		UpstreamProtocol: UpstreamMessages,
		UpstreamPath:     "/v1/messages",
		ModelMap:         ModelMap{AllowIdentity: true},
		Auth:             AuthPolicy{Mode: AuthNone},
		LossPolicy:       policy,
	}
	return NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			if captured != nil {
				data, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				*captured = string(data)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		},
		nil,
	)
}

const geminiMessagesStreamTarget = "/v1beta/models/claude-x:streamGenerateContent?alt=sse"

// anthropicStartSSE opens a message with the given input token count.
const anthropicStartSSE = "" +
	"event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n"

// anthropicTextBlockSSE is one text block made of two deltas, with a ping in
// between that carries no content.
const anthropicTextBlockSSE = "" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: ping\n" +
	`data: {"type":"ping"}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n"

func anthropicStopSSE(stopReason string) string {
	return "" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"` + stopReason + `","stop_sequence":null},"usage":{"output_tokens":2}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

func TestGeminiStreamMessagesTextAndTerminal(t *testing.T) {
	sse := anthropicStartSSE + anthropicTextBlockSSE + anthropicStopSSE("end_turn")
	var captured string
	rec := serveGeminiStreamBody(t,
		geminiMessagesStreamHandler(t, sse, &captured),
		geminiMessagesStreamTarget,
		geminiMessagesBody,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(captured, `"stream":true`) || !strings.Contains(captured, `"max_tokens":64`) {
		t.Fatalf("upstream body %s", captured)
	}
	frames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, frames)
	if got := geminiFrameText(frames); got != "hello" {
		t.Fatalf("text %q, want hello", got)
	}
	if finish := geminiFrameFinish(frames[len(frames)-1]); finish != "STOP" {
		t.Fatalf("finish %q, want STOP; body %s", finish, rec.Body.String())
	}
}

func TestGeminiStreamMessagesToolCall(t *testing.T) {
	sse := anthropicStartSSE +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		anthropicStopSSE("tool_use")
	rec := serveGeminiStreamBody(t,
		geminiMessagesStreamHandler(t, sse, nil),
		geminiMessagesStreamTarget,
		geminiMessagesBody,
	)
	frames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, frames)
	body := rec.Body.String()
	if !strings.Contains(body, `"functionCall"`) || !strings.Contains(body, `"name":"lookup"`) {
		t.Fatalf("no function call in stream: %s", body)
	}
	if !strings.Contains(body, `"q":"x"`) {
		t.Fatalf("arguments not carried through: %s", body)
	}
}

// anthropicThinkingSSE is a thinking block followed by an answer block.
const anthropicThinkingSSE = "" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n"

func TestGeminiStreamMessagesThinkingNeedsApproval(t *testing.T) {
	t.Run("refused when the loss is not approved", func(t *testing.T) {
		sse := anthropicStartSSE + anthropicThinkingSSE + anthropicStopSSE("end_turn")
		rec := serveGeminiStreamBody(t,
			geminiMessagesStreamHandler(t, sse, nil),
			geminiMessagesStreamTarget,
			geminiMessagesBody,
		)
		frames := geminiStreamFrames(t, rec.Body.String())
		for _, frame := range frames {
			if geminiFrameFinish(frame) != "" {
				t.Fatalf("unapproved thinking reported success: %v", frame)
			}
		}
		if _, ok := frames[len(frames)-1]["error"]; !ok {
			t.Fatalf("unapproved thinking did not end in an error frame: %v", frames)
		}
	})

	t.Run("approved loss drops the thinking and keeps the answer", func(t *testing.T) {
		sse := anthropicStartSSE + anthropicThinkingSSE + anthropicStopSSE("end_turn")
		rec := serveGeminiStreamBody(t,
			geminiMessagesStreamHandler(t, sse, nil, FeatureAuthenticatedThinking),
			geminiMessagesStreamTarget,
			geminiMessagesBody,
		)
		frames := geminiStreamFrames(t, rec.Body.String())
		assertSingleTerminal(t, frames)
		if got := geminiFrameText(frames); got != "answer" {
			t.Fatalf("text %q, want answer", got)
		}
		if strings.Contains(rec.Body.String(), "hmm") {
			t.Fatalf("thinking text reached the client: %s", rec.Body.String())
		}
	})
}

func TestGeminiStreamMessagesMaxTokensIsIncomplete(t *testing.T) {
	sse := anthropicStartSSE + anthropicTextBlockSSE + anthropicStopSSE("max_tokens")
	rec := serveGeminiStreamBody(t,
		geminiMessagesStreamHandler(t, sse, nil),
		geminiMessagesStreamTarget,
		geminiMessagesBody,
	)
	frames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, frames)
	if finish := geminiFrameFinish(frames[len(frames)-1]); finish != "MAX_TOKENS" {
		t.Fatalf("finish %q, want MAX_TOKENS; body %s", finish, rec.Body.String())
	}
}

func TestGeminiStreamMessagesFailuresNeverSucceed(t *testing.T) {
	tests := []struct {
		name string
		sse  string
	}{
		{
			name: "truncated before message_stop",
			sse:  anthropicStartSSE + anthropicTextBlockSSE,
		},
		{
			name: "upstream error event",
			sse: anthropicStartSSE + anthropicTextBlockSSE +
				"event: error\n" +
				`data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}` + "\n\n",
		},
		{
			name: "malformed frame",
			sse:  anthropicStartSSE + "event: content_block_start\ndata: {\"type\": oops\n\n",
		},
		{
			name: "event name disagrees with the JSON type",
			sse:  anthropicStartSSE + "event: ping\n" + `data: {"type":"message_stop"}` + "\n\n",
		},
		{
			name: "message_stop with an open block",
			sse: anthropicStartSSE +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				anthropicStopSSE("end_turn"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serveGeminiStreamBody(t,
				geminiMessagesStreamHandler(t, tt.sse, nil),
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
				t.Fatalf("failed stream did not end in an error frame: %v", frames)
			}
		})
	}
}

func TestGeminiStreamMessagesToleratesEnvelopeExtensions(t *testing.T) {
	start := "" +
		"event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x","stop_reason":null,"stop_sequence":null,"container":null,"usage":{"input_tokens":3,"output_tokens":1,"service_tier":"standard","cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"server_tool_use":{"web_search_requests":0}}}}` + "\n\n"
	stop := "" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null,"stop_details":null},"usage":{"output_tokens":2,"server_tool_use":{"web_search_requests":0}}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	rec := serveGeminiStreamBody(t,
		geminiMessagesStreamHandler(t, start+anthropicTextBlockSSE+stop, nil),
		geminiMessagesStreamTarget,
		geminiMessagesBody,
	)
	frames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, frames)
	if got := geminiFrameText(frames); got != "hello" {
		t.Fatalf("text %q, want hello; body %s", got, rec.Body.String())
	}
	if finish := geminiFrameFinish(frames[len(frames)-1]); finish != "STOP" {
		t.Fatalf("finish %q, want STOP; body %s", finish, rec.Body.String())
	}
}

func TestDecodeAnthropicStreamFrameContentStaysStrict(t *testing.T) {
	_, err := decodeAnthropicStreamFrame(SSEEvent{
		Event: "content_block_delta",
		Data:  []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x","surprise":1}}`),
	})
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("unknown field on a content delta: err = %T %v, want *UpstreamWireError", err, err)
	}
	_, err = decodeAnthropicStreamFrame(SSEEvent{
		Event: "content_block_start",
		Data:  []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"","surprise":1}}`),
	})
	if err == nil {
		t.Fatal("unknown field on a content block: nil error, want rejection")
	}
}

func TestAnthropicUpstreamStreamBudgetIsUpstreamFault(t *testing.T) {
	s := newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", LossPolicy{})
	s.budget.events = s.budget.eventsBudget
	_, err := s.Convert(anthropicmessages.StreamEvent{Type: anthropicmessages.StreamEventTypePing})
	if !isUpstreamConversionError(err) {
		t.Fatalf("event budget: err = %T %v, want an upstream conversion error", err, err)
	}
}

func TestGeminiStreamMessagesErrorEventLatchesFailure(t *testing.T) {
	// The upstream error event is a failure terminal: the composed converter
	// and the upstream state must keep reporting it, never a clean terminal.
	upstream := newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", usageOnlyPolicy())
	gemini := newGeminiStreamClientState(
		testStreamContext(), UpstreamMessages, usageOnlyPolicy(), "resp_1", "claude-x",
	)
	converter := newMessagesToGeminiConverter(upstream, gemini)

	if _, err := converter.Convert(SSEEvent{
		Event: "message_start",
		Data:  []byte(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`),
	}); err != nil {
		t.Fatalf("message_start: %v", err)
	}
	if _, err := converter.Convert(SSEEvent{
		Event: "error",
		Data:  []byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`),
	}); err == nil || !isUpstreamConversionError(err) {
		t.Fatalf("error event: err = %T %v, want an upstream conversion error", err, err)
	}
	latched := upstream.streamError
	if latched == nil || !strings.Contains(latched.Error(), "busy") {
		t.Fatalf("upstream failure not latched: %v", latched)
	}
	if _, err := upstream.Convert(anthropicmessages.StreamEvent{
		Type: anthropicmessages.StreamEventTypePing,
	}); !errors.Is(err, latched) {
		t.Fatalf("event after error: err = %v, want the latched %v", err, latched)
	}
	batch, err := converter.FinalizeEOF()
	if !errors.Is(err, latched) {
		t.Fatalf("FinalizeEOF after error event: err = %v, want the latched %v", err, latched)
	}
	if batch.Terminal || len(batch.Events) != 0 {
		t.Fatalf("FinalizeEOF after error event released %+v", batch)
	}

	// An error event with no message still names the failure type, so the
	// latched failure and the client-facing event are not blank.
	blank := newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", usageOnlyPolicy())
	if _, err := blank.Convert(anthropicmessages.StreamEvent{
		Type: anthropicmessages.StreamEventTypeError,
		Error: &anthropicmessages.StreamError{
			Type: "overloaded_error",
		},
	}); err != nil {
		t.Fatalf("blank error event: %v", err)
	}
	if blank.streamError == nil || !strings.Contains(blank.streamError.Error(), "overloaded_error") {
		t.Fatalf("blank error event latched %v, want a message naming the type", blank.streamError)
	}
}

func TestAnthropicUpstreamIgnoredBlockLossesAreGated(t *testing.T) {
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureAuthenticatedThinking: {},
		FeatureAnthropicServerTools:  {},
	}}
	newState := func(t *testing.T) *anthropicUpstreamResponsesStreamState {
		t.Helper()
		s := newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", policy)
		if _, err := s.Convert(anthropicmessages.StreamEvent{
			Type:    anthropicmessages.StreamEventTypeMessageStart,
			Message: &anthropicmessages.Response{},
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

	// Repeated ignored blocks record one loss per stream, not one per block.
	s := newState(t)
	for index, kind := range []anthropicmessages.ContentBlockType{
		anthropicmessages.ContentBlockTypeThinking,
		anthropicmessages.ContentBlockTypeRedactedThinking,
	} {
		if _, err := s.Convert(blockStart(index, kind)); err != nil {
			t.Fatalf("thinking block %d: %v", index, err)
		}
	}
	if n := countReportFeature(s.report, FeatureAuthenticatedThinking); n != 1 {
		t.Fatalf("thinking loss recorded %d times, want 1: %+v", n, s.report.Losses)
	}

	s = newState(t)
	for index, kind := range []anthropicmessages.ContentBlockType{
		anthropicmessages.ContentBlockTypeServerToolUse,
		anthropicmessages.ContentBlockTypeWebSearchToolResult,
	} {
		if _, err := s.Convert(blockStart(index, kind)); err != nil {
			t.Fatalf("server tool block %d: %v", index, err)
		}
	}
	if n := countReportFeature(s.report, FeatureAnthropicServerTools); n != 1 {
		t.Fatalf("server-tool loss recorded %d times, want 1: %+v", n, s.report.Losses)
	}

	// A block rejected by the state-entry budget records no loss.
	s = newState(t)
	s.budget.stateEntries = s.budget.stateEntriesBudget
	if _, err := s.Convert(blockStart(0, anthropicmessages.ContentBlockTypeThinking)); err == nil {
		t.Fatal("thinking block over the state-entry budget: nil error")
	}
	if reportHasFeature(s.report, FeatureAuthenticatedThinking) {
		t.Fatalf("thinking loss recorded for a rejected block: %+v", s.report.Losses)
	}
	s = newState(t)
	s.budget.stateEntries = s.budget.stateEntriesBudget
	if _, err := s.Convert(blockStart(0, anthropicmessages.ContentBlockTypeServerToolUse)); err == nil {
		t.Fatal("server tool block over the state-entry budget: nil error")
	}
	if reportHasFeature(s.report, FeatureAnthropicServerTools) {
		t.Fatalf("server-tool loss recorded for a rejected block: %+v", s.report.Losses)
	}
}
