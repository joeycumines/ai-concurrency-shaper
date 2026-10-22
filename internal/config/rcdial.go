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
	"net/url"
	"os"
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
		// A successful earlier resolve may have installed a dialer for an RC
		// target that was since removed. Close it so the config does not
		// retain a live channel after RC dialing is disabled.
		if p.rcDialer != nil {
			_ = p.rcDialer.Close()
			p.rcDialer = nil
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
	// Close the dialer from any prior resolution so a repeated
	// ResolveAndValidate never orphans its gRPC channel.
	if p.rcDialer != nil {
		_ = p.rcDialer.Close()
	}
	p.rcDialer = dialer
	return nil
}

// resolveSesameEndpoint validates -sesame-endpoint as an RC dial target and
// applies its scheme to the channel TLS decision. Two accepted shapes:
// http(s)://host[:port] URLs (scheme decides TLS; a path, query, fragment,
// or userinfo would corrupt the dial target and is rejected) and bare
// host[:port] values (TLS on, matching the sesame endpoint's TLS-by-default
// posture). An explicitly set -rc-channel-tls conflicts with the endpoint's
// scheme instead of being silently clobbered, and -rc-server alongside the
// endpoint is rejected: both name the same dial target.
func (p *Provider) resolveSesameEndpoint() error {
	if p.SesameEndpoint == "" {
		return nil
	}
	// The direct guard has already established that -rc-server is not
	// present. Here a non-explicit RCServer cannot be operator input.
	if p.explicitProviderFlag("rc-server") {
		return errors.New("-rc-server and -sesame-endpoint both configure the sesame channel dial target; configure only one")
	}
	tlsOn := true
	if strings.Contains(p.SesameEndpoint, "://") {
		u, err := url.Parse(p.SesameEndpoint)
		if err != nil {
			return fmt.Errorf("invalid -sesame-endpoint %q: %w", p.SesameEndpoint, err)
		}
		switch u.Scheme {
		case "https":
		case "http":
			tlsOn = false
		default:
			return fmt.Errorf(
				"invalid -sesame-endpoint %q: unsupported scheme %q (want http or https)",
				p.SesameEndpoint, u.Scheme,
			)
		}
		if u.Hostname() == "" {
			return fmt.Errorf("invalid -sesame-endpoint %q: missing host", p.SesameEndpoint)
		}
		if u.Path != "" && u.Path != "/" {
			return fmt.Errorf("invalid -sesame-endpoint %q: a dial target must not contain a path", p.SesameEndpoint)
		}
		if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil {
			return fmt.Errorf("invalid -sesame-endpoint %q: a dial target must not contain a query, fragment, or userinfo", p.SesameEndpoint)
		}
		p.RCServer = u.Host
	} else {
		if strings.TrimSpace(p.SesameEndpoint) == "" {
			return errors.New("invalid -sesame-endpoint: missing host")
		}
		if strings.ContainsAny(p.SesameEndpoint, "/?#") {
			return fmt.Errorf(
				"invalid -sesame-endpoint %q: must be host[:port] or http(s)://host[:port]",
				p.SesameEndpoint,
			)
		}
		p.RCServer = p.SesameEndpoint
	}
	if value, explicit := p.explicitFlags["rc-channel-tls"]; explicit && isTrueBool(value) != tlsOn {
		if tlsOn {
			return errors.New("-sesame-endpoint conflicts with -rc-channel-tls=false: the endpoint requires a TLS channel")
		}
		return errors.New("-sesame-endpoint conflicts with -rc-channel-tls=true: the endpoint selects a plaintext channel")
	}
	p.RCChannelTLS = tlsOn
	return nil
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

// resetDerivedSesameState clears provider fields derived by an earlier
// resolve whenever their corresponding operator flag was not explicit. It
// leaves explicit -rc-server and -rc-channel-* values untouched so they
// remain authoritative across repeated resolution.
func (p *Provider) resetDerivedSesameState() {
	if !p.explicitProviderFlag("rc-server") {
		p.RCServer = ""
	}
	if !p.explicitProviderFlag("rc-channel-tls") {
		p.RCChannelTLS = false
	}
	if !p.explicitProviderFlag("rc-channel-ca") {
		p.RCChannelCA = ""
	}
	if !p.explicitProviderFlag("rc-channel-cert") {
		p.RCChannelCert = ""
	}
	if !p.explicitProviderFlag("rc-channel-key") {
		p.RCChannelKey = ""
	}
	if !p.explicitProviderFlag("rc-channel-server-name") {
		p.RCChannelServerName = ""
	}
}

// sesameMaterialPropagation derives the -rc-channel-* mTLS fields from the
// current -sesame-* material, deferring to every explicitly configured
// channel option. It applies regardless of whether the dial target came from
// -sesame-endpoint or a plain -rc-server: both address the same sesame
// endpoint channel. Repeated resolution is re-entrant: fields derived by an
// earlier resolve are recomputed, so changed or removed sesame material is
// reflected instead of being frozen by its first derived value.
func (p *Provider) sesameMaterialPropagation() {
	if !p.explicitProviderFlag("rc-channel-ca") {
		p.RCChannelCA = normalizeMaterial(p.SesameCACert)
	}
	if !p.explicitProviderFlag("rc-channel-cert") {
		p.RCChannelCert = normalizeMaterial(p.SesameClientCert)
	}
	if !p.explicitProviderFlag("rc-channel-key") {
		p.RCChannelKey = normalizeMaterial(p.SesameClientKey)
	}
	if !p.explicitProviderFlag("rc-channel-server-name") {
		p.RCChannelServerName = p.SesameServerName
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
// spelling) and returns the canonical form rcclient parses. A malformed
// prefix such as "fil:/x" is left untouched so rcclient's material parser
// reports it instead of silently reinterpreting it.
func normalizeMaterial(v string) string {
	if v == "" || strings.HasPrefix(v, "file:") || strings.HasPrefix(v, "base64:") {
		return v
	}
	return "file:" + v
}
