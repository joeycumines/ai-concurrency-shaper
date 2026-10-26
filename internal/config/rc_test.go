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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestParse_RCFlags_LegacyMode binds every provider-scope RC dial flag at top
// level (the single implicit provider), exactly like every other
// provider-scope flag. It checks binding only: the values are not resolved, so
// they need not be ones a resolve would accept.
func TestParse_RCFlags_LegacyMode(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-server", "127.0.0.1:9090",
		"-rc-channel-tls",
		"-rc-channel-server-name", "rc.local",
		"-rc-dial-timeout", "5s",
		"-rc-terminate-tls",
		"-rc-target-server-name", "api.example.com",
		"-rc-target-alpn", "http/1.1",
		"-rc-target-alpn", "h2",
		"-rc-target-min-version", "1.2",
		"-rc-target-max-version", "1.3",
		"-rc-target-client-hello", `{"sessionIdLength":32}`,
		"-rc-target-insecure",
		"-rc-proxy-hop", "http-connect@proxy.local:8080;user=u;pass=p",
		"-rc-proxy-hop", "socks5@10.0.0.1:1080",
		"-rc-flow-control",
		"-rc-opportunistic-tls",
		"-rc-window-size", "65536",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("len(Providers) = %d, want 1", len(cfg.Providers))
	}
	p := cfg.Providers[0]
	if p.RCServer != "127.0.0.1:9090" {
		t.Errorf("RCServer = %q, want 127.0.0.1:9090", p.RCServer)
	}
	if !p.RCChannelTLS {
		t.Error("RCChannelTLS = false, want true")
	}
	if p.RCChannelServerName != "rc.local" {
		t.Errorf("RCChannelServerName = %q, want rc.local", p.RCChannelServerName)
	}
	if p.RCDialTimeout != 5*time.Second {
		t.Errorf("RCDialTimeout = %s, want 5s", p.RCDialTimeout)
	}
	if !p.RCTerminateTLS {
		t.Error("RCTerminateTLS = false, want true")
	}
	if p.RCTargetServerName != "api.example.com" {
		t.Errorf("RCTargetServerName = %q, want api.example.com", p.RCTargetServerName)
	}
	wantALPN := []string{"http/1.1", "h2"}
	if !reflect.DeepEqual(p.RCTargetALPN, wantALPN) {
		t.Errorf("RCTargetALPN = %q, want %q", p.RCTargetALPN, wantALPN)
	}
	if p.RCTargetMinVersion != "1.2" {
		t.Errorf("RCTargetMinVersion = %q, want 1.2", p.RCTargetMinVersion)
	}
	if p.RCTargetMaxVersion != "1.3" {
		t.Errorf("RCTargetMaxVersion = %q, want 1.3", p.RCTargetMaxVersion)
	}
	if p.RCTargetClientHello != `{"sessionIdLength":32}` {
		t.Errorf("RCTargetClientHello = %q, want `{\"sessionIdLength\":32}`", p.RCTargetClientHello)
	}
	if !p.RCTargetInsecure {
		t.Error("RCTargetInsecure = false, want true")
	}
	wantHops := []string{
		"http-connect@proxy.local:8080;user=u;pass=p",
		"socks5@10.0.0.1:1080",
	}
	if !reflect.DeepEqual(p.RCProxyHop, wantHops) {
		t.Errorf("RCProxyHop = %q, want %q", p.RCProxyHop, wantHops)
	}
	if !p.RCFlowControl {
		t.Error("RCFlowControl = false, want true")
	}
	if !p.RCOpportunisticTLS {
		t.Error("RCOpportunisticTLS = false, want true")
	}
	if p.RCWindowSize != 65536 {
		t.Errorf("RCWindowSize = %d, want 65536", p.RCWindowSize)
	}
}

