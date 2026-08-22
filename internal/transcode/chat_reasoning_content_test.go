package transcode

// The DeepSeek/Qwen reasoning_content provider extension (field regression
// 2026-08-22): real open-weights gateways stream delta.reasoning_content and
// return message.reasoning_content on non-streaming completions. The strict
// chat wire decode rejected the field as unknown on BOTH surfaces — streams
// died with an SSE error event at TTFB and non-streaming completions became
// 502s that discarded finished generations. These tests pin the decode-side
// fix: reasoning_content mirrors the existing OpenRouter-style `reasoning`
// semantics exactly (ProviderReasoningText capability gates mapping to
// ordinary text; otherwise an approved loss or a typed rejection), a body
// carrying two non-empty spellings at once is a typed contradiction, and the
// extension bytes never reach rendered client output.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openaichat"
)

// qwenReasoningChunkFrame is a byte-faithful qwen-style streaming chunk: the
// delta carries role and the DeepSeek/Qwen reasoning_content spelling.
const qwenReasoningChunkFrame = `{"id":"chatcmpl-q","object":"chat.completion.chunk","created":1710000000,"model":"qwen","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"let me think"},"finish_reason":null}]}`

// TestChatStreamChunkReasoningContentDecodes is the reproduce-or-fail pin for
// the observed stream failure: a chunk whose delta carries reasoning_content
// must decode through chatStreamChunkFromSSE (the strict shadow) instead of
// failing as an unknown field.
func TestChatStreamChunkReasoningContentDecodes(t *testing.T) {
	chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(qwenReasoningChunkFrame)})
	if err != nil {
		t.Fatalf("reasoning_content chunk rejected: %v", err)
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].Delta == nil {
		t.Fatalf("choices = %+v", chunk.Choices)
	}
	got := chunk.Choices[0].Delta.ReasoningContent
	if got == nil || *got != "let me think" {
		t.Fatalf("delta.reasoning_content = %v, want %q", got, "let me think")
	}

	// The wire type decodes the same value directly (honesty rule: assert,
	// never just accept).
	var probe openaichat.StreamChunk
	if err := json.Unmarshal([]byte(qwenReasoningChunkFrame), &probe); err != nil {
		t.Fatalf("wire chunk decode: %v", err)
	}
	if probe.Choices[0].Delta.ReasoningContent == nil ||
		*probe.Choices[0].Delta.ReasoningContent != "let me think" {
		t.Fatalf("wire delta.reasoning_content = %v", probe.Choices[0].Delta.ReasoningContent)
	}
}

// TestDecodeChatResponseReasoningContentMirrorsReasoning is the
// reproduce-or-fail pin for the observed non-stream failure: a completion
// whose message carries reasoning_content decodes end-to-end and follows the
// exact ProviderReasoningText semantics of message.reasoning.
func TestDecodeChatResponseReasoningContentMirrorsReasoning(t *testing.T) {
	body := []byte(`{
		"id":"x","object":"chat.completion","created":1,"model":"m",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"answer","reasoning_content":"deep think"}}]
	}`)

	// Wire type: the value decodes (honesty rule).
	var probe openaichat.Response
	if err := wire.Decode(body, &probe); err != nil {
		t.Fatalf("wire decode: %v", err)
	}
	if probe.Choices[0].Message.ChatAssistantMessage == nil ||
		probe.Choices[0].Message.ChatAssistantMessage.ReasoningContent == nil ||
		*probe.Choices[0].Message.ChatAssistantMessage.ReasoningContent != "deep think" {
		t.Fatalf("message.reasoning_content did not decode: %+v", probe.Choices[0].Message)
	}

	// Without the capability: the same typed rejection as message.reasoning,
	// naming the actual field.
	_, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, StrictLossPolicy())
	var unsupported *UnsupportedFeatureError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %T %v, want *UnsupportedFeatureError", err, err)
	}
	if unsupported.Path != "choices[].message.reasoning_content" {
		t.Fatalf("path = %q, want choices[].message.reasoning_content", unsupported.Path)
	}

	// With the capability (a CLI default): mapped to ordinary text.
	response, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{ProviderReasoningText: true}, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode with capability: %v", err)
	}
	message, ok := response.Items[0].(*CanonicalMessageItem)
	if !ok {
		t.Fatalf("item = %T", response.Items[0])
	}
	found := false
	for _, part := range message.Parts {
		if text, ok := part.(CanonicalText); ok && text.Text == "deep think" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasoning_content text missing from parts: %+v", message.Parts)
	}

	// The extension bytes never leak into a rendered client response.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageUnknown:           {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	rendered, _, err := RenderMessagesResponse(response, context)
	if err != nil {
		t.Fatalf("RenderMessagesResponse: %v", err)
	}
	if strings.Contains(string(rendered), "reasoning_content") {
		t.Fatalf("client response leaks reasoning_content: %s", rendered)
	}
}

