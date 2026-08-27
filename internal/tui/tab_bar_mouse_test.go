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
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rivo/uniseg"
)

func TestMouseClickTabBarGeometry(t *testing.T) {
	tests := []struct {
		name         string
		width        int
		x            int
		startTab     tabID
		wantTab      tabID
		wantNoChange bool
	}{
		{"width80 x0 Overview", 80, 0, tabDashboard, tabDashboard, false},
		{"width80 x13 edge Overview last cell", 80, 13, tabDashboard, tabDashboard, false},
		{"width80 x14 Requests start", 80, 14, tabDashboard, tabRequests, false},
		{"width80 x27 Requests last", 80, 27, tabDashboard, tabRequests, false},
		{"width80 x28 Network start", 80, 28, tabDashboard, tabNetwork, false},
		{"width80 x40 Network last", 80, 40, tabDashboard, tabNetwork, false},
		{"width80 x41 Logs start", 80, 41, tabDashboard, tabLogs, false},
		{"width80 x50 Logs last cell", 80, 50, tabDashboard, tabLogs, false},
		{"width80 x51 Concurrency start", 80, 51, tabDashboard, tabConcurrency, false},
		{"width80 x67 Concurrency last", 80, 67, tabDashboard, tabConcurrency, false},
		{"width80 x68 Routes start", 80, 68, tabDashboard, tabRoutes, false},
		{"width80 x79 Routes last", 80, 79, tabDashboard, tabRoutes, false},
		{"width120 x50 should be Logs not Network", 120, 50, tabDashboard, tabLogs, false},
		{"width120 x90 beyond bar no-op from Dashboard", 120, 90, tabDashboard, tabDashboard, true},
		{"width120 x90 beyond bar no-op from Requests", 120, 90, tabRequests, tabRequests, true},
		{"width120 x20 still Requests", 120, 20, tabDashboard, tabRequests, false},
		{"width120 x40 Network", 120, 40, tabDashboard, tabNetwork, false},
		{"width40 x12 Overview", 40, 12, tabDashboard, tabDashboard, false},
		{"width40 x14 Requests", 40, 14, tabDashboard, tabRequests, false},
		{"width40 x28 Network", 40, 28, tabDashboard, tabNetwork, false},
		{"width40 x35 Network partial", 40, 35, tabDashboard, tabNetwork, false},
		{"width40 x39 Network last visible", 40, 39, tabDashboard, tabNetwork, false},
		{"width200 x90 beyond no-op", 200, 90, tabDashboard, tabDashboard, true},
		{"width200 x79 still Routes", 200, 79, tabDashboard, tabRoutes, false},
		{"width0 x50 beyond bar no-op", 0, 50, tabDashboard, tabDashboard, true},
		{"width0 x90 beyond bar no-op", 0, 90, tabDashboard, tabDashboard, true},
		{"width200 x50 still Logs", 200, 50, tabDashboard, tabLogs, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = tc.width
			m.height = 24
			m.tab = tc.startTab
			// ensure styles are set (NewModelForProviders does)
			// call Update with MouseClickMsg
			m2 := update(m, tea.MouseClickMsg{X: tc.x, Y: 1})
			if tc.wantNoChange {
				if m2.tab != tc.startTab {
					t.Errorf("width=%d x=%d start=%d: got tab %d, want no change (stay %d) (old ratio would have switched)", tc.width, tc.x, tc.startTab, m2.tab, tc.startTab)
				}
			} else {
				if m2.tab != tc.wantTab {
					t.Errorf("width=%d x=%d start=%d: got tab %d, want %d", tc.width, tc.x, tc.startTab, m2.tab, tc.wantTab)
				}
			}
		})
	}
}

func TestTabAtBoundaries(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	// Check total width is 80 via rendered tab bar.
	bar := m.renderTabBar()
	if w := uniseg.StringWidth(stripANSI(bar)); w != 80 {
		t.Errorf("renderTabBar width = %d, want 80; bar=%q", w, stripANSI(bar))
	}
	// Check individual offsets via tabAt
	cases := []struct {
		x    int
		want tabID
		ok   bool
	}{
		{0, tabDashboard, true},
		{13, tabDashboard, true},
		{14, tabRequests, true},
		{28, tabNetwork, true},
		{41, tabLogs, true},
		{51, tabConcurrency, true},
		{68, tabRoutes, true},
		{79, tabRoutes, true},
		{80, 0, false},
		{90, 0, false},
		{-1, 0, false},
	}
	for _, c := range cases {
		got, ok := m.tabAt(c.x)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("tabAt(%d) = %d,%v want %d,%v", c.x, got, ok, c.want, c.ok)
		}
	}
}

