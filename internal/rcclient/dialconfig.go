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
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/joeycumines/sesame/rc"
	"github.com/joeycumines/sesame/rc/netconn"
	sesameproxy "github.com/joeycumines/sesame/rc/proxy"
	sesametls "github.com/joeycumines/sesame/rc/tls"
	"github.com/joeycumines/sesame/type/netaddr"
)

// DialConfig models the NetConnRequest.dial surface of a RemoteControl.NetConn
// client as flag-friendly values. All material fields use the file:/base64:
// convention of DecodeMaterial, versions are parsed by ParseTLSVersion, the
// upstream TLS fingerprint is a wire-level ClientHelloSpec parsed by
// ParseClientHello, and each ProxyHops entry is parsed by ParseProxyHop.
type DialConfig struct {
	// Timeout bounds the remote dial (the dial message's timeout). Zero
	// leaves it unset.
	Timeout time.Duration

	// TerminateTLS requests TLS termination at the RC server. The Target*
	// fields below configure that handshake and are rejected unless
	// TerminateTLS is set.
	TerminateTLS     bool
	TargetServerName string
	ALPN             []string
	MinVersion       string
	MaxVersion       string
	// ClientHello is the decomposed, wire-level ClientHello specification
	// for the upstream TLS handshake. Nil means the engine default.
	ClientHello    *sesametls.ClientHelloSpec
	TargetInsecure bool
	TargetCA       string
	TargetCert     string
	TargetKey      string

	// ProxyHops configures egress proxy chaining ahead of the target dial.
	ProxyHops []string

	// FlowControl and OpportunisticTLS advertise the corresponding stream
	// capabilities; any capability switches the connection to the in-stream
	// control path. WindowSize is the flow-control window (0 = server
	// default) and requires FlowControl.
	FlowControl      bool
	OpportunisticTLS bool
	WindowSize       int
}

// Validate checks the configuration without connecting anywhere.
func (c *DialConfig) Validate() error {
	if _, err := c.tlsOptions(); err != nil {
		return err
	}
	if _, err := c.proxyOptions(); err != nil {
		return err
	}
	if c.WindowSize < 0 {
		return errors.New("rcclient: window size must not be negative")
	}
	if c.WindowSize > math.MaxInt32 {
		// Capabilities renders WindowSize as int32; a larger int would
		// overflow on the wire, so reject it fail-closed here.
		return errors.New("rcclient: window size exceeds int32 range")
	}
	if c.WindowSize != 0 && !c.FlowControl {
		return errors.New("rcclient: window size requires flow control")
	}
	return nil
}

// tlsOptions renders the flag values into the wire TLSOptions for remote
// termination, or nil when termination is not requested.
func (c *DialConfig) tlsOptions() (*sesametls.TLSOptions, error) {
	if !c.TerminateTLS {
		if c.TargetServerName != "" || len(c.ALPN) != 0 || c.MinVersion != "" ||
			c.MaxVersion != "" || c.ClientHello != nil ||
			c.TargetInsecure || c.TargetCA != "" || c.TargetCert != "" || c.TargetKey != "" {
			return nil, errors.New("rcclient: target TLS options require remote TLS termination (-rc-terminate-tls)")
		}
		return nil, nil
	}

	opts := &sesametls.TLSOptions{
		ServerName:         c.TargetServerName,
		AlpnProtocols:      c.ALPN,
		InsecureSkipVerify: c.TargetInsecure,
	}

	if c.MinVersion != "" {
		v, err := ParseTLSVersion(c.MinVersion)
		if err != nil {
			return nil, fmt.Errorf("rcclient: min version: %w", err)
		}
		opts.MinVersion = v
	}
	if c.MaxVersion != "" {
		v, err := ParseTLSVersion(c.MaxVersion)
		if err != nil {
			return nil, fmt.Errorf("rcclient: max version: %w", err)
		}
		opts.MaxVersion = v
	}

	if c.ClientHello != nil {
		if err := netconn.ValidateClientHelloSpec(c.ClientHello); err != nil {
			return nil, fmt.Errorf("rcclient: client hello spec: %w", err)
		}
		opts.ClientHello = c.ClientHello
	}

	ca, err := DecodeMaterial(c.TargetCA)
	if err != nil {
		return nil, fmt.Errorf("rcclient: target CA: %w", err)
	}
	opts.CaCertificates = ca

	cert, err := DecodeMaterial(c.TargetCert)
	if err != nil {
		return nil, fmt.Errorf("rcclient: target cert: %w", err)
	}
	key, err := DecodeMaterial(c.TargetKey)
	if err != nil {
		return nil, fmt.Errorf("rcclient: target key: %w", err)
	}
	if (cert != nil) != (key != nil) {
		return nil, errors.New("rcclient: target client cert and key must both be set or both unset")
	}
	opts.ClientCertificate = cert
	opts.ClientPrivateKey = key

	return opts, nil
}

// proxyOptions renders ProxyHops into the wire ProxyOptions, or nil when no
// hops are configured.
func (c *DialConfig) proxyOptions() (*sesameproxy.ProxyOptions, error) {
	if len(c.ProxyHops) == 0 {
		return nil, nil
	}
	var hops []*sesameproxy.ProxyHop
	for i, raw := range c.ProxyHops {
		hop, err := ParseProxyHop(raw)
		if err != nil {
			return nil, fmt.Errorf("rcclient: proxy hop %d: %w", i, err)
		}
		hops = append(hops, hop)
	}
	return &sesameproxy.ProxyOptions{Hops: hops}, nil
}

