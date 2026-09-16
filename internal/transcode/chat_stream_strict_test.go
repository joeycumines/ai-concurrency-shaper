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
// wrong choice index, more than one choice, a missing delta, a non-streaming
// message arm (a STRUCTURAL rejection — the streaming surface carries only
// deltas), and a usage object omitting any required total are all corrupt
// upstream wire. Contract-role note (2026-09-06): an
// UNKNOWN field on the upstream stream envelope is a provider extension and is
// TOLERATED (never a failure), but the message arm is a KNOWN non-streaming
// field and is a structural rejection — it must never be silently dropped.
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
			name: "message arm without delta",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"y"},"finish_reason":null}]}`,
		},
		{
			name: "empty delta with message content",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"message":{"role":"assistant","content":"full text"},"finish_reason":"stop"}]}`,
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
// output. The minimal malformed chunk, which
// also omits every envelope field, is rejected for the envelope first.
func TestChatStreamRejectsNonAssistantRole(t *testing.T) {
	// The exact counterexample chunk: a user-role delta and no
	// envelope fields at all.
	chunk := `{"choices":[{"delta":{"role":"user","content":"x"},"finish_reason":"stop"}]}`
	_, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(chunk)})
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
				_, err := chatStreamChunkFromSSE(SSEEvent{Data: fmt.Appendf(nil, base, tt.call)})
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
			Index: new(0),
			ID:    new("call_1"),
			Type:  new("function"),
			Function: ChatToolCallFunction{
				Name:      new("f"),
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
					Index:    new(0),
					ID:       new("call_zz"),
					Function: ChatToolCallFunction{Name: new("f")},
				},
			},
			{
				name: "name changes after added",
				call: ChatToolCallDelta{Index: new(0), Function: ChatToolCallFunction{Name: new("g")}},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := state.Convert(chunk(tt.call))
				if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
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
			Index: new(1),
			ID:    new("call_2"),
			Type:  new("function"),
			Function: ChatToolCallFunction{
				Name:      new("g"),
				Arguments: "{}",
			},
		})); err != nil {
			t.Fatal(err)
		}
		// The fragment id resolves to call_1 while its index resolves to
		// call_2: the conflicting index must never be silently ignored.
		_, err := state.Convert(chunk(ChatToolCallDelta{
			Index: new(1),
			ID:    new("call_1"),
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
			Function: ChatToolCallFunction{Name: new("h")},
		}))
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
		}
		// Same identity fragments remain accepted (argument continuation).
		if _, err := state.Convert(chunk(ChatToolCallDelta{
			Index: new(0),
			ID:    new("call_1"),
			Type:  new("function"),
			Function: ChatToolCallFunction{
				Name:      new("f"),
				Arguments: `"z":2}`,
			},
		})); err != nil {
			t.Fatalf("same-identity continuation rejected: %v", err)
		}
	})
}

