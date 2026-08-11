package transcode

// J3 regression tests (review-k finding 3, high): corrupt upstream wire and
// protocol data must classify as an upstream failure — never a local
// conversion failure — via the typed UpstreamWireError, while valid source
// features the transcoder knows but does not support (UnsupportedFeatureError)
// and loss-policy rejections stay local.

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestUpstreamWireDecodeMatrix proves the non-streaming decode boundary
// classifies each corrupt-wire form as UpstreamWireError and keeps typed
// unsupported features local.
func TestUpstreamWireDecodeMatrix(t *testing.T) {
	tests := []struct {
		name string
		body string
		// wantWire reports whether the error must be an UpstreamWireError.
		wantWire bool
		// wantUnsupported reports whether the error must remain an
		// UnsupportedFeatureError (local).
		wantUnsupported bool
		// wantAccept reports that the decode must succeed.
		wantAccept bool
	}{
		{
			name:     "chat invalid envelope",
			body:     `{"not":"chat"}`,
			wantWire: true,
		},
		{
			name:     "chat malformed JSON",
			body:     `{"id":`,
			wantWire: true,
		},
		{
			name:     "chat no choices",
			body:     `{"id":"c","object":"chat.completion"}`,
			wantWire: true,
		},
		{
			name:     "chat multiple choices",
			body:     `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"a"}},{"index":1,"finish_reason":"stop","message":{"role":"assistant","content":"b"}}]}`,
			wantWire: true,
		},
		{
			name:     "chat missing message",
			body:     `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop"}]}`,
			wantWire: true,
		},
		// Invalid model-generated tool arguments are PRESERVED byte-exact,
		// never corrupt wire (review-z commit 2); the preservation is
		// asserted in TestToolArgumentsFidelityAcrossTargets.
		{
			name:       "chat invalid tool arguments preserved",
			body:       `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
			wantAccept: true,
		},
		{
			name:     "chat unknown field",
			body:     `{"id":"c","object":"chat.completion","created":1,"model":"m","bogus":1,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]}`,
			wantWire: true,
		},
		{
			name:            "chat unsupported finish reason",
			body:            `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"weird","message":{"role":"assistant","content":"x"}}]}`,
			wantUnsupported: true,
		},
		{
			name:            "chat unsupported image content",
			body:            `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"https://example.test/x.png"}}]}}]}`,
			wantUnsupported: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeChatResponse([]byte(tt.body), ChatCapabilities{})
			if tt.wantAccept {
				if err != nil {
					t.Fatalf("decode = %v, want acceptance", err)
				}
				return
			}
			assertWireClassification(t, err, tt.wantWire, tt.wantUnsupported)
		})
	}

	// The Responses decoder shares the same boundary doctrine.
	if _, err := DecodeResponsesResponse([]byte(`{"not":"responses"}`)); err == nil {
		t.Fatal("expected decode error")
	} else {
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("responses err = %T %v, want *UpstreamWireError", err, err)
		}
		if wireErr.Protocol != UpstreamResponses {
			t.Fatalf("responses protocol = %v", wireErr.Protocol)
		}
	}
	// Invalid model-generated Responses tool arguments are preserved
	// byte-exact (review-z commit 2): the decode succeeds and carries the
	// raw string.
	response, err := DecodeResponsesResponse([]byte(
		`{"id":"r","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"f","arguments":"{\"a\":"}]}`,
	))
	if err != nil {
		t.Fatalf("invalid model arguments rejected: %v", err)
	}
	call, ok := response.Items[0].(*CanonicalFunctionCallItem)
	if !ok || call.Arguments.Raw != `{"a":` || call.Arguments.IsObject {
		t.Fatalf("arguments = %+v, want the raw string preserved", call.Arguments)
	}
	if _, err := DecodeResponsesResponse([]byte(
		`{"id":"r","object":"response","created_at":1,"status":"bogus","model":"m","output":[]}`,
	)); err == nil {
		t.Fatal("expected decode error")
	} else {
		assertWireClassification(t, err, false, true)
	}
	// A valid-but-unsupported OUTPUT ITEM type (web_search_call) nested
	// inside an otherwise valid envelope must stay local: the typed
	// UnsupportedFeatureError is never wrapped as corrupt wire even when it
	// surfaces through a strict decode boundary.
	if _, err := DecodeResponsesResponse([]byte(
		`{"id":"r","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"ws_1","type":"web_search_call","status":"completed","web_search":{"query":"x"}}]}`,
	)); err == nil {
		t.Fatal("expected decode error")
	} else {
		assertWireClassification(t, err, false, true)
	}
}