// Capabilities renders the capability flags into the wire Capabilities, or nil
// when no capability is enabled.
func (c *DialConfig) Capabilities() *rc.NetConnRequest_Capabilities {
	if !c.FlowControl && !c.OpportunisticTLS {
		return nil
	}
	return &rc.NetConnRequest_Capabilities{
		SupportsFlowControl:      c.FlowControl,
		SupportsOpportunisticTls: c.OpportunisticTLS,
		InitialWindowSize:        int32(c.WindowSize),
	}
}

// ParseTLSVersion maps a version name to the wire enum.
// Accepted forms: "1.0", "1.1", "1.2", "1.3", and the "tls"-prefixed
// variants ("tls1.2", "TLS1.3").
func ParseTLSVersion(s string) (sesametls.TLSVersion, error) {
	lower := strings.ToLower(s)
	lower = strings.TrimPrefix(lower, "tls")
	switch lower {
	case "1.0":
		return sesametls.TLSVersion_TLS_1_0, nil
	case "1.1":
		return sesametls.TLSVersion_TLS_1_1, nil
	case "1.2":
		return sesametls.TLSVersion_TLS_1_2, nil
	case "1.3":
		return sesametls.TLSVersion_TLS_1_3, nil
	}
	return 0, fmt.Errorf("unknown TLS version %q (want 1.0, 1.1, 1.2, or 1.3)", s)
}

// ParseClientHello parses a wire-level ClientHelloSpec from a flag value.
// Accepted forms: file:PATH (reads the file), base64:DATA (decodes), or
// inline protojson (starts with '{'). The spec is decoded strictly: unknown
// fields are rejected. An empty value returns nil (engine default).
func ParseClientHello(value string) (*sesametls.ClientHelloSpec, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, nil
	}
	var data []byte
	switch {
	case strings.HasPrefix(trimmed, "{"):
		// Inline protojson.
		data = []byte(trimmed)
	case strings.HasPrefix(value, "file:"):
		path := strings.TrimPrefix(value, "file:")
		if path == "" {
			return nil, errors.New("rcclient: client hello spec: empty path in file: value")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("rcclient: client hello spec: %w", err)
		}
		data = b
	case strings.HasPrefix(value, "base64:"):
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "base64:"))
		if err != nil {
			return nil, fmt.Errorf("rcclient: client hello spec: decoding base64: %w", err)
		}
		data = b
	default:
		// Report the size only, never the value, matching the DecodeMaterial
		// and hop-parse no-echo discipline.
		return nil, fmt.Errorf("rcclient: client hello spec must be file:PATH, base64:DATA, or inline protojson (got %d bytes)", len(value))
	}
	spec := &sesametls.ClientHelloSpec{}
	if err := protojson.Unmarshal(data, spec); err != nil {
		return nil, fmt.Errorf("rcclient: client hello spec: %w", err)
	}
	return spec, nil
}

// ParseProxyHop parses one proxy hop specification:
//
//	TYPE@ADDRESS[;user=U][;pass=P][;header=H]
//
// TYPE is http-connect or socks5 (case-insensitive). ADDRESS is
// host:port. The optional key=value pairs set credentials or a custom
// Proxy-Authorization header.
//
// Errors never echo the raw spec: it may carry a proxy password or an
// auth header, and a parse failure must not leak credentials into the
// startup error and the journal. They name only the offending token, and
// not even that where a token can hold credential bytes: URL-style misuse
// (socks5://user:pass@host) puts credentials ahead of the first '@', so
// the unknown-type error reports a byte count instead, DecodeMaterial-style.
func ParseProxyHop(s string) (*sesameproxy.ProxyHop, error) {
	rest, after, ok := strings.Cut(s, "@")
	if !ok {
		return nil, errors.New("proxy hop: want TYPE@ADDRESS")
	}

	var hopType sesameproxy.ProxyHop_Type
	switch strings.ToLower(strings.TrimSpace(rest)) {
	case "http-connect", "http_connect", "connect":
		hopType = sesameproxy.ProxyHop_HTTP_CONNECT
	case "socks5":
		hopType = sesameproxy.ProxyHop_SOCKS5
	default:
		return nil, fmt.Errorf("proxy hop: unknown type (%d bytes before '@'; want http-connect or socks5)", len(strings.TrimSpace(rest)))
	}

	parts := strings.Split(after, ";")
	addr := strings.TrimSpace(parts[0])
	if addr == "" {
		return nil, errors.New("proxy hop: empty address")
	}

	hop := &sesameproxy.ProxyHop{
		Type: hopType,
		Address: &netaddr.NetAddr{
			Network: "tcp",
			Address: addr,
		},
	}

	for _, kv := range parts[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			// kv carries no '=' and therefore no value; naming it is safe.
			return nil, fmt.Errorf("proxy hop: option %q: want key=value", kv)
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "user", "username":
			hop.Username = v
		case "pass", "password":
			hop.Password = v
		case "header", "auth-header":
			hop.AuthHeader = v
		default:
			return nil, fmt.Errorf("proxy hop: unknown option %q", k)
		}
	}

	return hop, nil
}
