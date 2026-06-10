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
	for i := 0; i < 12; i++ {
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

	for i := 0; i < 3; i++ {
		if _, err := h.Console().WriteString("j"); err != nil {
			t.Fatalf("WriteString j: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	scrolledOut := h.Console().String()
	if out == scrolledOut {
		t.Error("Output should change after scrolling")
	}

	for i := 0; i < 3; i++ {
		if _, err := h.Console().WriteString("k"); err != nil {
			t.Fatalf("WriteString k: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
