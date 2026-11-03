package transcode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const geminiChatCompletion = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gemini-x",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

// geminiChatHandler builds a Gemini-client handler over a chat upstream. The
// captured upstream request is returned through seen, and calls counts how
// many upstream exchanges the handler made.
func geminiChatHandler(t *testing.T, seen *[]*http.Request, calls *int) http.Handler {
	t.Helper()
	key, err := NewRouteKey(http.MethodPost, "/v1beta/models/{model}:generateContent")
	if err != nil {
		t.Fatal(err)
	}
	mapping := Mapping{
		ClientRoute:      key,
		ClientProtocol:   ClientGemini,
		UpstreamProtocol: UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         ModelMap{AllowIdentity: true},
		Auth:             AuthPolicy{Mode: AuthNone},
		ChatCapabilities: defaultChatCapabilities(),
		LossPolicy: LossPolicy{Allowed: map[Feature]struct{}{
			FeatureUsageCacheReadUnknown:  {},
			FeatureUsageCacheWriteUnknown: {},
			FeatureUsageReasoningUnknown:  {},
			FeatureUsageUnknown:           {},
		}},
	}
	return NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			*calls++
			if seen != nil {
				*seen = append(*seen, req)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(geminiChatCompletion)),
			}, nil
		},
		nil,
	)
}

func geminiClientRequest(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

const geminiSimpleBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`

func TestGeminiClientGenerateContentOverChat(t *testing.T) {
	var seen []*http.Request
	var calls int
	handler := geminiChatHandler(t, &seen, &calls)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, geminiClientRequest("/v1beta/models/gemini-x:generateContent", geminiSimpleBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if calls != 1 || len(seen) != 1 {
		t.Fatalf("upstream calls = %d", calls)
	}
	if got := seen[0].URL.Path; got != "/v1/chat/completions" {
		t.Fatalf("upstream path %q", got)
	}
	if q := seen[0].URL.RawQuery; q != "" {
		t.Fatalf("upstream query %q, want none", q)
	}
	body, err := io.ReadAll(seen[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"model":"gemini-x"`) || !strings.Contains(string(body), `"hi"`) {
		t.Fatalf("upstream body %s", body)
	}

	var doc struct {
		Candidates []struct {
			Content struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			TotalTokenCount int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
		ModelVersion string `json:"modelVersion"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	if len(doc.Candidates) != 1 || doc.Candidates[0].Content.Role != "model" {
		t.Fatalf("candidates = %+v", doc.Candidates)
	}
	if got := doc.Candidates[0].Content.Parts[0].Text; got != "hello" {
		t.Fatalf("text %q, want hello", got)
	}
	if doc.Candidates[0].FinishReason != "STOP" || doc.UsageMetadata.TotalTokenCount != 5 {
		t.Fatalf("finish %q total %d", doc.Candidates[0].FinishReason, doc.UsageMetadata.TotalTokenCount)
	}
	if doc.ModelVersion != "gemini-x" {
		t.Fatalf("modelVersion %q", doc.ModelVersion)
	}
}

func TestGeminiClientRefusalsNeverReachUpstream(t *testing.T) {
	tests := []struct {
		name   string
		target string
		body   string
		want   string
	}{
		{
			name:   "API key in the query string",
			target: "/v1beta/models/gemini-x:generateContent?key=AIzaSyFAKEKEY",
			body:   geminiSimpleBody,
			want:   "X-Goog-Api-Key",
		},
		{
			name:   "access token in the query string",
			target: "/v1beta/models/gemini-x:generateContent?access_token=t",
			body:   geminiSimpleBody,
			want:   "must not be sent in the query string",
		},
		{
			name:   "alt=sse on generateContent",
			target: "/v1beta/models/gemini-x:generateContent?alt=sse",
			body:   geminiSimpleBody,
			want:   "accepts alt=json only",
		},
		{
			name:   "body model conflicts with the path model",
			target: "/v1beta/models/gemini-x:generateContent",
			body:   `{"model":"gemini-y","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want:   "conflicts with path model",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			handler := geminiChatHandler(t, nil, &calls)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, geminiClientRequest(tt.target, tt.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400; body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Fatalf("body %s does not contain %q", rec.Body.String(), tt.want)
			}
			if calls != 0 {
				t.Fatalf("refused request reached the upstream %d times", calls)
			}
		})
	}
}

func TestGeminiClientAcceptsMatchingBodyModel(t *testing.T) {
	var calls int
	handler := geminiChatHandler(t, nil, &calls)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, geminiClientRequest(
		"/v1beta/models/gemini-x:generateContent?alt=json",
		`{"model":"models/gemini-x","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}
