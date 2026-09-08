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
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
)

// mustNoteDetail asserts the report carries feature and returns its detail.
func mustNoteDetail(t *testing.T, report *ConversionReport, feature Feature, context string) string {
	t.Helper()
	for _, entry := range report.Losses {
		if entry.Feature == feature {
			return entry.Detail
		}
	}
	t.Fatalf("%s: %s note missing: %+v", context, feature, report.Losses)
	return ""
}

// mustNoNote asserts the report carries no entry for feature.
func mustNoNote(t *testing.T, report *ConversionReport, feature Feature, context string) {
	t.Helper()
	for _, entry := range report.Losses {
		if entry.Feature == feature {
			t.Fatalf("%s: unexpected %s note: %+v", context, feature, entry)
		}
	}
}

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
// exactly, with the mismatch recorded by the stream's usage clamp.
func TestChatUsageMismatchStreamingRelayed(t *testing.T) {
	chunk := chatChunk(t, ChatStreamDelta{Content: new("x")}, nil)
	chunk.Usage = &ChatLLMUsage{
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      20, // not 15
	}
	state := newChatResponsesStreamState(
		testStreamContext(), j6PermissivePolicy(), ChatCapabilities{},
		"resp_1", "gpt-4.1", 1710000000, nil,
	)
	if _, err := state.Convert(chunk); err != nil {
		t.Fatalf("mismatch must be relayed, not rejected: %v", err)
	}
	if state.usage == nil || state.usage.TotalTokens != 20 {
		t.Fatalf("usage = %+v, want the source's own total 20", state.usage)
	}
	mustMismatchNote(t, &state.report, "streaming chat usage")
}

