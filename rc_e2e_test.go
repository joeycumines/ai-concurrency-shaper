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

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/sesame/rc"
	"github.com/joeycumines/sesame/rc/netconn"
	"google.golang.org/grpc"
)

// ---- RC e2e test infrastructure ----

// testPKI is a self-contained certificate authority and leaf pair generated
// in-test (no committed fixtures). The server cert carries an IP SAN for
// 127.0.0.1 so hostname verification succeeds for loopback connections
// without any skip-verify.
type testPKI struct {
	caPEM                       []byte
	serverCertPEM, serverKeyPEM []byte
	clientCertPEM, clientKeyPEM []byte
}

// generateTestPKI creates a fresh CA plus server-auth and client-auth leaf
// certificates signed by it.
func generateTestPKI(t *testing.T) *testPKI {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          randomSerial(t),
		Subject:               pkix.Name{CommonName: "rc-e2e-test-ca"},
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

	leaf := func(cn string, server bool) (certPEM, keyPEM []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate leaf key: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: randomSerial(t),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}
		if server {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
			tmpl.DNSNames = []string{"localhost"}
		} else {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("create leaf cert: %v", err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatalf("marshal leaf key: %v", err)
		}
		certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
		return certPEM, keyPEM
	}

	pki := &testPKI{
		caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
	}
	pki.serverCertPEM, pki.serverKeyPEM = leaf("rc-e2e-server", true)
	pki.clientCertPEM, pki.clientKeyPEM = leaf("rc-e2e-client", false)
	return pki
}

func randomSerial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil || n.Sign() == 0 {
		t.Fatalf("random serial: %v", err)
	}
	return n
}

// writeFiles materializes the PKI as file: flag values in the test's temp dir.
func (p *testPKI) writeFiles(t *testing.T) (ca, serverCert, serverKey, clientCert, clientKey string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	return write("ca.pem", p.caPEM),
		write("server-cert.pem", p.serverCertPEM),
		write("server-key.pem", p.serverKeyPEM),
		write("client-cert.pem", p.clientCertPEM),
		write("client-key.pem", p.clientKeyPEM)
}

// startCountingRCServer runs an in-test gRPC server hosting
// RemoteControl.NetConn, whose dial factory counts every dial it serves.
func startCountingRCServer(t *testing.T) (addr string, dialCount *atomic.Int64) {
	t.Helper()
	var count atomic.Int64
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	rc.RegisterRemoteControlServer(srv, &netconn.Server{
		Dialer: func(req *rc.NetConnRequest_Dial) (netconn.Dialer, error) {
			count.Add(1)
			return netconn.DefaultDialer(req)
		},
	})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), &count
}

// startTLSUpstream serves handler over TLS with the given server certificate.
func startTLSUpstream(t *testing.T, handler http.Handler, serverCertPEM, serverKeyPEM []byte) string {
	t.Helper()
	cert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("tls key pair: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(tlsLn) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// newCountingHandler counts requests and records whether they arrived over TLS.
func newCountingHandler(tlsSeen *atomic.Bool) (http.Handler, *atomic.Int64) {
	var count atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if tlsSeen != nil {
			tlsSeen.Store(r.TLS != nil)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}), &count
}

// rcE2EPost sends one request through the proxy and reports outcome.
func rcE2EPost(t *testing.T, proxyAddr string) (status int, elapsed time.Duration, err error) {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second}
	start := time.Now()
	resp, err := client.Post("http://"+proxyAddr+"/v1/messages", "application/json", strings.NewReader(`{"model":"test"}`))
	elapsed = time.Since(start)
	if err != nil {
		return 0, elapsed, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, elapsed, nil
}

// waitForRCBind polls subprocess output for the embedded RC server's bind
// address, which run() logs as "rc-server: bind=<addr> tls=<bool>". Only
// complete lines are scanned: a partial line at the buffer edge could be
// cut mid-address, so text past the last newline stays unparsed until it
// is complete.
func waitForRCBind(t *testing.T, out *lockedBuffer) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	scanned := 0
	for time.Now().Before(deadline) {
		s := out.String()
		if end := strings.LastIndex(s, "\n"); end >= scanned {
			for line := range strings.SplitSeq(s[scanned:end], "\n") {
				if _, after, ok := strings.Cut(line, "rc-server: bind="); ok {
					addr, _, _ := strings.Cut(after, " tls=")
					if addr != "" {
						return addr
					}
				}
			}
			scanned = end + 1
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("rc server subprocess did not report a bind address")
	return ""
}

// waitForSubstring polls the subprocess output until substr appears or the
// timeout elapses. The proxy logs transport failures before writing the
// error response, but stderr reaches the test's buffer through a copy
// goroutine, so the marker can lag the client's receipt of the response; a
// one-shot read could race it.
func waitForSubstring(out *lockedBuffer, substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), substr) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return strings.Contains(out.String(), substr)
}

