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
	responses := TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap:         identityMap(),
		ProfileMap:       profiles,
		LossPolicy:       claimsLossPolicy(),
	}}
	chat := TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         identityMap(),
		ProfileMap:       profiles,
		LossPolicy:       claimsLossPolicy(),
	}}
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
			mapping: &TranscodeMapping{Mapping: transcode.Mapping{
				ClientRoute:      routeKey,
				ClientProtocol:   transcode.ClientMessages,
				UpstreamProtocol: up,
				ModelMap:         modelMap,
				ProfileMap:       profiles,
			}},
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
