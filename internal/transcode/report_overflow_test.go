package transcode

// The report-overflow note is the ONLY record an operator gets when a
// conversion report saturates, so the count it states has to be the count that
// actually happened. Two ways it can lie: formatting the count before the
// increment (so a saturation always reads "0"), and formatting it once (so
// every later drop leaves the frozen number in place while report.Dropped grows
// past it).

import (
	"fmt"
	"strings"
	"testing"
)

// theOverflowNote returns the report's single overflow note and how many such
// notes exist.
func theOverflowNote(report ConversionReport) (ConversionLoss, int) {
	var note ConversionLoss
	found := 0
	for _, loss := range report.Losses {
		if loss.Feature == FeatureReportOverflow {
			note = loss
			found++
		}
	}
	return note, found
}

// TestReportOverflowNoteStatesTheTrueDroppedCount proves a saturated report
// names the real number of entries it could not record, on every later drop.
func TestReportOverflowNoteStatesTheTrueDroppedCount(t *testing.T) {
	var report ConversionReport
	for range maxStreamConversionReportEntries + 900 {
		if err := report.Note(FeatureMetadata, "p", "d"); err != nil {
			t.Fatalf("Note: %v", err)
		}
	}
	if report.Dropped != 900 {
		t.Fatalf("Dropped = %d, want 900", report.Dropped)
	}
	if len(report.Losses) != maxStreamConversionReportEntries+1 {
		t.Fatalf("losses = %d, want the bound plus one note", len(report.Losses))
	}
	note, found := theOverflowNote(report)
	if found != 1 {
		t.Fatalf("overflow notes = %d, want exactly 1", found)
	}
	if !strings.Contains(note.Detail, "900") {
		t.Fatalf("overflow detail = %q, want it to name the 900 dropped entries", note.Detail)
	}

	// The count keeps tracking: a report does not lose entries in silence
	// after its note exists.
	for range 100 {
		if err := report.Note(FeatureMetadata, "p", "d"); err != nil {
			t.Fatalf("Note: %v", err)
		}
	}
	if report.Dropped != 1000 {
		t.Fatalf("Dropped = %d, want 1000", report.Dropped)
	}
	note, found = theOverflowNote(report)
	if found != 1 {
		t.Fatalf("overflow notes = %d, want exactly 1", found)
	}
	if !strings.Contains(note.Detail, "1000") {
		t.Fatalf("overflow detail = %q, want it to name the 1000 dropped entries", note.Detail)
	}
}

// TestReportOverflowNoteFromAnAbsorbedScratch proves an absorbed scratch's
// dropped entries are reported with their real count, and that the note does
// not claim the parent itself reached its bound: the parent here has room for
// everything the scratch recorded, so a claim about ITS bound would be false.
func TestReportOverflowNoteFromAnAbsorbedScratch(t *testing.T) {
	var parent ConversionReport
	for range 10 {
		if err := parent.Note(FeatureMetadata, "p", "d"); err != nil {
			t.Fatal(err)
		}
	}
	// The scratch dropped 7 entries while recording one. Construct that state
	// directly: a scratch can only reach it by saturating, and a saturated
	// scratch's recorded entries would leave the parent no room.
	scratch := ConversionReport{
		Losses: []ConversionLoss{{
			Feature: FeatureMetadata,
			Path:    "scratch",
			Detail:  "entry",
			Kind:    NoteRecord,
		}},
		Dropped: 7,
	}

	parent.absorb(scratch)
	if parent.Dropped != 7 {
		t.Fatalf("parent Dropped = %d, want 7", parent.Dropped)
	}
	if len(parent.Losses) != 12 {
		t.Fatalf("parent losses = %d, want 10 + the scratch entry + the note", len(parent.Losses))
	}
	note, found := theOverflowNote(parent)
	if found != 1 {
		t.Fatalf("overflow notes = %d, want exactly 1", found)
	}
	if !strings.Contains(note.Detail, "7") {
		t.Fatalf("overflow detail = %q, want it to name the 7 dropped entries", note.Detail)
	}
	if strings.Contains(note.Detail, "past the") {
		t.Fatalf("overflow detail = %q, want no claim that the parent reached its own bound", note.Detail)
	}
}

// TestReportOverflowNoteFollowsRestore proves a transactional rollback rewinds
// the overflow fact with everything else: a failed converter call must not
// leave a saturation note behind, and a report that had already saturated
// before the mark keeps the count it had then.
func TestReportOverflowNoteFollowsRestore(t *testing.T) {
	var report ConversionReport
	for range maxStreamConversionReportEntries {
		if err := report.Note(FeatureMetadata, "p", "d"); err != nil {
			t.Fatal(err)
		}
	}
	// Saturate before the mark.
	for range 5 {
		if err := report.Note(FeatureMetadata, "p", "d"); err != nil {
			t.Fatal(err)
		}
	}
	mark := report.mark()
	if note, found := theOverflowNote(report); found != 1 || !strings.Contains(note.Detail, "5") {
		t.Fatalf("pre-mark note = %+v (found %d), want it to name 5", note, found)
	}

	// A failed call drops more entries and records one.
	if err := report.Note(FeatureMetadata, "p", "d"); err != nil {
		t.Fatal(err)
	}
	if note, found := theOverflowNote(report); found != 1 || !strings.Contains(note.Detail, "6") {
		t.Fatalf("post-drop note = %+v (found %d), want it to name 6", note, found)
	}

	report.restore(mark)
	if report.Dropped != 5 {
		t.Fatalf("Dropped = %d, want the pre-mark 5", report.Dropped)
	}
	if note, found := theOverflowNote(report); found != 1 || !strings.Contains(note.Detail, "5") {
		t.Fatalf("restored note = %+v (found %d), want the pre-mark 5", note, found)
	}

	// A rollback past the saturation leaves neither the note nor the count.
	var fresh ConversionReport
	for range maxStreamConversionReportEntries - 1 {
		if err := fresh.Note(FeatureMetadata, "p", "d"); err != nil {
			t.Fatal(err)
		}
	}
	freshMark := fresh.mark()
	for range 4 {
		if err := fresh.Note(FeatureMetadata, "p", "d"); err != nil {
			t.Fatal(err)
		}
	}
	if _, found := theOverflowNote(fresh); found != 1 {
		t.Fatal("the over-bound drops recorded no overflow note")
	}
	fresh.restore(freshMark)
	if fresh.Dropped != 0 {
		t.Fatalf("Dropped = %d, want 0 after the rollback", fresh.Dropped)
	}
	if note, found := theOverflowNote(fresh); found != 0 {
		t.Fatalf("a rolled-back report kept an overflow note: %+v", note)
	}
	if len(fresh.Losses) != maxStreamConversionReportEntries-1 {
		t.Fatalf("losses = %d, want the pre-mark prefix", len(fresh.Losses))
	}
}

// TestReportOverflowNoteIsTheOnlyAggregate proves the aggregate never competes
// with the entries it aggregates: the note is last-wins state, not a counter
// that grows a second entry per drop.
func TestReportOverflowNoteIsTheOnlyAggregate(t *testing.T) {
	var report ConversionReport
	for i := range maxStreamConversionReportEntries + 50 {
		if err := report.Note(FeatureMetadata, fmt.Sprintf("p%d", i), "d"); err != nil {
			t.Fatal(err)
		}
	}
	if _, found := theOverflowNote(report); found != 1 {
		t.Fatalf("overflow notes = %d, want 1", found)
	}
	if len(report.Losses) != maxStreamConversionReportEntries+1 {
		t.Fatalf("losses = %d, want bound+1", len(report.Losses))
	}
}