// ---- End-to-end tests ----

// TestRC_PlainTunnel_E2E: a subprocess proxy with -rc-server routes its
// upstream connections through an in-test RemoteControl.NetConn server; the
// request succeeds and both the upstream and the RC server observe it.
func TestRC_PlainTunnel_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	handler, upstreamCount := newCountingHandler(nil)
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	rcAddr, dialCount := startCountingRCServer(t)

	sub, out := startProxyCmd(t,
		"-upstream", upstream.URL,
		"-rc-server", rcAddr,
		"-bind", "127.0.0.1:0",
		"-retry", "0",
		"-circuit-breaker=false",
	)
	proxyAddr := sub.waitForListeningAddr(t, out)

	status, _, err := rcE2EPost(t, proxyAddr)
	if err != nil {
		t.Fatalf("POST through proxy: %v\nproxy output:\n%s", err, out.String())
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\nproxy output:\n%s", status, out.String())
	}
	if got := upstreamCount.Load(); got != 1 {
		t.Errorf("upstream request count = %d, want 1", got)
	}
	if got := dialCount.Load(); got < 1 {
		t.Errorf("RC server dial count = %d, want >= 1 (request must traverse the NetConn stream)", got)
	}
}

// TestRC_RemoteTLSTermination_E2E: with -rc-terminate-tls the RC server
// performs the upstream TLS handshake itself; the proxy treats the stream as
// already-terminated, the upstream sees a genuine TLS connection verified
// against the configured CA (no skip-verify).
func TestRC_RemoteTLSTermination_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pki := generateTestPKI(t)
	caPath, _, _, _, _ := pki.writeFiles(t)

	var tlsSeen atomic.Bool
	handler, upstreamCount := newCountingHandler(&tlsSeen)
	upstreamAddr := startTLSUpstream(t, handler, pki.serverCertPEM, pki.serverKeyPEM)

	rcAddr, dialCount := startCountingRCServer(t)

	sub, out := startProxyCmd(t,
		"-upstream", "https://"+upstreamAddr,
		"-rc-server", rcAddr,
		"-rc-terminate-tls",
		"-rc-target-ca", "file:"+caPath,
		"-bind", "127.0.0.1:0",
		"-retry", "0",
		"-circuit-breaker=false",
	)
	proxyAddr := sub.waitForListeningAddr(t, out)

	status, _, err := rcE2EPost(t, proxyAddr)
	if err != nil {
		t.Fatalf("POST through proxy: %v\nproxy output:\n%s", err, out.String())
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\nproxy output:\n%s", status, out.String())
	}
	if got := upstreamCount.Load(); got != 1 {
		t.Errorf("upstream request count = %d, want 1", got)
	}
	if !tlsSeen.Load() {
		t.Error("upstream did not observe a TLS connection: remote termination did not happen")
	}
	if got := dialCount.Load(); got < 1 {
		t.Errorf("RC server dial count = %d, want >= 1", got)
	}
}

