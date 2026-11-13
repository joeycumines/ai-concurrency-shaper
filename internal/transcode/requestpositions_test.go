package transcode

// The retained-turn exemption documented on CanonicalRequest.RetainedTurns and
// the several-system-turns join are renderer-wide rules, not Chat-only or
// Responses-only ones. A shared helper only helps if every renderer calls it,
// so these tests drive all four request renderers through the same shapes: a
// position the continuity store reconstructed must never be charged to the
// client, a position the client created must always be, and the join of several
// system turns is ONE decision under ONE key whichever dialect renders it.

import (
	"strings"
	"testing"
)

// positionRequest builds a request from the given turns with a retained prefix.
// It delegates to retainedMessagesRequest so the package has ONE spelling of
// this fixture.
func positionRequest(retained int, turns ...CanonicalTurn) CanonicalRequest {
	return retainedMessagesRequest(turns, retained)
}

// textTurn is a one-text-part turn.
func textTurn(role CanonicalRole, text string) CanonicalTurn {
	return CanonicalTurn{Role: role, Parts: []CanonicalPart{CanonicalText{Text: text}}}
}

// allRequestRenderers maps every target that consumes the canonical IR to its
// request render entry point. Chat's developer-role capability is off, which is
// the default and the interesting case: a developer turn then has to be folded
// into the system channel.
func allRequestRenderers() map[string]func(CanonicalRequest, *ExchangeContext) ([]byte, ConversionReport, error) {
	return map[string]func(CanonicalRequest, *ExchangeContext) ([]byte, ConversionReport, error){
		"chat": func(request CanonicalRequest, context *ExchangeContext) ([]byte, ConversionReport, error) {
			return RenderChatRequest(request, context, ChatCapabilities{})
		},
		"messages":  RenderMessagesRequest,
		"responses": RenderResponsesRequest,
		"gemini":    RenderGeminiRequest,
	}
}

// TestRetainedDeveloperTurnIsANoteOnEveryRenderer proves a developer turn the
// continuity store reconstructed never trips a client-facing role gate: the
// encoding is identical, only the policy decision differs, so it is a Note. A
// client-authored developer turn in the same shape is still the policy-gated
// loss and still refuses under strict policy.
func TestRetainedDeveloperTurnIsANoteOnEveryRenderer(t *testing.T) {
	for name, render := range allRequestRenderers() {
		t.Run(name, func(t *testing.T) {
			retained := positionRequest(1,
				textTurn(CanonicalDeveloper, "retained dev"),
				textTurn(CanonicalUser, "next"),
			)
			// Strict policy: nothing is approved, yet the retained role
			// collapse must not reject a request the client authored cleanly.
			_, report, err := render(retained, testExchangeContext())
			if err != nil {
				t.Fatalf("retained developer turn was charged to the client: %v", err)
			}
			for _, loss := range report.Losses {
				if loss.Feature == FeatureDeveloperRole && loss.Kind != NoteRecord {
					t.Fatalf("retained developer turn recorded a %v: %+v", loss.Kind, loss)
				}
			}
			if !reportHasFeature(report, FeatureDeveloperRole) {
				t.Fatalf("report = %+v, want the developer_role Note", report.Losses)
			}

			// The same shape authored by the client is the gated loss.
			client := positionRequest(0,
				textTurn(CanonicalDeveloper, "client dev"),
				textTurn(CanonicalUser, "next"),
			)
			if _, _, err := render(client, testExchangeContext()); err == nil {
				t.Fatal("strict render accepted a client-authored developer turn")
			}
			context := testExchangeContext()
			context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureDeveloperRole: {}}}
			_, report, err = render(client, context)
			if err != nil {
				t.Fatalf("approved render: %v", err)
			}
			found := false
			for _, loss := range report.Losses {
				if loss.Feature == FeatureDeveloperRole && loss.Kind == LossRecord {
					found = true
				}
			}
			if !found {
				t.Fatalf("report = %+v, want the policy-gated developer_role loss", report.Losses)
			}
		})
	}
}

