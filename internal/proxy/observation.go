package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/diagnostic"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

type observationStateKey struct{}
type admissionDeadlineKey struct{}
type authResendKey struct{}
type transportObservationState struct {
	next              atomic.Uint64
	mu                sync.Mutex
	lastStatus        int
	bodyFailure       bool
	transportFailure  bool
	localFailureStage observation.Stage
}

func (p *Proxy) beginObservedRequest(r *http.Request) *http.Request {
	ctx := r.Context()
	request := observation.FromContext(ctx)
	if request == nil && p.observations != nil {
		ctx, request = p.observations.Begin(ctx, observation.BeginInfo{Provider: p.providerName, Method: r.Method, Path: r.URL.Path, Route: p.statsRouteKey(r)})
	}
	if request != nil {
		if p.providerName != "" {
			request.SetProvider(p.providerName)
		}
		routeKey := p.statsRouteKey(r)
		if routeKey == "" {
			if pattern := p.matcher.FindMatch(r.Method, r.URL.Path); pattern != nil {
				routeKey = pattern.Raw
				if pattern.Group != "" {
					routeKey = pattern.Group
				}
			}
		}
		request.SetRoute(routeKey)
		ctx = context.WithValue(ctx, observationStateKey{}, &transportObservationState{})
	}
	if p.runtimeControl != nil && p.timeout > 0 {
		ctx = context.WithValue(ctx, admissionDeadlineKey{}, time.Now().Add(p.timeout))
	}
	if request == nil && (p.runtimeControl == nil || p.timeout <= 0) {
		return r
	}
	return r.WithContext(ctx)
}

// admissionContext shares one queue budget across an operator pause and all
// limiter phases, without attaching a queue-only deadline to upstream I/O.
func (p *Proxy) admissionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	if deadline, ok := ctx.Value(admissionDeadlineKey{}).(time.Time); ok {
		return context.WithDeadline(ctx, deadline)
	}
	return context.WithTimeout(ctx, p.timeout)
}

func observedFinishInfo(r *http.Request, rec *statusRecorder, aborted bool) observation.FinishInfo {
	info := observation.FinishInfo{Outcome: observation.OutcomeUnknown, Stage: observation.StageUnknown, Cancelled: r.Context().Err() != nil}
	if rec == nil {
		return info
	}
	info.Status = rec.status
	state, _ := r.Context().Value(observationStateKey{}).(*transportObservationState)
	var attempts uint64
	var bodyFailure, transportFailure bool
	var lastStatus int
	var localFailureStage observation.Stage
	if state != nil {
		attempts = state.next.Load()
		state.mu.Lock()
		bodyFailure, transportFailure = state.bodyFailure, state.transportFailure
		lastStatus = state.lastStatus
		localFailureStage = state.localFailureStage
		state.mu.Unlock()
	}
	if o := rec.transcodeOutcome; o != nil {
		info.Cancelled = info.Cancelled || o.ClientAborted
		switch o.Provenance {
		case transcode.ProvenanceLocalRequestConversionError:
			info.Outcome, info.Stage = observation.OutcomeFailed, observation.StageConversionRequest
		case transcode.ProvenanceLocalResponseConversionError, transcode.ProvenanceLocalStreamValidationError:
			info.Outcome, info.Stage = observation.OutcomeFailed, observation.StageConversionResponse
		case transcode.ProvenanceDownstreamWriteError:
			info.Outcome, info.Stage = observation.OutcomeFailed, observation.StageDownstream
		case transcode.ProvenanceClientAbort:
			info.Outcome = observation.OutcomeAborted
		default:
			if o.UpstreamFailure || (o.UpstreamStatus.Set && o.UpstreamStatus.Value >= 400) {
				info.Outcome, info.Stage = observation.OutcomeFailed, observation.StageUpstream
			} else if o.DownstreamComplete && !aborted {
				info.Outcome = observation.OutcomeCompleted
			} else {
				info.Outcome = observation.OutcomeAborted
			}
		}
		// A known failed upstream body/status is retained if a concurrent
		// cancellation or failed downstream error write follows it.
		if o.UpstreamFailure && (info.Outcome == observation.OutcomeAborted || info.Stage == observation.StageDownstream) {
			info.Outcome, info.Stage = observation.OutcomeFailed, observation.StageUpstream
		}
		if o.CircuitRejected {
			info.Outcome, info.Stage = observation.OutcomeRejected, observation.StageAdmission
		}
	} else {
		switch {
		case bodyFailure || transportFailure || (attempts > 0 && lastStatus >= 400):
			info.Outcome, info.Stage = observation.OutcomeFailed, observation.StageUpstream
		case rec.localUpgradeFailure || rec.localPanic:
			info.Outcome, info.Stage = observation.OutcomeFailed, observation.StageProxy
		case rec.downstreamWriteFailed():
			info.Outcome, info.Stage = observation.OutcomeFailed, observation.StageDownstream
		case info.Cancelled || aborted:
			info.Outcome = observation.OutcomeAborted
		case attempts == 0 && !rec.admissionCompleted && info.Status >= 400:
			info.Outcome, info.Stage = observation.OutcomeRejected, observation.StageAdmission
		case info.Status >= 400:
			info.Outcome = observation.OutcomeFailed
			if attempts > 0 && !rec.proxyGeneratedError {
				info.Stage = observation.StageUpstream
			} else {
				info.Stage = observation.StageProxy
			}
		case info.Status > 0:
			info.Outcome = observation.OutcomeCompleted
		}
	}
	if localFailureStage == observation.StageAuthentication {
		info.Outcome, info.Stage = observation.OutcomeFailed, observation.StageAuthentication
		if info.Cancelled {
			info.Outcome = observation.OutcomeAborted
		}
	}
	if rec.retryCircuitOpen() {
		info.Outcome, info.Stage = observation.OutcomeRejected, observation.StageAdmission
	}
	return info
}

