package transcode

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestParseLossFeatures proves the CLI loss-feature names validate at
// startup and unknown names are rejected (review-j finding 14).
func TestParseLossFeatures(t *testing.T) {
	allowed, err := ParseLossFeatures("usage_unknown", "reasoning_summary,output_phase", " image_input ")
	if err != nil {
		t.Fatal(err)
	}
	for _, feature := range []Feature{FeatureUsageUnknown, FeatureReasoningSummary, FeatureOutputPhase, FeatureImageInput} {
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

// TestConversionReportNoteBound proves Note enforces the same exchange bound
// as Lose (review-gate task-12 finding 5): notes and losses accumulate into
// one slice, so either path driving the report past the bound must surface
// the typed corruption error instead of growing the report unboundedly.
func TestConversionReportNoteBound(t *testing.T) {
	features := []Feature{FeatureUsageUnknown, FeatureReasoningSummary, FeatureOutputPhase, FeatureImageInput}

	allowed := make(map[Feature]struct{}, len(features))
	for _, feature := range features {
		allowed[feature] = struct{}{}
	}
	fill := func(r *ConversionReport) {
		for i := 0; i < maxStreamConversionReportEntries; i++ {
			// Alternate Lose and Note so the shared reserve is exercised by
			// both entry paths on one report.
			if i%2 == 0 {
				if err := r.Lose(LossPolicy{Allowed: allowed}, features[i%len(features)], "p", "d"); err != nil {
					t.Fatalf("Lose #%d: %v", i, err)
				}
			} else {
				if err := r.Note(features[i%len(features)], "p", "d"); err != nil {
					t.Fatalf("Note #%d: %v", i, err)
				}
			}
		}
		if len(r.Losses) != maxStreamConversionReportEntries {
			t.Fatalf("entries = %d, want %d", len(r.Losses), maxStreamConversionReportEntries)
		}
	}

	for name, overflow := range map[string]func(r *ConversionReport) error{
		"lose": func(r *ConversionReport) error {
			return r.Lose(LossPolicy{Allowed: allowed}, FeatureUsageUnknown, "p", "d")
		},
		"note": func(r *ConversionReport) error {
			return r.Note(FeatureUsageUnknown, "p", "d")
		},
	} {
		t.Run(name, func(t *testing.T) {
			var report ConversionReport
			fill(&report)
			err := overflow(&report)
			if err == nil {
				t.Fatal("overflow accepted; want the typed exchange-bound error")
			}
			var target *UpstreamWireError
			if !errors.As(err, &target) {
				t.Fatalf("error = %T: %v, want UpstreamWireError", err, err)
			}
			if got := fmt.Sprintf("%v", target.Cause); !strings.Contains(got, fmt.Sprintf("exchange bound of %d entries", maxStreamConversionReportEntries)) {
				t.Fatalf("cause = %q, want it to name the bound", got)
			}
			if len(report.Losses) != maxStreamConversionReportEntries {
				t.Fatalf("entries = %d, want the bound held", len(report.Losses))
			}
		})
	}
}
