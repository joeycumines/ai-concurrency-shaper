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

package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/rcclient"
)

// This file owns the provider's RemoteControl.NetConn dial configuration:
// the -transport mode selection, the -sesame-endpoint URL form and its
// mTLS material, the -sesame-token-env credential resolution, and the raw
// -rc-server/-rc-channel-*/-rc-target-* channel construction.

// resolveRC builds the provider's RemoteControl.NetConn dialer when
// -rc-server or -sesame-endpoint is configured. Validation is fail-closed:
// contradictory or unusable dial options are rejected here at startup, and
// the credential the channel needs (the sesame token) is resolved once here
// so a missing or empty secret fails before any socket dials. The
// underlying gRPC channel is lazy, so an unreachable RC server does not fail
// startup; dial errors surface at request time, matching the transport's
// existing semantics.
func (p *Provider) resolveRC() error {
	if p.Transport != "" && p.Transport != "sesame" && p.Transport != "direct" {
		return fmt.Errorf("invalid -transport: %q (must be direct or sesame)", p.Transport)
	}
	// Repeated resolution starts from the operator's current inputs. Values
	// derived by an earlier resolve must be removed first, or a later direct
	// resolve would be rejected by the state of a former sesame endpoint.
	// The channel credential is one of those derived values: it survives an
	// -sesame-token-env removal otherwise, attaching a stale secret to the
	// next channel (or failing every RPC when the new channel is plaintext).
	// So is the previous resolve's dialer: it belongs to inputs that may have
	// changed, and a failure below must not leave it installed.
	p.rcToken = ""
	p.closeRCDialer()
	p.resetDerivedSesameState()
	// direct is an enforced mode, not a synonym for the default: any dial
	// configuration alongside it is a contradiction, and honoring it
	// silently would route upstream connections through RemoteControl
	// while the operator asked for direct dialing.
	if p.Transport == "direct" && (p.RCServer != "" || p.SesameEndpoint != "" || p.rcFlagSet()) {
		return errors.New("transport direct conflicts with remote-control or sesame dial options; remove -transport=direct or the dial options")
	}
	if err := p.resolveSesameEndpoint(); err != nil {
		return err
	}
	p.sesameMaterialPropagation()
	if p.Transport == "sesame" && p.RCServer == "" {
		return errors.New("transport sesame requires -rc-server or -sesame-endpoint")
	}

	if p.RCServer == "" {
		if p.rcFlagSet() {
			return errors.New("rc dial options require -rc-server")
		}
		return nil
	}
	if p.SesameTokenEnv != "" {
		// The token rides per-RPC credentials that refuse plaintext
		// channels, so resolving it against a plaintext channel would
		// only move the failure to the first request. Reject the
		// pairing here, where the operator sees it.
		if !p.RCChannelTLS {
			return errors.New("-sesame-token-env requires a TLS sesame channel (use -sesame-endpoint https://host:port or -rc-channel-tls)")
		}
		token, ok := os.LookupEnv(p.SesameTokenEnv)
		if !ok {
			return fmt.Errorf("-sesame-token-env %q: environment variable is not set", p.SesameTokenEnv)
		}
		token = strings.TrimSpace(token)
		if token == "" {
			return fmt.Errorf("-sesame-token-env %q: environment variable is empty", p.SesameTokenEnv)
		}
		// The value is held for the channel credential and never
		// logged: errors name the variable, not the secret.
		p.rcToken = token
	}
	if p.RCDialTimeout < 0 {
		return errors.New("rc dial timeout must be >= 0")
	}
	channel := rcclient.ChannelConfig{
		Server:     p.RCServer,
		TLS:        p.RCChannelTLS,
		Cert:       p.RCChannelCert,
		Key:        p.RCChannelKey,
		CA:         p.RCChannelCA,
		ServerName: p.RCChannelServerName,
		Insecure:   p.RCChannelInsecure,
		Token:      p.rcToken,
	}
	// The client-hello spec configures the remote-termination handshake. When
	// termination is not requested, say that first: parsing the value can
	// fail on a path that the operator would only have to fix after also
	// learning the flag combination is meaningless.
	if !p.RCTerminateTLS && strings.TrimSpace(p.RCTargetClientHello) != "" {
		return errors.New("-rc-target-client-hello requires -rc-terminate-tls")
	}
	clientHello, err := rcclient.ParseClientHello(p.RCTargetClientHello)
	if err != nil {
		return err
	}
	dial := rcclient.DialConfig{
		Timeout:          p.RCDialTimeout,
		TerminateTLS:     p.RCTerminateTLS,
		TargetServerName: p.RCTargetServerName,
		ALPN:             p.RCTargetALPN,
		MinVersion:       p.RCTargetMinVersion,
		MaxVersion:       p.RCTargetMaxVersion,
		ClientHello:      clientHello,
		TargetInsecure:   p.RCTargetInsecure,
		TargetCA:         p.RCTargetCA,
		TargetCert:       p.RCTargetCert,
		TargetKey:        p.RCTargetKey,
		ProxyHops:        p.RCProxyHop,
		FlowControl:      p.RCFlowControl,
		OpportunisticTLS: p.RCOpportunisticTLS,
		WindowSize:       p.RCWindowSize,
	}
	dialer, err := rcclient.NewDialer(channel, dial)
	if err != nil {
		return err
	}
	p.rcDialer = dialer
	return nil
}

