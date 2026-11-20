package anthropicmessages

import (
	"encoding/json"
	"testing"
)

// Reusing a wire value must replace the selected arm. Otherwise a valid
// type-only arm can retain a previous budget/display and marshal invalid JSON.
func TestThinkingConfigDecodeReplacesPreviousArm(t *testing.T) {
	for _, tc := range []struct {
		name, body, kind string
		budget           int
		display          string
	}{
		{"enabled absent display", `{"type":"enabled","budget_tokens":2048}`, "enabled", 2048, ""},
		{"enabled null display", `{"type":"enabled","budget_tokens":2048,"display":null}`, "enabled", 2048, ""},
		{"adaptive absent display", `{"type":"adaptive"}`, "adaptive", 0, ""},
		{"adaptive null display", `{"type":"adaptive","display":null}`, "adaptive", 0, ""},
		{"adaptive selected display", `{"type":"adaptive","display":"omitted"}`, "adaptive", 0, "omitted"},
		{"disabled", `{"type":"disabled"}`, "disabled", 0, ""},
		{"between tools", `{"type":"between_tools"}`, "between_tools", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget, display := 4096, "updates"
			config := ThinkingConfig{Type: "enabled", BudgetTokens: &budget, Display: &display}
			if err := json.Unmarshal([]byte(tc.body), &config); err != nil {
				t.Fatal(err)
			}
			if config.Type != tc.kind {
				t.Fatalf("type = %q, want %q", config.Type, tc.kind)
			}
			if tc.budget == 0 {
				if config.BudgetTokens != nil {
					t.Fatalf("%s retained the previous budget: %d", tc.kind, *config.BudgetTokens)
				}
			} else if config.BudgetTokens == nil || *config.BudgetTokens != tc.budget {
				t.Fatalf("budget = %v, want %d", config.BudgetTokens, tc.budget)
			}
			if tc.display == "" {
				if config.Display != nil {
					t.Fatalf("%s retained the previous display: %q", tc.kind, *config.Display)
				}
			} else if config.Display == nil || *config.Display != tc.display {
				t.Fatalf("display = %v, want %q", config.Display, tc.display)
			}
			encoded, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			var replay ThinkingConfig
			if err := json.Unmarshal(encoded, &replay); err != nil {
				t.Fatalf("decoded arm rendered invalid thinking %s: %v", encoded, err)
			}
		})
	}
}
