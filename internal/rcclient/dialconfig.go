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
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/joeycumines/sesame/rc"
	sesameproxy "github.com/joeycumines/sesame/rc/proxy"
	sesametls "github.com/joeycumines/sesame/rc/tls"
	"github.com/joeycumines/sesame/type/netaddr"
)

// DialConfig models the NetConnRequest.dial surface of a RemoteControl.NetConn
// client as flag-friendly values. All material fields use the file:/base64:
// convention of DecodeMaterial, versions and cipher suites are parsed by
// ParseTLSVersion and ParseCipherSuite, and each ProxyHops entry is parsed by
// ParseProxyHop.
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
	// Fingerprint names a client fingerprint preset; the server fails closed
	// when it cannot satisfy a preset other than the runtime default.
	Fingerprint    string
	CipherSuites   []string
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
			c.MaxVersion != "" || c.Fingerprint != "" || len(c.CipherSuites) != 0 ||
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

	if c.Fingerprint != "" {
		p, err := ParseFingerprint(c.Fingerprint)
		if err != nil {
			return nil, fmt.Errorf("rcclient: fingerprint: %w", err)
		}
		opts.FingerprintPreset = p
	}

	for _, cs := range c.CipherSuites {
		n, err := ParseCipherSuite(cs)
		if err != nil {
			return nil, fmt.Errorf("rcclient: cipher suite: %w", err)
		}
		opts.CipherSuites = append(opts.CipherSuites, n)
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
		InitialWindowSize:        uint32(c.WindowSize),
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

// ParseCipherSuite parses a single cipher suite identifier. Decimal and
// 0x-prefixed hex forms are accepted; values that do not fit uint16 are
// rejected.
func ParseCipherSuite(s string) (uint32, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 0, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid cipher suite %q: %w", s, err)
	}
	if n > math.MaxUint16 {
		return 0, fmt.Errorf("cipher suite %q exceeds uint16 range", s)
	}
	return uint32(n), nil
}

// ParseFingerprint maps a preset name (case-insensitive, hyphens and
// underscores interchangeable) to the wire enum. The generated enum map
// keys are SCREAMING_SNAKE, so the input is uppercased and hyphens are
// normalized before lookup.
func ParseFingerprint(s string) (sesametls.FingerprintPreset, error) {
	key := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", "_"))
	if key == "" || key == "UNSPECIFIED" || key == "FINGERPRINT_PRESET_UNSPECIFIED" {
		return sesametls.FingerprintPreset_FINGERPRINT_PRESET_UNSPECIFIED, nil
	}
	if v, ok := sesametls.FingerprintPreset_value[key]; ok {
		return sesametls.FingerprintPreset(v), nil
	}
	// Accept the FINGERPRINT_PRESET_ prefix as well.
	if v, ok := sesametls.FingerprintPreset_value["FINGERPRINT_PRESET_"+key]; ok {
		return sesametls.FingerprintPreset(v), nil
	}
	return 0, fmt.Errorf("unknown fingerprint preset %q", s)
}

// ParseProxyHop parses one proxy hop specification:
//
//	TYPE@ADDRESS[;user=U][;pass=P][;header=H]
//
// TYPE is http-connect or socks5 (case-insensitive). ADDRESS is
// host:port. The optional key=value pairs set credentials or a custom
// Proxy-Authorization header.
func ParseProxyHop(s string) (*sesameproxy.ProxyHop, error) {
	rest, after, ok := strings.Cut(s, "@")
	if !ok {
		return nil, fmt.Errorf("proxy hop %q: want TYPE@ADDRESS", s)
	}

	var hopType sesameproxy.ProxyHop_Type
	switch strings.ToLower(strings.TrimSpace(rest)) {
	case "http-connect", "http_connect", "connect":
		hopType = sesameproxy.ProxyHop_HTTP_CONNECT
	case "socks5":
		hopType = sesameproxy.ProxyHop_SOCKS5
	default:
		return nil, fmt.Errorf("proxy hop %q: unknown type %q (want http-connect or socks5)", s, rest)
	}

	parts := strings.Split(after, ";")
	addr := strings.TrimSpace(parts[0])
	if addr == "" {
		return nil, fmt.Errorf("proxy hop %q: empty address", s)
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
			return nil, fmt.Errorf("proxy hop %q: option %q: want key=value", s, kv)
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "user", "username":
			hop.Username = v
		case "pass", "password":
			hop.Password = v
		case "header", "auth-header":
			hop.AuthHeader = v
		default:
			return nil, fmt.Errorf("proxy hop %q: unknown option %q", s, k)
		}
	}

	return hop, nil
}
