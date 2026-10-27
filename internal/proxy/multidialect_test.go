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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

type multiDialectRecordedRequest struct {
	Path             string
	Method           string
	AuthHeader       string
	XAPIKeyHeader    string
	AnthropicVersion string
	Body             []byte
	Model            string
}

type multiDialectUpstream struct {
	mu       sync.Mutex
	requests []multiDialectRecordedRequest
}

func (u *multiDialectUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)

	u.mu.Lock()
	u.requests = append(u.requests, multiDialectRecordedRequest{
		Path:             r.URL.Path,
		Method:           r.Method,
		AuthHeader:       r.Header.Get("Authorization"),
		XAPIKeyHeader:    r.Header.Get("X-Api-Key"),
		AnthropicVersion: r.Header.Get("Anthropic-Version"),
		Body:             body,
		Model:            probe.Model,
	})
	u.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	switch r.URL.Path {
	case "/v1/messages":
		// Anthropic Messages response format
		resp := map[string]any{
			"id":    "msg_test123",
			"type":  "message",
			"role":  "assistant",
			"model": probe.Model,
			"content": []map[string]any{
				{"type": "text", "text": "Hello from messages upstream"},
			},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 15,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)

	case "/v1/chat/completions":
		// OpenAI Chat Completions response format
		resp := map[string]any{
			"id":      "chatcmpl-test123",
			"object":  "chat.completion",
			"created": 1234567890,
			"model":   probe.Model,
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "Hello from chat completions upstream",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 15,
				"total_tokens":      25,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)

	case "/v1/responses":
		// OpenAI Responses response format
		resp := map[string]any{
			"id":      "resp_test123",
			"object":  "response",
			"created": 1234567890,
			"model":   probe.Model,
			"status":  "completed",
			"output": []map[string]any{
				{
					"id":     "item_test123",
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{
							"type": "output_text",
							"text": "Hello from responses upstream",
						},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 15,
				"total_tokens":  25,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)

	default:
		http.NotFound(w, r)
	}
}

func (u *multiDialectUpstream) lastRequest() multiDialectRecordedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		return multiDialectRecordedRequest{}
	}
	return u.requests[len(u.requests)-1]
}

func testMultiDialectModelMap() transcode.ModelMap {
	return transcode.ModelMap{
		Exact: map[string]transcode.ModelMapping{
			"claude-opus": {
				ClientModel:         "claude-opus",
				UpstreamModel:       "wire-opus",
				ClientResponseModel: "claude-opus",
				Via:                 transcode.NativeMessages,
			},
			"gpt-nano": {
				ClientModel:         "gpt-nano",
				UpstreamModel:       "wire-nano",
				ClientResponseModel: "gpt-nano",
				Via:                 transcode.NativeResponses,
			},
			"deepseek-v3": {
				ClientModel:         "deepseek-v3",
				UpstreamModel:       "wire-deepseek",
				ClientResponseModel: "deepseek-v3",
				Via:                 transcode.NativeChat,
			},
		},
		AllowIdentity:      false,
		RequireExplicitMap: true,
	}
}

