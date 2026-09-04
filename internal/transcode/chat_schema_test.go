package transcode

// J4 regression tests: the Chat schema aligned with the pinned contract
// (openai-go v1.12.0, internal/transcode/pins.md) — logprobs/service_tier
// model every pinned field (review-j finding 4), and the tool-call wire types
// are split into the non-stream shape (no index) and the streaming delta
// (index-carrying) (review-j finding 5).

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// currentChatResponse returns a standard current Chat completion response:
// logprobs:null and service_tier present (the pin's required/nullable
// fields), with the pinned usage details.
func currentChatResponse() string {
	return `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4.1","system_fingerprint":"fp_1","service_tier":"auto","choices":[{"index":0,"logprobs":null,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":2,"audio_tokens":0},"completion_tokens_details":{"accepted_prediction_tokens":0,"audio_tokens":0,"reasoning_tokens":1,"rejected_prediction_tokens":0}}}`
}

// TestChatResponseCurrentShapeStrictDecodes proves that a standard current
// Chat response (logprobs:null, service_tier present, pinned usage details)
// strict-decodes — no unknown-field failure (review-j finding 4).
func TestChatResponseCurrentShapeStrictDecodes(t *testing.T) {
	response, _, err := DecodeChatResponseWithPolicy([]byte(currentChatResponse()), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if response.Source.ChatLogProbs {
		t.Fatal("logprobs:null must not set the logprobs presence flag")
	}
	if response.Source.ChatServiceTier != "auto" {
		t.Fatalf("service tier = %q, want auto", response.Source.ChatServiceTier)
	}
	if response.Usage.InputTokens != 5 || response.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", response.Usage)
	}
}

