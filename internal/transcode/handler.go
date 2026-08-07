package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
)

// HandlerConfig configures one transcoded route.
type HandlerConfig struct {
	// Mapping is the validated route mapping.
	Mapping Mapping

	// Upstream is the upstream base URL.
	Upstream *url.URL

	// BodyLimits are the independent request/response body limits.
	BodyLimits BodyLimits

	// AuthPolicy authenticates the upstream request.
	AuthPolicy AuthPolicy

	// ModelMap resolves client model identifiers.
	ModelMap ModelMap

	// LossPolicy decides whether non-portable features may be dropped.
	LossPolicy LossPolicy

	// ChatCapabilities gates provider-extension fields for the Chat
	// upstream.
	ChatCapabilities ChatCapabilities

	// AllowedClientQuery is the set of client query parameters permitted on
	// the transcoded route. Unknown client query parameters are rejected.
	AllowedClientQuery map[string]struct{}
}

// RoundTrip executes an outbound HTTP request through the proxy engine.
type RoundTrip func(*http.Request) (*http.Response, error)

// ExchangeProvenance is the explicit outcome provenance recorded for breaker
// accounting. Breaker health is derived from provenance, never from the final
// status code alone.
//
// https://platform.openai.com/docs/guides/error-codes/api-errors
type ExchangeProvenance uint8

// ExchangeProvenance values.
const (
	ProvenanceUpstreamHTTP ExchangeProvenance = iota
	ProvenanceUpstreamTransportError
	ProvenanceUpstreamBodyError
	ProvenanceClientAbort
	ProvenanceDownstreamWriteError
	ProvenanceLocalRequestConversionError
	ProvenanceLocalResponseConversionError
	ProvenanceLocalStreamValidationError
)

func (p ExchangeProvenance) String() string {
	switch p {
	case ProvenanceUpstreamHTTP:
		return "upstream_http"
	case ProvenanceUpstreamTransportError:
		return "upstream_transport_error"
	case ProvenanceUpstreamBodyError:
		return "upstream_body_error"
	case ProvenanceClientAbort:
		return "client_abort"
	case ProvenanceDownstreamWriteError:
		return "downstream_write_error"
	case ProvenanceLocalRequestConversionError:
		return "local_request_conversion_error"
	case ProvenanceLocalResponseConversionError:
		return "local_response_conversion_error"
	case ProvenanceLocalStreamValidationError:
		return "local_stream_validation_error"
	default:
		return "unknown"
	}
}

// Outcome is the recorded result of one transcoded exchange.
type Outcome struct {
	Status     int
	Provenance ExchangeProvenance

	// UpstreamFailure is true when the outcome is a definitive upstream
	// failure (transport error or upstream HTTP >= 500).
	UpstreamFailure bool

	// StreamOutcome is the stream classification for streaming exchanges.
	StreamOutcome streamOutcome
}

// OutcomeFunc receives the exchange outcome. The proxy wires this to breaker
// accounting. It must be safe to call at most once per exchange, after the
// handler has stopped mutating the recorder.
type OutcomeFunc func(Outcome)

// outcomeContextKey carries the recorded Outcome through the request context
// so the proxy can read explicit provenance after ServeHTTP returns. This is
// per-request state and is race-free.
type outcomeContextKey struct{}

// WithOutcomeSink attaches a buffered outcome sink to the request context.
// The handler records the outcome here in addition to invoking OutcomeFunc.
// The sink is buffered so the handler never blocks.
func WithOutcomeSink(ctx context.Context) (context.Context, chan Outcome) {
	sink := make(chan Outcome, 1)
	return context.WithValue(ctx, outcomeContextKey{}, sink), sink
}

