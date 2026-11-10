package transcode

import (
	"encoding/json"
	"testing"
)

func lossScenariosMessagesUsage(t *testing.T) []lossScenario {
	return []lossScenario{
		{
			key: FeatureOutputItemBoundaries,
			perm: []Feature{
				FeatureOutputItemBoundaries,
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
					Stop:   CanonicalStop{Reason: CanonicalStopToolUse},
					Items: []CanonicalResponseItem{
						&CanonicalFunctionCallItem{
							CallID: "call_1",
							Name:   "f",
							Arguments: ToolArguments{
								Raw:      `{}`,
								Object:   json.RawMessage(`{}`),
								IsObject: true,
							},
						},
						&CanonicalFunctionResultItem{
							CallID: "call_1",
							Parts:  []CanonicalPart{CanonicalText{Text: "ok"}},
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
		{
			key: FeatureOutputPhase,
			perm: []Feature{
				FeatureOutputPhase,
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
					Items: []CanonicalResponseItem{&CanonicalMessageItem{
						Role:  CanonicalAssistant,
						Phase: Optional[string]{Value: "commentary", Set: true},
						Parts: []CanonicalPart{CanonicalText{Text: "thinking"}},
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				context.RequestedClientModel = "m"
				_, report, err := RenderMessagesResponse(response, context)
				return report, err
			},
		},
		{
			key: FeatureUsageUnknown,
			perm: []Feature{
				FeatureUsageUnknown,
				FeatureUsageCacheReadUnknown,
				FeatureUsageCacheWriteUnknown,
				FeatureUsageReasoningUnknown,
			},
			run: func(policy LossPolicy) (ConversionReport, error) {
				response := CanonicalResponse{
					ID:     "resp_1",
					Model:  "m",
					Status: CanonicalResponseCompleted,
					Stop:   CanonicalStop{Reason: CanonicalStopEndTurn},
					Items: []CanonicalResponseItem{&CanonicalMessageItem{
						Role:  CanonicalAssistant,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				context.RequestedClientModel = "m"
				_, report, err := RenderMessagesResponse(response, context)
				return report, err
			},
		},
		{
			// A total that is not prompt + completion is relayed as-is and
			// recorded by the usage clamp; the note needs no policy approval
			// (Notes are sanctioned encodings), so the scenario runs with an
			// empty allow-set and every usage component known, and records the
			// key anyway.
			key:  FeatureUsageTotalMismatch,
			perm: []Feature{},
			note: true,
			run: func(policy LossPolicy) (ConversionReport, error) {
				chunk := chatChunk(t, ChatStreamDelta{Content: new("x")}, nil)
				chunk.Usage = &ChatLLMUsage{
					PromptTokens:     10,
					CompletionTokens: 5,
					TotalTokens:      20,
					PromptTokensDetails: &ChatPromptTokensDetails{
						CachedTokens:       0,
						CreatedCacheTokens: new(0),
					},
					CompletionTokensDetails: &ChatCompletionTokensDetails{
						ReasoningTokens: 0,
					},
				}
				state := newChatResponsesStreamState(
					testStreamContext(), policy, ChatCapabilities{},
					"resp_1", "gpt-4.1", 1710000000, nil,
				)
				if _, err := state.Convert(chunk); err != nil {
					return state.report, err
				}
				return state.report, nil
			},
		},
		{
			// A cached breakdown exceeding the
			// input total is clamped into the Messages invariants and recorded
			// as an ungated note. The note needs no policy approval, so the
			// scenario runs under the strict policy with every usage component
			// known (no gated loss is in play) and records the key anyway.
			key:  FeatureUsageCacheExceedsInput,
			perm: []Feature{},
			note: true,
			run: func(policy LossPolicy) (ConversionReport, error) {
				response := CanonicalResponse{
					ID:     "resp_1",
					Model:  "m",
					Status: CanonicalResponseCompleted,
					Stop:   CanonicalStop{Reason: CanonicalStopEndTurn},
					Items: []CanonicalResponseItem{&CanonicalMessageItem{
						Role:  CanonicalAssistant,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
					Usage: CanonicalUsage{
						InputKnown:      true,
						InputTokens:     10,
						CacheReadKnown:  true,
						CacheReadTokens: 11,
						CacheWriteKnown: true,
						OutputKnown:     true,
						OutputTokens:    5,
						ReasoningKnown:  true,
						TotalKnown:      true,
						TotalTokens:     15,
					},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				context.RequestedClientModel = "m"
				_, report, err := RenderMessagesResponse(response, context)
				return report, err
			},
		},
		{
			// Negative upstream counts are clamped to zero and recorded
			// as an ungated note on the stream surface. The scenario feeds a
			// response.created envelope whose usage carries a negative input;
			// the in-memory cache-write carrier and the reasoning details are
			// provided so no gated loss is in play and the strict policy run
			// still records the note.
			key:  FeatureUsageNegativeCounts,
			perm: []Feature{},
			note: true,
			run: func(policy LossPolicy) (ConversionReport, error) {
				state := newAnthropicResponsesStreamState(
					testStreamContext(), policy, ChatCapabilities{},
					"msg_1", "claude-x", 1710000000,
				)
				envelope := anthropicLifecycleEnvelope("resp_1")
				envelope.Usage = &ResponsesUsage{
					InputTokens:        -1,
					OutputTokens:       5,
					TotalTokens:        4,
					InputTokensDetails: &UsageInputTokensDetails{CachedTokens: 0},
					OutputTokensDetails: &UsageOutputTokensDetails{
						ReasoningTokens: 0,
					},
					CreatedCacheTokens: new(int64(0)),
				}
				if _, err := state.Convert(ResponseCreatedEvent{
					Type: "response.created", SequenceNumber: 0,
					Response: envelope,
				}); err != nil {
					return state.report, err
				}
				return state.report, nil
			},
		},
		{
			// the report-overflow bound: the aggregated overflow note is reachable by
			// simply saturating the report; it is a Note (no policy gate).
			key:  FeatureReportOverflow,
			perm: []Feature{},
			note: true,
			run: func(policy LossPolicy) (ConversionReport, error) {
				var report ConversionReport
				for i := 0; i <= maxStreamConversionReportEntries; i++ {
					if err := report.Note(FeatureUsageUnknown, "p", "d"); err != nil {
						return report, err
					}
				}
				return report, nil
			},
		},
		{
			key: FeatureUsageCacheReadUnknown,
			perm: []Feature{
				FeatureUsageCacheReadUnknown,
				FeatureUsageCacheWriteUnknown,
				FeatureUsageReasoningUnknown,
			},
			run: func(policy LossPolicy) (ConversionReport, error) {
				response := CanonicalResponse{
					ID:     "resp_1",
					Model:  "m",
					Status: CanonicalResponseCompleted,
					Stop:   CanonicalStop{Reason: CanonicalStopEndTurn},
					Items: []CanonicalResponseItem{&CanonicalMessageItem{
						Role:  CanonicalAssistant,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
					Usage: CanonicalUsage{
						InputTokens: 5, InputKnown: true,
						OutputTokens: 2, OutputKnown: true,
						TotalTokens: 7, TotalKnown: true,
					},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				context.RequestedClientModel = "m"
				_, report, err := RenderMessagesResponse(response, context)
				return report, err
			},
		},
		{
			key: FeatureUsageCacheWriteUnknown,
			perm: []Feature{
				FeatureUsageCacheReadUnknown,
				FeatureUsageCacheWriteUnknown,
				FeatureUsageReasoningUnknown,
			},
			run: func(policy LossPolicy) (ConversionReport, error) {
				response := CanonicalResponse{
					ID:     "resp_1",
					Model:  "m",
					Status: CanonicalResponseCompleted,
					Stop:   CanonicalStop{Reason: CanonicalStopEndTurn},
					Items: []CanonicalResponseItem{&CanonicalMessageItem{
						Role:  CanonicalAssistant,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
					Usage: CanonicalUsage{
						InputTokens: 5, InputKnown: true,
						OutputTokens: 2, OutputKnown: true,
						TotalTokens: 7, TotalKnown: true,
					},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				context.RequestedClientModel = "m"
				_, report, err := RenderMessagesResponse(response, context)
				return report, err
			},
		},
		{
			key: FeatureUsageReasoningUnknown,
			perm: []Feature{
				FeatureUsageCacheReadUnknown,
				FeatureUsageCacheWriteUnknown,
				FeatureUsageReasoningUnknown,
			},
			run: func(policy LossPolicy) (ConversionReport, error) {
				response := CanonicalResponse{
					ID:     "resp_1",
					Model:  "m",
					Status: CanonicalResponseCompleted,
					Stop:   CanonicalStop{Reason: CanonicalStopEndTurn},
					Items: []CanonicalResponseItem{&CanonicalMessageItem{
						Role:  CanonicalAssistant,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
					Usage: CanonicalUsage{
						InputTokens: 5, InputKnown: true,
						OutputTokens: 2, OutputKnown: true,
						TotalTokens: 7, TotalKnown: true,
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
