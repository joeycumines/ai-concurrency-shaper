package transcode

import (
	"io"
	"net/http"
	"strings"
	"testing"
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
