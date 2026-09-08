package transcode

// THINK-2 (operator-adjudicated design, 2026-09-07): native thinking
// rendering via marker-signature synthesis + request scrubbing.
//
// With the provider_reasoning_thinking capability, provider plaintext
// reasoning renders as NATIVE Anthropic thinking blocks carrying
// SyntheticThinkingSignature, and the Messages request path scrubs
// marker-signature thinking blocks out of replayed history before any
// upstream rendering — the synthetic signature never reaches an upstream.
// Without the capability, behavior is byte-identical to the previous
// provider_reasoning_text mapping.

import (
	"errors"
	"strings"
	"testing"
)

func TestNonStreamReasoningRendersThinkingBlockWithMarker(t *testing.T) {
	response, _, err := DecodeChatResponseWithPolicy([]byte(
		`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"It is sunny.","reasoning_content":"I should check the weather."}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`,
	), ChatCapabilities{ProviderReasoningThinking: true}, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.RequestedClientModel = "m"
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageUnknown:           {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	rendered, _, err := RenderMessagesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), `"type":"thinking"`) {
		t.Fatalf("rendered Messages response lacks a native thinking block: %s", rendered)
	}
	if !strings.Contains(string(rendered), SyntheticThinkingSignature) {
		t.Fatalf("thinking block lacks the marker signature: %s", rendered)
	}
	if !strings.Contains(string(rendered), "I should check the weather.") {
		t.Fatalf("thinking block lost the reasoning text: %s", rendered)
	}
	// Anthropic ordering: the thinking block must PRECEDE the text block.
	// The chat message walk appends the reasoning part after the content
	// parts (reasoning_content follows content on the chat wire); the
	// Messages renderer emits thinking in a first pass. The pre-fix render
	// put thinking AFTER text (live Claude Code conformance, 2026-09-08).
	thinkingIdx := strings.Index(string(rendered), `"type":"thinking"`)
	textIdx := strings.Index(string(rendered), `"type":"text"`)
	if thinkingIdx < 0 || textIdx < 0 || thinkingIdx > textIdx {
		t.Fatalf("thinking block must precede the text block: thinking@%d text@%d in %s", thinkingIdx, textIdx, rendered)
	}
	_ = rendered
}

func TestNonStreamReasoningTextMappingUnchangedWithoutCapability(t *testing.T) {
	response, _, err := DecodeChatResponseWithPolicy([]byte(
		`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"It is sunny.","reasoning_content":"I should check the weather."}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`,
	), ChatCapabilities{ProviderReasoningText: true}, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	context := testExchangeContext()
	context.RequestedClientModel = "m"
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageUnknown:           {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	rendered, _, err := RenderMessagesResponse(response, context)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), `"type":"thinking"`) {
		t.Fatalf("without the capability the mapping must stay ordinary text: %s", rendered)
	}
}

