package observation

import (
	"math"
	"sort"
	"time"
)

func add64(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}
func addDuration(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}
func (c *Counters) Add(b Counters) {
	a := []*uint64{&c.Received, &c.Terminal, &c.Completed, &c.Failed, &c.Aborted, &c.Rejected, &c.Unknown, &c.Cancelled, &c.Attempts, &c.Retries, &c.AuthResends, &c.Upstream429, &c.Upstream401, &c.Upstream5xx, &c.ConversionLosses, &c.ConversionNotes, &c.ConversionDropped, &c.UsageReports, &c.UsageInputKnown, &c.UsageOutputKnown, &c.UsageTotalKnown, &c.UsageInputTokens, &c.UsageOutputTokens, &c.UsageTotalTokens, &c.UsageCorrections, &c.UsageOverflow}
	v := []uint64{b.Received, b.Terminal, b.Completed, b.Failed, b.Aborted, b.Rejected, b.Unknown, b.Cancelled, b.Attempts, b.Retries, b.AuthResends, b.Upstream429, b.Upstream401, b.Upstream5xx, b.ConversionLosses, b.ConversionNotes, b.ConversionDropped, b.UsageReports, b.UsageInputKnown, b.UsageOutputKnown, b.UsageTotalKnown, b.UsageInputTokens, b.UsageOutputTokens, b.UsageTotalTokens, b.UsageCorrections, b.UsageOverflow}
	for i, p := range a {
		*p = add64(*p, v[i])
	}
	for i := range c.HTTP {
		for j := range c.HTTP[i] {
			c.HTTP[i][j] = add64(c.HTTP[i][j], b.HTTP[i][j])
		}
	}
	for i := range c.Failures {
		c.Failures[i] = add64(c.Failures[i], b.Failures[i])
	}
}
func (h *Histogram) Observe(d time.Duration) {
	v := d.Seconds()
	if v < 0 {
		return
	}
	i := sort.Search(len(DurationBounds), func(i int) bool { return v <= DurationBounds[i] })
	h.Bins[i] = add64(h.Bins[i], 1)
	h.Count = add64(h.Count, 1)
	h.Sum += v
}
func (h *Histogram) Add(b Histogram) {
	for i := range h.Bins {
		h.Bins[i] = add64(h.Bins[i], b.Bins[i])
	}
	h.Count = add64(h.Count, b.Count)
	h.Sum += b.Sum
}

// Quantile returns a bound, not invented precision within a histogram bucket.
// Merge histograms with Add BEFORE asking for a fleet percentile.
func (h Histogram) Quantile(q float64) (lower, upper float64, ok bool) {
	if h.Count == 0 || q < 0 || q > 1 || math.IsNaN(q) {
		return 0, 0, false
	}
	rank := uint64(math.Ceil(q * float64(h.Count)))
	if rank == 0 {
		rank = 1
	}
	var n uint64
	for i, v := range h.Bins {
		n = add64(n, v)
		if n >= rank {
			if i > 0 {
				lower = DurationBounds[i-1]
			}
			upper = math.Inf(1)
			if i < len(DurationBounds) {
				upper = DurationBounds[i]
			}
			return lower, upper, true
		}
	}
	return 0, 0, false
}
func (g *Gauge) integrate(value int64, d time.Duration, known bool) {
	if !known || d < 0 {
		return
	}
	if !g.Known {
		g.Min = value
		g.Max = value
	}
	g.Known = true
	g.Min = min(g.Min, value)
	g.Max = max(g.Max, value)
	g.Last = value
	g.Area += float64(value) * float64(d)
	g.DurationNS = addDuration(g.DurationNS, int64(d))
}
func (g *Gauge) Add(b Gauge) {
	if !b.Known {
		return
	}
	if !g.Known {
		*g = b
		return
	}
	g.Area += b.Area
	g.DurationNS = addDuration(g.DurationNS, b.DurationNS)
	g.Min = min(g.Min, b.Min)
	g.Max = max(g.Max, b.Max)
	g.Last = b.Last
}
func (g *Gauges) Add(b Gauges) {
	g.Active.Add(b.Active)
	g.Queued.Add(b.Queued)
	g.Exchanges.Add(b.Exchanges)
}

type resourceKey struct{ provider, kind, id string }

func mergeResources(a, b []ResourceIntegral) []ResourceIntegral {
	merge := func(dst *ResourceIntegral, src ResourceIntegral) {
		dst.Limit.Add(src.Limit)
		dst.Occupied.Add(src.Occupied)
		dst.Cooldown.Add(src.Cooldown)
		dst.Queued.Add(src.Queued)
	}
	if len(a)*len(b) <= 256 {
		for _, v := range b {
			found := false
			for i := range a {
				if a[i].Provider == v.Provider && a[i].Kind == v.Kind && a[i].ID == v.ID {
					merge(&a[i], v)
					found = true
					break
				}
			}
			if !found {
				a = append(a, v)
			}
		}
		return a
	}
	index := make(map[resourceKey]int, len(a)+len(b))
	for i, v := range a {
		index[resourceKey{v.Provider, v.Kind, v.ID}] = i
	}
	for _, v := range b {
		k := resourceKey{v.Provider, v.Kind, v.ID}
		if i, ok := index[k]; ok {
			merge(&a[i], v)
		} else {
			index[k] = len(a)
			a = append(a, v)
		}
	}
	return a
}
func (a *Interval) Add(b Interval) {
	a.Samples = add64(max(a.Samples, uint64(1)), max(b.Samples, uint64(1)))
	a.Gap = a.Gap || b.Gap
	a.Overlap = a.Overlap || b.Overlap
	a.Counts.Add(b.Counts)
	a.Duration.Add(b.Duration)
	a.Admission.Add(b.Admission)
	a.AttemptDuration.Add(b.AttemptDuration)
	a.Gauges.Add(b.Gauges)
	a.Resources = mergeResources(a.Resources, b.Resources)
	a.ElapsedNS = addDuration(a.ElapsedNS, b.ElapsedNS)
	a.ClockUncertaintyNS = addDuration(a.ClockUncertaintyNS, b.ClockUncertaintyNS)
	a.ClockStep = a.ClockStep || b.ClockStep || a.End.Before(a.Start) || b.End.Before(b.Start)
	as, ae := intervalEnvelope(*a)
	bs, be := intervalEnvelope(b)
	a.Start = minTime(as, bs)
	a.End = maxTime(ae, be)
	a.LastSequence = max(a.LastSequence, b.Sequence, b.LastSequence)
}
func intervalEnvelope(v Interval) (time.Time, time.Time) {
	if v.End.Before(v.Start) {
		return v.End, v.Start
	}
	return v.Start, v.End
}
func cloneInterval(v Interval) Interval {
	v.Resources = append([]ResourceIntegral(nil), v.Resources...)
	return v
}
func cloneFact(v RequestFact) RequestFact {
	v.Attempts = append([]AttemptFact(nil), v.Attempts...)
	if v.Usage != nil {
		u := *v.Usage
		v.Usage = &u
	}
	return v
}
