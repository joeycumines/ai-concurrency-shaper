package transcode

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// geminiRequestScenario decodes a Gemini request body under the given policy
// and returns the report the decode produced.
func geminiRequestScenario(body string) func(LossPolicy) (ConversionReport, error) {
	return func(policy LossPolicy) (ConversionReport, error) {
		result, _, err := DecodeGeminiRequest([]byte(body), policy, UpstreamChatCompletions)
		return result.Report, err
	}
}

// geminiResponseScenario decodes a Gemini response body under the given
// policy and returns the report the decode produced.
func geminiResponseScenario(body string) func(LossPolicy) (ConversionReport, error) {
	return func(policy LossPolicy) (ConversionReport, error) {
		_, report, err := DecodeGeminiResponse([]byte(body), ChatCapabilities{}, policy, NewExchangeIDs())
		return report, err
	}
}

// geminiRenderScenario renders a canonical request to Gemini under the given
// policy and returns the report the render produced.
func geminiRenderScenario(request CanonicalRequest) func(LossPolicy) (ConversionReport, error) {
	return func(policy LossPolicy) (ConversionReport, error) {
		context := testExchangeContext()
		context.LossPolicy = policy
		_, report, err := RenderGeminiRequest(request, context)
		return report, err
	}
}

// lossScenariosGemini covers the request-side Gemini losses. Each body keeps a
// text part in its turn, so the turn survives the drop and the decode
// completes under the permissive policy.
func lossScenariosGemini(t *testing.T) []lossScenario {
	return []lossScenario{
		{
			key:  FeatureGeminiGenerationControls,
			perm: []Feature{FeatureGeminiGenerationControls},
			run: geminiRequestScenario(`{
				"generationConfig": {"seed": 7},
				"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
			}`),
		},
		{
			key:  FeatureGeminiSafetySettings,
			perm: []Feature{FeatureGeminiSafetySettings},
			run: geminiRequestScenario(`{
				"safetySettings": [{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "BLOCK_NONE"}],
				"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
			}`),
		},
		{
			key:  FeatureGeminiRequestControls,
			perm: []Feature{FeatureGeminiRequestControls},
			run: geminiRequestScenario(`{
				"store": true,
				"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
			}`),
		},
		{
			key:  FeatureGeminiThoughtSignature,
			perm: []Feature{FeatureGeminiThoughtSignature},
			run: geminiRequestScenario(`{
				"contents": [
					{"role": "model", "parts": [{"text": "considering", "thought": true}, {"text": "answer"}]},
					{"role": "user", "parts": [{"text": "next"}]}
				]
			}`),
		},
		{
			key:  FeatureGeminiPartMetadata,
			perm: []Feature{FeatureGeminiPartMetadata},
			run: geminiRequestScenario(`{
				"contents": [{"role": "user", "parts": [{"text": "hi", "mediaResolution": {"level": "MEDIA_RESOLUTION_LOW"}}]}]
			}`),
		},
		{
			key:  FeatureGeminiProviderTools,
			perm: []Feature{FeatureGeminiProviderTools},
			run: geminiRequestScenario(`{
				"contents": [
					{"role": "model", "parts": [{"text": "running"}, {"executableCode": {"language": "PYTHON", "code": "print(1)"}}]},
					{"role": "user", "parts": [{"text": "next"}]}
				]
			}`),
		},
		// X2G render-side losses.
		{
			key:  FeatureGeminiThoughtSignaturePlaceholder,
			perm: []Feature{FeatureGeminiThoughtSignaturePlaceholder},
			run: geminiRenderScenario(CanonicalRequest{
				ClientModel: "m",
				Turns: []CanonicalTurn{
					{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
					{Role: CanonicalAssistant, Parts: []CanonicalPart{
						CanonicalFunctionCall{CallID: "call_1", Name: "f", Arguments: json.RawMessage(`{}`)},
					}},
				},
			}),
		},
		{
			key:  FeatureGeminiToolStrictness,
			perm: []Feature{FeatureGeminiToolStrictness},
			run: geminiRenderScenario(CanonicalRequest{
				ClientModel: "m",
				Turns: []CanonicalTurn{
					{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
				},
				Tools: []CanonicalTool{{
					Name:       "f",
					JSONSchema: json.RawMessage(`{"type":"object"}`),
					Strict:     new(true),
				}},
			}),
		},
		{
			key:  FeatureGeminiImageDetail,
			perm: []Feature{FeatureGeminiImageDetail},
			run: geminiRenderScenario(CanonicalRequest{
				ClientModel: "m",
				Turns: []CanonicalTurn{{
					Role: CanonicalUser,
					Parts: []CanonicalPart{
						CanonicalImage{MediaType: "image/png", Base64: "aGk=", Detail: "high"},
						CanonicalText{Text: "what"},
					},
				}},
			}),
		},
		// G2X decode-side losses.
		{
			key:  FeatureGeminiCandidateCollapse,
			perm: []Feature{FeatureGeminiCandidateCollapse},
			run: geminiResponseScenario(`{
				"candidates": [
					{"content": {"role": "model", "parts": [{"text": "a"}]}, "finishReason": "STOP", "index": 0},
					{"content": {"role": "model", "parts": [{"text": "b"}]}, "finishReason": "STOP", "index": 1}
				],
				"modelVersion": "m"
			}`),
		},
		{
			key:  FeatureGeminiSafetyMetadata,
			perm: []Feature{FeatureGeminiSafetyMetadata},
			run: geminiResponseScenario(`{
				"candidates": [{
					"content": {"role": "model", "parts": [{"text": "hi"}]},
					"finishReason": "STOP",
					"safetyRatings": [{"category": "X", "probability": "LOW"}]
				}],
				"modelVersion": "m"
			}`),
		},
		{
			key:  FeatureGeminiGroundingMetadata,
			perm: []Feature{FeatureGeminiGroundingMetadata},
			run: geminiResponseScenario(`{
				"candidates": [{
					"content": {"role": "model", "parts": [{"text": "hi"}]},
					"finishReason": "STOP",
					"groundingMetadata": {"groundingChunks": []}
				}],
				"modelVersion": "m"
			}`),
		},
		{
			key:  FeatureGeminiUsageBreakdown,
			perm: []Feature{FeatureGeminiUsageBreakdown},
			run: geminiResponseScenario(`{
				"candidates": [{
					"content": {"role": "model", "parts": [{"text": "hi"}]},
					"finishReason": "STOP",
					"tokenCount": 2
				}],
				"usageMetadata": {
					"promptTokenCount": 3,
					"candidatesTokenCount": 2,
					"totalTokenCount": 5,
					"promptTokensDetails": [{"modality": "TEXT", "tokenCount": 3}]
				},
				"modelVersion": "m"
			}`),
		},
		{
			key:  FeatureGeminiResponseThoughtSignature,
			perm: []Feature{FeatureGeminiResponseThoughtSignature},
			run: geminiResponseScenario(`{
				"candidates": [{
					"content": {"role": "model", "parts": [{"text": "hi", "thoughtSignature": "sig"}]},
					"finishReason": "STOP"
				}],
				"modelVersion": "m"
			}`),
		},
		{
			key:  FeatureGeminiModelStatus,
			perm: []Feature{FeatureGeminiModelStatus},
			run: geminiResponseScenario(`{
				"candidates": [{
					"content": {"role": "model", "parts": [{"text": "hi"}]},
					"finishReason": "STOP"
				}],
				"modelStatus": {"lifecycleStage": "ACTIVE"},
				"modelVersion": "m"
			}`),
		},
		{
			key:  FeatureGeminiClientQuery,
			perm: []Feature{FeatureGeminiClientQuery},
			run: func(policy LossPolicy) (ConversionReport, error) {
				h := NewTranscodeHandler(HandlerConfig{
					Mapping: Mapping{
						ClientRoute:        RouteKey{Method: "POST", Path: "/v1/messages"},
						ClientProtocol:     ClientMessages,
						UpstreamProtocol:   UpstreamGemini,
						UpstreamPath:       "/v1beta",
						ModelMap:           ModelMap{AllowIdentity: true},
						Auth:               AuthPolicy{Mode: AuthNone},
						LossPolicy:         policy,
						AllowedClientQuery: map[string]struct{}{"beta": {}},
					},
					Upstream: mustParseURL(t, "https://gemini.example.com"),
				}, func(*http.Request) (*http.Response, error) {
					return nil, errors.New("no round trip in this test")
				}, nil)
				r := httptest.NewRequest("POST", "/v1/messages?beta=true", nil)
				report, err := h.geminiClientQueryLosses(r)
				return report, err
			},
		},
	}
}
