package transcode

import (
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

// TestConversionReportOverflowAggregated pins the CC-REPORT-BOUND
// disposition (operator-observed 2026-09-08: a 1.25MB Claude Code agentic
// request exhausted the 4096-entry bound and 502'd): overflow is an
// observability saturation, never an exchange failure. Both entry paths
// (Lose and Note) stop recording at the bound, record exactly one
// aggregated note, and count the dropped entries.
func TestConversionReportOverflowAggregated(t *testing.T) {
	features := []Feature{FeatureUsageUnknown, FeatureReasoningSummary, FeatureOutputPhase, FeatureImageInput}

	allowed := make(map[Feature]struct{}, len(features))
	for _, feature := range features {
		allowed[feature] = struct{}{}
	}
	fill := func(r *ConversionReport) {
		for i := range maxStreamConversionReportEntries {
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
			// The overflow attempt must SUCCEED (never fail the exchange)
			// and be absorbed into the aggregated note.
			if err := overflow(&report); err != nil {
				t.Fatalf("overflow must not fail the exchange: %v", err)
			}
			if len(report.Losses) != maxStreamConversionReportEntries+1 {
				t.Fatalf("entries = %d, want the bound plus exactly one aggregated note", len(report.Losses))
			}
			last := report.Losses[len(report.Losses)-1]
			if last.Feature != FeatureReportOverflow || last.Kind != NoteRecord {
				t.Fatalf("last entry = %+v, want the report_overflow aggregated note", last)
			}
			if report.Dropped != 1 {
				t.Fatalf("dropped = %d, want 1", report.Dropped)
			}
			// Further overflows keep counting silently; the note stays single.
			for i := range 5 {
				if err := overflow(&report); err != nil {
					t.Fatalf("overflow #%d must not fail: %v", i, err)
				}
			}
			if report.Dropped != 6 {
				t.Fatalf("dropped = %d, want 6", report.Dropped)
			}
			if len(report.Losses) != maxStreamConversionReportEntries+1 {
				t.Fatalf("entries = %d, want the note to stay single", len(report.Losses))
			}
		})
	}
}
