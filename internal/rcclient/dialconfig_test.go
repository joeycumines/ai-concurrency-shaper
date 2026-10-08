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

package rcclient

import (
	"math"
	"strings"
	"testing"

	sesameproxy "github.com/joeycumines/sesame/rc/proxy"
	sesametls "github.com/joeycumines/sesame/rc/tls"
)

func TestParseTLSVersion(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want sesametls.TLSVersion
	}{
		{"1.0", sesametls.TLSVersion_TLS_1_0},
		{"1.1", sesametls.TLSVersion_TLS_1_1},
		{"1.2", sesametls.TLSVersion_TLS_1_2},
		{"1.3", sesametls.TLSVersion_TLS_1_3},
		{"tls1.2", sesametls.TLSVersion_TLS_1_2},
		{"TLS1.3", sesametls.TLSVersion_TLS_1_3},
	} {
		got, err := ParseTLSVersion(tc.in)
		if err != nil {
			t.Errorf("ParseTLSVersion(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseTLSVersion(%q): got %v want %v", tc.in, got, tc.want)
		}
	}

	for _, bad := range []string{"", "2.0", "1.4", "ssl3", "garbage"} {
		if _, err := ParseTLSVersion(bad); err == nil {
			t.Errorf("ParseTLSVersion(%q): want error, got nil", bad)
		}
	}
}

func TestParseCipherSuite(t *testing.T) {
	// 0x1301 == TLS_AES_128_GCM_SHA256 == 4865 decimal.
	if got, err := ParseCipherSuite("0x1301"); err != nil || got != 0x1301 {
		t.Errorf("ParseCipherSuite(0x1301): got %d, %v; want %d, nil", got, err, 0x1301)
	}
	if got, err := ParseCipherSuite("4865"); err != nil || got != 4865 {
		t.Errorf("ParseCipherSuite(4865): got %d, %v; want 4865, nil", got, err)
	}
	for _, bad := range []string{"0x10000", "65536", "not-a-number", ""} {
		if _, err := ParseCipherSuite(bad); err == nil {
			t.Errorf("ParseCipherSuite(%q): want error, got nil", bad)
		}
	}
}

func TestParseFingerprint(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want sesametls.FingerprintPreset
	}{
		{"chrome-auto", sesametls.FingerprintPreset_CHROME_AUTO},
		{"CHROME_AUTO", sesametls.FingerprintPreset_CHROME_AUTO},
		{"chrome_120", sesametls.FingerprintPreset_CHROME_120},
		{"firefox-120", sesametls.FingerprintPreset_FIREFOX_120},
		{"runtime-default", sesametls.FingerprintPreset_RUNTIME_DEFAULT},
		{"RUNTIME_DEFAULT", sesametls.FingerprintPreset_RUNTIME_DEFAULT},
		{"safari-17", sesametls.FingerprintPreset_SAFARI_17},
	} {
		got, err := ParseFingerprint(tc.in)
		if err != nil {
			t.Errorf("ParseFingerprint(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseFingerprint(%q): got %v want %v", tc.in, got, tc.want)
		}
	}

	if got, err := ParseFingerprint(""); err != nil || got != sesametls.FingerprintPreset_FINGERPRINT_PRESET_UNSPECIFIED {
		t.Errorf("ParseFingerprint(\"\"): got %v, %v; want UNSPECIFIED, nil", got, err)
	}

	for _, bad := range []string{"bogus-preset", "chrome-99999"} {
		if _, err := ParseFingerprint(bad); err == nil {
			t.Errorf("ParseFingerprint(%q): want error, got nil", bad)
		}
	}
}

func TestParseProxyHop(t *testing.T) {
	hop, err := ParseProxyHop("socks5@127.0.0.1:1080;user=alice;pass=secret")
	if err != nil {
		t.Fatalf("socks5 hop: unexpected error: %v", err)
	}
	if hop.Type != sesameproxy.ProxyHop_SOCKS5 {
		t.Errorf("socks5 hop: type got %v want SOCKS5", hop.Type)
	}
	if hop.Address.GetNetwork() != "tcp" || hop.Address.GetAddress() != "127.0.0.1:1080" {
		t.Errorf("socks5 hop: address got %v", hop.Address)
	}
	if hop.Username != "alice" || hop.Password != "secret" {
		t.Errorf("socks5 hop: creds got user=%q pass=%q", hop.Username, hop.Password)
	}

	hop, err = ParseProxyHop("http-connect@proxy.example:8080;header=Basic dXNlcjpwYXNz")
	if err != nil {
		t.Fatalf("http-connect hop: unexpected error: %v", err)
	}
	if hop.Type != sesameproxy.ProxyHop_HTTP_CONNECT {
		t.Errorf("http-connect hop: type got %v want HTTP_CONNECT", hop.Type)
	}
	if hop.AuthHeader != "Basic dXNlcjpwYXNz" {
		t.Errorf("http-connect hop: header got %q", hop.AuthHeader)
	}

	for _, bad := range []string{
		"no-at-sign",
		"ftp@host:21",
		"socks5@",
		"socks5@host:1080;badopt",
		"socks5@host:1080;unknown=value",
	} {
		if _, err := ParseProxyHop(bad); err == nil {
			t.Errorf("ParseProxyHop(%q): want error, got nil", bad)
		}
	}
}