// TestChatResponseCurrentShapeConverts proves the current-shaped response
// converts through every Chat-upstream direction when no loss is triggered
// (logprobs null, service tier absent), and that the presence of the pinned
// attributes enters the explicit loss/reject decision under both policies.
func TestChatResponseCurrentShapeConverts(t *testing.T) {
	// The normal transcoded case: no service tier, no logprobs. The
	// Responses render requires the usage breakdown detail objects (pinned
	// contract) and the Messages render additionally requires the
	// cache-creation component — the source provided neither, so both
	// renders enter the usage-timing loss decision: strict rejects, an
	// approved usage_timing loss converts (review-k finding 6).
	response, _, err := DecodeChatResponseWithPolicy([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"logprobs":null,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.RequestedClientModel = "m"
	if _, _, err := RenderResponsesResponse(response, context); err == nil {
		t.Fatal("strict policy accepted a Responses render with unknown usage breakdowns")
	}
	if _, _, err := RenderMessagesResponse(response, context); err == nil {
		t.Fatal("strict policy accepted a Messages render with unknown usage breakdowns")
	}
	usagePermissive := testExchangeContext()
	usagePermissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	usagePermissive.RequestedClientModel = "m"
	if _, _, err := RenderResponsesResponse(response, usagePermissive); err != nil {
		t.Fatalf("responses render with approved usage loss: %v", err)
	}
	if _, _, err := RenderMessagesResponse(response, usagePermissive); err != nil {
		t.Fatalf("messages render with approved usage loss: %v", err)
	}

	// service_tier present: strict policy rejects, permissive policy
	// converts (the explicit loss decision, never a silent drop).
	response, _, err = DecodeChatResponseWithPolicy([]byte(currentChatResponse()), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := RenderResponsesResponse(response, context); err == nil {
		t.Fatal("strict policy accepted a chat response with service_tier")
	}
	if _, _, err := RenderMessagesResponse(response, context); err == nil {
		t.Fatal("strict policy accepted a chat response with service_tier")
	}
	permissive := testExchangeContext()
	permissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureResponseServiceTier:    {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	permissive.RequestedClientModel = "m"
	if _, _, err := RenderResponsesResponse(response, permissive); err != nil {
		t.Fatalf("permissive responses render: %v", err)
	}
	if _, _, err := RenderMessagesResponse(response, permissive); err != nil {
		t.Fatalf("permissive messages render: %v", err)
	}
}

// TestChatResponseTypedLogprobsLossDecision proves that a chat response with
// typed logprobs (not null) decodes into the typed field and enters the
// explicit loss/reject decision (never an unknown-field failure, never a
// silent drop).
func TestChatResponseTypedLogprobsLossDecision(t *testing.T) {
	body := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"logprobs":{"content":[{"token":"hi","bytes":[104,105],"logprob":-0.5,"top_logprobs":[{"token":"hi","bytes":[104,105],"logprob":-0.5}]}],"refusal":[]},"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	response, _, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatalf("typed logprobs must strict-decode: %v", err)
	}
	if !response.Source.ChatLogProbs {
		t.Fatal("typed logprobs must set the presence flag")
	}
	context := testExchangeContext()
	context.RequestedClientModel = "m"
	if _, _, err := RenderMessagesResponse(response, context); err == nil {
		t.Fatal("strict policy accepted typed logprobs")
	}
	permissive := testExchangeContext()
	permissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureLogprobs:               {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	permissive.RequestedClientModel = "m"
	if _, _, err := RenderMessagesResponse(response, permissive); err != nil {
		t.Fatalf("permissive render with logprobs: %v", err)
	}
}

// TestChatStreamCurrentShape proves the streaming chunk envelope accepts the
// pinned current fields: service_tier present and logprobs:null strict-decode,
// with service_tier entering the loss decision in the state machine.
func TestChatStreamCurrentShape(t *testing.T) {
	chunkBody := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","service_tier":"auto","choices":[{"index":0,"logprobs":null,"delta":{"content":"x"}}]}`
	var chunk ChatStreamResponse
	if err := json.Unmarshal([]byte(chunkBody), &chunk); err != nil {
		t.Fatal(err)
	}
	if chunk.ServiceTier == nil || *chunk.ServiceTier != "auto" {
		t.Fatalf("service tier = %v", chunk.ServiceTier)
	}
	if chunk.Choices[0].LogProbs != nil {
		t.Fatal("logprobs:null must decode to nil")
	}

	// Strict policy: service_tier presence rejects the stream.
	state := newChatResponsesStreamState(
		testExchangeContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	if _, err := state.Convert(chunk); err == nil {
		t.Fatal("strict policy accepted a chunk with service_tier")
	}

	// Permissive policy: the stream converts with the loss recorded.
	permissive := testExchangeContext()
	permissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureResponseServiceTier: {},
	}}
	state = newChatResponsesStreamState(
		permissive,
		permissive.LossPolicy,
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	if _, err := state.Convert(chunk); err != nil {
		t.Fatalf("permissive policy rejected a chunk with service_tier: %v", err)
	}
}

// TestNonStreamToolCallNoIndexOnWire proves that an outbound non-stream
// assistant message with a tool call renders id/function/type only — the
// index field is never serialized (review-j finding 5).
func TestNonStreamToolCallNoIndexOnWire(t *testing.T) {
	result, err := DecodeMessagesRequest(
		testcorpus.AnthropicMessagesRequestJSON(),
		// The fixture carries top_k (an approved loss here; the fixture's
		// conversation content is what this test needs).
		LossPolicy{Allowed: map[Feature]struct{}{FeatureTopK: {}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture has no tool calls; build a request with one by appending
	// an assistant turn carrying a function call.
	result.Request.Turns = append(result.Request.Turns, CanonicalTurn{
		Role: CanonicalAssistant,
		Parts: []CanonicalPart{CanonicalFunctionCall{
			CallID:    "call_1",
			Name:      "get_weather",
			Arguments: json.RawMessage(`{"location":"Tokyo"}`),
		}},
	})

	context := testExchangeContext()
	rendered, _, err := RenderChatRequest(result.Request, context, ChatCapabilities{ParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Messages []struct {
			Role      string                       `json:"role"`
			ToolCalls []map[string]json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rendered, &probe); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range probe.Messages {
		for _, call := range message.ToolCalls {
			found = true
			if _, ok := call["index"]; ok {
				t.Fatalf("non-stream tool call serialized index: %s", rendered)
			}
			if _, ok := call["id"]; !ok {
				t.Fatalf("non-stream tool call missing id: %s", rendered)
			}
			if _, ok := call["function"]; !ok {
				t.Fatalf("non-stream tool call missing function: %s", rendered)
			}
		}
	}
	if !found {
		t.Fatalf("no tool call found in rendered chat request: %s", rendered)
	}
}

// TestStreamToolCallDeltaKeepsIndex proves the streaming delta keeps its
// index-carrying wire type: a chunk delta with an index decodes into
// ChatToolCallDelta.Index, and the type is distinct from the non-stream
// ChatMessageToolCall (which has no Index field).
func TestStreamToolCallDeltaKeepsIndex(t *testing.T) {
	body := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"x\":"}}]}}]}`
	var chunk ChatStreamResponse
	if err := json.Unmarshal([]byte(body), &chunk); err != nil {
		t.Fatal(err)
	}
	if len(chunk.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(chunk.Choices[0].Delta.ToolCalls))
	}
	call := chunk.Choices[0].Delta.ToolCalls[0]
	if call.Index == nil || *call.Index != 0 {
		t.Fatalf("delta index = %v", call.Index)
	}
	if call.ID == nil || *call.ID != "call_1" {
		t.Fatalf("delta id = %v", call.ID)
	}
	// The non-stream type has no Index field (compile-time assertion):
	// ChatMessageToolCall and ChatToolCallDelta are distinct wire types.
	var nonStream ChatMessageToolCall
	_ = nonStream
}

// TestChatResponseTopLevelReasoningTokens reproduces the field-observed 502
// (autopsy 03): open-weights gateways (DeepSeek/vLLM/LiteLLM convention)
// report reasoning_tokens at the TOP LEVEL of usage. Pre-fix the strict
// decode rejected the unknown field and discarded 15-40s successful
// completions; post-fix it decodes into a known reasoning breakdown.
func TestChatResponseTopLevelReasoningTokens(t *testing.T) {
	body := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"logprobs":null,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"reasoning_tokens":15}}`
	response, _, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !response.Usage.ReasoningKnown || response.Usage.ReasoningTokens != 15 {
		t.Fatalf("usage = %+v, want known reasoning 15", response.Usage)
	}

	// The reasoning signal reaches both client renders without the
	// unknown-reasoning loss: approve only the unrelated cache components
	// under the strict policy — if usage_reasoning_unknown still fired, the
	// strict render would fail.
	context := testExchangeContext()
	context.RequestedClientModel = "m"
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
	}}
	messagesRendered, messagesReport, err := RenderMessagesResponse(response, context)
	if err != nil {
		t.Fatalf("strict messages render: %v", err)
	}
	responsesRendered, responsesReport, err := RenderResponsesResponse(response, context)
	if err != nil {
		t.Fatalf("strict responses render: %v", err)
	}
	for name, report := range map[string]ConversionReport{
		"messages":  messagesReport,
		"responses": responsesReport,
	} {
		if reportHasFeature(report, FeatureUsageReasoningUnknown) {
			t.Fatalf("%s render recorded %v despite the top-level reasoning signal: %+v",
				name, FeatureUsageReasoningUnknown, report.Losses)
		}
	}

	// Zero leakage: no provider-extension key that has no canonical home in
	// either client dialect appears in any rendered body. (reasoning_tokens
	// and cached_tokens exist legitimately as nested detail fields; the
	// DeepSeek cache pair never does.)
	for _, rendered := range [][]byte{messagesRendered, responsesRendered} {
		for _, forbidden := range []string{"prompt_cache_hit_tokens", "prompt_cache_miss_tokens"} {
			if bytes.Contains(rendered, []byte(forbidden)) {
				t.Fatalf("client render leaks %s: %s", forbidden, rendered)
			}
		}
	}
}

