package transcode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
)

func (h *TranscodeHandler) writeLocalError(
	r *http.Request,
	w http.ResponseWriter,
	status int,
	message string,
	provenance ExchangeProvenance,
) {
	apiErr := CanonicalAPIError{
		Status:  status,
		Type:    typeForStatus(status),
		Code:    codeForStatus(status),
		Message: message,
	}
	h.writeDialectHTTPError(r, w, apiErr, provenance)
}

// WriteCommittedStreamError failed-closes an already-committed SSE stream
// with a client-dialect error event. The re-declared WriteHeader never
// reaches the wire (the proxy's committed-stream guard silences it and
// marks the exchange aborted); the SSE error frame is the observable
// failure. A failed downstream write is returned so the outcome records
// the delivery fact exactly like the JSON error writers.
//
// It is exported so a route-selection refusal that happens before any
// handler runs (the multi-target dispatcher) emits exactly the frame the
// handler's own post-commit error path emits, instead of a second spelling
// of the same wire event.

func WriteCommittedStreamError(
	w http.ResponseWriter,
	client ClientProtocol,
	apiErr CanonicalAPIError,
) error {
	apiErr = normalizeCanonicalError(apiErr)
	// A writer that owns a failed-close comment (the proxy's committed-stream
	// recorder) must not add its own upstream-blame close once this terminal
	// is framed: the failure is local, or already stated by the dialect
	// event, and a second close comment would misreport the cause. The
	// recorder sits behind diagnostic wrappers, so the chain is walked.
	for cur := w; cur != nil; {
		if suppressor, ok := cur.(interface{ SuppressFailedCloseComment() }); ok {
			suppressor.SuppressFailedCloseComment()
			break
		}
		unwrapper, ok := cur.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		cur = unwrapper.Unwrap()
	}
	w.WriteHeader(apiErr.Status)
	var payload []byte
	var err error
	switch client {
	case ClientMessages:
		payload, err = json.Marshal(AnthropicStreamEvent{
			Type: AnthropicStreamEventTypeError,
			Error: &AnthropicStreamError{
				Type:    anthropicErrorType(apiErr),
				Message: apiErr.Message,
			},
		})
	case ClientResponses:
		code := apiErr.Code
		if code == "" {
			code = "conversion_error"
		}
		payload, err = json.Marshal(ResponseErrorEvent{
			Type:           "error",
			SequenceNumber: 0,
			Code:           code,
			Message:        apiErr.Message,
			Param:          "",
		})
	default:
		return fmt.Errorf("unknown client error dialect %q", client)
	}
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}

// writeDialectOrCommittedStreamError renders the canonical error as a dialect-legal
// SSE error event when the streaming representation was already committed, or as a
// standard dialect HTTP JSON error otherwise.

func (h *TranscodeHandler) writeDialectOrCommittedStreamError(
	r *http.Request,
	w http.ResponseWriter,
	client ClientProtocol,
	apiErr CanonicalAPIError,
) error {
	if CommittedStreamFromContext(r.Context()) {
		return WriteCommittedStreamError(w, client, apiErr)
	}
	return WriteDialectHTTPError(w, client, apiErr)
}

func (h *TranscodeHandler) writeCircuitOpenError(r *http.Request, w http.ResponseWriter) {
	client := ClientProtocol(h.cfg.Mapping.ClientProtocol)
	apiErr := CanonicalAPIError{
		Status:  http.StatusServiceUnavailable,
		Type:    "api_error",
		Code:    "circuit_open",
		Message: "circuit open",
	}
	apiErr.Message = h.boundErrorMessage(apiErr.Message)
	writeErr := h.writeDialectOrCommittedStreamError(r, w, client, apiErr)
	outcome := Outcome{
		UpstreamAttempted: false,
		UpstreamStatus:    Optional[int]{Value: http.StatusServiceUnavailable, Set: true},
		Provenance:        ProvenanceLocalRequestConversionError,
		UpstreamFailure:   false,
		LocalFailure:      true,
		CircuitRejected:   true,
	}
	if writeErr != nil {
		if r.Context().Err() != nil {
			outcome.Provenance = ProvenanceClientAbort
			outcome.ClientAborted = true
		} else {
			outcome.Provenance = ProvenanceDownstreamWriteError
		}
	} else {
		outcome.DownstreamComplete = true
	}
	h.recordOutcome(r, outcome)
}

