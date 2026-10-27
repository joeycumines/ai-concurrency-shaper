// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// committedRefusalMount builds a dual-mapped Messages route over an upstream
// that answers every request, so a request refused before dispatch can only
// be refused by the route itself. Comments are enabled and the limiter holds
// exactly one slot, letting the test commit the streaming representation
// before admission and then release it into the refusal.
func committedRefusalMount(t *testing.T) (*Proxy, *multiDialectUpstream, *metrics.Collector) {
	t.Helper()
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	lossPolicy := transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureToolSchemaStrictness:   {},
		transcode.FeatureUsageUnknown:           {},
		transcode.FeatureUsageCacheReadUnknown:  {},
		transcode.FeatureUsageCacheWriteUnknown: {},
		transcode.FeatureUsageReasoningUnknown:  {},
	}}
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	// The messages-native model below is mapped by no target, so the
	// dispatcher refuses it; the identity-fallback model reaches chat.
	modelMap := transcode.ModelMap{
		Exact: map[string]transcode.ModelMapping{
			"native-only": {
				ClientModel: "native-only", UpstreamModel: "wire-native",
				ClientResponseModel: "native-only", Via: transcode.NativeMessages,
			},
		},
		AllowIdentity: true,
	}
	messagesChat := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         modelMap,
		LossPolicy:       lossPolicy}
	messagesResponses := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap:         modelMap,
		LossPolicy:       lossPolicy}

	met := metrics.NewCollector()
	pat, err := route.Parse("POST /messages:1")
	if err != nil {
		t.Fatalf("parse route pattern: %v", err)
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithQueueComments(10*time.Millisecond),
		WithQueueTimeout(10*time.Second),
		WithTranscodeMapping(messagesChat, messagesResponses),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}
	return p, upstream, met
}

// TestDispatcherCommittedStreamRefusalIsSSEErrorTerminal pins the committed
// stream contract for a dispatcher refusal: with SSE queue comments enabled
// the client has already received 200 + text/event-stream, so a local refusal
// must arrive as a well-formed Anthropic error terminal after the queue-wait
// comments — never as an unframed JSON body, and never as the recorder's
// upstream-blame close comment. The exchange counts as aborted, not proxied.
func TestDispatcherCommittedStreamRefusalIsSSEErrorTerminal(t *testing.T) {
	p, upstream, met := committedRefusalMount(t)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"native-only","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	body := string(raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (committed by the queue comments); body: %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream (committed before admission)", ct)
	}
	if !strings.Contains(body, ": queue-wait elapsed=0") {
		t.Fatalf("queue-wait comment missing from the committed stream: %q", body)
	}
	if !strings.Contains(body, "event: error\n") {
		t.Fatalf("refusal must be framed as an SSE error terminal: %q", body)
	}
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "native-only") {
		t.Fatalf("error terminal must carry the dialect error naming the model: %q", body)
	}
	// Every error payload must ride an SSE data line: an unframed JSON body
	// inside the committed stream is what this contract forbids.
	for i := 0; i < len(body); {
		idx := strings.Index(body[i:], `{"type":"error","error":`)
		if idx < 0 {
			break
		}
		at := i + idx
		if at < len("data: ") || body[at-len("data: "):at] != "data: " {
			t.Fatalf("unframed JSON error body written into the committed stream at byte %d: %q", at, body)
		}
		i = at + 1
	}
	if strings.Contains(body, ": queue-wait failed: upstream") {
		t.Fatalf("a local refusal must not be reported as an upstream failure: %q", body)
	}
	if len(upstream.requests) != 0 {
		t.Fatalf("upstream received %d requests, want 0 (refused before dispatch)", len(upstream.requests))
	}
	snap := met.Snapshot()
	if snap.TotalAborted != 1 {
		t.Fatalf("TotalAborted = %d, want 1 (the refused committed stream did not complete)", snap.TotalAborted)
	}
	if snap.TotalProxied != 0 {
		t.Fatalf("TotalProxied = %d, want 0 (a refusal is never a clean completion)", snap.TotalProxied)
	}
}

