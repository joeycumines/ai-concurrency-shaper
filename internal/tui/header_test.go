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
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
)

func TestMouseClickTabBar(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	// Click on the first tab area (row 1)
	m = update(m, tea.MouseClickMsg{X: 3, Y: 1})
	if m.tab != tabDashboard {
		t.Errorf("expected tabDashboard after clicking first tab, got %d", m.tab)
	}
}

func TestProviderSwitcherKeys(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
		{Name: "openai", Concurrency: 12},
	})
	m.width = 80
	m.height = 24

	// Tab cycles to the next provider and wraps from the last back to the first.
	for _, want := range []string{"anthropic", "openai", "acme"} {
		m = update(m, tea.KeyPressMsg{Code: tea.KeyTab})
		if got := m.providers[m.active].name; got != want {
			t.Fatalf("after Tab: active provider = %q, want %q", got, want)
		}
		if got := m.providerName(); got != " "+want {
			t.Errorf("after Tab: header name = %q, want %q", got, " "+want)
		}
	}

	// Shift+Tab cycles to the previous provider and wraps from the first back
	// to the last.
	for _, want := range []string{"openai", "anthropic", "acme"} {
		m = update(m, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
		if got := m.providers[m.active].name; got != want {
			t.Fatalf("after Shift+Tab: active provider = %q, want %q", got, want)
		}
	}
}

func TestProviderSwitcherChipClick(t *testing.T) {
	p := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
		{Name: "openai", Concurrency: 12},
	})
	p.width = 100
	p.height = 24

	// A click on header row 0 inside a chip's left-aligned range switches
	// to that provider. Chips start after the body+divider+gap and are
	// joined left-to-right: each chip occupies [col, col+w-1].
	// The spans come from the production budgetedChips layout — the same
	// parts chipAt hit-tests — so the test cannot drift from what is
	// actually rendered. At width 100 all three chips are at full width.
	col := p.row0ChipStart()
	layout := p.budgetedChips()
	parts := layout.parts
	if len(parts) != 3 {
		t.Fatalf("budgetedChips rendered %d chips at width 100, want all 3", len(parts))
	}
	for i, part := range parts {
		w := lipgloss.Width(part)
		p = update(p, tea.MouseClickMsg{X: col, Y: 0})
		if p.active != i {
			t.Fatalf("click at column %d: active = %d, want %d", col, p.active, i)
		}
		col += w + 1
	}

	// A click on row 0 far outside the chips (the brand-filled left side)
	// must leave the active provider unchanged.
	lastActive := p.active
	p = update(p, tea.MouseClickMsg{X: 0, Y: 0})
	if p.active != lastActive {
		t.Errorf("click outside chips changed active to %d, want %d (unchanged)", p.active, lastActive)
	}
}

func TestSingleProviderHeaderKeepsShaper(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	if m.hasSwitcher() {
		t.Fatal("a single unnamed provider must not render a provider switcher")
	}
	if s := m.renderProviderSwitcher(); s != "" {
		t.Fatalf("renderProviderSwitcher = %q, want \"\" for a single unnamed provider", s)
	}
	if got := m.providerName(); got != " ⚡ shaper" {
		t.Errorf("providerName = %q, want %q", got, " ⚡ shaper")
	}
	if _, ok := m.chipAt(0, 0); ok {
		t.Error("chipAt must not hit a chip for a single unnamed provider")
	}
	h := stripANSI(m.renderHeader())
	if !strings.Contains(h, "⚡ shaper") {
		t.Errorf("header must keep the \"⚡ shaper\" brand, got: %s", h)
	}
}

