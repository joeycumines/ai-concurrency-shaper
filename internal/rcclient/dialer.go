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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/joeycumines/sesame/rc"
	"github.com/joeycumines/sesame/rc/netconn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// ChannelConfig configures the gRPC channel to the RC server.
type ChannelConfig struct {
	// Server is the RC server address (host:port). Required.
	Server string
	// TLS enables TLS on the channel itself (distinct from remote target
	// TLS termination).
	TLS        bool
	Cert       string // client certificate (file:|base64:)
	Key        string // client private key (file:|base64:)
	CA         string // server CA (file:|base64:)
	ServerName string // TLS SNI override for the channel
	Insecure   bool   // InsecureSkipVerify on the channel TLS
}

// Validate checks the channel configuration without connecting.
func (c *ChannelConfig) Validate() error {
	if c.Server == "" {
		return errors.New("rcclient: channel server address is required")
	}
	cert, err := DecodeMaterial(c.Cert)
	if err != nil {
		return fmt.Errorf("rcclient: channel cert: %w", err)
	}
	key, err := DecodeMaterial(c.Key)
	if err != nil {
		return fmt.Errorf("rcclient: channel key: %w", err)
	}
	if (cert != nil) != (key != nil) {
		return errors.New("rcclient: channel client cert and key must both be set or both unset")
	}
	if _, err := DecodeMaterial(c.CA); err != nil {
		return fmt.Errorf("rcclient: channel CA: %w", err)
	}
	if !c.TLS && (cert != nil || key != nil || c.CA != "" || c.ServerName != "" || c.Insecure) {
		return errors.New("rcclient: channel TLS options require -rc-channel-tls")
	}
	return nil
}

// tlsConfig builds the channel TLS credentials, or nil when TLS is disabled.
func (c *ChannelConfig) tlsConfig() (*tls.Config, error) {
	if !c.TLS {
		return nil, nil
	}

	cfg := &tls.Config{
		ServerName:         c.ServerName,
		InsecureSkipVerify: c.Insecure,
	}

	ca, err := DecodeMaterial(c.CA)
	if err != nil {
		return nil, err
	}
	if len(ca) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("rcclient: invalid channel CA PEM")
		}
		cfg.RootCAs = pool
	}

	certPEM, err := DecodeMaterial(c.Cert)
	if err != nil {
		return nil, err
	}
	keyPEM, err := DecodeMaterial(c.Key)
	if err != nil {
		return nil, err
	}
	if len(certPEM) > 0 && len(keyPEM) > 0 {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("rcclient: channel client cert/key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

// Dialer dials upstream connections through a RemoteControl.NetConn stream.
// It implements the DialContext signature that http.Transport.DialContext
// expects.
type Dialer struct {
	client     *netconn.Client
	remoteTerm bool
	conn       *grpc.ClientConn
	closeOnce  sync.Once
}

// NewDialer validates the configurations, opens the gRPC channel, and
// returns a Dialer ready for use. The caller must call Close when the
// dialer is no longer needed.
func NewDialer(channel ChannelConfig, dial DialConfig) (*Dialer, error) {
	if err := channel.Validate(); err != nil {
		return nil, err
	}
	if err := dial.Validate(); err != nil {
		return nil, err
	}

	opts, err := dial.tlsOptions()
	if err != nil {
		return nil, err
	}
	proxyOpts, err := dial.proxyOptions()
	if err != nil {
		return nil, err
	}

	// Channel credentials.
	var dialOpts []grpc.DialOption
	if channel.TLS {
		tlsCfg, err := channel.tlsConfig()
		if err != nil {
			return nil, err
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(channel.Server, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("rcclient: grpc channel to %s: %w", channel.Server, err)
	}

	client := &netconn.Client{
		API:          rc.NewRemoteControlClient(conn),
		Timeout:      dial.Timeout,
		TLS:          opts,
		Proxy:        proxyOpts,
		Capabilities: dial.Capabilities(),
	}

	return &Dialer{
		client:     client,
		remoteTerm: dial.TerminateTLS,
		conn:       conn,
	}, nil
}

// DialContext dials address through the RC stream. network is passed
// through to the remote dialer.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.client.DialContext(ctx, network, address)
}

// RemoteTerminate reports whether remote TLS termination is configured.
// When true the http.Transport must not perform its own TLS handshake over
// the returned connection.
func (d *Dialer) RemoteTerminate() bool { return d.remoteTerm }

// Close releases the gRPC channel. It is idempotent and safe for concurrent
// use.
func (d *Dialer) Close() error {
	d.closeOnce.Do(func() { _ = d.conn.Close() })
	return nil
}
