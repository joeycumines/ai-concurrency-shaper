package transcode

import (
	"fmt"
	"testing"
)

// Probe: a developer turn whose ONLY part is dropped renders no message at all.
// A following system turn then has nothing before it, so its text cannot have
// moved. Is it still charged mid_conversation_system?
func TestProbeKeptBeforeFromDroppedDeveloper(t *testing.T) {
	image := CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"}
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalDeveloper, Parts: []CanonicalPart{image}},
			textTurn(CanonicalSystem, "s"),
			textTurn(CanonicalUser, "u"),
		},
	}
	for _, dev := range []bool{false, true} {
		context := testExchangeContext()
		context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
			FeatureSystemNonTextContent: {},
			FeatureMidConversationSystem: {},
			FeatureDeveloperRole:        {},
		}}
		rendered, report, err := RenderChatRequest(request, context, ChatCapabilities{DeveloperRole: dev})
		if err != nil {
			t.Fatalf("dev=%v render: %v", dev, err)
		}
		fmt.Printf("dev=%v BODY: %s\n", dev, rendered)
		for _, loss := range report.Losses {
			fmt.Printf("  dev=%v ENTRY kind=%v feature=%s path=%s\n", dev, loss.Kind, loss.Feature, loss.Path)
		}
	}
}

// Probe: same shape under a STRICT policy (nothing approved) - does the request
// get refused for a move that never happens?
func TestProbeKeptBeforeFromDroppedDeveloperStrict(t *testing.T) {
	image := CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"}
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalDeveloper, Parts: []CanonicalPart{image}},
			textTurn(CanonicalSystem, "s"),
			textTurn(CanonicalUser, "u"),
		},
	}
	context := testExchangeContext()
	// Approve only the drop that empties the developer turn, so the render can
	// only fail on a position charge.
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent: {},
	}}
	rendered, report, err := RenderChatRequest(request, context, ChatCapabilities{DeveloperRole: true})
	fmt.Printf("STRICT err=%v\nBODY: %s\n", err, rendered)
	for _, loss := range report.Losses {
		fmt.Printf("  ENTRY kind=%v feature=%s path=%s\n", loss.Kind, loss.Feature, loss.Path)
	}
}

// Probe: a system turn whose ONLY part is dropped, followed by another system
// turn - does keptBefore fire from the dropped system turn?
func TestProbeDroppedSystemThenSystem(t *testing.T) {
	image := CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"}
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			textTurn(CanonicalUser, "u"),
			{Role: CanonicalSystem, Parts: []CanonicalPart{image}},
			textTurn(CanonicalSystem, "s"),
			textTurn(CanonicalUser, "u2"),
		},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureSystemNonTextContent: {},
		FeatureMidConversationSystem: {},
		FeatureMultipleSystemTurns:  {},
	}}
	rendered, report, err := RenderChatRequest(request, context, ChatCapabilities{})
	fmt.Printf("BODY: %s\nERR: %v\n", rendered, err)
	for _, loss := range report.Losses {
		fmt.Printf("  ENTRY kind=%v feature=%s path=%s\n", loss.Kind, loss.Feature, loss.Path)
	}
}
