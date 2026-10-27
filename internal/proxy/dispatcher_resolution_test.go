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
	"fmt"
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

// identityMap is a model map with no exact entries and identity fallback: every
// model resolves, none declares a dialect. It is the shape that used to make
// the dispatcher stop at the first target.
func identityMap() transcode.ModelMap {
	return transcode.ModelMap{AllowIdentity: true}
}

// TestDispatcher_IdentityFallbackDoesNotStopSearch pins that an identity
// fallback answer from an earlier target does not decide the route: a later
// target whose map names the model's dialect must still win. Before the fix
// the first target's empty-Via answer broke the loop and the model was served
// by the chat upstream regardless of its configured dialect.
func TestDispatcher_IdentityFallbackDoesNotStopSearch(t *testing.T) {
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	lossPolicy := transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureToolSchemaStrictness:   {},
		transcode.FeatureUsageUnknown:           {},
		transcode.FeatureUsageCacheReadUnknown:  {},
		transcode.FeatureUsageCacheWriteUnknown: {},
		transcode.FeatureUsageReasoningUnknown:  {},
	}}
	// Registered chat-first, but only the second (responses) target declares
	// the model's dialect.
	chat := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         identityMap(),
		LossPolicy:       lossPolicy}
	responses := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
			"gpt-nano": {
				ClientModel: "gpt-nano", UpstreamModel: "wire-nano",
				ClientResponseModel: "gpt-nano", Via: transcode.NativeResponses,
			},
		}},
		LossPolicy: lossPolicy}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(chat, responses),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}

	rec := dispatcherPost(t, p, `{"model":"gpt-nano","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if last := upstream.lastRequest(); last.Path != "/v1/responses" || last.Model != "wire-nano" {
		t.Fatalf("upstream = %s %s, want /v1/responses wire-nano (the later target names the dialect)", last.Path, last.Model)
	}
}

// TestDispatcher_ProfileClaimedByTargetWins pins that a profile claimed by a
// target whose target model that target cannot map still decides the route:
// the owning handler renders the profile error, rather than the request
// falling through to a different target that would serve a different
// dialect under a name the operator mapped to this one.
func TestDispatcher_ProfileClaimedByTargetWins(t *testing.T) {
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	lossPolicy := transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureToolSchemaStrictness:   {},
		transcode.FeatureUsageUnknown:           {},
		transcode.FeatureUsageCacheReadUnknown:  {},
		transcode.FeatureUsageCacheWriteUnknown: {},
		transcode.FeatureUsageReasoningUnknown:  {},
	}}
	// The chat target claims the profile but cannot map its target model; the
	// responses target has a raw model match under the same name. The profile
	// claim is authoritative: the owning handler renders its profile error
	// rather than falling through to the raw model match.
	chat := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
			"chat-only": {
				ClientModel: "chat-only", UpstreamModel: "wire-chat",
				ClientResponseModel: "chat-only", Via: transcode.NativeChat,
			},
		}},
		ProfileMap: transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
			"worker": {Model: "unmapped-target"},
		}},
		LossPolicy: lossPolicy}
	responses := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
			"worker": {
				ClientModel: "worker", UpstreamModel: "wire-target",
				ClientResponseModel: "worker", Via: transcode.NativeResponses,
			},
		}},
		LossPolicy: lossPolicy}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(chat, responses),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}

	rec := dispatcherPost(t, p, `{"model":"worker","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (the claiming target renders its profile error); body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "worker") || !strings.Contains(rec.Body.String(), "unmapped-target") {
		t.Fatalf("error body = %q, want it to name the profile and its unmapped target model", rec.Body.String())
	}
	if len(upstream.requests) != 0 {
		t.Fatalf("upstream received %d requests, want 0", len(upstream.requests))
	}
}

