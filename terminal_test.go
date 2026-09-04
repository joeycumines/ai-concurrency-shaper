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

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
)

// terminalStateDiff compares two terminal states and returns a human-readable
// description of any differences. Returns empty string if identical.
func terminalStateDiff(a, b *term.State) string {
	// Use the fact that term.State wraps unix.Termios which has
	// exported fields. Serialize via fmt.Sprintf for comparison.
	aStr := fmt.Sprintf("%+v", a)
	bStr := fmt.Sprintf("%+v", b)
	if aStr == bStr {
		return ""
	}
	return fmt.Sprintf("terminal state changed:\n  before: %s\n  after:  %s", aStr, bStr)
}

// TestSubprocessTerminalIsolation verifies that running the binary with -tui
// as a subprocess does NOT corrupt the parent process's terminal flags.
// This is a regression test for a bug where CombinedOutput() left stdin
// inherited from the parent, allowing bubbletea's MakeRaw() to disable
// ICANON/ECHO on the shared terminal FD.
//
// This test only runs when stdin is a real terminal. It will be skipped
// in CI or when output is piped. Use -count=N to detect cross-run
// contamination from prior test invocations.
func TestSubprocessTerminalIsolation(t *testing.T) {
	fd := os.Stdin.Fd()
	if !term.IsTerminal(fd) {
		t.Skip("skipping: stdin is not a terminal (run from a real terminal to enable)")
	}

	// Capture terminal state before running the subprocess.
	before, err := term.GetState(fd)
	if err != nil {
		t.Fatalf("get terminal state before: %v", err)
	}

	// Build the binary.
	bin := t.TempDir() + "/test-shaper"
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	// Occupy a port so the binary exits quickly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin,
		"-bind", addr,
		"-upstream", "http://127.0.0.1:1",
		"-tui",
	)
	// Apply the same isolation as TestTUIExitsOnBindFailure:
	// /dev/null stdin + isolateSubprocess to prevent terminal access.
	// On Unix this creates a new session (Setsid); on Windows it is a no-op.
	stdinR, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer stdinR.Close()
	cmd.Stdin = stdinR
	isolateSubprocess(cmd)

	_, _ = cmd.CombinedOutput()

	// Verify terminal state is preserved.
	after, err := term.GetState(fd)
	if err != nil {
		t.Fatalf("get terminal state after: %v", err)
	}

	if diff := terminalStateDiff(before, after); diff != "" {
		t.Error(diff)
		// Restore the saved state to prevent leaving the terminal broken.
		if err := term.Restore(fd, before); err != nil {
			t.Errorf("failed to restore terminal state: %v", err)
		}
	}
}
