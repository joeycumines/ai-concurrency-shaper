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

// TestCLIOpencodePresetPairsRCClientHello proves the startup log reports the
// first-party shape spanning the rc-dial handshake when both the opencode
// preset and a remote-terminating RC dial with a client-hello spec are set.
func TestCLIOpencodePresetPairsRCClientHello(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI acceptance test in short mode")
	}
	out, _ := runCLIStartup(t,
		"-bind", "127.0.0.1:1",
		"-upstream", "https://opencode.ai/zen",
		"-name", "zen",
		"-model-table", "m@zen=wire;via=messages",
		"-native-route", "messages@/v1/messages",
		"-opencode",
		"-rc-server", "127.0.0.1:9090",
		"-rc-terminate-tls",
		"-rc-target-client-hello", `{"sessionIdLength":32}`,
	)
	if !strings.Contains(out, "pairs first-party headers with the rc-dial client-hello fingerprint") {
		t.Fatalf("output missing rc-dial client-hello pairing line: %s", out)
	}
}

// TestCLIOpencodePresetEngineDefaultFingerprint proves the startup log states
// the engine-default fingerprint honestly when the opencode preset is paired
// with remote TLS termination but no client-hello spec is configured.
func TestCLIOpencodePresetEngineDefaultFingerprint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI acceptance test in short mode")
	}
	out, _ := runCLIStartup(t,
		"-bind", "127.0.0.1:1",
		"-upstream", "https://opencode.ai/zen",
		"-name", "zen",
		"-model-table", "m@zen=wire;via=messages",
		"-native-route", "messages@/v1/messages",
		"-opencode",
		"-rc-server", "127.0.0.1:9090",
		"-rc-terminate-tls",
	)
	if !strings.Contains(out, "keeps the engine-default tls fingerprint") {
		t.Fatalf("output missing engine-default fingerprint note: %s", out)
	}
}
