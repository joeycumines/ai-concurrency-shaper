package transcode

import "testing"

// TestChatToolResultMultiPartTextJoinReported pins autopsy 2026-09-06 H2: a
// multi-part all-text tool result renders as the joined string AND records
// the tool_result_text_join decision — the join is observable, never
// silent. A single-part text result stays byte-exact with no entry.
func TestChatToolResultMultiPartTextJoinReported(t *testing.T) {
	multi := []CanonicalPart{
		CanonicalText{Text: "alpha"},
		CanonicalText{Text: "beta"},
		CanonicalText{Text: "gamma"},
	}

	// Under a policy approving the join, the rendering is the joined string
	// and the report carries the key.
	var report ConversionReport
	msg, err := renderChatToolResult(
		CanonicalFunctionResult{CallID: "call_1", Parts: multi},
		LossPolicy{Allowed: map[Feature]struct{}{FeatureToolResultTextJoin: {}}},
		&report,
	)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content == nil || msg.Content.ContentStr == nil {
		t.Fatalf("expected string content arm: %+v", msg)
	}
	if got := *msg.Content.ContentStr; got != "alpha\nbeta\ngamma" {
		t.Fatalf("joined text = %q", got)
	}
	if !reportHasFeature(report, FeatureToolResultTextJoin) {
		t.Fatalf("silent join: report lacks tool_result_text_join: %+v", report)
	}

	// Strict policy rejects the join instead of merging silently.
	var strictReport ConversionReport
	if _, err := renderChatToolResult(
		CanonicalFunctionResult{CallID: "call_1", Parts: multi},
		StrictLossPolicy(),
		&strictReport,
	); err == nil {
		t.Fatal("strict policy silently accepted the multi-part join")
	}

	// A single text part stays exact and records nothing.
	var singleReport ConversionReport
	msg, err = renderChatToolResult(
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
