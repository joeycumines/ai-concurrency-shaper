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
		{query: "key%3Dsecret%zz", want: true},
		{query: "key%3D%zz=x", want: true},
		{query: "KEY%3dsecret%zz", want: true},
		{query: "foo=1;key=SECRET", want: true},
		{query: "foo=1;key%3Dsecret%zz", want: true},
		{query: "x;access_token=T", want: true},
		{query: "alt=sse;;oauth_token=T&", want: true},
		{query: "monkey=1;keys=2", want: false},
		{query: "foo=1;keys=2", want: false},
		// Non-ASCII bytes change length under strings.ToLower (invalid UTF-8
		// becomes the 3-byte RuneError; U+023A lowercases to a 3-byte rune),
		// so an index into the lowered string can exceed len(name) and panic.
		// Such a pair's name is not a credential to the upstream either, but
		// the scan must not crash on it.
		{query: "\x80\x80\x80%3d", want: false},
		{query: "\x80\x80\x80%3d=x", want: false},
		{query: "Ⱥ%3Dsecret", want: false},
		// A credential followed by non-ASCII is still detected: the raw cut
		// leaves exactly "key".
		{query: "key%3D\x80secret", want: true},
		{query: "key%3dȺsecret", want: true},
		{query: "key%3Dsecret\x80\x80", want: true},
		// U+0130 lowercases to 1 byte ('i'), shifting a lowered index left;
		// the raw byte scan is unaffected.
		{query: "İkey%3Dsecret%zz", want: false},
		{query: "key%3Dİsecret%zz", want: true},
	}
	for _, tt := range tests {
		if got := HasQueryCredential(tt.query); got != tt.want {
			t.Errorf("HasQueryCredential(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestHasQueryCredentialNeverPanicsOnArbitraryBytes(t *testing.T) {
	// Every byte string must be handled without a slice-bounds panic; the
	// scan reads the raw bytes only.
	probes := []string{
		string([]byte{0x80, 0x80, '%', '3', 'd'}),
		string([]byte{0xff, 0xfe, '%', '3', 'D', 'k', 'e', 'y'}),
		"ȺȾ%3d",
		"İẞ%3D",
	}
	for _, p := range probes {
		_ = HasQueryCredential(p)
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
	// The same holds when the credential name is %3D-escaped and the value
	// carries a malformed escape: the credential refusal wins, not the
	// malformed-query refusal.
	for _, query := range []string{"key%3Dsecret%zz", "key%3D%zz=x&alt=sse"} {
		if err := geminiClientQuery(match, query); !errors.Is(err, errGeminiQueryCredential) {
			t.Errorf("geminiClientQuery(%q) = %v, want the credential refusal", query, err)
		}
	}
}

func TestParseClientQueryErrorNamesNoValue(t *testing.T) {
	// url.ParseQuery's error embeds up to 3 raw bytes of the offending escape
	// (e.g. url.EscapeError("%zz")); the returned error must name only the
	// parse failure, never a value fragment.
	for _, query := range []string{"token=my_secret%zz", "key%3Dsecret%zz", "bad=%zz&alt=sse"} {
		_, err := parseClientQuery(query)
		if !errors.Is(err, errClientQueryParameter) {
			t.Fatalf("parseClientQuery(%q) = %v, want the malformed-query refusal", query, err)
		}
		for _, fragment := range []string{"%zz", "secret", "my_secret"} {
			if strings.Contains(err.Error(), fragment) {
				t.Errorf("parseClientQuery(%q) error %q echoes value fragment %q", query, err, fragment)
			}
		}
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
