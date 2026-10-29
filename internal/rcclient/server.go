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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/joeycumines/sesame/rc/netconn"
	"github.com/joeycumines/sesame/sesamepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// ServerConfig configures the embedded RC server listener.
type ServerConfig struct {
	// Bind is the listen address (host:port). Required.
	Bind string
	// TLS enables TLS on the listener. When CA is set, client certificates
	// are required and verified (mutual TLS).
	TLS  bool
	CA   string // CA for client cert verification (file:|base64:)
	Cert string // server certificate (file:|base64:)
	Key  string // server private key (file:|base64:)
}

// Validate checks the server configuration.
func (c *ServerConfig) Validate() error {
	if c.Bind == "" {
		return errors.New("rcclient: server bind address is required")
	}
	if !c.TLS {
		if c.CA != "" || c.Cert != "" || c.Key != "" {
			return errors.New("rcclient: server TLS options require -rc-server-tls")
		}
		return nil
	}
	if c.Cert == "" || c.Key == "" {
		return errors.New("rcclient: server TLS requires both cert and key")
	}
	caPEM, err := DecodeMaterial(c.CA)
	if err != nil {
		return fmt.Errorf("rcclient: server CA: %w", err)
	}
	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return errors.New("rcclient: invalid server CA PEM")
		}
	}
	certPEM, err := DecodeMaterial(c.Cert)
	if err != nil {
		return fmt.Errorf("rcclient: server cert: %w", err)
	}
	keyPEM, err := DecodeMaterial(c.Key)
	if err != nil {
		return fmt.Errorf("rcclient: server key: %w", err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("rcclient: rc-server cert/key: %w", err)
	}
	return nil
}

// Server is a running RC server: a gRPC server with RemoteControl.NetConn
// registered, listening on a TCP address.
type Server struct {
	grpcServer *grpc.Server
	listener   net.Listener
}

// NewServer validates the configuration, binds the listener, and starts the
// gRPC server in a background goroutine. The caller must call Stop to shut
// down gracefully.
func NewServer(cfg ServerConfig) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", cfg.Bind)
	if err != nil {
		return nil, fmt.Errorf("rcclient: rc-server bind %s: %w", cfg.Bind, err)
	}

	var serverOpts []grpc.ServerOption
	if cfg.TLS {
		certPEM, err := DecodeMaterial(cfg.Cert)
		if err != nil {
			_ = ln.Close()
			return nil, err
		}
		keyPEM, err := DecodeMaterial(cfg.Key)
		if err != nil {
			_ = ln.Close()
			return nil, err
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("rcclient: rc-server cert/key: %w", err)
		}

		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
		}

		if cfg.CA != "" {
			caPEM, err := DecodeMaterial(cfg.CA)
			if err != nil {
				_ = ln.Close()
				return nil, err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caPEM) {
				_ = ln.Close()
				return nil, errors.New("rcclient: rc-server invalid CA PEM")
			}
			tlsCfg.ClientCAs = pool
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		}

		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}

	grpcServer := grpc.NewServer(serverOpts...)
	sesamepb.RegisterRemoteControlServer(grpcServer, &netconn.Server{})

	go func() { _ = grpcServer.Serve(ln) }()

	return &Server{
		grpcServer: grpcServer,
		listener:   ln,
	}, nil
}

// Addr returns the listener address.
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Stop gracefully stops the server.
func (s *Server) Stop() { s.grpcServer.GracefulStop() }

// StopAfter attempts a graceful stop and force-stops the server when active
// RPCs do not complete within the timeout. It never blocks past the timeout,
// so shutdown cannot hang on a client that holds a stream open.
func (s *Server) StopAfter(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		s.grpcServer.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		s.grpcServer.Stop()
	}
}