// TranscodeHandler intercepts requests for a transcoded route, converts the
// payload to the upstream schema, forwards through the proxy engine, and
// converts the response back to the client schema. Streaming responses are
// translated incrementally through the state machines.
//
// The stream path uses stream.Proxy as the mandated bidirectional copy and
// cancellation boundary. The converted HTTP request body has already been
// submitted by RoundTrip; stream.Proxy's local EOF triggers the configured
// adapter soft-close; the handler owns downstream sealing and body closure;
// the tests verify translated-stream continuation and cancellation, not a new
// transport-level request-body half-close mechanism.
type TranscodeHandler struct {
	cfg       HandlerConfig
	roundTrip RoundTrip
	outcomeFn OutcomeFunc
}

// NewTranscodeHandler returns a handler for the given configuration.
func NewTranscodeHandler(
	cfg HandlerConfig,
	roundTrip RoundTrip,
	outcomeFn OutcomeFunc,
) *TranscodeHandler {
	return &TranscodeHandler{
		cfg:       cfg,
		roundTrip: roundTrip,
		outcomeFn: outcomeFn,
	}
}

// ClientPath returns the client-facing route path of the handler.
func (h *TranscodeHandler) ClientPath() string {
	return h.cfg.Mapping.ClientRoute.Path
}

