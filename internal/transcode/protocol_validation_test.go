package transcode

// J13 regression tests: the remaining protocol-validation details (review-j
// finding 15) — exact media-type matching, the base64 data-URL parameter,
// and Anthropic source union exclusivity.

import (
	"net/http"
	"testing"
)

// TestMediaTypeMatchingExact proves media recognition is exact: lookalike
// types are rejected and the JSON structured-syntax family is accepted
// (review-j finding 15).
func TestMediaTypeMatchingExact(t *testing.T) {
	response := func(contentType string) *http.Response {
		return &http.Response{Header: http.Header{"Content-Type": {contentType}}}
	}
	tests := []struct {
		name        string
		contentType string
		isJSON      bool
		isEvent     bool
	}{
		{"application/json", "application/json", true, false},
		{"json with charset", "application/json; charset=utf-8", true, false},
		{"structured syntax suffix", "application/ld+json", true, false},
		{"notjson lookalike", "application/notjson", false, false},
		{"event stream", "text/event-stream", false, true},
		{"event stream with charset", "text/event-stream; charset=utf-8", false, true},
		{"event-streaming lookalike", "text/event-streaming", false, false},
		{"missing content type", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := response(tt.contentType)
			if got := isJSON(resp); got != tt.isJSON {
				t.Fatalf("isJSON(%q) = %v, want %v", tt.contentType, got, tt.isJSON)
			}
			if got := isEventStream(resp); got != tt.isEvent {
				t.Fatalf("isEventStream(%q) = %v, want %v", tt.contentType, got, tt.isEvent)
			}
		})
	}
}

// TestSplitImageDataURLBase64Parameter proves the data-URL parameters section
// must include base64 (review-j finding 15).
func TestSplitImageDataURLBase64Parameter(t *testing.T) {
	if _, _, err := splitImageDataURL("data:image/png;base64,aGk="); err != nil {
		t.Fatalf("valid data URL rejected: %v", err)
	}
	if _, _, err := splitImageDataURL("data:image/png;charset=utf-8;base64,aGk="); err != nil {
		t.Fatalf("data URL with extra parameters rejected: %v", err)
	}
	if _, _, err := splitImageDataURL("data:image/png;base64;foo,aGk="); err != nil {
		t.Fatalf("data URL with base64 among parameters rejected: %v", err)
	}
	for _, bad := range []string{
		"data:image/png;foo,aGk=",
		"data:image/png,aGk=",
		"data:image/png;base64",
	} {
		if _, _, err := splitImageDataURL(bad); err == nil {
			t.Fatalf("malformed data URL %q accepted", bad)
		}
	}
	if mediaType, data, err := splitImageDataURL("https://example.com/image.png"); err != nil || mediaType != "" || data != "" {
		t.Fatalf("non-data URL = (%q, %q, %v)", mediaType, data, err)
	}
}

// TestAnthropicSourceExclusivity proves ambiguous sources are rejected —
// a base64 source must not carry a url and a url source must not carry data
// (review-j finding 15).
func TestAnthropicSourceExclusivity(t *testing.T) {
	valid := []AnthropicSource{
		{Type: AnthropicSourceTypeBase64, MediaType: "image/png", Data: "aGk="},
		{Type: AnthropicSourceTypeURL, URL: "https://example.com/image.png"},
	}
	for _, source := range valid {
		if err := source.Validate(); err != nil {
			t.Fatalf("valid source rejected: %v", err)
		}
	}
	ambiguous := []AnthropicSource{
		{Type: AnthropicSourceTypeBase64, MediaType: "image/png", Data: "aGk=", URL: "https://example.com/image.png"},
		{Type: AnthropicSourceTypeURL, URL: "https://example.com/image.png", Data: "aGk="},
	}
	for _, source := range ambiguous {
		if err := source.Validate(); err == nil {
			t.Fatalf("ambiguous source accepted: %+v", source)
		}
	}

	// The decode path rejects ambiguous sources through block validation.
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk=","url":"https://example.com/image.png"}}]}]}`
	if _, err := DecodeMessagesRequest([]byte(body), StrictLossPolicy()); err == nil {
		t.Fatal("decode accepted an ambiguous image source")
	}
}
