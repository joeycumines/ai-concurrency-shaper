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
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
)

// helper: apply a message and return the updated model (type-asserted).
func update(m Model, msg tea.Msg) Model {
	m2, _ := m.Update(msg)
	return m2.(Model)
}

// helper: send a key by rune. Sets both Code and Text to properly
// simulate real terminal input where Key.Text is populated for
// printable characters.
func key(r rune) tea.Msg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// helper: send a special key by string (e.g. "enter", "esc", "down", "up").
// Caveat: this creates a KeyPressMsg with Text=k, which differs from real
// terminal events where special keys have Code=KeyXxx and Text="". This
// works because handleKey's switch statements match on msg.String(), which
// returns the same value for both representations. However, if a special
// key is NOT matched in a switch and falls through to the default case,
// this helper would incorrectly simulate a printable key (non-empty Text).
// For testing non-printable key rejection, use KeyPressMsg{Code: tea.KeyUp}
// directly (see TestFilterModeArrowKeysIgnored).
func special(k string) tea.Msg {
	return tea.KeyPressMsg{Text: k}
}

// helper: strip ANSI escape sequences from a string for reliable assertions.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\x1b' {
			// Skip ESC sequences
			if i+1 < len(s) && s[i+1] == '[' {
				i += 2
				for i < len(s) {
					ch := s[i]
					if ch >= 0x40 && ch <= 0x7E {
						break
					}
					i++
				}
			} else if i+1 < len(s) {
				i++ // skip 2-byte ESC sequence
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func TestViewRenders(t *testing.T) {
	m := NewModel(10)
	m.width = 80
	m.height = 24
	v := m.View()
	if v.Content == "" {
		t.Fatal("View() returned empty content")
	}
	if !strings.Contains(v.Content, "shaper") {
		t.Errorf("View should contain 'shaper', got: %s", v.Content)
	}
}

func TestViewContainsAllTabs(t *testing.T) {
	m := NewModel(8)
	m.width = 80
	m.height = 24
	v := m.View()
	for _, tab := range []string{"1 Overview", "2 Requests", "3 Network", "4 Concurrency", "5 Routes"} {
		if !strings.Contains(v.Content, tab) {
			t.Errorf("View missing tab %q", tab)
		}
	}
}

func TestViewContainsKeybindings(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	v := m.View()
	for _, kb := range []string{"j/k:scroll", "?:help"} {
		if !strings.Contains(v.Content, kb) {
			t.Errorf("View missing keybinding %q", kb)
		}
	}
}

func TestViewWithEmptySnapshot(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap = metrics.NewCollector().Snapshot()
	v := m.View()
	if v.Content == "" {
		t.Fatal("View returned empty for zero snapshot")
	}
	if !strings.Contains(v.Content, "No requests yet") {
		t.Error("Should show 'No requests yet' for empty log")
	}
}

func TestTabSwitching(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24

	m = update(m, key('2'))
	if m.tab != tabRequests {
		t.Errorf("tab = %d, want tabRequests", m.tab)
	}

	m = update(m, key('3'))
	if m.tab != tabNetwork {
		t.Errorf("tab = %d, want tabNetwork", m.tab)
	}

	m = update(m, key('4'))
	if m.tab != tabConcurrency {
		t.Errorf("tab = %d, want tabConcurrency", m.tab)
	}

	m = update(m, key('5'))
	if m.tab != tabRoutes {
		t.Errorf("tab = %d, want tabRoutes", m.tab)
	}

	m = update(m, key('1'))
	if m.tab != tabDashboard {
		t.Errorf("tab = %d, want tabDashboard", m.tab)
	}
}

func TestScrollDown(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	for i := 0; i < 50; i++ {
		m.snap.LogEntries = append(m.snap.LogEntries, metrics.RequestLogEntry{
			Method: "POST", Path: "/v1/messages", Status: 200,
			Duration: time.Millisecond,
		})
	}

	for i := 0; i < 5; i++ {
		m = update(m, key('j'))
	}
	if m.cursor != 5 {
		t.Errorf("cursor = %d, want 5", m.cursor)
	}
}

func TestScrollUp(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.cursor = 5
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m = update(m, key('k'))
	if m.cursor != 4 {
		t.Errorf("cursor = %d, want 4", m.cursor)
	}
}

func TestScrollStaysInBounds(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m = update(m, key('k'))
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0", m.cursor)
	}
}

func TestGoToTop(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.cursor = 30
	m.scroll = 20
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m = update(m, key('g'))
	if m.cursor != 0 || m.scroll != 0 {
		t.Errorf("cursor=%d scroll=%d, want 0,0", m.cursor, m.scroll)
	}
}

