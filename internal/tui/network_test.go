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

package tui

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
)

func mustParseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestNetworkFilterType_Cycle(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	if m.networkFilterType != networkFilterAll {
		t.Errorf("initial type = %d, want all", m.networkFilterType)
	}
	m = update(m, key('t'))
	if m.networkFilterType != networkFilterJSON {
		t.Errorf("after one 't': type = %d, want json", m.networkFilterType)
	}
	for i := 1; i < 5; i++ {
		m = update(m, key('t'))
	}
	if m.networkFilterType != networkFilterAll {
		t.Errorf("after cycling: type = %d, want all", m.networkFilterType)
	}
}

func TestNetworkFilterStatus_Cycle(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	if m.networkFilterStatus != networkStatusAll {
		t.Errorf("initial status = %d, want all", m.networkFilterStatus)
	}
	m = update(m, key('s'))
	if m.networkFilterStatus != networkStatus2xx {
		t.Errorf("after one 's': status = %d, want 2xx", m.networkFilterStatus)
	}
	for i := 1; i < 4; i++ {
		m = update(m, key('s'))
	}
	if m.networkFilterStatus != networkStatusAll {
		t.Errorf("after cycling: status = %d, want all", m.networkFilterStatus)
	}
}

func TestComputeVisibleNetworkEntries_NoJournal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	entries := m.computeVisibleNetworkEntries()
	if entries != nil {
		t.Errorf("entries = %v, want nil with no journal", entries)
	}
}

func TestComputeVisibleNetworkEntries_WithTypeFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	j := journal.New(100, 1<<20)
	j.Record(&journal.Entry{Method: "POST", URL: mustParseURL("/v1/messages"), StatusCode: 200, ContentType: "application/json"})
	j.Record(&journal.Entry{Method: "GET", URL: mustParseURL("/health"), StatusCode: 200, ContentType: "text/html"})
	m.journal = j
	m.networkFilterType = networkFilterJSON
	entries := m.computeVisibleNetworkEntries()
	if len(entries) != 1 {
		t.Errorf("len(entries) = %d, want 1 (json only)", len(entries))
	}
}

func TestComputeVisibleNetworkEntries_WithStatusFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	j := journal.New(100, 1<<20)
	j.Record(&journal.Entry{Method: "POST", URL: mustParseURL("/v1/messages"), StatusCode: 200, ContentType: "application/json"})
	j.Record(&journal.Entry{Method: "POST", URL: mustParseURL("/v1/messages"), StatusCode: 429, ContentType: "application/json"})
	m.journal = j
	m.networkFilterStatus = networkStatus4xx
	entries := m.computeVisibleNetworkEntries()
	if len(entries) != 1 {
		t.Errorf("len(entries) = %d, want 1 (4xx only)", len(entries))
	}
}

func TestComputeVisibleNetworkEntries_WithTextFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	j := journal.New(100, 1<<20)
	j.Record(&journal.Entry{Method: "POST", URL: mustParseURL("/v1/messages"), StatusCode: 200, ContentType: "application/json"})
	j.Record(&journal.Entry{Method: "GET", URL: mustParseURL("/health"), StatusCode: 200, ContentType: "text/html"})
	m.journal = j
	m.filterText = "messages"
	entries := m.computeVisibleNetworkEntries()
	if len(entries) != 1 {
		t.Errorf("len(entries) = %d, want 1 (text filter)", len(entries))
	}
}

func TestRenderNetwork_EmptyEntries(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	s := m.renderNetwork()
	if !strings.Contains(s, "No network entries") {
		t.Errorf("should mention 'No network entries', got: %s", s)
	}
}

func TestRenderNetworkMarksAbortedEntries(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 100
	m.height = 24
	m.tab = tabNetwork
	j := journal.New(100, 1<<20)
	e := &journal.Entry{Method: "POST", URL: mustParseURL("/v1/messages"), StatusCode: 200, ContentType: "application/json", Aborted: true}
	j.Record(e)
	m.journal = j
	m.networkFiltered = m.computeVisibleNetworkEntries()

	s := stripANSI(m.renderNetwork())
	if !strings.Contains(s, "abort") {
		t.Fatalf("Network tab should mark aborted entries, got: %s", s)
	}
	detail := stripANSI(m.renderNetworkDetail(e))
	if !strings.Contains(detail, "Outcome:  aborted") {
		t.Fatalf("Network detail should show aborted outcome, got: %s", detail)
	}
}

