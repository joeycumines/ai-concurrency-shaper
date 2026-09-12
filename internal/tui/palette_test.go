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

	tea "charm.land/bubbletea/v2"
)

func TestPaletteOpenClose(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	})
	m.width = 80
	m.height = 24

	if m.mode == modePalette {
		t.Fatal("palette should not be open initially")
	}

	// Ctrl+K opens palette
	m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})
	if m.mode != modePalette {
		t.Fatalf("after ctrl+k: mode = %d, want modePalette (%d)", m.mode, modePalette)
	}

	// Esc closes palette
	m = update(m, tea.KeyPressMsg{Code: tea.KeyEscape, Text: "esc"})
	if m.mode != modeBrowse {
		t.Fatalf("after esc: mode = %d, want modeBrowse", m.mode)
	}

	// Ctrl+K again opens, then ctrl+k again closes (toggle)
	m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})
	if m.mode != modePalette {
		t.Fatal("second ctrl+k should open palette")
	}
	m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})
	if m.mode != modeBrowse {
		t.Fatal("third ctrl+k should close palette (toggle)")
	}
}

func TestPaletteFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
		{Name: "openai", Concurrency: 12},
	})
	m.width = 80
	m.height = 24
	m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})

	allCount := len(m.palette.cmds)
	if allCount == 0 {
		t.Fatal("palette should have commands")
	}

	// Type 'a' — should filter to entries containing 'a' (case-insensitive subsequence)
	m = update(m, tea.KeyPressMsg{Code: 'a', Text: "a"})
	if m.palette.query != "a" {
		t.Fatalf("query = %q, want %q", m.palette.query, "a")
	}
	filteredCount := len(m.palette.cmds)
	if filteredCount >= allCount {
		t.Errorf("filtering with 'a' should reduce count: got %d, had %d", filteredCount, allCount)
	}

	// Backspace clears the filter
	m = update(m, tea.KeyPressMsg{Code: tea.KeyBackspace, Text: "backspace"})
	if m.palette.query != "" {
		t.Fatalf("after backspace: query = %q, want empty", m.palette.query)
	}
	if len(m.palette.cmds) != allCount {
		t.Errorf("after clearing filter: %d cmds, want %d", len(m.palette.cmds), allCount)
	}
}

func TestPaletteNavigateAndExecute(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	})
	m.width = 80
	m.height = 24
	m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})

	if m.palette.cursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.palette.cursor)
	}

	// Down moves cursor
	m = update(m, tea.KeyPressMsg{Code: tea.KeyDown, Text: "down"})
	if m.palette.cursor != 1 {
		t.Fatalf("after down: cursor = %d, want 1", m.palette.cursor)
	}

	// Up moves cursor back
	m = update(m, tea.KeyPressMsg{Code: tea.KeyUp, Text: "up"})
	if m.palette.cursor != 0 {
		t.Fatalf("after up: cursor = %d, want 0", m.palette.cursor)
	}

	// Up at top stays at 0
	m = update(m, tea.KeyPressMsg{Code: tea.KeyUp, Text: "up"})
	if m.palette.cursor != 0 {
		t.Fatalf("up at top: cursor = %d, want 0", m.palette.cursor)
	}

	// Execute first entry (should be "Switch to: acme (active)")
	firstCmd := m.palette.cmds[0]
	if firstCmd.kind != cmdSwitchProvider {
		t.Fatalf("first cmd kind = %d, want cmdSwitchProvider", firstCmd.kind)
	}
	m = update(m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m.mode != modeBrowse {
		t.Fatalf("after enter: mode = %d, want modeBrowse (palette should close)", m.mode)
	}
}

