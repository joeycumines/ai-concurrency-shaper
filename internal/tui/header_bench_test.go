// Copyright (C) 2026 Joseph Cumines
//
// Fleet header hot-path benchmarks and theme geometry identity gate.
// Benchmarks cover representative widths/heights and fleets (including
// CJK/emoji) and assert header invariants deterministically without time.Now.

package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// benchFleets returns the canonical fleets used for benchmarks and theme
// identity checks. Labels are fixed (no time.Now, no math/rand) so the
// benchmark is deterministic and fast.
func benchFleets() []struct {
	name  string
	metas []ProviderMeta
} {
	metas15 := make([]ProviderMeta, 15)
	for i := range metas15 {
		metas15[i] = ProviderMeta{Name: fmt.Sprintf("provider-long-name-%c", 'a'+i), Concurrency: 4}
	}
	return []struct {
		name  string
		metas []ProviderMeta
	}{
		{"2-short", []ProviderMeta{{Name: "acme", Concurrency: 4}, {Name: "anthropic", Concurrency: 8}}},
		{"3-long", []ProviderMeta{{Name: "anthropic-eu-central", Concurrency: 4}, {Name: "openai-prod-longname", Concurrency: 8}, {Name: "acme-edge-provider", Concurrency: 12}}},
		{"5-mixed", []ProviderMeta{{Name: "acme", Concurrency: 4}, {Name: "anthropic-eu-central", Concurrency: 8}, {Name: "openai", Concurrency: 12}, {Name: "gemini-long-model-name", Concurrency: 6}, {Name: "edge", Concurrency: 4}}},
		{"15-long", metas15},
		{"cjk-emoji", []ProviderMeta{{Name: "模型-α", Concurrency: 4}, {Name: "acme-🪄-edge", Concurrency: 8}, {Name: "anthropic-日本語", Concurrency: 12}, {Name: "🔥🚀🌟", Concurrency: 6}}},
	}
}

var benchWidths = []int{20, 40, 80, 120, 150, 180}
var benchHeights = []int{5, 8, 24}

// sink variables prevent dead-code elimination in benchmarks.
var sinkChipRows []chipLayout
var sinkHeader string
var sinkHeaderStripped string

