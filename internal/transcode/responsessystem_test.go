package transcode

// The Responses create-request carries system content in ONE instructions
// string, so every system-channel turn must join into it: discarding the
// prompt because the source happened to split it across turns is a stronger
// loss than the documented "they cannot keep their boundaries". Non-text parts
// take the system_non_text_content decision individually, and a mid-conversation
// or developer turn records its own position decision rather than moving
// silently.

import (
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// responsesSystemRequest builds a request with the given turns and no tools.
func responsesSystemRequest(turns ...CanonicalTurn) CanonicalRequest {
	return CanonicalRequest{ClientModel: "m", MaxOutputTokens: new(64), Turns: turns}
}

// renderResponsesInstructions renders request and returns the emitted
// instructions together with the report.
func renderResponsesInstructions(
	t *testing.T,
	request CanonicalRequest,
	policy LossPolicy,
) (*string, ConversionReport) {
	t.Helper()
	context := testExchangeContext()
	context.LossPolicy = policy
	rendered, report, err := RenderResponsesRequest(request, context)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var envelope openairesponses.Request
	if err := strictDecode(rendered, &envelope); err != nil {
		t.Fatalf("rendered responses: %v\n%s", err, rendered)
	}
	return envelope.Instructions, report
}

// TestResponsesInstructionsJoinEverySystemTurn proves the two-turn system
// prompt survives with its text in encounter order rather than being dropped
// wholesale.
func TestResponsesInstructionsJoinEverySystemTurn(t *testing.T) {
	request := responsesSystemRequest(
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "A"}}},
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "B"}}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
	)
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureMultipleSystemTurns: {}}}
	instructions, report := renderResponsesInstructions(t, request, policy)
	if instructions == nil {
		t.Fatal("instructions = nil, want both system turns' text joined")
	}
	if *instructions != "A\nB" {
		t.Fatalf("instructions = %q, want A\\nB", *instructions)
	}
	if !reportHasAt(report, FeatureMultipleSystemTurns, "instructions") {
		t.Fatalf("report = %+v, want the multi-turn decision", report.Losses)
	}

	// Strict policy still refuses: the joined encoding is a policy decision.
	context := testExchangeContext()
	if _, _, err := RenderResponsesRequest(request, context); err == nil {
		t.Fatal("strict render accepted multiple system turns")
	}
}

// TestResponsesInstructionsNonTextInLaterSystemTurn proves a non-text part of a
// system turn AFTER the first no longer escapes the system_non_text_content
// decision, and the first turn's text still renders.
func TestResponsesInstructionsNonTextInLaterSystemTurn(t *testing.T) {
	request := responsesSystemRequest(
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "keep"}}},
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{
			CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"},
			CanonicalText{Text: "visible"},
		}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
	)
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMultipleSystemTurns:  {},
		FeatureSystemNonTextContent: {},
	}}
	instructions, report := renderResponsesInstructions(t, request, policy)
	if instructions == nil || *instructions != "keep\nvisible" {
		t.Fatalf("instructions = %v, want the text of every system turn", instructions)
	}
	if !reportHasFeature(report, FeatureSystemNonTextContent) {
		t.Fatalf("report = %+v, want the later turn's non-text decision", report.Losses)
	}

	// Without the non-text approval the same request refuses on that key.
	denied := LossPolicy{Allowed: map[Feature]struct{}{FeatureMultipleSystemTurns: {}}}
	context := testExchangeContext()
	context.LossPolicy = denied
	if _, _, err := RenderResponsesRequest(request, context); err == nil {
		t.Fatal("non-text system content accepted without its decision")
	}
}

// TestResponsesInstructionsAllNonTextSystemTurnsAgreeWithSingleTurn proves a
// multi-turn system prompt whose parts were all dropped behaves exactly like
// the single-turn case: no instructions field, no invented empty string.
func TestResponsesInstructionsAllNonTextSystemTurnsAgreeWithSingleTurn(t *testing.T) {
	image := CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"}
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMultipleSystemTurns:  {},
		FeatureSystemNonTextContent: {},
	}}
	multi := responsesSystemRequest(
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{image}},
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{image}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
	)
	single := responsesSystemRequest(
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{image}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
	)
	multiInstructions, _ := renderResponsesInstructions(t, multi, policy)
	singleInstructions, _ := renderResponsesInstructions(t, single, policy)
	if multiInstructions != nil || singleInstructions != nil {
		t.Fatalf("instructions = (%v, %v), want nil for an all-non-text system prompt",
			multiInstructions, singleInstructions)
	}
}

// TestResponsesRelocatedSystemTurnIsReported proves a mid-conversation system
// turn records its position loss instead of moving silently.
func TestResponsesRelocatedSystemTurnIsReported(t *testing.T) {
	request := responsesSystemRequest(
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "mid"}}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "again"}}},
	)
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureMidConversationSystem: {}}}
	instructions, report := renderResponsesInstructions(t, request, policy)
	if instructions == nil || *instructions != "mid" {
		t.Fatalf("instructions = %v, want the relocated system text", instructions)
	}
	if !reportHasAt(report, FeatureMidConversationSystem, "turns[1]") {
		t.Fatalf("report = %+v, want a mid_conversation_system loss at the turn's path", report.Losses)
	}

	// Strict policy refuses the client-authored relocation.
	context := testExchangeContext()
	if _, _, err := RenderResponsesRequest(request, context); err == nil {
		t.Fatal("strict render accepted a mid-conversation system turn")
	}
}

