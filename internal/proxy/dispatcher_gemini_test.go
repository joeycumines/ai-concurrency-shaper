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

// geminiRouteKey is the client route key of the Gemini GenerateContent
// template the dispatcher tests register.
var geminiRouteKey = transcode.RouteKey{Method: http.MethodPost, Path: "/v1beta/models/{model}:generateContent"}

// geminiRecordingHandler records that it served a request and returns a
// minimal Gemini GenerateContentResponse.
type geminiRecordingHandler struct {
	name   string
	mu     sync.Mutex
	hits   int
	bodies [][]byte
}

func (h *geminiRecordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.hits++
	h.bodies = append(h.bodies, body)
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
}

func (h *geminiRecordingHandler) served() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits
}

// geminiDispatcherTarget builds one mapping target on the Gemini route whose
// model map carries the given exact entries and whose handler records hits.
func geminiDispatcherTarget(up transcode.UpstreamProtocol, upstreamPath string, modelMap transcode.ModelMap, profiles map[string]transcode.ProfileMapping, handler http.Handler) targetTranscodeHandler {
	return targetTranscodeHandler{
		mapping: &TranscodeMapping{
			ClientRoute:      geminiRouteKey,
			ClientProtocol:   transcode.ClientGemini,
			UpstreamProtocol: up,
			UpstreamPath:     upstreamPath,
			ModelMap:         modelMap,
			ProfileMap:       transcode.ProfileMap{Profiles: profiles},
			LossPolicy:       claimsLossPolicy()},
		handler: handler,
	}
}

// geminiViaMap builds a model map with one exact entry per dialect.
func geminiViaMap(entries map[string]transcode.NativeProtocol) transcode.ModelMap {
	exact := make(map[string]transcode.ModelMapping, len(entries))
	for model, via := range entries {
		exact[model] = transcode.ModelMapping{
			ClientModel:         model,
			UpstreamModel:       "wire-" + model,
			ClientResponseModel: model,
			Via:                 via,
		}
	}
	return transcode.ModelMap{Exact: exact, RequireExplicitMap: true}
}

// geminiDispatchPost sends a POST to the Gemini JSON operation path through
// the dispatcher and returns the recorder.
func geminiDispatchPost(t *testing.T, d *multiTranscodeDispatcher, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	return rec
}

// failingBody is a request body whose Read fails, proving the dispatcher never
// touched the body.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failingBody) Close() error             { return nil }

// TestDispatcher_GeminiRouteDispatchesByVia runs a Gemini route carrying chat,
// responses, and messages mappings in both registration orders and asserts the
// same handler serves each via model, with byte-identical refusals.
func TestDispatcher_GeminiRouteDispatchesByVia(t *testing.T) {
	modelMap := geminiViaMap(map[string]transcode.NativeProtocol{
		"m-chat":      transcode.NativeChat,
		"m-responses": transcode.NativeResponses,
		"m-messages":  transcode.NativeMessages,
		"m-gemini":    transcode.NativeGemini,
	})
	chatH, respH, msgH := &geminiRecordingHandler{name: "chat"}, &geminiRecordingHandler{name: "responses"}, &geminiRecordingHandler{name: "messages"}
	chat := geminiDispatcherTarget(transcode.UpstreamChatCompletions, "/v1/chat/completions", modelMap, nil, chatH)
	resp := geminiDispatcherTarget(transcode.UpstreamResponses, "/v1/responses", modelMap, nil, respH)
	msgs := geminiDispatcherTarget(transcode.UpstreamMessages, "/v1/messages", modelMap, nil, msgH)

	var bodies map[string]string
	for _, order := range [][]targetTranscodeHandler{{chat, resp, msgs}, {msgs, resp, chat}, {resp, msgs, chat}} {
		d, err := newMultiTranscodeDispatcher(geminiRouteKey, order)
		if err != nil {
			t.Fatalf("newMultiTranscodeDispatcher: %v", err)
		}
		if bodies == nil {
			bodies = map[string]string{}
		}
		for _, tc := range []struct {
			model  string
			want   *geminiRecordingHandler
			status int
		}{
			{"m-chat", chatH, http.StatusOK},
			{"m-responses", respH, http.StatusOK},
			{"m-gemini", nil, http.StatusNotFound},
		} {
			chatH.hits, respH.hits, msgH.hits = 0, 0, 0
			rec := geminiDispatchPost(t, d, "/v1beta/models/"+tc.model+":generateContent", `{"contents":[]}`)
			if rec.Code != tc.status {
				t.Fatalf("order %v model %s: status = %d, want %d; body: %s", order[0].mapping.UpstreamProtocol, tc.model, rec.Code, tc.status, rec.Body.String())
			}
			key := tc.model
			if prev, ok := bodies[key]; ok {
				if prev != rec.Body.String() {
					t.Fatalf("model %s: refusal bytes differ across registration orders:\n%s\nvs\n%s", tc.model, prev, rec.Body.String())
				}
			} else {
				bodies[key] = rec.Body.String()
			}
			if tc.want != nil && tc.want.served() != 1 {
				t.Fatalf("model %s: want the %s handler to serve exactly once, got %d", tc.model, tc.want.name, tc.want.served())
			}
			if tc.want == nil && (chatH.served()+respH.served()+msgH.served()) != 0 {
				t.Fatalf("model %s: a transcode handler ran for a native-dialect model", tc.model)
			}
		}
	}
	// via=gemini on a Gemini client route is the client's own dialect: 404.
	if !strings.Contains(bodies["m-gemini"], "served natively as gemini") {
		t.Fatalf("via=gemini refusal = %q, want the own-dialect 404", bodies["m-gemini"])
	}
}

