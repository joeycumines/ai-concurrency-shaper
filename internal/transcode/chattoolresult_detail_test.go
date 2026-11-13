package transcode

// The multipart tool-result attempt is TRANSACTIONAL: it may decline a shape the
// multipart form cannot represent (a document, an image media type outside the
// chat vocabulary, no blocks at all) and leave the caller to take its own
// loss/reject decisions on the JSON envelope. It must not decline on a decision
// the POLICY already took.
//
// Two error classes reach the call site from chatImageDetail, and conflating
// them inverts the operator's intent in one case and silently accepts malformed
// client input in the other:
//
//   - a policy refusal of image_detail_original means the operator denied the
//     downgrade to "high". The envelope carries no detail field at all, so the
//     fallback applies a TOTAL drop — strictly stronger than the downgrade that
//     was refused. Propagating the refusal is the only outcome that honours it.
//   - an UnsupportedFeatureError for a detail value outside the vocabulary is
//     malformed client input. Declining would serve it with the value silently
//     re-encoded away, while the same value on a user-message image hard-fails;
//     the content location must not decide the verdict.

import (
	"errors"
	"strings"
	"testing"
)

// toolResultOriginalDetail is a tool result whose image requests the
// Responses-only detail value.
func toolResultOriginalDetail() CanonicalFunctionResult {
	return CanonicalFunctionResult{
		CallID: "call_1",
		Parts: []CanonicalPart{
			CanonicalText{Text: "look"},
			CanonicalImage{MediaType: "image/png", Base64: "aGk=", Detail: "original"},
		},
	}
}

// TestMultipartDetailRefusalPropagatesToTheOperator proves a denied
// image_detail_original fails the request rather than falling through to an
// envelope that drops the detail entirely.
func TestMultipartDetailRefusalPropagatesToTheOperator(t *testing.T) {
	// The envelope conversions are approved; image_detail_original is denied.
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolResultMultimodalContent: {},
		FeatureToolResultJSONEnvelope:      {},
	}}
	var report ConversionReport
	_, err := renderChatToolResult(
		toolResultOriginalDetail(),
		toolResultImageCapabilities(),
		policy,
		&report,
	)
	if err == nil {
		t.Fatal("a denied image_detail_original was served through the envelope, " +
			"applying a total drop stronger than the downgrade the operator refused")
	}
	if !strings.Contains(err.Error(), string(FeatureImageDetailOriginal)) {
		t.Fatalf("err = %q, want it to name %s", err, FeatureImageDetailOriginal)
	}
	// The refusal is local and typed, never an upstream wire error.
	if _, ok := errors.AsType[*UnsupportedFeatureError](err); !ok {
		t.Fatalf("err = %T %v, want *UnsupportedFeatureError", err, err)
	}
	if reportHasFeature(report, FeatureToolResultJSONEnvelope) {
		t.Fatalf("the envelope decision was recorded for a refused request: %+v", report.Losses)
	}
}

// TestMultipartDetailRefusalIsServedWhenApproved proves the same request renders
// through the multipart form once the operator approves the downgrade: the
// refusal is a policy decision, not a hard rejection.
func TestMultipartDetailRefusalIsServedWhenApproved(t *testing.T) {
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolResultMultimodalContent: {},
		FeatureToolResultJSONEnvelope:      {},
		FeatureImageDetailOriginal:         {},
	}}
	var report ConversionReport
	message, err := renderChatToolResult(
		toolResultOriginalDetail(),
		toolResultImageCapabilities(),
		policy,
		&report,
	)
	if err != nil {
		t.Fatalf("approved render: %v", err)
	}
	if message.Content == nil || message.Content.ContentBlocks == nil {
		t.Fatalf("content = %+v, want the multipart block form", message.Content)
	}
	if !reportHasFeature(report, FeatureImageDetailOriginal) {
		t.Fatalf("report = %+v, want the approved downgrade recorded", report.Losses)
	}
	if reportHasFeature(report, FeatureToolResultJSONEnvelope) {
		t.Fatalf("the envelope was used for a request the multipart form carries: %+v", report.Losses)
	}
}

// TestMultipartDetailMalformedValueFails proves an invalid detail value is not
// laundered into an approved envelope loss, and that its verdict does not depend
// on whether the image sits in a tool result or a user message.
func TestMultipartDetailMalformedValueFails(t *testing.T) {
	// Every envelope key is approved, so nothing but the invalid value can
	// refuse the request.
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolResultMultimodalContent: {},
		FeatureToolResultJSONEnvelope:      {},
		FeatureImageInput:                  {},
		FeatureImageDetailOriginal:         {},
	}}
	result := CanonicalFunctionResult{
		CallID: "call_1",
		Parts: []CanonicalPart{
			CanonicalImage{MediaType: "image/png", Base64: "aGk=", Detail: "ultra"},
		},
	}
	var report ConversionReport
	if _, err := renderChatToolResult(result, toolResultImageCapabilities(), policy, &report); err == nil {
		t.Fatal("an image detail value outside the chat vocabulary was served in a tool result")
	}

	// The same value in a user message already fails; the two must agree.
	userRequest := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role: CanonicalUser,
			Parts: []CanonicalPart{
				CanonicalImage{MediaType: "image/png", Base64: "aGk=", Detail: "ultra"},
			},
		}},
	}
	context := testExchangeContext()
	context.LossPolicy = policy
	if _, _, err := RenderChatRequest(userRequest, context, toolResultImageCapabilities()); err == nil {
		t.Fatal("an image detail value outside the chat vocabulary was served in a user message")
	}
}

// TestMultipartDocumentStillDeclines proves the transactional decline itself
// still works: a document cannot be a multipart tool-message part, so the
// envelope carries it and no image-detail decision is invented for content the
// multipart form never rendered.
func TestMultipartDocumentStillDeclines(t *testing.T) {
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureToolResultMultimodalContent: {},
		FeatureToolResultJSONEnvelope:      {},
		FeatureImageInput:                  {},
	}}
	result := CanonicalFunctionResult{
		CallID: "call_1",
		Parts: []CanonicalPart{
			CanonicalDocument{MediaType: "application/pdf", Base64: "aGk="},
		},
	}
	var report ConversionReport
	message, err := renderChatToolResult(result, toolResultImageCapabilities(), policy, &report)
	if err != nil {
		t.Fatalf("a document must decline to the envelope, not fail: %v", err)
	}
	if message.Content == nil || message.Content.ContentStr == nil {
		t.Fatalf("content = %+v, want the JSON envelope string arm", message.Content)
	}
	if !reportHasFeature(report, FeatureToolResultJSONEnvelope) {
		t.Fatalf("report = %+v, want the envelope decision", report.Losses)
	}
	if reportHasFeature(report, FeatureImageDetailOriginal) ||
		reportHasFeature(report, FeatureImageDetailInvented) {
		t.Fatalf("an image-detail decision was recorded for a declined attempt: %+v", report.Losses)
	}
}
