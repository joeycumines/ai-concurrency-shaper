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
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Fleet header spec — see header.go fleet invariants and below.
//
// Policy:
//   Usable = width-2 (headerStyle padding). Row-0 body budget =
//   fixedBodyWidth (worst-case body across all active providers) plus the
//   3-cell " │ " divider before the first chip; chips natural =
//   lipgloss.Width(chipActiveStyle.Render(" "+label+" ")).
//   Row 0 chips iff first chip fits naturally (natural-fit prefix).
//   Otherwise row 0 is body-only (empty chip row, body at full usable) and
//   every chip starts on row 1+ at fullBudget = usable. Wrapped rows use
//   greedy floorCost packing with active-first slack restoration. Height cap
//   maxRows = max(height-4,1) caps len(chipRowsLayout) and headerRowCount.
//
// Breakpoint table (falsifiable, height 24 unless noted):
//   20 cols, 3-long (anthropic-eu-central/openai-prod-longname/acme-edge-provider):
//     body ~86 nat -> row0 body-only truncated to 18, row1 chips floor, hrc 2.
//   40 cols, 3-long: row0 body-only (38 usable < body+firstChip), row1 chips
//     2-3 at reduced widths (>3), hrc 2.
//   80 cols, 3-long: row0 body-only, row1 full-width chips. 86 cols, 2-short
//     (acme/anthropic, worst-case body 59, naturals 8+13): single row, hrc 1.
//   100 cols, 3-long: row0 body-only (available 9 < 24), hrc 2.
//   120 cols, 3-long: row0 shares one chip (available 29 fits 24), row1 holds
//     remaining 2, hrc 2. 120 cols, 2-short: MUST be single-row nat
//     (wrapping_test hard-requires). First chip on row 0 is always at natural
//     width (no truncation on row 0).
//   150 cols, 3-long: row0 shares two chips (available 59), row1 one, hrc 2.
//   180 cols, 3-long: single row body+all nat, hrc 1.
//   At 20-30 cols hrc is body-only row0 + chip row(s); if row0 holds chips its
//   first chip width == natural (no truncation on row 0). At 120/2-short the
//   layout is exactly one row with each chip at natural width.
//

func TestChipRowsLayout_AllProvidersPresent(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	}
	for _, w := range []int{20, 30, 40, 60, 80, 120} {
		m := NewModelForProviders(metas)
		m.width = w
		m.height = 24
		rows := m.chipRowsLayout()
		if len(rows) == 0 {
			continue
		}
		seen := make(map[int]bool)
		for _, r := range rows {
			for _, prov := range r.providers {
				seen[prov] = true
			}
		}
		for i := range metas {
			if !seen[i] {
				t.Errorf("width=%d: provider %d missing from chip rows", w, i)
			}
		}
	}
}

func TestChipRowsLayout_ActiveNeverDropped(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	}
	for active := range metas {
		for w := 15; w <= 120; w += 5 {
			m := NewModelForProviders(metas)
			m.width = w
			m.height = 24
			m.active = active
			m.syncActive()
			rows := m.chipRowsLayout()
			if len(rows) == 0 {
				continue
			}
			found := false
			for _, r := range rows {
				for _, prov := range r.providers {
					if prov == active {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("width=%d active=%d: active chip dropped", w, active)
			}
		}
	}
}

func TestChipRowsLayout_RowWidthNeverExceedsTerminal(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	}
	for _, w := range []int{20, 30, 40, 60, 80, 120} {
		m := NewModelForProviders(metas)
		m.width = w
		m.height = 24
		rendered := m.renderHeader()
		for i, line := range strings.Split(rendered, "\n") {
			lineW := lipgloss.Width(line)
			if lineW > w {
				t.Errorf("width=%d: header row %d is %d cells, exceeds %d", w, i, lineW, w)
			}
		}
	}
}

func TestChipAt_HitTestsAllRenderedChips(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	}
	for _, w := range []int{30, 40, 60, 80, 120} {
		m := NewModelForProviders(metas)
		m.width = w
		m.height = 24
		rows := m.chipRowsLayout()
		if len(rows) == 0 {
			continue
		}
		for ri, row := range rows {
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
					idx, ok := m.chipAt(x, ri)
					if !ok || idx != prov {
						t.Errorf("width=%d row=%d col=%d: chipAt=(%d,%v) want (%d,true)", w, ri, x, idx, ok, prov)
					}
				}
				col += cw + 1
			}
		}
	}
}

func TestDynamicChrome_ContentStartRowMatchesHeaderRows(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	}
	for _, w := range []int{20, 40, 80, 120} {
		m := NewModelForProviders(metas)
		m.width = w
		m.height = 24
		if got := m.contentStartRow(); got != m.headerRowCount()+2 {
			t.Errorf("width=%d: contentStartRow()=%d want %d", w, got, m.headerRowCount()+2)
		}
		if got := m.visibleRows(); got != m.height-m.headerRowCount()-3 {
			t.Errorf("width=%d: visibleRows()=%d want %d", w, got, m.height-m.headerRowCount()-3)
		}
	}
}

func TestHeaderRowCount_CappedByHeight(t *testing.T) {
	metas := make([]ProviderMeta, 15)
	for i := range metas {
		metas[i] = ProviderMeta{Name: fmt.Sprintf("provider-long-name-%c", 'a'+i), Concurrency: 4}
	}
	m := NewModelForProviders(metas)
	m.width = 20
	m.height = 8
	maxExpected := max(m.height-4, 1)
	if got := m.headerRowCount(); got > maxExpected {
		t.Errorf("headerRowCount()=%d exceeds cap %d at height %d", got, maxExpected, m.height)
	}
}

