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
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// parkedServer returns a live http.Server on a loopback ephemeral port whose
// handler signals entered once it starts and parks until release is closed.
type parkedServer struct {
	srv     *http.Server
	addr    string
	entered chan struct{}
	release chan struct{}
}

func newParkedServer(t *testing.T) *parkedServer {
	t.Helper()
	ps := &parkedServer{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ps.addr = ln.Addr().String()
	ps.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-ps.entered:
		default:
			close(ps.entered)
		}
		<-ps.release // park: the connection stays non-idle until released
	})}
	go func() { _ = ps.srv.Serve(ln) }() //nolint:errcheck // closed via Shutdown below
	t.Cleanup(func() {
		select {
		case <-ps.release:
		default:
			close(ps.release)
		}
		_ = ps.srv.Close()
	})
	return ps
}

func waitParked(t *testing.T, ps *parkedServer) {
	t.Helper()
	select {
	case <-ps.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("server %s handler never parked", ps.addr)
	}
}

// fire sends one request whose response stays parked until release closes.
// It runs on a detached goroutine with its own client: the call outlives
// t.Context() cancellation and ends when the server drains or is closed, so
// errors are expected during teardown and deliberately discarded.
func fire(ps *parkedServer) {
	go func() {
		resp, err := (&http.Client{}).Get("http://" + ps.addr + "/")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
}

// TestShutdownServersDrainsBothUnderStall pins the fix for the review finding
// that sequentially shutting the metrics and proxy servers down against ONE
// shared context lets the first consumer starve the second: stdlib answers an
// already-expired context after a single idle-conn poll, abandoning the
// proxy's active connections instead of draining them.
//
// The metrics server stalls for most of the grace window; the proxy server's
// parked connection must STILL be drained gracefully by its own full window.
// Under the old sequential shared-context shape the proxy Shutdown provably
// received context.DeadlineExceeded while its request was still parked
// (TestSequentialShutdownStarvesSecondServer demonstrates that shape).
func TestShutdownServersDrainsBothUnderStall(t *testing.T) {
	metricsSrv := newParkedServer(t)
	proxySrv := newParkedServer(t)
	fire(metricsSrv)
	fire(proxySrv)

	// Parked state must be established BEFORE shutdown starts: Shutdown
	// closes listeners immediately, which would otherwise race the dial.
	waitParked(t, metricsSrv)
	waitParked(t, proxySrv)

	const grace = 750 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- shutdownServers(grace, metricsSrv.srv, proxySrv.srv) }()

	// Release inside their windows: metrics early, proxy late-but-safe.
	time.Sleep(150 * time.Millisecond)
	close(metricsSrv.release)
	time.Sleep(200 * time.Millisecond)
	close(proxySrv.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdownServers = %v, want both servers drained", err)
		}
	case <-time.After(grace + 3*time.Second):
		t.Fatal("shutdownServers did not finish within the grace window")
	}
}

// TestSequentialShutdownStarvesSecondServer demonstrates the defect the
// concurrent helper replaces: with ONE shared context, a stalled first server
// exhausts the budget and the second server's Shutdown inherits an expired
// context - returning immediately with context.DeadlineExceeded while its own
// connection is still active and undrained.
func TestSequentialShutdownStarvesSecondServer(t *testing.T) {
	first := newParkedServer(t)
	second := newParkedServer(t)
	fire(first)
	fire(second)
	waitParked(t, first)
	waitParked(t, second)

	// Nobody releases either handler during the window: the first Shutdown
	// can only end at the deadline.
	const grace = 300 * time.Millisecond
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	if err := first.srv.Shutdown(sctx); err != context.DeadlineExceeded {
		t.Fatalf("first Shutdown = %v, want DeadlineExceeded (stalled handler)", err)
	}
	start := time.Now()
	err := second.srv.Shutdown(sctx)
	elapsed := time.Since(start)
	if err != context.DeadlineExceeded {
		t.Fatalf("second Shutdown = %v, want DeadlineExceeded from the inherited dead context", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("second Shutdown took %v; an expired context should be answered immediately", elapsed)
	}
	// The second server's connection was abandoned rather than drained: its
	// handler is still parked at this point (release happens in t.Cleanup),
	// which is exactly the client-severing behavior under review.
}