// TestParse_RCFlags_SectionedMode checks the RC dial flags are provider-scoped:
// legal inside a --provider section, and each provider keeps its own values.
func TestParse_RCFlags_SectionedMode(t *testing.T) {
	cfg, err := Parse([]string{
		"--provider=acme",
		"-upstream", "https://acme.example.com",
		"-prefix", "/acme",
		"-rc-server", "127.0.0.1:9090",
		"-rc-terminate-tls",
		"--provider=beta",
		"-upstream", "https://beta.example.com",
		"-prefix", "/beta",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("len(Providers) = %d, want 2", len(cfg.Providers))
	}
	if cfg.Providers[0].RCServer != "127.0.0.1:9090" {
		t.Errorf("acme RCServer = %q, want 127.0.0.1:9090", cfg.Providers[0].RCServer)
	}
	if !cfg.Providers[0].RCTerminateTLS {
		t.Error("acme RCTerminateTLS = false, want true")
	}
	if cfg.Providers[1].RCServer != "" {
		t.Errorf("beta RCServer = %q, want empty (flags must not leak between sections)", cfg.Providers[1].RCServer)
	}
}

// TestParse_RCFlags_ScopeError rejects provider-scope RC flags at server scope
// in sectioned mode (mixed mode stays forbidden).
func TestParse_RCFlags_ScopeError(t *testing.T) {
	_, err := Parse([]string{
		"-rc-server", "127.0.0.1:9090",
		"--provider=acme",
		"-upstream", "https://x",
		"-prefix", "/acme",
	})
	if !errors.Is(err, ErrUsage) {
		t.Errorf("err = %v, want ErrUsage", err)
	}
}

// TestParse_RCServerFlags binds the embedded RC server flags at server scope in
// both modes.
func TestParse_RCServerFlags(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-server-bind", "127.0.0.1:9191",
		"-rc-server-tls",
		"-rc-server-cert", "file:/tmp/cert.pem",
		"-rc-server-key", "file:/tmp/key.pem",
	})
	if err != nil {
		t.Fatalf("Parse legacy: %v", err)
	}
	if cfg.Server.RCBind != "127.0.0.1:9191" {
		t.Errorf("Server.RCBind = %q, want 127.0.0.1:9191", cfg.Server.RCBind)
	}
	if !cfg.Server.RCTLS {
		t.Error("Server.RCTLS = false, want true")
	}
	if cfg.Server.RCCert != "file:/tmp/cert.pem" {
		t.Errorf("Server.RCCert = %q, want file:/tmp/cert.pem", cfg.Server.RCCert)
	}
	if cfg.Server.RCKey != "file:/tmp/key.pem" {
		t.Errorf("Server.RCKey = %q, want file:/tmp/key.pem", cfg.Server.RCKey)
	}

	// Sectioned mode: server-scope RC flags bind on the Server.
	cfg2, err := Parse([]string{
		"-rc-server-bind", "127.0.0.1:9191",
		"--provider=acme",
		"-upstream", "https://x",
		"-prefix", "/acme",
	})
	if err != nil {
		t.Fatalf("Parse sectioned: %v", err)
	}
	if cfg2.Server.RCBind != "127.0.0.1:9191" {
		t.Errorf("Server.RCBind = %q, want 127.0.0.1:9191", cfg2.Server.RCBind)
	}
}

// TestResolveAndValidate_RCValid resolves a minimal valid RC dial
// configuration into a non-nil dialer.
func TestResolveAndValidate_RCValid(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-server", "127.0.0.1:9090",
		"-rc-dial-timeout", "5s",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	d := cfg.Providers[0].RCDialer()
	if d == nil {
		t.Fatal("RCDialer() = nil, want non-nil")
	}
	t.Cleanup(func() { _ = d.Close() })
	if d.RemoteTerminate() {
		t.Error("RemoteTerminate() = true, want false")
	}
}

// TestResolveAndValidate_RCRemoteTerminate confirms -rc-terminate-tls flips
// the dialer's RemoteTerminate bit.
func TestResolveAndValidate_RCRemoteTerminate(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-server", "127.0.0.1:9090",
		"-rc-terminate-tls",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	d := cfg.Providers[0].RCDialer()
	if d == nil {
		t.Fatal("RCDialer() = nil, want non-nil")
	}
	t.Cleanup(func() { _ = d.Close() })
	if !d.RemoteTerminate() {
		t.Error("RemoteTerminate() = false, want true")
	}
}