// TestChatStreamTerminalRelease proves the release rules of the held chat
// terminal: the [DONE] sentinel releases it, and an upstream that ends after
// a finishing chunk without the sentinel is released on EOF — every semantic
// terminal already arrived, so refusing would misreport a real completion —
// with the quirk recorded as an ungated note. A stream that ends WITHOUT any
// finish_reason is still a typed upstream truncation: the client receives an
// error event and the exchange is never reported as a successful completion.
func TestChatStreamTerminalRelease(t *testing.T) {
	finishChunk := `{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`
	// A finish chunk that opened no items is a legitimate zero-output
	// completion: the held terminal is empty but must still be pinned to the
	// [DONE] sentinel.
	emptyFinishChunk := `{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

	t.Run("state finalize without done releases and notes", func(t *testing.T) {
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
				Delta:        &ChatStreamDelta{Content: new("hi")},
				FinishReason: new("stop"),
			}},
		}); err != nil {
			t.Fatal(err)
		}
		events, err := state.FinalizeEOF()
		if err != nil {
			t.Fatalf("EOF after finish = %v, want a clean release", err)
		}
		released := false
		for _, event := range events {
			if event.EventType() == "response.completed" {
				released = true
			}
		}
		if !released {
			t.Fatalf("released events lack response.completed: %+v", events)
		}
		if !reportHasFeature(state.report, FeatureMissingStreamSentinel) {
			t.Fatal("the missing-sentinel note is not recorded")
		}
	})

	t.Run("reader releases on EOF after finish without done", func(t *testing.T) {
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
		reader := newConvertingReaderWithLimits(
			NewSSEReaderWithLimits(strings.NewReader(
				"data: "+finishChunk+"\n\n"), 0, 0),
			converter, 0, 0, 0,
		)
		output, readErr := drainReader(t, reader)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			t.Fatalf("read err = %v, want a clean EOF", readErr)
		}
		if !reader.SawTerminal() {
			t.Fatal("EOF after finish must release the held terminal")
		}
		if reader.SawErrorEvent() {
			t.Fatal("the release is a completed exchange, not an error")
		}
		if !strings.Contains(output, "response.completed") {
			t.Fatalf("missing success terminal: %q", output)
		}
		if !reportHasFeature(state.report, FeatureMissingStreamSentinel) {
			t.Fatal("the missing-sentinel note is not recorded")
		}
		if got := classifyStreamObservation(streamObservation{
			ReaderErr:         readErr,
			SawErrorEvent:     reader.SawErrorEvent(),
			SawTerminal:       reader.SawTerminal(),
			UpstreamBodyError: reader.UpstreamBodyError(),
		}); got != streamOutcomeSuccess {
			t.Fatalf("classification = %v, want success", got)
		}
	})

	t.Run("reader truncates before any finish", func(t *testing.T) {
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
		reader := newConvertingReaderWithLimits(
			NewSSEReaderWithLimits(strings.NewReader(
				`data: {"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n"), 0, 0),
			converter, 0, 0, 0,
		)
		output, readErr := drainReader(t, reader)
		if readErr == nil || errors.Is(readErr, io.EOF) {
			t.Fatalf("read err = %v, want a truncation error", readErr)
		}
		if !errors.Is(readErr, errStreamTruncated) {
			t.Fatalf("read err = %v, want an errStreamTruncated wrap", readErr)
		}
		if !strings.Contains(readErr.Error(), "before a terminal condition") {
			t.Fatalf("read err = %q, want the before-terminal violation", readErr.Error())
		}
		if reader.SawTerminal() {
			t.Fatal("a stream without any finish_reason must not report success")
		}
		if !reader.SawErrorEvent() {
			t.Fatalf("truncation must emit a client error event: %q", output)
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
			ChatCapabilities{},
			"msg_1",
			"gpt-4.1",
			1710000000,
		)
		converter := newChatToAnthropicConverter(chat, anthropic)
		if _, err := converter.Convert(SSEEvent{Data: []byte(finishChunk)}); err != nil {
			t.Fatal(err)
		}
		batch, err := converter.FinalizeEOF()
		if err != nil {
			t.Fatalf("EOF after finish = %v, want a clean release", err)
		}
		if !batch.Terminal {
			t.Fatal("the released batch is not terminal")
		}
		stopped := false
		for _, frame := range batch.Events {
			if frame.Type == "message_stop" {
				stopped = true
			}
		}
		if !stopped {
			t.Fatalf("released frames lack message_stop: %+v", batch.Events)
		}
		if !reportHasFeature(chat.report, FeatureMissingStreamSentinel) {
			t.Fatal("the missing-sentinel note is not recorded")
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
			ChatCapabilities{},
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
		reader := newConvertingReaderWithLimits(
			NewSSEReaderWithLimits(strings.NewReader(
				"data: "+emptyFinishChunk+"\n\n"), 0, 0),
			converter, 0, 0, 0,
		)
		output, readErr := drainReader(t, reader)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			t.Fatalf("read err = %v, want a clean EOF", readErr)
		}
		if !reader.SawTerminal() {
			t.Fatal("EOF after a zero-output finish must release the held terminal")
		}
		if reader.SawErrorEvent() {
			t.Fatal("the zero-output release is a completed exchange, not an error")
		}
		if !strings.Contains(output, "response.completed") {
			t.Fatalf("missing success terminal: %q", output)
		}
		if !reportHasFeature(state.report, FeatureMissingStreamSentinel) {
			t.Fatal("the missing-sentinel note is not recorded")
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
		reader := newConvertingReaderWithLimits(
			NewSSEReaderWithLimits(strings.NewReader(
				"data: "+emptyFinishChunk+"\n\n"+"data: [DONE]\n\n"), 0, 0),
			converter, 0, 0, 0,
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
			ChatCapabilities{},
			"msg_1",
			"gpt-4.1",
			1710000000,
		)
		converter := newChatToAnthropicConverter(chat, anthropic)
		if _, err := converter.Convert(SSEEvent{Data: []byte(emptyFinishChunk)}); err != nil {
			t.Fatal(err)
		}
		batch, err := converter.FinalizeEOF()
		if err != nil {
			t.Fatalf("EOF after zero-output finish = %v, want a clean release", err)
		}
		if !batch.Terminal {
			t.Fatal("the released zero-output batch is not terminal")
		}
		stopped := false
		for _, frame := range batch.Events {
			if frame.Type == "message_stop" {
				stopped = true
			}
		}
		if !stopped {
			t.Fatalf("released frames lack message_stop: %+v", batch.Events)
		}
		if !reportHasFeature(chat.report, FeatureMissingStreamSentinel) {
			t.Fatal("the missing-sentinel note is not recorded")
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
			ChatCapabilities{},
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
		reader := newConvertingReaderWithLimits(
			NewSSEReaderWithLimits(strings.NewReader(
				"data: "+finishChunk+"\n\n"+"data: [DONE]\n\n"), 0, 0),
			converter, 0, 0, 0,
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

// TestChatStreamReleasesCapturedMissingSentinelTail replays, verbatim, an
// upstream chat stream captured live from a gateway that closed the
// connection after the usage-only tail chunk without ever sending the [DONE]
// sentinel. Every semantic terminal (finish_reason plus the usage
// accounting) had already arrived, so the exchange must release the held
// terminal at EOF: a clean message_stop with the usage applied, no error
// event, and the missing_stream_sentinel note recorded on the conversion
// report.
func TestChatStreamReleasesCapturedMissingSentinelTail(t *testing.T) {
	const captured = `data: {"choices":[{"delta":{"content":null,"reasoning_content":"We","role":"assistant"},"finish_reason":null,"index":0,"logprobs":null}],"created":1789526350,"id":"chatcmpl-f02e903cd786813e728847a3b9770bd6","model":"deepseek-v4-flash-0731","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":null,"reasoning_content":" need answer exactly"},"finish_reason":null,"index":0,"logprobs":null}],"created":1789526350,"id":"chatcmpl-f02e903cd786813e728847a3b9770bd6","model":"deepseek-v4-flash-0731","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":null,"reasoning_content":" STREAM_OK."},"finish_reason":null,"index":0,"logprobs":null}],"created":1789526350,"id":"chatcmpl-f02e903cd786813e728847a3b9770bd6","model":"deepseek-v4-flash-0731","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"reasoning_content":null},"finish_reason":null,"index":0,"logprobs":null}],"created":1789526350,"id":"chatcmpl-f02e903cd786813e728847a3b9770bd6","model":"deepseek-v4-flash-0731","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","reasoning_content":null},"finish_reason":"stop","index":0,"logprobs":null}],"created":1789526350,"id":"chatcmpl-f02e903cd786813e728847a3b9770bd6","model":"deepseek-v4-flash-0731","object":"chat.completion.chunk","usage":null}

data: {"choices":[],"created":1789526350,"id":"chatcmpl-f02e903cd786813e728847a3b9770bd6","model":"deepseek-v4-flash-0731","object":"chat.completion.chunk","usage":{"completion_tokens":12,"completion_tokens_details":{"reasoning_tokens":8},"prompt_tokens":89,"prompt_tokens_details":{"cached_tokens":0},"total_tokens":101}}


`
	chat := newChatResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"resp_1",
		"deepseek-v4-flash-0731",
		1789526350,
		nil,
	)
	anthropic := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"msg_1",
		"deepseek-v4-flash-0731",
		1789526350,
	)
	converter := newChatToAnthropicConverter(chat, anthropic)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(captured), 0, 0),
		converter, 0, 0, 0,
	)
	output, readErr := drainReader(t, reader)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		t.Fatalf("read err = %v, want a clean EOF", readErr)
	}
	if !reader.SawTerminal() {
		t.Fatal("the captured EOF after finish must release the held terminal")
	}
	if reader.SawErrorEvent() {
		t.Fatal("the release is a completed exchange, not an error")
	}
	if !strings.Contains(output, "event: message_stop") {
		t.Fatalf("missing message_stop: %q", output)
	}
	if strings.Contains(output, "event: error") {
		t.Fatalf("released stream carries an error event: %q", output)
	}
	if !strings.Contains(output, `"output_tokens":12`) {
		t.Fatalf("the usage tail was not applied: %q", output)
	}
	if !reportHasFeature(chat.report, FeatureMissingStreamSentinel) {
		t.Fatal("the missing-sentinel note is not recorded")
	}
}

