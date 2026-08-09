package transcode

import (
	"net/http"
	"strings"
	"testing"
)

func TestNewRouteKey(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		wantKey RouteKey
		wantErr bool
	}{
		{"normalized", "post", "/v1/responses", RouteKey{Method: "POST", Path: "/v1/responses"}, false},
		{"empty method", " ", "/v1/x", RouteKey{}, true},
		{"relative path", "POST", "v1/x", RouteKey{}, true},
		{"absolute path", "GET", "/v1/x", RouteKey{Method: "GET", Path: "/v1/x"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := NewRouteKey(tt.method, tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewRouteKey error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && key != tt.wantKey {
				t.Fatalf("NewRouteKey = %+v, want %+v", key, tt.wantKey)
			}
		})
	}
}

// TestNewRouteKeyRejectsQueryFragment verifies that route paths carrying
// query or fragment syntax are configuration errors (review-08 additional 5):
// such characters would never match a request path, which is a literal path
// match.
func TestNewRouteKeyRejectsQueryFragment(t *testing.T) {
	for _, path := range []string{
		"/v1/responses?model=x",
		"/v1/responses#frag",
		"/v1/responses?model=x#frag",
		"/v1/responses?",
		"/v1/responses#",
	} {
		if _, err := NewRouteKey(http.MethodPost, path); err == nil {
			t.Fatalf("NewRouteKey(%q): want error, got nil", path)
		}
	}
}

func TestMappingValidate(t *testing.T) {
	tests := []struct {
		name    string
		mapping Mapping
		wantErr bool
	}{
		{
			name: "responses to chat",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:   ClientResponses,
				UpstreamProtocol: UpstreamChatCompletions,
				UpstreamPath:     "/v1/chat/completions",
				Auth:             AuthPolicy{Mode: AuthNone},
			},
			wantErr: false,
		},
		{
			name: "messages to responses",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
				ClientProtocol:   ClientMessages,
				UpstreamProtocol: UpstreamResponses,
				UpstreamPath:     "/v1/responses",
				Auth:             AuthPolicy{Mode: AuthNone},
			},
			wantErr: false,
		},
		{
			name: "messages to chat",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
				ClientProtocol:   ClientMessages,
				UpstreamProtocol: UpstreamChatCompletions,
				UpstreamPath:     "/v1/chat/completions",
				Auth:             AuthPolicy{Mode: AuthNone},
			},
			wantErr: false,
		},
		{
			name: "responses to messages is unsupported",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:   ClientResponses,
				UpstreamProtocol: UpstreamMessages,
				UpstreamPath:     "/v1/messages",
			},
			wantErr: true,
		},
		{
			name: "chat as client is unsupported",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/chat/completions"},
				ClientProtocol:   "chat-completions",
				UpstreamProtocol: UpstreamResponses,
				UpstreamPath:     "/v1/responses",
			},
			wantErr: true,
		},
		{
			name: "GET client route is rejected",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodGet, Path: "/v1/responses"},
				ClientProtocol:   ClientResponses,
				UpstreamProtocol: UpstreamChatCompletions,
				UpstreamPath:     "/v1/chat/completions",
			},
			wantErr: true,
		},
		{
			name: "relative upstream path is rejected",
			mapping: Mapping{
				ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
				ClientProtocol:   ClientResponses,
				UpstreamProtocol: UpstreamChatCompletions,
				UpstreamPath:     "chat/completions",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.mapping.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestMappingValidateConfiguration proves the immutable configuration
// dimensions fail at startup validation (review-j finding 14).
func TestMappingValidateConfiguration(t *testing.T) {
	valid := Mapping{
		ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
		ClientProtocol:   ClientResponses,
		UpstreamProtocol: UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		Auth:             AuthPolicy{Mode: AuthNone},
	}

	tests := []struct {
		name    string
		mutate  func(*Mapping)
		wantErr string
	}{
		{
			name: "unknown auth mode",
			mutate: func(m *Mapping) {
				m.Auth = AuthPolicy{Mode: AuthMode("bogus")}
			},
			wantErr: "auth policy",
		},
		{
			name: "header auth without a header name",
			mutate: func(m *Mapping) {
				m.Auth = AuthPolicy{Mode: AuthCustomHeader}
			},
			wantErr: "auth policy",
		},
		{
			name: "invalid custom header name",
			mutate: func(m *Mapping) {
				m.Auth = AuthPolicy{Mode: AuthCustomHeader, CustomHeader: "bad header!", Inbound: true}
			},
			wantErr: "not a valid HTTP field name",
		},
		{
			name: "reserved custom header name",
			mutate: func(m *Mapping) {
				m.Auth = AuthPolicy{Mode: AuthCustomHeader, CustomHeader: "Authorization", Inbound: true}
			},
			wantErr: "reserved",
		},
		{
			name: "external signer without a signer",
			mutate: func(m *Mapping) {
				m.Auth = AuthPolicy{Mode: AuthExternalSigner}
			},
			wantErr: "auth policy",
		},
		{
			name: "secret mode without a source",
			mutate: func(m *Mapping) {
				m.Auth = AuthPolicy{Mode: AuthBearer}
			},
			wantErr: "auth policy",
		},
		{
			name: "secret mode with a source",
			mutate: func(m *Mapping) {
				m.Auth = AuthPolicy{Mode: AuthBearer, Secret: staticSecret{value: "s3cr3t"}}
			},
			wantErr: "",
		},
		{
			name: "empty upstream model",
			mutate: func(m *Mapping) {
				m.ModelMap = ModelMap{Exact: map[string]ModelMapping{
					"client": {UpstreamModel: ""},
				}}
			},
			wantErr: "empty upstream model",
		},
		{
			name: "empty client model key",
			mutate: func(m *Mapping) {
				m.ModelMap = ModelMap{Exact: map[string]ModelMapping{
					"": {UpstreamModel: "upstream"},
				}}
			},
			wantErr: "empty client model key",
		},
		{
			name: "content-type custom auth header",
			mutate: func(m *Mapping) {
				m.Auth = AuthPolicy{Mode: AuthCustomHeader, CustomHeader: "Content-Type", Inbound: true}
			},
			wantErr: "reserved",
		},
		{
			name: "x-forwarded-for custom auth header",
			mutate: func(m *Mapping) {
				m.Auth = AuthPolicy{Mode: AuthCustomHeader, CustomHeader: "X-Forwarded-For", Inbound: true}
			},
			wantErr: "reserved",
		},
		{
			name: "accept-encoding custom auth header",
			mutate: func(m *Mapping) {
				m.Auth = AuthPolicy{Mode: AuthCustomHeader, CustomHeader: "Accept-Encoding", Inbound: true}
			},
			wantErr: "reserved",
		},
		{
			name: "x-amz-prefixed custom auth header",
			mutate: func(m *Mapping) {
				m.Auth = AuthPolicy{Mode: AuthCustomHeader, CustomHeader: "X-Amz-Date", Inbound: true}
			},
			wantErr: "reserved",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapping := valid
			tt.mutate(&mapping)
			err := mapping.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validation rejected a valid configuration: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validation accepted the invalid configuration")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
