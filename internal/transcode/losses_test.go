package transcode

import "testing"

// TestParseLossFeatures proves the CLI loss-feature names validate at
// startup and unknown names are rejected (review-j finding 14).
func TestParseLossFeatures(t *testing.T) {
	allowed, err := ParseLossFeatures("usage_timing", "reasoning_summary,phase", " image_input ")
	if err != nil {
		t.Fatal(err)
	}
	for _, feature := range []Feature{FeatureUsageTiming, FeatureReasoningSummary, FeaturePhase, FeatureImageInput} {
		if _, ok := allowed[feature]; !ok {
			t.Fatalf("feature %s missing", feature)
		}
	}
	if _, err := ParseLossFeatures("bogus_feature"); err == nil {
		t.Fatal("unknown feature accepted")
	}
	if _, err := ParseLossFeatures(""); err != nil {
		t.Fatalf("empty input must be valid: %v", err)
	}
}
