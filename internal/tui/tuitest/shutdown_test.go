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
	"net"
	"testing"
	"time"

	"github.com/joeycumines/go-prompt/termtest"
)

// TestPTY_BindFailureExitsFastWhileTUIRunning is a regression test for the
// TUI shutdown hang on bind failure. main_test.go's TestTUIExitsOnBindFailure
// doesn't cover this because it launches with /dev/null stdin, so the TUI
// never starts. This test runs the TUI in a real PTY, then triggers a bind
// failure. Without the proactive stop()+Quit() in main.go's teardown defer,
// the process hangs for the full 3-second Kill() fallback, so this test
// fails with elapsed >= 3s.
func TestPTY_BindFailureExitsFastWhileTUIRunning(t *testing.T) {
	// Occupy a port so the binary's bind fails immediately and
	// deterministically. We hold the listener for the whole test.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	bindAddr := ln.Addr().String()
	defer ln.Close()

	bin := buildBinary(t)

	ctx := t.Context()

	// The upstream is never reached (bind fails first), so a placeholder
	// suffices; it only needs to pass run()'s -upstream URL validation.
	console, err := termtest.NewConsole(ctx,
		termtest.WithCommand(bin,
			"-tui",
			"-upstream", "http://127.0.0.1:1",
			"-bind", bindAddr,
		),
		termtest.WithSize(40, 120),
		termtest.WithDefaultTimeout(15*time.Second),
		termtest.WithEnv([]string{"TERM=xterm-256color"}),
	)
	if err != nil {
		t.Fatalf("termtest.NewConsole: %v", err)
	}
	defer console.Close()

	// Capture start after NewConsole returns so elapsed measures process
	// lifetime only. The unfixed binary blocks on time.After(3*time.Second)
	// in its teardown defer, so it is always >= 3s; the fixed binary exits
	// promptly.
	start := time.Now()

	// Budget well above the 3s Kill() fallback so a stuck process is caught.
	exitCtx, exitCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer exitCancel()

	exitCode, waitErr := console.WaitExit(exitCtx)
	elapsed := time.Since(start)

	// A non-zero exit is expected (bind failure -> os.Exit(1)). The only
	// genuine failure is the budget expiring, meaning the TUI is hanging.
	if exitCtx.Err() != nil {
		t.Fatalf("process did not exit within 8s; the TUI teardown is hanging (last err=%v)\noutput:\n%s",
			waitErr, console.String())
	}

	t.Logf("bind-failure shutdown exited in %s (code=%d)", elapsed, exitCode)
	if elapsed >= 3*time.Second {
		t.Fatalf("process took %s to exit on bind failure; expected <3s "+
			"(>=3s means the Kill() fallback fired — the shutdown-hang regression)\noutput:\n%s",
			elapsed, console.String())
	}

	if exitCode == 0 {
		t.Fatalf("expected non-zero exit on bind failure, got 0\noutput:\n%s", console.String())
	}
}
