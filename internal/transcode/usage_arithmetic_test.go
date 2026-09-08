package transcode

// Usage arithmetic acceptance tests. History: review-z commit 5 pinned
// exact total == input + output and failed the exchange on mismatch.
// CC-USAGE-ARITHMETIC (operator-observed 2026-09-08: a real glm gateway
// emitted total 293640 vs sum 293581, 502-ing Claude Code 8 retries on a
// 293K-token session) re-adjudicated the disposition: a mismatched total is
// an OBSERVABILITY fact — the source values are relayed as-is and the
// mismatch is recorded as a usage_total_mismatch note. The
// architecture-independent int64-to-int width checks and the
// absent-vs-zero usage fidelity pins are unchanged.

import (
	"errors"
	"strconv"
	"testing"
)

// mustMismatchNote asserts the report carries the usage_total_mismatch note.
func mustMismatchNote(t *testing.T, report *ConversionReport, context string) {
	t.Helper()
	for _, l := range report.Losses {
		if l.Feature == FeatureUsageTotalMismatch {
			return
		}
	}
	t.Fatalf("%s: usage_total_mismatch note missing: %+v", context, report.Losses)
}

// TestChatUsageMismatchStreamingRelayed proves the streaming chat ->
// responses conversion relays a known total that is not prompt + completion
// exactly, with the mismatch recorded (CC-USAGE-ARITHMETIC).
func TestChatUsageMismatchStreamingRelayed(t *testing.T) {
	usage := &ChatLLMUsage{
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      20, // not 15
	}
	state := newChatResponsesStreamState(
		testStreamContext(), StrictLossPolicy(), ChatCapabilities{},
		"resp_1", "m", 1710000000, nil,
	)
	state.noteUsageTotalMismatchChat(usage)
	got, err := chatUsageToResponsesUsage(usage)
	if err != nil {
		t.Fatalf("mismatch must be relayed, not rejected: %v", err)
	}
	if got.TotalTokens != 20 {
		t.Fatalf("total = %d, want the source's own 20", got.TotalTokens)
	}
	mustMismatchNote(t, &state.report, "streaming chat usage")
}

// TestResponsesUsageMismatchStreamingRelayed proves the streaming responses
// -> anthropic conversion relays a known total that is not input + output
// exactly, with the mismatch recorded (CC-USAGE-ARITHMETIC).
func TestResponsesUsageMismatchStreamingRelayed(t *testing.T) {
	usage := &ResponsesUsage{
		InputTokens:  10,
		OutputTokens: 5,
		TotalTokens:  20, // not 15
	}
	state := newAnthropicResponsesStreamState(
		testStreamContext(), j6PermissivePolicy(), ChatCapabilities{ProviderReasoningThinking: true},
		"msg_1", "claude-x", 1710000000,
	)
	state.noteUsageTotalMismatch(usage)
	got, err := responsesUsageToAnthropicUsage(usage)
	if err != nil {
		t.Fatalf("mismatch must be relayed, not rejected: %v", err)
	}
	if got.OutputTokens != 5 {
		t.Fatalf("output = %d, want the source's own 5", got.OutputTokens)
	}
	mustMismatchNote(t, &state.report, "streaming responses usage")
}

// TestResponsesUsageMismatchNonStreamingRelayed proves the non-streaming
// responses decode relays a contract-violating total (CC-USAGE-ARITHMETIC).
func TestResponsesUsageMismatchNonStreamingRelayed(t *testing.T) {
	body := []byte(`{"object":"response","id":"resp_1","created_at":1.0,"model":"m","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":20,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`)
	response, err := DecodeResponsesResponse(body)
	if err != nil {
		t.Fatalf("mismatch must be relayed, not rejected: %v", err)
	}
	if response.Usage.TotalTokens != 20 {
		t.Fatalf("source total must be relayed as-is: %d", response.Usage.TotalTokens)
	}
}

// TestChatUsageMismatchNonStreamingRelayed proves the non-streaming chat
// decode relays a contract-violating total with the mismatch note recorded
// (CC-USAGE-ARITHMETIC).
func TestChatUsageMismatchNonStreamingRelayed(t *testing.T) {
	body := []byte(`{"object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":20}}`)
	response, report, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatalf("mismatch must be relayed, not rejected: %v", err)
	}
	if !response.Usage.TotalKnown || response.Usage.TotalTokens != 20 {
		t.Fatalf("source total must be relayed as-is: %+v", response.Usage)
	}
	mustMismatchNote(t, &report, "non-streaming chat usage")
}

// TestCheckedInt64ToInt32BitSafety proves the conversion helper rejects
// values that do not fit the platform int width: on 32-bit builds
// MaxInt32+1 overflows a plain cast, on 64-bit builds MaxInt64 is
// representable. The test is architecture-independent (review-z commit 5).
func TestCheckedInt64ToInt32BitSafety(t *testing.T) {
	if strconv.IntSize == 32 {
		// 32-bit build: int64 values above MaxInt32 must be rejected.
		value := int64(1) << 31 // MaxInt32 + 1
		if _, err := checkedInt64ToInt(value); err == nil {
			t.Fatal("32-bit: MaxInt32+1 accepted; silent overflow")
		}
		if converted, err := checkedInt64ToInt(value - 1); err != nil || int64(converted) != value-1 {
			t.Fatalf("32-bit: MaxInt32 rejected: %d %v", converted, err)
		}
	} else {
		// 64-bit build: every int64 is representable.
		value := int64(1)<<62 + 12345
		if converted, err := checkedInt64ToInt(value); err != nil || int64(converted) != value {
			t.Fatalf("64-bit: %d rejected: %v", value, err)
		}
	}
	// Zero and small values always convert.
	for _, value := range []int64{0, 1, 1000} {
		if converted, err := checkedInt64ToInt(value); err != nil || int64(converted) != value {
			t.Fatalf("value %d rejected: %v", value, err)
		}
	}
}

