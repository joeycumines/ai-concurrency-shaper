package transcode

import (
	"net/http"
	"testing"
)

func TestParseGeminiClientRoutePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "versioned prefix", path: "/v1beta/models/{model}:generateContent", want: "/v1beta"},
		{name: "empty prefix", path: "/models/{model}:generateContent", want: ""},
		{name: "nested prefix", path: "/x/v1beta/models/{model}:generateContent", want: "/x/v1beta"},
		{name: "concrete model is not a template", path: "/v1beta/models/gemini:generateContent", wantErr: true},
		{name: "stream action is not a route key", path: "/v1beta/models/{model}:streamGenerateContent", wantErr: true},
		{name: "placeholder in prefix", path: "/v1beta/{x}/models/{model}:generateContent", wantErr: true},
		{name: "colon in prefix", path: "/a:b/models/{model}:generateContent", wantErr: true},
		{name: "non-canonical path", path: "/v1beta//models/{model}:generateContent", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseGeminiClientRoutePath(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseGeminiClientRoutePath(%q) = %q, want an error", tt.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGeminiClientRoutePath(%q): %v", tt.path, err)
			}
			if got != tt.want {
				t.Fatalf("prefix = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMatchGeminiRequestPath(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		wantOK     bool
		wantOp     GeminiOperation
		wantModel  string
		wantKey    string
		wantTmpl   string
		checkMatch bool
	}{
		{
			name:       "generate on versioned prefix",
			method:     http.MethodPost,
			path:       "/v1beta/models/gemini-2.5-pro:generateContent",
			wantOK:     true,
			wantOp:     GeminiGenerate,
			wantModel:  "gemini-2.5-pro",
			wantKey:    "/v1beta/models/{model}:generateContent",
			wantTmpl:   "/v1beta/models/{model}:generateContent",
			checkMatch: true,
		},
		{
			name:       "stream shares the generate route key",
			method:     http.MethodPost,
			path:       "/v1beta/models/gemini-2.5-pro:streamGenerateContent",
			wantOK:     true,
			wantOp:     GeminiStream,
			wantModel:  "gemini-2.5-pro",
			wantKey:    "/v1beta/models/{model}:generateContent",
			wantTmpl:   "/v1beta/models/{model}:streamGenerateContent",
			checkMatch: true,
		},
		{
			name:       "empty prefix",
			method:     http.MethodPost,
			path:       "/models/m:generateContent",
			wantOK:     true,
			wantOp:     GeminiGenerate,
			wantModel:  "m",
			wantKey:    "/models/{model}:generateContent",
			wantTmpl:   "/models/{model}:generateContent",
			checkMatch: true,
		},
		{
			name:       "nested prefix",
			method:     http.MethodPost,
			path:       "/x/v1beta/models/m.1-b_2:generateContent",
			wantOK:     true,
			wantOp:     GeminiGenerate,
			wantModel:  "m.1-b_2",
			wantKey:    "/x/v1beta/models/{model}:generateContent",
			wantTmpl:   "/x/v1beta/models/{model}:generateContent",
			checkMatch: true,
		},
		{name: "method is case-insensitive", method: "post", path: "/v1beta/models/m:generateContent", wantOK: true, wantOp: GeminiGenerate, wantModel: "m", wantKey: "/v1beta/models/{model}:generateContent", wantTmpl: "/v1beta/models/{model}:generateContent", checkMatch: true},
		{name: "get is not a create operation", method: http.MethodGet, path: "/v1beta/models/m:generateContent"},
		{name: "action names are case-sensitive", method: http.MethodPost, path: "/v1beta/models/m:GenerateContent"},
		{name: "count tokens is a different operation", method: http.MethodPost, path: "/v1beta/models/m:countTokens"},
		{name: "missing model", method: http.MethodPost, path: "/v1beta/models/:generateContent"},
		{name: "colon inside model", method: http.MethodPost, path: "/v1beta/models/a:b:generateContent"},
		{name: "dot model", method: http.MethodPost, path: "/v1beta/models/.:generateContent"},
		{name: "dot-dot model", method: http.MethodPost, path: "/v1beta/models/..:generateContent"},
		{name: "trailing segment", method: http.MethodPost, path: "/v1beta/models/m:generateContent/extra"},
		{name: "tuned models directory", method: http.MethodPost, path: "/v1beta/tunedModels/m:generateContent"},
		{name: "non-canonical path", method: http.MethodPost, path: "/v1beta//models/m:generateContent"},
		{name: "no action", method: http.MethodPost, path: "/v1beta/models/m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MatchGeminiRequestPath(tt.method, tt.path)
			if ok != tt.wantOK {
				t.Fatalf("MatchGeminiRequestPath(%q, %q) ok = %v, want %v", tt.method, tt.path, ok, tt.wantOK)
			}
			if !tt.checkMatch {
				return
			}
			if got.Operation != tt.wantOp {
				t.Errorf("Operation = %v, want %v", got.Operation, tt.wantOp)
			}
			if got.Model != tt.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, tt.wantModel)
			}
			if want := (RouteKey{Method: http.MethodPost, Path: tt.wantKey}); got.Key != want {
				t.Errorf("Key = %+v, want %+v", got.Key, want)
			}
			if got.TemplatePath != tt.wantTmpl {
				t.Errorf("TemplatePath = %q, want %q", got.TemplatePath, tt.wantTmpl)
			}
		})
	}
}

func TestIsGeminiOperationPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/v1beta/models/m:generateContent", want: true},
		{path: "/v1beta/models/m:streamGenerateContent", want: true},
		// Broader than the matcher: the queue-comment gate must refuse these
		// too, so a non-model Gemini operation never receives comments.
		{path: "/v1beta/tunedModels/x:generateContent", want: true},
		{path: "/v1beta/models/m:countTokens", want: false},
		{path: "/v1/chat/completions", want: false},
		{path: "/v1beta/models/m:generateContent/extra", want: false},
	}
	for _, tt := range tests {
		if got := IsGeminiOperationPath(tt.path); got != tt.want {
			t.Errorf("IsGeminiOperationPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
	if _, ok := MatchGeminiRequestPath(http.MethodPost, "/v1beta/tunedModels/x:generateContent"); ok {
		t.Error("MatchGeminiRequestPath accepted a tunedModels path that IsGeminiOperationPath reports")
	}
}