// ServeHTTP implements http.Handler.
func (h *TranscodeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Reject Upgrade requests on transcoded routes: a 101 Switching
	// Protocols response cannot be meaningfully schema-transcoded.
	if isUpgradeRequest(r) {
		h.writeLocalError(r, w, ClientProtocol(h.cfg.Mapping.ClientProtocol),
			http.StatusBadRequest, "upgrade requests are not supported on transcoded routes",
			ProvenanceLocalRequestConversionError)
		return
	}

	// Reject non-identity request Content-Encoding with a dialect-correct
	// 415. JSON-decoding compressed bytes as plain input is not adequate.
	if r.Header.Get("Content-Encoding") != "" {
		apiErr := CanonicalAPIError{
			Status:  http.StatusUnsupportedMediaType,
			Type:    "invalid_request_error",
			Code:    "unsupported_content_encoding",
			Message: "content-encoding is not supported on transcoded routes",
		}
		h.writeDialectHTTPError(r, w, apiErr, ProvenanceLocalRequestConversionError)
		return
	}

	body, err := h.readRequestBody(r)
	if err != nil {
		if r.Context().Err() != nil {
			h.recordOutcome(r, Outcome{Provenance: ProvenanceClientAbort})
			// A cancelled client context means the exchange is already over.
			// Return normally: the proxy classifies the abort from the
			// recorded outcome, and the net/http server tolerates a handler
			// that returns without writing to a dead connection.
			return
		}
		if errors.Is(err, errRequestBodyTooLarge) {
			h.writeLocalError(r, w, ClientProtocol(h.cfg.Mapping.ClientProtocol),
				http.StatusRequestEntityTooLarge, "request body too large",
				ProvenanceLocalRequestConversionError)
			return
		}
		h.writeLocalError(r, w, ClientProtocol(h.cfg.Mapping.ClientProtocol),
			http.StatusBadRequest, "read request body: "+err.Error(),
			ProvenanceLocalRequestConversionError)
		return
	}

	// Decode the source request into the canonical IR and render the target
	// request, resolving the model through the map.
	upstreamBody, context, err := h.convertRequest(r, body)
	if err != nil {
		if r.Context().Err() != nil {
			h.recordOutcome(r, Outcome{Provenance: ProvenanceClientAbort})
			// A cancelled client context means the exchange is already over.
			// Return normally: the proxy classifies the abort from the
			// recorded outcome, and the net/http server tolerates a handler
			// that returns without writing to a dead connection.
			return
		}
		h.writeLocalError(r, w, ClientProtocol(h.cfg.Mapping.ClientProtocol),
			http.StatusBadRequest, "convert request: "+err.Error(),
			ProvenanceLocalRequestConversionError)
		return
	}

	outReq, err := h.buildUpstreamRequest(r, upstreamBody)
	if err != nil {
		// An unallowed client query parameter or an invalid inbound
		// credential is a client fault (400); other request-construction
		// failures are internal (500).
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "client query parameter") ||
			errors.Is(err, errAuthInboundCredential) {
			status = http.StatusBadRequest
		}
		h.writeLocalError(r, w, ClientProtocol(h.cfg.Mapping.ClientProtocol),
			status, "build upstream request: "+err.Error(),
			ProvenanceLocalRequestConversionError)
		return
	}

	resp, err := h.roundTrip(outReq)
	if err != nil {
		// A RoundTripper may return a non-nil response alongside an error;
		// release its body so the connection is not leaked (sound behavior).
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		// A cancelled client context means the exchange is already over.
		if r.Context().Err() != nil {
			h.recordOutcome(r, Outcome{Provenance: ProvenanceClientAbort})
			// A cancelled client context means the exchange is already over.
			// Return normally: the proxy classifies the abort from the
			// recorded outcome, and the net/http server tolerates a handler
			// that returns without writing to a dead connection.
			return
		}
		h.writeDialectHTTPError(r, w, CanonicalAPIError{
			Status:  http.StatusBadGateway,
			Type:    "api_error",
			Code:    "upstream_transport_error",
			Message: "upstream request failed: " + err.Error(),
		}, ProvenanceUpstreamTransportError)
		return
	}
	defer resp.Body.Close()

	// A 101 Switching Protocols response on a transcoded JSON/SSE create
	// route is an upstream protocol error; it cannot be schema-transcoded.
	// It is rejected before the generic non-2xx handling because 101 cannot
	// carry an error body.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		h.writeDialectHTTPError(r, w, CanonicalAPIError{
			Status:  http.StatusBadGateway,
			Type:    "api_error",
			Code:    "upstream_protocol_error",
			Message: "upstream switched protocols on a transcoded route",
		}, ProvenanceUpstreamBodyError)
		return
	}

	// A non-2xx upstream response is parsed and rendered before any
	// downstream status is committed (operational rule).
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		apiErr, readErr := ReadCanonicalUpstreamError(
			resp,
			h.cfg.Mapping.UpstreamProtocol,
			h.cfg.BodyLimits.ErrorResponseBytes,
		)
		if readErr != nil {
			h.writeUpstreamHTTPError(r, w, resp, CanonicalAPIError{
				Status:  resp.StatusCode,
				Type:    "api_error",
				Code:    codeForStatus(resp.StatusCode),
				Message: "read upstream error body: " + readErr.Error(),
			})
			return
		}
		h.writeUpstreamHTTPError(r, w, resp, apiErr)
		return
	}

	// The response media type must agree with the client's stream intent
	// (merge gate 17): a streaming request cannot be answered with JSON and
	// a non-streaming request cannot be answered with an SSE stream.
	switch {
	case isEventStream(resp):
		if !context.StreamIntent {
			h.writeLocalError(r, w, ClientProtocol(h.cfg.Mapping.ClientProtocol),
				http.StatusBadGateway,
				"upstream returned a stream for a non-streaming request",
				ProvenanceLocalStreamValidationError)
			return
		}
		h.streamResponse(w, r, resp, context)
	case isJSON(resp):
		if context.StreamIntent {
			h.writeLocalError(r, w, ClientProtocol(h.cfg.Mapping.ClientProtocol),
				http.StatusBadGateway,
				"upstream returned a non-streaming response for a streaming request",
				ProvenanceLocalStreamValidationError)
			return
		}
		h.jsonResponse(w, r, resp, context)
	default:
		// A 2xx non-JSON non-SSE response is a stream-intent mismatch.
		h.writeLocalError(r, w, ClientProtocol(h.cfg.Mapping.ClientProtocol),
			http.StatusBadGateway,
			"upstream returned an unrecognized response content type",
			ProvenanceLocalStreamValidationError)
	}
}

// readRequestBody reads and bounds the client request body.
var errRequestBodyTooLarge = errors.New("request body too large")

