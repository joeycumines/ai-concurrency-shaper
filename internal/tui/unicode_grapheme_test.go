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

func TestUnicodeGraphemeWidth(t *testing.T) {
	tests := []struct {
		label     string
		wantCells int
	}{
		{"模型-α", 6},
		{"acme-🪄-edge", 12},
		{"e\u0301", 1},
		{"anthropic-日本語", 16},
		{"🔥🚀🌟", 6},
		{"provider-N", 10},
	}
	m := NewModelForProviders([]ProviderMeta{{Name: "test", Concurrency: 4}})
	for _, tt := range tests {
		lbl := " " + tt.label + " "
		styled := m.styles.chipActiveStyle.Render(lbl)
		got := lipgloss.Width(styled)
		want := tt.wantCells + 4
		if got != want {
			t.Errorf("label %q: styled width %d want %d (natural %d + padding 4)", tt.label, got, want, tt.wantCells)
		}
	}
}

func TestUnicodeTruncateANSIIdentity(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Name: "test", Concurrency: 4}})
	labels := []string{"模型-α", "acme-🪄-edge", "e\u0301", "anthropic-日本語", "🔥🚀🌟"}
	for _, label := range labels {
		lbl := " " + label + " "
		styled := m.styles.chipActiveStyle.Render(lbl)
		natural := lipgloss.Width(styled)
		truncated := truncateANSI(styled, natural)
		if truncated != styled {
			t.Errorf("truncateANSI(%q, %d) modified input: got %q", label, natural, truncated)
		}
		truncW := lipgloss.Width(truncated)
		if truncW != natural {
			t.Errorf("truncateANSI width %d != natural %d for %q", truncW, natural, label)
		}
	}
}

func TestUnicodeFleetHeaderAtThinWidths(t *testing.T) {
	metas := make([]ProviderMeta, 15)
	for i := range metas {
		metas[i] = ProviderMeta{Name: fmt.Sprintf("模型-provider-%c", 'a'+i), Concurrency: 4}
	}
	for _, w := range []int{20, 40} {
		m := NewModelForProviders(metas)
		m.width = w
		m.height = 24
		m.active = 0
		m.syncActive()
		rows := m.chipRowsLayout()
		hrc := m.headerRowCount()
		rendered := m.renderHeader()
		lines := strings.Split(rendered, "\n")
		if len(lines) != hrc {
			t.Errorf("w=%d: rendered lines %d != hrc %d", w, len(lines), hrc)
		}
		for i, line := range lines {
			if lw := lipgloss.Width(line); lw > w {
				t.Errorf("w=%d: header line %d width %d exceeds %d", w, i, lw, w)
			}
		}
		if len(rows) > 0 && len(rows[0].providers) > 0 {
			firstProv := rows[0].providers[0]
			lbl := " " + m.providerLabel(firstProv) + " "
			natural := lipgloss.Width(m.styles.chipActiveStyle.Render(lbl))
			alloc := lipgloss.Width(rows[0].parts[0])
			if alloc != natural {
				t.Errorf("w=%d: row0 first chip alloc %d != natural %d", w, alloc, natural)
			}
		}
	}
}