// TestHeaderWidthBudget pins the narrow-pane contract: for every
// width >= 40 and any provider names, the header row must never exceed the
// terminal width (no wrap onto the tab bar), the active provider's chip must
// stay visible and clickable, chipAt must map clicks to exactly the chips
// actually rendered (walking every rendered x-position), and the
// single-provider "⚡ shaper" header must be byte-identical at every width.
func TestHeaderWidthBudget(t *testing.T) {
	widths := []int{40, 60, 80, 120}
	multi := []ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	}

	// (a) Row 0 never exceeds m.width, for every width and every active
	// provider.
	for _, w := range widths {
		for active := range multi {
			m := NewModelForProviders(multi)
			m.width = w
			m.height = 24
			m.active = active
			m.syncActive() // mirror the new active provider's state, as switchProvider does
			if got := lipgloss.Width(m.renderHeader()); got > w {
				t.Errorf("width=%d active=%d: header row is %d cells, exceeds %d (wrap onto tab bar):\n%s",
					w, active, got, w, m.renderHeader())
			}
			// (b) The active provider's chip is always present across all
			// wrapped rows, and chipAt maps clicks to it.
			allRows := m.chipRowsLayout()
			found := false
			for ri, row := range allRows {
				for k, prov := range row.providers {
					if prov == active {
						found = true
						var col int
						if ri == 0 {
							col = m.row0ChipStart()
						} else {
							col = 1
						}
						for i, v := range row.parts {
							cw := lipgloss.Width(v)
							if i == k {
								if idx, ok := m.chipAt(col, ri); !ok || idx != prov {
									t.Errorf("width=%d active=%d: chipAt(%d,%d) = (%d,%v), want (%d,true)",
										w, active, col, ri, idx, ok, prov)
								}
								break
							}
							col += cw + 1
						}
					}
				}
			}
			if !found {
				t.Errorf("width=%d active=%d: active provider's chip missing from all rendered switcher rows", w, active)
			}
		}
	}

	// (c) chipAt maps every rendered chip x-position to exactly that chip,
	// and never reports a provider whose chip is not rendered.
	for _, w := range widths {
		for active := range multi {
			m := NewModelForProviders(multi)
			m.width = w
			m.height = 24
			m.active = active
			m.syncActive()
			allRows := m.chipRowsLayout()
			rendered := make(map[int]bool)
			for _, row := range allRows {
				for _, prov := range row.providers {
					rendered[prov] = true
				}
			}
			// Every column of every header row: a hit must be a rendered chip.
			for ri := range allRows {
				for x := 0; x < m.width; x++ {
					idx, ok := m.chipAt(x, ri)
					if ok && !rendered[idx] {
						t.Errorf("width=%d active=%d: chipAt(%d,%d) hit provider %d whose chip is not rendered", w, active, x, ri, idx)
					}
				}
			}
			// Every rendered chip: each of its columns maps back to itself.
			for ri, row := range allRows {
				var col int
				if ri == 0 {
					col = m.row0ChipStart()
				} else {
					col = 1
				}
				for i, v := range row.parts {
					cw := lipgloss.Width(v)
					prov := row.providers[i]
					for x := col; x < col+cw; x++ {
						if idx, ok := m.chipAt(x, ri); !ok || idx != prov {
							t.Errorf("width=%d active=%d: chipAt(%d,%d) = (%d,%v), want (%d,true) across the rendered chip span",
								w, active, x, ri, idx, ok, prov)
						}
					}
					col += cw + 1
				}
			}
		}
	}

	// (d) The single-provider header keeps the legacy layout: byte-identical
	// at every width the legacy row fits (no switcher, no budgeting), and
	// truncated — never wrapping — below that. The legacy natural row is 67
	// content cells + 2 padding = 69 total.
	single := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	single.width = 80
	single.height = 24
	want := single.renderHeader()
	if w := lipgloss.Width(want); w != 69 {
		t.Fatalf("legacy single-provider header width = %d, want the 69-cell baseline this test pins", w)
	}
	for _, w := range widths {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		got := m.renderHeader()
		if w >= 69 && got != want {
			t.Errorf("width=%d: single-provider header changed:\n got: %q\nwant: %q", w, got, want)
		}
		if lipgloss.Width(got) > w {
			t.Errorf("width=%d: single-provider header is %d cells, exceeds %d", w, lipgloss.Width(got), w)
		}
	}
}

