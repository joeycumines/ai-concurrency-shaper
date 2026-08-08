package transcode

// J5 regression tests: the Chat stream lifecycle (review-j finding 6) —
// header-derived stream intent is written back into the rendered request,
// include_usage is requested, chunk identity and creation time are pinned
// before the first client event, the usage-only tail chunk is consumed into
// the terminal envelope, and output_item.added never invents complete
// arguments.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// TestAcceptIsEventStream pins the media-range parsing of the Accept header.
func TestAcceptIsEventStream(t *testing.T) {
	tests := []struct {
		accept string
		want   bool
	}{
		{"text/event-stream", true},
		{"text/event-stream, application/json", true},
		{"application/json, text/event-stream", false},
		{"text/event-streaming", false},
		{"TEXT/EVENT-STREAM", true},
		{"", false},
		{"application/notjson", false},
		{"text/event-stream; charset=utf-8", true},
		// The first parseable range decides: a garbage first range is not
		// an SSE preference.
		{"bogus, text/event-stream", false},
	}
	for _, tt := range tests {
		if got := acceptIsEventStream(tt.accept); got != tt.want {
			t.Errorf("acceptIsEventStream(%q) = %v, want %v", tt.accept, got, tt.want)
		}
	}
}

// TestChatStreamIntentWrittenBack proves the merged stream intent is written
// back into the canonical request so the upstream renderer emits stream:true
// — an Accept-only stream request must not ask the upstream for JSON
// (review-j finding 6).
func TestChatStreamIntentWrittenBack(t *testing.T) {
	result, echo, err := DecodeResponsesRequest(
		[]byte(`{"model":"m","input":"x"}`),
		StrictLossPolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Request.Stream {
		t.Fatal("decoded request must not be streaming")
	}
	context := &ExchangeContext{
		IDs:                      NewExchangeIDs(),
		LossPolicy:               StrictLossPolicy(),
		StreamIntent:             true, // from Accept: text/event-stream
		OriginalResponsesRequest: echo,
	}
	// The handler merges the Accept-derived intent into the canonical
	// request before rendering; replicate that write-back here.
	context.StreamIntent = context.StreamIntent || result.Request.Stream
	result.Request.Stream = context.StreamIntent
	rendered, _, err := RenderChatRequest(result.Request, context, ChatCapabilities{ParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Stream        *bool `json:"stream"`
		StreamOptions *struct {
			IncludeUsage *bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(rendered, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Stream == nil || !*probe.Stream {
		t.Fatalf("rendered request stream = %v, want true: %s", probe.Stream, rendered)
	}
	if probe.StreamOptions == nil || probe.StreamOptions.IncludeUsage == nil || !*probe.StreamOptions.IncludeUsage {
		t.Fatalf("rendered request include_usage = %v, want true: %s", probe.StreamOptions, rendered)
	}
}

// TestChatStreamUsageTailProducesRealTotals proves the usage-only tail chunk
// after the finish reason updates the terminal envelope's usage — no
// fabricated zero usage and no rejection of the tail (review-j finding 6).
func TestChatStreamUsageTailProducesRealTotals(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: str("hi")}, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{}, str("stop"))); err != nil {
		t.Fatal(err)
	}

	// The usage-only tail: choices empty, usage present. It must be accepted
	// and folded into the terminal.
	tail := ChatStreamResponse{
		ID:      "chatcmpl-1",
		Object:  "chat.completion.chunk",
		Created: 1710000000,
		Model:   "gpt-4.1",
		Choices: []ChatChoice{},
		Usage: &ChatLLMUsage{
			PromptTokens:     42,
			CompletionTokens: 18,
			TotalTokens:      60,
			PromptTokensDetails: &ChatPromptTokensDetails{
				CachedTokens: 5,
			},
			CompletionTokensDetails: &ChatCompletionTokensDetails{
				ReasoningTokens: 12,
			},
		},
	}
	if _, err := state.Convert(tail); err != nil {
		t.Fatalf("usage tail rejected: %v", err)
	}

	held, ok := state.releaseTerminal()
	if !ok {
		t.Fatal("no held terminal")
	}
	terminal := held[len(held)-1]
	completed, ok := terminal.(ResponseCompletedEvent)
	if !ok {
		t.Fatalf("terminal = %T", terminal)
	}
	if completed.Response.Usage == nil {
		t.Fatal("terminal usage is nil")
	}
	if completed.Response.Usage.TotalTokens != 60 ||
		completed.Response.Usage.InputTokens != 42 ||
		completed.Response.Usage.OutputTokens != 18 {
		t.Fatalf("terminal usage = %+v, want real totals", completed.Response.Usage)
	}
	if completed.Response.Usage.InputTokensDetails == nil ||
		completed.Response.Usage.InputTokensDetails.CachedTokens != 5 {
		t.Fatalf("cached tokens = %+v", completed.Response.Usage.InputTokensDetails)
	}
	if completed.Response.Usage.OutputTokensDetails == nil ||
		completed.Response.Usage.OutputTokensDetails.ReasoningTokens != 12 {
		t.Fatalf("reasoning tokens = %+v", completed.Response.Usage.OutputTokensDetails)
	}
}

// TestChatStreamFixtureUsageTailEndToEnd proves the official-shaped fixture
// (finish chunk, then a usage-only tail chunk, then [DONE]) converts through
// the full reader with the real totals folded into the terminal envelope.
func TestChatStreamFixtureUsageTailEndToEnd(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	reader := newConvertingReader(
		NewSSEReaderWithLimits(bytes.NewReader(testcorpus.ChatCompletionsStreamSSE()), 0, 0),
		converter,
	)
	var output bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if !reader.SawTerminal() {
		t.Fatal("no terminal seen")
	}
	body := output.String()
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("missing terminal: %q", body)
	}
	// The usage tail's totals must be folded into the terminal envelope —
	// not fabricated zeros and not omitted.
	if !strings.Contains(body, `"total_tokens":60`) {
		t.Fatalf("terminal usage missing real totals: %q", body)
	}
	if !strings.Contains(body, `"cached_tokens":5`) {
		t.Fatalf("terminal usage missing cached tokens: %q", body)
	}
	if !strings.Contains(body, `"reasoning_tokens":12`) {
		t.Fatalf("terminal usage missing reasoning tokens: %q", body)
	}
}

// TestChatStreamCreatedAtConsistent proves response.created,
// response.in_progress, and the terminal envelope share one created_at taken
// from the first chunk BEFORE the first client event (review-j finding 6).
func TestChatStreamCreatedAtConsistent(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"gpt-4.1",
		999, // the handler's placeholder; the first chunk's created wins
		nil,
	)
	events, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: str("hi")}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("first batch = %d events", len(events))
	}
	created, ok := events[0].(ResponseCreatedEvent)
	if !ok {
		t.Fatalf("event 0 = %T", events[0])
	}
	inProgress, ok := events[1].(ResponseInProgressEvent)
	if !ok {
		t.Fatalf("event 1 = %T", events[1])
	}
	if created.Response.CreatedAt != 1710000000 || inProgress.Response.CreatedAt != 1710000000 {
		t.Fatalf("created_at = %d/%d, want 1710000000",
			created.Response.CreatedAt, inProgress.Response.CreatedAt)
	}
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{}, str("stop"))); err != nil {
		t.Fatal(err)
	}
	held, ok := state.releaseTerminal()
	if !ok {
		t.Fatal("no held terminal")
	}
	completed, ok := held[len(held)-1].(ResponseCompletedEvent)
	if !ok {
		t.Fatalf("terminal = %T", held[len(held)-1])
	}
	if completed.Response.CreatedAt != 1710000000 {
		t.Fatalf("terminal created_at = %d, want 1710000000", completed.Response.CreatedAt)
	}
}

