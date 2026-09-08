package transcode

// A chat response without model and a Responses envelope without status are
// missing required semantic fields: the exchange is refused, but as a LOCAL
// conversion error, never an upstream failure. Refusing is what the client
// contract requires (the client cannot be told the missing field truthfully);
// the local classification keeps a provider that omits the field from opening
// the circuit breaker for every route. Genuinely corrupt upstream wire (a
// wrong object discriminant, malformed syntax, a type-corrupt modeled field)
// still classifies as an upstream failure. An arithmetically inconsistent
// usage is neither: it is clamped and noted
// (see TestSourceInconsistentUsageClampedAndNoted).

import (
	"errors"
	"strings"
	"testing"
)

// TestChatResponseWithoutModelIsLocalConversionError pins the missing-model
// classification: model is a required field of the pinned Chat response
// contract, so absent or empty refuses the exchange LOCALLY — a provider that
// omits it must not be breaker-visible, or a sloppy gateway would open the
// circuit breaker for every route.
func TestChatResponseWithoutModelIsLocalConversionError(t *testing.T) {
	for name, body := range map[string]string{
		"absent model": `{"id":"c","object":"chat.completion","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		"empty model":  `{"id":"c","object":"chat.completion","created":1,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	} {
		_, _, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, StrictLossPolicy())
		if err == nil {
			t.Fatalf("%s: decode must fail", name)
		}
		if _, ok := errors.AsType[*UpstreamWireError](err); ok {
			t.Fatalf("%s: err = %T %v, must not be an upstream wire error", name, err, err)
		}
		if got := conversionProvenance(err); got != ProvenanceLocalResponseConversionError {
			t.Fatalf("%s: provenance = %v, want local", name, got)
		}
	}
}

// TestResponsesEnvelopeWithoutStatusIsLocalConversionError pins the
// missing-status classification: status is a required semantic field of the
// pinned Responses contract, so absent or empty refuses the exchange LOCALLY,
// never as a breaker-visible upstream failure. A PRESENT but unknown status
// remains an unsupported feature (also local).
func TestResponsesEnvelopeWithoutStatusIsLocalConversionError(t *testing.T) {
	for name, body := range map[string]string{
		"absent status": `{"id":"resp_1","object":"response","created_at":1,"model":"m","output":[]}`,
		"empty status":  `{"id":"resp_1","object":"response","created_at":1,"status":"","model":"m","output":[]}`,
	} {
		_, err := DecodeResponsesResponse([]byte(body))
		if err == nil {
			t.Fatalf("%s: decode must fail", name)
		}
		if _, ok := errors.AsType[*UpstreamWireError](err); ok {
			t.Fatalf("%s: err = %T %v, must not be an upstream wire error", name, err, err)
		}
		if got := conversionProvenance(err); got != ProvenanceLocalResponseConversionError {
			t.Fatalf("%s: provenance = %v, want local", name, got)
		}
	}

	_, err := DecodeResponsesResponse([]byte(
		`{"id":"resp_1","object":"response","created_at":1,"status":"teleported","model":"m","output":[]}`,
	))
	if _, ok := errors.AsType[*UnsupportedFeatureError](err); !ok {
		t.Fatalf("unknown status err = %T %v, want *UnsupportedFeatureError (local)", err, err)
	}
	if got := conversionProvenance(err); got != ProvenanceLocalResponseConversionError {
		t.Fatalf("unknown status provenance = %v, want local", got)
	}
}

// TestSourceInconsistentUsageClampedAndNoted pins the clamp disposition:
// usage whose cached breakdown exceeds the input total (or negative counts) is
// a subject-to-change provider value, so the non-stream render and the stream
// converter both CLAMP it and record an ungated note instead of failing the
// exchange (the pre-fix behaviour was a 502 reading "source usage is
// arithmetically inconsistent").
func TestSourceInconsistentUsageClampedAndNoted(t *testing.T) {
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
		body, report, err := RenderMessagesResponse(response, context)
		if err != nil {
			t.Fatalf("cache-exceeds-input must clamp, not fail the exchange: %v", err)
		}
		mustUsageClampNote(t, &report, FeatureUsageCacheExceedsInput, "non-stream Messages render")
		// The clamp preserves the Anthropic identity: uncached (0) +
		// cache-read (10) + cache-creation (0) = the clamped input total.
		for _, want := range []string{
			`"input_tokens":0`,
			`"cache_read_input_tokens":10`,
			`"cache_creation_input_tokens":0`,
			`"output_tokens":5`,
		} {
			if !strings.Contains(string(body), want) {
				t.Fatalf("rendered usage missing %s: %s", want, body)
			}
		}
	})
	t.Run("stream usage conversion", func(t *testing.T) {
		usage := &ResponsesUsage{
			InputTokens:  -1,
			OutputTokens: 5,
			TotalTokens:  4,
		}
		converted, clamp, err := responsesUsageToAnthropicUsage(usage, responsesUsagePresence(usage))
		if err != nil {
			t.Fatalf("negative counts must clamp, not fail the stream: %v", err)
		}
		if !clamp.negativeCounts || converted.InputTokens != 0 || converted.OutputTokens != 5 {
			t.Fatalf("converted = %+v (clamp %+v), want input 0 output 5", converted, clamp)
		}
	})
	t.Run("source inconsistency covers negative counts", func(t *testing.T) {
		// The platform-overflow arm of UsageArithmeticError stays local by
		// construction; negative counts are now clamped and noted, never
		// classified as upstream failures.
		usage := &ResponsesUsage{
			InputTokens:        -2,
			OutputTokens:       5,
			TotalTokens:        3,
			InputTokensDetails: &UsageInputTokensDetails{CachedTokens: 1},
		}
		converted, clamp, err := responsesUsageToAnthropicUsage(usage, responsesUsagePresence(usage))
		if err != nil {
			t.Fatalf("negative counts must clamp, not reject: %v", err)
		}
		// The clamped input (0) then bounds the cache read: no component can
		// exceed the input total, so the cached 1 also clamps to 0.
		if !clamp.negativeCounts || !clamp.cacheExceedsInput {
			t.Fatalf("clamp = %+v, want negative counts and cache-exceeds-input", clamp)
		}
		if converted.InputTokens != 0 || converted.CacheReadInputTokens != 0 || converted.OutputTokens != 5 {
			t.Fatalf("converted = %+v, want input 0 cache-read 0 output 5", converted)
		}
	})
}

// mustUsageClampNote asserts the report carries the ungated usage-clamp note.
func mustUsageClampNote(t *testing.T, report *ConversionReport, feature Feature, context string) {
	t.Helper()
	for _, l := range report.Losses {
		if l.Feature == feature && l.Kind == NoteRecord {
			return
		}
	}
	t.Fatalf("%s: %s note missing: %+v", context, feature, report.Losses)
}
