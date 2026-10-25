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
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/joeycumines/sesame/rc/netconn"
	"github.com/joeycumines/sesame/sesamepb"
	"github.com/joeycumines/sesame/typepb"
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
	ClientHello    *sesamepb.ClientHelloSpec
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

// Validate checks the configuration without connecting anywhere. The
// remote-termination TLS options are rendered here, so every PEM value is
// parsed and every version pair checked before any socket exists: a malformed
// target CA, certificate, or key is a configuration error, never a per-request
// surprise at the RC server.
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

// tlsOptions renders the flag values into the wire TlsOptions for remote
// termination, or nil when termination is not requested.
//
// The ALPN offer is constrained to http/1.1. Under remote termination the
// proxy dials the upstream through http.Transport.DialTLSContext, whose
// returned connection is not a *tls.Conn, so net/http never learns the
// negotiated protocol and can only speak HTTP/1.1 — it never sends an HTTP/2
// preface. An offer of "h2" would therefore let an upstream select a protocol
// this client cannot use, failing the exchange with a preface mismatch. With
// no offer at all the sesame server sends no ALPN extension (its documented
// contract), which is why an empty list is left empty rather than defaulted.
func (c *DialConfig) tlsOptions() (*sesamepb.TlsOptions, error) {
	if !c.TerminateTLS {
		if c.TargetServerName != "" || len(c.ALPN) != 0 || c.MinVersion != "" ||
			c.MaxVersion != "" || c.ClientHello != nil ||
			c.TargetInsecure || c.TargetCA != "" || c.TargetCert != "" || c.TargetKey != "" {
			return nil, errors.New("rcclient: target TLS options require remote TLS termination (-rc-terminate-tls)")
		}
		return nil, nil
	}
	for _, protocol := range c.ALPN {
		if strings.EqualFold(strings.TrimSpace(protocol), "h2") {
			return nil, errors.New("rcclient: -rc-target-alpn must not offer h2: the remote-terminated connection carries HTTP/1.1 only, so an h2 selection would fail every request with a protocol mismatch")
		}
	}

	opts := &sesamepb.TlsOptions{
		Sni:                c.TargetServerName,
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
	// A floor above its ceiling can never negotiate; the remote server would
	// reject it, so refuse it here where the operator sees which flags
	// disagree.
	if c.MinVersion != "" && c.MaxVersion != "" && opts.MinVersion > opts.MaxVersion {
		return nil, fmt.Errorf(
			"rcclient: -rc-target-min-version %s is above -rc-target-max-version %s",
			c.MinVersion, c.MaxVersion,
		)
	}

	if c.ClientHello != nil {
		if err := netconn.ValidateClientHelloSpec(c.ClientHello); err != nil {
			return nil, fmt.Errorf("rcclient: client hello spec: %w", err)
		}
		if clientHelloOffersH2(c.ClientHello) {
			return nil, errors.New("rcclient: -rc-target-client-hello must not offer h2: the remote-terminated connection carries HTTP/1.1 only, so an h2 selection would fail every request with a protocol mismatch")
		}
		opts.ClientHello = c.ClientHello
	}

	ca, err := DecodeMaterial(c.TargetCA)
	if err != nil {
		return nil, fmt.Errorf("rcclient: target CA: %w", err)
	}
	if len(ca) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("rcclient: invalid target CA PEM")
		}
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
	if cert != nil {
		if _, err := tls.X509KeyPair(cert, key); err != nil {
			return nil, fmt.Errorf("rcclient: target client cert/key: %w", err)
		}
	}
	opts.ClientCertificate = cert
	opts.ClientPrivateKey = key

	return opts, nil
}

// proxyOptions renders ProxyHops into the wire ProxyOptions, or nil when no
// hops are configured.
func (c *DialConfig) proxyOptions() (*sesamepb.ProxyOptions, error) {
	if len(c.ProxyHops) == 0 {
		return nil, nil
	}
	var hops []*sesamepb.ProxyHop
	for i, raw := range c.ProxyHops {
		hop, err := ParseProxyHop(raw)
		if err != nil {
			return nil, fmt.Errorf("rcclient: proxy hop %d: %w", i, err)
		}
		hops = append(hops, hop)
	}
	return &sesamepb.ProxyOptions{Hops: hops}, nil
}

// Capabilities renders the capability flags into the wire Capabilities, or nil
// when no capability is enabled. The caller must have validated first:
// Validate rejects a negative or int32-overflowing WindowSize, and the cast
// below is exact only for validated input. Every in-binary path goes through
// NewDialer, which validates before Capabilities is ever called.
func (c *DialConfig) Capabilities() *sesamepb.NetConnRequest_Capabilities {
	if !c.FlowControl && !c.OpportunisticTLS {
		return nil
	}
	return &sesamepb.NetConnRequest_Capabilities{
		SupportsFlowControl:      c.FlowControl,
		SupportsOpportunisticTls: c.OpportunisticTLS,
		InitialWindowSize:        int32(c.WindowSize),
	}
}