// TestDispatcher_GeminiRouteRawModelFallsToChat pins that a model with no via
// fact goes to the chat fallback on a Gemini route.
func TestDispatcher_GeminiRouteRawModelFallsToChat(t *testing.T) {
	chatH := &geminiRecordingHandler{name: "chat"}
	respH := &geminiRecordingHandler{name: "responses"}
	identity := transcode.ModelMap{AllowIdentity: true}
	chat := geminiDispatcherTarget(transcode.UpstreamChatCompletions, "/v1/chat/completions", identity, nil, chatH)
	resp := geminiDispatcherTarget(transcode.UpstreamResponses, "/v1/responses", identity, nil, respH)
	for _, order := range [][]targetTranscodeHandler{{chat, resp}, {resp, chat}} {
		d, err := newMultiTranscodeDispatcher(geminiRouteKey, order)
		if err != nil {
			t.Fatalf("newMultiTranscodeDispatcher: %v", err)
		}
		rec := geminiDispatchPost(t, d, "/v1beta/models/raw-model:generateContent", `{"contents":[]}`)
		if rec.Code != http.StatusOK || chatH.served() != 1 || respH.served() != 0 {
			t.Fatalf("order %v: raw model = %d hits chat=%d resp=%d, want chat only", order[0].mapping.UpstreamProtocol, rec.Code, chatH.served(), respH.served())
		}
		chatH.hits, respH.hits = 0, 0
	}
}

// TestDispatcher_GeminiRouteSeveralNonChatRefusal pins the sorted refusal when
// no chat mapping exists and the model declares no dialect.
func TestDispatcher_GeminiRouteSeveralNonChatRefusal(t *testing.T) {
	respH := &geminiRecordingHandler{name: "responses"}
	msgH := &geminiRecordingHandler{name: "messages"}
	modelMap := geminiViaMap(map[string]transcode.NativeProtocol{"x": transcode.NativeResponses})
	resp := geminiDispatcherTarget(transcode.UpstreamResponses, "/v1/responses", modelMap, nil, respH)
	msgs := geminiDispatcherTarget(transcode.UpstreamMessages, "/v1/messages", modelMap, nil, msgH)
	var bodies []string
	for _, order := range [][]targetTranscodeHandler{{resp, msgs}, {msgs, resp}} {
		d, err := newMultiTranscodeDispatcher(geminiRouteKey, order)
		if err != nil {
			t.Fatalf("newMultiTranscodeDispatcher: %v", err)
		}
		// identity model, no via: no chat fallback, several non-chat handlers.
		d2targets := d
		_ = d2targets
		rec := geminiDispatchPost(t, d, "/v1beta/models/raw-model:generateContent", `{"contents":[]}`)
		// raw-model is unmapped (RequireExplicitMap), so resolve fails and the
		// fallback path runs: no chat, two non-chat handlers -> 500 sorted list.
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
		}
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("refusal bytes differ across orders:\n%s\nvs\n%s", bodies[0], bodies[1])
	}
	if !strings.Contains(bodies[0], "messages and responses") {
		t.Fatalf("refusal = %q, want the sorted dialect list", bodies[0])
	}
}