func TestPaletteProviderSwitch(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	})
	m.width = 80
	m.height = 24
	m.active = 0

	m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})

	// Find the anthropic switch command
	targetIdx := -1
	for i, cmd := range m.palette.cmds {
		if cmd.kind == cmdSwitchProvider && strings.Contains(cmd.label, "anthropic") {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		t.Fatal("no 'Switch to: anthropic' command found")
	}

	// Navigate to it
	for i := 0; i < targetIdx; i++ {
		m = update(m, tea.KeyPressMsg{Code: tea.KeyDown, Text: "down"})
	}
	if m.palette.cursor != targetIdx {
		t.Fatalf("cursor = %d, want %d", m.palette.cursor, targetIdx)
	}

	// Execute
	m = update(m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m.active != 1 {
		t.Fatalf("after executing switch: active = %d, want 1 (anthropic)", m.active)
	}
	if m.mode != modeBrowse {
		t.Fatalf("mode = %d, want modeBrowse after execution", m.mode)
	}
}

func TestPaletteTabSwitch(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Name: "acme", Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabDashboard

	m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})

	// Find the Requests tab command
	targetIdx := -1
	for i, cmd := range m.palette.cmds {
		if cmd.kind == cmdSwitchTab && strings.Contains(cmd.label, "Requests") {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		t.Fatal("no 'Tab: Requests' command found")
	}

	for i := 0; i < targetIdx; i++ {
		m = update(m, tea.KeyPressMsg{Code: tea.KeyDown, Text: "down"})
	}
	m = update(m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m.tab != tabRequests {
		t.Fatalf("after executing tab switch: tab = %d, want tabRequests (%d)", m.tab, tabRequests)
	}
}

func TestPaletteHelpCommand(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Name: "acme", Concurrency: 4}})
	m.width = 80
	m.height = 24

	m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})

	// Find help command
	targetIdx := -1
	for i, cmd := range m.palette.cmds {
		if cmd.kind == cmdShowHelp {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		t.Fatal("no 'Show help' command found")
	}

	for i := 0; i < targetIdx; i++ {
		m = update(m, tea.KeyPressMsg{Code: tea.KeyDown, Text: "down"})
	}
	m = update(m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m.mode != modeHelp {
		t.Fatalf("after help command: mode = %d, want modeHelp", m.mode)
	}
}

func TestPaletteResetCommand(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Name: "acme", Concurrency: 4}})
	m.width = 80
	m.height = 24

	m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})

	targetIdx := -1
	for i, cmd := range m.palette.cmds {
		if cmd.kind == cmdResetStats {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		t.Fatal("no 'Reset stats' command found")
	}

	for i := 0; i < targetIdx; i++ {
		m = update(m, tea.KeyPressMsg{Code: tea.KeyDown, Text: "down"})
	}
	m = update(m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m.mode != modeConfirm {
		t.Fatalf("after reset command: mode = %d, want modeConfirm", m.mode)
	}
}

func TestPaletteEmptyFilter(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Name: "acme", Concurrency: 4}})
	m.width = 80
	m.height = 24

	m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})

	// Type something that matches nothing
	for _, ch := range "zzzzz" {
		m = update(m, tea.KeyPressMsg{Code: ch, Text: string(ch)})
	}
	if len(m.palette.cmds) != 0 {
		t.Errorf("expected 0 commands after impossible filter, got %d", len(m.palette.cmds))
	}

	// Enter on empty list should just close
	m = update(m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m.mode != modeBrowse {
		t.Fatalf("enter on empty palette: mode = %d, want modeBrowse", m.mode)
	}
}

func TestPaletteRenderThinTerminal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	})
	// Very thin terminal
	m.width = 10
	m.height = 8

	m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})

	rendered := m.renderCommandPalette()
	if rendered == "" {
		t.Fatal("palette should render even at width 10")
	}

	// Each line should not exceed terminal width
	for i, line := range strings.Split(rendered, "\n") {
		// Strip ANSI for width measurement
		w := 0
		stripped := stripANSI(line)
		for _, r := range stripped {
			_ = r
			w++
		}
		// Rough check — lipgloss adds border chars so allow some margin
		if w > m.width+10 {
			t.Errorf("line %d width ~%d exceeds terminal width %d: %q", i, w, m.width, line)
		}
	}
}

func TestFuzzyMatch(t *testing.T) {
	tests := []struct {
		query, label string
		want         bool
	}{
		{"", "anything", true},
		{"acme", "Switch to: acme", true},
		{"acm", "Switch to: acme", true},
		{"swac", "Switch to: acme", true},
		{"xyz", "Switch to: acme", false},
		{"ACME", "Switch to: acme", true}, // case insensitive
		{"ae", "Switch to: acme", true},   // subsequence
	}
	for _, tt := range tests {
		got := fuzzyMatch(tt.query, tt.label)
		if got != tt.want {
			t.Errorf("fuzzyMatch(%q, %q) = %v, want %v", tt.query, tt.label, got, tt.want)
		}
	}
}
