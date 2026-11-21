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
	"time"
)

func TestPTY_RequestDetailOverlay(t *testing.T) {
	h := Launch(t)
	defer h.Close()

	sendRequest(t, t.Context(), h.ProxyURL()+"/v1/messages")

	time.Sleep(2 * time.Second)

	if _, err := h.Console().WriteString("2"); err != nil {
		t.Fatalf("WriteString 2: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	if err := h.Console().Send("enter"); err != nil {
		t.Fatalf("Send enter: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	screenContains(t, h, "REQUEST", "Request ID:", "POST /v1/messages", "Outcome: completed", "Esc return")

	if err := h.Console().Send("esc"); err != nil {
		t.Fatalf("Send esc: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	awaitScreen(t, h, "request inspector closed", func(screen string) bool {
		return strings.Contains(screen, "REQUESTS ·") && !strings.Contains(screen, "Request ID:")
	})
}
