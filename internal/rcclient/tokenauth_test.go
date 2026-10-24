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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/sesame/rc/netconn"
	"github.com/joeycumines/sesame/sesamepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// selfSignedServerCert generates one self-signed server certificate and key
// for a token-enforcing test gRPC server. Clients dial with the same PEM as
// their CA and Insecure verification, so no SANs are needed.
func selfSignedServerCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "rc-test-server"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// tokenTestServer is a TLS gRPC server running the real NetConn service
// behind a stream interceptor that requires exactly wantAuth as the
// authorization metadata on every RPC, recording each value it observes.
type tokenTestServer struct {
	addr  net.Addr
	caPEM string // base64:DATA reference the dialer can use as its CA
	seen  []string
	mu    sync.Mutex
}

func (s *tokenTestServer) observed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func (s *tokenTestServer) record(auth string) {
	s.mu.Lock()
	s.seen = append(s.seen, auth)
	s.mu.Unlock()
}

// startTokenEnforcingServer starts the server for the duration of the test.
func startTokenEnforcingServer(t *testing.T, wantAuth string) *tokenTestServer {
	t.Helper()
	certPEM, keyPEM := selfSignedServerCert(t)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}

	srv := &tokenTestServer{
		caPEM: "base64:" + base64.StdEncoding.EncodeToString(certPEM),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}})),
		grpc.ChainStreamInterceptor(func(handlerSrv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			auth := ""
			if md, ok := metadata.FromIncomingContext(ss.Context()); ok {
				if vals := md.Get("authorization"); len(vals) > 0 {
					auth = vals[0]
				}
			}
			srv.record(auth)
			if auth != wantAuth {
				return status.Error(codes.Unauthenticated, "invalid authorization metadata")
			}
			return handler(handlerSrv, ss)
		}),
	)
	sesamepb.RegisterRemoteControlServer(grpcSrv, &netconn.Server{})
	go func() { _ = grpcSrv.Serve(ln) }()
	t.Cleanup(grpcSrv.Stop)
	srv.addr = ln.Addr()
	return srv
}

// TestDialerTokenCredentials verifies the channel token contract end to end:
// the configured token reaches the server as per-RPC "authorization: Bearer"
// metadata over TLS, a wrong or missing token is rejected by the server, and
// the credentials refuse to ride a plaintext channel.
func TestDialerTokenCredentials(t *testing.T) {
	echoLn := startEchoServer(t)
	echoAddr := echoLn.Addr().String()

	const token = "test-sesame-token"
	server := startTokenEnforcingServer(t, "Bearer "+token)

	tlsChannel := ChannelConfig{
		Server:   server.addr.String(),
		TLS:      true,
		CA:       server.caPEM,
		Insecure: true,
	}

	// Correct token: the RPC passes the interceptor and the dial works.
	d, err := NewDialer(applyToken(tlsChannel, token), DialConfig{})
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	roundTrip(t, d, echoAddr)

	seen := server.observed()
	if len(seen) == 0 || seen[len(seen)-1] != "Bearer "+token {
		t.Fatalf("server observed authorization %v, want %q", seen, "Bearer "+token)
	}

	// Wrong token: the server rejects the RPC before the service runs.
	dWrong, err := NewDialer(applyToken(tlsChannel, "wrong-token"), DialConfig{})
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	t.Cleanup(func() { _ = dWrong.Close() })
	_, err = dWrong.DialContext(context.Background(), "tcp", echoAddr)
	if err == nil {
		t.Fatal("DialContext with wrong token: nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "Unauthenticated") {
		t.Errorf("DialContext with wrong token: error = %v, want Unauthenticated", err)
	}

	// No token against a token-enforcing server: same rejection.
	dNone, err := NewDialer(tlsChannel, DialConfig{})
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	t.Cleanup(func() { _ = dNone.Close() })
	if _, err := dNone.DialContext(context.Background(), "tcp", echoAddr); err == nil {
		t.Fatal("DialContext without token: nil error, want rejection")
	} else if !strings.Contains(err.Error(), "Unauthenticated") {
		t.Errorf("DialContext without token: error = %v, want Unauthenticated", err)
	}

	// A token on a plaintext channel is refused at dialer construction —
	// grpc-go rejects per-RPC credentials that require transport security on
	// an insecure channel — so the secret is never sent in the clear and the
	// misconfiguration surfaces before any request.
	dPlain, err := NewDialer(ChannelConfig{
		Server: server.addr.String(),
		Token:  token,
	}, DialConfig{})
	if err == nil {
		_ = dPlain.Close()
		t.Fatal("NewDialer with token over plaintext: nil error, want credentials refusal")
	}
	if !strings.Contains(err.Error(), "transport level security") {
		t.Errorf("NewDialer with token over plaintext: error = %v, want transport security refusal", err)
	}
}

// applyToken returns a copy of the channel configuration carrying the token,
// keeping each dialer's inputs independent.
func applyToken(c ChannelConfig, token string) ChannelConfig {
	c.Token = token
	return c
}