// TestRC_TunnelTLS_E2E: without -rc-terminate-tls the tunnel carries plain
// TCP and the proxy performs its own TLS handshake over the stream. The
// proxy has no upstream trust hook, so against the in-test PKI the local
// handshake must fail closed on certificate verification — proving the
// handshake happens locally and over the tunnel (the TCP connection is
// counted on the RC server, no HTTP reaches the upstream) — and the same
// topology with -rc-terminate-tls succeeds, proving the handshake location
// is the only difference between the two modes.
func TestRC_TunnelTLS_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pki := generateTestPKI(t)
	caPath, _, _, _, _ := pki.writeFiles(t)

	var tlsSeen atomic.Bool
	handler, upstreamCount := newCountingHandler(&tlsSeen)
	upstreamAddr := startTLSUpstream(t, handler, pki.serverCertPEM, pki.serverKeyPEM)

	// Tunnel mode: plain TCP through the RC stream; the transport handshakes
	// locally and must fail on the test CA certificate.
	rcAddr, dialCount := startCountingRCServer(t)
	sub, out := startProxyCmd(t,
		"-upstream", "https://"+upstreamAddr,
		"-rc-server", rcAddr,
		"-bind", "127.0.0.1:0",
		"-retry", "0",
		"-circuit-breaker=false",
	)
	proxyAddr := sub.waitForListeningAddr(t, out)

	status, elapsed, err := rcE2EPost(t, proxyAddr)
	if elapsed > 15*time.Second {
		t.Fatalf("request took %s; a failed local handshake must fail promptly", elapsed)
	}
	if err == nil && status == http.StatusOK {
		t.Fatalf("tunnel-mode request succeeded against an untrusted upstream certificate")
	}
	if got := upstreamCount.Load(); got != 0 {
		t.Errorf("upstream request count = %d, want 0 (handshake must fail before HTTP)", got)
	}
	if got := dialCount.Load(); got < 1 {
		t.Errorf("RC server dial count = %d, want >= 1 (TCP must traverse the NetConn stream)", got)
	}
	if !waitForSubstring(out, "x509", 10*time.Second) {
		t.Errorf("proxy log lacks an x509 certificate verification failure; the transport must do its own TLS handshake over the tunnel\nproxy output:\n%s", out.String())
	}

	// Control: identical topology, remote termination. Now the RC server
	// verifies the certificate against -rc-target-ca and the request succeeds.
	sub2, out2 := startProxyCmd(t,
		"-upstream", "https://"+upstreamAddr,
		"-rc-server", rcAddr,
		"-rc-terminate-tls",
		"-rc-target-ca", "file:"+caPath,
		"-bind", "127.0.0.1:0",
		"-retry", "0",
		"-circuit-breaker=false",
	)
	proxyAddr2 := sub2.waitForListeningAddr(t, out2)

	status2, _, err2 := rcE2EPost(t, proxyAddr2)
	if err2 != nil {
		t.Fatalf("remote-termination control POST: %v\nproxy output:\n%s", err2, out2.String())
	}
	if status2 != http.StatusOK {
		t.Fatalf("remote-termination control status = %d, want 200\nproxy output:\n%s", status2, out2.String())
	}
	if got := upstreamCount.Load(); got != 1 {
		t.Errorf("upstream request count = %d, want 1 (the control request must arrive)", got)
	}
	if !tlsSeen.Load() {
		t.Error("upstream did not observe a TLS connection in the control run")
	}
}

// TestRC_ChannelMTLS_E2E: the gRPC channel between proxy and RC server runs
// mutual TLS end to end, both of them subprocesses. A client presenting the
// CA-signed client certificate succeeds; one without a certificate is
// rejected and never reaches the upstream.
func TestRC_ChannelMTLS_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pki := generateTestPKI(t)
	caPath, serverCert, serverKey, clientCert, clientKey := pki.writeFiles(t)

	handler, upstreamCount := newCountingHandler(nil)
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	// Embedded RC server subprocess with mTLS on the listener.
	_, rcOut := startProxyCmd(t,
		"-upstream", upstream.URL,
		"-bind", "127.0.0.1:0",
		"-rc-server-bind", "127.0.0.1:0",
		"-rc-server-tls",
		"-rc-server-ca", "file:"+caPath,
		"-rc-server-cert", "file:"+serverCert,
		"-rc-server-key", "file:"+serverKey,
	)
	rcAddr := waitForRCBind(t, rcOut)

	// Authenticated client: full channel mTLS.
	sub, out := startProxyCmd(t,
		"-upstream", upstream.URL,
		"-bind", "127.0.0.1:0",
		"-rc-server", rcAddr,
		"-rc-channel-tls",
		"-rc-channel-ca", "file:"+caPath,
		"-rc-channel-cert", "file:"+clientCert,
		"-rc-channel-key", "file:"+clientKey,
		"-retry", "0",
		"-circuit-breaker=false",
	)
	proxyAddr := sub.waitForListeningAddr(t, out)

	status, _, err := rcE2EPost(t, proxyAddr)
	if err != nil {
		t.Fatalf("authenticated POST: %v\nproxy output:\n%s\nrc output:\n%s", err, out.String(), rcOut.String())
	}
	if status != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200\nproxy output:\n%s\nrc output:\n%s", status, out.String(), rcOut.String())
	}
	if got := upstreamCount.Load(); got != 1 {
		t.Errorf("upstream request count = %d, want 1", got)
	}

	// Unauthenticated client: TLS on the channel but no client certificate.
	sub2, out2 := startProxyCmd(t,
		"-upstream", upstream.URL,
		"-bind", "127.0.0.1:0",
		"-rc-server", rcAddr,
		"-rc-channel-tls",
		"-rc-channel-ca", "file:"+caPath,
		"-retry", "0",
		"-circuit-breaker=false",
	)
	proxyAddr2 := sub2.waitForListeningAddr(t, out2)

	status2, _, err2 := rcE2EPost(t, proxyAddr2)
	if err2 == nil && status2 == http.StatusOK {
		t.Fatalf("unauthenticated client succeeded; the mTLS channel must reject it\nproxy output:\n%s", out2.String())
	}
	if got := upstreamCount.Load(); got != 1 {
		t.Errorf("upstream request count after rejected client = %d, want 1 (rejected request must not reach upstream)", got)
	}

	// Prove the rejection cause directly: the listener demands a client
	// certificate. In TLS 1.3 the client side of the handshake completes
	// before the server enforces the requirement, so a cert-less dial alone
	// does not fail — surface the server's rejection on the first read,
	// which carries the TLS alert. With the CA-signed client cert the
	// handshake is accepted.
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(pki.caPEM) {
		t.Fatal("test CA PEM failed to parse")
	}
	certLessErr := func() error {
		conn, err := tls.Dial("tcp", rcAddr, &tls.Config{RootCAs: caPool})
		if err != nil {
			return err // rejected during the handshake itself
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); err != nil {
			return err // server alert after the client-side handshake (TLS 1.3)
		}
		return nil
	}()
	if certLessErr == nil {
		t.Fatalf("cert-less TLS connection against the mTLS listener was usable; want certificate rejection\nrc output:\n%s", rcOut.String())
	} else if !strings.Contains(certLessErr.Error(), "certificate") && !strings.Contains(certLessErr.Error(), "x509") {
		t.Errorf("cert-less TLS error = %v, want a certificate-rejection error", certLessErr)
	}
	clientTLSCert, err := tls.X509KeyPair(pki.clientCertPEM, pki.clientKeyPEM)
	if err != nil {
		t.Fatalf("client key pair: %v", err)
	}
	conn, err := tls.Dial("tcp", rcAddr, &tls.Config{RootCAs: caPool, Certificates: []tls.Certificate{clientTLSCert}})
	if err != nil {
		t.Fatalf("authenticated raw TLS handshake failed: %v", err)
	}
	_ = conn.Close()
}

