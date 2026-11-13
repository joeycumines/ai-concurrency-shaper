package transcode

// Replayed output text can carry two different pieces of provider metadata, and
// they are not the same kind of thing:
//
//   - CITATIONS are content the client can see and reason about (an answer with
//     sources). The target request cannot reproduce them, so their loss is a
//     policy-gated decision under request_citations.
//   - TOKEN LOG-PROBABILITIES are scoring metadata the provider emitted. The
//     client is relaying what it already received; refusing the replay buys the
//     operator nothing and breaks ordinary traffic (the next turn replays the
//     previous turn's own output). They are a deduped Note, like an input item
//     id.

import (
	"testing"
)

// replayedOutputBody builds a Responses body whose previous output message
// carries the given output_text JSON.
func replayedOutputBody(outputText string) []byte {
	return []byte(`{"model":"m","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"message","id":"item_1","role":"assistant","status":"completed","content":[` +
		outputText +
		`]}]}`)
}

// TestReplayedLogprobsAreNotedNotRefused proves a replayed logprob structure
// decodes under the STRICT policy: it is provider metadata, and refusing it
// would make a default deployment reject its own previous output.
func TestReplayedLogprobsAreNotedNotRefused(t *testing.T) {
	body := replayedOutputBody(`{"type":"output_text","text":"scored",` +
		`"annotations":[],"logprobs":[{"token":"scored","logprob":-0.1}]}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("replayed logprobs refused under the strict policy: %v", err)
	}
	if reportHasFeature(result.Report, FeatureLogprobs) {
		t.Fatalf("the RESPONSE-side logprobs key was charged on the request path: %+v",
			result.Report.Losses)
	}
	if !reportHasFeature(result.Report, FeatureRequestLogprobsReplay) {
		t.Fatalf("report = %+v, want the request_logprobs_replay Note", result.Report.Losses)
	}
	// The text itself is lossless.
	found := false
	for _, turn := range result.Request.Turns {
		for _, part := range turn.Parts {
			if text, ok := part.(CanonicalText); ok && text.Text == "scored" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("replayed text lost: %+v", result.Request.Turns)
	}
}

// TestReplayedCitationsStayPolicyGated proves the citation decision is
// unchanged: approving response logprobs does not approve dropping citations.
func TestReplayedCitationsStayPolicyGated(t *testing.T) {
	body := replayedOutputBody(`{"type":"output_text","text":"cited",` +
		`"annotations":[{"type":"url_citation"}]}`)
	if _, _, err := DecodeResponsesRequest(body, StrictLossPolicy()); err == nil {
		t.Fatal("a replayed citation structure passed the strict policy")
	}
	result, _, err := DecodeResponsesRequest(body, LossPolicy{Allowed: map[Feature]struct{}{
		FeatureRequestCitations: {},
	}})
	if err != nil {
		t.Fatalf("approved decode: %v", err)
	}
	if !reportHasFeature(result.Report, FeatureRequestCitations) {
		t.Fatalf("report = %+v, want the request_citations loss", result.Report.Losses)
	}
}

// TestReplayedLogprobsAreNotedOncePerExchange proves the Note is deduped: the
// same fact repeated on every replayed part is one line, not one per part.
func TestReplayedLogprobsAreNotedOncePerExchange(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"message","id":"item_1","role":"assistant","status":"completed","content":[` +
		`{"type":"output_text","text":"a","annotations":[],"logprobs":[{"token":"a","logprob":-0.1}]},` +
		`{"type":"output_text","text":"b","annotations":[],"logprobs":[{"token":"b","logprob":-0.2}]}` +
		`]}]}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	count := 0
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureRequestLogprobsReplay {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("request_logprobs_replay entries = %d, want 1: %+v", count, result.Report.Losses)
	}
}

// TestReplayedLogprobsOnAnEasyMessageAreNoted proves the other arm behaves
// identically: an assistant easy message replaying output parts takes the same
// decision, because the two arms are one walker.
func TestReplayedLogprobsOnAnEasyMessageAreNoted(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"message","role":"assistant","content":[` +
		`{"type":"output_text","text":"scored","annotations":[],"logprobs":[{"token":"scored","logprob":-0.1}]}` +
		`]}]}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reportHasFeature(result.Report, FeatureRequestLogprobsReplay) {
		t.Fatalf("report = %+v, want the request_logprobs_replay Note", result.Report.Losses)
	}
	if reportHasFeature(result.Report, FeatureLogprobs) {
		t.Fatalf("the RESPONSE-side key was charged on the request path: %+v", result.Report.Losses)
	}
}
