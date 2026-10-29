package config

import (
	"strings"
	"testing"
)

func TestResolveAndValidateRejectsHostlessUpstream(t *testing.T) {
	for _, upstream := range []string{"http://:8080", "https://"} {
		t.Run(upstream, func(t *testing.T) {
			cfg, err := Parse([]string{"-upstream=" + upstream})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = cfg.ResolveAndValidate()
			if err == nil || !strings.Contains(err.Error(), "hostname") {
				t.Fatalf("ResolveAndValidate error = %v, want missing hostname validation", err)
			}
		})
	}
}

func TestConfigCloseClearsResolvedServerConfigAndDerivedProviderNames(t *testing.T) {
	cfg, err := Parse([]string{
		"-rc-server-bind=127.0.0.1:0",
		"--provider=acme",
		"-upstream=https://acme.example.com",
		"-prefix=/acme",
		"--provider",
		"-upstream=https://api.anthropic.com",
		"-prefix=/claude",
		"--provider",
		"-upstream=https://api.openai.com",
		"-prefix=/openai",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	if cfg.Server.rcServerConfig == nil {
		t.Fatal("precondition: resolved server RC configuration is nil")
	}
	if got := cfg.Providers[1].Name; got != "anthropic" {
		t.Fatalf("precondition: derived provider name = %q, want anthropic", got)
	}
	if got := cfg.Providers[2].Name; got != "openai" {
		t.Fatalf("precondition: derived provider name = %q, want openai", got)
	}
	cfg.Providers[2].Name = "operator-chosen"

	cfg.Close()

	if cfg.Server.rcServerConfig != nil {
		t.Fatal("Close left resolved server RC configuration attached")
	}
	if got := cfg.Providers[1].Name; got != "" {
		t.Fatalf("Close left derived provider name %q attached", got)
	}
	if got := cfg.Providers[1].derivedName; got != "" {
		t.Fatalf("Close left derivedName %q attached", got)
	}
	if got := cfg.Providers[2].Name; got != "operator-chosen" {
		t.Fatalf("Close erased caller-assigned provider name %q", got)
	}
	if got := cfg.Providers[2].derivedName; got != "" {
		t.Fatalf("Close left stale derivedName %q attached", got)
	}
}
