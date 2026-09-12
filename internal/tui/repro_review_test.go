package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Repro A: header render vs headerRowCount cap divergence
func TestRepro_HeaderCapDivergence(t *testing.T) {
	metas := make([]ProviderMeta, 15)
	for i := range metas {
		metas[i] = ProviderMeta{Name: fmt.Sprintf("provider-long-name-%c", 'a'+i), Concurrency: 4}
	}
	cases := []struct{ w, h int }{{20, 8}, {30, 8}, {20, 10}}
	for _, c := range cases {
		m := NewModelForProviders(metas)
		m.width = c.w
		m.height = c.h
		hrc := m.headerRowCount()
		rendered := m.renderHeader()
		lines := strings.Count(rendered, "\n") + 1
		// headerRowCount is defined as number of rendered header lines.
		if lines != hrc {
			t.Logf("DIVERGENCE w=%d h=%d: headerRowCount=%d but renderHeader lines=%d", c.w, c.h, hrc, lines)
			t.Logf("  chipRowsLayout len=%d maxRows=%d", len(m.chipRowsLayout()), max(c.h-4, 1))
		} else {
			t.Logf("OK w=%d h=%d: lines=%d hrc=%d", c.w, c.h, lines, hrc)
		}
		if lines > hrc {
			t.Errorf("w=%d h=%d: rendered header lines %d exceeds headerRowCount %d", c.w, c.h, lines, hrc)
		}
		// Also check tab bar row mismatch: chipAt vs headerRowCount classification
		layoutLen := len(m.chipRowsLayout())
		if layoutLen > hrc {
			// visual row hrc is a chip row but mouse treats it as tab bar
			// Demonstrate misclassification
			y := hrc // would be tabBarRow
			if y < layoutLen {
				if idx, ok := m.chipAt(5, y); ok {
					t.Errorf("w=%d h=%d: chipAt(5,%d) hits chip %d but mouse handler treats Y=%d as tab bar (headerRowCount=%d)", c.w, c.h, y, idx, y, hrc)
				}
			}
		}
	}
}

// Repro B: row 0 budget fallback overflow
func TestRepro_Row0FallbackOverflow(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	}
	for _, w := range []int{20, 25, 30, 40} {
		m := NewModelForProviders(metas)
		m.width = w
		m.height = 24
		// Check each line width
		rendered := m.renderHeader()
		for i, line := range strings.Split(rendered, "\n") {
			lw := lipgloss.Width(line)
			if lw > w {
				t.Errorf("w=%d: header row %d width %d exceeds %d => row0 fallback overflow", w, i, lw, w)
				t.Logf("  row %d: %q width %d", i, stripANSI(line), lw)
			}
		}
		// Detect row0 concatenation case: row0Budget < chipFloor but row0Chips non-empty
		row0Budget := m.width - 2 - lipgloss.Width(m.headerBody(true)) - 1
		fullBudget := m.width - 2
		rows := m.chipRowsLayout()
		if row0Budget < chipFloor && fullBudget >= chipFloor && len(rows) > 0 && len(rows[0].parts) > 0 {
			bodyW := lipgloss.Width(m.headerBody(true))
			chipsW := lipgloss.Width(strings.Join(rows[0].parts, " "))
			combined := bodyW + 1 + chipsW + 2 // +2 header padding
			if combined > w {
				t.Errorf("w=%d: row0 fallback packs %d chip width onto body width %d (budget %d) -> combined %d > %d", w, chipsW, bodyW, row0Budget, combined, w)
			}
		}
	}
}

