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
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// dispatcherMount builds a proxy whose POST /v1/messages route is dual-mapped
// (messages->chat and messages->responses) over the shared recording upstream,
// with no native routes, so every request exercises the dispatcher's probe
// and selection paths directly.
func dispatcherMount(
	t *testing.T,
	modelMap transcode.ModelMap,
	profileMap transcode.ProfileMap,
	chatLimits, responsesLimits transcode.BodyLimits,
) (*Proxy, *multiDialectUpstream) {
	t.Helper()
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	lossPolicy := transcode.LossPolicy{
		Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureToolSchemaStrictness:   {},
			transcode.FeatureUsageUnknown:           {},
			transcode.FeatureUsageCacheReadUnknown:  {},
			transcode.FeatureUsageCacheWriteUnknown: {},
			transcode.FeatureUsageReasoningUnknown:  {},
		},
	}

	messagesChat := TranscodeMapping{
		Mapping: transcode.Mapping{
			ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			ModelMap:         modelMap,
			ProfileMap:       profileMap,
			LossPolicy:       lossPolicy,
		},
		BodyLimits: chatLimits,
	}
	messagesResponses := TranscodeMapping{
		Mapping: transcode.Mapping{
			ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamResponses,
			UpstreamPath:     "/v1/responses",
			ModelMap:         modelMap,
			ProfileMap:       profileMap,
			LossPolicy:       lossPolicy,
		},
		BodyLimits: responsesLimits,
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(messagesChat, messagesResponses),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}
	return p, upstream
}

// dispatcherModelMap is the shared model vocabulary of the dispatcher tests:
// one model per native dialect.
func dispatcherModelMap() transcode.ModelMap {
	return transcode.ModelMap{
		Exact: map[string]transcode.ModelMapping{
			"claude-opus": {
				ClientModel: "claude-opus", UpstreamModel: "wire-opus",
				ClientResponseModel: "claude-opus", Via: transcode.NativeMessages,
			},
			"gpt-nano": {
				ClientModel: "gpt-nano", UpstreamModel: "wire-nano",
				ClientResponseModel: "gpt-nano", Via: transcode.NativeResponses,
			},
			"deepseek-v3": {
				ClientModel: "deepseek-v3", UpstreamModel: "wire-deepseek",
				ClientResponseModel: "deepseek-v3", Via: transcode.NativeChat,
			},
		},
		RequireExplicitMap: true,
	}
}

