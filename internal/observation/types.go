// Package observation owns immutable, bounded observations shared by the terminal,
// Prometheus and optional history. A logical request is not an upstream attempt.
package observation

import (
	"context"
	"time"
)

const UnresolvedProvider = "__unresolved__"
const MaxProviders = 128
const MaxProviderNameBytes = 256
const MaxResourceIDBytes = 512
const MaxResourcePools = 256
const MaxPoints = 1000
const MaxSourceIntervals = 100000
const RequestLimit = 10000
const AttemptDetailLimit = 32

// Bounds are seconds; the final histogram bin is +Inf. Bins are disjoint.
var DurationBounds = [...]float64{.001, .002, .005, .01, .02, .05, .1, .2, .5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 1800, 3600, 7200}

type Outcome string

const (
	OutcomeCompleted Outcome = "completed"
	OutcomeFailed    Outcome = "failed"
	OutcomeAborted   Outcome = "aborted"
	OutcomeRejected  Outcome = "rejected"
	OutcomeUnknown   Outcome = "unknown"
)

type Stage string

const (
	StageUnknown            Stage = "unknown"
	StageAdmission          Stage = "admission"
	StageAuthentication     Stage = "authentication"
	StageUpstream           Stage = "upstream"
	StageDownstream         Stage = "downstream"
	StageConversionRequest  Stage = "conversion_request"
	StageConversionResponse Stage = "conversion_response"
	StageProxy              Stage = "proxy"
)

var Stages = [...]Stage{StageUnknown, StageAdmission, StageAuthentication, StageUpstream, StageDownstream, StageConversionRequest, StageConversionResponse, StageProxy}

type AttemptKind string

const (
	AttemptInitial    AttemptKind = "initial"
	AttemptRetry      AttemptKind = "retry"
	AttemptAuthResend AttemptKind = "auth_resend"
)

type BeginInfo struct {
	Provider         string `json:"provider"`
	Method           string `json:"method"`
	Path             string `json:"path"`
	Route            string `json:"route,omitempty"`
	ClientProtocol   string `json:"client_protocol,omitempty"`
	UpstreamProtocol string `json:"upstream_protocol,omitempty"`
	Model            string `json:"model,omitempty"`
}
type FinishInfo struct {
	Status    int     `json:"status"`
	Outcome   Outcome `json:"outcome"`
	Stage     Stage   `json:"stage"`
	Cancelled bool    `json:"cancelled,omitempty"`
}

// Usage carries source-reported totals. A known zero is different from absence.
// Totals do not assume that input+output equals total. Reports are accepted only
// at a valid final semantic boundary by the codec integration, never from bytes.
type Usage struct {
	Input       int64  `json:"input"`
	Output      int64  `json:"output"`
	Total       int64  `json:"total"`
	InputKnown  bool   `json:"input_known"`
	OutputKnown bool   `json:"output_known"`
	TotalKnown  bool   `json:"total_known"`
	Source      string `json:"source,omitempty"`
}
type AttemptFact struct {
	Provider   string      `json:"provider"`
	Number     uint64      `json:"number"`
	Kind       AttemptKind `json:"kind"`
	Started    time.Time   `json:"started"`
	Finished   time.Time   `json:"finished"`
	DurationNS int64       `json:"duration_ns"`
	Status     int         `json:"status"`
	Outcome    Outcome     `json:"outcome"`
}
type RequestFact struct {
	ConversionDropped uint64    `json:"conversion_dropped"`
	Waited            bool      `json:"waited"`
	AttemptHistogram  Histogram `json:"attempt_histogram"`
	ID                string    `json:"id"`
	Epoch             string    `json:"epoch"`
	BeginInfo
	FinishInfo
	ReceivedProvider  string        `json:"received_provider"`
	Started           time.Time     `json:"started"`
	Finished          time.Time     `json:"finished"`
	DurationNS        int64         `json:"duration_ns"`
	QueueNS           int64         `json:"queue_ns"`
	Queued            bool          `json:"queued"`
	Live              bool          `json:"live"`
	AttemptCount      uint64        `json:"attempt_count"`
	Attempts          []AttemptFact `json:"attempts,omitempty"`
	AttemptsTruncated bool          `json:"attempts_truncated"`
	Usage             *Usage        `json:"usage,omitempty"`
	UsageCorrections  uint64        `json:"usage_corrections"`
	ConversionLosses  uint64        `json:"conversion_losses"`
	ConversionNotes   uint64        `json:"conversion_notes"`
	Fingerprint       string        `json:"fingerprint,omitempty"`
	JournalID         uint64        `json:"journal_id,omitempty"`
}