// TestNativeCommittedStreamLocalRefusalIsSSEErrorTerminal pins the same
// contract on the native path: a locally refused natively served request
// (here a model the native map does not serve) must not surface as the
// recorder's upstream-blame close comment after the queue comments.
func TestNativeCommittedStreamLocalRefusalIsSSEErrorTerminal(t *testing.T) {
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	met := metrics.NewCollector()
	pat, err := route.Parse("POST /messages:1")
	if err != nil {
		t.Fatalf("parse route pattern: %v", err)
	}
	nativeMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"chat-only": {
			ClientModel: "chat-only", UpstreamModel: "wire-chat",
			ClientResponseModel: "chat-only", Via: transcode.NativeChat,
		},
	}}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithQueueComments(10*time.Millisecond),
		WithQueueTimeout(10*time.Second),
		WithNativeRoutes(NativeRoute{
			RouteKey: transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
			Protocol: transcode.NativeMessages,
			Provider: "committed-native-refusal",
			ModelMap: nativeMap,
		}),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}

	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)
	req, rerr := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages",
		strings.NewReader(`{"model":"chat-only","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if rerr != nil {
		t.Fatalf("new request: %v", rerr)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	body := string(raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (committed by the queue comments); body: %q", resp.StatusCode, body)
	}
	if !strings.Contains(body, ": queue-wait elapsed=0") {
		t.Fatalf("queue-wait comment missing from the committed stream: %q", body)
	}
	if !strings.Contains(body, "event: error\n") {
		t.Fatalf("local native refusal must be framed as an SSE error terminal: %q", body)
	}
	if !strings.Contains(body, "chat-only") || !strings.Contains(body, "natively as chat") {
		t.Fatalf("error terminal must name the model and its native dialect: %q", body)
	}
	if strings.Contains(body, ": queue-wait failed: upstream") {
		t.Fatalf("a local refusal must not be reported as an upstream failure: %q", body)
	}
	if len(upstream.requests) != 0 {
		t.Fatalf("upstream received %d requests, want 0", len(upstream.requests))
	}
	if snap := met.Snapshot(); snap.TotalAborted != 1 || snap.TotalProxied != 0 {
		t.Fatalf("counters = aborted %d / proxied %d, want 1 / 0", snap.TotalAborted, snap.TotalProxied)
	}
}

// TestNativeLocal5xxMarksProxyGeneratedAfterCommit pins the classification
// input of a local 5xx on the native path: the queue-comment commit already
// wrote a terminal 200 through the recorder, so a terminalWritten-guarded
// marker would be skipped and the exchange's status override would be
// classified as an upstream failure — a purely local defect opening the
// breaker against a healthy upstream.
func TestNativeLocal5xxMarksProxyGeneratedAfterCommit(t *testing.T) {
	rec := httptest.NewRecorder()
	// The queue-comment commit: startQueueComments sets the flag and writes
	// the 200 (the recorder's commentsCommitted branch silences the dup, so
	// recordImplicitOK is what marks the exchange terminal).
	statusRec := &statusRecorder{ResponseWriter: rec, commentsCommitted: true}
	statusRec.recordImplicitOK(nil)
	if !statusRec.terminalWritten {
		t.Fatal("terminalWritten = false after the queue-comment commit, want true")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(transcode.WithCommittedStreamContext(req.Context()))
	writeNativeDialectError(statusRec, req, transcode.NativeMessages, http.StatusInternalServerError, "internal error")

	if !statusRec.proxyGeneratedError {
		t.Fatal("proxyGeneratedError = false after a local 5xx, want true (never an upstream health signal)")
	}
	if !strings.Contains(rec.Body.String(), "event: error\n") {
		t.Fatalf("body = %q, want the SSE error terminal", rec.Body.String())
	}
}

// TestDispatcherRefusalCountsAsDelivered pins the counting parity between a
// refusal the dispatcher renders and the identical refusal a handler renders:
// DownstreamComplete means "the response was delivered", so a delivered 4xx is
// a local failure that completed downstream — not an aborted exchange. Without
// the parity the same client-visible outcome counted as aborted on a
// multi-mapping route and as a clean request on a single-mapping one.
func TestDispatcherRefusalCountsAsDelivered(t *testing.T) {
	p, upstream, met := committedRefusalMount(t)

	// native-only is mapped as messages-native, which no target on the route
	// can serve: the dispatcher renders the 404 (no queue comments here).
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"native-only","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
	if len(upstream.requests) != 0 {
		t.Fatalf("upstream received %d requests, want 0", len(upstream.requests))
	}
	snap := met.Snapshot()
	if snap.TotalAborted != 0 {
		t.Fatalf("TotalAborted = %d, want 0 (the refusal was delivered, so the exchange completed downstream)", snap.TotalAborted)
	}
	// A refusal never reaches an upstream attempt, so it never increments the
	// clean-completion counter either — that is the pre-existing shape for a
	// handler-rendered refusal too. The property under test is that the
	// dispatcher matches it rather than adding an abort.
	if snap.TotalProxied != 0 {
		t.Fatalf("TotalProxied = %d, want 0 (a refusal is not an upstream completion)", snap.TotalProxied)
	}
}

// TestPanicClassificationAfterCommit pins the panic-recovery classification
// input under the queue comments. The 200 commit already marked the exchange
// terminal, so the recovered-panic branch must set proxyGeneratedError
// unconditionally: with the old terminalWritten guard it was skipped, and the
// local 502 was then read from the status override as an upstream failure.
//
// The assertion is on the classifier rather than the breaker because
// localPanic independently suppresses breaker reporting; the classification
// input (and the journal's proxy-generated fact) is what regresses.
func TestPanicClassificationAfterCommit(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), commentsCommitted: true}
	rec.recordImplicitOK(nil)
	if !rec.terminalWritten {
		t.Fatal("terminalWritten = false after the queue-comment commit, want true")
	}
	// Recovered panic: the branch marks the exchange aborted and
	// proxy-generated, and cannot rewrite the status.
	rec.aborted = true
	rec.proxyGeneratedError = true

	if isUpstreamFailureStatus(rec, time.Now(), nil) {
		t.Fatal("a proxy-generated 502 after the commit must not classify as an upstream failure")
	}

	// Contrast: the same recorder WITHOUT the fact is the defect being fixed —
	// the override 502 reads as an upstream failure.
	defective := &statusRecorder{
		ResponseWriter:         httptest.NewRecorder(),
		commentsCommitted:      true,
		terminalWritten:        true,
		upstreamStatusOverride: http.StatusBadGateway,
	}
	if !isUpstreamFailureStatus(defective, time.Now(), nil) {
		t.Fatal("precondition: an unmarked 502 override classifies as an upstream failure")
	}
}