func TestMouseClickTabBarOffRowNoChange(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	// Start on tabDashboard (NOT tabRequests) on purpose. X=15 falls inside the
	// tabRequests hit-test bucket [14,28). If handleMouseClick ever regressed
	// to ignore the Y coordinate and route every click through the tab-bar
	// branch, tabAt(15) would return tabRequests and switchTab would switch
	// AWAY from tabDashboard — a change this test would catch. Starting on
	// tabRequests would make the assertion vacuous: a Y-agnostic regression
	// would "switch" to tabRequests (the already-active tab), leaving it
	// unchanged and letting the test pass silently despite the bug.
	// See scratch/review-04.md (tautological test flaw).
	m.tab = tabDashboard
	// Click at X=15 (geometrically tabRequests) but off the tab bar row
	// (header Y=0, separator Y=2, content rows Y=3/5/10) — must not change tab.
	for _, y := range []int{0, 2, 3, 5, 10} {
		m2 := update(m, tea.MouseClickMsg{X: 15, Y: y})
		if m2.tab != tabDashboard {
			t.Errorf("Y=%d off-row click should not switch tab, got %d want %d (tabDashboard)",
				y, m2.tab, tabDashboard)
		}
	}
}

func TestMouseClickTabBarEmptySpaceNoOpAcrossWidths(t *testing.T) {
	// Empty space is any x >= 80 (total bar width) when terminal wider than bar.
	// Regardless of starting tab, clicking there must be no-op.
	for _, width := range []int{80, 90, 120, 200} {
		for _, start := range []tabID{tabDashboard, tabRequests, tabNetwork, tabLogs, tabConcurrency, tabRoutes} {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = width
			m.height = 24
			m.tab = start
			xBeyond := 85
			if width < 85 {
				// For width 80, beyond is 80 itself (last+1)
				xBeyond = 80
			}
			// Only test if beyond is within terminal (x < width) or for width 80 equal boundary.
			if xBeyond >= width && width != 80 {
				continue
			}
			m2 := update(m, tea.MouseClickMsg{X: xBeyond, Y: 1})
			if m2.tab != start {
				t.Errorf("width=%d start=%d x=%d beyond bar should be no-op, got %d", width, start, xBeyond, m2.tab)
			}
		}
	}
}

func TestMouseClickTabBarNarrowStillGeometry(t *testing.T) {
	// Even when terminal is narrow (40), hit-test still uses fixed geometry, not ratio.
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 40
	m.height = 24
	cases := []struct {
		x    int
		want tabID
	}{
		{0, tabDashboard},
		{13, tabDashboard},
		{14, tabRequests},
		{28, tabNetwork},
	}
	for _, c := range cases {
		m.tab = tabDashboard
		m2 := update(m, tea.MouseClickMsg{X: c.x, Y: 1})
		if m2.tab != c.want {
			t.Errorf("narrow width 40 x=%d got %d want %d", c.x, m2.tab, c.want)
		}
	}
}

// TestTabAtActiveStyleConsistency closes the gap identified in review-01 and
// review-03: no test previously rendered with a non-zero active tab and checked
// that tabAt boundaries hold. This iterates EVERY tab as the selected tab and
// verifies the rendered bar is still 80 cells and tabAt's boundaries match.
func TestTabAtActiveStyleConsistency(t *testing.T) {
	boundaries := []struct {
		x    int
		want tabID
	}{
		{0, tabDashboard}, {13, tabDashboard},
		{14, tabRequests}, {27, tabRequests},
		{28, tabNetwork}, {40, tabNetwork},
		{41, tabLogs}, {50, tabLogs},
		{51, tabConcurrency}, {67, tabConcurrency},
		{68, tabRoutes}, {79, tabRoutes},
		{80, 0}, {90, 0}, {-1, 0},
	}
	for active := range numTabs {
		t.Run(fmt.Sprintf("active=%d", active), func(t *testing.T) {
			m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
			m.width = 80
			m.height = 24
			m.tab = active

			// The rendered bar must always be 80 cells: the theme gives
			// active and inactive the same horizontal box model.
			bar := m.renderTabBar()
			if w := uniseg.StringWidth(stripANSI(bar)); w != 80 {
				t.Fatalf("renderTabBar width = %d, want 80 (active=%d)", w, active)
			}

			// tabAt boundaries must hold regardless of which tab is selected.
			for _, b := range boundaries {
				got, ok := m.tabAt(b.x)
				if b.x < 0 || b.x >= 80 {
					if ok {
						t.Errorf("active=%d tabAt(%d) = ok, want !ok", active, b.x)
					}
					continue
				}
				if !ok || got != b.want {
					t.Errorf("active=%d tabAt(%d) = %d,%v want %d,true", active, b.x, got, ok, b.want)
				}
			}
		})
	}
}