type Counters struct {
	ConversionDropped uint64 `json:"conversion_dropped"`
	Received          uint64 `json:"received"`
	Terminal          uint64 `json:"terminal"`
	Completed         uint64 `json:"completed"`
	Failed            uint64 `json:"failed"`
	Aborted           uint64 `json:"aborted"`
	Rejected          uint64 `json:"rejected"`
	Unknown           uint64 `json:"unknown"`
	Cancelled         uint64 `json:"cancelled"`
	Attempts          uint64 `json:"attempts"`
	Retries           uint64 `json:"retries"`
	AuthResends       uint64 `json:"auth_resends"`
	Upstream429       uint64 `json:"upstream_429"`
	Upstream401       uint64 `json:"upstream_401"`
	Upstream5xx       uint64 `json:"upstream_5xx"`
	ConversionLosses  uint64 `json:"conversion_losses"`
	ConversionNotes   uint64 `json:"conversion_notes"`
	UsageReports      uint64 `json:"usage_reports"`
	UsageInputKnown   uint64 `json:"usage_input_known"`
	UsageOutputKnown  uint64 `json:"usage_output_known"`
	UsageTotalKnown   uint64 `json:"usage_total_known"`
	UsageInputTokens  uint64 `json:"usage_input_tokens"`
	UsageOutputTokens uint64 `json:"usage_output_tokens"`
	UsageTotalTokens  uint64 `json:"usage_total_tokens"`
	UsageCorrections  uint64 `json:"usage_corrections"`
	UsageOverflow     uint64 `json:"usage_overflow"`
	// [downstream, upstream][1xx..5xx]; zero never means an HTTP response.
	HTTP     [2][5]uint64 `json:"http_status_classes"`
	Failures [8]uint64    `json:"failures_by_stage"`
}
type Histogram struct {
	Bins  [23]uint64 `json:"bins"`
	Count uint64     `json:"count"`
	Sum   float64    `json:"sum_seconds"`
}

// Gauge is integrated over real elapsed time at each transition. DurationNS is
// known coverage, not the width of the wall-clock interval.
type Gauge struct {
	Area       float64 `json:"area_value_nanoseconds"`
	DurationNS int64   `json:"duration_ns"`
	Min        int64   `json:"min"`
	Max        int64   `json:"max"`
	Last       int64   `json:"last"`
	Known      bool    `json:"known"`
}

func (g Gauge) Mean() (float64, bool) {
	if !g.Known || g.DurationNS <= 0 {
		return 0, false
	}
	return g.Area / float64(g.DurationNS), true
}

