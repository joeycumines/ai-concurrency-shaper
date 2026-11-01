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

package proxy

// Which requests the opt-in SSE queue-comment commit is allowed to represent.
// The commit replaces the response the upstream would have produced, so it is
// only ever eligible for an ordinary method whose response is a body-bearing
// HTTP message: a HEAD response has no body to carry comments, and a
// protocol-upgrade exchange switches the connection to another protocol
// instead of streaming HTTP.

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

func TestQueueCommentsEligible(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		header http.Header
		want   bool
	}{
		{name: "post", method: http.MethodPost, want: true},
		{name: "get", method: http.MethodGet, want: true},
		{name: "head", method: http.MethodHead},
		{
			name:   "upgrade",
			method: http.MethodPost,
			header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}},
		},
		{
			name:   "upgrade token among others",
			method: http.MethodPost,
			header: http.Header{"Connection": {"keep-alive, Upgrade"}, "Upgrade": {"h2c"}},
		},
		{
			name:   "upgrade header without the connection token",
			method: http.MethodPost,
			header: http.Header{"Upgrade": {"websocket"}},
			want:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/v1/messages", nil)
			for name, values := range tc.header {
				req.Header[name] = values
			}
			if got := queueCommentsEligible(req); got != tc.want {
				t.Fatalf("queueCommentsEligible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProxy_QueueCommentsSkipHead(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "11")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimitAll(true),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueComments(20*time.Millisecond),
		WithQueueTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodHead, srv.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); strings.Contains(got, "text/event-stream") {
		t.Fatalf("HEAD response was committed as an event stream (Content-Type %q), want the upstream's own representation", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestProxy_QueueCommentsSkipProtocolUpgrade(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			t.Errorf("upstream Upgrade header = %q, want websocket", r.Header.Get("Upgrade"))
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream writer is not a http.Hijacker")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimitAll(true),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueComments(20*time.Millisecond),
		WithQueueTimeout(10*time.Second),
		WithMaxRetries(0),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	req := "POST /v1/messages HTTP/1.1\r\n" +
		"Host: " + strings.TrimPrefix(srv.URL, "http://") + "\r\n" +
		"Accept: text/event-stream\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Content-Length: 0\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101 (the upgrade must not be turned into a committed SSE stream)", resp.StatusCode)
	}
}
