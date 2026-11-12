package transcode

// Namespace flattening and replay decisions that must stay honest: a flatten is
// not a drop, a duplicate flatten is refused, a client's named choice resolves
// through the exchange map, and replayed citation metadata is not silently
// discarded.

import (
	"strings"
	"testing"
)

// TestSuccessfulNamespaceFlattenIsNotABuiltinDrop proves the success note uses
// its own key so the log never reads as if a built-in tool was dropped.
func TestSuccessfulNamespaceFlattenIsNotABuiltinDrop(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tools":[
			{"type":"namespace","name":"ns","tools":[
				{"type":"function","name":"search","strict":false,"parameters":{"type":"object"}}
			]}
		]
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var flattenNotes, builtinEntries int
	for _, loss := range result.Report.Losses {
		switch loss.Feature {
		case FeatureToolNamespaceFlatten:
			flattenNotes++
			if loss.Kind != NoteRecord {
				t.Fatalf("flatten entry is a %v, want a Note", loss.Kind)
			}
		case FeatureBuiltinTools:
			builtinEntries++
		}
	}
	if flattenNotes != 1 {
		t.Fatalf("flatten notes = %d, want exactly 1: %+v", flattenNotes, result.Report.Losses)
	}
	if builtinEntries != 0 {
		t.Fatalf("a successful flatten recorded a builtin_tools entry: %+v", result.Report.Losses)
	}
}

// TestNestedNamespaceFlatNameCollisionIsRefused proves two outer namespaces that
// each contain an inner namespace of the same name and child cannot produce two
// distinct tools, so the request is refused rather than silently re-pointed.
func TestNestedNamespaceFlatNameCollisionIsRefused(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tools":[
			{"type":"namespace","name":"outer1","tools":[
				{"type":"namespace","name":"inner","tools":[
					{"type":"function","name":"spawn","strict":false,"parameters":{"type":"object"}}
				]}
			]},
			{"type":"namespace","name":"outer2","tools":[
				{"type":"namespace","name":"inner","tools":[
					{"type":"function","name":"spawn","strict":false,"parameters":{"type":"object"}}
				]}
			]}
		]
	}`)
	_, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err == nil {
		t.Fatal("an ambiguous nested namespace flatten was accepted")
	}
	if !strings.Contains(err.Error(), "same name") {
		t.Fatalf("err = %q, want it to explain the collision", err)
	}
}

// TestNamedToolChoiceResolvesThroughTheNamespaceMap proves a client naming a
// namespace child whose flat name had to be qualified still renders.
func TestNamedToolChoiceResolvesThroughTheNamespaceMap(t *testing.T) {
	// A plain tool owns "spawn", so the namespace child "spawn" is qualified
	// as ns__spawn. A client naming "spawn" means the plain tool.
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tool_choice":{"type":"function","name":"spawn"},
		"tools":[
			{"type":"function","name":"spawn","strict":false,"parameters":{"type":"object"}},
			{"type":"namespace","name":"ns","tools":[
				{"type":"function","name":"spawn","strict":false,"parameters":{"type":"object"}}
			]}
		]
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Request.ToolChoice == nil || result.Request.ToolChoice.Name != "spawn" {
		t.Fatalf("tool_choice = %+v, want the plain tool's own name", result.Request.ToolChoice)
	}

	// A namespace child whose bare name is NOT taken by a plain tool flattens
	// to the bare name, so the client's own name resolves without translation.
	unqualified := []byte(`{
		"model":"m",
		"input":"hi",
		"tool_choice":{"type":"function","name":"search"},
		"tools":[
			{"type":"namespace","name":"ns","tools":[
				{"type":"function","name":"search","strict":false,"parameters":{"type":"object"}}
			]}
		]
	}`)
	unqualifiedResult, _, err := DecodeResponsesRequest(unqualified, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if unqualifiedResult.Request.ToolChoice == nil || unqualifiedResult.Request.ToolChoice.Name != "search" {
		t.Fatalf("tool_choice = %+v, want the child's bare flat name", unqualifiedResult.Request.ToolChoice)
	}

	// When a plain function owns the name, the client means that function: the
	// flatten qualifies the namespace child, so the bare name is unambiguous and
	// the choice is not re-pointed at the child.
	plainWins := []byte(`{
		"model":"m",
		"input":"hi",
		"tool_choice":{"type":"function","name":"dup"},
		"tools":[
			{"type":"function","name":"dup","strict":false,"parameters":{"type":"object"}},
			{"type":"namespace","name":"a","tools":[
				{"type":"function","name":"dup","strict":false,"parameters":{"type":"object"}}
			]}
		]
	}`)
	plainResult, _, err := DecodeResponsesRequest(plainWins, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if plainResult.Request.ToolChoice == nil || plainResult.Request.ToolChoice.Name != "dup" {
		t.Fatalf("tool_choice = %+v, want the plain tool's own name", plainResult.Request.ToolChoice)
	}
	// The bind is a possible reinterpretation (the client might have meant the
	// qualified namespace child), so it is recorded rather than silent.
	if !reportHasAt(plainResult.Report, FeatureNamespaceReplayUndeclared, "tool_choice") {
		t.Fatalf("report = %+v, want a tool_choice note for the plain-owner bind", plainResult.Report.Losses)
	}

	// A name matching NO surviving tool still refuses: the client asked for a
	// tool the exchange cannot provide.
	missing := []byte(`{
		"model":"m",
		"input":"hi",
		"tool_choice":{"type":"function","name":"nope"},
		"tools":[
			{"type":"function","name":"real","strict":false,"parameters":{"type":"object"}}
		]
	}`)
	if _, _, err := DecodeResponsesRequest(missing, StrictLossPolicy()); err == nil {
		t.Fatal("a dangling named choice was accepted")
	}
}

// TestReplayedCitationsAreLossGated proves annotations on replayed output text
// are recorded under request_citations rather than discarded silently, matching
// the Anthropic decoder.
func TestReplayedCitationsAreLossGated(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"message","id":"item_1","role":"assistant","status":"completed","content":[
				{"type":"output_text","text":"cited","annotations":[{"type":"url_citation"}]}
			]}
		]
	}`)
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureRequestCitations: {}}}
	result, _, err := DecodeResponsesRequest(body, policy)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reportHasFeature(result.Report, FeatureRequestCitations) {
		t.Fatalf("report = %+v, want the request_citations decision", result.Report.Losses)
	}
	// Strict policy refuses rather than dropping the citation structure.
	if _, _, err := DecodeResponsesRequest(body, StrictLossPolicy()); err == nil {
		t.Fatal("strict decode accepted replayed annotations")
	}
	// The text reached the IR losslessly.
	found := false
	for _, turn := range result.Request.Turns {
		for _, part := range turn.Parts {
			if text, ok := part.(CanonicalText); ok && text.Text == "cited" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("replayed text lost: %+v", result.Request.Turns)
	}
}
