package transcode

// Row-35 evidence tests: the chat delta `reasoning_details` array.
//
// LIVE FINDING (2026-09-16, recorded at knowledgeStore.row35_live_check): raw
// streaming captures from the operator gateway (dialagram meta-muse-spark-1.3,
// verboo deepseek-v4-flash-0731) carried reasoning ONLY in the modeled
// `reasoning_content` spelling — `reasoning_details` was never observed at
// all, let alone alone. The pins.md note records it as a SIBLING of the
// modeled text, never a sole carrier.
//
// These tests pin the honest behavior that finding implies: an array-only
// reasoning delta is TOLERATED (never a stream failure) and its array content
// is never surfaced as client output — so nothing is silently mis-attributed
// to content. If a real array-only carrier is ever captured, the row's
// modeling work re-opens; until then the tolerant discard is the documented
// disposition.

import (
	"strings"
	"testing"
)

// arrayOnlyReasoningDelta is a chat stream chunk carrying ONLY the
// reasoning_details array: no `reasoning`, no `reasoning_content`.
const arrayOnlyReasoningDelta = `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning_details":[{"type":"reasoning.text","text":"ARRAY_ONLY_SECRET","format":"unknown","index":0}]}}]}`

// TestChatStreamArrayOnlyReasoningTolerated proves the production chunk
// decoder accepts an array-only reasoning delta (the upstream envelope is
// tolerant) and that neither reasoning spelling is populated from it, so the
// array text cannot leak into client output as model content.
func TestChatStreamArrayOnlyReasoningTolerated(t *testing.T) {
	chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(arrayOnlyReasoningDelta)})
	if err != nil {
		t.Fatalf("array-only reasoning delta rejected by production decode: %v", err)
	}
	if len(chunk.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(chunk.Choices))
	}
	delta := chunk.Choices[0].Delta
	if delta == nil {
		t.Fatal("delta is nil")
	}
	if delta.Reasoning != nil || delta.ReasoningContent != nil {
		t.Fatalf(
			"array-only delta populated a reasoning text field (reasoning=%v reasoning_content=%v); "+
				"the array is a separate carrier and must not be silently mapped into the modeled text",
			delta.Reasoning,
			delta.ReasoningContent,
		)
	}
}

// TestChatStreamArrayOnlyReasoningNeverBecomesOutput proves the array text is
// never emitted as reasoning or content through the chat->responses state
// machine: the array is discarded, so no client-visible event carries it.
func TestChatStreamArrayOnlyReasoningNeverBecomesOutput(t *testing.T) {
	state := newChatResponsesStreamState(
		testStreamContext(),
		j6PermissivePolicy(),
		ChatCapabilities{ProviderReasoningThinking: true},
		"resp_1",
		"gpt-4.1",
		1,
		nil,
	)
	chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(arrayOnlyReasoningDelta)})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	events, err := state.Convert(chunk)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("array-only delta produced no events; the stream would stall")
	}
	for _, event := range events {
		rendered, err := MarshalResponsesEvent(event)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(rendered), "ARRAY_ONLY_SECRET") {
			t.Fatalf("array-only reasoning text leaked into a client event: %s", rendered)
		}
	}
}
