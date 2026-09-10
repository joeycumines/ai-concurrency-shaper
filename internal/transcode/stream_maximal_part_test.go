package transcode

import (
	"errors"
	"strings"
	"testing"
)

// TestChatStreamMaximalPartCompletesRelease pins the maximal-part release: a
// chat stream accumulating exactly maxStreamAccumulatedBytes (1 MiB) of text
// in one content part was accepted by the accumulator, then the [DONE]
// terminal envelope (which repeats the full text inside the Responses
// envelope JSON) failed the EQUAL generated-frame bound — a completed
// upstream conversation errored at its terminal. The release bounds now
// derive from the exchange total and the echo bound (limits.go), strictly
// above the worst-case accepted release, so the maximal part completes its
// release.
func TestChatStreamMaximalPartCompletesRelease(t *testing.T) {
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

	// Two legal 512 KiB text deltas: the part accumulates to exactly the
	// accepted maximum.
	half := strings.Repeat("a", maxStreamAccumulatedBytes/2)
	for i := range 2 {
		if _, err := converter.Convert(SSEEvent{Data: []byte(
			`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"` + half + `"},"finish_reason":null}]}`,
		)}); err != nil {
			t.Fatalf("delta %d rejected: %v", i+1, err)
		}
	}

	// Finish, then [DONE]: the terminal release must succeed. Pre-fix this
	// failed with SSEBoundError at the equal 1 MiB frame bound.
	if _, err := converter.Convert(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)}); err != nil {
		t.Fatal(err)
	}
	terminalBatch, err := converter.Convert(SSEEvent{Data: []byte("[DONE]")})
	if err != nil {
		t.Fatalf("maximal part terminal release failed (collision): %v", err)
	}
	if !terminalBatch.Terminal {
		t.Fatal("[DONE] batch is not terminal")
	}
}

// TestChatStreamMaximalExchangeCompletesRelease pins the round-2/3 M1
// derivation: the terminal envelope aggregates EVERY accepted accumulator
// (output items, tool arguments) plus the request echo, so the
// generated-frame/batch/generated-total bounds derive from the EXCHANGE
// total (maxStreamTotalAccumulatedBytes), not one accumulator — and the
// payloads use '<' so the 6x JSON-escaping worst case is pinned, not just
// 1x. Two tool calls each accumulating exactly the per-item maximum of
// escaping-heavy text must complete their [DONE] release.
func TestChatStreamMaximalExchangeCompletesRelease(t *testing.T) {
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

	maxArgs := strings.Repeat("<", maxStreamAccumulatedBytes)
	// Tool call A: identity, then arguments accumulating to exactly the
	// per-item maximum across two deltas.
	a := `{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_A","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`
	if _, err := converter.Convert(SSEEvent{Data: []byte(a)}); err != nil {
		t.Fatal(err)
	}
	half := maxStreamAccumulatedBytes / 2
	for i := range 2 {
		if _, err := converter.Convert(SSEEvent{Data: []byte(
			`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"` + maxArgs[:half] + `"}}]},"finish_reason":null}]}`,
		)}); err != nil {
			t.Fatalf("call A delta %d rejected: %v", i+1, err)
		}
	}
	// Tool call B: same, on index 1.
	b := `{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_B","type":"function","function":{"name":"g","arguments":""}}]},"finish_reason":null}]}`
	if _, err := converter.Convert(SSEEvent{Data: []byte(b)}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := converter.Convert(SSEEvent{Data: []byte(
			`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"` + maxArgs[:half] + `"}}]},"finish_reason":null}]}`,
		)}); err != nil {
			t.Fatalf("call B delta %d rejected: %v", i+1, err)
		}
	}
	// Finish, then [DONE]: the terminal envelope carries BOTH completed
	// function calls (2 MiB total semantics, 12 MiB escaped at 6x) and must
	// release. Every release bound (frame, batch, generated total) is
	// derived above this worst case.
	if _, err := converter.Convert(SSEEvent{Data: []byte(
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)}); err != nil {
		t.Fatal(err)
	}
	terminalBatch, err := converter.Convert(SSEEvent{Data: []byte("[DONE]")})
	if err != nil {
		t.Fatalf("maximal two-accumulator exchange release failed: %v", err)
	}
	if !terminalBatch.Terminal {
		t.Fatal("[DONE] batch is not terminal")
	}
}

// TestGeneratedFrameBoundEnforced proves the generated-frame bound is
// load-bearing at the converting reader (mutation target: disabling the
// reader frame check must fail this test). A frame one byte over the
// configured generated-frame bound is a typed SSEBoundError.
func TestGeneratedFrameBoundEnforced(t *testing.T) {
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(""), 0, 0),
		&fixedConverter{},
		1024, 0, 0,
	)
	err := reader.appendBatch(convertedBatch{Events: []frameEvent{{
		Type: "x",
		Data: make([]byte, 1025),
	}}})
	var boundErr *SSEBoundError
	if !errors.As(err, &boundErr) {
		t.Fatalf("err = %T %v, want *SSEBoundError", err, err)
	}
	if boundErr.Line {
		t.Fatal("bound error must be a frame violation, not a line violation")
	}
}
