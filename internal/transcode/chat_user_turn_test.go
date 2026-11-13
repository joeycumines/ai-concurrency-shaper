package transcode

// A user turn renders as a sequence of Chat messages: the content before each
// tool result, the tool result itself, and the content after. Two properties the
// report depends on:
//
//   - a segment that scrubbed to nothing must be SKIPPED, not refused: the turn
//     is not empty, it already produced a tool message;
//   - every decision must name the canonical part it belongs to, so an operator
//     reading turns[0].parts[2] finds the part that was actually dropped rather
//     than a renumbered index into an internal slice.

import (
	"strings"
	"testing"
)

// TestChatUserTurnThinkingPrefixBeforeToolResult proves a user turn whose
// leading segment scrubbed to nothing still renders its tool message.
func TestChatUserTurnThinkingPrefixBeforeToolResult(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{
				CanonicalThinkingPart{Signature: SyntheticThinkingSignature},
				CanonicalFunctionResult{CallID: "call_1", Parts: []CanonicalPart{
					CanonicalText{Text: "done"},
				}},
			}},
		},
	}
	rendered, _, err := RenderChatRequest(request, testExchangeContext(), ChatCapabilities{})
	if err != nil {
		t.Fatalf("a scrubbed prefix before a tool result failed the render: %v", err)
	}
	messages := chatMessages(t, rendered)
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want only the tool message: %s", len(messages), rendered)
	}
	if role, _ := messageRoleAndText(messages[0]); role != ChatMessageRoleTool {
		t.Fatalf("messages[0] role = %s, want tool", role)
	}
}

// TestChatUserTurnDroppedImagePrefixBeforeToolResult proves the same for a
// segment whose only part took an approved image drop.
func TestChatUserTurnDroppedImagePrefixBeforeToolResult(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{
				CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"},
				CanonicalFunctionResult{CallID: "call_1", Parts: []CanonicalPart{
					CanonicalText{Text: "done"},
				}},
			}},
		},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureImageInput: {}}}
	rendered, report, err := RenderChatRequest(request, context, ChatCapabilities{})
	if err != nil {
		t.Fatalf("an approved image drop before a tool result failed the render: %v", err)
	}
	messages := chatMessages(t, rendered)
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want only the tool message: %s", len(messages), rendered)
	}
	if !reportHasAt(report, FeatureImageInput, "turns[0].parts[0]") {
		t.Fatalf("report = %+v, want the image drop at turns[0].parts[0]", report.Losses)
	}
}

// TestChatUserTurnLatePartPathsAreCanonical proves a decision on a part that
// FOLLOWS a tool result names its real index in the turn, not its index in the
// internal slice that was reset around the result.
func TestChatUserTurnLatePartPathsAreCanonical(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{
				CanonicalImage{MediaType: "image/png", URL: "https://example.test/first.png"},
				CanonicalFunctionResult{CallID: "call_1", Parts: []CanonicalPart{
					CanonicalText{Text: "done"},
				}},
				CanonicalDocument{MediaType: "application/pdf", URL: "https://example.test/doc.pdf"},
			}},
		},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureImageInput:    {},
		FeatureDocumentInput: {},
	}}
	_, report, err := RenderChatRequest(request, context, ChatCapabilities{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasAt(report, FeatureImageInput, "turns[0].parts[0]") {
		t.Fatalf("report = %+v, want the image decision at turns[0].parts[0]", report.Losses)
	}
	if !reportHasAt(report, FeatureDocumentInput, "turns[0].parts[2]") {
		t.Fatalf("a part after a tool result was renumbered: %+v, want turns[0].parts[2]",
			report.Losses)
	}
}

// TestChatToolResultDecisionsNameTheCanonicalPath proves the tool-result
// renderer reports at the canonical part path rather than a hardcoded wire-ish
// messages[] fragment.
func TestChatToolResultDecisionsNameTheCanonicalPath(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{
				CanonicalFunctionResult{
					CallID:  "call_1",
					IsError: true,
					Parts:   []CanonicalPart{CanonicalText{Text: "boom"}},
				},
			}},
		},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureToolResultErrorStatus: {}}}
	_, report, err := RenderChatRequest(request, context, ChatCapabilities{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var paths []string
	for _, loss := range report.Losses {
		if loss.Feature == FeatureToolResultErrorStatus {
			paths = append(paths, loss.Path)
		}
	}
	if len(paths) != 1 {
		t.Fatalf("tool_result_error_status entries = %d, want 1: %+v", len(paths), report.Losses)
	}
	if !strings.HasPrefix(paths[0], "turns[0].parts[0]") {
		t.Fatalf("tool_result_error_status path = %q, want the canonical tool-result part", paths[0])
	}
}

// TestChatToolResultNestedFailureNamesTheNestedPath proves a failure inside a
// result part names the nested location, so the operator can find the bytes.
func TestChatToolResultNestedFailureNamesTheNestedPath(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{
				CanonicalFunctionResult{CallID: "call_1", Parts: []CanonicalPart{
					CanonicalImage{MediaType: "image/png", Base64: "AAAA"},
				}},
			}},
		},
	}
	caps := ChatCapabilities{ToolResultImages: true, ImageInput: true}
	_, report, err := RenderChatRequest(request, testExchangeContext(), caps)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, loss := range report.Losses {
		if loss.Feature == FeatureImageDetailInvented {
			if !strings.HasPrefix(loss.Path, "turns[0].parts[0]") {
				t.Fatalf("image decision path = %q, want the canonical tool-result part", loss.Path)
			}
			return
		}
	}
	t.Fatalf("report = %+v, want the invented-detail Note", report.Losses)
}
