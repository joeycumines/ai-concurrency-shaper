package transcode

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/geminigeneratecontent"
)

// geminiStreamTestContext builds a minimal exchange context for the Gemini
// upstream stream state.
func geminiStreamTestContext() *ExchangeContext {
	return &ExchangeContext{
		RequestedClientModel: "gemini-3-pro-preview",
		UpstreamModel:        "gemini-3-pro-preview",
		IDs:                  NewExchangeIDs(),
		LossPolicy: LossPolicy{Allowed: map[Feature]struct{}{
			FeatureProviderReasoningText:          {},
			FeatureGeminiSafetyMetadata:           {},
			FeatureGeminiGroundingMetadata:        {},
			FeatureGeminiPartMetadata:             {},
			FeatureGeminiResponseThoughtSignature: {},
			FeatureGeminiUsageBreakdown:           {},
			FeatureGeminiModelStatus:              {},
			FeatureLogprobs:                       {},
			FeatureResponseServiceTier:            {},
			FeatureGeminiPromptBlocked:            {},
		}},
	}
}

func geminiChunk(t *testing.T, body string) geminigeneratecontent.GenerateContentResponse {
	t.Helper()
	chunk, err := geminigeneratecontent.DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	return chunk
}

func newGeminiStreamState(t *testing.T, ctx *ExchangeContext) *geminiResponsesStreamState {
	t.Helper()
	if ctx == nil {
		ctx = geminiStreamTestContext()
	}
	return newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_test", "gemini-3-pro-preview", 1700000000, nil)
}

// collectResponses runs chunks through the state and returns the flattened
// events plus the EOF result.
func collectResponses(t *testing.T, s *geminiResponsesStreamState, chunks ...string) ([]ResponsesSSEEvent, []ResponsesSSEEvent, error) {
	t.Helper()
	var all []ResponsesSSEEvent
	for _, c := range chunks {
		events, err := s.Convert(geminiChunk(t, c))
		if err != nil {
			return all, nil, err
		}
		all = append(all, events...)
	}
	final, err := s.FinalizeEOF()
	return all, final, err
}

func geminiStreamEventTypes(events []ResponsesSSEEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.EventType())
	}
	return out
}

func TestGeminiUpstreamStreamTextThenFinishThenUsage(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	events, final, err := collectResponses(t,
		s,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"index":0}]}`,
		`{"candidates":[{"content":{"parts":[{"text":", world"}]},"index":0}]}`,
		`{"candidates":[{"finishReason":"STOP","index":0}]}`,
		`{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}`,
	)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	all := append(append([]ResponsesSSEEvent{}, events...), final...)
	types := geminiStreamEventTypes(all)
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done", "response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	// The terminal envelope must carry the trailing usage (last chunk wins).
	last := all[len(all)-1]
	completed, ok := last.(interface {
		GetResponse() ResponseEnvelope
	})
	_ = completed
	_ = ok
	data, err := MarshalResponsesEvent(last)
	if err != nil {
		t.Fatalf("marshal terminal: %v", err)
	}
	var envelope struct {
		Type     string `json:"type"`
		Response struct {
			Status string          `json:"status"`
			Usage  *ResponsesUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal terminal: %v", err)
	}
	if envelope.Type != "response.completed" || envelope.Response.Status != "completed" {
		t.Fatalf("terminal = %s/%s", envelope.Type, envelope.Response.Status)
	}
	if envelope.Response.Usage == nil || envelope.Response.Usage.TotalTokens != 7 ||
		envelope.Response.Usage.InputTokens != 5 || envelope.Response.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", envelope.Response.Usage)
	}
	// Exactly one success terminal.
	terminals := 0
	for _, e := range all {
		if e.EventType() == "response.completed" || e.EventType() == "response.incomplete" || e.EventType() == "response.failed" || e.EventType() == "error" {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminals = %d, want 1", terminals)
	}
}

func TestGeminiUpstreamStreamEOFWithoutFinishIsTruncation(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	_, _, err := collectResponses(t, s,
		`{"candidates":[{"content":{"parts":[{"text":"partial"}]},"index":0}]}`,
	)
	if err == nil {
		t.Fatal("EOF without finish: nil error, want truncation")
	}
	if !strings.Contains(err.Error(), "before a terminal condition") {
		t.Fatalf("err = %v, want a truncation", err)
	}
}

func TestGeminiUpstreamStreamDoneSentinel(t *testing.T) {
	t.Run("after terminal releases", func(t *testing.T) {
		s := newGeminiStreamState(t, nil)
		c := newGeminiToResponsesConverter(s)
		batch, err := c.Convert(SSEEvent{Data: []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"index":0}]}`)})
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		if batch.Terminal {
			t.Fatal("text chunk is terminal")
		}
		batch, err = c.Convert(SSEEvent{Data: []byte(`{"candidates":[{"finishReason":"STOP","index":0}]}`)})
		if err != nil {
			t.Fatalf("finish chunk: %v", err)
		}
		if batch.Terminal {
			t.Fatal("finish chunk must hold the terminal until [DONE]/EOF")
		}
		batch, err = c.Convert(SSEEvent{Data: []byte(`[DONE]`)})
		if err != nil {
			t.Fatalf("[DONE]: %v", err)
		}
		if !batch.Terminal {
			t.Fatal("[DONE] after finish must release a terminal batch")
		}
		var sawCompleted bool
		for _, ev := range batch.Events {
			if string(ev.Type) == "response.completed" {
				sawCompleted = true
			}
		}
		if !sawCompleted {
			t.Fatalf("[DONE] batch = %v, want response.completed", batch.Events)
		}
	})
	t.Run("before terminal is a wire error", func(t *testing.T) {
		s := newGeminiStreamState(t, nil)
		c := newGeminiToResponsesConverter(s)
		_, err := c.Convert(SSEEvent{Data: []byte(`[DONE]`)})
		if err == nil {
			t.Fatal("[DONE] before any finish: nil error, want wire error")
		}
	})
	t.Run("named event frame is a wire error", func(t *testing.T) {
		s := newGeminiStreamState(t, nil)
		c := newGeminiToResponsesConverter(s)
		_, err := c.Convert(SSEEvent{Event: "message", Data: []byte(`{"candidates":[]}`)})
		if err == nil {
			t.Fatal("named event frame: nil error, want wire error")
		}
	})
}

