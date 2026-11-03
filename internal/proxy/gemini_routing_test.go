package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

const geminiRoutingChatReply = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gemini-x",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

// geminiRoutingUpstream records every "METHOD path" it receives and answers
// the chat completions path with a chat reply.
type geminiRoutingUpstream struct {
	mu   sync.Mutex
	seen []string
}

func (u *geminiRoutingUpstream) handler(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.seen = append(u.seen, r.Method+" "+r.URL.Path)
	u.mu.Unlock()
	if r.URL.Path == "/v1/chat/completions" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geminiRoutingChatReply)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{}`)
}

func (u *geminiRoutingUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seen...)
}

func TestGeminiRouteReachesTranscodeHandler(t *testing.T) {
	upstream := &geminiRoutingUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(route.DefaultPatterns())),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(TranscodeMapping{
			ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1beta/models/{model}:generateContent"},
			ClientProtocol:   transcode.ClientGemini,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
			Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
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

	t.Run("a concrete model path is served by the Gemini mapping", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-x:generateContent",
			strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"text":"hello"`) {
			t.Fatalf("body %s does not carry the translated text", rec.Body.String())
		}
		if got := upstream.requests(); len(got) != 1 || got[0] != "POST /v1/chat/completions" {
			t.Fatalf("upstream saw %v, want one chat completions request", got)
		}
	})

	t.Run("a non-POST method on the Gemini path passes through transparently", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1beta/models/gemini-x:generateContent", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		got := upstream.requests()
		if len(got) < 2 || got[len(got)-1] != "GET /v1beta/models/gemini-x:generateContent" {
			t.Fatalf("upstream saw %v, want the GET forwarded unchanged", got)
		}
	})

	t.Run("an unmatched Gemini action is not claimed by the mapping", func(t *testing.T) {
		before := len(upstream.requests())
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-x:countTokens", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		got := upstream.requests()
		if len(got) != before+1 || got[len(got)-1] != "POST /v1beta/models/gemini-x:countTokens" {
			t.Fatalf("upstream saw %v, want the countTokens request forwarded unchanged", got)
		}
	})
}