func (h *TranscodeHandler) readRequestBody(r *http.Request) ([]byte, error) {
	limit := h.cfg.BodyLimits.AcceptedRequestBytes
	if limit <= 0 {
		limit = 32 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errRequestBodyTooLarge
	}
	return body, nil
}

// convertRequest decodes the source request into the canonical IR and renders
// the target request body, building the exchange context.
func (h *TranscodeHandler) convertRequest(
	r *http.Request,
	body []byte,
) ([]byte, *ExchangeContext, error) {
	policy := h.cfg.LossPolicy
	mapping := h.cfg.Mapping

	context := &ExchangeContext{
		IDs:        NewExchangeIDs(),
		LossPolicy: policy,
		// Stream intent is expressed either by the client Accept header (the
		// Messages dialect signals streaming only this way) or by the
		// request body's stream flag (Responses). Both are merged after
		// decode below; the response media type must agree (merge gate 17).
		StreamIntent: strings.EqualFold(
			strings.TrimSpace(r.Header.Get("Accept")),
			"text/event-stream",
		),
	}

	// Resolve the client model through the mapping once the decoded request
	// reveals it. The client-facing alias is returned in the response; the
	// upstream model is used on the outbound request.
	resolveModel := func(clientModel string) error {
		mappingModel, err := h.cfg.ModelMap.Resolve(clientModel)
		if err != nil {
			return err
		}
		context.RequestedClientModel = mappingModel.ClientResponseModel
		context.UpstreamModel = mappingModel.UpstreamModel
		return nil
	}

	switch mapping.ClientProtocol {
	case ClientResponses:
		result, echo, err := DecodeResponsesRequest(body, policy)
		if err != nil {
			return nil, nil, err
		}
		context.StreamIntent = context.StreamIntent || result.Request.Stream
		if err := resolveModel(result.Request.ClientModel); err != nil {
			return nil, nil, err
		}
		context.OriginalResponsesRequest = echo
		result.Request.ClientModel = context.UpstreamModel

		var rendered []byte
		var report ConversionReport
		switch mapping.UpstreamProtocol {
		case UpstreamChatCompletions:
			rendered, report, err = RenderChatRequest(
				result.Request,
				context,
				h.cfg.ChatCapabilities,
			)
		case UpstreamResponses:
			rendered, report, err = RenderResponsesRequest(result.Request, context)
		default:
			return nil, nil, fmt.Errorf(
				"unsupported upstream protocol %q for responses client",
				mapping.UpstreamProtocol,
			)
		}
		if err != nil {
			return nil, nil, err
		}
		if err := h.checkDecodedRequestSize(rendered); err != nil {
			return nil, nil, err
		}
		logConversionReport(report, r)
		return rendered, context, nil

	case ClientMessages:
		result, err := DecodeMessagesRequest(body, policy)
		if err != nil {
			return nil, nil, err
		}
		context.StreamIntent = context.StreamIntent || result.Request.Stream
		if err := resolveModel(result.Request.ClientModel); err != nil {
			return nil, nil, err
		}
		context.OriginalMessagesRequest = &MessagesRequestContext{
			MaxTokens: derefInt(result.Request.MaxOutputTokens),
			Metadata:  result.Request.Metadata,
		}
		result.Request.ClientModel = context.UpstreamModel

		var rendered []byte
		var report ConversionReport
		switch mapping.UpstreamProtocol {
		case UpstreamResponses:
			rendered, report, err = RenderResponsesRequest(result.Request, context)
		case UpstreamChatCompletions:
			rendered, report, err = RenderChatRequest(
				result.Request,
				context,
				h.cfg.ChatCapabilities,
			)
		default:
			return nil, nil, fmt.Errorf(
				"unsupported upstream protocol %q for messages client",
				mapping.UpstreamProtocol,
			)
		}
		if err != nil {
			return nil, nil, err
		}
		if err := h.checkDecodedRequestSize(rendered); err != nil {
			return nil, nil, err
		}
		logConversionReport(report, r)
		return rendered, context, nil

	default:
		return nil, nil, fmt.Errorf("unknown client protocol %q", mapping.ClientProtocol)
	}
}

