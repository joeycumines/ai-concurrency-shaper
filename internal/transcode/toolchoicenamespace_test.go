package transcode

// A named tool_choice is the client pointing at ONE tool it declared. The
// flattening map exists to translate a namespace child's bare name onto the
// wire spelling, and it must never be used to guess: a bare name two namespace
// children share identifies neither of them, so the request is refused rather
// than silently bound to whichever child happened to claim the bare flat name
// first. A plain function that owns the name is a different case — the client
// plausibly meant it — so that bind is made but recorded, because the client
// could have meant a child whose flat name had to be qualified.

import (
	"strings"
	"testing"
)

// decodeToolChoice decodes a Responses request body and returns the resulting
// canonical choice together with the report.
func decodeToolChoice(t *testing.T, body []byte, policy LossPolicy) (*CanonicalToolChoice, ConversionReport, error) {
	t.Helper()
	result, _, err := DecodeResponsesRequest(body, policy)
	if err != nil {
		return nil, ConversionReport{}, err
	}
	return result.Request.ToolChoice, result.Report, nil
}

// TestNamedToolChoiceSharedBareNameIsRefused proves the ambiguity refusal the
// README promises: two namespace children that share a bare child name make a
// choice of that bare name ambiguous, so it is refused — not bound to the child
// that happened to claim the bare flat name.
func TestNamedToolChoiceSharedBareNameIsRefused(t *testing.T) {
	for _, order := range []string{"ns1-first", "ns2-first"} {
		t.Run(order, func(t *testing.T) {
			first, second := "ns1", "ns2"
			if order == "ns2-first" {
				first, second = second, first
			}
			body := []byte(`{
				"model":"m",
				"input":"hi",
				"tool_choice":{"type":"function","name":"dup"},
				"tools":[
					{"type":"namespace","name":"` + first + `","tools":[
						{"type":"function","name":"dup","strict":false,"parameters":{"type":"object"}}
					]},
					{"type":"namespace","name":"` + second + `","tools":[
						{"type":"function","name":"dup","strict":false,"parameters":{"type":"object"}}
					]}
				]
			}`)
			choice, _, err := decodeToolChoice(t, body, StrictLossPolicy())
			if err == nil {
				t.Fatalf("an ambiguous named choice was accepted and bound to %+v", choice)
			}
			if !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("err = %q, want it to explain the ambiguity", err)
			}
		})
	}
}

// TestNamedToolChoiceSharedBareNameIsRefusedEvenWithAPlainOwner proves the
// refusal does not depend on which tool claimed the bare flat name: a plain
// function that owns the name does not make an ambiguous child reference safe
// to bind, because the client may have meant either child.
func TestNamedToolChoiceSharedBareNameIsRefusedEvenWithAPlainOwner(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tool_choice":{"type":"function","name":"dup"},
		"tools":[
			{"type":"function","name":"dup","strict":false,"parameters":{"type":"object"}},
			{"type":"namespace","name":"a","tools":[
				{"type":"function","name":"dup","strict":false,"parameters":{"type":"object"}}
			]},
			{"type":"namespace","name":"b","tools":[
				{"type":"function","name":"dup","strict":false,"parameters":{"type":"object"}}
			]}
		]
	}`)
	choice, _, err := decodeToolChoice(t, body, StrictLossPolicy())
	if err == nil {
		t.Fatalf("an ambiguous named choice was accepted and bound to %+v", choice)
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %q, want it to explain the ambiguity", err)
	}
}

// TestNamedToolChoiceUniqueChildBindsWithoutTranslation proves the ordinary
// resolving case still works: a single namespace child whose bare name is free
// keeps that name, so the client's own spelling is already the wire spelling.
func TestNamedToolChoiceUniqueChildBindsWithoutTranslation(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"input":"hi",
		"tool_choice":{"type":"function","name":"search"},
		"tools":[
			{"type":"namespace","name":"ns","tools":[
				{"type":"function","name":"search","strict":false,"parameters":{"type":"object"}}
			]}
		]
	}`)
	choice, report, err := decodeToolChoice(t, body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if choice == nil || choice.Mode != "named" || choice.Name != "search" {
		t.Fatalf("tool_choice = %+v, want the child's own bare flat name", choice)
	}
	// The flatten itself is still a sanctioned encoding worth recording; what
	// must be absent is a reinterpretation note, because the client's own
	// spelling already named the tool.
	if reportHasFeature(report, FeatureToolChoiceNamespaceBind) {
		t.Fatalf("a unique child bind is not a reinterpretation: %+v", report.Losses)
	}
}

// TestPlainOwnerToolChoiceBindIsNotedUnderItsOwnKey proves the plain-owner bind
// is recorded under a key that describes a tool-choice bind, not under the key
// for replayed calls that name an undeclared namespace. An operator grepping
// one meaning must not see the other.
func TestPlainOwnerToolChoiceBindIsNotedUnderItsOwnKey(t *testing.T) {
	body := []byte(`{
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
	choice, report, err := decodeToolChoice(t, body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if choice == nil || choice.Name != "dup" {
		t.Fatalf("tool_choice = %+v, want the plain function's own name", choice)
	}
	if !reportHasAt(report, FeatureToolChoiceNamespaceBind, "tool_choice") {
		t.Fatalf("report = %+v, want the bind Note under its own key", report.Losses)
	}
	if reportHasFeature(report, FeatureNamespaceReplayUndeclared) {
		t.Fatalf("a tool-choice bind was filed under namespace_replay_undeclared: %+v", report.Losses)
	}
	for _, loss := range report.Losses {
		if loss.Feature == FeatureToolChoiceNamespaceBind && loss.Kind != NoteRecord {
			t.Fatalf("the bind is a sanctioned encoding, not a policy decision: %+v", loss)
		}
	}
}
