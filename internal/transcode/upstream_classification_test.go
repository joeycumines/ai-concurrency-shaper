package transcode

// Autopsy 2026-09-06 M2: a chat response without model, a Responses envelope
// without status, and source-inconsistent usage are corrupt UPSTREAM data —
// they must classify as upstream failures (breaker-visible provenance),
// never as local conversion errors. A poisonous upstream that always emits
// these shapes must open the breaker instead of silently poisoning every
// exchange while the breaker stays closed.

import (
	"errors"
	"testing"
)

// TestChatResponseWithoutModelIsUpstreamWireError pins the missing-model
// classification: model is a required field of the pinned Chat response
// contract, so absent or empty is corrupt upstream wire (pre-fix the plain
// IR-validation error classified local).
func TestChatResponseWithoutModelIsUpstreamWireError(t *testing.T) {
	for name, body := range map[string]string{
		"absent model": `{"id":"c","object":"chat.completion","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		"empty model":  `{"id":"c","object":"chat.completion","created":1,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	} {
		_, _, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, StrictLossPolicy())
		if err == nil {
			t.Fatalf("%s: decode must fail", name)
		}
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("%s: err = %T %v, want *UpstreamWireError", name, err, err)
		}
		if got := conversionProvenance(err); got != ProvenanceUpstreamBodyError {
			t.Fatalf("%s: provenance = %v, want upstream body error", name, got)
		}
	}
}

// TestResponsesEnvelopeWithoutStatusIsUpstreamWireError pins the missing
// -status classification: status is a required semantic field of the pinned
// Responses contract, so absent or empty is corrupt upstream wire. A PRESENT
// but unknown status remains an unsupported feature (local).
func TestResponsesEnvelopeWithoutStatusIsUpstreamWireError(t *testing.T) {
	for name, body := range map[string]string{
		"absent status": `{"id":"resp_1","object":"response","created_at":1,"model":"m","output":[]}`,
		"empty status":  `{"id":"resp_1","object":"response","created_at":1,"status":"","model":"m","output":[]}`,
	} {
		_, err := DecodeResponsesResponse([]byte(body))
		if err == nil {
			t.Fatalf("%s: decode must fail", name)
		}
		var wireErr *UpstreamWireError
		if !errors.As(err, &wireErr) {
			t.Fatalf("%s: err = %T %v, want *UpstreamWireError", name, err, err)
		}
		if got := conversionProvenance(err); got != ProvenanceUpstreamBodyError {
			t.Fatalf("%s: provenance = %v, want upstream body error", name, got)
		}
	}

	_, err := DecodeResponsesResponse([]byte(
		`{"id":"resp_1","object":"response","created_at":1,"status":"teleported","model":"m","output":[]}`,
	))
	var featureErr *UnsupportedFeatureError
	if !errors.As(err, &featureErr) {
		t.Fatalf("unknown status err = %T %v, want *UnsupportedFeatureError (local)", err, err)
	}
	if got := conversionProvenance(err); got != ProvenanceLocalResponseConversionError {
		t.Fatalf("unknown status provenance = %v, want local", got)
	}
}

// TestSourceInconsistentUsageIsUpstream pins the source-inconsistent-usage
// classification on both surfaces: usage whose cached breakdown exceeds the
// input total (or negative counts) is internally inconsistent SOURCE data,
// so the non-stream render and the stream converter both produce the typed
// SourceInconsistencyError and the exchange classifies upstream.
func TestSourceInconsistentUsageIsUpstream(t *testing.T) {
	t.Run("non-stream Messages render", func(t *testing.T) {
		response := CanonicalResponse{
			ID:     "resp_1",
			Model:  "m",
			Status: CanonicalResponseCompleted,
			Stop:   CanonicalStop{Reason: CanonicalStopEndTurn},
			Items: []CanonicalResponseItem{
				&CanonicalMessageItem{
					Role:  CanonicalAssistant,
					Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
				},
			},
			Usage: CanonicalUsage{
				InputKnown:      true,
				InputTokens:     10,
				CacheReadKnown:  true,
				CacheReadTokens: 11,
				OutputKnown:     true,
				OutputTokens:    5,
				TotalKnown:      true,
				TotalTokens:     15,
			},
		}
		context := testExchangeContext()
		context.LossPolicy = j6PermissivePolicy()
		context.RequestedClientModel = "m"
		_, _, err := RenderMessagesResponse(response, context)
		if err == nil {
			t.Fatal("render must fail on cached > input usage")
		}
		var inconsistent *SourceInconsistencyError
		if !errors.As(err, &inconsistent) {
			t.Fatalf("err = %T %v, want *SourceInconsistencyError", err, err)
		}
		if got := conversionProvenance(err); got != ProvenanceUpstreamBodyError {
			t.Fatalf("provenance = %v, want upstream body error", got)
		}
	})
	t.Run("stream usage conversion", func(t *testing.T) {
		_, err := responsesUsageToAnthropicUsage(&ResponsesUsage{
			InputTokens:  -1,
			OutputTokens: 5,
			TotalTokens:  4,
		})
		var inconsistent *SourceInconsistencyError
		if !errors.As(err, &inconsistent) {
			t.Fatalf("err = %T %v, want *SourceInconsistencyError", err, err)
		}
	})
	t.Run("source inconsistency covers negative counts", func(t *testing.T) {
		// The platform-overflow arm of UsageArithmeticError stays local by
		// construction: the M2 classification branches on the TYPE
		// (SourceInconsistencyError), never on the detail text, so the
		// overflow path is untouched. Negative counts are the
		// source-inconsistency discriminator this test pins.
		_, err := responsesUsageToAnthropicUsage(&ResponsesUsage{
			InputTokens:        -2,
			OutputTokens:       5,
			TotalTokens:        3,
			InputTokensDetails: &UsageInputTokensDetails{CachedTokens: 1},
		})
		var inconsistent *SourceInconsistencyError
		if !errors.As(err, &inconsistent) {
			t.Fatalf("err = %T %v, want *SourceInconsistencyError", err, err)
		}
	})
}
