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
	"sync"
	"testing"
	"time"
)

func TestPTY_ConcurrencyInFlight(t *testing.T) {
	h := Launch(t)
	defer h.Close()

	// Set upstream to have a delay so requests stay in-flight
	h.Ctrl().SetDelay(2 * time.Second)

	// Fire 2 concurrent requests (limited routes)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendRequest(t, t.Context(), h.ProxyURL()+"/v1/messages")
		}()
	}

	// Give requests time to reach upstream but not complete
	time.Sleep(500 * time.Millisecond)

	// Switch to Concurrency tab
	if _, err := h.Console().WriteString("4"); err != nil {
		t.Fatalf("WriteString 3: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Verify in-flight entries are visible
	out := h.Console().String()
	if !strings.Contains(out, "POST") {
		t.Error("Concurrency tab should show in-flight POST requests")
	}

	// Wait for requests to complete
	wg.Wait()
	time.Sleep(1 * time.Second)

	// Switch to Concurrency tab again
	if _, err := h.Console().WriteString("4"); err != nil {
		t.Fatalf("WriteString 3: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
}