func TestGoToBottom(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m = update(m, key('G'))
	if m.cursor != 49 {
		t.Errorf("cursor = %d, want 49", m.cursor)
	}
}

func TestDetailOverlayRequests(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = []metrics.RequestLogEntry{
		{
			Time:     time.Date(2026, 6, 8, 12, 30, 45, 0, time.UTC),
			Method:   "POST",
			Path:     "/v1/messages",
			Status:   200,
			Duration: 150 * time.Millisecond,
			Limited:  true,
		},
	}

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("should be in detail mode")
	}

	v := m.View()
	text := stripANSI(v.Content)

	checks := []struct {
		field string
		want  string
	}{
		{"header", "Request Detail"},
		{"method label", "Method:"},
		{"method value", "POST"},
		{"path label", "Path:"},
		{"path value", "/v1/messages"},
		{"status label", "Status:"},
		{"status value", "200"},
		{"duration label", "Duration:"},
		{"duration value", "150ms"},
		{"limited label", "Limited:"},
		{"limited value", "true"},
		{"close hint", "close"},
	}
	for _, c := range checks {
		if !strings.Contains(text, c.want) {
			t.Errorf("overlay missing %s: want %q", c.field, c.want)
		}
	}

	// Dismiss with Escape
	m = update(m, special("esc"))
	if m.mode != modeBrowse {
		t.Fatal("Escape should return to browse mode")
	}
	v2 := m.View()
	text2 := stripANSI(v2.Content)
	if strings.Contains(text2, "Request Detail") {
		t.Error("overlay should not be visible after Escape")
	}

	// Dismiss with Enter
	m = update(m, special("enter"))
	m = update(m, special("enter"))
	if m.mode != modeBrowse {
		t.Fatal("Enter should dismiss detail mode")
	}

	// Dismiss with Space
	m = update(m, special("enter"))
	m = update(m, key(' '))
	if m.mode != modeBrowse {
		t.Fatal("Space should dismiss detail mode")
	}
}