// closeRCDialer releases the dialer a previous resolve installed, leaving the
// provider with no live channel. resolveRC calls it first, so a resolve that
// fails never retains a dialer built from earlier inputs, and Config.Close
// calls it to release every channel.
func (p *Provider) closeRCDialer() {
	if p.rcDialer != nil {
		_ = p.rcDialer.Close()
		p.rcDialer = nil
	}
}

// resolveSesameEndpoint validates -sesame-endpoint as an RC dial target and
// applies its scheme to the channel TLS decision. Two accepted shapes:
// http(s)://host[:port] URLs (scheme decides TLS; a path, query, fragment,
// or userinfo would corrupt the dial target and is rejected) and bare
// host[:port] values (TLS on, matching the sesame endpoint's TLS-by-default
// posture). An explicitly set -rc-channel-tls conflicts with the endpoint's
// scheme instead of being silently clobbered, and -rc-server alongside the
// endpoint is rejected: both name the same dial target. The conflicts read the
// live fields, which after the derived-state reset hold only operator flags or
// a programmatic caller's direct assignments: both are input, and neither may
// be silently overwritten.
func (p *Provider) resolveSesameEndpoint() error {
	if p.SesameEndpoint == "" {
		return nil
	}
	if p.RCServer != "" {
		return errors.New("-rc-server and -sesame-endpoint both configure the sesame channel dial target; configure only one")
	}
	// The value is validated as a dial target, and its bytes never reach an
	// error: a mistyped URL can carry userinfo, and the endpoint is logged at
	// startup by main.go, so an error that echoed it would publish the
	// credential into the journal. Every message names the flag and the
	// defect instead.
	tlsOn := true
	endpoint := strings.TrimSpace(p.SesameEndpoint)
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return errors.New("invalid -sesame-endpoint: not a parseable URL")
		}
		switch u.Scheme {
		case "https":
		case "http":
			tlsOn = false
		default:
			// The scheme slot is operator text: a password pasted ahead of
			// "://" lands there. Report its length, never its bytes.
			return fmt.Errorf(
				"invalid -sesame-endpoint: unsupported scheme (%d bytes; want http or https)",
				len(u.Scheme),
			)
		}
		if u.User != nil {
			return errors.New("invalid -sesame-endpoint: a dial target must not contain userinfo")
		}
		if err := validateSesameHostPort(u.Host); err != nil {
			return err
		}
		if u.Path != "" && u.Path != "/" {
			return errors.New("invalid -sesame-endpoint: a dial target must not contain a path")
		}
		if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return errors.New("invalid -sesame-endpoint: a dial target must not contain a query or fragment")
		}
		p.RCServer = u.Host
		p.derivedSesame.server = u.Host
	} else {
		if endpoint == "" {
			return errors.New("invalid -sesame-endpoint: missing host")
		}
		if strings.ContainsAny(endpoint, "/?#@") {
			return errors.New("invalid -sesame-endpoint: must be host[:port] or http(s)://host[:port], without userinfo, a path, a query, or a fragment")
		}
		if err := validateSesameHostPort(endpoint); err != nil {
			return err
		}
		p.RCServer = endpoint
		p.derivedSesame.server = endpoint
	}
	// A true RCChannelTLS is input (the flag, or a direct assignment); a false
	// one is indistinguishable from unset unless the flag recorded it.
	if p.RCChannelTLS && !tlsOn {
		return errors.New("-sesame-endpoint conflicts with -rc-channel-tls=true: the endpoint selects a plaintext channel")
	}
	if value, explicit := p.explicitFlags["rc-channel-tls"]; explicit && !isTrueBool(value) && tlsOn {
		return errors.New("-sesame-endpoint conflicts with -rc-channel-tls=false: the endpoint requires a TLS channel")
	}
	if tlsOn && !p.RCChannelTLS {
		p.RCChannelTLS = true
		p.derivedSesame.channelTLS = true
	}
	return nil
}

