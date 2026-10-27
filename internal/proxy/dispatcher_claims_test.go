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

func claimsLossPolicy() transcode.LossPolicy {
	return transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureToolSchemaStrictness:   {},
		transcode.FeatureUsageUnknown:           {},
		transcode.FeatureUsageCacheReadUnknown:  {},
		transcode.FeatureUsageCacheWriteUnknown: {},
		transcode.FeatureUsageReasoningUnknown:  {},
	}}
}

func claimsProxy(t *testing.T, mappings ...TranscodeMapping) (*Proxy, *multiDialectUpstream) {
	t.Helper()
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
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
	return p, upstream
}

// TestDispatcher_ViaLessProfileFollowsItsTargetModel pins that a profile whose
// target model resolves without a declared dialect is dispatched exactly like
// that model requested by name: the chat-first fallback. The responses mapping
// is registered first (an explicit route ahead of a preset is that order), and
// before the fix its successful profile claim ended the search, so the profile
// was served by the responses upstream while the model itself went to chat.
func TestDispatcher_ViaLessProfileFollowsItsTargetModel(t *testing.T) {
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	profiles := transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
		"fast": {Model: "gpt-4o"},
	}}
	responses := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap:         identityMap(),
		ProfileMap:       profiles,
		LossPolicy:       claimsLossPolicy()}
	chat := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         identityMap(),
		ProfileMap:       profiles,
		LossPolicy:       claimsLossPolicy()}
	p, upstream := claimsProxy(t, responses, chat)

	for _, model := range []string{"gpt-4o", "fast"} {
		rec := dispatcherPost(t, p, `{"model":"`+model+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("model %q: status = %d, want 200; body: %s", model, rec.Code, rec.Body.String())
		}
		if last := upstream.lastRequest(); last.Path != "/v1/chat/completions" || last.Model != "gpt-4o" {
			t.Fatalf("model %q: upstream = %s %s, want /v1/chat/completions gpt-4o (the chat-first fallback)", model, last.Path, last.Model)
		}
	}
}

// messagesOnlyDispatcher builds a two-target dispatcher with stub handlers
// whose shared map declares "native-m" as messages-native and the profile
// "prof" targeting it.
func messagesOnlyDispatcher(t *testing.T) *multiTranscodeDispatcher {
	t.Helper()
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	modelMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"native-m": {
			ClientModel: "native-m", UpstreamModel: "wire-m",
			ClientResponseModel: "native-m", Via: transcode.NativeMessages,
		},
	}}
	profiles := transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
		"prof": {Model: "native-m"},
	}}
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a messages-native model must not reach a transcode handler")
		w.WriteHeader(http.StatusTeapot)
	})
	var targets []targetTranscodeHandler
	for _, up := range []transcode.UpstreamProtocol{transcode.UpstreamChatCompletions, transcode.UpstreamResponses} {
		targets = append(targets, targetTranscodeHandler{
			mapping: &TranscodeMapping{
				ClientRoute:      routeKey,
				ClientProtocol:   transcode.ClientMessages,
				UpstreamProtocol: up,
				ModelMap:         modelMap,
				ProfileMap:       profiles},
			handler: stub,
		})
	}
	d, err := newMultiTranscodeDispatcher(routeKey, targets)
	if err != nil {
		t.Fatalf("newMultiTranscodeDispatcher: %v", err)
	}
	return d
}

