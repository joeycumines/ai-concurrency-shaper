package config

// QUEUE-1-B regression: the -queue-depth and -queue-comments flags must
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