func TestMarkerSignatureThinkingBlocksScrubbedFromReplay(t *testing.T) {
	// A Claude Code replay: the assistant turn carries the thinking block
	// the proxy synthesized (marker signature) followed by the answer text.
	body := []byte(`{
		"model":"m","max_tokens":100,
		"messages":[
			{"role":"user","content":"what weather?"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"I should check the weather.","signature":"` + SyntheticThinkingSignature + `"},
				{"type":"text","text":"It is sunny."}
			]},
			{"role":"user","content":"thanks"}
		]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("a replayed marker-signature thinking block must not fail the request: %v", err)
	}
	// The marker block is scrubbed: no authenticated_thinking artifact, and
	// the assistant turn keeps only the text.
	for _, turn := range result.Request.Turns {
		for _, part := range turn.Parts {
			if ct, ok := part.(CanonicalText); ok && strings.Contains(ct.Text, "I should check the weather.") {
				t.Fatalf("the marker-signature thinking text leaked into the canonical request: %+v", part)
			}
		}
	}
	if len(result.Request.Artifacts.AnthropicThinkingBlocks) != 0 {
		t.Fatalf("marker-signature blocks must not be captured as authenticated artifacts: %+v", result.Request.Artifacts.AnthropicThinkingBlocks)
	}
}

func TestNonMarkerThinkingBlocksKeepAuthenticatedBehavior(t *testing.T) {
	body := []byte(`{
		"model":"m","max_tokens":100,
		"messages":[
			{"role":"user","content":"what weather?"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"genuine artifact","signature":"real-anthropic-signature"},
				{"type":"text","text":"It is sunny."}
			]},
			{"role":"user","content":"thanks"}
		]
	}`)
	result, err := DecodeMessagesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Request.Artifacts.AnthropicThinkingBlocks) != 1 {
		t.Fatalf("non-marker thinking blocks keep the authenticated-artifact behavior: %d blocks", len(result.Request.Artifacts.AnthropicThinkingBlocks))
	}
}

// TestStreamReasoningContiguousDeltasOneThinkingBlock pins the
// CC-FRAGMENTATION fix (operator-observed 2026-09-08): contiguous reasoning
// deltas must render as exactly ONE thinking block — the transition close
// fires only when a delta actually carries content or tool output, never for
// a reasoning-only delta. The pre-fix behavior sealed the reasoning item on
// every reasoning delta, so Claude Code rendered one ∴ fragment per line.
func TestStreamReasoningContiguousDeltasOneThinkingBlock(t *testing.T) {
	chat := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"resp_1",
		"m",
		1710000000,
		nil,
	)
	anthropic := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"msg_1",
		"claude-x",
		1710000000,
	)
	converter := newChatToAnthropicConverter(chat, anthropic)

	frames := []string{
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"frag one "},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"m","choices":[{"index":0,"delta":{"reasoning_content":"frag two "},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"m","choices":[{"index":0,"delta":{"reasoning_content":"frag three"},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"m","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	starts, stops, thinkingDeltas := 0, 0, 0
	for _, f := range frames {
		batch, err := converter.Convert(SSEEvent{Data: []byte(f)})
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range batch.Events {
			switch {
			case ev.Type == "content_block_start" && strings.Contains(string(ev.Data), `"type":"thinking"`):
				starts++
			case ev.Type == "content_block_stop" && strings.Contains(string(ev.Data), `"index":0`):
				stops++
			case ev.Type == "content_block_delta" && strings.Contains(string(ev.Data), `"thinking_delta"`):
				thinkingDeltas++
			}
		}
	}
	if _, err := converter.Convert(SSEEvent{Data: []byte("[DONE]")}); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatalf("thinking content_block_start count = %d, want exactly 1 for contiguous reasoning deltas (one-block-per-delta is the CC-FRAGMENTATION regression)", starts)
	}
	if thinkingDeltas != 3 {
		t.Fatalf("thinking_delta count = %d, want 3 (all fragments in the one block)", thinkingDeltas)
	}
}

func TestStreamReasoningRendersThinkingLifecycle(t *testing.T) {
	chat := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"resp_1",
		"m",
		1710000000,
		nil,
	)
	anthropic := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"msg_1",
		"claude-x",
		1710000000,
	)
	converter := newChatToAnthropicConverter(chat, anthropic)

	frames := []string{
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"checking weather"},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"m","choices":[{"index":0,"delta":{"content":"It is sunny."},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1710000000,"model":"m","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`,
	}
	var kinds []string
	var allFrames []frameEvent
	var signatures []string
	for _, f := range frames {
		batch, err := converter.Convert(SSEEvent{Data: []byte(f)})
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range batch.Events {
			kinds = append(kinds, ev.Type)
			allFrames = append(allFrames, ev)
			if strings.Contains(string(ev.Data), `"signature_delta"`) {
				if i := strings.Index(string(ev.Data), `"signature":"`); i >= 0 {
					rest := string(ev.Data)[i+len(`"signature":"`):]
					if j := strings.Index(rest, `"`); j >= 0 {
						signatures = append(signatures, rest[:j])
					}
				}
			}
		}
	}
	batch, err := converter.Convert(SSEEvent{Data: []byte("[DONE]")})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range batch.Events {
		kinds = append(kinds, ev.Type)
		allFrames = append(allFrames, ev)
		if strings.Contains(string(ev.Data), `"signature_delta"`) {
			if i := strings.Index(string(ev.Data), `"signature":"`); i >= 0 {
				rest := string(ev.Data)[i+len(`"signature":"`):]
				if j := strings.Index(rest, `"`); j >= 0 {
					signatures = append(signatures, rest[:j])
				}
			}
		}
	}

	// The delta SUBTYPES live in the frame data, not the event type.
	joined := strings.Join(kinds, ",")
	var thinkingDeltaIdx, signatureDeltaIdx = -1, -1
	for i, ev := range allFrames {
		data := string(ev.Data)
		if thinkingDeltaIdx < 0 && strings.Contains(data, `"type":"thinking_delta"`) {
			thinkingDeltaIdx = i
		}
		if signatureDeltaIdx < 0 && strings.Contains(data, `"type":"signature_delta"`) {
			signatureDeltaIdx = i
		}
	}
	startIdx := -1
	for i, k := range kinds {
		if k == "content_block_start" {
			startIdx = i
			break
		}
	}
	stopIdx := strings.LastIndex(joined, "content_block_stop")
	if startIdx < 0 || thinkingDeltaIdx < 0 || signatureDeltaIdx < 0 || stopIdx < 0 {
		t.Fatalf("missing thinking lifecycle events: %s", joined)
	}
	if !(startIdx < thinkingDeltaIdx && thinkingDeltaIdx < signatureDeltaIdx && signatureDeltaIdx < stopIdx) {
		t.Fatalf("thinking lifecycle out of order: kinds=%s thinking@%d signature@%d stop@%d", joined, thinkingDeltaIdx, signatureDeltaIdx, stopIdx)
	}
	if len(signatures) == 0 || signatures[0] != SyntheticThinkingSignature {
		t.Fatalf("signature_delta must carry the marker signature: %v", signatures)
	}
}

