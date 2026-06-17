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

func TestPTY_DashboardSections(t *testing.T) {
	h := Launch(t)
	defer h.Close()

	// Inject some requests to populate the dashboard
	proxyURL := h.ProxyURL()
	for range 5 {
		sendRequest(t, t.Context(), proxyURL+"/v1/messages")
		time.Sleep(100 * time.Millisecond)
	}

	time.Sleep(2 * time.Second)

	// Dashboard is the default tab (1)
	if _, err := h.Console().WriteString("1"); err != nil {
		t.Fatalf("WriteString 1: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	out := h.Console().String()

	// Verify dashboard sections
	checks := []string{
		"Throughput",
		"Concurrency",
		"Queue Depth",
		"Status Distribution",
		"In-Flight Requests",
		"Summary",
	}
	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("Dashboard missing section: %s", check)
		}
	}

	// Verify sparkline characters are present (Unicode block chars)
	sparklineChars := []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}
	hasSparkline := false
	for _, ch := range sparklineChars {
		if strings.Contains(out, ch) {
			hasSparkline = true
			break
		}
	}
	if !hasSparkline {
		t.Log("Note: sparkline may be flat (all zeros) if no throughput data yet")
	}
}

func TestPTY_DashboardScrollsDown(t *testing.T) {
	// Use a short terminal so the dashboard content overflows.
	h := Launch(t, WithTermSize(20, 80))
	defer h.Close()

	proxyURL := h.ProxyURL()
	for range 5 {
		sendRequest(t, t.Context(), proxyURL+"/v1/messages")
		time.Sleep(50 * time.Millisecond)
	}

	time.Sleep(2 * time.Second)

	// Ensure we are on the Overview tab.
	if _, err := h.Console().WriteString("1"); err != nil {
		t.Fatalf("WriteString 1: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	before := h.Console().String()
	if strings.Contains(before, "Summary") {
		t.Fatal("Summary should not be visible before scrolling; terminal is too tall for this regression")
	}

	// Scroll down until the Summary section is visible.
	for range 4 {
		if _, err := h.Console().WriteString(string([]byte{0x04})); err != nil {
			t.Fatalf("WriteString ctrl-d: %v", err)
		}
		time.Sleep(150 * time.Millisecond)
	}

	after := h.Console().String()
	if !strings.Contains(after, "Summary") {
		t.Error("Dashboard should scroll down far enough to reveal the Summary section")
	}
}
