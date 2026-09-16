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
// documented in header.go: 20/40 body-only, 80 3-long body-only, 85 2-short
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
		{width: 20, height: 24, metas: metas3, fleetName: "3-long-w20", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{}, {0, 1, 2}}, wantWidths: [][]int{{}, {6, 5, 5}}},
		{width: 40, height: 24, metas: metas3, fleetName: "3-long-w40", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{}, {0, 1, 2}}, wantWidths: [][]int{{}, {24, 7, 5}}},
		{width: 80, height: 24, metas: metas3, fleetName: "3-long-w80", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{}, {0, 1, 2}}, wantWidths: [][]int{{}, {24, 24, 22}}},
		{width: 120, height: 24, metas: metas3, fleetName: "3-long-w120", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{0}, {1, 2}}, wantWidths: [][]int{{24}, {24, 22}}},
		{width: 150, height: 24, metas: metas3, fleetName: "3-long-w150", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{0, 1}, {2}}, wantWidths: [][]int{{24, 24}, {22}}},
		{width: 180, height: 24, metas: metas3, fleetName: "3-long-w180", active: 0, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 1, 2}}, wantWidths: [][]int{{24, 24, 22}}},
		{width: 20, height: 24, metas: metas2, fleetName: "2-short-w20", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{}, {0, 1}}, wantWidths: [][]int{{}, {8, 9}}},
		{width: 40, height: 24, metas: metas2, fleetName: "2-short-w40", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{}, {0, 1}}, wantWidths: [][]int{{}, {8, 13}}},
		{width: 80, height: 24, metas: metas2, fleetName: "2-short-w80", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{0}, {1}}, wantWidths: [][]int{{8}, {13}}},
		{width: 84, height: 24, metas: metas2, fleetName: "2-short-w84", active: 0, wantHRC: 2, wantRowCount: 2, wantProviders: [][]int{{0}, {1}}, wantWidths: [][]int{{8}, {13}}},
		{width: 85, height: 24, metas: metas2, fleetName: "2-short-w85", active: 0, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 1}}, wantWidths: [][]int{{8, 13}}},
		{width: 86, height: 24, metas: metas2, fleetName: "2-short-w86", active: 0, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 1}}, wantWidths: [][]int{{8, 13}}},
		{width: 120, height: 24, metas: metas2, fleetName: "2-short-w120", active: 0, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 1}}, wantWidths: [][]int{{8, 13}}},
		{width: 150, height: 24, metas: metas2, fleetName: "2-short-w150", active: 0, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 1}}, wantWidths: [][]int{{8, 13}}},
		{width: 180, height: 24, metas: metas2, fleetName: "2-short-w180", active: 0, wantHRC: 1, wantRowCount: 1, wantProviders: [][]int{{0, 1}}, wantWidths: [][]int{{8, 13}}},
		{width: 120, height: 5, metas: metas3, fleetName: "3-long-h5-active0", active: 0, wantHRC: 1, wantRowCount: 0, wantProviders: nil, wantWidths: nil},
		{width: 120, height: 5, metas: metas3, fleetName: "3-long-h5-active1", active: 1, wantHRC: 1, wantRowCount: 0, wantProviders: nil, wantWidths: nil},
		{width: 120, height: 5, metas: metas3, fleetName: "3-long-h5-active2", active: 2, wantHRC: 1, wantRowCount: 0, wantProviders: nil, wantWidths: nil},
	}

	for _, g := range goldens {
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
				if ri == 0 && len(rows[ri].providers) > 0 {
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
				var col int
				if ri == 0 {
					col = m.row0ChipStart()
				} else {
					col = 1
				}
				for i := range r.parts {
					cw := lipgloss.Width(r.parts[i])
					prov := r.providers[i]
					for x := col; x < col+cw; x++ {
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
					col += cw + 1
				}
			}
		})
	}
}

