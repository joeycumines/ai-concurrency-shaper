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

//go:build unix

package tuitest

import (
	"strings"
	"testing"
)

func TestPTY_LaunchAndQuit(t *testing.T) {
	h := Launch(t)
	defer h.Close()

	out := h.console.String()
	if !strings.Contains(strings.ToLower(out), "shaper") {
		t.Fatalf("expected TUI output to contain 'shaper', got: %q", out[:minLen(out, 200)])
	}
}

func minLen(a string, b int) int {
	if len(a) < b {
		return len(a)
	}
	return b
}