func TestMultiProviderHeaderShowsNames(t *testing.T) {
	// Width 120 gives the budget for both full names, so this test pins natural
	// rendering and styling; the width/height tests pin responsive degradation
	// and wrapping at narrower terminals.
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	})
	m.width = 120
	m.height = 24

	if !m.hasSwitcher() {
		t.Fatal("multiple providers must render a provider switcher")
	}
	// At width 120 both chips fit at their full styled widths, so the
	// budgeted layout must equal the unbudgeted rendering exactly.
	layout := m.budgetedChips()
	if len(layout.parts) != 2 || layout.providers[0] != 0 || layout.providers[1] != 1 {
		t.Fatalf("budgetedChips layout = %v, want both providers [0 1] at width 120", layout.providers)
	}
	if layout.parts[0] != m.styles.chipActiveStyle.Render(" acme ") {
		t.Errorf("active chip = %q, want the untruncated active style %q", layout.parts[0], m.styles.chipActiveStyle.Render(" acme "))
	}
	if layout.parts[1] != m.styles.chipInactiveStyle.Render(" anthropic ") {
		t.Errorf("inactive chip = %q, want the untruncated inactive style %q", layout.parts[1], m.styles.chipInactiveStyle.Render(" anthropic "))
	}
	for _, want := range []string{"acme", "anthropic"} {
		if !strings.Contains(m.renderHeader(), want) {
			t.Errorf("header should show both provider names in its chips, missing %q", want)
		}
	}

	// Switching providers updates the highlighted chip.
	col := m.row0ChipStart() + lipgloss.Width(layout.parts[0]) + 1
	m = update(m, tea.MouseClickMsg{X: col, Y: 0})
	layout = m.budgetedChips()
	if layout.parts[0] != m.styles.chipInactiveStyle.Render(" acme ") {
		t.Errorf("after switching, acme chip = %q, want the inactive style", layout.parts[0])
	}
	if layout.parts[1] != m.styles.chipActiveStyle.Render(" anthropic ") {
		t.Errorf("after switching, anthropic chip = %q, want the active style", layout.parts[1])
	}
}

// TestFleetStrip_AggregateObservability pins the one-line fleet strip atop the
// TUI in multi-provider mode (M6/G8).
func TestFleetStrip_AggregateObservability(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "openai", Concurrency: 8},
		{Name: "anthropic", Concurrency: 4},
		{Name: "gemini", Concurrency: 6},
	}
	m := NewModelForProviders(metas)
	m.width = 80
	m.height = 24

	// Update snapshots across providers:
	// - openai: 3 active, 1 queued, circuit breaker CLOSED
	// - anthropic: 2 active, 1 queued, circuit breaker OPEN
	// - gemini: 0 active, 0 queued, circuit breaker CLOSED
	// Total: 5 active · 2 queued · 1 OPEN · busiest: openai
	m = update(m, ProviderUpdate{
		Index: 0,
		Snapshot: metrics.Snapshot{
			Active: 3,
			Queued: 1,
			CircuitBreaker: &metrics.CBStats{
				State: "CLOSED",
			},
		},
	})
	m = update(m, ProviderUpdate{
		Index: 1,
		Snapshot: metrics.Snapshot{
			Active: 2,
			Queued: 1,
			CircuitBreaker: &metrics.CBStats{
				State: "OPEN",
			},
		},
	})
	m = update(m, ProviderUpdate{
		Index: 2,
		Snapshot: metrics.Snapshot{
			Active: 0,
			Queued: 0,
			CircuitBreaker: &metrics.CBStats{
				State: "CLOSED",
			},
		},
	})

	hdr := stripANSI(m.renderHeader())
	// The fleet header now shows the active provider identity with a scroll
	// affordance, followed by aggregate stats (no "Fleet:" prefix).
	wantStats := "5 active · 2 queued · 1 OPEN · busiest: openai"
	if !strings.Contains(hdr, wantStats) {
		t.Fatalf("header missing fleet stats %q; got header: %q", wantStats, hdr)
	}
	wantIdentity := "openai ↕"
	if !strings.Contains(hdr, wantIdentity) {
		t.Fatalf("header missing active provider identity %q; got header: %q", wantIdentity, hdr)
	}

	// Provider switcher chips are rendered on the right side of the same row.
	layout := m.budgetedChips()
	if len(layout.parts) == 0 {
		t.Fatal("budgetedChips should fit chips at 80 cols")
	}
	// Active provider (index 0) is present
	foundActive := slices.Contains(layout.providers, 0)
	if !foundActive {
		t.Error("active provider chip must be present in switcher")
	}
}

// ─── T01: horizontal scrolling on the Network tab ───
