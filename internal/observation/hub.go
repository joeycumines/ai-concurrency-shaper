package observation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Config struct {
	Providers []string
	History   Sink
	Interval  time.Duration
	// Now and ManualTicks are intended for deterministic clocks and replay tools.
	Now         func() time.Time
	ManualTicks bool
}
type providerState struct {
	name                                              string
	totals, counts                                    Counters
	duration, admission, attemptDuration              Histogram
	currentDuration, currentAdmission, currentAttempt Histogram
	active, queued, exchanges                         int64
	gauges                                            Gauges
	start, last                                       time.Time
	resources                                         map[string]*resourceState
	uncertainty                                       int64
}
type resourceState struct {
	value    Resource
	integral ResourceIntegral
	last     time.Time
}
type Hub struct {
	mu                                      sync.Mutex
	now                                     func() time.Time
	epoch                                   string
	seq, requestSeq, markerSeq              uint64
	providers                               map[string]*providerState
	live                                    map[string]*Request
	recent                                  []RequestFact
	recentNext                              int
	closed                                  []Interval
	markers                                 []Marker
	pendingFacts                            []RequestFact
	pendingMarkers                          []Marker
	droppedFacts, droppedMarkers, untracked uint64
	reportedFactDrops, reportedMarkerDrops  uint64
	sink                                    Sink
	stop                                    chan struct{}
	stopped                                 chan struct{}
	closeOnce                               sync.Once
	closedHub                               bool
}
type Request struct {
	h          *Hub
	fact       RequestFact
	started    time.Time
	queueStart time.Time
	queueDepth int
	queueTotal time.Duration
	queueEver  bool
	done       bool
	cancel     context.CancelFunc
	attempt    *Attempt
}
type Attempt struct {
	r       *Request
	fact    AttemptFact
	started time.Time
	done    bool
	headers bool
	detail  int
}
type requestContextKey struct{}

