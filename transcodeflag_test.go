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
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// TestParseTranscodeRoute verifies the -transcode-route value parsing.
func TestParseTranscodeRoute(t *testing.T) {
	m, err := parseTranscodeRoute("/v1/responses=/v1/chat/completions:responses:chat-completions")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.ClientPath != "/v1/responses" || m.UpstreamPath != "/v1/chat/completions" ||
		m.ClientFormat != transcode.FormatResponses || m.UpstreamFormat != transcode.FormatChatCompletions {
		t.Errorf("mapping = %+v", m)
	}

	for _, bad := range []string{
		"",
		"/v1/responses",
		"/v1/responses=/v1/chat/completions",
		"/v1/responses=/v1/chat/completions:responses",
		"=/v1/chat/completions:responses:chat-completions",
		"/v1/responses=:responses:chat-completions",
		"/v1/responses=/v1/chat/completions:responses:chat-completions:extra",
	} {
		if _, err := parseTranscodeRoute(bad); err == nil {
			t.Errorf("parseTranscodeRoute(%q): want error", bad)
		}
	}
}

// TestTranscodeRouteFlagsSet verifies the flag.Value behavior of the
// repeatable -transcode-route flag.
func TestTranscodeRouteFlagsSet(t *testing.T) {
	var routes transcodeRouteFlags
	if err := routes.Set("/v1/messages=/v1/responses:messages:responses"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := routes.Set("/v1/messages=/v1/chat/completions:messages:chat-completions"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %d, want 2", len(routes))
	}
	if routes[0].ClientPath != "/v1/messages" || routes[0].UpstreamFormat != transcode.FormatResponses {
		t.Errorf("routes[0] = %+v", routes[0])
	}
	if routes[1].UpstreamFormat != transcode.FormatChatCompletions {
		t.Errorf("routes[1] = %+v", routes[1])
	}
	if err := routes.Set("garbage"); err == nil {
		t.Error("Set(garbage): want error")
	}
	if routes.String() == "" {
		t.Error("String() = empty")
	}
}

// TestBuildTranscodeMappings verifies the preset flags expand to the correct
// route mappings, appended after any explicit -transcode-route values.
func TestBuildTranscodeMappings(t *testing.T) {
	explicit := proxy.TranscodeMapping{
		ClientPath:     "/v1/custom",
		UpstreamPath:   "/v1/upstream",
		ClientFormat:   transcode.FormatResponses,
		UpstreamFormat: transcode.FormatChatCompletions,
	}

	none := buildTranscodeMappings(nil, false, false, false)
	if len(none) != 0 {
		t.Errorf("no flags: mappings = %+v, want none", none)
	}

	all := buildTranscodeMappings([]proxy.TranscodeMapping{explicit}, true, true, true)
	if len(all) != 4 {
		t.Fatalf("all flags: mappings = %d, want 4", len(all))
	}
	if all[0] != explicit {
		t.Errorf("mappings[0] = %+v, want explicit route", all[0])
	}
	wantPresets := []proxy.TranscodeMapping{
		{ClientPath: "/v1/responses", UpstreamPath: "/v1/chat/completions", ClientFormat: transcode.FormatResponses, UpstreamFormat: transcode.FormatChatCompletions},
		{ClientPath: "/v1/messages", UpstreamPath: "/v1/chat/completions", ClientFormat: transcode.FormatMessages, UpstreamFormat: transcode.FormatChatCompletions},
		{ClientPath: "/v1/messages", UpstreamPath: "/v1/responses", ClientFormat: transcode.FormatMessages, UpstreamFormat: transcode.FormatResponses},
	}
	for i, want := range wantPresets {
		if all[i+1] != want {
			t.Errorf("preset %d = %+v, want %+v", i, all[i+1], want)
		}
	}

	single := buildTranscodeMappings(nil, true, false, false)
	if len(single) != 1 || single[0].ClientPath != "/v1/responses" {
		t.Errorf("responses-chat = %+v", single)
	}
}

// TestParseTranscodeRouteColonPath verifies upstream paths containing colons
// parse correctly (e.g. /v1/models:predict).
func TestParseTranscodeRouteColonPath(t *testing.T) {
	m, err := parseTranscodeRoute("/v1/models=/v1/models:predict:responses:chat-completions")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.ClientPath != "/v1/models" || m.UpstreamPath != "/v1/models:predict" {
		t.Errorf("mapping = %+v, want upstream path with colon", m)
	}
	if m.ClientFormat != transcode.FormatResponses || m.UpstreamFormat != transcode.FormatChatCompletions {
		t.Errorf("formats = %q -> %q", m.ClientFormat, m.UpstreamFormat)
	}
}
