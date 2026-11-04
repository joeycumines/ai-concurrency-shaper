package proxy

import (
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// TestNativeAuthVariantsGeminiAdaptation pins the native authentication table
// for the Gemini dialect and proves it agrees with the transcode adaptation:
// one provider mount authenticates a model the same way whether the model is
// served natively or converted.
func TestNativeAuthVariantsGeminiAdaptation(t *testing.T) {
	secret := auth.NewStaticSecretSource("synthetic-secret")
	modes := []auth.AuthMode{
		auth.AuthBearer,
		auth.AuthXAPIKey,
		auth.AuthAPIKey,
		auth.AuthGoogAPIKey,
	}
	natives := []struct {
		native   transcode.NativeProtocol
		upstream transcode.UpstreamProtocol
	}{
		{transcode.NativeGemini, transcode.UpstreamGemini},
		{transcode.NativeMessages, transcode.UpstreamMessages},
		{transcode.NativeResponses, transcode.UpstreamResponses},
		{transcode.NativeChat, transcode.UpstreamChatCompletions},
	}
	for _, mode := range modes {
		for _, n := range natives {
			t.Run(string(mode)+"/"+string(n.native), func(t *testing.T) {
				policy := &auth.AuthPolicy{Mode: mode, Secret: secret, AnthropicVersion: "2024-01-01"}
				variants := newNativeAuthVariants(policy)
				got := variants.forProtocol(n.native)
				want := transcode.FromProviderAuthForUpstream(policy, n.upstream)
				if string(got.Mode) != string(want.Mode) {
					t.Fatalf("native Mode = %q, transcode Mode = %q: the two paths must agree", got.Mode, want.Mode)
				}
				if got.Mode == auth.AuthXAPIKey && got.AnthropicVersion != want.AnthropicVersion {
					t.Fatalf("native AnthropicVersion = %q, transcode = %q", got.AnthropicVersion, want.AnthropicVersion)
				}
			})
		}
	}
}

func TestNativeAuthVariantsGeminiRows(t *testing.T) {
	secret := auth.NewStaticSecretSource("synthetic-secret")
	tests := []struct {
		name     string
		mode     auth.AuthMode
		protocol transcode.NativeProtocol
		want     auth.AuthMode
	}{
		{name: "bearer stays bearer for gemini", mode: auth.AuthBearer, protocol: transcode.NativeGemini, want: auth.AuthBearer},
		{name: "x-api-key becomes goog-api-key for gemini", mode: auth.AuthXAPIKey, protocol: transcode.NativeGemini, want: auth.AuthGoogAPIKey},
		{name: "api-key becomes goog-api-key for gemini", mode: auth.AuthAPIKey, protocol: transcode.NativeGemini, want: auth.AuthGoogAPIKey},
		{name: "goog-api-key stays for gemini", mode: auth.AuthGoogAPIKey, protocol: transcode.NativeGemini, want: auth.AuthGoogAPIKey},
		{name: "goog-api-key becomes bearer for chat", mode: auth.AuthGoogAPIKey, protocol: transcode.NativeChat, want: auth.AuthBearer},
		{name: "goog-api-key becomes x-api-key for messages", mode: auth.AuthGoogAPIKey, protocol: transcode.NativeMessages, want: auth.AuthXAPIKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := &auth.AuthPolicy{Mode: tt.mode, Secret: secret}
			got := newNativeAuthVariants(policy).forProtocol(tt.protocol)
			if got.Mode != tt.want {
				t.Fatalf("Mode = %q, want %q", got.Mode, tt.want)
			}
			if policy.Mode != tt.mode {
				t.Fatalf("building variants changed the provider policy mode to %q", policy.Mode)
			}
		})
	}
}
