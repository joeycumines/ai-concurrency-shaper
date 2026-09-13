// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// TestFleetIdentityClick_GeometryLocks exhausts responsive widths and
// active indices, locking the invariants that the blueprint declares sacred:
// header rows never exceed terminal width, active chip never dropped,
// chipAt hit-tests every rendered chip pixel, and identity clicks outside the
// canonical region are strict no-ops. This test is the hostile lock that
// Task 3 requires before the Rule-of-Two gate.
func TestFleetIdentityClick_GeometryLocks(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	}
	widths := []int{20, 30, 40, 60, 80, 120}
	for _, w := range widths {
		for active := range metas {
			m := NewModelForProviders(metas)
			m.width = w
			m.height = 24
			m.active = active
			m.syncActive()

			// (a1) Every header row width <= w.
			rendered := m.renderHeader()
			for i, line := range strings.Split(rendered, "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Errorf("w=%d active=%d: header row %d width %d exceeds %d\n%s", w, active, i, lw, w, stripANSI(line))
				}
			}

			// (a2) headerRowCount / content geometry lockstep.
			rows := m.chipRowsLayout()
			hrc := m.headerRowCount()
			// chipRowsLayout is capped to max(height-4,1); headerRowCount
			// wraps that same cap, so len(rows) must equal hrc when
			// hasSwitcher true unless rows==0 (elided). When elided,
			// hrc is 1 (single row body-only).
			if m.hasSwitcher() && len(rows) > 0 {
				if hrc != len(rows) {
					t.Errorf("w=%d active=%d: headerRowCount=%d want len(chipRowsLayout)=%d", w, active, hrc, len(rows))
				}
			}
			if got := m.contentStartRow(); got != hrc+2 {
				t.Errorf("w=%d active=%d: contentStartRow=%d want %d", w, active, got, hrc+2)
			}
			if got := m.visibleRows(); got != m.height-hrc-3 {
				t.Errorf("w=%d active=%d: visibleRows=%d want %d", w, active, got, m.height-hrc-3)
			}

			// (a3) Active chip never dropped (when not elided).
			if len(rows) > 0 {
				found := false
				for _, r := range rows {
					if slices.Contains(r.providers, active) {
						found = true
					}
				}
				if !found {
					t.Errorf("w=%d active=%d: active chip dropped (rows=%v)", w, active, rows)
				}
			}

			// (a4) chipAt hit-tests every rendered chip cell exactly.
			for ri, row := range rows {
				right := m.width - 2
				for i, part := range slices.Backward(row.parts) {
					cw := lipgloss.Width(part)
					prov := row.providers[i]
					for x := right - cw + 1; x <= right; x++ {
						idx, ok := m.chipAt(x, ri)
						if !ok || idx != prov {
							t.Errorf("w=%d active=%d row=%d x=%d: chipAt=(%d,%v) want (%d,true)", w, active, ri, x, idx, ok, prov)
						}
					}
					right -= cw + 1
				}
			}

			// (a5) Identity click geometry no-ops.
			// padding col 0, past col 1+idW, row 1, and headerRowCount row must not cycle.
			idW := m.identityWidth()
			if idW == 0 {
				t.Fatalf("w=%d active=%d: identityWidth==0 in fleet mode", w, active)
			}
			base := NewModelForProviders(metas)
			base.width = w
			base.height = 24
			base.active = active
			base.syncActive()

			// padding
			m2 := update(base, tea.MouseClickMsg{X: 0, Y: 0})
			if m2.active != active {
				t.Errorf("w=%d active=%d: click at padding X=0 must not cycle, got %d", w, active, m2.active)
			}
			// just past identity
			m2 = update(base, tea.MouseClickMsg{X: 1 + idW, Y: 0})
			if m2.active != active {
				t.Errorf("w=%d active=%d: click at X=%d (past identity) must not cycle, got %d", w, active, 1+idW, m2.active)
			}
			// row 1 must not cycle identity — whitelist chip hits (covers both
			// multi-row chip row and single-row tab bar at Y=1)
			m2 = update(base, tea.MouseClickMsg{X: 1, Y: 1})
			if m2.active != active {
				if _, ok := base.chipAt(1, 1); !ok {
					t.Errorf("w=%d active=%d: click at Y=1 X=1 must not cycle, got %d", w, active, m2.active)
				}
			}
			// tab bar row (headerRowCount) must never cycle provider — unconditional
			// even when hrc==1 (single-row header: Y=1 is the tab bar)
			m2 = update(base, tea.MouseClickMsg{X: 1, Y: hrc})
			if m2.active != active {
				t.Errorf("w=%d active=%d: click at Y=%d (tab bar) must not cycle provider, got %d", w, active, hrc, m2.active)
			}
			// left identity column must never bleed into chip region
			if idx, ok := base.chipAt(1, 0); ok {
				t.Errorf("w=%d active=%d: chipAt at identity column X=1 must not hit chip %d", w, active, idx)
			}
		}
	}
}