// TestResolveAndValidate_RCChannelTLS resolves a TLS channel without material
// (system roots, no client cert).
func TestResolveAndValidate_RCChannelTLS(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-server", "127.0.0.1:9090",
		"-rc-channel-tls",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	d := cfg.Providers[0].RCDialer()
	if d == nil {
		t.Fatal("RCDialer() = nil, want non-nil")
	}
	t.Cleanup(func() { _ = d.Close() })
}

// TestResolveAndValidate_RCClientHello pins the resolve-time acceptance of the
// client-hello flag: a valid spec resolves into the dialer, and an
// out-of-range spec fails closed at config time.
func TestResolveAndValidate_RCClientHello(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-server", "127.0.0.1:9090",
		"-rc-terminate-tls",
		"-rc-target-client-hello", `{"sessionIdLength":32}`,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	d := cfg.Providers[0].RCDialer()
	if d == nil {
		t.Fatal("RCDialer() = nil, want non-nil")
	}
	t.Cleanup(func() { _ = d.Close() })
	if !d.RemoteTerminate() {
		t.Error("RemoteTerminate() = false, want true")
	}

	bad, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-server", "127.0.0.1:9090",
		"-rc-terminate-tls",
		"-rc-target-client-hello", `{"sessionIdLength":99}`,
	})
	if err != nil {
		t.Fatalf("Parse out-of-range: %v", err)
	}
	err = bad.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate with out-of-range spec: nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "client hello spec") {
		t.Errorf("error = %q, want 'client hello spec'", err)
	}
}

// TestResolveAndValidate_RCDisabled confirms the zero configuration leaves RC
// dialing off (nil dialer).
func TestResolveAndValidate_RCDisabled(t *testing.T) {
	cfg, err := Parse([]string{"-upstream", "https://x"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	if d := cfg.Providers[0].RCDialer(); d != nil {
		t.Errorf("RCDialer() = %v, want nil", d)
		_ = d.Close()
	}
}

// TestResolveAndValidate_RCFlagWithoutServer rejects RC dial options without
// -rc-server.
func TestResolveAndValidate_RCFlagWithoutServer(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-terminate-tls",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want rc dial options require -rc-server")
	}
	if !strings.Contains(err.Error(), "rc dial options require -rc-server") {
		t.Errorf("error = %q, want 'rc dial options require -rc-server'", err)
	}
}

// TestResolveAndValidate_RCTargetWithoutTerminate rejects target TLS options
// without -rc-terminate-tls (rcclient fail-closed validation).
func TestResolveAndValidate_RCTargetWithoutTerminate(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-server", "127.0.0.1:9090",
		"-rc-target-insecure",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want target TLS options require remote TLS termination")
	}
	if !strings.Contains(err.Error(), "target TLS options require remote TLS termination") {
		t.Errorf("error = %q, want 'target TLS options require remote TLS termination'", err)
	}
}

// TestResolveAndValidate_RCWindowWithoutFlowControl rejects a window size
// without flow control.
func TestResolveAndValidate_RCWindowWithoutFlowControl(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-server", "127.0.0.1:9090",
		"-rc-window-size", "65536",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want window size requires flow control")
	}
	if !strings.Contains(err.Error(), "window size requires flow control") {
		t.Errorf("error = %q, want 'window size requires flow control'", err)
	}
}

// TestResolveAndValidate_RCServerFlagWithoutBind rejects embedded RC server
// options without -rc-server-bind.
func TestResolveAndValidate_RCServerFlagWithoutBind(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-server-tls",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want rc-server TLS options require -rc-server-bind")
	}
	if !strings.Contains(err.Error(), "rc-server TLS options require -rc-server-bind") {
		t.Errorf("error = %q, want 'rc-server TLS options require -rc-server-bind'", err)
	}
}

// TestResolveAndValidate_RCServerMissingCert rejects TLS without both cert and
// key.
func TestResolveAndValidate_RCServerMissingCert(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-server-bind", "127.0.0.1:9191",
		"-rc-server-tls",
		"-rc-server-cert", "file:/tmp/cert.pem",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want server TLS requires both cert and key")
	}
	if !strings.Contains(err.Error(), "server TLS requires both cert and key") {
		t.Errorf("error = %q, want 'server TLS requires both cert and key'", err)
	}
}