// TestAnthropicStreamReasoningInterleavedPartsRejected pins the review
// finding (ses_f82433a3affeYcnpN3ETKBmQxz, 2026-09-08): the Responses FSM
// tracks reasoning phase per item, so two items can concurrently hold open
// summary parts, but the Anthropic dialect cannot represent two
// concurrently-open content blocks — the render state holds exactly one
// open thinking block. The pre-fix behavior let the second part.added
// overwrite the block index (misattributing B's deltas into A's block) and
// then panicked on a nil reasoningBlockIndex dereference inside the
// converting reader after A's part.done — killing the whole proxy. The
// rejection now fires at the second part.added as a typed upstream wire
// error, so the panic window can never open.
func TestAnthropicStreamReasoningInterleavedPartsRejected(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"msg_1",
		"claude-x",
		1,
	)
	created := ResponseCreatedEvent{
		Type: "response.created", SequenceNumber: 0,
		Response: ResponseEnvelope{
			ID: "resp_1", Object: "response", CreatedAt: 1, Status: "in_progress", Model: "m",
			Output: []ResponsesOutputItem{},
			Usage: &ResponsesUsage{
				InputTokens: 0, OutputTokens: 0, TotalTokens: 0,
				InputTokensDetails:  &UsageInputTokensDetails{CachedTokens: 0},
				OutputTokensDetails: &UsageOutputTokensDetails{ReasoningTokens: 0},
			},
		},
	}
	if _, err := state.Convert(created); err != nil {
		t.Fatal(err)
	}
	part := ResponsesSummaryTextPart{Type: "summary_text", Text: ""}
	if _, err := state.Convert(ResponseReasoningSummaryPartAddedEvent{
		Type: "response.reasoning_summary_part.added", SequenceNumber: 1, ItemID: "rs_a", OutputIndex: 0, SummaryIndex: 0, Part: part,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := state.Convert(ResponseReasoningSummaryPartAddedEvent{
		Type: "response.reasoning_summary_part.added", SequenceNumber: 2, ItemID: "rs_b", OutputIndex: 1, SummaryIndex: 0, Part: part,
	})
	if err == nil {
		t.Fatal("a second concurrently-open thinking part must be rejected")
	}
	var wire *UpstreamWireError
	if !errors.As(err, &wire) {
		t.Fatalf("failure must be a typed upstream wire error, got %T: %v", err, err)
	}
}

// TestAnthropicStreamReasoningNilIndexGuarded pins the defense-in-depth
// guards: reasoningTextDelta and reasoningPartDone with no open thinking
// block return typed upstream wire errors (mirroring the textDelta
// precedent) instead of dereferencing the nil block index.
func TestAnthropicStreamReasoningNilIndexGuarded(t *testing.T) {
	state := newAnthropicResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"msg_1",
		"claude-x",
		1,
	)
	part := ResponsesSummaryTextPart{Type: "summary_text", Text: ""}
	_, err := state.reasoningTextDelta(ResponseReasoningSummaryTextDeltaEvent{
		EventBase: EventBase{Type: "response.reasoning_summary_text.delta", SequenceNumber: 1},
		ItemID:    "rs_a", OutputIndex: 0, SummaryIndex: 0, Delta: "x",
	})
	if err == nil {
		t.Fatal("reasoningTextDelta with no open thinking block must fail")
	}
	var wire *UpstreamWireError
	if !errors.As(err, &wire) {
		t.Fatalf("reasoningTextDelta failure must be a typed upstream wire error, got %T: %v", err, err)
	}
	_, err = state.reasoningPartDone(ResponseReasoningSummaryPartDoneEvent{
		EventBase: EventBase{Type: "response.reasoning_summary_part.done", SequenceNumber: 2},
		ItemID:    "rs_a", OutputIndex: 0, SummaryIndex: 0, Part: part,
	})
	if err == nil {
		t.Fatal("reasoningPartDone with no open thinking block must fail")
	}
	if !errors.As(err, &wire) {
		t.Fatalf("reasoningPartDone failure must be a typed upstream wire error, got %T: %v", err, err)
	}
}
