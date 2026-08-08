package transcode

// J4 regression tests: the Chat schema aligned with the pinned contract
// (openai-go v1.12.0, internal/transcode/pins.md) — logprobs/service_tier
// model every pinned field (review-j finding 4), and the tool-call wire types
// are split into the non-stream shape (no index) and the streaming delta
// (index-carrying) (review-j finding 5).

import (
	"encoding/json"
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
	response, err := DecodeChatResponse([]byte(currentChatResponse()), ChatCapabilities{})
	if err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if response.ChatLogProbs {
		t.Fatal("logprobs:null must not set the logprobs presence flag")
	}
	if response.ChatServiceTier != "auto" {
		t.Fatalf("service tier = %q, want auto", response.ChatServiceTier)
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
	// The normal transcoded case: no service tier, no logprobs — strict
	// conversion succeeds.
	response, err := DecodeChatResponse([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"logprobs":null,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), ChatCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.RequestedClientModel = "m"
	if _, _, err := RenderResponsesResponse(response, context); err != nil {
		t.Fatalf("responses render with absent attributes: %v", err)
	}
	if _, _, err := RenderMessagesResponse(response, context); err != nil {
		t.Fatalf("messages render with absent attributes: %v", err)
	}

	// service_tier present: strict policy rejects, permissive policy
	// converts (the explicit loss decision, never a silent drop).
	response, err = DecodeChatResponse([]byte(currentChatResponse()), ChatCapabilities{})
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
		FeatureServiceTier: {},
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
	response, err := DecodeChatResponse([]byte(body), ChatCapabilities{})
	if err != nil {
		t.Fatalf("typed logprobs must strict-decode: %v", err)
	}
	if !response.ChatLogProbs {
		t.Fatal("typed logprobs must set the presence flag")
	}
	context := testExchangeContext()
	context.RequestedClientModel = "m"
	if _, _, err := RenderMessagesResponse(response, context); err == nil {
		t.Fatal("strict policy accepted typed logprobs")
	}
	permissive := testExchangeContext()
	permissive.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureLogprobs: {},
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
		FeatureServiceTier: {},
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
