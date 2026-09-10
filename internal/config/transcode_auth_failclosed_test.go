package config

// -transcode-auth <mode> without -transcode-auth-source
// silently set Inbound=true, forwarding the CLIENT credential to the transcode
// target and contradicting the documented fail-closed default. The missing
// source is now a startup configuration error: Inbound stays false and
// Secret=nil, so the mapping's auth validation rejects the policy ("auth mode
// requires a secret source or inbound credentials").

import (
	"strings"
	"testing"
)

func TestTranscodeAuthModeWithoutSourceFailsClosed(t *testing.T) {
	for _, mode := range []string{"auto", "bearer", "x-api-key", "api-key"} {
		cfg, err := Parse([]string{
			"-upstream", "https://api.openai.com",
			"-transcode-responses-chat",
			"-transcode-auth", mode,
		})
		if err != nil {
			t.Fatalf("mode %s: Parse: %v", mode, err)
		}
		err = cfg.ResolveAndValidate()
		if err == nil {
			t.Fatalf("mode %s: ResolveAndValidate must fail without a source", mode)
		}
		if !strings.Contains(err.Error(), "requires a secret source or inbound credentials") {
			t.Fatalf("mode %s: err = %v, want the missing-source validation error", mode, err)
		}
	}
}

func TestTranscodeAuthInboundWithoutSourceRemainsValid(t *testing.T) {
	// Inbound IS the explicit source (the client's own credential): it stays
	// valid without -transcode-auth-source.
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
		"-transcode-auth", "bearer",
		"-transcode-auth-source", "inbound",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("inbound source must stay valid: %v", err)
	}
	authP := cfg.Providers[0].TranscodeMappings()[0].Mapping.Auth
	if !authP.Inbound {
		t.Fatalf("auth = %+v, want Inbound=true", authP)
	}
}

func TestTranscodeAuthHeaderModeWithoutSourceFailsWithHeaderErrorFirst(t *testing.T) {
	// A header-mode route with neither a header name nor a source reports
	// the header-name error first, then the operator fixes the source.
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
		"-transcode-auth", "header",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate must fail")
	}
	if !strings.Contains(err.Error(), "custom auth header is empty") {
		t.Fatalf("err = %v, want the empty-header-name error", err)
	}
}