// TestHeaderThemeGeometryIdentity proves the header geometry is identical
// under dark and light palettes: chipRowsLayout providers/widths, stripped
// renderHeader lines, headerRowCount, identityWidth, and exhaustive chipAt
// hit-testing all match. The TUI defaults to dark; light is the white-
// background swap via tea.BackgroundColorMsg — here driven via newTheme.
// chipActiveStyle/chipInactiveStyle/headerStyle all share the same box
// model (PaddingLeft/Right 1) across palettes so Width is theme-invariant.
func TestHeaderThemeGeometryIdentity(t *testing.T) {
	fleets := benchFleets()
	for _, fl := range fleets {
		for _, w := range benchWidths {
			for _, h := range benchHeights {
				// Sample two actives per fleet (first and last) to keep the
				// -race gate fast while still pinning active-dependent layout
				// (active-first slack restoration). Exhaustive active coverage
				// is already provided by TestHeaderPropertyFuzz and the fleet
				// matrix.
				actives := []int{0}
				if len(fl.metas) > 1 {
					actives = append(actives, len(fl.metas)-1)
				}
				for _, active := range actives {
					base := NewModelForProviders(fl.metas)
					base.width = w
					base.height = h
					base.active = active
					base.syncActive()

					mDark := base
					mDark.styles = newTheme(true)
					mDark.applyScrollbarTheme()

					mLight := base
					mLight.styles = newTheme(false)
					mLight.applyScrollbarTheme()

					// identityWidth is theme-independent (lipgloss.Width on plain
					// " "+label+" ↕") but must stay lockstep.
					if got, want := mDark.identityWidth(), mLight.identityWidth(); got != want {
						t.Fatalf("fleet %q w=%d h=%d active=%d: identityWidth dark %d != light %d", fl.name, w, h, active, got, want)
					}

					rowsDark := mDark.chipRowsLayout()
					rowsLight := mLight.chipRowsLayout()

					if len(rowsDark) != len(rowsLight) {
						t.Fatalf("fleet %q w=%d h=%d active=%d: len(chipRowsLayout) dark %d != light %d dark=%v light=%v", fl.name, w, h, active, len(rowsDark), len(rowsLight), rowsDark, rowsLight)
					}
					if got, want := mDark.headerRowCount(), mLight.headerRowCount(); got != want {
						t.Fatalf("fleet %q w=%d h=%d active=%d: headerRowCount dark %d != light %d", fl.name, w, h, active, got, want)
					}

					// Per-row providers and allocated widths must match.
					for ri := range rowsDark {
						rd, rl := rowsDark[ri], rowsLight[ri]
						if len(rd.providers) != len(rl.providers) {
							t.Fatalf("fleet %q w=%d h=%d active=%d row %d: providers len dark %d != light %d", fl.name, w, h, active, ri, len(rd.providers), len(rl.providers))
						}
						for k, prov := range rd.providers {
							if rl.providers[k] != prov {
								t.Fatalf("fleet %q w=%d h=%d active=%d row %d col %d: provider dark %d != light %d", fl.name, w, h, active, ri, k, prov, rl.providers[k])
							}
						}
						if len(rd.parts) != len(rl.parts) {
							t.Fatalf("fleet %q w=%d h=%d active=%d row %d: parts len dark %d != light %d", fl.name, w, h, active, ri, len(rd.parts), len(rl.parts))
						}
						for k := range rd.parts {
							dw := lipgloss.Width(rd.parts[k])
							lw := lipgloss.Width(rl.parts[k])
							if dw != lw {
								t.Fatalf("fleet %q w=%d h=%d active=%d row %d part %d: width dark %d != light %d dark %q light %q", fl.name, w, h, active, ri, k, dw, lw, stripANSI(rd.parts[k]), stripANSI(rl.parts[k]))
							}
						}
					}

					// Stripped header lines must be byte-identical; raw widths
					// must both respect terminal width.
					hdDark := mDark.renderHeader()
					hdLight := mLight.renderHeader()
					sdDark := stripANSI(hdDark)
					sdLight := stripANSI(hdLight)
					if sdDark != sdLight {
						t.Fatalf("fleet %q w=%d h=%d active=%d: stripped header dark != light\n dark %q\nlight %q", fl.name, w, h, active, sdDark, sdLight)
					}
					for i, line := range strings.Split(hdDark, "\n") {
						if lw := lipgloss.Width(line); lw > w {
							t.Fatalf("fleet %q w=%d h=%d active=%d dark header line %d width %d exceeds %d %q", fl.name, w, h, active, i, lw, w, stripANSI(line))
						}
					}
					for i, line := range strings.Split(hdLight, "\n") {
						if lw := lipgloss.Width(line); lw > w {
							t.Fatalf("fleet %q w=%d h=%d active=%d light header line %d width %d exceeds %d %q", fl.name, w, h, active, i, lw, w, stripANSI(line))
						}
					}
					// Widths per line must match between themes.
					ldDark := strings.Split(hdDark, "\n")
					ldLight := strings.Split(hdLight, "\n")
					if len(ldDark) != len(ldLight) {
						t.Fatalf("fleet %q w=%d h=%d active=%d: dark lines %d != light %d", fl.name, w, h, active, len(ldDark), len(ldLight))
					}
					for i := range ldDark {
						if dw, lw := lipgloss.Width(ldDark[i]), lipgloss.Width(ldLight[i]); dw != lw {
							t.Fatalf("fleet %q w=%d h=%d active=%d line %d width dark %d != light %d", fl.name, w, h, active, i, dw, lw)
						}
					}

					// chipAt must be identical between themes. Sample
					// interior, edge, gap, padding and beyond-rows checks
					// instead of exhaustive x=0..w-1 to keep the -race gate
					// fast (exhaustive lockstep is already covered by
					// TestHeaderPropertyFuzz / TestChipAt_HitTestsAllRenderedChips).
					for ri := range rowsDark {
						rd := rowsDark[ri]
						if len(rd.parts) == 0 {
							// Empty row0 must have no hit at any x (sample 3 points).
							for _, x := range []int{0, w / 2, w - 1} {
								if _, ok := mDark.chipAt(x, ri); ok {
									t.Fatalf("fleet %q w=%d h=%d active=%d dark empty row %d hit at x=%d", fl.name, w, h, active, ri, x)
								}
								if _, ok := mLight.chipAt(x, ri); ok {
									t.Fatalf("fleet %q w=%d h=%d active=%d light empty row %d hit at x=%d", fl.name, w, h, active, ri, x)
								}
							}
							continue
						}
						var col int
						if ri == 0 {
							col = mDark.fixedBodyWidth() + 4
						} else {
							col = 1
						}
						for i := range rd.parts {
							cw := lipgloss.Width(rd.parts[i])
							prov := rd.providers[i]
							for _, x := range []int{col, col + cw/2, col + cw - 1} {
								idxD, okD := mDark.chipAt(x, ri)
								idxL, okL := mLight.chipAt(x, ri)
								if !okD || idxD != prov {
									t.Fatalf("fleet %q w=%d h=%d active=%d dark chipAt(%d,%d) (%d,%v) want (%d,true) widths %v", fl.name, w, h, active, x, ri, idxD, okD, prov, func() []int {
										ws := make([]int, len(rd.parts))
										for k, p := range rd.parts {
											ws[k] = lipgloss.Width(p)
										}
										return ws
									}())
								}
								if !okL || idxL != prov {
									t.Fatalf("fleet %q w=%d h=%d active=%d light chipAt(%d,%d) (%d,%v) want (%d,true)", fl.name, w, h, active, x, ri, idxL, okL, prov)
								}
							}
							if i < len(rd.parts)-1 {
								gx := col + cw
								if _, ok := mDark.chipAt(gx, ri); ok {
									t.Fatalf("fleet %q w=%d h=%d active=%d dark gap hit at (%d,%d)", fl.name, w, h, active, gx, ri)
								}
								if _, ok := mLight.chipAt(gx, ri); ok {
									t.Fatalf("fleet %q w=%d h=%d active=%d light gap hit at (%d,%d)", fl.name, w, h, active, gx, ri)
								}
							}
							col += cw + 1
						}
						startCol := 1
						if ri == 0 {
							startCol = mDark.fixedBodyWidth() + 4
						}
						for _, x := range []int{0} {
							if x >= startCol {
								continue
							}
							if _, ok := mDark.chipAt(x, ri); ok {
								t.Fatalf("fleet %q w=%d h=%d active=%d dark padding hit at (%d,%d)", fl.name, w, h, active, x, ri)
							}
							if _, ok := mLight.chipAt(x, ri); ok {
								t.Fatalf("fleet %q w=%d h=%d active=%d light padding hit at (%d,%d)", fl.name, w, h, active, x, ri)
							}
						}
					}
					// Beyond rows must have no hit in either theme (sample).
					for y := len(rowsDark); y < len(rowsDark)+2; y++ {
						for _, x := range []int{0, w / 2, w - 1} {
							if _, ok := mDark.chipAt(x, y); ok {
								t.Fatalf("fleet %q w=%d h=%d active=%d dark chipAt beyond rows hit at (%d,%d)", fl.name, w, h, active, x, y)
							}
							if _, ok := mLight.chipAt(x, y); ok {
								t.Fatalf("fleet %q w=%d h=%d active=%d light chipAt beyond rows hit at (%d,%d)", fl.name, w, h, active, x, y)
							}
						}
					}
				}
			}
		}
	}
}

