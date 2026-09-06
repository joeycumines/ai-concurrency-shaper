package transcode

import (
	"strings"
	"testing"
)

// TestChatStreamMaximalPartCompletesRelease pins autopsy 2026-09-06 M1: a
// chat stream accumulating exactly maxStreamAccumulatedBytes (1 MiB) of text
// in one content part was accepted by the accumulator, then the [DONE]
// terminal envelope (which repeats the full text inside the Responses
// envelope JSON) failed the EQUAL generated-frame bound — a completed
// upstream conversation errored at its terminal. The generated-frame bound
// (6*maxStreamAccumulatedBytes + 1 MiB headroom) must strictly accommodate
// the worst case, so the maximal part completes its release.
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
	for i := 0; i < 2; i++ {
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
		t.Fatalf("maximal part terminal release failed (autopsy M1 collision): %v", err)
	}
	if !terminalBatch.Terminal {
		t.Fatal("[DONE] batch is not terminal")
	}
}
