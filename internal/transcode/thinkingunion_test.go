package transcode

// The canonical thinking union mirrors the Anthropic contract, which has four
// arms: enabled={type,budget_tokens,display?}, disabled={type},
// between_tools={type}, and adaptive={type,display?}. The display enum is
// summarized|omitted|updates, the last gated upstream by the
// thinking-display-updates-2026-08-18 Anthropic-Beta member that claude-cli
// 2.x sends. Decode enforces the union, so a violation can only arrive on a
// hand-built CanonicalRequest — and that is exactly the case
// ValidateCanonicalRequest exists for, the same reason it bounds
// RetainedTurns. Without this check a bogus type fell through every
// renderer's switch silently, and a bogus display value rendered an invalid
// Anthropic union.
//
// Two deliberate allowances: display is legal on enabled as well as adaptive
// (the contract documents it on both), and enabled WITHOUT a budget stays
// valid. The renderers handle the budget-less shape as a named Note (the
// upstream applies its own default effort), so validating it away here would
// turn a documented, observable mapping into a refusal.

import (
	"strings"
	"testing"
)

// TestThinkingUnionIsValidated proves every malformed arm is refused before any
// renderer reads it, and that the refusal names the offending member.
func TestThinkingUnionIsValidated(t *testing.T) {
	budget := 4096
	cases := []struct {
		name     string
		thinking *CanonicalThinking
		wantErr  string
	}{
		{
			name:     "unknown type",
			thinking: &CanonicalThinking{Type: "bogus"},
			wantErr:  "bogus",
		},
		{
			name:     "display on disabled",
			thinking: &CanonicalThinking{Type: "disabled", Display: "summarized"},
			wantErr:  "display",
		},
		{
			name:     "display on between_tools",
			thinking: &CanonicalThinking{Type: "between_tools", Display: "omitted"},
			wantErr:  "display",
		},
		{
			name:     "unknown display value",
			thinking: &CanonicalThinking{Type: "adaptive", Display: "full"},
			wantErr:  "display",
		},
		{
			// A display on the budget-less enabled arm cannot be rendered
			// anywhere — Messages drops the whole config (the enabled arm
			// requires budget_tokens) and the other targets map the shape to a
			// "no control" Note — so accepting it would destroy a
			// client-chosen value with no entry naming it.
			name:     "display on enabled without a budget",
			thinking: &CanonicalThinking{Type: "enabled", Display: "updates"},
			wantErr:  "budget_tokens",
		},
		{
			name:     "budget on disabled",
			thinking: &CanonicalThinking{Type: "disabled", BudgetTokens: &budget},
			wantErr:  "budget_tokens",
		},
		{
			name:     "budget on between_tools",
			thinking: &CanonicalThinking{Type: "between_tools", BudgetTokens: &budget},
			wantErr:  "budget_tokens",
		},
		{
			name:     "budget on adaptive",
			thinking: &CanonicalThinking{Type: "adaptive", BudgetTokens: &budget},
			wantErr:  "budget_tokens",
		},
	}
	request := CanonicalRequest{
		ClientModel: "m",
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request.Thinking = tc.thinking
			err := ValidateCanonicalRequest(request)
			if err == nil {
				t.Fatalf("%+v was accepted", tc.thinking)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidThinkingArmsStillRender is the scope guard: every legal shape
// validates, and the Messages target carries each one natively — including the
// beta display member on both arms it is documented for, and the budget-less
// enabled arm the renderers map to a Note.
func TestValidThinkingArmsStillRender(t *testing.T) {
	budget := 4096
	for _, thinking := range []*CanonicalThinking{
		{Type: "enabled", BudgetTokens: &budget},
		{Type: "enabled"},
		{Type: "enabled", BudgetTokens: &budget, Display: "summarized"},
		{Type: "enabled", BudgetTokens: &budget, Display: "updates"},
		{Type: "disabled"},
		{Type: "between_tools"},
		{Type: "adaptive", Display: "summarized"},
		{Type: "adaptive", Display: "updates"},
	} {
		request := CanonicalRequest{
			ClientModel:     "m",
			MaxOutputTokens: new(64),
			Thinking:        thinking,
			Turns: []CanonicalTurn{{
				Role:  CanonicalUser,
				Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
			}},
		}
		if err := ValidateCanonicalRequest(request); err != nil {
			t.Fatalf("%+v was refused: %v", thinking, err)
		}
		rendered, _, err := RenderMessagesRequest(request, testExchangeContext())
		if err != nil {
			t.Fatalf("%+v failed the Messages render: %v", thinking, err)
		}
		if thinking.Display != "" && !strings.Contains(
			string(rendered), `"display":"`+thinking.Display+`"`,
		) {
			t.Fatalf("display %q is missing from the Messages wire: %s", thinking.Display, rendered)
		}
	}
}