// TestChatUsageExtensionPrecedence pins the value precedence: the pinned
// detail objects win over the top-level provider extensions when both are
// present (exactly one signal reaches the IR), and cached_tokens outranks
// prompt_cache_hit_tokens when there are no details.
func TestChatUsageExtensionPrecedence(t *testing.T) {
	body := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"logprobs":null,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"reasoning_tokens":99,"cached_tokens":9,"prompt_cache_hit_tokens":8,"completion_tokens_details":{"accepted_prediction_tokens":0,"audio_tokens":0,"reasoning_tokens":7,"rejected_prediction_tokens":0},"prompt_tokens_details":{"cached_tokens":2}}}`
	response, _, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !response.Usage.ReasoningKnown || response.Usage.ReasoningTokens != 7 {
		t.Fatalf("reasoning = %+v, want details value 7", response.Usage)
	}
	if !response.Usage.CacheReadKnown || response.Usage.CacheReadTokens != 2 {
		t.Fatalf("cache read = %+v, want details value 2", response.Usage)
	}

	noDetails := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"logprobs":null,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"cached_tokens":9,"prompt_cache_hit_tokens":8}}`
	response, _, err = DecodeChatResponseWithPolicy([]byte(noDetails), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !response.Usage.CacheReadKnown || response.Usage.CacheReadTokens != 9 {
		t.Fatalf("cache read = %+v, want cached_tokens value 9", response.Usage)
	}
}

