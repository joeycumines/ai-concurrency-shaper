// Copyright (C) 2026 Joseph Cumines
//
// Golden pin for fleet header breakpoints: locks renderHeader
// stripANSI widths and chipRowsLayout provider indices/widths byte-exact
// at representative widths so a future header edit that shifts a chip or
// reintroduces a row-0 orphan fails fast.

package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// TestHeaderBreakpointGoldens pins the dynamic natural-fit breakpoints
// documented in header.go: 20/40 body-only, 80 3-long body-only vs 2-short
// single-row, 120 share1, 150 share2, 180 single-row, plus the h=5
// height-cap single-row exemption (observed at w120).
func TestHeaderBreakpointGoldens(t *testing.T) {
	metas3 := []ProviderMeta{
		{Name: "anthropic-eu-central", Concurrency: 4},
		{Name: "openai-prod-longname", Concurrency: 8},
		{Name: "acme-edge-provider", Concurrency: 12},
	}
	metas2 := []ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	}

	type golden struct {
		width         int
		height        int
		metas         []ProviderMeta
		fleetName     string
		active        int
		wantHRC       int
		wantRowCount  int
		wantProviders [][]int
		wantWidths    [][]int
	}

	goldens := []golden{
		{width: 20, height: 24, metas: metas3, fleetName: "3-long-w20", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{}, {0, 1, 2}}, wantWidths: [][]int{{}, {10, 3, 3}}},
		{width: 40, height: 24, metas: metas3, fleetName: "3-long-w40", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{}, {0, 1, 2}}, wantWidths: [][]int{{}, {24, 9, 3}}},
		{width: 80, height: 24, metas: metas3, fleetName: "3-long-w80", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{}, {0, 1, 2}}, wantWidths: [][]int{{}, {24, 24, 22}}},
		{width: 120, height: 24, metas: metas3, fleetName: "3-long-w120", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{0}, {1, 2}}, wantWidths: [][]int{{24}, {24, 22}}},
		{width: 150, height: 24, metas: metas3, fleetName: "3-long-w150", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{0, 1}, {2}}, wantWidths: [][]int{{24, 24}, {22}}},
		{width: 180, height: 24, metas: metas3, fleetName: "3-long-w180", active: 0, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 1, 2}}, wantWidths: [][]int{{24, 24, 22}}},
		{width: 20, height: 24, metas: metas2, fleetName: "2-short-w20", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{}, {0, 1}}, wantWidths: [][]int{{}, {8, 9}}},
		{width: 40, height: 24, metas: metas2, fleetName: "2-short-w40", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{}, {0, 1}}, wantWidths: [][]int{{}, {8, 13}}},
		{width: 80, height: 24, metas: metas2, fleetName: "2-short-w80", active: 0, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 1}}, wantWidths: [][]int{{8, 13}}},
		{width: 120, height: 24, metas: metas2, fleetName: "2-short-w120", active: 0, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 1}}, wantWidths: [][]int{{8, 13}}},
		{width: 150, height: 24, metas: metas2, fleetName: "2-short-w150", active: 0, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 1}}, wantWidths: [][]int{{8, 13}}},
		{width: 180, height: 24, metas: metas2, fleetName: "2-short-w180", active: 0, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 1}}, wantWidths: [][]int{{8, 13}}},
		{width: 120, height: 5, metas: metas3, fleetName: "3-long-h5-active0", active: 0, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0}}, wantWidths: [][]int{{24}}},
		{width: 120, height: 5, metas: metas3, fleetName: "3-long-h5-active1", active: 1, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 1}}, wantWidths: [][]int{{3, 20}}},
		{width: 120, height: 5, metas: metas3, fleetName: "3-long-h5-active2", active: 2, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 2}}, wantWidths: [][]int{{3, 20}}},
	}

	for _, g := range goldens {
		g := g
		t.Run(g.fleetName, func(t *testing.T) {
			m := NewModelForProviders(g.metas)
			m.width = g.width
			m.height = g.height
			m.active = g.active
			m.syncActive()
			rows := m.chipRowsLayout()
			hrc := m.headerRowCount()
			rendered := m.renderHeader()
			lines := strings.Split(rendered, "\n")

			if hrc != g.wantHRC {
				t.Fatalf("fleet %q w=%d h=%d active=%d: headerRowCount %d want %d rows=%v strip %q", g.fleetName, g.width, g.height, g.active, hrc, g.wantHRC, rows, stripANSI(rendered))
			}
			if len(rows) != g.wantRowCount {
				t.Fatalf("fleet %q w=%d h=%d active=%d: len(chipRowsLayout) %d want %d rows=%v strip %q", g.fleetName, g.width, g.height, g.active, len(rows), g.wantRowCount, rows, stripANSI(rendered))
			}
			if len(lines) != hrc {
				t.Fatalf("fleet %q w=%d h=%d active=%d: rendered lines %d != hrc %d strip %q", g.fleetName, g.width, g.height, g.active, len(lines), hrc, stripANSI(rendered))
			}
			for i, line := range lines {
				if lw := lipgloss.Width(line); lw > g.width {
					t.Fatalf("fleet %q w=%d h=%d active=%d: line %d width %d exceeds %d strip %q", g.fleetName, g.width, g.height, g.active, i, lw, g.width, stripANSI(line))
				}
			}
			for ri, wantProv := range g.wantProviders {
				if ri >= len(rows) {
					t.Fatalf("fleet %q w=%d h=%d: missing row %d want providers %v", g.fleetName, g.width, g.height, ri, wantProv)
				}
				gotProv := rows[ri].providers
				if len(gotProv) != len(wantProv) {
					t.Fatalf("fleet %q w=%d h=%d row %d: providers %v want %v rows=%v", g.fleetName, g.width, g.height, ri, gotProv, wantProv, rows)
				}
				for k, want := range wantProv {
					if gotProv[k] != want {
						t.Fatalf("fleet %q w=%d h=%d row %d col %d: provider %d want %d rows=%v", g.fleetName, g.width, g.height, ri, k, gotProv[k], want, rows)
					}
				}
				wantW := g.wantWidths[ri]
				if len(rows[ri].parts) != len(wantW) {
					t.Fatalf("fleet %q w=%d h=%d row %d: parts len %d want %d widths want %v", g.fleetName, g.width, g.height, ri, len(rows[ri].parts), len(wantW), wantW)
				}
				for k, want := range wantW {
					gotW := lipgloss.Width(rows[ri].parts[k])
					if gotW != want {
						t.Fatalf("fleet %q w=%d h=%d row %d part %d: width %d want %d parts %v providers %v strip %q", g.fleetName, g.width, g.height, ri, k, gotW, want, rows[ri].parts, rows[ri].providers, stripANSI(rendered))
					}
				}
				if max(g.height-4, 1) > 1 && ri == 0 && len(rows[ri].providers) > 0 {
					for k, prov := range rows[ri].providers {
						label := " " + m.providerLabel(prov) + " "
						natural := lipgloss.Width(m.styles.chipActiveStyle.Render(label))
						alloc := lipgloss.Width(rows[ri].parts[k])
						if alloc != natural {
							t.Fatalf("fleet %q w=%d h=%d row0 chip %d alloc %d != natural %d (providers %v) strip %q", g.fleetName, g.width, g.height, prov, alloc, natural, rows[ri].providers, stripANSI(rendered))
						}
					}
				}
			}
			for ri, r := range rows {
				if len(r.parts) == 0 {
					for x := 0; x < m.width; x++ {
						if _, ok := m.chipAt(x, ri); ok {
							t.Fatalf("fleet %q w=%d h=%d row %d empty but chipAt hit at x=%d", g.fleetName, g.width, g.height, ri, x)
						}
					}
					continue
				}
				right := m.width - 2
				for i := len(r.parts) - 1; i >= 0; i-- {
					cw := lipgloss.Width(r.parts[i])
					prov := r.providers[i]
					for x := right - cw + 1; x <= right; x++ {
						idx, ok := m.chipAt(x, ri)
						if !ok || idx != prov {
							t.Fatalf("fleet %q w=%d h=%d row %d x=%d chipAt (%d,%v) want (%d,true) widths %v", g.fleetName, g.width, g.height, ri, x, idx, ok, prov, func() []int {
								ws := make([]int, len(r.parts))
								for k, p := range r.parts {
									ws[k] = lipgloss.Width(p)
								}
								return ws
							}())
						}
					}
					right -= cw + 1
				}
			}
		})
	}
}