// observeAttempt is called immediately inside signing, at the actual transport
// boundary. Header receipt is separate from body completion. It never consumes
// bytes on its own and leaves upgraded connection interfaces untouched.
func observeAttempt(req *http.Request, inner http.RoundTripper) (resp *http.Response, err error) {
	request := observation.FromContext(req.Context())
	if request == nil {
		return inner.RoundTrip(req)
	}
	state, _ := req.Context().Value(observationStateKey{}).(*transportObservationState)
	kind := observation.AttemptInitial
	if state != nil && state.next.Add(1) > 1 {
		kind = observation.AttemptRetry
	}
	if resend, _ := req.Context().Value(authResendKey{}).(bool); resend {
		kind = observation.AttemptAuthResend
	}
	attempt := request.BeginAttempt(kind)
	returned := false
	defer func() {
		if !returned {
			attempt.Finish(observation.OutcomeUnknown)
		}
	}()
	resp, err = inner.RoundTrip(req)
	returned = true
	if state != nil {
		state.mu.Lock()
		state.bodyFailure = false
		state.transportFailure = err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
		state.lastStatus = 0
		if resp != nil {
			state.lastStatus = resp.StatusCode
		}
		state.mu.Unlock()
	}
	if resp != nil {
		attempt.Headers(resp.StatusCode)
	}
	if err != nil {
		outcome := observation.OutcomeFailed
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = observation.OutcomeAborted
		}
		attempt.Finish(outcome)
		return
	}
	if resp == nil {
		attempt.Finish(observation.OutcomeUnknown)
		return
	}
	terminal := observation.OutcomeCompleted
	if resp.StatusCode >= 400 {
		terminal = observation.OutcomeFailed
	}
	if resp.StatusCode == http.StatusSwitchingProtocols || resp.Body == nil || resp.Body == http.NoBody {
		attempt.Finish(terminal)
		return
	}
	b := &observedAttemptBody{ReadCloser: resp.Body, attempt: attempt, ctx: req.Context(), state: state, terminal: terminal}
	if rwc, ok := resp.Body.(io.ReadWriteCloser); ok {
		if cw, ok := resp.Body.(closeWriter); ok {
			resp.Body = &observedAttemptReadWriteCloseWriter{observedAttemptBody: b, writer: rwc, closeWriter: cw}
		} else {
			resp.Body = &observedAttemptReadWriteBody{observedAttemptBody: b, writer: rwc}
		}
	} else {
		resp.Body = b
	}
	return
}