// TestRetainedMidDialogSystemTurnIsANoteOnEveryRenderer proves a system turn the
// continuity store placed after dialog is a Note on every target, so a
// continuity-enabled route cannot refuse a cleanly authored follow-up.
func TestRetainedMidDialogSystemTurnIsANoteOnEveryRenderer(t *testing.T) {
	for name, render := range allRequestRenderers() {
		t.Run(name, func(t *testing.T) {
			request := positionRequest(3,
				textTurn(CanonicalUser, "retained q"),
				textTurn(CanonicalAssistant, "retained a"),
				textTurn(CanonicalSystem, "retained mid"),
				textTurn(CanonicalUser, "next"),
			)
			_, report, err := render(request, testExchangeContext())
			if err != nil {
				t.Fatalf("a retained mid-dialog system turn must not be charged: %v", err)
			}
			if !reportHasFeature(report, FeatureMidConversationSystem) {
				t.Fatalf("report = %+v, want the mid_conversation_system Note", report.Losses)
			}
			for _, loss := range report.Losses {
				if loss.Feature == FeatureMidConversationSystem && loss.Kind != NoteRecord {
					t.Fatalf("retained position recorded as a %v: %+v", loss.Kind, loss)
				}
			}
		})
	}
}

// TestClientSystemTurnAfterRetainedDialogStaysLeadingOnEveryRenderer proves the
// mirror rule: a client's own system turn is leading even when retained dialog
// physically precedes it, because the proxy created that position.
func TestClientSystemTurnAfterRetainedDialogStaysLeadingOnEveryRenderer(t *testing.T) {
	for name, render := range allRequestRenderers() {
		t.Run(name, func(t *testing.T) {
			request := positionRequest(2,
				textTurn(CanonicalUser, "retained q"),
				textTurn(CanonicalAssistant, "retained a"),
				textTurn(CanonicalSystem, "client instructions"),
				textTurn(CanonicalUser, "next"),
			)
			_, report, err := render(request, testExchangeContext())
			if err != nil {
				t.Fatalf("the client's own leading system turn was charged: %v", err)
			}
			if reportHasFeature(report, FeatureMidConversationSystem) {
				t.Fatalf("the client's own leading instructions were charged: %+v", report.Losses)
			}
		})
	}
}

// TestMixedProvenanceMidDialogTurnsAreChargedPerTurn proves one aggregate
// decision cannot hide which turn paid: a retained and a client-authored
// mid-dialog system turn are two decisions at their own turn paths, exactly one
// of them a policy-gated loss.
func TestMixedProvenanceMidDialogTurnsAreChargedPerTurn(t *testing.T) {
	for name, render := range allRequestRenderers() {
		t.Run(name, func(t *testing.T) {
			request := positionRequest(2,
				textTurn(CanonicalUser, "retained q"),
				textTurn(CanonicalSystem, "retained mid"),
				textTurn(CanonicalUser, "client q"),
				textTurn(CanonicalSystem, "client mid"),
			)
			context := testExchangeContext()
			context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureMidConversationSystem: {}}}
			_, report, err := render(request, context)
			if err != nil {
				t.Fatalf("approved render: %v", err)
			}
			var losses, notes int
			paths := map[string]bool{}
			for _, loss := range report.Losses {
				if loss.Feature != FeatureMidConversationSystem {
					continue
				}
				paths[loss.Path] = true
				switch loss.Kind {
				case LossRecord:
					losses++
				case NoteRecord:
					notes++
				}
			}
			if losses != 1 || notes != 1 {
				t.Fatalf("mid_conversation_system: %d losses, %d notes, want exactly 1 of each: %+v",
					losses, notes, report.Losses)
			}
			if len(paths) != 2 {
				t.Fatalf("decisions at %v, want one path per turn: %+v", paths, report.Losses)
			}
			for path := range paths {
				if !strings.HasPrefix(path, "turns[") {
					t.Fatalf("position decision at %q, want the canonical turn path", path)
				}
			}
		})
	}
}

