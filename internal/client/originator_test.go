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

package client

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http/httptest"
	"testing"
)

func TestExtractOriginator_RemoteTCP(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/api", nil)
	req.RemoteAddr = "203.0.113.50:41234"

	o := ExtractOriginator(req)

	if o.IP != "203.0.113.50" {
		t.Errorf("expected IP 203.0.113.50, got %q", o.IP)
	}
	if o.RemoteAddr != "203.0.113.50:41234" {
		t.Errorf("expected RemoteAddr 203.0.113.50:41234, got %q", o.RemoteAddr)
	}
	if o.IsLocal {
		t.Error("expected IsLocal=false for remote TCP address")
	}
	if o.PID != 0 {
		t.Errorf("expected PID=0 for remote address, got %d", o.PID)
	}
	if o.TLSCertSubject != "" {
		t.Errorf("expected empty TLSCertSubject, got %q", o.TLSCertSubject)
	}
}

func TestExtractOriginator_Loopback(t *testing.T) {
	req := httptest.NewRequest("GET", "http://localhost/api", nil)
	req.RemoteAddr = "127.0.0.1:41234"

	o := ExtractOriginator(req)

	if !o.IsLocal {
		t.Error("expected IsLocal=true for loopback address")
	}
	if o.IP != "127.0.0.1" {
		t.Errorf("expected IP 127.0.0.1, got %q", o.IP)
	}
}

func TestExtractOriginator_IPv6Loopback(t *testing.T) {
	req := httptest.NewRequest("GET", "http://[::1]/api", nil)
	req.RemoteAddr = "[::1]:41234"

	o := ExtractOriginator(req)

	if !o.IsLocal {
		t.Error("expected IsLocal=true for IPv6 loopback")
	}
	if o.IP != "::1" {
		t.Errorf("expected IP ::1, got %q", o.IP)
	}
}

func TestExtractOriginator_LocalPrivateIP(t *testing.T) {
	tests := []struct {
		addr   string
		expect bool
	}{
		// Loopback addresses are local.
		{"127.0.0.1:41234", true},
		{"127.0.0.2:41234", true},
		{"[::1]:41234", true},
		// Private RFC1918 addresses are NOT local — they can belong
		// to remote clients on the same LAN.
		{"10.0.0.1:41234", false},
		{"10.255.255.255:41234", false},
		{"172.16.0.1:41234", false},
		{"172.31.255.255:41234", false},
		{"192.168.1.1:41234", false},
		{"192.168.0.100:41234", false},
		{"172.15.0.1:41234", false}, // below 172.16
		{"172.32.0.1:41234", false}, // above 172.31
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://example.com/api", nil)
			req.RemoteAddr = tt.addr
			o := ExtractOriginator(req)
			if o.IsLocal != tt.expect {
				t.Errorf("addr %s: expected IsLocal=%v, got %v", tt.addr, tt.expect, o.IsLocal)
			}
		})
	}
}

func TestExtractOriginator_UNIXSocket(t *testing.T) {
	req := httptest.NewRequest("GET", "http://localhost/api", nil)
	req.RemoteAddr = "/var/run/app.sock"

	o := ExtractOriginator(req)

	if !o.IsLocal {
		t.Error("expected IsLocal=true for UNIX socket path")
	}
}

func TestExtractOriginator_TLS(t *testing.T) {
	req := httptest.NewRequest("GET", "https://example.com/api", nil)
	req.RemoteAddr = "127.0.0.1:41234"
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{
			{},
		},
	}
	// Set a non-empty subject on the peer cert for testing.
	req.TLS.PeerCertificates[0].Subject = pkix.Name{CommonName: "test-client", Organization: []string{"Test Corp"}}

	o := ExtractOriginator(req)

	if o.TLSCertSubject == "" {
		t.Error("expected non-empty TLSCertSubject")
	}
}

// pkixName is a helper to create a pkix.Name for tests.
// We use a simpler approach since we can't import crypto/x509/pkix easily.
func TestExtractOriginator_Fingerprint(t *testing.T) {
	// Remote originator uses IP.
	remote := Originator{IP: "203.0.113.50", IsLocal: false}
	fp := remote.Fingerprint()
	if fp != "ip:203.0.113.50" {
		t.Errorf("expected fingerprint ip:203.0.113.50, got %q", fp)
	}

	// Local originator with PID uses pid prefix with decimal PID.
	local := Originator{IsLocal: true, PID: 1234, IP: "127.0.0.1"}
	fp = local.Fingerprint()
	if fp != "pid:1234" {
		t.Errorf("expected fingerprint pid:1234, got %q", fp)
	}

	// Local originator without PID uses IP.
	localNoPID := Originator{IsLocal: true, PID: 0, IP: "127.0.0.1"}
	fp = localNoPID.Fingerprint()
	if fp != "ip:127.0.0.1" {
		t.Errorf("expected fingerprint ip:127.0.0.1 for PID=0, got %q", fp)
	}
}

func TestExtractOriginator_SetLocalProcessInfo(t *testing.T) {
	o := Originator{RemoteAddr: "127.0.0.1:41234"}
	SetLocalProcessInfo(&o, 5678, "my-app")

	if o.PID != 5678 {
		t.Errorf("expected PID=5678, got %d", o.PID)
	}
	if o.AppName != "my-app" {
		t.Errorf("expected AppName=my-app, got %q", o.AppName)
	}
	if !o.IsLocal {
		t.Error("expected IsLocal=true after SetLocalProcessInfo with PID>0")
	}
}

func TestExtractOriginator_EmptyRemoteAddr(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/api", nil)
	req.RemoteAddr = ""

	o := ExtractOriginator(req)

	if o.IP != "" {
		t.Errorf("expected empty IP, got %q", o.IP)
	}
	if o.IsLocal {
		t.Error("expected IsLocal=false for empty remote address")
	}
}

func TestExtractOriginator_BadRemoteAddr(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/api", nil)
	req.RemoteAddr = "not-a-valid-addr"

	// Should not panic; IP will be empty if SplitHostPort fails.
	o := ExtractOriginator(req)
	if o.IP != "" {
		t.Errorf("expected empty IP for bad remote addr, got %q", o.IP)
	}
	if o.IsLocal {
		t.Error("expected IsLocal=false for bad remote address")
	}
}
