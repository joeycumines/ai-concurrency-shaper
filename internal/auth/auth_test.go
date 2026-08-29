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

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripCredentials(t *testing.T) {
	h := http.Header{}
	for _, name := range []string{
		"Authorization",
		"Proxy-Authorization",
		"X-Api-Key",
		"Api-Key",
		"X-Goog-Api-Key",
		"Anthropic-Version",
		"Anthropic-Beta",
		"X-Amz-Date",
		"x-amz-security-token",
		"X-GOOG-Aip-Url", // non-canonical spelling of an X-Goog-* family key
	} {
		h.Set(name, "value")
	}
	// Headers outside the credential/protocol/cloud families must survive.
	h.Set("Content-Type", "application/json")
	h.Set("X-Request-Id", "abc")
	h.Set("Cookie", "session=1") // display-redacted only; never stripped

	StripCredentials(h)

	for _, name := range []string{
		"Authorization",
		"Proxy-Authorization",
		"X-Api-Key",
		"Api-Key",
		"X-Goog-Api-Key",
		"Anthropic-Version",
		"Anthropic-Beta",
		"X-Amz-Date",
		"X-Amz-Security-Token",
		"X-Goog-Aip-Url",
	} {
		if got := h.Values(name); len(got) != 0 {
			t.Errorf("%s must be stripped, got %q", name, got)
		}
	}
	for name, want := range map[string]string{
		"Content-Type": "application/json",
		"X-Request-Id": "abc",
		"Cookie":       "session=1",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q (non-credential headers must survive)", name, got, want)
		}
	}
}

func TestStripCredentialsNonCanonicalKeys(t *testing.T) {
	// http.Header.Set canonicalizes its argument, so a test populated via
	// Set can never exercise non-canonical map keys. Hand-built headers
	// (raw map assignment, middleware, future transports) can carry them,
	// and Header.Del canonicalizes ITS argument too - so every stored
	// spelling below must be deleted by exact key. Cookie survives by
	// design; Content-Type proves unrelated headers are untouched.
	h := http.Header{
		"authorization":        {"Bearer client-secret"},
		"PROXY-AUTHORIZATION":  {"Basic z"},
		"x-api-key":            {"sk-client"},
		"API-KEY":              {"az-client"},
		"x-goog-api-key":       {"goog-client"},
		"anthropic-version":    {"1999-01-01"},
		"ANTHROPIC-BETA":       {"beta-feature"},
		"x-amz-date":           {"20260101T000000Z"},
		"x-amz-security-token": {"aws-token"},
		"x-goog-signature":     {"sig-value"},
		// Canonical spellings stored alongside must also go.
		"Authorization": {"Bearer canonical-secret"},
		"X-Amz-Date":    {"20260102T000000Z"},
		// Non-credential survivors.
		"Content-Type": {"application/json"},
		"Cookie":       {"session=1"},
	}

	StripCredentials(h)

	for name := range h {
		switch name {
		case "Content-Type", "Cookie":
			continue
		}
		t.Errorf("%q survived StripCredentials (stored spelling must be deleted exactly)", name)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want untouched", got)
	}
	if got := h.Get("Cookie"); got != "session=1" {
		t.Errorf("Cookie = %q, want preserved (never stripped)", got)
	}
}

