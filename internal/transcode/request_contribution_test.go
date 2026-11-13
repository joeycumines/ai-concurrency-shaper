package transcode

// A shared decision helper only helps when its caller knows the FACT the
// decision depends on. Position, role and join decisions describe where a turn
// ended up, so a turn that rendered nothing must pay for nothing; and Chat's
// placement channel (where a message goes) must not be confused with its
// provenance (whether the client authored it), or a developer message the
// developer_role capability lets us keep is mistaken for dialog.

import (
	"strings"
	"testing"
)

// thinkingOnlyTurn is a turn whose only part is marker-signature thinking.
func thinkingOnlyTurn(role CanonicalRole) CanonicalTurn {
	return CanonicalTurn{Role: role, Parts: []CanonicalPart{
		CanonicalThinkingPart{Signature: SyntheticThinkingSignature},
	}}
}

// chatMessages decodes the rendered Chat messages.
func chatMessages(t *testing.T, rendered []byte) []ChatMessage {
	t.Helper()
	var out ChatRequest
	if err := strictDecode(rendered, &out); err != nil {
		t.Fatalf("rendered chat: %v\n%s", err, rendered)
	}
	return out.Messages
}

// messageRoleAndText returns a message's role and its first text block.
func messageRoleAndText(message ChatMessage) (ChatMessageRole, string) {
	if message.Content == nil || len(message.Content.ContentBlocks) == 0 {
		return message.Role, ""
	}
	text := ""
	if block := message.Content.ContentBlocks[0]; block.Text != nil {
		text = *block.Text
	}
	return message.Role, text
}

// TestChatSystemTurnOfScrubbedThinkingRendersNoSystemMessage proves the Chat
// target agrees with the other three: marker-signature thinking is scrubbed in
// every turn role, so a system turn holding only that renders nothing rather
// than failing with "empty system or developer turn".
func TestChatSystemTurnOfScrubbedThinkingRendersNoSystemMessage(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			thinkingOnlyTurn(CanonicalSystem),
			thinkingOnlyTurn(CanonicalDeveloper),
			textTurn(CanonicalUser, "hi"),
		},
	}
	for _, capabilities := range []ChatCapabilities{{}, {DeveloperRole: true}} {
		rendered, report, err := RenderChatRequest(request, testExchangeContext(), capabilities)
		if err != nil {
			t.Fatalf("capabilities %+v: a scrubbed system turn failed the render: %v", capabilities, err)
		}
		messages := chatMessages(t, rendered)
		if len(messages) != 1 {
			t.Fatalf("capabilities %+v: messages = %d, want only the user turn: %s",
				capabilities, len(messages), rendered)
		}
		if role, _ := messageRoleAndText(messages[0]); role != ChatMessageRoleUser {
			t.Fatalf("capabilities %+v: first message role = %s, want user", capabilities, role)
		}
		for _, loss := range report.Losses {
			if loss.Kind == LossRecord {
				t.Fatalf("capabilities %+v: scrubbing marker thinking recorded a loss: %+v",
					capabilities, loss)
			}
		}
	}
}

// TestChatDeveloperTurnIsNotDialog proves Chat's placement channel is not its
// provenance channel. With developer_role on, a developer turn keeps the
// developer role and opens no dialog: it is an INSTRUCTION, so a following
// system turn is not charged as a mid-DIALOG system turn.
//
// It is nevertheless charged a position, and that is honest rather than
// incidental: the consolidated system prompt is emitted at index 0 because
// open-weights chat templates reject a system message after index 0, so a system
// turn that had a developer message before it really does move ahead of it. The
// key records the move; the developer_role distinction is preserved.
func TestChatDeveloperTurnIsNotDialog(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			textTurn(CanonicalDeveloper, "dev"),
			textTurn(CanonicalSystem, "sys"),
			textTurn(CanonicalUser, "hi"),
		},
	}
	// The position the consolidation creates is charged, so a strict render
	// refuses it; approve it to inspect the placement.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureMidConversationSystem: {}}}
	rendered, report, err := RenderChatRequest(request, context,
		ChatCapabilities{DeveloperRole: true})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The developer ROLE is preserved, so no developer_role decision.
	if reportHasFeature(report, FeatureDeveloperRole) {
		t.Fatalf("a kept developer turn recorded developer_role: %+v", report.Losses)
	}
	if !reportHasAt(report, FeatureMidConversationSystem, "turns[1]") {
		t.Fatalf("report = %+v, want the system turn's move charged at turns[1]", report.Losses)
	}
	messages := chatMessages(t, rendered)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want system prompt, developer, user: %s", len(messages), rendered)
	}
	want := []struct {
		role ChatMessageRole
		text string
	}{
		{ChatMessageRoleSystem, "sys"},
		{ChatMessageRoleDeveloper, "dev"},
		{ChatMessageRoleUser, "hi"},
	}
	for i, entry := range want {
		role, text := messageRoleAndText(messages[i])
		if role != entry.role || text != entry.text {
			t.Fatalf("messages[%d] = (%s, %q), want (%s, %q)", i, role, text, entry.role, entry.text)
		}
	}

	// The scope guard: a system turn after real DIALOG is charged under the same
	// key, so the key does not depend on the developer message existing.
	midDialog := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			textTurn(CanonicalUser, "hi"),
			textTurn(CanonicalSystem, "late"),
		},
	}
	_, _, err = RenderChatRequest(midDialog, testExchangeContext(),
		ChatCapabilities{DeveloperRole: true})
	if err == nil {
		t.Fatal("a client-authored mid-dialog system turn was accepted under strict policy")
	}
	if !strings.Contains(err.Error(), string(FeatureMidConversationSystem)) {
		t.Fatalf("err = %q, want it to name %s", err, FeatureMidConversationSystem)
	}
}