// validateSesameHostPort checks a -sesame-endpoint dial target, shared by the
// URL form (its host component) and the bare form so both accept exactly the
// same shapes: host or host:port, where a present port is a number in
// 1..65535 and an IPv6 host is bracketed, with or without a port. The value is
// never echoed.
func validateSesameHostPort(hostport string) error {
	host := hostport
	if h, port, err := net.SplitHostPort(hostport); err == nil {
		host = h
		if port == "" {
			return errors.New("invalid -sesame-endpoint: missing port")
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return errors.New("invalid -sesame-endpoint: port must be a number in 1..65535")
		}
		if strings.Contains(host, ":") && !isIPv6Literal(host) {
			return errors.New("invalid -sesame-endpoint: bracketed host must be an IPv6 address")
		}
	} else if strings.HasPrefix(hostport, "[") && strings.HasSuffix(hostport, "]") {
		host = hostport[1 : len(hostport)-1]
		if !isIPv6Literal(host) {
			return errors.New("invalid -sesame-endpoint: bracketed host must be an IPv6 address")
		}
	} else if strings.ContainsAny(hostport, ":[]") {
		// A colon that SplitHostPort rejects is a malformed host:port (a
		// scheme typo such as "http:host:8080" lands here too, since it
		// carries no "://"), as is an unbalanced bracket.
		return errors.New("invalid -sesame-endpoint: malformed host:port")
	}
	if host == "" {
		return errors.New("invalid -sesame-endpoint: missing host")
	}
	for _, c := range host {
		if c <= ' ' || c == 0x7f {
			return errors.New("invalid -sesame-endpoint: host must not contain whitespace or control characters")
		}
	}
	return nil
}

// isIPv6Literal reports whether host is an unzoned IPv6 literal, 4-in-6
// included. A zone is refused because the gRPC dial target cannot carry one.
func isIPv6Literal(host string) bool {
	a, err := netip.ParseAddr(host)
	return err == nil && a.Is6() && a.Zone() == ""
}

// rcFlagSet reports whether any RC dial option other than -rc-server itself
// was configured, so a stray option without -rc-server is rejected rather
// than silently ignored. The client-hello check trims whitespace to match
// ParseClientHello, for which a whitespace-only value is the engine default
// (nil): without the trim a whitespace-only value would error here yet
// absorb silently when -rc-server is set.
func (p *Provider) rcFlagSet() bool {
	return p.RCChannelTLS || p.RCChannelCert != "" || p.RCChannelKey != "" ||
		p.RCChannelCA != "" || p.RCChannelServerName != "" || p.RCChannelInsecure ||
		p.RCDialTimeout != 0 || p.RCTerminateTLS || p.RCTargetServerName != "" ||
		len(p.RCTargetALPN) != 0 || p.RCTargetMinVersion != "" || p.RCTargetMaxVersion != "" ||
		strings.TrimSpace(p.RCTargetClientHello) != "" || p.RCTargetInsecure ||
		p.RCTargetCA != "" || p.RCTargetCert != "" || p.RCTargetKey != "" ||
		len(p.RCProxyHop) != 0 || p.RCFlowControl || p.RCOpportunisticTLS || p.RCWindowSize != 0 ||
		p.SesameEndpoint != "" || p.SesameCACert != "" || p.SesameClientCert != "" ||
		p.SesameClientKey != "" || p.SesameServerName != "" || p.SesameTokenEnv != ""
}

