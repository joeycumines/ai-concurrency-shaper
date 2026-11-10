package transcode

import (
	"encoding/json"
	"testing"
)

// chatAudioOutputBody is a chat completion whose assistant message
// carries audio output (transcript plus base64 data): the shared body for
// the provider_audio_transcript Note scenario and the provider_audio_data
// loss scenario.
var chatAudioOutputBody = []byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
	`"choices":[{"index":0,"finish_reason":"stop",` +
	`"message":{"role":"assistant","content":"hi",` +
	`"audio":{"id":"a1","data":"AAAA","expires_at":2,"transcript":"spoken hi"}}}],` +
	`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)

func lossScenariosReasoning(t *testing.T) []lossScenario {
	return []lossScenario{
		{
			key:  FeatureProviderReasoningText,
			perm: []Feature{FeatureProviderReasoningText},
			run: func(policy LossPolicy) (ConversionReport, error) {
				state := newChatResponsesStreamState(
					testStreamContext(),
					policy,
					ChatCapabilities{},
					"resp_1",
					"m",
					1,
					nil,
				)
				_, err := state.Convert(chatChunk(t, ChatStreamDelta{Reasoning: new("think")}, nil))
				return state.report, err
			},
		},
		{
			// Upstream chat audio output: the transcript Note records
			// without any policy decision, so the perm list holds only
			// the sibling data loss the exchange separately needs.
			key:  FeatureProviderAudioTranscript,
			perm: []Feature{FeatureProviderAudioData},
			note: true,
			run: func(policy LossPolicy) (ConversionReport, error) {
				_, report, err := DecodeChatResponseWithPolicy(chatAudioOutputBody, ChatCapabilities{}, policy, nil)
				return report, err
			},
		},
		{
			// The base64 audio data has no target field: an approved
			// loss under its own permission, a rejection under strict.
			key:  FeatureProviderAudioData,
			perm: []Feature{FeatureProviderAudioData},
			run: func(policy LossPolicy) (ConversionReport, error) {
				_, report, err := DecodeChatResponseWithPolicy(chatAudioOutputBody, ChatCapabilities{}, policy, nil)
				return report, err
			},
		},
		{
			key:  FeatureRequestReasoning,
			perm: []Feature{FeatureRequestReasoning},
			run: func(policy LossPolicy) (ConversionReport, error) {
				// An explicit enabled thinking budget crossing to a Chat
				// provider without the reasoning_effort capability (the
				// request-side reasoning control).
				result, err := DecodeMessagesRequest([]byte(
					`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":4096}}`,
				), policy)
				if err != nil {
					return ConversionReport{}, err
				}
				ctx := testExchangeContext()
				ctx.LossPolicy = policy
				_, report, err := RenderChatRequest(result.Request, ctx, ChatCapabilities{})
				return report, err
			},
		},
		{
			key: FeatureReasoningSummary,
			perm: []Feature{
				FeatureReasoningSummary,
				FeatureUsageCacheReadUnknown,
				FeatureUsageCacheWriteUnknown,
				FeatureUsageReasoningUnknown,
				FeatureUsageUnknown,
			},
			run: func(policy LossPolicy) (ConversionReport, error) {
				response := CanonicalResponse{
					ID:     "resp_1",
					Model:  "m",
					Status: CanonicalResponseCompleted,
					Stop:   CanonicalStop{Reason: CanonicalStopEndTurn},
					Items: []CanonicalResponseItem{
						&CanonicalReasoningItem{Raw: json.RawMessage(`{"type":"reasoning"}`)},
						&CanonicalMessageItem{
							Role:  CanonicalAssistant,
							Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
						},
					},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				context.RequestedClientModel = "m"
				_, report, err := RenderMessagesResponse(response, context)
				return report, err
			},
		},
	}
}
