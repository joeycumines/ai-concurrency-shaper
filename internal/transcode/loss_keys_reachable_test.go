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
// accepted anywhere (review-z commit 2; no deprecated aliases).
// legacyLossNames are the REMOVED broad permission names: none may be
// accepted anywhere (review-z commit 2; no deprecated aliases). Names that
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
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderResponsesRequest(request, context)
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
				}, policy, &report)
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
				}, policy, &report)
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
				}, policy, &report)
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
				_, err := state.Convert(chatChunk(t, ChatStreamDelta{Reasoning: stringPtr("think")}, nil))
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
			// Strict policy rejects the key's decision.
			if _, err := s.run(strict); err == nil {
				t.Fatal("strict policy accepted the loss")
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
