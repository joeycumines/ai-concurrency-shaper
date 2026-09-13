// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestFleetIdentityClick_CyclesProvider pins the desired operator-visible
// behavior for Task 2: left-click on the fleet identity (` <label> ↕` at
// header row 0 col [1,1+identityWidth)) cycles active provider forward (+1,
// wrapping), matching the wheel guard `my==0 && hasSwitcher && mx>=1 &&
// mx<1+identityWidth()`. Boundary/padding/single-provider clicks are no-ops.
// This test is expected to FAIL before the Task 2 fix (current
// handleMouseClick swallows non-chip header clicks) and PASS after.
func TestFleetIdentityClick_CyclesProvider(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
		{Name: "openai", Concurrency: 12},
	})
	m.width = 80
	m.height = 24
	m.active = 0
	m.syncActive()

	idW := m.identityWidth()
	if idW == 0 {
		t.Fatalf("identityWidth() = 0, want >0 in fleet mode")
	}
	hdr := stripANSI(m.renderHeader())
	if !strings.Contains(hdr, "acme \u2195") {
		t.Fatalf("header missing fleet identity %q; got %q", "acme \u2195", hdr)
	}
	if !strings.Contains(hdr, "active") || !strings.Contains(hdr, "busiest:") {
		t.Fatalf("header missing fleetStats markers; got %q", hdr)
	}

	// Click on first content column of identity (X=1, Y=0) cycles forward.
	// No explicit Button → Button==0 (MouseNone) must be treated as left.
	m = update(m, tea.MouseClickMsg{X: 1, Y: 0})
	if m.active != 1 {
		t.Fatalf("after click at X=1,Y=0: active=%d want 1", m.active)
	}
	m = update(m, tea.MouseClickMsg{X: 1, Y: 0})
	if m.active != 2 {
		t.Fatalf("after second identity click: active=%d want 2", m.active)
	}
	m = update(m, tea.MouseClickMsg{X: 1, Y: 0})
	if m.active != 0 {
		t.Fatalf("after third identity click (wrap): active=%d want 0", m.active)
	}

	// Last column of identity (X=1+idW-1) still cycles.
	m.active = 0
	m.syncActive()
	m = update(m, tea.MouseClickMsg{X: 1 + idW - 1, Y: 0})
	if m.active != 1 {
		t.Fatalf("click at last identity col X=%d: active=%d want 1", 1+idW-1, m.active)
	}

	// Boundary: padding col 0 must NOT cycle.
	m.active = 0
	m.syncActive()
	m2 := update(m, tea.MouseClickMsg{X: 0, Y: 0})
	if m2.active != 0 {
		t.Fatalf("click at padding X=0,Y=0 changed active to %d, want 0 (no-op)", m2.active)
	}

	// Boundary: just past identity (X=1+idW) must NOT cycle.
	m2 = update(m, tea.MouseClickMsg{X: 1 + idW, Y: 0})
	if m2.active != 0 {
		t.Fatalf("click at X=%d (past identity) changed active to %d, want 0", 1+idW, m2.active)
	}

	// Not on row 0: Y=1 with same X must NOT cycle.
	m2 = update(m, tea.MouseClickMsg{X: 1, Y: 1})
	if m2.active != 0 {
		t.Fatalf("click at Y=1 should not cycle identity, got active %d", m2.active)
	}

	// Single-provider (no switcher) must never cycle — legacy brand pinned.
	single := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	single.width = 80
	single.height = 24
	if single.hasSwitcher() {
		t.Fatalf("single unnamed provider hasSwitcher should be false")
	}
	if single.identityWidth() != 0 {
		t.Fatalf("single identityWidth=%d want 0", single.identityWidth())
	}
	single2 := update(single, tea.MouseClickMsg{X: 1, Y: 0})
	if single2.active != 0 {
		t.Fatalf("single provider identity click changed active to %d, want 0", single2.active)
	}
}

// TestFleetIdentityClick_LeftButtonOnly documents that only the
// left button should cycle. Right/middle clicks on the same geometry are
// no-ops. MouseNone (0) is tolerated as left for backward compatibility with
// existing tests that omit Button.
func TestFleetIdentityClick_LeftButtonOnly(t *testing.T) {
	m := NewModelForProviders([]ProviderMeta{
		{Name: "acme", Concurrency: 4},
		{Name: "anthropic", Concurrency: 8},
	})
	m.width = 80
	m.height = 24
	m.active = 0
	m.syncActive()

	// Explicit left button should cycle.
	mL := update(m, tea.MouseClickMsg{X: 1, Y: 0, Button: tea.MouseLeft})
	if mL.active != 1 {
		t.Fatalf("left-button click at identity: active=%d want 1", mL.active)
	}

	// Right and middle must NOT cycle.
	for _, btn := range []tea.MouseButton{tea.MouseRight, tea.MouseMiddle} {
		m.active = 0
		m.syncActive()
		m2 := update(m, tea.MouseClickMsg{X: 1, Y: 0, Button: btn})
		if m2.active != 0 {
			t.Fatalf("button %v click at identity must not cycle, got active %d", btn, m2.active)
		}
	}
}
