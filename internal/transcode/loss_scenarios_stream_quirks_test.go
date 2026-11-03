package transcode

import (
	"encoding/json"
	"testing"
)

func lossScenariosStreamQuirks(t *testing.T) []lossScenario {
	return []lossScenario{
		{
			// An upstream chat stream that ends after a finishing chunk
			// without the [DONE] sentinel releases the held terminal on EOF
			// and records the provider quirk as an ungated note, so the
			// scenario runs under the strict policy and records the key
			// anyway.
			key:  FeatureMissingStreamSentinel,
			perm: []Feature{},
			note: true,
			run: func(policy LossPolicy) (ConversionReport, error) {
				state := newChatResponsesStreamState(
					testStreamContext(),
					policy,
					ChatCapabilities{},
					"resp_1",
					"gpt-4.1",
					1710000000,
					nil,
				)
				chunk := ChatStreamResponse{
					ID:      "c",
					Object:  "chat.completion.chunk",
					Created: 1710000000,
					Model:   "gpt-4.1",
					Choices: []ChatChoice{{
						Index:        0,
						Delta:        &ChatStreamDelta{Content: new("hi")},
						FinishReason: new("stop"),
					}},
				}
				if _, err := state.Convert(chunk); err != nil {
					return state.report, err
				}
				if _, err := state.FinalizeEOF(); err != nil {
					return state.report, err
				}
				return state.report, nil
			},
		},
		{
			// A data-only upstream Responses stream (the SSE event: name
			// omitted on every frame) routes each event by its decoded JSON
			// type and records the provider quirk as an ungated note. The
			// created envelope's wire JSON cannot carry the in-memory
			// cache-write usage carrier, so the scenario's own-permission run
			// also approves the usage component the Messages contract
			// requires (the note itself needs no permission).
			key:  FeatureMissingEventName,
			perm: []Feature{FeatureUsageCacheWriteUnknown},
			note: true,
			run: func(policy LossPolicy) (ConversionReport, error) {
				state := newAnthropicResponsesStreamState(
					testStreamContext(), policy, ChatCapabilities{},
					"msg_1", "claude-x", 1710000000,
				)
				converter := newResponsesToAnthropicConverter(state)
				envelope := anthropicLifecycleEnvelope("resp_1")
				envelope.Usage = &ResponsesUsage{
					InputTokens:        10,
					OutputTokens:       5,
					TotalTokens:        15,
					InputTokensDetails: &UsageInputTokensDetails{CachedTokens: 0},
					OutputTokensDetails: &UsageOutputTokensDetails{
						ReasoningTokens: 0,
					},
					CreatedCacheTokens: new(int64(0)),
				}
				payload, err := json.Marshal(ResponseCreatedEvent{
					Type:           "response.created",
					SequenceNumber: 0,
					Response:       envelope,
				})
				if err != nil {
					return state.report, err
				}
				if _, err := converter.Convert(SSEEvent{Data: payload}); err != nil {
					return state.report, err
				}
				return state.report, nil
			},
		},
		{
			// A usage-only tail that omits exactly one total has it DERIVED
			// from the two present values, recorded as the ungated
			// usage_total_derived note (so the scenario runs under the
			// strict policy). The pinned Messages contract needs the
			// cache-write component the wire cannot carry, so the
			// own-permission run also approves that usage key.
			key: FeatureUsageTotalDerived,
			perm: []Feature{
				FeatureUsageCacheWriteUnknown,
				FeatureUsageCacheReadUnknown,
				FeatureUsageReasoningUnknown,
			},
			note: true,
			run: func(policy LossPolicy) (ConversionReport, error) {
				state := newChatResponsesStreamState(
					testStreamContext(),
					policy,
					ChatCapabilities{},
					"resp_1",
					"gpt-4.1",
					1710000000,
					nil,
				)
				// Phase 1: a content-bearing finish chunk.
				finish, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(
					`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
				)})
				if err != nil {
					return state.report, err
				}
				if _, err := state.Convert(finish); err != nil {
					return state.report, err
				}
				// Phase 2: the derived usage-only tail.
				tail, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(
					`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
				)})
				if err != nil {
					return state.report, err
				}
				if _, err := state.Convert(tail); err != nil {
					return state.report, err
				}
				return state.report, nil
			},
		},
	}
}