// buildUpstreamRequest builds the outbound request: hop-by-hop sanitization,
// source auth stripping, target auth application, recomputed Content-Length,
// and the client context carried so downstream cancellation aborts upstream.
func (h *TranscodeHandler) buildUpstreamRequest(
	r *http.Request,
	body []byte,
) (*http.Request, error) {
	targetURL, err := BuildMappedURL(
		h.cfg.Upstream,
		h.cfg.Mapping.UpstreamPath,
		r.URL.Query(),
		h.cfg.AllowedClientQuery,
	)
	if err != nil {
		return nil, err
	}

	headers := r.Header.Clone()
	// Sound behavior: remove the client's Accept-Encoding so the upstream
	// does not compress the response, which would break JSON/SSE conversion.
	headers.Del("Accept-Encoding")
	// Sound behavior: remove forwarded headers like the passthrough path.
	headers.Del("X-Forwarded-For")
	headers.Del("X-Forwarded-Host")
	headers.Del("X-Forwarded-Proto")

	RemoveHopByHopHeaders(headers)
	headers.Set("Content-Type", "application/json")

	outReq, err := BuildConvertedRequest(r.Context(), r.Method, targetURL.String(), body, headers)
	if err != nil {
		return nil, err
	}

	// Sound behavior: preserve the request context on the upstream request.
	outReq = outReq.WithContext(r.Context())

	// Extract the inbound credential, then strip and re-apply per policy.
	inbound, err := ExtractInboundCredential(headers)
	if err != nil {
		return nil, err
	}
	if err := ApplyTargetAuthentication(
		r.Context(),
		outReq,
		h.cfg.Mapping.UpstreamProtocol,
		h.cfg.AuthPolicy,
		inbound,
	); err != nil {
		return nil, err
	}

	return outReq, nil
}

// jsonResponse converts a non-streaming upstream response back to the client
// schema and writes it downstream.
func (h *TranscodeHandler) jsonResponse(
	w http.ResponseWriter,
	r *http.Request,
	resp *http.Response,
	context *ExchangeContext,
) {
	limit := h.cfg.BodyLimits.SuccessfulResponseBytes
	if limit <= 0 {
		limit = 32 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if r.Context().Err() != nil {
			h.recordOutcome(r, Outcome{Provenance: ProvenanceClientAbort})
			// A cancelled client context means the exchange is already over.
			// Return normally: the proxy classifies the abort from the
			// recorded outcome, and the net/http server tolerates a handler
			// that returns without writing to a dead connection.
			return
		}
		h.writeDialectHTTPError(r, w, CanonicalAPIError{
			Status:  http.StatusBadGateway,
			Type:    "api_error",
			Code:    "upstream_body_error",
			Message: "read upstream response: " + err.Error(),
		}, ProvenanceUpstreamBodyError)
		return
	}
	if int64(len(body)) > limit {
		h.writeDialectHTTPError(r, w, CanonicalAPIError{
			Status:  http.StatusBadGateway,
			Type:    "api_error",
			Code:    "upstream_response_too_large",
			Message: "upstream response body exceeds the configured limit",
		}, ProvenanceUpstreamBodyError)
		return
	}

	converted, provenance, err := h.convertResponse(r, resp, body, context)
	if err != nil {
		// A local response conversion failure before headers is a
		// client-dialect 502 and is reported via the outcome hook so the
		// proxy never classifies it as an upstream failure.
		h.writeDialectHTTPError(r, w, CanonicalAPIError{
			Status:  http.StatusBadGateway,
			Type:    "api_error",
			Code:    "response_conversion_error",
			Message: "convert response: " + err.Error(),
		}, provenance)
		return
	}

	// Sound behavior: strip hop-by-hop headers from the upstream response
	// BEFORE copying (copyResponseHeaders skips the Connection header, so
	// Connection-nominated tokens can only be resolved from the source),
	// then recompute Content-Length after conversion.
	RemoveHopByHopHeaders(resp.Header)
	h.copyResponseHeaders(w.Header(), resp.Header)
	RemoveTransformedRepresentationHeaders(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(converted)))
	w.WriteHeader(resp.StatusCode)
	// A failed write after a client abort must not be recorded as a
	// successful completion (the streaming path classifies the abort the
	// same way); a write failure without a client cancel is a downstream
	// error.
	_, writeErr := w.Write(converted)
	switch {
	case writeErr != nil && r.Context().Err() != nil:
		h.recordOutcome(r, Outcome{Provenance: ProvenanceClientAbort})
	case writeErr != nil:
		h.recordOutcome(r, Outcome{Provenance: ProvenanceDownstreamWriteError})
	default:
		h.recordOutcome(r, Outcome{
			Status:     resp.StatusCode,
			Provenance: ProvenanceUpstreamHTTP,
		})
	}
}

