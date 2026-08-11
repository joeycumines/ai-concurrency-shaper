package main

// Review-z commit 6 acceptance: the CLI rejects every enumerated impossible
// transcoding configuration at startup, and only the granular loss names are
// accepted.

import (
	"os/exec"
	"strings"
	"testing"
)

// runCLIStartup runs the built binary with the given args and returns
// (stderr+stdout, exitError). The binary exits during configuration with a
// non-zero status and an error message; the upstream/listen targets are
// unreachable and must never be contacted.
func runCLIStartup(t *testing.T, args ...string) (string, error) {
	t.Helper()
	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestCLIRejectsImpossibleTranscodeConfigs proves the six enumerated
// startup rejections fire before any traffic is served (review-z commit 6).
func TestCLIRejectsImpossibleTranscodeConfigs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			// Messages->Responses under the strict loss policy: Messages
			// tools cannot preserve strictness, so tool requests could
			// never be served.
			name: "messages-responses under strict policy",
			args: []string{
				"-bind", "127.0.0.1:1",
				"-upstream", "http://127.0.0.1:1",
				"-transcode-messages-responses",
			},
			wantErr: "tool_schema_strictness",
		},
		{
			// The same preset becomes valid once the strictness loss is
			// explicitly allowed (granular names only).
			name: "messages-responses with the strictness loss allowed",
			args: []string{
				// The unreachable bind fails AFTER config validation, so a
				// config-accepted run exits with a bind error, never a
				// config error.
				"-bind", "127.0.0.1:1",
				"-upstream", "http://127.0.0.1:1",
				"-transcode-messages-responses",
				"-transcode-allow-loss", "tool_schema_strictness",
			},
			wantErr: "", // must pass config; only the bind may fail
		},
		{
			// Unknown loss names are rejected; the broad legacy names are
			// removed, not aliased.
			name: "legacy broad loss name rejected",
			args: []string{
				"-bind", "127.0.0.1:1",
				"-upstream", "http://127.0.0.1:1",
				"-transcode-responses-chat",
				"-transcode-allow-loss", "all",
			},
			wantErr: "unknown loss feature",
		},
		{
			name: "legacy permissive loss name rejected",
			args: []string{
				"-bind", "127.0.0.1:1",
				"-upstream", "http://127.0.0.1:1",
				"-transcode-responses-chat",
				"-transcode-allow-loss", "permissive",
			},
			wantErr: "unknown loss feature",
		},
		{
			// Invalid custom auth header name.
			name: "invalid custom auth header",
			args: []string{
				"-bind", "127.0.0.1:1",
				"-upstream", "http://127.0.0.1:1",
				"-transcode-responses-chat",
				"-transcode-auth", "header",
				"-transcode-auth-header", "bad header!",
			},
			wantErr: "not a valid HTTP field name",
		},
		{
			// A custom auth header reserved by the pipeline.
			name: "reserved custom auth header",
			args: []string{
				"-bind", "127.0.0.1:1",
				"-upstream", "http://127.0.0.1:1",
				"-transcode-responses-chat",
				"-transcode-auth", "header",
				"-transcode-auth-header", "Authorization",
			},
			wantErr: "reserved",
		},
		{
			// An explicit messages->responses route under the strict
			// policy fails the FINAL-policy validation in
			// buildTranscodeMappings.
			name: "explicit messages-responses route under strict policy",
			args: []string{
				"-bind", "127.0.0.1:1",
				"-upstream", "http://127.0.0.1:1",
				"-transcode-route", "messages@/v1/messages=responses@/v1/responses",
			},
			wantErr: "tool_schema_strictness",
		},
		{
			// Duplicate normalized client routes.
			name: "duplicate client routes",
			args: []string{
				"-bind", "127.0.0.1:1",
				"-upstream", "http://127.0.0.1:1",
				"-transcode-route", "responses@/v1/responses=chat-completions@/v1/chat/completions",
				"-transcode-responses-chat",
			},
			wantErr: "duplicate transcode client route",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runCLIStartup(t, tt.args...)
			if tt.wantErr == "" {
				if err != nil && !strings.Contains(out, "listen") && !strings.Contains(out, "bind") {
					t.Fatalf("startup failed: %v\n%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("startup succeeded, want rejection: %s", out)
			}
			if !strings.Contains(out, tt.wantErr) {
				t.Fatalf("output = %q, want %q", out, tt.wantErr)
			}
		})
	}
}
