package transcode

// Two invariants of the chat tool-result renderer that the surrounding comments
// already claimed but the code did not implement:
//
//   - the multipart attempt is TRANSACTIONAL: a shape it cannot carry (a
//     document, an unsupported media type, or a client-sent detail the policy
//     refuses) declines so the caller falls back to the JSON envelope, which
//     carries no detail field and is gated by its own approved keys;
//   - committing the scratch report goes through the SAME bounded path as
//     Lose/Note, so a scratch batch can neither push the parent past its
//     entry bound nor emit a second report_overflow note.

import (
	"testing"
)

// TestConversionReportAbsorbRespectsTheBound proves a scratch commit cannot
// exceed the report entry bound and cannot add a second overflow note.
func TestConversionReportAbsorbRespectsTheBound(t *testing.T) {
	var report ConversionReport
	for range maxStreamConversionReportEntries {
		if err := report.Note(FeatureMetadata, "path", "filler"); err != nil {
			t.Fatal(err)
		}
	}
	if len(report.Losses) != maxStreamConversionReportEntries {
		t.Fatalf("losses = %d, want the bound", len(report.Losses))
	}

	scratch := ConversionReport{}
	for range 5 {
		if err := scratch.Note(FeatureMetadata, "scratch", "entry"); err != nil {
			t.Fatal(err)
		}
	}

	report.absorb(scratch)
	// reserve's established contract is "the bound, plus at most one
	// aggregated overflow note": absorbing must honour exactly that, never the
	// unbounded append a raw slice copy produced.
	overflowNotes := 0
	ordinary := 0
	for _, loss := range report.Losses {
		if loss.Feature == FeatureReportOverflow {
			overflowNotes++
			continue
		}
		ordinary++
	}
	if ordinary > maxStreamConversionReportEntries {
		t.Fatalf("absorb recorded %d ordinary entries, past the %d bound",
			ordinary, maxStreamConversionReportEntries)
	}
	if overflowNotes > 1 {
		t.Fatalf("report_overflow notes = %d, want at most one across both reports", overflowNotes)
	}
	if report.Dropped == 0 {
		t.Fatal("absorb did not count the unrecordable entries")
	}
}

// TestConversionReportAbsorbKeepsASmallBatch proves the normal case is
// unchanged: a small scratch batch lands exactly as a direct Note would.
func TestConversionReportAbsorbKeepsASmallBatch(t *testing.T) {
	var report ConversionReport
	scratch := ConversionReport{}
	if err := scratch.Lose(
		LossPolicy{Allowed: map[Feature]struct{}{FeatureMetadata: {}}},
		FeatureMetadata, "metadata", "detail",
	); err != nil {
		t.Fatal(err)
	}
	report.absorb(scratch)
	if len(report.Losses) != 1 || report.Losses[0].Feature != FeatureMetadata {
		t.Fatalf("report = %+v, want the single metadata entry", report.Losses)
	}
	if report.Dropped != 0 {
		t.Fatalf("dropped = %d, want 0", report.Dropped)
	}
}