type Gauges struct {
	Active    Gauge `json:"active"`
	Queued    Gauge `json:"queued"`
	Exchanges Gauge `json:"exchanges"`
}
type Resource struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Kind     string `json:"kind"` // default, route or global; these are NOT additive.
	Limit    int64  `json:"limit"`
	Occupied int64  `json:"occupied"`
	Cooldown int64  `json:"cooldown"` // a subset of occupied, never another load.
	Queued   int64  `json:"queued"`
	Known    bool   `json:"known"`
}
type ResourceIntegral struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Limit    Gauge  `json:"limit"`
	Occupied Gauge  `json:"occupied"`
	Cooldown Gauge  `json:"cooldown"`
	Queued   Gauge  `json:"queued"`
}
type Interval struct {
	Samples            uint64             `json:"samples"`
	Epoch              string             `json:"epoch"`
	Sequence           uint64             `json:"sequence"`
	LastSequence       uint64             `json:"last_sequence,omitempty"`
	Provider           string             `json:"provider"`
	Start              time.Time          `json:"start"`
	End                time.Time          `json:"end"`
	ElapsedNS          int64              `json:"elapsed_ns"`
	ClockUncertaintyNS int64              `json:"clock_uncertainty_ns"`
	ClockStep          bool               `json:"clock_step"`
	Gap                bool               `json:"gap"`
	Overlap            bool               `json:"overlap"`
	Counts             Counters           `json:"counts"`
	Duration           Histogram          `json:"duration"`
	Admission          Histogram          `json:"admission"`
	AttemptDuration    Histogram          `json:"attempt_duration"`
	Gauges             Gauges             `json:"gauges"`
	Resources          []ResourceIntegral `json:"resources,omitempty"`
}
type Marker struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Provider  string    `json:"provider"`
	Kind      string    `json:"kind"`
	Summary   string    `json:"summary"`
	RequestID string    `json:"request_id,omitempty"`
}
type Packet struct {
	Intervals []Interval    `json:"intervals,omitempty"`
	Requests  []RequestFact `json:"requests,omitempty"`
	Markers   []Marker      `json:"markers,omitempty"`
}
type Health struct {
	BufferedBytes    uint64    `json:"buffered_bytes"`
	ByteCapacity     uint64    `json:"byte_capacity"`
	Enabled          bool      `json:"enabled"`
	Healthy          bool      `json:"healthy"`
	Pending          int       `json:"pending"`
	Capacity         int       `json:"capacity"`
	DroppedBatches   uint64    `json:"dropped_batches"`
	DroppedFacts     uint64    `json:"dropped_facts"`
	DroppedMarkers   uint64    `json:"dropped_markers"`
	WriteErrors      uint64    `json:"write_errors"`
	CommittedThrough time.Time `json:"committed_through"`
	LastError        string    `json:"last_error,omitempty"`
}
type ProviderSnapshot struct {
	Provider        string     `json:"provider"`
	Totals          Counters   `json:"totals"`
	Duration        Histogram  `json:"duration"`
	Admission       Histogram  `json:"admission"`
	AttemptDuration Histogram  `json:"attempt_duration"`
	Active          int64      `json:"active"`
	Queued          int64      `json:"queued"`
	Exchanges       int64      `json:"exchanges"`
	Resources       []Resource `json:"resources,omitempty"`
}
type Snapshot struct {
	At                time.Time          `json:"at"`
	Epoch             string             `json:"epoch"`
	Providers         []ProviderSnapshot `json:"providers"`
	History           Health             `json:"history"`
	LiveRequests      int                `json:"live_requests"`
	UntrackedRequests uint64             `json:"untracked_requests"`
}
type Query struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Providers []string  `json:"providers,omitempty"` // empty means all providers.
	MaxPoints int       `json:"max_points"`
}
type Point struct {
	Start           time.Time          `json:"start"`
	End             time.Time          `json:"end"`
	ElapsedNS       int64              `json:"elapsed_ns"`
	Coverage        float64            `json:"coverage"`
	Complete        bool               `json:"complete"`
	Partial         bool               `json:"partial"`
	Gap             bool               `json:"gap"`
	Overlap         bool               `json:"overlap"`
	ClockStep       bool               `json:"clock_step"`
	Samples         uint64             `json:"samples"`
	Counts          Counters           `json:"counts"`
	Duration        Histogram          `json:"duration"`
	Admission       Histogram          `json:"admission"`
	AttemptDuration Histogram          `json:"attempt_duration"`
	Gauges          Gauges             `json:"gauges"`
	Resources       []ResourceIntegral `json:"resources,omitempty"`
}
type ProviderSeries struct {
	Provider string  `json:"provider"`
	Points   []Point `json:"points"`
}
type QueryResult struct {
	ByProvider      []ProviderSeries `json:"by_provider,omitempty"`
	Query           Query            `json:"query"`
	Points          []Point          `json:"points"`
	Markers         []Marker         `json:"markers,omitempty"`
	SourceIntervals int              `json:"source_intervals"`
	Truncated       bool             `json:"truncated"`
	History         Health           `json:"history"`
}
type RequestQuery struct {
	DurationKind  string    `json:"duration_kind,omitempty"`
	DurationMinNS *int64    `json:"duration_min_ns,omitempty"`
	DurationMaxNS *int64    `json:"duration_max_ns,omitempty"`
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	Providers     []string  `json:"providers,omitempty"`
	Outcome       Outcome   `json:"outcome,omitempty"`
	Stage         Stage     `json:"stage,omitempty"`
	Search        string    `json:"search,omitempty"`
	Limit         int       `json:"limit"`
	Offset        int       `json:"offset"`
	LiveOnly      bool      `json:"live_only"`
}
type RequestResult struct {
	Requests  []RequestFact `json:"requests"`
	Total     int           `json:"total"`
	Truncated bool          `json:"truncated"`
}

// Exclusion prevents overlaying queued live intervals on their committed copy.
// Stores must fall back to child intervals when a rollup straddles the boundary.
type Exclusion struct {
	Epoch        string
	FromSequence uint64
}
type Source interface {
	Read(context.Context, Query, Exclusion) ([]Interval, []Marker, bool, error)
	Requests(context.Context, RequestQuery) (RequestResult, error)
	Health() Health
}
type Sink interface {
	Source
	Enqueue(Packet) bool // nonblocking; ownership of a deep immutable packet transfers.
	Close(context.Context) error
}
