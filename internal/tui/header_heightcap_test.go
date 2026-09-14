// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestHeightCapEdgeCases(t *testing.T) {
	heights := []int{4, 5, 6, 8, 10, 15, 20, 24, 30}
	widths := []int{10, 18, 20, 40, 80, 120, 200}
	if isRace {
		heights = []int{5, 8, 24}
		widths = []int{10, 20, 80, 200}
	}
	for n := 1; n <= 15; n++ {
		metas := make([]ProviderMeta, n)
		for i := range metas {
			metas[i] = ProviderMeta{Name: fmt.Sprintf("prov-%d", i+1), Concurrency: 4}
		}
		for _, h := range heights {
			for _, w := range widths {
				actives := []int{0}
				if n > 1 && !isRace {
					actives = append(actives, n-1)
				} else if n > 1 {
					actives = append(actives, n/2)
				}
				for _, active := range actives {
					m := NewModelForProviders(metas)
					m.width = w
					m.height = h
					m.active = active
					m.syncActive()
					rows := m.chipRowsLayout()
					hrc := m.headerRowCount()
					maxRows := max(h-4, 1)
					if hrc > maxRows {
						t.Fatalf("n=%d w=%d h=%d active=%d: hrc %d > maxRows %d", n, w, h, active, hrc, maxRows)
					}
					if len(rows) > maxRows {
						t.Fatalf("n=%d w=%d h=%d active=%d: len(rows) %d > maxRows %d", n, w, h, active, len(rows), maxRows)
					}
					rendered := m.renderHeader()
					renderedLines := strings.Count(rendered, "\n") + 1
					if renderedLines != hrc {
						t.Fatalf("n=%d w=%d h=%d active=%d: renderedLines %d != hrc %d", n, w, h, active, renderedLines, hrc)
					}
					for i, line := range strings.Split(rendered, "\n") {
						if lw := lipgloss.Width(line); lw > w {
							t.Fatalf("n=%d w=%d h=%d active=%d: line %d width %d > %d", n, w, h, active, i, lw, w)
						}
					}
					if len(rows) > 0 {
						found := false
						for _, r := range rows {
							for _, p := range r.providers {
								if p == active {
									found = true
								}
							}
						}
						if !found {
							t.Fatalf("n=%d w=%d h=%d active=%d: active chip dropped", n, w, h, active)
						}
					}
					for y := len(rows); y < len(rows)+2; y++ {
						for x := 0; x < w; x++ {
							if _, ok := m.chipAt(x, y); ok {
								t.Fatalf("n=%d w=%d h=%d active=%d: chipAt hit beyond rows at (%d,%d)", n, w, h, active, x, y)
							}
						}
					}
					if w <= 18 && len(rows) > 0 && len(rows[0].providers) == 0 {
						for x := 0; x < w; x++ {
							if _, ok := m.chipAt(x, 0); ok {
								t.Fatalf("n=%d w=%d h=%d active=%d: empty row0 hit at x=%d", n, w, h, active, x)
							}
						}
					}
				}
			}
		}
	}
}