// TestRC_FailureModes_E2E: a down RC server and a wrong target CA both fail
// promptly at request time, never hanging and never reporting success.
func TestRC_FailureModes_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Run("RCServerDown", func(t *testing.T) {
		handler, upstreamCount := newCountingHandler(nil)
		upstream := httptest.NewServer(handler)
		t.Cleanup(upstream.Close)

		sub, out := startProxyCmd(t,
			"-upstream", upstream.URL,
			"-rc-server", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
			"-bind", "127.0.0.1:0",
			"-retry", "0",
			"-circuit-breaker=false",
		)
		proxyAddr := sub.waitForListeningAddr(t, out)

		status, elapsed, err := rcE2EPost(t, proxyAddr)
		if elapsed > 15*time.Second {
			t.Fatalf("request took %s; a down RC server must fail promptly, not hang", elapsed)
		}
		if err == nil && status == http.StatusOK {
			t.Fatalf("request succeeded against a down RC server")
		}
		if got := upstreamCount.Load(); got != 0 {
			t.Errorf("upstream request count = %d, want 0", got)
		}
	})

	t.Run("WrongTargetCA", func(t *testing.T) {
		goodPKI := generateTestPKI(t)
		badPKI := generateTestPKI(t)
		badCAPath, _, _, _, _ := badPKI.writeFiles(t)

		var tlsSeen atomic.Bool
		handler, upstreamCount := newCountingHandler(&tlsSeen)
		upstreamAddr := startTLSUpstream(t, handler, goodPKI.serverCertPEM, goodPKI.serverKeyPEM)

		rcAddr, dialCount := startCountingRCServer(t)

		sub, out := startProxyCmd(t,
			"-upstream", "https://"+upstreamAddr,
			"-rc-server", rcAddr,
			"-rc-terminate-tls",
			"-rc-target-ca", "file:"+badCAPath,
			"-bind", "127.0.0.1:0",
			"-retry", "0",
			"-circuit-breaker=false",
		)
		proxyAddr := sub.waitForListeningAddr(t, out)

		status, elapsed, err := rcE2EPost(t, proxyAddr)
		if elapsed > 15*time.Second {
			t.Fatalf("request took %s; a failed remote handshake must fail promptly", elapsed)
		}
		if err == nil && status == http.StatusOK {
			t.Fatalf("request succeeded despite a wrong target CA")
		}
		if got := upstreamCount.Load(); got != 0 {
			t.Errorf("upstream request count = %d, want 0 (handshake must fail before HTTP)", got)
		}
		if got := dialCount.Load(); got < 1 {
			t.Errorf("RC server dial count = %d, want >= 1 (the dial must be attempted before the handshake fails)", got)
		}
	})
}

