package auth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestDeriveModeGoogleHost(t *testing.T) {
	tests := map[string]AuthMode{
		"generativelanguage.googleapis.com":      AuthGoogAPIKey,
		"GENERATIVELANGUAGE.googleapis.COM":      AuthGoogAPIKey,
		"eu.generativelanguage.googleapis.com":   AuthBearer,
		"generativelanguage.googleapis.com.evil": AuthBearer,
		"api.anthropic.com":                      AuthXAPIKey,
		"api.openai.com":                         AuthBearer,
	}
	for host, want := range tests {
		if got := DeriveMode(host); got != want {
			t.Errorf("DeriveMode(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestResolveModeAutoGoogleHost(t *testing.T) {
	got, err := ResolveMode(AuthAuto, "generativelanguage.googleapis.com")
	if err != nil {
		t.Fatal(err)
	}
	if got != AuthGoogAPIKey {
		t.Fatalf("ResolveMode(auto, google host) = %q, want %q", got, AuthGoogAPIKey)
	}
}

func TestGoogAPIKeyPolicyValidate(t *testing.T) {
	missing := &AuthPolicy{Mode: AuthGoogAPIKey}
	if err := missing.Validate(); err == nil {
		t.Fatal("goog-api-key without a secret source passed validation")
	}
	ok := &AuthPolicy{Mode: AuthGoogAPIKey, Secret: NewStaticSecretSource("synthetic-key")}
	if err := ok.Validate(); err != nil {
		t.Fatalf("goog-api-key with a secret: %v", err)
	}
}

func TestApplyUpstreamAuthenticationGoogAPIKey(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://generativelanguage.googleapis.com/v1beta/models/m:generateContent", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer inbound")
	req.Header.Set("X-Goog-Api-Key", "inbound-goog")
	policy := &AuthPolicy{Mode: AuthGoogAPIKey, Secret: NewStaticSecretSource("synthetic-key")}
	if err := ApplyUpstreamAuthentication(context.Background(), req, policy); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("X-Goog-Api-Key"); got != "synthetic-key" {
		t.Errorf("X-Goog-Api-Key = %q, want the provider secret", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want every inbound credential stripped", got)
	}
}

func TestRedactSensitiveURLOAuthToken(t *testing.T) {
	u, err := url.Parse("https://h/x?oauth_token=synthetic-token&alt=sse&Oauth_Token=other")
	if err != nil {
		t.Fatal(err)
	}
	got := RedactSensitiveURL(u).String()
	if strings.Contains(got, "synthetic-token") || strings.Contains(got, "other") {
		t.Fatalf("redacted URL still carries the token: %s", got)
	}
	if !strings.Contains(got, "oauth_token=%5BREDACTED%5D") {
		t.Errorf("redacted URL = %s, want oauth_token replaced by the placeholder", got)
	}
	if !strings.Contains(got, "alt=sse") {
		t.Errorf("redacted URL = %s, want unrelated parameters preserved", got)
	}
}
