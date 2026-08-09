package transcode

// Review-08 blocker 2 regression tests: the Chat stream chunk decode is
// strict and presence-aware (the pinned envelope fields are required, never
// zero-defaulted), non-assistant roles are rejected rather than relabeled,
// tool-call fragments enforce the pinned index and type and keep immutable
// identity, streaming usage never fabricates omitted totals, and the held
// terminal is released ONLY by the [DONE] sentinel — EOF after finish_reason
// without [DONE] is a typed upstream truncation.

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// chatStreamChunkBase is a minimal pin-conformant streaming chunk used to
// build the negative fixtures.
const chatStreamChunkBase = `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`

// assertChatStreamChunkWireError asserts err is a typed Chat upstream wire
// error.
func assertChatStreamChunkWireError(t *testing.T, err error) {
	t.Helper()
	var wireErr *UpstreamWireError
	if !errors.As(err, &wireErr) {
		t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
	}
	if wireErr.Protocol != UpstreamChatCompletions {
		t.Fatalf("protocol = %v, want chat completions", wireErr.Protocol)
	}
}

// TestChatStreamRejectsMissingRequiredEnvelopeFields proves every chunk
// envelope and choice requirement of the pinned Chat streaming contract is
// enforced: missing or wrong object, missing id/model/created, a missing or
// wrong choice index, more than one choice, a missing delta, a message arm
// coexisting with a delta, unknown fields, and a usage object omitting any
// required total are all corrupt upstream wire (review-08 blocker 2).
func TestChatStreamRejectsMissingRequiredEnvelopeFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "missing object",
			body: `{"id":"c","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`,
		},
		{
			name: "wrong object",
			body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`,
		},
		{
			name: "missing id",
			body: `{"object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`,
		},
		{
			name: "missing model",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`,
		},
		{
			name: "missing created",
			body: `{"id":"c","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`,
		},
		{
			name: "choice missing index",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"delta":{"content":"x"},"finish_reason":null}]}`,
		},
		{
			name: "choice index one",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":1,"delta":{"content":"x"},"finish_reason":null}]}`,
		},
		{
			name: "two choices",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null},{"index":0,"delta":{"content":"y"},"finish_reason":null}]}`,
		},
		{
			name: "choice missing delta",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":null}]}`,
		},
		{
			name: "message arm coexists with delta",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"message":{"role":"assistant","content":"y"},"finish_reason":null}]}`,
		},
		{
			name: "unknown envelope field",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","bogus":1,"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`,
		},
		{
			name: "unknown choice field",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null,"bogus":1}]}`,
		},
		{
			name: "malformed json",
			body: `{"id":"c","object":"chat.completion.chunk",`,
		},
		{
			name: "usage omits prompt tokens",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"completion_tokens":5,"total_tokens":5}}`,
		},
		{
			name: "usage omits completion tokens",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":5,"total_tokens":5}}`,
		},
		{
			name: "usage omits total tokens",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":5}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(tt.body)})
			assertChatStreamChunkWireError(t, err)
		})
	}

	// The official shape still decodes.
	if _, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(chatStreamChunkBase)}); err != nil {
		t.Fatalf("official shape rejected: %v", err)
	}
}

// TestChatStreamRejectsNonAssistantRole proves a delta role that is present
// but not assistant is corrupt upstream wire — never relabeled as assistant
// output (review-08 blocker 2). The review's minimal malformed chunk, which
// also omits every envelope field, is rejected for the envelope first.
func TestChatStreamRejectsNonAssistantRole(t *testing.T) {
	// The review's exact counterexample chunk: a user-role delta and no
	// envelope fields at all.
	review := `{"choices":[{"delta":{"role":"user","content":"x"},"finish_reason":"stop"}]}`
	_, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(review)})
	assertChatStreamChunkWireError(t, err)

	// A user-role delta with a full envelope is rejected for the role.
	full := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"user","content":"x"},"finish_reason":null}]}`
	_, err = chatStreamChunkFromSSE(SSEEvent{Data: []byte(full)})
	assertChatStreamChunkWireError(t, err)
	if !strings.Contains(err.Error(), "role") {
		t.Fatalf("error = %q, want the role violation", err.Error())
	}

	// The assistant role passes.
	ok := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"x"},"finish_reason":null}]}`
	if _, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(ok)}); err != nil {
		t.Fatalf("assistant role rejected: %v", err)
	}
}

