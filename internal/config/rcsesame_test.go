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

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveAndValidate_SesameEndpointRepeatResolve verifies that the
// endpoint-to-RC-channel resolution is idempotent: the first pass derives
// Provider.RCServer from -sesame-endpoint, and that derived value must not be
// mistaken for an explicit -rc-server by a later ResolveAndValidate call.
func TestResolveAndValidate_SesameEndpointRepeatResolve(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame",
		"-sesame-endpoint=https://127.0.0.1:50051",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	first := cfg.Providers[0].RCDialer()
	if first == nil {
		t.Fatal("first RCDialer() = nil, want non-nil")
	}
	t.Cleanup(func() { _ = first.Close() })

	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("second ResolveAndValidate: %v", err)
	}
	p := cfg.Providers[0]
	if p.RCServer != "127.0.0.1:50051" {
		t.Errorf("RCServer = %q, want 127.0.0.1:50051", p.RCServer)
	}
	if !p.RCChannelTLS {
		t.Error("RCChannelTLS = false, want true")
	}
	second := p.RCDialer()
	if second == nil {
		t.Fatal("second RCDialer() = nil, want non-nil")
	}
	if second == first {
		t.Fatal("second RCDialer() reused the first dialer, want a replacement")
	}
	t.Cleanup(func() { _ = second.Close() })
}

// TestResolveAndValidate_SesameEndpointExplicitRCServerConflict verifies that
// explicit -rc-server is rejected alongside -sesame-endpoint even when both
// resolve to the same dial target: provenance, not value equality, decides the
// conflict.
func TestResolveAndValidate_SesameEndpointExplicitRCServerConflict(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame",
		"-sesame-endpoint=https://127.0.0.1:50051",
		"-rc-server=127.0.0.1:50051",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = cfg.ResolveAndValidate()
	if err == nil || !strings.Contains(err.Error(), "-rc-server and -sesame-endpoint") {
		t.Fatalf("ResolveAndValidate = %v, want explicit dial-target conflict even when values match", err)
	}
}

// TestResolveAndValidate_SesameEndpointChangeAfterResolve verifies that a
// later ResolveAndValidate replaces a previously derived RC target instead of
// mistaking it for explicit -rc-server input.
func TestResolveAndValidate_SesameEndpointChangeAfterResolve(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame",
		"-sesame-endpoint=https://127.0.0.1:50051",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	first := cfg.Providers[0].RCDialer()
	if first == nil {
		t.Fatal("first RCDialer() = nil, want non-nil")
	}
	t.Cleanup(func() { _ = first.Close() })

	cfg.Providers[0].SesameEndpoint = "http://127.0.0.1:60051"
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("second ResolveAndValidate after endpoint change: %v", err)
	}
	p := cfg.Providers[0]
	if p.RCServer != "127.0.0.1:60051" {
		t.Errorf("RCServer = %q, want 127.0.0.1:60051", p.RCServer)
	}
	if p.RCChannelTLS {
		t.Error("RCChannelTLS = true, want false for http endpoint")
	}
	second := p.RCDialer()
	if second == nil {
		t.Fatal("second RCDialer() = nil, want non-nil")
	}
	if second == first {
		t.Fatal("second RCDialer() reused the first dialer, want a replacement")
	}
	t.Cleanup(func() { _ = second.Close() })
}

