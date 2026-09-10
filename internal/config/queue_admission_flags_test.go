package config

// Regression: the -queue-depth and -queue-comments flags must
// actually PARSE. The -queue-depth registration was silently missing from
// flags.go for a window (the wiring and validation existed, the flag did
// not) — a config-layer parse test is the only guard that catches a flag
// registered nowhere, because the proxy-level tests construct options
// directly and never exercise the CLI surface.

import (
	"strings"
	"testing"
	"time"
)

func TestQueueAdmissionFlagsParse(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-queue-depth", "5",
		"-queue-comments", "15s",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	p := cfg.Providers[0]
	if p.QueueDepth != 5 {
		t.Fatalf("QueueDepth = %d, want 5", p.QueueDepth)
	}
	if p.QueueComments != 15*time.Second {
		t.Fatalf("QueueComments = %v, want 15s", p.QueueComments)
	}
}

func TestQueueAdmissionFlagsDefault(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers[0]
	if p.QueueDepth != 0 {
		t.Fatalf("QueueDepth = %d, want 0 (unbounded default)", p.QueueDepth)
	}
	if p.QueueComments != 0 {
		t.Fatalf("QueueComments = %v, want 0 (disabled default)", p.QueueComments)
	}
}

func TestQueueAdmissionFlagsNegativeRejected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flags   []string
		wantErr string
	}{
		{
			name:    "negative depth",
			flags:   []string{"-upstream", "https://api.openai.com", "-queue-depth", "-1"},
			wantErr: "-queue-depth must be >= 0",
		},
		{
			name:    "negative comments",
			flags:   []string{"-upstream", "https://api.openai.com", "-queue-comments", "-5s"},
			wantErr: "-queue-comments must be >= 0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse(tc.flags)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = cfg.ResolveAndValidate()
			if err == nil {
				t.Fatal("ResolveAndValidate expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestQueueAdmissionFlagsMultiProvider pins section-scoped parsing of the
// queue admission flags: in a multi-provider invocation, -queue-depth and
// -queue-comments must land on the provider section they appear in, and must
// not leak into the other sections (per-provider flag scoping is the whole
// point of the sectioned parser).
func TestQueueAdmissionFlagsMultiProvider(t *testing.T) {
	cfg, err := Parse([]string{
		"--provider=anthropic",
		"-upstream", "https://api.anthropic.com",
		"-prefix", "/claude",
		"-queue-depth", "1",
		"-queue-comments", "25ms",
		"--provider=openai",
		"-upstream", "https://api.openai.com",
		"-prefix", "/openai",
		"-queue-depth", "2",
		"-queue-comments", "0",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("len(Providers) = %d, want 2", len(cfg.Providers))
	}

	pA, pB := cfg.Providers[0], cfg.Providers[1]
	if pA.Name != "anthropic" || pB.Name != "openai" {
		t.Fatalf("provider names = %q, %q; want anthropic, openai", pA.Name, pB.Name)
	}
	if pA.QueueDepth != 1 {
		t.Errorf("anthropic QueueDepth = %d, want 1", pA.QueueDepth)
	}
	if pA.QueueComments != 25*time.Millisecond {
		t.Errorf("anthropic QueueComments = %v, want 25ms", pA.QueueComments)
	}
	if pB.QueueDepth != 2 {
		t.Errorf("openai QueueDepth = %d, want 2", pB.QueueDepth)
	}
	if pB.QueueComments != 0 {
		t.Errorf("openai QueueComments = %v, want 0 (explicitly disabled)", pB.QueueComments)
	}
}
