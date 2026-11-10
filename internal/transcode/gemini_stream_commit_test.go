package transcode

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// geminiSignatureStoreContext returns a stream context with a signature store
// and every loss the stream tests use approved, except the named features.
func geminiSignatureStoreContext(drop ...Feature) (*ExchangeContext, *GeminiSignatureStore) {
	ctx := geminiStreamSignatureTestContext()
	for _, feature := range drop {
		delete(ctx.LossPolicy.Allowed, feature)
	}
	store := NewGeminiSignatureStore(GeminiSignatureConfig{})
	ctx.GeminiSignatures = store
	ctx.GeminiSignatureScope = "scope"
	return ctx, store
}

// TestGeminiStreamChunkCommitsOnlyWhenWhole proves a chunk refused after one of
// its parts was converted leaves nothing behind: no signature in the store and
// no call-id note in the report. Signatures are written only when the whole
// call succeeds, as on the non-streaming path.
func TestGeminiStreamChunkCommitsOnlyWhenWhole(t *testing.T) {
	t.Run("a later refused part leaves no signature and no call-id note", func(t *testing.T) {
		ctx, store := geminiSignatureStoreContext()
		c := newGeminiToResponsesConverter(newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_t", "m", 1, nil))
		_, err := c.Convert(SSEEvent{Data: []byte(
			`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"sig"},{"functionResponse":{"name":"f","response":{}}}]},"index":0}]}`,
		)})
		if err == nil {
			t.Fatal("functionResponse part: nil error")
		}
		if n := store.Len(); n != 0 {
			t.Fatalf("signature store holds %d entries after a refused chunk, want 0", n)
		}
		if report := c.ConversionReport(); countReportFeature(*report, FeatureGeminiCallIDSynthesized) != 0 {
			t.Fatalf("call-id note recorded for a refused chunk: %+v", report.Losses)
		}
	})

	t.Run("a refused candidate safety rating leaves no signature", func(t *testing.T) {
		ctx, store := geminiSignatureStoreContext(FeatureGeminiSafetyMetadata)
		c := newGeminiToResponsesConverter(newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_t", "m", 1, nil))
		_, err := c.Convert(SSEEvent{Data: []byte(
			`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f","args":{}},"thoughtSignature":"sig"}]},"safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"LOW"}],"index":0}]}`,
		)})
		var unsupported *UnsupportedFeatureError
		if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureGeminiSafetyMetadata) {
			t.Fatalf("err = %T %v, want the refused safety metadata", err, err)
		}
		if n := store.Len(); n != 0 {
			t.Fatalf("signature store holds %d entries after a refused chunk, want 0", n)
		}
	})

	t.Run("an accepted call stores its signature once the call succeeds", func(t *testing.T) {
		ctx, store := geminiSignatureStoreContext()
		c := newGeminiToResponsesConverter(newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_t", "m", 1, nil))
		if _, err := c.Convert(SSEEvent{Data: []byte(
			`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f","args":{}},"thoughtSignature":"sig"}]},"index":0}]}`,
		)}); err != nil {
			t.Fatalf("accepted call: %v", err)
		}
		if sig, ok := store.Get("scope", "c1"); !ok || sig != "sig" {
			t.Fatalf("stored signature = %q, %v; want sig, true", sig, ok)
		}
	})
}

// TestGeminiStreamFunctionCallWireErrorBeforeMetadataGate proves a function
// call whose arguments exceed the stream bound is an upstream wire fault even
// when its part metadata is also refused. The metadata gate must not preempt
// the upstream fault, which counts against the upstream.
func TestGeminiStreamFunctionCallWireErrorBeforeMetadataGate(t *testing.T) {
	ctx, _ := geminiSignatureStoreContext(FeatureGeminiPartMetadata)
	c := newGeminiToResponsesConverter(newGeminiResponsesStreamState(ctx, ctx.LossPolicy, ChatCapabilities{}, "resp_t", "m", 1, nil))
	oversized := strings.Repeat("a", maxStreamAccumulatedBytes+16)
	_, err := c.Convert(SSEEvent{Data: []byte(fmt.Sprintf(
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f","args":{"p":%q}},"partMetadata":{"foo":1}}]},"index":0}]}`,
		oversized,
	))})
	if _, ok := errors.AsType[*UpstreamWireError](err); !ok {
		t.Fatalf("err = %T %v, want an upstream wire fault for the over-bound arguments", err, err)
	}
}

// lossKeys renders each recorded entry as kind, feature, and path, so two
// reports can be compared in order.
func lossKeys(losses []ConversionLoss) []string {
	keys := make([]string, 0, len(losses))
	for _, loss := range losses {
		keys = append(keys, fmt.Sprintf("%d %s at %s", loss.Kind, loss.Feature, loss.Path))
	}
	return keys
}

// TestGeminiStreamRecordsMatchNonStreamingDecode proves each part shape records
// the same entries, in the same order and at the same field paths, on the
// streaming and the non-streaming paths.
func TestGeminiStreamRecordsMatchNonStreamingDecode(t *testing.T) {
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureProviderReasoningText:          {},
		FeatureGeminiResponseThoughtSignature: {},
		FeatureGeminiPartMetadata:             {},
	}}
	for name, tc := range map[string]struct {
		part string
		caps ChatCapabilities
	}{
		"thought with signature and metadata": {
			part: `{"text":"thinking","thought":true,"thoughtSignature":"sig","partMetadata":{"foo":1}}`,
			caps: ChatCapabilities{ProviderReasoningThinking: true},
		},
		"signature-only part": {
			part: `{"thoughtSignature":"sig","partMetadata":{"foo":1}}`,
		},
		"text part with signature and metadata": {
			part: `{"text":"hi","thoughtSignature":"sig","partMetadata":{"foo":1}}`,
		},
		"id-less function call with metadata": {
			part: `{"functionCall":{"name":"f","args":{}},"partMetadata":{"foo":1}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := &ExchangeContext{IDs: NewExchangeIDs(), LossPolicy: policy}
			state := newGeminiResponsesStreamState(ctx, policy, tc.caps, "resp_t", "m", 1, nil)
			if _, err := state.Convert(geminiChunk(t,
				`{"candidates":[{"content":{"role":"model","parts":[`+tc.part+`]},"finishReason":"STOP","index":0}]}`,
			)); err != nil {
				t.Fatalf("stream: %v", err)
			}
			_, decodeReport, err := DecodeGeminiResponse(
				[]byte(`{"candidates":[{"content":{"role":"model","parts":[`+tc.part+`]},"finishReason":"STOP","index":0}],"modelVersion":"m"}`),
				tc.caps, policy, NewExchangeIDs(), nil,
			)
			if err != nil {
				t.Fatalf("non-streaming: %v", err)
			}
			if got, want := lossKeys(state.report.Losses), lossKeys(decodeReport.Losses); !slices.Equal(got, want) {
				t.Fatalf("stream records %v, non-streaming records %v", got, want)
			}
		})
	}
}