// TestMultiDialect_SingleMount_RoutingAndAuth tests a single provider mount configured with
// native routes and transcode mappings for chat, messages, and responses, verifying:
// 1. POST /v1/messages for via=messages routes natively and applies X-Api-Key + Anthropic-Version
// 2. POST /v1/messages for via=chat transcodes to /v1/chat/completions and applies Authorization: Bearer
// 3. POST /v1/messages for via=responses transcodes to /v1/responses and applies Authorization: Bearer
// 4. POST /v1/responses for via=responses routes natively and applies Authorization: Bearer
// 5. POST /v1/responses for via=chat transcodes to /v1/chat/completions and applies Authorization: Bearer
// 6. POST /v1/chat/completions for via=chat routes natively and applies Authorization: Bearer
func TestMultiDialect_SingleMount_RoutingAndAuth(t *testing.T) {
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)

	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	modelMap := testMultiDialectModelMap()

	nativeChat := NativeRoute{
		RouteKey: transcode.RouteKey{Method: http.MethodPost, Path: "/v1/chat/completions"},
		Protocol: transcode.NativeChat,
		Provider: "opencode-go",
		ModelMap: modelMap,
	}
	nativeMessages := NativeRoute{
		RouteKey: transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		Protocol: transcode.NativeMessages,
		Provider: "opencode-go",
		ModelMap: modelMap,
	}
	nativeResponses := NativeRoute{
		RouteKey: transcode.RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
		Protocol: transcode.NativeResponses,
		Provider: "opencode-go",
		ModelMap: modelMap,
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

	transcodeMessagesChat := TranscodeMapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         modelMap,
		LossPolicy:       lossPolicy,
	}

	transcodeMessagesResponses := TranscodeMapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap:         modelMap,
		LossPolicy:       lossPolicy,
	}

	transcodeResponsesChat := TranscodeMapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
		ClientProtocol:   transcode.ClientResponses,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         modelMap,
		LossPolicy:       lossPolicy,
	}

	authPolicy := &auth.AuthPolicy{
		Mode:             auth.AuthBearer,
		Secret:           auth.NewStaticSecretSource("opencode-token"),
		AnthropicVersion: "2023-06-01",
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithAuthPolicy(authPolicy),
		WithNativeRoutes(nativeChat, nativeMessages, nativeResponses),
		WithTranscodeMapping(transcodeMessagesChat, transcodeMessagesResponses, transcodeResponsesChat),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}

	// 1. POST /v1/messages for claude-opus (via=messages)
	t.Run("messages_via_messages_native", func(t *testing.T) {
		reqBody := `{"model":"claude-opus","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		last := upstream.lastRequest()
		if last.Path != "/v1/messages" {
			t.Errorf("upstream path = %q, want /v1/messages", last.Path)
		}
		if last.Model != "wire-opus" {
			t.Errorf("upstream model = %q, want wire-opus", last.Model)
		}
		if last.XAPIKeyHeader != "opencode-token" {
			t.Errorf("X-Api-Key = %q, want opencode-token", last.XAPIKeyHeader)
		}
		if last.AnthropicVersion != "2023-06-01" {
			t.Errorf("Anthropic-Version = %q, want 2023-06-01", last.AnthropicVersion)
		}
		if last.AuthHeader != "" {
			t.Errorf("Authorization = %q, want empty (rewritten to X-Api-Key)", last.AuthHeader)
		}
		// Client response model restored to claude-opus
		var resMsg struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resMsg)
		if resMsg.Model != "claude-opus" {
			t.Errorf("client response model = %q, want claude-opus", resMsg.Model)
		}
	})

	// 2. POST /v1/messages for deepseek-v3 (via=chat) -> transcode to /v1/chat/completions
	t.Run("messages_via_chat_transcode", func(t *testing.T) {
		reqBody := `{"model":"deepseek-v3","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		last := upstream.lastRequest()
		if last.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", last.Path)
		}
		if last.Model != "wire-deepseek" {
			t.Errorf("upstream model = %q, want wire-deepseek", last.Model)
		}
		if last.AuthHeader != "Bearer opencode-token" {
			t.Errorf("Authorization = %q, want Bearer opencode-token", last.AuthHeader)
		}
		if last.XAPIKeyHeader != "" {
			t.Errorf("X-Api-Key = %q, want empty", last.XAPIKeyHeader)
		}
		// Client response translated back to Messages format
		var resMsg struct {
			Type    string `json:"type"`
			Model   string `json:"model"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resMsg)
		if resMsg.Type != "message" {
			t.Errorf("type = %q, want message", resMsg.Type)
		}
		if resMsg.Model != "deepseek-v3" {
			t.Errorf("model = %q, want deepseek-v3", resMsg.Model)
		}
		if len(resMsg.Content) == 0 || resMsg.Content[0].Text != "Hello from chat completions upstream" {
			t.Errorf("content = %+v, want Hello from chat completions upstream", resMsg.Content)
		}
	})

	// 3. POST /v1/messages for gpt-nano (via=responses) -> transcode to /v1/responses
	t.Run("messages_via_responses_transcode", func(t *testing.T) {
		reqBody := `{"model":"gpt-nano","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		last := upstream.lastRequest()
		if last.Path != "/v1/responses" {
			t.Errorf("upstream path = %q, want /v1/responses", last.Path)
		}
		if last.Model != "wire-nano" {
			t.Errorf("upstream model = %q, want wire-nano", last.Model)
		}
		if last.AuthHeader != "Bearer opencode-token" {
			t.Errorf("Authorization = %q, want Bearer opencode-token", last.AuthHeader)
		}
		if last.XAPIKeyHeader != "" {
			t.Errorf("X-Api-Key = %q, want empty", last.XAPIKeyHeader)
		}
		// Client response translated back to Messages format
		var resMsg struct {
			Type    string `json:"type"`
			Model   string `json:"model"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resMsg)
		if resMsg.Type != "message" {
			t.Errorf("type = %q, want message", resMsg.Type)
		}
		if resMsg.Model != "gpt-nano" {
			t.Errorf("model = %q, want gpt-nano", resMsg.Model)
		}
		if len(resMsg.Content) == 0 || resMsg.Content[0].Text != "Hello from responses upstream" {
			t.Errorf("content = %+v, want Hello from responses upstream", resMsg.Content)
		}
	})

	// 4. POST /v1/responses for gpt-nano (via=responses) -> native route
	t.Run("responses_via_responses_native", func(t *testing.T) {
		reqBody := `{"model":"gpt-nano","input":"hi"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		last := upstream.lastRequest()
		if last.Path != "/v1/responses" {
			t.Errorf("upstream path = %q, want /v1/responses", last.Path)
		}
		if last.Model != "wire-nano" {
			t.Errorf("upstream model = %q, want wire-nano", last.Model)
		}
		if last.AuthHeader != "Bearer opencode-token" {
			t.Errorf("Authorization = %q, want Bearer opencode-token", last.AuthHeader)
		}
	})

	// 5. POST /v1/responses for deepseek-v3 (via=chat) -> transcode to /v1/chat/completions
	t.Run("responses_via_chat_transcode", func(t *testing.T) {
		reqBody := `{"model":"deepseek-v3","input":"hi"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		last := upstream.lastRequest()
		if last.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", last.Path)
		}
		if last.Model != "wire-deepseek" {
			t.Errorf("upstream model = %q, want wire-deepseek", last.Model)
		}
		if last.AuthHeader != "Bearer opencode-token" {
			t.Errorf("Authorization = %q, want Bearer opencode-token", last.AuthHeader)
		}
		var resObj struct {
			Object string `json:"object"`
			Model  string `json:"model"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resObj)
		if resObj.Object != "response" {
			t.Errorf("object = %q, want response", resObj.Object)
		}
		if resObj.Model != "deepseek-v3" {
			t.Errorf("model = %q, want deepseek-v3", resObj.Model)
		}
	})

	// 6. POST /v1/chat/completions for deepseek-v3 (via=chat) -> native route
	t.Run("chat_via_chat_native", func(t *testing.T) {
		reqBody := `{"model":"deepseek-v3","messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		last := upstream.lastRequest()
		if last.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", last.Path)
		}
		if last.Model != "wire-deepseek" {
			t.Errorf("upstream model = %q, want wire-deepseek", last.Model)
		}
		if last.AuthHeader != "Bearer opencode-token" {
			t.Errorf("Authorization = %q, want Bearer opencode-token", last.AuthHeader)
		}
	})
}

// TestMultiDialect_AuthXAPIKey_AdaptsToBearer verifies that if the mount's default auth
// mode is x-api-key, native requests on chat or responses adapt to Authorization: Bearer.
func TestMultiDialect_AuthXAPIKey_AdaptsToBearer(t *testing.T) {
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)

	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	modelMap := testMultiDialectModelMap()

	nativeChat := NativeRoute{
		RouteKey: transcode.RouteKey{Method: http.MethodPost, Path: "/v1/chat/completions"},
		Protocol: transcode.NativeChat,
		Provider: "test-anthropic-mount",
		ModelMap: modelMap,
	}

	authPolicy := &auth.AuthPolicy{
		Mode:             auth.AuthXAPIKey,
		Secret:           auth.NewStaticSecretSource("sk-ant-secret"),
		AnthropicVersion: "2023-06-01",
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithAuthPolicy(authPolicy),
		WithNativeRoutes(nativeChat),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}

	reqBody := `{"model":"deepseek-v3","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	last := upstream.lastRequest()
	if last.AuthHeader != "Bearer sk-ant-secret" {
		t.Errorf("Authorization = %q, want Bearer sk-ant-secret", last.AuthHeader)
	}
	if last.XAPIKeyHeader != "" {
		t.Errorf("X-Api-Key = %q, want empty", last.XAPIKeyHeader)
	}
}

// TestMultiDialect_AuthXAPIKey_AdaptsToBearerOnTranscode extends the
// adaptation contract to the transcode path: on one x-api-key mount, a
// via=chat model authenticates with Authorization: Bearer whether it is
// served natively on POST /v1/chat/completions or transcoded from
// POST /v1/messages to the same upstream — the dialect decides the header,
// the mount mode provides the secret, and the two paths cannot disagree.
func TestMultiDialect_AuthXAPIKey_AdaptsToBearerOnTranscode(t *testing.T) {
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)

	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	modelMap := testMultiDialectModelMap()

	nativeChat := NativeRoute{
		RouteKey: transcode.RouteKey{Method: http.MethodPost, Path: "/v1/chat/completions"},
		Protocol: transcode.NativeChat,
		Provider: "test-anthropic-mount",
		ModelMap: modelMap,
	}

	messagesChat := TranscodeMapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         modelMap,
		LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureToolSchemaStrictness:   {},
			transcode.FeatureUsageUnknown:           {},
			transcode.FeatureUsageCacheReadUnknown:  {},
			transcode.FeatureUsageCacheWriteUnknown: {},
			transcode.FeatureUsageReasoningUnknown:  {},
		}},
	}

	authPolicy := &auth.AuthPolicy{
		Mode:             auth.AuthXAPIKey,
		Secret:           auth.NewStaticSecretSource("sk-ant-secret"),
		AnthropicVersion: "2023-06-01",
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithAuthPolicy(authPolicy),
		WithNativeRoutes(nativeChat),
		WithTranscodeMapping(messagesChat),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}

	// Native path: POST /v1/chat/completions for the via=chat model.
	nativeReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"deepseek-v3","messages":[{"role":"user","content":"hi"}]}`))
	nativeReq.Header.Set("Content-Type", "application/json")
	nativeRec := httptest.NewRecorder()
	p.ServeHTTP(nativeRec, nativeReq)
	if nativeRec.Code != http.StatusOK {
		t.Fatalf("native status = %d, want 200; body: %s", nativeRec.Code, nativeRec.Body.String())
	}
	last := upstream.lastRequest()
	if last.AuthHeader != "Bearer sk-ant-secret" {
		t.Errorf("native Authorization = %q, want Bearer sk-ant-secret", last.AuthHeader)
	}
	if last.XAPIKeyHeader != "" {
		t.Errorf("native X-Api-Key = %q, want empty", last.XAPIKeyHeader)
	}

	// Transcode path: POST /v1/messages for the same via=chat model,
	// converted to the same /v1/chat/completions upstream.
	transcodeReq := httptest.NewRequest(http.MethodPost, "/v1/messages",
		bytes.NewBufferString(`{"model":"deepseek-v3","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	transcodeReq.Header.Set("Content-Type", "application/json")
	transcodeRec := httptest.NewRecorder()
	p.ServeHTTP(transcodeRec, transcodeReq)
	if transcodeRec.Code != http.StatusOK {
		t.Fatalf("transcode status = %d, want 200; body: %s", transcodeRec.Code, transcodeRec.Body.String())
	}
	last = upstream.lastRequest()
	if last.Path != "/v1/chat/completions" {
		t.Errorf("transcode upstream path = %q, want /v1/chat/completions", last.Path)
	}
	if last.AuthHeader != "Bearer sk-ant-secret" {
		t.Errorf("transcode Authorization = %q, want Bearer sk-ant-secret", last.AuthHeader)
	}
	if last.XAPIKeyHeader != "" {
		t.Errorf("transcode X-Api-Key = %q, want empty", last.XAPIKeyHeader)
	}
}