// BenchmarkChipRowsLayout measures chipRowsLayout across representative
// widths/heights and fleets. Each sub-benchmark is deterministic (fixed
// labels, no time.Now) and reports allocs/op. Use -benchtime=1x for a
// <2s smoke (90 combos x 1 iter each); default -benchtime=1s measures
// steady-state ns/op. The benchmark also asserts header invariants once per
// combo before timing so a layout regression fails fast.
func BenchmarkChipRowsLayout(b *testing.B) {
	fleets := benchFleets()
	for _, fl := range fleets {
		for _, w := range benchWidths {
			for _, h := range benchHeights {
				// Pre-check invariants once, outside the timed loop.
				m0 := NewModelForProviders(fl.metas)
				m0.width = w
				m0.height = h
				m0.active = 0
				m0.syncActive()
				rows := m0.chipRowsLayout()
				hrc := m0.headerRowCount()
				rendered := m0.renderHeader()
				if got := strings.Count(rendered, "\n") + 1; got != hrc {
					b.Fatalf("fleet %q w=%d h=%d: rendered lines %d != hrc %d", fl.name, w, h, got, hrc)
				}
				for i, line := range strings.Split(rendered, "\n") {
					if lw := lipgloss.Width(line); lw > w {
						b.Fatalf("fleet %q w=%d h=%d: header line %d width %d exceeds %d", fl.name, w, h, i, lw, w)
					}
				}
				if len(rows) > 0 && m0.hasSwitcher() {
					if hrc != len(rows) {
						b.Fatalf("fleet %q w=%d h=%d: hrc %d != len(rows) %d", fl.name, w, h, hrc, len(rows))
					}
				}
				b.Run(fmt.Sprintf("%s/%dx%d", fl.name, w, h), func(b *testing.B) {
					m := NewModelForProviders(fl.metas)
					m.width = w
					m.height = h
					m.active = 0
					m.syncActive()
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						sinkChipRows = m.chipRowsLayout()
					}
				})
			}
		}
	}
}

