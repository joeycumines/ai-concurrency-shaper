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
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/go-prompt/termtest"
)

func TestPTY_ConcurrencyInFlight(t *testing.T) {
	h := Launch(t)
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Gate upstream deterministically so both requests remain in-flight until
	// the Concurrency tab has rendered them.
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	h.Ctrl().SetGate(entered, release)
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer doRelease()

	// Fire 2 concurrent requests (limited routes).
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			sendRequest(t, ctx, h.ProxyURL()+"/v1/messages")
		})
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("timed out waiting for in-flight requests to reach upstream")
		}
	}

	// Snapshot BEFORE switching to Concurrency tab (key 5).
	snap := h.Console().Snapshot()
	if _, err := h.Console().WriteString("5"); err != nil {
		t.Fatalf("WriteString 5: %v", err)
	}
	if err := h.Console().Expect(ctx, snap, termtest.Contains("POST"), "in-flight POST requests"); err != nil {
		t.Errorf("Concurrency tab should show in-flight POST requests: %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}

	// Release upstream and wait for requests to complete.
	doRelease()
	wg.Wait()
}
