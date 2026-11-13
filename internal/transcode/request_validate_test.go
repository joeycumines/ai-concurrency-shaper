package transcode

// ValidateCanonicalRequest is the single gate every request renderer passes, so
// it is the only place a hand-built CanonicalRequest can be policed. Two gaps
// let an invalid request through: a thinking budget that is present but not
// positive (which the Messages renderer copies straight onto the wire), and the
// parts nested inside a function result, which are walked by every renderer but
// were never type-checked.

import (
	"strconv"
	"strings"
	"testing"
)

// TestValidateCanonicalRequestRejectsNonPositiveBudget proves the thinking
// budget's sign is validated: the Messages decoder rejects a non-positive
// budget, so a hand-built request that slips past validation would render a
// request the upstream refuses.
func TestValidateCanonicalRequestRejectsNonPositiveBudget(t *testing.T) {
	for _, budget := range []int{0, -1, -4096} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			value := budget
			request := CanonicalRequest{
				ClientModel: "m",
				Turns: []CanonicalTurn{{
					Role:  CanonicalUser,
					Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
				}},
				Thinking: &CanonicalThinking{Type: "enabled", BudgetTokens: &value},
			}
			err := ValidateCanonicalRequest(request)
			if err == nil {
				t.Fatalf("budget %d passed validation", budget)
			}
			if !strings.Contains(err.Error(), "budget_tokens") {
				t.Fatalf("err = %q, want it to name budget_tokens", err)
			}
			// Every renderer must refuse it, not just the validator.
			if _, _, err := RenderMessagesRequest(request, testExchangeContext()); err == nil {
				t.Fatalf("budget %d rendered to Messages", budget)
			}
		})
	}

	// A positive budget still passes.
	positive := 1024
	if err := ValidateCanonicalRequest(CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
		Thinking: &CanonicalThinking{Type: "enabled", BudgetTokens: &positive},
	}); err != nil {
		t.Fatalf("a positive budget was refused: %v", err)
	}
}

// TestValidateCanonicalRequestRejectsNestedThinkingInAFunctionResult proves the
// marker-signature contract holds at EVERY depth: a thinking part nested inside
// a function result must be refused, because no renderer can place it and the
// Responses renderer would otherwise strip it silently.
func TestValidateCanonicalRequestRejectsNestedThinkingInAFunctionResult(t *testing.T) {
	for name, part := range map[string]CanonicalPart{
		"authenticated": CanonicalThinkingPart{Signature: "real-provider-signature"},
		"marker":        CanonicalThinkingPart{Signature: SyntheticThinkingSignature},
	} {
		t.Run(name, func(t *testing.T) {
			request := CanonicalRequest{
				ClientModel: "m",
				Turns: []CanonicalTurn{{
					Role: CanonicalUser,
					Parts: []CanonicalPart{
						CanonicalFunctionResult{CallID: "call_1", Parts: []CanonicalPart{part}},
					},
				}},
			}
			if err := ValidateCanonicalRequest(request); err == nil {
				t.Fatalf("a %s thinking part nested in a function result passed validation", name)
			}
		})
	}
}

// TestValidateCanonicalRequestRejectsAPlacedNowhereThinkingPart proves the ONE
// restriction a tool result needs, and the one it must NOT have. A thinking part
// has no place inside a result (the IR records no position for one, and the
// Responses renderer would strip it silently), so it is refused. A nested
// tool_result or tool_use is a different matter: an Anthropic tool_result's
// content is the same Content union, so the Messages decoder legitimately
// produces those shapes and refusing them would reject a request the client sent
// in its own dialect. That reachability is pinned in
// TestNestedToolResultIsReachableAndRenderable.
func TestValidateCanonicalRequestRejectsAPlacedNowhereThinkingPart(t *testing.T) {
	for name, part := range map[string]CanonicalPart{
		"marker thinking":        CanonicalThinkingPart{Signature: SyntheticThinkingSignature},
		"authenticated thinking": CanonicalThinkingPart{Signature: "provider-sig"},
	} {
		t.Run(name, func(t *testing.T) {
			request := CanonicalRequest{
				ClientModel: "m",
				Turns: []CanonicalTurn{{
					Role: CanonicalUser,
					Parts: []CanonicalPart{
						CanonicalFunctionResult{CallID: "call_1", Parts: []CanonicalPart{part}},
					},
				}},
			}
			err := ValidateCanonicalRequest(request)
			if err == nil {
				t.Fatalf("a %s part nested in a function result passed validation", name)
			}
			// Validation speaks in canonical indices ("turn N part M").
			if !strings.Contains(err.Error(), "turn 0 part 0") {
				t.Fatalf("err = %q, want it to name the canonical turn and part", err)
			}
		})
	}

	// Content still validates, including the nested shapes the wire allows.
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role: CanonicalUser,
			Parts: []CanonicalPart{
				CanonicalFunctionResult{CallID: "call_1", Parts: []CanonicalPart{
					CanonicalText{Text: "done"},
					CanonicalImage{MediaType: "image/png", URL: "https://example.test/i.png"},
					CanonicalDocument{MediaType: "application/pdf", URL: "https://example.test/d.pdf"},
				}},
			},
		}},
	}
	if err := ValidateCanonicalRequest(request); err != nil {
		t.Fatalf("content-only nested parts were refused: %v", err)
	}
}

// TestNestedResultFailureIsTypedOnEveryTarget proves the refusal is the SAME
// central one everywhere, rather than each renderer inventing its own: the
// Responses renderer used to strip a nested thinking part with no record at all.
func TestNestedResultFailureIsTypedOnEveryTarget(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role: CanonicalUser,
			Parts: []CanonicalPart{
				CanonicalFunctionResult{CallID: "call_1", Parts: []CanonicalPart{
					CanonicalThinkingPart{Signature: "real-provider-signature"},
				}},
			},
		}},
	}
	for name, render := range allRequestRenderers() {
		t.Run(name, func(t *testing.T) {
			_, report, err := render(request, testExchangeContext())
			if err == nil {
				t.Fatalf("the renderer accepted a nested authenticated thinking part")
			}
			if len(report.Losses) != 0 {
				t.Fatalf("a validation refusal recorded report entries: %+v", report.Losses)
			}
		})
	}
}

// TestNestedThinkingRefusalIsStable guards against a silently narrowed check:
// the refusal must not depend on the signature being absent or non-marker in
// some particular way.
func TestNestedThinkingRefusalIsStable(t *testing.T) {
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role: CanonicalUser,
			Parts: []CanonicalPart{
				CanonicalFunctionResult{CallID: "call_1", Parts: []CanonicalPart{
					CanonicalThinkingPart{Signature: SyntheticThinkingSignature},
					CanonicalText{Text: "done"},
				}},
			},
		}},
	}
	if err := ValidateCanonicalRequest(request); err == nil {
		t.Fatal("a marker-signature thinking part nested in a function result passed validation")
	}
}
