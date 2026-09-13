// Copyright (C) 2026 Joseph Cumines
//
// Property fuzz for fleet header geometry: deterministic random
// corpus over labels/widths/heights/unicode to prove invariants generalize.

package tui

import (
	"math/rand"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestHeaderPropertyFuzz(t *testing.T) {
	r := rand.New(rand.NewSource(1)) // deterministic
	const iterations = 200

	latinChars := []rune("abcdefghijklmnopqrstuvwxyz0123456789-")
	cjkRunes := []rune("模型日本語中文한국어αβγδε🪄⚡🔥🚀🌟")
	emojiRunes := []rune("🪄⚡🔥🚀🌟😀😁😂🤖👾🎉✨")

	randLatin := func(n int, pool []rune) string {
		b := make([]rune, n)
		for i := range b {
			b[i] = pool[r.Intn(len(pool))]
		}
		return string(b)
	}

	for iter := 0; iter < iterations; iter++ {
		// 1..6 providers
		n := 1 + r.Intn(6)
		metas := make([]ProviderMeta, n)
		for i := 0; i < n; i++ {
			roll := r.Intn(100)
			var name string
			switch {
			case roll < 10:
				// Empty name -> provider-N fallback, tests label fallback path
				name = ""
			case roll < 40:
				// short ascii 2-6
				name = randLatin(2+r.Intn(5), latinChars)
			case roll < 60:
				// long ascii 12-24
				name = randLatin(12+r.Intn(13), latinChars)
			case roll < 75:
				// CJK/mixed 2-6 runes from cjkRunes
				name = randLatin(2+r.Intn(5), cjkRunes)
			case roll < 85:
				// emoji mix 2-4
				name = randLatin(2+r.Intn(3), emojiRunes)
			default:
				// mixed long with CJK + latin + emoji
				mix := []rune{}
				for k := 0; k < 3+r.Intn(4); k++ {
					pool := latinChars
					if k%3 == 1 {
						pool = cjkRunes
					} else if k%3 == 2 {
						pool = emojiRunes
					}
					mix = append(mix, pool[r.Intn(len(pool))])
				}
				// pad with latin to reach 6-14 runes
				for len(mix) < 6+r.Intn(9) {
					mix = append(mix, latinChars[r.Intn(len(latinChars))])
				}
				name = string(mix)
			}
			metas[i] = ProviderMeta{Name: name, Concurrency: 4 + r.Intn(12)}
		}
		w := 10 + r.Intn(191) // 10..200
		h := 4 + r.Intn(27)   // 4..30
		active := r.Intn(n)

		m := NewModelForProviders(metas)
		m.width = w
		m.height = h
		m.active = active
		m.syncActive()

		rows := m.chipRowsLayout()
		hrc := m.headerRowCount()
		rendered := m.renderHeader()
		maxRows := max(h-4, 1)

		// (a) header lines never exceed width
		lines := strings.Split(rendered, "\n")
		for li, line := range lines {
			if lw := lipgloss.Width(line); lw > w {
				t.Fatalf("iter %d w=%d h=%d active=%d n=%d metas=%v\nrow %d width %d exceeds %d strip %q\nrows=%v hrc=%d rendered=%q", iter, w, h, active, n, metas, li, lw, w, stripANSI(line), rows, hrc, stripANSI(rendered))
			}
		}
		// (b) headerRowCount / len(rows) lockstep and rendered lines == hrc
		renderedLines := strings.Count(rendered, "\n") + 1
		if renderedLines != hrc {
			t.Fatalf("iter %d w=%d h=%d active=%d: renderedLines %d != hrc %d rows=%v strip %q", iter, w, h, active, renderedLines, hrc, rows, stripANSI(rendered))
		}
		if !m.hasSwitcher() {
			if hrc != 1 {
				t.Fatalf("iter %d w=%d h=%d hasSwitcher=false but hrc=%d want 1 rows=%v", iter, w, h, hrc, rows)
			}
			if len(rows) != 0 {
				t.Fatalf("iter %d w=%d h=%d hasSwitcher=false but rows len %d want 0", iter, w, h, len(rows))
			}
		} else {
			if len(rows) == 0 {
				// elided: fullBudget < chipFloor or (k==0 && maxRows==1)
				if hrc != 1 {
					t.Fatalf("iter %d w=%d h=%d active=%d elided but hrc=%d want 1 rows=%v strip %q", iter, w, h, active, hrc, rows, stripANSI(rendered))
				}
			} else {
				if hrc != len(rows) {
					t.Fatalf("iter %d w=%d h=%d active=%d hrc %d != len(rows) %d rows=%v strip %q", iter, w, h, active, hrc, len(rows), rows, stripANSI(rendered))
				}
				if len(rows) > maxRows {
					t.Fatalf("iter %d w=%d h=%d hrc %d exceeds maxRows %d rows=%v", iter, w, h, hrc, maxRows, rows)
				}
			}
		}
		if hrc > maxRows {
			t.Fatalf("iter %d w=%d h=%d hrc %d > maxRows %d", iter, w, h, hrc, maxRows)
		}

		// (c) chipAt exhaustive per-row, right-aligned to width-2
		// and (d) active never dropped, (e) first-chip natural when maxRows>1, (f) no unrendered hit
		if len(rows) > 0 {
			seen := make(map[int]bool)
			for _, r := range rows {
				for _, p := range r.providers {
					seen[p] = true
				}
			}
			// active never dropped (when rows non-nil)
			if !seen[active] {
				t.Fatalf("iter %d w=%d h=%d active=%d dropped seen=%v rows=%v strip %q", iter, w, h, active, seen, rows, stripANSI(rendered))
			}
			// first-chip natural when maxRows>1 and row0 holds chips
			if maxRows > 1 && len(rows[0].providers) > 0 {
				firstProv := rows[0].providers[0]
				label := " " + m.providerLabel(firstProv) + " "
				natural := lipgloss.Width(m.styles.chipActiveStyle.Render(label))
				alloc := lipgloss.Width(rows[0].parts[0])
				if alloc != natural {
					t.Fatalf("iter %d w=%d h=%d active=%d row0 first chip truncated alloc %d != natural %d providers %v widths %v strip %q rows=%v", iter, w, h, active, alloc, natural, rows[0].providers, func() []int {
						ws := make([]int, len(rows[0].parts))
						for i, p := range rows[0].parts {
							ws[i] = lipgloss.Width(p)
						}
						return ws
					}(), stripANSI(rendered), rows)
				}
				if alloc == chipFloor && alloc < natural {
					t.Fatalf("iter %d w=%d h=%d orphan on row0 floor %d < natural %d", iter, w, h, alloc, natural)
				}
			} else if maxRows > 1 && len(rows[0].providers) == 0 {
				if len(rows[0].parts) != 0 {
					t.Fatalf("iter %d w=%d h=%d empty row0 has parts %d want 0 rows=%v", iter, w, h, len(rows[0].parts), rows)
				}
				// empty row0 must have no hits
				for x := 0; x < w; x++ {
					if _, ok := m.chipAt(x, 0); ok {
						t.Fatalf("iter %d w=%d h=%d empty row0 hit at x=%d", iter, w, h, x)
					}
				}
			}
			// per-row chipAt lockstep
			for ri, rr := range rows {
				if len(rr.parts) == 0 {
					for x := 0; x < w; x++ {
						if _, ok := m.chipAt(x, ri); ok {
							t.Fatalf("iter %d w=%d h=%d ri=%d empty row hit at x=%d rows=%v", iter, w, h, ri, x, rows)
						}
					}
					continue
				}
				right := m.width - 2
				for i := len(rr.parts) - 1; i >= 0; i-- {
					cw := lipgloss.Width(rr.parts[i])
					prov := rr.providers[i]
					for x := right - cw + 1; x <= right; x++ {
						idx, ok := m.chipAt(x, ri)
						if !ok || idx != prov {
							t.Fatalf("iter %d w=%d h=%d ri=%d x=%d chipAt=(%d,%v) want (%d,true) widths %v rows=%v strip %q", iter, w, h, ri, x, idx, ok, prov, func() []int {
								ws := make([]int, len(rr.parts))
								for k, p := range rr.parts {
									ws[k] = lipgloss.Width(p)
								}
								return ws
							}(), rows, stripANSI(rendered))
						}
					}
					right -= cw + 1
				}
				// gaps and padding must not hit
				// Check all x from 0..w-1 where chipAt hits, provider must be in seen
				for x := 0; x < w; x++ {
					if idx, ok := m.chipAt(x, ri); ok && !seen[idx] {
						t.Fatalf("iter %d w=%d h=%d ri=%d x=%d hit unrendered provider %d seen %v rows=%v", iter, w, h, ri, x, idx, seen, rows)
					}
				}
			}
			// no hit beyond rows len
			for y := len(rows); y < len(rows)+2; y++ {
				for x := 0; x < w; x++ {
					if _, ok := m.chipAt(x, y); ok {
						t.Fatalf("iter %d w=%d h=%d y=%d beyond rows hit", iter, w, h, y)
					}
				}
			}
		}
		// sanity: recompute golden reproducibility info for failure reproduction
		_ = randLatin
	}
}
