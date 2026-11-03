package transcode

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseNativeProtocolGemini(t *testing.T) {
	got, err := ParseNativeProtocol("gemini")
	if err != nil {
		t.Fatalf("ParseNativeProtocol(gemini): %v", err)
	}
	if got != NativeGemini {
		t.Fatalf("ParseNativeProtocol(gemini) = %q, want %q", got, NativeGemini)
	}
	_, err = ParseNativeProtocol("bogus")
	if err == nil || !strings.Contains(err.Error(), "or gemini") {
		t.Fatalf("ParseNativeProtocol(bogus) error = %v, want the vocabulary to name gemini", err)
	}
}

func TestValidateGeminiShape(t *testing.T) {
	geminiClient := func(path string) Mapping {
		return Mapping{
			ClientRoute:      RouteKey{Method: http.MethodPost, Path: path},
			ClientProtocol:   ClientGemini,
			UpstreamProtocol: UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
		}
	}
	tests := []struct {
		name    string
		mapping Mapping
		wantErr string
	}{
		{
			name:    "gemini client template is accepted",
			mapping: geminiClient("/v1beta/models/{model}:generateContent"),
		},
		{
			name:    "gemini client path without the template is rejected",
			mapping: geminiClient("/v1beta/models/fixed:generateContent"),
			wantErr: "gemini route path",
		},
		{
			name: "placeholder on a responses client path is rejected",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses/{model}"},
				ClientProtocol:   ClientResponses,
				UpstreamProtocol: UpstreamChatCompletions,
				UpstreamPath:     "/v1/chat/completions",
			},
			wantErr: "only gemini routes accept {model}",
		},
		{
			name: "gemini upstream prefix root is accepted",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:   ClientResponses,
				UpstreamProtocol: UpstreamGemini,
				UpstreamPath:     "/",
			},
		},
		{
			name: "gemini upstream prefix with a trailing slash is rejected",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:   ClientResponses,
				UpstreamProtocol: UpstreamGemini,
				UpstreamPath:     "/v1beta/",
			},
			wantErr: "must be canonical",
		},
		{
			name: "gemini upstream prefix with a colon is rejected",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:   ClientResponses,
				UpstreamProtocol: UpstreamGemini,
				UpstreamPath:     "/v1beta:x",
			},
			wantErr: "must be canonical",
		},
		{
			name: "via gemini with a provider-qualified model is rejected",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:   ClientResponses,
				UpstreamProtocol: UpstreamGemini,
				UpstreamPath:     "/v1beta",
				ModelMap: ModelMap{Exact: map[string]ModelMapping{
					"alias": {ClientModel: "alias", UpstreamModel: "models/gemini-2.5-pro", Via: NativeGemini},
				}},
			},
			wantErr: "must be a bare model id",
		},
		{
			name: "via gemini with a provider-qualified model is rejected on a chat upstream too",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:   ClientResponses,
				UpstreamProtocol: UpstreamChatCompletions,
				UpstreamPath:     "/v1/chat/completions",
				ModelMap: ModelMap{Exact: map[string]ModelMapping{
					"alias": {ClientModel: "alias", UpstreamModel: "models/gemini-pro", Via: NativeGemini},
				}},
			},
			wantErr: "must be a bare model id",
		},
		{
			name: "gemini upstream prefix with a query is rejected",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:   ClientResponses,
				UpstreamProtocol: UpstreamGemini,
				UpstreamPath:     "/v1beta?y=1",
			},
			wantErr: "must be canonical",
		},
		{
			name: "provider-wide map with chat model names is accepted",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:   ClientResponses,
				UpstreamProtocol: UpstreamGemini,
				UpstreamPath:     "/v1beta",
				ModelMap: ModelMap{Exact: map[string]ModelMapping{
					"openai/gpt-4o": {ClientModel: "openai/gpt-4o", UpstreamModel: "openai/gpt-4o"},
				}},
			},
		},
		{
			name: "credential name in the client query allowlist is rejected",
			mapping: Mapping{
				ClientRoute:        RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:     ClientResponses,
				UpstreamProtocol:   UpstreamChatCompletions,
				UpstreamPath:       "/v1/chat/completions",
				AllowedClientQuery: map[string]struct{}{"Access_Token": {}},
			},
			wantErr: "carries a credential",
		},
		{
			name: "oauth_token in the client query allowlist is rejected",
			mapping: Mapping{
				ClientRoute:        RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:     ClientResponses,
				UpstreamProtocol:   UpstreamChatCompletions,
				UpstreamPath:       "/v1/chat/completions",
				AllowedClientQuery: map[string]struct{}{"oauth_token": {}},
			},
			wantErr: "carries a credential",
		},
		{
			name: "ordinary query names are accepted",
			mapping: Mapping{
				ClientRoute:        RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:     ClientResponses,
				UpstreamProtocol:   UpstreamChatCompletions,
				UpstreamPath:       "/v1/chat/completions",
				AllowedClientQuery: map[string]struct{}{"api-version": {}, "alt": {}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.mapping.validateGeminiShape()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateGeminiShape: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateGeminiShape error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestMappingValidateGeminiDirectionsStillUnsupported(t *testing.T) {
	// The remaining Gemini directions stay refused until their conversions
	// exist in the direction switch: G2C and G2R are served, and the
	// G2M direction plus any other combination must keep failing here.
	tests := []struct {
		name     string
		client   ClientProtocol
		path     string
		upstream UpstreamProtocol
		upPath   string
	}{
		{name: "R2M", client: ClientResponses, path: "/v1/responses", upstream: UpstreamMessages, upPath: "/v1/messages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: tt.path},
				ClientProtocol:   tt.client,
				UpstreamProtocol: tt.upstream,
				UpstreamPath:     tt.upPath,
				ModelMap:         ModelMap{AllowIdentity: true},
				Auth:             AuthPolicy{Mode: AuthNone},
			}
			err := m.Validate()
			if err == nil || !strings.Contains(err.Error(), "unsupported transcode direction") {
				t.Fatalf("Validate() = %v, want unsupported transcode direction", err)
			}
		})
	}
}

func TestMappingValidateGeminiUpstreamDirectionsSupported(t *testing.T) {
	// R2G and M2G are served: validation must pass with the standard shape.
	tests := []struct {
		name     string
		client   ClientProtocol
		path     string
		upstream UpstreamProtocol
		upPath   string
	}{
		{name: "R2G", client: ClientResponses, path: "/v1/responses", upstream: UpstreamGemini, upPath: "/v1beta"},
		{name: "M2G", client: ClientMessages, path: "/v1/messages", upstream: UpstreamGemini, upPath: "/v1beta"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: tt.path},
				ClientProtocol:   tt.client,
				UpstreamProtocol: tt.upstream,
				UpstreamPath:     tt.upPath,
				ModelMap:         ModelMap{AllowIdentity: true},
				Auth:             AuthPolicy{Mode: AuthNone},
			}
			if err := m.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil for a supported direction", err)
			}
		})
	}
}