func TestGeminiUpstreamStreamErrorFrame(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	_, err := s.Convert(geminiChunk(t, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota exceeded"}}`))
	if err == nil {
		t.Fatal("in-stream error frame: nil error, want upstream failure")
	}
	var streamErr *StreamConversionError
	if !asStreamConversionError(err, &streamErr) {
		t.Fatalf("err type %T, want *StreamConversionError", err)
	}
	if !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("err = %v", err)
	}
}

func asStreamConversionError(err error, out **StreamConversionError) bool {
	for err != nil {
		if sc, ok := err.(*StreamConversionError); ok {
			*out = sc
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestGeminiUpstreamStreamContentAfterFinish(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	_, err := s.Convert(geminiChunk(t, `{"candidates":[{"finishReason":"STOP","index":0}]}`))
	if err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	_, err = s.Convert(geminiChunk(t, `{"candidates":[{"content":{"parts":[{"text":"late"}]},"index":0}]}`))
	if err == nil {
		t.Fatal("content after finish: nil error, want wire error")
	}
}

func TestGeminiUpstreamStreamFunctionCallStoresSignature(t *testing.T) {
	ctx := geminiStreamTestContext()
	store := NewGeminiSignatureStore(GeminiSignatureConfig{Capacity: 16, TTL: time.Hour})
	ctx.GeminiSignatures = store
	ctx.GeminiSignatureScope = "gemini"
	s := newGeminiStreamState(t, ctx)
	events, err := s.Convert(geminiChunk(t,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call-1","name":"get_weather","args":{"city":"Paris"}},"thoughtSignature":"sig-abc"}]},"index":0}]}`))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	types := geminiStreamEventTypes(events)
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("types = %v, want %v", types, want)
	}
	if got, ok := store.Get("gemini", "call-1"); !ok || got != "sig-abc" {
		t.Fatalf("stored signature = %q, want sig-abc", got)
	}
}

func TestGeminiUpstreamStreamThoughtCapability(t *testing.T) {
	t.Run("reasoning capability renders summary deltas", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{ProviderReasoningThinking: true}, "resp_t", "m", 1, nil)
		_ = ctx
		events, _, err := collectResponses(t, s,
			`{"candidates":[{"content":{"parts":[{"text":"thinking","thought":true}]},"index":0}]}`,
			`{"candidates":[{"finishReason":"STOP","index":0}]}`,
		)
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		types := geminiStreamEventTypes(events)
		if !contains(types, "response.reasoning_summary_text.delta") {
			t.Fatalf("types = %v, want a reasoning summary delta", types)
		}
	})
	t.Run("no capability loses provider reasoning", func(t *testing.T) {
		// strict policy: provider_reasoning_text not approved -> rejection
		strict := geminiStreamTestContext()
		strict.LossPolicy = StrictLossPolicy()
		s := newGeminiResponsesStreamState(strict, strict.LossPolicy, ChatCapabilities{}, "resp_t", "m", 1, nil)
		_, err := s.Convert(geminiChunk(t, `{"candidates":[{"content":{"parts":[{"text":"thinking","thought":true}]},"index":0}]}`))
		if err == nil {
			t.Fatal("thought text under strict policy: nil error, want loss rejection")
		}
	})
}

// TestGeminiUpstreamStreamNullRawMemberIsNotAPayload proves an explicit JSON
// null on a raw-member payload field is not a payload, matching Part.Validate
// and the non-streaming decode: the text renders, and a bare null member
// part is a no-op rather than an unrepresentable refusal.
func TestGeminiUpstreamStreamNullRawMemberIsNotAPayload(t *testing.T) {
	t.Run("text with a null payload member renders", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_t", "m", 1, nil)
		events, err := s.Convert(geminiChunk(t,
			`{"candidates":[{"content":{"parts":[{"text":"hi","executableCode":null}]},"index":0}]}`,
		))
		if err != nil {
			t.Fatalf("text with null executableCode: %v", err)
		}
		types := geminiStreamEventTypes(events)
		if !contains(types, "response.output_text.delta") {
			t.Fatalf("types = %v, want a text delta", types)
		}
	})

	t.Run("bare null member part is refused at the wire boundary", func(t *testing.T) {
		// The wire model's own payload-union count (Part.Validate) treats an
		// explicit null as absent, so a part carrying ONLY a null payload
		// member has no payload and is refused at decode — no state machine
		// ever observes it. Pinned so the decode boundary stays the single
		// authority for the payload union.
		_, err := geminigeneratecontent.DecodeResponse([]byte(
			`{"candidates":[{"content":{"parts":[{"executableCode":null}]},"index":0}]}`,
		))
		if err == nil || !strings.Contains(err.Error(), "part requires a payload member") {
			t.Fatalf("bare null member part: err = %v, want the missing-payload wire rejection", err)
		}
	})
}

func contains(list []string, want string) bool {
	return slices.Contains(list, want)
}

// TestGeminiUpstreamStreamPartLossesFollowPayloadCommit proves the
// part-metadata and thought-signature losses are recorded only after the
// encoding they describe commits: a part whose payload is refused (bounds,
// invalid arguments) leaves neither loss behind, and the happy path records
// each once.
func TestGeminiUpstreamStreamPartLossesFollowPayloadCommit(t *testing.T) {
	t.Run("oversized thought with part metadata leaves no losses", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		s := newGeminiResponsesStreamState(
			ctx, ctx.LossPolicy, ChatCapabilities{ProviderReasoningThinking: true}, "resp_t", "m", 1, nil,
		)
		big := strings.Repeat("a", maxStreamAccumulatedBytes+1)
		_, err := s.Convert(geminiChunk(t,
			`{"candidates":[{"content":{"parts":[{"text":"`+big+`","thought":true,"partMetadata":{"foo":1}}]},"index":0}]}`,
		))
		if err == nil {
			t.Fatal("oversized thought delta: nil error")
		}
		if n := countReportFeature(s.report, FeatureGeminiPartMetadata); n != 0 {
			t.Fatalf("part metadata loss recorded for a refused payload: %+v", s.report.Losses)
		}
	})

	t.Run("refused functionCall with part metadata leaves no losses", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_t", "m", 1, nil)
		// A function call with no name is refused inside the call conversion,
		// after the part-metadata decision point today.
		_, err := s.Convert(geminiChunk(t,
			`{"candidates":[{"content":{"parts":[{"functionCall":{"args":{"x":1}},"partMetadata":{"foo":1}}]},"index":0}]}`,
		))
		if err == nil {
			t.Fatal("nameless functionCall: nil error")
		}
		if n := countReportFeature(s.report, FeatureGeminiPartMetadata); n != 0 {
			t.Fatalf("part metadata loss recorded for a refused payload: %+v", s.report.Losses)
		}
	})

	t.Run("signature with oversized text delta leaves no signature loss", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_t", "m", 1, nil)
		_, err := s.Convert(geminiChunk(t,
			`{"candidates":[{"content":{"parts":[{"text":"`+strings.Repeat("a", maxStreamAccumulatedBytes+1)+
				`","thoughtSignature":"sig"}]},"index":0}]}`,
		))
		if err == nil {
			t.Fatal("oversized text delta: nil error")
		}
		if n := countReportFeature(s.report, FeatureGeminiResponseThoughtSignature); n != 0 {
			t.Fatalf("signature loss recorded for a refused payload: %+v", s.report.Losses)
		}
	})

	t.Run("happy path records each loss once", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_t", "m", 1, nil)
		if _, err := s.Convert(geminiChunk(t,
			`{"candidates":[{"content":{"parts":[{"text":"hi","partMetadata":{"foo":1},"thoughtSignature":"sig"}]},"index":0}]}`,
		)); err != nil {
			t.Fatalf("part with metadata and signature: %v", err)
		}
		if n := countReportFeature(s.report, FeatureGeminiPartMetadata); n != 1 {
			t.Fatalf("part metadata loss recorded %d times, want 1: %+v", n, s.report.Losses)
		}
		if n := countReportFeature(s.report, FeatureGeminiResponseThoughtSignature); n != 1 {
			t.Fatalf("signature loss recorded %d times, want 1: %+v", n, s.report.Losses)
		}
		// A second part with the same modifiers records nothing further.
		if _, err := s.Convert(geminiChunk(t,
			`{"candidates":[{"content":{"parts":[{"text":"again","partMetadata":{"foo":1},"thoughtSignature":"sig2"}]},"index":0}]}`,
		)); err != nil {
			t.Fatalf("second part: %v", err)
		}
		if n := countReportFeature(s.report, FeatureGeminiPartMetadata); n != 1 {
			t.Fatalf("part metadata loss recorded %d times after two parts, want 1: %+v", n, s.report.Losses)
		}
	})

	t.Run("signature-only part records before its nil return", func(t *testing.T) {
		// A signature-only part emits no content events (only the lifecycle
		// pair the chunk itself opens) but still loses the signature: the
		// loss must be recorded before the nil return.
		ctx := geminiStreamTestContext()
		s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_t", "m", 1, nil)
		events, err := s.Convert(geminiChunk(t,
			`{"candidates":[{"content":{"parts":[{"thoughtSignature":"sig"}]},"index":0}]}`,
		))
		if err != nil {
			t.Fatalf("signature-only part: %v", err)
		}
		types := geminiStreamEventTypes(events)
		want := []string{"response.created", "response.in_progress"}
		if strings.Join(types, ",") != strings.Join(want, ",") {
			t.Fatalf("signature-only part emitted %v, want only %v", types, want)
		}
		if n := countReportFeature(s.report, FeatureGeminiResponseThoughtSignature); n != 1 {
			t.Fatalf("signature loss recorded %d times, want 1: %+v", n, s.report.Losses)
		}
	})
}