// TestResolveAndValidate_SesameMaterialRefreshAfterResolve verifies that a
// repeated resolution refreshes previously propagated sesame mTLS material:
// the derived RC channel fields follow the current -sesame-* values instead
// of being frozen by their first non-empty state.
func TestResolveAndValidate_SesameMaterialRefreshAfterResolve(t *testing.T) {
	caPEM, clientCertPEM, clientKeyPEM := generateTestCerts(t)
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	first := materialPaths(t, firstDir, caPEM, clientCertPEM, clientKeyPEM)
	second := materialPaths(t, secondDir, caPEM, clientCertPEM, clientKeyPEM)

	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame",
		"-sesame-endpoint=https://127.0.0.1:50051",
		"-sesame-ca-cert=" + first.ca,
		"-sesame-client-cert=" + first.cert,
		"-sesame-client-key=" + first.key,
		"-sesame-server-name=first.local",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	if err := cfg.Providers[0].RCDialer().Close(); err != nil {
		t.Fatal(err)
	}

	p := cfg.Providers[0]
	p.SesameCACert = second.ca
	p.SesameClientCert = second.cert
	p.SesameClientKey = second.key
	p.SesameServerName = "second.local"
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("second ResolveAndValidate after material change: %v", err)
	}
	if got, want := p.RCChannelCA, "file:"+second.ca; got != want {
		t.Errorf("RCChannelCA = %q, want %q", got, want)
	}
	if got, want := p.RCChannelCert, "file:"+second.cert; got != want {
		t.Errorf("RCChannelCert = %q, want %q", got, want)
	}
	if got, want := p.RCChannelKey, "file:"+second.key; got != want {
		t.Errorf("RCChannelKey = %q, want %q", got, want)
	}
	if got := p.RCChannelServerName; got != "second.local" {
		t.Errorf("RCChannelServerName = %q, want second.local", got)
	}
	if err := p.RCDialer().Close(); err != nil {
		t.Fatal(err)
	}
}

// TestResolveAndValidate_SesameMaterialRemovalAfterResolve verifies that
// removing sesame material also removes its previously derived channel
// fields. A stale CA/cert/key/SNI must not survive a later resolve.
func TestResolveAndValidate_SesameMaterialRemovalAfterResolve(t *testing.T) {
	caPEM, clientCertPEM, clientKeyPEM := generateTestCerts(t)
	material := materialPaths(t, t.TempDir(), caPEM, clientCertPEM, clientKeyPEM)

	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame",
		"-sesame-endpoint=https://127.0.0.1:50051",
		"-sesame-ca-cert=" + material.ca,
		"-sesame-client-cert=" + material.cert,
		"-sesame-client-key=" + material.key,
		"-sesame-server-name=localhost",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	if err := cfg.Providers[0].RCDialer().Close(); err != nil {
		t.Fatal(err)
	}

	p := cfg.Providers[0]
	p.SesameCACert = ""
	p.SesameClientCert = ""
	p.SesameClientKey = ""
	p.SesameServerName = ""
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("second ResolveAndValidate after material removal: %v", err)
	}
	if p.RCChannelCA != "" || p.RCChannelCert != "" || p.RCChannelKey != "" || p.RCChannelServerName != "" {
		t.Fatalf(
			"stale derived channel material after removal: CA=%q cert=%q key=%q server-name=%q",
			p.RCChannelCA, p.RCChannelCert, p.RCChannelKey, p.RCChannelServerName,
		)
	}
	if err := p.RCDialer().Close(); err != nil {
		t.Fatal(err)
	}
}

// TestResolveAndValidate_SesameMaterialExplicitChannelWins verifies that
// explicit -rc-channel-* values stay authoritative across repeated
// resolution: current sesame material may fill only the channel fields the
// operator did not set explicitly.
func TestResolveAndValidate_SesameMaterialExplicitChannelWins(t *testing.T) {
	caPEM, clientCertPEM, clientKeyPEM := generateTestCerts(t)
	first := materialPaths(t, t.TempDir(), caPEM, clientCertPEM, clientKeyPEM)
	second := materialPaths(t, t.TempDir(), caPEM, clientCertPEM, clientKeyPEM)

	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame",
		"-sesame-endpoint=https://127.0.0.1:50051",
		"-rc-channel-ca=file:" + first.ca,
		"-rc-channel-cert=file:" + first.cert,
		"-rc-channel-key=file:" + first.key,
		"-rc-channel-server-name=explicit.local",
		"-sesame-ca-cert=" + second.ca,
		"-sesame-client-cert=" + second.cert,
		"-sesame-client-key=" + second.key,
		"-sesame-server-name=implicit.local",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	if err := cfg.Providers[0].RCDialer().Close(); err != nil {
		t.Fatal(err)
	}

	p := cfg.Providers[0]
	p.SesameCACert = second.ca + "-changed"
	p.SesameServerName = "changed.local"
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("second ResolveAndValidate: %v", err)
	}
	if got, want := p.RCChannelCA, "file:"+first.ca; got != want {
		t.Errorf("RCChannelCA = %q, want explicit %q", got, want)
	}
	if got, want := p.RCChannelCert, "file:"+first.cert; got != want {
		t.Errorf("RCChannelCert = %q, want explicit %q", got, want)
	}
	if got, want := p.RCChannelKey, "file:"+first.key; got != want {
		t.Errorf("RCChannelKey = %q, want explicit %q", got, want)
	}
	if got := p.RCChannelServerName; got != "explicit.local" {
		t.Errorf("RCChannelServerName = %q, want explicit explicit.local", got)
	}
	if err := p.RCDialer().Close(); err != nil {
		t.Fatal(err)
	}
}