// checkDecodedRequestSize rejects decoded requests that amplify beyond the
// decoded-request body limit (merge gate 19: the decoded limit is separate
// from the accepted raw-body limit).
func (h *TranscodeHandler) checkDecodedRequestSize(rendered []byte) error {
	limit := h.cfg.BodyLimits.DecodedRequestBytes
	if limit <= 0 {
		return nil
	}
	if int64(len(rendered)) > limit {
		return errRequestBodyTooLarge
	}
	return nil
}

// convertResponse decodes the upstream JSON response into the canonical IR
// and renders the client response envelope. The output dialect is determined
// by the CLIENT protocol, never by the upstream protocol.
func (h *TranscodeHandler) convertResponse(
	r *http.Request,
	resp *http.Response,
	body []byte,
	context *ExchangeContext,
) ([]byte, ExchangeProvenance, error) {
	switch h.cfg.Mapping.ClientProtocol {
	case ClientResponses:
		switch h.cfg.Mapping.UpstreamProtocol {
		case UpstreamChatCompletions:
			response, err := DecodeChatResponse(body, h.cfg.ChatCapabilities)
			if err != nil {
				return nil, ProvenanceLocalResponseConversionError, err
			}
			converted, err := RenderResponsesResponse(response, context)
			if err != nil {
				return nil, ProvenanceLocalResponseConversionError, err
			}
			return converted, ProvenanceLocalResponseConversionError, nil
		default:
			return nil, ProvenanceLocalResponseConversionError, fmt.Errorf(
				"unsupported upstream protocol %q for responses client",
				h.cfg.Mapping.UpstreamProtocol,
			)
		}

	case ClientMessages:
		switch h.cfg.Mapping.UpstreamProtocol {
		case UpstreamChatCompletions:
			response, err := DecodeChatResponse(body, h.cfg.ChatCapabilities)
			if err != nil {
				return nil, ProvenanceLocalResponseConversionError, err
			}
			converted, err := RenderMessagesResponse(response, context)
			if err != nil {
				return nil, ProvenanceLocalResponseConversionError, err
			}
			return converted, ProvenanceLocalResponseConversionError, nil

		case UpstreamResponses:
			response, err := DecodeResponsesResponse(body)
			if err != nil {
				return nil, ProvenanceLocalResponseConversionError, err
			}
			converted, err := RenderMessagesResponse(response, context)
			if err != nil {
				return nil, ProvenanceLocalResponseConversionError, err
			}
			return converted, ProvenanceLocalResponseConversionError, nil

		default:
			return nil, ProvenanceLocalResponseConversionError, fmt.Errorf(
				"unsupported upstream protocol %q for messages client",
				h.cfg.Mapping.UpstreamProtocol,
			)
		}

	default:
		return nil, ProvenanceLocalResponseConversionError, fmt.Errorf(
			"unsupported client protocol %q",
			h.cfg.Mapping.ClientProtocol,
		)
	}
}