func TestGeminiUpstreamStreamPromptBlocked(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	events, final, err := collectResponses(t, s,
		`{"promptFeedback":{"blockReason":"SAFETY"}}`,
	)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	all := append(append([]ResponsesSSEEvent{}, events...), final...)
	assertGeminiStreamTypes(t, all,
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.refusal.delta",
		"response.refusal.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.incomplete",
	)
	assertGeminiIncompleteReason(t, all[len(all)-1], "content_filter")
}

// assertGeminiStreamTypes fails unless the events have exactly the given types
// in order.
func assertGeminiStreamTypes(t *testing.T, events []ResponsesSSEEvent, want ...string) {
	t.Helper()
	got := geminiStreamEventTypes(events)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event types:\n got  %v\n want %v", got, want)
	}
}

// assertGeminiIncompleteReason fails unless the event is response.incomplete
// with the given incomplete_details.reason.
func assertGeminiIncompleteReason(t *testing.T, event ResponsesSSEEvent, want string) {
	t.Helper()
	incomplete, ok := event.(ResponseIncompleteEvent)
	if !ok {
		t.Fatalf("terminal = %T, want response.incomplete", event)
	}
	if incomplete.Response.IncompleteDetails == nil || incomplete.Response.IncompleteDetails.Reason != want {
		t.Fatalf("incomplete_details = %+v, want reason %q", incomplete.Response.IncompleteDetails, want)
	}
}

func TestGeminiUpstreamStreamMaxTokensIsIncomplete(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	_, final, err := collectResponses(t, s,
		`{"candidates":[{"content":{"parts":[{"text":"cut"}]},"index":0}]}`,
		`{"candidates":[{"finishReason":"MAX_TOKENS","index":0}]}`,
	)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	assertGeminiIncompleteReason(t, final[len(final)-1], "max_output_tokens")
}

func TestGeminiUpstreamStreamMultipleCandidatesRejected(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	_, err := s.Convert(geminiChunk(t, `{"candidates":[{"index":0},{"index":1}]}`))
	if err == nil {
		t.Fatal("two candidates: nil error, want wire error")
	}
}

func TestGeminiUpstreamStreamNonZeroIndexRejected(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	_, err := s.Convert(geminiChunk(t, `{"candidates":[{"index":3}]}`))
	if err == nil {
		t.Fatal("index 3: nil error, want wire error")
	}
}

func TestGeminiUpstreamStreamMalformedChunk(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	c := newGeminiToResponsesConverter(s)
	_, err := c.Convert(SSEEvent{Data: []byte(`{"candidates":[`)})
	if err == nil {
		t.Fatal("malformed chunk: nil error, want wire error")
	}
}

func TestGeminiUpstreamStreamContinuityTurns(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	_, _, err := collectResponses(t, s,
		`{"candidates":[{"content":{"parts":[{"text":"answer"}]},"index":0}]}`,
		`{"candidates":[{"finishReason":"STOP","index":0}]}`,
	)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	id, turns, _, _, ok := s.continuityTurns([]CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "q"}}}}, 0)
	if !ok {
		t.Fatal("continuityTurns not ok after terminal")
	}
	if id != "resp_test" {
		t.Fatalf("id = %q", id)
	}
	if len(turns) != 2 || turns[1].Role != CanonicalAssistant {
		t.Fatalf("turns = %+v", turns)
	}
}

func TestGeminiUpstreamStreamAnthropicComposition(t *testing.T) {
	ctx := geminiStreamTestContext()
	ctx.LossPolicy.Allowed[FeatureUsageUnknown] = struct{}{}
	gemini := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_a", "m", 1, nil)
	anthropic := newAnthropicResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "msg_a", "m", 1)
	c := newGeminiToAnthropicConverter(gemini, anthropic)
	first, err := c.Convert(SSEEvent{Data: []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"index":0}]}`)})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if first.Terminal {
		t.Fatal("text chunk terminal")
	}
	_, err = c.Convert(SSEEvent{Data: []byte(`{"candidates":[{"finishReason":"STOP","index":0}]}`)})
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	batch, err := c.FinalizeEOF()
	if err != nil {
		t.Fatalf("EOF: %v", err)
	}
	if !batch.Terminal {
		t.Fatal("EOF must release the terminal")
	}
	var sawMessageDelta, sawMessageStop, sawTextDelta bool
	for _, ev := range append(first.Events, batch.Events...) {
		switch string(ev.Type) {
		case "message_delta", "content_block_delta":
			sawMessageDelta = true
		case "message_stop":
			sawMessageStop = true
		}
		if strings.Contains(string(ev.Data), `"text_delta"`) {
			sawTextDelta = true
		}
	}
	if !sawMessageDelta || !sawMessageStop || !sawTextDelta {
		t.Fatalf("events = %v / %v, want a text delta, message_delta and message_stop", first.Events, batch.Events)
	}
}

func TestGeminiUpstreamStreamTextThenFunctionCallClosesMessage(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	events, final, err := collectResponses(t, s,
		`{"candidates":[{"content":{"parts":[{"text":"Let me check"}]},"index":0}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP","index":0}]}`,
	)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	all := append(append([]ResponsesSSEEvent{}, events...), final...)
	assertGeminiStreamTypes(t, all,
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	)
	completed := all[len(all)-1].(ResponseCompletedEvent)
	if len(completed.Response.Output) != 2 {
		t.Fatalf("terminal output = %+v, want message + call", completed.Response.Output)
	}
	message, ok := completed.Response.Output[0].(*ResponsesOutputMessage)
	if !ok || message.Status != ResponsesItemCompleted {
		t.Fatalf("terminal output[0] = %#v, want a completed message", completed.Response.Output[0])
	}
	if text := message.Content[0].(*ResponsesOutputText).Text; text != "Let me check" {
		t.Fatalf("terminal message text = %q", text)
	}
}

