package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func TestProbe_Messages404Text(t *testing.T) {
	p, upstream := dispatcherMount(t, dispatcherModelMap(), transcode.ProfileMap{}, transcode.BodyLimits{}, transcode.BodyLimits{})
	rec := dispatcherPost(t, p, `{"model":"","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	t.Logf("status=%d body=%s", rec.Code, rec.Body.String())
	if len(upstream.requests) != 0 {
		t.Logf("upstream requests = %d", len(upstream.requests))
	}
	if !strings.Contains(rec.Body.String(), "-native-route") {
		t.Logf("PROBE: reachable 404 does NOT name -native-route")
	} else {
		t.Logf("PROBE: reachable 404 names -native-route")
	}
}

func TestProbe_NonCommittedRefusalOutcome(t *testing.T) {
	upstream := &multiDialectUpstream{}
	met := metrics.NewCollector()
	p, _ := dispatcherMountWithMetrics(t, met)
	rec := dispatcherPost(t, p, `{"model":"mystery-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	t.Logf("status=%d body=%s", rec.Code, rec.Body.String())
	_ = upstream
	snap := met.Snapshot()
	t.Logf("PROBE: TotalProxied=%d TotalAborted=%d TotalPassThrough=%d", snap.TotalProxied, snap.TotalAborted, snap.TotalPassThrough)
}

func dispatcherMountWithMetrics(t *testing.T, met *metrics.Collector) (*Proxy, *multiDialectUpstream) {
	t.Helper()
	upstream := &multiDialectUpstream{}
	srv := newTestServer(t, upstream.handler)
	upstreamURL := mustParseURL(t, srv)
	lossPolicy := transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureToolSchemaStrictness: {},
		transcode.FeatureUsageUnknown:         {},
	}}
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	chat := TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute: routeKey, ClientProtocol: transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions, UpstreamPath: "/v1/chat/completions",
		ModelMap: transcode.ModelMap{AllowIdentity: true}, LossPolicy: lossPolicy,
	}}
	resp := TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute: routeKey, ClientProtocol: transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses, UpstreamPath: "/v1/responses",
		ModelMap: transcode.ModelMap{AllowIdentity: true}, LossPolicy: lossPolicy,
	}}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(met),
		WithTranscodeMapping(chat, resp),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, upstream
}
