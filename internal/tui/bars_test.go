// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package tui

import (
	"strings"
	"testing"

	"github.com/rivo/uniseg"
)

func TestRenderGaugeBar(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 10}})
	m.width = 80
	g := m.renderGaugeBar(5, 10, 20)
	if !strings.Contains(g, "█") {
		t.Error("gauge should contain filled blocks")
	}
	if strings.Contains(g, "%") {
		t.Errorf("gauge should not contain a percentage, got: %s", g)
	}
	stripped := stripANSI(g)
	if got, want := uniseg.StringWidth(stripped), 3+20+1+2; got != want {
		t.Errorf("gauge visible width = %d, want %d; got: %q", got, want, stripped)
	}
	if !strings.HasSuffix(stripped, "]  ") {
		t.Errorf("gauge should have two-cell right padding, got: %q", stripped)
	}
}

func TestRenderGaugeBarOverflow(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 10}})
	m.width = 80
	g := m.renderGaugeBar(100, 10, 20)
	if strings.Contains(g, "%") {
		t.Errorf("gauge should not contain a percentage, got: %s", g)
	}
	stripped := stripANSI(g)
	if got, want := uniseg.StringWidth(stripped), 3+20+1+2; got != want {
		t.Errorf("gauge visible width = %d, want %d; got: %q", got, want, stripped)
	}
	if !strings.HasSuffix(stripped, "]  ") {
		t.Errorf("gauge should have two-cell right padding, got: %q", stripped)
	}
}

func TestRenderGaugeBarEmpty(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 10}})
	m.width = 80
	g := m.renderGaugeBar(0, 10, 20)
	if strings.Contains(g, "█") && !strings.Contains(g, "░") {
		t.Error("empty gauge should not contain filled blocks")
	}
	stripped := stripANSI(g)
	if !strings.HasSuffix(stripped, "]  ") {
		t.Errorf("empty gauge should have two-cell right padding, got: %q", stripped)
	}
}

func TestRenderHBar_Padding(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	g := stripANSI(m.renderHBar(5, 10, 20, m.queueFillStyle(5, 10)))
	if !strings.HasPrefix(g, "  [") {
		t.Errorf("hbar should have two-cell left margin, got: %q", g)
	}
	if !strings.HasSuffix(g, "]  ") {
		t.Errorf("hbar should have two-cell right padding, got: %q", g)
	}
	if got, want := uniseg.StringWidth(g), 3+20+1+2; got != want {
		t.Errorf("hbar visible width = %d, want %d; got: %q", got, want, g)
	}
}

func TestGaugeFillStyleSeverity(t *testing.T) {
	cases := []struct {
		name     string
		pct      int
		wantHex  string
		wantBold bool
	}{
		{"blue at 59", 59, "#58A6FF", true},
		{"amber at 60", 60, "#D29922", true},
		{"amber at 89", 89, "#D29922", true},
		{"red at 90", 90, "#F85149", true},
		{"red at 95", 95, "#F85149", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			style := darkModel().gaugeFillStyle(c.pct)
			gotHex := hexString(style.GetForeground())
			if gotHex != c.wantHex {
				t.Errorf("gaugeFillStyle(%d) foreground = %s, want %s", c.pct, gotHex, c.wantHex)
			}
			if gotBold := style.GetBold(); gotBold != c.wantBold {
				t.Errorf("gaugeFillStyle(%d) bold = %v, want %v", c.pct, gotBold, c.wantBold)
			}
		})
	}
}

func TestQueueFillStyleSeverity(t *testing.T) {
	cases := []struct {
		name     string
		value    int
		max      int
		wantHex  string
		wantBold bool
	}{
		{"empty at zero", 0, 16, "#21262D", false},
		{"fractional visible", 1, 300, "#39D353", true},
		{"vivid green below half", 7, 16, "#39D353", true},
		{"orange at 50% bound", 8, 16, "#F0883E", true},
		{"orange at 89%", 14, 16, "#F0883E", true},
		{"red at 90%", 15, 16, "#F85149", true},
		{"red at full", 16, 16, "#F85149", true},
		{"invalid max zero", 0, 0, "#21262D", false},
		{"invalid max with value", 1, 0, "#21262D", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			style := darkModel().queueFillStyle(c.value, c.max)
			gotHex := hexString(style.GetForeground())
			if gotHex != c.wantHex {
				t.Errorf("queueFillStyle(%d, %d) foreground = %s, want %s", c.value, c.max, gotHex, c.wantHex)
			}
			if gotBold := style.GetBold(); gotBold != c.wantBold {
				t.Errorf("queueFillStyle(%d, %d) bold = %v, want %v", c.value, c.max, gotBold, c.wantBold)
			}
		})
	}
}

func TestGaugeBarWidth(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	if got, want := m.gaugeBarWidth(), m.hBarWidth(); got != want {
		t.Errorf("gaugeBarWidth() = %d, want hBarWidth() = %d", got, want)
	}
}

func TestStatusStyle(t *testing.T) {
	m := darkModel()
	for _, code := range []int{200, 201, 301, 404, 429, 500, 503, 0, 99} {
		_ = m.statusStyle(code) // just verify no panic
	}
}

func TestSparklineFillStyleSeverity(t *testing.T) {
	cases := []struct {
		name string
		last int
		want string
	}{
		{"blue at 50%", 5, "#58A6FF"},
		{"amber at 70%", 7, "#D29922"},
		{"red at window max", 10, "#F85149"},
		{"blue at zero value", 0, "#58A6FF"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hexString(darkModel().sparklineFillStyle(c.last, 10).GetForeground())
			if got != c.want {
				t.Errorf("sparklineFillStyle(%d, 10) foreground = %s, want %s", c.last, got, c.want)
			}
		})
	}
}
