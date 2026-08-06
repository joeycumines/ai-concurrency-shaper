package transcode

import (
	"net/http"
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
