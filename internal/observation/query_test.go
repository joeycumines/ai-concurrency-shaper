package observation

import (
	"context"
	"errors"
	"testing"
	"time"
)

func interval(start time.Time, seq uint64, seconds int64) Interval {
	d := time.Duration(seconds) * time.Second
	return Interval{Samples: 1, Epoch: "epoch", Sequence: seq, LastSequence: seq, Provider: "north", Start: start, End: start.Add(d), ElapsedNS: int64(d), Counts: Counters{Received: uint64(seconds), Terminal: uint64(seconds)}}
}
func TestQueryDoesNotProrateEdgesOrBridgeGap(t *testing.T) {
	start := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	rows := []Interval{interval(start, 1, 10), interval(start.Add(20*time.Second), 2, 10)}
	q := Query{From: start, To: start.Add(30 * time.Second), MaxPoints: 1}
	r, err := Aggregate(context.Background(), q, rows)
	if err != nil {
		t.Fatal(err)
	}
	p := r.Points[0]
	if p.Counts.Terminal != 20 || !p.Gap || p.Complete || p.ElapsedNS != 0 || p.Coverage < .66 || p.Coverage > .67 {
		t.Fatalf("gap became observed zero: %+v", p)
	}
	q.From = start.Add(5 * time.Second)
	r, err = Aggregate(context.Background(), q, rows)
	if err != nil {
		t.Fatal(err)
	}
	p = r.Points[0]
	if p.Counts.Terminal != 10 || !p.Partial {
		t.Fatalf("partial interval was prorated: %+v", p)
	}
}
func TestQueryEpochOverlapRetainsCountsButNotRate(t *testing.T) {
	start := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	a, b := interval(start, 1, 10), interval(start, 1, 10)
	b.Epoch = "restarted"
	r, err := Aggregate(context.Background(), Query{From: start, To: start.Add(10 * time.Second), MaxPoints: 1}, []Interval{a, b})
	if err != nil {
		t.Fatal(err)
	}
	p := r.Points[0]
	if p.Counts.Terminal != 20 || !p.Overlap || p.Complete || p.ElapsedNS != 0 {
		t.Fatalf("overlap=%+v", p)
	}
}
func TestQueryMergedHistogramAndPerProviderAxis(t *testing.T) {
	start := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	var rows []Interval
	for i := range 60 {
		a := interval(start.Add(time.Duration(i)*time.Second), uint64(i+1), 1)
		a.Duration.Observe(time.Millisecond)
		rows = append(rows, a)
		b := a
		b.Provider = "south"
		b.Duration = Histogram{}
		b.Duration.Observe(time.Second)
		rows = append(rows, b)
	}
	r, err := Aggregate(context.Background(), Query{From: start, To: start.Add(time.Minute), MaxPoints: 7}, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Points) > 7 || len(r.ByProvider) != 2 {
		t.Fatalf("axes=%+v", r)
	}
	var count uint64
	for i, p := range r.Points {
		if !p.Complete {
			t.Fatalf("complete observations incomplete: %+v", p)
		}
		count += p.Counts.Terminal
		if p.Start != r.ByProvider[0].Points[i].Start || p.End != r.ByProvider[1].Points[i].End {
			t.Fatal("provider comparisons use different axes")
		}
	}
	if count != 120 {
		t.Fatalf("count=%d", count)
	}
}
func TestQueryLegacyElapsedAndClockStepRemainUnavailable(t *testing.T) {
	start := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	for _, legacy := range []bool{true, false} {
		v := interval(start, 1, 10)
		if legacy {
			v.ElapsedNS = 0
		} else {
			v.Start = start.Add(10 * time.Second)
			v.End = start
			v.ClockStep = true
		}
		r, err := Aggregate(context.Background(), Query{From: start, To: start.Add(10 * time.Second), MaxPoints: 1}, []Interval{v})
		if err != nil {
			t.Fatal(err)
		}
		p := r.Points[0]
		if p.Counts.Terminal != 10 || p.Complete || p.ElapsedNS != 0 {
			t.Fatalf("legacy=%v: %+v", legacy, p)
		}
	}
}
func TestQueryRollupGapRemainsVisible(t *testing.T) {
	start := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	v := interval(start, 1, 60)
	v.Gap = true
	v.ElapsedNS = int64(50 * time.Second)
	r, err := Aggregate(context.Background(), Query{From: start, To: start.Add(time.Minute), MaxPoints: 1}, []Interval{v})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Points[0].Gap || r.Points[0].Complete {
		t.Fatalf("rollup erased internal gap: %+v", r.Points[0])
	}
}
func TestQueryCancellationAndBounds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := Aggregate(ctx, Query{From: start, To: start.Add(time.Second), MaxPoints: 1}, []Interval{interval(start, 1, 1)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	for _, q := range []Query{{}, {From: start, To: start.Add(time.Second), MaxPoints: 1001}, {From: start, To: start.Add(367 * 24 * time.Hour), MaxPoints: 1}} {
		if _, err = ValidateQuery(q); err == nil {
			t.Fatalf("accepted invalid query %+v", q)
		}
	}
}
func TestDurationDrilldownBeyondAttemptDetailRing(t *testing.T) {
	h, c := newTestHub(t, "north")
	_, r := h.Begin(context.Background(), BeginInfo{Provider: "north"})
	for i := range 100 {
		a := r.BeginAttempt(AttemptRetry)
		if i == 45 {
			c.Advance(5 * time.Second)
		} else {
			c.Advance(time.Millisecond)
		}
		a.Finish(OutcomeCompleted)
	}
	r.Finish(FinishInfo{Outcome: OutcomeCompleted})
	lower, upper := int64(2*time.Second), int64(5*time.Second)
	q := RequestQuery{DurationKind: "attempt", DurationMinNS: &lower, DurationMaxNS: &upper}
	result, err := h.Requests(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || !result.Requests[0].AttemptsTruncated {
		t.Fatalf("failed to locate evicted slow attempt: %+v", result)
	}
	q.DurationKind = "admission"
	result, err = h.Requests(context.Background(), q)
	if err != nil || result.Total != 0 {
		t.Fatalf("unobserved queue appeared: %+v %v", result, err)
	}
}
func TestRequestFilterPaginationStableAndImmutable(t *testing.T) {
	h, c := newTestHub(t, "north")
	for range 8 {
		_, r := h.Begin(context.Background(), BeginInfo{Provider: "north", Path: "/v1/messages", Model: "model-x"})
		r.SetFingerprint("stable-feature")
		c.Advance(time.Second)
		r.Finish(FinishInfo{Outcome: OutcomeFailed, Stage: StageConversionRequest})
	}
	q := RequestQuery{Search: "stable-feature", Outcome: OutcomeFailed, Stage: StageConversionRequest, Limit: 3}
	first, err := h.Requests(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	q.Offset = 3
	second, err := h.Requests(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 8 || len(first.Requests) != 3 || !first.Truncated || len(second.Requests) != 3 || first.Requests[2].ID == second.Requests[0].ID {
		t.Fatalf("pagination %+v %+v", first, second)
	}
	first.Requests[0].Model = "mutation"
	q.Offset = 0
	again, _ := h.Requests(context.Background(), q)
	if again.Requests[0].Model != "model-x" {
		t.Fatal("fact export mutates recorder")
	}
}

func TestHistoricalProviderAndResourceScopesAreBounded(t *testing.T) {
	start := time.Now()
	q := Query{From: start, To: start.Add(time.Second), MaxPoints: 1000}
	rows := make([]Interval, MaxProviders+2)
	for i := range rows {
		rows[i] = interval(start, 1, 1)
		rows[i].Provider = sequenceString(uint64(i))
	}
	if _, err := Aggregate(context.Background(), q, rows); !errors.Is(err, ErrQueryScope) {
		t.Fatalf("historical provider scope=%v", err)
	}
	v := interval(start, 1, 1)
	for i := 0; i <= MaxResourcePools; i++ {
		v.Resources = append(v.Resources, ResourceIntegral{Provider: "north", Kind: "route", ID: sequenceString(uint64(i))})
	}
	if _, err := Aggregate(context.Background(), q, []Interval{v}); !errors.Is(err, ErrQueryScope) {
		t.Fatalf("historical resource scope=%v", err)
	}
	q.Providers = []string{rows[0].Provider}
	if result, err := Aggregate(context.Background(), q, rows); err != nil || len(result.ByProvider) != 1 {
		t.Fatalf("narrow provider selection did not recover: %v", err)
	}
}
func TestLargeResourceMergePreservesIdentityAndExactAreas(t *testing.T) {
	start := time.Now()
	a, b := interval(start, 1, 1), interval(start.Add(time.Second), 2, 1)
	for i := range MaxResourcePools {
		g := Gauge{Known: true, Area: float64(time.Second), DurationNS: int64(time.Second), Min: 1, Max: 1, Last: 1}
		a.Resources = append(a.Resources, ResourceIntegral{Provider: "north", Kind: "route", ID: sequenceString(uint64(i)), Occupied: g})
		b.Resources = append(b.Resources, ResourceIntegral{Provider: "north", Kind: "route", ID: sequenceString(uint64(MaxResourcePools - i - 1)), Occupied: g})
	}
	result, err := Aggregate(context.Background(), Query{From: start, To: start.Add(2 * time.Second), MaxPoints: 1}, []Interval{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Points[0].Resources) != MaxResourcePools {
		t.Fatal("resource identities collapsed")
	}
	for _, r := range result.Points[0].Resources {
		if r.Occupied.Area != float64(2*time.Second) || r.Occupied.DurationNS != int64(2*time.Second) {
			t.Fatalf("resource area=%+v", r)
		}
	}
}
