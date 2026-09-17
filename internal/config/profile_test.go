package config

import (
	"strings"
	"testing"
)

// TestTranscodeProfileStartupValidation verifies that startup validation checks
// profile target models against the resolved model map or model table.
func TestTranscodeProfileStartupValidation(t *testing.T) {
	base := func() *Provider {
		return &Provider{
			Name:                   "test-provider",
			TranscodeResponsesChat: true,
			TranscodeAuth:          "none",
		}
	}

	t.Run("unmapped_model_with_explicit_model_map_fails", func(t *testing.T) {
		p := base()
		p.TranscodeProfiles = []string{"scanner=unmapped-model:low"}
		p.TranscodeModelMap = []string{"other=other-wire"}
		err := p.resolveTranscode(nil)
		if err == nil {
			t.Fatal("want startup error for profile with unmapped target model, got nil")
		}
		if !strings.Contains(err.Error(), "invalid -transcode-profile \"scanner\": target model \"unmapped-model\" cannot be resolved") {
			t.Errorf("error = %q, want containing profile and model name", err.Error())
		}
	})

	t.Run("mapped_model_with_explicit_model_map_succeeds", func(t *testing.T) {
		p := base()
		p.TranscodeProfiles = []string{"scanner=mapped-model:low"}
		p.TranscodeModelMap = []string{"mapped-model=upstream-wire"}
		if err := p.resolveTranscode(nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("unmapped_model_with_model_table_fails", func(t *testing.T) {
		p := base()
		p.TranscodeProfiles = []string{"analyst=unlisted-model:high"}
		table := []modelTableEntry{
			{Provider: "test-provider", Surrogate: "listed-model", Wire: "listed-wire"},
		}
		err := p.resolveTranscode(table)
		if err == nil {
			t.Fatal("want startup error for profile with unlisted model in table, got nil")
		}
		if !strings.Contains(err.Error(), "invalid -transcode-profile \"analyst\": target model \"unlisted-model\" cannot be resolved") {
			t.Errorf("error = %q, want containing profile and model name", err.Error())
		}
	})

	t.Run("identity_fallback_active_succeeds", func(t *testing.T) {
		p := base()
		p.TranscodeProfiles = []string{"scanner=any-model:low"}
		// No -transcode-model and no table: identity fallback is active
		if err := p.resolveTranscode(nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
