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
		{query: "key%3DSECRET", want: true},
		{query: "key%3D%3D=x", want: true},
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

func TestGeminiClientQueryMalformedIsRefused(t *testing.T) {
	match := GeminiRouteMatch{Operation: GeminiStream}
	if err := geminiClientQuery(match, "alt=sse"); err != nil {
		t.Fatalf("alt=sse: %v", err)
	}
	for _, query := range []string{"foo=1;alt=sse", "bad=%zz&alt=sse"} {
		if err := geminiClientQuery(match, query); !errors.Is(err, errClientQueryParameter) {
			t.Errorf("geminiClientQuery(%q) = %v, want the malformed-query refusal", query, err)
		}
	}
	// The credential scan runs first, so a credential in a malformed pair
	// still gets the credential refusal.
	if err := geminiClientQuery(match, "foo=1;key=SECRET"); !errors.Is(err, errGeminiQueryCredential) {
		t.Fatalf("credential after ';' = %v, want the credential refusal", err)
	}
}

func TestUpstreamClientQueryRefusesUnparsedPairs(t *testing.T) {
	for _, target := range []string{
		"/v1/messages?unallowed=1;alt=sse",
		"/v1/messages?unallowed=%zz&alt=sse",
	} {
		r := httptest.NewRequest(http.MethodPost, target, nil)
		if _, err := upstreamClientQuery(r, ClientMessages); !errors.Is(err, errClientQueryParameter) {
			t.Errorf("upstreamClientQuery(%q) = %v, want the malformed-query refusal", target, err)
		}
	}
	h := NewTranscodeHandler(HandlerConfig{
		Mapping: Mapping{
			ClientRoute:        RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
			ClientProtocol:     ClientMessages,
			UpstreamProtocol:   UpstreamGemini,
			UpstreamPath:       "/v1beta",
			ModelMap:           ModelMap{AllowIdentity: true},
			Auth:               AuthPolicy{Mode: AuthNone},
			LossPolicy:         LossPolicy{Allowed: map[Feature]struct{}{FeatureGeminiClientQuery: {}}},
			AllowedClientQuery: map[string]struct{}{"beta": {}},
		},
		Upstream: mustParseURL(t, "https://gemini.example.com"),
	}, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no round trip in this test")
	}, nil)
	// An allowed pair must not carry an unallowed one past the allowlist.
	r := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true;evil=1", nil)
	if _, err := h.geminiClientQueryLosses(r); !errors.Is(err, errClientQueryParameter) {
		t.Fatalf("geminiClientQueryLosses = %v, want the malformed-query refusal", err)
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