// TestResolveAndValidate_SesameEndpointMalformedAfterResolve verifies that a
// failed endpoint revalidation clears the target derived by the previous
// successful resolve and releases the channel it opened, instead of leaving
// stale state or a live dialer on the failed Config.
func TestResolveAndValidate_SesameEndpointMalformedAfterResolve(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame",
		"-sesame-endpoint=https://127.0.0.1:50051",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	first := cfg.Providers[0].RCDialer()
	if first == nil {
		t.Fatal("first RCDialer() = nil, want non-nil")
	}
	t.Cleanup(func() { _ = first.Close() })

	cfg.Providers[0].SesameEndpoint = "grpc://127.0.0.1:50051"
	if err := cfg.ResolveAndValidate(); err == nil || !strings.Contains(err.Error(), "invalid -sesame-endpoint") {
		t.Fatalf("second ResolveAndValidate = %v, want invalid -sesame-endpoint error", err)
	}
	if cfg.Providers[0].RCServer != "" {
		t.Fatalf("RCServer after failed revalidation = %q, want empty", cfg.Providers[0].RCServer)
	}
	// A failed resolve owns nothing: the channel installed by the earlier
	// success is released, so no caller can inherit a live gRPC channel
	// alongside an error (main.go registers its close defer only after a
	// successful resolve).
	if got := cfg.Providers[0].RCDialer(); got != nil {
		t.Fatalf("RCDialer() after failed revalidation = %v, want nil (no open channel)", got)
	}
}

// TestResolveAndValidate_SesameEndpointRemovalAfterResolve verifies that
// removing -sesame-endpoint removes its derived dial target and returns the
// config to the explicitly configured RC state.
func TestResolveAndValidate_SesameEndpointRemovalAfterResolve(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame",
		"-sesame-endpoint=https://127.0.0.1:50051",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	first := cfg.Providers[0].RCDialer()
	if first == nil {
		t.Fatal("first RCDialer() = nil, want non-nil")
	}
	t.Cleanup(func() { _ = first.Close() })

	cfg.Providers[0].SesameEndpoint = ""
	if err := cfg.ResolveAndValidate(); err == nil || !strings.Contains(err.Error(), "transport sesame requires") {
		t.Fatalf("second ResolveAndValidate = %v, want transport sesame requires error", err)
	}
	if cfg.Providers[0].RCServer != "" {
		t.Fatalf("RCServer after endpoint removal = %q, want empty", cfg.Providers[0].RCServer)
	}
}

// TestResolveAndValidate_SesameEndpointRemovalReturnsToDirect verifies that
// removing a sesame endpoint and selecting direct (or default) transport
// removes every endpoint-derived dial target and dialer instead of leaving
// the previous resolution's state behind.
func TestResolveAndValidate_SesameEndpointRemovalReturnsToDirect(t *testing.T) {
	for name, transport := range map[string]string{
		"direct":  "direct",
		"default": "",
	} {
		cfg, err := Parse([]string{
			"-upstream=https://api.example.com",
			"-transport=sesame",
			"-sesame-endpoint=https://127.0.0.1:50051",
		})
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		if err := cfg.ResolveAndValidate(); err != nil {
			t.Fatalf("%s: first ResolveAndValidate: %v", name, err)
		}
		first := cfg.Providers[0].RCDialer()
		if first == nil {
			t.Fatalf("%s: first RCDialer() = nil, want non-nil", name)
		}
		t.Cleanup(func() { _ = first.Close() })

		p := cfg.Providers[0]
		p.Transport = transport
		p.SesameEndpoint = ""
		if err := cfg.ResolveAndValidate(); err != nil {
			t.Fatalf("%s: second ResolveAndValidate: %v", name, err)
		}
		if p.RCServer != "" {
			t.Errorf("%s: RCServer = %q, want empty", name, p.RCServer)
		}
		if p.RCChannelTLS {
			t.Errorf("%s: RCChannelTLS = true, want false after endpoint removal", name)
		}
		if got := p.RCDialer(); got != nil {
			t.Errorf("%s: RCDialer() = %v, want nil", name, got)
			_ = got.Close()
		}
	}
}