// TestDispatcher_MessagesNativeRemedyMatchesThePath pins that the 404 for a
// messages-native model only advises adding a native route when the path has
// none. A request that reached the dispatcher through a native miss came from
// a path that already carries its one native route, so that advice would be a
// duplicate-route startup error. A -transcode-profile name is never resolved
// by native serving, so every remedy for one names its target model.
func TestDispatcher_MessagesNativeRemedyMatchesThePath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		model     string
		miss      *nativeMissInfo
		want      []string
		forbidden string
	}{
		{
			name:  "no native route on the path",
			model: "native-m",
			want:  []string{"native-m", "add a -native-route messages@/v1/messages"},
		},
		{
			name:      "native route of another dialect on the path",
			model:     "native-m",
			miss:      &nativeMissInfo{clientModel: "native-m", via: transcode.NativeMessages, nativeProtocol: transcode.NativeResponses},
			want:      []string{"native-m", "already carries a responses native route", "path whose -native-route serves messages"},
			forbidden: "add a -native-route",
		},
		{
			name:      "profile with no native route on the path",
			model:     "prof",
			want:      []string{`"prof" is a -transcode-profile`, `request "native-m" by name`},
			forbidden: `model "prof" is served natively`,
		},
		{
			name:      "profile through a native route of another dialect",
			model:     "prof",
			miss:      &nativeMissInfo{clientModel: "prof", nativeProtocol: transcode.NativeChat},
			want:      []string{`"prof" is a -transcode-profile`, `request "native-m" by name on a path whose -native-route serves messages`},
			forbidden: "add a -native-route",
		},
		{
			name:      "messages native route missed a profile",
			model:     "prof",
			miss:      &nativeMissInfo{clientModel: "prof", nativeProtocol: transcode.NativeMessages},
			want:      []string{`"prof" is a -transcode-profile`, "native serving resolves model names only", `request "native-m" by name`},
			forbidden: "add a -native-route",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := messagesOnlyDispatcher(t)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages",
				bytes.NewBufferString(`{"model":"`+tc.model+`","max_tokens":16,"messages":[]}`))
			if tc.miss != nil {
				req = req.WithContext(withNativeMiss(req.Context(), *tc.miss))
			}
			rec := httptest.NewRecorder()
			d.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
			}
			var envelope struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
			}
			body := envelope.Error.Message
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("body = %q, want it to contain %q", body, want)
				}
			}
			if tc.forbidden != "" && strings.Contains(body, tc.forbidden) {
				t.Errorf("body = %q, must not advise %q on a path that already has a native route", body, tc.forbidden)
			}
		})
	}
}

// wrappingWriter is a diagnostic ResponseWriter wrapper exposing Unwrap, the
// shape WriteCommittedStreamError already traverses.
type wrappingWriter struct{ http.ResponseWriter }

