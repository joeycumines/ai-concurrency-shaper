package transcode

import (
	"net/url"
	"testing"
)

func TestBuildGeminiURL(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		prefix  string
		model   string
		stream  bool
		want    string
		wantErr bool
	}{
		{
			name:   "stream keeps base query and adds alt",
			base:   "https://h/x?api-version=1",
			prefix: "/v1beta",
			model:  "gemini-2.5-pro",
			stream: true,
			want:   "https://h/x/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse&api-version=1",
		},
		{
			name:   "json keeps base query without alt",
			base:   "https://h/x?api-version=1",
			prefix: "/v1beta",
			model:  "gemini-2.5-pro",
			want:   "https://h/x/v1beta/models/gemini-2.5-pro:generateContent?api-version=1",
		},
		{
			name:   "empty base path with versioned prefix",
			base:   "https://h",
			prefix: "/v1beta",
			model:  "m",
			want:   "https://h/v1beta/models/m:generateContent",
		},
		{
			name:   "root prefix means no prefix",
			base:   "https://h/x",
			prefix: "/",
			model:  "m",
			want:   "https://h/x/models/m:generateContent",
		},
		{
			name:   "empty prefix",
			base:   "https://h/x",
			prefix: "",
			model:  "m",
			stream: true,
			want:   "https://h/x/models/m:streamGenerateContent?alt=sse",
		},
		{
			name:   "encoded slash in the base path is preserved",
			base:   "https://h/a%2Fb",
			prefix: "/v1beta",
			model:  "m",
			want:   "https://h/a%2Fb/v1beta/models/m:generateContent",
		},
		{
			name:   "encoded question mark in the base path stays in the path",
			base:   "https://h/a%3Fb",
			prefix: "/v1beta",
			model:  "m",
			want:   "https://h/a%3Fb/v1beta/models/m:generateContent",
		},
		{
			name:    "provider-qualified model is rejected",
			base:    "https://h",
			prefix:  "/v1beta",
			model:   "openai/gpt-4o",
			wantErr: true,
		},
		{
			name:    "models prefix is rejected",
			base:    "https://h",
			prefix:  "/v1beta",
			model:   "models/gemini-2.5-pro",
			wantErr: true,
		},
		{
			name:    "colon in model is rejected",
			base:    "https://h",
			prefix:  "/v1beta",
			model:   "m:generateContent",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, err := url.Parse(tt.base)
			if err != nil {
				t.Fatal(err)
			}
			got, err := BuildGeminiURL(base, tt.prefix, tt.model, tt.stream)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("BuildGeminiURL = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildGeminiURL: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("URL = %q, want %q", got.String(), tt.want)
			}
		})
	}
}

func TestBuildGeminiURLRejectsNilBase(t *testing.T) {
	if _, err := BuildGeminiURL(nil, "/v1beta", "m", false); err == nil {
		t.Fatal("BuildGeminiURL accepted a nil base URL")
	}
}
