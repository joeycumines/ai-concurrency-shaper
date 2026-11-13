package transcode

// A loss entry's path is the operator's only way to find the content the entry
// is about. Two conventions must therefore never be mixed inside one stage: a
// CANONICAL index (turns[N], as Chat, Responses and Gemini all report) and a
// WIRE index (messages[N], which is what the client actually sent).
//
// Decode-side paths are wire-relative and stay so — they name the client's own
// array. Render-side paths are canonical, because the rendered array is not the
// client's: system turns are lifted into the system field, so a wire index would
// point at a message the entry does not describe.

import (
	"testing"
)

// TestMessagesRenderPathsNameTheStageTheyDescribe proves a render-side entry
// never points at a wire position it did not render into.
func TestMessagesRenderPathsNameTheStageTheyDescribe(t *testing.T) {
	image := CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"}
	request := CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns: []CanonicalTurn{
			// turn 0: system, contributes a non-text part decision
			{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "keep"}, image}},
			// turn 1: user, renders at messages[0]
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
			// turn 2: system, mid-dialog, renders into the system field
			{Role: CanonicalSystem, Parts: []CanonicalPart{CanonicalText{Text: "mid"}}},
			// turn 3: user, renders at messages[1]
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "again"}}},
		},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent:  {},
		FeatureMidConversationSystem: {},
		FeatureMultipleSystemTurns:   {},
	}}
	_, report, err := RenderMessagesRequest(request, context)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, loss := range report.Losses {
		if hasWireIndexPrefix(loss.Path) {
			t.Fatalf("render-side entry %s at path %q; render paths name the canonical turn or the "+
				"system field, never a wire index that disagrees with them",
				loss.Feature, loss.Path)
		}
	}

	if !reportHasAt(report, FeatureSystemNonTextContent, "turns[0].parts[1]") {
		t.Fatalf("report = %+v, want the system part decision at turns[0].parts[1]", report.Losses)
	}
	if !reportHasAt(report, FeatureMidConversationSystem, "turns[2]") {
		t.Fatalf("report = %+v, want the mid-conversation decision at turns[2]", report.Losses)
	}
}

// hasWireIndexPrefix reports whether path begins with a messages[ index.
func hasWireIndexPrefix(path string) bool {
	const prefix = "messages["
	if len(path) < len(prefix)+2 || path[:len(prefix)] != prefix {
		return false
	}
	// A digit followed by ']' is a wire index; anything else (a bare
	// messages[] template) is not.
	return path[len(prefix)] >= '0' && path[len(prefix)] <= '9'
}
