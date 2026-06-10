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
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/joeycumines/go-prompt/termtest"
)

func TestPTY_RequestLogPopulates(t *testing.T) {
	h := Launch(t)
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	proxyURL := h.ProxyURL()
	paths := []string{"/v1/messages", "/v1/chat/completions", "/v1/messages"}
	for _, path := range paths {
		sendRequest(t, ctx, proxyURL+path)
	}

	// Wait for metrics to propagate and TUI to render
	time.Sleep(2 * time.Second)

	snap := h.Console().Snapshot()
	if _, err := h.Console().WriteString("2"); err != nil {
		t.Fatalf("WriteString 2: %v", err)
	}

	if err := h.Console().Expect(ctx, snap, termtest.Contains("POST"), "POST in request log"); err != nil {
		t.Errorf("Requests tab should show POST method after injecting requests: %v", err)
		t.Logf("Full output: %s", h.Console().String())
	}
}

func sendRequest(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if notOK(err) {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if notOK(err) {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	t.Logf("Request %s: status=%d body=%s", url, resp.StatusCode, string(body))
}

func notOK(err error) bool { return err != nil }
