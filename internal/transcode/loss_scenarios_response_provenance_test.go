package transcode

import (
	"testing"
)

func lossScenariosResponseProvenance(t *testing.T) []lossScenario {
	return []lossScenario{
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
			// Anthropic server-side tools (the type-discriminated
			// web_search_20250305 definition a real Claude Code session
			// sends) are approved or rejected per the exchange policy;
			// the approved drop is reported with the key, never as an
			// unattributed unknown-field error.
			key:  FeatureAnthropicServerTools,
			perm: []Feature{FeatureAnthropicServerTools},
			run: func(policy LossPolicy) (ConversionReport, error) {
				result, err := DecodeMessagesRequest([]byte(
					`{"model":"m","max_tokens":8,"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}],"messages":[{"role":"user","content":"hi"}]}`,
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
				_, report, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, policy, nil)
				return report, err
			},
		},
	}
}