// TestChatStreamToolCallFragmentEnforced proves the pinned streaming
// tool-call fragment contract: index is required, a present type must be
// "function", and identity (call id and name) is immutable once the
// output_item.added event was announced — a later fragment changing identity
// or resolving through a conflicting index is corrupt upstream wire
// (review-08 blocker 2).
func TestChatStreamToolCallFragmentEnforced(t *testing.T) {
	t.Run("decode index and type", func(t *testing.T) {
		base := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[%s]},"finish_reason":null}]}`
		tests := []struct {
			name string
			call string
			want bool // wantErr
		}{
			{"missing index", `{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}`, true},
			{"wrong type", `{"index":0,"id":"call_1","type":"bogus","function":{"name":"f","arguments":"{}"}}`, true},
			// The pinned contract marks type optional on the streaming
			// fragment; only a present non-function type is corrupt.
			{"missing type", `{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}`, false},
			{"complete", `{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}`, false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(fmt.Sprintf(base, tt.call))})
				if tt.want {
					assertChatStreamChunkWireError(t, err)
				} else if err != nil {
					t.Fatalf("chunk rejected: %v", err)
				}
			})
		}
	})

	t.Run("identity immutability", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"gpt-4.1",
			1710000000,
			nil,
		)
		chunk := func(calls ...ChatToolCallDelta) ChatStreamResponse {
			return ChatStreamResponse{
				ID:      "c",
				Object:  "chat.completion.chunk",
				Created: 1710000000,
				Model:   "gpt-4.1",
				Choices: []ChatChoice{{
					Index: 0,
					Delta: &ChatStreamDelta{ToolCalls: calls},
				}},
			}
		}
		// The added event is announced once id and name are both known.
		if _, err := state.Convert(chunk(ChatToolCallDelta{
			Index: intPtr(0),
			ID:    stringPtr("call_1"),
			Type:  stringPtr("function"),
			Function: ChatToolCallFunction{
				Name:      stringPtr("f"),
				Arguments: `{"x":`,
			},
		})); err != nil {
			t.Fatal(err)
		}
		tests := []struct {
			name string
			call ChatToolCallDelta
		}{
			{
				name: "index carries a different id",
				call: ChatToolCallDelta{
					Index:    intPtr(0),
					ID:       stringPtr("call_zz"),
					Function: ChatToolCallFunction{Name: stringPtr("f")},
				},
			},
			{
				name: "name changes after added",
				call: ChatToolCallDelta{Index: intPtr(0), Function: ChatToolCallFunction{Name: stringPtr("g")}},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := state.Convert(chunk(tt.call))
				var wireErr *UpstreamWireError
				if !errors.As(err, &wireErr) {
					t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
				}
			})
		}
		// An index-less, id-less continuation resolves to the single pending
		// call and is accepted.
		if _, err := state.Convert(chunk(ChatToolCallDelta{
			Function: ChatToolCallFunction{
				Arguments: `"y":1}`,
			},
		})); err != nil {
			t.Fatalf("single-pending continuation rejected: %v", err)
		}
		// A second call opens at index 1.
		if _, err := state.Convert(chunk(ChatToolCallDelta{
			Index: intPtr(1),
			ID:    stringPtr("call_2"),
			Type:  stringPtr("function"),
			Function: ChatToolCallFunction{
				Name:      stringPtr("g"),
				Arguments: "{}",
			},
		})); err != nil {
			t.Fatal(err)
		}
		// The fragment id resolves to call_1 while its index resolves to
		// call_2: the conflicting index must never be silently ignored.
		_, err := state.Convert(chunk(ChatToolCallDelta{
			Index: intPtr(1),
			ID:    stringPtr("call_1"),
		}))
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
		if !strings.Contains(err.Error(), "resolves to call") {
			t.Fatalf("error = %q, want the conflicting-index violation", err.Error())
		}
		// With two pending calls, an index-less, id-less fragment is
		// ambiguous.
		_, err = state.Convert(chunk(ChatToolCallDelta{
			Function: ChatToolCallFunction{Name: stringPtr("h")},
		}))
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
		// Same identity fragments remain accepted (argument continuation).
		if _, err := state.Convert(chunk(ChatToolCallDelta{
			Index: intPtr(0),
			ID:    stringPtr("call_1"),
			Type:  stringPtr("function"),
			Function: ChatToolCallFunction{
				Name:      stringPtr("f"),
				Arguments: `"z":2}`,
			},
		})); err != nil {
			t.Fatalf("same-identity continuation rejected: %v", err)
		}
	})
}

// TestChatStreamRequiresPinnedTerminal proves the [DONE] sentinel is the only
// release of the held terminal (review-08 blocker 2): a stream that ends
// after finish_reason without [DONE] is a typed upstream truncation — the
// terminal batch is never released at EOF, the exchange is never reported as
// a successful completion, and the client receives an error event.
func TestChatStreamRequiresPinnedTerminal(t *testing.T) {
	finishChunk := `{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`
	// A finish chunk that opened no items is a legitimate zero-output
	// completion: the held terminal is empty but must still be pinned to the
	// [DONE] sentinel (review-08 blocker 2).
	emptyFinishChunk := `{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

	t.Run("state finalize without done", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"gpt-4.1",
			1710000000,
			nil,
		)
		if _, err := state.Convert(ChatStreamResponse{
			ID:      "c",
			Object:  "chat.completion.chunk",
			Created: 1710000000,
			Model:   "gpt-4.1",
			Choices: []ChatChoice{{
				Index:        0,
				Delta:        &ChatStreamDelta{Content: stringPtr("hi")},
				FinishReason: stringPtr("stop"),
			}},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := state.FinalizeEOF()
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
		if !strings.Contains(err.Error(), "[DONE]") {
			t.Fatalf("error = %q, want the [DONE] violation", err.Error())
		}
	})

	t.Run("reader truncation classification", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"gpt-4.1",
			1710000000,
			nil,
		)
		converter := newChatToResponsesConverter(state)
		reader := newConvertingReader(
			NewSSEReaderWithLimits(strings.NewReader(
				"data: "+finishChunk+"\n\n",
			), 0, 0),
			converter,
		)
		output, readErr := drainReader(t, reader)
		if readErr == nil || errors.Is(readErr, io.EOF) {
			t.Fatalf("read err = %v, want the pinned-terminal truncation error", readErr)
		}
		if !errors.Is(readErr, errStreamTruncated) {
			t.Fatalf("read err = %v, want an errStreamTruncated wrap", readErr)
		}
		if !strings.Contains(readErr.Error(), "[DONE]") {
			t.Fatalf("read err = %q, want the [DONE] violation", readErr.Error())
		}
		if reader.SawTerminal() {
			t.Fatal("EOF after finish without [DONE] must not report a success terminal")
		}
		if !reader.SawErrorEvent() {
			t.Fatal("truncation must emit a client error event")
		}
		if !strings.Contains(output, "event: error") {
			t.Fatalf("client error event missing: %q", output)
		}
		if strings.Contains(output, "response.completed") {
			t.Fatalf("EOF without [DONE] must not emit a success terminal: %q", output)
		}
		if got := classifyStreamObservation(streamObservation{
			ReaderErr:         readErr,
			SawErrorEvent:     reader.SawErrorEvent(),
			SawTerminal:       reader.SawTerminal(),
			UpstreamBodyError: reader.UpstreamBodyError(),
		}); got != streamOutcomeUpstreamFailure {
			t.Fatalf("classification = %v, want upstream failure", got)
		}
	})

	t.Run("composed converter finalize without done", func(t *testing.T) {
		chat := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"gpt-4.1",
			1710000000,
			nil,
		)
		anthropic := newAnthropicResponsesStreamState(
			testStreamContext(),
			j6PermissivePolicy(),
			"msg_1",
			"gpt-4.1",
			1710000000,
		)
		converter := newChatToAnthropicConverter(chat, anthropic)
		if _, err := converter.Convert(SSEEvent{Data: []byte(finishChunk)}); err != nil {
			t.Fatal(err)
		}
		_, err := converter.FinalizeEOF()
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
		if !strings.Contains(err.Error(), "[DONE]") {
			t.Fatalf("error = %q, want the [DONE] violation", err.Error())
		}
	})

	t.Run("composed done then finalize", func(t *testing.T) {
		chat := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"gpt-4.1",
			1710000000,
			nil,
		)
		anthropic := newAnthropicResponsesStreamState(
			testStreamContext(),
			j6PermissivePolicy(),
			"msg_1",
			"gpt-4.1",
			1710000000,
		)
		converter := newChatToAnthropicConverter(chat, anthropic)
		if _, err := converter.Convert(SSEEvent{Data: []byte(finishChunk)}); err != nil {
			t.Fatal(err)
		}
		batch, err := converter.Convert(SSEEvent{Data: []byte("[DONE]")})
		if err != nil {
			t.Fatal(err)
		}
		if !batch.Terminal {
			t.Fatal("[DONE] batch is not terminal")
		}
		// The subsequent EOF finalize is a clean no-op terminal.
		final, err := converter.FinalizeEOF()
		if err != nil {
			t.Fatalf("finalize after [DONE] = %v, want clean", err)
		}
		if !final.Terminal {
			t.Fatal("finalize after [DONE] must still report terminal")
		}
	})

	t.Run("zero-output finish without done", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"gpt-4.1",
			1710000000,
			nil,
		)
		converter := newChatToResponsesConverter(state)
		reader := newConvertingReader(
			NewSSEReaderWithLimits(strings.NewReader(
				"data: "+emptyFinishChunk+"\n\n",
			), 0, 0),
			converter,
		)
		output, readErr := drainReader(t, reader)
		if readErr == nil || errors.Is(readErr, io.EOF) {
			t.Fatalf("read err = %v, want the pinned-terminal truncation error", readErr)
		}
		if !errors.Is(readErr, errStreamTruncated) {
			t.Fatalf("read err = %v, want an errStreamTruncated wrap", readErr)
		}
		if reader.SawTerminal() {
			t.Fatal("zero-output EOF without [DONE] must not report a success terminal")
		}
		if !reader.SawErrorEvent() {
			t.Fatal("zero-output truncation must emit a client error event")
		}
		if strings.Contains(output, "response.completed") {
			t.Fatalf("zero-output EOF without [DONE] must not emit a success terminal: %q", output)
		}
	})

	t.Run("zero-output finish with done", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"gpt-4.1",
			1710000000,
			nil,
		)
		converter := newChatToResponsesConverter(state)
		reader := newConvertingReader(
			NewSSEReaderWithLimits(strings.NewReader(
				"data: "+emptyFinishChunk+"\n\n"+"data: [DONE]\n\n",
			), 0, 0),
			converter,
		)
		output, readErr := drainReader(t, reader)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			t.Fatalf("read err = %v", readErr)
		}
		if !reader.SawTerminal() {
			t.Fatal("[DONE] must release the zero-output held terminal")
		}
		if !strings.Contains(output, "response.completed") {
			t.Fatalf("missing success terminal: %q", output)
		}
	})

	t.Run("composed zero-output finish without done", func(t *testing.T) {
		chat := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"gpt-4.1",
			1710000000,
			nil,
		)
		anthropic := newAnthropicResponsesStreamState(
			testStreamContext(),
			j6PermissivePolicy(),
			"msg_1",
			"gpt-4.1",
			1710000000,
		)
		converter := newChatToAnthropicConverter(chat, anthropic)
		if _, err := converter.Convert(SSEEvent{Data: []byte(emptyFinishChunk)}); err != nil {
			t.Fatal(err)
		}
		_, err := converter.FinalizeEOF()
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
		if !strings.Contains(err.Error(), "[DONE]") {
			t.Fatalf("error = %q, want the [DONE] violation", err.Error())
		}
	})

	t.Run("composed zero-output finish with done", func(t *testing.T) {
		chat := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"gpt-4.1",
			1710000000,
			nil,
		)
		anthropic := newAnthropicResponsesStreamState(
			testStreamContext(),
			j6PermissivePolicy(),
			"msg_1",
			"gpt-4.1",
			1710000000,
		)
		converter := newChatToAnthropicConverter(chat, anthropic)
		if _, err := converter.Convert(SSEEvent{Data: []byte(emptyFinishChunk)}); err != nil {
			t.Fatal(err)
		}
		batch, err := converter.Convert(SSEEvent{Data: []byte("[DONE]")})
		if err != nil {
			t.Fatalf("[DONE] after zero-output finish = %v, want clean release", err)
		}
		if !batch.Terminal {
			t.Fatal("[DONE] batch is not terminal")
		}
	})

	t.Run("done releases cleanly", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"gpt-4.1",
			1710000000,
			nil,
		)
		converter := newChatToResponsesConverter(state)
		reader := newConvertingReader(
			NewSSEReaderWithLimits(strings.NewReader(
				"data: "+finishChunk+"\n\n"+"data: [DONE]\n\n",
			), 0, 0),
			converter,
		)
		output, readErr := drainReader(t, reader)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			t.Fatalf("read err = %v", readErr)
		}
		if !reader.SawTerminal() {
			t.Fatal("[DONE] must release the held terminal")
		}
		if !strings.Contains(output, "response.completed") {
			t.Fatalf("missing success terminal: %q", output)
		}
		// A subsequent EOF finalize after the [DONE] release is a clean
		// no-op: the terminal was already emitted.
		if _, err := converter.FinalizeEOF(); err != nil {
			t.Fatalf("finalize after [DONE] = %v, want clean", err)
		}
		if _, err := state.FinalizeEOF(); err != nil {
			t.Fatalf("state finalize after [DONE] = %v, want clean", err)
		}
		// A second release attempt reports no release (idempotence).
		if _, ok := state.releaseTerminal(); ok {
			t.Fatal("releaseTerminal after [DONE] must not release again")
		}
	})
}

