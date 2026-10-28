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
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// dispatchBody serves one request naming model through the dispatcher built
// from targets, in the registration order given.
func dispatchBody(t *testing.T, model string, targets ...targetTranscodeHandler) *httptest.ResponseRecorder {
	t.Helper()
	routeKey := transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"}
	d, err := newMultiTranscodeDispatcher(routeKey, targets)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, routeKey.Path,
		strings.NewReader(`{"model":"`+model+`","max_tokens":16,"messages":[]}`))
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	return rec
}

// refusalText returns the error message of a dialect-shaped refusal body.
func refusalText(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode refusal %q: %v", rec.Body.String(), err)
	}
	return env.Error.Message
}

// requireSymmetricRefusal serves model through the chat and responses targets
// in both registration orders. Each order must refuse with a 500 that
// dispatches nothing, and the two refusals must be identical: no registration
// order may decide the outcome.
func requireSymmetricRefusal(
	t *testing.T,
	model string,
	chat, responses func(called *string) targetTranscodeHandler,
) string {
	t.Helper()
	var texts []string
	for _, chatFirst := range []bool{true, false} {
		var called string
		c, r := chat(&called), responses(&called)
		targets := []targetTranscodeHandler{r, c}
		if chatFirst {
			targets = []targetTranscodeHandler{c, r}
		}
		rec := dispatchBody(t, model, targets...)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("chatFirst=%v: status = %d, want the 500 refusal; body %s", chatFirst, rec.Code, rec.Body.String())
		}
		if called != "" {
			t.Fatalf("chatFirst=%v: handler %q was dispatched a conflicting request", chatFirst, called)
		}
		texts = append(texts, refusalText(t, rec))
	}
	if texts[0] == "" || texts[0] != texts[1] {
		t.Fatalf("refusal depends on registration order:\n chat first: %q\n responses first: %q", texts[0], texts[1])
	}
	return texts[0]
}

// Two claimants define the same profile name with different target models:
// the route cannot say which definition the client meant.
func TestDispatcher_DivergentProfileTargetsRefusedInEveryOrder(t *testing.T) {
	chatMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"model-chat": {ClientModel: "model-chat", UpstreamModel: "model-chat", ClientResponseModel: "model-chat", Via: transcode.NativeChat},
	}}
	respMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"model-resp": {ClientModel: "model-resp", UpstreamModel: "model-resp", ClientResponseModel: "model-resp", Via: transcode.NativeResponses},
	}}
	requireSymmetricRefusal(t, "fast",
		func(called *string) targetTranscodeHandler {
			return profileDispatchTarget(transcode.UpstreamChatCompletions, chatMap,
				map[string]transcode.ProfileMapping{"fast": {Model: "model-chat"}}, called)
		},
		func(called *string) targetTranscodeHandler {
			return profileDispatchTarget(transcode.UpstreamResponses, respMap,
				map[string]transcode.ProfileMapping{"fast": {Model: "model-resp"}}, called)
		},
	)
}

// Two claimants define the same profile name with different target models,
// and BOTH target models carry conflicting explicit dialects across the
// route's mappings: the refusal must be byte-identical in every registration
// order and name every conflicted target model, so no order can decide which
// single model the message blames.
func TestDispatcher_DualConflictedProfileTargetsRefusedIdenticallyInEveryOrder(t *testing.T) {
	chatMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"model-chat": {ClientModel: "model-chat", UpstreamModel: "model-chat", ClientResponseModel: "model-chat", Via: transcode.NativeChat},
	}}
	respMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"model-chat": {ClientModel: "model-chat", UpstreamModel: "model-chat", ClientResponseModel: "model-chat", Via: transcode.NativeResponses},
	}}
	// Both targets also map the OTHER claimant's target model, to the
	// opposite dialect, so both target models are doubly mapped.
	shared := func(base transcode.ModelMap, extraName string, extra transcode.ModelMapping) transcode.ModelMap {
		out := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{}}
		maps.Copy(out.Exact, base.Exact)
		out.Exact[extraName] = extra
		return out
	}
	chatFull := shared(chatMap, "model-resp", transcode.ModelMapping{
		ClientModel: "model-resp", UpstreamModel: "model-resp", ClientResponseModel: "model-resp", Via: transcode.NativeResponses,
	})
	respFull := shared(respMap, "model-resp", transcode.ModelMapping{
		ClientModel: "model-resp", UpstreamModel: "model-resp", ClientResponseModel: "model-resp", Via: transcode.NativeChat,
	})
	text := requireSymmetricRefusal(t, "fast",
		func(called *string) targetTranscodeHandler {
			return profileDispatchTarget(transcode.UpstreamChatCompletions, chatFull,
				map[string]transcode.ProfileMapping{"fast": {Model: "model-chat"}}, called)
		},
		func(called *string) targetTranscodeHandler {
			return profileDispatchTarget(transcode.UpstreamResponses, respFull,
				map[string]transcode.ProfileMapping{"fast": {Model: "model-resp"}}, called)
		},
	)
	if !strings.Contains(text, "model-chat") || !strings.Contains(text, "model-resp") {
		t.Fatalf("refusal %q does not name both conflicted target models", text)
	}
}