// TestDispatcher_NativeRouteProfileFallsThroughToMapping pins the native-plus-
// transcode composition: with -native-route on the same path and a profile the
// transcode mapping knows, the native route's model-resolution failure must
// fall through to the mapping rather than 400. Before the fix the native path
// refused the profile name before the transcode fallback could serve it.
func TestDispatcher_NativeRouteProfileFallsThroughToMapping(t *testing.T) {
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	lossPolicy := transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureToolSchemaStrictness:   {},
		transcode.FeatureUsageUnknown:           {},
		transcode.FeatureUsageCacheReadUnknown:  {},
		transcode.FeatureUsageCacheWriteUnknown: {},
		transcode.FeatureUsageReasoningUnknown:  {},
	}}
	// The native route knows only its own exact models; the profile's target
	// model exists only in the transcode mapping's map.
	native := NativeRoute{
		RouteKey: routeKey,
		Protocol: transcode.NativeMessages,
		Provider: "native-profile",
		ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
			"local-model": {
				ClientModel: "local-model", UpstreamModel: "wire-local",
				ClientResponseModel: "local-model", Via: transcode.NativeMessages,
			},
		}},
	}
	chat := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
			"profile-target": {
				ClientModel: "profile-target", UpstreamModel: "wire-profile",
				ClientResponseModel: "profile-target", Via: transcode.NativeChat,
			},
		}},
		ProfileMap: transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
			"scanner": {Model: "profile-target"},
		}},
		LossPolicy: lossPolicy}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithNativeRoutes(native),
		WithTranscodeMapping(chat),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}

	rec := dispatcherPost(t, p, `{"model":"scanner","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the mapping serves the profile); body: %s", rec.Code, rec.Body.String())
	}
	if last := upstream.lastRequest(); last.Path != "/v1/chat/completions" || last.Model != "wire-profile" {
		t.Fatalf("upstream = %s %s, want /v1/chat/completions wire-profile", last.Path, last.Model)
	}
}

// TestDispatcher_ProfileFollowsExplicitTargetModelDialectAcrossClaimants pins
// that when multiple targets claim a profile, an explicit dialect declared for
// the target model decides the route regardless of mapping registration order.
// An unmapped claimant on another dialect does not veto or preempt the target
// that can explicitly serve the model.
func TestDispatcher_ProfileFollowsExplicitTargetModelDialectAcrossClaimants(t *testing.T) {
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	lossPolicy := transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureToolSchemaStrictness:   {},
		transcode.FeatureUsageUnknown:           {},
		transcode.FeatureUsageCacheReadUnknown:  {},
		transcode.FeatureUsageCacheWriteUnknown: {},
		transcode.FeatureUsageReasoningUnknown:  {},
	}}
	// Both targets claim the profile; chat cannot map its target model,
	// while responses maps it explicitly with Via=responses.
	chat := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
			"chat-only": {
				ClientModel: "chat-only", UpstreamModel: "wire-chat",
				ClientResponseModel: "chat-only", Via: transcode.NativeChat,
			},
		}},
		ProfileMap: transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
			"worker": {Model: "resp-only"},
		}},
		LossPolicy: lossPolicy}
	responses := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
			"resp-only": {
				ClientModel: "resp-only", UpstreamModel: "wire-resp",
				ClientResponseModel: "resp-only", Via: transcode.NativeResponses,
			},
		}},
		ProfileMap: transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
			"worker": {Model: "resp-only"},
		}},
		LossPolicy: lossPolicy}

	for _, chatFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("chatFirst=%v", chatFirst), func(t *testing.T) {
			upstream := &multiDialectUpstream{}
			srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
			defer srv.Close()
			upstreamURL, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatalf("parse upstream URL: %v", err)
			}
			mappings := []TranscodeMapping{chat, responses}
			if !chatFirst {
				mappings = []TranscodeMapping{responses, chat}
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

			rec := dispatcherPost(t, p, `{"model":"worker","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (responses serves the mapped profile); body: %s", rec.Code, rec.Body.String())
			}
			if last := upstream.lastRequest(); last.Path != "/v1/responses" || last.Model != "wire-resp" {
				t.Fatalf("upstream = %s %s, want /v1/responses wire-resp", last.Path, last.Model)
			}
		})
	}
}

