package transcode

// A gateway may redeliver the terminal chunk with the usage accounting
// piggybacked on it — the same single choice, the same finish reason, an
// insubstantial delta — instead of the bare choices:[] usage tail
// (observed dialagram meta-muse-spark-1.3: finish_reason tool_calls
// repeated with delta role/content:""/reasoning:null plus usage). The
// redelivery is pure accounting, never new output: it folds into the
// terminal envelope and completes to exactly one message_stop, never a
// mid-stream error after the thinking/tool blocks already yielded.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openaichat"
)

func TestStreamLifecycleRepeatedTerminalChunkAbsorbed(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{}
	mapping.ChatCapabilities = ChatCapabilities{ProviderReasoningThinking: true}
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	sse := ": keep-alive\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":1789334259,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning\":\"The\",\"reasoning_details\":[{\"type\":\"reasoning.text\",\"text\":\"The\",\"format\":\"unknown\",\"index\":0}]}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":1789334259,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning\":\" baseline.\",\"reasoning_details\":[{\"type\":\"reasoning.text\",\"text\":\" baseline.\",\"format\":\"unknown\",\"index\":0}]}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":1789334259,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":1789334259,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":1789334259,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"tool_calls\",\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning\":null}}]}\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"created\":1789334259,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"tool_calls\",\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150,\"completion_tokens_details\":{\"reasoning_tokens\":20}}}\n\n" +
		"data: [DONE]\n\n"
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `"type":"error"`) {
		t.Fatalf("error terminal in repeated-terminal stream: %q", body)
	}
	if strings.Count(body, `"type":"message_stop"`) != 1 {
		t.Fatalf("message_stop count != 1: %q", body)
	}
	if !strings.Contains(body, `"thinking_delta"`) {
		t.Fatalf("missing thinking deltas: %q", body)
	}
	if !strings.Contains(body, `"input_json_delta"`) {
		t.Fatalf("missing tool input deltas: %q", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("missing tool_use stop: %q", body)
	}
	if !strings.Contains(body, `"input_tokens":100`) {
		t.Fatalf("repeated-terminal usage not folded: %q", body)
	}
}

// The captured gateway tail replays through the full production handler and
// stream converter: the redelivered terminal folds its usage into the
// terminal envelope and the exchange completes with exactly one message_stop.
func TestStreamCapturedRepeatedTerminalTailReplayed(t *testing.T) {
	capture := testcorpus.FieldRepeatedTerminalTailSSE()
	if got := strings.Count(string(capture), `"finish_reason":"tool_calls"`); got != 2 {
		t.Fatalf("captured tail carries %d non-null finish reasons, want 2", got)
	}
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{}
	mapping.ChatCapabilities = ChatCapabilities{ProviderReasoningThinking: true}
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(bytes.NewReader(capture)),
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"meta-muse-spark-1.3","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `"type":"error"`) {
		t.Fatalf("error terminal in captured repeated-terminal stream: %q", body)
	}
	if strings.Count(body, `"type":"message_stop"`) != 1 {
		t.Fatalf("message_stop count != 1: %q", body)
	}
	if !strings.Contains(body, `"thinking_delta"`) {
		t.Fatalf("missing thinking deltas: %q", body)
	}
	if !strings.Contains(body, `"input_json_delta"`) {
		t.Fatalf("missing tool input deltas: %q", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("missing tool_use stop: %q", body)
	}
	if !strings.Contains(body, `"input_tokens":143940`) {
		t.Fatalf("captured usage not folded into the terminal envelope: %q", body)
	}
}

// A post-finish chunk that carries new output or a contradictory terminal
// stays corrupt upstream wire: only the insubstantial same-finish repeat
// is absorbed.
func TestStreamRepeatedTerminalChunkRejectsSubstantivePayload(t *testing.T) {
	finish := "tool_calls"
	envelope := func(choices []openaichat.Choice) ChatStreamResponse {
		return ChatStreamResponse{
			ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
			Choices: choices,
		}
	}
	cases := map[string]ChatStreamResponse{
		"different finish reason": envelope([]openaichat.Choice{
			{Index: 0, FinishReason: new("stop")},
		}),
		"content payload": envelope([]openaichat.Choice{
			{Index: 0, FinishReason: &finish, Delta: &openaichat.StreamDelta{Content: new("late")}},
		}),
		"reasoning payload": envelope([]openaichat.Choice{
			{Index: 0, FinishReason: &finish, Delta: &openaichat.StreamDelta{Reasoning: new("late")}},
		}),
		"reasoning content payload": envelope([]openaichat.Choice{
			{Index: 0, FinishReason: &finish, Delta: &openaichat.StreamDelta{ReasoningContent: new("late")}},
		}),
		"refusal payload": envelope([]openaichat.Choice{
			{Index: 0, FinishReason: &finish, Delta: &openaichat.StreamDelta{Refusal: new("no")}},
		}),
		"tool call payload": envelope([]openaichat.Choice{
			{Index: 0, FinishReason: &finish, Delta: &openaichat.StreamDelta{ToolCalls: []openaichat.ToolCallDelta{{Index: new(0), ID: new("call-1")}}}},
		}),
		"second choice": envelope([]openaichat.Choice{
			{Index: 0, FinishReason: &finish},
			{Index: 1, FinishReason: &finish},
		}),
	}
	for name, chunk := range cases {
		t.Run(name, func(t *testing.T) {
			state := newChatResponsesStreamState(
				testStreamContext(),
				permissiveLossPolicy(),
				ChatCapabilities{},
				"resp_1",
				"m",
				1710000000,
				nil,
			)
			seed := ChatStreamResponse{
				ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
				Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{Content: new("x")}}},
			}
			if _, err := state.Convert(seed); err != nil {
				t.Fatal(err)
			}
			done := ChatStreamResponse{
				ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
				Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{}, FinishReason: &finish}},
			}
			if _, err := state.Convert(done); err != nil {
				t.Fatal(err)
			}
			if _, err := state.Convert(chunk); err == nil {
				t.Fatalf("substantive post-finish chunk accepted")
			} else if !strings.Contains(err.Error(), "after finish_reason") {
				t.Fatalf("wrong error: %v", err)
			}
		})
	}
}

