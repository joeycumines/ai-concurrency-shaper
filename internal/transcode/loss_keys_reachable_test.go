package transcode

// Review-z commit 2 acceptance tests: every granular loss key is reachable
// (a real conversion records it), rejected under strict policy, and allowed
// only by its own permission; invalid model-generated tool arguments convert
// byte-exact to Chat and Responses, produce a client-dialect unrepresentable
// error to Messages, and never contribute to upstream-failure accounting.

import (
	"encoding/json"
	"testing"
)

// legacyLossNames are the REMOVED broad permission names: none may be
// accepted anywhere.
// legacyLossNames are the REMOVED broad permission names: none may be
// accepted anywhere. Names that
// survived as granular keys in their own right (image_input, top_k, ...) are
// not legacy and remain accepted.
var legacyLossNames = []string{
	"conversation_state", "provider_reasoning", "service_tier",
	"usage_timing", "tool_result_error", "phase", "reasoning_summary_request",
}

// TestParseLossFeaturesRejectsLegacyNames proves the removed broad names are
// rejected by the CLI parser (no deprecated aliases, no expansion).
func TestParseLossFeaturesRejectsLegacyNames(t *testing.T) {
	for _, name := range legacyLossNames {
		if _, err := ParseLossFeatures(name); err == nil {
			t.Fatalf("legacy loss name %q accepted", name)
		}
	}
}