// streamResponse translates an upstream SSE stream into client SSE events,
// delegating copy and cancellation to stream.Proxy and sealing downstream
// writes before returning.
func (h *TranscodeHandler) streamResponse(
	w http.ResponseWriter,
	r *http.Request,
	resp *http.Response,
	context *ExchangeContext,
) {
	converter, err := h.newFrameConverter(context)
	if err != nil {
		// writeLocalError -> writeDialectHTTPError records the outcome
		// exactly once.
		h.writeLocalError(r, w, ClientProtocol(h.cfg.Mapping.ClientProtocol),
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

	reader := newConvertingReader(NewSSEReaderWithLimits(
		resp.Body,
		h.cfg.BodyLimits.SSELineBytes,
		h.cfg.BodyLimits.SSEFrameBytes,
	), converter)
	observation := runTranslatedStream(r.Context(), w, resp.Body, reader)

	// Classify the outcome from explicit provenance.
	var outcome Outcome
	switch classifyStreamObservation(observation) {
	case streamOutcomeSuccess:
		outcome = Outcome{
			Status:     resp.StatusCode,
			Provenance: ProvenanceUpstreamHTTP,
		}
	case streamOutcomeClientAbort:
		outcome = Outcome{
			Provenance: ProvenanceClientAbort,
		}
	case streamOutcomeUpstreamFailure:
		outcome = Outcome{
			Status:          resp.StatusCode,
			Provenance:      ProvenanceUpstreamBodyError,
			UpstreamFailure: true,
		}
	case streamOutcomeLocalConversionFailure:
		outcome = Outcome{
			Status:     http.StatusBadGateway,
			Provenance: ProvenanceLocalResponseConversionError,
		}
	case streamOutcomeDownstreamFailure:
		outcome = Outcome{
			Provenance: ProvenanceDownstreamWriteError,
		}
	default:
		outcome = Outcome{
			Status:     resp.StatusCode,
			Provenance: ProvenanceUpstreamHTTP,
		}
	}
	h.recordOutcome(r, outcome)
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
			h.cfg.LossPolicy,
			responseID,
			model,
			createdAt,
			context.OriginalResponsesRequest,
		)
		return newChatToResponsesConverter(state), nil

	case client == ClientMessages && upstream == UpstreamResponses:
		state := newAnthropicResponsesStreamState(
			context,
			context.IDs.New("msg_"),
			model,
			createdAt,
		)
		return newResponsesToAnthropicConverter(state), nil

	case client == ClientMessages && upstream == UpstreamChatCompletions:
		chat := newChatResponsesStreamState(
			context,
			h.cfg.LossPolicy,
			responseID,
			model,
			createdAt,
			nil, // Responses envelope is internal to the composition
		)
		anthropic := newAnthropicResponsesStreamState(
			context,
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

// writeLocalError writes a plain-text local error. Dialect-correct rendering
// is used for API errors; local infrastructure errors are plain text because
// they are not provider API errors.
func (h *TranscodeHandler) writeLocalError(
	r *http.Request,
	w http.ResponseWriter,
	client ClientProtocol,
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

// writeUpstreamHTTPError renders a non-2xx upstream response in the client
// dialect. The failure classification uses the response-aware breaker
// semantics (IsFailureStatusWithHeaders) so 429, 5xx, and 403 with
// rate-limit signals (Retry-After or x-ratelimit-* headers) are upstream
// failures — matching the native passthrough path exactly.
func (h *TranscodeHandler) writeUpstreamHTTPError(
	r *http.Request,
	w http.ResponseWriter,
	resp *http.Response,
	apiErr CanonicalAPIError,
) {
	now := time.Now()
	upstreamFailure := circuitbreaker.IsFailureStatusWithHeaders(
		apiErr.Status,
		resp.Header,
		now,
		now,
	)
	client := ClientProtocol(h.cfg.Mapping.ClientProtocol)
	if err := WriteDialectHTTPError(w, client, apiErr); err != nil {
		log.Printf("transcode: write dialect error: %v", err)
	}
	h.recordOutcome(r, Outcome{
		Status:          apiErr.Status,
		Provenance:      ProvenanceUpstreamHTTP,
		UpstreamFailure: upstreamFailure,
	})
}

// writeDialectHTTPError renders a canonical error in the client dialect and
// records the outcome. UpstreamFailure is derived from explicit provenance:
// only definitive upstream outcomes (transport errors and upstream HTTP >=
// 500) are upstream failures; local conversion errors are not.
func (h *TranscodeHandler) writeDialectHTTPError(
	r *http.Request,
	w http.ResponseWriter,
	apiErr CanonicalAPIError,
	provenance ExchangeProvenance,
) {
	client := ClientProtocol(h.cfg.Mapping.ClientProtocol)
	if err := WriteDialectHTTPError(w, client, apiErr); err != nil {
		log.Printf("transcode: write dialect error: %v", err)
	}
	upstreamFailure := false
	switch provenance {
	case ProvenanceUpstreamTransportError:
		upstreamFailure = true
	case ProvenanceUpstreamHTTP, ProvenanceUpstreamBodyError:
		// Match the proxy's breaker classification (IsFailureStatus /
		// IsFailureStatusWithHeaders): 429 and 5xx are failures, and 403 is
		// a failure only when it carries a rate-limit signal (Retry-After).
		upstreamFailure = apiErr.Status == http.StatusTooManyRequests ||
			(apiErr.Status >= 500 && apiErr.Status < 600) ||
			(apiErr.Status == http.StatusForbidden && apiErr.RetryAfter != "")
	}
	h.recordOutcome(r, Outcome{
		Status:          apiErr.Status,
		Provenance:      provenance,
		UpstreamFailure: upstreamFailure,
	})
}

// recordOutcome forwards the outcome to the proxy breaker hook and, when an
// outcome sink is present on the request context, to the proxy's per-request
// provenance reader.
func (h *TranscodeHandler) recordOutcome(r *http.Request, outcome Outcome) {
	if r != nil {
		if sink, _ := r.Context().Value(outcomeContextKey{}).(chan Outcome); sink != nil {
			select {
			case sink <- outcome:
			default:
			}
		}
	}
	if h.outcomeFn != nil {
		h.outcomeFn(outcome)
	}
}

// copyResponseHeaders copies upstream entity headers to the downstream
// writer, skipping hop-by-hop and content-identity headers handled elsewhere.
func (h *TranscodeHandler) copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		if isHopByHopName(name) {
			continue
		}
		if strings.EqualFold(name, "Connection") {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

// isHopByHopName reports whether the header is a fixed hop-by-hop name.
func isHopByHopName(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

// isEventStream reports whether the response is an SSE stream.
func isEventStream(resp *http.Response) bool {
	contentType := resp.Header.Get("Content-Type")
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// isJSON reports whether the response is JSON.
func isJSON(resp *http.Response) bool {
	contentType := resp.Header.Get("Content-Type")
	return strings.Contains(strings.ToLower(contentType), "json")
}

// isUpgradeRequest reports whether the request carries an Upgrade token.
func isUpgradeRequest(r *http.Request) bool {
	if len(r.Header.Values("Upgrade")) > 0 {
		return true
	}
	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// nowUnix returns the current unix time in seconds.
func nowUnix() int64 {
	return time.Now().Unix()
}

// derefInt returns the value of v, or 0 when v is nil.
func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// logConversionReport logs approved losses for observability.
func logConversionReport(report ConversionReport, r *http.Request) {
	for _, loss := range report.Losses {
		log.Printf(
			"transcode: %s %s: approved loss %s at %s: %s",
			r.Method,
			r.URL.Path,
			loss.Feature,
			loss.Path,
			loss.Detail,
		)
	}
}
