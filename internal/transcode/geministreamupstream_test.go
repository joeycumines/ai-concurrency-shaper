package transcode

import (
	"encoding/json"
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
			FeatureProviderReasoningText:   {},
			FeatureGeminiSafetyMetadata:    {},
			FeatureGeminiGroundingMetadata: {},
			FeatureGeminiPartMetadata:      {},
			FeatureGeminiUsageBreakdown:    {},
			FeatureGeminiModelStatus:       {},
			FeatureLogprobs:                {},
			FeatureResponseServiceTier:     {},
			FeatureGeminiPromptBlocked:     {},
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

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
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
	types := geminiStreamEventTypes(all)
	if !contains(types, "response.refusal.done") || !contains(types, "response.completed") {
		t.Fatalf("types = %v, want a refusal and a completed terminal", types)
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
	data, err := MarshalResponsesEvent(final[len(final)-1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"response.incomplete"`) || !strings.Contains(string(data), `"max_tokens"`) {
		t.Fatalf("terminal = %s", data)
	}
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