// Repro C: active chip force-append overflow
func TestRepro_ActiveAppendOverflow(t *testing.T) {
	// Need many providers and narrow width to trigger hasActive false path
	// Current greedy packs in order so active last may be on last row already,
	// but we can still check floorCost invariant
	metas := make([]ProviderMeta, 10)
	for i := range metas {
		metas[i] = ProviderMeta{Name: fmt.Sprintf("prov-%d-long", i), Concurrency: 4}
	}
	for _, w := range []int{20, 25, 30} {
		for active := range metas {
			m := NewModelForProviders(metas)
			m.width = w
			m.height = 24
			m.active = active
			m.syncActive()
			rows := m.chipRowsLayout()
			if len(rows) == 0 {
				continue
			}
			for ri, r := range rows {
				// recompute floorCost
				fc := len(r.providers)*chipFloor + max(len(r.providers)-1, 0)
				budget := 0
				if ri == 0 {
					budget = m.width - 2 - lipgloss.Width(m.headerBody(true)) - 1
					if budget < chipFloor {
						budget = m.width - 2
					}
				} else {
					budget = m.width - 2
				}
				// Also compare actual rendered width
				renderedW := lipgloss.Width(strings.Join(r.parts, " "))
				if fc > budget {
					t.Errorf("w=%d active=%d row=%d: floorCost %d > budget %d (providers %v)", w, active, ri, fc, budget, r.providers)
				}
				if renderedW > budget && renderedW > m.width-2 {
					t.Errorf("w=%d active=%d row=%d: rendered width %d > budget %d", w, active, ri, renderedW, budget)
				}
				// Also overall header line check
				hdr := m.renderHeader()
				for i, line := range strings.Split(hdr, "\n") {
					if lipgloss.Width(line) > w {
						t.Errorf("w=%d active=%d: header line %d width %d > %d", w, active, i, lipgloss.Width(line), w)
					}
				}
			}
		}
	}
}

// Repro D: wheel hit-test off-by-one due to PaddingLeft 1
func TestRepro_WheelPadding(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	})
	m.width = 80
	m.height = 24
	m.active = 0
	m.syncActive()
	idW := m.identityWidth()
	if idW == 0 {
		t.Fatal("idW zero")
	}
	// mx=0 is headerStyle left padding space, should NOT cycle
	m2 := m
	m2 = update(m2, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 0, Y: 0})
	if m2.active != 0 {
		t.Errorf("mx=0 should NOT cycle (padding), but active changed to %d", m2.active)
		t.Logf("  idW=%d, mx=0 hit incorrectly", idW)
	}
	// mx=1 is first char of identity (" "), should cycle
	m3 := m
	m3 = update(m3, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 1, Y: 0})
	if m3.active != 1 {
		t.Errorf("mx=1 should cycle, but active %d want 1 idW=%d", m3.active, idW)
	}
	// mx = 1+idW-1 = idW is last char of identity (arrow), should still cycle? Actually range [1,1+idW) so idW is last inclusive
	m4 := m
	m4 = update(m4, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 1 + idW - 1, Y: 0})
	if m4.active != 1 {
		t.Errorf("mx=%d (last identity column) should cycle, got active %d", 1+idW-1, m4.active)
	}
	// mx = 1+idW is first column after identity (" "), should NOT cycle
	m5 := m
	m5 = update(m5, tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 1 + idW, Y: 0})
	if m5.active != 0 {
		t.Errorf("mx=%d (after identity) should NOT cycle, got %d", 1+idW, m5.active)
	}
	t.Logf("idW=%d padding check done", idW)
}

// Repro E: palette vertical overflow (border)
func TestRepro_PaletteVerticalOverflow(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	})
	testHeights := []int{8, 10, 24}
	for _, h := range testHeights {
		m.width = 80
		m.height = h
		m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})
		if m.mode != modePalette {
			t.Fatalf("palette not opened")
		}
		rendered := m.renderCommandPalette()
		contentLines := countContentLines(rendered)
		vr := m.visibleRows()
		t.Logf("h=%d headerRows=%d visibleRows=%d palette lines=%d", h, m.headerRowCount(), vr, contentLines)
		if contentLines > vr {
			t.Errorf("h=%d: palette lines %d exceeds visibleRows %d (border not accounted)", h, contentLines, vr)
		}
		// close for next iter
		m = update(m, tea.KeyPressMsg{Code: tea.KeyEscape, Text: "esc"})
	}
}

// Repro F: horizontal: palette frame width <= terminal width
func TestRepro_PaletteHorizontal(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Name: "acme", Concurrency: 4}})
	for _, w := range []int{10, 15, 20, 80} {
		m.width = w
		m.height = 24
		m = update(m, tea.KeyPressMsg{Text: "ctrl+k"})
		rendered := m.renderCommandPalette()
		for i, line := range strings.Split(rendered, "\n") {
			if line == "" {
				continue
			}
			lw := lipgloss.Width(line)
			if lw > w {
				t.Errorf("w=%d: palette line %d width %d > %d", w, i, lw, w)
				t.Logf("  line %d: %q", i, stripANSI(line))
			}
		}
		m = update(m, tea.KeyPressMsg{Code: tea.KeyEscape, Text: "esc"})
	}
}
