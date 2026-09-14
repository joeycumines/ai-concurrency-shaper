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
			{Index: 0, FinishReason: repTermStrptr("stop")},
		}),
		"content payload": envelope([]openaichat.Choice{
			{Index: 0, FinishReason: &finish, Delta: &openaichat.StreamDelta{Content: repTermStrptr("late")}},
		}),
		"reasoning payload": envelope([]openaichat.Choice{
			{Index: 0, FinishReason: &finish, Delta: &openaichat.StreamDelta{Reasoning: repTermStrptr("late")}},
		}),
		"reasoning content payload": envelope([]openaichat.Choice{
			{Index: 0, FinishReason: &finish, Delta: &openaichat.StreamDelta{ReasoningContent: repTermStrptr("late")}},
		}),
		"refusal payload": envelope([]openaichat.Choice{
			{Index: 0, FinishReason: &finish, Delta: &openaichat.StreamDelta{Refusal: repTermStrptr("no")}},
		}),
		"tool call payload": envelope([]openaichat.Choice{
			{Index: 0, FinishReason: &finish, Delta: &openaichat.StreamDelta{ToolCalls: []openaichat.ToolCallDelta{{Index: repTermIntptr(0), ID: repTermStrptr("call-1")}}}},
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
				j6PermissivePolicy(),
				ChatCapabilities{},
				"resp_1",
				"m",
				1710000000,
				nil,
			)
			seed := ChatStreamResponse{
				ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
				Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{Content: repTermStrptr("x")}}},
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
		j6PermissivePolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1710000000,
		nil,
	)
	finish := "stop"
	seed := ChatStreamResponse{
		ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
		Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{Content: repTermStrptr("x")}}},
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
			Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{Content: repTermStrptr("x")}}},
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
		testStreamContext(), j6PermissivePolicy(), ChatCapabilities{}, "resp_1", "m", 1710000000, nil,
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
		j6PermissivePolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1710000000,
		nil,
	)
	finish := "stop"
	seed := ChatStreamResponse{
		ID: "c", Object: "chat.completion.chunk", Created: 1710000000, Model: "m",
		Choices: []openaichat.Choice{{Index: 0, Delta: &openaichat.StreamDelta{Content: repTermStrptr("x")}}},
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
			Delta:        &openaichat.StreamDelta{Role: repTermStrptr("assistant"), Content: repTermStrptr("")},
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

func repTermStrptr(s string) *string { return &s }

func repTermIntptr(v int) *int { return &v }
