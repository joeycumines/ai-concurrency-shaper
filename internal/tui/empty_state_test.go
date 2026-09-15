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
)

// TestEmptyStates pins the TUI empty-state copy and the zero-provider legacy
// paths, so copy drift fails fast: the legacy brand for a zero Model and a
// single unnamed provider, the no-entries lines per tab, and the filter-match
// variants that name the active filter.
func TestEmptyStates(t *testing.T) {
	// Zero Model and single unnamed provider keep the legacy brand.
	if got := (Model{}).providerName(); got != " ⚡ shaper" {
		t.Errorf("zero Model providerName = %q, want legacy brand", got)
	}
	single := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	if single.hasSwitcher() {
		t.Fatal("single unnamed provider must not render a switcher")
	}
	if got := single.providerName(); got != " ⚡ shaper" {
		t.Errorf("single unnamed providerName = %q, want legacy brand", got)
	}
	if s := single.renderHelpOverlay(); strings.Contains(s, "Switch provider") {
		t.Errorf("single-provider help must not document the switcher, got:\n%s", s)
	}

	m := NewModelForProviders([]ProviderMeta{{Concurrency: 4}})
	m.width = 80
	m.height = 24

	if got := stripANSI(m.renderRequests()); !strings.Contains(got, "No requests yet.") {
		t.Errorf("empty requests tab should show 'No requests yet.', got:\n%s", got)
	}
	if got := stripANSI(m.renderNetwork()); !strings.Contains(got, "No network entries yet.") {
		t.Errorf("empty network tab should show 'No network entries yet.', got:\n%s", got)
	}
	if got := stripANSI(m.renderConcurrency()); !strings.Contains(got, "No requests in flight.") {
		t.Errorf("empty concurrency tab should show 'No requests in flight.', got:\n%s", got)
	}

	m.filterText = "zzz-no-match"
	if got := stripANSI(m.renderRequests()); !strings.Contains(got, `No requests matching "zzz-no-match"`) {
		t.Errorf("filtered requests tab should name the filter, got:\n%s", got)
	}
	if got := stripANSI(m.renderNetwork()); !strings.Contains(got, `No entries matching "zzz-no-match"`) {
		t.Errorf("filtered network tab should name the filter, got:\n%s", got)
	}
}