// TestChatLeadingDeveloperKeepsEncounterOrder proves the consolidated system
// message is not pasted at index 0 ahead of a leading developer message: the
// client's turn order is what the model reads.
func TestChatLeadingDeveloperKeepsEncounterOrder(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			textTurn(CanonicalDeveloper, "dev"),
			textTurn(CanonicalSystem, "sys"),
			textTurn(CanonicalUser, "hi"),
		},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMultipleSystemTurns: {},
		FeatureDeveloperRole:       {},
	}}
	// With the developer-role capability OFF the developer turn joins the
	// system channel, so the pair consolidates; the developer text must still
	// precede the system text in the joined prompt.
	rendered, _, err := RenderChatRequest(request, context, ChatCapabilities{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	messages := chatMessages(t, rendered)
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want the consolidated system prompt and the user turn: %s",
			len(messages), rendered)
	}
	role, text := messageRoleAndText(messages[0])
	if role != ChatMessageRoleSystem {
		t.Fatalf("messages[0] role = %s, want system", role)
	}
	if !strings.Contains(text, "dev") || !strings.Contains(text, "sys") {
		t.Fatalf("consolidated system text = %q, want both turns", text)
	}
	if strings.Index(text, "dev") > strings.Index(text, "sys") {
		t.Fatalf("consolidated system text = %q, want the developer text first", text)
	}
}

// TestChatEmptyDialogTurnIsNotRefused proves a dialog turn the Messages decoder
// preserved with an empty text part still renders: the turn boundary is the
// point of that synthesis, and refusing it would delete a turn the model is
// expected to see.
func TestChatEmptyDialogTurnIsNotRefused(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			textTurn(CanonicalUser, "hi"),
			{Role: CanonicalAssistant, Parts: []CanonicalPart{CanonicalText{Text: ""}}},
		},
	}
	rendered, _, err := RenderChatRequest(request, testExchangeContext(), ChatCapabilities{})
	if err != nil {
		t.Fatalf("a preserved empty assistant turn was refused: %v", err)
	}
	messages := chatMessages(t, rendered)
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want the user and the assistant turn: %s", len(messages), rendered)
	}
}

// TestAllDroppedDeveloperTurnIsNotCharged proves the contribution rule on all
// four targets with the developer-role capability ON: a developer turn whose
// only part was dropped renders nothing, so it must not record a
// developer_role decision and must not emit an empty developer message.
func TestAllDroppedDeveloperTurnIsNotCharged(t *testing.T) {
	image := CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"}
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent: {},
	}}
	request := positionRequest(0,
		CanonicalTurn{Role: CanonicalDeveloper, Parts: []CanonicalPart{image}},
		textTurn(CanonicalUser, "hi"),
	)
	for name, render := range allRequestRenderersWithDeveloperRole() {
		t.Run(name, func(t *testing.T) {
			context := testExchangeContext()
			context.LossPolicy = policy
			rendered, report, err := render(request, context)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !reportHasFeature(report, FeatureSystemNonTextContent) {
				t.Fatalf("report = %+v, want the dropped-image decision", report.Losses)
			}
			if reportHasFeature(report, FeatureDeveloperRole) {
				t.Fatalf("a developer turn that rendered nothing was charged developer_role: %+v",
					report.Losses)
			}
			if strings.Contains(string(rendered), "i.png") {
				t.Fatalf("a dropped developer image reached the wire: %s", rendered)
			}
			if strings.Contains(string(rendered), `"role":"developer"`) {
				t.Fatalf("an empty developer message was invented: %s", rendered)
			}
		})
	}
}

// TestAllDroppedSystemTurnIsNotChargedAMidConversationPosition proves the same
// contribution rule for the position decision: a mid-dialog system turn whose
// only part was dropped occupies no position, so it is charged nothing beyond
// the drop itself.
func TestAllDroppedSystemTurnIsNotChargedAMidConversationPosition(t *testing.T) {
	image := CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"}
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent: {},
		FeatureMultipleSystemTurns:  {},
	}}
	request := positionRequest(0,
		textTurn(CanonicalUser, "hi"),
		CanonicalTurn{Role: CanonicalSystem, Parts: []CanonicalPart{image}},
		textTurn(CanonicalUser, "again"),
	)
	for name, render := range allRequestRenderers() {
		t.Run(name, func(t *testing.T) {
			context := testExchangeContext()
			context.LossPolicy = policy
			_, report, err := render(request, context)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !reportHasFeature(report, FeatureSystemNonTextContent) {
				t.Fatalf("report = %+v, want the dropped-image decision", report.Losses)
			}
			if reportHasFeature(report, FeatureMidConversationSystem) {
				t.Fatalf("a system turn that rendered nothing was charged a position: %+v",
					report.Losses)
			}
		})
	}
}

