package transcode

// Per-target decisions that must live in the renderer, not the decoder, plus
// the request-path invariant that a client value some target carries natively is
// never destroyed before that target is known.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

// decodeMessagesForTest decodes body under the strict policy: a decode that
// depends on the target is the defect these tests exist to prevent.
func decodeMessagesForTest(t *testing.T, body string) CanonicalRequest {
	t.Helper()
	result, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return result.Request
}

// TestMessagesTopKSurvivesDecodeAndRendersNatively proves top_k is carried by
// the IR and reproduced by the Messages target rather than destroyed at decode.
func TestMessagesTopKSurvivesDecodeAndRendersNatively(t *testing.T) {
	request := decodeMessagesForTest(t, `{"model":"m","max_tokens":10,"top_k":40,"messages":[{"role":"user","content":"hi"}]}`)
	if request.TopK == nil || *request.TopK != 40 {
		t.Fatalf("top_k = %v, want 40", request.TopK)
	}
	rendered, report, err := RenderMessagesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("messages render: %v", err)
	}
	var out anthropicmessages.Request
	if err := strictDecode(rendered, &out); err != nil {
		t.Fatalf("rendered messages: %v\n%s", err, rendered)
	}
	if out.TopK == nil || *out.TopK != 40 {
		t.Fatalf("rendered top_k = %v, want 40", out.TopK)
	}
	if reportHasFeature(report, FeatureTopK) {
		t.Fatalf("Messages target charged for a field it carries: %+v", report.Losses)
	}

	// A Chromium-like target without the field takes the loss at render.
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureTopK: {}}}
	rendered, report, err = RenderChatRequest(request, context, ChatCapabilities{})
	if err != nil {
		t.Fatalf("chat render: %v", err)
	}
	if !reportHasAt(report, FeatureTopK, "top_k") {
		t.Fatalf("report = %+v, want the chat-side top_k loss", report.Losses)
	}
	if strings.Contains(string(rendered), `"top_k"`) {
		t.Fatalf("chat body carries top_k: %s", rendered)
	}
}

// TestMessagesDisableParallelToolUseIsCarried proves the flag is the canonical
// parallel-tools setting and is emitted natively by the Messages target.
func TestMessagesDisableParallelToolUseIsCarried(t *testing.T) {
	request := decodeMessagesForTest(t,
		`{"model":"m","max_tokens":10,"tool_choice":{"type":"auto","disable_parallel_tool_use":true},`+
			`"tools":[{"name":"f","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`)
	if request.ParallelTools == nil || *request.ParallelTools {
		t.Fatalf("parallel tools = %v, want false", request.ParallelTools)
	}
	rendered, _, err := RenderMessagesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("messages render: %v", err)
	}
	var out anthropicmessages.Request
	if err := strictDecode(rendered, &out); err != nil {
		t.Fatalf("rendered: %v\n%s", err, rendered)
	}
	if out.ToolChoice == nil || out.ToolChoice.DisableParallelToolUse == nil ||
		!*out.ToolChoice.DisableParallelToolUse {
		t.Fatalf("tool_choice = %+v, want disable_parallel_tool_use", out.ToolChoice)
	}
}

// TestMessagesSystemNonTextContentIsLossGated proves a non-text system part
// takes the same decision as the other targets instead of a hard refusal.
func TestMessagesSystemNonTextContentIsLossGated(t *testing.T) {
	request := CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns: []CanonicalTurn{
			{Role: CanonicalSystem, Parts: []CanonicalPart{
				CanonicalText{Text: "be brief"},
				CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"},
			}},
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
		},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureSystemNonTextContent: {}}}
	rendered, report, err := RenderMessagesRequest(request, context)
	if err != nil {
		t.Fatalf("approved system_non_text_content must render: %v", err)
	}
	if !reportHasFeature(report, FeatureSystemNonTextContent) {
		t.Fatalf("report = %+v, want the system_non_text_content loss", report.Losses)
	}
	var out anthropicmessages.Request
	if err := strictDecode(rendered, &out); err != nil {
		t.Fatalf("rendered: %v\n%s", err, rendered)
	}
	if out.System == nil || out.System.ContentStr == nil || *out.System.ContentStr != "be brief" {
		t.Fatalf("system = %+v, want the surviving text", out.System)
	}

	// Strict policy is the same keyed refusal the other targets give.
	if _, _, err := RenderMessagesRequest(request, testExchangeContext()); err == nil {
		t.Fatal("strict render accepted non-text system content")
	}
}

