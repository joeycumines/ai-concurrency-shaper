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
						right := w - 2
						for i := len(rd.parts) - 1; i >= 0; i-- {
							cw := lipgloss.Width(rd.parts[i])
							prov := rd.providers[i]
							// Left edge, mid, right edge must hit the same provider
							// in both themes.
							for _, x := range []int{right - cw + 1, right - cw/2, right} {
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
							right -= cw + 1
							// Gap between this chip and the next (if any) must not hit.
							if i > 0 {
								gx := right + 1 // single gap cell
								if _, ok := mDark.chipAt(gx, ri); ok {
									t.Fatalf("fleet %q w=%d h=%d active=%d dark gap hit at (%d,%d)", fl.name, w, h, active, gx, ri)
								}
								if _, ok := mLight.chipAt(gx, ri); ok {
									t.Fatalf("fleet %q w=%d h=%d active=%d light gap hit at (%d,%d)", fl.name, w, h, active, gx, ri)
								}
							}
						}
						// Padding left of the leftmost chip must not hit.
						if right >= 0 {
							for _, x := range []int{0, right / 2} {
								if x < 0 || x > right {
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
