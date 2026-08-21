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
		// A range that matches neither representation is ignored, never a
		// veto: text/event-stream still applies after it.
		{"bogus, text/event-stream", true},
	}
	for _, tt := range tests {
		if got := acceptIsEventStream(tt.accept); got != tt.want {
			t.Errorf("acceptIsEventStream(%q) = %v, want %v", tt.accept, got, tt.want)
		}
	}
}

// TestAcceptStreamSelectionQualityAndOrdering pins the RFC 9110 media-range
// selection of the Accept header (review-08 blocker 1): every range is
// evaluated with its quality value, q=0 excludes a representation, and the
// client's most-preferred acceptable representation between text/event-stream
// and application/json wins — by effective quality, then specificity, then
// order of appearance.
func TestAcceptStreamSelectionQualityAndOrdering(t *testing.T) {
	tests := []struct {
		name   string
		accept string
		want   bool
	}{
		// The review-08 examples: the first parseable range must not decide,
		// and q=0 must be honored.
		{"json-then-sse", "application/json, text/event-stream", false},
		{"sse-q0-then-json", "text/event-stream;q=0, application/json", false},
		{"json-q01-sse-q1", "application/json;q=0.1, text/event-stream;q=1", true},
		// q=0 excludes a representation entirely, even against */*.
		{"sse-q0-only", "text/event-stream;q=0", false},
		{"both-q0", "text/event-stream;q=0, application/json;q=0", false},
		{"sse-q0-beats-star", "text/event-stream;q=0, */*;q=1", false},
		// Excluding one representation selects the other: an unmentioned
		// representation is acceptable by default (RFC 9110 §12.5.1), so
		// application/json;q=0 must never deliver JSON.
		{"json-q0-only", "application/json;q=0", true},
		{"json-q0-json-star", "application/json;q=0, application/*;q=0.1", true},
		{"star-q0-only", "*/*;q=0", false},
		{"wildcard-excluded-sse-wins", "*/*;q=0, text/event-stream;q=1", true},
		// Equal quality: the more specific applicable range wins.
		{"star-then-sse", "*/*;q=1, text/event-stream;q=1", true},
		{"text-star-vs-json-exact", "text/*;q=1, application/json;q=1", false},
		{"text-star-only", "text/*;q=1", true},
		// Equal quality and specificity: the first-listed range wins.
		{"sse-then-json-equal", "text/event-stream;q=0.5, application/json;q=0.5", true},
		{"json-then-sse-equal", "application/json;q=0.5, text/event-stream;q=0.5", false},
		// A lone */* is a full tie with no order information: the
		// non-streaming default applies.
		{"star-only", "*/*", false},
		// Among equally specific ranges the highest q applies.
		{"sse-tier-max-q", "text/event-stream;q=0.4, application/json;q=0.4, text/event-stream;q=0.9", true},
		// Malformed ranges are ignored: they never decide and never disturb
		// the ordering of the ranges around them.
		{"non-matching-first", "bogus, text/event-stream", true},
		{"non-matching-mid", "application/json, bogus, text/event-stream", false},
		{"malformed-q", "text/event-stream;q=abc, application/json", false},
		{"q-out-of-range", "text/event-stream;q=2", false},
		// NaN parses as a float but is outside the documented 0..1 q
		// contract; the range is skipped like any other malformed q.
		{"q-nan-skipped", "text/event-stream;q=NaN, application/json;q=NaN, text/event-stream;q=1", true},
		// A comma inside a quoted parameter value is part of the range, not
		// a range separator (RFC 9110 quoted-string); the range survives.
		{"quoted-comma-sse", `text/event-stream;note="a,b", application/json`, true},
		{"quoted-comma-json", `application/json;note="a,b", text/event-stream`, false},
		{"quoted-comma-escaped", `text/event-stream;note="a\,b", application/json`, true},
		// Media types and parameter names are case-insensitive.
		{"case-insensitive", "TEXT/EVENT-STREAM;Q=0.5, APPLICATION/JSON;Q=0.5", true},
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
	// The handler applies the documented stream-intent precedence: the
	// request body's stream field, when explicitly present, is authoritative;
	// here it is absent, so the Accept-derived intent stands. Replicate the
	// handler's merge and write-back.
	if result.StreamSet {
		context.StreamIntent = result.Request.Stream
	}
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
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: new("hi")}, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{}, new("stop"))); err != nil {
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
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	converter := newChatToResponsesConverter(state)
	reader := newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(bytes.NewReader(testcorpus.ChatCompletionsStreamSSE()), 0, 0),
		converter, 0, 0, 0,
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
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		999, // the handler's placeholder; the first chunk's created wins
		nil,
	)
	events, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: new("hi")}, nil))
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
		t.Fatalf("created_at = %v/%v, want 1710000000",
			created.Response.CreatedAt, inProgress.Response.CreatedAt)
	}
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{}, new("stop"))); err != nil {
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
		t.Fatalf("terminal created_at = %v, want 1710000000", completed.Response.CreatedAt)
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
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	events, err := state.Convert(chatChunk(t, ChatStreamDelta{
		ToolCalls: []ChatToolCallDelta{{
			Index: new(0),
			ID:    new("call_1"),
			Function: ChatToolCallFunction{
				Name:      new("get_weather"),
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
		ChatCapabilities{},
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
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	chunk := chatChunk(t, ChatStreamDelta{Content: new("x")}, nil)
	chunk.Choices[0].Index = 1
	if _, err := state.Convert(chunk); err == nil {
		t.Fatal("choice index 1 accepted")
	}

	state = newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: new("x")}, nil)); err != nil {
		t.Fatal(err)
	}
	mismatchedID := chatChunk(t, ChatStreamDelta{Content: new("y")}, nil)
	mismatchedID.ID = "chatcmpl-other"
	if _, err := state.Convert(mismatchedID); err == nil {
		t.Fatal("chunk id mismatch accepted")
	}

	state = newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"gpt-4.1",
		1710000000,
		nil,
	)
	if _, err := state.Convert(chatChunk(t, ChatStreamDelta{Content: new("x")}, nil)); err != nil {
		t.Fatal(err)
	}
	mismatchedModel := chatChunk(t, ChatStreamDelta{Content: new("y")}, nil)
	mismatchedModel.Model = "gpt-other"
	if _, err := state.Convert(mismatchedModel); err == nil {
		t.Fatal("chunk model mismatch accepted")
	}
}

// j6PermissivePolicy returns a policy approving the response-side losses the
// Responses->Messages path triggers (reasoning and usage timing) plus the
// tool strictness loss a messages->responses mapping requires to serve tool
// traffic (review-z commit 6).
func j6PermissivePolicy() LossPolicy {
	return LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolSchemaStrictness:   {},
		FeatureReasoningSummary:       {},
		FeatureUsageUnknown:           {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
}

// TestChatStreamComposedCreatedCacheTokens pins the stream-path parity of
// the created_cache_tokens provider extension with the non-streaming decode
// (review-gate task-11 finding 5): a usage tail carrying it renders
// cache_creation_input_tokens with the value and records NO
// usage_cache_write_unknown loss (the component is known); a tail without it
// keeps the loss-gated zero path with exactly one recorded loss. The
// uncached input is the total minus the full cached breakdown
// (cached + created), matching Messages semantics.
func TestChatStreamComposedCreatedCacheTokens(t *testing.T) {
	run := func(t *testing.T, usageTail string) (*chatToAnthropicConverter, *AnthropicStreamEvent) {
		t.Helper()
		chat := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{},
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
			_, err := converter.Convert(SSEEvent{Data: []byte(raw)})
			return err
		}
		if err := convert(`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`); err != nil {
			t.Fatal(err)
		}
		if err := convert(`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`); err != nil {
			t.Fatal(err)
		}
		if err := convert(usageTail); err != nil {
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
		return converter, delta
	}

	t.Run("extension present", func(t *testing.T) {
		converter, delta := run(t,
			`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[],"usage":{"prompt_tokens":42,"completion_tokens":18,"total_tokens":60,"prompt_tokens_details":{"cached_tokens":5,"created_cache_tokens":7},"completion_tokens_details":{"reasoning_tokens":12}}}`)
		if delta.Usage == nil {
			t.Fatal("message_delta usage is nil")
		}
		if delta.Usage.CacheCreationInputTokens != 7 {
			t.Fatalf("cache_creation_input_tokens = %d, want 7", delta.Usage.CacheCreationInputTokens)
		}
		if delta.Usage.CacheReadInputTokens != 5 {
			t.Fatalf("cache_read_input_tokens = %d, want 5", delta.Usage.CacheReadInputTokens)
		}
		// Uncached input: 42 - 5 (cached) - 7 (created).
		if delta.Usage.InputTokens != 30 {
			t.Fatalf("input_tokens = %d, want 30", delta.Usage.InputTokens)
		}
		if got := countFeature(*converter.ConversionReport(), FeatureUsageCacheWriteUnknown); got != 0 {
			t.Fatalf("usage_cache_write_unknown losses = %d, want 0 (the component is known)", got)
		}
	})
	t.Run("extension absent", func(t *testing.T) {
		converter, delta := run(t,
			`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4.1","choices":[],"usage":{"prompt_tokens":42,"completion_tokens":18,"total_tokens":60,"prompt_tokens_details":{"cached_tokens":5},"completion_tokens_details":{"reasoning_tokens":12}}}`)
		if delta.Usage == nil {
			t.Fatal("message_delta usage is nil")
		}
		if delta.Usage.CacheCreationInputTokens != 0 {
			t.Fatalf("cache_creation_input_tokens = %d, want 0", delta.Usage.CacheCreationInputTokens)
		}
		if got := countFeature(*converter.ConversionReport(), FeatureUsageCacheWriteUnknown); got != 1 {
			t.Fatalf("usage_cache_write_unknown losses = %d, want exactly 1", got)
		}
	})
}
