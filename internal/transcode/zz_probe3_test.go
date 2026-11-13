package transcode

import (
	"fmt"
	"testing"
)

// End-to-end probe: a Responses client sends a developer input item whose text
// is empty, followed by a system item, then a user turn. With the developer_role
// capability on (the case that keeps the developer message in place), the
// developer message renders EMPTY and is filtered out — so the consolidated
// system prompt has nothing before it. Is mid_conversation_system still charged?
func TestProbeResponsesEmptyDeveloperThenSystem(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"input": [
			{"type": "message", "role": "developer", "content": [{"type": "input_text", "text": ""}]},
			{"type": "message", "role": "system", "content": [{"type": "input_text", "text": "be terse"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]}
		]
	}`)
	result, _, err := DecodeResponsesRequest(body, StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i, turn := range result.Request.Turns {
		fmt.Printf("TURN %d role=%s parts=%d %#v\n", i, turn.Role, len(turn.Parts), turn.Parts)
	}
	// Default-like policy: mid_conversation_system approved (it is in
	// defaultTranscodeLosses), nothing else needed.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureMidConversationSystem: {},
	}}
	rendered, report, err := RenderChatRequest(result.Request, context, ChatCapabilities{DeveloperRole: true})
	fmt.Printf("BODY: %s\nERR: %v\n", rendered, err)
	for _, loss := range report.Losses {
		fmt.Printf("  ENTRY kind=%v feature=%s path=%s\n", loss.Kind, loss.Feature, loss.Path)
	}

	// Strict: the same request must not be refused for a move that never happens.
	strict := testExchangeContext()
	_, _, err = RenderChatRequest(result.Request, strict, ChatCapabilities{DeveloperRole: true})
	fmt.Printf("STRICT ERR: %v\n", err)
}