func TestAuthPolicyValidate(t *testing.T) {
	tests := []struct {
		name    string
		policy  AuthPolicy
		wantErr bool
	}{
		{
			name:   "bearer with source",
			policy: AuthPolicy{Mode: AuthBearer, Secret: NewStaticSecretSource("s")},
		},
		{
			name:    "bearer without source fails closed",
			policy:  AuthPolicy{Mode: AuthBearer},
			wantErr: true,
		},
		{
			name:   "x-api-key with version",
			policy: AuthPolicy{Mode: AuthXAPIKey, Secret: NewStaticSecretSource("s"), AnthropicVersion: "2023-06-01"},
		},
		{
			name:    "x-api-key without version",
			policy:  AuthPolicy{Mode: AuthXAPIKey, Secret: NewStaticSecretSource("s")},
			wantErr: true,
		},
		{
			name:   "none needs no source or version",
			policy: AuthPolicy{Mode: AuthNone},
		},
		{
			name:   "api-key with source",
			policy: AuthPolicy{Mode: AuthAPIKey, Secret: NewStaticSecretSource("s")},
		},
		{
			name:   "custom header with name and source",
			policy: AuthPolicy{Mode: AuthCustomHeader, CustomHeader: "X-Upstream-Key", Secret: NewStaticSecretSource("s")},
		},
		{
			name:    "custom header without name",
			policy:  AuthPolicy{Mode: AuthCustomHeader, Secret: NewStaticSecretSource("s")},
			wantErr: true,
		},
		{
			name:    "custom header with invalid token characters",
			policy:  AuthPolicy{Mode: AuthCustomHeader, CustomHeader: "Bad Header\n", Secret: NewStaticSecretSource("s")},
			wantErr: true,
		},
		{
			name:    "auto must be resolved before use",
			policy:  AuthPolicy{Mode: AuthAuto, Secret: NewStaticSecretSource("s")},
			wantErr: true,
		},
		{
			name:    "unknown mode",
			policy:  AuthPolicy{Mode: "bogus", Secret: NewStaticSecretSource("s")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDeriveMode(t *testing.T) {
	tests := []struct {
		host string
		want AuthMode
	}{
		{"api.anthropic.com", AuthXAPIKey},
		{"API.ANTHROPIC.COM", AuthXAPIKey},
		{"proxy.anthropic.com", AuthXAPIKey},
		{"notanthropic.com", AuthBearer}, // suffix must be dot-delimited
		{"anthropic.com.evil.test", AuthBearer},
		{"api.openai.com", AuthBearer},
		{"localhost", AuthBearer},
		{"[::1]", AuthBearer},
	}
	for _, tt := range tests {
		if got := DeriveMode(tt.host); got != tt.want {
			t.Errorf("DeriveMode(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}

func TestResolveMode(t *testing.T) {
	got, err := ResolveMode(AuthAuto, "api.anthropic.com")
	if err != nil || got != AuthXAPIKey {
		t.Fatalf("ResolveMode(auto, anthropic) = %q, %v", got, err)
	}
	got, err = ResolveMode(AuthAuto, "api.openai.com")
	if err != nil || got != AuthBearer {
		t.Fatalf("ResolveMode(auto, openai) = %q, %v", got, err)
	}
	got, err = ResolveMode(AuthAPIKey, "anything")
	if err != nil || got != AuthAPIKey {
		t.Fatalf("explicit mode must pass through, got %q, %v", got, err)
	}
	if _, err = ResolveMode(AuthAuto, ""); err == nil {
		t.Fatal("auto with empty host must error")
	}
}

// failingSecretSource is a SecretSource whose Secret always returns err, used
// to exercise resolveSecret's source-error branch without a real source.
type failingSecretSource struct{ err error }

func (f failingSecretSource) Secret(context.Context) (string, error) { return "", f.err }

func TestApplyUpstreamAuthentication(t *testing.T) {
	newReq := func(t *testing.T) *http.Request {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "http://upstream/v1/messages", nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}

	t.Run("bearer replaces client credentials", func(t *testing.T) {
		req := newReq(t)
		req.Header.Set("Authorization", "Bearer client-key")
		req.Header.Set("X-Api-Key", "client-key")
		policy := &AuthPolicy{Mode: AuthBearer, Secret: NewStaticSecretSource("cfg-secret")}
		if err := ApplyUpstreamAuthentication(context.Background(), req, policy); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer cfg-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("X-Api-Key"); got != "" {
			t.Fatalf("X-Api-Key must be stripped, got %q", got)
		}
	})

	t.Run("x-api-key sets anthropic-version", func(t *testing.T) {
		req := newReq(t)
		req.Header.Set("Authorization", "Bearer client-key")
		req.Header.Set("Anthropic-Version", "1999-01-01")
		policy := &AuthPolicy{Mode: AuthXAPIKey, Secret: NewStaticSecretSource("sk"), AnthropicVersion: "2023-06-01"}
		if err := ApplyUpstreamAuthentication(context.Background(), req, policy); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("X-Api-Key"); got != "sk" {
			t.Fatalf("X-Api-Key = %q", got)
		}
		if got := req.Header.Get("Anthropic-Version"); got != "2023-06-01" {
			t.Fatalf("Anthropic-Version = %q, want configured value to replace client's", got)
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization must be stripped, got %q", got)
		}
	})

	t.Run("never forwards foreign headers", func(t *testing.T) {
		// An Anthropic-shaped client must not leak protocol headers into a
		// bearer-authenticated upstream request.
		req := newReq(t)
		req.Header.Set("X-Api-Key", "anthropic-key")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		req.Header.Set("Anthropic-Beta", "some-beta")
		req.Header.Set("x-amz-date", "20260101T000000Z")
		policy := &AuthPolicy{Mode: AuthBearer, Secret: NewStaticSecretSource("openai-key")}
		if err := ApplyUpstreamAuthentication(context.Background(), req, policy); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"X-Api-Key", "Anthropic-Version", "Anthropic-Beta", "X-Amz-Date"} {
			if got := req.Header.Get(name); got != "" {
				t.Errorf("%s must be stripped, got %q", name, got)
			}
		}
		if got := req.Header.Get("Authorization"); got != "Bearer openai-key" {
			t.Fatalf("Authorization = %q", got)
		}
	})

	t.Run("none strips without injecting", func(t *testing.T) {
		req := newReq(t)
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		if err := ApplyUpstreamAuthentication(context.Background(), req, &AuthPolicy{Mode: AuthNone}); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization must be stripped in none mode, got %q", got)
		}
		if got := req.Header.Get("Anthropic-Version"); got != "" {
			t.Fatalf("Anthropic-Version must be stripped in none mode, got %q", got)
		}
	})

	t.Run("azure api-key", func(t *testing.T) {
		req := newReq(t)
		req.Header.Set("Authorization", "Bearer whatever")
		policy := &AuthPolicy{Mode: AuthAPIKey, Secret: NewStaticSecretSource("az-key")}
		if err := ApplyUpstreamAuthentication(context.Background(), req, policy); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Api-Key"); got != "az-key" {
			t.Fatalf("Api-Key = %q", got)
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization must be stripped, got %q", got)
		}
	})

	t.Run("gemini custom header re-injectable after cloud-prefix strip", func(t *testing.T) {
		req := newReq(t)
		req.Header.Set("X-Goog-Api-Key", "client-google-key") // stripped as cloud prefix...
		policy := &AuthPolicy{Mode: AuthCustomHeader, CustomHeader: "X-Goog-Api-Key", Secret: NewStaticSecretSource("gateway-google-key")}
		if err := ApplyUpstreamAuthentication(context.Background(), req, policy); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Values("X-Goog-Api-Key"); len(got) != 1 || got[0] != "gateway-google-key" {
			t.Fatalf("X-Goog-Api-Key = %q, want exactly the injected value", got)
		}
	})

	t.Run("empty resolved secret errors loudly", func(t *testing.T) {
		req := newReq(t)
		policy := &AuthPolicy{Mode: AuthBearer, Secret: NewStaticSecretSource("   ")}
		if err := ApplyUpstreamAuthentication(context.Background(), req, policy); err == nil {
			t.Fatal("expected empty secret error")
		}
	})

	t.Run("unresolved auto rejected", func(t *testing.T) {
		req := newReq(t)
		policy := &AuthPolicy{Mode: AuthAuto, Secret: NewStaticSecretSource("s")}
		if err := ApplyUpstreamAuthentication(context.Background(), req, policy); err == nil {
			t.Fatal("expected unresolved auto mode error")
		}
	})

	t.Run("failing secret source surfaces its error", func(t *testing.T) {
		req := newReq(t)
		want := errors.New("secret source exploded")
		policy := &AuthPolicy{Mode: AuthBearer, Secret: failingSecretSource{err: want}}
		if err := ApplyUpstreamAuthentication(context.Background(), req, policy); !errors.Is(err, want) {
			t.Fatalf("ApplyUpstreamAuthentication = %v, want %v", err, want)
		}
	})
}

func TestEnvSecretSource(t *testing.T) {
	const variable = "AUTH_TEST_SECRET_VAR"

	t.Run("set variable returns raw value", func(t *testing.T) {
		t.Setenv(variable, "  sekrit  ")
		got, err := NewEnvSecretSource(variable).Secret(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Sources return raw values; resolveSecret performs the single
		// TrimSpace before injection.
		if got != "  sekrit  " {
			t.Fatalf("Secret() = %q", got)
		}
	})

	t.Run("unset variable names it in the error", func(t *testing.T) {
		t.Setenv(variable, "")
		os.Unsetenv(variable)
		_, err := NewEnvSecretSource(variable).Secret(context.Background())
		if err == nil || !strings.Contains(err.Error(), variable) {
			t.Fatalf("err = %v, want unset-variable error naming %s", err, variable)
		}
	})

	t.Run("empty variable errors", func(t *testing.T) {
		t.Setenv(variable, "   ")
		_, err := NewEnvSecretSource(variable).Secret(context.Background())
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("err = %v, want empty-variable error", err)
		}
	})
}

func TestFileSecretSource(t *testing.T) {
	t.Run("reads and trims", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, []byte("  file-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := NewFileSecretSource(path).Secret(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got != "file-secret" {
			t.Fatalf("Secret() = %q", got)
		}
	})

	t.Run("unreadable file names the path", func(t *testing.T) {
		_, err := NewFileSecretSource(filepath.Join(t.TempDir(), "missing")).Secret(context.Background())
		if err == nil {
			t.Fatal("expected unreadable-file error")
		}
	})

	t.Run("empty file errors", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := NewFileSecretSource(path).Secret(context.Background())
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("err = %v, want empty-file error", err)
		}
	})

	t.Run("blank path errors", func(t *testing.T) {
		if _, err := NewFileSecretSource("").Secret(context.Background()); err == nil {
			t.Fatal("expected blank-path error")
		}
	})
}

func TestStaticSecretSource(t *testing.T) {
	got, err := NewStaticSecretSource("startup-resolved").Secret(context.Background())
	if err != nil || got != "startup-resolved" {
		t.Fatalf("Secret() = %q, %v", got, err)
	}
}

func TestStripCredentialsKeepsDisplayOnlyResponseHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Set-Cookie", "session=abc")
	StripCredentials(h)
	if got := h.Get("Set-Cookie"); got != "session=abc" {
		t.Errorf("StripCredentials removed Set-Cookie: %q", got)
	}
}

func TestRedactSensitiveURL(t *testing.T) {
	t.Run("nil is nil", func(t *testing.T) {
		if got := RedactSensitiveURL(nil); got != nil {
			t.Errorf("RedactSensitiveURL(nil) = %v, want nil", got)
		}
	})
	t.Run("no query returns input unchanged", func(t *testing.T) {
		u := mustParseURL(t, "http://up/v1/messages")
		if got := RedactSensitiveURL(u); got != u {
			t.Errorf("want same *url.URL for queryless input")
		}
	})
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"gemini key", "/v1/models?key=topsecret", "/v1/models?key=%5BREDACTED%5D"},
		{"oauth token", "/p?access_token=tok&x=1", "/p?access_token=%5BREDACTED%5D&x=1"},
		{"case-insensitive key name preserved", "/p?KEY=abc&b=2", "/p?KEY=%5BREDACTED%5D&b=2"},
		{"order and unknowns preserved", "/p?a=1&api_key=k&b=2&apikey=j&c=3", "/p?a=1&api_key=%5BREDACTED%5D&b=2&apikey=%5BREDACTED%5D&c=3"},
		{"repeated keys all redacted", "/p?key=1&key=2", "/p?key=%5BREDACTED%5D&key=%5BREDACTED%5D"},
		{"signature family", "/p?X-Goog-Signature=z&sig=s", "/p?X-Goog-Signature=%5BREDACTED%5D&sig=%5BREDACTED%5D"},
		{"unknown params untouched", "/p?foo=secretvalue&bar=baz", "/p?foo=secretvalue&bar=baz"},
		{"valueless param untouched", "/p?flag&token=t", "/p?flag&token=%5BREDACTED%5D"},
		{"encoded key spelling preserved", "/p?api%5Fkey=x", "/p?api%5Fkey=%5BREDACTED%5D"},
		{"semicolon delimiter", "/p?key=s1;token=s2&x=1", "/p?key=%5BREDACTED%5D;token=%5BREDACTED%5D&x=1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := mustParseURL(t, "http://up"+tt.raw)
			got := RedactSensitiveURL(u)
			if got.String() != "http://up"+tt.want {
				t.Errorf("RedactSensitiveURL(%q).String() = %q, want %q", tt.raw, got.String(), "http://up"+tt.want)
			}
			if got == u && got.RawQuery != u.RawQuery {
				t.Errorf("mutated the input URL in place")
			}
		})
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
