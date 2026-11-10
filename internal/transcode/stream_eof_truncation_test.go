package transcode

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// assertEOFTruncation drives one production converter to EOF with no terminal
// condition and asserts the failure is a genuine truncation: an
// *UpstreamWireError whose chain matches errStreamTruncated (UpstreamWireError
// unwraps to its cause), never a bare local error.
func assertEOFTruncation(t *testing.T, name string, conv frameConverter, sse string) {
	t.Helper()
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(sse), 0, 0),
		conv, 0, 0, 0,
	)
	_, err := io.ReadAll(reader)
	if err == nil {
		t.Fatalf("%s: Read: nil error, want the EOF truncation", name)
	}
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("%s: err = %T %v, want *UpstreamWireError", name, err, err)
	}
	if !errors.Is(err, errStreamTruncated) {
		t.Fatalf("%s: err = %v, want errors.Is(err, errStreamTruncated) through the wrap", name, err)
	}
	if reader.SawUpstreamErrorFrame() {
		t.Fatalf("%s: SawUpstreamErrorFrame = true for a truncation; a truncation is not a frame of real upstream data", name)
	}
	if outcome := classifyStreamObservation(streamObservation{
		ReaderErr:             err,
		SawErrorEvent:         reader.SawErrorEvent(),
		SawTerminal:           reader.SawTerminal(),
		SawUpstreamErrorFrame: reader.SawUpstreamErrorFrame(),
	}); outcome != streamOutcomeUpstreamFailure {
		t.Fatalf("%s: outcome = %v, a genuine truncation classifies as an upstream failure", name, outcome)
	}
}

// TestConverterEOFTruncationIsMarkedAtEverySite drives every production stream
// converter to EOF with no terminal and proves each truncation site returns
// errStreamTruncated wrapped in *UpstreamWireError, so the reader's EOF
// classification (upstream failure, not a local conversion failure, and never
// marked as a real upstream error frame) holds for every direction.
func TestConverterEOFTruncationIsMarkedAtEverySite(t *testing.T) {
	// chat upstream SSE: one content chunk, then EOF (no finish, no [DONE]).
	const chatSSE = "data: " + `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n"

	// responses upstream SSE: created + one text delta, then EOF (no completed).
	const responsesSSE = "data: " + `{"type":"response.created","sequence_number":1,"response":{"id":"r1","object":"response","created_at":1,"status":"in_progress","model":"m","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"m1","type":"message","role":"assistant","status":"in_progress","content":[]}}` + "\n\n" +
		"data: " + `{"type":"response.content_part.added","sequence_number":3,"item_id":"m1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_text.delta","sequence_number":4,"item_id":"m1","output_index":0,"content_index":0,"delta":"hi","logprobs":[]}` + "\n\n"

	// gemini upstream SSE: one text chunk, then EOF (no finish chunk).
	const geminiSSE = "data: " + `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"index":0}]}` + "\n\n"

	// messages upstream SSE: message_start + one text block, then EOF (no message_stop).
	const messagesSSE = anthropicStartSSE + anthropicTextBlockSSE

	t.Run("chat to responses", func(t *testing.T) {
		assertEOFTruncation(t, "chatToResponses",
			newChatToResponsesConverter(newChatResponsesStreamState(
				testStreamContext(), permissiveLossPolicy(), ChatCapabilities{},
				"resp_1", "m", 1, nil,
			)), chatSSE)
	})
	t.Run("chat to anthropic", func(t *testing.T) {
		assertEOFTruncation(t, "chatToAnthropic",
			newChatToAnthropicConverter(
				newChatResponsesStreamState(
					testStreamContext(), permissiveLossPolicy(), ChatCapabilities{},
					"resp_1", "m", 1, nil,
				),
				newAnthropicResponsesStreamState(
					testStreamContext(), permissiveLossPolicy(), ChatCapabilities{},
					"msg_1", "m", 1,
				),
			), chatSSE)
	})
	t.Run("chat to gemini", func(t *testing.T) {
		assertEOFTruncation(t, "chatToGemini",
			newChatToGeminiConverter(
				newChatResponsesStreamState(
					testStreamContext(), permissiveLossPolicy(), ChatCapabilities{},
					"resp_1", "m", 1, nil,
				),
				newGeminiStreamClientState(testStreamContext(), UpstreamChatCompletions, permissiveLossPolicy(), "resp_1", "m"),
			), chatSSE)
	})
	t.Run("responses to anthropic", func(t *testing.T) {
		assertEOFTruncation(t, "responsesToAnthropic",
			newResponsesToAnthropicConverter(newAnthropicResponsesStreamState(
				testStreamContext(), permissiveLossPolicy(), ChatCapabilities{},
				"msg_1", "m", 1,
			)), responsesSSE)
	})
	t.Run("responses to gemini", func(t *testing.T) {
		assertEOFTruncation(t, "responsesToGemini",
			newResponsesToGeminiConverter(newGeminiStreamClientState(
				testStreamContext(), UpstreamResponses, permissiveLossPolicy(), "resp_1", "m",
			)), responsesSSE)
	})
	t.Run("gemini to responses", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		assertEOFTruncation(t, "geminiToResponses",
			newGeminiToResponsesConverter(newGeminiResponsesStreamState(
				ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_1", "m", 1, nil,
			)), geminiSSE)
	})
	t.Run("gemini to anthropic", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		assertEOFTruncation(t, "geminiToAnthropic",
			newGeminiToAnthropicConverter(
				newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_1", "m", 1, nil),
				newAnthropicResponsesStreamState(
					testStreamContext(), permissiveLossPolicy(), ChatCapabilities{},
					"msg_1", "m", 1,
				),
			), geminiSSE)
	})
	t.Run("messages to gemini", func(t *testing.T) {
		assertEOFTruncation(t, "messagesToGemini",
			newMessagesToGeminiConverter(
				newAnthropicUpstreamResponsesStreamState("resp_1", "claude-x", permissiveLossPolicy()),
				newGeminiStreamClientState(testStreamContext(), UpstreamMessages, permissiveLossPolicy(), "resp_1", "m"),
			), messagesSSE)
	})
}