// TestChatStreamReasoningContentReportedOnce proves the reasoning_content
// spelling flows through the identical capability-gated block: TextDelta
// events with the capability (one Note per stream), one approved Lose entry
// without it, both reported under the actual field path.
func TestChatStreamReasoningContentReportedOnce(t *testing.T) {
	t.Run("loss without capability", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			LossPolicy{Allowed: map[Feature]struct{}{FeatureProviderReasoningText: {}}},
			ChatCapabilities{},
			"resp_1",
			"m",
			1,
			nil,
		)
		for i := range 3 {
			if _, err := state.Convert(chatChunk(t, ChatStreamDelta{ReasoningContent: new("think")}, nil)); err != nil {
				t.Fatalf("delta %d: %v", i, err)
			}
		}
		losses := state.report.Losses
		count := 0
		for _, loss := range losses {
			if loss.Feature == FeatureProviderReasoningText {
				count++
				if loss.Path != "choices[].delta.reasoning_content" {
					t.Fatalf("loss path = %q, want choices[].delta.reasoning_content", loss.Path)
				}
			}
		}
		if count != 1 {
			t.Fatalf("reasoning_content losses = %d, want exactly one", count)
		}
	})
	t.Run("text with capability", func(t *testing.T) {
		state := newChatResponsesStreamState(
			testStreamContext(),
			StrictLossPolicy(),
			ChatCapabilities{ProviderReasoningText: true},
			"resp_1",
			"m",
			1,
			nil,
		)
		events, err := state.Convert(chatChunk(t, ChatStreamDelta{Role: new("assistant"), ReasoningContent: new("think")}, nil))
		if err != nil {
			t.Fatal(err)
		}
		sawDelta := false
		for _, event := range events {
			if delta, ok := event.(ResponseTextDeltaEvent); ok && delta.Delta == "think" {
				sawDelta = true
			}
		}
		if !sawDelta {
			t.Fatalf("no text delta carrying the reasoning text among %d events", len(events))
		}
		for i := range 2 {
			if _, err := state.Convert(chatChunk(t, ChatStreamDelta{ReasoningContent: new(" more")}, nil)); err != nil {
				t.Fatalf("delta %d: %v", i, err)
			}
		}
		count := 0
		for _, entry := range state.report.Losses {
			if entry.Feature == FeatureProviderReasoningText {
				count++
				if entry.Path != "choices[].delta.reasoning_content" {
					t.Fatalf("note path = %q, want choices[].delta.reasoning_content", entry.Path)
				}
			}
		}
		if count != 1 {
			t.Fatalf("reasoning_content notes = %d, want exactly one per stream", count)
		}
	})
}