// TestResolveAndValidate_SesameEndpointRemovalExplicitChannelTLSStays verifies
// that an explicit -rc-channel-tls choice remains authoritative after the
// sesame endpoint is removed: direct transport still rejects it as an RC dial
// option rather than treating the stale choice as derived endpoint state.
func TestResolveAndValidate_SesameEndpointRemovalExplicitChannelTLSStays(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame",
		"-sesame-endpoint=https://127.0.0.1:50051",
		"-rc-channel-tls=true",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	if err := cfg.Providers[0].RCDialer().Close(); err != nil {
		t.Fatal(err)
	}

	p := cfg.Providers[0]
	p.Transport = "direct"
	p.SesameEndpoint = ""
	if err := cfg.ResolveAndValidate(); err == nil || !strings.Contains(err.Error(), "transport direct") {
		t.Fatalf("second ResolveAndValidate = %v, want explicit -rc-channel-tls to conflict with direct transport", err)
	}
	if !p.RCChannelTLS {
		t.Fatal("RCChannelTLS = false, want explicit true preserved")
	}
}

// materialPaths writes one generated certificate set to dir and returns the
// file paths, keeping sesame material tests focused on resolution behavior.
type materialPathsResult struct {
	ca, cert, key string
}

func materialPaths(t *testing.T, dir string, caPEM, clientCertPEM, clientKeyPEM []byte) materialPathsResult {
	t.Helper()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return materialPathsResult{
		ca:   write("ca.crt", caPEM),
		cert: write("client.crt", clientCertPEM),
		key:  write("client.key", clientKeyPEM),
	}
}