func TestGeminiUpstreamStreamThoughtThenTextClosesReasoning(t *testing.T) {
	ctx := geminiStreamTestContext()
	s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{ProviderReasoningThinking: true},
		"resp_test", "gemini-3-pro-preview", 1700000000, nil)
	events, final, err := collectResponses(t, s,
		`{"candidates":[{"content":{"parts":[{"text":"hmm","thought":true}]},"index":0}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP","index":0}]}`,
	)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	all := append(append([]ResponsesSSEEvent{}, events...), final...)
	assertGeminiStreamTypes(t, all,
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	)
	completed := all[len(all)-1].(ResponseCompletedEvent)
	reasoning, ok := completed.Response.Output[0].(*ResponsesReasoningOutputItem)
	if !ok || reasoning.Status != ResponsesItemCompleted || reasoning.Summary[0].Text != "hmm" {
		t.Fatalf("terminal output[0] = %#v, want the completed reasoning item", completed.Response.Output[0])
	}
}

func TestGeminiUpstreamStreamRefusalFinishReason(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	events, final, err := collectResponses(t, s,
		`{"candidates":[{"content":{"parts":[{"text":"partial"}]},"index":0}]}`,
		`{"candidates":[{"finishReason":"SAFETY","finishMessage":"blocked by safety","index":0}]}`,
	)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	all := append(append([]ResponsesSSEEvent{}, events...), final...)
	assertGeminiStreamTypes(t, all,
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.content_part.added",
		"response.refusal.delta",
		"response.refusal.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.incomplete",
	)
	assertGeminiIncompleteReason(t, all[len(all)-1], "content_filter")
	message := all[len(all)-1].(ResponseIncompleteEvent).Response.Output[0].(*ResponsesOutputMessage)
	if refusal, ok := message.Content[1].(*ResponsesOutputRefusal); !ok || refusal.Refusal != "blocked by safety" {
		t.Fatalf("message content = %#v, want text then the finishMessage refusal", message.Content)
	}
	if !reportHasFeature(s.report, FeatureGeminiFinishRefusal) {
		t.Fatalf("report lacks the finish-refusal note: %+v", s.report.Losses)
	}
}

// geminiAnthropicStream runs Gemini chunks through the composed
// Gemini->Responses->Anthropic converter and returns every Anthropic frame.
func geminiAnthropicStream(t *testing.T, chunks ...string) []frameEvent {
	t.Helper()
	ctx := geminiStreamTestContext()
	ctx.LossPolicy.Allowed[FeatureUsageUnknown] = struct{}{}
	gemini := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_a", "m", 1, nil)
	anthropic := newAnthropicResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "msg_a", "m", 1)
	c := newGeminiToAnthropicConverter(gemini, anthropic)
	var frames []frameEvent
	for _, chunk := range chunks {
		batch, err := c.Convert(SSEEvent{Data: []byte(chunk)})
		if err != nil {
			t.Fatalf("convert %s: %v", chunk, err)
		}
		frames = append(frames, batch.Events...)
	}
	batch, err := c.FinalizeEOF()
	if err != nil {
		t.Fatalf("EOF: %v", err)
	}
	if !batch.Terminal {
		t.Fatal("EOF must release the terminal")
	}
	return append(frames, batch.Events...)
}

// anthropicStopReason returns the stop_reason of the message_delta frame.
func anthropicStopReason(t *testing.T, frames []frameEvent) string {
	t.Helper()
	for _, frame := range frames {
		if string(frame.Type) != "message_delta" {
			continue
		}
		var delta struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(frame.Data, &delta); err != nil {
			t.Fatalf("message_delta: %v", err)
		}
		return delta.Delta.StopReason
	}
	t.Fatalf("no message_delta in %v", frames)
	return ""
}

func TestGeminiUpstreamStreamAnthropicCompositionKindChanges(t *testing.T) {
	frames := geminiAnthropicStream(t,
		`{"candidates":[{"content":{"parts":[{"text":"Let me check"}]},"index":0}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP","index":0}]}`,
	)
	if got := anthropicStopReason(t, frames); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got)
	}
}

func TestGeminiUpstreamStreamAnthropicCompositionRefusals(t *testing.T) {
	t.Run("finish reason", func(t *testing.T) {
		frames := geminiAnthropicStream(t,
			`{"candidates":[{"content":{"parts":[{"text":"partial"}]},"index":0}]}`,
			`{"candidates":[{"finishReason":"SAFETY","index":0}]}`,
		)
		if got := anthropicStopReason(t, frames); got != "refusal" {
			t.Fatalf("stop_reason = %q, want refusal", got)
		}
	})
	t.Run("blocked prompt", func(t *testing.T) {
		frames := geminiAnthropicStream(t, `{"promptFeedback":{"blockReason":"SAFETY"}}`)
		if got := anthropicStopReason(t, frames); got != "refusal" {
			t.Fatalf("stop_reason = %q, want refusal", got)
		}
	})
}

func TestGeminiUpstreamStreamFunctionCallChargesExchangeTotal(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	s.totalAccumulated = maxStreamTotalAccumulatedBytes - 4
	_, err := s.Convert(geminiChunk(t,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","name":"lookup","args":{"q":"x"}}}]},"index":0}]}`,
	))
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("err = %T %v, want *UpstreamWireError for the exchange total", err, err)
	}
	if len(s.items) != 0 {
		t.Fatalf("items = %d, want the call refused before it was retained", len(s.items))
	}
}

func TestGeminiUpstreamStreamPromptBlockedKeepsUsageAndMetadata(t *testing.T) {
	const blocked = `{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"HIGH"}]},` +
		`"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7},"modelStatus":{"modelStage":"STABLE"}}`
	s := newGeminiStreamState(t, nil)
	_, final, err := collectResponses(t, s, blocked)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	terminal := final[len(final)-1].(ResponseIncompleteEvent)
	if terminal.Response.Usage == nil || terminal.Response.Usage.InputTokens != 7 || terminal.Response.Usage.TotalTokens != 7 {
		t.Fatalf("terminal usage = %+v, want the blocked chunk's usage", terminal.Response.Usage)
	}
	for _, feature := range []Feature{FeatureGeminiSafetyMetadata, FeatureGeminiModelStatus} {
		if !reportHasFeature(s.report, feature) {
			t.Errorf("report lacks %s: %+v", feature, s.report.Losses)
		}
	}

	strictCtx := geminiStreamTestContext()
	strictCtx.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureGeminiPromptBlocked: {}}}
	strict := newGeminiStreamState(t, strictCtx)
	if _, err := strict.Convert(geminiChunk(t, blocked)); err == nil {
		t.Fatal("strict policy: nil error, want the prompt safety ratings refused")
	}
}

func TestGeminiUpstreamStreamTextBoundIncludesTheDelta(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	big := strings.Repeat("a", maxStreamAccumulatedBytes+1)
	_, err := s.Convert(geminiChunk(t, `{"candidates":[{"content":{"parts":[{"text":"`+big+`"}]},"index":0}]}`))
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("over-bound delta: err = %T %v, want *UpstreamWireError", err, err)
	}
}

func TestGeminiUpstreamStreamRefusalTextIsBounded(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	big := strings.Repeat("a", maxStreamAccumulatedBytes+1)
	_, err := s.Convert(geminiChunk(t, `{"candidates":[{"finishReason":"SAFETY","finishMessage":"`+big+`","index":0}]}`))
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("over-bound finishMessage: err = %T %v, want *UpstreamWireError", err, err)
	}
}

