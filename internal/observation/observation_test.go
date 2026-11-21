package observation

import (
	"bytes"
	"context"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); c.at = c.at.Add(d); c.mu.Unlock() }
func newTestHub(t *testing.T, providers ...string) (*Hub, *testClock) {
	t.Helper()
	c := &testClock{at: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}
	h := New(Config{Providers: providers, Now: c.Now, ManualTicks: true})
	t.Cleanup(func() {
		if err := h.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return h, c
}
func provider(t *testing.T, h *Hub, name string) ProviderSnapshot {
	t.Helper()
	for _, p := range h.Snapshot().Providers {
		if p.Provider == name {
			return p
		}
	}
	t.Fatalf("missing provider %s", name)
	return ProviderSnapshot{}
}

func TestRequestAttemptsBodyOutcomeAndQueueUnion(t *testing.T) {
	h, c := newTestHub(t, "north")
	ctx, r := h.Begin(context.Background(), BeginInfo{Provider: "north", Method: "POST", Path: "/v1/messages"})
	if FromContext(ctx) != r {
		t.Fatal("context identity missing")
	}
	_, same := h.Begin(ctx, BeginInfo{Provider: "other"})
	if same != r {
		t.Fatal("nested handler created a second request")
	}
	r.Queue(true)
	c.Advance(2 * time.Second)
	r.Queue(true)
	c.Advance(3 * time.Second)
	r.Queue(false)
	c.Advance(2 * time.Second)
	r.Queue(false)
	a := r.BeginAttempt(AttemptInitial)
	a.Headers(503)
	a.Headers(200)
	c.Advance(time.Second)
	a.Finish(OutcomeFailed)
	retry := r.BeginAttempt(AttemptRetry)
	retry.Headers(401)
	c.Advance(time.Second)
	retry.Finish(OutcomeFailed)
	auth := r.BeginAttempt(AttemptAuthResend)
	auth.Headers(200)
	c.Advance(3 * time.Second)
	auth.Finish(OutcomeFailed)
	r.Finish(FinishInfo{Status: 200, Outcome: OutcomeFailed, Stage: StageConversionResponse})
	r.Finish(FinishInfo{Status: 200, Outcome: OutcomeCompleted})
	p := provider(t, h, "north")
	if p.Totals.Received != 1 || p.Totals.Terminal != 1 || p.Totals.Completed != 0 || p.Totals.Failed != 1 || p.Totals.Attempts != 3 || p.Totals.Retries != 1 || p.Totals.AuthResends != 1 {
		t.Fatalf("counts=%+v", p.Totals)
	}
	if p.Totals.HTTP[0][1] != 1 || p.Totals.HTTP[1][1] != 1 || p.Totals.HTTP[1][3] != 1 || p.Totals.HTTP[1][4] != 1 || p.Totals.Upstream401 != 1 || p.Totals.Upstream5xx != 1 {
		t.Fatalf("statuses=%+v", p.Totals)
	}
	if p.Active != 0 || p.Queued != 0 || p.Exchanges != 0 {
		t.Fatalf("leaked gauges=%+v", p)
	}
	f := r.Snapshot()
	if f.QueueNS != int64(7*time.Second) || f.DurationNS != int64(12*time.Second) || len(f.Attempts) != 3 || f.Attempts[2].Outcome != OutcomeFailed {
		t.Fatalf("fact=%+v", f)
	}
	if p.Admission.Count != 1 || p.Admission.Sum != 7 || p.AttemptDuration.Sum != 5 {
		t.Fatalf("histograms=%+v", p)
	}
	if !r.IsFinished() {
		t.Fatal("not finished")
	}
}
func TestLocalRejectionDoesNotInventUpstream(t *testing.T) {
	h, _ := newTestHub(t)
	_, r := h.Begin(context.Background(), BeginInfo{Path: "/missing"})
	r.Finish(FinishInfo{Status: 404, Outcome: OutcomeRejected, Stage: StageProxy})
	p := provider(t, h, UnresolvedProvider)
	if p.Totals.Attempts != 0 || p.Totals.HTTP[1] != [5]uint64{} || p.Totals.HTTP[0][3] != 1 || p.Duration.Count != 1 {
		t.Fatalf("local status conflated with upstream: %+v", p)
	}
}
func TestHandoffPreservesIngressAndTransfersOccupancy(t *testing.T) {
	h, c := newTestHub(t, "north")
	start := c.Now()
	_, r := h.Begin(context.Background(), BeginInfo{})
	r.Queue(true)
	c.Advance(3 * time.Second)
	r.SetProvider("north")
	c.Advance(2 * time.Second)
	h.Tick()
	live, liveErr := h.Requests(context.Background(), RequestQuery{LiveOnly: true})
	if liveErr != nil || len(live.Requests) != 1 || live.Requests[0].DurationNS != int64(5*time.Second) || live.Requests[0].QueueNS != int64(5*time.Second) {
		t.Fatalf("live fact durations=%+v %v", live, liveErr)
	}
	u := provider(t, h, UnresolvedProvider)
	n := provider(t, h, "north")
	if u.Totals.Received != 1 || n.Totals.Received != 0 || n.Active != 1 || n.Queued != 1 || u.Active != 0 || u.Queued != 0 {
		t.Fatalf("handoff: unresolved=%+v north=%+v", u, n)
	}
	result, err := h.Query(context.Background(), Query{From: start, To: c.Now(), MaxPoints: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Points) != 1 || !result.Points[0].Complete {
		t.Fatalf("coverage=%+v", result)
	}
	mean, ok := result.Points[0].Gauges.Active.Mean()
	if !ok || mean != 1 {
		t.Fatalf("fleet active mean=%v %v", mean, ok)
	}
	r.Queue(false)
	r.Finish(FinishInfo{Status: 200, Outcome: OutcomeCompleted})
	if provider(t, h, "north").Totals.Terminal != 1 {
		t.Fatal("terminal did not follow provider")
	}
}
func TestUsageKnownZeroCorrectionsAndOverflow(t *testing.T) {
	h, _ := newTestHub(t, "north")
	_, r := h.Begin(context.Background(), BeginInfo{Provider: "north"})
	r.ReportUsage(Usage{InputKnown: true, Input: 0, OutputKnown: false, TotalKnown: true, Total: 0, Source: "first"})
	r.ReportUsage(Usage{InputKnown: true, TotalKnown: true, Source: "renamed"})
	r.ReportUsage(Usage{InputKnown: true, OutputKnown: true, Output: 2, TotalKnown: true, Total: 2})
	r.ReportUsage(Usage{InputKnown: true, OutputKnown: true, Output: 2, TotalKnown: true, Total: 2})
	p := provider(t, h, "north")
	if p.Totals.UsageReports != 1 || p.Totals.UsageInputKnown != 1 || p.Totals.UsageOutputKnown != 0 || p.Totals.UsageInputTokens != 0 || p.Totals.UsageCorrections != 1 || p.Totals.UsageOutputTokens != 0 {
		t.Fatalf("usage=%+v", p.Totals)
	}
	f := r.Snapshot()
	if f.Usage == nil || !f.Usage.OutputKnown || f.Usage.Output != 2 || f.UsageCorrections != 1 {
		t.Fatalf("corrected fact=%+v", f)
	}
	h.mu.Lock()
	h.providers["north"].totals.UsageInputTokens = math.MaxUint64
	h.mu.Unlock()
	_, r2 := h.Begin(context.Background(), BeginInfo{Provider: "north"})
	r2.ReportUsage(Usage{InputKnown: true, Input: 1, OutputKnown: true, Output: 4})
	p = provider(t, h, "north")
	if p.Totals.UsageOverflow != 1 || p.Totals.UsageInputTokens != math.MaxUint64 || p.Totals.UsageOutputTokens != 4 {
		t.Fatalf("overflow not explicit: %+v", p.Totals)
	}
}
func TestImmutableBoundedDetailsAndCancellation(t *testing.T) {
	h, c := newTestHub(t, "north")
	ctx, r := h.Begin(context.Background(), BeginInfo{Provider: "north"})
	for range 100 {
		a := r.BeginAttempt(AttemptRetry)
		a.Headers(200)
		c.Advance(time.Millisecond)
		a.Finish(OutcomeCompleted)
	}
	f := r.Snapshot()
	if len(f.Attempts) != 32 || !f.AttemptsTruncated || f.AttemptCount != 100 || f.Attempts[31].Number != 100 {
		t.Fatalf("attempt bound=%+v", f)
	}
	f.Attempts[0].Status = 999
	r.ReportUsage(Usage{InputKnown: true, Input: 4})
	f = r.Snapshot()
	f.Usage.Input = 999
	if r.Snapshot().Usage.Input != 4 || r.Snapshot().Attempts[0].Status != 200 {
		t.Fatal("mutable DTO escaped")
	}
	if !h.Cancel(r.ID()) {
		t.Fatal("cancel failed")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancel did not reach request context")
	}
	r.Finish(FinishInfo{Outcome: OutcomeAborted})
	if h.Cancel(r.ID()) || !r.Snapshot().Cancelled {
		t.Fatal("cancel identity lifecycle incorrect")
	}
	if provider(t, h, "north").Totals.Cancelled != 1 {
		t.Fatal("cancellation not counted")
	}
}
func TestConcurrentRequestIDsAndSnapshots(t *testing.T) {
	h, _ := newTestHub(t, "north", "south")
	var wg sync.WaitGroup
	ids := make(chan string, 300)
	for n := range 300 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, r := h.Begin(context.Background(), BeginInfo{Provider: "north"})
			ids <- r.ID()
			r.Queue(true)
			r.SetProvider("south")
			r.Queue(false)
			a := r.BeginAttempt(AttemptInitial)
			a.Headers(200)
			a.Finish(OutcomeCompleted)
			_ = h.Snapshot()
			r.Finish(FinishInfo{Status: 200, Outcome: OutcomeCompleted})
		}(n)
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatal("ID reused")
		}
		seen[id] = true
	}
	if len(seen) != 300 || provider(t, h, "south").Totals.Completed != 300 || provider(t, h, "north").Totals.Received != 300 {
		t.Fatal("concurrent observations lost")
	}
}
func TestResourcesAreSeparateAndTransitionIntegrated(t *testing.T) {
	h, c := newTestHub(t, "north", "south")
	start := c.Now()
	h.SetResourceAt(start, Resource{Provider: "north", Kind: "global", ID: "global", Limit: 4, Occupied: 2, Cooldown: 1, Known: true})
	h.SetResourceAt(start, Resource{Provider: "north", Kind: "default", ID: "default", Limit: 2, Occupied: 1, Known: true})
	h.SetResourceAt(start, Resource{Provider: "south", Kind: "global", ID: "global", Known: false})
	c.Advance(2 * time.Second)
	h.SetResourceAt(c.Now(), Resource{Provider: "north", Kind: "global", ID: "global", Limit: 1, Occupied: 2, Cooldown: 1, Known: true})
	c.Advance(2 * time.Second)
	h.Tick()
	q, err := h.Query(context.Background(), Query{From: start, To: c.Now(), MaxPoints: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Points[0].Resources) != 3 {
		t.Fatalf("pools merged: %+v", q.Points[0].Resources)
	}
	for _, r := range q.Points[0].Resources {
		if r.Provider == "north" && r.Kind == "global" {
			mean, ok := r.Limit.Mean()
			if !ok || mean != 2.5 {
				t.Fatalf("limit=%v %v", mean, ok)
			}
		}
		if r.Provider == "south" && r.Limit.Known {
			t.Fatal("unknown capacity became zero")
		}
	}
}
func TestHistogramMergeBeforeQuantile(t *testing.T) {
	var fast, slow Histogram
	for range 99 {
		fast.Observe(time.Millisecond)
	}
	slow.Observe(10 * time.Second)
	fast.Add(slow)
	lo, hi, ok := fast.Quantile(.95)
	if !ok || lo != 0 || hi != .001 {
		t.Fatalf("p95=%v..%v %v", lo, hi, ok)
	}
	if _, _, ok = fast.Quantile(math.NaN()); ok {
		t.Fatal("NaN quantile accepted")
	}
}
func TestPrometheusSnapshotDoesNotConsumeAndHasBoundedLabels(t *testing.T) {
	h, c := newTestHub(t, "north")
	_, r := h.Begin(context.Background(), BeginInfo{Provider: "north", Path: "/secret/model/path", Model: "arbitrary-model"})
	a := r.BeginAttempt(AttemptInitial)
	a.Headers(429)
	c.Advance(time.Second)
	a.Finish(OutcomeFailed)
	r.Finish(FinishInfo{Status: 200, Outcome: OutcomeFailed, Stage: StageUpstream})
	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(h.PrometheusCollector()); err != nil {
		t.Fatal(err)
	}
	aFamilies, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	bFamilies, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(aFamilies) != 44 || len(bFamilies) != 44 {
		t.Fatalf("families before committed watermark=%d", len(aFamilies))
	}
	if h.seq != 0 || len(h.recent) != 1 || len(h.pendingFacts) != 0 {
		t.Fatal("scrape consumed observations")
	}
	var text bytes.Buffer
	if err = h.WritePrometheus(&text); err != nil {
		t.Fatal(err)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	parsed, err := parser.TextToMetricFamilies(strings.NewReader(text.String()))
	if err != nil {
		t.Fatal(err)
	}
	if parsed["shaper_observed_upstream_429_total"].GetType().String() != "COUNTER" {
		t.Fatal("wrong metric type")
	}
	for _, family := range parsed {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if !slices.Contains([]string{"provider", "side", "class", "stage"}, label.GetName()) {
					t.Fatalf("unbounded label %s", label.GetName())
				}
			}
		}
	}
	if strings.Contains(text.String(), "arbitrary-model") || strings.Contains(text.String(), "secret/model") {
		t.Fatal("high-cardinality identity leaked into Prometheus")
	}
}

