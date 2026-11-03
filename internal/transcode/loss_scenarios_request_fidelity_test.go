package transcode

import (
	"encoding/json"
	"testing"
)

func lossScenariosRequestFidelity(t *testing.T) []lossScenario {
	return []lossScenario{
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
	}
}