// TestHeaderRenderedChipSpansMatchLayout verifies the actual visible output,
// rather than only repeating chipAt's column formula. Every rendered chip's
// visible cells must begin at the coordinate chipAt uses, for both palettes
// and for Unicode labels.
func TestHeaderRenderedChipSpansMatchLayout(t *testing.T) {
	metas := []ProviderMeta{
		{Name: "模型-α", Concurrency: 4},
		{Name: "acme-🪄-edge", Concurrency: 8},
		{Name: "anthropic-日本語", Concurrency: 12},
	}
	for _, dark := range []bool{true, false} {
		for _, width := range []int{20, 40, 80, 120, 150, 180} {
			for _, height := range []int{5, 24} {
				m := NewModelForProviders(metas)
				m.styles = newTheme(dark)
				m.width = width
				m.height = height
				for _, active := range []int{0, 1, 2} {
					m.active = active
					m.syncActive()
					rows := m.chipRowsLayout()
					lines := strings.Split(m.renderHeader(), "\n")
					if len(lines) != m.headerRowCount() {
						t.Fatalf("dark=%v width=%d height=%d active=%d: rendered lines=%d header rows=%d", dark, width, height, active, len(lines), m.headerRowCount())
					}
					for ri, line := range lines {
						if gotWidth := lipgloss.Width(line); gotWidth != width {
							t.Fatalf("dark=%v width=%d height=%d active=%d row=%d: styled line width=%d, want full header width", dark, width, height, active, ri, gotWidth)
						}
					}
					if len(rows) == 0 {
						body := m.headerBody(true)
						if bodyWidth := lipgloss.Width(body); bodyWidth < max(m.width-2, 1) {
							body += strings.Repeat(" ", max(m.width-2, 1)-bodyWidth)
						}
						if want := m.styles.headerStyle.Render(body); lines[0] != want {
							t.Fatalf("dark=%v width=%d height=%d active=%d: elided switcher is not a full-width header-styled body", dark, width, height, active)
						}
						continue
					}
					for ri, row := range rows {
						if len(row.parts) == 0 {
							body := m.headerBody(true)
							if bodyWidth := lipgloss.Width(body); bodyWidth < max(m.width-2, 1) {
								body += strings.Repeat(" ", max(m.width-2, 1)-bodyWidth)
							}
							if want := m.styles.headerStyle.Render(body); lines[ri] != want {
								t.Fatalf("dark=%v width=%d height=%d active=%d row=%d: body-only row is not a full-width header-styled body", dark, width, height, active, ri)
							}
							continue
						}
						visible := stripANSI(lines[ri])
						col := 1
						if ri == 0 {
							col = m.row0ChipStart()
						}
						for i, part := range row.parts {
							want := stripANSI(part)
							start, seen := splitAtCells(visible, col)
							if seen != col {
								t.Fatalf("dark=%v width=%d active=%d row=%d chip=%d: visible prefix has %d cells, want %d: %q", dark, width, active, ri, i, seen, col, visible)
							}
							end, gotWidth := splitAtCells(visible[start:], lipgloss.Width(part))
							if gotWidth != lipgloss.Width(part) {
								t.Fatalf("dark=%v width=%d active=%d row=%d chip=%d: rendered span width=%d, want %d: %q", dark, width, active, ri, i, gotWidth, lipgloss.Width(part), visible)
							}
							if got := visible[start : start+end]; got != want {
								t.Fatalf("dark=%v width=%d active=%d row=%d chip=%d: rendered span %q, want %q at col %d: %q", dark, width, active, ri, i, got, want, col, visible)
							}
							if got, ok := m.chipAt(col, ri); !ok || got != row.providers[i] {
								t.Fatalf("dark=%v width=%d active=%d row=%d chip=%d: chipAt(%d)=(%d,%v), want provider %d", dark, width, active, ri, i, col, got, ok, row.providers[i])
							}
							col += lipgloss.Width(part) + 1
						}
						fill := m.styles.headerStyle.PaddingLeft(0).PaddingRight(0)
						if ri == 0 {
							rowBody := m.headerBody(true)
							if fw := m.fixedBodyWidth(); lipgloss.Width(rowBody) < fw {
								rowBody += strings.Repeat(" ", fw-lipgloss.Width(rowBody))
							}
							if want := fill.Render(" " + rowBody + " │ "); !strings.Contains(lines[ri], want) {
								t.Fatalf("dark=%v width=%d active=%d: row-0 body/divider is not header-styled", dark, width, active)
							}
							if len(row.parts) > 1 {
								if want := fill.Render(" "); !strings.Contains(lines[ri], want) {
									t.Fatalf("dark=%v width=%d active=%d: row-0 chip gap is not header-styled", dark, width, active)
								}
							}
							used := 1 + lipgloss.Width(rowBody) + 3 + lipgloss.Width(strings.Join(row.parts, " "))
							if pad := width - used; pad > 0 {
								if want := fill.Render(strings.Repeat(" ", pad)); !strings.Contains(lines[ri], want) {
									t.Fatalf("dark=%v width=%d active=%d: row-0 trailing fill is not header-styled", dark, width, active)
								}
							}
						} else {
							if want := fill.Render(" "); !strings.Contains(lines[ri], want) {
								t.Fatalf("dark=%v width=%d active=%d row=%d: wrapped left pad is not header-styled", dark, width, active, ri)
							}
							if len(row.parts) > 1 {
								if want := fill.Render(" "); !strings.Contains(lines[ri], want) {
									t.Fatalf("dark=%v width=%d active=%d row=%d: wrapped chip gap is not header-styled", dark, width, active, ri)
								}
							}
							if pad := width - 1 - lipgloss.Width(strings.Join(row.parts, " ")); pad > 0 {
								if want := fill.Render(strings.Repeat(" ", pad)); !strings.Contains(lines[ri], want) {
									t.Fatalf("dark=%v width=%d active=%d row=%d: trailing fill is not header-styled", dark, width, active, ri)
								}
							}
						}
					}
				}
			}
		}
	}
}