func (w wrappingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// TestRouteErrorMarksRecorderBehindWrapper pins that a local 5xx refusal
// marks the proxy-generated fact on the recorder even when the recorder sits
// behind an Unwrap-capable wrapper, exactly as the committed-stream writer
// finds it there. Without the mark, the refusal is upstream-health input.
func TestRouteErrorMarksRecorderBehindWrapper(t *testing.T) {
	d := messagesOnlyDispatcher(t)
	for _, write := range []struct {
		name string
		fn   func(w http.ResponseWriter, r *http.Request)
	}{
		{"dispatcher", func(w http.ResponseWriter, r *http.Request) {
			d.writeRouteError(w, r, http.StatusInternalServerError, "boom")
		}},
		{"native", func(w http.ResponseWriter, r *http.Request) {
			writeNativeDialectError(w, r, transcode.NativeMessages, http.StatusInternalServerError, "boom")
		}},
	} {
		t.Run(write.name, func(t *testing.T) {
			rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			write.fn(wrappingWriter{rec}, req)
			if !rec.proxyGeneratedError {
				t.Fatal("proxyGeneratedError = false behind a wrapper, want true")
			}
		})
	}
}

// TestDispatcher_ProfileAnswerNeedsAClaimingHandler pins that a profile whose
// target model names a dialect is served only by a handler that defines the
// profile. Programmatic mappings may carry different profile maps; before the
// fix the dispatcher sent the request to the dialect's handler regardless, and
// that handler, not knowing the profile, forwarded the profile name upstream
// as a model name.
func TestDispatcher_ProfileAnswerNeedsAClaimingHandler(t *testing.T) {
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	chatModel := transcode.ModelMap{
		AllowIdentity: true,
		Exact: map[string]transcode.ModelMapping{
			"chat-m": {
				ClientModel: "chat-m", UpstreamModel: "chat-m",
				ClientResponseModel: "chat-m", Via: transcode.NativeChat,
			},
		},
	}
	profiles := transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
		"p": {Model: "chat-m"},
	}}
	responses := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap:         chatModel,
		ProfileMap:       profiles,
		LossPolicy:       claimsLossPolicy()}
	chat := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         chatModel,
		LossPolicy:       claimsLossPolicy()}
	body := `{"model":"p","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	t.Run("dialect handler lacks the profile", func(t *testing.T) {
		p, upstream := claimsProxy(t, responses, chat)
		rec := dispatcherPost(t, p, body)
		if last := upstream.lastRequest(); last.Path != "" {
			t.Fatalf("upstream received %s model %q, want no upstream request for an unservable profile", last.Path, last.Model)
		}
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
		}
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
		}
		for _, want := range []string{`"p"`, `"chat-m"`, "served as chat"} {
			if !strings.Contains(envelope.Error.Message, want) {
				t.Errorf("message = %q, want it to name %s", envelope.Error.Message, want)
			}
		}
	})
	t.Run("dialect handler defines the profile", func(t *testing.T) {
		withProfile := chat
		withProfile.ProfileMap = profiles
		p, upstream := claimsProxy(t, responses, withProfile)
		rec := dispatcherPost(t, p, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		if last := upstream.lastRequest(); last.Path != "/v1/chat/completions" || last.Model != "chat-m" {
			t.Fatalf("upstream = %s %q, want /v1/chat/completions chat-m", last.Path, last.Model)
		}
	})
}

// TestDispatcher_MessagesNativeRefusalNamesTheDecidingSource pins that the
// messages-native refusal is worded from whatever actually decided the route.
// Before the fix it reported the first target whose profile map knew the
// name, even when that target's answer decided nothing, and used profile
// wording for a model the native route itself resolved.
func TestDispatcher_MessagesNativeRefusalNamesTheDecidingSource(t *testing.T) {
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a messages-native model must not reach a transcode handler")
		w.WriteHeader(http.StatusTeapot)
	})
	chatTarget := targetTranscodeHandler{
		mapping: &TranscodeMapping{
			ClientRoute:      routeKey,
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			ModelMap:         identityMap(),
			ProfileMap: transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
				"p": {Model: "model-a"},
			}}},
		handler: stub,
	}
	responsesTarget := targetTranscodeHandler{
		mapping: &TranscodeMapping{
			ClientRoute:      routeKey,
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamResponses,
			ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
				"claude": {
					ClientModel: "claude", UpstreamModel: "claude",
					ClientResponseModel: "claude", Via: transcode.NativeMessages,
				},
			}},
			ProfileMap: transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
				"p": {Model: "claude"},
			}}},
		handler: stub,
	}
	d, err := newMultiTranscodeDispatcher(routeKey, []targetTranscodeHandler{chatTarget, responsesTarget})
	if err != nil {
		t.Fatalf("newMultiTranscodeDispatcher: %v", err)
	}
	refusal := func(t *testing.T, miss *nativeMissInfo) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			bytes.NewBufferString(`{"model":"p","max_tokens":16,"messages":[]}`))
		if miss != nil {
			req = req.WithContext(withNativeMiss(req.Context(), *miss))
		}
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	t.Run("profile decided by a later target", func(t *testing.T) {
		body := refusal(t, nil)
		if !strings.Contains(body, `target model \"claude\"`) || strings.Contains(body, "model-a") {
			t.Fatalf("body = %s, want the deciding target's model claude, never model-a", body)
		}
	})
	t.Run("model decided by the native route", func(t *testing.T) {
		body := refusal(t, &nativeMissInfo{clientModel: "p", via: transcode.NativeMessages, nativeProtocol: transcode.NativeResponses})
		if strings.Contains(body, "-transcode-profile") || !strings.Contains(body, `model \"p\" is served natively as messages`) {
			t.Fatalf("body = %s, want model wording for a name the native route resolved", body)
		}
	})
}

