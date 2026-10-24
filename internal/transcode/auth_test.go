package transcode

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
)

func TestExtractInboundCredential(t *testing.T) {
	tests := []struct {
		name    string
		header  http.Header
		want    Credential
		wantErr bool
	}{
		{
			name:   "bearer only",
			header: http.Header{"Authorization": []string{"Bearer abc123"}},
			want:   Credential{Kind: CredentialBearer, Value: "abc123"},
		},
		{
			name:   "x-api-key only",
			header: http.Header{"X-Api-Key": []string{"key123"}},
			want:   Credential{Kind: CredentialXAPIKey, Value: "key123"},
		},
		{
			name:   "api-key only",
			header: http.Header{"Api-Key": []string{"key123"}},
			want:   Credential{Kind: CredentialAPIKey, Value: "key123"},
		},
		{
			name: "duplicate equal collapse",
			header: http.Header{
				"Authorization": []string{"Bearer same"},
				"X-Api-Key":     []string{"same"},
			},
			want: Credential{Kind: CredentialBearer, Value: "same"},
		},
		{
			name: "multiple different rejected",
			header: http.Header{
				"Authorization": []string{"Bearer a"},
				"X-Api-Key":     []string{"b"},
			},
			wantErr: true,
		},
		{
			name:    "malformed authorization rejected",
			header:  http.Header{"Authorization": []string{"not-bearer"}},
			wantErr: true,
		},
		{
			name:   "empty is fine",
			header: http.Header{},
			want:   Credential{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractInboundCredential(tt.header)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("credential = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAuthPolicyValidate(t *testing.T) {
	tests := []struct {
		name    string
		policy  AuthPolicy
		target  UpstreamProtocol
		wantErr bool
	}{
		{
			name:    "auto with version for messages",
			policy:  AuthPolicy{Mode: AuthAuto, AnthropicVersion: "2023-06-01", Inbound: true},
			target:  UpstreamMessages,
			wantErr: false,
		},
		{
			name:    "auto without version or secret source",
			policy:  AuthPolicy{Mode: AuthAuto, Inbound: true},
			target:  UpstreamMessages,
			wantErr: true,
		},
		{
			name:    "secret mode without a source",
			policy:  AuthPolicy{Mode: AuthBearer},
			target:  UpstreamResponses,
			wantErr: true,
		},
		{
			name:    "secret mode with inbound",
			policy:  AuthPolicy{Mode: AuthBearer, Inbound: true},
			target:  UpstreamResponses,
			wantErr: false,
		},
		{
			name:    "none does not require version",
			policy:  AuthPolicy{Mode: AuthNone},
			target:  UpstreamMessages,
			wantErr: false,
		},
		{
			name:    "custom header requires name",
			policy:  AuthPolicy{Mode: AuthCustomHeader},
			target:  UpstreamResponses,
			wantErr: true,
		},
		{
			name:    "external signer requires signer",
			policy:  AuthPolicy{Mode: AuthExternalSigner},
			target:  UpstreamResponses,
			wantErr: true,
		},
		{
			name:    "unknown mode",
			policy:  AuthPolicy{Mode: "bogus"},
			target:  UpstreamResponses,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Validate(tt.target)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestApplyTargetAuthenticationAuto(t *testing.T) {
	// Auto for Responses/Chat -> Authorization Bearer.
	req, err := http.NewRequest(http.MethodPost, "http://upstream/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "client-key")
	err = ApplyTargetAuthentication(
		context.Background(),
		req,
		UpstreamResponses,
		AuthPolicy{Mode: AuthAuto, Inbound: true},
		Credential{Kind: CredentialXAPIKey, Value: "client-key"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer client-key" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("X-Api-Key"); got != "" {
		t.Fatalf("X-Api-Key must be stripped, got %q", got)
	}

	// Auto for Messages -> X-Api-Key + anthropic-version.
	req, err = http.NewRequest(http.MethodPost, "http://upstream/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer client-key")
	err = ApplyTargetAuthentication(
		context.Background(),
		req,
		UpstreamMessages,
		AuthPolicy{Mode: AuthAuto, Inbound: true, AnthropicVersion: "2023-06-01"},
		Credential{Kind: CredentialBearer, Value: "client-key"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("X-Api-Key"); got != "client-key" {
		t.Fatalf("X-Api-Key = %q", got)
	}
	if got := req.Header.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Fatalf("Anthropic-Version = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization must be stripped, got %q", got)
	}
}

func TestApplyTargetAuthenticationNeverForwardsForeignHeaders(t *testing.T) {
	// An Anthropic client's headers must never reach an OpenAI upstream.
	req, err := http.NewRequest(http.MethodPost, "http://upstream/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "anthropic-key")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "some-beta")
	err = ApplyTargetAuthentication(
		context.Background(),
		req,
		UpstreamResponses,
		AuthPolicy{Mode: AuthBearer, Inbound: true},
		Credential{Kind: CredentialXAPIKey, Value: "anthropic-key"},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"X-Api-Key", "Anthropic-Version", "Anthropic-Beta"} {
		if got := req.Header.Get(name); got != "" {
			t.Fatalf("%s must be stripped, got %q", name, got)
		}
	}
	if got := req.Header.Get("Authorization"); got != "Bearer anthropic-key" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestApplyTargetAuthenticationNone(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://upstream/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	err = ApplyTargetAuthentication(
		context.Background(),
		req,
		UpstreamMessages,
		AuthPolicy{Mode: AuthNone},
		Credential{Kind: CredentialBearer, Value: "secret"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization must be stripped with AuthNone, got %q", got)
	}
}

func TestApplyTargetAuthenticationCustomHeader(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://upstream/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = ApplyTargetAuthentication(
		context.Background(),
		req,
		UpstreamResponses,
		AuthPolicy{Mode: AuthCustomHeader, CustomHeader: "X-Upstream-Key", Inbound: true},
		Credential{Kind: CredentialBearer, Value: "sekret"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("X-Upstream-Key"); got != "sekret" {
		t.Fatalf("X-Upstream-Key = %q", got)
	}
}

type staticSecret struct{ value string }

func (s staticSecret) Secret(context.Context) (string, error) {
	return s.value, nil
}

type failingSecret struct{}

func (failingSecret) Secret(context.Context) (string, error) {
	return "", errors.New("secret source failed")
}

func TestApplyTargetAuthenticationConfiguredSecret(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://upstream/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = ApplyTargetAuthentication(
		context.Background(),
		req,
		UpstreamResponses,
		AuthPolicy{Mode: AuthBearer, Secret: staticSecret{"cfg-secret"}},
		Credential{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer cfg-secret" {
		t.Fatalf("Authorization = %q", got)
	}

	// Empty configured secret is an error.
	err = ApplyTargetAuthentication(
		context.Background(),
		req,
		UpstreamResponses,
		AuthPolicy{Mode: AuthBearer, Secret: staticSecret{""}},
		Credential{},
	)
	if err == nil {
		t.Fatal("expected empty secret error")
	}

	// Failing secret source is surfaced.
	err = ApplyTargetAuthentication(
		context.Background(),
		req,
		UpstreamResponses,
		AuthPolicy{Mode: AuthBearer, Secret: failingSecret{}},
		Credential{},
	)
	if err == nil || !strings.Contains(err.Error(), "secret source failed") {
		t.Fatalf("err = %v", err)
	}

	// Inbound mode with no inbound credential is an error.
	err = ApplyTargetAuthentication(
		context.Background(),
		req,
		UpstreamResponses,
		AuthPolicy{Mode: AuthBearer, Inbound: true},
		Credential{},
	)
	if err == nil {
		t.Fatal("expected missing inbound credential error")
	}
}

// TestFromProviderAuthForUpstream pins the dialect-adaptation contract of
// inherited provider auth: a bearer mount serving the Messages upstream
// authenticates with X-Api-Key (plus the default Anthropic version unless the
// policy carries one), an x-api-key mount serving the OpenAI upstreams
// authenticates with Bearer, and every other mode passes through verbatim —
// mirroring the auto rule ApplyTargetAuthentication applies at request time,
// so one mount authenticates a model identically whether served natively or
// transcoded.
func TestFromProviderAuthForUpstream(t *testing.T) {
	tests := []struct {
		name   string
		policy *auth.AuthPolicy
		target UpstreamProtocol
		want   AuthPolicy
	}{
		{
			name:   "nil policy",
			policy: nil,
			target: UpstreamChatCompletions,
			want:   AuthPolicy{Mode: AuthNone},
		},
		{
			name:   "none policy",
			policy: &auth.AuthPolicy{Mode: auth.AuthNone},
			target: UpstreamMessages,
			want:   AuthPolicy{Mode: AuthNone},
		},
		{
			name: "bearer to messages adapts with default version",
			policy: &auth.AuthPolicy{
				Mode:   auth.AuthBearer,
				Secret: auth.NewStaticSecretSource("tok"),
			},
			target: UpstreamMessages,
			want: AuthPolicy{
				Mode:             AuthXAPIKey,
				Secret:           auth.NewStaticSecretSource("tok"),
				AnthropicVersion: auth.DefaultAnthropicVersion,
			},
		},
		{
			name: "bearer to messages keeps explicit version",
			policy: &auth.AuthPolicy{
				Mode:             auth.AuthBearer,
				Secret:           auth.NewStaticSecretSource("tok"),
				AnthropicVersion: "2024-01-01",
			},
			target: UpstreamMessages,
			want: AuthPolicy{
				Mode:             AuthXAPIKey,
				Secret:           auth.NewStaticSecretSource("tok"),
				AnthropicVersion: "2024-01-01",
			},
		},
		{
			name: "bearer to responses verbatim",
			policy: &auth.AuthPolicy{
				Mode:   auth.AuthBearer,
				Secret: auth.NewStaticSecretSource("tok"),
			},
			target: UpstreamResponses,
			want: AuthPolicy{
				Mode:   AuthBearer,
				Secret: auth.NewStaticSecretSource("tok"),
			},
		},
		{
			name: "bearer to chat completions verbatim",
			policy: &auth.AuthPolicy{
				Mode:   auth.AuthBearer,
				Secret: auth.NewStaticSecretSource("tok"),
			},
			target: UpstreamChatCompletions,
			want: AuthPolicy{
				Mode:   AuthBearer,
				Secret: auth.NewStaticSecretSource("tok"),
			},
		},
		{
			name: "x-api-key to responses adapts to bearer",
			policy: &auth.AuthPolicy{
				Mode:             auth.AuthXAPIKey,
				Secret:           auth.NewStaticSecretSource("tok"),
				AnthropicVersion: "2023-06-01",
			},
			target: UpstreamResponses,
			want: AuthPolicy{
				Mode:   AuthBearer,
				Secret: auth.NewStaticSecretSource("tok"),
				// The version rides along faithfully: it is inert on the
				// OpenAI upstreams (the header is stripped there) and
				// survives a round trip back to a Messages target.
				AnthropicVersion: "2023-06-01",
			},
		},
		{
			name: "x-api-key to chat completions adapts to bearer",
			policy: &auth.AuthPolicy{
				Mode:             auth.AuthXAPIKey,
				Secret:           auth.NewStaticSecretSource("tok"),
				AnthropicVersion: "2023-06-01",
			},
			target: UpstreamChatCompletions,
			want: AuthPolicy{
				Mode:             AuthBearer,
				Secret:           auth.NewStaticSecretSource("tok"),
				AnthropicVersion: "2023-06-01",
			},
		},
		{
			name: "x-api-key to messages verbatim",
			policy: &auth.AuthPolicy{
				Mode:             auth.AuthXAPIKey,
				Secret:           auth.NewStaticSecretSource("tok"),
				AnthropicVersion: "2023-06-01",
			},
			target: UpstreamMessages,
			want: AuthPolicy{
				Mode:             AuthXAPIKey,
				Secret:           auth.NewStaticSecretSource("tok"),
				AnthropicVersion: "2023-06-01",
			},
		},
		{
			name: "api-key verbatim for every target",
			policy: &auth.AuthPolicy{
				Mode:   auth.AuthAPIKey,
				Secret: auth.NewStaticSecretSource("tok"),
			},
			target: UpstreamMessages,
			want: AuthPolicy{
				Mode:   AuthAPIKey,
				Secret: auth.NewStaticSecretSource("tok"),
			},
		},
		{
			name: "header verbatim for every target",
			policy: &auth.AuthPolicy{
				Mode:         auth.AuthCustomHeader,
				Secret:       auth.NewStaticSecretSource("tok"),
				CustomHeader: "X-Custom-Key",
			},
			target: UpstreamMessages,
			want: AuthPolicy{
				Mode:         AuthCustomHeader,
				Secret:       auth.NewStaticSecretSource("tok"),
				CustomHeader: "X-Custom-Key",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromProviderAuthForUpstream(tt.policy, tt.target)
			if got.Mode != tt.want.Mode {
				t.Errorf("mode = %q, want %q", got.Mode, tt.want.Mode)
			}
			if got.AnthropicVersion != tt.want.AnthropicVersion {
				t.Errorf("anthropic version = %q, want %q", got.AnthropicVersion, tt.want.AnthropicVersion)
			}
			if got.CustomHeader != tt.want.CustomHeader {
				t.Errorf("custom header = %q, want %q", got.CustomHeader, tt.want.CustomHeader)
			}
			if tt.want.Secret == nil {
				if got.Secret != nil {
					t.Fatal("secret = non-nil, want nil")
				}
				return
			}
			if got.Secret == nil {
				t.Fatal("secret = nil, want non-nil")
			}
			gotValue, err := got.Secret.Secret(context.Background())
			if err != nil {
				t.Fatalf("resolve secret: %v", err)
			}
			wantValue, err := tt.want.Secret.Secret(context.Background())
			if err != nil {
				t.Fatalf("resolve want secret: %v", err)
			}
			if gotValue != wantValue {
				t.Errorf("secret = %q, want %q", gotValue, wantValue)
			}
		})
	}
}
