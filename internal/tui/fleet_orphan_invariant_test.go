// Copyright (C) 2026 Joseph Cumines
//
// Fleet header orphan invariant: row 0 never holds a truncated first chip.
// If row 0 holds chips, the first chip is at its natural width; otherwise
// row 0 is empty (body-only) and every chip starts on row 1+ at full width.
// This directly pins the fix for narrow terminals leaving a single-letter
// orphan on row 0.

package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestFleetFirstChipNotOrphaned(t *testing.T) {
	fleets := []struct {
		name  string
		metas []ProviderMeta
	}{
		{"3-long", []ProviderMeta{
			{Name: "anthropic-eu-central", Concurrency: 4},
			{Name: "openai-prod-longname", Concurrency: 8},
			{Name: "acme-edge-provider", Concurrency: 12},
		}},
		{"2-short", []ProviderMeta{
			{Name: "acme", Concurrency: 4},
			{Name: "anthropic", Concurrency: 8},
		}},
		{"5-mixed", []ProviderMeta{
			{Name: "acme", Concurrency: 4},
			{Name: "anthropic-eu-central", Concurrency: 8},
			{Name: "openai", Concurrency: 12},
			{Name: "gemini-long-model-name", Concurrency: 6},
			{Name: "edge", Concurrency: 4},
		}},
		{"unicode", []ProviderMeta{
			{Name: "模型-α", Concurrency: 4},
			{Name: "acme-🪄-edge", Concurrency: 8},
			{Name: "anthropic-日本語", Concurrency: 12},
		}},
	}
	widths := []int{18, 20, 24, 30, 40, 60, 80, 100, 120, 150, 180}
	heights := []int{5, 8, 24}
	if isRace {
		widths = []int{20, 40, 80, 120, 150, 180}
		heights = []int{5, 24}
	}

	for _, fleet := range fleets {
		for _, h := range heights {
			for _, w := range widths {
				for active := range fleet.metas {
					m := NewModelForProviders(fleet.metas)
					m.width = w
					m.height = h
					m.active = active
					m.syncActive()
					rows := m.chipRowsLayout()
					if len(rows) == 0 {
						maxRows := max(h-4, 1)
						if fleet.name == "3-long" && h >= 8 && w >= 18 && maxRows > 1 {
							if m.width-2 >= chipFloor {
								t.Errorf("fleet %q w=%d h=%d active=%d: rows elided unexpectedly (maxRows=%d fullBudget=%d)", fleet.name, w, h, active, maxRows, m.width-2)
							}
						}
						continue
					}
					maxRowsCheck := max(h-4, 1)
					if maxRowsCheck > 1 {
						if len(rows[0].providers) > 0 {
							firstProv := rows[0].providers[0]
							label := " " + m.providerLabel(firstProv) + " "
							natural := lipgloss.Width(m.styles.chipActiveStyle.Render(label))
							alloc := lipgloss.Width(rows[0].parts[0])
							if alloc != natural {
								t.Errorf("fleet %q w=%d h=%d active=%d: row0 first chip truncated: alloc %d != natural %d (providers %v)", fleet.name, w, h, active, alloc, natural, rows[0].providers)
							}
							if alloc == chipFloor && alloc < natural {
								t.Errorf("fleet %q w=%d h=%d active=%d: orphan on row0: first chip at floor %d < natural %d", fleet.name, w, h, active, alloc, natural)
							}
						} else {
							if len(rows[0].parts) != 0 {
								t.Errorf("fleet %q w=%d h=%d active=%d: empty row0 must have 0 parts, got %d", fleet.name, w, h, active, len(rows[0].parts))
							}
						}
					} else {
						if len(rows[0].providers) == 0 && len(rows[0].parts) != 0 {
							t.Errorf("fleet %q w=%d h=%d active=%d: empty row0 must have 0 parts, got %d", fleet.name, w, h, active, len(rows[0].parts))
						}
					}

					rendered := m.renderHeader()
					for i, line := range strings.Split(rendered, "\n") {
						if lw := lipgloss.Width(line); lw > w {
							t.Errorf("fleet %q w=%d h=%d active=%d: header row %d width %d exceeds %d %q", fleet.name, w, h, active, i, lw, w, stripANSI(line))
						}
					}

					hrc := m.headerRowCount()
					if m.hasSwitcher() && len(rows) > 0 {
						if hrc != len(rows) {
							t.Errorf("fleet %q w=%d h=%d active=%d: headerRowCount %d != len(chipRowsLayout) %d", fleet.name, w, h, active, hrc, len(rows))
						}
					}
					renderedLines := strings.Count(rendered, "\n") + 1
					if renderedLines != hrc {
						t.Errorf("fleet %q w=%d h=%d active=%d: renderedLines %d != headerRowCount %d", fleet.name, w, h, active, renderedLines, hrc)
					}

					seen := make(map[int]bool)
					for _, r := range rows {
						for _, p := range r.providers {
							seen[p] = true
						}
					}
					for ri, r := range rows {
						if len(r.parts) == 0 {
							if isRace {
								for _, x := range []int{0, m.width / 2, m.width - 1} {
									if x < 0 || x >= m.width {
										continue
									}
									if _, ok := m.chipAt(x, ri); ok {
										t.Errorf("fleet %q w=%d h=%d active=%d: empty row %d hit at x=%d", fleet.name, w, h, active, ri, x)
									}
								}
							} else {
								for x := 0; x < m.width; x++ {
									if _, ok := m.chipAt(x, ri); ok {
										t.Errorf("fleet %q w=%d h=%d active=%d: empty row %d hit at x=%d", fleet.name, w, h, active, ri, x)
									}
								}
							}
							continue
						}
						var col int
						if ri == 0 {
							col = m.fixedBodyWidth() + 4
						} else {
							col = 1
						}
						for i := range r.parts {
							cw := lipgloss.Width(r.parts[i])
							prov := r.providers[i]
							if isRace {
								for _, x := range []int{col, col + cw/2, col + cw - 1} {
									idx, ok := m.chipAt(x, ri)
									if !ok || idx != prov {
										t.Errorf("fleet %q w=%d h=%d active=%d row %d x=%d: chipAt=(%d,%v) want (%d,true) widths %v", fleet.name, w, h, active, ri, x, idx, ok, prov, func() []int {
											var ws []int
											for _, p := range r.parts {
												ws = append(ws, lipgloss.Width(p))
											}
											return ws
										}())
									}
								}
							} else {
								for x := col; x < col+cw; x++ {
									idx, ok := m.chipAt(x, ri)
									if !ok || idx != prov {
										t.Errorf("fleet %q w=%d h=%d active=%d row %d x=%d: chipAt=(%d,%v) want (%d,true) widths %v", fleet.name, w, h, active, ri, x, idx, ok, prov, func() []int {
											var ws []int
											for _, p := range r.parts {
												ws = append(ws, lipgloss.Width(p))
											}
											return ws
										}())
									}
								}
							}
							if isRace && i < len(r.parts)-1 {
								gx := col + cw
								if _, ok := m.chipAt(gx, ri); ok {
									t.Errorf("fleet %q w=%d h=%d active=%d: gap hit at (%d,%d)", fleet.name, w, h, active, gx, ri)
								}
							}
							col += cw + 1
						}
					}
					if isRace {
						for ri, r := range rows {
							for _, x := range []int{0, m.width / 2, m.width - 1} {
								if idx, ok := m.chipAt(x, ri); ok && !seen[idx] {
									t.Errorf("fleet %q w=%d h=%d active=%d: chipAt(%d,%d) hit unrendered provider %d seen %v", fleet.name, w, h, active, x, ri, idx, seen)
								}
							}
							_ = r
						}
					} else {
						for ri := range rows {
							for x := 0; x < m.width; x++ {
								if idx, ok := m.chipAt(x, ri); ok && !seen[idx] {
									t.Errorf("fleet %q w=%d h=%d active=%d: chipAt(%d,%d) hit unrendered provider %d seen %v", fleet.name, w, h, active, x, ri, idx, seen)
								}
							}
						}
					}
				}
			}
		}
	}

	t.Run("spot_20_30_body_only", func(t *testing.T) {
		metas := []ProviderMeta{{Name: "anthropic-eu-central", Concurrency: 4}, {Name: "openai-prod-longname", Concurrency: 8}, {Name: "acme-edge-provider", Concurrency: 12}}
		for _, w := range []int{20, 30} {
			m := NewModelForProviders(metas)
			m.width = w
			m.height = 24
			m.active = 0
			m.syncActive()
			rows := m.chipRowsLayout()
			if len(rows) == 0 || len(rows[0].providers) != 0 {
				t.Errorf("w=%d: at 20-30 cols 3-long must be body-only on row0, got row0 providers %v", w, func() []int {
					if len(rows) > 0 {
						return rows[0].providers
					}
					return nil
				}())
			}
		}
	})
	t.Run("spot_120_2short_single_row", func(t *testing.T) {
		m := NewModelForProviders([]ProviderMeta{{Name: "acme", Concurrency: 4}, {Name: "anthropic", Concurrency: 8}})
		m.width = 120
		m.height = 24
		rows := m.chipRowsLayout()
		if len(rows) != 1 {
			t.Fatalf("120/2-short must be single row, got %d rows %v", len(rows), rows)
		}
		for i, idx := range rows[0].providers {
			label := " " + m.providerLabel(idx) + " "
			nat := lipgloss.Width(m.styles.chipActiveStyle.Render(label))
			alloc := lipgloss.Width(rows[0].parts[i])
			if alloc != nat {
				t.Errorf("provider %d alloc %d != nat %d", idx, alloc, nat)
			}
		}
	})
	t.Run("spot_120_3long_share_one", func(t *testing.T) {
		m := NewModelForProviders([]ProviderMeta{{Name: "anthropic-eu-central", Concurrency: 4}, {Name: "openai-prod-longname", Concurrency: 8}, {Name: "acme-edge-provider", Concurrency: 12}})
		m.width = 120
		m.height = 24
		m.active = 0
		m.syncActive()
		rows := m.chipRowsLayout()
		if len(rows) != 2 || len(rows[0].providers) != 1 {
			t.Fatalf("120/3-long must share one chip on row0, got rows %v", rows)
		}
		label := " " + m.providerLabel(rows[0].providers[0]) + " "
		nat := lipgloss.Width(m.styles.chipActiveStyle.Render(label))
		if got := lipgloss.Width(rows[0].parts[0]); got != nat {
			t.Errorf("row0 first chip %d != nat %d", got, nat)
		}
	})
	t.Run("spot_150_3long_share_two", func(t *testing.T) {
		m := NewModelForProviders([]ProviderMeta{{Name: "anthropic-eu-central", Concurrency: 4}, {Name: "openai-prod-longname", Concurrency: 8}, {Name: "acme-edge-provider", Concurrency: 12}})
		m.width = 150
		m.height = 24
		rows := m.chipRowsLayout()
		if len(rows) != 2 || len(rows[0].providers) != 2 {
			t.Fatalf("150/3-long must share two chips on row0, got rows %v", rows)
		}
	})
	t.Run("spot_180_3long_single", func(t *testing.T) {
		m := NewModelForProviders([]ProviderMeta{{Name: "anthropic-eu-central", Concurrency: 4}, {Name: "openai-prod-longname", Concurrency: 8}, {Name: "acme-edge-provider", Concurrency: 12}})
		m.width = 180
		m.height = 24
		rows := m.chipRowsLayout()
		if len(rows) != 1 || len(rows[0].providers) != 3 {
			t.Fatalf("180/3-long must be single row with all 3, got rows %v", rows)
		}
	})
}
