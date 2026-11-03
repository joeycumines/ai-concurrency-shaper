package transcode

import (
	"fmt"
	"net/http"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
)

// streamResponse translates an upstream SSE stream into client SSE events,
// delegating copy and cancellation to stream.Proxy and sealing downstream
// writes before returning.
func (h *TranscodeHandler) streamResponse(
	w http.ResponseWriter,
	r *http.Request,
	resp *http.Response,
	context *ExchangeContext,
	receivedAt time.Time,
) {
	converter, err := h.newFrameConverter(context)
	if err != nil {
		// writeLocalError -> writeDialectHTTPError records the outcome
		// exactly once.
		h.logRequestError(r, fmt.Errorf("[%s] build stream converter: %s", ProvenanceLocalRequestConversionError, h.boundErrorMessage(err.Error())))
		h.writeLocalError(r, w,
			http.StatusInternalServerError, "build stream converter: "+err.Error(),
			ProvenanceLocalRequestConversionError)
		return
	}

	// Copy entity headers without Connection (the SSE writer must not
	// reintroduce a hop-by-hop header, which is invalid for HTTP/2) and
	// without transformed-representation headers: the translated stream is
	// a different representation than the upstream body, so Content-Length,
	// ETag, and Digest from the upstream would be stale and corrupting.
	// Connection-nominated tokens are resolved from the upstream source
	// before the copy (the copy itself skips the Connection header).
	RemoveHopByHopHeaders(resp.Header)
	h.copyResponseHeaders(w.Header(), resp.Header)
	RemoveTransformedRepresentationHeaders(w.Header())
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)

	reader := newConvertingReaderWithLimits(NewSSEReaderWithLimits(
		resp.Body,
		h.cfg.BodyLimits.SSELineBytes,
		h.cfg.BodyLimits.SSEFrameBytes,
	), converter,
		h.cfg.BodyLimits.GeneratedSSEFrameBytes,
		h.cfg.BodyLimits.GeneratedSSEBatchBytes,
		h.cfg.BodyLimits.ErrorMessageBytes,
	)
	observation := runTranslatedStream(r.Context(), w, resp.Body, reader)

	// DownstreamComplete reflects the actual write state, not the
	// classification bucket: a converted upstream error frame whose
	// downstream write failed was never delivered, and the exchange must
	// not be recorded as a clean completion. The streaming path needs no
	// short-write check of its own: every downstream frame is written
	// through sealedSSEWriter.writeAll, which treats a partial write as
	// io.ErrShortWrite, and the writer's first error surfaces as WriterErr.
	downstreamComplete := observation.WriterErr == nil && observation.SealErr == nil

	// Classify the outcome from explicit provenance. The classification is
	// recorded on the outcome  so downstream sinks
	// observe the truthful stream bucket for every exchange, not the zero
	// value.
	var outcome Outcome
	classification := classifyStreamObservation(observation)
	outcome.StreamOutcome = classification
	switch classification {
	case streamOutcomeSuccess:
		outcome.UpstreamAttempted = true
		outcome.UpstreamStatus = Optional[int]{Value: resp.StatusCode, Set: true}
		outcome.Provenance = ProvenanceUpstreamHTTP
		outcome.DownstreamComplete = downstreamComplete
	case streamOutcomeClientAbort:
		outcome.UpstreamAttempted = true
		outcome.UpstreamStatus = Optional[int]{Value: resp.StatusCode, Set: true}
		outcome.Provenance = ProvenanceClientAbort
		outcome.ClientAborted = true
	case streamOutcomeUpstreamFailure:
		outcome.UpstreamAttempted = true
		outcome.UpstreamStatus = Optional[int]{Value: resp.StatusCode, Set: true}
		outcome.Provenance = ProvenanceUpstreamBodyError
		outcome.UpstreamFailure = true
		outcome.DownstreamComplete = downstreamComplete
		// A hold signaled on the streamed response is a header fact that
		// survives the failed stream, anchored at the TRUE header-receipt
		// time captured when the upstream response arrived: the remaining
		// hold excludes the stream duration, exactly as the JSON error
		// writers record it. Set=true whenever the upstream signaled, even
		// when already expired.
		outcome.RetryAfter = Optional[time.Duration]{
			Value: circuitbreaker.ParseRetryAfter(resp.Header, receivedAt, time.Now()),
			Set:   resp.Header.Get("Retry-After") != "",
		}
	case streamOutcomeLocalConversionFailure:
		outcome.UpstreamAttempted = true
		outcome.UpstreamStatus = Optional[int]{Value: http.StatusBadGateway, Set: true}
		outcome.Provenance = ProvenanceLocalResponseConversionError
		outcome.LocalFailure = true
		outcome.DownstreamComplete = downstreamComplete
		h.logStreamConversionError(r, observation.ReaderErr)
	case streamOutcomeDownstreamFailure:
		outcome.UpstreamAttempted = true
		outcome.UpstreamStatus = Optional[int]{Value: resp.StatusCode, Set: true}
		outcome.Provenance = ProvenanceDownstreamWriteError
	default:
		outcome.UpstreamAttempted = true
		outcome.UpstreamStatus = Optional[int]{Value: resp.StatusCode, Set: true}
		outcome.Provenance = ProvenanceUpstreamHTTP
		outcome.DownstreamComplete = downstreamComplete
	}
	h.recordOutcome(r, outcome)
	// The streamed classification ran: the diagnostic record may now report
	// StreamOutcome (its zero value is a success, so an exchange that never
	// reached this point must not claim one).
	flowFromContext(r.Context()).setStreamClassified()
	// Response-side approved losses are logged with the same fidelity as
	// request-side losses.
	h.logConversionReport(*converter.ConversionReport(), r, "response")
	// Opt-in continuity for streams: on a successful Responses-client
	// stream, retain the conversation that produced the emitted id
	// (request turns + terminal assistant turns) so a follow-up works
	// exactly like the non-streaming path. Any converter implementing
	// continuityTurnSource participates; every other direction is a no-op.
	if classification == streamOutcomeSuccess && h.cfg.Continuity != nil &&
		h.cfg.Mapping.ClientProtocol == ClientResponses {
		if src, ok := converter.(continuityTurnSource); ok && src != nil {
			if id, turns, depth, omitted, ok := src.continuityTurns(context.RequestTurns, context.RequestDepth); ok {
				if omitted > 0 {
					h.logRequestError(r, fmt.Errorf(
						"[%s] continuity retained history omitted %d tool call(s) whose arguments are not a JSON object; a follow-up previous_response_id replays without them",
						ProvenanceLocalResponseConversionError, omitted,
					))
				}
				storeFalse := false
				if echo := context.OriginalResponsesRequest; echo != nil && echo.Store != nil {
					storeFalse = !*echo.Store
				}
				if dropped := h.cfg.Continuity.RecordEvicted(h.cfg.ContinuityKey, id, turns, depth, storeFalse); dropped > 0 {
					h.logRequestError(r, fmt.Errorf("[local_response_conversion_error] continuity store evicted %d chain(s) at capacity/TTL bound", dropped))
				}
			}
		}
	}
}

