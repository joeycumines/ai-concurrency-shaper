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

package config

import (
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// TestParseModelTableEntry_ViaFact proves the via fact accepts exactly the
// closed native-dialect vocabulary and rejects everything else.
func TestParseModelTableEntry_ViaFact(t *testing.T) {
	for _, via := range []string{"responses", "messages", "chat"} {
		entry, err := parseModelTableEntry("s@p=w;via=" + via)
		if err != nil {
			t.Fatalf("parseModelTableEntry via=%s: %v", via, err)
		}
		if entry.Via != transcode.NativeProtocol(via) {
			t.Fatalf("Via = %q, want %q", entry.Via, via)
		}
	}
	for _, raw := range []string{
		"s@p=w;via=",
		"s@p=w;via=chat-completions",
		"s@p=w;via=MESSAGES",
		"s@p=w;via=messages;via=chat",
	} {
		if _, err := parseModelTableEntry(raw); err == nil {
			t.Fatalf("parseModelTableEntry(%q): want error, got nil", raw)
		}
	}
}

// TestModelTable_ViaFactSuffix renders the routing-active fact in the
// startup summary like every other fact.
func TestModelTable_ViaFactSuffix(t *testing.T) {
	cfg := resolveModelTableArgs(t,
		"-upstream", "https://api.example.com",
		"-model-table", "s@example=wire;via=messages",
		"-native-route", "messages@/v1/messages",
	)
	summary := cfg.ModelTableSummary()
	want := "s@example->wire(via=messages)"
	if !strings.Contains(summary, want) {
		t.Fatalf("summary = %q, want substring %q", summary, want)
	}
}

// TestResolveTranscode_NativeRoutes proves native declarations resolve with
// the projected model map (Via included), collide loudly with transcode
// routes, and require table coverage exactly like transcode routes.
func TestResolveTranscode_NativeRoutes(t *testing.T) {
	cfg := resolveModelTableArgs(t,
		"-upstream", "https://api.example.com",
		"-model-table", "s@example=wire;via=messages",
		"-model-table", "c@example=chat-wire;via=chat",
		"-transcode-responses-chat",
		"-native-route", "messages@/v1/messages",
		"-native-route", "chat@/v1/chat/completions",
	)
	routes := cfg.Providers[0].NativeRoutes()
	if len(routes) != 2 {
		t.Fatalf("native routes = %d, want 2", len(routes))
	}
	byPath := make(map[string]transcode.ModelMap)
	for _, nr := range routes {
		byPath[nr.RouteKey.Path] = nr.ModelMap
	}
	m, err := byPath["/v1/messages"].Resolve("s")
	if err != nil {
		t.Fatalf("Resolve(s): %v", err)
	}
	if m.UpstreamModel != "wire" || m.Via != transcode.NativeMessages {
		t.Fatalf("Resolve(s) = %+v, want wire model with messages via", m)
	}

	assertModelTableSemanticError(t, []string{
		"-upstream", "https://api.example.com",
		"-model-table", "s@example=wire;via=messages",
		"-transcode-messages-chat",
		"-native-route", "messages@/v1/messages",
	}, "native route POST /v1/messages collides with a transcode mapping for the same client route")

	assertModelTableSemanticError(t, []string{
		"-upstream", "https://api.example.com",
		"-model-table", "s@example=wire;via=messages",
		"-native-route", "messages@/v1/messages",
		"-native-route", "messages@/v1/messages",
	}, "duplicate native route for client route POST /v1/messages")

	assertModelTableSemanticError(t, []string{
		"-model-table", "a@anthropic=w",
		"--provider=anthropic",
		"-upstream", "https://api.anthropic.com",
		"-prefix", "/anthropic",
		"-transcode-messages-chat",
		"--provider=openai",
		"-upstream", "https://api.openai.com",
		"-prefix", "/openai",
		"-native-route", "messages@/v1/messages",
	}, `provider "openai" has native routes but no -model-table entry names it`)

	assertModelTableSemanticError(t, []string{
		"-upstream", "https://api.example.com",
		"-model-table", "s@example=wire",
		"-native-route", "bogus@/v1/messages",
	}, `invalid native route "bogus@/v1/messages": unknown native protocol "bogus" (want responses, messages, or chat)`)
}

// TestResolveTranscode_NativeOnlyProviderCovered proves a provider with only
// native routes (no transcode mappings) resolves its table and catalog.
func TestResolveTranscode_NativeOnlyProviderCovered(t *testing.T) {
	cfg := resolveModelTableArgs(t,
		"-upstream", "https://api.example.com",
		"-model-table", "s@example=wire;via=messages",
		"-native-route", "messages@/v1/messages",
	)
	if n := len(cfg.Providers[0].TranscodeMappings()); n != 0 {
		t.Fatalf("transcode mappings = %d, want 0", n)
	}
	catalog, ok := cfg.Providers[0].ModelCatalog()
	if !ok {
		t.Fatal("ModelCatalog: want snapshot, got none")
	}
	if !catalog.ServesMessages || catalog.ServesResponses {
		t.Fatalf("catalog serves responses=%v messages=%v, want false/true",
			catalog.ServesResponses, catalog.ServesMessages)
	}
}