// assertWireClassification asserts the boundary doctrine for one decode
// error: corrupt wire is UpstreamWireError (never UnsupportedFeatureError),
// and a known-but-unsupported feature is UnsupportedFeatureError (never
// wrapped as corrupt wire).
func assertWireClassification(
	t *testing.T,
	err error,
	wantWire bool,
	wantUnsupported bool,
) {
	t.Helper()
	if wantWire {
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
		var unsupportedErr *UnsupportedFeatureError
		if errors.As(err, &unsupportedErr) {
			t.Fatal("corrupt wire must not be an UnsupportedFeatureError")
		}
		return
	}
	if wantUnsupported {
		var unsupportedErr *UnsupportedFeatureError
		if !errors.As(err, &unsupportedErr) {
			t.Fatalf("err = %T %v, want *UnsupportedFeatureError", err, err)
		}
		var wireErr *UpstreamWireError
		if errors.As(err, &wireErr) {
			t.Fatal("unsupported feature must not be wrapped as corrupt wire")
		}
		return
	}
	t.Fatalf("test case missing expectation")
}

// TestUpstreamWireStreamMatrix proves the streaming boundaries classify each
// corrupt-wire form as an upstream error frame (SawUpstreamErrorFrame) and
// keep typed unsupported features and loss rejections local.
func TestUpstreamWireStreamMatrix(t *testing.T) {
	tests := []struct {
		name string
		// direction selects the converter: "chat" (chat→responses) or
		// "responses" (responses→anthropic).
		direction string
		// frames are the raw upstream SSE frames, one per event.
		frames []string
		// wantUpstream expects the reader to stop with an UpstreamWireError
		// classified as an upstream error frame.
		wantUpstream bool
		// wantUnsupported expects an UnsupportedFeatureError that stays
		// local (no upstream error frame). Loss-policy rejections return
		// UnsupportedFeatureError too (report.Lose), so they share this
		// bucket.
		wantUnsupported bool
		// wantSuccess expects the stream to terminate cleanly.
		wantSuccess bool
	}{
		{
			name:      "chat malformed chunk JSON",
			direction: "chat",
			frames: []string{
				"data: not-json\n\n",
			},
			wantUpstream: true,
		},
		{
			name:      "chat chunk id mismatch",
			direction: "chat",
			frames: []string{
				"data: {\"id\":\"a\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
				"data: {\"id\":\"b\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"y\"},\"finish_reason\":null}]}\n\n",
			},
			wantUpstream: true,
		},
		{
			name:      "chat usage tail id mismatch",
			direction: "chat",
			frames: []string{
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
				"data: {\"id\":\"other\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n",
			},
			wantUpstream: true,
		},
		{
			name:      "chat usage tail model mismatch",
			direction: "chat",
			frames: []string{
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"other\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n",
			},
			wantUpstream: true,
		},
		{
			name:      "chat chunk multiple choices",
			direction: "chat",
			frames: []string{
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null},{\"index\":1,\"delta\":{\"content\":\"y\"},\"finish_reason\":null}]}\n\n",
			},
			wantUpstream: true,
		},
		{
			name:      "chat chunk choice index nonzero",
			direction: "chat",
			frames: []string{
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":1,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
			},
			wantUpstream: true,
		},
		{
			// Invalid model-generated tool arguments are preserved
			// byte-exact in the emitted function_call arguments, never
			// corrupt wire (review-z commit 2).
			name:      "chat invalid final tool arguments preserved",
			direction: "chat",
			frames: []string{
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":\"}}]},\"finish_reason\":null}]}\n\n",
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
				"data: [DONE]\n\n",
			},
			wantSuccess: true,
		},
		{
			name:      "composed chat wire error",
			direction: "composed",
			frames: []string{
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":1,\"delta\":{\"content\":\"y\"},\"finish_reason\":null}]}\n\n",
			},
			wantUpstream: true,
		},
		{
			name:      "responses malformed event JSON",
			direction: "responses",
			frames: []string{
				"data: not-json\n\n",
			},
			wantUpstream: true,
		},
		{
			name:      "responses event name type mismatch",
			direction: "responses",
			frames: []string{
				"event: response.created\n" +
					"data: {\"type\":\"response.completed\",\"sequence_number\":0,\"response\":{\"id\":\"r\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"m\",\"output\":[]}}\n\n",
			},
			wantUpstream: true,
		},
		{
			name:      "responses text delta with no open block",
			direction: "responses",
			frames: []string{
				"event: response.created\n" +
					"data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"r\",\"object\":\"response\",\"created_at\":1,\"status\":\"in_progress\",\"model\":\"m\",\"output\":[]}}\n\n",
				"event: response.output_text.delta\n" +
					"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"x\"}\n\n",
			},
			wantUpstream: true,
		},
		{
			name:      "responses duplicate message start",
			direction: "responses",
			frames: []string{
				"event: response.created\n" +
					"data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"r\",\"object\":\"response\",\"created_at\":1,\"status\":\"in_progress\",\"model\":\"m\",\"output\":[]}}\n\n",
				"event: response.created\n" +
					"data: {\"type\":\"response.created\",\"sequence_number\":1,\"response\":{\"id\":\"r\",\"object\":\"response\",\"created_at\":1,\"status\":\"in_progress\",\"model\":\"m\",\"output\":[]}}\n\n",
			},
			wantUpstream: true,
		},
		{
			name:      "responses done before terminal",
			direction: "responses",
			frames: []string{
				"data: [DONE]\n\n",
			},
			wantUpstream: true,
		},
		{
			name:      "responses unsupported event type",
			direction: "responses",
			frames: []string{
				"data: {\"type\":\"bogus\"}\n\n",
			},
			wantUnsupported: true,
		},
		{
			name:      "responses unsupported output item type",
			direction: "responses",
			frames: []string{
				"event: response.output_item.added\n" +
					"data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"output_index\":0,\"item\":{\"id\":\"ws_1\",\"type\":\"web_search_call\",\"status\":\"completed\",\"web_search\":{\"query\":\"x\"}}}\n\n",
			},
			wantUnsupported: true,
		},
		{
			name:      "chat unsupported finish reason",
			direction: "chat",
			frames: []string{
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"weird\"}]}\n\n",
			},
			wantUnsupported: true,
		},
		{
			name:      "chat service tier loss rejection",
			direction: "chat",
			frames: []string{
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"service_tier\":\"auto\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
			},
			wantUnsupported: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Join(tt.frames, "")
			var reader *convertingReader
			if tt.direction == "responses" {
				state := newAnthropicResponsesStreamState(
					testStreamContext(),
					j6PermissivePolicy(),
					"msg_1",
					"m",
					1,
				)
				reader = newConvertingReader(
					NewSSEReaderWithLimits(strings.NewReader(input), 0, 0),
					newResponsesToAnthropicConverter(state),
				)
			} else if tt.direction == "composed" {
				chat := newChatResponsesStreamState(
					testStreamContext(),
					StrictLossPolicy(),
					ChatCapabilities{},
					"resp_1",
					"m",
					1,
					nil,
				)
				anthropic := newAnthropicResponsesStreamState(
					testStreamContext(),
					j6PermissivePolicy(),
					"msg_1",
					"m",
					1,
				)
				reader = newConvertingReader(
					NewSSEReaderWithLimits(strings.NewReader(input), 0, 0),
					newChatToAnthropicConverter(chat, anthropic),
				)
			} else {
				state := newChatResponsesStreamState(
					testStreamContext(),
					StrictLossPolicy(),
					ChatCapabilities{},
					"resp_1",
					"m",
					1,
					nil,
				)
				reader = newConvertingReader(
					NewSSEReaderWithLimits(strings.NewReader(input), 0, 0),
					newChatToResponsesConverter(state),
				)
			}

			_, readErr := drainReader(t, reader)
			var wireErr *UpstreamWireError
			isWire := errors.As(readErr, &wireErr)
			var unsupportedErr *UnsupportedFeatureError
			isUnsupported := errors.As(readErr, &unsupportedErr)

			switch {
			case tt.wantSuccess:
				// drainReader reports the terminal EOF; a clean terminal is
				// EOF (or nil), never a typed error.
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					t.Fatalf("read err = %v, want a clean terminal", readErr)
				}
				if reader.SawUpstreamErrorFrame() {
					t.Fatal("preserved arguments must not mark the stream as an upstream error")
				}
				if !reader.SawTerminal() {
					t.Fatal("stream must terminate cleanly")
				}
			case tt.wantUpstream:
				if !isWire {
					t.Fatalf("read err = %T %v, want *UpstreamWireError", readErr, readErr)
				}
				if !reader.SawUpstreamErrorFrame() {
					t.Fatal("corrupt wire not marked as an upstream error frame")
				}
				if reader.SawTerminal() {
					t.Fatal("corrupt wire must not report a success terminal")
				}
			case tt.wantUnsupported:
				if isWire {
					t.Fatalf("unsupported feature wrapped as wire error: %v", readErr)
				}
				if !isUnsupported {
					t.Fatalf("err = %T %v, want *UnsupportedFeatureError", readErr, readErr)
				}
				if reader.SawUpstreamErrorFrame() {
					t.Fatal("unsupported feature must not be marked as an upstream error frame")
				}
			default:
				t.Fatalf("test case missing expectation")
			}
		})
	}
}
