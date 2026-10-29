// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

//go:build unix

package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// execAuthSource writes a /bin/sh credential helper and returns the
// -auth-source value running it plus a run counter.
func execAuthSource(t *testing.T, body string) (string, func() int) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "runs.log")
	path := filepath.Join(dir, "credential.sh")
	script := "#!/bin/sh\necho run >> '" + log + "'\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	argv, err := json.Marshal([]string{path, "ai-credential", "--", "bedrock"})
	if err != nil {
		t.Fatal(err)
	}
	return "exec:" + string(argv), func() int {
		raw, _ := os.ReadFile(log)
		return strings.Count(string(raw), "\n")
	}
}

func TestResolveAndValidate_ExecAuthSourceStaysLive(t *testing.T) {
	source, runs := execAuthSource(t, `[ "$1" = ai-credential ] || exit 9; echo bedrock-key-1`)
	cfg, err := Parse([]string{
		"-upstream", "https://bedrock-runtime.us-east-1.amazonaws.com/openai",
		"-auth-source=" + source,
		"-auth-source-ttl", "50m",
		"-auth-source-timeout", "10s",
		"-transcode-messages-chat",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	p := cfg.Providers[0]
	policy := p.AuthPolicy()
	if policy == nil || policy.Mode != auth.AuthBearer {
		t.Fatalf("AuthPolicy = %+v, want bearer", policy)
	}
	live, ok := policy.Secret.(*auth.ExecSecretSource)
	if !ok {
		t.Fatalf("policy Secret = %T, want the live *auth.ExecSecretSource", policy.Secret)
	}
	if n := runs(); n != 1 {
		t.Fatalf("command ran %d times at startup, want exactly 1 (eager resolution)", n)
	}
	got, err := live.Secret(context.Background())
	if err != nil || got != "bedrock-key-1" {
		t.Fatalf("Secret = %q, %v; want bedrock-key-1", got, err)
	}

	mappings := p.TranscodeMappings()
	if len(mappings) != 1 {
		t.Fatalf("mappings = %d, want 1", len(mappings))
	}
	inherited := mappings[0].Mapping.Auth
	if inherited.Mode != transcode.AuthBearer {
		t.Fatalf("transcode auth mode = %q, want bearer", inherited.Mode)
	}
	if inherited.Secret != transcode.SecretSource(live) {
		t.Fatalf("transcode mapping Secret = %T, want the provider's live exec source", inherited.Secret)
	}
}

func TestResolveAndValidate_ExecAuthSourceFailureFailsStartup(t *testing.T) {
	source, _ := execAuthSource(t, "echo would-be-secret\nexit 1")
	cfg, err := Parse([]string{
		"-upstream", "https://x.example",
		"-auth-source=" + source,
		"-auth-source-ttl", "1h",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = cfg.ResolveAndValidate()
	if err == nil || !strings.Contains(err.Error(), "exec secret source: command exited with status 1") {
		t.Fatalf("ResolveAndValidate error = %v, want the exit-status class", err)
	}
	if strings.Contains(err.Error(), "would-be-secret") {
		t.Fatalf("startup error leaks command output: %v", err)
	}
}