func TestGeminiUpstreamStreamAddedEventsAreCreationSnapshots(t *testing.T) {
	ctx := geminiStreamTestContext()
	s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{ProviderReasoningThinking: true},
		"resp_test", "gemini-3-pro-preview", 1700000000, nil)
	events, final, err := collectResponses(t, s,
		`{"candidates":[{"content":{"parts":[{"text":"hmm","thought":true},{"text":"hi"},{"functionCall":{"id":"call_1","name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP","index":0}]}`,
	)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	added := 0
	for _, event := range append(events, final...) {
		value, ok := event.(ResponseOutputItemAddedEvent)
		if !ok {
			continue
		}
		added++
		// Marshal as the converter would, after the whole chunk converted.
		data, err := MarshalResponsesEvent(value)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded struct {
			Item map[string]any `json:"item"`
		}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		item := decoded.Item
		if item["status"] != "in_progress" {
			t.Errorf("%s added with status %v, want in_progress", item["type"], item["status"])
		}
		switch item["type"] {
		case "message":
			if content, _ := item["content"].([]any); len(content) != 0 {
				t.Errorf("message added with content %v, want []", content)
			}
		case "reasoning":
			if summary, _ := item["summary"].([]any); len(summary) != 0 {
				t.Errorf("reasoning added with summary %v, want []", summary)
			}
		case "function_call":
			if item["arguments"] != "" {
				t.Errorf("function call added with arguments %v, want empty", item["arguments"])
			}
		}
	}
	if added != 3 {
		t.Fatalf("added events = %d, want 3", added)
	}
}

func TestGeminiUpstreamStreamEmptySafetyRatingsAreNotALoss(t *testing.T) {
	// An explicit empty safetyRatings:[] carries no information, so it is not
	// a loss — matching the prompt-feedback and non-streaming gates.
	ctx := geminiStreamTestContext()
	ctx.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{}}
	s := newGeminiStreamState(t, ctx)
	_, final, err := collectResponses(t, s,
		`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"safetyRatings":[],"index":0}]}`,
		`{"candidates":[{"finishReason":"STOP","safetyRatings":[],"index":0}]}`,
		`{"promptFeedback":{"safetyRatings":[]},"usageMetadata":{"totalTokenCount":1}}`,
	)
	if err != nil {
		t.Fatalf("stream under a fully strict policy: %v", err)
	}
	_ = final
	if reportHasFeature(s.report, FeatureGeminiSafetyMetadata) {
		t.Fatalf("empty safetyRatings arrays recorded a loss: %+v", s.report.Losses)
	}
}

func TestGeminiUpstreamStreamClampNotesAreOncePerStream(t *testing.T) {
	// Repeated usage chunks carrying the same arithmetic inconsistency must
	// record the clamp note once, not saturate the report (the chat state
	// gates these via usageClampNotes).
	s := newGeminiStreamState(t, nil)
	badUsage := `{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":99}}`
	if _, err := s.Convert(geminiChunk(t, geminiFinishChunk)); err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Convert(geminiChunk(t, badUsage)); err != nil {
			t.Fatalf("usage chunk %d: %v", i, err)
		}
	}
	if n := countReportFeature(s.report, FeatureUsageTotalMismatch); n != 1 {
		t.Fatalf("total-mismatch note recorded %d times, want 1: %+v", n, s.report.Losses)
	}
}