// ParseTLSVersion maps a version name to the wire enum.
// Accepted forms: "1.0", "1.1", "1.2", "1.3", and the "tls"-prefixed
// variants ("tls1.2", "TLS1.3").
func ParseTLSVersion(s string) (sesamepb.TlsVersion, error) {
	lower := strings.ToLower(s)
	lower = strings.TrimPrefix(lower, "tls")
	switch lower {
	case "1.0":
		return sesamepb.TlsVersion_TLS_VERSION_TLS_1_0, nil
	case "1.1":
		return sesamepb.TlsVersion_TLS_VERSION_TLS_1_1, nil
	case "1.2":
		return sesamepb.TlsVersion_TLS_VERSION_TLS_1_2, nil
	case "1.3":
		return sesamepb.TlsVersion_TLS_VERSION_TLS_1_3, nil
	}
	return 0, fmt.Errorf("unknown TLS version %q (want 1.0, 1.1, 1.2, or 1.3)", s)
}

// ParseClientHello parses a wire-level ClientHelloSpec from a flag value.
// Accepted forms: file:PATH (reads the file), base64:DATA (decodes), or
// inline protojson (starts with '{'). The spec is decoded strictly: unknown
// fields are rejected. An empty value returns nil (engine default).
func ParseClientHello(value string) (*sesamepb.ClientHelloSpec, error) {
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
	spec := &sesamepb.ClientHelloSpec{}
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
// startup error and the journal. No error names a token that can hold
// credential bytes: URL-style misuse (socks5://user:pass@host) puts
// credentials ahead of the first '@', and a credential containing '=' splits
// into key/value, so every such error reports the option's position and byte
// length instead, DecodeMaterial-style.
func ParseProxyHop(s string) (*sesamepb.ProxyHop, error) {
	rest, after, ok := strings.Cut(s, "@")
	if !ok {
		return nil, errors.New("proxy hop: want TYPE@ADDRESS")
	}

	var hopType sesamepb.ProxyHop_Type
	switch strings.ToLower(strings.TrimSpace(rest)) {
	case "http-connect", "http_connect", "connect":
		hopType = sesamepb.ProxyHop_TYPE_HTTP_CONNECT
	case "socks5":
		hopType = sesamepb.ProxyHop_TYPE_SOCKS5
	default:
		return nil, fmt.Errorf("proxy hop: unknown type (%d bytes before '@'; want http-connect or socks5)", len(strings.TrimSpace(rest)))
	}

	parts := strings.Split(after, ";")
	addr := strings.TrimSpace(parts[0])
	if addr == "" {
		return nil, errors.New("proxy hop: empty address")
	}

	hop := &sesamepb.ProxyHop{
		Type: hopType,
		Address: &typepb.NetAddr{
			Network: "tcp",
			Address: addr,
		},
	}

	for i, kv := range parts[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			// The segment has no '=' and no value, but it can still be a
			// mistyped credential ("pass;secret") — report its position and
			// size only, never its bytes.
			return nil, fmt.Errorf("proxy hop: option %d (%d bytes): want key=value", i+1, len(kv))
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "user", "username":
			hop.Username = v
		case "pass", "password":
			hop.Password = v
		case "header", "auth-header":
			hop.AuthHeader = v
		default:
			// The key is operator text that may itself be a pasted secret
			// (a password containing '=' splits into key/value), so the
			// error reports its position and size instead of the key.
			return nil, fmt.Errorf("proxy hop: unknown option %d (%d bytes before '='): want user, pass, or header", i+1, len(k))
		}
	}

	return hop, nil
}

// alpnExtensionType is the IANA TLS extension type of application_layer_protocol_negotiation.
const alpnExtensionType = 16

// clientHelloOffersH2 reports whether a wire-level ClientHelloSpec would offer
// "h2" in its ALPN extension. Only a raw extension body can carry an offer —
// the engine-derived (auto) form takes its ALPN list from TlsOptions, which
// tlsOptions constrains — so a malformed body is treated as offering nothing
// and left to the server's own validation.
func clientHelloOffersH2(spec *sesamepb.ClientHelloSpec) bool {
	for _, ext := range spec.GetExtensions() {
		if ext.GetType() != alpnExtensionType {
			continue
		}
		body := ext.GetRaw()
		// The ALPN body is a 2-byte list length followed by 1-byte-length
		// prefixed protocol names.
		if len(body) < 2 {
			continue
		}
		list := body[2:]
		for len(list) > 0 {
			n := int(list[0])
			if n == 0 || 1+n > len(list) {
				break
			}
			if strings.EqualFold(string(list[1:1+n]), "h2") {
				return true
			}
			list = list[1+n:]
		}
	}
	return false
}