// TestDispatcher_ProfileTargetModelExplicitViaOnOtherTarget pins that a profile
// follows its target model's dialect even when that dialect is declared by
// another mapping's model map. When the dialect's mapping does not define the
// profile, the request must be refused, not erroneously served by a claiming
// target whose own model map had no explicit dialect.
func TestDispatcher_ProfileTargetModelExplicitViaOnOtherTarget(t *testing.T) {
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	profiles := transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
		"fast": {Model: "o3-mini"},
	}}
	responsesMap := transcode.ModelMap{
		AllowIdentity: true,
		Exact: map[string]transcode.ModelMapping{
			"o3-mini": {
				ClientModel: "o3-mini", UpstreamModel: "o3-mini",
				ClientResponseModel: "o3-mini", Via: transcode.NativeResponses,
			},
		},
	}
	chatMap := transcode.ModelMap{
		AllowIdentity: true,
		Exact: map[string]transcode.ModelMapping{
			"o3-mini": {
				ClientModel: "o3-mini", UpstreamModel: "o3-mini",
				ClientResponseModel: "o3-mini", Via: transcode.NativeChat,
			},
		},
	}
	messagesMap := transcode.ModelMap{
		AllowIdentity: true,
		Exact: map[string]transcode.ModelMapping{
			"o3-mini": {
				ClientModel: "o3-mini", UpstreamModel: "o3-mini",
				ClientResponseModel: "o3-mini", Via: transcode.NativeMessages,
			},
		},
	}
	body := `{"model":"fast","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	t.Run("target model via responses triggers profile refusal when responses target lacks profile", func(t *testing.T) {
		chat := TranscodeMapping{
			ClientRoute:      routeKey,
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			ModelMap:         identityMap(),
			ProfileMap:       profiles,
			LossPolicy:       claimsLossPolicy()}
		responses := TranscodeMapping{
			ClientRoute:      routeKey,
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamResponses,
			UpstreamPath:     "/v1/responses",
			ModelMap:         responsesMap,
			LossPolicy:       claimsLossPolicy()}
		p, upstream := claimsProxy(t, chat, responses)
		rec := dispatcherPost(t, p, body)
		if last := upstream.lastRequest(); last.Path != "" {
			t.Fatalf("upstream received %s model %q, want no upstream request", last.Path, last.Model)
		}
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
		}
		var env struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		for _, want := range []string{`"fast"`, `"o3-mini"`, "served as responses", "responses mapping on /v1/messages does not define the profile"} {
			if !strings.Contains(env.Error.Message, want) {
				t.Errorf("error message = %q, want it to contain %q", env.Error.Message, want)
			}
		}
	})

	t.Run("symmetric: profile on responses target, target model via chat on chat target", func(t *testing.T) {
		chat := TranscodeMapping{
			ClientRoute:      routeKey,
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			ModelMap:         chatMap,
			LossPolicy:       claimsLossPolicy()}
		responses := TranscodeMapping{
			ClientRoute:      routeKey,
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamResponses,
			UpstreamPath:     "/v1/responses",
			ModelMap:         identityMap(),
			ProfileMap:       profiles,
			LossPolicy:       claimsLossPolicy()}
		p, upstream := claimsProxy(t, chat, responses)
		rec := dispatcherPost(t, p, body)
		if last := upstream.lastRequest(); last.Path != "" {
			t.Fatalf("upstream received %s model %q, want no upstream request", last.Path, last.Model)
		}
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
		}
		var env struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		for _, want := range []string{`"fast"`, `"o3-mini"`, "served as chat", "chat mapping on /v1/messages does not define the profile"} {
			if !strings.Contains(env.Error.Message, want) {
				t.Errorf("error message = %q, want it to contain %q", env.Error.Message, want)
			}
		}
	})

	t.Run("target model via messages triggers messagesNativeRefusal", func(t *testing.T) {
		chat := TranscodeMapping{
			ClientRoute:      routeKey,
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			UpstreamPath:     "/v1/chat/completions",
			ModelMap:         identityMap(),
			ProfileMap:       profiles,
			LossPolicy:       claimsLossPolicy()}
		responses := TranscodeMapping{
			ClientRoute:      routeKey,
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamResponses,
			UpstreamPath:     "/v1/responses",
			ModelMap:         messagesMap,
			LossPolicy:       claimsLossPolicy()}
		p, upstream := claimsProxy(t, chat, responses)
		rec := dispatcherPost(t, p, body)
		if last := upstream.lastRequest(); last.Path != "" {
			t.Fatalf("upstream received %s model %q, want no upstream request", last.Path, last.Model)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
		}
		var env struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		for _, want := range []string{`"fast" is a -transcode-profile`, `target model "o3-mini" is served natively as messages`} {
			if !strings.Contains(env.Error.Message, want) {
				t.Errorf("error message = %q, want it to contain %q", env.Error.Message, want)
			}
		}
	})
}

// TestDispatcher_ProfileCannotBeStolenByRawModelMatch pins that a raw model
// match on a non-claiming target cannot steal a request whose model name is
// claimed as a profile by another target. The non-claiming handler would forward
// the profile name upstream as a model name.
func TestDispatcher_ProfileCannotBeStolenByRawModelMatch(t *testing.T) {
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	responses := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap:         identityMap(),
		ProfileMap: transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
			"fast": {Model: "o3-mini"},
		}},
		LossPolicy: claimsLossPolicy()}
	chat := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap: transcode.ModelMap{
			AllowIdentity: true,
			Exact: map[string]transcode.ModelMapping{
				"fast": {
					ClientModel: "fast", UpstreamModel: "wire-fast",
					ClientResponseModel: "fast", Via: transcode.NativeChat,
				},
			},
		},
		LossPolicy: claimsLossPolicy()}
	p, upstream := claimsProxy(t, responses, chat)
	body := `{"model":"fast","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	rec := dispatcherPost(t, p, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	last := upstream.lastRequest()
	if last.Path != "/v1/responses" || last.Model != "o3-mini" {
		t.Fatalf("upstream = %s %q, want /v1/responses o3-mini (claimed profile must not be stolen by raw model map)", last.Path, last.Model)
	}
}