func TestGeminiUpstreamStreamErrorFrameFinalizeEOFStaysFailed(t *testing.T) {
	// An in-stream error frame is an upstream failure: FinalizeEOF after it
	// must propagate the failure, never release a successful terminal.
	s := newGeminiStreamState(t, nil)
	_, err := s.Convert(geminiChunk(t, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"bad request"}}`))
	if err == nil || !isUpstreamConversionError(err) {
		t.Fatalf("error frame: err = %T %v, want an upstream conversion error", err, err)
	}
	events, finalErr := s.FinalizeEOF()
	if finalErr == nil || !isUpstreamConversionError(finalErr) {
		t.Fatalf("FinalizeEOF after error frame: err = %T %v, want an upstream conversion error", finalErr, finalErr)
	}
	for _, e := range events {
		if e.EventType() == "response.completed" {
			t.Fatalf("FinalizeEOF released a success terminal for an errored stream: %+v", e)
		}
	}
}

func TestGeminiUpstreamStreamDoneAfterErrorStaysFailed(t *testing.T) {
	// A failed stream must stay failed: neither the Responses nor the
	// Anthropic composition may release the held terminal on [DONE], and a
	// later chunk must keep returning the latched failure instead of being
	// absorbed as trailing accounting.
	errFrame := SSEEvent{Data: []byte(`{"error":{"code":500,"status":"INTERNAL","message":"boom"}}`)}

	t.Run("responses", func(t *testing.T) {
		s := newGeminiStreamState(t, nil)
		c := newGeminiToResponsesConverter(s)
		if _, err := c.Convert(errFrame); err == nil || !isUpstreamConversionError(err) {
			t.Fatalf("error frame: err = %T %v, want an upstream conversion error", err, err)
		}
		batch, err := c.Convert(SSEEvent{Data: []byte(`[DONE]`)})
		if err == nil || !isUpstreamConversionError(err) {
			t.Fatalf("[DONE] after error: err = %T %v, want the latched upstream failure", err, err)
		}
		if batch.Terminal || len(batch.Events) != 0 {
			t.Fatalf("[DONE] after error released events %v (terminal %v)", batch.Events, batch.Terminal)
		}
		if _, err := c.Convert(SSEEvent{Data: []byte(`{"usageMetadata":{"totalTokenCount":1}}`)}); err == nil {
			t.Fatal("chunk after error: accepted, want the latched failure")
		}
	})

	t.Run("anthropic composition", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		ctx.LossPolicy.Allowed[FeatureUsageUnknown] = struct{}{}
		gemini := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_a", "m", 1, nil)
		anthropic := newAnthropicResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "msg_a", "m", 1)
		c := newGeminiToAnthropicConverter(gemini, anthropic)
		if _, err := c.Convert(errFrame); err == nil || !isUpstreamConversionError(err) {
			t.Fatalf("error frame: err = %T %v, want an upstream conversion error", err, err)
		}
		batch, err := c.Convert(SSEEvent{Data: []byte(`[DONE]`)})
		if err == nil || !isUpstreamConversionError(err) {
			t.Fatalf("[DONE] after error: err = %T %v, want the latched upstream failure", err, err)
		}
		if batch.Terminal || len(batch.Events) != 0 {
			t.Fatalf("[DONE] after error released events %v (terminal %v)", batch.Events, batch.Terminal)
		}
	})
}

func TestGeminiUpstreamStreamTrailingFrameFailureLatches(t *testing.T) {
	// A frame the converter rejects after the finish chunk — a non-empty SSE
	// event name, which Gemini's data-only stream cannot carry — must latch
	// on the converter itself: [DONE] and FinalizeEOF keep reporting the
	// failure instead of releasing the held success terminal.
	t.Run("responses", func(t *testing.T) {
		s := newGeminiStreamState(t, nil)
		c := newGeminiToResponsesConverter(s)
		if _, err := c.Convert(SSEEvent{Data: []byte(geminiFinishChunk)}); err != nil {
			t.Fatalf("finish chunk: %v", err)
		}
		_, latched := c.Convert(SSEEvent{Event: "message", Data: []byte(`{}`)})
		if latched == nil || !isUpstreamConversionError(latched) {
			t.Fatalf("event-named frame: err = %T %v, want an upstream conversion error", latched, latched)
		}
		batch, err := c.Convert(SSEEvent{Data: []byte("[DONE]")})
		if !errors.Is(err, latched) {
			t.Fatalf("[DONE] after a failed frame: err = %v, want the latched %v", err, latched)
		}
		if batch.Terminal || len(batch.Events) != 0 {
			t.Fatalf("[DONE] after a failed frame released %+v", batch)
		}
		batch, err = c.FinalizeEOF()
		if !errors.Is(err, latched) {
			t.Fatalf("FinalizeEOF after a failed frame: err = %v, want the latched %v", err, latched)
		}
		if batch.Terminal || len(batch.Events) != 0 {
			t.Fatalf("FinalizeEOF after a failed frame released %+v", batch)
		}
	})

	t.Run("anthropic composition", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		ctx.LossPolicy.Allowed[FeatureUsageUnknown] = struct{}{}
		gemini := newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_a", "m", 1, nil)
		anthropic := newAnthropicResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "msg_a", "m", 1)
		c := newGeminiToAnthropicConverter(gemini, anthropic)
		if _, err := c.Convert(SSEEvent{Data: []byte(geminiFinishChunk)}); err != nil {
			t.Fatalf("finish chunk: %v", err)
		}
		_, latched := c.Convert(SSEEvent{Event: "message", Data: []byte(`{}`)})
		if latched == nil || !isUpstreamConversionError(latched) {
			t.Fatalf("event-named frame: err = %T %v, want an upstream conversion error", latched, latched)
		}
		batch, err := c.Convert(SSEEvent{Data: []byte("[DONE]")})
		if !errors.Is(err, latched) {
			t.Fatalf("[DONE] after a failed frame: err = %v, want the latched %v", err, latched)
		}
		if batch.Terminal || len(batch.Events) != 0 {
			t.Fatalf("[DONE] after a failed frame released %+v", batch)
		}
		batch, err = c.FinalizeEOF()
		if !errors.Is(err, latched) {
			t.Fatalf("FinalizeEOF after a failed frame: err = %v, want the latched %v", err, latched)
		}
		if batch.Terminal || len(batch.Events) != 0 {
			t.Fatalf("FinalizeEOF after a failed frame released %+v", batch)
		}
	})
}

func TestGeminiUpstreamStreamTrailingFailureDoesNotReleaseTerminal(t *testing.T) {
	// A chunk rejected after the finish chunk was accepted is still a stream
	// failure: EOF must propagate it, never release the held success.
	s := newGeminiStreamState(t, nil)
	if _, err := s.Convert(geminiChunk(t, geminiFinishChunk)); err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	if _, err := s.Convert(geminiChunk(t,
		`{"candidates":[{"content":{"parts":[{"text":"late"}]},"index":0}]}`,
	)); err == nil {
		t.Fatal("content after finish: nil error")
	}
	final, err := s.FinalizeEOF()
	if err == nil || !isUpstreamConversionError(err) {
		t.Fatalf("FinalizeEOF after a trailing failure: err = %T %v, want the latched upstream failure", err, err)
	}
	if len(final) != 0 {
		t.Fatalf("FinalizeEOF released events after a trailing failure: %v", final)
	}
}

func TestGeminiUpstreamStreamBlockedMetadataFailureDoesNotReleaseTerminal(t *testing.T) {
	// The blocked-prompt chunk fails its metadata loss under this policy:
	// the refusal encoding never committed, so EOF must fail rather than
	// release the held refusal terminal.
	ctx := geminiStreamTestContext()
	ctx.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureGeminiPromptBlocked: {}}}
	s := newGeminiStreamState(t, ctx)
	if _, err := s.Convert(geminiChunk(t,
		`{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"HIGH"}]}}`,
	)); err == nil {
		t.Fatal("blocked chunk with an unapproved safety loss: nil error")
	}
	final, err := s.FinalizeEOF()
	var unsupported *UnsupportedFeatureError
	if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureGeminiSafetyMetadata) {
		t.Fatalf("FinalizeEOF after a rejected blocked chunk: err = %T %v, want the latched %s refusal", err, err, FeatureGeminiSafetyMetadata)
	}
	if len(final) != 0 {
		t.Fatalf("FinalizeEOF released events after a rejected blocked chunk: %v", final)
	}
	if n := countReportFeature(s.report, FeatureGeminiPromptBlocked); n != 0 {
		t.Fatalf("blocked note recorded for a rejected refusal: %+v", s.report.Losses)
	}
}

func TestGeminiUpstreamStreamBlockReasonWithCandidateIsCorrupt(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	_, err := s.Convert(geminiChunk(t,
		`{"promptFeedback":{"blockReason":"SAFETY"},"candidates":[{"content":{"parts":[{"text":"hi"}]},"index":0}]}`,
	))
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("err = %T %v, want *UpstreamWireError", err, err)
	}
}

func TestGeminiUpstreamStreamCrossChunkBlockContradictionIsCorrupt(t *testing.T) {
	// The blockReason+candidates contradiction is corrupt wire even when the
	// two halves arrive in different chunks, in either order.
	candidateChunk := `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"index":0}]}`
	blockedChunk := `{"promptFeedback":{"blockReason":"SAFETY"}}`
	trailingCandidateChunk := `{"candidates":[{"safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"HIGH"}],"index":0}]}`

	// Candidate first, then a blocked-prompt chunk.
	s := newGeminiStreamState(t, nil)
	if _, err := s.Convert(geminiChunk(t, candidateChunk)); err != nil {
		t.Fatalf("candidate chunk: %v", err)
	}
	if _, err := s.Convert(geminiChunk(t, blockedChunk)); err == nil {
		t.Fatal("blocked chunk after a candidate: err = nil, want *UpstreamWireError")
	} else if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("blocked after candidate: err = %T %v, want *UpstreamWireError", err, err)
	}

	// Blocked first, then a trailing candidate chunk (absorbTrailing path).
	s = newGeminiStreamState(t, nil)
	if _, err := s.Convert(geminiChunk(t, blockedChunk)); err != nil {
		t.Fatalf("blocked chunk: %v", err)
	}
	if _, err := s.Convert(geminiChunk(t, trailingCandidateChunk)); err == nil {
		t.Fatal("candidate chunk after a blocked prompt: err = nil, want *UpstreamWireError")
	} else if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("candidate after blocked: err = %T %v, want *UpstreamWireError", err, err)
	}
}

func countReportFeature(report ConversionReport, feature Feature) int {
	n := 0
	for _, loss := range report.Losses {
		if loss.Feature == feature {
			n++
		}
	}
	return n
}

func TestGeminiUpstreamStreamThoughtThinkingNoteIsOncePerStream(t *testing.T) {
	// Under the provider_reasoning_thinking capability every thought chunk
	// mapped to a thinking block must record the note exactly once, not per
	// chunk (report saturation / Dropped inflation).
	ctx := geminiStreamTestContext()
	ctx.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureProviderReasoningText: {},
	}}
	s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy,
		ChatCapabilities{ProviderReasoningThinking: true}, "resp_test", "gemini-3-pro-preview", 1700000000, nil)
	chunks := []string{
		`{"candidates":[{"content":{"parts":[{"text":"t1","thought":true}]},"index":0}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"t2","thought":true}]},"index":0}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"t3","thought":true}]},"index":0}]}`,
		geminiFinishChunk,
	}
	if _, _, err := collectResponses(t, s, chunks...); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if n := countReportFeature(s.report, FeatureProviderReasoningText); n != 1 {
		t.Fatalf("thinking note recorded %d times, want 1 (Dropped=%d): %+v", n, s.report.Dropped, s.report.Losses)
	}
}

