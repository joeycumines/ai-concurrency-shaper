package transcode

// Autopsy 2026-09-06 M1 round 4: the release-bound derivation is closed on
// every axis the round-3 gate falsified — the per-event framing overhead
// times the event budget (F1), the 6x-escaped request echo (F2, bounded at
// decode), and tool-call identity rendered into the terminal envelope
// (charged against the exchange accumulated total).

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// sseReplayReader replays a fixed sequence of raw SSE frames without
// materializing the whole stream: each frame is served from the same backing
// slice, so a million-delta source costs O(frames) pointers, not O(bytes).
type sseReplayReader struct {
	frames   [][]byte
	frameIdx int
	offset   int
}

func (r *sseReplayReader) Read(p []byte) (int, error) {
	for r.frameIdx < len(r.frames) {
		frame := r.frames[r.frameIdx]
		if r.offset < len(frame) {
			n := copy(p, frame[r.offset:])
			r.offset += n
			return n, nil
		}
		r.frameIdx++
		r.offset = 0
	}
	return 0, io.EOF
}

// TestChatStreamFramingOverheadCompletesRelease pins M1 round 3 finding F1:
// a legal stream of single-byte deltas is bounded by the EVENT budget, and
// its generated total is dominated by per-event framing overhead (~221 bytes
// per chat→responses text-delta frame, measured), not payload escaping. The
// pre-round-4 generated total (terminal batch + 6x semantics + 1 MiB) died
// at ~69% of the event budget with SSEBoundError; the derivation now carries
// maxStreamTotalEvents x maxStreamPerEventFramingBytes, so an exchange at
// the full event budget completes its [DONE] release.
func TestChatStreamFramingOverheadCompletesRelease(t *testing.T) {
	const deltas = int(maxStreamTotalEvents) - 2
	deltaFrame := []byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1710000000,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
	finishFrame := []byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1710000000,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	doneFrame := []byte("data: [DONE]\n\n")

	frames := make([][]byte, 0, deltas+2)
	for range deltas {
		frames = append(frames, deltaFrame)
	}
	frames = append(frames, finishFrame, doneFrame)

	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1710000000,
		nil,
	)
	source := NewSSEReaderWithLimits(&sseReplayReader{frames: frames}, 0, 0)
	reader := newConvertingReaderWithLimits(source, newChatToResponsesConverter(state), 0, 0, 0)

	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("full-event-budget exchange must complete its release (M1 round 3 F1): %v", err)
	}
	if !reader.sawTerminal {
		t.Fatal("exchange did not reach its terminal")
	}
	if reader.sawErrorEvent {
		t.Fatal("exchange emitted an error event")
	}
}

// TestResponsesRequestEchoCapped pins M1 round 3 finding F2: the request
// echo is bounded at decode (maxStreamEchoBytes, fail-closed 413 resource
// limit) because it re-marshals into every generated envelope frame at up to
// 6x JSON escaping. A '<'-heavy echo above the bound is rejected at decode —
// accepted by the pre-round-4 code (raw body well under AcceptedRequestBytes),
// which then failed the first upstream frame.
func TestResponsesRequestEchoCapped(t *testing.T) {
	t.Run("escaping-heavy instructions above the bound rejected", func(t *testing.T) {
		body := fmt.Sprintf(
			`{"model":"m","input":"x","instructions":%q}`,
			strings.Repeat("<", maxStreamEchoBytes+1),
		)
		_, _, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy())
		if !errors.Is(err, errEchoTooLarge) {
			t.Fatalf("err = %T %v, want errEchoTooLarge", err, err)
		}
	})
	t.Run("escaping-heavy metadata above the bound rejected", func(t *testing.T) {
		body := fmt.Sprintf(
			`{"model":"m","input":"x","metadata":{"k":%q}}`,
			strings.Repeat("<", maxStreamEchoBytes),
		)
		_, _, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy())
		if !errors.Is(err, errEchoTooLarge) {
			t.Fatalf("err = %T %v, want errEchoTooLarge", err, err)
		}
	})
	t.Run("escaping-heavy tools above the bound rejected", func(t *testing.T) {
		body := fmt.Sprintf(
			`{"model":"m","input":"x","tools":[{"type":"function","name":"f","strict":true,"description":%q}]}`,
			strings.Repeat("<", maxStreamEchoBytes),
		)
		_, _, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy())
		if !errors.Is(err, errEchoTooLarge) {
			t.Fatalf("err = %T %v, want errEchoTooLarge", err, err)
		}
	})
	t.Run("echo under the bound accepted", func(t *testing.T) {
		body := fmt.Sprintf(
			`{"model":"m","input":"x","instructions":%q}`,
			strings.Repeat("<", maxStreamEchoBytes-64),
		)
		_, _, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy())
		if err != nil {
			t.Fatalf("echo under the bound must be accepted: %v", err)
		}
	})
	t.Run("ordinary request unaffected", func(t *testing.T) {
		_, _, err := DecodeResponsesRequest(
			[]byte(`{"model":"m","input":"x","instructions":"be concise"}`),
			StrictLossPolicy(),
		)
		if err != nil {
			t.Fatalf("ordinary request rejected: %v", err)
		}
	})
}

