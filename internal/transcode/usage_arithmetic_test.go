package transcode

// Review-z commit 5 acceptance tests: exact usage-total equality everywhere
// the pinned contract defines total as input + output, checked
// architecture-independent int64-to-int conversions before rendering
// Messages usage, and preserved absent-vs-zero usage fidelity.

import (
	"errors"
	"strconv"
	"testing"
)

// mustUsageError asserts err is (or wraps) a UsageArithmeticError.
func mustUsageError(t *testing.T, err error, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: usage mismatch accepted", context)
	}
	var usageErr *UsageArithmeticError
	if !errors.As(err, &usageErr) {
		t.Fatalf("%s: err = %T %v, want *UsageArithmeticError", context, err, err)
	}
}

// TestChatUsageExactTotalStreaming proves the streaming chat -> responses
// conversion rejects a known total that is not prompt + completion exactly
// (review-z commit 5).
func TestChatUsageExactTotalStreaming(t *testing.T) {
	usage := &ChatLLMUsage{
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      20, // must be 15
	}
	_, err := chatUsageToResponsesUsage(usage)
	mustUsageError(t, err, "streaming chat usage")
}

// TestResponsesUsageExactTotalStreaming proves the streaming responses ->
// anthropic conversion rejects a known total that is not input + output
// exactly (review-z commit 5).
func TestResponsesUsageExactTotalStreaming(t *testing.T) {
	usage := &ResponsesUsage{
		InputTokens:  10,
		OutputTokens: 5,
		TotalTokens:  20, // must be 15
	}
	_, err := responsesUsageToAnthropicUsage(usage)
	mustUsageError(t, err, "streaming responses usage")
}

// TestResponsesUsageExactTotalNonStreaming proves the non-streaming
// responses decode rejects a contract-violating total: the canonical
// validation is the single chokepoint, and the decode wraps the violation as
// corrupt upstream wire (an upstream failure, never local) (review-z
// commit 5).
func TestResponsesUsageExactTotalNonStreaming(t *testing.T) {
	body := []byte(`{"object":"response","id":"resp_1","created_at":1.0,"model":"m","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":20,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`)
	_, err := DecodeResponsesResponse(body)
	mustUsageError(t, err, "non-streaming responses usage")
	var wireErr *UpstreamWireError
	if !errors.As(err, &wireErr) {
		t.Fatalf("err = %T %v, want *UpstreamWireError wrap (corrupt upstream wire)", err, err)
	}
}

// TestChatUsageExactTotalNonStreaming proves the non-streaming chat decode
// rejects a contract-violating total the same way (review-z commit 5).
func TestChatUsageExactTotalNonStreaming(t *testing.T) {
	body := []byte(`{"object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":20}}`)
	_, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, StrictLossPolicy())
	mustUsageError(t, err, "non-streaming chat usage")
	var wireErr *UpstreamWireError
	if !errors.As(err, &wireErr) {
		t.Fatalf("err = %T %v, want *UpstreamWireError wrap (corrupt upstream wire)", err, err)
	}
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
	mustUsageError(t, err, "32-bit overflow to anthropic")
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

// TestStreamResponsesUsageMismatchIsUpstreamWire pins the stream-path
// discrimination (review-z commit 5): a source-total mismatch in the
// responses -> anthropic stream must surface as corrupt upstream wire
// (SawUpstreamErrorFrame, an upstream failure), never as a local conversion
// failure.
func TestStreamResponsesUsageMismatchIsUpstreamWire(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		"msg_1",
		"claude-x",
		1,
	)
	// The stream lifecycle begins with response.created (the FSM rejects a
	// terminal before it).
	if _, err := state.Convert(ResponseCreatedEvent{
		EventBase: EventBase{Type: "response.created", SequenceNumber: 0},
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
	// The terminal envelope carries a contract-violating total: 10 + 5 = 15,
	// not 20. The exchange must be an upstream wire failure.
	_, err := state.completed(ResponseEnvelope{
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
	})
	if err == nil {
		t.Fatal("contract-violating total accepted")
	}
	var wireErr *UpstreamWireError
	if !errors.As(err, &wireErr) {
		t.Fatalf("err = %T %v, want *UpstreamWireError (source-total mismatch is corrupt upstream wire)", err, err)
	}
	var usageErr *UsageArithmeticError
	if !errors.As(err, &usageErr) || !usageErr.SourceMismatch {
		t.Fatalf("err = %T %v, want the flagged source-mismatch typed error inside", err, err)
	}
	if state.sawTerminal {
		t.Fatal("terminal state reached despite the corrupt usage")
	}
}

// TestStreamResponsesUsageMismatchAtCreatedIsUpstreamWire pins the SECOND
// stream call site (messageStart): a contract-violating total on the
// response.created envelope is corrupt upstream wire, exactly like the
// terminal-envelope path (review-z commit 5).
func TestStreamResponsesUsageMismatchAtCreatedIsUpstreamWire(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		"msg_1",
		"claude-x",
		1,
	)
	_, err := state.Convert(ResponseCreatedEvent{
		EventBase: EventBase{Type: "response.created", SequenceNumber: 0},
		Response: ResponseEnvelope{
			ID: "msg_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "claude-x",
			Output: []ResponsesOutputItem{},
			Usage: &ResponsesUsage{
				InputTokens: 10, OutputTokens: 5, TotalTokens: 20, // must be 15
				InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
				OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
			},
		},
	})
	if err == nil {
		t.Fatal("contract-violating total on response.created accepted")
	}
	var wireErr *UpstreamWireError
	if !errors.As(err, &wireErr) {
		t.Fatalf("err = %T %v, want *UpstreamWireError at messageStart", err, err)
	}
	var usageErr *UsageArithmeticError
	if !errors.As(err, &usageErr) || !usageErr.SourceMismatch {
		t.Fatalf("err = %T %v, want the flagged source-mismatch typed error inside", err, err)
	}
}