func TestRenderNetwork_WithFilterIndicators(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabNetwork
	m.networkFilterType = networkFilterJSON
	m.networkFilterStatus = networkStatus2xx
	s := m.renderNetwork()
	if !strings.Contains(s, "type:json") {
		t.Errorf("should show type filter indicator, got: %s", s)
	}
	if !strings.Contains(s, "status:2xx") {
		t.Errorf("should show status filter indicator, got: %s", s)
	}
}

// ─── TUI-10: Scrollbar, Status Bar, canInspect, Overlays ───

func TestRoutesTabSortedByTotal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRoutes
	m.snap.RouteStats = map[string]metrics.RouteStat{
		"POST /v1/chat/completions": {Total: 5},
		"POST /v1/messages":         {Total: 100},
		"POST /embeddings":          {Total: 1},
	}

	v := m.View()
	msgIdx := strings.Index(v.Content, "POST /v1/messages")
	chatIdx := strings.Index(v.Content, "POST /v1/chat/completions")
	if msgIdx < 0 || chatIdx < 0 {
		t.Fatal("missing route entries")
	}
	if msgIdx > chatIdx {
		t.Error("POST /v1/messages (total=100) should appear before chat/completions (total=5)")
	}
}

func TestRoutesTabDeterministicSort(t *testing.T) {
	// Three routes with the same total — must sort alphabetically every time.
	for iter := range 10 {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = 80
		m.height = 24
		m.tab = tabRoutes
		m.snap.RouteStats = map[string]metrics.RouteStat{
			"POST /v1/messages":         {Total: 5},
			"POST /v1/chat/completions": {Total: 5},
			"POST /embeddings":          {Total: 5},
		}

		v := m.View()
		// All tied at 5 → alphabetical: embeddings, chat/completions, messages
		embIdx := strings.Index(v.Content, "POST /embeddings")
		chatIdx := strings.Index(v.Content, "POST /v1/chat/completions")
		msgIdx := strings.Index(v.Content, "POST /v1/messages")
		if chatIdx < 0 || embIdx < 0 || msgIdx < 0 {
			t.Fatalf("iter %d: missing route entries", iter)
		}
		if embIdx >= chatIdx {
			t.Errorf("iter %d: embeddings (%d) should precede chat/completions (%d)", iter, embIdx, chatIdx)
		}
		if chatIdx >= msgIdx {
			t.Errorf("iter %d: chat/completions (%d) should precede messages (%d)", iter, chatIdx, msgIdx)
		}
	}
}

func TestPerRouteRate_TUI10(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200, Time: time.Now()},
		{Method: "POST", Path: "/v1/messages", Status: 200, Time: time.Now()},
		{Method: "GET", Path: "/health", Status: 200, Time: time.Now()},
	}
	rates := m.perRouteRate()
	if len(rates) == 0 {
		t.Error("perRouteRate should return non-empty map")
	}
	msgRate, ok := rates["POST /v1/messages"]
	if !ok {
		t.Error("perRouteRate should have entry for POST /v1/messages")
	}
	if msgRate <= 0 {
		t.Errorf("rate for POST /v1/messages = %f, want > 0", msgRate)
	}
}

// TestRenderNetworkDetailMarksTruncatedBody pins the marker's position: it
// precedes the (256-rune-capped) preview so a normal terminal width cannot
// clip it, and it is absent for a complete body.
func TestRenderNetworkDetailMarksTruncatedBody(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	// Tall enough that the variable-line budget admits the body preview
	// (the renderer reserves 17 fixed lines and splits the rest).
	m.height = 60
	m.tab = tabNetwork
	j := journal.New(100, 1<<20)
	truncated := &journal.Entry{
		Method: "POST", URL: mustParseURL("/v1/messages"), StatusCode: 200,
		ContentType: "application/json", RequestBody: []byte(strings.Repeat("x", 4096)),
		RequestBodyTruncated: true,
	}
	complete := &journal.Entry{
		Method: "POST", URL: mustParseURL("/v1/messages"), StatusCode: 200,
		ContentType: "application/json", RequestBody: []byte(`{"hello":"world"}`),
	}
	j.Record(truncated)
	j.Record(complete)
	m.journal = j
	m.networkFiltered = m.computeVisibleNetworkEntries()

	detail := stripANSI(m.renderNetworkDetail(truncated))
	if !strings.Contains(detail, "Body:     (truncated) ") {
		t.Fatalf("truncated body marker must precede the preview, got: %s", detail)
	}
	detail = stripANSI(m.renderNetworkDetail(complete))
	if strings.Contains(detail, "(truncated)") {
		t.Fatalf("complete body must not carry the marker, got: %s", detail)
	}
}