// TestParseProxyHopDoesNotLeakCredentials pins the secret-hygiene contract:
// a hop spec legitimately carries a proxy password or Proxy-Authorization
// header, so no parse error may echo the spec back — a typo in a
// credential-bearing hop must not leak the credential into the startup
// error and the journal. One malformed spec per error site, each carrying
// credentials beside the defect.
func TestParseProxyHopDoesNotLeakCredentials(t *testing.T) {
	const (
		pass   = "hunter2-s3cret"
		header = "Basic c3VwZXItc2VjcmV0LWhlYWRlcg=="
	)
	for _, spec := range []string{
		"header=" + header,                          // no TYPE@ADDRESS
		"ftp@host:21;pass=" + pass,                  // unknown type
		"socks5@;pass=" + pass,                      // empty address
		"socks5@host:1080;pass=" + pass + ";badopt", // option without '='
		"socks5@host:1080;pass=" + pass + ";nope=x", // unknown option
		"socks5://user:" + pass + "@proxy:1080",     // URL-style: creds ahead of '@'
	} {
		_, err := ParseProxyHop(spec)
		if err == nil {
			t.Fatal("malformed hop: want error, got nil")
		}
		if strings.Contains(err.Error(), pass) || strings.Contains(err.Error(), header) {
			t.Errorf("hop error leaks credentials: %v", err)
		}
		if strings.Contains(err.Error(), spec) {
			t.Errorf("hop error echoes the raw spec: %v", err)
		}
	}

	// The same contract through the startup validation path.
	verr := (&DialConfig{ProxyHops: []string{"socks5@host:1080;pass=" + pass + ";badopt"}}).Validate()
	if verr == nil {
		t.Fatal("Validate with malformed hop: want error, got nil")
	}
	if strings.Contains(verr.Error(), pass) {
		t.Errorf("Validate error leaks credentials: %v", verr)
	}
}

func TestDialConfigValidate(t *testing.T) {
	// Valid: full remote-TLS config.
	good := DialConfig{
		TerminateTLS:     true,
		TargetServerName: "example.com",
		ALPN:             []string{"h2", "http/1.1"},
		MinVersion:       "1.2",
		MaxVersion:       "1.3",
		Fingerprint:      "runtime-default",
		CipherSuites:     []string{"0x1301"},
		FlowControl:      true,
		WindowSize:       65536,
	}
	if err := good.Validate(); err != nil {
		t.Errorf("valid config: unexpected error: %v", err)
	}

	// Cert without key.
	certNoKey := good
	certNoKey.TargetCert = "base64:aGVsbG8="
	certNoKey.TargetKey = ""
	if err := certNoKey.Validate(); err == nil {
		t.Error("cert without key: want error, got nil")
	}

	// Target TLS options without TerminateTLS.
	noTerm := DialConfig{TargetServerName: "example.com"}
	if err := noTerm.Validate(); err == nil {
		t.Error("target options without terminate: want error, got nil")
	}

	// Window size without flow control.
	noFC := DialConfig{WindowSize: 65536}
	if err := noFC.Validate(); err == nil {
		t.Error("window without flow control: want error, got nil")
	}

	// Negative window size.
	negWin := DialConfig{FlowControl: true, WindowSize: -1}
	if err := negWin.Validate(); err == nil {
		t.Error("negative window: want error, got nil")
	}

	// Bad TLS version.
	badVer := good
	badVer.MinVersion = "9.9"
	if err := badVer.Validate(); err == nil {
		t.Error("bad version: want error, got nil")
	}

	// Bad cipher suite.
	badCS := good
	badCS.CipherSuites = []string{"0x10000"}
	if err := badCS.Validate(); err == nil {
		t.Error("bad cipher suite: want error, got nil")
	}

	// Bad proxy hop.
	badHop := DialConfig{ProxyHops: []string{"ftp@host:21"}}
	if err := badHop.Validate(); err == nil {
		t.Error("bad proxy hop: want error, got nil")
	}
}

// TestDialConfigValidateWindowUint32Range pins the fail-closed upper bound:
// Capabilities renders WindowSize as uint32, so an int above that range must
// be rejected here rather than truncated silently on the wire.
func TestDialConfigValidateWindowUint32Range(t *testing.T) {
	if err := (&DialConfig{FlowControl: true, WindowSize: math.MaxUint32}).Validate(); err != nil {
		t.Fatalf("window at uint32 max: err = %v, want nil", err)
	}
	err := (&DialConfig{FlowControl: true, WindowSize: math.MaxUint32 + 1}).Validate()
	if err == nil || !strings.Contains(err.Error(), "window size exceeds uint32 range") {
		t.Fatalf("window above uint32 max: err = %v, want 'window size exceeds uint32 range'", err)
	}
}

func TestChannelConfigValidate(t *testing.T) {
	// Missing server.
	if err := (&ChannelConfig{}).Validate(); err == nil {
		t.Error("missing server: want error, got nil")
	}

	// TLS options without TLS enabled.
	noTLS := ChannelConfig{Server: "127.0.0.1:1", CA: "base64:aGVsbG8="}
	if err := noTLS.Validate(); err == nil {
		t.Error("TLS options without -rc-channel-tls: want error, got nil")
	}

	// Cert without key.
	certNoKey := ChannelConfig{Server: "127.0.0.1:1", TLS: true, Cert: "base64:aGVsbG8="}
	if err := certNoKey.Validate(); err == nil {
		t.Error("cert without key: want error, got nil")
	}

	// Valid plaintext.
	if err := (&ChannelConfig{Server: "127.0.0.1:1"}).Validate(); err != nil {
		t.Errorf("valid plaintext channel: unexpected error: %v", err)
	}
}