// TestChatStreamOutputItemAddedEmptyArguments proves output_item.added never
// invents complete arguments ("{}") — the added event carries an empty
// argument string and the real arguments arrive through deltas and the done
// event (review-j finding 6).
func TestChatStreamOutputItemAddedEmptyArguments(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	events, err := state.Convert(chatChunk(t, ChatStreamDelta{
		ToolCalls: []ChatToolCallDelta{{
			Index: intPtr(0),
			ID:    str("call_1"),
			Function: ChatToolCallFunction{
				Name:      str("get_weather"),
				Arguments: `{"city":"`,
			},
		}},
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	var added ResponseOutputItemAddedEvent
	found := false
	for _, event := range events {
		if candidate, ok := event.(ResponseOutputItemAddedEvent); ok {
			added = candidate
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no output_item.added in batch: %+v", events)
	}
	call, ok := added.Item.(*ResponsesFunctionCallOutputItem)
	if !ok {
		t.Fatalf("item = %T", added.Item)
	}
	if call.Arguments != "" {
		t.Fatalf("output_item.added arguments = %q, want empty", call.Arguments)
	}
}

// TestChatStreamComposedAnthropicUsageTail proves the composed Chat→Anthropic
// direction: a usage-only tail chunk is folded into message_delta's usage —
// real totals, never fabricated zeros (review-j finding 6).
func TestChatStreamComposedAnthropicUsageTail(t *testing.T) {
	chat := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	anthropic := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		"msg_1",
		"claude-x",
		1710000000,
	)
	converter := newChatToAnthropicConverter(chat, anthropic)

	convert := func(raw string) error {
		batch, err := converter.Convert(SSEEvent{Data: []byte(raw)})
		if err != nil {
			return err
		}
		_ = batch
		return nil
	}

	if err := convert(`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`); err != nil {
		t.Fatal(err)
	}
	if err := convert(`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`); err != nil {
		t.Fatal(err)
	}
	// The usage-only tail.
	if err := convert(`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[],"usage":{"prompt_tokens":42,"completion_tokens":18,"total_tokens":60,"prompt_tokens_details":{"cached_tokens":5},"completion_tokens_details":{"reasoning_tokens":12}}}`); err != nil {
		t.Fatalf("usage tail rejected: %v", err)
	}

	batch, err := converter.Convert(SSEEvent{Data: []byte("[DONE]")})
	if err != nil {
		t.Fatal(err)
	}
	var delta *AnthropicStreamEvent
	for _, frame := range batch.Events {
		var event AnthropicStreamEvent
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == AnthropicStreamEventTypeMessageDelta {
			delta = &event
		}
	}
	if delta == nil {
		t.Fatal("no message_delta in the terminal batch")
	}
	if delta.Usage == nil {
		t.Fatal("message_delta usage is nil")
	}
	// Anthropic semantics: input_tokens + cache_read = total (42); the
	// uncached input is 42 - 5.
	if delta.Usage.InputTokens != 37 || delta.Usage.OutputTokens != 18 {
		t.Fatalf("message_delta usage = %+v, want uncached totals", delta.Usage)
	}
	if delta.Usage.CacheReadInputTokens != 5 {
		t.Fatalf("message_delta cache_read = %d, want 5", delta.Usage.CacheReadInputTokens)
	}
	if delta.Usage.OutputTokensDetails == nil || delta.Usage.OutputTokensDetails.ThinkingTokens != 12 {
		t.Fatalf("message_delta thinking tokens = %+v, want 12", delta.Usage.OutputTokensDetails)
	}
}

// TestChatStreamChoiceIndexAndIdentityEnforced proves a choice index != 0 and
// a mismatched chunk id/model are upstream protocol errors (review-j finding
// 6).
func TestChatStreamChoiceIndexAndIdentityEnforced(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	chunk := chatChunk(t, ChatStreamDelta{Content: str("x")}, nil)
	chunk.Choices[0].Index = 1
	if _, err := state.Convert(chunk); err == nil {
		t.Fatal("choice index 1 accepted")
	}

	state = newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: str("x")}, nil)); err != nil {
		t.Fatal(err)
	}
	mismatchedID := chatChunk(t, ChatStreamDelta{Content: str("y")}, nil)
	mismatchedID.ID = "chatcmpl-other"
	if _, err := state.Convert(mismatchedID); err == nil {
		t.Fatal("chunk id mismatch accepted")
	}

	state = newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: str("x")}, nil)); err != nil {
		t.Fatal(err)
	}
	mismatchedModel := chatChunk(t, ChatStreamDelta{Content: str("y")}, nil)
	mismatchedModel.Model = "gpt-other"
	if _, err := state.Convert(mismatchedModel); err == nil {
		t.Fatal("chunk model mismatch accepted")
	}
}

// j6PermissivePolicy returns a policy approving the response-side losses the
// Responses->Messages path triggers (reasoning and usage timing).
func j6PermissivePolicy() LossPolicy {
	return LossPolicy{Allowed: map[Feature]struct{}{
		FeatureReasoningSummary: {},
		FeatureUsageTiming:      {},
	}}
}