func TestGeminiUpstreamStreamPartMetadataLossesAreOncePerStream(t *testing.T) {
	// Part-modifier and non-call thought-signature losses gate once per
	// stream, like the candidate/chunk-level losses.
	ctx := geminiStreamTestContext() // allows FeatureGeminiPartMetadata
	ctx.LossPolicy.Allowed[FeatureGeminiResponseThoughtSignature] = struct{}{}
	s := newGeminiStreamState(t, ctx)
	chunks := []string{
		`{"candidates":[{"content":{"parts":[{"text":"a","videoMetadata":{"fps":30}}]},"index":0}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"b","thoughtSignature":"sig"}]},"index":0}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"c","partMetadata":{"k":1},"thoughtSignature":"sig2"}]},"index":0}]}`,
		geminiFinishChunk,
	}
	if _, _, err := collectResponses(t, s, chunks...); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if n := countReportFeature(s.report, FeatureGeminiPartMetadata); n != 1 {
		t.Fatalf("part metadata loss recorded %d times, want 1: %+v", n, s.report.Losses)
	}
	if n := countReportFeature(s.report, FeatureGeminiResponseThoughtSignature); n != 1 {
		t.Fatalf("thought signature loss recorded %d times, want 1: %+v", n, s.report.Losses)
	}
}

func TestGeminiUpstreamStreamEmptyThoughtPartRecordsNothing(t *testing.T) {
	// A signature-only thought chunk (empty text) emits nothing and must not
	// record a provider-reasoning-text note before the empty-delta short
	// circuit.
	ctx := geminiStreamTestContext()
	ctx.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureProviderReasoningText:          {},
		FeatureGeminiResponseThoughtSignature: {},
	}}
	s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy,
		ChatCapabilities{ProviderReasoningThinking: true}, "resp_test", "gemini-3-pro-preview", 1700000000, nil)
	events, err := s.Convert(geminiChunk(t,
		`{"candidates":[{"content":{"parts":[{"text":"","thought":true,"thoughtSignature":"sig"}]},"index":0}]}`,
	))
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	if len(events) != 2 { // only created + in_progress
		t.Fatalf("signature-only thought part emitted %d events, want 2 (lifecycle only)", len(events))
	}
	if n := countReportFeature(s.report, FeatureProviderReasoningText); n != 0 {
		t.Fatalf("empty thought delta recorded a reasoning-text note: %+v", s.report.Losses)
	}
}

func TestGeminiUpstreamStreamBlockedNoteFollowsSuccessfulRefusal(t *testing.T) {
	// The prompt-blocked note describes an encoding that was actually
	// rendered; when the refusal text exceeds the bound the chunk fails and
	// no note may be logged for it.
	s := newGeminiStreamState(t, nil)
	long := strings.Repeat("x", maxStreamAccumulatedBytes+1)
	// A blockReason with no candidates goes through appendRefusal, whose
	// bound rejects the synthesized refusal text only when the block reason
	// itself is long; use a long blockReason so the refusal text exceeds it.
	_, err := s.Convert(geminiChunk(t,
		`{"promptFeedback":{"blockReason":"`+long+`"}}`,
	))
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("oversized refusal: err = %T %v, want *UpstreamWireError", err, err)
	}
	if n := countReportFeature(s.report, FeatureGeminiPromptBlocked); n != 0 {
		t.Fatalf("note recorded for a rejected refusal: %+v", s.report.Losses)
	}
}

// finishChunk is a terminal STOP chunk; trailing chunks follow it.
const geminiFinishChunk = `{"candidates":[{"finishReason":"STOP","index":0}]}`

func TestGeminiUpstreamStreamTrailingChunkWithFinishReasonIsCorrupt(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	if _, err := s.Convert(geminiChunk(t, geminiFinishChunk)); err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	_, err := s.Convert(geminiChunk(t,
		`{"candidates":[{"finishReason":"SAFETY","index":0}]}`,
	))
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("trailing finishReason: err = %T %v, want *UpstreamWireError", err, err)
	}
}

func TestGeminiUpstreamStreamTrailingChunkWithFinishMessageIsCorrupt(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	if _, err := s.Convert(geminiChunk(t, geminiFinishChunk)); err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	_, err := s.Convert(geminiChunk(t,
		`{"candidates":[{"finishMessage":"late","index":0}]}`,
	))
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("trailing finishMessage: err = %T %v, want *UpstreamWireError", err, err)
	}
}

func TestGeminiUpstreamStreamTrailingChunkWithBadRoleIsCorrupt(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	if _, err := s.Convert(geminiChunk(t, geminiFinishChunk)); err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	_, err := s.Convert(geminiChunk(t,
		`{"candidates":[{"content":{"role":"user","parts":[]},"index":0}]}`,
	))
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("trailing bad role: err = %T %v, want *UpstreamWireError", err, err)
	}
}

func TestGeminiUpstreamStreamTrailingCandidateMetadataIsNotDropped(t *testing.T) {
	// A trailing candidate chunk with no content parts must still record its
	// metadata losses under the policy: safety ratings are lost, and a
	// continuation token is unrepresentable.
	trailingSafety := `{"candidates":[{"safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"HIGH"}],"index":0}]}`
	trailingContinuation := `{"candidates":[{"continuationToken":"tok","index":0}]}`

	// Permissive policy: the safety loss is recorded and the chunk absorbed.
	s := newGeminiStreamState(t, nil)
	if _, err := s.Convert(geminiChunk(t, geminiFinishChunk)); err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	if _, err := s.Convert(geminiChunk(t, trailingSafety)); err != nil {
		t.Fatalf("trailing safety chunk: %v", err)
	}
	if !reportHasFeature(s.report, FeatureGeminiSafetyMetadata) {
		t.Fatalf("trailing candidate safety ratings were not recorded: %+v", s.report.Losses)
	}

	// Strict policy: the trailing safety ratings refuse the stream.
	strictCtx := geminiStreamTestContext()
	strictCtx.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{}}
	strict := newGeminiStreamState(t, strictCtx)
	if _, err := strict.Convert(geminiChunk(t, geminiFinishChunk)); err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	if _, err := strict.Convert(geminiChunk(t, trailingSafety)); err == nil ||
		!strings.Contains(err.Error(), string(FeatureGeminiSafetyMetadata)) {
		t.Fatalf("strict trailing safety chunk: err = %v, want a %s refusal", err, FeatureGeminiSafetyMetadata)
	}

	// A trailing continuation token is unrepresentable on any policy.
	c := newGeminiStreamState(t, nil)
	if _, err := c.Convert(geminiChunk(t, geminiFinishChunk)); err != nil {
		t.Fatalf("finish chunk: %v", err)
	}
	_, err := c.Convert(geminiChunk(t, trailingContinuation))
	if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
		t.Fatalf("trailing continuationToken: err = %T %v, want *UnrepresentableError", err, err)
	}
}