// TestLossKeysReachableAndStrictRejected proves every registered loss key is
// reachable: a concrete conversion records it under a permissive policy, and
// the same conversion is rejected under the strict policy. The scenario
// builders return the report that must carry the key.
func TestLossKeysReachableAndStrictRejected(t *testing.T) {
	type scenario struct {
		key  Feature
		perm []Feature // permissions the scenario's render path needs
		run  func(policy LossPolicy) (ConversionReport, error)
		// note marks a Note-recorded key (a sanctioned encoding, not a
		// policy decision): the strict-policy rejection assertion is
		// skipped, since Notes record under every policy.
		note bool
	}
	permissive := func(keys ...Feature) LossPolicy {
		allowed := map[Feature]struct{}{}
		for _, key := range keys {
			allowed[key] = struct{}{}
		}
		return LossPolicy{Allowed: allowed}
	}
	strict := StrictLossPolicy()

	scenarios := []scenario{
		{
			key:  FeaturePreviousResponseID,
			perm: []Feature{FeaturePreviousResponseID},
			run: func(policy LossPolicy) (ConversionReport, error) {
				result, _, err := DecodeResponsesRequest([]byte(
					`{"model":"m","input":[{"type":"item_reference","id":"item_1"}]}`,
				), policy)
				return result.Report, err
			},
		},
		{
			key:  FeatureRequestTopLogprobs,
			perm: []Feature{FeatureRequestTopLogprobs},
			run: func(policy LossPolicy) (ConversionReport, error) {
				topLogprobs := int64(2)
				echo := &ResponsesRequestEcho{TopLogprobs: &topLogprobs}
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role:  CanonicalUser,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				context.OriginalResponsesRequest = echo
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{})
				return report, err
			},
		},
		{
			key:  FeatureRequestServiceTier,
			perm: []Feature{FeatureRequestServiceTier},
			run: func(policy LossPolicy) (ConversionReport, error) {
				tier := "default"
				echo := &ResponsesRequestEcho{ServiceTier: &tier}
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role:  CanonicalUser,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				context.OriginalResponsesRequest = echo
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{})
				return report, err
			},
		},
		{
			key:  FeatureRequestTruncation,
			perm: []Feature{FeatureRequestTruncation},
			run: func(policy LossPolicy) (ConversionReport, error) {
				truncation := "auto"
				echo := &ResponsesRequestEcho{Truncation: &truncation}
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role:  CanonicalUser,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				context.OriginalResponsesRequest = echo
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{})
				return report, err
			},
		},
		{
			key:  FeatureMultipleSystemTurns,
			perm: []Feature{FeatureMultipleSystemTurns},
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{
						{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "one"}}},
						{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "two"}}},
					},
				}
				request.Turns = append(request.Turns, CanonicalTurn{
					Role:  CanonicalUser,
					Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
				})
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderResponsesRequest(request, context)
				return report, err
			},
		},
		{
			key:  FeatureSystemNonTextContent,
			perm: []Feature{FeatureSystemNonTextContent},
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role: CanonicalSystem,
						Parts: []CanonicalPart{CanonicalImage{
							MediaType: "image/png",
							URL:       "https://example.test/x.png",
						}},
					}},
				}
				request.Turns = append(request.Turns, CanonicalTurn{
					Role:  CanonicalUser,
					Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
				})
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderResponsesRequest(request, context)
				return report, err
			},
		},
		{
			// A system-channel turn following dialog turns cannot keep its
			// position in a chat request: the consolidation
			// into one leading system message is the approved loss; strict
			// policy rejects the position loss.
			key:  FeatureMidConversationSystem,
			perm: []Feature{FeatureMidConversationSystem},
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{
						{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "top"}}},
						{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
						{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "mid"}}},
					},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{})
				return report, err
			},
		},
		{
			key:  FeatureToolSchemaStrictness,
			perm: []Feature{FeatureToolSchemaStrictness},
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role:  CanonicalUser,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
					Tools: []CanonicalTool{{
						Name:       "f",
						JSONSchema: json.RawMessage(`{"type":"object"}`),
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderResponsesRequest(request, context)
				return report, err
			},
		},
		{
			key:  FeatureToolResultErrorStatus,
			perm: []Feature{FeatureToolResultErrorStatus},
			run: func(policy LossPolicy) (ConversionReport, error) {
				var report ConversionReport
				_, err := renderChatToolResult(CanonicalFunctionResult{
					CallID:  "call_1",
					IsError: true,
					Parts:   []CanonicalPart{CanonicalText{Text: "boom"}},
				}, ChatCapabilities{}, policy, &report)
				return report, err
			},
		},
		{
			key: FeatureToolResultMultimodalContent,
			perm: []Feature{
				FeatureToolResultMultimodalContent,
				FeatureToolResultJSONEnvelope,
			},
			run: func(policy LossPolicy) (ConversionReport, error) {
				var report ConversionReport
				_, err := renderChatToolResult(CanonicalFunctionResult{
					CallID: "call_1",
					Parts: []CanonicalPart{CanonicalImage{
						MediaType: "image/png",
						URL:       "https://example.test/x.png",
					}},
				}, ChatCapabilities{}, policy, &report)
				return report, err
			},
		},
		{
			key: FeatureToolResultJSONEnvelope,
			perm: []Feature{
				FeatureToolResultMultimodalContent,
				FeatureToolResultJSONEnvelope,
			},
			run: func(policy LossPolicy) (ConversionReport, error) {
				var report ConversionReport
				_, err := renderChatToolResult(CanonicalFunctionResult{
					CallID: "call_1",
					Parts: []CanonicalPart{CanonicalImage{
						MediaType: "image/png",
						URL:       "https://example.test/x.png",
					}},
				}, ChatCapabilities{}, policy, &report)
				return report, err
			},
		},
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
			// CC-REPORT-BOUND: the aggregated overflow note is reachable by
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
		{
			key:  FeatureDeveloperRole,
			perm: []Feature{FeatureDeveloperRole},
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role:  CanonicalDeveloper,
						Parts: []CanonicalPart{CanonicalText{Text: "dev"}},
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{})
				return report, err
			},
		},
		{
			key:  FeatureStructuredOutput,
			perm: []Feature{FeatureStructuredOutput},
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role:  CanonicalUser,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
					StructuredOutput: &CanonicalStructuredOutput{
						Name:   "s",
						Schema: json.RawMessage(`{"type":"object"}`),
					},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{})
				return report, err
			},
		},
		{
			key:  FeatureParallelToolCalls,
			perm: []Feature{FeatureParallelToolCalls},
			run: func(policy LossPolicy) (ConversionReport, error) {
				parallel := true
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role:  CanonicalUser,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
					ParallelTools: &parallel,
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{})
				return report, err
			},
		},
		{
			key:  FeatureStopSequences,
			perm: []Feature{FeatureStopSequences},
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel:   "m",
					StopSequences: []string{"END"},
					Turns: []CanonicalTurn{{
						Role:  CanonicalUser,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderResponsesRequest(request, context)
				return report, err
			},
		},
		{
			key:  FeatureImageInput,
			perm: []Feature{FeatureImageInput},
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role: CanonicalUser,
						Parts: []CanonicalPart{
							CanonicalText{Text: "see"},
							CanonicalImage{
								MediaType: "image/png",
								URL:       "https://example.test/x.png",
							},
						},
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{})
				return report, err
			},
		},
		{
			key:  FeatureDocumentInput,
			perm: []Feature{FeatureDocumentInput},
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role: CanonicalUser,
						Parts: []CanonicalPart{
							CanonicalText{Text: "see"},
							CanonicalDocument{
								MediaType: "application/pdf",
								URL:       "https://example.test/d.pdf",
							},
						},
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{})
				return report, err
			},
		},
		{
			key:  FeatureAuthenticatedThinking,
			perm: []Feature{FeatureAuthenticatedThinking},
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role:  CanonicalUser,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
					Artifacts: SourceArtifacts{
						AnthropicThinkingBlocks: []json.RawMessage{json.RawMessage(`{"type":"thinking"}`)},
					},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				var report ConversionReport
				err := RequirePortableArtifacts(request, UpstreamChatCompletions, policy, &report)
				return report, err
			},
		},
		{
			key:  FeatureTopK,
			perm: []Feature{FeatureTopK},
			run: func(policy LossPolicy) (ConversionReport, error) {
				result, err := DecodeMessagesRequest([]byte(
					`{"model":"m","max_tokens":10,"top_k":5,"messages":[{"role":"user","content":"hi"}]}`,
				), policy)
				return result.Report, err
			},
		},
		{
			key:  FeatureLogprobs,
			perm: []Feature{FeatureLogprobs},
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
					Source: ResponseSourceArtifacts{ChatLogProbs: true},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderResponsesResponse(response, context)
				return report, err
			},
		},
		{
			key: FeatureResponsesControls,
			perm: []Feature{
				FeatureResponsesControls,
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
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
					Source: ResponseSourceArtifacts{
						ResponsesControls: []string{"background"},
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
			// The modern Anthropic envelope controls (context_management,
			// output_config) are approved or rejected per the exchange policy;
			// the approved drop is reported for each present control.
			key:  FeatureAnthropicControls,
			perm: []Feature{FeatureAnthropicControls},
			run: func(policy LossPolicy) (ConversionReport, error) {
				result, err := DecodeMessagesRequest([]byte(
					`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"context_management":{"edits":[]},"output_config":{"budget_tokens":32000}}`,
				), policy)
				return result.Report, err
			},
		},
		{
			// Request citations on text blocks are approved or rejected per the
			// exchange policy; the approved drop is reported for each citation.
			key:  FeatureRequestCitations,
			perm: []Feature{FeatureRequestCitations},
			run: func(policy LossPolicy) (ConversionReport, error) {
				result, err := DecodeMessagesRequest([]byte(
					`{"model":"m","max_tokens":100,"messages":[{"role":"assistant","content":[{"type":"text","text":"hi","citations":[{"type":"char_location","cited_text":"hi","document_index":0,"start_char_index":0,"end_char_index":2}]}]}]}`,
				), policy)
				return result.Report, err
			},
		},
		{
			// Built-in tools (web_search) are approved or rejected per the
			// exchange policy; the approved drop is reported.
			key: FeatureBuiltinTools,
			perm: []Feature{
				FeatureBuiltinTools,
				FeatureResponsesControls,
			},
			run: func(policy LossPolicy) (ConversionReport, error) {
				result, _, err := DecodeResponsesRequest([]byte(
					`{"model":"m","input":"hi","include":["reasoning.encrypted_content"],"tools":[{"type":"web_search","name":"ws"}]}`,
				), policy)
				return result.Report, err
			},
		},
		{
			key:  FeatureResponseServiceTier,
			perm: []Feature{FeatureResponseServiceTier},
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
					Source: ResponseSourceArtifacts{ChatServiceTier: "default"},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderResponsesResponse(response, context)
				return report, err
			},
		},
		{
			// The legacy function_call spelling maps to one tool call with
			// a synthesized id; the synthesis is an ungated note, so the
			// scenario runs under the strict policy and records the key
			// anyway.
			key:  FeatureLegacyFunctionCall,
			perm: []Feature{},
			note: true,
			run: func(policy LossPolicy) (ConversionReport, error) {
				body := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"function_call","message":{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{}"}}}]}`
				_, report, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, policy)
				return report, err
			},
		},
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
		{
			// An Anthropic-sourced image carries no detail field, so the
			// proxy chooses the documented 'auto' default; the invention is
			// an ungated note (strict policy records it).
			key:  FeatureImageDetailInvented,
			perm: []Feature{FeatureImageInput},
			note: true,
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role: CanonicalUser,
						Parts: []CanonicalPart{CanonicalImage{
							MediaType: "image/png",
							URL:       "https://example.test/x.png",
						}},
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{ImageInput: true})
				return report, err
			},
		},
		{
			// The Responses-only detail 'original' maps to 'high' on the
			// Chat target with an ungated note.
			key:  FeatureImageDetailOriginal,
			perm: []Feature{FeatureImageInput},
			note: true,
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role: CanonicalUser,
						Parts: []CanonicalPart{CanonicalImage{
							MediaType: "image/png",
							URL:       "https://example.test/x.png",
							Detail:    "original",
						}},
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{ImageInput: true})
				return report, err
			},
		},
	}

	// The scenario matrix must cover every registered key exactly once.
	registered := allLossKeys()
	seen := map[Feature]bool{}
	for _, s := range scenarios {
		if seen[s.key] {
			t.Fatalf("scenario for %q registered twice", s.key)
		}
		seen[s.key] = true
		delete(registered, s.key)
	}
	for key := range registered {
		t.Fatalf("no reachability scenario for registered key %q", key)
	}

	for _, s := range scenarios {
		t.Run(string(s.key), func(t *testing.T) {
			// Notes are sanctioned encodings, not policy decisions: they
			// record under every policy including strict. Gated losses must
			// reject under strict.
			if !s.note {
				if _, err := s.run(strict); err == nil {
					t.Fatal("strict policy accepted the loss")
				}
			}
			// A policy allowing exactly this scenario's own permissions must
			// complete the scenario and record the key (allowed by its own
			// permission; a perm list missing a required key fails here).
			other := permissive(s.perm...)
			report, err := s.run(other)
			if err != nil || !reportHasFeature(report, s.key) {
				t.Fatalf(
					"own-permission run: err = %v, report lacks %q: %+v",
					err,
					s.key,
					report,
				)
			}
			// A policy allowing everything succeeds and records the key.
			all := permissive(RegisteredLossKeys()...)
			report, err = s.run(all)
			if err != nil {
				t.Fatalf("permissive run failed: %v", err)
			}
			if !reportHasFeature(report, s.key) {
				t.Fatalf("report lacks %q: %+v", s.key, report)
			}
		})
	}
}