// TestFleetIdentityClick_NarrowAndHeightCaps verifies the fleet header's
// behaviour under extreme narrow width and tight height, and the single-
// provider byte-identical brand invariant.
func TestFleetIdentityClick_NarrowAndHeightCaps(t *testing.T) {
	// Narrow + height cap: 15 providers, w=20, h=8
	metas := make([]ProviderMeta, 15)
	for i := range metas {
		metas[i] = ProviderMeta{Name: fmt.Sprintf("provider-long-name-%c", 'a'+i), Concurrency: 4}
	}
	m := NewModelForProviders(metas)
	m.width = 20
	m.height = 8
	maxRows := max(m.height-4, 1)
	if got := m.headerRowCount(); got > maxRows {
		t.Fatalf("headerRowCount=%d exceeds cap %d at 20x8", got, maxRows)
	}
	for line := range strings.SplitSeq(m.renderHeader(), "\n") {
		if lw := lipgloss.Width(line); lw > m.width {
			t.Errorf("20x8: header line width %d exceeds %d: %q", lw, m.width, stripANSI(line))
		}
	}
	// Active chip must stay present even when caps truncate rows.
	for active := range metas {
		m.active = active
		m.syncActive()
		rows := m.chipRowsLayout()
		if len(rows) == 0 {
			continue
		}
		found := false
		for _, r := range rows {
			if slices.Contains(r.providers, active) {
				found = true
			}
		}
		if !found {
			t.Errorf("20x8 active=%d: active chip dropped", active)
		}
	}
	// Identity hit on fleet narrow: left click still cycles, everything else not.
	m = NewModelForProviders(metas[:3])
	m.width = 20
	m.height = 8
	m.active = 0
	m.syncActive()
	idW := m.identityWidth()
	m2 := update(m, tea.MouseClickMsg{X: 1, Y: 0})
	if m2.active == 0 && idW > 0 {
		t.Errorf("20x8: identity click at X=1 must cycle in fleet mode")
	}
	// padding / past / Y=1 no-ops still hold
	for _, tc := range []struct{ x, y int }{{0, 0}, {1 + idW, 0}, {1, 1}} {
		base := NewModelForProviders(metas[:3])
		base.width = 20
		base.height = 8
		base.active = 0
		base.syncActive()
		if _, ok := base.chipAt(tc.x, tc.y); ok {
			continue // chip hit takes precedence
		}
		// For identity region checks, skip the actual identity area
		if tc.y == 0 && tc.x >= 1 && tc.x < 1+idW {
			continue
		}
		m2 := update(base, tea.MouseClickMsg{X: tc.x, Y: tc.y})
		if m2.active != 0 {
			t.Errorf("20x8: click at %d,%d must not cycle, got %d", tc.x, tc.y, m2.active)
		}
	}
}