// TestChatStreamMaximalEchoReleases pins the release half of the echo bound:
// a maximal accepted echo (exactly maxStreamEchoBytes of '<', rendering at
// 6x = 24 MiB) rides every generated envelope frame — created, in_progress,
// and the terminal envelope — and the release must succeed within the frame
// and batch bounds derived from the echo term. The exchange is driven
// through the converting reader so the marshaled terminal envelope is
// checked against the real frame/batch bounds. The echo carries the pinned
// defaults (tool_choice auto) exactly as DecodeResponsesRequest builds it.
func TestChatStreamMaximalEchoReleases(t *testing.T) {
	auto := "auto"
	echo := &ResponsesRequestEcho{
		Instructions: &ResponsesInput{Text: new(strings.Repeat("<", maxStreamEchoBytes))},
		ToolChoice:   ResponsesToolChoice{Str: &auto},
	}
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1710000000,
		echo,
	)
	finishFrame := []byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1710000000,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	source := NewSSEReaderWithLimits(&sseReplayReader{frames: [][]byte{finishFrame, []byte("data: [DONE]\n\n")}}, 0, 0)
	reader := newConvertingReaderWithLimits(source, newChatToResponsesConverter(state), 0, 0, 0)

	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("maximal-echo terminal release failed (M1 round 3 F2): %v", err)
	}
	if !reader.sawTerminal {
		t.Fatal("exchange did not reach its terminal")
	}
	if reader.sawErrorEvent {
		t.Fatal("exchange emitted an error event")
	}
}

// TestChatStreamToolIdentityCharged pins M1 round 4: tool-call identity
// (call id, function name) renders into the terminal envelope and the done
// events, so it is charged against the exchange accumulated total. Identity
// bytes beyond the exchange total are corrupt upstream wire; a repeated id
// is charged once.
func TestChatStreamToolIdentityCharged(t *testing.T) {
	t.Run("identity beyond the exchange total rejected", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"m",
			1710000000,
			nil,
		)
		bigID := strings.Repeat("i", maxStreamAccumulatedBytes)
		for i := range 4 {
			call := ChatToolCallDelta{ID: new(fmt.Sprintf("%s-%d", bigID[:maxStreamAccumulatedBytes-4], i))}
			if _, err := state.Convert(chatChunk(t, ChatStreamDelta{ToolCalls: []ChatToolCallDelta{call}}, nil)); err != nil {
				t.Fatalf("fragment %d rejected before the exchange total: %v", i+1, err)
			}
		}
		fifth := ChatToolCallDelta{ID: new(bigID)}
		_, err := state.Convert(chatChunk(t, ChatStreamDelta{ToolCalls: []ChatToolCallDelta{fifth}}, nil))
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("err = %T %v, want UpstreamWireError for identity beyond the exchange total", err, err)
		}
	})
	t.Run("repeated identity charged once", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
			"resp_1",
			"m",
			1710000000,
			nil,
		)
		bigID := strings.Repeat("i", maxStreamAccumulatedBytes)
		call := ChatToolCallDelta{ID: new(bigID)}
		for range 4 {
			if _, err := state.Convert(chatChunk(t, ChatStreamDelta{ToolCalls: []ChatToolCallDelta{call}}, nil)); err != nil {
				t.Fatalf("repeated id fragment rejected: %v", err)
			}
		}
		if state.totalAccumulated != int64(len(bigID)) {
			t.Fatalf("totalAccumulated = %d, want %d (charged once)", state.totalAccumulated, len(bigID))
		}
	})
}
