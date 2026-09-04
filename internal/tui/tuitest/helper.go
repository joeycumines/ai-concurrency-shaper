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

// Package tuitest provides PTY-based integration tests for the TUI dashboard.
// The PTY console APIs (termtest.Console) are unix-only; every test file in
// this package carries the same constraint.
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
	args []string
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

// WithArgs appends extra command-line arguments to the launched binary, after
// the harness's own flags. It lets a test exercise configurations the default
// Launch cannot express (e.g. additional server-scope flags).
func WithArgs(args ...string) HarnessOption {
	return func(c *harnessConfig) {
		c.args = append(c.args, args...)
	}
}

// TUIHarness manages a running TUI instance in a PTY.
type TUIHarness struct {
	t         *testing.T
	console   *termtest.Console
	upstream  *httptest.Server
	upstreams []*httptest.Server
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

// buildBinary compiles the ai-concurrency-shaper binary into a per-test temp
// directory and returns its path.
func buildBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "test-shaper")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	buildCmd.Dir = projectRoot(t)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %s\n%s", err, out)
	}
	return binPath
}

// Launch builds the ai-concurrency-shaper binary, starts it with -tui in a PTY
// against a single controllable upstream, and waits for the initial render.
func Launch(t *testing.T, opts ...HarnessOption) *TUIHarness {
	t.Helper()

	cfg := &harnessConfig{rows: 40, cols: 120}
	for _, o := range opts {
		o(cfg)
	}

	// Build before allocating any test resources so a compile failure leaves
	// nothing to clean up.
	binPath := buildBinary(t)

	ctrl := newControllableUpstream()
	upstream := httptest.NewServer(ctrl)

	args := []string{
		"-tui",
		"-upstream", upstream.URL,
		"-release-cooldown", "0",
		"-cancel-cooldown", "0",
		"-failure-hold", "0",
		"-retry-min-delay", "0",
	}

	return startConsole(t, binPath, upstream, nil, ctrl, "shaper", args, cfg)
}

// LaunchMulti builds the binary and starts it with -tui in a PTY serving one
// --provider section per upstream. Each provider mounts at /prov<i> and is
// named prov<i>, so tests can address them deterministically in both the
// header chips and the proxy URLs. The fast-cooldown flags are repeated inside
// every provider section (they are provider-scoped). Extra args from WithArgs
// are appended after the provider sections. The launch awaits the first
// provider's name, since the multi-provider header shows it in place of
// "shaper".
func LaunchMulti(t *testing.T, upstreams []*httptest.Server, opts ...HarnessOption) *TUIHarness {
	t.Helper()

	cfg := &harnessConfig{rows: 40, cols: 120}
	for _, o := range opts {
		o(cfg)
	}

	if len(upstreams) == 0 {
		t.Fatal("LaunchMulti requires at least one upstream")
	}

	binPath := buildBinary(t)

	args := []string{"-tui"}
	for i, up := range upstreams {
		args = append(args,
			fmt.Sprintf("--provider=prov%d", i+1),
			"-upstream", up.URL,
			"-prefix", fmt.Sprintf("/prov%d", i+1),
			"-release-cooldown", "0",
			"-cancel-cooldown", "0",
			"-failure-hold", "0",
			"-retry-min-delay", "0",
			"-retry", "0",
			"-circuit-breaker=false",
		)
	}

	// Multi-provider mode has no single controllable upstream: the caller
	// owns every upstream server. The harness's ctrl field is left nil;
	// multi-provider tests that need to hold requests in flight pass their
	// own delayed/gated httptest servers.
	return startConsole(t, binPath, upstreams[0], upstreams, nil, "prov1", args, cfg)
}

// startConsole binds a free proxy port, starts the binary in a PTY with the
// given args, and waits until the header renders the awaited name. It owns
// every resource it allocates: on any failure it cleans them up before
// failing the test.
func startConsole(t *testing.T, binPath string, upstream *httptest.Server, upstreams []*httptest.Server, ctrl *controllableUpstream, awaitName string, args []string, cfg *harnessConfig) *TUIHarness {
	t.Helper()

	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// -bind is a server-scope flag: in sectioned (multi-provider) mode it
	// must come BEFORE the first --provider section, or the parser rejects
	// it as a stray flag inside the last provider section. Prefixing keeps
	// the flat single-provider invocation equivalent.
	fullArgs := append([]string{"-bind", "127.0.0.1:" + port}, args...)
	fullArgs = append(fullArgs, cfg.args...)

	// Brief pause to reduce the chance of port collision between freePort's
	// Listen/Close and the binary binding the same port. This is inherently
	// racy; a future improvement would have the proxy bind to :0 and report
	// its actual address.
	time.Sleep(50 * time.Millisecond)

	console, err := termtest.NewConsole(ctx,
		termtest.WithCommand(binPath, fullArgs...),
		termtest.WithSize(cfg.rows, cfg.cols),
		termtest.WithDefaultTimeout(15*time.Second),
		termtest.WithEnv([]string{"TERM=xterm-256color"}),
	)
	if err != nil {
		cancel()
		upstream.Close()
		for _, up := range upstreams {
			up.Close()
		}
		t.Fatalf("termtest.NewConsole: %v", err)
	}

	h := &TUIHarness{
		t:         t,
		console:   console,
		upstream:  upstream,
		upstreams: upstreams,
		ctrl:      ctrl,
		ctx:       cancel,
		proxyPort: port,
	}

	snap := console.Snapshot()
	if err := console.Await(ctx, snap, termtest.Contains(awaitName)); err != nil {
		h.Close()
		t.Fatalf("TUI did not render %q: %v\nOutput: %s", awaitName, err, console.String())
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

// ProviderURL returns the proxy URL with provider i's mount prefix applied,
// for sending requests to a specific provider in multi-provider mode.
func (h *TUIHarness) ProviderURL(i int) string {
	return fmt.Sprintf("http://127.0.0.1:%s/prov%d", h.proxyPort, i+1)
}

// Close terminates the TUI and cleans up.
//
// It sends Ctrl+C to trigger a graceful TUI exit, then waits for the process
// to exit. Ctrl+C is preferred over "q" because it cannot be swallowed by a
// focused text input. The 8-second budget clears the binary's 5-second
// graceful shutdown drain, avoiding a force-kill that would skip terminal
// restoration.
func (h *TUIHarness) Close() {
	// Send Ctrl+C to trigger a graceful TUI exit. Ignore errors (e.g. PTY
	// already closed) — the force-kill below is the fallback.
	_, _ = h.console.WriteString("\x03")

	// The binary's graceful shutdown can take up to 5s (srv.Shutdown drain),
	// so wait longer than that before force-killing.
	exitCtx, exitCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer exitCancel()
	_, _ = h.console.WaitExit(exitCtx)

	// Now cancel the context and close everything.
	h.ctx()
	h.console.Close()
	for _, up := range h.upstreams {
		up.Close()
	}
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