func dispatcherPost(t *testing.T, p *Proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// failingBodyReader serves its bytes and then reports err instead of EOF,
// simulating a transport failure mid-body (client disconnect, connection
// reset, MaxBytesReader overrun) after a syntactically complete prefix.
type failingBodyReader struct {
	data []byte
	off  int
	err  error
}

func (r *failingBodyReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

func (r *failingBodyReader) Close() error { return nil }

// TestDispatcher_ProfileRoutesByTargetDialect verifies that a profile-named
// request resolves through the profile to its target model's Via dialect and
// reaches the matching upstream — the dispatcher must resolve profiles
// exactly like the handler does (profile first, then model map), not skip
// them and fall back to chat.
func TestDispatcher_ProfileRoutesByTargetDialect(t *testing.T) {
	modelMap := dispatcherModelMap()
	profileMap := transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
		"worker": {Model: "gpt-nano"},
	}}
	p, upstream := dispatcherMount(t, modelMap, profileMap, transcode.BodyLimits{}, transcode.BodyLimits{})

	rec := dispatcherPost(t, p, `{"model":"worker","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	last := upstream.lastRequest()
	if last.Path != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses (profile target is responses-native)", last.Path)
	}
	if last.Model != "wire-nano" {
		t.Errorf("upstream model = %q, want wire-nano", last.Model)
	}
}

// TestDispatcher_MessagesViaFailsClosed verifies that a messages-native model
// on a dual-mapped transcode route without a native messages route is
// refused with a 404 in the client dialect naming the model — never silently
// transcoded through the wrong upstream — and that the dispatcher records an
// outcome so the proxy's exactly-one-outcome invariant holds (no panic).
func TestDispatcher_MessagesViaFailsClosed(t *testing.T) {
	p, upstream := dispatcherMount(t, dispatcherModelMap(), transcode.ProfileMap{}, transcode.BodyLimits{}, transcode.BodyLimits{})

	rec := dispatcherPost(t, p, `{"model":"claude-opus","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "claude-opus") {
		t.Errorf("error body = %q, want it to name the refused model", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "messages") {
		t.Errorf("error body = %q, want it to name the model's native dialect", rec.Body.String())
	}
	if len(upstream.requests) != 0 {
		t.Errorf("upstream received %d requests, want 0 (fail closed before any upstream attempt)", len(upstream.requests))
	}
}

// TestDispatcher_BodyReadErrorFailsClosed verifies that a transport failure
// mid-body is surfaced to the chosen handler as a read error (rendered as a
// 400 or an abort classification), never silently converted into a clean
// body: a request whose connection died after a syntactically complete prefix
// must not be forwarded upstream as if it had arrived intact.
func TestDispatcher_BodyReadErrorFailsClosed(t *testing.T) {
	p, upstream := dispatcherMount(t, dispatcherModelMap(), transcode.ProfileMap{}, transcode.BodyLimits{}, transcode.BodyLimits{})

	reqBody := `{"model":"deepseek-v3","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Body = &failingBodyReader{data: []byte(reqBody), err: errors.New("simulated transport failure")}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if len(upstream.requests) != 0 {
		t.Errorf("upstream received %d requests, want 0 (read failure never becomes an upstream attempt)", len(upstream.requests))
	}
}

// TestDispatcher_OverLimitBodyRejected verifies that a request body larger
// than every target's accepted-request bound is rejected with 413 — the
// dispatcher may pre-read and replay the over-limit prefix, and the chosen
// handler's own bound must produce the refusal.
func TestDispatcher_OverLimitBodyRejected(t *testing.T) {
	limits := transcode.BodyLimits{AcceptedRequestBytes: 64}
	p, upstream := dispatcherMount(t, dispatcherModelMap(), transcode.ProfileMap{}, limits, limits)

	rec := dispatcherPost(t, p, `{"model":"deepseek-v3","max_tokens":64,"messages":[{"role":"user","content":"hi there, this body is deliberately longer than sixty four bytes"}]}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
	if len(upstream.requests) != 0 {
		t.Errorf("upstream received %d requests, want 0", len(upstream.requests))
	}
}

// TestDispatcher_UnresolvableModelKeepsHandlerError verifies that a model no
// target can resolve falls to the chat-first handler, which renders its own
// dialect-shaped model-resolution error — preserving single-handler
// semantics instead of the dispatcher inventing a second error shape.
func TestDispatcher_UnresolvableModelKeepsHandlerError(t *testing.T) {
	p, upstream := dispatcherMount(t, dispatcherModelMap(), transcode.ProfileMap{}, transcode.BodyLimits{}, transcode.BodyLimits{})

	rec := dispatcherPost(t, p, `{"model":"mystery-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "mystery-model") {
		t.Errorf("error body = %q, want it to name the unresolved model", rec.Body.String())
	}
	if len(upstream.requests) != 0 {
		t.Errorf("upstream received %d requests, want 0", len(upstream.requests))
	}
}

// TestDispatcher_NativeMissResolvesEmptyViaAgainstTargets verifies that a
// native miss which resolved no dialect (an identity fallback) does not stop
// the dispatcher: the route's mappings still resolve the model, and a model
// they name as responses-native reaches the Responses upstream even though
// the native route's own map knows nothing about its dialect.
func TestDispatcher_NativeMissResolvesAgainstTargets(t *testing.T) {
	nativeMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"m": {ClientModel: "m", UpstreamModel: "wire-m", ClientResponseModel: "m"},
	}}
	dualMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"m": {ClientModel: "m", UpstreamModel: "wire-m", ClientResponseModel: "m", Via: transcode.NativeResponses},
	}}

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
	nativeRoute := NativeRoute{
		RouteKey: transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		Protocol: transcode.NativeMessages,
		Provider: "dispatcher-test",
		ModelMap: nativeMap,
	}
	messagesChat := TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         dualMap, LossPolicy: lossPolicy,
	}}
	messagesResponses := TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap:         dualMap, LossPolicy: lossPolicy,
	}}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithNativeRoutes(nativeRoute),
		WithTranscodeMapping(messagesChat, messagesResponses),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}

	rec := dispatcherPost(t, p, `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	last := upstream.lastRequest()
	if last.Path != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses (the mappings' resolution decides an identity-fallback miss)", last.Path)
	}
	if last.Model != "wire-m" {
		t.Errorf("upstream model = %q, want wire-m", last.Model)
	}
}

// TestDispatcher_ConfigFrozenAfterNew verifies that mutating the caller's
// model map after New cannot change dispatch: the dispatcher resolves
// against its own frozen copy, like every handler the proxy builds.
func TestDispatcher_ConfigFrozenAfterNew(t *testing.T) {
	modelMap := dispatcherModelMap()
	p, upstream := dispatcherMount(t, modelMap, transcode.ProfileMap{}, transcode.BodyLimits{}, transcode.BodyLimits{})

	// Flip deepseek-v3's dialect after construction.
	modelMap.Exact["deepseek-v3"] = transcode.ModelMapping{
		ClientModel: "deepseek-v3", UpstreamModel: "wire-deepseek",
		ClientResponseModel: "deepseek-v3", Via: transcode.NativeResponses,
	}

	rec := dispatcherPost(t, p, `{"model":"deepseek-v3","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	last := upstream.lastRequest()
	if last.Path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions (post-New mutation must not change dispatch)", last.Path)
	}
}

// TestDispatcher_FirstTargetResolvesConflictingMaps verifies that when two
// mappings on one route carry different model maps, resolution tries each
// target's maps in registration order and the first target that resolves
// the model decides the dialect — deterministic, never "last mapping wins".
func TestDispatcher_FirstTargetResolvesConflictingMaps(t *testing.T) {
	chatMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"m": {ClientModel: "m", UpstreamModel: "wire-chat-m", ClientResponseModel: "m", Via: transcode.NativeChat},
	}}
	responsesMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"m": {ClientModel: "m", UpstreamModel: "wire-resp-m", ClientResponseModel: "m", Via: transcode.NativeResponses},
	}}

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
	messagesChat := TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         chatMap, LossPolicy: lossPolicy,
	}}
	messagesResponses := TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap:         responsesMap, LossPolicy: lossPolicy,
	}}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(messagesChat, messagesResponses),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}

	rec := dispatcherPost(t, p, `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	last := upstream.lastRequest()
	if last.Path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions (first target's map resolves first)", last.Path)
	}
	if last.Model != "wire-chat-m" {
		t.Errorf("upstream model = %q, want wire-chat-m (the chat target's own map renders it)", last.Model)
	}
}

// TestNativeMissStampsContextForEmptyVia verifies nativeRouteAction stamps
// the miss info even when the model resolves with an empty Via, so the
// dispatcher never needs a second body read to reach the same conclusion.
func TestNativeMissStampsContextForEmptyVia(t *testing.T) {
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	nativeMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"m": {ClientModel: "m", UpstreamModel: "wire-m", ClientResponseModel: "m"},
	}}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithNativeRoutes(NativeRoute{
			RouteKey: transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
			Protocol: transcode.NativeMessages,
			Provider: "miss-stamp-test",
			ModelMap: nativeMap,
		}),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}
	nr := NativeRoute{
		RouteKey: transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		Protocol: transcode.NativeMessages,
		Provider: "miss-stamp-test",
		ModelMap: nativeMap,
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		bytes.NewBufferString(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	out, action := p.nativeRouteAction(rec, req, &nr)
	if action != nativeMiss {
		t.Fatalf("action = %v, want nativeMiss", action)
	}
	miss, ok := nativeMissFromContext(out.Context())
	if !ok {
		t.Fatal("nativeMissFromContext: not found, want the miss to be stamped for an empty-Via resolution")
	}
	if miss.clientModel != "m" {
		t.Errorf("miss.clientModel = %q, want m", miss.clientModel)
	}
	if miss.via != "" {
		t.Errorf("miss.via = %q, want empty", miss.via)
	}
}

// TestNew_RejectsConflictingClientProtocolsOnRoute verifies the library
// surface rejects a route whose mappings disagree on the client dialect:
// one client route is one client dialect, and construction fails instead
// of building a dispatcher that would parse requests and render errors in
// the wrong shape. The two mappings use different upstream protocols so
// the duplicate check passes and the conflict is the only rejection left.
func TestNew_RejectsConflictingClientProtocolsOnRoute(t *testing.T) {
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
	messagesRoute := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	messagesToResponses := TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute:      messagesRoute,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap:         transcode.ModelMap{AllowIdentity: true},
		LossPolicy:       lossPolicy,
	}}
	responsesToChat := TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute:      messagesRoute,
		ClientProtocol:   transcode.ClientResponses,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         transcode.ModelMap{AllowIdentity: true},
		LossPolicy:       lossPolicy,
	}}

	_, err = New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(messagesToResponses, responsesToChat),
	)
	if err == nil {
		t.Fatal("New: nil error, want conflicting client protocol rejection")
	}
	if !strings.Contains(err.Error(), "conflicting client protocols") {
		t.Errorf("error = %v, want it to name the conflicting client protocols", err)
	}
}