// TestFleetIdentityClick_Precedence locks the mouse hit precedence:
// chipAt (right-aligned) retains priority over left identity; a chip click
// at the extreme right must hit the chip, not the identity, and never bleed
// into the identity column. Also verifies wheel and Tab paths stay green.
func TestFleetIdentityClick_Precedence(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
		{Name: "openai", Concurrency: 12},
	})
	m.width = 80
	m.height = 24
	m.active = 0
	m.syncActive()

	// Rightmost column must be a chip, not identity.
	right := m.width - 2
	layout := m.budgetedChips()
	if len(layout.parts) == 0 {
		t.Fatalf("no chips at 80 cols")
	}
	// ChipAt at right edge must hit.
	if _, ok := m.chipAt(right, 0); !ok {
		t.Fatalf("chipAt at rightmost %d must hit", right)
	}
	// Identity width must not extend to chip region: chipAt at identity col must miss.
	if _, ok := m.chipAt(1, 0); ok {
		t.Fatalf("chipAt at identity column 1 must not hit chip")
	}

	// Click at right hits chip and switches — lock chip precedence hard.
	m2 := update(m, tea.MouseClickMsg{X: right, Y: 0})
	if hitIdx, ok := m.chipAt(right, 0); ok {
		if m2.active != hitIdx {
			t.Errorf("click at rightmost chip X=%d must switch to provider %d (hit chipAt), got active %d layout providers %v", right, hitIdx, m2.active, layout.providers)
		}
		if m2.active == 0 {
			t.Errorf("right chip click must switch away from 0, got 0 hit provider %d layout %v", hitIdx, layout.providers)
		}
	} else {
		t.Fatalf("chipAt at rightmost %d must hit chip (precondition for precedence lock)", right)
	}
	// Identity click cycles forward independently.
	m.active = 0
	m.syncActive()
	m2 = update(m, tea.MouseClickMsg{X: 1, Y: 0})
	if m2.active != 1 {
		t.Fatalf("identity click must cycle to 1, got %d", m2.active)
	}
	// Wheel path must still cycle.
	m.active = 0
	m.syncActive()
	m2 = update(m, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 1, Y: 0})
	if m2.active != 1 {
		t.Fatalf("wheel down at identity must cycle to 1, got %d", m2.active)
	}
	m3 := update(m2, tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 1, Y: 0})
	if m3.active != 0 {
		t.Fatalf("wheel up must cycle back to 0, got %d", m3.active)
	}
	// Tab path must still wrap.
	m.active = 0
	m.syncActive()
	m2 = update(m, tea.KeyPressMsg{Code: tea.KeyTab})
	if m2.active != 1 {
		t.Fatalf("Tab must cycle to 1, got %d", m2.active)
	}
	m2 = update(m2, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m2.active != 0 {
		t.Fatalf("Shift+Tab must cycle back to 0, got %d", m2.active)
	}
}

// TestFleetIdentityClick_SingleProviderInvariant pins the single unnamed
// provider legacy brand: header byte-identical at >=69 cells, no switcher,
// no identityWidth, no cycling.
func TestFleetIdentityClick_SingleProviderInvariant(t *testing.T) {
	single := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	single.width = 80
	single.height = 24
	if single.hasSwitcher() {
		t.Fatalf("single unnamed must not have switcher")
	}
	if single.identityWidth() != 0 {
		t.Fatalf("single unnamed identityWidth=%d want 0", single.identityWidth())
	}
	want := single.renderHeader()
	if lw := lipgloss.Width(want); lw != 69 {
		t.Fatalf("single header width=%d want 69 baseline", lw)
	}
	for _, w := range []int{80, 120} {
		m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
		m.width = w
		m.height = 24
		if got := m.renderHeader(); got != want {
			t.Errorf("w=%d: single header changed:\n got %q\nwant %q", w, got, want)
		}
	}
	// No cycling on any identity-like coordinate.
	for _, tc := range []struct{ x, y int }{{1, 0}, {0, 0}, {10, 0}, {1, 1}} {
		m2 := update(single, tea.MouseClickMsg{X: tc.x, Y: tc.y})
		if m2.active != 0 {
			t.Errorf("single provider click at %d,%d must not cycle, got %d", tc.x, tc.y, m2.active)
		}
	}
	// Wheel must not cycle either.
	m2 := update(single, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 1, Y: 0})
	if m2.active != 0 {
		t.Errorf("single provider wheel at identity must not cycle, got %d", m2.active)
	}
}