// TestResolveAndValidate_SesameEndpointAndTransport tests parsing and resolution
// of -transport and -sesame-* flags onto the underlying RC channel.
func TestResolveAndValidate_SesameEndpointAndTransport(t *testing.T) {
	caPEM, clientCertPEM, clientKeyPEM := generateTestCerts(t)
	certFile := filepath.Join(t.TempDir(), "client.crt")
	keyFile := filepath.Join(t.TempDir(), "client.key")
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(certFile, clientCertPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, clientKeyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, caPEM, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame",
		"-sesame-endpoint=https://127.0.0.1:50051",
		"-sesame-ca-cert=" + caFile,
		"-sesame-client-cert=" + certFile,
		"-sesame-client-key=" + keyFile,
		"-sesame-server-name=localhost",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	p := cfg.Providers[0]
	if p.RCServer != "127.0.0.1:50051" {
		t.Errorf("RCServer = %q, want 127.0.0.1:50051", p.RCServer)
	}
	if !p.RCChannelTLS {
		t.Error("RCChannelTLS = false, want true")
	}
	if p.RCChannelServerName != "localhost" {
		t.Errorf("RCChannelServerName = %q, want localhost", p.RCChannelServerName)
	}
	if p.RCChannelCA != "file:"+caFile {
		t.Errorf("RCChannelCA = %q, want file:%s", p.RCChannelCA, caFile)
	}
	if p.RCDialer() == nil {
		t.Fatal("RCDialer() = nil, want non-nil dialer")
	}

	// Invalid transport
	cfgBad, err := Parse([]string{"-upstream=https://api.example.com", "-transport=invalid"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfgBad.ResolveAndValidate(); err == nil || !strings.Contains(err.Error(), "invalid -transport") {
		t.Errorf("ResolveAndValidate want invalid -transport error, got %v", err)
	}

	// Transport sesame without endpoint
	cfgNoEp, err := Parse([]string{"-upstream=https://api.example.com", "-transport=sesame"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfgNoEp.ResolveAndValidate(); err == nil || !strings.Contains(err.Error(), "transport sesame requires") {
		t.Errorf("ResolveAndValidate want transport sesame requires error, got %v", err)
	}
}

// TestResolveAndValidate_TransportDirectRejectsDialOptions verifies that
// -transport=direct is an enforced mode rather than a no-op: any
// remote-control or sesame dial configuration alongside it is rejected at
// startup instead of silently dialing through RemoteControl, and a direct
// transport with no dial options stays valid.
func TestResolveAndValidate_TransportDirectRejectsDialOptions(t *testing.T) {
	for name, args := range map[string][]string{
		"sesame endpoint": {"-transport=direct", "-sesame-endpoint=https://127.0.0.1:50051"},
		"rc server":       {"-transport=direct", "-rc-server=127.0.0.1:50051"},
		"sesame token":    {"-transport=direct", "-sesame-token-env=SHAPER_TEST_SESAME_TOKEN"},
		"sesame ca cert":  {"-transport=direct", "-sesame-ca-cert=/tmp/ca.crt"},
		"rc channel tls":  {"-transport=direct", "-rc-channel-tls"},
	} {
		cfg, err := Parse(append([]string{"-upstream=https://api.example.com"}, args...))
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		err = cfg.ResolveAndValidate()
		if err == nil {
			t.Errorf("%s: ResolveAndValidate: nil error, want transport direct rejection", name)
			continue
		}
		if !strings.Contains(err.Error(), "transport direct") {
			t.Errorf("%s: error = %q, want transport direct rejection", name, err)
		}
	}

	cfg, err := Parse([]string{"-upstream=https://api.example.com", "-transport=direct"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Errorf("plain -transport=direct: ResolveAndValidate: %v", err)
	}
}

// TestResolveAndValidate_SesameEndpointValidation verifies that
// -sesame-endpoint is parsed as a dial target at startup: only http(s) URLs
// or bare host[:port] values are accepted, and malformed shapes are rejected
// with errors naming the flag instead of surfacing as request-time dial
// failures or misleading rc-option errors.
func TestResolveAndValidate_SesameEndpointValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		args    []string
		wantErr string
		wantRC  string
		wantTLS bool
	}{
		"scheme only":        {args: []string{"-sesame-endpoint=https://"}, wantErr: "invalid -sesame-endpoint"},
		"path in url":        {args: []string{"-sesame-endpoint=https://127.0.0.1:50051/path"}, wantErr: "invalid -sesame-endpoint"},
		"query in url":       {args: []string{"-sesame-endpoint=https://127.0.0.1:50051?x=1"}, wantErr: "invalid -sesame-endpoint"},
		"userinfo in url":    {args: []string{"-sesame-endpoint=https://u@127.0.0.1:50051"}, wantErr: "invalid -sesame-endpoint"},
		"unsupported scheme": {args: []string{"-sesame-endpoint=grpc://127.0.0.1:50051"}, wantErr: "invalid -sesame-endpoint"},
		"bare with slash":    {args: []string{"-sesame-endpoint=127.0.0.1:50051/x"}, wantErr: "invalid -sesame-endpoint"},
		"bare with query":    {args: []string{"-sesame-endpoint=127.0.0.1:50051?x"}, wantErr: "invalid -sesame-endpoint"},
		"both dial targets": {
			args:    []string{"-sesame-endpoint=https://127.0.0.1:50051", "-rc-server=127.0.0.1:60051"},
			wantErr: "only one",
		},
		"http with explicit tls": {
			args:    []string{"-sesame-endpoint=http://127.0.0.1:50051", "-rc-channel-tls=true"},
			wantErr: "conflicts",
		},
		"https with explicit no tls": {
			args:    []string{"-sesame-endpoint=https://127.0.0.1:50051", "-rc-channel-tls=false"},
			wantErr: "conflicts",
		},
		"bare with explicit no tls": {
			args:    []string{"-sesame-endpoint=127.0.0.1:50051", "-rc-channel-tls=false"},
			wantErr: "conflicts",
		},
		"https uppercase": {
			args:    []string{"-sesame-endpoint=HTTPS://127.0.0.1:50051"},
			wantRC:  "127.0.0.1:50051",
			wantTLS: true,
		},
		"https trailing slash": {
			args:    []string{"-sesame-endpoint=https://127.0.0.1:50051/"},
			wantRC:  "127.0.0.1:50051",
			wantTLS: true,
		},
		"bare host port": {
			args:    []string{"-sesame-endpoint=127.0.0.1:50051"},
			wantRC:  "127.0.0.1:50051",
			wantTLS: true,
		},
		"http endpoint": {
			args:    []string{"-sesame-endpoint=http://127.0.0.1:50051"},
			wantRC:  "127.0.0.1:50051",
			wantTLS: false,
		},
	} {
		cfg, err := Parse(append([]string{"-upstream=https://api.example.com"}, tc.args...))
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		err = cfg.ResolveAndValidate()
		if tc.wantErr != "" {
			if err == nil {
				t.Errorf("%s: ResolveAndValidate: nil error, want %q", name, tc.wantErr)
				continue
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: error = %q, want %q", name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: ResolveAndValidate: %v", name, err)
			continue
		}
		p := cfg.Providers[0]
		if p.RCServer != tc.wantRC {
			t.Errorf("%s: RCServer = %q, want %q", name, p.RCServer, tc.wantRC)
		}
		if p.RCChannelTLS != tc.wantTLS {
			t.Errorf("%s: RCChannelTLS = %v, want %v", name, p.RCChannelTLS, tc.wantTLS)
		}
	}
}

// TestResolveAndValidate_SesameMaterialWithRCServer verifies that the
// -sesame-* mTLS material applies to the RC channel even when the dial
// target comes from -rc-server instead of -sesame-endpoint: gating the
// propagation on -sesame-endpoint silently dropped it.
func TestResolveAndValidate_SesameMaterialWithRCServer(t *testing.T) {
	caPEM, clientCertPEM, clientKeyPEM := generateTestCerts(t)
	certFile := filepath.Join(t.TempDir(), "client.crt")
	keyFile := filepath.Join(t.TempDir(), "client.key")
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(certFile, clientCertPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, clientKeyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, caPEM, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame",
		"-rc-server=127.0.0.1:50051",
		"-rc-channel-tls",
		"-sesame-ca-cert=" + caFile,
		"-sesame-client-cert=" + certFile,
		"-sesame-client-key=" + keyFile,
		"-sesame-server-name=localhost",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	p := cfg.Providers[0]
	if p.RCChannelCA != "file:"+caFile {
		t.Errorf("RCChannelCA = %q, want file:%s", p.RCChannelCA, caFile)
	}
	if p.RCChannelCert != "file:"+certFile {
		t.Errorf("RCChannelCert = %q, want file:%s", p.RCChannelCert, certFile)
	}
	if p.RCChannelKey != "file:"+keyFile {
		t.Errorf("RCChannelKey = %q, want file:%s", p.RCChannelKey, keyFile)
	}
	if p.RCChannelServerName != "localhost" {
		t.Errorf("RCChannelServerName = %q, want localhost", p.RCChannelServerName)
	}
	if p.RCDialer() == nil {
		t.Fatal("RCDialer() = nil, want non-nil dialer")
	}
}

// TestResolveAndValidate_SesameTokenEnv verifies that -sesame-token-env
// resolves the named environment variable once at startup (fail-closed when
// unset or empty, never logging the value) and that the credential requires
// a TLS channel so it can never ride a plaintext connection.
func TestResolveAndValidate_SesameTokenEnv(t *testing.T) {
	const envVar = "SHAPER_TEST_SESAME_TOKEN"

	// Grammar: an invalid variable name is a usage error before any read.
	cfgBad, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame", "-sesame-endpoint=https://127.0.0.1:50051",
		"-sesame-token-env=9BAD",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfgBad.ResolveAndValidate(); err == nil || !strings.Contains(err.Error(), "environment variable name") {
		t.Errorf("ResolveAndValidate want env-var-name error, got %v", err)
	}

	// Unset variable: fail closed, naming the variable (never the value).
	t.Setenv(envVar, "")
	cfgUnset, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame", "-sesame-endpoint=https://127.0.0.1:50051",
		"-sesame-token-env=" + envVar,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = cfgUnset.ResolveAndValidate()
	if err == nil || !strings.Contains(err.Error(), envVar) {
		t.Errorf("ResolveAndValidate want error naming %s, got %v", envVar, err)
	}

	// Plaint sesame channel: the credential would ride plaintext; reject.
	t.Setenv(envVar, "test-token")
	cfgPlain, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame", "-sesame-endpoint=http://127.0.0.1:50051",
		"-sesame-token-env=" + envVar,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfgPlain.ResolveAndValidate(); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Errorf("ResolveAndValidate want token-requires-TLS error, got %v", err)
	}

	// Happy path: token resolved once and held for the channel credential.
	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame", "-sesame-endpoint=https://127.0.0.1:50051",
		"-sesame-token-env=" + envVar,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	if got := cfg.Providers[0].rcToken; got != "test-token" {
		t.Errorf("rcToken = %q, want test-token", got)
	}
	if cfg.Providers[0].RCDialer() == nil {
		t.Fatal("RCDialer() = nil, want non-nil dialer")
	}

	// A token without any dial target stays an rc dial-option error.
	cfgStray, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-sesame-token-env=" + envVar,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfgStray.ResolveAndValidate(); err == nil || !strings.Contains(err.Error(), "rc dial options require -rc-server") {
		t.Errorf("ResolveAndValidate want rc dial options error, got %v", err)
	}
}

// TestResolveAndValidate_SesameEndpointRejectsMalformedHostsAndNeverEchoes
// pins two contracts for the bare (scheme-less) endpoint form: a userinfo,
// empty host, missing port, or scheme-typo value is refused at startup, and no
// endpoint error ever contains a byte of the value — the endpoint is logged
// verbatim at startup, so an error that echoed it would publish userinfo into
// the journal.
func TestResolveAndValidate_SesameEndpointRejectsMalformedHostsAndNeverEchoes(t *testing.T) {
	const secret = "s3cret-userinfo-value"
	for name, tc := range map[string]struct {
		endpoint string
		wantErr  string
	}{
		"userinfo in bare form": {endpoint: secret + "@127.0.0.1:50051", wantErr: "without userinfo"},
		"userinfo in url form":  {endpoint: "https://" + secret + "@127.0.0.1:50051", wantErr: "must not contain userinfo"},
		"empty host":            {endpoint: ":8443", wantErr: "missing host"},
		"scheme typo":           {endpoint: "http:host:8080", wantErr: "malformed host:port"},
		"whitespace only":       {endpoint: "   ", wantErr: "missing host"},
	} {
		cfg, err := Parse([]string{
			"-upstream=https://api.example.com",
			"-sesame-endpoint=" + tc.endpoint,
		})
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		err = cfg.ResolveAndValidate()
		if err == nil {
			t.Errorf("%s: ResolveAndValidate = nil, want %q", name, tc.wantErr)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, tc.wantErr)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error leaks the endpoint userinfo: %v", name, err)
		}
	}

	// Surrounding whitespace is trimmed rather than producing a target that
	// main.go would log with the spaces.
	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-sesame-endpoint= 127.0.0.1:50051 ",
	})
	if err != nil {
		t.Fatalf("Parse padded endpoint: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate padded endpoint: %v", err)
	}
	if got := cfg.Providers[0].RCServer; got != "127.0.0.1:50051" {
		t.Fatalf("RCServer = %q, want the trimmed 127.0.0.1:50051", got)
	}
}

