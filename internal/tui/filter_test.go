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
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
)

func TestFilterModeToggle(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	// Press '/' to enter filter mode
	m = update(m, special("/"))
	if m.mode != modeFilter {
		t.Fatal("should be in filter mode")
	}
	if m.filterText != "" {
		t.Fatal("filter text should be empty initially")
	}
}

func TestFilterModeAccumulate(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	// Enter filter mode and type 'messages'
	m = update(m, special("/"))
	m = update(m, key('m'))
	m = update(m, key('e'))
	m = update(m, key('s'))

	if m.filterText != "mes" {
		t.Errorf("filterText = %q, want %q", m.filterText, "mes")
	}
}

func TestFilterModeEnterAccepts(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m = update(m, special("/"))
	m = update(m, key('m'))
	m = update(m, special("enter"))

	if m.mode != modeBrowse {
		t.Fatal("Enter should accept filter and return to browse")
	}
	if m.filterText != "m" {
		t.Errorf("filterText = %q, want %q", m.filterText, "m")
	}
}

func TestFilterModeEscClears(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m = update(m, special("/"))
	m = update(m, key('m'))
	m = update(m, special("esc"))

	if m.mode != modeBrowse {
		t.Fatal("Esc should return to browse")
	}
	if m.filterText != "" {
		t.Errorf("filterText = %q, want empty", m.filterText)
	}
}

func TestFilterModeBackspace(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m = update(m, special("/"))
	m = update(m, key('m'))
	m = update(m, key('e'))
	m = update(m, key('s'))
	m = update(m, special("backspace"))

	if m.filterText != "me" {
		t.Errorf("filterText = %q, want %q", m.filterText, "me")
	}
}

func TestFilterModeBackspaceMultiByte(t *testing.T) {
	// Verify that backspace removes a full rune, not just one byte.
	// The Japanese character '日' is 3 bytes in UTF-8.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m = update(m, special("/"))
	// Simulate typing multi-byte characters by setting filterText directly
	// and then pressing backspace to verify rune-aware deletion.
	m.filterText = "abc日"
	m = update(m, special("backspace"))

	if m.filterText != "abc" {
		t.Errorf("filterText = %q, want %q after backspace on multi-byte rune", m.filterText, "abc")
	}
	// Verify the string is valid UTF-8.
	if !utf8.ValidString(m.filterText) {
		t.Errorf("filterText is invalid UTF-8: %x", m.filterText)
	}

	// Backspace again removes 'c'.
	m = update(m, special("backspace"))
	if m.filterText != "ab" {
		t.Errorf("filterText = %q, want %q", m.filterText, "ab")
	}

	// Two multi-byte runes, delete one.
	m.filterText = "日月"
	m = update(m, special("backspace"))
	if m.filterText != "日" {
		t.Errorf("filterText = %q, want %q after deleting multi-byte rune", m.filterText, "日")
	}
}

func TestFilterFiltersEntries(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200},
		{Method: "GET", Path: "/health", Status: 200},
		{Method: "POST", Path: "/v1/chat/completions", Status: 200},
		{Method: "POST", Path: "/v1/messages", Status: 429},
	}

	// Filter for "messages"
	entries := m.visibleEntries()
	if len(entries) != 4 {
		t.Fatalf("without filter: got %d entries, want 4", len(entries))
	}

	// Apply filter
	m.filterText = "messages"
	entries = m.visibleEntries()
	if len(entries) != 2 {
		t.Errorf("with filter 'messages': got %d entries, want 2", len(entries))
	}

	// Filter for "health"
	m.filterText = "health"
	entries = m.visibleEntries()
	if len(entries) != 1 {
		t.Errorf("with filter 'health': got %d entries, want 1", len(entries))
	}
	if entries[0].Path != "/health" {
		t.Errorf("filtered entry path = %q, want /health", entries[0].Path)
	}

	// Filter with no matches
	m.filterText = "nonexistent"
	entries = m.visibleEntries()
	if len(entries) != 0 {
		t.Errorf("with non-matching filter: got %d entries, want 0", len(entries))
	}
}

func TestFilterByMethod(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200},
		{Method: "GET", Path: "/v1/models", Status: 200},
		{Method: "POST", Path: "/v1/chat/completions", Status: 200},
	}

	// Filter by method "POST"
	m.filterText = "post"
	entries := m.visibleEntries()
	if len(entries) != 2 {
		t.Errorf("filter 'post': got %d entries, want 2", len(entries))
	}

	// Filter by method "GET"
	m.filterText = "get"
	entries = m.visibleEntries()
	if len(entries) != 1 {
		t.Errorf("filter 'get': got %d entries, want 1", len(entries))
	}
}

func TestFilterByStatus(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200},
		{Method: "POST", Path: "/v1/messages", Status: 429},
		{Method: "GET", Path: "/health", Status: 500},
	}

	// Filter by status code "429"
	m.filterText = "429"
	entries := m.visibleEntries()
	if len(entries) != 1 {
		t.Errorf("filter '429': got %d entries, want 1", len(entries))
	}

	// Filter by partial status "4" (matches 429)
	m.filterText = "4"
	entries = m.visibleEntries()
	if len(entries) != 1 {
		t.Errorf("filter '4': got %d entries, want 1", len(entries))
	}

	// Filter by "500"
	m.filterText = "500"
	entries = m.visibleEntries()
	if len(entries) != 1 {
		t.Errorf("filter '500': got %d entries, want 1", len(entries))
	}
}

func TestFilterModeArrowKeysIgnored(t *testing.T) {
	// Verify that non-printable control keys (arrows, F-keys, etc.)
	// do NOT corrupt the filter text. In Bubble Tea v2, these keys
	// have Key.Text == "" (no printable characters), so they must
	// be silently discarded in the filter default handler.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	// Enter filter mode and type a character.
	m = update(m, special("/"))
	m = update(m, key('a'))
	if m.filterText != "a" {
		t.Fatalf("filterText = %q, want %q before arrow key", m.filterText, "a")
	}

	// Press Up arrow (special key code, no printable Text).
	// In real terminal input, Key.Code = KeyUp, Key.Text = "".
	m = update(m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.filterText != "a" {
		t.Errorf("after KeyUp: filterText = %q, want %q (arrow key should not corrupt filter)", m.filterText, "a")
	}

	// Press Down arrow.
	m = update(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.filterText != "a" {
		t.Errorf("after KeyDown: filterText = %q, want %q (arrow key should not corrupt filter)", m.filterText, "a")
	}

	// Press F1 (function key).
	m = update(m, tea.KeyPressMsg{Code: tea.KeyF1})
	if m.filterText != "a" {
		t.Errorf("after F1: filterText = %q, want %q (F-key should not corrupt filter)", m.filterText, "a")
	}

	// Press Home.
	m = update(m, tea.KeyPressMsg{Code: tea.KeyHome})
	if m.filterText != "a" {
		t.Errorf("after Home: filterText = %q, want %q (Home should not corrupt filter)", m.filterText, "a")
	}
}
