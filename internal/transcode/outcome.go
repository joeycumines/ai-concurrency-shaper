package transcode

// Exact per-request outcome accounting (review-z commit 4): every transcoded
// exchange records EXACTLY ONE outcome through a synchronous per-request
// sink. There is no non-blocking channel that can silently lose provenance:
// the handler installs a defer that records an internal local-failure outcome
// if no path recorded one, and the proxy reads the outcome after the handler
// returns — a missing outcome is an internal invariant violation.

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Outcome is the explicit record of one transcoded exchange. Every field is
// an explicit fact; breaker classification for transcoded exchanges uses ONLY
// this outcome.
type Outcome struct {
	// UpstreamAttempted is true when an upstream request was actually sent.
	UpstreamAttempted bool
	// UpstreamStatus is the upstream HTTP status when the exchange reached
	// an upstream response. Local request conversion errors carry the
	// rendered gateway status (e.g. 502) as the response the client
	// receives, which is not an upstream status (Set=false when no
	// response of any kind was rendered).
	UpstreamStatus Optional[int]
	// UpstreamFailure is true for a definitive upstream failure: corrupt
	// upstream wire, a transport error, an upstream HTTP >= 500, a 429, or
	// a rate-signalled 403. Local conversion errors are never upstream
	// failures.
	UpstreamFailure bool
	// RetryAfter is the REMAINING Retry-After hold signaled by the ORIGINAL
	// upstream response, anchored at header receipt (Set=false when the
	// upstream supplied none; Set with a zero value = present-but-expired).
	RetryAfter Optional[time.Duration]

	// Provenance is the explicit exchange provenance.
	Provenance ExchangeProvenance
	// ClientAborted is true when the client disconnected before the
	// exchange completed.
	ClientAborted bool
	// DownstreamComplete is true when the translated response was fully
	// written downstream.
	DownstreamComplete bool
	// LocalFailure is true when the exchange failed for a local reason
	// (request/response conversion, stream validation, rendering, signing):
	// never an upstream failure, never a breaker penalty.
	LocalFailure bool

	// StreamOutcome is the stream classification for streaming exchanges.
	StreamOutcome streamOutcome
}

// OutcomeSink is the synchronous per-request outcome sink: Record stores the
// FIRST outcome (subsequent records are ignored), Load reports whether one
// was recorded.
type OutcomeSink struct {
	once     sync.Once
	outcome  Outcome
	recorded atomic.Bool
}

// Record stores the outcome exactly once.
func (s *OutcomeSink) Record(outcome Outcome) {
	s.once.Do(func() {
		s.outcome = outcome
		s.recorded.Store(true)
	})
}

// Load returns the recorded outcome and whether one was recorded. The
// recorded flag is acquired BEFORE the outcome is read, so a true result is
// always paired with the fully-published outcome (Record stores the outcome
// before the release-store of the flag) — a Load that observed the flag can
// never observe a stale outcome (review-z commit 4).
func (s *OutcomeSink) Load() (Outcome, bool) {
	if !s.recorded.Load() {
		return Outcome{}, false
	}
	return s.outcome, true
}

// outcomeContextKey carries the OutcomeSink through the request context.
type outcomeContextKey struct{}

// WithOutcomeSink attaches a synchronous outcome sink to the request context.
func WithOutcomeSink(ctx context.Context) (context.Context, *OutcomeSink) {
	sink := &OutcomeSink{}
	return context.WithValue(ctx, outcomeContextKey{}, sink), sink
}

// OutcomeSinkFromContext returns the request's outcome sink, or nil when the
// request was not dispatched through the proxy's transcoded-route path.
func OutcomeSinkFromContext(ctx context.Context) *OutcomeSink {
	if sink, _ := ctx.Value(outcomeContextKey{}).(*OutcomeSink); sink != nil {
		return sink
	}
	return nil
}

// LocalFailureOutcome builds the internal local-failure outcome recorded by
// the handler's defer when no path recorded a real outcome: the exchange
// never completed through any documented path, which is an internal failure.
func LocalFailureOutcome() Outcome {
	return Outcome{
		Provenance:     ProvenanceLocalStreamValidationError,
		LocalFailure:   true,
		UpstreamStatus: Optional[int]{Set: true, Value: http.StatusBadGateway},
	}
}