// TestChatStreamReasoningSpellingContradictionRejected pins that a delta
// carrying two non-empty reasoning spellings at once is corrupt upstream wire
// — two conflicting texts must never be merged or silently ordered.
func TestChatStreamReasoningSpellingContradictionRejected(t *testing.T) {
	frame := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"reasoning":"a","reasoning_content":"b"},"finish_reason":null}]}`
	if _, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(frame)}); err != nil {
		assertChatStreamChunkWireError(t, err)
		return
	}
	// The SSE decode accepts both spellings individually present; the
	// contradiction surfaces when the state machine consumes the delta.
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{ProviderReasoningText: true},
		"resp_1",
		"m",
		1,
		nil,
	)
	chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(frame)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Convert(chunk); err == nil {
		t.Fatal("expected contradiction rejection for reasoning + reasoning_content in one delta")
	} else {
		assertChatStreamChunkWireError(t, err)
	}
}

// TestDecodeChatResponseReasoningContradictionRejected pins the same
// contradiction rule on the non-streaming surface.
func TestDecodeChatResponseReasoningContradictionRejected(t *testing.T) {
	body := []byte(`{
		"id":"x","object":"chat.completion","created":1,"model":"m",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"answer","reasoning":"a","reasoning_content":"b"}}]
	}`)
	if _, _, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{ProviderReasoningText: true}, StrictLossPolicy()); err == nil {
		t.Fatal("expected contradiction rejection for reasoning + reasoning_content in one message")
	}
}

// TestChatProviderReasoningCapabilityOffParity pins the task-22
// de-asymmetry: a non-stream chat response carrying plaintext reasoning
// while the ProviderReasoningText capability is off now follows the STREAM
// disposition — an approved provider_reasoning_text loss with the reasoning
// dropped and the ordinary content still rendered, sharing the same loss
// detail text — instead of a hard local rejection. The clause matches the
// streaming surface's capability-off clause (same loss feature, same detail);
// the both-spellings contradiction stays a hard reject on both surfaces.
func TestChatProviderReasoningCapabilityOffParity(t *testing.T) {
	approved := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureProviderReasoningText: {},
	}}
	body := []byte(`{
		"id":"x","object":"chat.completion","created":1,"model":"m",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"answer","reasoning_content":"deep think"}}]
	}`)

	nonStream, report, err := DecodeChatResponseWithPolicy(body, ChatCapabilities{}, approved)
	if err != nil {
		t.Fatalf("decode with an approved provider_reasoning_text loss: %v", err)
	}
	// The reasoning is dropped; the ordinary content still renders.
	var found strings.Builder
	for _, part := range nonStream.Items[0].(*CanonicalMessageItem).Parts {
		if text, ok := part.(CanonicalText); ok {
			found.WriteString(text.Text)
		}
	}
	if found.String() != "answer" {
		t.Fatalf("non-stream items = %q, want only the answer with the reasoning dropped", found.String())
	}
	// The loss is recorded with the shared detail text.
	detail := ""
	path := ""
	lossCount := 0
	for _, loss := range report.Losses {
		if loss.Feature == FeatureProviderReasoningText {
			lossCount++
			detail = loss.Detail
			path = loss.Path
		}
	}
	if lossCount != 1 {
		t.Fatalf("provider_reasoning_text losses = %d (%+v), want exactly one", lossCount, report.Losses)
	}
	if detail != chatProviderReasoningDroppedDetail {
		t.Fatalf("loss detail = %q, want the shared %q", detail, chatProviderReasoningDroppedDetail)
	}
	if path != "choices[].message.reasoning_content" {
		t.Fatalf("loss path = %q, want choices[].message.reasoning_content", path)
	}

	// The streaming surface's capability-off clause records the same feature
	// with the same detail text (this is the disposition now shared).
	stream := newChatResponsesStreamState(
		testStreamContext(),
		approved,
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	if _, err := stream.Convert(chatChunk(t, ChatStreamDelta{ReasoningContent: new("deep think")}, nil)); err != nil {
		t.Fatalf("stream convert: %v", err)
	}
	streamDetail := ""
	streamCount := 0
	for _, loss := range stream.report.Losses {
		if loss.Feature == FeatureProviderReasoningText {
			streamCount++
			streamDetail = loss.Detail
		}
	}
	if streamCount != 1 {
		t.Fatalf("stream provider_reasoning_text losses = %d (%+v), want exactly one", streamCount, stream.report.Losses)
	}
	if streamCount == 1 && detail != streamDetail {
		t.Fatalf("non-stream detail %q != stream detail %q", detail, streamDetail)
	}

	// The both-spellings contradiction stays a hard reject on the non-stream
	// surface, exactly like the stream surface — an approved loss cannot waive
	// contradictory upstream wire.
	both := []byte(`{
		"id":"x","object":"chat.completion","created":1,"model":"m",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"answer","reasoning":"a","reasoning_content":"b"}}]
	}`)
	if _, _, err := DecodeChatResponseWithPolicy(both, ChatCapabilities{ProviderReasoningText: true}, approved); err == nil {
		t.Fatal("non-stream accepted two reasoning spellings with an approved loss")
	}
}
