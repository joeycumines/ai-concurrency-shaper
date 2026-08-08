package transcode

import (
	"net/http"
	"net/url"
	"testing"
)

func TestRemoveHopByHopHeaders(t *testing.T) {
	tests := []struct {
		name     string
		input    http.Header
		wantDel  []string
		wantKeep []string
	}{
		{
			name: "fixed list",
			input: http.Header{
				"Connection":        []string{"keep-alive"},
				"Keep-Alive":        []string{"timeout=5"},
				"Transfer-Encoding": []string{"chunked"},
				"Upgrade":           []string{"websocket"},
				"Proxy-Connection":  []string{"keep-alive"},
				"X-Custom":          []string{"keep"},
			},
			wantDel:  []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "Proxy-Connection"},
			wantKeep: []string{"X-Custom"},
		},
		{
			name: "connection nominated header removed",
			input: http.Header{
				"Connection": []string{"X-Internal"},
				"X-Internal": []string{"secret"},
				"X-Custom":   []string{"keep"},
			},
			wantDel:  []string{"Connection", "X-Internal"},
			wantKeep: []string{"X-Custom"},
		},
		{
			name: "multiple connection tokens",
			input: http.Header{
				"Connection": []string{"X-A, X-B"},
				"X-A":        []string{"1"},
				"X-B":        []string{"2"},
				"X-C":        []string{"3"},
			},
			wantDel:  []string{"Connection", "X-A", "X-B"},
			wantKeep: []string{"X-C"},
		},
		{
			name: "malformed connection token ignored",
			input: http.Header{
				"Connection": []string{"bad token"},
				"X-Custom":   []string{"keep"},
			},
			wantDel:  []string{"Connection"},
			wantKeep: []string{"X-Custom"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			RemoveHopByHopHeaders(tt.input)
			for _, name := range tt.wantDel {
				if got := tt.input.Get(name); got != "" {
					t.Errorf("header %s not removed: %q", name, got)
				}
			}
			for _, name := range tt.wantKeep {
				if got := tt.input.Get(name); got == "" {
					t.Errorf("header %s removed unexpectedly", name)
				}
			}
		})
	}
}

func TestValidHTTPFieldName(t *testing.T) {
	valid := []string{"X-Foo", "x", "A1", "ETag", "X*Y"}
	invalid := []string{"", "has space", "bad\tname", "x\n", "co mma"}
	for _, name := range valid {
		if !ValidHTTPFieldName(name) {
			t.Errorf("%q should be valid", name)
		}
	}
	for _, name := range invalid {
		if ValidHTTPFieldName(name) {
			t.Errorf("%q should be invalid", name)
		}
	}
}

func TestRemoveTransformedRepresentationHeaders(t *testing.T) {
	header := http.Header{
		"Content-Length":   []string{"123"},
		"Content-Encoding": []string{"gzip"},
		"Content-Md5":      []string{"abc"},
		"Etag":             []string{"w/\"x\""},
		"Last-Modified":    []string{"Mon"},
		"Digest":           []string{"sha-256=..."},
		"Content-Digest":   []string{"sha-256=..."},
		"Repr-Digest":      []string{"sha-256=..."},
		"Accept-Ranges":    []string{"bytes"},
		"X-Custom":         []string{"keep"},
	}
	RemoveTransformedRepresentationHeaders(header)
	for _, name := range []string{
		"Content-Length", "Content-Encoding", "Content-Md5", "Etag",
		"Last-Modified", "Digest", "Content-Digest", "Repr-Digest", "Accept-Ranges",
	} {
		if got := header.Get(name); got != "" {
			t.Errorf("%s not removed: %q", name, got)
		}
	}
	if got := header.Get("X-Custom"); got != "keep" {
		t.Errorf("X-Custom = %q", got)
	}
}

func TestBuildMappedURL(t *testing.T) {
	base, err := url.Parse("https://upstream.example/api")
	if err != nil {
		t.Fatal(err)
	}

	// Base query is preserved; client query allowed only when listed.
	out, err := BuildMappedURL(
		base,
		"/v1/chat/completions",
		url.Values{"api-version": []string{"2024-01-01"}},
		map[string]struct{}{"api-version": {}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Path != "/api/v1/chat/completions" {
		t.Fatalf("path = %q", out.Path)
	}
	if got := out.Query().Get("api-version"); got != "2024-01-01" {
		t.Fatalf("query api-version = %q", got)
	}

	// Unknown client query is rejected.
	_, err = BuildMappedURL(
		base,
		"/v1/chat/completions",
		url.Values{"evil": []string{"1"}},
		map[string]struct{}{},
	)
	if err == nil {
		t.Fatal("expected unknown client query rejection")
	}

	// Base query survives even with no client query.
	baseWithQuery, _ := url.Parse("https://upstream.example/api?api-version=2024-01-01")
	out, err = BuildMappedURL(
		baseWithQuery,
		"/v1/chat/completions",
		nil,
		map[string]struct{}{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Query().Get("api-version"); got != "2024-01-01" {
		t.Fatalf("base query lost: %q", out.RawQuery)
	}

	// Nil base is an error.
	if _, err := BuildMappedURL(nil, "/v1/x", nil, nil); err == nil {
		t.Fatal("expected nil base error")
	}
}

func TestBuildMappedURLJoinPath(t *testing.T) {
	base, _ := url.Parse("https://upstream.example")
	out, err := BuildMappedURL(base, "/v1/messages", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Path != "/v1/messages" {
		t.Fatalf("path = %q", out.Path)
	}
}