type observedAttemptBody struct {
	io.ReadCloser
	attempt  *observation.Attempt
	ctx      context.Context
	state    *transportObservationState
	terminal observation.Outcome
	once     sync.Once
}

func (b *observedAttemptBody) finish(o observation.Outcome) {
	b.once.Do(func() { b.attempt.Finish(o) })
}
func (b *observedAttemptBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		switch {
		case errors.Is(err, io.EOF):
			b.finish(b.terminal)
		case (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && b.ctx.Err() != nil:
			b.finish(observation.OutcomeAborted)
		default:
			if b.state != nil {
				b.state.mu.Lock()
				b.state.bodyFailure = true
				b.state.mu.Unlock()
			}
			b.finish(observation.OutcomeFailed)
		}
	}
	return n, err
}
func (b *observedAttemptBody) Close() error {
	err := b.ReadCloser.Close()
	// Closing unread success bytes proves neither EOF nor successful content.
	outcome := observation.OutcomeUnknown
	if b.terminal == observation.OutcomeFailed {
		outcome = observation.OutcomeFailed
	} else if b.ctx.Err() != nil {
		outcome = observation.OutcomeAborted
	}
	b.finish(outcome)
	return err
}

type observedAttemptReadWriteBody struct {
	*observedAttemptBody
	writer io.Writer
}

func (b *observedAttemptReadWriteBody) Write(p []byte) (int, error) { return b.writer.Write(p) }

type observedAttemptReadWriteCloseWriter struct {
	observedAttemptReadWriteBody
	closeWriter
}

// annotateObservedFailure runs before journal publication. Details are controlled
// descriptions of gateway behavior, never error strings containing URLs, headers
// or credentials. The existing codec/writer failure remains the primary evidence
// unless an observed upstream failure takes precedence over a delivery failure.
func annotateObservedFailure(r *http.Request, rec *statusRecorder, info observation.FinishInfo) {
	capture := diagnostic.FromContext(r.Context())
	if capture == nil || info.Outcome == observation.OutcomeCompleted || info.Outcome == observation.OutcomeUnknown {
		return
	}
	fingerprint := capture.FirstFailureFingerprint()
	state, _ := r.Context().Value(observationStateKey{}).(*transportObservationState)
	kind, detail := "request_failed", "The logical exchange did not complete successfully."
	switch info.Stage {
	case observation.StageAdmission:
		kind, detail = "admission_refused", "The gateway refused admission locally; no upstream response is implied."
	case observation.StageAuthentication:
		kind, detail = "authentication_failed", "Upstream authentication could not be prepared locally."
	case observation.StageUpstream:
		kind, detail = "http_error", "The selected upstream response reported an error status."
		if state != nil {
			state.mu.Lock()
			if state.bodyFailure {
				kind, detail = "body_read_failed", "The upstream body ended with a non-EOF read error."
			} else if state.transportFailure {
				kind, detail = "round_trip_failed", "The actual upstream transport failed before a complete HTTP response."
			}
			state.mu.Unlock()
		}
	case observation.StageDownstream:
		kind, detail = "delivery_failed", "The downstream writer did not accept the complete response."
	case observation.StageProxy:
		kind, detail = "gateway_failed", "The gateway failed locally."
		if rec != nil && rec.localPanic {
			kind, detail = "local_panic", "The gateway recovered a local panic."
		}
	case observation.StageConversionRequest:
		kind, detail = "request_conversion_failed", "The request could not be represented under the configured conversion contract."
	case observation.StageConversionResponse:
		kind, detail = "response_conversion_failed", "The response could not be represented under the configured conversion contract."
	default:
		if info.Cancelled {
			kind, detail = "request_cancelled", "The request context was cancelled before completion."
		}
	}
	if fingerprint == "" || (info.Stage == observation.StageUpstream && rec != nil && rec.downstreamWriteFailed()) {
		fingerprint = capture.Failure(string(info.Stage), kind, "", "", detail)
	}
	observation.FromContext(r.Context()).SetFingerprint(fingerprint)
}