// TestDispatcher_DialectlessClaimReachesResponsesHandler pins that a
// dialectless claim reaches the handler that made it, not the chat handler the
// fallback would pick. The responses mapping claims the profile but cannot map
// its target model, so its own profile refusal is the answer; before the fix
// the empty dialect fell through to chat, which rendered the wrong mapping's
// error (and would have served the model on the wrong upstream had its map
// covered the name).
func TestDispatcher_DialectlessClaimReachesResponsesHandler(t *testing.T) {
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	lossPolicy := transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureToolSchemaStrictness:   {},
		transcode.FeatureUsageUnknown:           {},
		transcode.FeatureUsageCacheReadUnknown:  {},
		transcode.FeatureUsageCacheWriteUnknown: {},
		transcode.FeatureUsageReasoningUnknown:  {},
	}}
	// Chat is registered first but does NOT claim the profile; responses
	// claims it with an unmappable target.
	chat := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
			"chat-only": {
				ClientModel: "chat-only", UpstreamModel: "wire-chat",
				ClientResponseModel: "chat-only", Via: transcode.NativeChat,
			},
		}},
		LossPolicy: lossPolicy}
	responses := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamResponses,
		UpstreamPath:     "/v1/responses",
		ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
			"resp-only": {
				ClientModel: "resp-only", UpstreamModel: "wire-resp",
				ClientResponseModel: "resp-only", Via: transcode.NativeResponses,
			},
		}},
		ProfileMap: transcode.ProfileMap{Profiles: map[string]transcode.ProfileMapping{
			"worker": {Model: "not-mapped-here"},
		}},
		LossPolicy: lossPolicy}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(chat, responses),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}

	rec := dispatcherPost(t, p, `{"model":"worker","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (the claiming responses handler renders its profile error); body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "worker") || !strings.Contains(rec.Body.String(), "not-mapped-here") {
		t.Fatalf("error body = %q, want the claiming mapping's profile error", rec.Body.String())
	}
	if len(upstream.requests) != 0 {
		t.Fatalf("upstream received %d requests, want 0", len(upstream.requests))
	}
}

// TestNew_ProfileMapFrozenAfterNew pins that mutating a TranscodeMapping's
// ProfileMap after New cannot change a running handler's routing: the proxy
// deep-copies it like the model map and loss policy.
func TestNew_ProfileMapFrozenAfterNew(t *testing.T) {
	upstream := &multiDialectUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	lossPolicy := transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureToolSchemaStrictness:   {},
		transcode.FeatureUsageUnknown:           {},
		transcode.FeatureUsageCacheReadUnknown:  {},
		transcode.FeatureUsageCacheWriteUnknown: {},
		transcode.FeatureUsageReasoningUnknown:  {},
	}}
	profiles := map[string]transcode.ProfileMapping{"worker": {Model: "worker-model"}}
	mapping := TranscodeMapping{
		ClientRoute:      routeKey,
		ClientProtocol:   transcode.ClientMessages,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
			"worker-model": {
				ClientModel: "worker-model", UpstreamModel: "wire-worker",
				ClientResponseModel: "worker-model", Via: transcode.NativeChat,
			},
		}},
		ProfileMap: transcode.ProfileMap{Profiles: profiles},
		LossPolicy: lossPolicy}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(mapping),
	)
	if err != nil {
		t.Fatalf("New proxy: %v", err)
	}

	// Repoint the caller's profile after construction.
	profiles["worker"] = transcode.ProfileMapping{Model: "hijacked"}

	rec := dispatcherPost(t, p, `{"model":"worker","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if last := upstream.lastRequest(); last.Model != "wire-worker" {
		t.Fatalf("upstream model = %q, want wire-worker (post-New profile mutation must not change routing)", last.Model)
	}
}
