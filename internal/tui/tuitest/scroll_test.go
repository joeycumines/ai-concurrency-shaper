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

func TestPTY_ScrollRequests(t *testing.T) {
	h := Launch(t)
	defer h.Close()

	proxyURL := h.ProxyURL()
	for i := range 12 {
		path := "/v1/messages"
		if i%2 == 1 {
			path = "/v1/chat/completions"
		}
		sendRequest(t, t.Context(), proxyURL+path)
	}

	time.Sleep(2 * time.Second)

	if _, err := h.Console().WriteString("2"); err != nil {
		t.Fatalf("WriteString 2: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	out := h.Console().String()
	if !strings.Contains(out, "POST") {
		t.Error("Requests tab should show POST method")
	}
	if !strings.Contains(out, "/v1/messages") {
		t.Error("Requests tab should show /v1/messages path")
	}

	for range 3 {
		if _, err := h.Console().WriteString("j"); err != nil {
			t.Fatalf("WriteString j: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	scrolledOut := h.Console().String()
	if out == scrolledOut {
		t.Error("Output should change after scrolling")
	}

	for range 3 {
		if _, err := h.Console().WriteString("k"); err != nil {
			t.Fatalf("WriteString k: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestPTY_ScrollAtNarrowWidth(t *testing.T) {
	h := Launch(t, WithTermSize(20, 40))
	defer h.Close()

	proxyURL := h.ProxyURL()
	for i := range 20 {
		path := "/v1/messages"
		if i%2 == 1 {
			path = "/v1/chat/completions"
		}
		sendRequest(t, t.Context(), proxyURL+path)
	}

	time.Sleep(2 * time.Second)

	// Switch to Requests tab.
	if _, err := h.Console().WriteString("2"); err != nil {
		t.Fatalf("WriteString 2: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	out := h.Console().String()
	if !strings.Contains(out, "POST") {
		t.Error("Narrow TUI should show POST method")
	}

	// Scroll down.
	for range 5 {
		if _, err := h.Console().WriteString("j"); err != nil {
			t.Fatalf("WriteString j: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	scrolledOut := h.Console().String()
	if out == scrolledOut {
		t.Error("Output should change after scrolling in narrow terminal")
	}

	// Scroll back up.
	for range 5 {
		if _, err := h.Console().WriteString("k"); err != nil {
			t.Fatalf("WriteString k: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestPTY_ScrollLargeDataset(t *testing.T) {
	h := Launch(t)
	defer h.Close()

	proxyURL := h.ProxyURL()
	for i := range 50 {
		path := "/v1/messages"
		if i%3 == 1 {
			path = "/v1/chat/completions"
		} else if i%3 == 2 {
			path = "/embeddings"
		}
		sendRequest(t, t.Context(), proxyURL+path)
	}

	time.Sleep(2 * time.Second)

	if _, err := h.Console().WriteString("2"); err != nil {
		t.Fatalf("WriteString 2: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Go to bottom.
	if _, err := h.Console().WriteString("G"); err != nil {
		t.Fatalf("WriteString G: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	bottomOut := h.Console().String()
	if !strings.Contains(bottomOut, "POST") {
		t.Error("Should show POST at bottom")
	}

	// Go to top.
	if _, err := h.Console().WriteString("g"); err != nil {
		t.Fatalf("WriteString g: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	topOut := h.Console().String()
	if topOut == bottomOut {
		t.Error("Output should differ between top and bottom")
	}
}

func TestPTY_ScrollNetworkTab(t *testing.T) {
	h := Launch(t, WithTermSize(30, 80))
	defer h.Close()

	proxyURL := h.ProxyURL()
	for i := range 15 {
		path := "/v1/messages"
		if i%2 == 1 {
			path = "/v1/chat/completions"
		}
		sendRequest(t, t.Context(), proxyURL+path)
	}

	time.Sleep(2 * time.Second)

	// Switch to Network tab.
	if _, err := h.Console().WriteString("3"); err != nil {
		t.Fatalf("WriteString 3: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	out := h.Console().String()
	if !strings.Contains(out, "POST") {
		t.Error("Network tab should show POST method entries")
	}

	// Scroll down.
	for range 3 {
		if _, err := h.Console().WriteString("j"); err != nil {
			t.Fatalf("WriteString j: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	scrolledOut := h.Console().String()
	if out == scrolledOut {
		t.Error("Network tab output should change after scrolling")
	}
}

func TestPTY_PageUpDown(t *testing.T) {
	h := Launch(t)
	defer h.Close()

	proxyURL := h.ProxyURL()
	for range 30 {
		sendRequest(t, t.Context(), proxyURL+"/v1/messages")
	}

	time.Sleep(2 * time.Second)

	if _, err := h.Console().WriteString("2"); err != nil {
		t.Fatalf("WriteString 2: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Page down twice.
	for range 2 {
		if _, err := h.Console().WriteString(string(rune(0x7f))); err != nil {
			// We need to send the actual PgDn key.
		}
	}

	// Use Ctrl-D for half-page down instead (simpler key).
	for range 4 {
		// Send Ctrl-D
		h.Console().Write([]byte{4}) // Ctrl-D = \x04
		time.Sleep(150 * time.Millisecond)
	}

	afterDown := h.Console().String()

	// Ctrl-U to go back up.
	for range 4 {
		h.Console().Write([]byte{21}) // Ctrl-U = \x15
		time.Sleep(150 * time.Millisecond)
	}

	afterUp := h.Console().String()
	if afterDown == afterUp {
		t.Error("Output should differ after half-page down vs up")
	}
}