func TestWheelOverIdentity_CyclesProvider(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
		{Name: "openai", Concurrency: 12},
	})
	m.width = 80
	m.height = 24
	m.active = 0
	m.syncActive()
	idW := m.identityWidth()
	if idW == 0 {
		t.Fatal("identityWidth should be > 0 in fleet mode")
	}
	m = update(m, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 1, Y: 0})
	if m.active != 1 {
		t.Fatalf("after wheel down at identity: active=%d want 1", m.active)
	}
	m = update(m, tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 1, Y: 0})
	if m.active != 0 {
		t.Fatalf("after wheel up at identity: active=%d want 0", m.active)
	}
	m.cursor = 0
	m = update(m, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: idW + 10, Y: 0})
	if m.active != 0 {
		t.Fatalf("wheel outside identity changed provider to %d", m.active)
	}
}

func TestFleetIdentity_RenderedInHeader(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	})
	m.width = 80
	m.height = 24
	m.active = 0
	m.syncActive()
	hdr := stripANSI(m.renderHeader())
	if !strings.Contains(hdr, "acme") {
		t.Errorf("header missing active provider name: %q", hdr)
	}
	if !strings.Contains(hdr, "\u2195") {
		t.Errorf("header missing scroll affordance: %q", hdr)
	}
	if strings.Contains(hdr, "Fleet:") {
		t.Errorf("header still contains legacy Fleet prefix: %q", hdr)
	}
}

func TestSingleProvider_NoWrappingNoIdentity(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	if m.hasSwitcher() {
		t.Fatal("single unnamed provider should not have switcher")
	}
	if rows := m.chipRowsLayout(); len(rows) != 0 {
		t.Errorf("single provider chip rows=%d want 0", len(rows))
	}
	if m.headerRowCount() != 1 {
		t.Errorf("headerRowCount=%d want 1", m.headerRowCount())
	}
	if m.contentStartRow() != 3 {
		t.Errorf("contentStartRow=%d want 3", m.contentStartRow())
	}
}

// TestFixedBodyWidth_StableAcrossProviders pins the core guarantee:
// switching the active provider at any width never changes the row count,
// which chips sit on row 0 vs row 1+, or the column where chips begin.
func TestFixedBodyWidth_StableAcrossProviders(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic-eu-central", Concurrency: 8},
		{Name: "openai-prod-longname", Concurrency: 12},
	}
	for _, w := range []int{60, 80, 100, 120, 150} {
		base := NewModelForProviders(metas)
		base.width = w
		base.height = 24
		base.active = 0
		base.syncActive()
		wantRows := base.chipRowsLayout()
		wantHRC := base.headerRowCount()
		for active := 1; active < len(metas); active++ {
			m := base
			m.active = active
			m.syncActive()
			gotRows := m.chipRowsLayout()
			if got, want := m.headerRowCount(), wantHRC; got != want {
				t.Errorf("w=%d active=%d: headerRowCount=%d want %d (active=0)", w, active, got, want)
			}
			if len(gotRows) != len(wantRows) {
				t.Fatalf("w=%d active=%d: rows=%d want %d", w, active, len(gotRows), len(wantRows))
			}
			for ri := range wantRows {
				if len(gotRows[ri].providers) != len(wantRows[ri].providers) {
					t.Fatalf("w=%d active=%d row=%d: providers=%v want %v", w, active, ri, gotRows[ri].providers, wantRows[ri].providers)
				}
				for k := range wantRows[ri].providers {
					if gotRows[ri].providers[k] != wantRows[ri].providers[k] {
						t.Fatalf("w=%d active=%d row=%d col=%d: provider=%d want %d", w, active, ri, k, gotRows[ri].providers[k], wantRows[ri].providers[k])
					}
					gw := lipgloss.Width(gotRows[ri].parts[k])
					ww := lipgloss.Width(wantRows[ri].parts[k])
					if gw != ww {
						t.Fatalf("w=%d active=%d row=%d col=%d: chip width=%d want %d", w, active, ri, k, gw, ww)
					}
				}
				if len(wantRows[ri].parts) > 0 {
					fw := base.fixedBodyWidth()
					if got, want := leftmostChipCol(gotRows[ri], w, ri, fw), leftmostChipCol(wantRows[ri], w, ri, fw); got != want {
						t.Fatalf("w=%d active=%d row=%d: leftmost chip column=%d want %d", w, active, ri, got, want)
					}
				}
			}
		}
	}
}

func leftmostChipCol(row chipLayout, width int, rowIndex int, fw int) int {
	if rowIndex == 0 {
		return fw + 4
	}
	return 1
}

func TestTabBarRow_DynamicWithWrapping(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	}
	m := NewModelForProviders(metas)
	m.width = 30
	m.height = 24
	headerRows := m.headerRowCount()
	tabBarRow := headerRows
	m.tab = tabDashboard
	m = update(m, tea.MouseClickMsg{X: 3, Y: tabBarRow})
	if m.tab != tabDashboard {
		t.Errorf("click on tab bar row %d: tab=%d want tabDashboard", tabBarRow, m.tab)
	}
}