// TestResolveAndValidate_RCServerCAWithoutTLS rejects a CA without TLS
// enabled.
func TestResolveAndValidate_RCServerCAWithoutTLS(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-server-bind", "127.0.0.1:9191",
		"-rc-server-ca", "base64:QUJD",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want server TLS options require -rc-server-tls")
	}
	if !strings.Contains(err.Error(), "server TLS options require -rc-server-tls") {
		t.Errorf("error = %q, want 'server TLS options require -rc-server-tls'", err)
	}
}

// TestParse_UpstreamTrustFlags_LegacyMode binds the upstream trust flags at
// top level alongside the other provider-scope flags.
func TestParse_UpstreamTrustFlags_LegacyMode(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-upstream-ca", "file:/tmp/ca.pem",
		"-upstream-insecure",
		"-upstream-server-name", "upstream.local",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("len(Providers) = %d, want 1", len(cfg.Providers))
	}
	p := cfg.Providers[0]
	if p.UpstreamCA != "file:/tmp/ca.pem" {
		t.Errorf("UpstreamCA = %q, want file:/tmp/ca.pem", p.UpstreamCA)
	}
	if !p.UpstreamInsecure {
		t.Error("UpstreamInsecure = false, want true")
	}
	if p.UpstreamServerName != "upstream.local" {
		t.Errorf("UpstreamServerName = %q, want upstream.local", p.UpstreamServerName)
	}
}

// TestParse_UpstreamTrustFlags_SectionedMode checks the trust flags stay with
// their own provider section and do not leak across sections.
func TestParse_UpstreamTrustFlags_SectionedMode(t *testing.T) {
	cfg, err := Parse([]string{
		"--provider=acme",
		"-upstream", "https://acme.example.com",
		"-prefix", "/acme",
		"-upstream-ca", "file:/tmp/acme-ca.pem",
		"-upstream-insecure",
		"-upstream-server-name", "acme.local",
		"--provider=beta",
		"-upstream", "https://beta.example.com",
		"-prefix", "/beta",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("len(Providers) = %d, want 2", len(cfg.Providers))
	}
	if cfg.Providers[0].UpstreamCA != "file:/tmp/acme-ca.pem" {
		t.Errorf("acme UpstreamCA = %q, want file:/tmp/acme-ca.pem", cfg.Providers[0].UpstreamCA)
	}
	if !cfg.Providers[0].UpstreamInsecure {
		t.Error("acme UpstreamInsecure = false, want true")
	}
	if cfg.Providers[0].UpstreamServerName != "acme.local" {
		t.Errorf("acme UpstreamServerName = %q, want acme.local", cfg.Providers[0].UpstreamServerName)
	}
	if cfg.Providers[1].UpstreamCA != "" {
		t.Errorf("beta UpstreamCA = %q, want empty (flags must not leak between sections)", cfg.Providers[1].UpstreamCA)
	}
	if cfg.Providers[1].UpstreamInsecure {
		t.Error("beta UpstreamInsecure = true, want false (flags must not leak between sections)")
	}
	if cfg.Providers[1].UpstreamServerName != "" {
		t.Errorf("beta UpstreamServerName = %q, want empty (flags must not leak between sections)", cfg.Providers[1].UpstreamServerName)
	}
}

// TestResolveAndValidate_UpstreamTrustWithTerminate rejects the local
// handshake trust flags when the handshake runs at the RC server, where they
// would be silently ignored. The -rc-target-* options are the replacement.
func TestResolveAndValidate_UpstreamTrustWithTerminate(t *testing.T) {
	for _, args := range [][]string{
		{"-upstream-ca", "file:/tmp/ca.pem"},
		{"-upstream-insecure"},
		{"-upstream-server-name", "upstream.local"},
	} {
		cfg, err := Parse(append([]string{
			"-upstream", "https://x",
			"-rc-server", "127.0.0.1:9090",
			"-rc-terminate-tls",
		}, args...))
		if err != nil {
			t.Fatalf("Parse %q: %v", args, err)
		}
		err = cfg.ResolveAndValidate()
		if err == nil {
			t.Fatalf("ResolveAndValidate %q: nil error, want upstream trust rejection", args)
		}
		if !strings.Contains(err.Error(), "upstream TLS trust options") {
			t.Errorf("ResolveAndValidate %q error = %q, want 'upstream TLS trust options'", args, err)
		}
	}
}

// TestResolveAndValidate_UpstreamTrustBadCA rejects an undecodable CA at
// startup instead of failing every request later. base64:aGVsbG8= decodes
// to "hello", which is not PEM.
func TestResolveAndValidate_UpstreamTrustBadCA(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-upstream-ca", "base64:aGVsbG8=",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err == nil {
		t.Fatal("ResolveAndValidate: nil error, want invalid upstream CA PEM")
	} else if !strings.Contains(err.Error(), "upstream CA") {
		t.Errorf("error = %q, want 'upstream CA'", err)
	}
}