// writeUpstreamHTTPError renders a non-2xx upstream response in the client
// dialect. The failure classification uses the response-aware breaker
// semantics (IsFailureStatusWithHeaders) so 429, 5xx, and 403 with
// rate-limit signals (Retry-After or x-ratelimit-* headers) are upstream
// failures — matching the native passthrough path exactly. The Retry-After
// from the ORIGINAL upstream response is recorded on the outcome so a
// rate-signalled 403 keeps its hold signal even when the rendered client
// error cannot carry the upstream rate-limit headers.

func (h *TranscodeHandler) writeUpstreamHTTPError(
	r *http.Request,
	w http.ResponseWriter,
	resp *http.Response,
	receivedAt time.Time,
	apiErr CanonicalAPIError,
) {
	// Evaluate the failure classification and Retry-After at outcome
	// construction time, anchored at header receipt: the error body was read
	// between the two, and the hold must measure from when the headers
	// arrived.
	evaluatedAt := time.Now()
	upstreamFailure := circuitbreaker.IsFailureStatusWithHeaders(
		apiErr.Status,
		resp.Header,
		receivedAt,
		evaluatedAt,
	)
	retryAfter := circuitbreaker.ParseRetryAfter(resp.Header, receivedAt, evaluatedAt)
	outcome := Outcome{
		UpstreamAttempted: true,
		UpstreamStatus:    Optional[int]{Value: apiErr.Status, Set: true},
		Provenance:        ProvenanceUpstreamHTTP,
		UpstreamFailure:   upstreamFailure,
		// Set=true whenever the upstream SIGNALED a hold, even when it has
		// already expired (a zero remaining hold is present-but-expired);
		// Set=false when no hold was signaled.
		RetryAfter: Optional[time.Duration]{
			Value: retryAfter,
			Set:   resp.Header.Get("Retry-After") != "",
		},
	}
	client := ClientProtocol(h.cfg.Mapping.ClientProtocol)
	apiErr.Message = h.boundErrorMessage(apiErr.Message)
	if err := h.writeDialectOrCommittedStreamError(r, w, client, apiErr); err != nil {
		// A failed downstream write is a fact about the exchange, not a
		// log line: the translated error was never delivered. The recorded
		// provenance changes to the downstream failure (or a client abort
		// when the context is cancelled) while the upstream facts (status,
		// failure, retry-after) are retained so the breaker still sees a
		// definitive upstream failure.
		if r.Context().Err() != nil {
			outcome.Provenance = ProvenanceClientAbort
			outcome.ClientAborted = true
		} else {
			outcome.Provenance = ProvenanceDownstreamWriteError
		}
	} else {
		outcome.DownstreamComplete = true
	}
	h.recordOutcome(r, outcome)
}

// writeUpstreamBodyError renders a client-dialect error for a failed
// non-2xx upstream body transfer: the error body could not be read within
// its bound, so the upstream's own status cannot be trusted as the exchange
// outcome. The exchange is recorded as an upstream body failure regardless
// of the status — a truncated 400 is never a healthy non-failure. A failed downstream write changes the provenance exactly like
// the other error writers while the upstream failure fact is retained.

func (h *TranscodeHandler) writeUpstreamBodyError(
	r *http.Request,
	w http.ResponseWriter,
	resp *http.Response,
	receivedAt time.Time,
	apiErr CanonicalAPIError,
) {
	client := ClientProtocol(h.cfg.Mapping.ClientProtocol)
	apiErr.Message = h.boundErrorMessage(apiErr.Message)
	writeErr := h.writeDialectOrCommittedStreamError(r, w, client, apiErr)
	retryAfter := circuitbreaker.ParseRetryAfter(resp.Header, receivedAt, time.Now())
	outcome := Outcome{
		UpstreamAttempted: true,
		UpstreamStatus:    Optional[int]{Value: apiErr.Status, Set: true},
		Provenance:        ProvenanceUpstreamBodyError,
		UpstreamFailure:   true,
		// The upstream's Retry-After is a header fact that survives the
		// failed body transfer: the rate-signalled hold must be recorded
		// even when the error body could not be read, anchored at header
		// receipt. Set=true whenever the
		// upstream signaled a hold, even when it has already expired.
		RetryAfter: Optional[time.Duration]{
			Value: retryAfter,
			Set:   resp.Header.Get("Retry-After") != "",
		},
	}
	if writeErr != nil {
		if r.Context().Err() != nil {
			outcome.Provenance = ProvenanceClientAbort
			outcome.ClientAborted = true
		} else {
			outcome.Provenance = ProvenanceDownstreamWriteError
		}
	} else {
		outcome.DownstreamComplete = true
	}
	h.recordOutcome(r, outcome)
}

// writeUpstreamSemanticFailure renders a client-dialect error for a typed
// upstream semantic failure (a 2xx envelope whose payload reports failure)
// and records the outcome with the upstream's HTTP status and
// UpstreamFailure=true, matching the streamed response.failed classification
// A failed downstream write changes the provenance
// exactly like the other error writers.

