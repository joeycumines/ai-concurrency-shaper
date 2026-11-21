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
	"net/http"
	"regexp"
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

	out := screenContains(t, h, "FLEET HEALTH", "ADMISSION POOLS", "THROUGHPUT", "REQUEST OUTCOMES", "HISTORY RECORDER", "5 received")

	// Verify dashboard sections (Circuit Breaker is optional
	// and only rendered when configured).
	checks := []string{
		"FLEET HEALTH",
		"ACTIVE",
		"QUEUED",
		"ADMISSION POOLS",
		"THROUGHPUT",
		"REQUEST OUTCOMES",
		"HISTORY RECORDER",
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

func TestPTY_DashboardNarrowStatusVisibility(t *testing.T) {
	// A 40-column terminal must still show every non-zero status class
	// label. Before the budget/render mismatch fix the composed status row
	// was 50 cells wide, so renderContentWithScrollbar truncated the
	// rightmost cells and "4xx:" / "5xx:" were cut off the screen.
	h := Launch(t, WithTermSize(40, 40), WithArgs("-retry", "0", "-circuit-breaker=false"))
	defer h.Close()

	proxyURL := h.ProxyURL()
	// Drive each class twice. Disable retries and the breaker so this
	// viewport check observes six exact upstream responses without backoff.
	for range 2 {
		h.Ctrl().SetStatus(http.StatusOK)
		sendRequest(t, t.Context(), proxyURL+"/v1/messages")
		time.Sleep(50 * time.Millisecond)
	}
	for range 2 {
		h.Ctrl().SetStatus(http.StatusNotFound)
		sendRequest(t, t.Context(), proxyURL+"/v1/messages")
		time.Sleep(50 * time.Millisecond)
	}
	for range 2 {
		h.Ctrl().SetStatus(http.StatusInternalServerError)
		sendRequest(t, t.Context(), proxyURL+"/v1/messages")
		time.Sleep(50 * time.Millisecond)
	}

	time.Sleep(2 * time.Second)

	// Dashboard is the default tab (1).
	if _, err := h.Console().WriteString("1"); err != nil {
		t.Fatalf("WriteString 1: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	awaitScreen(t, h, "all nonzero status classes at 40 columns", func(screen string) bool {
		for _, class := range []string{"2xx", "4xx", "5xx"} {
			if !regexp.MustCompile(class + `:\s*2\b`).MatchString(screen) {
				return false
			}
		}
		return true
	})
}

func TestPTY_DashboardScrollsDown(t *testing.T) {
	// Use a short terminal so the dashboard content overflows.
	h := Launch(t, WithTermSize(10, 80))
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

	before := screenContains(t, h, "FLEET HEALTH")
	if strings.Contains(before, "HISTORY RECORDER") {
		t.Fatal("history recorder should not be visible before scrolling; terminal is too tall for this regression")
	}

	// The enriched overview has more rows. Half-page movement must still
	// expose the lower sections even when only six content rows are available.
	for range 16 {
		if _, err := h.Console().WriteString(string([]byte{0x04})); err != nil {
			t.Fatalf("WriteString ctrl-d: %v", err)
		}
		time.Sleep(150 * time.Millisecond)
	}

	screenContains(t, h, "HISTORY RECORDER")
	writeKeys(t, h, strings.Repeat("\x15", 16))
	awaitScreen(t, h, "overview scrolled back to the beginning", func(screen string) bool {
		return strings.Contains(screen, "FLEET HEALTH") && !strings.Contains(screen, "HISTORY RECORDER")
	})
}