// TestResolveAndValidate_TokenDoesNotSurviveRemoval pins the credential
// hygiene of repeated resolution: a token resolved from -sesame-token-env must
// not survive a later resolve whose inputs no longer select that variable.
// Before the fix the stale token stayed on the provider and was attached to
// the next channel (or failed every RPC once that channel was plaintext).
func TestResolveAndValidate_TokenDoesNotSurviveRemoval(t *testing.T) {
	const envVar = "SHAPER_TEST_SESAME_TOKEN_REMOVAL"
	t.Setenv(envVar, "first-token")
	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-transport=sesame",
		"-sesame-endpoint=https://127.0.0.1:50051",
		"-sesame-token-env=" + envVar,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	p := cfg.Providers[0]
	if p.rcToken != "first-token" {
		t.Fatalf("rcToken = %q, want first-token", p.rcToken)
	}

	// The operator drops the token flag but keeps the (now plaintext) channel.
	p.SesameTokenEnv = ""
	p.SesameEndpoint = "http://127.0.0.1:50051"
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("second ResolveAndValidate: %v", err)
	}
	if p.rcToken != "" {
		t.Fatalf("rcToken after removal = %q, want empty (a stale secret must not be attached)", p.rcToken)
	}
}

// TestResolveAndValidate_ServerConfigClearedOnBindRemoval pins that removing
// -rc-server-bind clears the resolved server configuration: main.go starts the
// embedded RC server from RCServerConfig, so a stale value would start a
// listener the current inputs do not ask for.
func TestResolveAndValidate_ServerConfigClearedOnBindRemoval(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream=https://x",
		"-rc-server-bind", "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	if cfg.Server.RCServerConfig() == nil {
		t.Fatal("RCServerConfig() = nil after a resolve with -rc-server-bind, want the config")
	}
	cfg.Server.RCBind = ""
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("second ResolveAndValidate: %v", err)
	}
	if got := cfg.Server.RCServerConfig(); got != nil {
		t.Fatalf("RCServerConfig() = %+v after bind removal, want nil", got)
	}
}