// TestSystemTurnJoinIsOneDecisionOnEveryRenderer proves the same encoding — the
// boundaries of several system turns lost in one system field — is one key with
// one severity whichever dialect renders it. That is what lets a single policy
// control the encoding everywhere, and what stops an operator denying it on one
// target while the same source renders on another.
func TestSystemTurnJoinIsOneDecisionOnEveryRenderer(t *testing.T) {
	for name, render := range allRequestRenderers() {
		t.Run(name, func(t *testing.T) {
			// Two CLIENT-authored leading system turns: the client's own
			// surplus, so a policy-gated loss.
			client := positionRequest(0,
				textTurn(CanonicalSystem, "A"),
				textTurn(CanonicalSystem, "B"),
				textTurn(CanonicalUser, "hi"),
			)
			if _, _, err := render(client, testExchangeContext()); err == nil {
				t.Fatal("strict render accepted the client's two-turn system prompt")
			}
			approved := testExchangeContext()
			approved.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureMultipleSystemTurns: {}}}
			_, report, err := render(client, approved)
			if err != nil {
				t.Fatalf("approved render: %v", err)
			}
			if !reportHasFeature(report, FeatureMultipleSystemTurns) {
				t.Fatalf("report = %+v, want the shared multiple_system_turns decision", report.Losses)
			}
			if reportHasFeature(report, FeatureMidConversationSystem) {
				t.Fatalf("the leading join was filed under mid_conversation_system: %+v", report.Losses)
			}

			// A retained surplus is the proxy's own: a Note, never a refusal.
			retained := positionRequest(1,
				textTurn(CanonicalSystem, "retained"),
				textTurn(CanonicalSystem, "client"),
				textTurn(CanonicalUser, "hi"),
			)
			_, report, err = render(retained, testExchangeContext())
			if err != nil {
				t.Fatalf("a retained system surplus must not be charged: %v", err)
			}
			found := false
			for _, loss := range report.Losses {
				if loss.Feature != FeatureMultipleSystemTurns {
					continue
				}
				found = true
				if loss.Kind != NoteRecord {
					t.Fatalf("retained surplus recorded as a %v: %+v", loss.Kind, loss)
				}
			}
			if !found {
				t.Fatalf("report = %+v, want the retained-surplus Note", report.Losses)
			}
		})
	}
}

// TestSystemTurnJoinIgnoresTurnsThatContributedNothing proves the join decision
// describes the merge that actually happened: a system turn whose parts were all
// dropped contributes no content, so two such turns are not a merge of two
// prompts and must not produce a join decision.
func TestSystemTurnJoinIgnoresTurnsThatContributedNothing(t *testing.T) {
	image := CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"}
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent: {},
		FeatureMultipleSystemTurns:  {},
	}}
	for name, render := range allRequestRenderers() {
		t.Run(name, func(t *testing.T) {
			// One text system turn plus two all-dropped ones: exactly one turn
			// contributes, so there is nothing to join.
			request := positionRequest(0,
				CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{image}},
				textTurn(CanonicalSystem, "real"),
				CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{image}},
				textTurn(CanonicalUser, "hi"),
			)
			context := testExchangeContext()
			context.LossPolicy = policy
			rendered, report, err := render(request, context)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !reportHasFeature(report, FeatureSystemNonTextContent) {
				t.Fatalf("report = %+v, want the dropped non-text decisions", report.Losses)
			}
			if reportHasFeature(report, FeatureMultipleSystemTurns) {
				t.Fatalf("a join was reported for turns that contributed nothing: %+v", report.Losses)
			}
			if strings.Contains(string(rendered), "i.png") {
				t.Fatalf("a dropped system image reached the wire: %s", rendered)
			}
		})
	}
}

// TestAllDroppedSystemTurnsEmitNoSystemField proves no target invents empty
// system content. A system turn whose only part was dropped leaves nothing to
// send, and an empty prompt fragment is proxy-invented text the model would
// read as an instruction the client never wrote.
func TestAllDroppedSystemTurnsEmitNoSystemField(t *testing.T) {
	image := CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"}
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent: {},
		FeatureMultipleSystemTurns:  {},
	}}
	const wantNoBytes = "image/png"
	renderers := allRequestRenderers()
	// The chat target has a second placement mode that bypasses the
	// consolidation entirely, and it must hold the same rule: rendering
	// positionally must not mean emitting a contentless system message.
	renderers["chat-system-anywhere"] = func(request CanonicalRequest, context *ExchangeContext) ([]byte, ConversionReport, error) {
		return RenderChatRequest(request, context, ChatCapabilities{SystemAnywhere: true})
	}
	for name, render := range renderers {
		t.Run(name, func(t *testing.T) {
			request := positionRequest(0,
				CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{image}},
				CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{image}},
				textTurn(CanonicalUser, "hi"),
			)
			context := testExchangeContext()
			context.LossPolicy = policy
			rendered, report, err := render(request, context)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !reportHasFeature(report, FeatureSystemNonTextContent) {
				t.Fatalf("report = %+v, want the drop decisions", report.Losses)
			}
			body := string(rendered)
			for _, empty := range []string{`"system":""`, `"instructions":""`, `"text":""`, `"systemInstruction":{}`} {
				if strings.Contains(body, empty) {
					t.Fatalf("empty system content invented (%s): %s", empty, body)
				}
			}
			if strings.Contains(body, wantNoBytes) {
				t.Fatalf("a dropped system image reached the wire: %s", body)
			}
		})
	}
}
