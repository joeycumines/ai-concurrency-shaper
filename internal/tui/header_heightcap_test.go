// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestHeightCapEdgeCases(t *testing.T) {
	heights := []int{4, 5, 6, 8, 10, 15, 20, 24, 30}
	widths := []int{10, 18, 20, 40, 80, 120, 200}
	if isRace {
		heights = []int{5, 8, 24}
		widths = []int{10, 20, 80, 200}
	}
	for n := 1; n <= 15; n++ {
		metas := make([]ProviderMeta, n)
		for i := range metas {
			metas[i] = ProviderMeta{Name: fmt.Sprintf("prov-%d", i+1), Concurrency: 4}
		}
		for _, h := range heights {
			for _, w := range widths {
				actives := []int{0}
				if n > 1 && !isRace {
					actives = append(actives, n-1)
				} else if n > 1 {
					actives = append(actives, n/2)
				}
				for _, active := range actives {
					m := NewModelForProviders(metas)
					m.width = w
					m.height = h
					m.active = active
					m.syncActive()
					rows := m.chipRowsLayout()
					hrc := m.headerRowCount()
					maxRows := max(h-4, 1)
					if hrc > maxRows {
						t.Fatalf("n=%d w=%d h=%d active=%d: hrc %d > maxRows %d", n, w, h, active, hrc, maxRows)
					}
					if len(rows) > maxRows {
						t.Fatalf("n=%d w=%d h=%d active=%d: len(rows) %d > maxRows %d", n, w, h, active, len(rows), maxRows)
					}
					rendered := m.renderHeader()
					renderedLines := strings.Count(rendered, "\n") + 1
					if renderedLines != hrc {
						t.Fatalf("n=%d w=%d h=%d active=%d: renderedLines %d != hrc %d", n, w, h, active, renderedLines, hrc)
					}
					for i, line := range strings.Split(rendered, "\n") {
						if lw := lipgloss.Width(line); lw > w {
							t.Fatalf("n=%d w=%d h=%d active=%d: line %d width %d > %d", n, w, h, active, i, lw, w)
						}
					}
					if len(rows) > 0 {
						found := false
						for _, r := range rows {
							for _, p := range r.providers {
								if p == active {
									found = true
								}
							}
						}
						if !found {
							t.Fatalf("n=%d w=%d h=%d active=%d: active chip dropped", n, w, h, active)
						}
					}
					for y := len(rows); y < len(rows)+2; y++ {
						for x := range w {
							if _, ok := m.chipAt(x, y); ok {
								t.Fatalf("n=%d w=%d h=%d active=%d: chipAt hit beyond rows at (%d,%d)", n, w, h, active, x, y)
							}
						}
					}
					if w <= 18 && len(rows) > 0 && len(rows[0].providers) == 0 {
						for x := range w {
							if _, ok := m.chipAt(x, 0); ok {
								t.Fatalf("n=%d w=%d h=%d active=%d: empty row0 hit at x=%d", n, w, h, active, x)
							}
						}
					}
				}
			}
		}
	}
}

func TestHelpOverlayHidesMouseSwitcherWhenHeaderElidesChips(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
	})
	m.width = 20
	m.height = 5

	if !m.hasSwitcher() {
		t.Fatal("multiple providers must retain keyboard provider switching")
	}
	if rows := m.chipRowsLayout(); len(rows) != 0 {
		t.Fatalf("height-capped header rendered chip rows %v, want switcher elided", rows)
	}
	help := m.renderHelpOverlay()
	if !strings.Contains(help, "Tab/Shift+Tab") {
		t.Fatalf("height-capped help should retain keyboard provider switching, got:\n%s", help)
	}
	for _, forbidden := range []string{"Click name ↕", "Wheel"} {
		if strings.Contains(help, forbidden) {
			t.Fatalf("height-capped help must not advertise hidden mouse control %q, got:\n%s", forbidden, help)
		}
	}
}

func TestIdentityHitWidthStopsAtTruncatedFleetBody(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	})
	m.width = 20
	m.height = 24
	m.active = 0
	m.syncActive()

	visibleBodyWidth := lipgloss.Width(m.headerBody(true))
	if got := m.identityWidth(); got > visibleBodyWidth {
		t.Fatalf("identity hit width=%d exceeds truncated body width=%d", got, visibleBodyWidth)
	}
	if got := m.identityWidth(); got != visibleBodyWidth {
		t.Fatalf("identity hit width=%d, want visible truncated body width=%d", got, visibleBodyWidth)
	}
	lastCell := m.width - 1
	if lastCell < 1+m.identityWidth() {
		t.Fatalf("test coordinate x=%d is still within identity width=%d", lastCell, m.identityWidth())
	}
	updated := update(m, tea.MouseClickMsg{X: lastCell, Y: 0})
	if updated.active != m.active {
		t.Fatalf("click at invisible identity/stat cell x=%d changed active provider from %d to %d", lastCell, m.active, updated.active)
	}
}

func TestHeaderNarrowWidthsNeverExceedTerminal(t *testing.T) {
	for _, width := range []int{1, 2, 3, 4} {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = width
		m.height = 24
		for lineNo, line := range strings.Split(m.renderHeader(), "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("width=%d header line %d has width %d: %q", width, lineNo, got, stripANSI(line))
			}
		}
	}
}

func TestSingleNamedProviderKeepsRowZeroChipAtNormalWidth(t *testing.T) {
	for _, width := range []int{7, 8, 40, 60, 80} {
		m := NewModelForProviders([]ProviderMeta{{Name: "acme", Concurrency: 4}})
		m.width = width
		m.height = 24

		rows := m.chipRowsLayout()
		if width == 7 {
			if len(rows) != 0 {
				t.Fatalf("width=%d single named provider rows=%v, want safe switcher elision", width, rows)
			}
			if header := stripANSI(m.renderHeader()); !strings.Contains(header, "acme") {
				t.Fatalf("width=%d elided single named header lost active provider identity: %q", width, header)
			}
			if renderedWidth := lipgloss.Width(m.renderHeader()); renderedWidth != m.width {
				t.Fatalf("width=%d elided single named header width=%d, want %d", width, renderedWidth, m.width)
			}
			continue
		}
		if len(rows) != 1 || len(rows[0].providers) != 1 || rows[0].providers[0] != 0 {
			t.Fatalf("width=%d single named provider rows=%v, want provider 0 on row 0", width, rows)
		}
		start := m.fixedBodyWidth() + 2
		if got, ok := m.chipAt(start, 0); !ok || got != 0 {
			t.Fatalf("width=%d chipAt(%d,0)=(%d,%v), want single named provider 0", width, start, got, ok)
		}
		if renderedWidth := lipgloss.Width(m.renderHeader()); renderedWidth != m.width {
			t.Fatalf("width=%d single named header width=%d, want %d", width, renderedWidth, m.width)
		}
	}
}

func TestUnicodeChipFloorShowsLeadingDoubleCellGrapheme(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Name: "🔥🚀", Concurrency: 4}})
	m.width = 12
	m.height = 24
	rows := m.chipRowsLayout()
	if len(rows) != 1 || len(rows[0].parts) != 1 {
		t.Fatalf("unicode floor layout=%v, want one visible row-0 chip", rows)
	}
	part := rows[0].parts[0]
	if !strings.Contains(stripANSI(part), "🔥") {
		t.Fatalf("unicode floor chip %q lost its leading grapheme", stripANSI(part))
	}
	if width := lipgloss.Width(part); width < chipFloor {
		t.Fatalf("unicode floor chip width=%d, want >= %d", width, chipFloor)
	}
}