// TestUpstreamTLSConfig_DefaultNil confirms no trust flag means a nil config:
// the transport verifies against the system roots exactly as before.
func TestUpstreamTLSConfig_DefaultNil(t *testing.T) {
	cfg, err := Parse([]string{"-upstream", "https://x"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	got, err := cfg.Providers[0].UpstreamTLSConfig()
	if err != nil {
		t.Fatalf("UpstreamTLSConfig: %v", err)
	}
	if got != nil {
		t.Errorf("UpstreamTLSConfig() = %v, want nil", got)
	}
}

// TestUpstreamTLSConfig_InsecureAndServerName confirms the non-CA knobs land
// on the config without needing any material.
func TestUpstreamTLSConfig_InsecureAndServerName(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-upstream-insecure",
		"-upstream-server-name", "upstream.local",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	got, err := cfg.Providers[0].UpstreamTLSConfig()
	if err != nil {
		t.Fatalf("UpstreamTLSConfig: %v", err)
	}
	if got == nil {
		t.Fatal("UpstreamTLSConfig() = nil, want non-nil")
	}
	if !got.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = false, want true")
	}
	if got.ServerName != "upstream.local" {
		t.Errorf("ServerName = %q, want upstream.local", got.ServerName)
	}
	if got.RootCAs != nil {
		t.Error("RootCAs != nil, want nil without -upstream-ca")
	}
}

// TestResolveAndValidate_RCWhitespaceOnlyClientHelloIgnored confirms a
// whitespace-only -rc-target-client-hello is the engine default everywhere:
// ParseClientHello trims it to nil, the pairing log trims it to the
// engine-default note, and rcFlagSet trims it too, so without -rc-server it
// is not a stray option.
func TestResolveAndValidate_RCWhitespaceOnlyClientHelloIgnored(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://x",
		"-rc-target-client-hello", "   ",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v (whitespace-only hello must absorb as engine default)", err)
	}
	if d := cfg.Providers[0].RCDialer(); d != nil {
		t.Errorf("RCDialer() = %v, want nil", d)
		_ = d.Close()
	}
}

// TestResolveAndValidate_UpstreamTrustPlaintext rejects the trust flags on a
// plaintext upstream, where the transport would never consult them.
func TestResolveAndValidate_UpstreamTrustPlaintext(t *testing.T) {
	for _, args := range [][]string{
		{"-upstream-ca", "file:/tmp/ca.pem"},
		{"-upstream-insecure"},
		{"-upstream-server-name", "upstream.local"},
	} {
		cfg, err := Parse(append([]string{"-upstream", "http://x"}, args...))
		if err != nil {
			t.Fatalf("Parse %q: %v", args, err)
		}
		err = cfg.ResolveAndValidate()
		if err == nil {
			t.Fatalf("ResolveAndValidate %q: nil error, want plaintext rejection", args)
		}
		if !strings.Contains(err.Error(), "do not apply to a plaintext http upstream") {
			t.Errorf("ResolveAndValidate %q error = %q, want plaintext rejection", args, err)
		}
	}
}

func generateTestCerts(t *testing.T) (caPEM, clientCertPEM, clientKeyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	clientTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTmpl, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	clientCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})
	clientKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return caPEM, clientCertPEM, clientKeyPEM
}

