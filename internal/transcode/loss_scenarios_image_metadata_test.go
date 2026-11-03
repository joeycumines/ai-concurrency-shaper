package transcode

import (
	"encoding/json"
	"testing"
)

func lossScenariosImageMetadata(t *testing.T) []lossScenario {
	return []lossScenario{
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
			// Chat target. The CLIENT asked for it, so the downgrade is a
			// fidelity-only knob and must be a POLICY-GATED loss, not an
			// ungated note: an operator can refuse the downgrade.
			key:  FeatureImageDetailOriginal,
			perm: []Feature{FeatureImageDetailOriginal, FeatureImageInput},
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
		{
			key:  FeatureVacuousSchemaBounds,
			perm: []Feature{},
			note: true,
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role:  CanonicalUser,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
					Tools: []CanonicalTool{{
						Name:        "t",
						Description: "d",
						JSONSchema:  json.RawMessage(`{"type":"object","properties":{"n":{"type":"number","maximum":1.7976931348623157e+308}}}`),
					}},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{Float64Bounds: false, Metadata: true})
				return report, err
			},
		},
		{
			key:  FeatureMetadata,
			perm: []Feature{FeatureMetadata},
			run: func(policy LossPolicy) (ConversionReport, error) {
				request := CanonicalRequest{
					ClientModel: "m",
					Turns: []CanonicalTurn{{
						Role:  CanonicalUser,
						Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
					}},
					Metadata: map[string]string{"session_id": "s1"},
				}
				context := testExchangeContext()
				context.LossPolicy = policy
				_, report, err := RenderChatRequest(request, context, ChatCapabilities{Metadata: false, Float64Bounds: true})
				return report, err
			},
		},
	}
}