// A redelivery whose identity contradicts the stream is corrupt wire, even
// when its delta is insubstantial.
func TestStreamRepeatedTerminalChunkRejectsIdentityMismatch(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		permissiveLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1710000000,
		nil,
	)
	finish := "stop"
	seed := ChatStreamResponse{
		ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
		Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{Content: new("x")}}},
	}
	if _, err := state.Convert(seed); err != nil {
		t.Fatal(err)
	}
	done := ChatStreamResponse{
		ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
		Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{}, FinishReason: &finish}},
	}
	if _, err := state.Convert(done); err != nil {
		t.Fatal(err)
	}
	mismatch := ChatStreamResponse{
		ID: "other", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
		Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{}, FinishReason: &finish}},
	}
	if _, err := state.Convert(mismatch); err == nil {
		t.Fatal("identity-mismatched redelivery accepted")
	}
}

// Under the strict policy a redelivered frame carrying a loss-gated
// extension (unknown usage breakdown) still fails, and under a permissive
// policy the loss is recorded exactly once.
func TestStreamRepeatedTerminalChunkLossPolicy(t *testing.T) {
	stream := func(state *chatResponsesStreamState, usage *openaichat.LLMUsage) error {
		finish := "stop"
		seed := ChatStreamResponse{
			ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
			Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{Content: new("x")}}},
		}
		if _, err := state.Convert(seed); err != nil {
			return err
		}
		done := ChatStreamResponse{
			ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
			Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{}, FinishReason: &finish}},
		}
		if _, err := state.Convert(done); err != nil {
			return err
		}
		repeat := ChatStreamResponse{
			ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
			Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{}, FinishReason: &finish}},
			Usage:   usage,
		}
		_, err := state.Convert(repeat)
		return err
	}

	strict := newChatResponsesStreamState(
		testStreamContext(), StrictLossPolicy(), ChatCapabilities{}, "resp_1", "m", 1710000000, nil,
	)
	if err := stream(strict, &openaichat.LLMUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}); err == nil {
		t.Fatal("strict policy accepted a redelivered usage without the required breakdown")
	}

	relaxed := newChatResponsesStreamState(
		testStreamContext(), permissiveLossPolicy(), ChatCapabilities{}, "resp_1", "m", 1710000000, nil,
	)
	if err := stream(relaxed, &openaichat.LLMUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}); err != nil {
		t.Fatalf("permissive policy rejected the redelivery: %v", err)
	}
	count := 0
	for _, loss := range relaxed.report.Losses {
		if loss.Feature == FeatureUsageCacheReadUnknown {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("cache-read loss recorded %d times, want exactly once", count)
	}
}

// An insubstantial same-finish repeat folds usage and emits nothing.
func TestStreamRepeatedTerminalChunkFoldsUsage(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		permissiveLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1710000000,
		nil,
	)
	finish := "stop"
	seed := ChatStreamResponse{
		ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
		Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{Content: new("x")}}},
	}
	if _, err := state.Convert(seed); err != nil {
		t.Fatal(err)
	}
	done := ChatStreamResponse{
		ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
		Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{}, FinishReason: &finish}},
	}
	if _, err := state.Convert(done); err != nil {
		t.Fatal(err)
	}
	repeat := ChatStreamResponse{
		ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
		Choices: []openaichat.Choice{{
			Index:        0,
			Delta:        &openaichat.StreamDelta{Role: new("assistant"), Content: new("")},
			FinishReason: &finish,
		}},
		Usage: &openaichat.LLMUsage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12},
	}
	events, err := state.Convert(repeat)
	if err != nil {
		t.Fatalf("insubstantial repeat rejected: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("repeat emitted %d events, want none", len(events))
	}
	if state.usage == nil || state.usage.TotalTokens != 12 {
		t.Fatalf("usage not folded: %+v", state.usage)
	}
}