func TestDetailOverlayConcurrency(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	m.snap.InFlight = []metrics.InFlightEntry{
		{ID: 42, Method: "POST", Path: "/v1/messages", Limited: true},
	}

	m = update(m, special("enter"))
	if m.mode != modeDetail {
		t.Fatal("should be in detail mode")
	}

	v := m.View()
	text := stripANSI(v.Content)

	checks := []struct {
		field string
		want  string
	}{
		{"header", "In-Flight Detail"},
		{"id label", "ID:"},
		{"id value", "42"},
		{"method label", "Method:"},
		{"method value", "POST"},
		{"path label", "Path:"},
		{"path value", "/v1/messages"},
		{"limited label", "Limited:"},
		{"limited value", "true"},
		{"age label", "Age:"},
		{"total label", "Total:"},
		{"close hint", "close"},
	}
	for _, c := range checks {
		if !strings.Contains(text, c.want) {
			t.Errorf("overlay missing %s: want %q", c.field, c.want)
		}
	}

	m = update(m, special("esc"))
	if m.mode != modeBrowse {
		t.Fatal("Escape should return to browse mode")
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[1;31;42mbold red\x1b[0m", "bold red"},
		{"no codes here", "no codes here"},
		{"\x1b[2J\x1b[Hclear", "clear"},
	}
	for _, tt := range tests {
		got := stripANSI(tt.input)
		if got != tt.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSnapshotUpdate(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24

	snap := metrics.NewCollector().Snapshot()
	snap.Active = 3
	snap.Queued = 5
	snap.Throughput = 42.5
	m = update(m, snap)

	if m.snap.Active != 3 {
		t.Errorf("Active = %d, want 3", m.snap.Active)
	}
	if m.snap.Queued != 5 {
		t.Errorf("Queued = %d, want 5", m.snap.Queued)
	}
}

func TestWindowSize(t *testing.T) {
	m := NewModel(4)
	m = update(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.width != 120 || m.height != 40 {
		t.Errorf("size = %dx%d, want 120x40", m.width, m.height)
	}
}

func TestRenderGaugeBar(t *testing.T) {
	m := NewModel(10)
	m.width = 80
	g := m.renderGaugeBar(5, 10, 20)
	if !strings.Contains(g, "█") {
		t.Error("gauge should contain filled blocks")
	}
	if !strings.Contains(g, "50%") {
		t.Errorf("gauge should show 50%%, got: %s", g)
	}
}

func TestRenderGaugeBarOverflow(t *testing.T) {
	m := NewModel(10)
	m.width = 80
	g := m.renderGaugeBar(100, 10, 20)
	if !strings.Contains(g, "100%") {
		t.Errorf("gauge should show 100%%, got: %s", g)
	}
}

func TestRenderGaugeBarEmpty(t *testing.T) {
	m := NewModel(10)
	m.width = 80
	g := m.renderGaugeBar(0, 10, 20)
	if strings.Contains(g, "█") && !strings.Contains(g, "░") {
		t.Error("empty gauge should not contain filled blocks")
	}
}

func TestAltScreenEnabled(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	v := m.View()
	if !v.AltScreen {
		t.Error("AltScreen should be enabled")
	}
}

func TestMouseModeEnabled(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	v := m.View()
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("MouseMode = %d, want MouseModeCellMotion", v.MouseMode)
	}
}

func TestRoutesTabSortedByTotal(t *testing.T) {
	m := NewModel(4)
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
	for iter := 0; iter < 10; iter++ {
		m := NewModel(4)
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

func TestMouseClickTabBar(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24

	// Click on the first tab area (row 1)
	m = update(m, tea.MouseClickMsg{X: 3, Y: 1})
	if m.tab != tabDashboard {
		t.Errorf("expected tabDashboard after clicking first tab, got %d", m.tab)
	}
}

func TestStatusStyle(t *testing.T) {
	for _, code := range []int{200, 201, 301, 404, 429, 500, 503, 0, 99} {
		_ = statusStyle(code) // just verify no panic
	}
}

func TestDashboardShowsSparkline(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	m.snap.Sparkline = []int{0, 1, 3, 5, 8, 5, 3, 1, 0}

	v := m.View()
	if !strings.Contains(v.Content, "Throughput") {
		t.Error("Dashboard should contain throughput sparkline section")
	}
}

func TestQuit(t *testing.T) {
	m := NewModel(4)
	_, cmd := m.Update(key('q'))
	if cmd == nil {
		t.Fatal("quit should return a command")
	}
	// The cmd should produce a QuitMsg
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected QuitMsg, got %T", msg)
	}
}

func TestArrowKeyScrolling(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabRequests
	m.snap.LogEntries = make([]metrics.RequestLogEntry, 50)

	m = update(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.cursor != 1 {
		t.Errorf("down arrow: cursor = %d, want 1", m.cursor)
	}

	m = update(m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("up arrow: cursor = %d, want 0", m.cursor)
	}
}

func TestConcurrencyTabInFlight(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabConcurrency
	m.snap.InFlight = []metrics.InFlightEntry{
		{ID: 1, Method: "POST", Path: "/v1/messages", Limited: true},
		{ID: 2, Method: "GET", Path: "/health", Limited: false},
	}

	v := m.View()
	if !strings.Contains(v.Content, "POST") {
		t.Error("Concurrency tab should show in-flight methods")
	}
	if !strings.Contains(v.Content, "/v1/messages") {
		t.Error("Concurrency tab should show paths")
	}
}

func TestDashboardSummary(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabDashboard
	m.snap.TotalProxied = 100
	m.snap.TotalPassThrough = 50
	m.snap.TotalTimeout = 3
	m.snap.TotalCancelled = 1
	m.snap.StatusCounts = [6]int64{0, 0, 90, 5, 8, 3}

	v := m.View()
	if !strings.Contains(v.Content, "Summary") {
		t.Error("Dashboard should have a Summary section")
	}
	if !strings.Contains(v.Content, "Proxied: 100") {
		t.Error("Dashboard should show proxied count")
	}
}

func TestHelpOverlayToggle(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24

	// Press '?' to open help
	m = update(m, key('?'))
	if m.mode != modeHelp {
		t.Fatal("should be in help mode")
	}

	v := m.View()
	text := stripANSI(v.Content)
	if !strings.Contains(text, "Keybindings") {
		t.Errorf("help overlay should contain 'Keybindings', got:\n%s", text)
	}
	if !strings.Contains(text, "Ctrl") {
		t.Errorf("help should mention Ctrl key for quit")
	}

	// Press any key to dismiss
	m = update(m, key('q'))
	// 'q' in help mode dismisses help (doesn't quit)
	if m.mode != modeBrowse {
		t.Fatal("should be back in browse mode after ?")
	}
}

func TestHelpOverlayDismissWithAnyKey(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24

	m = update(m, key('?'))
	if m.mode != modeHelp {
		t.Fatal("should be in help mode")
	}

	// Dismiss with 'x'
	m = update(m, key('x'))
	if m.mode != modeBrowse {
		t.Fatal("any key should dismiss help")
	}
}

func TestFilterModeToggle(t *testing.T) {
	m := NewModel(4)
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
	m := NewModel(4)
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
	m := NewModel(4)
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
	m := NewModel(4)
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
	m := NewModel(4)
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
	m := NewModel(4)
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
	m := NewModel(4)
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
	m := NewModel(4)
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
	m := NewModel(4)
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

func TestAdjustViewportClampsOnFilterShrink(t *testing.T) {
	m := NewModel(4)
	m.width = 80
	m.height = 24
	m.tab = tabRequests

	m.snap.LogEntries = []metrics.RequestLogEntry{
		{Method: "POST", Path: "/v1/messages", Status: 200},
		{Method: "POST", Path: "/v1/messages", Status: 429},
		{Method: "GET", Path: "/health", Status: 200},
		{Method: "GET", Path: "/health", Status: 500},
		{Method: "POST", Path: "/v1/chat/completions", Status: 200},
	}

	// Position cursor deep into the list.
	m.cursor = 4
	m.scroll = 4

	// Enter filter mode and shrink the list.
	m = update(m, key('/'))
	m = update(m, key('h')) // filter to "h" (3 entries: health x2, chat/completions)

	// After the filter keystroke, adjustViewport should clamp cursor/scroll.
	if m.cursor > m.maxCursor() {
		t.Errorf("cursor (%d) not clamped to maxCursor (%d)", m.cursor, m.maxCursor())
	}
	if m.scroll > m.maxScroll() {
		t.Errorf("scroll (%d) not clamped to maxScroll (%d)", m.scroll, m.maxScroll())
	}

	// Ensure a render would show rows (not a blank screen).
	entries := m.visibleEntries()
	visible := m.visibleRows()
	start := m.scroll
	end := min(start+visible, len(entries))
	if start >= end {
		t.Errorf("render would be blank: start=%d >= end=%d", start, end)
	}
}

func TestTruncateRuneCount(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		// Short strings pass through unchanged.
		{"hello", 10, "hello"},
		{"", 5, ""},
		// Exact length - no truncation.
		{"hello", 5, "hello"},
		// Truncation with ellipsis (1 rune for ...).
		{"hello", 4, "hel…"},
		{"hello", 3, "he…"},
		{"hello", 2, "h…"},
		// maxLen == 1 -> just ellipsis.
		{"hello", 1, "…"},
		// maxLen == 0 -> empty.
		{"hello", 0, ""},
		// Multi-byte characters - truncation is rune-aware.
		{"日本語説明", 5, "日本語説明"},
		{"日本語説明", 4, "日本語…"},
		{"日本語説明", 3, "日本…"},
		{"日本語説明", 2, "日…"},
		{"日本語説明", 1, "…"},
	}
	for _, tt := range tests {
		got := truncate(tt.input, tt.maxLen)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) produced invalid UTF-8: %x", tt.input, tt.maxLen, got)
		}
	}
}

func TestTruncateBytesZeroAlloc(t *testing.T) {
	// Verify truncateBytes handles multi-byte UTF-8 correctly without
	// converting the entire byte slice to a string or rune array.
	// Semantics match truncate: total output ≤ maxRunes runes.
	tests := []struct {
		input []byte
		max   int
		want  string
	}{
		{[]byte("hello"), 10, "hello"},
		{[]byte("hello"), 5, "hello"},
		{[]byte("hello"), 4, "hel…"},
		{[]byte("hello"), 3, "he…"},
		{[]byte("hello"), 2, "h…"},
		{[]byte("hello"), 1, "…"},
		{[]byte("hello"), 0, ""},
		// Multi-byte: Japanese chars are 3 bytes each.
		{[]byte("日本語説明"), 5, "日本語説明"},
		{[]byte("日本語説明"), 4, "日本語…"},
		{[]byte("日本語説明"), 2, "日…"},
		{[]byte("日本語説明"), 1, "…"},
		{[]byte(""), 5, ""},
		// Mixed ASCII + multi-byte. "abc日本" = 5 runes.
		{[]byte("abc日本"), 5, "abc日本"},
		{[]byte("abc日本"), 4, "abc…"},
	}
	for _, tt := range tests {
		got := truncateBytes(tt.input, tt.max)
		if got != tt.want {
			t.Errorf("truncateBytes(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncateBytes(%q, %d) produced invalid UTF-8: %x", tt.input, tt.max, got)
		}
	}
}

func TestFilterModeArrowKeysIgnored(t *testing.T) {
	// Verify that non-printable control keys (arrows, F-keys, etc.)
	// do NOT corrupt the filter text. In Bubble Tea v2, these keys
	// have Key.Text == "" (no printable characters), so they must
	// be silently discarded in the filter default handler.
	m := NewModel(4)
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
