package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

const anthropicGatewayMessage = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
	`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,` +
	`"usage":{"input_tokens":3,"output_tokens":2}}`

// TestGeminiMessagesGatewayAppliesTargetAuth proves the Messages upstream
// receives the target's credential and version header, and that the client's
// own credentials never reach it.
func TestGeminiMessagesGatewayAppliesTargetAuth(t *testing.T) {
	var (
		gotPath   string
		gotHeader http.Header
		gotBody   string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotPath = r.URL.Path
		gotHeader = r.Header.Clone()
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicGatewayMessage)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(route.DefaultPatterns())),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(TranscodeMapping{
			ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1beta/models/{model}:generateContent"},
			ClientProtocol:   transcode.ClientGemini,
			UpstreamProtocol: transcode.UpstreamMessages,
			UpstreamPath:     "/v1/messages",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth: transcode.AuthPolicy{
				Mode:             transcode.AuthXAPIKey,
				Secret:           auth.NewStaticSecretSource("anthropic-secret"),
				AnthropicVersion: "2023-06-01",
			},
			LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
				transcode.FeatureUsageUnknown:           {},
				transcode.FeatureUsageCacheReadUnknown:  {},
				transcode.FeatureUsageCacheWriteUnknown: {},
				transcode.FeatureUsageReasoningUnknown:  {},
			}}}),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	req, err := http.NewRequest(http.MethodPost,
		front.URL+"/v1beta/models/claude-x:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":64}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", "client-goog-key")
	req.Header.Set("X-Api-Key", "client-key")
	req.Header.Set("Authorization", "Bearer client-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), `"text":"hello"`) {
		t.Fatalf("response %s", respBody)
	}

	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path %q", gotPath)
	}
	if got := gotHeader.Get("X-Api-Key"); got != "anthropic-secret" {
		t.Fatalf("upstream x-api-key %q, want the target secret", got)
	}
	if got := gotHeader.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Fatalf("upstream anthropic-version %q", got)
	}
	for _, name := range []string{"X-Goog-Api-Key", "Authorization"} {
		if got := gotHeader.Get(name); got != "" {
			t.Fatalf("client credential %s reached the upstream: %q", name, got)
		}
	}
	if !strings.Contains(gotBody, `"max_tokens":64`) {
		t.Fatalf("upstream body %s", gotBody)
	}
}
