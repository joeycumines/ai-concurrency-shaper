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
	"strings"
	"testing"
	"time"
)

func TestPTY_RoutesSorted(t *testing.T) {
	h := Launch(t)
	defer h.Close()

	proxyURL := h.ProxyURL()
	for range 5 {
		sendRequest(t, t.Context(), proxyURL+"/v1/messages")
	}
	for range 3 {
		sendRequest(t, t.Context(), proxyURL+"/v1/chat/completions")
	}
	sendRequest(t, t.Context(), proxyURL+"/embeddings")
	for range 3 {
		sendRequest(t, t.Context(), proxyURL+"/audio/speech")
	}

	time.Sleep(2 * time.Second)

	if _, err := h.Console().WriteString("6"); err != nil {
		t.Fatalf("WriteString 6: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	out := h.Console().String()

	if !strings.Contains(out, "/v1/messages") {
		t.Error("Routes tab should show /v1/messages")
	}
	if !strings.Contains(out, "/v1/chat/completions") {
		t.Error("Routes tab should show /v1/chat/completions")
	}
	if !strings.Contains(out, "/embeddings") {
		t.Error("Routes tab should show /embeddings")
	}

	// Verify sort order: messages(5) > audio/speech(3) > chat/completions(3) > embeddings(1)
	// audio/speech comes before chat/completions alphabetically
	msgIdx := strings.Index(out, "/v1/messages")
	audioIdx := strings.Index(out, "/audio/speech")
	chatIdx := strings.Index(out, "/v1/chat/completions")
	embIdx := strings.Index(out, "/embeddings")

	if msgIdx < 0 || audioIdx < 0 || chatIdx < 0 || embIdx < 0 {
		t.Fatal("missing route entries")
	}

	if msgIdx > audioIdx {
		t.Error("messages (count=5) should appear before audio/speech (count=3)")
	}
	if audioIdx > chatIdx {
		t.Error("audio/speech (count=3, alpha-first) should appear before chat/completions (count=3)")
	}
	if chatIdx > embIdx {
		t.Error("chat/completions (count=3) should appear before embeddings (count=1)")
	}
}
