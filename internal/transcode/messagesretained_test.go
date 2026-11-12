package transcode

// The retained-turn exemption documented on CanonicalRequest.RetainedTurns is a
// renderer-wide rule, not a Chat-only one: a position the continuity store
// reconstructed was already decided when the chain was first rendered, so
// re-charging it to the client can reject a request the client authored
// cleanly. These tests pin the Messages renderer's side of that rule.

import (
	"encoding/json"
	"testing"
)

// messagesRetainedRequest is a client-authored request whose leading turns are
// marked as reconstructed by the continuity store.
func retainedMessagesRequest(turns []CanonicalTurn, retained int) CanonicalRequest {
	return CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns:           turns,
		RetainedTurns:   retained,
	}
}

// TestMessagesRetainedMidConversationSystemIsANote proves a retained
// mid-dialog system turn does not trip the client-facing position gate.
func TestMessagesRetainedMidConversationSystemIsANote(t *testing.T) {
	text := func(s string) []CanonicalPart { return []CanonicalPart{CanonicalText{Text: s}} }
	request := retainedMessagesRequest([]CanonicalTurn{
		{Role: CanonicalUser, Parts: text("hello")},
		{Role: CanonicalAssistant, Parts: text("hi")},
		{Role: CanonicalSystem, Parts: text("retained mid")},
		{Role: CanonicalUser, Parts: text("next")},
	}, 3)

	// Strict: no approval, yet the retained position must not reject.
	rendered, report, err := RenderMessagesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("retained history must not be charged to the client: %v", err)
	}
	if len(report.Losses) > 0 {
		for _, loss := range report.Losses {
			if loss.Feature == FeatureMidConversationSystem && loss.Kind == LossRecord {
				t.Fatalf("retained position recorded as a policy-gated loss: %+v", loss)
			}
		}
	}
	if !reportHasFeature(report, FeatureMidConversationSystem) {
		t.Fatalf("report = %+v, want the mid_conversation_system Note", report.Losses)
	}

	// The system field carries the retained text.
	var out struct {
		Model     string            `json:"model"`
		MaxTokens int               `json:"max_tokens"`
		System    json.RawMessage   `json:"system"`
		Messages  []json.RawMessage `json:"messages"`
	}
	if err := strictDecode(rendered, &out); err != nil {
		t.Fatalf("rendered: %v\n%s", err, rendered)
	}
	// A single text block renders as the string arm.
	var system string
	if err := json.Unmarshal(out.System, &system); err != nil {
		t.Fatalf("system = %s, want the single-string arm: %v", out.System, err)
	}
	if system != "retained mid" {
		t.Fatalf("system = %q, want the retained text", system)
	}
}

// TestMessagesClientMidConversationSystemIsStillGated is the scope guard: a
// CLIENT-authored mid-conversation system turn still takes the policy-gated
// loss and still refuses under strict policy.
func TestMessagesClientMidConversationSystemIsStillGated(t *testing.T) {
	text := func(s string) []CanonicalPart { return []CanonicalPart{CanonicalText{Text: s}} }
	request := retainedMessagesRequest([]CanonicalTurn{
		{Role: CanonicalUser, Parts: text("hello")},
		{Role: CanonicalSystem, Parts: text("client mid")},
	}, 0)

	if _, _, err := RenderMessagesRequest(request, testExchangeContext()); err == nil {
		t.Fatal("strict render accepted a client-authored mid-conversation system turn")
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureMidConversationSystem: {}}}
	_, report, err := RenderMessagesRequest(request, context)
	if err != nil {
		t.Fatalf("approved render: %v", err)
	}
	found := false
	for _, loss := range report.Losses {
		if loss.Feature == FeatureMidConversationSystem && loss.Kind == LossRecord {
			found = true
		}
	}
	if !found {
		t.Fatalf("report = %+v, want the policy-gated mid_conversation_system loss", report.Losses)
	}
}

// TestMessagesLeadingMergeNoteCountsClientTurns proves the leading-merge Note
// reports a real consolidation of two leading system turns whichever provenance
// they came from: the merge is a sanctioned encoding (content is joined, not
// dropped), so it consults no policy and the Chat target reports the identical
// shape. One client turn plus one retained turn still consolidates two system
// blocks, so it is noted too.
func TestMessagesLeadingMergeNoteCountsClientTurns(t *testing.T) {
	text := func(s string) []CanonicalPart { return []CanonicalPart{CanonicalText{Text: s}} }

	// Two client leading system turns.
	client := retainedMessagesRequest([]CanonicalTurn{
		{Role: CanonicalSystem, Parts: text("one")},
		{Role: CanonicalSystem, Parts: text("two")},
		{Role: CanonicalUser, Parts: text("hi")},
	}, 0)
	_, report, err := RenderMessagesRequest(client, testExchangeContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasFeature(report, FeatureMidConversationSystem) {
		t.Fatalf("report = %+v, want the leading-merge Note", report.Losses)
	}

	// One retained leading system turn plus one client one: two system blocks
	// still consolidate into one field, so the merge is still reported.
	mixed := retainedMessagesRequest([]CanonicalTurn{
		{Role: CanonicalSystem, Parts: text("retained")},
		{Role: CanonicalSystem, Parts: text("client")},
		{Role: CanonicalUser, Parts: text("hi")},
	}, 1)
	_, report, err = RenderMessagesRequest(mixed, testExchangeContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasFeature(report, FeatureMidConversationSystem) {
		t.Fatalf("report = %+v, want the leading-merge Note for the mixed leading turns", report.Losses)
	}
	for _, loss := range report.Losses {
		if loss.Feature == FeatureMidConversationSystem && loss.Kind == LossRecord {
			t.Fatalf("a sanctioned merge was recorded as a policy-gated loss: %+v", loss)
		}
	}
}

// TestMessagesClientLeadingMergeAfterRetainedDialogIsNoted proves retained
// dialog does not suppress the leading-merge Note for two CLIENT leading system
// turns: the proxy created the earlier dialog position, so the client's own
// leading turns still consolidate and must still be reported.
func TestMessagesClientLeadingMergeAfterRetainedDialogIsNoted(t *testing.T) {
	text := func(s string) []CanonicalPart { return []CanonicalPart{CanonicalText{Text: s}} }
	request := retainedMessagesRequest([]CanonicalTurn{
		// Retained prefix: dialog only.
		{Role: CanonicalUser, Parts: text("retained q")},
		{Role: CanonicalAssistant, Parts: text("retained a")},
		// Client: two leading system turns, then dialog.
		{Role: CanonicalSystem, Parts: text("one")},
		{Role: CanonicalSystem, Parts: text("two")},
		{Role: CanonicalUser, Parts: text("hi")},
	}, 2)
	_, report, err := RenderMessagesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasFeature(report, FeatureMidConversationSystem) {
		t.Fatalf("report = %+v, want the leading-merge Note", report.Losses)
	}
	for _, loss := range report.Losses {
		if loss.Feature == FeatureMidConversationSystem && loss.Kind == LossRecord {
			t.Fatalf("the client's leading turns were charged as a position loss: %+v", loss)
		}
	}
}