// TestNew_DuplicateRouteNamesUpstreamProtocol verifies the duplicate
// (client route, upstream protocol) rejection at the library surface
// identifies the exact collision, now that one client route may
// legitimately carry mappings to several different upstreams.
func TestNew_DuplicateRouteNamesUpstreamProtocol(t *testing.T) {
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
	messagesRoute := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	messagesToChat := TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute:      messagesRoute,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         transcode.ModelMap{AllowIdentity: true},
		LossPolicy:       lossPolicy,
	}}
	duplicate := messagesToChat

	_, err = New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(messagesToChat, duplicate),
	)
	if err == nil {
		t.Fatal("New: nil error, want duplicate rejection")
	}
	if !strings.Contains(err.Error(), "chat-completions") {
		t.Errorf("error = %v, want it to name the duplicated upstream protocol", err)
	}
}

// countingBody counts reads against its underlying reader.
type countingBody struct {
	reads int
	r     io.Reader
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

func (c *countingBody) Close() error { return nil }

// TestDispatcher_MissSkipsBodyProbe verifies the dispatcher's probe-skip
// directly: a request carrying native miss info is never read or replaced by
// the dispatcher (the native path already extracted the model name). The
// selected handler receives the original body untouched.
func TestDispatcher_MissSkipsBodyProbe(t *testing.T) {
	var stubbed []*http.Request
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stubbed = append(stubbed, r)
		w.WriteHeader(http.StatusOK)
	})
	mapping := &TranscodeMapping{
		Mapping: transcode.Mapping{
			ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			ModelMap:         transcode.ModelMap{AllowIdentity: true},
		},
	}
	dispatcher, err := newMultiTranscodeDispatcher(mapping.ClientRoute, []targetTranscodeHandler{
		{mapping: mapping, handler: stub},
	})
	if err != nil {
		t.Fatalf("newMultiTranscodeDispatcher: %v", err)
	}

	body := &countingBody{r: bytes.NewBufferString(`{"model":"deepseek-v3","messages":[]}`)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Body = body
	req = req.WithContext(withNativeMiss(req.Context(), nativeMissInfo{
		clientModel: "deepseek-v3",
		via:         transcode.NativeChat,
	}))
	rec := httptest.NewRecorder()
	dispatcher.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body.reads != 0 {
		t.Errorf("original body reads = %d, want 0 (miss info must skip the dispatcher's probe)", body.reads)
	}
	if len(stubbed) != 1 {
		t.Fatalf("stub handler invocations = %d, want 1", len(stubbed))
	}
	if got, ok := stubbed[0].Body.(*countingBody); !ok || got != body {
		t.Error("handler body was replaced, want the original body passed through untouched")
	}
}