func TestGeminiUpstreamStreamTrailingUsageOnlyChunkIsAbsorbed(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	events, final, err := collectResponses(t, s,
		`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"index":0}]}`,
		geminiFinishChunk,
		`{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}`,
	)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	all := append(append([]ResponsesSSEEvent{}, events...), final...)
	last := all[len(all)-1]
	data, err := MarshalResponsesEvent(last)
	if err != nil {
		t.Fatalf("marshal terminal: %v", err)
	}
	var envelope struct {
		Response struct {
			Usage *ResponsesUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal terminal: %v", err)
	}
	if envelope.Response.Usage == nil || envelope.Response.Usage.TotalTokens != 7 {
		t.Fatalf("terminal usage = %+v, want the trailing usage chunk (total 7)", envelope.Response.Usage)
	}
}

func TestGeminiUpstreamStreamEmptyThoughtSignatureRecordsNothing(t *testing.T) {
	// An explicit empty signature carries no signature, so nothing is lost.
	// The strict policy would refuse the stream if a loss were recorded.
	ctx := geminiStreamTestContext()
	ctx.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{}}
	s := newGeminiStreamState(t, ctx)
	if _, err := s.Convert(geminiChunk(t,
		`{"candidates":[{"content":{"parts":[{"text":"hello","thoughtSignature":""}]},"index":0}]}`,
	)); err != nil {
		t.Fatalf("empty thoughtSignature: %v", err)
	}
	if n := countReportFeature(s.report, FeatureGeminiResponseThoughtSignature); n != 0 {
		t.Fatalf("empty thoughtSignature recorded %d losses, want 0: %+v", n, s.report.Losses)
	}
}

func TestGeminiUpstreamStreamOversizedDeltaLeavesNoOutput(t *testing.T) {
	// A delta rejected by the exchange total must not leave the item, part,
	// or observability note it would have produced.
	exhaust := func(s *geminiResponsesStreamState) {
		s.totalAccumulated = maxStreamTotalAccumulatedBytes - 3
	}
	t.Run("thought", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy,
			ChatCapabilities{ProviderReasoningThinking: true}, "resp_test", "m", 1, nil)
		exhaust(s)
		_, err := s.Convert(geminiChunk(t,
			`{"candidates":[{"content":{"parts":[{"text":"four","thought":true}]},"index":0}]}`,
		))
		if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
			t.Fatalf("oversized thought delta: err = %T %v, want *UpstreamWireError", err, err)
		}
		if len(s.items) != 0 {
			t.Fatalf("oversized thought delta left %d items", len(s.items))
		}
		if n := countReportFeature(s.report, FeatureProviderReasoningText); n != 0 {
			t.Fatalf("oversized thought delta recorded %d notes: %+v", n, s.report.Losses)
		}
	})
	t.Run("thought as text", func(t *testing.T) {
		ctx := geminiStreamTestContext()
		s := newGeminiResponsesStreamState(ctx, ctx.LossPolicy,
			ChatCapabilities{ProviderReasoningText: true}, "resp_test", "m", 1, nil)
		exhaust(s)
		_, err := s.Convert(geminiChunk(t,
			`{"candidates":[{"content":{"parts":[{"text":"four","thought":true}]},"index":0}]}`,
		))
		if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
			t.Fatalf("oversized mapped thought delta: err = %T %v, want *UpstreamWireError", err, err)
		}
		if len(s.items) != 0 {
			t.Fatalf("oversized mapped thought delta left %d items", len(s.items))
		}
		if n := countReportFeature(s.report, FeatureProviderReasoningText); n != 0 {
			t.Fatalf("oversized mapped thought delta recorded %d notes: %+v", n, s.report.Losses)
		}
	})
	t.Run("text", func(t *testing.T) {
		s := newGeminiStreamState(t, nil)
		exhaust(s)
		_, err := s.Convert(geminiChunk(t,
			`{"candidates":[{"content":{"parts":[{"text":"four"}]},"index":0}]}`,
		))
		if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
			t.Fatalf("oversized text delta: err = %T %v, want *UpstreamWireError", err, err)
		}
		if len(s.items) != 0 {
			t.Fatalf("oversized text delta left %d items", len(s.items))
		}
	})
}

func TestGeminiUpstreamStreamOversizedRefusalLeavesNoOutput(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	long := strings.Repeat("x", maxStreamAccumulatedBytes+1)
	if _, err := s.Convert(geminiChunk(t,
		`{"promptFeedback":{"blockReason":"`+long+`"}}`,
	)); err == nil {
		t.Fatal("oversized refusal: nil error")
	}
	if len(s.items) != 0 {
		t.Fatalf("oversized refusal left %d items", len(s.items))
	}
	if n := countReportFeature(s.report, FeatureGeminiPromptBlocked); n != 0 {
		t.Fatalf("oversized refusal recorded %d notes: %+v", n, s.report.Losses)
	}
}

func TestGeminiUpstreamStreamUnrepresentableFinishLeavesNoOutput(t *testing.T) {
	// The part losses are allowed by policy, so any that were recorded before
	// the finish reason was validated would be observable pollution.
	ctx := geminiStreamTestContext()
	ctx.LossPolicy.Allowed[FeatureGeminiResponseThoughtSignature] = struct{}{}
	s := newGeminiStreamState(t, ctx)
	_, err := s.Convert(geminiChunk(t,
		`{"candidates":[{"content":{"parts":[{"text":"hi","partMetadata":{"k":1},"thoughtSignature":"sig"}]},"finishReason":"OTHER","index":0}]}`,
	))
	if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
		t.Fatalf("finishReason OTHER: err = %T %v, want *UnrepresentableError", err, err)
	}
	if len(s.items) != 0 || s.started {
		t.Fatalf("unrepresentable finish left %d items (started %v)", len(s.items), s.started)
	}
	if len(s.report.Losses) != 0 {
		t.Fatalf("unrepresentable finish recorded %+v", s.report.Losses)
	}
}

func TestGeminiUpstreamStreamSynthesizedCallIDNoteIsOncePerStream(t *testing.T) {
	s := newGeminiStreamState(t, nil)
	chunks := []string{
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"a","args":{}}}]},"index":0}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"b","args":{}}}]},"index":0}]}`,
		geminiFinishChunk,
	}
	if _, _, err := collectResponses(t, s, chunks...); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if n := countReportFeature(s.report, FeatureGeminiCallIDSynthesized); n != 1 {
		t.Fatalf("synthesized call id note recorded %d times, want 1: %+v", n, s.report.Losses)
	}
}

func TestGeminiUpstreamStreamContinuationTokenRejectedBeforeLosses(t *testing.T) {
	ctx := geminiStreamTestContext()
	ctx.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{}}
	s := newGeminiStreamState(t, ctx)
	_, err := s.Convert(geminiChunk(t,
		`{"candidates":[{"safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"HIGH"}],"continuationToken":"tok","index":0}]}`,
	))
	if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
		t.Fatalf("continuationToken with safety ratings: err = %T %v, want *UnrepresentableError", err, err)
	}
	if len(s.report.Losses) != 0 {
		t.Fatalf("continuationToken rejection recorded %+v", s.report.Losses)
	}
}

func TestGeminiUpstreamStreamContinuationTokenRejectsBeforeParts(t *testing.T) {
	// The continuation token decides the chunk before its parts are converted,
	// so a rejected chunk leaves no item and no part-level loss.
	ctx := geminiStreamTestContext() // allows the part-metadata loss
	s := newGeminiStreamState(t, ctx)
	_, err := s.Convert(geminiChunk(t,
		`{"candidates":[{"content":{"parts":[{"text":"hi","partMetadata":{"k":1}}]},"continuationToken":"tok","index":0}]}`,
	))
	if _, ok := errors.AsType[*UnrepresentableError](err); !ok {
		t.Fatalf("continuationToken with parts: err = %T %v, want *UnrepresentableError", err, err)
	}
	if len(s.items) != 0 {
		t.Fatalf("continuationToken rejection left %d items", len(s.items))
	}
	if len(s.report.Losses) != 0 {
		t.Fatalf("continuationToken rejection recorded %+v", s.report.Losses)
	}
}