// TestDispatcher_GeminiRouteNeverReadsBody proves the dispatcher routes a
// Gemini request by the path model alone: the body fails on first read and the
// request still dispatches.
func TestDispatcher_GeminiRouteNeverReadsBody(t *testing.T) {
	chatH := &geminiRecordingHandler{name: "chat"}
	modelMap := geminiViaMap(map[string]transcode.NativeProtocol{"m": transcode.NativeChat})
	chat := geminiDispatcherTarget(transcode.UpstreamChatCompletions, "/v1/chat/completions", modelMap, nil, chatH)
	d, err := newMultiTranscodeDispatcher(geminiRouteKey, []targetTranscodeHandler{chat})
	if err != nil {
		t.Fatalf("newMultiTranscodeDispatcher: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", failingBody{})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || chatH.served() != 1 {
		t.Fatalf("status = %d, hits = %d; want 200 and one handler hit despite the failing body; body: %s", rec.Code, chatH.served(), rec.Body.String())
	}
}

// TestDispatcher_GeminiProfileClaimedByTwoNonChatMappingsRefused pins that a
// profile whose target model resolves by identity on two non-chat mappings is
// refused identically in both registration orders, and its name never reaches
// a handler.
func TestDispatcher_GeminiProfileClaimedByTwoNonChatMappingsRefused(t *testing.T) {
	respH := &geminiRecordingHandler{name: "responses"}
	msgH := &geminiRecordingHandler{name: "messages"}
	profiles := map[string]transcode.ProfileMapping{"prof": {Model: "target"}}
	identity := transcode.ModelMap{AllowIdentity: true}
	resp := geminiDispatcherTarget(transcode.UpstreamResponses, "/v1/responses", identity, profiles, respH)
	msgs := geminiDispatcherTarget(transcode.UpstreamMessages, "/v1/messages", identity, profiles, msgH)
	var bodies []string
	for _, order := range [][]targetTranscodeHandler{{resp, msgs}, {msgs, resp}} {
		d, err := newMultiTranscodeDispatcher(geminiRouteKey, order)
		if err != nil {
			t.Fatalf("newMultiTranscodeDispatcher: %v", err)
		}
		rec := geminiDispatchPost(t, d, "/v1beta/models/prof:generateContent", `{"contents":[]}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
		}
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("refusal bytes differ across orders:\n%s\nvs\n%s", bodies[0], bodies[1])
	}
	if respH.served() != 0 || msgH.served() != 0 {
		t.Fatalf("a handler ran for an ambiguous profile: resp=%d msg=%d", respH.served(), msgH.served())
	}
}

// TestDispatcher_GeminiProfileViaMessagesNotDefinedRefused pins the
// profile-guard generalization: on a Gemini route, a profile whose target
// model is via=messages is refused by the profile check (the messages handler
// does not define the profile), and the profile name never reaches a handler.
func TestDispatcher_GeminiProfileViaMessagesNotDefinedRefused(t *testing.T) {
	chatH := &geminiRecordingHandler{name: "chat"}
	msgH := &geminiRecordingHandler{name: "messages"}
	profiles := map[string]transcode.ProfileMapping{"prof": {Model: "wire-msg"}}
	// chat defines the profile; its target model is via=messages on the
	// messages mapping's map.
	chatMap := geminiViaMap(map[string]transcode.NativeProtocol{"wire-msg": transcode.NativeMessages})
	chatMap.AllowIdentity = true
	msgMap := geminiViaMap(map[string]transcode.NativeProtocol{"wire-msg": transcode.NativeMessages})
	chat := geminiDispatcherTarget(transcode.UpstreamChatCompletions, "/v1/chat/completions", chatMap, profiles, chatH)
	msgs := geminiDispatcherTarget(transcode.UpstreamMessages, "/v1/messages", msgMap, nil, msgH)
	for _, order := range [][]targetTranscodeHandler{{chat, msgs}, {msgs, chat}} {
		d, err := newMultiTranscodeDispatcher(geminiRouteKey, order)
		if err != nil {
			t.Fatalf("newMultiTranscodeDispatcher: %v", err)
		}
		rec := geminiDispatchPost(t, d, "/v1beta/models/prof:generateContent", `{"contents":[]}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("order %v: status = %d, want 500; body: %s", order[0].mapping.UpstreamProtocol, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "does not define the profile") {
			t.Fatalf("body = %q, want the profile refusal", rec.Body.String())
		}
		if chatH.served() != 0 || msgH.served() != 0 {
			t.Fatalf("order %v: a handler ran: chat=%d msg=%d", order[0].mapping.UpstreamProtocol, chatH.served(), msgH.served())
		}
		chatH.hits, msgH.hits = 0, 0
	}
}

// TestDispatcher_MessagesRouteOwnDialectNamesRouteDialects pins the generalized
// refusal wording: on a /v1/messages route with chat and responses mappings, a
// via=messages model gets a 404 whose dialect list is computed from the route's
// handlers, identical in both registration orders.
func TestDispatcher_MessagesRouteOwnDialectNamesRouteDialects(t *testing.T) {
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	modelMap := geminiViaMap(map[string]transcode.NativeProtocol{"m": transcode.NativeMessages})
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	chat := targetTranscodeHandler{mapping: &TranscodeMapping{
		ClientRoute: routeKey, ClientProtocol: transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions, UpstreamPath: "/v1/chat/completions",
		ModelMap: modelMap}, handler: stub}
	resp := targetTranscodeHandler{mapping: &TranscodeMapping{
		ClientRoute: routeKey, ClientProtocol: transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses, UpstreamPath: "/v1/responses",
		ModelMap: modelMap}, handler: stub}
	var bodies []string
	for _, order := range [][]targetTranscodeHandler{{chat, resp}, {resp, chat}} {
		d, err := newMultiTranscodeDispatcher(routeKey, order)
		if err != nil {
			t.Fatalf("newMultiTranscodeDispatcher: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"m","max_tokens":1,"messages":[]}`))
		r2 := httptest.NewRecorder()
		d.ServeHTTP(r2, req)
		if r2.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body: %s", r2.Code, r2.Body.String())
		}
		bodies = append(bodies, r2.Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("refusal bytes differ across orders:\n%s\nvs\n%s", bodies[0], bodies[1])
	}
	if !strings.Contains(bodies[0], "transcodes to the chat and responses upstreams only") {
		t.Fatalf("refusal = %q, want the computed sorted dialect list", bodies[0])
	}
}

// geminiGatewayMount builds a real Proxy with a Gemini route carrying three
// mappings over a recording fake upstream, for gateway-level dispatch tests.
func geminiGatewayMount(t *testing.T, order []targetTranscodeHandler) *Proxy {
	t.Helper()
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	mappings := make([]TranscodeMapping, 0, len(order))
	for _, th := range order {
		mappings = append(mappings, *th.mapping)
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(mappings...),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}
	return p
}

// TestDispatcher_GeminiGatewayDispatchesByPathModel runs the full proxy: the
// same Gemini request through both registration orders reaches the same
// upstream path.
func TestDispatcher_GeminiGatewayDispatchesByPathModel(t *testing.T) {
	modelMap := geminiViaMap(map[string]transcode.NativeProtocol{
		"g-chat": transcode.NativeChat,
		"g-resp": transcode.NativeResponses,
	})
	mk := func() []targetTranscodeHandler {
		return []targetTranscodeHandler{
			geminiDispatcherTarget(transcode.UpstreamChatCompletions, "/v1/chat/completions", modelMap, nil, http.NotFoundHandler()),
			geminiDispatcherTarget(transcode.UpstreamResponses, "/v1/responses", modelMap, nil, http.NotFoundHandler()),
		}
	}
	forward := mk()
	reverse := []targetTranscodeHandler{forward[1], forward[0]}
	for _, order := range [][]targetTranscodeHandler{forward, reverse} {
		p := geminiGatewayMount(t, order)
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/g-resp:generateContent", bytes.NewBufferString(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("order %v: status = %d, want 200; body: %s", order[0].mapping.UpstreamProtocol, rec.Code, rec.Body.String())
		}
	}
}