// newFrameConverter builds the direction-specific stream converter.
func (h *TranscodeHandler) newFrameConverter(
	context *ExchangeContext,
) (frameConverter, error) {
	client := h.cfg.Mapping.ClientProtocol
	upstream := h.cfg.Mapping.UpstreamProtocol

	responseID := context.IDs.New("resp_")
	createdAt := nowUnix()
	model := context.RequestedClientModel

	switch {
	case client == ClientResponses && upstream == UpstreamChatCompletions:
		state := newChatResponsesStreamState(
			context,
			h.cfg.Mapping.LossPolicy,
			h.cfg.Mapping.ChatCapabilities,
			responseID,
			model,
			createdAt,
			context.OriginalResponsesRequest,
		)
		return newChatToResponsesConverter(state), nil

	case client == ClientGemini && upstream == UpstreamChatCompletions:
		chat := newChatResponsesStreamState(
			context,
			h.cfg.Mapping.LossPolicy,
			h.cfg.Mapping.ChatCapabilities,
			responseID,
			model,
			createdAt,
			nil, // Responses envelope is internal to the composition
		)
		gemini := newGeminiStreamClientState(context, h.cfg.Mapping.LossPolicy, responseID, model)
		return newChatToGeminiConverter(chat, gemini), nil

	case client == ClientGemini && upstream == UpstreamResponses:
		state := newGeminiStreamClientState(context, h.cfg.Mapping.LossPolicy, responseID, model)
		return newResponsesToGeminiConverter(state), nil

	case client == ClientGemini && upstream == UpstreamMessages:
		upstreamState := newAnthropicUpstreamResponsesStreamState(responseID, model, h.cfg.Mapping.LossPolicy)
		gemini := newGeminiStreamClientState(context, h.cfg.Mapping.LossPolicy, responseID, model)
		return newMessagesToGeminiConverter(upstreamState, gemini), nil

	case client == ClientMessages && upstream == UpstreamResponses:
		state := newAnthropicResponsesStreamState(
			context,
			h.cfg.Mapping.LossPolicy,
			h.cfg.Mapping.ChatCapabilities,
			context.IDs.New("msg_"),
			model,
			createdAt,
		)
		return newResponsesToAnthropicConverter(state), nil

	case client == ClientResponses && upstream == UpstreamGemini:
		state := newGeminiResponsesStreamState(
			context,
			h.cfg.Mapping.LossPolicy,
			h.cfg.Mapping.ChatCapabilities,
			responseID,
			model,
			createdAt,
			context.OriginalResponsesRequest,
		)
		return newGeminiToResponsesConverter(state), nil

	case client == ClientMessages && upstream == UpstreamGemini:
		gemini := newGeminiResponsesStreamState(
			context,
			h.cfg.Mapping.LossPolicy,
			h.cfg.Mapping.ChatCapabilities,
			responseID,
			model,
			createdAt,
			nil, // Responses envelope is internal to the composition
		)
		anthropic := newAnthropicResponsesStreamState(
			context,
			h.cfg.Mapping.LossPolicy,
			h.cfg.Mapping.ChatCapabilities,
			context.IDs.New("msg_"),
			model,
			createdAt,
		)
		return newGeminiToAnthropicConverter(gemini, anthropic), nil

	case client == ClientMessages && upstream == UpstreamChatCompletions:
		chat := newChatResponsesStreamState(
			context,
			h.cfg.Mapping.LossPolicy,
			h.cfg.Mapping.ChatCapabilities,
			responseID,
			model,
			createdAt,
			nil, // Responses envelope is internal to the composition
		)
		anthropic := newAnthropicResponsesStreamState(
			context,
			h.cfg.Mapping.LossPolicy,
			h.cfg.Mapping.ChatCapabilities,
			context.IDs.New("msg_"),
			model,
			createdAt,
		)
		return newChatToAnthropicConverter(chat, anthropic), nil

	default:
		return nil, fmt.Errorf(
			"unsupported stream direction %q -> %q",
			client,
			upstream,
		)
	}
}
