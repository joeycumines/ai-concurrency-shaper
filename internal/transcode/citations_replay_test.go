package transcode

// A replayed assistant turn reaches the canonical IR through one of two
// supported Responses input arms: a previous output message (an item carrying an
// id) or an easy message (assistant history replayed without one). Both route
// their output_text parts through the same mapper, so both need the same
// request_citations decision — the citation structure does not reach the target,
// and the text itself does.
//
// A function_call_output is the third arm that shares the mapper, but the pinned
// wire contract rejects an output-typed part there, so no annotation can reach
// it: the typed rejection is the honest outcome, not a silent drop.

import (
	"strings"
	"testing"
)

// TestReplayedCitationsAreLossGatedOnEveryArm proves the annotation decision is
// not an accident of which supported arm the item happened to decode through:
// each reachable replay shape records exactly one request_citations loss at the
// path of the part that carried the annotations, and each is refused under
// strict policy.
func TestReplayedCitationsAreLossGatedOnEveryArm(t *testing.T) {
	annotated := `{"type":"output_text","text":"cited","annotations":[{"type":"url_citation"}]}`
	cases := []struct {
		name     string
		body     string
		wantPath string
	}{
		{
			name: "previous output message",
			body: `{"model":"m","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
				{"type":"message","id":"item_1","role":"assistant","status":"completed","content":[` + annotated + `]}
			]}`,
			wantPath: "input[1].content[0].annotations",
		},
		{
			// No id and no status: this dispatches to the EASY message arm,
			// which is the shape a previous-output-only scan missed.
			name: "easy message",
			body: `{"model":"m","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
				{"type":"message","role":"assistant","content":[` + annotated + `]}
			]}`,
			wantPath: "input[1].content[0].annotations",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureRequestCitations: {}}}
			result, _, err := DecodeResponsesRequest([]byte(tc.body), policy)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reportHasAt(result.Report, FeatureRequestCitations, tc.wantPath) {
				t.Fatalf("report = %+v, want request_citations at %s", result.Report.Losses, tc.wantPath)
			}
			count := 0
			for _, loss := range result.Report.Losses {
				if loss.Feature == FeatureRequestCitations {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("request_citations entries = %d, want exactly 1: %+v", count, result.Report.Losses)
			}

			// Strict policy refuses rather than dropping the citation
			// structure silently.
			if _, _, err := DecodeResponsesRequest([]byte(tc.body), StrictLossPolicy()); err == nil {
				t.Fatal("strict decode accepted replayed annotations")
			}

			// The text itself reached the IR losslessly.
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
		})
	}
}

// TestFunctionOutputRejectsOutputTypedParts proves the third arm is closed by the
// pinned contract rather than by an accident of the decoder: an output_text part
// inside a function_call_output is refused as a type outside the supported
// subset, so its annotations can never be dropped silently.
func TestFunctionOutputRejectsOutputTypedParts(t *testing.T) {
	body := `{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[
			{"type":"output_text","text":"cited","annotations":[{"type":"url_citation"}]}
		]}
	]}`
	_, _, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy())
	if err == nil {
		t.Fatal("an output-typed part inside a function_call_output was accepted")
	}
	if !strings.Contains(err.Error(), "output_text") {
		t.Fatalf("err = %q, want it to name the rejected part type", err)
	}
}

// TestReplayedLogprobsAreLossGated proves the SAME replayed output_text part
// that carries citation metadata can also carry token log-probabilities, and
// that dropping them is recorded rather than silent. Both are non-portable
// structures the target request cannot reproduce, and both live on this part, so
// a walker that checks only one of them leaves the other dropped with no entry.
func TestReplayedLogprobsAreLossGated(t *testing.T) {
	part := `{"type":"output_text","text":"chosen","annotations":[],"logprobs":[` +
		`{"token":"chosen","logprob":-0.1}]}`
	cases := []struct {
		name     string
		body     string
		wantPath string
	}{
		{
			name: "previous output message",
			body: `{"model":"m","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
				{"type":"message","id":"item_1","role":"assistant","status":"completed","content":[` + part + `]}
			]}`,
			wantPath: "input[1].content[0].logprobs",
		},
		{
			name: "easy message",
			body: `{"model":"m","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
				{"type":"message","role":"assistant","content":[` + part + `]}
			]}`,
			wantPath: "input[1].content[0].logprobs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureLogprobs: {}}}
			result, _, err := DecodeResponsesRequest([]byte(tc.body), policy)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reportHasAt(result.Report, FeatureLogprobs, tc.wantPath) {
				t.Fatalf("report = %+v, want logprobs at %s", result.Report.Losses, tc.wantPath)
			}
			// Strict policy refuses rather than dropping them silently.
			if _, _, err := DecodeResponsesRequest([]byte(tc.body), StrictLossPolicy()); err == nil {
				t.Fatal("strict decode accepted replayed log-probabilities")
			}
			// The text itself is still lossless.
			found := false
			for _, turn := range result.Request.Turns {
				for _, p := range turn.Parts {
					if text, ok := p.(CanonicalText); ok && text.Text == "chosen" {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("replayed text lost: %+v", result.Request.Turns)
			}
		})
	}
}

// TestUnannotatedReplayedOutputRecordsNothing is the scope guard: an
// output_text part with no annotations is the ordinary case and must stay
// silent, or every replayed assistant turn would look like lost citations.
func TestUnannotatedReplayedOutputRecordsNothing(t *testing.T) {
	body := `{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"plain"}]}
	]}`
	result, _, err := DecodeResponsesRequest([]byte(body), StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reportHasFeature(result.Report, FeatureRequestCitations) {
		t.Fatalf("unannotated replay recorded a citation loss: %+v", result.Report.Losses)
	}
}
