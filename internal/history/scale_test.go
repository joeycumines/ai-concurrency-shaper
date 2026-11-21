package history

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

// This drives the same transaction/retention code used by the asynchronous
// writer, in hour-sized fixture batches. Queries use the public two-second cap.
func TestRetentionSeventyThreeHoursAndSevenDayQuery(t *testing.T) {
	s, now, _ := testStore(t)
	fixtureStarted := time.Now()
	start := now.Add(-73 * time.Hour)
	seq := uint64(0)
	for hour := range 73 {
		intervals := make([]observation.Interval, 3600)
		for second := range intervals {
			seq++
			intervals[second] = sampleInterval(start.Add(time.Duration(hour)*time.Hour+time.Duration(second)*time.Second), seq, time.Second)
		}
		commitPacket(t, s, observation.Packet{Intervals: intervals})
	}
	var minutes, hours, raw int
	if err := s.writer.QueryRow("SELECT count(*) FROM rollups WHERE tier=60").Scan(&minutes); err != nil {
		t.Fatal(err)
	}
	if err := s.writer.QueryRow("SELECT count(*) FROM rollups WHERE tier=3600").Scan(&hours); err != nil {
		t.Fatal(err)
	}
	raw = count(t, s, "buckets")
	if minutes != 1440 || hours != 73 || raw != 900 {
		t.Fatalf("retention minute=%d hour=%d raw=%d", minutes, hours, raw)
	}
	t.Logf("73-hour fixture transaction and retention time=%s", time.Since(fixtureStarted))
	for _, window := range []time.Duration{24 * time.Hour, 7 * 24 * time.Hour} {
		q := observation.Query{From: now.Add(-window), To: now, MaxPoints: 120}
		readStarted := time.Now()
		rows, _, truncated, err := s.Read(context.Background(), q, observation.Exclusion{})
		t.Logf("window=%s public read=%s sources=%d", window, time.Since(readStarted), len(rows))
		if err != nil || truncated {
			t.Fatalf("window=%s: %v truncated=%v", window, err, truncated)
		}
		result, err := observation.Aggregate(context.Background(), q, rows)
		if err != nil {
			t.Fatal(err)
		}
		var total uint64
		gap := false
		for _, p := range result.Points {
			total += p.Counts.Terminal
			gap = gap || p.Gap
		}
		expected := uint64(86400)
		sources := 1440
		if window > 24*time.Hour {
			expected = 262800
			sources = 73
		}
		if total != expected || len(rows) != sources || len(result.Points) > 120 || gap != (window > 24*time.Hour) {
			t.Fatalf("window=%s total=%d sources=%d points=%d gap=%v", window, total, len(rows), len(result.Points), gap)
		}
	}
}
func fullPayload(at time.Time, seq uint64) observation.Interval {
	v := sampleInterval(at, seq, time.Minute)
	v.Counts = observation.Counters{Received: 60, Terminal: 60, Completed: 57, Failed: 2, Aborted: 1, Attempts: 67, Retries: 5, AuthResends: 2, Upstream429: 3, ConversionLosses: 3, ConversionNotes: 12, UsageReports: 57, UsageInputKnown: 57, UsageOutputKnown: 57, UsageTotalKnown: 57, UsageInputTokens: 57000, UsageOutputTokens: 5700, UsageTotalTokens: 62700}
	v.Counts.HTTP[0][1] = 57
	v.Counts.HTTP[0][4] = 2
	v.Counts.HTTP[1][1] = 60
	v.Counts.Failures[3] = 2
	for i := range 60 {
		v.Duration.Observe(time.Duration(i%20+1) * time.Second)
		v.Admission.Observe(time.Duration(i%7) * time.Millisecond)
		v.AttemptDuration.Observe(time.Duration(i%10+1) * time.Second)
	}
	g := func(n int64) observation.Gauge {
		return observation.Gauge{Known: true, Area: float64(n) * float64(time.Minute), DurationNS: int64(time.Minute), Min: n, Max: n + 1, Last: n}
	}
	v.Gauges = observation.Gauges{Active: g(3), Queued: g(2), Exchanges: g(2)}
	for _, kind := range []string{"default", "global", "route:POST /v1/messages", "route:POST /v1/responses"} {
		v.Resources = append(v.Resources, observation.ResourceIntegral{Provider: "north", ID: kind, Kind: resourceKind(kind), Limit: g(8), Occupied: g(3), Cooldown: g(1), Queued: g(2)})
	}
	return v
}
func TestFullPayloadDayQueryWithinPublicBudget(t *testing.T) {
	s, now, _ := testStore(t)
	fixtureStarted := time.Now()
	start := now.Add(-24 * time.Hour)
	intervals := make([]observation.Interval, 1440)
	for i := range intervals {
		intervals[i] = fullPayload(start.Add(time.Duration(i)*time.Minute), uint64(i+1))
	}
	commitPacket(t, s, observation.Packet{Intervals: intervals})
	var bytes int
	if err := s.writer.QueryRow("SELECT sum(length(payload)) FROM rollups WHERE tier=60").Scan(&bytes); err != nil {
		t.Fatal(err)
	}
	if bytes < 4<<20 {
		t.Fatalf("fixture is too sparse to exercise full payload: %d bytes", bytes)
	}
	t.Logf("full-payload fixture transaction time=%s", time.Since(fixtureStarted))
	q := observation.Query{From: start, To: now, MaxPoints: 120}
	readStarted := time.Now()
	rows, _, truncated, err := s.Read(context.Background(), q, observation.Exclusion{})
	t.Logf("full-payload public read=%s sources=%d", time.Since(readStarted), len(rows))
	if err != nil || truncated {
		t.Fatalf("public full-payload query failed: %v truncated=%v", err, truncated)
	}
	if len(rows) != 1440 {
		t.Fatalf("selected sources=%d", len(rows))
	}
	result, err := observation.Aggregate(context.Background(), q, rows)
	if err != nil {
		t.Fatal(err)
	}
	var total, count uint64
	var elapsed int64
	for _, p := range result.Points {
		total += p.Counts.Terminal
		count += p.Duration.Count
		elapsed += p.ElapsedNS
		if !p.Complete || p.Gap || p.Overlap || p.ClockStep {
			t.Fatalf("full payload coverage=%+v", p)
		}
		mean, ok := p.Gauges.Active.Mean()
		if !ok || mean != 3 {
			t.Fatalf("exact active mean=%v %v", mean, ok)
		}
	}
	if total != 86400 || count != 86400 || elapsed != int64(24*time.Hour) || len(result.Points) != 120 {
		t.Fatalf("totals=%d histogram=%d elapsed=%s points=%d", total, count, time.Duration(elapsed), len(result.Points))
	}
	t.Logf("decoded %d bytes / %d source rows to %d complete points", bytes, len(rows), len(result.Points))
}
func TestRequestRetentionLatestTenThousand(t *testing.T) {
	s, now, _ := testStore(t)
	facts := make([]observation.RequestFact, 10010)
	for i := range facts {
		at := now.Add(time.Duration(i-10010) * time.Millisecond)
		facts[i] = observation.RequestFact{ID: fmt.Sprintf("request-%05d", i), Provider: "north", Started: at, Finished: at, Outcome: observation.OutcomeCompleted}
	}
	commitPacket(t, s, observation.Packet{Requests: facts})
	result, err := s.Requests(context.Background(), observation.RequestQuery{Limit: 3, Offset: 9997})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 10000 || len(result.Requests) != 3 || result.Requests[2].ID != "request-00010" || result.Truncated {
		t.Fatalf("bounded retention=%+v", result)
	}
}
func TestDecodeFullPayloadCancellationReturnsWorkers(t *testing.T) {
	raw, err := json.Marshal(fullPayload(time.Now(), 1))
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]encodedRow, 10000)
	for i := range rows {
		rows[i] = encodedRow{tier: 60, data: raw}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := decodeRows(ctx, rows); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled full-payload decode reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decode workers did not stop on cancellation")
	}
}

func resourceKind(s string) string {
	if len(s) >= 6 && s[:6] == "route:" {
		return "route"
	}
	return s
}
