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
	"strings"
	"testing"
)

// closeDialer releases the provider's dialer between resolves so a test
// never leaks a channel.
func closeDialer(t *testing.T, p *Provider) {
	t.Helper()
	if d := p.RCDialer(); d != nil {
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestResolveAndValidate_ReassignedDerivedFieldsSurvive pins that a value the
// caller assigns after an earlier resolve derived that field is the caller's,
// not stale derived state. Before the fix the next resolve cleared every
// field the previous one had marked derived, erasing the new assignment.
func TestResolveAndValidate_ReassignedDerivedFieldsSurvive(t *testing.T) {
	caPEM, clientCertPEM, clientKeyPEM := generateTestCerts(t)
	first := materialPaths(t, t.TempDir(), caPEM, clientCertPEM, clientKeyPEM)
	second := materialPaths(t, t.TempDir(), caPEM, clientCertPEM, clientKeyPEM)

	t.Run("dial target", func(t *testing.T) {
		cfg, err := Parse([]string{
			"-upstream=https://api.example.com",
			"-sesame-endpoint=https://127.0.0.1:50051",
		})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if err := cfg.ResolveAndValidate(); err != nil {
			t.Fatalf("first ResolveAndValidate: %v", err)
		}
		p := cfg.Providers[0]
		closeDialer(t, p)
		p.SesameEndpoint = ""
		p.RCServer = "127.0.0.1:60051"
		if err := cfg.ResolveAndValidate(); err != nil {
			t.Fatalf("second ResolveAndValidate: %v", err)
		}
		defer closeDialer(t, p)
		if p.RCServer != "127.0.0.1:60051" {
			t.Fatalf("RCServer = %q, want the caller's 127.0.0.1:60051", p.RCServer)
		}
	})

	t.Run("channel material", func(t *testing.T) {
		cfg, err := Parse([]string{
			"-upstream=https://api.example.com",
			"-sesame-endpoint=https://127.0.0.1:50051",
			"-sesame-ca-cert=" + first.ca,
		})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if err := cfg.ResolveAndValidate(); err != nil {
			t.Fatalf("first ResolveAndValidate: %v", err)
		}
		p := cfg.Providers[0]
		closeDialer(t, p)
		p.SesameCACert = ""
		p.RCChannelCA = "file:" + second.ca
		if err := cfg.ResolveAndValidate(); err != nil {
			t.Fatalf("second ResolveAndValidate: %v", err)
		}
		defer closeDialer(t, p)
		if want := "file:" + second.ca; p.RCChannelCA != want {
			t.Fatalf("RCChannelCA = %q, want the caller's %q", p.RCChannelCA, want)
		}
	})
}

// TestResolveAndValidate_ProgrammaticSesameConflicts pins that a dial target
// or channel-TLS value assigned directly (no Parse, so no explicit-flag
// record) conflicts with -sesame-endpoint exactly as the flag would, instead
// of being silently overwritten — a direct RCChannelTLS=true was downgraded to
// a plaintext channel by an http:// endpoint.
func TestResolveAndValidate_ProgrammaticSesameConflicts(t *testing.T) {
	for name, tc := range map[string]struct {
		assign  func(p *Provider)
		wantErr string
	}{
		"direct RCServer": {
			assign: func(p *Provider) {
				p.RCServer = "127.0.0.1:60051"
				p.SesameEndpoint = "https://127.0.0.1:50051"
			},
			wantErr: "only one",
		},
		"direct RCChannelTLS with plaintext endpoint": {
			assign: func(p *Provider) {
				p.RCChannelTLS = true
				p.SesameEndpoint = "http://127.0.0.1:50051"
			},
			wantErr: "conflicts with -rc-channel-tls=true",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]string{"-upstream=https://api.example.com"})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			tc.assign(cfg.Providers[0])
			err = cfg.ResolveAndValidate()
			if err == nil {
				closeDialer(t, cfg.Providers[0])
				t.Fatalf("ResolveAndValidate = nil, want %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}

	// A direct RCChannelTLS=true that agrees with an https endpoint is still
	// the caller's: removing the endpoint must not take it away.
	cfg, err := Parse([]string{"-upstream=https://api.example.com"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p := cfg.Providers[0]
	p.RCChannelTLS = true
	p.SesameEndpoint = "https://127.0.0.1:50051"
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	closeDialer(t, p)
	p.SesameEndpoint = ""
	p.RCServer = "127.0.0.1:60051"
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("second ResolveAndValidate: %v", err)
	}
	defer closeDialer(t, p)
	if !p.RCChannelTLS {
		t.Fatal("RCChannelTLS = false, want the caller's true")
	}
}

// TestResolveAndValidate_SesameEndpointFormsAgree pins that the URL and bare
// forms of -sesame-endpoint accept and refuse the same host[:port] shapes:
// the port is optional, but a present one must be a dialable 1..65535 number,
// and a bracketed IPv6 host is accepted with or without a port. Before the
// fix the URL form took "host:" and out-of-range ports verbatim and the bare
// form refused "[::1]" that the URL form accepted. No error echoes the value.
func TestResolveAndValidate_SesameEndpointFormsAgree(t *testing.T) {
	const secret = "s3cr3tvalue"
	for _, tc := range []struct {
		hostport string
		wantRC   string
		wantErr  string
	}{
		{hostport: "example.com", wantRC: "example.com"},
		{hostport: "example.com:443", wantRC: "example.com:443"},
		{hostport: "[::1]", wantRC: "[::1]"},
		{hostport: "[::1]:443", wantRC: "[::1]:443"},
		{hostport: secret + ":", wantErr: "missing port"},
		{hostport: secret + ":65536", wantErr: "port"},
		{hostport: secret + ":0", wantErr: "port"},
		{hostport: "[" + secret + "]", wantErr: "invalid -sesame-endpoint"},
		// url.Parse refuses these itself, so only the shared prefix is
		// common to both forms; the property is that both reject.
		{hostport: "[" + secret + "]:8080", wantErr: "invalid -sesame-endpoint"},
		{hostport: "[127.0.0.1]:8080", wantErr: "invalid -sesame-endpoint"},
		{hostport: secret + ":+443", wantErr: "invalid -sesame-endpoint"},
	} {
		for _, form := range []string{"bare", "url"} {
			endpoint := tc.hostport
			if form == "url" {
				endpoint = "https://" + tc.hostport
			}
			cfg, err := Parse([]string{"-upstream=https://api.example.com", "-sesame-endpoint=" + endpoint})
			if err != nil {
				t.Fatalf("%s %q: Parse: %v", form, endpoint, err)
			}
			err = cfg.ResolveAndValidate()
			if tc.wantErr != "" {
				if err == nil {
					closeDialer(t, cfg.Providers[0])
					t.Errorf("%s %q: ResolveAndValidate = nil, want %q", form, endpoint, tc.wantErr)
					continue
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("%s %q: error = %v, want it to contain %q", form, endpoint, err, tc.wantErr)
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("%s %q: error echoes the value: %v", form, endpoint, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s %q: ResolveAndValidate: %v", form, endpoint, err)
				continue
			}
			if got := cfg.Providers[0].RCServer; got != tc.wantRC {
				t.Errorf("%s %q: RCServer = %q, want %q", form, endpoint, got, tc.wantRC)
			}
			closeDialer(t, cfg.Providers[0])
		}
	}

	// A 4-in-6 literal is an IPv6 host with or without a port, in either form.
	for _, tc := range []struct{ endpoint, wantRC string }{
		{"[::ffff:1.2.3.4]", "[::ffff:1.2.3.4]"},
		{"[::ffff:1.2.3.4]:443", "[::ffff:1.2.3.4]:443"},
		{"https://[::ffff:1.2.3.4]", "[::ffff:1.2.3.4]"},
	} {
		cfg, err := Parse([]string{"-upstream=https://api.example.com", "-sesame-endpoint=" + tc.endpoint})
		if err != nil {
			t.Fatalf("%q: Parse: %v", tc.endpoint, err)
		}
		if err := cfg.ResolveAndValidate(); err != nil {
			t.Errorf("%q: ResolveAndValidate: %v", tc.endpoint, err)
			continue
		}
		if got := cfg.Providers[0].RCServer; got != tc.wantRC {
			t.Errorf("%q: RCServer = %q, want %q", tc.endpoint, got, tc.wantRC)
		}
		closeDialer(t, cfg.Providers[0])
	}

	// A zoned IPv6 host cannot be carried by the gRPC dial target, so it is
	// refused up front rather than at dial time.
	for _, endpoint := range []string{"[fe80::1%en0]:8443", "[fe80::1%en0]", "https://[fe80::1%25en0]:8443", "https://[fe80::1%25en0]"} {
		cfg, err := Parse([]string{"-upstream=https://api.example.com", "-sesame-endpoint=" + endpoint})
		if err != nil {
			t.Fatalf("%q: Parse: %v", endpoint, err)
		}
		err = cfg.ResolveAndValidate()
		if err == nil {
			closeDialer(t, cfg.Providers[0])
			t.Errorf("%q: ResolveAndValidate = nil, want a zone rejection", endpoint)
		} else if !strings.Contains(err.Error(), "invalid -sesame-endpoint") || strings.Contains(err.Error(), "en0") {
			t.Errorf("%q: error = %v, want a flag-naming error without the value", endpoint, err)
		}
	}

	// Whitespace inside a bare host is not a dialable name.
	cfg, err := Parse([]string{"-upstream=https://api.example.com", "-sesame-endpoint=my " + secret})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err == nil {
		closeDialer(t, cfg.Providers[0])
		t.Error("ResolveAndValidate = nil for a bare host containing whitespace, want a rejection")
	} else if strings.Contains(err.Error(), secret) {
		t.Errorf("error echoes the value: %v", err)
	}
}

// TestResolveAndValidate_FailedReResolveHoldsNoEarlierState pins that a
// repeated resolve which fails keeps no server configuration from the earlier
// successful one, and that runtime objects whose flags were turned off are
// released rather than retained from the earlier resolve.
func TestResolveAndValidate_FailedReResolveHoldsNoEarlierState(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-rc-server-bind", "127.0.0.1:0",
		"-global-concurrency", "4",
		"-circuit-breaker=true",
		"-transcode-continuity",
		"-transcode-continuity-capacity", "8",
		"-transcode-responses-chat",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	p := cfg.Providers[0]
	if cfg.Server.RCServerConfig() == nil || p.GlobalLimiter() == nil || p.Breaker() == nil || p.continuityStore == nil {
		t.Fatal("precondition: the first resolve builds the server config, limiter, breaker, and continuity store")
	}

	// Off switches: a fresh resolve of these inputs builds none of them.
	p.GlobalConcurrency = 0
	p.CBEnabled = false
	p.TranscodeContinuityCap = 16
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("second ResolveAndValidate: %v", err)
	}
	if p.GlobalLimiter() != nil {
		t.Error("GlobalLimiter() retained after -global-concurrency 0")
	}
	if p.Breaker() != nil {
		t.Error("Breaker() retained after -circuit-breaker=false")
	}
	if got := p.continuityStore.Capacity(); got != 16 {
		t.Errorf("continuity store capacity = %d, want the current 16", got)
	}

	// A failing resolve: server TLS options without a bind.
	cfg.Server.RCBind = ""
	cfg.Server.RCTLS = true
	if err := cfg.ResolveAndValidate(); err == nil {
		t.Fatal("third ResolveAndValidate = nil, want the server-option rejection")
	}
	if got := cfg.Server.RCServerConfig(); got != nil {
		t.Fatalf("RCServerConfig() = %+v after a failed resolve, want nil", got)
	}
}

// TestResolveAndValidate_SesameEndpointURLDelimiters pins that the URL form
// rejects an empty fragment exactly like the bare form rejects '#', and that
// a bare value is only parsed as a URL when "://" is its scheme separator:
// "://" after a path delimiter is a malformed bare value, reported as one
// rather than as an empty URL scheme.
func TestResolveAndValidate_SesameEndpointURLDelimiters(t *testing.T) {
	for _, tc := range []struct{ endpoint, wantErr, notErr string }{
		{endpoint: "https://example.com#", wantErr: "fragment"},
		{endpoint: "https://example.com:443#", wantErr: "fragment"},
		{endpoint: "example.com#", wantErr: "fragment"},
		{endpoint: "example.com/http://x", wantErr: "must be host[:port]", notErr: "scheme"},
	} {
		cfg, err := Parse([]string{"-upstream=https://api.example.com", "-sesame-endpoint=" + tc.endpoint})
		if err != nil {
			t.Fatalf("%q: Parse: %v", tc.endpoint, err)
		}
		err = cfg.ResolveAndValidate()
		if err == nil {
			closeDialer(t, cfg.Providers[0])
			t.Errorf("%q: ResolveAndValidate = nil, want %q", tc.endpoint, tc.wantErr)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) || (tc.notErr != "" && strings.Contains(err.Error(), tc.notErr)) {
			t.Errorf("%q: error = %v, want it to contain %q", tc.endpoint, err, tc.wantErr)
		}
	}
}

// TestResolveAndValidate_FailedReResolveDropsChannelCredential pins that a
// repeated resolve which fails keeps neither the earlier channel credential
// nor the dial target derived from the earlier endpoint, whether it fails on
// the provider's own -transport check or before the provider is reached.
// Before the fix the -transport check returned ahead of the reset.
func TestResolveAndValidate_FailedReResolveDropsChannelCredential(t *testing.T) {
	t.Setenv("ACS_TEST_SESAME_TOKEN", "tok3n")
	for _, tc := range []struct {
		name   string
		break_ func(cfg *Config)
	}{
		{"invalid transport", func(cfg *Config) { cfg.Providers[0].Transport = "bogus" }},
		{"server rejection before the provider", func(cfg *Config) { cfg.Server.RCTLS = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]string{
				"-upstream=https://api.example.com",
				"-sesame-endpoint=https://127.0.0.1:50051",
				"-sesame-token-env=ACS_TEST_SESAME_TOKEN",
			})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if err := cfg.ResolveAndValidate(); err != nil {
				t.Fatalf("first ResolveAndValidate: %v", err)
			}
			p := cfg.Providers[0]
			if p.rcToken != "tok3n" || p.RCServer != "127.0.0.1:50051" {
				t.Fatalf("precondition: token %q server %q", p.rcToken, p.RCServer)
			}
			tc.break_(cfg)
			if err := cfg.ResolveAndValidate(); err == nil {
				closeDialer(t, p)
				t.Fatal("second ResolveAndValidate = nil, want a rejection")
			}
			if p.rcToken != "" {
				t.Error("the channel credential survived a failed resolve")
			}
			if p.RCDialer() != nil {
				t.Error("a dialer survived a failed resolve")
			}
		})
	}

	cfg, err := Parse([]string{
		"-upstream=https://api.example.com",
		"-sesame-endpoint=https://127.0.0.1:50051",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	p := cfg.Providers[0]
	closeDialer(t, p)
	p.Transport = "bogus"
	if err := cfg.ResolveAndValidate(); err == nil {
		t.Fatal("second ResolveAndValidate = nil, want the -transport rejection")
	}
	if p.RCServer != "" || p.RCChannelTLS {
		t.Errorf("RCServer %q RCChannelTLS %v survived a failed resolve, want the derived values cleared", p.RCServer, p.RCChannelTLS)
	}
}

// TestResolveAndValidate_DerivedProviderNameFollowsUpstream pins that a
// provider name derived from the upstream host is recomputed when the
// upstream changes, while a configured name survives. Before the fix the
// first derived name was written into Name and frozen there.
func TestResolveAndValidate_DerivedProviderNameFollowsUpstream(t *testing.T) {
	cfg, err := Parse([]string{
		"--provider=acme",
		"-upstream", "https://acme.example.com",
		"-prefix", "/acme",
		"--provider",
		"-upstream", "https://api.anthropic.com",
		"-prefix", "/claude",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("first ResolveAndValidate: %v", err)
	}
	if got := cfg.Providers[1].Name; got != "anthropic" {
		t.Fatalf("precondition: derived name = %q, want anthropic", got)
	}
	cfg.Providers[0].Upstream = "https://other.example.org"
	cfg.Providers[1].Upstream = "https://api.openai.com"
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("second ResolveAndValidate: %v", err)
	}
	if got := cfg.Providers[0].Name; got != "acme" {
		t.Errorf("configured name = %q, want acme to survive", got)
	}
	if got := cfg.Providers[1].Name; got != "openai" {
		t.Errorf("derived name = %q, want openai for the new upstream", got)
	}
}

// TestResolveAndValidate_FailedReResolveClearsDerivedSesameState pins that
// when repeated resolution fails anywhere before resolveRC (for example, in
// basic validation or server configuration), all derived sesame fields are
// reset rather than lingering from the earlier successful resolve.
func TestResolveAndValidate_FailedReResolveClearsDerivedSesameState(t *testing.T) {
	caPEM, clientCertPEM, clientKeyPEM := generateTestCerts(t)
	paths := materialPaths(t, t.TempDir(), caPEM, clientCertPEM, clientKeyPEM)

	for _, tc := range []struct {
		name   string
		break_ func(cfg *Config)
	}{
		{"validation failure before resolveRC (missing upstream)", func(cfg *Config) { cfg.Providers[0].Upstream = "" }},
		{"server rejection before resolveRC", func(cfg *Config) { cfg.Server.RCTLS = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]string{
				"-upstream=https://api.example.com",
				"-sesame-endpoint=https://127.0.0.1:50051",
				"-sesame-ca-cert=" + paths.ca,
				"-sesame-client-cert=" + paths.cert,
				"-sesame-client-key=" + paths.key,
			})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if err := cfg.ResolveAndValidate(); err != nil {
				t.Fatalf("first ResolveAndValidate: %v", err)
			}
			p := cfg.Providers[0]
			if p.RCServer == "" || !p.RCChannelTLS || p.RCChannelCA == "" {
				t.Fatalf("precondition: derived sesame fields not set: %+v", p.derivedSesame)
			}
			closeDialer(t, p)
			tc.break_(cfg)
			if err := cfg.ResolveAndValidate(); err == nil {
				t.Fatal("second ResolveAndValidate = nil, want error")
			}
			if p.RCServer != "" || p.RCChannelTLS || p.RCChannelCA != "" || p.RCChannelCert != "" || p.RCChannelKey != "" {
				t.Errorf("derived sesame state survived failed resolve: server=%q tls=%v ca=%q cert=%q key=%q",
					p.RCServer, p.RCChannelTLS, p.RCChannelCA, p.RCChannelCert, p.RCChannelKey)
			}
		})
	}
}
