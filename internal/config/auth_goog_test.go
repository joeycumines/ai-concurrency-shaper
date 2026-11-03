package config

import (
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
)

func TestGoogAPIKeyAuthModeFlag(t *testing.T) {
	t.Setenv("SYNTHETIC_GEMINI_KEY", "synthetic-key")
	cfg, err := Parse([]string{
		"-upstream", "https://generativelanguage.googleapis.com/v1beta",
		"-auth-source", "env:SYNTHETIC_GEMINI_KEY",
		"-auth-mode", "goog-api-key",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	policy := cfg.Providers[0].AuthPolicy()
	if policy == nil || policy.Mode != auth.AuthGoogAPIKey {
		t.Fatalf("AuthPolicy = %+v, want mode %q", policy, auth.AuthGoogAPIKey)
	}
}

func TestGoogAPIKeyAutoDerivedForGoogleHost(t *testing.T) {
	t.Setenv("SYNTHETIC_GEMINI_KEY", "synthetic-key")
	cfg, err := Parse([]string{
		"-upstream", "https://generativelanguage.googleapis.com/v1beta",
		"-auth-source", "env:SYNTHETIC_GEMINI_KEY",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	policy := cfg.Providers[0].AuthPolicy()
	if policy == nil || policy.Mode != auth.AuthGoogAPIKey {
		t.Fatalf("auto AuthPolicy = %+v, want mode %q for the Google host", policy, auth.AuthGoogAPIKey)
	}
}

func TestTranscodeAuthAcceptsGoogAPIKey(t *testing.T) {
	t.Setenv("SYNTHETIC_GEMINI_KEY", "synthetic-key")
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
		"-transcode-auth", "goog-api-key",
		"-transcode-auth-source", "env:SYNTHETIC_GEMINI_KEY",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
}
