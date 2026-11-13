package transcode

// A render-side report path names the canonical turn and part the operator sent,
// on every target. The Responses renderer was the last one still filing a
// dropped system part under the wire field it never entered, which also collapsed
// several distinct parts into one deduplicated log line.

import (
	"testing"
)

// TestResponsesSystemPartLossNamesTheCanonicalPath proves a dropped non-text
// system part is reported at turns[N].parts[M] like every other renderer, and
// that two such parts stay distinguishable after deduplication.
func TestResponsesSystemPartLossNamesTheCanonicalPath(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role: CanonicalSystem,
			Parts: []CanonicalPart{
				CanonicalImage{MediaType: "image/png", URL: "https://example.test/a.png"},
				CanonicalText{Text: "keep this"},
				CanonicalDocument{MediaType: "application/pdf", URL: "https://example.test/b.pdf"},
			},
		}, {
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureSystemNonTextContent: {}}}
	_, report, err := RenderResponsesRequest(request, context)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"turns[0].parts[0]", "turns[0].parts[2]"} {
		if !reportHasAt(report, FeatureSystemNonTextContent, want) {
			t.Fatalf("report = %+v, want a system_non_text_content entry at %s", report.Losses, want)
		}
	}
	for _, loss := range report.Losses {
		if loss.Feature == FeatureSystemNonTextContent && loss.Path == "instructions" {
			t.Fatalf("a dropped system part was filed under the wire field it never entered: %+v", loss)
		}
	}
}
