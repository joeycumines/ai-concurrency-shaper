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

// TestPTY_LogsTabShowsCapturedLogs verifies that application log output is
// captured into the on-screen Logs tab instead of leaking to stderr: the startup
// summary appears in the Logs tab after switching to it.
func TestPTY_LogsTabShowsCapturedLogs(t *testing.T) {
	h := Launch(t)
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	snap := h.Console().Snapshot()
	if _, err := h.Console().WriteString("4"); err != nil {
		t.Fatalf("WriteString 4: %v", err)
	}

	// Match the truncation-safe prefix rather than the full "auto-detecting LLM
	// endpoints" string: the Logs tab truncates long lines to the column width,
	// which can cut trailing content on narrower viewports, so this prefix is the
	// reliably-visible marker that the startup log was captured.
	if err := h.Console().Expect(ctx, snap, termtest.Contains("auto-detecting LLM"), "captured startup log"); err != nil {
		t.Errorf("Logs tab should show captured startup log: %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}
}

// TestPTY_NoStderrLogLeak verifies the startup/config logs never reach the
// terminal once the TUI is enabled — they are consumed by the Logs buffer and
// not printed to stderr (which would corrupt the dashboard). It also pins the
// shutdown half of the lifecycle (review-10 #4 / review-11 #2): once the TUI
// exits, the captured buffer is redirected to stderr, so graceful-shutdown
// logging streams live instead of evaporating inside the unpolled buffer —
// while the ring's retained contents are never dumped wholesale at teardown.
func TestPTY_NoStderrLogLeak(t *testing.T) {
	h := Launch(t)
	defer h.Close()

	out := h.console.String()
	if strings.Contains(out, "auto-detecting LLM endpoints") {
		t.Errorf("startup log line leaked to terminal output; expected it to be captured by the Logs tab")
		t.Logf("Full output: %s", out[:minLen(out, 500)])
	}

	// Quit through the same path an operator uses and wait out the full
	// graceful shutdown (srv.Shutdown drains for up to 5s).
	if _, err := h.Console().WriteString("\x03"); err != nil {
		t.Fatalf("write ctrl+c: %v", err)
	}
	exitCtx, exitCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer exitCancel()
	if _, err := h.Console().WaitExit(exitCtx); err != nil {
		t.Fatalf("process did not exit within budget: %v\noutput:\n%s", err, h.console.String())
	}
	final := h.console.String()

	if !strings.Contains(final, "shutting down...") {
		t.Errorf("post-TUI shutdown log never reached stderr; the log-buffer redirect wiring is broken\nfull output:\n%s", final[:minLen(final, 1000)])
	}
}

// TestPTY_GroupConflictWarningCaptured verifies that a config warning emitted during
// route parsing is captured into the Logs tab AND surfaced as a toast. Two -limit
// flags sharing a @group at different limits trigger exactly that log.Printf in
// main; the wiring must keep it at stdlib identity (not bridged to slog INFO),
// which the toast assertion pins end to end — see review-07 #1.
func TestPTY_GroupConflictWarningCaptured(t *testing.T) {
	h := Launch(t,
		WithArgs("-limit", "POST /a:2@g", "-limit", "POST /b:3@g"),
	)
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// The warning must not leak to the PRE-TUI terminal (a bare stdlib line
	// would land before the alt-screen takeover and corrupt the dashboard).
	// Once the TUI is up the same text legitimately appears — first as the
	// toast under test, later in the Logs tab — so occurrences are only a leak
	// when they precede the alt-screen enter sequence (\x1b[?1049h).
	if str := h.console.String(); func() bool {
		idx := strings.Index(str, "WARNING: route")
		if idx < 0 {
			return false
		}
		alt := strings.Index(str, "\x1b[?1049h")
		return alt < 0 || idx < alt
	}() {
		t.Errorf("config warning leaked to pre-TUI terminal output; expected it to be captured by the Logs tab")
		t.Logf("Full output: %s", str[:minLen(str, 500)])
	}

	// The actionable warning must toast on whatever tab is active (the
	// dashboard here) within a poll interval of startup.
	snapToast := h.Console().Snapshot()
	if err := h.Console().Expect(ctx, snapToast, termtest.Contains("WARNING: route"), "config warning toast"); err != nil {
		t.Errorf("config warning should raise a dashboard toast: %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}

	if _, err := h.Console().WriteString("4"); err != nil {
		t.Fatalf("WriteString 4: %v", err)
	}

	snap := h.Console().Snapshot()
	if err := h.Console().Expect(ctx, snap, termtest.Contains("WARNING: route"), "captured config warning"); err != nil {
		t.Errorf("Logs tab should show the captured config warning: %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}
}

// TestPTY_TransportErrorToast verifies an actionable (error) log line triggers a
// toast banner on the dashboard.
func TestPTY_TransportErrorToast(t *testing.T) {
	h := Launch(t)
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Closing the upstream makes the proxy report a transport error, which is both
	// captured in the Logs tab and surfaced as a toast.
	h.upstream.Close()

	sendRequest(t, ctx, h.ProxyURL()+"/v1/messages")

	snap := h.Console().Snapshot()
	if err := h.Console().Expect(ctx, snap, termtest.Contains("transport error"), "transport error toast"); err != nil {
		t.Errorf("expected transport-error toast after upstream failure: %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}
}