// The same target model with different reasoning tiers is a different profile.
func TestDispatcher_ProfileTierDivergenceRefusedInEveryOrder(t *testing.T) {
	respMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"shared-model": {ClientModel: "shared-model", UpstreamModel: "shared-model", ClientResponseModel: "shared-model", Via: transcode.NativeResponses},
	}}
	requireSymmetricRefusal(t, "fast",
		func(called *string) targetTranscodeHandler {
			return profileDispatchTarget(transcode.UpstreamChatCompletions, respMap,
				map[string]transcode.ProfileMapping{"fast": {Model: "shared-model", ReasoningTier: "low"}}, called)
		},
		func(called *string) targetTranscodeHandler {
			return profileDispatchTarget(transcode.UpstreamResponses, respMap,
				map[string]transcode.ProfileMapping{"fast": {Model: "shared-model", ReasoningTier: "high"}}, called)
		},
	)
}

// Agreeing profile definitions whose target model the two mappings declare
// with conflicting explicit dialects leave the dialect undecidable.
func TestDispatcher_ConflictingExplicitDialectForProfileTargetRefusedInEveryOrder(t *testing.T) {
	profiles := map[string]transcode.ProfileMapping{"fast": {Model: "shared-model"}}
	chatMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"shared-model": {ClientModel: "shared-model", UpstreamModel: "shared-model", ClientResponseModel: "shared-model", Via: transcode.NativeChat},
	}}
	respMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"shared-model": {ClientModel: "shared-model", UpstreamModel: "shared-model", ClientResponseModel: "shared-model", Via: transcode.NativeResponses},
	}}
	message := requireSymmetricRefusal(t, "fast",
		func(called *string) targetTranscodeHandler {
			return profileDispatchTarget(transcode.UpstreamChatCompletions, chatMap, profiles, called)
		},
		func(called *string) targetTranscodeHandler {
			return profileDispatchTarget(transcode.UpstreamResponses, respMap, profiles, called)
		},
	)
	if !strings.Contains(message, "chat") || !strings.Contains(message, "responses") {
		t.Fatalf("refusal %q does not name both conflicting dialects", message)
	}
}

// A raw model name the route maps to conflicting explicit dialects is refused
// in both orders, as a profile target is.
func TestDispatcher_ConflictingExplicitDialectForRawModelRefusedInEveryOrder(t *testing.T) {
	chatMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"shared-model": {ClientModel: "shared-model", UpstreamModel: "shared-model", ClientResponseModel: "shared-model", Via: transcode.NativeChat},
	}}
	respMap := transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
		"shared-model": {ClientModel: "shared-model", UpstreamModel: "shared-model", ClientResponseModel: "shared-model", Via: transcode.NativeResponses},
	}}
	requireSymmetricRefusal(t, "shared-model",
		func(called *string) targetTranscodeHandler {
			return profileDispatchTarget(transcode.UpstreamChatCompletions, chatMap, nil, called)
		},
		func(called *string) targetTranscodeHandler {
			return profileDispatchTarget(transcode.UpstreamResponses, respMap, nil, called)
		},
	)
}