// derivedSesameFields records the sesame/RCServer field values a resolve
// computed from other inputs (as opposed to reading them from operator flags
// or a caller's direct assignment). An empty string (or false) means the field
// was not derived.
type derivedSesameFields struct {
	server            string
	channelTLS        bool
	channelCA         string
	channelCert       string
	channelKey        string
	channelServerName string
}

// resetDerivedSesameState clears the provider fields the previous resolve
// derived, but only while they still hold the derived value: a value the
// caller assigned since is input and survives, as does any value read from an
// operator flag (never derived). Reassigning exactly the derived value is
// indistinguishable from leaving it, so it is cleared like derived state —
// most visibly for RCChannelTLS, whose only derived value is true.
func (p *Provider) resetDerivedSesameState() {
	d := p.derivedSesame
	if d.server != "" && p.RCServer == d.server {
		p.RCServer = ""
	}
	if d.channelTLS && p.RCChannelTLS {
		p.RCChannelTLS = false
	}
	if d.channelCA != "" && p.RCChannelCA == d.channelCA {
		p.RCChannelCA = ""
	}
	if d.channelCert != "" && p.RCChannelCert == d.channelCert {
		p.RCChannelCert = ""
	}
	if d.channelKey != "" && p.RCChannelKey == d.channelKey {
		p.RCChannelKey = ""
	}
	if d.channelServerName != "" && p.RCChannelServerName == d.channelServerName {
		p.RCChannelServerName = ""
	}
	p.derivedSesame = derivedSesameFields{}
}

// sesameMaterialPropagation derives the -rc-channel-* mTLS fields from the
// current -sesame-* material, deferring to every explicitly configured
// channel option. It applies regardless of whether the dial target came from
// -sesame-endpoint or a plain -rc-server: both address the same sesame
// endpoint channel. Repeated resolution is re-entrant: fields derived by an
// earlier resolve are recomputed, so changed or removed sesame material is
// reflected instead of being frozen by its first derived value.
func (p *Provider) sesameMaterialPropagation() {
	// Each field is filled only from a non-empty -sesame-* input and only
	// while the channel field is empty. The empty-field condition is what
	// preserves a value a programmatic caller assigned directly (no Parse, so
	// no explicit-flag record): it is not derived state, and the reset above
	// has already removed whatever a previous resolve derived.
	if !p.explicitProviderFlag("rc-channel-ca") && p.SesameCACert != "" && p.RCChannelCA == "" {
		p.RCChannelCA = normalizeMaterial(p.SesameCACert)
		p.derivedSesame.channelCA = p.RCChannelCA
	}
	if !p.explicitProviderFlag("rc-channel-cert") && p.SesameClientCert != "" && p.RCChannelCert == "" {
		p.RCChannelCert = normalizeMaterial(p.SesameClientCert)
		p.derivedSesame.channelCert = p.RCChannelCert
	}
	if !p.explicitProviderFlag("rc-channel-key") && p.SesameClientKey != "" && p.RCChannelKey == "" {
		p.RCChannelKey = normalizeMaterial(p.SesameClientKey)
		p.derivedSesame.channelKey = p.RCChannelKey
	}
	if !p.explicitProviderFlag("rc-channel-server-name") && p.SesameServerName != "" && p.RCChannelServerName == "" {
		p.RCChannelServerName = p.SesameServerName
		p.derivedSesame.channelServerName = p.RCChannelServerName
	}
}

// explicitProviderFlag reports whether the operator set the named
// provider-scoped flag, independently of that flag's current value.
func (p *Provider) explicitProviderFlag(name string) bool {
	_, ok := p.explicitProviderFlags[name]
	return ok
}

// normalizeMaterial accepts a -sesame-* material value as either an explicit
// file:/base64: reference or a bare filesystem path (the common operator
// spelling) and returns the canonical form rcclient parses. A value carrying
// any other prefix is treated as a bare path and gets "file:" prepended, so a
// typo such as "fil:/x" becomes the path "fil:/x": rcclient's material parser
// then reports the unreadable path rather than silently reinterpreting the
// value, and the error still names the path the operator wrote.
func normalizeMaterial(v string) string {
	if v == "" || strings.HasPrefix(v, "file:") || strings.HasPrefix(v, "base64:") {
		return v
	}
	return "file:" + v
}
