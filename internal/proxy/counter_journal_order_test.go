package proxy

// Autopsy 2026-09-06 M10: the clean-completion counters (TotalProxied /
// TotalPassThrough) and the status buckets published BEFORE the journal
// entry — the same counter-before-journal shape c9ff856 fixed for the
// aborted pair. A consumer that observes the counter must find the journal
// entry already present. The poll-then-assert consumer pattern (the shape
// that exposed the aborted window under -race stress) pins the ordering.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

func TestProxy_CleanCountersPublishAfterJournalEntry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		method  string
		path    string
		limited bool
		counter func(s metrics.Snapshot) int64
	}{
		{name: "proxied", method: http.MethodPost, path: "/v1/messages", limited: true,
			counter: func(s metrics.Snapshot) int64 { return s.TotalProxied }},
		{name: "passthrough", method: http.MethodGet, path: "/health", limited: false,
			counter: func(s metrics.Snapshot) int64 { return s.TotalPassThrough }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Production capacity: main.go derives 51 entries from the
			// default 5 MiB retry body budget ((512 MiB)/(5 MiB*2)).
			// The invariant must hold under eviction, so the test pins
			// it at that capacity instead of an oversized ring.
			const journalCap = 51
			j := journal.New(journalCap, 5<<20)
			met := metrics.NewCollector()
			pat, err := route.Parse("POST /v1/messages")
			if err != nil {
				t.Fatal(err)
			}
			opts := []Option{
				WithUpstream(upstreamURL),
				WithMatcher(route.NewMatcher([]route.Pattern{pat})),
				WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				WithMetrics(met),
				WithJournal(j),
			}
			p, err := New(opts...)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			// A consumer that polls the counter and immediately reads the
			// journal must never observe the counter without the entry. The
			// pre-fix window (counter first, journal later) made this
			// assertion flaky under -race; it is deterministic now.
			var wg sync.WaitGroup
			for range 32 {
				wg.Go(func() {
					for range 25 {
						rec := httptest.NewRecorder()
						req := httptest.NewRequest(tc.method, tc.path, nil)
						p.ServeHTTP(rec, req)
						if rec.Code != http.StatusOK {
							t.Errorf("status = %d, want 200", rec.Code)
							return
						}
						snap := met.Snapshot()
						if tc.counter(snap) < 1 {
							t.Errorf("counter = %d before any completion", tc.counter(snap))
							return
						}
						entries := j.Entries()
						got := tc.counter(snap)
						wantLen := int(got)
						if wantLen > journalCap {
							wantLen = journalCap
						}
						if len(entries) < wantLen {
							t.Errorf("journal entries = %d < min(counter = %d, cap = %d): counter published before the journal entry (M10)", len(entries), got, journalCap)
							return
						}
					}
				})
			}
			wg.Wait()

			snap := met.Snapshot()
			entries := j.Entries()
			const totalRequests = 32 * 25
			if got := tc.counter(snap); got != totalRequests {
				t.Fatalf("final: counter = %d, want %d", got, totalRequests)
			}
			if len(entries) != journalCap {
				t.Fatalf("final: journal entries = %d, want cap %d (ring evicts under production capacity)", len(entries), journalCap)
			}
			for _, entry := range entries {
				if entry.Aborted {
					t.Fatalf("clean exchange journalized as aborted: %+v", entry)
				}
			}
			_ = time.Now
		})
	}
}
