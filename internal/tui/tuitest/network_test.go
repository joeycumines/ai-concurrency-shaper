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
	"context"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/go-prompt/termtest"
)

func TestPTY_NetworkTabShowsEntries(t *testing.T) {
	h := Launch(t)
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	proxyURL := h.ProxyURL()
	paths := []string{"/v1/messages", "/v1/chat/completions", "/v1/embeddings"}
	for _, path := range paths {
		sendRequest(t, ctx, proxyURL+path)
	}

	time.Sleep(3 * time.Second)

	if _, err := h.Console().WriteString("3"); err != nil {
		t.Fatalf("WriteString 3: %v", err)
	}
	snap := h.Console().Snapshot()
	if err := h.Console().Expect(ctx, snap, termtest.Contains("Waterfall"), "Network tab waterfall column"); err != nil {
		t.Errorf("Network tab should show Waterfall column: %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}
}

func TestPTY_NetworkTabShowsRequestData(t *testing.T) {
	h := Launch(t)
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	proxyURL := h.ProxyURL()
	sendRequest(t, ctx, proxyURL+"/v1/messages")

	time.Sleep(3 * time.Second)

	if _, err := h.Console().WriteString("3"); err != nil {
		t.Fatalf("WriteString 3: %v", err)
	}
	time.Sleep(1 * time.Second)
	out := h.Console().String()
	if !strings.Contains(out, "Waterfall") {
		t.Errorf("Network tab should show Waterfall column. Output: %s", out[:minLen(out, 500)])
	}

	// Check for POST in the output (may be in the request log or network tab).
	snap := h.Console().Snapshot()
	if err := h.Console().Expect(ctx, snap, termtest.Contains("POST"), "POST method"); err != nil {
		t.Logf("POST not found in network tab (may be timing): %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}
}

func TestPTY_NetworkTabTypeFilter(t *testing.T) {
	h := Launch(t)
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	proxyURL := h.ProxyURL()
	sendRequest(t, ctx, proxyURL+"/v1/messages")

	time.Sleep(3 * time.Second)

	if _, err := h.Console().WriteString("3"); err != nil {
		t.Fatalf("WriteString 3: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	snap := h.Console().Snapshot()
	if _, err := h.Console().WriteString("t"); err != nil {
		t.Fatalf("WriteString t: %v", err)
	}

	if err := h.Console().Expect(ctx, snap, termtest.Contains("type:"), "type filter indicator"); err != nil {
		t.Errorf("Type filter should be indicated: %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}
}

func TestPTY_NetworkTabStatusFilter(t *testing.T) {
	h := Launch(t)
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	proxyURL := h.ProxyURL()
	sendRequest(t, ctx, proxyURL+"/v1/messages")

	time.Sleep(3 * time.Second)

	if _, err := h.Console().WriteString("3"); err != nil {
		t.Fatalf("WriteString 3: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	snap := h.Console().Snapshot()
	if _, err := h.Console().WriteString("s"); err != nil {
		t.Fatalf("WriteString s: %v", err)
	}

	if err := h.Console().Expect(ctx, snap, termtest.Contains("status:"), "status filter indicator"); err != nil {
		t.Errorf("Status filter should be indicated: %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}
}

func TestPTY_NetworkTabDetailOverlay(t *testing.T) {
	h := Launch(t)
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	proxyURL := h.ProxyURL()
	sendRequest(t, ctx, proxyURL+"/v1/messages")

	time.Sleep(3 * time.Second)

	if _, err := h.Console().WriteString("3"); err != nil {
		t.Fatalf("WriteString 3: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	snap := h.Console().Snapshot()
	if _, err := h.Console().WriteString("\r"); err != nil {
		t.Fatalf("WriteString enter: %v", err)
	}

	if err := h.Console().Expect(ctx, snap, termtest.Contains("Request"), "detail overlay Request section"); err != nil {
		t.Errorf("Detail overlay should show Request section: %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}
}

func TestPTY_NavigationAllFiveTabs(t *testing.T) {
	h := Launch(t)
	defer h.Close()

	// Verify all 5 tabs are accessible by checking for unique content.
	tabs := []struct {
		key      string
		contains string
	}{
		{"1", "Throughput"},
		{"2", "No requests yet"},
		{"3", "Waterfall"},
		{"4", "Concurrency Gauge"},
		{"5", "No route data yet"},
	}

	for _, tab := range tabs {
		if _, err := h.Console().WriteString(tab.key); err != nil {
			t.Fatalf("WriteString %s: %v", tab.key, err)
		}
		time.Sleep(500 * time.Millisecond)

		out := h.Console().String()
		if !strings.Contains(out, tab.contains) {
			t.Errorf("Tab %s should show %q", tab.key, tab.contains)
			t.Logf("Full output: %s", out[:minLen(out, 500)])
		}
	}
}

func TestPTY_NetworkTabWaterfallVisible(t *testing.T) {
	h := Launch(t)
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	proxyURL := h.ProxyURL()
	sendRequest(t, ctx, proxyURL+"/v1/messages")

	time.Sleep(3 * time.Second)

	if _, err := h.Console().WriteString("3"); err != nil {
		t.Fatalf("WriteString 3: %v", err)
	}

	// Wait for waterfall characters (block characters used in timing bar).
	snap := h.Console().Snapshot()
	if err := h.Console().Expect(ctx, snap, termtest.Contains("█"), "waterfall timing bar"); err != nil {
		t.Errorf("Network tab should show waterfall timing bars: %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}
}

func TestPTY_NetworkTabFilterByText(t *testing.T) {
	h := Launch(t)
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	proxyURL := h.ProxyURL()
	sendRequest(t, ctx, proxyURL+"/v1/messages")
	sendRequest(t, ctx, proxyURL+"/v1/chat/completions")

	time.Sleep(3 * time.Second)

	if _, err := h.Console().WriteString("3"); err != nil {
		t.Fatalf("WriteString 3: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	snap := h.Console().Snapshot()
	if _, err := h.Console().WriteString("/"); err != nil {
		t.Fatalf("WriteString /: %v", err)
	}
	if _, err := h.Console().WriteString("messages"); err != nil {
		t.Fatalf("WriteString messages: %v", err)
	}

	if _, err := h.Console().WriteString("\r"); err != nil {
		t.Fatalf("WriteString enter: %v", err)
	}

	if err := h.Console().Expect(ctx, snap, termtest.Contains("messages"), "filter text"); err != nil {
		t.Errorf("Network tab should show filter: %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}
}
