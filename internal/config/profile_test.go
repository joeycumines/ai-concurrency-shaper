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

// TestTranscodeProfileNameModelCollision proves a profile whose NAME is also a
// mapped model is rejected at startup. A profile is resolved before the model
// map, so such a profile silently shadows the mapping: the client would be told
// the mapped name while being served the profile's target, which is a false
// identity rather than a routing choice.
func TestTranscodeProfileNameModelCollision(t *testing.T) {
	base := func() *Provider {
		return &Provider{
			Name:                   "test-provider",
			TranscodeResponsesChat: true,
			TranscodeAuth:          "none",
		}
	}

	t.Run("colliding_profile_name_fails", func(t *testing.T) {
		p := base()
		p.TranscodeModelMap = []string{"gpt-4o-mini=real-wire", "other-model=other-wire"}
		p.TranscodeProfiles = []string{"gpt-4o-mini=other-model:high"}
		err := p.resolveTranscode(nil)
		if err == nil {
			t.Fatal("want startup error for a profile name that collides with a mapped model, got nil")
		}
		if !strings.Contains(err.Error(), `invalid -transcode-profile "gpt-4o-mini"`) ||
			!strings.Contains(err.Error(), "collides with a mapped model") {
			t.Errorf("error = %q, want naming the colliding profile and the collision", err.Error())
		}
	})

	t.Run("non_colliding_profile_name_succeeds", func(t *testing.T) {
		p := base()
		p.TranscodeModelMap = []string{"gpt-4o-mini=real-wire"}
		p.TranscodeProfiles = []string{"scanner=gpt-4o-mini:high"}
		if err := p.resolveTranscode(nil); err != nil {
			t.Errorf("non-colliding profile rejected: %v", err)
		}
	})
}
