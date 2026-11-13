package transcode

// The conversion report is bounded, and its bound is observable: a report that
// dropped entries must carry exactly one aggregated report_overflow note and a
// Dropped count. A transactional sub-render commits through absorb, so absorb
// must preserve that property no matter which of the two reports saturated —
// including when the SCRATCH one did, which is the case a parent-saturated test
// alone cannot see.

import (
	"testing"
)

// fillReport adds count ordinary notes to a report.
func fillReport(t *testing.T, report *ConversionReport, count int, path string) {
	t.Helper()
	for range count {
		if err := report.Note(FeatureMetadata, path, "entry"); err != nil {
			t.Fatalf("note: %v", err)
		}
	}
}

// overflowNoteCount returns how many report_overflow notes a report holds.
func overflowNoteCount(report ConversionReport) int {
	count := 0
	for _, loss := range report.Losses {
		if loss.Feature == FeatureReportOverflow {
			count++
		}
	}
	return count
}

// TestConversionReportAbsorbWithASaturatedScratch proves the absorbing report
// ends in the documented state when the absorbed one had already saturated: the
// bound of ordinary entries, ONE overflow note, and a Dropped count equal to the
// entries neither report could hold. Previously the flag was carried across
// before the copy, so the parent never appended its own note and then counted
// the scratch's note as a dropped entry — a report claiming dropped entries it
// could not explain.
func TestConversionReportAbsorbWithASaturatedScratch(t *testing.T) {
	// Saturate the scratch: the bound of ordinary entries plus its own
	// aggregated note, plus one further dropped entry.
	var scratch ConversionReport
	fillReport(t, &scratch, maxStreamConversionReportEntries, "scratch")
	if err := scratch.Note(FeatureMetadata, "over-the-bound", "entry"); err != nil {
		t.Fatalf("note: %v", err)
	}
	if scratch.Dropped != 1 {
		t.Fatalf("scratch Dropped = %d, want 1", scratch.Dropped)
	}
	if got := overflowNoteCount(scratch); got != 1 {
		t.Fatalf("scratch overflow notes = %d, want 1", got)
	}

	var parent ConversionReport
	parent.absorb(scratch)

	if got := overflowNoteCount(parent); got != 1 {
		t.Fatalf("absorbed overflow notes = %d, want exactly 1: a report that dropped entries "+
			"must say so, and only once", got)
	}
	if got := len(parent.Losses); got != maxStreamConversionReportEntries+1 {
		t.Fatalf("absorbed losses = %d, want the bound plus the aggregated note (%d)",
			got, maxStreamConversionReportEntries+1)
	}
	// One ordinary entry was dropped by the scratch and the parent could take
	// everything the scratch recorded, so exactly that one entry is unrecorded.
	if parent.Dropped != 1 {
		t.Fatalf("absorbed Dropped = %d, want 1 (the entry the scratch itself dropped)", parent.Dropped)
	}
}

// TestConversionReportAbsorbReachesTheBoundExactlyWhenTheScratchDid proves both
// sides saturating still yields one note and an exact count: the parent takes
// the single entry it has room for, and the rest — including the entries the
// scratch itself dropped — are counted once each.
func TestConversionReportAbsorbIntoNearlyFullParentWithSaturatedScratch(t *testing.T) {
	var parent ConversionReport
	fillReport(t, &parent, maxStreamConversionReportEntries-1, "parent")

	var scratch ConversionReport
	fillReport(t, &scratch, maxStreamConversionReportEntries, "scratch")
	if err := scratch.Note(FeatureMetadata, "over-the-bound", "entry"); err != nil {
		t.Fatalf("note: %v", err)
	}
	if scratch.Dropped != 1 {
		t.Fatalf("scratch Dropped = %d, want 1", scratch.Dropped)
	}

	parent.absorb(scratch)

	if got := overflowNoteCount(parent); got != 1 {
		t.Fatalf("absorbed overflow notes = %d, want exactly 1", got)
	}
	if got := len(parent.Losses); got != maxStreamConversionReportEntries+1 {
		t.Fatalf("absorbed losses = %d, want the bound plus the aggregated note (%d)",
			got, maxStreamConversionReportEntries+1)
	}
	// The parent had room for exactly one scratch entry; every other scratch
	// entry plus the scratch's own dropped entry is unrecorded.
	wantDropped := (maxStreamConversionReportEntries - 1) + scratch.Dropped
	if parent.Dropped != wantDropped {
		t.Fatalf("absorbed Dropped = %d, want %d", parent.Dropped, wantDropped)
	}
}

// TestConversionReportAbsorbSmallScratchIsUnchanged proves the ordinary path is
// untouched: a small scratch appends exactly its own entries and records no
// overflow note.
func TestConversionReportAbsorbSmallScratchIsUnchanged(t *testing.T) {
	var parent, scratch ConversionReport
	fillReport(t, &parent, 2, "parent")
	fillReport(t, &scratch, 3, "scratch")

	parent.absorb(scratch)

	if got := len(parent.Losses); got != 5 {
		t.Fatalf("absorbed losses = %d, want 5", got)
	}
	if parent.Dropped != 0 {
		t.Fatalf("absorbed Dropped = %d, want 0", parent.Dropped)
	}
	if got := overflowNoteCount(parent); got != 0 {
		t.Fatalf("absorbed overflow notes = %d, want 0", got)
	}
}

// TestConversionReportAbsorbCarriesADroppedCountEvenWithRoom proves a scratch's
// dropped entries are carried as a COUNT rather than replayed as entries. The
// parent here has room for everything the scratch RECORDED, so a replay would
// silently discard the dropped count; the count must ride across and the note
// must still be present, because a report that dropped entries has to say so.
func TestConversionReportAbsorbCarriesADroppedCountEvenWithRoom(t *testing.T) {
	// The scratch reports entries it could not record while the parent still
	// has room: construct that state directly, since a scratch can only reach
	// it by saturating and a saturated scratch's recorded entries would not
	// leave the parent room.
	scratch := ConversionReport{
		Losses: []ConversionLoss{{
			Feature: FeatureMetadata,
			Path:    "scratch",
			Detail:  "entry",
			Kind:    NoteRecord,
		}},
		Dropped: 3,
	}

	var parent ConversionReport
	parent.absorb(scratch)

	if got := len(parent.Losses); got != 2 {
		t.Fatalf("absorbed losses = %d, want the scratch entry plus one overflow note", got)
	}
	if parent.Dropped != 3 {
		t.Fatalf("absorbed Dropped = %d, want the scratch's 3", parent.Dropped)
	}
	if got := overflowNoteCount(parent); got != 1 {
		t.Fatalf("absorbed overflow notes = %d, want exactly 1", got)
	}
}
