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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// repaintBudget is the per-assertion timeout. The dashboard's renderer paints
// cell diffs (throughput digits) between full repaints, so a contiguous
// header string is only observable in the raw PTY byte stream when a full
// frame is emitted — which the binary does every redrawInterval (10s). A
// snapshot taken at a random phase therefore waits a uniform 0-10s for the
// next full frame; 15s covers the worst case with scheduling margin.
const repaintBudget = 15 * time.Second

// awaitRender waits until want appears in the console output produced after
// this call, within repaintBudget. Failing fast per assertion (rather than
// sharing one long budget) keeps each failure attributed to its own step.
func awaitRender(t *testing.T, h *TUIHarness, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), repaintBudget)
	defer cancel()
	snap := h.Console().Snapshot()
	if err := h.Console().Await(ctx, snap, func(out string) bool {
		return strings.Contains(out, want)
	}); err != nil {
		t.Fatalf("never rendered %q: %v\nFull output: %s", want, err, h.Console().String())
	}
}

// headerLine returns the header row of the current render: the first line of
// the console output containing the active/queued metrics segment shared by
// every header variant. It only matches full repaints — the renderer emits
// cell diffs between them.
func headerLine(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, " active ") && strings.Contains(line, "queued") {
			return line
		}
	}
	return ""
}

// gatedUpstream returns an httptest server whose handler parks every request
// until the client side goes away. A parked request stays in flight through
// any number of snapshot ticks (an instant echo answers between ticks and is
// invisible to the header counters), and the handler self-releases when the
// proxy under test dies — so it can never block httptest.Server.Close past
// teardown.
func gatedUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fireRequest sends a POST to url from a detached goroutine and returns
// immediately. The request is meant to park in the provider's limiter until
// teardown kills the proxy's connections, so the goroutine must never call
// Fatalf (the test goroutine may already be gone): hard failures use Errorf
// and the connection reset at teardown is the expected, logged outcome.
func fireRequest(t *testing.T, wg *sync.WaitGroup, url string) {
	t.Helper()
	wg.Go(func() {
		req, err := http.NewRequest(http.MethodPost, url, nil)
		if err != nil {
			t.Errorf("NewRequest(%s): %v", url, err)
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Logf("request to %s ended (expected at teardown): %v", url, err)
			return
		}
		resp.Body.Close()
	})
}

// twoProviders launches the binary in a PTY with two --provider sections
// (prov1 at /prov1, prov2 at /prov2) fronting gated upstreams.
func twoProviders(t *testing.T) *TUIHarness {
	t.Helper()
	return LaunchMulti(t, []*httptest.Server{gatedUpstream(t), gatedUpstream(t)})
}

// TestPTY_MultiProviderTabSwitch proves the multi-provider feature through
// the real binary and a real terminal: both provider chips render, a request
// fired at a provider's mount drives THAT provider's in-flight counter, and
// Tab moves the visible dashboard — header metrics and Concurrency gauge —
// to the other provider.
func TestPTY_MultiProviderTabSwitch(t *testing.T) {
	h := twoProviders(t)
	var fired sync.WaitGroup
	// LIFO: Close runs first (killing the proxy, which fails the parked
	// requests and releases the fireRequest goroutines), then Wait joins
	// them. The reverse order would wait forever on a live proxy.
	defer fired.Wait()
	defer h.Close()

	// Both provider names render as chips in the header of the first view.
	// The launch await lands on the initial full paint, so the zero-state
	// header is already in the buffer.
	out := h.Console().String()
	for _, name := range []string{"prov1", "prov2"} {
		if !strings.Contains(out, name) {
			t.Fatalf("header should show chip %q; got: %q", name, out)
		}
	}
	if !strings.Contains(out, "prov1 │ 0/4 active") {
		t.Fatalf("initial header should show prov1 with 0/4 active; got: %q", headerLine(out))
	}

	// One parked request at prov1's mount: /v1/messages is a limited route by
	// default, so it enters prov1's limiter and holds its counter at 1.
	fireRequest(t, &fired, h.ProviderURL(0)+"/v1/messages")
	awaitRender(t, h, "prov1 │ 1/4 active")

	// Tab switches the visible dashboard to prov2 (which still has zero
	// in-flight — its counter must not have been moved by prov1's request).
	if _, err := h.Console().WriteString("\t"); err != nil {
		t.Fatalf("WriteString tab: %v", err)
	}
	awaitRender(t, h, "prov2 │ 0/4 active")

	// A parked request at prov2's mount drives the now-active view's counter.
	fireRequest(t, &fired, h.ProviderURL(1)+"/v1/messages")
	awaitRender(t, h, "prov2 │ 1/4 active")

	// The dashboard body follows the switch: the Concurrency view (key 5)
	// renders the active provider's gauge line.
	if _, err := h.Console().WriteString("5"); err != nil {
		t.Fatalf("WriteString 5: %v", err)
	}
	awaitRender(t, h, "1 / 4 active")
}

