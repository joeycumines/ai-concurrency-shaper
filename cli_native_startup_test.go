package main

// Native-route CLI acceptance: valid native declarations pass
// configuration (only the unreachable bind may fail), while colliding and
// malformed declarations fail before any traffic is served.

import (
	"strings"
	"testing"
)

// TestCLIValidNativeConfigPasses proves a natively served provider with
// table coverage and via dialects clears startup validation.
func TestCLIValidNativeConfigPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	out, err := runCLIStartup(t,
		"-bind", "127.0.0.1:1",
		"-upstream", "http://127.0.0.1:1",
		"-name", "zen",
		"-model-table", "m@zen=wire;via=messages",
		"-model-table", "c@zen=chat-wire;via=chat",
		"-native-route", "messages@/v1/messages",
		"-native-route", "chat@/v1/chat/completions",
	)
	if err == nil {
		t.Fatalf("want bind failure, got success: %s", out)
	}
	// The unreachable bind fails AFTER config validation, so output must
	// carry the native startup line and no config error.
	if !strings.Contains(out, "native: 2 route(s): messages@/v1/messages, chat@/v1/chat/completions") {
		t.Fatalf("output missing native startup line: %s", out)
	}
	if strings.Contains(out, "invalid -model-table") ||
		strings.Contains(out, "native route") && strings.Contains(out, "collides") {
		t.Fatalf("valid native config rejected: %s", out)
	}
}

// TestCLIRejectsBadNativeConfigs proves malformed and colliding native
// declarations fail at startup.
func TestCLIRejectsBadNativeConfigs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name: "native collides with transcode mapping",
			args: []string{
				"-bind", "127.0.0.1:1",
				"-upstream", "http://127.0.0.1:1",
				"-name", "zen",
				"-model-table", "m@zen=wire;via=messages",
				"-transcode-messages-chat",
				"-native-route", "messages@/v1/messages",
			},
			wantErr: "collides with a transcode mapping",
		},
		{
			name: "unknown native protocol",
			args: []string{
				"-bind", "127.0.0.1:1",
				"-upstream", "http://127.0.0.1:1",
				"-name", "zen",
				"-model-table", "m@zen=wire;via=messages",
				"-native-route", "grpc@/v1/messages",
			},
			wantErr: "unknown native protocol",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runCLIStartup(t, tt.args...)
			if err == nil {
				t.Fatalf("want startup failure, got success: %s", out)
			}
			if !strings.Contains(out, tt.wantErr) {
				t.Fatalf("output = %s, want substring %q", out, tt.wantErr)
			}
		})
	}
}
