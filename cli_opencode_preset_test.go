package main

// Opencode-preset CLI acceptance: the preset line appears for opencode.ai
// mounts and warns elsewhere; the flags parse in both scopes.

import (
	"strings"
	"testing"
)

// TestCLIValidOpencodePreset proves -opencode clears validation and logs
// the first-party line on an opencode.ai mount.
func TestCLIValidOpencodePreset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	out, err := runCLIStartup(t,
		"-bind", "127.0.0.1:1",
		"-upstream", "https://opencode.ai/zen",
		"-name", "zen",
		"-model-table", "m@zen=wire;via=messages",
		"-native-route", "messages@/v1/messages",
		"-opencode",
	)
	if err == nil {
		t.Fatalf("want bind failure, got success: %s", out)
	}
	if !strings.Contains(out, `opencode preset: provider "zen" emits the first-party request shape`) {
		t.Fatalf("output missing preset line: %s", out)
	}
}

// TestCLIOpencodePresetWarnsOffHost proves the preset warns when the mount
// points away from opencode.ai instead of silently impersonating there.
func TestCLIOpencodePresetWarnsOffHost(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	out, err := runCLIStartup(t,
		"-bind", "127.0.0.1:1",
		"-upstream", "http://127.0.0.1:1",
		"-name", "local",
		"-model-table", "m@local=wire;via=messages",
		"-native-route", "messages@/v1/messages",
		"-opencode",
	)
	if err == nil {
		t.Fatalf("want bind failure, got success: %s", out)
	}
	if !strings.Contains(out, "is not opencode.ai") {
		t.Fatalf("output missing off-host note: %s", out)
	}
}
