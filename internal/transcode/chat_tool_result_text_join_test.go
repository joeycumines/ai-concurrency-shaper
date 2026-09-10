package transcode

import "testing"

// TestChatToolResultMultiPartTextJoinReported pins the multi-part join
// (acceptance): a multi-part all-text tool result renders as the
// joined string AND records exactly one sanctioned Note — the join is
// observable, never silent, and NEVER policy-gated (every content byte is
// preserved; the part boundaries are structural metadata the chat dialect
// cannot carry). A single-part text result stays byte-exact with no entry.
func TestChatToolResultMultiPartTextJoinReported(t *testing.T) {
	multi := []CanonicalPart{
		CanonicalText{Text: "alpha"},
		CanonicalText{Text: "beta"},
		CanonicalText{Text: "gamma"},
	}

	// The join works under EVERY policy — strict included — because it is a
	// sanctioned encoding, not a loss.
	for name, policy := range map[string]LossPolicy{
		"strict":     StrictLossPolicy(),
		"permissive": {Allowed: map[Feature]struct{}{FeatureToolResultErrorStatus: {}}},
	} {
		var report ConversionReport
		msg, err := renderChatToolResult(
			CanonicalFunctionResult{CallID: "call_1", Parts: multi},
			policy,
			&report,
		)
		if err != nil {
			t.Fatalf("%s: the join must never be policy-rejected: %v", name, err)
		}
		if msg.Content == nil || msg.Content.ContentStr == nil {
			t.Fatalf("%s: expected string content arm: %+v", name, msg)
		}
		if got := *msg.Content.ContentStr; got != "alpha\nbeta\ngamma" {
			t.Fatalf("%s: joined text = %q", name, got)
		}
		if !reportHasFeature(report, FeatureToolResultTextJoin) {
			t.Fatalf("%s: silent join: report lacks tool_result_text_join: %+v", name, report)
		}
		// EXACTLY ONE note: a duplicate Note would spam the aggregated loss
		// line.
		entries := 0
		for _, entry := range report.Losses {
			if entry.Feature != FeatureToolResultTextJoin {
				t.Fatalf("%s: unexpected report entry: %+v", name, entry)
			}
			if entry.Kind != NoteRecord {
				t.Fatalf("%s: join entry kind = %v, want NoteRecord", name, entry.Kind)
			}
			entries++
		}
		if entries != 1 {
			t.Fatalf("%s: join entries = %d, want exactly 1", name, entries)
		}
	}

	// A single text part stays exact and records nothing.
	var singleReport ConversionReport
	msg, err := renderChatToolResult(
		CanonicalFunctionResult{
			CallID: "call_1",
			Parts:  []CanonicalPart{CanonicalText{Text: "only"}},
		},
		StrictLossPolicy(),
		&singleReport,
	)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content == nil || msg.Content.ContentStr == nil || *msg.Content.ContentStr != "only" {
		t.Fatalf("single-part rendering changed: %+v", msg)
	}
	if len(singleReport.Losses) != 0 {
		t.Fatalf("single-part result recorded entries: %+v", singleReport)
	}
}