func FromContext(ctx context.Context) *Request {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(requestContextKey{}).(*Request)
	return r
}
func WithRequest(ctx context.Context, r *Request) context.Context {
	return context.WithValue(ctx, requestContextKey{}, r)
}
func New(c Config) *Hub {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Interval <= 0 {
		c.Interval = time.Second
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("observation epoch: %w", err))
	}
	h := &Hub{now: c.Now, epoch: hex.EncodeToString(b), providers: make(map[string]*providerState), live: make(map[string]*Request), sink: c.History, stop: make(chan struct{}), stopped: make(chan struct{})}
	t, uncertainty := h.sampleClock()
	h.provider(UnresolvedProvider, t).uncertainty = uncertainty
	for _, p := range c.Providers {
		h.provider(p, t).uncertainty = uncertainty
	}
	if c.ManualTicks {
		close(h.stopped)
	} else {
		go func() {
			defer close(h.stopped)
			tick := time.NewTicker(c.Interval)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					h.Tick()
				case <-h.stop:
					return
				}
			}
		}()
	}
	return h
}
func bounded(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= n {
		return strings.Clone(s)
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return strings.Clone(s)
}
func (h *Hub) provider(name string, t time.Time) *providerState {
	if len(name) > MaxProviderNameBytes || !utf8.ValidString(name) {
		name = UnresolvedProvider
	}
	if name == "" {
		name = UnresolvedProvider
	}
	if p := h.providers[name]; p != nil {
		return p
	}
	if len(h.providers) >= MaxProviders+1 {
		return h.providers[UnresolvedProvider]
	}
	p := &providerState{name: name, start: t, last: t, resources: make(map[string]*resourceState)}
	h.providers[name] = p
	return p
}
func (p *providerState) integrate(t time.Time) {
	d := max(t.Sub(p.last), 0)
	p.gauges.Active.integrate(p.active, d, true)
	p.gauges.Queued.integrate(p.queued, d, true)
	p.gauges.Exchanges.integrate(p.exchanges, d, true)
	p.last = t
}
func (p *providerState) add(c Counters) { p.counts.Add(c); p.totals.Add(c) }
func (h *Hub) Begin(ctx context.Context, in BeginInfo) (context.Context, *Request) {
	if h == nil {
		return ctx, nil
	}
	if r := FromContext(ctx); r != nil {
		return ctx, r
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closedHub {
		return ctx, nil
	}
	t := h.now()
	p := h.provider(in.Provider, t)
	p.integrate(t)
	p.active++
	p.add(Counters{Received: 1})
	in.Provider = p.name
	in.Method = bounded(in.Method, 32)
	in.Path = bounded(in.Path, 1024)
	in.Route = bounded(in.Route, 1024)
	in.Model = bounded(in.Model, 256)
	in.ClientProtocol = bounded(in.ClientProtocol, 64)
	in.UpstreamProtocol = bounded(in.UpstreamProtocol, 64)
	h.requestSeq++
	ctx, cancel := context.WithCancel(ctx)
	r := &Request{h: h, started: t, cancel: cancel, fact: RequestFact{ID: fmt.Sprintf("%s-%016x", h.epoch, h.requestSeq), Epoch: h.epoch, BeginInfo: in, ReceivedProvider: p.name, Started: t, Live: true}}
	if len(h.live) < RequestLimit {
		h.live[r.fact.ID] = r
	} else {
		h.untracked++
	}
	return WithRequest(ctx, r), r
}
func (r *Request) ID() string {
	if r == nil {
		return ""
	}
	return r.fact.ID
}
func (r *Request) SetProvider(name string) {
	if r == nil {
		return
	}
	h := r.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.done {
		return
	}
	t := h.now()
	old := h.provider(r.fact.Provider, t)
	p := h.provider(name, t)
	if old == p {
		return
	}
	old.integrate(t)
	p.integrate(t)
	old.active--
	p.active++
	if r.queueDepth > 0 {
		old.queued--
		p.queued++
	}
	r.fact.Provider = p.name
}
func (r *Request) SetProtocols(client, upstream, model string) {
	if r == nil {
		return
	}
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	if r.done {
		return
	}
	r.fact.ClientProtocol = bounded(client, 64)
	r.fact.UpstreamProtocol = bounded(upstream, 64)
	r.fact.Model = bounded(model, 256)
}
func (r *Request) SetRoute(route string) {
	if r == nil {
		return
	}
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	if !r.done {
		r.fact.Route = bounded(route, 1024)
	}
}
func (r *Request) SetFingerprint(s string) {
	if r == nil {
		return
	}
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	if !r.done {
		r.fact.Fingerprint = bounded(s, 128)
	}
}
func (r *Request) SetJournalID(id uint64) {
	if r == nil {
		return
	}
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	if !r.done {
		r.fact.JournalID = id
	}
}

// Queue records a nested union of waits. Overlapping queue phases count once.
func (r *Request) Queue(enter bool) {
	if r == nil {
		return
	}
	h := r.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.done {
		return
	}
	t := h.now()
	p := h.provider(r.fact.Provider, t)
	if enter {
		r.queueDepth++
		if r.queueDepth == 1 {
			p.integrate(t)
			p.queued++
			r.queueStart = t
			r.queueEver = true
			r.fact.Waited = true
			r.fact.Queued = true
		}
	} else if r.queueDepth > 0 {
		r.queueDepth--
		if r.queueDepth == 0 {
			p.integrate(t)
			p.queued--
			r.queueTotal += max(t.Sub(r.queueStart), 0)
			r.fact.Queued = false
		}
	}
}
func (r *Request) BeginAttempt(kind AttemptKind) *Attempt {
	if r == nil {
		return nil
	}
	h := r.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.done {
		return nil
	}
	t := h.now()
	if r.attempt != nil && !r.attempt.done {
		r.attempt.finishLocked(OutcomeUnknown, t)
	}
	switch kind {
	case AttemptInitial, AttemptRetry, AttemptAuthResend:
	default:
		kind = AttemptInitial
	}
	p := h.provider(r.fact.Provider, t)
	p.integrate(t)
	p.exchanges++
	c := Counters{Attempts: 1}
	if kind == AttemptRetry {
		c.Retries = 1
	}
	if kind == AttemptAuthResend {
		c.AuthResends = 1
	}
	p.add(c)
	r.fact.AttemptCount++
	a := &Attempt{r: r, started: t, detail: -1, fact: AttemptFact{Provider: p.name, Number: r.fact.AttemptCount, Kind: kind, Started: t}}
	if len(r.fact.Attempts) < AttemptDetailLimit {
		a.detail = len(r.fact.Attempts)
		r.fact.Attempts = append(r.fact.Attempts, a.fact)
	} else {
		r.fact.AttemptsTruncated = true
		a.detail = AttemptDetailLimit - 1
		r.fact.Attempts[a.detail] = a.fact
	}
	r.attempt = a
	return a
}
func (a *Attempt) Headers(status int) {
	if a == nil {
		return
	}
	h := a.r.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if a.done || a.headers || status < 100 || status >= 600 {
		return
	}
	a.headers = true
	a.fact.Status = status
	p := h.provider(a.fact.Provider, h.now())
	var c Counters
	c.HTTP[1][status/100-1] = 1
	if status == 429 {
		c.Upstream429 = 1
	}
	if status == 401 {
		c.Upstream401 = 1
	}
	if status >= 500 {
		c.Upstream5xx = 1
	}
	p.add(c)
	a.r.fact.Attempts[a.detail] = a.fact
}
func normalizeOutcome(o Outcome) Outcome {
	switch o {
	case OutcomeCompleted, OutcomeFailed, OutcomeAborted, OutcomeRejected:
		return o
	}
	return OutcomeUnknown
}
func normalizeStage(s Stage) Stage {
	for _, x := range Stages {
		if s == x {
			return s
		}
	}
	return StageUnknown
}
func (a *Attempt) Finish(outcome Outcome) {
	if a == nil {
		return
	}
	h := a.r.h
	h.mu.Lock()
	defer h.mu.Unlock()
	a.finishLocked(outcome, h.now())
}
func (a *Attempt) finishLocked(outcome Outcome, t time.Time) {
	if a.done {
		return
	}
	a.done = true
	p := a.r.h.provider(a.fact.Provider, t)
	p.integrate(t)
	p.exchanges--
	d := max(t.Sub(a.started), 0)
	p.currentAttempt.Observe(d)
	p.attemptDuration.Observe(d)
	a.fact.Finished = t
	a.fact.DurationNS = int64(d)
	a.r.fact.AttemptHistogram.Observe(d)
	a.fact.Outcome = normalizeOutcome(outcome)
	a.r.fact.Attempts[a.detail] = a.fact
}
func (r *Request) Conversion(losses, notes int, dropped ...int) {
	if r == nil {
		return
	}
	h := r.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.done {
		return
	}
	losses = max(0, losses)
	notes = max(0, notes)
	r.fact.ConversionLosses = add64(r.fact.ConversionLosses, uint64(losses))
	r.fact.ConversionNotes = add64(r.fact.ConversionNotes, uint64(notes))
	var omitted uint64
	for _, n := range dropped {
		omitted = add64(omitted, uint64(max(0, n)))
	}
	r.fact.ConversionDropped = add64(r.fact.ConversionDropped, omitted)
	h.provider(r.fact.Provider, h.now()).add(Counters{ConversionLosses: uint64(losses), ConversionNotes: uint64(notes), ConversionDropped: omitted})
}
func usageEqual(a, b Usage) bool {
	return a.Input == b.Input && a.Output == b.Output && a.Total == b.Total && a.InputKnown == b.InputKnown && a.OutputKnown == b.OutputKnown && a.TotalKnown == b.TotalKnown
}
func normalizeUsage(u Usage) Usage {
	u.Source = bounded(u.Source, 64)
	if !u.InputKnown || u.Input < 0 {
		u.Input = 0
		u.InputKnown = false
	}
	if !u.OutputKnown || u.Output < 0 {
		u.Output = 0
		u.OutputKnown = false
	}
	if !u.TotalKnown || u.Total < 0 {
		u.Total = 0
		u.TotalKnown = false
	}
	return u
}
func (r *Request) ReportUsage(u Usage) {
	if r == nil {
		return
	}
	h := r.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.done {
		return
	}
	u = normalizeUsage(u)
	p := h.provider(r.fact.Provider, h.now())
	if r.fact.Usage != nil {
		if !usageEqual(*r.fact.Usage, u) {
			r.fact.UsageCorrections++
			r.fact.Usage = &u
			p.add(Counters{UsageCorrections: 1})
		}
		return
	}
	r.fact.Usage = &u
	c := Counters{UsageReports: 1}
	if u.InputKnown {
		c.UsageInputKnown = 1
		if ^uint64(0)-p.totals.UsageInputTokens < uint64(u.Input) {
			c.UsageOverflow++
		} else {
			c.UsageInputTokens = uint64(u.Input)
		}
	}
	if u.OutputKnown {
		c.UsageOutputKnown = 1
		if ^uint64(0)-p.totals.UsageOutputTokens < uint64(u.Output) {
			c.UsageOverflow++
		} else {
			c.UsageOutputTokens = uint64(u.Output)
		}
	}
	if u.TotalKnown {
		c.UsageTotalKnown = 1
		if ^uint64(0)-p.totals.UsageTotalTokens < uint64(u.Total) {
			c.UsageOverflow++
		} else {
			c.UsageTotalTokens = uint64(u.Total)
		}
	}
	p.add(c)
}
func (r *Request) Finish(in FinishInfo) {
	if r == nil {
		return
	}
	h := r.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.done {
		return
	}
	t := h.now()
	r.done = true
	if r.attempt != nil && !r.attempt.done {
		out := OutcomeUnknown
		if in.Outcome == OutcomeAborted {
			out = OutcomeAborted
		}
		r.attempt.finishLocked(out, t)
	}
	p := h.provider(r.fact.Provider, t)
	p.integrate(t)
	p.active--
	if r.queueDepth > 0 {
		p.queued--
		r.queueTotal += max(t.Sub(r.queueStart), 0)
		r.queueDepth = 0
	}
	d := max(t.Sub(r.started), 0)
	p.duration.Observe(d)
	p.currentDuration.Observe(d)
	if r.queueEver {
		p.admission.Observe(r.queueTotal)
		p.currentAdmission.Observe(r.queueTotal)
	}
	in.Outcome = normalizeOutcome(in.Outcome)
	in.Stage = normalizeStage(in.Stage)
	in.Cancelled = in.Cancelled || r.fact.Cancelled
	r.fact.FinishInfo = in
	r.fact.Live = false
	r.fact.Queued = false
	r.fact.Finished = t
	r.fact.DurationNS = int64(d)
	r.fact.QueueNS = int64(r.queueTotal)
	c := Counters{Terminal: 1}
	switch in.Outcome {
	case OutcomeCompleted:
		c.Completed = 1
	case OutcomeFailed:
		c.Failed = 1
	case OutcomeAborted:
		c.Aborted = 1
	case OutcomeRejected:
		c.Rejected = 1
	default:
		c.Unknown = 1
	}
	if in.Cancelled {
		c.Cancelled = 1
	}
	if in.Status >= 100 && in.Status < 600 {
		c.HTTP[0][in.Status/100-1] = 1
	}
	if in.Outcome == OutcomeFailed || in.Outcome == OutcomeRejected {
		for i, s := range Stages {
			if s == in.Stage {
				c.Failures[i] = 1
				break
			}
		}
	}
	p.add(c)
	delete(h.live, r.fact.ID)
	f := cloneFact(r.fact)
	if len(h.recent) < RequestLimit {
		h.recent = append(h.recent, f)
	} else {
		h.recent[h.recentNext] = f
		h.recentNext = (h.recentNext + 1) % RequestLimit
	}
	if h.sink != nil {
		if len(h.pendingFacts) < RequestLimit {
			h.pendingFacts = append(h.pendingFacts, cloneFact(f))
		} else {
			h.droppedFacts++
		}
	}
	r.cancel()
}
func (h *Hub) Cancel(id string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	r := h.live[id]
	if r == nil || r.done {
		h.mu.Unlock()
		return false
	}
	r.fact.Cancelled = true
	cancel := r.cancel
	h.mu.Unlock()
	cancel()
	return true
}
func (r *Request) Snapshot() RequestFact {
	if r == nil {
		return RequestFact{}
	}
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	f := cloneFact(r.fact)
	if !r.done {
		t := r.h.now()
		f.DurationNS = int64(max(t.Sub(r.started), 0))
		f.QueueNS = int64(r.queueTotal)
		if r.queueDepth > 0 {
			f.QueueNS += int64(max(t.Sub(r.queueStart), 0))
		}
	}
	return f
}
func (h *Hub) SetResource(v Resource) {
	if h == nil {
		return
	}
	h.SetResourceAt(h.now(), v)
}

func (h *Hub) SetResourceAt(at time.Time, v Resource) {
	if h == nil {
		return
	}
	if v.Limit < 0 || v.Occupied < 0 || v.Cooldown < 0 || v.Queued < 0 || v.Cooldown > v.Occupied {
		return
	}
	switch v.Kind {
	case "default", "route", "global":
	default:
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closedHub {
		return
	}
	t := at
	if t.IsZero() {
		return
	}
	p := h.provider(v.Provider, t)
	v.Provider = p.name
	if len(v.ID) > MaxResourceIDBytes || !utf8.ValidString(v.ID) {
		return
	}
	key := v.Kind + ":" + v.ID
	r := p.resources[key]
	if r == nil {
		if len(p.resources) >= MaxResourcePools {
			return
		}
		r = &resourceState{value: v, last: t, integral: ResourceIntegral{Provider: v.Provider, ID: v.ID, Kind: v.Kind}}
		p.resources[key] = r
	} else {
		if t.Before(r.last) {
			return
		}
		r.integrate(t)
		r.value = v
	}
}
func (r *resourceState) integrate(t time.Time) {
	d := max(t.Sub(r.last), 0)
	r.integral.Limit.integrate(r.value.Limit, d, r.value.Known)
	r.integral.Occupied.integrate(r.value.Occupied, d, r.value.Known)
	r.integral.Cooldown.integrate(r.value.Cooldown, d, r.value.Known)
	r.integral.Queued.integrate(r.value.Queued, d, r.value.Known)
	r.last = t
}
func (h *Hub) Marker(m Marker) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closedHub {
		return
	}
	h.markerSeq++
	m.ID = fmt.Sprintf("%s-m%016x", h.epoch, h.markerSeq)
	if m.At.IsZero() {
		m.At = h.now()
	}
	if len(m.Provider) > MaxProviderNameBytes || !utf8.ValidString(m.Provider) {
		m.Provider = UnresolvedProvider
	}
	m.Kind = bounded(m.Kind, 64)
	m.Summary = bounded(m.Summary, 1024)
	m.RequestID = bounded(m.RequestID, 128)
	h.markers = append(h.markers, m)
	if len(h.markers) > 1024 {
		h.markers = append([]Marker(nil), h.markers[len(h.markers)-1024:]...)
	}
	if h.sink != nil {
		if len(h.pendingMarkers) < 1024 {
			h.pendingMarkers = append(h.pendingMarkers, m)
		} else {
			h.droppedMarkers++
		}
	}

}
func (h *Hub) health() Health {
	var v Health
	if h.sink != nil {
		v = h.sink.Health()
	}
	v.DroppedFacts = add64(v.DroppedFacts, h.droppedFacts)
	v.DroppedMarkers = add64(v.DroppedMarkers, h.droppedMarkers)
	return v
}
func (h *Hub) Snapshot() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	h.mu.Lock()
	s := Snapshot{At: h.now(), Epoch: h.epoch, History: h.health(), LiveRequests: len(h.live), UntrackedRequests: h.untracked}
	for _, p := range h.providers {
		v := ProviderSnapshot{Provider: p.name, Totals: p.totals, Duration: p.duration, Admission: p.admission, AttemptDuration: p.attemptDuration, Active: p.active, Queued: p.queued, Exchanges: p.exchanges}
		for _, r := range p.resources {
			v.Resources = append(v.Resources, r.value)
		}
		s.Providers = append(s.Providers, v)
	}
	h.mu.Unlock()
	// Sorting touches only copied DTOs, so a large terminal or metrics snapshot
	// cannot hold the request-path lock while comparing long configured IDs.
	for i := range s.Providers {
		slices.SortFunc(s.Providers[i].Resources, func(a, b Resource) int {
			if order := strings.Compare(a.Kind, b.Kind); order != 0 {
				return order
			}
			return strings.Compare(a.ID, b.ID)
		})
	}
	slices.SortFunc(s.Providers, func(a, b ProviderSnapshot) int { return strings.Compare(a.Provider, b.Provider) })
	return s
}

// Tick seals one immutable absolute interval. Scraping and UI rendering never do.
func (h *Hub) Tick() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closedHub {
		return
	}
	h.tickLocked()
}
func (h *Hub) tickLocked() {
	t, endUncertainty := h.sampleClock()
	h.seq++
	if h.sink != nil && (h.droppedFacts > h.reportedFactDrops || h.droppedMarkers > h.reportedMarkerDrops) {
		h.markerSeq++
		marker := Marker{ID: fmt.Sprintf("%s-m%016x", h.epoch, h.markerSeq), At: t, Kind: "detail_gap", Summary: fmt.Sprintf("Recorder detail buffers omitted %d request facts and %d markers; aggregate measurements remain counted.", h.droppedFacts-h.reportedFactDrops, h.droppedMarkers-h.reportedMarkerDrops)}
		h.pendingMarkers = append(h.pendingMarkers, marker)
		h.markers = append(h.markers, marker)
		if len(h.markers) > 1024 {
			h.markers = append([]Marker(nil), h.markers[len(h.markers)-1024:]...)
		}
		h.reportedFactDrops = h.droppedFacts
		h.reportedMarkerDrops = h.droppedMarkers
	}
	packet := Packet{Requests: h.pendingFacts, Markers: h.pendingMarkers}
	h.pendingFacts = nil
	h.pendingMarkers = nil
	for _, p := range h.providers {
		p.integrate(t)
		d := max(t.Sub(p.start), 0)
		wall := t.Round(0).Sub(p.start.Round(0))
		uncertainty := addDuration(p.uncertainty, endUncertainty)
		iv := Interval{Samples: 1, Epoch: h.epoch, Sequence: h.seq, LastSequence: h.seq, Provider: p.name, Start: p.start, End: t, ElapsedNS: int64(d), ClockUncertaintyNS: uncertainty, ClockStep: absDuration(wall-d) > time.Duration(uncertainty), Counts: p.counts, Duration: p.currentDuration, Admission: p.currentAdmission, AttemptDuration: p.currentAttempt, Gauges: p.gauges}
		for _, r := range p.resources {
			r.integrate(t)
			iv.Resources = append(iv.Resources, r.integral)
			r.integral = ResourceIntegral{Provider: r.value.Provider, ID: r.value.ID, Kind: r.value.Kind}
		}
		packet.Intervals = append(packet.Intervals, iv)
		h.closed = append(h.closed, cloneInterval(iv))
		p.counts = Counters{}
		p.currentDuration = Histogram{}
		p.currentAdmission = Histogram{}
		p.currentAttempt = Histogram{}
		p.gauges = Gauges{}
		p.start = t
		p.uncertainty = endUncertainty
	}
	cut := 0
	until := t.Add(-15 * time.Minute)
	for cut < len(h.closed) {
		_, end := intervalEnvelope(h.closed[cut])
		if !end.Before(until) && len(h.closed)-cut <= 12000 {
			break
		}
		cut++
	}
	if cut > 0 {
		copy(h.closed, h.closed[cut:])
		clear(h.closed[len(h.closed)-cut:])
		h.closed = h.closed[:len(h.closed)-cut]
	}
	if h.sink != nil {
		h.sink.Enqueue(packet)
	}
}
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
func (h *Hub) Close(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.closeOnce.Do(func() { close(h.stop) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.stopped:
	}
	h.mu.Lock()
	if !h.closedHub {
		h.tickLocked()
		h.closedHub = true
	}
	sink := h.sink
	h.mu.Unlock()
	if sink != nil {
		return sink.Close(ctx)
	}
	return nil
}

var ErrClosed = errors.New("observation history is closed")

// IsFinished avoids copying request details when an outer boundary supplies a fallback.
func (r *Request) IsFinished() bool {
	if r == nil {
		return true
	}
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	return r.done
}

// sampleClock measures the actual clock-reading bracket. Unlike a fixed epsilon,
// this budget follows scheduling/read uncertainty without erasing real steps.
func (h *Hub) sampleClock() (time.Time, int64) {
	a := h.now()
	b := h.now()
	mono := absDuration(b.Sub(a))
	wall := absDuration(b.Round(0).Sub(a.Round(0)))
	return b, int64(max(mono, wall))
}