// TestMessagesStrictFalseIsNotAStrictnessLoss proves strict:false (no
// enforcement, exactly what Messages does) is not charged as a loss.
func TestMessagesStrictFalseIsNotAStrictnessLoss(t *testing.T) {
	strict := false
	request := CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns:           []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}}},
		Tools: []CanonicalTool{{
			Name:       "f",
			JSONSchema: json.RawMessage(`{"type":"object"}`),
			Strict:     &strict,
		}},
	}
	_, report, err := RenderMessagesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("strict:false render: %v", err)
	}
	if reportHasFeature(report, FeatureToolSchemaStrictness) {
		t.Fatalf("strict:false charged as a loss: %+v", report.Losses)
	}
}

// TestMessagesToolResultOrderingErrorUsesTargetVocabulary proves the ordering
// refusal speaks the Messages dialect, not Gemini's.
func TestMessagesToolResultOrderingErrorUsesTargetVocabulary(t *testing.T) {
	request := CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns: []CanonicalTurn{{
			Role: CanonicalUser,
			Parts: []CanonicalPart{
				CanonicalText{Text: "before"},
				CanonicalFunctionResult{CallID: "call_1", Parts: []CanonicalPart{CanonicalText{Text: "ok"}}},
			},
		}},
	}
	_, _, err := RenderMessagesRequest(request, testExchangeContext())
	if err == nil {
		t.Fatal("a tool result after other user content must be refused")
	}
	if !strings.Contains(err.Error(), "tool_result") || !strings.Contains(err.Error(), "message") {
		t.Fatalf("err = %q, want the Messages dialect (tool_result, message)", err)
	}
	if strings.Contains(err.Error(), "functionResponse") {
		t.Fatalf("err = %q leaks Gemini vocabulary", err)
	}
}

// TestRetainedTurnsIsBounded proves an out-of-range retained count is a local
// validation error rather than a silent reclassification of client input.
func TestRetainedTurnsIsBounded(t *testing.T) {
	turns := []CanonicalTurn{{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}}}
	for _, retained := range []int{-1, 2} {
		request := CanonicalRequest{ClientModel: "m", RetainedTurns: retained, Turns: turns}
		if err := ValidateCanonicalRequest(request); err == nil {
			t.Fatalf("RetainedTurns %d accepted", retained)
		} else if !strings.Contains(err.Error(), "retained turn count") {
			t.Fatalf("err = %q, want it to name the field", err)
		}
	}
	if err := ValidateCanonicalRequest(CanonicalRequest{ClientModel: "m", RetainedTurns: 1, Turns: turns}); err != nil {
		t.Fatalf("a retained count equal to the turn count is valid: %v", err)
	}
}

// TestMarkerThinkingIsScrubbedByEveryRenderer proves synthetic thinking in a
// replayed turn is dropped instead of aborting the render.
func TestMarkerThinkingIsScrubbedByEveryRenderer(t *testing.T) {
	request := CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hi"}}},
			{Role: CanonicalAssistant, Parts: []CanonicalPart{
				CanonicalThinkingPart{Text: "reasoning", Signature: SyntheticThinkingSignature},
				CanonicalText{Text: "answer"},
			}},
		},
	}
	for name, render := range allRequestRenderers() {
		t.Run(name, func(t *testing.T) {
			rendered, _, err := render(request, testExchangeContext())
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.Contains(string(rendered), "reasoning") {
				t.Fatalf("marker thinking reached the wire: %s", rendered)
			}
		})
	}
}