// TestChatStreamReleasesCapturedClaudeCodeMissingSentinelTail replays,
// verbatim, the upstream stream captured from a real Claude Code exchange
// that failed pre-fix: role and reasoning-only deltas, a finishing chunk
// with empty content, then the usage-only tail, and EOF with no [DONE].
// The release must emit exactly one message_stop with no error event, apply
// the usage accounting, and record the missing_stream_sentinel note.
func TestChatStreamReleasesCapturedClaudeCodeMissingSentinelTail(t *testing.T) {
	const captured = `data: {"choices":[{"delta":{"reasoning_content":null,"role":"assistant"},"finish_reason":null,"index":0,"logprobs":null}],"created":1789525525,"id":"chatcmpl-6f1f4d72635a635009d16cf2f2f44990","model":"deepseek-v4-flash-0731","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"reasoning_content":null},"finish_reason":null,"index":0,"logprobs":null}],"created":1789525525,"id":"chatcmpl-6f1f4d72635a635009d16cf2f2f44990","model":"deepseek-v4-flash-0731","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"reasoning_content":null},"finish_reason":null,"index":0,"logprobs":null}],"created":1789525525,"id":"chatcmpl-6f1f4d72635a635009d16cf2f2f44990","model":"deepseek-v4-flash-0731","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"reasoning_content":null},"finish_reason":null,"index":0,"logprobs":null}],"created":1789525525,"id":"chatcmpl-6f1f4d72635a635009d16cf2f2f44990","model":"deepseek-v4-flash-0731","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","reasoning_content":null},"finish_reason":"stop","index":0,"logprobs":null}],"created":1789525525,"id":"chatcmpl-6f1f4d72635a635009d16cf2f2f44990","model":"deepseek-v4-flash-0731","object":"chat.completion.chunk","usage":null}

data: {"choices":[],"created":1789525525,"id":"chatcmpl-6f1f4d72635a635009d16cf2f2f44990","model":"deepseek-v4-flash-0731","object":"chat.completion.chunk","usage":{"completion_tokens":8,"completion_tokens_details":{"reasoning_tokens":0},"prompt_tokens":35411,"prompt_tokens_details":{"cached_tokens":0},"total_tokens":35419}}

`
	chat := newChatResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"resp_1",
		"deepseek-v4-flash-0731",
		1789525525,
		nil,
	)
	anthropic := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"msg_1",
		"deepseek-v4-flash-0731",
		1789525525,
	)
	converter := newChatToAnthropicConverter(chat, anthropic)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(captured), 0, 0),
		converter, 0, 0, 0,
	)
	output, readErr := drainReader(t, reader)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		t.Fatalf("read err = %v, want a clean EOF", readErr)
	}
	if !reader.SawTerminal() {
		t.Fatal("the captured EOF after finish must release the held terminal")
	}
	if reader.SawErrorEvent() {
		t.Fatal("the release is a completed exchange, not an error")
	}
	if got := strings.Count(output, "event: message_stop"); got != 1 {
		t.Fatalf("message_stop count = %d, want exactly one: %q", got, output)
	}
	if strings.Contains(output, "event: error") {
		t.Fatalf("released stream carries an error event: %q", output)
	}
	if !strings.Contains(output, `"output_tokens":8`) {
		t.Fatalf("the usage tail was not applied: %q", output)
	}
	if !reportHasFeature(chat.report, FeatureMissingStreamSentinel) {
		t.Fatal("the missing-sentinel note is not recorded")
	}
	if got := classifyStreamObservation(streamObservation{
		ReaderErr:         readErr,
		SawErrorEvent:     reader.SawErrorEvent(),
		SawTerminal:       reader.SawTerminal(),
		UpstreamBodyError: reader.UpstreamBodyError(),
	}); got != streamOutcomeSuccess {
		t.Fatalf("classification = %v, want success", got)
	}
}