// TestPTY_MultiProviderShiftTabRawEscape writes the literal Shift+Tab byte
// sequence (ESC [ Z) to the terminal and asserts the active provider cycles
// BACKWARD, proving the ultraviolet key-table decode path end-to-end rather
// than only the model's handling of a synthesized KeyPressMsg.
func TestPTY_MultiProviderShiftTabRawEscape(t *testing.T) {
	h := twoProviders(t)
	defer h.Close()

	// Tab once: the active provider becomes prov2.
	if _, err := h.Console().WriteString("\t"); err != nil {
		t.Fatalf("WriteString tab: %v", err)
	}
	awaitRender(t, h, "prov2 │ 0/4 active")

	// Send the raw backward-cycle sequence. With two providers it must wrap
	// from prov2 back to prov1, not advance to a nonexistent third provider.
	if _, err := h.Console().WriteString("\x1b[Z"); err != nil {
		t.Fatalf("WriteString ESC[Z: %v", err)
	}
	awaitRender(t, h, "prov1 │ 0/4 active")
}

// TestPTY_MultiProviderTeardown probes shutdown while BOTH providers hold
// limited requests in flight: Ctrl+C must still produce a clean process exit
// with the terminal restored. The upstreams are gated on the request context,
// so they self-release the moment the proxy's connections die.
func TestPTY_MultiProviderTeardown(t *testing.T) {
	h := twoProviders(t)
	// LIFO: Wait joins the fireRequest goroutines after Close has killed
	// the proxy (failing their requests). By this point the process has
	// already exited via the Ctrl+C below, so Close's own teardown is fast.
	var fired sync.WaitGroup
	defer fired.Wait()
	defer h.Close()

	// One parked request at prov1's mount holds its counter at 1.
	fireRequest(t, &fired, h.ProviderURL(0)+"/v1/messages")
	awaitRender(t, h, "prov1 │ 1/4 active")

	// Switch to prov2 and hold a request there too: both providers must
	// carry an in-flight request at the same time.
	if _, err := h.Console().WriteString("\t"); err != nil {
		t.Fatalf("WriteString tab: %v", err)
	}
	fireRequest(t, &fired, h.ProviderURL(1)+"/v1/messages")
	awaitRender(t, h, "prov2 │ 1/4 active")

	// Ctrl+C: the TUI exits, main's shutdown runs, the server drain (5s
	// grace) expires with the parked handlers, and the process exits cleanly.
	// 12s covers the 5s drain with margin.
	if _, err := h.Console().WriteString("\x03"); err != nil {
		t.Fatalf("WriteString ctrl+c: %v", err)
	}

	exitCtx, exitCancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer exitCancel()
	code, waitErr := h.Console().WaitExit(exitCtx)
	if exitCtx.Err() != nil {
		t.Fatalf("process did not exit within 12s of Ctrl+C with requests in flight (last err=%v)\nFull output: %s",
			waitErr, h.Console().String())
	}
	if code != 0 {
		t.Errorf("expected exit code 0 on Ctrl+C teardown, got %d\nFull output: %s", code, h.Console().String())
	}

	// The alt-screen exit sequence must have been emitted, proving the
	// terminal was restored rather than left in fullscreen limbo.
	if out := h.Console().String(); !strings.Contains(out, "\x1b[?1049l") {
		t.Errorf("terminal was not restored: no alt-screen exit sequence in output\nFull output: %s", out)
	}
}
