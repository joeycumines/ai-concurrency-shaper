package transcode

import (
	"encoding/json"
	"testing"
)

func lossScenariosRequestConversion(t *testing.T) []lossScenario {
	return []lossScenario{
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
			// policy rejects the position loss. The merge of the two
			// contributing turns rides the separate shared
			// multiple_system_turns decision, so both permissions are granted
			// here (only the key under test is a fixation point).
			key:  FeatureMidConversationSystem,
			perm: []Feature{FeatureMidConversationSystem, FeatureMultipleSystemTurns},
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
				}, "turns[0].parts[0]", ChatCapabilities{}, policy, &report)
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
				}, "turns[0].parts[0]", ChatCapabilities{}, policy, &report)
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
				}, "turns[0].parts[0]", ChatCapabilities{}, policy, &report)
				return report, err
			},
		},
		{
			// A replayed function_call naming a namespace the rendering
			// exchange does not declare: the qualifier stays on the canonical
			// part and the flattening renderer decides it, so the call is
			// recorded as this loss and forwarded under the bare child name.
			// The bare name here is owned by nothing, so the conversion is not
			// refused outright.
			key: FeatureNamespaceReplayUndeclared,
			perm: []Feature{
				FeatureNamespaceReplayUndeclared,
				FeatureUsageCacheReadUnknown,
				FeatureUsageCacheWriteUnknown,
				FeatureUsageReasoningUnknown,
				FeatureUsageUnknown,
			},
			run: func(policy LossPolicy) (ConversionReport, error) {
				result, echo, err := DecodeResponsesRequest([]byte(`{
					"model": "m",
					"tools": [
						{"type": "namespace", "name": "declared", "description": "d", "tools": [
							{"type": "function", "name": "known", "description": "x", "strict": false,
							 "parameters": {"type": "object", "properties": {}}}]}
					],
					"input": [
						{"type": "function_call", "call_id": "call_1", "name": "unknown",
						 "namespace": "undeclared", "arguments": "{}"}
					]
				}`), policy)
				if err != nil {
					return result.Report, err
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				context.OriginalResponsesRequest = echo
				context.ToolNames = result.ToolNames
				_, report, err := RenderChatRequest(result.Request, context, ChatCapabilities{})
				return report, err
			},
		},
	}
}