func (h *TranscodeHandler) writeUpstreamSemanticFailure(
	r *http.Request,
	w http.ResponseWriter,
	apiErr CanonicalAPIError,
	upstreamStatus int,
) {
	client := ClientProtocol(h.cfg.Mapping.ClientProtocol)
	apiErr.Message = h.boundErrorMessage(apiErr.Message)
	writeErr := h.writeDialectOrCommittedStreamError(r, w, client, apiErr)
	outcome := Outcome{
		UpstreamAttempted: true,
		UpstreamStatus:    Optional[int]{Value: upstreamStatus, Set: true},
		Provenance:        ProvenanceUpstreamHTTP,
		UpstreamFailure:   true,
	}
	if writeErr != nil {
		if r.Context().Err() != nil {
			outcome.Provenance = ProvenanceClientAbort
			outcome.ClientAborted = true
		} else {
			outcome.Provenance = ProvenanceDownstreamWriteError
		}
	} else {
		outcome.DownstreamComplete = true
	}
	h.recordOutcome(r, outcome)
}

// writeDialectHTTPError renders a canonical error in the client dialect and
// records the outcome. UpstreamFailure is derived from explicit provenance:
// only definitive upstream outcomes (transport errors and upstream HTTP >=
// 500) are upstream failures; local conversion errors are not. A failed
// downstream write changes the recorded provenance to the downstream failure
// (or a client abort when the context is cancelled) instead of being logged
// and ignored.

func (h *TranscodeHandler) writeDialectHTTPError(
	r *http.Request,
	w http.ResponseWriter,
	apiErr CanonicalAPIError,
	provenance ExchangeProvenance,
) {
	client := ClientProtocol(h.cfg.Mapping.ClientProtocol)
	// Every client-visible error message respects the configured
	// ErrorMessageBytes bound.
	apiErr.Message = h.boundErrorMessage(apiErr.Message)
	writeErr := h.writeDialectOrCommittedStreamError(r, w, client, apiErr)
	upstreamFailure := false
	switch provenance {
	case ProvenanceUpstreamTransportError:
		upstreamFailure = true
	case ProvenanceUpstreamHTTP, ProvenanceUpstreamBodyError:
		// Match the proxy's breaker classification (IsFailureStatus /
		// IsFailureStatusWithHeaders): 429 and 5xx are failures, and 403 is
		// a failure only when it carries a rate-limit signal (Retry-After).
		//
		// Disposition (no change): the upstream-failure
		// classification of body errors is pinned — a body
		// violating the wire contract IS an upstream health signal, and a
		// consistently-poisonous upstream SHOULD open the breaker
		// (TestHandlerCorruptUpstreamResponseIsUpstreamFailure). The
		// field-observed poison sources are eliminated at the decode layer
		// (the upstream-failure classification), not by widening breaker tolerance here.
		upstreamFailure = apiErr.Status == http.StatusTooManyRequests ||
			(apiErr.Status >= 500 && apiErr.Status < 600) ||
			(apiErr.Status == http.StatusForbidden && apiErr.RetryAfter != "")
	}
	outcome := Outcome{
		// The attempt fact reflects whether an upstream request was
		// dispatched: local REQUEST conversion and signing errors never
		// are; upstream HTTP/body/transport outcomes and local RESPONSE
		// conversion errors are.
		UpstreamAttempted: provenance != ProvenanceLocalRequestConversionError,
		UpstreamStatus:    Optional[int]{Value: apiErr.Status, Set: true},
		Provenance:        provenance,
		UpstreamFailure:   upstreamFailure,
		LocalFailure: provenance == ProvenanceLocalRequestConversionError ||
			provenance == ProvenanceLocalResponseConversionError ||
			provenance == ProvenanceLocalStreamValidationError,
	}
	if writeErr != nil {
		// The translated error was never delivered: the exchange did not
		// complete cleanly. Change the provenance to the downstream failure
		// (or a client abort when the context is cancelled); retained
		// upstream facts (status, failure) still classify the exchange.
		if r.Context().Err() != nil {
			outcome.Provenance = ProvenanceClientAbort
			outcome.ClientAborted = true
		} else {
			outcome.Provenance = ProvenanceDownstreamWriteError
		}
	} else {
		outcome.DownstreamComplete = true
	}
	h.recordOutcome(r, outcome)
}

// recordOutcome forwards the outcome to the proxy breaker hook and, when an
// outcome sink is present on the request context, to the proxy's per-request
// provenance reader.
