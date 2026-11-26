package proxy

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func TestProxyNativeCaseDistinctDataNamesPreserveBytes(t *testing.T) {
	for name, body := range map[string]string{
		"schema properties":   `{"model":"msg-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{"type":"object","properties":{"ID":{"type":"string"},"id":{"type":"number"}}}}]}`,
		"metadata":            `{"model":"msg-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"customer":1,"Customer":2}}`,
		"provider extensions": `{"model":"msg-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"Extension":1,"extension":2}`,
	} {
		t.Run(name, func(t *testing.T) {
			var received string
			p, err := New(
				WithUpstream(&url.URL{Scheme: "http", Host: "upstream.invalid"}),
				WithMatcher(route.NewMatcher(nil)),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(metrics.NewCollector()),
				WithNativeRoutes(nativeRoute(t, transcode.NativeMessages, "/v1/messages")),
				WithTransport(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					payload, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					received = string(payload)
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: r}, nil
				})),
			)
			if err != nil {
				t.Fatal(err)
			}
			rec := postNative(t, p, "/v1/messages", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("native request was refused: %d %s", rec.Code, rec.Body.String())
			}
			want := strings.Replace(body, `"msg-model"`, `"wire-msg"`, 1)
			if received != want {
				t.Fatalf("native bytes changed: got %s, want %s", received, want)
			}
		})
	}
}
