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

// Package tuitest provides PTY-based integration tests for the TUI dashboard.
package tuitest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/go-prompt/termtest"
)

// controllableUpstream is an httptest.Server handler that lets tests set
// response status codes and injection delays at runtime.
type controllableUpstream struct {
	mu         sync.Mutex
	statusCode int
	delay      time.Duration
}

func newControllableUpstream() *controllableUpstream {
	return &controllableUpstream{statusCode: 200}
}

func (u *controllableUpstream) SetStatus(code int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.statusCode = code
}

func (u *controllableUpstream) SetDelay(d time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.delay = d
}

func (u *controllableUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	code := u.statusCode
	delay := u.delay
	u.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"ok":true}`)
}

type harnessConfig struct {
	rows uint16
	cols uint16
}

// HarnessOption configures the test harness.
type HarnessOption func(*harnessConfig)

// WithTermSize sets the PTY dimensions. Default is 40x120.
func WithTermSize(rows, cols uint16) HarnessOption {
	return func(c *harnessConfig) {
		c.rows = rows
		c.cols = cols
	}
}

// TUIHarness manages a running TUI instance in a PTY.
type TUIHarness struct {
	t         *testing.T
	console   *termtest.Console
	upstream  *httptest.Server
	ctrl      *controllableUpstream
	ctx       context.CancelFunc
	proxyPort string
}

// freePort returns a free TCP port by asking the OS for port 0.
// This avoids the race condition between checking availability and using the port
// that occurs with sequential port probing.
func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := l.Addr().(*net.TCPAddr)
	l.Close()
	return fmt.Sprintf("%d", addr.Port), nil
}

// Launch builds the ai-concurrency-shaper binary, starts it with -tui in a PTY,
// and waits for the initial render.
func Launch(t *testing.T, opts ...HarnessOption) *TUIHarness {
	t.Helper()

	cfg := &harnessConfig{rows: 40, cols: 120}
	for _, o := range opts {
		o(cfg)
	}

	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}

	ctrl := newControllableUpstream()
	upstream := httptest.NewServer(ctrl)

	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "test-shaper")

	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	buildCmd.Dir = projectRoot(t)
	out, err := buildCmd.CombinedOutput()
	if err != nil {
		upstream.Close()
		t.Fatalf("go build failed: %s\n%s", err, out)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Brief pause to reduce the chance of port collision between freePort's
	// Listen/Close and the binary binding the same port. This is inherently
	// racy; a future improvement would have the proxy bind to :0 and report
	// its actual address.
	time.Sleep(50 * time.Millisecond)

	console, err := termtest.NewConsole(ctx,
		termtest.WithCommand(binPath,
			"-tui",
			"-upstream", upstream.URL,
			"-bind", "127.0.0.1:"+port,
			"-release-cooldown", "0",
			"-cancel-cooldown", "0",
			"-failure-hold", "0",
			"-retry-min-delay", "0",
		),
		termtest.WithSize(cfg.rows, cfg.cols),
		termtest.WithDefaultTimeout(15*time.Second),
		termtest.WithEnv([]string{"TERM=xterm-256color"}),
	)
	if err != nil {
		cancel()
		upstream.Close()
		t.Fatalf("termtest.NewConsole: %v", err)
	}

	h := &TUIHarness{
		t:         t,
		console:   console,
		upstream:  upstream,
		ctrl:      ctrl,
		ctx:       cancel,
		proxyPort: port,
	}

	snap := console.Snapshot()
	if err := console.Await(ctx, snap, termtest.Contains("shaper")); err != nil {
		h.Close()
		t.Fatalf("TUI did not render: %v\nOutput: %s", err, console.String())
	}

	return h
}

// Console returns the PTY console.
func (h *TUIHarness) Console() *termtest.Console {
	return h.console
}

// Upstream returns the httptest.Server.
func (h *TUIHarness) Upstream() *httptest.Server {
	return h.upstream
}

// Ctrl returns the controllable upstream.
func (h *TUIHarness) Ctrl() *controllableUpstream {
	return h.ctrl
}

// ProxyURL returns the proxy's base URL for sending HTTP requests.
func (h *TUIHarness) ProxyURL() string {
	return "http://127.0.0.1:" + h.proxyPort
}

// Close terminates the TUI and cleans up.
//
// It first sends a "q" keypress to trigger bubbletea's graceful Quit path,
// which lets p.Run() return cleanly and call p.shutdown() before the
// process is killed. This avoids a shutdown race where the deferred
// tuiProgram.Kill() in main.go fires concurrently with p.Run()'s internal
// shutdown, which can corrupt sync.Once inside bubbletea's stopRenderer
// and produce a "sync: unlock of unlocked mutex" panic.
func (h *TUIHarness) Close() {
	// Try to send "q" to trigger a graceful TUI exit. Ignore errors
	// (e.g. PTY already closed) — the force-kill below is the fallback.
	_, _ = h.console.WriteString("q")

	// Give the TUI a brief window to process the quit and exit gracefully.
	// The binary's main.go also waits for tuiDone (up to 3s) before calling
	// Kill(), so this aligns with that timeout.
	exitCtx, exitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer exitCancel()
	_, _ = h.console.WaitExit(exitCtx)

	// Now cancel the context and close everything.
	h.ctx()
	h.console.Close()
	h.upstream.Close()
}

func projectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod")
		}
		dir = parent
	}
}