// TestDispatcher_ProfileDialectRefusal_TargetModelMismatch pins that a dialect
// handler cannot serve a profile if its own profile definition maps that name
// to a different target model.
func TestDispatcher_ProfileDialectRefusal_TargetModelMismatch(t *testing.T) {
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	chatModel := transcode.ModelMap{
		AllowIdentity: true,
		Exact: map[string]transcode.ModelMapping{
			"chat-m": {
				ClientModel: "chat-m", UpstreamModel: "chat-m",
				ClientResponseModel: "chat-m", Via: transcode.NativeChat,
			},
		},
	}
	responses := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap:         chatModel,
		ProfileMap: transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
			"p": {Model: "chat-m"},
		}},
		LossPolicy: claimsLossPolicy()}
	chat := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         chatModel,
		ProfileMap: transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
			"p": {Model: "other-m"},
		}},
		LossPolicy: claimsLossPolicy()}
	p, upstream := claimsProxy(t, responses, chat)
	body := `{"model":"p","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	rec := dispatcherPost(t, p, body)
	if last := upstream.lastRequest(); last.Path != "" {
		t.Fatalf("upstream received %s model %q, want refusal", last.Path, last.Model)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	for _, want := range []string{`"p"`, `"chat-m"`, "served as chat"} {
		if !strings.Contains(env.Error.Message, want) {
			t.Errorf("message = %q, want to contain %q", env.Error.Message, want)
		}
	}
}

// TestDispatcher_ProfileDialectRefusal_NoMappingForDialect pins that when a
// profile's target model declares a dialect that has no handler on the route
// (e.g. via=responses on a route with only chat mappings), the dispatcher
// renders a 500 refusal rather than falling through to the wrong dialect's
// handler.
func TestDispatcher_ProfileDialectRefusal_NoMappingForDialect(t *testing.T) {
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	respModel := transcode.ModelMap{
		AllowIdentity: true,
		Exact: map[string]transcode.ModelMapping{
			"resp-m": {
				ClientModel: "resp-m", UpstreamModel: "resp-m",
				ClientResponseModel: "resp-m", Via: transcode.NativeResponses,
			},
		},
	}
	profiles := transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
		"p": {Model: "resp-m"},
	}}
	// A multi-mapping route where all targets are chat completions (no responses target).
	chat1 := targetTranscodeHandler{
		mapping: &TranscodeMapping{
			ClientRoute:      routeKey,
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			ModelMap:         respModel,
			ProfileMap:       profiles},
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("chat handler must not receive request for a profile whose target model is responses")
			w.WriteHeader(http.StatusOK)
		}),
	}
	chat2 := targetTranscodeHandler{
		mapping: &TranscodeMapping{
			ClientRoute:      routeKey,
			ClientProtocol:   transcode.ClientMessages,
			UpstreamProtocol: transcode.UpstreamChatCompletions,
			ModelMap:         respModel,
			ProfileMap:       profiles},
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("chat handler must not receive request for a profile whose target model is responses")
			w.WriteHeader(http.StatusOK)
		}),
	}
	d, err := newMultiTranscodeDispatcher(routeKey, []targetTranscodeHandler{chat1, chat2})
	if err != nil {
		t.Fatalf("newMultiTranscodeDispatcher: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		bytes.NewBufferString(`{"model":"p","max_tokens":16,"messages":[]}`))
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	for _, want := range []string{`"p"`, `"resp-m"`, "served as responses", "has no responses mapping"} {
		if !strings.Contains(env.Error.Message, want) {
			t.Errorf("message = %q, want to contain %q", env.Error.Message, want)
		}
	}
}