// TestResponsesUsageMismatchStreamingRelayed proves the streaming responses
// -> anthropic conversion relays a known total that is not input + output
// exactly, with the mismatch recorded by the stream's usage clamp.
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
	if err := state.finalizeMessage(CanonicalStopEndTurn, usage); err != nil {
		t.Fatalf("mismatch must be relayed, not rejected: %v", err)
	}
	if state.usage == nil || state.usage.OutputTokens != 5 {
		t.Fatalf("source usage must be relayed as-is: %+v", state.usage)
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
// decode relays a contract-violating total and the RENDER records the
// mismatch: the note describes the emitted counts, so it belongs to the
// conversion that emits them, never to the decode.
func TestChatUsageMismatchNonStreamingRelayed(t *testing.T) {
	body := []byte(`{"object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":20}}`)
	response, report, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatalf("mismatch must be relayed, not rejected: %v", err)
	}
	if !response.Usage.TotalKnown || response.Usage.TotalTokens != 20 {
		t.Fatalf("source total must be relayed as-is: %+v", response.Usage)
	}
	if reportHasFeature(report, FeatureUsageTotalMismatch) {
		t.Fatalf("decode recorded the mismatch before the counts were emitted: %+v", report)
	}
	context := testExchangeContext()
	context.LossPolicy = j6PermissivePolicy()
	context.RequestedClientModel = "m"
	_, renderReport, err := RenderResponsesResponse(response, context)
	if err != nil {
		t.Fatalf("render must not reject the mismatch: %v", err)
	}
	mustMismatchNote(t, &renderReport, "non-streaming chat usage")
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
	_, _, err := responsesUsageToAnthropicUsage(usage, responsesUsagePresence(usage))
	if err == nil {
		t.Fatal("32-bit overflow to anthropic accepted")
	}
	if _, ok := errors.AsType[*UsageArithmeticError](err); !ok {
		t.Fatalf("err = %T %v, want *UsageArithmeticError (width overflow)", err, err)
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

// TestChatResponsesRenderDerivedTotalSaturates pins the Responses render of a
// source that omitted total_tokens: the derived input + output sum is checked,
// so a sum that cannot be represented saturates the emitted total and records
// the saturation instead of wrapping into a negative count — a negative total
// would violate the arithmetic invariant the clamp exists to keep.
func TestChatResponsesRenderDerivedTotalSaturates(t *testing.T) {
	body := []byte(`{"object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],` +
		`"usage":{"prompt_tokens":9223372036854775807,"completion_tokens":1}}`)
	response, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, j6PermissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	context := testExchangeContext()
	context.LossPolicy = j6PermissivePolicy()
	context.RequestedClientModel = "m"
	rendered, report, err := RenderResponsesResponse(response, context)
	if err != nil {
		t.Fatalf("render must not fail on a saturating derived total: %v", err)
	}
	var envelope struct {
		Usage ResponsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(rendered, &envelope); err != nil {
		t.Fatalf("rendered response is not valid JSON: %v", err)
	}
	if envelope.Usage.TotalTokens != math.MaxInt64 {
		t.Fatalf("total = %d, want saturation to %d", envelope.Usage.TotalTokens, int64(math.MaxInt64))
	}
	if envelope.Usage.TotalTokens < 0 {
		t.Fatalf("negative total rendered: %+v", envelope.Usage)
	}
	if detail := mustNoteDetail(t, &report, FeatureUsageTotalMismatch, "derived-total saturation"); !strings.Contains(detail, "saturated") {
		t.Fatalf("saturation detail = %q", detail)
	}
}

// TestChatResponsesRenderPostClampMismatchNoted pins the Responses-client
// surface for a mismatch the CLAMP creates: the source was self-consistent
// (3 = -2 + 5) and only its negative count was inconsistent, so the emitted
// 0 + 5 is not 3 and the note must name the emitted counts plus the corrected
// source numbers rather than describing the clamped values as relayed as-is.
// The attribution is exact: only the input was corrected, so the note names
// that correction with both numbers and never claims corrections the clamp did
// not apply (the total 3 and output 5 are source values it left untouched).
func TestChatResponsesRenderPostClampMismatchNoted(t *testing.T) {
	body := []byte(`{"object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],` +
		`"usage":{"prompt_tokens":-2,"completion_tokens":5,"total_tokens":3,` +
		`"prompt_tokens_details":{"cached_tokens":1}}}`)
	response, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, j6PermissivePolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	context := testExchangeContext()
	context.LossPolicy = j6PermissivePolicy()
	context.RequestedClientModel = "m"
	rendered, report, err := RenderResponsesResponse(response, context)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var envelope struct {
		Usage ResponsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(rendered, &envelope); err != nil {
		t.Fatalf("rendered response is not valid JSON: %v", err)
	}
	if envelope.Usage.InputTokens != 0 || envelope.Usage.OutputTokens != 5 || envelope.Usage.TotalTokens != 3 {
		t.Fatalf("usage = %+v, want the clamped input 0, output 5, and the source total 3", envelope.Usage)
	}
	mustNoteDetail(t, &report, FeatureUsageNegativeCounts, "post-clamp mismatch")
	detail := mustNoteDetail(t, &report, FeatureUsageTotalMismatch, "post-clamp mismatch")
	want := "the usage total 3 is not the exact sum of input 0 + output 5 " +
		"(the clamp corrected the source's input -2 to 0); the mismatch is recorded"
	if detail != want {
		t.Fatalf("detail = %q, want %q", detail, want)
	}
}

// TestChatResponsesStreamPostClampMismatchNoted pins the same disposition on
// the streaming Responses-client surface: the clamp-created mismatch is
// recorded once and the stream carries the clamped values.
func TestChatResponsesStreamPostClampMismatchNoted(t *testing.T) {
	chunk := chatChunk(t, ChatStreamDelta{Content: new("x")}, nil)
	chunk.Usage = &ChatLLMUsage{
		PromptTokens:     -2,
		CompletionTokens: 5,
		TotalTokens:      3,
		PromptTokensDetails: &ChatPromptTokensDetails{
			CachedTokens: 1,
		},
	}
	state := newChatResponsesStreamState(
		testStreamContext(), j6PermissivePolicy(), ChatCapabilities{},
		"resp_1", "gpt-4.1", 1710000000, nil,
	)
	if _, err := state.Convert(chunk); err != nil {
		t.Fatalf("clamp-created mismatch must be relayed, not rejected: %v", err)
	}
	if state.usage == nil || state.usage.InputTokens != 0 || state.usage.TotalTokens != 3 {
		t.Fatalf("usage = %+v, want the clamped input 0 with the source total 3", state.usage)
	}
	mustMismatchNote(t, &state.report, "streaming chat clamp mismatch")
}

// TestTotalMismatchDetailNamesOnlyAppliedCorrections pins the correction
// attribution of the usage_total_mismatch detail: it names each source value
// the clamp actually changed, with both the source and the emitted number, and
// never claims a correction the clamp did not apply. A source whose total and
// output are relayed unchanged must not be described as corrected.
func TestTotalMismatchDetailNamesOnlyAppliedCorrections(t *testing.T) {
	present := usagePresence{input: true, output: true, total: true}
	tests := []struct {
		name     string
		emitted  usageTotals
		source   usageTotals
		presence usagePresence
		want     string
	}{
		{
			name:     "input corrected",
			emitted:  usageTotals{input: 0, output: 5, total: 3},
			source:   usageTotals{input: -2, output: 5, total: 3},
			presence: present,
			want:     "the usage total 3 is not the exact sum of input 0 + output 5 (the clamp corrected the source's input -2 to 0); the mismatch is recorded",
		},
		{
			name:     "output and total corrected",
			emitted:  usageTotals{input: 2, output: 0, total: 0},
			source:   usageTotals{input: 2, output: -3, total: -1},
			presence: present,
			want:     "the usage total 0 is not the exact sum of input 2 + output 0 (the clamp corrected the source's output -3 to 0 and total -1 to 0); the mismatch is recorded",
		},
		{
			name:     "no correction relayed as-is",
			emitted:  usageTotals{input: 10, output: 5, total: 20},
			source:   usageTotals{input: 10, output: 5, total: 20},
			presence: present,
			want:     "the usage total 20 is not the exact sum of input 10 + output 5; the source total, input, and output are relayed as-is",
		},
		{
			// The emitted input 0 is the target's default, not a source
			// fact: the detail must name it absent, never as relayed.
			name:     "input absent named absent",
			emitted:  usageTotals{input: 0, output: 5, total: 20},
			source:   usageTotals{input: 0, output: 5, total: 20},
			presence: usagePresence{output: true, total: true},
			want:     "the usage total 20 is not the exact sum of input 0 + output 5; the source did not report input, and the values it did report are relayed as-is",
		},
		{
			name:     "output absent named absent",
			emitted:  usageTotals{input: 10, output: 0, total: 20},
			source:   usageTotals{input: 10, output: 0, total: 20},
			presence: usagePresence{input: true, total: true},
			want:     "the usage total 20 is not the exact sum of input 10 + output 0; the source did not report output, and the values it did report are relayed as-is",
		},
		{
			name:     "input and output absent named absent",
			emitted:  usageTotals{input: 0, output: 0, total: 20},
			source:   usageTotals{input: 0, output: 0, total: 20},
			presence: usagePresence{total: true},
			want:     "the usage total 20 is not the exact sum of input 0 + output 0; the source did not report input or output, and the values it did report are relayed as-is",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail, ok := totalMismatchDetail(tt.emitted, tt.source, tt.presence)
			if !ok {
				t.Fatalf("totalMismatchDetail(%+v, %+v) recorded nothing, want a note", tt.emitted, tt.source)
			}
			if detail != tt.want {
				t.Fatalf("detail = %q, want %q", detail, tt.want)
			}
		})
	}
	// A consistent emitted triple records nothing, whatever the source was.
	if detail, ok := totalMismatchDetail(
		usageTotals{input: 2, output: 3, total: 5},
		usageTotals{input: -2, output: 3, total: 5},
		present,
	); ok {
		t.Fatalf("consistent emitted triple recorded a note: %q", detail)
	}
}

// TestStreamUsageClampNotesGateOncePerKey pins the per-stream-per-key gating:
// three chunks carrying the same arithmetic inconsistency record ONE note per
// key, not one per chunk. The per-request log line dedupes by feature at path
// independently, so only the report length (a bounded resource) shows the
// difference.
func TestStreamUsageClampNotesGateOncePerKey(t *testing.T) {
	chunk := chatChunk(t, ChatStreamDelta{Content: new("x")}, nil)
	chunk.Usage = &ChatLLMUsage{
		PromptTokens:     1,
		CompletionTokens: 1,
		TotalTokens:      2,
		PromptTokensDetails: &ChatPromptTokensDetails{
			CachedTokens: 5,
		},
	}
	state := newChatResponsesStreamState(
		testStreamContext(), j6PermissivePolicy(), ChatCapabilities{},
		"resp_1", "gpt-4.1", 1710000000, nil,
	)
	for i := range 3 {
		if _, err := state.Convert(chunk); err != nil {
			t.Fatalf("chunk %d: the clamp must be relayed, not rejected: %v", i, err)
		}
	}
	if got := countClampNotes(&state.report, FeatureUsageCacheExceedsInput); got != 1 {
		t.Fatalf("usage_cache_exceeds_input notes = %d, want 1 (gated once per stream per key)", got)
	}
	if got := countClampNotes(&state.report, FeatureUsageNegativeCounts); got != 0 {
		t.Fatalf("usage_negative_counts notes = %d, want 0", got)
	}
}

// countClampNotes counts the Note entries for one feature.
func countClampNotes(report *ConversionReport, feature Feature) int {
	count := 0
	for _, l := range report.Losses {
		if l.Feature == feature && l.Kind == NoteRecord {
			count++
		}
	}
	return count
}

// TestUsageClampDetailFidelity pins the absent-vs-zero wording of the clamp
// details (a defaulted target zero is never presented as a source fact) and
// the no-spurious-note guarantee: a source that needs no correction records
// nothing, and a render of it carries no clamp note.
func TestUsageClampDetailFidelity(t *testing.T) {
	t.Run("absent input total", func(t *testing.T) {
		usage := &CanonicalUsage{
			OutputTokens:    2,
			OutputKnown:     true,
			TotalTokens:     2,
			TotalKnown:      true,
			CacheReadTokens: 5,
			CacheReadKnown:  true,
		}
		clamp := clampCanonicalUsage(usage)
		if !clamp.cacheExceedsInput {
			t.Fatalf("clamp = %+v, want cacheExceedsInput", clamp)
		}
		if usage.CacheReadTokens != 0 {
			t.Fatalf("cache read = %d, want the clamp to zero it", usage.CacheReadTokens)
		}
		if !strings.Contains(clamp.cacheDetail, "no reported input total to bound it") {
			t.Fatalf("detail = %q, want the absent-input wording", clamp.cacheDetail)
		}
		if strings.Contains(clamp.cacheDetail, "exceeds the input total") {
			t.Fatalf("detail asserts a source excess the source never established: %q", clamp.cacheDetail)
		}
	})
	t.Run("corrected input total", func(t *testing.T) {
		usage := &CanonicalUsage{
			InputTokens:     -2,
			InputKnown:      true,
			OutputTokens:    5,
			OutputKnown:     true,
			TotalTokens:     3,
			TotalKnown:      true,
			CacheReadTokens: 1,
			CacheReadKnown:  true,
		}
		clamp := clampCanonicalUsage(usage)
		if !clamp.cacheExceedsInput {
			t.Fatalf("clamp = %+v, want cacheExceedsInput", clamp)
		}
		if !strings.Contains(clamp.cacheDetail, "exceeds the input total 0") ||
			!strings.Contains(clamp.cacheDetail, "the source reported input -2") {
			t.Fatalf("detail = %q, want the applied bound 0 and the corrected source input -2", clamp.cacheDetail)
		}
	})
	t.Run("reported input total", func(t *testing.T) {
		usage := &CanonicalUsage{
			InputTokens:     10,
			InputKnown:      true,
			OutputTokens:    5,
			OutputKnown:     true,
			TotalTokens:     15,
			TotalKnown:      true,
			CacheReadTokens: 11,
			CacheReadKnown:  true,
		}
		clamp := clampCanonicalUsage(usage)
		if !strings.Contains(clamp.cacheDetail, "exceeds the input total 10") {
			t.Fatalf("detail = %q, want the reported input total named", clamp.cacheDetail)
		}
	})
	t.Run("consistent source records nothing", func(t *testing.T) {
		usage := &CanonicalUsage{
			InputTokens:     10,
			InputKnown:      true,
			OutputTokens:    5,
			OutputKnown:     true,
			TotalTokens:     15,
			TotalKnown:      true,
			CacheReadTokens: 3,
			CacheReadKnown:  true,
		}
		if clamp := clampCanonicalUsage(usage); !clamp.empty() {
			t.Fatalf("clamp = %+v, want empty for a consistent source", clamp)
		}
	})
	t.Run("consistent render carries no clamp note", func(t *testing.T) {
		body := []byte(`{"object":"chat.completion","created":1,"model":"m",` +
			`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,` +
			`"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":0}}}`)
		response, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, j6PermissivePolicy())
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		context := testExchangeContext()
		context.LossPolicy = j6PermissivePolicy()
		context.RequestedClientModel = "m"
		rendered, report, err := RenderResponsesResponse(response, context)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		mustNoNote(t, &report, FeatureUsageTotalMismatch, "consistent render")
		mustNoNote(t, &report, FeatureUsageCacheExceedsInput, "consistent render")
		mustNoNote(t, &report, FeatureUsageNegativeCounts, "consistent render")
		var envelope struct {
			Usage ResponsesUsage `json:"usage"`
		}
		if err := json.Unmarshal(rendered, &envelope); err != nil {
			t.Fatalf("rendered response is not valid JSON: %v", err)
		}
		if envelope.Usage.InputTokens != 10 || envelope.Usage.OutputTokens != 5 || envelope.Usage.TotalTokens != 15 {
			t.Fatalf("usage = %+v, want the source values relayed", envelope.Usage)
		}
	})
}

// TestUsageClampBoundsArePinned pins the clamp lines whose removal would emit
// a client-dialect-invalid usage: each subtest fails when the named bound is
// removed (the mutations were identified by an independent review of the
// committed diff). The canonical cache-write bound and its Responses-shape
// twin keep the Anthropic identity uncached = input - cache-read - cache-write
// nonnegative; the total clamp and the detail builders keep the recorded note
// accurate.
func TestUsageClampBoundsArePinned(t *testing.T) {
	t.Run("canonical cache-creation bound", func(t *testing.T) {
		usage := &CanonicalUsage{
			InputTokens: 10, InputKnown: true,
			CacheReadTokens: 0, CacheReadKnown: true,
			CacheWriteTokens: 20, CacheWriteKnown: true,
			OutputTokens: 5, OutputKnown: true,
			TotalTokens: 35, TotalKnown: true,
		}
		clamp := clampCanonicalUsage(usage)
		if !clamp.cacheExceedsInput || usage.CacheWriteTokens != 10 {
			t.Fatalf("clamp = %+v, cache-write = %d, want the bound to input-cache-read = 10", clamp, usage.CacheWriteTokens)
		}
		if uncached := usage.InputTokens - usage.CacheReadTokens - usage.CacheWriteTokens; uncached != 0 {
			t.Fatalf("uncached = %d, want 0 (the Anthropic identity)", uncached)
		}
	})
	t.Run("responses-shape cache-creation bound", func(t *testing.T) {
		created := int64(20)
		usage := &ResponsesUsage{
			InputTokens:        10,
			OutputTokens:       5,
			TotalTokens:        35,
			CreatedCacheTokens: &created,
		}
		clamp := clampResponsesUsage(usage, responsesUsagePresence(usage))
		if !clamp.cacheExceedsInput || *usage.CreatedCacheTokens != 10 {
			t.Fatalf("clamp = %+v, created = %d, want the bound to 10", clamp, *usage.CreatedCacheTokens)
		}
	})
	t.Run("negative total clamped and noted", func(t *testing.T) {
		usage := &CanonicalUsage{
			InputTokens: 5, InputKnown: true,
			OutputTokens: 3, OutputKnown: true,
			TotalTokens: -8, TotalKnown: true,
		}
		clamp := clampCanonicalUsage(usage)
		if usage.TotalTokens != 0 || !clamp.negativeCounts {
			t.Fatalf("clamp = %+v, total = %d, want the total clamped to 0 with a negative-counts note", clamp, usage.TotalTokens)
		}
	})
	t.Run("negative detail names every source count", func(t *testing.T) {
		usage := &CanonicalUsage{
			InputTokens: -2, InputKnown: true,
			OutputTokens: -3, OutputKnown: true,
			TotalTokens: -1, TotalKnown: true,
		}
		clamp := clampCanonicalUsage(usage)
		for _, want := range []string{
			"input -2", "output -3", "total -1",
			"cache-read absent", "cache-creation absent", "reasoning absent",
		} {
			if !strings.Contains(clamp.negativeDetail, want) {
				t.Fatalf("detail = %q, missing %q", clamp.negativeDetail, want)
			}
		}
	})
	t.Run("cache detail names the source breakdown", func(t *testing.T) {
		usage := &CanonicalUsage{
			InputTokens: 10, InputKnown: true,
			CacheReadTokens: 3, CacheReadKnown: true,
			CacheWriteTokens: 20, CacheWriteKnown: true,
			OutputTokens: 5, OutputKnown: true,
			TotalTokens: 35, TotalKnown: true,
		}
		clamp := clampCanonicalUsage(usage)
		if !clamp.cacheExceedsInput {
			t.Fatalf("clamp = %+v, want cacheExceedsInput", clamp)
		}
		for _, want := range []string{"3 read", "20 creation", "exceeds the input total 10"} {
			if !strings.Contains(clamp.cacheDetail, want) {
				t.Fatalf("detail = %q, missing %q", clamp.cacheDetail, want)
			}
		}
	})
}

// TestComposedClampNoteRecordedOnce pins the composed Messages<-Chat merge:
// both sub-states convert the same source usage, so both can record the same
// clamp fact. The merged report must carry it once, and the surviving detail
// must be the chat state's, which names the source numbers instead of
// presenting the clamp-corrected values as source values.
func TestComposedClampNoteRecordedOnce(t *testing.T) {
	chat := newChatResponsesStreamState(
		testStreamContext(), j6PermissivePolicy(), ChatCapabilities{},
		"resp_1", "gpt-4.1", 1710000000, nil,
	)
	anthropic := newAnthropicResponsesStreamState(
		testStreamContext(), j6PermissivePolicy(), ChatCapabilities{},
		"msg_1", "gpt-4.1", 1710000000,
	)
	converter := newChatToAnthropicConverter(chat, anthropic)
	for name, chunk := range map[string]string{
		"usage chunk":  `{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"x"}}],"usage":{"prompt_tokens":-2,"completion_tokens":5,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":1}}}`,
		"finish chunk": `{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	} {
		if _, err := converter.Convert(SSEEvent{Data: []byte(chunk)}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := converter.Convert(SSEEvent{Data: []byte("[DONE]")}); err != nil {
		t.Fatalf("terminal sentinel: %v", err)
	}
	report := converter.ConversionReport()
	if got := countFeature(*report, FeatureUsageTotalMismatch); got != 1 {
		t.Fatalf("usage_total_mismatch entries = %d, want exactly 1 (the merge dedupes by feature at path)", got)
	}
	detail := mustNoteDetail(t, report, FeatureUsageTotalMismatch, "composed clamp")
	if want := "the clamp corrected the source's input -2 to 0"; !strings.Contains(detail, want) {
		t.Fatalf("detail = %q, want it to name the applied correction (%q)", detail, want)
	}
}
