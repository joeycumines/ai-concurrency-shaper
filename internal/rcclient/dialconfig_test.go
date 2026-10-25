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
	"encoding/base64"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeycumines/sesame/sesamepb"
)

func TestParseTLSVersion(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want sesamepb.TlsVersion
	}{
		{"1.0", sesamepb.TlsVersion_TLS_VERSION_TLS_1_0},
		{"1.1", sesamepb.TlsVersion_TLS_VERSION_TLS_1_1},
		{"1.2", sesamepb.TlsVersion_TLS_VERSION_TLS_1_2},
		{"1.3", sesamepb.TlsVersion_TLS_VERSION_TLS_1_3},
		{"tls1.2", sesamepb.TlsVersion_TLS_VERSION_TLS_1_2},
		{"TLS1.3", sesamepb.TlsVersion_TLS_VERSION_TLS_1_3},
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

func TestParseClientHello(t *testing.T) {
	// Inline protojson.
	spec, err := ParseClientHello(`{"cipherSuites":[4865],"sessionIdLength":32}`)
	if err != nil {
		t.Fatalf("inline protojson: unexpected error: %v", err)
	}
	if len(spec.GetCipherSuites()) != 1 || spec.GetCipherSuites()[0] != 4865 {
		t.Errorf("cipher suites = %v, want [4865]", spec.GetCipherSuites())
	}
	if spec.GetSessionIdLength() != 32 {
		t.Errorf("session_id_length = %d, want 32", spec.GetSessionIdLength())
	}

	// base64:DATA form.
	enc := base64.StdEncoding.EncodeToString([]byte(`{"targetSize":512}`))
	spec, err = ParseClientHello("base64:" + enc)
	if err != nil {
		t.Fatalf("base64 form: unexpected error: %v", err)
	}
	if spec.GetTargetSize() != 512 {
		t.Errorf("target_size = %d, want 512", spec.GetTargetSize())
	}

	// file:PATH form.
	path := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(path, []byte(`{"session_id_length":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err = ParseClientHello("file:" + path)
	if err != nil {
		t.Fatalf("file form: unexpected error: %v", err)
	}
	if spec.GetSessionIdLength() != 0 {
		t.Errorf("session_id_length = %d, want 0", spec.GetSessionIdLength())
	}

	// Unknown fields are rejected (strict decode).
	if _, err := ParseClientHello(`{"bogusField":true}`); err == nil {
		t.Error("unknown field: want error, got nil")
	}

	// Malformed JSON.
	if _, err := ParseClientHello(`{not-json`); err == nil {
		t.Error("malformed json: want error, got nil")
	}

	// Unsupported prefix names the accepted forms instead of echoing the
	// value back.
	_, err = ParseClientHello("http://example.com/spec")
	if err == nil {
		t.Fatal("unsupported prefix: want error, got nil")
	}
	if !strings.Contains(err.Error(), "file:PATH") {
		t.Errorf("unsupported prefix error = %q, want the accepted forms named", err)
	}

	// Empty file: path.
	if _, err := ParseClientHello("file:"); err == nil {
		t.Error("empty file path: want error, got nil")
	}
}

func TestParseProxyHop(t *testing.T) {
	hop, err := ParseProxyHop("socks5@127.0.0.1:1080;user=alice;pass=secret")
	if err != nil {
		t.Fatalf("socks5 hop: unexpected error: %v", err)
	}
	if hop.Type != sesamepb.ProxyHop_TYPE_SOCKS5 {
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
	if hop.Type != sesamepb.ProxyHop_TYPE_HTTP_CONNECT {
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
		// The credential IS the offending token: a password pasted as its own
		// segment, and a password pasted as the key of a stray pair.
		"socks5@host:1080;" + pass,
		"socks5@host:1080;" + pass + "=x",
		"socks5://user:" + pass + "@proxy:1080", // URL-style: creds ahead of '@'
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
		ALPN:             []string{"http/1.1"},
		MinVersion:       "1.2",
		MaxVersion:       "1.3",
		ClientHello:      &sesamepb.ClientHelloSpec{SessionIdLength: 32},
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

	// Out-of-range ClientHelloSpec value.
	badSpec := good
	badSpec.ClientHello = &sesamepb.ClientHelloSpec{CipherSuites: []int32{70000}}
	if err := badSpec.Validate(); err == nil {
		t.Error("out-of-range client hello spec: want error, got nil")
	}

	// Bad proxy hop.
	badHop := DialConfig{ProxyHops: []string{"ftp@host:21"}}
	if err := badHop.Validate(); err == nil {
		t.Error("bad proxy hop: want error, got nil")
	}
}

// TestDialConfigValidateWindowInt32Range pins the fail-closed upper bound:
// Capabilities renders WindowSize as int32, so an int above that range must
// be rejected here rather than overflowed on the wire.
func TestDialConfigValidateWindowInt32Range(t *testing.T) {
	if err := (&DialConfig{FlowControl: true, WindowSize: math.MaxInt32}).Validate(); err != nil {
		t.Fatalf("window at int32 max: err = %v, want nil", err)
	}
	err := (&DialConfig{FlowControl: true, WindowSize: math.MaxInt32 + 1}).Validate()
	if err == nil || !strings.Contains(err.Error(), "window size exceeds int32 range") {
		t.Fatalf("window above int32 max: err = %v, want 'window size exceeds int32 range'", err)
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

// TestDialConfigValidateMaterialAndVersions pins the startup validation of
// every remote-termination TLS value: malformed PEM, an empty configured value,
// and an impossible version range all fail Validate, before any socket exists.
func TestDialConfigValidateMaterialAndVersions(t *testing.T) {
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
		cfg     DialConfig
		wantErr string
	}{
		"garbage target CA": {
			cfg:     DialConfig{TerminateTLS: true, TargetCA: "file:" + garbage},
			wantErr: "invalid target CA PEM",
		},
		"garbage target cert and key": {
			cfg:     DialConfig{TerminateTLS: true, TargetCert: "file:" + garbage, TargetKey: "file:" + garbage},
			wantErr: "target client cert/key",
		},
		"empty target CA": {
			cfg:     DialConfig{TerminateTLS: true, TargetCA: "file:" + empty},
			wantErr: "key material is empty",
		},
		"empty target cert with valid key": {
			cfg:     DialConfig{TerminateTLS: true, TargetCert: "base64:", TargetKey: "base64:aGVsbG8="},
			wantErr: "key material is empty",
		},
		"min above max": {
			cfg:     DialConfig{TerminateTLS: true, MinVersion: "1.3", MaxVersion: "1.2"},
			wantErr: "above -rc-target-max-version",
		},
	}
	for name, tc := range cases {
		err := tc.cfg.Validate()
		if err == nil {
			t.Errorf("%s: Validate = nil, want %q", name, tc.wantErr)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, tc.wantErr)
		}
	}
	// The equal-version pair is legal.
	if err := (&DialConfig{TerminateTLS: true, MinVersion: "1.2", MaxVersion: "1.2"}).Validate(); err != nil {
		t.Errorf("equal version range: unexpected error: %v", err)
	}
}

// TestChannelConfigValidateMaterial pins the same discipline for the channel:
// a malformed or empty configured CA/cert/key is a validation error, and the
// !TLS contradiction is reported before any file is read.
func TestChannelConfigValidateMaterial(t *testing.T) {
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
		cfg     ChannelConfig
		wantErr string
	}{
		"empty CA": {
			cfg:     ChannelConfig{Server: "127.0.0.1:1", TLS: true, CA: "file:" + empty},
			wantErr: "key material is empty",
		},
		"garbage CA": {
			cfg:     ChannelConfig{Server: "127.0.0.1:1", TLS: true, CA: "file:" + garbage},
			wantErr: "invalid channel CA PEM",
		},
		"garbage cert and key": {
			cfg:     ChannelConfig{Server: "127.0.0.1:1", TLS: true, Cert: "file:" + garbage, Key: "file:" + garbage},
			wantErr: "channel client cert/key",
		},
		"empty cert with valid key": {
			cfg:     ChannelConfig{Server: "127.0.0.1:1", TLS: true, Cert: "base64:", Key: "base64:aGVsbG8="},
			wantErr: "key material is empty",
		},
		"TLS options without TLS wins over material": {
			// The file does not exist: the contradiction must be reported, not
			// the missing file.
			cfg:     ChannelConfig{Server: "127.0.0.1:1", CA: "file:/nonexistent/rcclient-channel-test.pem"},
			wantErr: "require -rc-channel-tls",
		},
	}
	for name, tc := range cases {
		err := tc.cfg.Validate()
		if err == nil {
			t.Errorf("%s: Validate = nil, want %q", name, tc.wantErr)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, tc.wantErr)
		}
	}
}

// TestServerConfigValidateMaterial pins startup validation of the embedded RC
// server's material: malformed PEM never waits for the listener to be bound.
func TestServerConfigValidateMaterial(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not pem at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		cfg     ServerConfig
		wantErr string
	}{
		"garbage cert and key": {
			cfg:     ServerConfig{Bind: "127.0.0.1:0", TLS: true, Cert: "file:" + garbage, Key: "file:" + garbage},
			wantErr: "rc-server cert/key",
		},
		"garbage CA": {
			cfg:     ServerConfig{Bind: "127.0.0.1:0", TLS: true, CA: "file:" + garbage, Cert: "file:" + garbage, Key: "file:" + garbage},
			wantErr: "invalid server CA PEM",
		},
	}
	for name, tc := range cases {
		err := tc.cfg.Validate()
		if err == nil {
			t.Errorf("%s: Validate = nil, want %q", name, tc.wantErr)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, tc.wantErr)
		}
	}
}

// TestDialConfigRejectsH2Offer pins the HTTP/1.1-only constraint of a
// remote-terminated connection: the proxy dials through
// http.Transport.DialTLSContext, whose returned connection is not a *tls.Conn,
// so net/http never learns the negotiated protocol and never sends an HTTP/2
// preface. An h2 offer — in the ALPN list or in a wire-level client-hello
// extension — would let the upstream select a protocol this client cannot use.
func TestDialConfigRejectsH2Offer(t *testing.T) {
	// The ALPN list.
	for _, protocol := range []string{"h2", "H2", " h2 "} {
		err := (&DialConfig{TerminateTLS: true, ALPN: []string{"http/1.1", protocol}}).Validate()
		if err == nil || !strings.Contains(err.Error(), "must not offer h2") {
			t.Errorf("ALPN %q: err = %v, want the h2 rejection", protocol, err)
		}
	}
	// A client-hello spec whose raw ALPN extension offers h2.
	// Body: 2-byte list length (5) then 1-byte length + name: "\x02h2".
	h2Spec := &sesamepb.ClientHelloSpec{Extensions: []*sesamepb.ClientHelloExtension{
		{Type: 16, Body: &sesamepb.ClientHelloExtension_Raw{Raw: []byte{0x00, 0x03, 0x02, 'h', '2'}}},
	}}
	err := (&DialConfig{TerminateTLS: true, ClientHello: h2Spec}).Validate()
	if err == nil || !strings.Contains(err.Error(), "must not offer h2") {
		t.Errorf("client-hello h2: err = %v, want the h2 rejection", err)
	}
	// A multi-entry list whose second name is h2 is caught.
	multiSpec := &sesamepb.ClientHelloSpec{Extensions: []*sesamepb.ClientHelloExtension{
		{Type: 16, Body: &sesamepb.ClientHelloExtension_Raw{
			// list length 12: "\x02h2" then "\x08http/1.1"
			Raw: []byte{0x00, 0x0c, 0x02, 'h', '2', 0x08, 'h', 't', 't', 'p', '/', '1', '.', '1'},
		}},
	}}
	if err := (&DialConfig{TerminateTLS: true, ClientHello: multiSpec}).Validate(); err == nil || !strings.Contains(err.Error(), "must not offer h2") {
		t.Errorf("multi-entry client-hello: err = %v, want the h2 rejection", err)
	}
	// The scan reads the name list to the end of the body rather than trusting
	// the declared list length, so a body that under-declares its length still
	// exposes the offer it contains: refusing it is the fail-closed answer.
	truncated := &sesamepb.ClientHelloSpec{Extensions: []*sesamepb.ClientHelloExtension{
		{Type: 16, Body: &sesamepb.ClientHelloExtension_Raw{Raw: []byte{0x00, 0x20, 0x02, 'h', '2'}}},
	}}
	if err := (&DialConfig{TerminateTLS: true, ClientHello: truncated}).Validate(); err == nil || !strings.Contains(err.Error(), "must not offer h2") {
		t.Errorf("under-declared list length: err = %v, want the h2 rejection", err)
	}
	// Bodies that carry no readable name must not panic and must be accepted.
	for name, raw := range map[string][]byte{
		"zero-length name": {0x00, 0x01, 0x00},
		"short body":       {0x00},
		"empty body":       {},
	} {
		spec := &sesamepb.ClientHelloSpec{Extensions: []*sesamepb.ClientHelloExtension{
			{Type: 16, Body: &sesamepb.ClientHelloExtension_Raw{Raw: raw}},
		}}
		if err := (&DialConfig{TerminateTLS: true, ClientHello: spec}).Validate(); err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
	}
	// A spec offering only http/1.1 is accepted.
	http11Spec := &sesamepb.ClientHelloSpec{Extensions: []*sesamepb.ClientHelloExtension{
		{Type: 16, Body: &sesamepb.ClientHelloExtension_Raw{Raw: []byte{0x00, 0x08, 0x08, 'h', 't', 't', 'p', '/', '1', '.', '1'}}},
	}}
	if err := (&DialConfig{TerminateTLS: true, ClientHello: http11Spec}).Validate(); err != nil {
		t.Errorf("client-hello http/1.1: unexpected error: %v", err)
	}
	// An empty ALPN list stays empty: the sesame server then sends no ALPN
	// extension at all, so nothing can negotiate h2.
	opts, err := (&DialConfig{TerminateTLS: true}).tlsOptions()
	if err != nil {
		t.Fatalf("tlsOptions: %v", err)
	}
	if len(opts.GetAlpnProtocols()) != 0 {
		t.Fatalf("AlpnProtocols = %q, want empty (no ALPN extension is sent)", opts.GetAlpnProtocols())
	}
}
