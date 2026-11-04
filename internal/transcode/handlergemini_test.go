package transcode

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHasQueryCredential(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{query: "", want: false},
		{query: "alt=sse", want: false},
		{query: "key=SECRET", want: true},
		{query: "alt=sse&KEY=SECRET", want: true},
		{query: "key=secret%zz", want: true},
		{query: "key%3D=x", want: true},
		{query: "foo=1;key=SECRET", want: true},
		{query: "x;access_token=T", want: true},
		{query: "alt=sse;;oauth_token=T&", want: true},
		{query: "monkey=1;keys=2", want: false},
	}
	for _, tt := range tests {
		if got := HasQueryCredential(tt.query); got != tt.want {
			t.Errorf("HasQueryCredential(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestGeminiClientQuerySemicolonPairs(t *testing.T) {
	match := GeminiRouteMatch{Operation: GeminiStream}
	if err := geminiClientQuery(match, "foo=1;alt=sse"); err != nil {
		t.Fatalf("alt after ';': %v", err)
	}
	if err := geminiClientQuery(match, "alt=sse;alt=sse"); err == nil {
		t.Fatal("alt repeated across ';': nil error, want the at-most-once refusal")
	}
}

func TestGeminiUpstreamModelMustBeBareID(t *testing.T) {
	roundTrips := 0
	h := NewTranscodeHandler(HandlerConfig{
		Mapping: Mapping{
			ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
			ClientProtocol:   ClientMessages,
			UpstreamProtocol: UpstreamGemini,
			UpstreamPath:     "/v1beta",
			ModelMap:         ModelMap{AllowIdentity: true},
			Auth:             AuthPolicy{Mode: AuthNone},
		},
		Upstream: mustParseURL(t, "https://gemini.example.com"),
		BodyLimits: BodyLimits{
			AcceptedRequestBytes:    1 << 20,
			SuccessfulResponseBytes: 1 << 20,
		},
	}, func(*http.Request) (*http.Response, error) {
		roundTrips++
		return nil, errors.New("the request must be refused before the upstream")
	}, nil)
	body := `{"model":"models/gemini-2.5-pro","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "models/gemini-2.5-pro") {
		t.Fatalf("error does not name the model: %s", rec.Body.String())
	}
	if roundTrips != 0 {
		t.Fatalf("round trips = %d, want 0", roundTrips)
	}
}