func TestRegistryAndRecentRingBoundsDoNotLoseTotals(t *testing.T) {
	h, _ := newTestHub(t, "north")
	requests := make([]*Request, RequestLimit+25)
	for i := range requests {
		_, requests[i] = h.Begin(context.Background(), BeginInfo{Provider: "north"})
	}
	snap := h.Snapshot()
	if snap.LiveRequests != RequestLimit || snap.UntrackedRequests != 25 || provider(t, h, "north").Active != int64(len(requests)) {
		t.Fatalf("live bounds=%+v", snap)
	}
	if h.Cancel(requests[len(requests)-1].ID()) {
		t.Fatal("untracked request claims cancellability")
	}
	for _, r := range requests {
		r.Finish(FinishInfo{Status: 200, Outcome: OutcomeCompleted})
	}
	if len(h.recent) != RequestLimit || provider(t, h, "north").Totals.Completed != uint64(len(requests)) {
		t.Fatal("ring eviction lost aggregate counts")
	}
	facts, err := h.Requests(context.Background(), RequestQuery{Limit: RequestLimit})
	if err != nil {
		t.Fatal(err)
	}
	if facts.Total != RequestLimit || facts.Requests[len(facts.Requests)-1].ID != requests[25].ID() {
		t.Fatal("ring does notretain newest facts")
	}
	if h.Snapshot().History.DroppedFacts != 0 {
		t.Fatal("memory-only eviction falsely reported ashistory-write loss")
	}
}
func TestOpenIntervalBackwardClockIsVisible(t *testing.T) {
	h, c := newTestHub(t, "north")
	start := c.Now()
	_, r := h.Begin(context.Background(), BeginInfo{Provider: "north"})
	r.Finish(FinishInfo{Status: 200, Outcome: OutcomeCompleted})
	c.Advance(-time.Second)
	result, err := h.Query(context.Background(), Query{From: start.Add(-time.Second), To: start, Providers: []string{"north"}, MaxPoints: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Points) != 1 || !result.Points[0].ClockStep || result.Points[0].Complete || result.Points[0].Counts.Terminal != 1 {
		t.Fatalf("unsealed clock step hidden: %+v", result)
	}
}

func TestConversionOmissionsRemainDistinctFromLossAndNote(t *testing.T) {
	h, _ := newTestHub(t, "north")
	_, r := h.Begin(context.Background(), BeginInfo{Provider: "north"})
	r.Conversion(2, 3, 17)
	r.Conversion(1, 1)
	r.Conversion(-1, -1, -1)
	totals := provider(t, h, "north").Totals
	fact := r.Snapshot()
	if totals.ConversionLosses != 3 || totals.ConversionNotes != 4 || totals.ConversionDropped != 17 || fact.ConversionDropped != 17 {
		t.Fatalf("conversion omissions mislabeled: totals=%+v fact=%+v", totals, fact)
	}
}

func TestLongProviderAndResourceIdentitiesDoNotCollide(t *testing.T) {
	a := strings.Repeat("a", 200) + "north"
	b := strings.Repeat("a", 200) + "south"
	h, _ := newTestHub(t, a, b)
	_, ra := h.Begin(context.Background(), BeginInfo{Provider: a})
	_, rb := h.Begin(context.Background(), BeginInfo{Provider: b})
	ra.Finish(FinishInfo{Outcome: OutcomeCompleted})
	rb.Finish(FinishInfo{Outcome: OutcomeCompleted})
	if provider(t, h, a).Totals.Completed != 1 || provider(t, h, b).Totals.Completed != 1 {
		t.Fatal("long configured provider IDs collided")
	}
	base := strings.Repeat("x", 400)
	h.SetResource(Resource{Provider: a, ID: base + "1", Kind: "route", Limit: 1, Known: true})
	h.SetResource(Resource{Provider: a, ID: base + "2", Kind: "route", Limit: 2, Known: true})
	if got := provider(t, h, a).Resources; len(got) != 2 || got[0].ID == got[1].ID {
		t.Fatalf("resource IDs collided: %+v", got)
	}
	h.SetResource(Resource{Provider: a, ID: strings.Repeat("z", 513), Kind: "route", Known: true})
	if len(provider(t, h, a).Resources) != 2 {
		t.Fatal("invalid resource silently truncated into an identity")
	}
}

type recordingSink struct{ packets []Packet }

func (s *recordingSink) Enqueue(p Packet) bool { s.packets = append(s.packets, p); return true }
func (*recordingSink) Read(context.Context, Query, Exclusion) ([]Interval, []Marker, bool, error) {
	panic("scrape must not read storage")
}
func (*recordingSink) Requests(context.Context, RequestQuery) (RequestResult, error) {
	return RequestResult{}, nil
}
func (*recordingSink) Health() Health              { return Health{Enabled: true, Healthy: true} }
func (*recordingSink) Close(context.Context) error { return nil }
func TestDetailOmissionsGetPersistentMarkerWithoutDroppingMeasurements(t *testing.T) {
	clock := &testClock{at: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}
	sink := &recordingSink{}
	h := New(Config{Providers: []string{"north"}, History: sink, Now: clock.Now, ManualTicks: true})
	defer h.Close(context.Background())
	for range RequestLimit + 7 {
		_, r := h.Begin(context.Background(), BeginInfo{Provider: "north"})
		r.Finish(FinishInfo{Status: 200, Outcome: OutcomeCompleted})
	}
	registry := prometheus.NewRegistry()
	if err := registry.Register(h.PrometheusCollector()); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Gather(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	h.Tick()
	if len(sink.packets) != 1 || len(sink.packets[0].Requests) != RequestLimit {
		t.Fatalf("bounded packet count=%d", len(sink.packets))
	}
	if len(sink.packets[0].Markers) != 1 || sink.packets[0].Markers[0].Kind != "detail_gap" || !strings.Contains(sink.packets[0].Markers[0].Summary, "7 request facts") {
		t.Fatalf("detail loss not visible: %+v", sink.packets[0].Markers)
	}
	if provider(t, h, "north").Totals.Completed != RequestLimit+7 || h.Snapshot().History.DroppedFacts != 7 {
		t.Fatal("detail bound changed aggregate measurements")
	}
	clock.Advance(time.Second)
	h.Tick()
	if len(sink.packets[1].Markers) != 0 {
		t.Fatal("same omission marker repeated")
	}
}