// TestResolveAndValidate_ProgrammaticProviderKeepsDialFields pins the
// programmatic-construction contract: a Provider that was never built by Parse
// carries no explicit-flag record, and the values its caller set directly must
// survive ResolveAndValidate instead of being wiped as if they had been derived
// from a flag that is no longer present.
func TestResolveAndValidate_ProgrammaticProviderKeepsDialFields(t *testing.T) {
	cfg, err := Parse([]string{"-upstream", "https://x"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := cfg.Providers[0]
	p.RCServer = "127.0.0.1:9090"
	p.RCChannelTLS = true
	p.RCChannelServerName = "rc.local"

	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	if p.RCServer != "127.0.0.1:9090" {
		t.Errorf("RCServer = %q, want the caller's 127.0.0.1:9090", p.RCServer)
	}
	if !p.RCChannelTLS {
		t.Error("RCChannelTLS = false, want the caller's true")
	}
	if p.RCChannelServerName != "rc.local" {
		t.Errorf("RCChannelServerName = %q, want the caller's rc.local", p.RCChannelServerName)
	}
	if p.RCDialer() == nil {
		t.Error("RCDialer() = nil, want a dialer for the caller's target")
	} else {
		_ = p.RCDialer().Close()
	}
}

// TestResolveAndValidate_FailureReleasesDialers pins that a resolve which
// fails after some providers succeeded leaves no open gRPC channel behind:
// main.go registers its close defer only after a successful resolve, so a
// failed resolve would otherwise leak every channel it had opened.
func TestResolveAndValidate_FailureReleasesDialers(t *testing.T) {
	cfg, err := Parse([]string{
		"--provider=acme",
		"-upstream", "https://api.anthropic.com",
		"-prefix", "/acme",
		"-rc-server", "127.0.0.1:9090",
		"--provider=beta",
		"-upstream", "https://api.openai.com",
		"-prefix", "/beta",
		"-rc-server", "127.0.0.1:9091",
		// An impossible version range fails the second provider's resolve,
		// after the first provider's channel is already open.
		"-rc-terminate-tls",
		"-rc-target-min-version", "1.3",
		"-rc-target-max-version", "1.2",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err == nil {
		t.Fatal("ResolveAndValidate = nil, want the version-range failure")
	}
	for i, p := range cfg.Providers {
		if d := p.RCDialer(); d != nil {
			t.Errorf("provider %d RCDialer() = %v after a failed resolve, want nil", i+1, d)
			_ = d.Close()
		}
	}
}
