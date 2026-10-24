package main

// Upstream-trust CLI acceptance: the local-handshake trust flags clear
// startup validation and log their trust line, while the meaningless
// combination with remote termination fails before any traffic is served.

import (
	"os"
	"strings"
	"testing"
)

// TestCLIUpstreamTrustPasses proves -upstream-ca clears configuration (only
// the unreachable bind may fail) and the startup log names the trust hook
// without emitting material. The CA is generated at runtime: no committed
// fixture.
func TestCLIUpstreamTrustPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	pki := generateTestPKI(t)
	caPath, _, _, _, _ := pki.writeFiles(t)
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	out, err := runCLIStartup(t,
		"-bind", "127.0.0.1:1",
		"-upstream", "https://127.0.0.1:1",
		"-upstream-ca", "file:"+caPath,
	)
	if err == nil {
		t.Fatalf("want bind failure, got success: %s", out)
	}
	if !strings.Contains(out, "upstream-tls: trust=custom-ca insecure=false server-name=") {
		t.Fatalf("output missing upstream-tls trust line: %s", out)
	}
	// No line of the CA material may reach the startup output.
	for line := range strings.Lines(strings.TrimSpace(string(caPEM))) {
		if line != "" && strings.Contains(out, line) {
			t.Fatalf("output echoes a line of the CA material (%q): %s", line, out)
		}
	}
}

// TestCLIRejectsUpstreamTrustWithTerminate proves the local-handshake trust
// flags fail at startup under -rc-terminate-tls, where the proxy never
// handshakes and would otherwise ignore them silently.
func TestCLIRejectsUpstreamTrustWithTerminate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	for _, args := range [][]string{
		{"-upstream-ca", "file:/tmp/definitely-absent-ca.pem"},
		{"-upstream-insecure"},
		{"-upstream-server-name", "upstream.local"},
	} {
		out, err := runCLIStartup(t, append([]string{
			"-bind", "127.0.0.1:1",
			"-upstream", "https://127.0.0.1:1",
			"-rc-server", "127.0.0.1:9090",
			"-rc-terminate-tls",
		}, args...)...)
		if err == nil {
			t.Fatalf("%q: want startup failure, got success: %s", args, out)
		}
		if !strings.Contains(out, "upstream TLS trust options") {
			t.Fatalf("%q: output = %s, want the trust-options rejection", args, out)
		}
	}
}