// TestTabAtUsesActiveStyleWidth is the key reproducer for the hardcoded-inactive-
// style defect. It widens the active style (via Width(20)) while Logs is the
// selected tab. With the old code, tabAt always measures every tab with the
// inactive style, so x=55 falls in Concurrency's [51,68) range. With the fix,
// tabAt measures the selected tab with the active style (width 20), shifting
// Logs to [41,61) so x=55 hits Logs — matching what renderTabBar actually draws.
func TestTabAtUsesActiveStyleWidth(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.tab = tabLogs

	// Widen ONLY the active style to prove tabAt reads it (not hard-coded inactive).
	m.styles.tabActiveStyle = m.styles.tabActiveStyle.Width(20)

	// x=55: old code → Concurrency ([51,68)); fixed code → Logs ([41,61)).
	got, ok := m.tabAt(55)
	if !ok || got != tabLogs {
		t.Errorf("tabAt(55) with widened active Logs = %d,%v want Logs,true "+
			"(must measure selected tab with active style, not inactive)", got, ok)
	}

	// Sanity: the rendered bar now places Logs at width 20, so the cumulative
	// layout has Log at [41,61) and Concurrency starting at 61.
	got, ok = m.tabAt(60)
	if !ok || got != tabLogs {
		t.Errorf("tabAt(60) = %d,%v want Logs,true", got, ok)
	}
	got, ok = m.tabAt(61)
	if !ok || got != tabConcurrency {
		t.Errorf("tabAt(61) = %d,%v want Concurrency,true", got, ok)
	}
}

// TestMouseClickTabBarEmptySpaceKeepsFollowLogs verifies the regression fixed
// in T3: clicking empty space on the tab bar (Y=1, beyond the rendered bar but
// within the terminal) while on the Logs tab must NOT disable followLogs.
// Before this fix, handleMouseClick unconditionally set followLogs=false at
// the top, so an inadvertent click on the empty tab-bar margin silently killed
// tail-following until the user manually re-enabled it with G.
func TestMouseClickTabBarEmptySpaceKeepsFollowLogs(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 120
	m.height = 24
	m.switchTab(tabLogs)
	if !m.followLogs {
		t.Fatal("start state: followLogs should be true on Logs tab")
	}

	// Click empty space on the tab bar row (X=85, beyond the 80-cell bar
	// but within the 120-wide terminal). tabAt returns !ok → no-op.
	m2 := update(m, tea.MouseClickMsg{X: 85, Y: 1})
	if !m2.followLogs {
		t.Errorf("tab-bar empty-space click should preserve followLogs=true, got %v", m2.followLogs)
	}
	if m2.tab != tabLogs {
		t.Errorf("tab unchanged expected, got %d", m2.tab)
	}
}

// TestMouseClickContentAreaPausesFollowLogs verifies that a genuine content-
// area click (below the tab bar) on the Logs tab still pauses followLogs.
// This is the intended behaviour that T3 preserves.
func TestMouseClickContentAreaPausesFollowLogs(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24
	m.switchTab(tabLogs)
	if !m.followLogs {
		t.Fatal("start state: followLogs should be true on Logs tab")
	}

	// Content area starts at row 3 (header=0, tabbar=1, separator=2).
	// Clicking at Y=contentStartRow within the viewport pauses tailing.
	m2 := update(m, tea.MouseClickMsg{X: 10, Y: contentStartRow})
	if m2.followLogs {
		t.Errorf("content-area click should set followLogs=false, got %v", m2.followLogs)
	}
}