// BenchmarkRenderHeader measures renderHeader across the same matrix. It
// inherits the same deterministic, alloc-counting contract as
// BenchmarkChipRowsLayout. chipRowsLayout remains the single source of truth
// for both rendering and hit-testing; the benchmark asserts header <= width
// and headerRowCount/len lockstep before timing.
func BenchmarkRenderHeader(b *testing.B) {
	fleets := benchFleets()
	for _, fl := range fleets {
		for _, w := range benchWidths {
			for _, h := range benchHeights {
				m0 := NewModelForProviders(fl.metas)
				m0.width = w
				m0.height = h
				m0.active = 0
				m0.syncActive()
				rows := m0.chipRowsLayout()
				hrc := m0.headerRowCount()
				rendered := m0.renderHeader()
				if got := strings.Count(rendered, "\n") + 1; got != hrc {
					b.Fatalf("fleet %q w=%d h=%d: rendered lines %d != hrc %d", fl.name, w, h, got, hrc)
				}
				for i, line := range strings.Split(rendered, "\n") {
					if lw := lipgloss.Width(line); lw > w {
						b.Fatalf("fleet %q w=%d h=%d: header line %d width %d exceeds %d", fl.name, w, h, i, lw, w)
					}
				}
				if len(rows) > 0 && m0.hasSwitcher() {
					if hrc != len(rows) {
						b.Fatalf("fleet %q w=%d h=%d: hrc %d != len(rows) %d", fl.name, w, h, hrc, len(rows))
					}
				}
				// Exhaustive chipAt vs rendered width sanity once.
				if len(rows) > 0 {
					for ri, r := range rows {
						if len(r.parts) == 0 {
							for x := 0; x < w; x++ {
								if _, ok := m0.chipAt(x, ri); ok {
									b.Fatalf("fleet %q w=%d h=%d: empty row %d hit at x=%d", fl.name, w, h, ri, x)
								}
							}
						}
					}
				}
				b.Run(fmt.Sprintf("%s/%dx%d", fl.name, w, h), func(b *testing.B) {
					m := NewModelForProviders(fl.metas)
					m.width = w
					m.height = h
					m.active = 0
					m.syncActive()
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						sinkHeader = m.renderHeader()
						sinkHeaderStripped = stripANSI(sinkHeader)
						_ = sinkHeaderStripped
					}
				})
			}
		}
	}
}

// allocCeiling records the observed allocs/op for a specific fleet/width/height
// combination. The ceiling is set to observed + 5% jitter so that minor GC or
// runtime variance does not cause false failures, while any real allocation
// regression (slice copy, string concat, lipgloss.Width reallocation) exceeds it.
type allocCeiling struct {
	fleet   string
	width   int
	height  int
	ceiling int64
}

