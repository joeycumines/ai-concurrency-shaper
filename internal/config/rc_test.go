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
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestParse_RCFlags_LegacyMode binds every provider-scope RC dial flag at top
// level (the single implicit provider), exactly like every other
// provider-scope flag.
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