// TestResponsesUsageOverflowToAnthropic proves the streaming responses ->
// anthropic conversion rejects counts that cannot be represented on this
// platform instead of silently wrapping (review-z commit 5).
func TestResponsesUsageOverflowToAnthropic(t *testing.T) {
	if strconv.IntSize != 32 {
		t.Skip("32-bit-specific: int is 64 bits here, nothing can overflow")
	}
	usage := &ResponsesUsage{
		InputTokens:  1 << 40,
		OutputTokens: 1 << 40,
		TotalTokens:  1 << 41,
		InputTokensDetails: &UsageInputTokensDetails{
			CachedTokens: 1 << 40,
		},
		OutputTokensDetails: &UsageOutputTokensDetails{
			ReasoningTokens: 1 << 40,
		},
	}
	_, err := responsesUsageToAnthropicUsage(usage)
	if err == nil {
		t.Fatal("32-bit overflow to anthropic accepted")
	}
	var uaErr *UsageArithmeticError
	if !errors.As(err, &uaErr) || uaErr.SourceMismatch {
		t.Fatalf("err = %T %v, want a non-mismatch UsageArithmeticError (width overflow)", err, err)
	}
}

// TestUsageAbsentVsZeroPreserved proves the exact-equality work preserves
// absent-vs-zero fidelity: a chat response with usage present but no
// total_tokens still decodes with the derived total (Known flags intact) and
// never fabricates an inconsistent rejection (review-z commit 5).
func TestUsageAbsentVsZeroPreserved(t *testing.T) {
	// Chat usage without total_tokens: the shadow marks the total unknown,
	// the decode must not fabricate a mismatch.
	body := []byte(`{"object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	response, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatalf("chat decode with absent total rejected: %v", err)
	}
	if !response.Usage.InputKnown || !response.Usage.OutputKnown || response.Usage.TotalKnown {
		t.Fatalf("known flags = input %v output %v total %v, want known/known/unknown",
			response.Usage.InputKnown, response.Usage.OutputKnown, response.Usage.TotalKnown)
	}
	if response.Usage.TotalTokens != 0 {
		t.Fatalf("absent total fabricated as %d", response.Usage.TotalTokens)
	}
}

// TestStreamResponsesUsageMismatchRelayedAtTerminal pins the stream-path
// disposition (CC-USAGE-ARITHMETIC): a mismatched total on the terminal
// envelope is recorded as a usage_total_mismatch note and the stream
// completes with the source's own usage — never a 502.
func TestStreamResponsesUsageMismatchRelayedAtTerminal(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{},
		"msg_1",
		"claude-x",
		1,
	)
	if _, err := state.Convert(ResponseCreatedEvent{
		Type: "response.created", SequenceNumber: 0,
		Response: ResponseEnvelope{
			ID: "msg_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "claude-x",
			Output: []ResponsesOutputItem{},
			Usage: &ResponsesUsage{
				InputTokens: 0, OutputTokens: 0, TotalTokens: 0,
				InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
				OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// The terminal envelope carries a mismatched total: 10 + 5 = 15, not 20.
	if _, err := state.completed(ResponseEnvelope{
		ID:        "msg_1",
		Model:     "claude-x",
		CreatedAt: 1,
		Status:    "completed",
		Usage: &ResponsesUsage{
			InputTokens:  10,
			OutputTokens: 5,
			TotalTokens:  20,
			InputTokensDetails: &UsageInputTokensDetails{
				CachedTokens: 0,
			},
			OutputTokensDetails: &UsageOutputTokensDetails{
				ReasoningTokens: 0,
			},
		},
	}); err != nil {
		t.Fatalf("mismatched usage must not fail the stream: %v", err)
	}
	if !state.sawTerminal {
		t.Fatal("terminal not reached")
	}
	if state.usage == nil || state.usage.OutputTokens != 5 {
		t.Fatalf("source usage must be relayed as-is: %+v", state.usage)
	}
	mustMismatchNote(t, &state.report, "terminal envelope mismatch")
}

// TestStreamResponsesUsageMismatchRelayedAtCreated pins the messageStart
// call site: a mismatched total on response.created is recorded and the
// stream continues (CC-USAGE-ARITHMETIC).
func TestStreamResponsesUsageMismatchRelayedAtCreated(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{},
		"msg_1",
		"claude-x",
		1,
	)
	if _, err := state.Convert(ResponseCreatedEvent{
		Type: "response.created", SequenceNumber: 0,
		Response: ResponseEnvelope{
			ID: "msg_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "claude-x",
			Output: []ResponsesOutputItem{},
			Usage: &ResponsesUsage{
				InputTokens: 10, OutputTokens: 5, TotalTokens: 20,
				InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
				OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
			},
		},
	}); err != nil {
		t.Fatalf("mismatched created usage must not fail the stream: %v", err)
	}
	mustMismatchNote(t, &state.report, "created envelope mismatch")
}