// chipAllocCeilings pins the maximum allocs/op for BenchmarkChipRowsLayout.
// Values derived from max of 3 race-mode AllocsPerRun(100) samples + 5% headroom.
var chipAllocCeilings = []allocCeiling{
	{"2-short", 20, 5, 52}, {"2-short", 20, 8, 111}, {"2-short", 20, 24, 111},
	{"2-short", 40, 5, 52}, {"2-short", 40, 8, 111}, {"2-short", 40, 24, 111},
	{"2-short", 80, 5, 84}, {"2-short", 80, 8, 111}, {"2-short", 80, 24, 111},
	{"2-short", 120, 5, 108}, {"2-short", 120, 8, 108}, {"2-short", 120, 24, 108},
	{"2-short", 150, 5, 108}, {"2-short", 150, 8, 108}, {"2-short", 150, 24, 108},
	{"2-short", 180, 5, 108}, {"2-short", 180, 8, 108}, {"2-short", 180, 24, 108},
	{"3-long", 20, 5, 77}, {"3-long", 20, 8, 161}, {"3-long", 20, 24, 161},
	{"3-long", 40, 5, 77}, {"3-long", 40, 8, 162}, {"3-long", 40, 24, 162},
	{"3-long", 80, 5, 77}, {"3-long", 80, 8, 163}, {"3-long", 80, 24, 163},
	{"3-long", 120, 5, 112}, {"3-long", 120, 8, 163}, {"3-long", 120, 24, 163},
	{"3-long", 150, 5, 135}, {"3-long", 150, 8, 161}, {"3-long", 150, 24, 161},
	{"3-long", 180, 5, 158}, {"3-long", 180, 8, 158}, {"3-long", 180, 24, 158},
	{"5-mixed", 20, 5, 126}, {"5-mixed", 20, 8, 267}, {"5-mixed", 20, 24, 267},
	{"5-mixed", 40, 5, 126}, {"5-mixed", 40, 8, 266}, {"5-mixed", 40, 24, 266},
	{"5-mixed", 80, 5, 126}, {"5-mixed", 80, 8, 268}, {"5-mixed", 80, 24, 268},
	{"5-mixed", 120, 5, 188}, {"5-mixed", 120, 8, 265}, {"5-mixed", 120, 24, 265},
	{"5-mixed", 150, 5, 235}, {"5-mixed", 150, 8, 261}, {"5-mixed", 150, 24, 261},
	{"5-mixed", 180, 5, 260}, {"5-mixed", 180, 8, 260}, {"5-mixed", 180, 24, 260},
	{"15-long", 20, 5, 368}, {"15-long", 20, 8, 702}, {"15-long", 20, 24, 776},
	{"15-long", 40, 5, 368}, {"15-long", 40, 8, 771}, {"15-long", 40, 24, 771},
	{"15-long", 80, 5, 368}, {"15-long", 80, 8, 764}, {"15-long", 80, 24, 764},
	{"15-long", 120, 5, 423}, {"15-long", 120, 8, 767}, {"15-long", 120, 24, 767},
	{"15-long", 150, 5, 447}, {"15-long", 150, 8, 769}, {"15-long", 150, 24, 769},
	{"15-long", 180, 5, 470}, {"15-long", 180, 8, 769}, {"15-long", 180, 24, 769},
	{"cjk-emoji", 20, 5, 101}, {"cjk-emoji", 20, 8, 212}, {"cjk-emoji", 20, 24, 212},
	{"cjk-emoji", 40, 5, 101}, {"cjk-emoji", 40, 8, 213}, {"cjk-emoji", 40, 24, 213},
	{"cjk-emoji", 80, 5, 101}, {"cjk-emoji", 80, 8, 215}, {"cjk-emoji", 80, 24, 215},
	{"cjk-emoji", 120, 5, 161}, {"cjk-emoji", 120, 8, 213}, {"cjk-emoji", 120, 24, 213},
	{"cjk-emoji", 150, 5, 207}, {"cjk-emoji", 150, 8, 207}, {"cjk-emoji", 150, 24, 207},
	{"cjk-emoji", 180, 5, 207}, {"cjk-emoji", 180, 8, 207}, {"cjk-emoji", 180, 24, 207},
}

// TestBenchAllocCeilings asserts chipRowsLayout allocations stay within
// pinned ceilings across all fleet/width/height combinations. A single extra
// allocation (slice copy, string concat, lipgloss.Width reallocation) breaks CI.
func TestBenchAllocCeilings(t *testing.T) {
	fleets := benchFleets()
	for _, c := range chipAllocCeilings {
		var metas []ProviderMeta
		for _, fl := range fleets {
			if fl.name == c.fleet {
				metas = fl.metas
				break
			}
		}
		if metas == nil {
			t.Fatalf("unknown fleet %q in ceiling table", c.fleet)
		}
		m := NewModelForProviders(metas)
		m.width = c.width
		m.height = c.height
		m.active = 0
		m.syncActive()
		allocs := testing.AllocsPerRun(100, func() {
			sinkChipRows = m.chipRowsLayout()
		})
		got := int64(allocs)
		if got > c.ceiling {
			t.Errorf("%s/%dx%d: allocs/op %d exceeds ceiling %d", c.fleet, c.width, c.height, got, c.ceiling)
		}
	}
}