// TestResolveAndValidate_RCMaterialFailsClosed pins the startup rejection of
// every unusable piece of RC material, before any socket is bound or dialled.
// Each case names the flag through the error text so the operator can fix the
// configuration without guessing.
func TestResolveAndValidate_RCMaterialFailsClosed(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not pem at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		args    []string
		wantErr string
	}{
		"empty upstream CA": {
			args:    []string{"-upstream-ca", "base64:"},
			wantErr: "key material is empty",
		},
		"empty channel CA": {
			args:    []string{"-rc-server", "127.0.0.1:9090", "-rc-channel-tls", "-rc-channel-ca", "file:" + empty},
			wantErr: "key material is empty",
		},
		"empty channel cert with valid key": {
			args: []string{"-rc-server", "127.0.0.1:9090", "-rc-channel-tls",
				"-rc-channel-cert", "base64:", "-rc-channel-key", "base64:aGVsbG8="},
			wantErr: "key material is empty",
		},
		"garbage server cert": {
			args: []string{"-rc-server-bind", "127.0.0.1:0", "-rc-server-tls",
				"-rc-server-cert", "file:" + garbage, "-rc-server-key", "file:" + garbage},
			wantErr: "rc-server cert/key",
		},
		"garbage target CA": {
			args:    []string{"-rc-server", "127.0.0.1:9090", "-rc-terminate-tls", "-rc-target-ca", "file:" + garbage},
			wantErr: "invalid target CA PEM",
		},
		"min version above max": {
			args: []string{"-rc-server", "127.0.0.1:9090", "-rc-terminate-tls",
				"-rc-target-min-version", "1.3", "-rc-target-max-version", "1.2"},
			wantErr: "above -rc-target-max-version",
		},
		"client hello without termination": {
			args:    []string{"-rc-server", "127.0.0.1:9090", "-rc-target-client-hello", "file:/nonexistent/spec.json"},
			wantErr: "requires -rc-terminate-tls",
		},
	}
	for name, tc := range cases {
		cfg, err := Parse(append([]string{"-upstream", "https://x"}, tc.args...))
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
	}
}

// TestResolveAndValidate_TerminateTLSPlaintextUpstream rejects remote TLS
// termination on a plaintext upstream: there is no upstream handshake to run
// at the RC server, so accepting the flag would silently forward plaintext
// while the operator believes the handshake moved.
func TestResolveAndValidate_TerminateTLSPlaintextUpstream(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "http://127.0.0.1:8080",
		"-rc-server", "127.0.0.1:9090",
		"-rc-terminate-tls",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = cfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate = nil, want the plaintext-termination rejection")
	}
	for _, want := range []string{"-rc-terminate-tls", "plaintext http upstream"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

// TestResolveAndValidate_RCTargetOffersOnlyHTTP11 pins that a target ALPN
// offer other than http/1.1, in the list or in a client-hello extension, is
// refused at resolve time with an error naming the flag.
func TestResolveAndValidate_RCTargetOffersOnlyHTTP11(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		flag string
	}{
		{"alpn h3", []string{"-rc-target-alpn", "h3"}, "-rc-target-alpn"},
		{"alpn foo", []string{"-rc-target-alpn", "foo"}, "-rc-target-alpn"},
		{"client-hello h2", []string{"-rc-target-client-hello", `{"extensions":[{"type":16,"raw":"AAMCaDI="}]}`}, "-rc-target-client-hello"},
		{"client-hello malformed", []string{"-rc-target-client-hello", `{"extensions":[{"type":16,"raw":"AAE="}]}`}, "-rc-target-client-hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse(append([]string{
				"-upstream=https://api.example.com",
				"-rc-server", "127.0.0.1:9090",
				"-rc-terminate-tls",
			}, tc.args...))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = cfg.ResolveAndValidate()
			if err == nil {
				closeDialer(t, cfg.Providers[0])
				t.Fatal("ResolveAndValidate = nil, want a rejection")
			}
			if !strings.Contains(err.Error(), tc.flag) || !strings.Contains(err.Error(), "http/1.1") {
				t.Fatalf("error = %v, want it to name %s and http/1.1", err, tc.flag)
			}
		})
	}
}
