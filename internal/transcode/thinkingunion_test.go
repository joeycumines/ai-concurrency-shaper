package transcode

// The canonical thinking union mirrors the Anthropic contract: enabled={type,
// budget_tokens}, disabled={type}, adaptive={type, display}. Decode enforces it,
// so a violation can only arrive on a hand-built CanonicalRequest — and that is
// exactly the case ValidateCanonicalRequest exists for, the same reason it bounds
// RetainedTurns. Without this check a bogus type fell through both Chat's and
// Responses' switch silently, and a hand-built display on a non-adaptive type
// rendered an invalid Anthropic union beside budget_tokens.
//
// One deliberate exception: enabled WITHOUT a budget stays valid. The renderers
// handle that shape as a named Note (the upstream applies its own default
// effort), so validating it away here would turn a documented, observable
// mapping into a refusal.

import (
	"strings"
	"testing"
)

// TestThinkingUnionIsValidated proves every malformed arm is refused before any
// renderer reads it.
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
			name:     "display on enabled",
			thinking: &CanonicalThinking{Type: "enabled", BudgetTokens: &budget, Display: "summarized"},
			wantErr:  "display",
		},
		{
			name:     "display on disabled",
			thinking: &CanonicalThinking{Type: "disabled", Display: "summarized"},
			wantErr:  "display",
		},
		{
			name:     "budget on disabled",
			thinking: &CanonicalThinking{Type: "disabled", BudgetTokens: &budget},
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

// TestValidThinkingArmsStillRender is the scope guard: the three legal shapes
// validate, and enabled without a budget reaches the renderers to be noted.
func TestValidThinkingArmsStillRender(t *testing.T) {
	budget := 4096
	display := "summarized"
	for _, thinking := range []*CanonicalThinking{
		{Type: "enabled", BudgetTokens: &budget},
		{Type: "enabled"},
		{Type: "disabled"},
		{Type: "adaptive", Display: display},
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
		// The Messages target carries the whole union natively.
		if _, _, err := RenderMessagesRequest(request, testExchangeContext()); err != nil {
			t.Fatalf("%+v failed the Messages render: %v", thinking, err)
		}
	}
}
