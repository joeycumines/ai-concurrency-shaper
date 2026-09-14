// Copyright (C) 2026 Joseph Cumines
//
// Repro harness for fleet header single-letter orphan flaw.
// Exists solely to pin and demonstrate the degenerate row0Budget behaviour
// before the layout correction. Run via: go test -run TestRepro_FirstChipOrphan -count=1 -v

package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestRepro_FirstChipOrphan(t *testing.T) {
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
		{"15-long", func() []ProviderMeta {
			m := make([]ProviderMeta, 15)
			for i := range m {
				m[i] = ProviderMeta{Name: fmt.Sprintf("provider-long-name-%c", 'a'+i), Concurrency: 4}
			}
			return m
		}()},
		{"unicode", []ProviderMeta{
			{Name: "模型-α", Concurrency: 4},
			{Name: "acme-🪄-edge", Concurrency: 8},
			{Name: "anthropic-日本語", Concurrency: 12},
		}},
	}
	widths := []int{18, 20, 24, 30, 40, 60, 80, 100, 120, 150}
	heights := []int{5, 8, 24}

	t.Logf("fleet header repro: row0Budget = width-2 - Width(headerBody(true)) -1, chipFloor=%d, fullBudget=width-2", chipFloor)
	t.Logf("expected flaw: for 3-long names body natural (~86) exceeds usable-4 up to width ~92, so row0Budget is degenerate chipFloor=3 and first chip is truncated to \"  a\" alone on row0")

	orphanCount := 0
	totalThin := 0

	for _, fleet := range fleets {
		fleetName := fleet.name
		metas := fleet.metas
		t.Logf("\n=== fleet %q (%d providers) ===", fleetName, len(metas))
		// Log natural chip widths once
		tmp := NewModelForProviders(metas)
		tmp.width = 200
		tmp.height = 24
		t.Logf("  natural chip widths (styled):")
		for i, meta := range metas {
			label := " " + tmp.providerLabel(i) + " "
			naturalActive := lipgloss.Width(tmp.styles.chipActiveStyle.Render(label))
			naturalInactive := lipgloss.Width(tmp.styles.chipInactiveStyle.Render(label))
			t.Logf("    [%d] %q natural=%d/%d label=%q", i, meta.Name, naturalActive, naturalInactive, label)
			_ = naturalInactive
		}
		// Also log natural body width at large width
		tmp.width = 300
		bodyNatural := tmp.headerBody(false)
		bodyReserved := tmp.headerBody(true)
		t.Logf("  body natural (w=300): Width(headerBody(false))=%d %q", lipgloss.Width(bodyNatural), stripANSI(bodyNatural))
		t.Logf("  body reserved (w=300): Width(headerBody(true))=%d", lipgloss.Width(bodyReserved))

		for _, h := range heights {
			for _, w := range widths {
				// Only exhaustive on 3-long at height 24 for brevity, but cover all heights for thin widths
				if h != 24 && w > 40 && fleetName != "3-long" {
					continue
				}
				m := NewModelForProviders(metas)
				m.width = w
				m.height = h
				m.active = 0
				m.syncActive()

				usable := max(m.width-2, 1)
				row0Budget := m.width - 2 - lipgloss.Width(m.headerBody(true)) - 1
				fullBudget := m.width - 2
				maxRows := max(m.height-4, 1)
				rendered := m.renderHeader()
				lines := strings.Split(rendered, "\n")
				rows := m.chipRowsLayout()
				hrc := m.headerRowCount()
				idW := m.identityWidth()

				// Check orphan condition: thin widths 20-40, first row has single truncated chip
				isThin := w >= 20 && w <= 40
				if fleetName == "3-long" && isThin && h == 24 {
					totalThin++
					if len(rows) > 0 && len(rows[0].providers) == 1 {
						chipW := lipgloss.Width(rows[0].parts[0])
						// Natural for provider 0
						label0 := " " + m.providerLabel(0) + " "
						natural0 := lipgloss.Width(m.styles.chipActiveStyle.Render(label0))
						if chipW == chipFloor && chipW < natural0 {
							orphanCount++
						}
					}
				}

				// Log summary line
				t.Logf("  w=%3d h=%2d usable=%3d row0Budget=%3d fullBudget=%3d maxRows=%d hrc=%d rows=%d idW=%d",
					w, h, usable, row0Budget, fullBudget, maxRows, hrc, len(rows), idW)

				// Verify header line widths never exceed terminal
				for i, line := range lines {
					lw := lipgloss.Width(line)
					status := "ok"
					if lw > w {
						status = "OVERFLOW"
					}
					t.Logf("    header line %d: width=%d/%d %s %q", i, lw, w, status, stripANSI(line))
				}

				// Log chip rows detail
				for ri, r := range rows {
					var widths []int
					for _, p := range r.parts {
						widths = append(widths, lipgloss.Width(p))
					}
					// Compare to natural
					var naturals []int
					for _, idx := range r.providers {
						lbl := " " + m.providerLabel(idx) + " "
						// active vs inactive uses same width (padding identical)
						naturals = append(naturals, lipgloss.Width(m.styles.chipActiveStyle.Render(lbl)))
					}
					t.Logf("    row %d: providers=%v widths=%v naturals=%v budget=%d", ri, r.providers, widths, naturals, func() int {
						if ri == 0 {
							if row0Budget < chipFloor {
								return fullBudget
							}
							return row0Budget
						}
						return fullBudget
					}())
					// Check chipAt coverage for this row
					for x := 0; x < m.width; x++ {
						idx, ok := m.chipAt(x, ri)
						_ = idx
						_ = ok
					}
				}

				// Verify chipAt covers every rendered chip cell and no false positives on empty row
				if len(rows) > 0 {
					for ri, r := range rows {
						if len(r.parts) == 0 {
							// Empty row0 — should have no hit
							for x := 0; x < m.width; x++ {
								if _, ok := m.chipAt(x, ri); ok {
									t.Errorf("fleet %q w=%d h=%d: empty row %d has hit at x=%d", fleetName, w, h, ri, x)
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
							wch := lipgloss.Width(r.parts[i])
							prov := r.providers[i]
							for x := col; x < col+wch; x++ {
								idx, ok := m.chipAt(x, ri)
								if !ok || idx != prov {
									t.Errorf("fleet %q w=%d h=%d row %d x=%d: chipAt=(%d,%v) want (%d,true) chip %q", fleetName, w, h, ri, x, idx, ok, prov, r.parts[i])
								}
							}
							col += wch + 1
						}
					}
				}

				// Verify headerRowCount vs rendered lines
				renderedLines := strings.Count(rendered, "\n") + 1
				if renderedLines != hrc {
					t.Logf("    NOTE: renderedLines=%d != headerRowCount=%d (should be equal)", renderedLines, hrc)
				}
				if w < 18 {
					_ = usable
				}
			}
		}
		// Unicode-specific: verify truncateANSI path preserves width accounting
		if fleetName == "unicode" {
			m := NewModelForProviders(metas)
			m.width = 30
			m.height = 24
			m.active = 0
			m.syncActive()
			rendered := m.renderHeader()
			t.Logf("  unicode w=30 header lines:")
			for i, line := range strings.Split(rendered, "\n") {
				t.Logf("    line %d: width=%d strip=%q rawAnsiLen=%d", i, lipgloss.Width(line), stripANSI(line), len(line))
			}
			// Verify grapheme handling: CJK "日本語" width 6, emoji "🪄" width 2
			for _, s := range []string{"日本語", "🪄", "模型-α"} {
				lbl := " " + s + " "
				nat := lipgloss.Width(m.styles.chipActiveStyle.Render(lbl))
				t.Logf("    unicode natural %q -> %d cells styled", lbl, nat)
			}
		}
	}

	t.Logf("\n=== SUMMARY ===")
	t.Logf("thin-width orphan detections (3-long, 20-40, h=24): %d/%d rows had single-letter first chip on row0", orphanCount, totalThin)
	// Post-fix this is a hard invariant: no orphan on row 0 when maxRows>1.
	// The summary doubles as a regression gate (previously 4/4 orphaned via degenerate
	// row0Budget==3; after fix headerBody caps to full usable and row 0 becomes
	// body-only with available < natural[0], so 0/4).
	if orphanCount != 0 {
		t.Fatalf("fleet header orphan regression: %d/%d thin rows still have first chip at floor %d < natural on row 0 (expected 0)", orphanCount, totalThin, chipFloor)
	}
	// Also assert the documented empty-row0 property: at 20-40 cols row 0 must be body-only
	for _, fleet := range fleets {
		if fleet.name != "3-long" {
			continue
		}
		for _, w := range []int{20, 30, 40} {
			m := NewModelForProviders(fleet.metas)
			m.width = w
			m.height = 24
			m.active = 0
			m.syncActive()
			rows := m.chipRowsLayout()
			if len(rows) == 0 || len(rows[0].providers) != 0 {
				t.Fatalf("fleet %q w=%d h=24: expected empty row 0 (body-only) after fix, got providers %v widths %v", fleet.name, w, func() []int {
					if len(rows) > 0 {
						return rows[0].providers
					}
					return nil
				}(), func() []int {
					if len(rows) > 0 && len(rows[0].parts) > 0 {
						ws := make([]int, len(rows[0].parts))
						for i, pp := range rows[0].parts {
							ws[i] = lipgloss.Width(pp)
						}
						return ws
					}
					return nil
				}())
			}
			// All wrapped rows must still honor header <= width
			rendered := m.renderHeader()
			for i, line := range strings.Split(rendered, "\n") {
				if lipgloss.Width(line) > w {
					t.Fatalf("fleet %q w=%d: header line %d width %d exceeds %d after fix", fleet.name, w, i, lipgloss.Width(line), w)
				}
			}
		}
	}
}

func TestRepro_Row0BudgetDegenerate(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	}
	t.Logf("row0Budget degenerate check across widths 20-150 (h=24, active=0)")
	for _, w := range []int{20, 30, 40, 60, 80, 100, 120, 150} {
		m := NewModelForProviders(metas)
		m.width = w
		m.height = 24
		m.active = 0
		m.syncActive()
		row0Budget := m.width - 2 - lipgloss.Width(m.headerBody(true)) - 1
		fullBudget := m.width - 2
		rows := m.chipRowsLayout()
		var row0Info string
		if len(rows) > 0 {
			row0Info = fmt.Sprintf("row0 providers=%v widths=%v", rows[0].providers, func() []int {
				var ws []int
				for _, p := range rows[0].parts {
					ws = append(ws, lipgloss.Width(p))
				}
				return ws
			}())
			if len(rows[0].parts) > 0 {
				label0 := " " + m.providerLabel(0) + " "
				natural0 := lipgloss.Width(m.styles.chipActiveStyle.Render(label0))
				alloc0 := lipgloss.Width(rows[0].parts[0])
				truncated := alloc0 < natural0
				row0Info += fmt.Sprintf(" firstChip %d/%d truncated=%v", alloc0, natural0, truncated)
			} else {
				row0Info += " (empty row0)"
			}
		} else {
			row0Info = "no rows (elided)"
		}
		t.Logf(" w=%3d: row0Budget=%3d fullBudget=%3d body(true)=%q width=%d -> %s", w, row0Budget, fullBudget, stripANSI(m.headerBody(true)), lipgloss.Width(m.headerBody(true)), row0Info)
		if w >= 20 && w <= 100 && row0Budget == chipFloor {
			t.Logf("  -> degenerate: row0Budget==chipFloor (%d) for width %d", chipFloor, w)
		}
	}
}