// TestChatStreamReviewMalformedStreamIsUpstreamFailure proves the review's
// minimal malformed stream — a user-role delta chunk with no envelope fields,
// terminated by [DONE] — is rejected with a client-dialect error event and
// classified as an upstream failure: it can never become a successful
// assistant response (review-08 blocker 2 acceptance).
func TestChatStreamReviewMalformedStreamIsUpstreamFailure(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	reader := newConvertingReader(
		NewSSEReaderWithLimits(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"role\":\"user\",\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\n"+
				"data: [DONE]\n\n",
		), 0, 0),
		converter,
	)
	output, readErr := drainReader(t, reader)
	assertChatStreamChunkWireError(t, readErr)
	if !reader.SawUpstreamErrorFrame() {
		t.Fatal("malformed chunk must be marked as an upstream error frame")
	}
	if reader.SawTerminal() {
		t.Fatal("malformed stream must not report a success terminal")
	}
	if !strings.Contains(output, "event: error") {
		t.Fatalf("client error event missing: %q", output)
	}
	if strings.Contains(output, "response.completed") {
		t.Fatalf("malformed stream must not emit a success terminal: %q", output)
	}
	if got := classifyStreamObservation(streamObservation{
		ReaderErr:             readErr,
		SawErrorEvent:         reader.SawErrorEvent(),
		SawTerminal:           reader.SawTerminal(),
		SawUpstreamErrorFrame: reader.SawUpstreamErrorFrame(),
		UpstreamBodyError:     reader.UpstreamBodyError(),
	}); got != streamOutcomeUpstreamFailure {
		t.Fatalf("classification = %v, want upstream failure", got)
	}
}
