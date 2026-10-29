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

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// A stalled upload that exhausts the queue timeout waits for admission, not
// for the upstream: the journal records when admission ended, so the wait is
// queue time and the timeout answer's time-to-first-byte is small.
func TestProxy_StalledUploadQueueTimeoutJournalsAdmissionWait(t *testing.T) {
	upstreamURL, _ := url.Parse("http://upstream.invalid")
	pat, _ := route.Parse("POST /v1/messages")
	j := journal.New(16, 1<<16)
	const queueTimeout = 100 * time.Millisecond
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithJournal(j),
		WithQueueComments(10*time.Millisecond),
		WithQueueTimeout(queueTimeout),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "POST /v1/messages HTTP/1.1\r\n"+
		"Host: proxy.test\r\n"+
		"Accept: text/event-stream\r\n"+
		"Content-Type: application/json\r\n"+
		"Content-Length: 100\r\n\r\n"+`{"model":"m"`); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response while the upload is stalled: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 queue timeout", resp.StatusCode)
	}

	// The journal entry is written when the exchange finalizes, which can
	// follow the client's read of the response.
	var entry *journal.Entry
	deadline := time.Now().Add(2 * time.Second)
	for {
		if entries := j.Entries(); len(entries) > 0 {
			entry = entries[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no journal entry for the timed-out request")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if entry == nil {
		t.Fatal("no journal entry")
	}
	if entry.Timing.QueueEnd.IsZero() {
		t.Fatal("QueueEnd unset: the admission wait was journalled as zero queue time")
	}
	if qd := entry.Timing.QueueDuration(); qd < queueTimeout/2 {
		t.Fatalf("QueueDuration = %v, want about the queue timeout %v", qd, queueTimeout)
	}
	if ttfb, qd := entry.Timing.TTFB(), entry.Timing.QueueDuration(); ttfb >= qd {
		t.Fatalf("TTFB = %v, want the answer after admission ended (QueueDuration %v) to be quick", ttfb, qd)
	}
}
