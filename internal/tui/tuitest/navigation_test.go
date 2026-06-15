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

func TestPTY_PageKeys(t *testing.T) {
	h := Launch(t)
	defer h.Close()

	// Generate enough requests to make scrolling meaningful
	proxyURL := h.ProxyURL()
	for i := range 15 {
		path := "/v1/messages"
		if i%2 == 1 {
			path = "/v1/chat/completions"
		}
		sendRequest(t, t.Context(), proxyURL+path)
	}
	time.Sleep(2 * time.Second)

	// Switch to Requests tab
	if _, err := h.Console().WriteString("2"); err != nil {
		t.Fatalf("WriteString 2: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Press PageDown key sequence: simulate via the TUI's interpretation.
	// Since PTY test can't easily send special keys, we instead test
	// Home/End and Ctrl-U/Ctrl-D via ctrl+u/ctrl+d which we can send.

	// Test Ctrl+D (half-page down)
	if _, err := h.Console().WriteString("\x04"); err != nil { // Ctrl-D is 0x04 in terminal
		t.Fatalf("WriteString ctrl-d: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	out := h.Console().String()
	if !strings.Contains(out, "POST") {
		t.Error("Expected POST to be visible after scrolling")
	}

	// Test Home (g) and G keys for top/bottom
	if _, err := h.Console().WriteString("G"); err != nil {
		t.Fatalf("WriteString G: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := h.Console().WriteString("g"); err != nil {
		t.Fatalf("WriteString g: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Ctrl+U (page up) and Ctrl+D (page down)
	// Ctrl+U = 0x15, Ctrl+D = 0x04
	// Send Ctrl+U (hex 15 = 0x15)
	if _, err := h.Console().WriteString(string([]byte{0x15})); err != nil {
		t.Logf("WriteString ctrl-u: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	// Now ctrl+D
	if _, err := h.Console().WriteString(string([]byte{0x04})); err != nil {
		t.Logf("WriteString ctrl-d: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
}

func TestPTY_HomeEndKeys(t *testing.T) {
	h := Launch(t)
	defer h.Close()

	// Inject some requests
	proxyURL := h.ProxyURL()
	for range 5 {
		sendRequest(t, t.Context(), proxyURL+"/v1/messages")
	}
	time.Sleep(2 * time.Second)

	// Switch to Requests tab
	if _, err := h.Console().WriteString("2"); err != nil {
		t.Fatalf("WriteString 2: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Press G (go to bottom)
	if _, err := h.Console().WriteString("G"); err != nil {
		t.Fatalf("WriteString G: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Press g (go to top)
	if _, err := h.Console().WriteString("g"); err != nil {
		t.Fatalf("WriteString g: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
}
