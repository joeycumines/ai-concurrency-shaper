package observation

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"
)

var ErrQueryScope = errors.New("history query has too many provider or resource identities; narrow the time range or provider scope")

var ErrQuery = errors.New("history query requires an increasing nonzero time range, at most 366 days, and 1..1000 points")

func ValidateQuery(q Query) (Query, error) {
	if q.MaxPoints == 0 {
		q.MaxPoints = 120
	}
	if q.From.IsZero() || !time.Unix(0, q.From.UnixNano()).Equal(q.From) || !time.Unix(0, q.To.UnixNano()).Equal(q.To) || !q.To.After(q.From) || q.To.Sub(q.From) > 366*24*time.Hour || q.MaxPoints < 1 || q.MaxPoints > MaxPoints || len(q.Providers) > MaxProviders+1 {
		return q, ErrQuery
	}
	q.Providers = append([]string(nil), q.Providers...)
	slices.Sort(q.Providers)
	q.Providers = slices.Compact(q.Providers)
	return q, nil
}
func providerSelected(name string, providers []string) bool {
	return len(providers) == 0 || slices.Contains(providers, name)
}
func (h *Hub) Query(ctx context.Context, q Query) (QueryResult, error) {
	q, err := ValidateQuery(q)
	if err != nil {
		return QueryResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if h == nil {
		return QueryResult{Query: q}, nil
	}
	h.mu.Lock()
	memory := make([]Interval, 0, len(h.closed)+len(h.providers))
	ex := Exclusion{Epoch: h.epoch, FromSequence: h.seq + 1}
	for _, v := range h.closed {
		ex.FromSequence = min(ex.FromSequence, v.Sequence)
		a, b := intervalEnvelope(v)
		if b.After(q.From) && a.Before(q.To) && providerSelected(v.Provider, q.Providers) {
			memory = append(memory, cloneInterval(v))
		}
	}
	// Read the open interval without sealing it or mutating gauges. This makes UI
	// reads observational and lets pending disk commits overlay deterministically.
	t, endUncertainty := h.sampleClock()
	if !h.closedHub {
		for _, p := range h.providers {
			if !providerSelected(p.name, q.Providers) {
				continue
			}
			a, b := p.start, t
			if b.Before(a) {
				a, b = b, a
			}
			if !b.After(q.From) || !a.Before(q.To) {
				continue
			}
			pc := *p
			pc.integrate(t)
			v := Interval{Samples: 1, Epoch: h.epoch, Sequence: h.seq + 1, LastSequence: h.seq + 1, Provider: p.name, Start: p.start, End: t, ElapsedNS: int64(max(t.Sub(p.start), 0)), ClockUncertaintyNS: addDuration(p.uncertainty, endUncertainty), ClockStep: clockStep(max(t.Sub(p.start), 0), t.Round(0).Sub(p.start.Round(0)), addDuration(p.uncertainty, endUncertainty), t.Before(p.start)), Counts: p.counts, Duration: p.currentDuration, Admission: p.currentAdmission, AttemptDuration: p.currentAttempt, Gauges: pc.gauges}
			for _, rs := range p.resources {
				copyResource := *rs
				copyResource.integrate(t)
				v.Resources = append(v.Resources, copyResource.integral)
			}
			memory = append(memory, v)
		}
	}
	markers := append([]Marker(nil), h.markers...)
	sink := h.sink
	health := h.health()
	h.mu.Unlock()
	var persisted []Interval
	truncated := false
	if sink != nil {
		var ms []Marker
		persisted, ms, truncated, err = sink.Read(ctx, q, ex)
		if err != nil {
			return QueryResult{}, err
		}
		markers = append(ms, markers...)
	}
	if err = ctx.Err(); err != nil {
		return QueryResult{}, err
	}
	rows := append(persisted, memory...)
	if len(rows) > MaxSourceIntervals {
		rows = rows[len(rows)-MaxSourceIntervals:]
		truncated = true
	}
	result, err := Aggregate(ctx, q, rows)
	if err != nil {
		return QueryResult{}, err
	}
	result.Truncated = result.Truncated || truncated
	if result.Truncated {
		invalidateCoverage(&result)
	}
	result.History = health
	seen := make(map[string]bool)
	for _, m := range markers {
		if m.At.Before(q.From) || !m.At.Before(q.To) || (m.Provider != "" && !providerSelected(m.Provider, q.Providers)) || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		result.Markers = append(result.Markers, m)
		if len(result.Markers) >= 1024 {
			break
		}
	}
	slices.SortFunc(result.Markers, func(a, b Marker) int { return a.At.Compare(b.At) })
	return result, nil
}

// Aggregate uses whole observation intervals. Cuts follow source boundaries so
// it neither divides a request nor pretends a coarse rollup has fine precision.
// Requested edges cutting a source interval are partial and do not prorate it.
func Aggregate(ctx context.Context, q Query, rows []Interval) (QueryResult, error) {
	var err error
	q, err = ValidateQuery(q)
	if err != nil {
		return QueryResult{}, err
	}
	r := QueryResult{Query: q, SourceIntervals: len(rows)}
	if len(rows) > MaxSourceIntervals {
		rows = rows[:MaxSourceIntervals]
		r.Truncated = true
	}
	filtered := make([]Interval, 0, len(rows))
	providers := append([]string(nil), q.Providers...)
	resourceScope := make(map[string]map[resourceKey]bool)
	for i, v := range rows {
		if i%512 == 0 {
			if err := ctx.Err(); err != nil {
				return QueryResult{}, err
			}
		}
		if !providerSelected(v.Provider, q.Providers) {
			continue
		}
		a, b := intervalEnvelope(v)
		if !b.After(q.From) || !a.Before(q.To) {
			continue
		}
		if !slices.Contains(providers, v.Provider) {
			if len(providers) >= MaxProviders+1 {
				return QueryResult{}, ErrQueryScope
			}
			providers = append(providers, v.Provider)
		}
		scope := resourceScope[v.Provider]
		if scope == nil {
			scope = make(map[resourceKey]bool)
			resourceScope[v.Provider] = scope
		}
		for _, resource := range v.Resources {
			key := resourceKey{resource.Provider, resource.Kind, resource.ID}
			if !scope[key] {
				if len(scope) >= MaxResourcePools {
					return QueryResult{}, ErrQueryScope
				}
				scope[key] = true
			}
		}
		filtered = append(filtered, v)
	}
	slices.Sort(providers)
	slices.SortFunc(filtered, func(a, b Interval) int {
		as, _ := intervalEnvelope(a)
		bs, _ := intervalEnvelope(b)
		if d := as.Compare(bs); d != 0 {
			return d
		}
		if d := strings.Compare(a.Epoch, b.Epoch); d != 0 {
			return d
		}
		if d := cmp.Compare(a.Sequence, b.Sequence); d != 0 {
			return d
		}
		return strings.Compare(a.Provider, b.Provider)
	})
	bounds := []time.Time{q.From}
	step := max(q.To.Sub(q.From)/time.Duration(q.MaxPoints), 1)
	next := q.From.Add(step)
	for _, v := range filtered {
		a, _ := intervalEnvelope(v)
		if a.Before(next) || !a.Before(q.To) || len(bounds) >= q.MaxPoints {
			continue
		}
		bounds = append(bounds, a)
		next = a.Add(step)
	}
	bounds = append(bounds, q.To)
	if len(filtered) == 0 {
		bounds = []time.Time{q.From, q.To}
	}
	per := make(map[string][]Point, len(providers))
	for _, p := range providers {
		points := make([]Point, len(bounds)-1)
		for i := range points {
			points[i] = Point{Start: bounds[i], End: bounds[i+1], Gap: true}
		}
		per[p] = points
	}
	type segment struct {
		start, end  time.Time
		epoch       string
		uncertainty int64
	}
	segments := make(map[string][][]segment, len(providers))
	for _, p := range providers {
		segments[p] = make([][]segment, len(bounds)-1)
	}
	seen := make(map[string]bool, len(filtered))
	for n, v := range filtered {
		if n%256 == 0 {
			if err := ctx.Err(); err != nil {
				return QueryResult{}, err
			}
		}
		key := v.Epoch + ":" + v.Provider + ":" + sequenceString(v.Sequence) + ":" + sequenceString(v.LastSequence)
		if seen[key] {
			continue
		}
		seen[key] = true
		a, b := intervalEnvelope(v)
		i := sort.Search(len(bounds)-1, func(i int) bool { return bounds[i+1].After(a) })
		if i >= len(bounds)-1 {
			i = len(bounds) - 2
		}
		p := &per[v.Provider][i]
		if a.Before(q.From) || b.After(q.To) {
			p.Partial = true
			continue
		}
		p.Counts.Add(v.Counts)
		p.Duration.Add(v.Duration)
		p.Admission.Add(v.Admission)
		p.AttemptDuration.Add(v.AttemptDuration)
		p.Gauges.Add(v.Gauges)
		p.Resources = mergeResources(p.Resources, v.Resources)
		p.ElapsedNS = addDuration(p.ElapsedNS, v.ElapsedNS)
		p.Samples = add64(p.Samples, max(uint64(1), v.Samples))
		// Only a stored flag or an explicit backward envelope marks a step.
		// Comparing serialized wall endpoints to elapsed time here mixes
		// clock bases: elapsed is monotonic-derived and loses that component
		// in JSON/SQLite, so ordinary read jitter becomes a false step.
		p.ClockStep = p.ClockStep || v.ClockStep || v.End.Before(v.Start)
		p.Overlap = p.Overlap || v.Overlap
		if len(segments[v.Provider][i]) == 0 {
			p.Gap = false
		}
		p.Gap = p.Gap || v.Gap
		if b.After(p.End) {
			p.Partial = true
		}
		segments[v.Provider][i] = append(segments[v.Provider][i], segment{a, b, v.Epoch, v.ClockUncertaintyNS})
	}
	for _, provider := range providers {
		for i := range per[provider] {
			p := &per[provider][i]
			ss := segments[provider][i]
			covered := int64(0)
			cursor := p.Start
			gap := len(ss) == 0 || p.Gap
			for _, s := range ss {
				// Each segment's bracket bounds only its own endpoints.
				// Summing brackets across a cell grows to seconds at normal
				// tick rates and hides genuine gaps late in the cell.
				uncertainty := max(0, s.uncertainty)
				a := maxTime(s.start, p.Start)
				b := minTime(s.end, p.End)
				if a.After(cursor) {
					if a.Sub(cursor) > time.Duration(uncertainty) {
						gap = true
					}
				} else if a.Before(cursor) {
					if cursor.Sub(a) > time.Duration(uncertainty) {
						p.Overlap = true
					}
				}
				if b.After(cursor) {
					covered += int64(b.Sub(maxTime(a, cursor)))
					cursor = b
				}
			}
			trailingUncertainty := int64(0)
			if len(ss) > 0 {
				trailingUncertainty = max(0, ss[len(ss)-1].uncertainty)
			}
			if p.End.Sub(cursor) > time.Duration(trailingUncertainty) {
				gap = true
			}
			p.Coverage = min(1, max(0, float64(covered)/float64(p.End.Sub(p.Start))))
			p.Gap = gap
			p.Complete = !p.Gap && !p.Overlap && !p.Partial && !p.ClockStep && p.ElapsedNS > 0
			if !p.Complete {
				p.ElapsedNS = 0
			}
		}
		r.ByProvider = append(r.ByProvider, ProviderSeries{Provider: provider, Points: per[provider]})
	}
	r.Points = make([]Point, len(bounds)-1)
	for i := range r.Points {
		p := &r.Points[i]
		p.Start = bounds[i]
		p.End = bounds[i+1]
		p.Complete = len(providers) > 0
		p.Coverage = 1
		var common int64
		for j, provider := range providers {
			v := per[provider][i]
			p.Counts.Add(v.Counts)
			p.Duration.Add(v.Duration)
			p.Admission.Add(v.Admission)
			p.AttemptDuration.Add(v.AttemptDuration)
			p.Resources = mergeResources(p.Resources, v.Resources)
			p.Samples = add64(p.Samples, v.Samples)
			p.Complete = p.Complete && v.Complete
			p.Gap = p.Gap || v.Gap
			p.Partial = p.Partial || v.Partial
			p.Overlap = p.Overlap || v.Overlap
			p.ClockStep = p.ClockStep || v.ClockStep
			p.Coverage = min(p.Coverage, v.Coverage)
			if j == 0 {
				common = v.ElapsedNS
				p.Gauges = v.Gauges
			} else {
				common = min(common, v.ElapsedNS)
				addFleetGauge(&p.Gauges.Active, v.Gauges.Active)
				addFleetGauge(&p.Gauges.Queued, v.Gauges.Queued)
				addFleetGauge(&p.Gauges.Exchanges, v.Gauges.Exchanges)
			}
		}
		if p.Complete {
			p.ElapsedNS = common
		}
		if len(providers) == 0 {
			p.Gap = true
			p.Coverage = 0
		}
	}
	if r.Truncated {
		invalidateCoverage(&r)
	}
	return r, nil
}
func invalidateCoverage(r *QueryResult) {
	for i := range r.Points {
		r.Points[i].Complete = false
		r.Points[i].ElapsedNS = 0
		r.Points[i].Partial = true
	}
	for i := range r.ByProvider {
		for j := range r.ByProvider[i].Points {
			p := &r.ByProvider[i].Points[j]
			p.Complete = false
			p.ElapsedNS = 0
			p.Partial = true
		}
	}
}
func addFleetGauge(a *Gauge, b Gauge) {
	if !a.Known || !b.Known {
		*a = Gauge{}
		return
	}
	am, aok := a.Mean()
	bm, bok := b.Mean()
	if !aok || !bok {
		*a = Gauge{}
		return
	}
	a.DurationNS = min(a.DurationNS, b.DurationNS)
	a.Area = (am + bm) * float64(a.DurationNS)
	a.Min += b.Min
	a.Max += b.Max
	a.Last += b.Last
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func sequenceString(v uint64) string {
	const digits = "0123456789abcdef"
	var b [16]byte
	for i := 15; i >= 0; i-- {
		b[i] = digits[v&15]
		v >>= 4
	}
	return string(b[:])
}
func (h *Hub) Requests(ctx context.Context, q RequestQuery) (RequestResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if h == nil {
		return RequestResult{}, nil
	}
	q = NormalizeRequestQuery(q)
	h.mu.Lock()
	facts := make([]RequestFact, 0, len(h.recent)+len(h.live))
	for _, f := range h.recent {
		facts = append(facts, cloneFact(f))
	}
	now := h.now()
	for _, r := range h.live {
		f := cloneFact(r.fact)
		f.DurationNS = int64(max(now.Sub(r.started), 0))
		f.QueueNS = int64(r.queueTotal)
		if r.queueDepth > 0 {
			f.QueueNS += int64(max(now.Sub(r.queueStart), 0))
		}
		facts = append(facts, f)
	}
	sink := h.sink
	h.mu.Unlock()
	if sink != nil && !q.LiveOnly {
		sq := q
		sq.Limit = RequestLimit
		sq.Offset = 0
		res, err := sink.Requests(ctx, sq)
		if err != nil {
			return RequestResult{}, err
		}
		facts = append(res.Requests, facts...)
	}
	if err := ctx.Err(); err != nil {
		return RequestResult{}, err
	}
	return FilterRequests(facts, q), nil
}
func NormalizeRequestQuery(q RequestQuery) RequestQuery {
	if q.Limit <= 0 {
		q.Limit = 200
	}
	q.Limit = min(q.Limit, RequestLimit)
	q.Offset = max(0, min(q.Offset, 3*RequestLimit))
	q.Search = bounded(q.Search, 256)
	return q
}

// FilterRequests merges persisted/live facts by immutable request identity. The
// newest snapshot wins; offset is deterministic within this accepted snapshot.
func FilterRequests(facts []RequestFact, q RequestQuery) RequestResult {
	q = NormalizeRequestQuery(q)
	unique := make(map[string]RequestFact, len(facts))
	for _, f := range facts {
		// A committed finished fact must never be replaced by a stale live
		// snapshot captured before the write committed.
		if prev, exists := unique[f.ID]; exists && !prev.Live && f.Live {
			continue
		}
		unique[f.ID] = f
	}
	out := make([]RequestFact, 0, len(unique))
	search := strings.ToLower(q.Search)
	for _, f := range unique {
		if !MatchesDuration(f, q) {
			continue
		}
		if !providerSelected(f.Provider, q.Providers) || q.LiveOnly && !f.Live || q.Outcome != "" && f.Outcome != q.Outcome || q.Stage != "" && f.Stage != q.Stage {
			continue
		}
		at := f.Finished
		if at.IsZero() {
			at = f.Started
		}
		if !q.From.IsZero() && at.Before(q.From) || !q.To.IsZero() && !at.Before(q.To) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(strings.Join([]string{f.ID, f.Provider, f.Method, f.Path, f.Route, f.Model, f.ClientProtocol, f.UpstreamProtocol, f.Fingerprint, string(f.Outcome), string(f.Stage)}, " ")), search) {
			continue
		}
		out = append(out, cloneFact(f))
	}
	slices.SortFunc(out, func(a, b RequestFact) int {
		at, bt := a.Started, b.Started
		if d := bt.Compare(at); d != 0 {
			return d
		}
		return strings.Compare(b.ID, a.ID)
	})
	total := len(out)
	start := min(q.Offset, total)
	end := min(total, start+q.Limit)
	return RequestResult{Requests: out[start:end], Total: total, Truncated: end < total}
}

// MatchesDuration uses the exact (lower,upper] bounds used by the latency
// histogram. Attempt bounds must lie on histogram edges; this lets a request
// remain discoverable after its bounded individual-attempt details are evicted.
func MatchesDuration(f RequestFact, q RequestQuery) bool {
	within := func(d int64) bool {
		return (q.DurationMinNS == nil || d > *q.DurationMinNS) && (q.DurationMaxNS == nil || d <= *q.DurationMaxNS)
	}
	switch q.DurationKind {
	case "":
		return true
	case "client":
		return within(f.DurationNS)
	case "admission":
		return f.Waited && within(f.QueueNS)
	case "attempt":
		for i, count := range f.AttemptHistogram.Bins {
			if count == 0 {
				continue
			}
			var lower int64
			if i > 0 {
				lower = int64(DurationBounds[i-1] * 1e9)
			}
			upper := int64(0)
			if i < len(DurationBounds) {
				upper = int64(DurationBounds[i] * 1e9)
			}
			lowerOK := q.DurationMinNS == nil || lower >= *q.DurationMinNS
			upperOK := q.DurationMaxNS == nil || i == len(DurationBounds) || upper <= *q.DurationMaxNS
			if lowerOK && upperOK {
				return true
			}
		}
		return false
	default:
		return false
	}
}
