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

package main

import (
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// TestParseTranscodeRoute verifies the -transcode-route value parsing with
// the clientProtocol@clientPath=upstreamProtocol@upstreamPath format.
func TestParseTranscodeRoute(t *testing.T) {
	m, err := parseTranscodeRoute("responses@/v1/responses=chat-completions@/v1/chat/completions")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.ClientRoute.Path != "/v1/responses" || m.UpstreamPath != "/v1/chat/completions" ||
		m.ClientProtocol != transcode.ClientResponses || m.UpstreamProtocol != transcode.UpstreamChatCompletions {
		t.Errorf("mapping = %+v", m)
	}
	if m.ClientRoute.Method != "POST" {
		t.Errorf("client route method = %q, want POST", m.ClientRoute.Method)
	}

	for _, bad := range []string{
		"",
		"responses@/v1/responses",
		"responses@/v1/responses=chat-completions",
		"@/v1/responses=chat-completions@/v1/chat/completions",
		"responses@=chat-completions@/v1/chat/completions",
		"responses@v1/responses=chat-completions@/v1/chat/completions",
		"responses@/v1/responses=chat-completions@v1/chat/completions",
	} {
		if _, err := parseTranscodeRoute(bad); err == nil {
			t.Errorf("parseTranscodeRoute(%q): want error", bad)
		}
	}
}

// TestParseTranscodeRouteChatClientRejected verifies chat-completions is
// rejected as a client protocol at parse time (chat is upstream-only).
func TestParseTranscodeRouteChatClientRejected(t *testing.T) {
	_, err := parseTranscodeRoute("chat-completions@/v1/chat/completions=responses@/v1/responses")
	if err == nil {
		t.Fatal("expected chat-completions client rejection")
	}
	if !strings.Contains(err.Error(), "upstream-only") {
		t.Fatalf("error = %v", err)
	}
}

// TestParseTranscodeRouteAtInPath verifies paths containing '@' parse
// correctly (the first '@' separates the protocol).
func TestParseTranscodeRouteAtInPath(t *testing.T) {
	m, err := parseTranscodeRoute("responses@/v1/models=chat-completions@/v1/models@predict")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.UpstreamPath != "/v1/models@predict" {
		t.Errorf("upstream path = %q, want /v1/models@predict", m.UpstreamPath)
	}
}

// TestTranscodeRouteFlagsSet verifies the flag.Value behavior of the
// repeatable -transcode-route flag.
func TestTranscodeRouteFlagsSet(t *testing.T) {
	var routes transcodeRouteFlags
	if err := routes.Set("messages@/v1/messages=responses@/v1/responses"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := routes.Set("messages@/v1/messages=chat-completions@/v1/chat/completions"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %d, want 2", len(routes))
	}
	if routes[0].ClientRoute.Path != "/v1/messages" || routes[0].UpstreamProtocol != transcode.UpstreamResponses {
		t.Errorf("routes[0] = %+v", routes[0])
	}
	if routes[1].UpstreamProtocol != transcode.UpstreamChatCompletions {
		t.Errorf("routes[1] = %+v", routes[1])
	}
	if err := routes.Set("garbage"); err == nil {
		t.Error("Set(garbage): want error")
	}
	if err := routes.Set("chat-completions@/x=responses@/y"); err == nil {
		t.Error("Set(chat client): want error")
	}
	if routes.String() == "" {
		t.Error("String() = empty")
	}
}

// TestBuildTranscodeMappings verifies the preset flags expand to the correct
// route mappings, appended after any explicit -transcode-route values.
func TestBuildTranscodeMappings(t *testing.T) {
	explicit := proxy.TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute:      mustRouteKey("POST", "/v1/custom"),
		ClientProtocol:   transcode.ClientResponses,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/upstream",
	}}

	none, err := buildTranscodeMappings(nil, false, false, false)
	if err != nil {
		t.Fatalf("no flags: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("no flags: mappings = %+v, want none", none)
	}

	all, err := buildTranscodeMappings([]proxy.TranscodeMapping{explicit}, true, false, true)
	if err != nil {
		t.Fatalf("all flags: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all flags: mappings = %d, want 3", len(all))
	}
	if all[0].ClientRoute != explicit.ClientRoute {
		t.Errorf("mappings[0] = %+v, want explicit route", all[0])
	}
	wantPresets := []struct {
		path     string
		client   transcode.ClientProtocol
		upstream transcode.UpstreamProtocol
	}{
		{"/v1/responses", transcode.ClientResponses, transcode.UpstreamChatCompletions},
		{"/v1/messages", transcode.ClientMessages, transcode.UpstreamResponses},
	}
	for i, want := range wantPresets {
		got := all[i+1]
		if got.ClientRoute.Path != want.path || got.ClientProtocol != want.client || got.UpstreamProtocol != want.upstream {
			t.Errorf("preset %d = %+v, want %s %s->%s", i, got, want.path, want.client, want.upstream)
		}
		if got.ClientRoute.Method != "POST" {
			t.Errorf("preset %d method = %q, want POST", i, got.ClientRoute.Method)
		}
	}

	// The messages->chat preset maps the same client route as messages->
	// responses, so they are mutually exclusive; messages-chat alone works.
	withChat, err := buildTranscodeMappings(nil, false, true, false)
	if err != nil {
		t.Fatalf("messages-chat: %v", err)
	}
	if len(withChat) != 1 || withChat[0].ClientRoute.Path != "/v1/messages" ||
		withChat[0].UpstreamProtocol != transcode.UpstreamChatCompletions {
		t.Errorf("messages-chat = %+v", withChat)
	}

	single, err := buildTranscodeMappings(nil, true, false, false)
	if err != nil {
		t.Fatalf("responses-chat: %v", err)
	}
	if len(single) != 1 || single[0].ClientRoute.Path != "/v1/responses" {
		t.Errorf("responses-chat = %+v", single)
	}
}

// TestBuildTranscodeMappingsConflict verifies enabling both Messages presets
// fails before proxy.New runs.
func TestBuildTranscodeMappingsConflict(t *testing.T) {
	_, err := buildTranscodeMappings(nil, false, true, true)
	if err == nil {
		t.Fatal("expected both-messages-preset conflict")
	}
	if !strings.Contains(err.Error(), "both map /v1/messages") {
		t.Fatalf("error = %v", err)
	}
}

// TestParseTranscodeModelMap verifies the -transcode-model parsing.
func TestParseTranscodeModelMap(t *testing.T) {
	empty, err := parseTranscodeModelMap(nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.AllowIdentity != true {
		t.Fatal("empty map must allow identity")
	}

	models, err := parseTranscodeModelMap([]string{
		"claude-3=claude-3",
		"gpt-4o=gpt-4o-mini",
	})
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := models.Resolve("claude-3")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.UpstreamModel != "claude-3" || mapping.ClientResponseModel != "claude-3" {
		t.Fatalf("mapping = %+v", mapping)
	}
	mapping, err = models.Resolve("gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.UpstreamModel != "gpt-4o-mini" {
		t.Fatalf("upstream model = %q", mapping.UpstreamModel)
	}

	for _, bad := range []string{"no-equals", "=x", "x="} {
		if _, err := parseTranscodeModelMap([]string{bad}); err == nil {
			t.Errorf("parseTranscodeModelMap(%q): want error", bad)
		}
	}
}

// TestParseTranscodeAuth verifies the auth CLI contract.
func TestParseTranscodeAuth(t *testing.T) {
	policy, err := parseTranscodeAuth("auto", "inbound", "", "2023-06-01")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != transcode.AuthAuto || !policy.Inbound || policy.AnthropicVersion != "2023-06-01" {
		t.Fatalf("policy = %+v", policy)
	}

	policy, err = parseTranscodeAuth("header", "env:UPSTREAM_KEY", "X-Upstream-Key", "")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != transcode.AuthCustomHeader || policy.Inbound || policy.CustomHeader != "X-Upstream-Key" {
		t.Fatalf("policy = %+v", policy)
	}
	if policy.Secret == nil {
		t.Fatal("secret source missing")
	}

	if _, err := parseTranscodeAuth("auto", "bogus", "", ""); err == nil {
		t.Fatal("expected invalid source rejection")
	}
}
