package transcode

// The multipart tool-result attempt may DECLINE a shape the multipart form
// cannot carry, but a decision the POLICY already took is not a decline. A
// decline makes the caller fall back to the JSON envelope, which carries no
// detail field at all — a total drop, strictly stronger than the downgrade an
// operator refused — so the outcome must not depend on where the part sits in
// the result.

import (
	"strings"
	"testing"
)

// multipartCaps enables both capabilities the multipart form needs.
func multipartCaps() ChatCapabilities {
	return ChatCapabilities{ToolResultImages: true, ImageInput: true}
}

// deniedOriginalPolicy refuses the image-detail downgrade and approves the
// envelope keys, so a fallback would succeed and hide the refusal.
func deniedOriginalPolicy() LossPolicy {
	return LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolResultMultimodalContent: {},
		FeatureToolResultJSONEnvelope:      {},
	}}
}

// TestMultipartDetailDenialIsOrderIndependent proves a denied
// image_detail_original fails the request whichever side of a document the image
// is on.
func TestMultipartDetailDenialIsOrderIndependent(t *testing.T) {
	original := CanonicalImage{MediaType: "image/png", Base64: "AAAA", Detail: "original"}
	doc := CanonicalDocument{MediaType: "application/pdf", Base64: "BBBB"}
	for name, parts := range map[string][]CanonicalPart{
		"image first":    {original, doc},
		"document first": {doc, original},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := renderChatToolResult(
				CanonicalFunctionResult{CallID: "call_1", Parts: parts},
				"turns[0].parts[0]",
				multipartCaps(),
				deniedOriginalPolicy(),
				&ConversionReport{},
			)
			if err == nil {
				t.Fatal("a denied image_detail_original was routed around by the envelope fallback")
			}
			if !strings.Contains(err.Error(), string(FeatureImageDetailOriginal)) {
				t.Fatalf("err = %q, want it to name %s", err, FeatureImageDetailOriginal)
			}
		})
	}
}

// TestMultipartMalformedDetailIsOrderIndependent proves malformed client input is
// rejected wherever it sits, rather than silently re-encoded away on one order.
func TestMultipartMalformedDetailIsOrderIndependent(t *testing.T) {
	bad := CanonicalImage{MediaType: "image/png", Base64: "AAAA", Detail: "ultra"}
	doc := CanonicalDocument{MediaType: "application/pdf", Base64: "BBBB"}
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolResultMultimodalContent: {},
		FeatureToolResultJSONEnvelope:      {},
	}}
	for name, parts := range map[string][]CanonicalPart{
		"image first":    {bad, doc},
		"document first": {doc, bad},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := renderChatToolResult(
				CanonicalFunctionResult{CallID: "call_1", Parts: parts},
				"turns[0].parts[0]",
				multipartCaps(),
				policy,
				&ConversionReport{},
			); err == nil {
				t.Fatal("a malformed detail value was silently re-encoded away")
			}
		})
	}
}

// TestMultipartDocumentOnlyStillDeclines proves the scope guard: a shape the
// multipart form genuinely cannot carry still falls back to the envelope, and
// records no image-detail decision for a request that has no image.
func TestMultipartDocumentOnlyStillDeclines(t *testing.T) {
	doc := CanonicalDocument{MediaType: "application/pdf", Base64: "BBBB"}
	report := ConversionReport{}
	message, err := renderChatToolResult(
		CanonicalFunctionResult{CallID: "call_1", Parts: []CanonicalPart{doc}},
		"turns[0].parts[0]",
		multipartCaps(),
		deniedOriginalPolicy(),
		&report,
	)
	if err != nil {
		t.Fatalf("a document-only result failed instead of declining: %v", err)
	}
	if message.Content == nil || message.Content.ContentStr == nil {
		t.Fatalf("expected the JSON envelope string arm: %+v", message)
	}
	if reportHasFeature(report, FeatureImageDetailOriginal) {
		t.Fatalf("an image decision was recorded for a document-only result: %+v", report.Losses)
	}
}