// TestChatUsageDeepSeekCacheConvention proves the third cache convention:
// prompt_cache_hit_tokens makes the cache-read component known with hit =
// cached-read semantics (DeepSeek), and prompt_cache_miss_tokens decodes
// without a canonical home (derivable uncached prompt) and never leaks.
func TestChatUsageDeepSeekCacheConvention(t *testing.T) {
	body := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"logprobs":null,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"reasoning_tokens":2,"prompt_cache_hit_tokens":4,"prompt_cache_miss_tokens":6}}`
	response, _, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !response.Usage.CacheReadKnown || response.Usage.CacheReadTokens != 4 {
		t.Fatalf("cache read = %+v, want hit value 4", response.Usage)
	}
	if !response.Usage.ReasoningKnown || response.Usage.ReasoningTokens != 2 {
		t.Fatalf("reasoning = %+v, want top-level value 2", response.Usage)
	}
	context := testExchangeContext()
	context.RequestedClientModel = "m"
	// The fixture carries no created_cache_tokens, so the Messages render
	// fires the unrelated cache-write unknown loss under strict policy;
	// approve exactly that loss so the render succeeds and the leakage
	// assertion below actually runs against a body carrying the DeepSeek
	// keys.
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheWriteUnknown: {},
	}}
	rendered, report, err := RenderMessagesResponse(response, context)
	if err != nil {
		t.Fatalf("messages render with approved cache-write loss: %v", err)
	}
	if !reportHasFeature(report, FeatureUsageCacheWriteUnknown) {
		t.Fatalf("report lacks the expected cache-write loss: %+v", report.Losses)
	}
	if bytes.Contains(rendered, []byte("prompt_cache")) {
		t.Fatalf("client render leaks prompt_cache keys: %s", rendered)
	}
}

// TestChatUsageUnknownFieldStillRejected pins the strict wire contract: an
// unknown usage field that is NOT one of the four decoded extensions still
// fails decode — the leniency ends exactly at the modeled surface.
func TestChatUsageUnknownFieldStillRejected(t *testing.T) {
	body := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"logprobs":null,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"bogus_tokens":1}}`
	_, _, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, StrictLossPolicy())
	if err == nil {
		t.Fatal("decode accepted unknown usage field bogus_tokens")
	}
	if !strings.Contains(err.Error(), "bogus_tokens") {
		t.Fatalf("err = %v, want the offending field named", err)
	}
}