// TestResponsesDeveloperTurnIsReported proves the developer-role distinction is
// recorded rather than silently collapsing into the role-neutral instructions
// string.
func TestResponsesDeveloperTurnIsReported(t *testing.T) {
	request := responsesSystemRequest(
		CanonicalTurn{Role: CanonicalDeveloper, Parts: []CanonicalPart{CanonicalText{Text: "dev"}}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
	)
	policy := LossPolicy{Allowed: map[Feature]struct{}{FeatureDeveloperRole: {}}}
	instructions, report := renderResponsesInstructions(t, request, policy)
	if instructions == nil || *instructions != "dev" {
		t.Fatalf("instructions = %v, want the developer text", instructions)
	}
	if !reportHasAt(report, FeatureDeveloperRole, "turns[0]") {
		t.Fatalf("report = %+v, want a developer_role loss", report.Losses)
	}

	context := testExchangeContext()
	if _, _, err := RenderResponsesRequest(request, context); err == nil {
		t.Fatal("strict render accepted a developer turn without its decision")
	}
}

// TestResponsesClientSystemTurnAfterRetainedDialogStaysLeading proves a
// CLIENT-authored system turn that physically follows RETAINED dialog is not
// charged as a mid-conversation move: the proxy created that position, and the
// client's instructions must stay leading.
func TestResponsesClientSystemTurnAfterRetainedDialogStaysLeading(t *testing.T) {
	request := responsesSystemRequest(
		// Retained prefix: a dialog turn.
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "retained question"}}},
		CanonicalTurn{Role: CanonicalAssistant, Parts: []CanonicalPart{CanonicalText{Text: "retained answer"}}},
		// Client turns: its own instructions, then a user turn.
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "be brief"}}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "next"}}},
	)
	request.RetainedTurns = 2

	// Strict policy: nothing is approved, and the client's own leading
	// instructions must not be charged.
	instructions, report := renderResponsesInstructions(t, request, StrictLossPolicy())
	if instructions == nil || *instructions != "be brief" {
		t.Fatalf("instructions = %v, want the client's own system text", instructions)
	}
	if reportHasFeature(report, FeatureMidConversationSystem) {
		t.Fatalf("the client's own leading instructions were charged: %+v", report.Losses)
	}
}

// TestResponsesMixedRetainedAndClientSystemTurnsChargeOnlyTheClient proves the
// multi-turn decision counts CLIENT-authored system turns: one retained system
// turn plus one client system turn is the proxy's surplus (a Note), while two
// client system turns are the client's (a Loss). Positions alone would get this
// wrong, because the retained turn is always the earlier one.
func TestResponsesMixedRetainedAndClientSystemTurnsChargeOnlyTheClient(t *testing.T) {
	// One retained system turn, one client system turn.
	mixed := responsesSystemRequest(
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "retained"}}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hello"}}},
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "client"}}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "next"}}},
	)
	mixed.RetainedTurns = 2
	instructions, report := renderResponsesInstructions(t, mixed, StrictLossPolicy())
	if instructions == nil || *instructions != "retained\nclient" {
		t.Fatalf("instructions = %v, want both system turns joined", instructions)
	}
	for _, loss := range report.Losses {
		if loss.Feature == FeatureMultipleSystemTurns && loss.Kind == LossRecord {
			t.Fatalf("the proxy's surplus was charged to the client: %+v", loss)
		}
	}

	// Two client system turns: the surplus is the client's, so strict refuses.
	client := responsesSystemRequest(
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "one"}}},
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "two"}}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
	)
	context := testExchangeContext()
	if _, _, err := RenderResponsesRequest(client, context); err == nil {
		t.Fatal("two client system turns were accepted under strict policy")
	}
}

// TestResponsesRetainedSystemTurnIsNotChargedToTheClient proves the retained
// prefix's own positions are Notes, never policy-gated losses: without this a
// continuity-enabled Responses route would reject a cleanly authored request on
// the default-deny multiple_system_turns key.
func TestResponsesRetainedSystemTurnIsNotChargedToTheClient(t *testing.T) {
	request := responsesSystemRequest(
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "retained"}}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hello"}}},
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "retained mid"}}},
		CanonicalTurn{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "next"}}},
	)
	request.RetainedTurns = len(request.Turns)

	instructions, report := renderResponsesInstructions(t, request, StrictLossPolicy())
	if instructions == nil {
		t.Fatal("instructions = nil, want the retained system text")
	}
	// A retained position is a sanctioned encoding, not a policy decision:
	// every entry under either key must be a Note, never a Loss that could
	// refuse the request.
	mid := false
	for _, loss := range report.Losses {
		if loss.Feature != FeatureMidConversationSystem && loss.Feature != FeatureMultipleSystemTurns {
			continue
		}
		if loss.Kind == LossRecord {
			t.Fatalf("retained history charged as a policy-gated loss: %+v", loss)
		}
		if loss.Feature == FeatureMidConversationSystem {
			mid = true
		}
	}
	if !mid {
		t.Fatalf("report = %+v, want a mid_conversation_system Note for the retained position", report.Losses)
	}
}