// repeatedTerminalWithUsage builds the insubstantial post-finish redelivery
// that carries usage accounting, the shape this file is about.
func repeatedTerminalWithUsage(
	finish string,
	prompt, completion, total int,
	cached *int,
) ChatStreamResponse {
	usage := &openaichat.LLMUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
	}
	if cached != nil {
		usage.PromptTokensDetails = &openaichat.PromptTokensDetails{CachedTokens: *cached}
	}
	return ChatStreamResponse{
		ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
		Choices: []openaichat.Choice{{
			Index:        0,
			Delta:        &openaichat.StreamDelta{Role: new("assistant"), Content: new("")},
			FinishReason: &finish,
		}},
		Usage: usage,
	}
}

// finishedChatStreamState returns a state that has already seen a finish chunk,
// so the next chunk lands in the accounting-redelivery phase.
func finishedChatStreamState(t *testing.T) *chatResponsesStreamState {
	t.Helper()
	state := newChatResponsesStreamState(
		testStreamContext(),
		permissiveLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1710000000,
		nil,
	)
	seed := ChatStreamResponse{
		ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
		Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{Content: new("x")}}},
	}
	if _, err := state.Convert(seed); err != nil {
		t.Fatal(err)
	}
	done := ChatStreamResponse{
		ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
		Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{}, FinishReason: new("stop")}},
	}
	if _, err := state.Convert(done); err != nil {
		t.Fatal(err)
	}
	return state
}

// TestStreamRepeatedTerminalMergeNotedOncePerStream proves the
// usage_total_merged note is gated to once per exchange. The event is binary —
// a redelivery replaced the recorded accounting, or it did not — so an
// ungated note let a gateway that repeats the terminal many times push real
// entries out of a saturated report (4096) for a single exchange.
func TestStreamRepeatedTerminalMergeNotedOncePerStream(t *testing.T) {
	state := finishedChatStreamState(t)
	// The first redelivery establishes the accounting.
	if _, err := state.Convert(repeatedTerminalWithUsage("stop", 100, 50, 150, nil)); err != nil {
		t.Fatal(err)
	}
	// Four more redeliveries each carry DIFFERENT totals, so each one replaces
	// the recorded accounting. Every one of them is a merge; the report must
	// still name the event once.
	for i := range 4 {
		if _, err := state.Convert(repeatedTerminalWithUsage("stop", 200+i, 60+i, 260+2*i, nil)); err != nil {
			t.Fatalf("redelivery %d rejected: %v", i, err)
		}
	}
	if got := countFeature(state.report, FeatureUsageTotalMerged); got != 1 {
		t.Fatalf("usage_total_merged recorded %d times, want exactly 1", got)
	}
	// The last redelivery still wins: the gate is on the note, not the merge.
	if state.usage == nil || state.usage.TotalTokens != 266 {
		t.Fatalf("usage = %+v, want the last redelivery's total 266", state.usage)
	}
}

// TestStreamRepeatedTerminalBreakdownOnlyReplacementIsNoted proves the merge
// comparison covers the breakdown counts, not just the three totals. A
// redelivery that keeps the totals but zeroes cached_tokens has still replaced
// a client-observable value, and the rationale for the note applies to it
// exactly as it does to the totals.
func TestStreamRepeatedTerminalBreakdownOnlyReplacementIsNoted(t *testing.T) {
	state := finishedChatStreamState(t)
	cached := 3
	if _, err := state.Convert(repeatedTerminalWithUsage("stop", 10, 5, 15, &cached)); err != nil {
		t.Fatal(err)
	}
	if got := countFeature(state.report, FeatureUsageTotalMerged); got != 0 {
		t.Fatalf("the first accounting recorded %d merges, want 0", got)
	}
	// Identical totals, cached_tokens dropped from 3 to 0: a silent
	// replacement of a client-observable value.
	if _, err := state.Convert(repeatedTerminalWithUsage("stop", 10, 5, 15, new(0))); err != nil {
		t.Fatal(err)
	}
	if got := countFeature(state.report, FeatureUsageTotalMerged); got != 1 {
		t.Fatalf(
			"a breakdown-only replacement recorded %d merges, want 1: %+v",
			got,
			state.report,
		)
	}
}

// TestStreamRepeatedTerminalIdenticalRepeatStaysQuiet proves the comparison
// does not fire on a benign exact redelivery — the note must mean something.
func TestStreamRepeatedTerminalIdenticalRepeatStaysQuiet(t *testing.T) {
	state := finishedChatStreamState(t)
	cached := 3
	if _, err := state.Convert(repeatedTerminalWithUsage("stop", 10, 5, 15, &cached)); err != nil {
		t.Fatal(err)
	}
	again := 3
	if _, err := state.Convert(repeatedTerminalWithUsage("stop", 10, 5, 15, &again)); err != nil {
		t.Fatal(err)
	}
	if got := countFeature(state.report, FeatureUsageTotalMerged); got != 0 {
		t.Fatalf("an identical redelivery recorded %d merges, want 0", got)
	}
}
