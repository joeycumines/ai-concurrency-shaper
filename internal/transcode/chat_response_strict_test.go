package transcode

// J4 regression tests (review-k finding 4, high): the non-streaming Chat
// response decode is presence-aware and strict — the pinned required fields
// (object, one choice, choice index 0, finish_reason, message with role
// assistant, complete tool-call identity) must be explicitly present, never
// defaulted — and every violation is corrupt upstream wire. Streaming chunks
// reject a wrong object discriminator.

import (
	"errors"
	"strings"
	"testing"
)

// chatResponseBase is a minimal valid Chat response used to build the
// negative fixtures.
const chatResponseBase = `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]}`

// TestChatResponseStrictPresenceMatrix proves every presence requirement of
// the pinned Chat response contract: each malformed variant is rejected with
// an UpstreamWireError, never defaulted into a successful assistant response.
func TestChatResponseStrictPresenceMatrix(t *testing.T) {
	tests := []struct {
		name string
		body string
		// wantUnsupported marks cases that are rejected as a
		// known-but-unsupported feature (local) rather than corrupt wire.
		wantUnsupported bool
	}{
		{
			name: "missing finish reason",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"}}]}`,
		},
		{
			name: "null finish reason",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":null,"message":{"role":"assistant","content":"x"}}]}`,
		},
		{
			name: "empty finish reason",
			// An empty string is present but not in the supported enum: it is
			// rejected like any unknown finish_reason value — a
			// known-but-unsupported feature (local), never defaulted to
			// stop.
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"","message":{"role":"assistant","content":"x"}}]}`,
			// wantUnsupported mirrors the pinned unknown-finish_reason
			// convention (blueprint task 3 keeps it local).
			wantUnsupported: true,
		},
		{
			name: "user role message",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"user","content":"x"}}]}`,
		},
		{
			name: "missing message role",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"content":"x"}}]}`,
		},
		{
			name: "assistant message with tool call id",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x","tool_call_id":"t1"}}]}`,
		},
		{
			name: "choice index one",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":1,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]}`,
		},
		{
			name: "choice index absent",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]}`,
		},
		{
			name: "object absent",
			body: `{"id":"c","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]}`,
		},
		{
			name: "object wrong",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]}`,
		},
		{
			name: "message absent",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop"}]}`,
		},
		{
			name: "tool call empty name",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"","arguments":"{}"}}]}}]}`,
		},
		{
			name: "tool call missing name",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"arguments":"{}"}}]}}]}`,
		},
		{
			name: "tool call missing arguments",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f"}}]}}]}`,
		},
		{
			name: "tool call empty arguments",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]}}]}`,
		},
		{
			name: "tool call non-object arguments",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"[1,2]"}}]}}]}`,
		},
		{
			name: "tool call missing id",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
		},
		{
			name: "tool call wrong type",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"bogus","function":{"name":"f","arguments":"{}"}}]}}]}`,
		},
		{
			name: "tool call missing type",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeChatResponse([]byte(tt.body), ChatCapabilities{})
			if tt.wantUnsupported {
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
			var wireErr *UpstreamWireError
			if !errors.As(err, &wireErr) {
				t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
			}
			if wireErr.Protocol != UpstreamChatCompletions {
				t.Fatalf("protocol = %v", wireErr.Protocol)
			}
		})
	}
}

// TestChatResponseReviewKCounterexampleIsRejected proves the exact review-k
// counterexample — a single choice with index zero, a user-role message, and
// no finish_reason — is rejected as corrupt upstream wire and can never
// become a successful assistant response.
func TestChatResponseReviewKCounterexampleIsRejected(t *testing.T) {
	body := []byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"user","content":"x"}}]}`)
	_, err := DecodeChatResponse(body, ChatCapabilities{})
	var wireErr *UpstreamWireError
	if !errors.As(err, &wireErr) {
		t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
	}
	if !strings.Contains(wireErr.Error(), "finish_reason") {
		t.Fatalf("error = %q, want the finish_reason violation", wireErr.Error())
	}
}

// TestChatResponseOfficialShapesStillDecode proves the official-shaped
// fixtures decode under the strict presence requirements: stop, length,
// content_filter, refusal, and complete tool calls.
func TestChatResponseOfficialShapesStillDecode(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "stop",
			body: chatResponseBase,
		},
		{
			name: "length",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":"x"}}]}`,
		},
		{
			name: "content filter",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"content_filter","message":{"role":"assistant","content":"x"}}]}`,
		},
		{
			name: "function call legacy reason",
			// The legacy finish_reason value is in the supported enum. The
			// legacy message.function_call FIELD is not part of the modeled
			// surface (an unknown-field rejection), so it is not used here.
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"function_call","message":{"role":"assistant","content":"x"}}]}`,
		},
		{
			name: "refusal",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x","refusal":"no"}}]}`,
		},
		{
			name: "tool calls",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}}]}}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response, err := DecodeChatResponse([]byte(tt.body), ChatCapabilities{})
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Turns) != 1 || len(response.Turns[0].Parts) == 0 {
				t.Fatalf("response = %+v", response)
			}
		})
	}
}

// TestChatStreamChunkObjectDiscriminator proves the streaming parity: the
// chunk object discriminator is a required field of the pinned envelope —
// a chunk with a missing or wrong object is corrupt upstream wire
// (review-08 blocker 2); the correct discriminator passes.
func TestChatStreamChunkObjectDiscriminator(t *testing.T) {
	good := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`
	if _, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(good)}); err != nil {
		t.Fatalf("good chunk rejected: %v", err)
	}

	absent := `{"id":"c","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`
	if _, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(absent)}); err == nil {
		t.Fatal("object-absent chunk accepted")
	} else {
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
	}

	wrong := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`
	if _, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(wrong)}); err == nil {
		t.Fatal("wrong object discriminator accepted")
	} else {
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
	}
}
