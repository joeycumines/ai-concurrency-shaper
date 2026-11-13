package transcode

// A request-side reasoning control is a real Gemini loss when the client asked
// for something Gemini's thinkingConfig cannot express, and a Note when the
// client merely delegated the decision — which is exactly what Gemini does. The
// three arms are not one decision, and thinking.display is a separate field.

import (
	"strings"
	"testing"
)

// TestGeminiAdaptiveThinkingIsANote proves delegated reasoning is not charged: a
// strict render must serve it, because "let the model decide" is Gemini's own
// behaviour.
func TestGeminiAdaptiveThinkingIsANote(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
		Thinking: &CanonicalThinking{Type: "adaptive"},
	}
	_, report, err := RenderGeminiRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("adaptive thinking was refused under the strict policy: %v", err)
	}
	found := false
	for _, loss := range report.Losses {
		if loss.Feature != FeatureRequestReasoning {
			continue
		}
		found = true
		if loss.Kind != NoteRecord {
			t.Fatalf("adaptive thinking recorded a %v: %+v", loss.Kind, loss)
		}
	}
	if !found {
		t.Fatalf("report = %+v, want the request_reasoning Note", report.Losses)
	}
}

// TestGeminiEnabledAndDisabledThinkingAreGated proves the arms that really do ask
// for something Gemini cannot express stay policy-gated, so an operator can
// refuse them.
func TestGeminiEnabledAndDisabledThinkingAreGated(t *testing.T) {
	budget := 2048
	cases := map[string]*CanonicalThinking{
		"enabled with a budget": {Type: "enabled", BudgetTokens: &budget},
		"disabled":              {Type: "disabled"},
	}
	for name, thinking := range cases {
		t.Run(name, func(t *testing.T) {
			request := CanonicalRequest{
				ClientModel: "m",
				Turns: []CanonicalTurn{{
					Role:  CanonicalUser,
					Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
				}},
				Thinking: thinking,
			}
			// Strict refuses.
			if _, _, err := RenderGeminiRequest(request, testExchangeContext()); err == nil {
				t.Fatal("a lost reasoning control was accepted under strict policy")
			} else if !strings.Contains(err.Error(), string(FeatureRequestReasoning)) {
				t.Fatalf("err = %q, want it to name %s", err, FeatureRequestReasoning)
			}
			// Approved: a policy-gated loss, not a Note.
			context := testExchangeContext()
			context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureRequestReasoning: {}}}
			_, report, err := RenderGeminiRequest(request, context)
			if err != nil {
				t.Fatalf("approved render: %v", err)
			}
			gated := 0
			for _, loss := range report.Losses {
				if loss.Feature == FeatureRequestReasoning && loss.Kind == LossRecord {
					gated++
				}
			}
			if gated != 1 {
				t.Fatalf("policy-gated request_reasoning entries = %d, want 1: %+v", gated, report.Losses)
			}
		})
	}
}

// TestGeminiThinkingDisplayNamesItsOwnPath proves the display preference is
// reported where the operator can find it, matching Chat and Responses.
func TestGeminiThinkingDisplayNamesItsOwnPath(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
		Thinking: &CanonicalThinking{Type: "adaptive", Display: "summarized"},
	}
	context := testExchangeContext()
	context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{FeatureRequestReasoning: {}}}
	_, report, err := RenderGeminiRequest(request, context)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !reportHasAt(report, FeatureRequestReasoning, "thinking.display") {
		t.Fatalf("report = %+v, want the display decision at thinking.display", report.Losses)
	}
}

// TestGeminiAdaptiveWithoutDisplayIsNotOverCharged proves a plain adaptive
// request does not also pay for a display it never carried.
func TestGeminiAdaptiveWithoutDisplayIsNotOverCharged(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
		Thinking: &CanonicalThinking{Type: "adaptive"},
	}
	_, report, err := RenderGeminiRequest(request, testExchangeContext())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if reportHasAt(report, FeatureRequestReasoning, "thinking.display") {
		t.Fatalf("an absent display was charged: %+v", report.Losses)
	}
}