// allRequestRenderersWithDeveloperRole is allRequestRenderers with Chat's
// developer-role capability GRANTED, which is the case where the placement and
// provenance channels can be confused.
func allRequestRenderersWithDeveloperRole() map[string]func(CanonicalRequest, *ExchangeContext) ([]byte, ConversionReport, error) {
	renderers := allRequestRenderers()
	renderers["chat-developer-role"] = func(request CanonicalRequest, context *ExchangeContext) ([]byte, ConversionReport, error) {
		return RenderChatRequest(request, context, ChatCapabilities{DeveloperRole: true})
	}
	delete(renderers, "chat")
	return renderers
}

// TestChatSystemTurnAfterAKeptDeveloperTurnIsCharged proves the position rule is
// truthful for the ONE shape a kept developer message can still displace: a
// consolidating system turn that follows it cannot stay where it was, because
// the join moves its text to the first contributing turn. A leading developer
// message is different — nothing precedes the system turn, so it keeps its
// position and is charged nothing.
func TestChatSystemTurnAfterAKeptDeveloperTurnIsCharged(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			textTurn(CanonicalSystem, "first"),
			textTurn(CanonicalDeveloper, "dev"),
			textTurn(CanonicalSystem, "second"),
			textTurn(CanonicalUser, "hi"),
		},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMultipleSystemTurns:   {},
		FeatureMidConversationSystem: {},
	}}
	rendered, report, err := RenderChatRequest(request, context,
		ChatCapabilities{DeveloperRole: true})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The two system turns contribute, so the shared join decision applies.
	if !reportHasFeature(report, FeatureMultipleSystemTurns) {
		t.Fatalf("report = %+v, want the join decision", report.Losses)
	}
	// The second system turn is displaced by the kept developer message, so its
	// position is charged; the first is not.
	var charged []string
	for _, loss := range report.Losses {
		if loss.Feature == FeatureMidConversationSystem {
			charged = append(charged, loss.Path)
		}
	}
	if len(charged) != 1 || charged[0] != "turns[2]" {
		t.Fatalf("mid_conversation_system entries = %v, want exactly turns[2]", charged)
	}
	// The rendered order is consolidated prompt, developer, user.
	messages := chatMessages(t, rendered)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want consolidated, developer, user: %s", len(messages), rendered)
	}
	for i, want := range []ChatMessageRole{ChatMessageRoleSystem, ChatMessageRoleDeveloper, ChatMessageRoleUser} {
		if role, _ := messageRoleAndText(messages[i]); role != want {
			t.Fatalf("messages[%d] role = %s, want %s", i, role, want)
		}
	}
}

// TestChatConsolidatedSystemPromptIsLeading proves the loss text is TRUE: the
// consolidated system prompt is emitted at index 0, because every open-weights
// chat template rejects a system message after index 0 and that is the whole
// reason the consolidation exists. A contributing system turn that had a dialog
// turn before it therefore really does move, which is exactly what
// mid_conversation_system records.
func TestChatConsolidatedSystemPromptIsLeading(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			textTurn(CanonicalUser, "u"),
			textTurn(CanonicalSystem, "s"),
			textTurn(CanonicalAssistant, "a"),
		},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureMidConversationSystem: {}}}
	rendered, report, err := RenderChatRequest(request, context, ChatCapabilities{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasAt(report, FeatureMidConversationSystem, "turns[1]") {
		t.Fatalf("report = %+v, want the position charged at turns[1]", report.Losses)
	}
	messages := chatMessages(t, rendered)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3: %s", len(messages), rendered)
	}
	if role, text := messageRoleAndText(messages[0]); role != ChatMessageRoleSystem || text != "s" {
		t.Fatalf("messages[0] = (%s, %q), want the consolidated system prompt first: %s",
			role, text, rendered)
	}
	for i := 1; i < len(messages); i++ {
		if role, _ := messageRoleAndText(messages[i]); role == ChatMessageRoleSystem {
			t.Fatalf("messages[%d] is a system message after index 0: %s", i, rendered)
		}
	}
}

// TestChatLeadingConsolidatedPromptStaysLeading is the scope guard: the common
// leading case is unchanged and records no position.
func TestChatLeadingConsolidatedPromptStaysLeading(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			textTurn(CanonicalSystem, "s"),
			textTurn(CanonicalUser, "u"),
		},
	}
	rendered, report, err := RenderChatRequest(request, testExchangeContext(), ChatCapabilities{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if reportHasFeature(report, FeatureMidConversationSystem) {
		t.Fatalf("a leading system turn was charged a position: %+v", report.Losses)
	}
	messages := chatMessages(t, rendered)
	if role, text := messageRoleAndText(messages[0]); role != ChatMessageRoleSystem || text != "s" {
		t.Fatalf("messages[0] = (%s, %q), want the system prompt", role, text)
	}
}
