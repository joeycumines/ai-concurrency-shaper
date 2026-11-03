package transcode

import (
	"context"
	"net/http"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
)

// geminiTestSecret is a fixed secret source for policy fixtures.
type geminiTestSecret string

func (s geminiTestSecret) Secret(context.Context) (string, error) {
	return string(s), nil
}

func TestFromProviderAuthForUpstreamAdaptation(t *testing.T) {
	const secret = geminiTestSecret("synthetic-secret")
	tests := []struct {
		name   string
		mode   auth.AuthMode
		target UpstreamProtocol
		want   AuthMode
		// wantVersion is checked only for the Messages target.
		wantVersion string
	}{
		{name: "bearer to chat", mode: auth.AuthBearer, target: UpstreamChatCompletions, want: AuthBearer},
		{name: "bearer to responses", mode: auth.AuthBearer, target: UpstreamResponses, want: AuthBearer},
		{name: "bearer to messages", mode: auth.AuthBearer, target: UpstreamMessages, want: AuthXAPIKey, wantVersion: auth.DefaultAnthropicVersion},
		{name: "bearer to gemini", mode: auth.AuthBearer, target: UpstreamGemini, want: AuthBearer},
		{name: "x-api-key to chat", mode: auth.AuthXAPIKey, target: UpstreamChatCompletions, want: AuthBearer},
		{name: "x-api-key to responses", mode: auth.AuthXAPIKey, target: UpstreamResponses, want: AuthBearer},
		{name: "x-api-key to messages", mode: auth.AuthXAPIKey, target: UpstreamMessages, want: AuthXAPIKey, wantVersion: "2024-01-01"},
		{name: "x-api-key to gemini", mode: auth.AuthXAPIKey, target: UpstreamGemini, want: AuthGoogAPIKey},
		{name: "api-key to chat", mode: auth.AuthAPIKey, target: UpstreamChatCompletions, want: AuthAPIKey},
		{name: "api-key to responses", mode: auth.AuthAPIKey, target: UpstreamResponses, want: AuthAPIKey},
		{name: "api-key to messages", mode: auth.AuthAPIKey, target: UpstreamMessages, want: AuthAPIKey},
		{name: "api-key to gemini", mode: auth.AuthAPIKey, target: UpstreamGemini, want: AuthGoogAPIKey},
		{name: "goog-api-key to chat", mode: auth.AuthGoogAPIKey, target: UpstreamChatCompletions, want: AuthBearer},
		{name: "goog-api-key to responses", mode: auth.AuthGoogAPIKey, target: UpstreamResponses, want: AuthBearer},
		{name: "goog-api-key to messages", mode: auth.AuthGoogAPIKey, target: UpstreamMessages, want: AuthXAPIKey, wantVersion: auth.DefaultAnthropicVersion},
		{name: "goog-api-key to gemini", mode: auth.AuthGoogAPIKey, target: UpstreamGemini, want: AuthGoogAPIKey},
		{name: "custom header is unchanged", mode: auth.AuthCustomHeader, target: UpstreamGemini, want: AuthCustomHeader},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := &auth.AuthPolicy{Mode: tt.mode, Secret: secret, CustomHeader: "X-Vendor-Key"}
			// Only the x-api-key mount carries a version; every other row
			// relies on the default the adaptation supplies for Messages.
			if tt.mode == auth.AuthXAPIKey {
				policy.AnthropicVersion = "2024-01-01"
			}
			got := FromProviderAuthForUpstream(policy, tt.target)
			if got.Mode != tt.want {
				t.Fatalf("Mode = %q, want %q", got.Mode, tt.want)
			}
			if tt.wantVersion != "" && got.AnthropicVersion != tt.wantVersion {
				t.Fatalf("AnthropicVersion = %q, want %q", got.AnthropicVersion, tt.wantVersion)
			}
		})
	}
}

func TestFromProviderAuthForUpstreamNone(t *testing.T) {
	if got := FromProviderAuthForUpstream(nil, UpstreamGemini); got.Mode != AuthNone {
		t.Fatalf("nil policy Mode = %q, want %q", got.Mode, AuthNone)
	}
	policy := &auth.AuthPolicy{Mode: auth.AuthNone}
	if got := FromProviderAuthForUpstream(policy, UpstreamGemini); got.Mode != AuthNone {
		t.Fatalf("none policy Mode = %q, want %q", got.Mode, AuthNone)
	}
}

func TestExtractInboundCredentialGoogAPIKey(t *testing.T) {
	tests := []struct {
		name    string
		header  http.Header
		want    Credential
		wantErr bool
	}{
		{
			name:   "goog api key alone",
			header: http.Header{"X-Goog-Api-Key": []string{"gkey"}},
			want:   Credential{Kind: CredentialGoogAPIKey, Value: "gkey"},
		},
		{
			name: "equal to bearer collapses",
			header: http.Header{
				"Authorization":  []string{"Bearer same"},
				"X-Goog-Api-Key": []string{"same"},
			},
			want: Credential{Kind: CredentialBearer, Value: "same"},
		},
		{
			name: "different from bearer is rejected",
			header: http.Header{
				"Authorization":  []string{"Bearer a"},
				"X-Goog-Api-Key": []string{"b"},
			},
			wantErr: true,
		},
		{
			name:   "blank goog api key is ignored",
			header: http.Header{"X-Goog-Api-Key": []string{"   "}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractInboundCredential(tt.header)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ExtractInboundCredential = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("credential = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestApplyTargetAuthenticationGoogAPIKey(t *testing.T) {
	ctx := context.Background()
	inbound := Credential{}

	t.Run("gemini target sets the goog header and strips the inbound one", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test/v1beta/models/m:generateContent", nil)
		req.Header.Set("X-Goog-Api-Key", "inbound-key")
		req.Header.Set("Authorization", "Bearer inbound-bearer")
		policy := AuthPolicy{Mode: AuthAuto, Secret: geminiTestSecret("provider-key")}
		if err := ApplyTargetAuthentication(ctx, req, UpstreamGemini, policy, inbound); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("X-Goog-Api-Key"); got != "provider-key" {
			t.Errorf("X-Goog-Api-Key = %q, want the provider secret", got)
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want the inbound bearer stripped", got)
		}
	})

	t.Run("chat target strips the inbound goog header", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test/v1/chat/completions", nil)
		req.Header.Set("X-Goog-Api-Key", "inbound-key")
		policy := AuthPolicy{Mode: AuthBearer, Secret: geminiTestSecret("provider-key")}
		if err := ApplyTargetAuthentication(ctx, req, UpstreamChatCompletions, policy, inbound); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("X-Goog-Api-Key"); got != "" {
			t.Errorf("X-Goog-Api-Key = %q, want it stripped before a non-Gemini target", got)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer provider-key" {
			t.Errorf("Authorization = %q, want the provider bearer", got)
		}
	})

	t.Run("goog mode sets the goog header", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test/v1beta/models/m:generateContent", nil)
		policy := AuthPolicy{Mode: AuthGoogAPIKey, Secret: geminiTestSecret("provider-key")}
		if err := ApplyTargetAuthentication(ctx, req, UpstreamGemini, policy, inbound); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("X-Goog-Api-Key"); got != "provider-key" {
			t.Errorf("X-Goog-Api-Key = %q, want the provider secret", got)
		}
	})
}