// TestRC_ClientHelloSpec_E2E: the configured ClientHelloSpec flows through
// flag -> config -> dial -> server and is applied to the upstream TLS
// handshake. The positive arm uses a Go-honourable spec (max_version pinned
// to TLS1.2 plus a membership-filtered cipher_suites subset) and succeeds
// with a verified TLS connection; the negative arm uses an unhonourable spec
// (signature_algorithms, which crypto/tls cannot control) and fails closed
// before any upstream traffic.
func TestRC_ClientHelloSpec_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Go-honourable spec: cipher_suites restricted to a subset that filters
	// identically under both the AES and no-AES crypto/tls preference
	// tables, with max_version pinned to TLS1.2 so custom cipher_suites are
	// honourable (Go appends TLS1.3 suites otherwise). session_id_length 32
	// is the only explicit value Go can emit.
	const honourableSpec = `{"cipherSuites":[49195,49199],"sessionIdLength":32}`

	// Unhonourable spec: signature_algorithms has no public crypto/tls
	// control, so ApplyGoClientHelloSpec must reject it fail-closed.
	const unhonourableSpec = `{"signatureAlgorithms":[1027],"sessionIdLength":32}`

	t.Run("Honourable", func(t *testing.T) {
		pki := generateTestPKI(t)
		caPath, _, _, _, _ := pki.writeFiles(t)

		var tlsSeen atomic.Bool
		handler, upstreamCount := newCountingHandler(&tlsSeen)
		upstreamAddr := startTLSUpstream(t, handler, pki.serverCertPEM, pki.serverKeyPEM)

		rcAddr, dialCount := startCountingRCServer(t)

		sub, out := startProxyCmd(t,
			"-upstream", "https://"+upstreamAddr,
			"-rc-server", rcAddr,
			"-rc-terminate-tls",
			"-rc-target-ca", "file:"+caPath,
			"-rc-target-max-version", "1.2",
			"-rc-target-client-hello", honourableSpec,
			"-bind", "127.0.0.1:0",
			"-retry", "0",
			"-circuit-breaker=false",
		)
		proxyAddr := sub.waitForListeningAddr(t, out)

		status, _, err := rcE2EPost(t, proxyAddr)
		if err != nil {
			t.Fatalf("POST through proxy: %v\nproxy output:\n%s", err, out.String())
		}
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200\nproxy output:\n%s", status, out.String())
		}
		if got := upstreamCount.Load(); got != 1 {
			t.Errorf("upstream request count = %d, want 1", got)
		}
		if !tlsSeen.Load() {
			t.Error("upstream did not observe a TLS connection: the client-hello spec must not break remote termination")
		}
		if got := dialCount.Load(); got < 1 {
			t.Errorf("RC server dial count = %d, want >= 1", got)
		}
	})

	t.Run("Unhonourable", func(t *testing.T) {
		pki := generateTestPKI(t)
		caPath, _, _, _, _ := pki.writeFiles(t)

		handler, upstreamCount := newCountingHandler(nil)
		upstreamAddr := startTLSUpstream(t, handler, pki.serverCertPEM, pki.serverKeyPEM)

		rcAddr, dialCount := startCountingRCServer(t)

		sub, out := startProxyCmd(t,
			"-upstream", "https://"+upstreamAddr,
			"-rc-server", rcAddr,
			"-rc-terminate-tls",
			"-rc-target-ca", "file:"+caPath,
			"-rc-target-max-version", "1.2",
			"-rc-target-client-hello", unhonourableSpec,
			"-bind", "127.0.0.1:0",
			"-retry", "0",
			"-circuit-breaker=false",
		)
		proxyAddr := sub.waitForListeningAddr(t, out)

		status, elapsed, err := rcE2EPost(t, proxyAddr)
		if elapsed > 15*time.Second {
			t.Fatalf("request took %s; an unhonourable spec must fail promptly, not hang", elapsed)
		}
		if err == nil && status == http.StatusOK {
			t.Fatalf("request succeeded with an unhonourable client-hello spec; the engine must fail closed")
		}
		if got := upstreamCount.Load(); got != 0 {
			t.Errorf("upstream request count = %d, want 0 (unhonourable spec must fail before HTTP)", got)
		}
		if got := dialCount.Load(); got < 1 {
			t.Errorf("RC server dial count = %d, want >= 1 (the dial must be attempted before the handshake is rejected)", got)
		}
	})
}