// TestMessagesDisableParallelToolUseWithoutToolsIsNoted proves a tool-less
// Messages exchange cannot silently drop the setting — and that it must not
// REFUSE it either.
//
// The setting is a Note here rather than a policy-gated loss because it is
// already satisfied: with no tools declared the model cannot emit a tool call at
// all, parallel or otherwise. parallel_tool_calls is not a default approval, so
// charging it would refuse a request that loses nothing; the Note records the
// elision without pretending a client intent was destroyed.
func TestMessagesDisableParallelToolUseWithoutToolsIsNoted(t *testing.T) {
	request := decodeMessagesForTest(t,
		`{"model":"m","max_tokens":10,"tool_choice":{"type":"auto","disable_parallel_tool_use":true},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if request.ParallelTools == nil || *request.ParallelTools {
		t.Fatalf("parallel tools = %v, want false", request.ParallelTools)
	}
	// Strict: the elision is observable without a policy decision, so the
	// request renders.
	_, report, err := RenderMessagesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("a tool-less disable refused a default deployment: %v", err)
	}
	found := false
	for _, loss := range report.Losses {
		if loss.Feature != FeatureParallelToolCalls {
			continue
		}
		found = true
		if loss.Kind != NoteRecord {
			t.Fatalf("the inert tool-less disable recorded a %v: %+v", loss.Kind, loss)
		}
	}
	if !found {
		t.Fatalf("report = %+v, want the parallel_tool_calls Note", report.Losses)
	}
}

// TestMessagesLeadingMergeNoteFiresForRetainedTurns proves the leading-merge
// decision is reported even when the leading system turns came from the
// continuity store: the merge is a sanctioned encoding, not a policy gate, and
// the other three targets report the identical shape.
//
// The decision is filed under multiple_system_turns rather than
// mid_conversation_system: the same encoding must carry the same key on every
// target, or an operator denying multiple_system_turns would refuse the
// Responses target while the identical source rendered here. A leading merge is
// also not a mid-conversation move.
func TestMessagesLeadingMergeNoteFiresForRetainedTurns(t *testing.T) {
	text := func(s string) []CanonicalPart { return []CanonicalPart{CanonicalText{Text: s}} }
	request := retainedMessagesRequest([]CanonicalTurn{
		{Role: CanonicalSystem, Parts: text("one")},
		{Role: CanonicalSystem, Parts: text("two")},
		{Role: CanonicalUser, Parts: text("hi")},
	}, 3)
	_, report, err := RenderMessagesRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasFeature(report, FeatureMultipleSystemTurns) {
		t.Fatalf("report = %+v, want the leading-merge Note for retained leading turns", report.Losses)
	}
	for _, loss := range report.Losses {
		if loss.Feature == FeatureMultipleSystemTurns && loss.Kind == LossRecord {
			t.Fatalf("a sanctioned merge was recorded as a policy-gated loss: %+v", loss)
		}
	}
}

// TestToolResultFileIDDocumentIsNamedHonestly proves a file-id-only document
// fails with a message that names the identifier rather than "empty data".
func TestToolResultFileIDDocumentIsNamedHonestly(t *testing.T) {
	_, err := encodeToolResultEnvelope([]CanonicalPart{
		CanonicalDocument{FileID: "file-123"},
	}, "turns[0].parts[0].content")
	if err == nil {
		t.Fatal("a file-id-only document must not encode")
	}
	var unrepresentable *UnrepresentableError
	if !errors.As(err, &unrepresentable) {
		t.Fatalf("err = %T %v, want UnrepresentableError", err, err)
	}
	if !strings.Contains(unrepresentable.Detail, "file identifier") {
		t.Fatalf("detail = %q, want it to name the file identifier", unrepresentable.Detail)
	}
	// The identifier itself is the operator's only handle on the offending
	// document, so the message must carry it, not just its category.
	if !strings.Contains(unrepresentable.Detail, "file-123") {
		t.Fatalf("detail = %q, want it to name the identifier value", unrepresentable.Detail)
	}
}
