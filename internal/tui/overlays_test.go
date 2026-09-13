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
)

func TestHelpOverlayToggle(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
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
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
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

// The provider-switch binding documents the switcher, which only exists in
// multi-provider mode (or with a single named provider). A single unnamed
// provider must keep the legacy overlay byte-identical.
func TestHelpOverlaySingleProviderOmitsSwitchProvider(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	if m.hasSwitcher() {
		t.Fatal("a single unnamed provider must not render a provider switcher")
	}
	if s := m.renderHelpOverlay(); strings.Contains(s, "Switch provider") {
		t.Errorf("single-provider help overlay must not document the provider switcher, got:\n%s", s)
	}
}

func TestHelpOverlayMultiProviderShowsSwitchProvider(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	})
	m.width = 80
	m.height = 24

	if !m.hasSwitcher() {
		t.Fatal("multiple providers must render a provider switcher")
	}
	s := m.renderHelpOverlay()
	for _, want := range []string{"Tab/Shift+Tab", "Click name ↕", "Wheel", "Switch provider"} {
		if !strings.Contains(s, want) {
			t.Errorf("multi-provider help overlay should document %q, got:\n%s", want, s)
		}
	}
}

// A single provider with an explicit name also renders the switcher, so its
// overlay documents the binding too.
func TestHelpOverlaySingleNamedProviderShowsSwitchProvider(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Name: "acme", Concurrency: 4}})
	m.width = 80
	m.height = 24

	if !m.hasSwitcher() {
		t.Fatal("a single named provider renders a provider switcher")
	}
	s := m.renderHelpOverlay()
	for _, want := range []string{"Tab/Shift+Tab", "Click name ↕", "Wheel", "Switch provider"} {
		if !strings.Contains(s, want) {
			t.Errorf("single-named-provider help overlay should document %q, got:\n%s", want, s)
		}
	}
}

func TestRenderConfirmOverlay_ContainsPrompt(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	s := m.renderConfirmOverlay()
	text := stripANSI(s)
	if !strings.Contains(text, "Clear all cumulative counters") {
		t.Errorf("confirm overlay should contain prompt text, got: %s", text)
	}
}

func TestRenderHelpOverlay_ContainsKeybindings(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	s := m.renderHelpOverlay()
	text := stripANSI(s)
	for _, kw := range []string{"Switch tab", "scroll", "filter", "Quit", "Reset Stats"} {
		if !strings.Contains(text, kw) {
			t.Errorf("help overlay should contain %q", kw)
		}
	}
}

// TestFooterMentionsReset pins the footer's c:reset hint so the binding stays
// discoverable from every tab.

func TestHelpOverlay_DocumentsHorizontalScrollAndLogDetail(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	text := stripANSI(m.renderHelpOverlay())
	for _, want := range []string{"Scroll left/right", "full log message"} {
		if !strings.Contains(text, want) {
			t.Errorf("help overlay should contain %q, got:\n%s", want, text)
		}
	}
}