// TestChatStreamReviewMalformedStreamIsUpstreamFailure proves the
// minimal malformed stream — a user-role delta chunk with no envelope fields,
// terminated by [DONE] — is rejected with a client-dialect error event and
// classified as an upstream failure: it can never become a successful
// assistant response.
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
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"role\":\"user\", \"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\n"+
				"data: [DONE]\n\n"), 0, 0),
		converter, 0, 0, 0,
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

// TestChatStreamDecodesMatchedStopExtension proves the choice-level
// `matched_stop` provider extension (observed in the field 2026-08-24 on the
// yolo/qwen chat gateway: the opaque spelling of the stop signal that
// finish_reason already carries) decodes on the strict streaming surface in
// both its string and null forms — a current provider must never fail the
// strict chunk decode — while a genuinely unknown field is now TOLERATED.
func TestChatStreamDecodesMatchedStopExtension(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "string",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null,"matched_stop":"<|im_end|>"}]}`,
		},
		{
			name: "null",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null,"matched_stop":null}]}`,
		},
		{
			name: "finish chunk",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop","matched_stop":"<|im_end|>"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(tt.body)})
			if err != nil {
				t.Fatalf("matched_stop chunk rejected: %v", err)
			}
			if len(chunk.Choices) != 1 {
				t.Fatalf("chunk = %+v", chunk)
			}
		})
	}

	// Contract-role note (2026-09-06): a genuinely unknown choice-level field
	// on the upstream stream envelope is a provider extension and is now
	// TOLERATED, not a failure.
	bogus := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null,"bogus_field":1}]}`
	if _, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(bogus)}); err != nil {
		t.Fatalf("unknown choice field tolerated decode = %v, want success", err)
	}
}

// TestChatStreamMessageArmRejected proves the non-streaming message arm is a
// STRUCTURAL rejection on the streaming surface (the streaming surface carries
// only deltas), so its content can never be silently dropped. An empty
// delta-with-message-content chunk is rejected, not silently reduced to the
// empty delta. A chunk with a valid delta and NO message arm still decodes.
func TestChatStreamMessageArmRejected(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{
			name: "message arm with content and delta",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"message":{"role":"assistant","content":"y"},"finish_reason":null}]}`,
		},
		{
			name: "empty delta with message content",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"message":{"role":"assistant","content":"full text"},"finish_reason":"stop"}]}`,
		},
		{
			name: "message arm without delta",
			body: `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"y"},"finish_reason":null}]}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(tt.body)})
			assertChatStreamChunkWireError(t, err)
		})
	}

	// A chunk with a valid delta and no message arm still decodes.
	if _, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(chatStreamChunkBase)}); err != nil {
		t.Fatalf("official shape rejected: %v", err)
	}
}
