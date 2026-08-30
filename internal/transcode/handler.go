package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
)

// HandlerConfig configures one transcoded route.
// HandlerConfig is the transcode handler configuration. The route-specific
// values (auth, model map, loss policy, capabilities, allowed client query)
// live in Mapping — HandlerConfig does not duplicate them (review-z commit
// 4). The constructor validates, defaults, and deep-copies the mapping once
// into a private immutable configuration; runtime always reads the
// normalized copy.
type HandlerConfig struct {
	// Mapping is the validated route mapping.
	Mapping Mapping

	// Upstream is the upstream base URL.
	Upstream *url.URL

	// BodyLimits are the independent request/response body limits.
	BodyLimits BodyLimits
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

// Outcome is the recorded result of one transcoded exchange. Upstream
// provenance and downstream completion are separate facts: the upstream may
// have returned a definitive failure while the client disconnected before
// the translated error was fully written, and both must be recorded.
// OutcomeFunc receives the exchange outcome. The proxy wires this to breaker
// accounting. It must be safe to call at most once per exchange, after the
// handler has stopped mutating the recorder.
type OutcomeFunc func(Outcome)

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

// NewTranscodeHandler returns a handler for the given configuration. The
// configuration is validated at construction, never on the first request: a
// nil round trip would panic at request time, negative body limits would be
// silently defaulted, and an invalid mapping or upstream would fail
// mid-exchange (review-08 additional 9).
func NewTranscodeHandler(
	cfg HandlerConfig,
	roundTrip RoundTrip,
	outcomeFn OutcomeFunc,
) *TranscodeHandler {
	if roundTrip == nil {
		panic("transcode: nil round trip")
	}
	if err := cfg.Mapping.Validate(); err != nil {
		panic(fmt.Sprintf("transcode: invalid handler configuration: %v", err))
	}
	if err := cfg.BodyLimits.Validate(); err != nil {
		panic(fmt.Sprintf("transcode: invalid body limits: %v", err))
	}
	if cfg.Upstream == nil ||
		(cfg.Upstream.Scheme != "http" && cfg.Upstream.Scheme != "https") ||
		cfg.Upstream.Hostname() == "" {
		panic("transcode: invalid upstream URL: must be http or https with a hostname")
	}
	// The body limits contract (limits.go): zero values select the package
	// defaults, computed ONCE here so zero never reaches handler logic — in
	// particular, a zero DecodedRequestBytes is never treated as unlimited
	// (review-k finding 8). The handler-side per-use fallbacks remain as
	// defense-in-depth for any future construction path that bypasses this
	// normalization.
	cfg.BodyLimits = cfg.BodyLimits.WithDefaults()
	// The configuration is frozen at construction: the mapping and upstream
	// URL are deep-copied once into the private configuration, so
	// programmatic callers can never mutate live configuration (review-z
	// commit 4).
	cfg.Mapping = cloneMapping(cfg.Mapping)
	upstream := *cfg.Upstream
	cfg.Upstream = &upstream
	return &TranscodeHandler{
		cfg:       cfg,
		roundTrip: roundTrip,
		outcomeFn: outcomeFn,
	}
}

// cloneMapping deep-copies every mutable value of a Mapping.
func cloneMapping(m Mapping) Mapping {
	m.LossPolicy = LossPolicy{Allowed: cloneLossSet(m.LossPolicy.Allowed)}
	m.ModelMap = cloneModelMapValues(m.ModelMap)
	m.AllowedClientQuery = cloneQuerySet(m.AllowedClientQuery)
	return m
}

// cloneLossSet deep-copies a loss-policy allow set.
func cloneLossSet(allowed map[Feature]struct{}) map[Feature]struct{} {
	if allowed == nil {
		return nil
	}
	out := make(map[Feature]struct{}, len(allowed))
	for feature := range allowed {
		out[feature] = struct{}{}
	}
	return out
}

// cloneModelMapValues deep-copies the model-map entries.
func cloneModelMapValues(m ModelMap) ModelMap {
	m.Exact = cloneModelMappings(m.Exact)
	return m
}

// cloneModelMappings deep-copies a model-mapping map.
func cloneModelMappings(m map[string]ModelMapping) map[string]ModelMapping {
	if m == nil {
		return nil
	}
	out := make(map[string]ModelMapping, len(m))
	maps.Copy(out, m)
	return out
}

// cloneQuerySet deep-copies the allowed-client-query set.
func cloneQuerySet(s map[string]struct{}) map[string]struct{} {
	if s == nil {
		return nil
	}
	out := make(map[string]struct{}, len(s))
	for key := range s {
		out[key] = struct{}{}
	}
	return out
}

// ClientPath returns the client-facing route path of the handler.
func (h *TranscodeHandler) ClientPath() string {
	return h.cfg.Mapping.ClientRoute.Path
}

// ServeHTTP implements http.Handler. Every exchange records EXACTLY ONE
// outcome: the defer below records an internal local-failure outcome when no
// path recorded one, so the proxy's synchronous sink read can never observe a
// missing outcome (review-z commit 4).
func (h *TranscodeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if sink := OutcomeSinkFromContext(r.Context()); sink != nil {
			if _, recorded := sink.Load(); !recorded {
				sink.Record(LocalFailureOutcome())
				if h.outcomeFn != nil {
					h.outcomeFn(LocalFailureOutcome())
				}
			}
		}
	}()
	// Reject Upgrade requests on transcoded routes: a 101 Switching
	// Protocols response cannot be meaningfully schema-transcoded.
	if isUpgradeRequest(r) {
		h.writeLocalError(r, w,
			http.StatusBadRequest, "upgrade requests are not supported on transcoded routes",
			ProvenanceLocalRequestConversionError)
		return
	}

	// Reject non-identity request Content-Encoding with a dialect-correct
	// 415. JSON-decoding compressed bytes as plain input is not adequate;
	// the identity encoding is the no-op and is accepted.
	if enc := r.Header.Get("Content-Encoding"); enc != "" &&
		!strings.EqualFold(strings.TrimSpace(enc), "identity") {
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
		if r.Context().Err() != nil && isContextCancellationError(err) {
			h.recordOutcome(r, Outcome{Provenance: ProvenanceClientAbort, ClientAborted: true})
			// A cancelled client context means the exchange is already over.
			// Return normally: the proxy classifies the abort from the
			// recorded outcome, and the net/http server tolerates a handler
			// that returns without writing to a dead connection.
			return
		}
		if errors.Is(err, errRequestBodyTooLarge) {
			h.writeLocalError(r, w,
				http.StatusRequestEntityTooLarge, "request body too large",
				ProvenanceLocalRequestConversionError)
			return
		}
		h.writeLocalError(r, w,
			http.StatusBadRequest, "read request body: "+err.Error(),
			ProvenanceLocalRequestConversionError)
		return
	}

	// Decode the source request into the canonical IR and render the target
	// request, resolving the model through the map.
	upstreamBody, context, err := h.convertRequest(r, body)
	if err != nil {
		if r.Context().Err() != nil && isContextCancellationError(err) {
			h.recordOutcome(r, Outcome{Provenance: ProvenanceClientAbort, ClientAborted: true})
			// A cancelled client context means the exchange is already over.
			// Return normally: the proxy classifies the abort from the
			// recorded outcome, and the net/http server tolerates a handler
			// that returns without writing to a dead connection.
			return
		}
		// A decoded/rendered request that amplifies beyond the decoded-request
		// body limit is a 413, not a generic conversion 400.
		status := http.StatusBadRequest
		if errors.Is(err, errDecodedRequestTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		h.writeLocalError(r, w,
			status, "convert request: "+err.Error(),
			ProvenanceLocalRequestConversionError)
		return
	}

	outReq, err := h.buildUpstreamRequest(r, upstreamBody, context.StreamIntent)
	if err != nil {
		// An unallowed client query parameter or an invalid inbound
		// credential is a client fault (400); other request-construction
		// failures are internal (500). Internal construction errors (secret
		// resolution, signing) never leak details such as file paths into
		// the client message (review-j finding 14); the detail is logged.
		status := http.StatusInternalServerError
		message := "build upstream request: " + err.Error()
		if errors.Is(err, errClientQueryParameter) ||
			errors.Is(err, errAuthInboundCredential) {
			status = http.StatusBadRequest
		} else {
			log.Printf(
				"transcode: %s %s: build upstream request: %v",
				r.Method,
				r.URL.Path,
				err,
			)
			message = "build upstream request: internal error"
		}
		h.writeLocalError(r, w,
			status, message,
			ProvenanceLocalRequestConversionError)
		return
	}

	resp, err := h.roundTrip(outReq)
	// The response headers arrived: anchor Retry-After and the 403
	// rate-signal classification here so body-read time is excluded from the
	// recorded hold (review-08 blocker 9; the same pattern the retry
	// transport uses).
	receivedAt := time.Now()
	if err != nil {
		// A RoundTripper may return a non-nil response alongside an error;
		// release its body so the connection is not leaked (sound behavior).
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		// A client abort suppresses the failure only when the transport
		// error is itself cancellation-derived: an unrelated connection
		// reset, DNS, or TLS failure racing with the cancellation must win
		// as the upstream transport failure it is (review-08 blocker 8).
		if r.Context().Err() != nil && isContextCancellationError(err) {
			h.recordOutcome(r, Outcome{Provenance: ProvenanceClientAbort, ClientAborted: true})
			// A cancelled client context means the exchange is already over.
			// Return normally: the proxy classifies the abort from the
			// recorded outcome, and the net/http server tolerates a handler
			// that returns without writing to a dead connection.
			return
		}
		// A signer error is a local construction/auth failure (neutral),
		// never an upstream transport failure (review-z commit 4).
		if signingErr, ok := errors.AsType[*SigningError](err); ok {
			// The signer failure is logged with detail; the client message
			// is sanitized (review-j finding 14: local construction errors
			// never leak details). The code is request-side: the failure
			// happened building the upstream request, before any response
			// existed (review-z commit 4).
			h.logRequestError(r, signingErr)
			h.writeDialectHTTPError(r, w, CanonicalAPIError{
				Status:  http.StatusBadGateway,
				Type:    "api_error",
				Code:    "request_conversion_error",
				Message: "build upstream request: internal error",
			}, ProvenanceLocalRequestConversionError)
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
			h.writeUpstreamBodyError(r, w, resp, receivedAt, CanonicalAPIError{
				Status:  resp.StatusCode,
				Type:    "api_error",
				Code:    codeForStatus(resp.StatusCode),
				Message: "read upstream error body: " + readErr.Error(),
			})
			return
		}
		h.writeUpstreamHTTPError(r, w, resp, receivedAt, apiErr)
		return
	}

	// The response media type must agree with the client's stream intent
	// (merge gate 17): a streaming request cannot be answered with JSON and
	// a non-streaming request cannot be answered with an SSE stream.
	switch {
	case isEventStream(resp):
		if !context.StreamIntent {
			// The upstream returned the wrong representation for the
			// negotiated mode: corrupt upstream wire, an upstream failure
			// that fails a half-open probe and applies the failure hold
			// (review-08 blocker 8).
			h.writeLocalError(r, w,
				http.StatusBadGateway,
				"upstream returned a stream for a non-streaming request",
				ProvenanceUpstreamBodyError)
			return
		}
		h.streamResponse(w, r, resp, context, receivedAt)
	case isJSON(resp):
		if context.StreamIntent {
			h.writeLocalError(r, w,
				http.StatusBadGateway,
				"upstream returned a non-streaming response for a streaming request",
				ProvenanceUpstreamBodyError)
			return
		}
		h.jsonResponse(w, r, resp, context)
	default:
		// A 2xx non-JSON non-SSE response is a stream-intent mismatch.
		h.writeLocalError(r, w,
			http.StatusBadGateway,
			"upstream returned an unrecognized response content type",
			ProvenanceUpstreamBodyError)
	}
}

// readRequestBody reads and bounds the client request body.
var errRequestBodyTooLarge = errors.New("request body too large")

// errDecodedRequestTooLarge marks a decoded/rendered request that amplified
// beyond the decoded-request body limit. It renders as 413 RequestEntityTooLarge
// in the client dialect, not the generic conversion 400 (review-j finding 15).
var errDecodedRequestTooLarge = errors.New("decoded request exceeds the decoded-request body limit")

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
	policy := h.cfg.Mapping.LossPolicy
	mapping := h.cfg.Mapping

	context := &ExchangeContext{
		IDs:          NewExchangeIDs(),
		LossPolicy:   policy,
		Capabilities: mapping.ChatCapabilities,
		// Stream intent precedence (v1 blocker 1): the request body's
		// stream field, when explicitly present, is authoritative; only when
		// absent does the client Accept header select the representation
		// (parsed below with full media-range and q-value semantics — q=0
		// excludes, malformed ranges are ignored). The response media type
		// must agree with the resolved intent (merge gate 17).
		StreamIntent: acceptIsEventStream(r.Header.Get("Accept")),
	}

	// Resolve the client model through the mapping once the decoded request
	// reveals it. The client-facing alias is returned in the response; the
	// upstream model is used on the outbound request.
	resolveModel := func(clientModel string) error {
		mappingModel, err := h.cfg.Mapping.ModelMap.Resolve(clientModel)
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
		// The request body's stream field, when explicitly present, is
		// authoritative over the Accept header (review-08 blocker 1): an
		// explicit stream:false is never merged into streaming.
		if result.StreamSet {
			context.StreamIntent = result.Request.Stream
		}
		// Write the resolved intent back into the canonical request so the
		// upstream renderer emits the stream value matching the negotiated
		// mode (review-j finding 6): an Accept-only stream request must not
		// ask the upstream for JSON while the handler expects SSE.
		result.Request.Stream = context.StreamIntent
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
				h.cfg.Mapping.ChatCapabilities,
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
		report.Losses = append(report.Losses, result.Report.Losses...)
		logConversionReport(report, r)
		return rendered, context, nil

	case ClientMessages:
		result, err := DecodeMessagesRequest(body, policy)
		if err != nil {
			return nil, nil, err
		}
		// The request body's stream field, when explicitly present, is
		// authoritative over the Accept header (review-08 blocker 1): an
		// explicit stream:false is never merged into streaming.
		if result.StreamSet {
			context.StreamIntent = result.Request.Stream
		}
		// Write the resolved intent back into the canonical request so the
		// upstream renderer emits the stream value matching the negotiated
		// mode (review-j finding 6): an Accept-only stream request must not
		// ask the upstream for JSON while the handler expects SSE.
		result.Request.Stream = context.StreamIntent
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
				h.cfg.Mapping.ChatCapabilities,
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
		report.Losses = append(report.Losses, result.Report.Losses...)
		logConversionReport(report, r)
		return rendered, context, nil

	default:
		return nil, nil, fmt.Errorf("unknown client protocol %q", mapping.ClientProtocol)
	}
}

// buildUpstreamRequest builds the outbound request: hop-by-hop sanitization,
// source auth stripping, target auth application, recomputed Content-Length,
// the outbound Accept rewritten to the negotiated stream mode, and the client
// context carried so downstream cancellation aborts upstream.
func (h *TranscodeHandler) buildUpstreamRequest(
	r *http.Request,
	body []byte,
	streamIntent bool,
) (*http.Request, error) {
	targetURL, err := BuildMappedURL(
		h.cfg.Upstream,
		h.cfg.Mapping.UpstreamPath,
		r.URL.Query(),
		h.cfg.Mapping.AllowedClientQuery,
	)
	if err != nil {
		return nil, err
	}

	// The outbound request uses an explicit allowlist: NOTHING the client
	// sent is forwarded unless it is on the list (review-08 blocker 10).
	// Cookies, forwarding headers, range/conditional controls, idempotency
	// keys, source-provider controls, and unknown credential or extension
	// headers never reach the target provider; hop-by-hop and
	// transformed-representation metadata are structurally absent.
	headers := http.Header{}
	// The outbound Accept is rewritten to the representation actually
	// requested upstream: text/event-stream when the exchange streams,
	// otherwise application/json. The client's Accept describes its own
	// preferences, not the mode of the converted request, and must never
	// reach the target provider unnormalized (review-08 blockers 1 and 10).
	if streamIntent {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	headers.Set("Content-Type", "application/json")

	outReq, err := BuildConvertedRequest(r.Context(), r.Method, targetURL.String(), body, headers)
	if err != nil {
		return nil, err
	}

	// Sound behavior: preserve the request context on the upstream request.
	outReq = outReq.WithContext(r.Context())

	// Extract the inbound credential from the CLIENT's original headers when
	// the policy requires inbound credentials, then strip and re-apply per policy.
	var inbound Credential
	if h.cfg.Mapping.Auth.Inbound {
		var err error
		inbound, err = ExtractInboundCredential(r.Header)
		if err != nil {
			return nil, err
		}
	}
	if err := ApplyTargetAuthentication(
		r.Context(),
		outReq,
		h.cfg.Mapping.UpstreamProtocol,
		h.cfg.Mapping.Auth,
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
		if r.Context().Err() != nil && isContextCancellationError(err) {
			h.recordOutcome(r, Outcome{Provenance: ProvenanceClientAbort, ClientAborted: true})
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

	converted, provenance, err := h.convertResponse(r, body, context)
	if err != nil {
		apiErr := CanonicalAPIError{
			Status:  http.StatusBadGateway,
			Type:    "api_error",
			Code:    "response_conversion_error",
			Message: "convert response: " + err.Error(),
		}
		// A typed upstream semantic failure (a 2xx envelope whose payload
		// reports failure) classifies with the UPSTREAM status and
		// UpstreamFailure=true, matching the streamed classification
		// (review-j finding 11) — never a local conversion failure.
		if _, ok := errors.AsType[*UpstreamSemanticFailureError](err); ok {
			h.writeUpstreamSemanticFailure(r, w, apiErr, resp.StatusCode)
			return
		}
		// The conversion failure is logged server-side (autopsy 04 rec 3:
		// a parse-failure cascade must be visible to the operator) in
		// addition to the bounded client message below. Upstream-authored
		// semantic-failure text is NOT logged: it is already echoed to the
		// client and must not enter the server log unbounded.
		h.logRequestError(r, err)
		h.writeDialectHTTPError(r, w, apiErr, provenance)
		return
	}

	// Sound behavior: strip hop-by-hop headers from the upstream response
	// BEFORE copying (copyResponseHeaders skips the Connection header, so
	// Connection-nominated tokens can only be resolved from the source),
	// then recompute Content-Length after conversion.
	RemoveHopByHopHeaders(resp.Header)
	// The complete rendered JSON response is bounded AFTER conversion and
	// BEFORE any header commits: an oversized body fails the exchange
	// without a partial response escaping (review-z commit 3).
	if int64(len(converted)) > h.cfg.BodyLimits.GeneratedResponseBytes {
		h.writeDialectHTTPError(r, w, CanonicalAPIError{
			Status:  http.StatusBadGateway,
			Type:    "api_error",
			Code:    "response_conversion_error",
			Message: "generated response exceeds the configured limit",
		}, ProvenanceUpstreamBodyError)
		return
	}
	h.copyResponseHeaders(w.Header(), resp.Header)
	RemoveTransformedRepresentationHeaders(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(converted)))
	w.WriteHeader(resp.StatusCode)
	// A failed write after a client abort must not be recorded as a
	// successful completion (the streaming path classifies the abort the
	// same way); a write failure without a client cancel is a downstream
	// error. A short write (n < len) with a nil error violates the
	// io.Writer contract — the recorder detects it independently, and the
	// handler must never record it as a clean completion (review-k finding
	// 7).
	n, writeErr := w.Write(converted)
	if writeErr == nil && n != len(converted) {
		writeErr = io.ErrShortWrite
	}
	switch {
	case writeErr != nil && r.Context().Err() != nil && isContextCancellationError(writeErr):
		h.recordOutcome(r, Outcome{
			// The upstream did respond (headers were committed with the
			// 2xx status); the client abandoned the exchange mid-write. The
			// completed upstream success is retained so breaker recovery and
			// monitoring still see it, matching the sibling error writers.
			UpstreamAttempted: true,
			UpstreamStatus:    Optional[int]{Value: resp.StatusCode, Set: true},
			Provenance:        ProvenanceClientAbort,
			ClientAborted:     true,
		})
	case writeErr != nil:
		h.recordOutcome(r, Outcome{
			// A downstream write failure (e.g. a short write) after a
			// successful upstream response retains the upstream success fact;
			// only the provenance changes, exactly like the error writers.
			UpstreamAttempted: true,
			UpstreamStatus:    Optional[int]{Value: resp.StatusCode, Set: true},
			Provenance:        ProvenanceDownstreamWriteError,
		})
	default:
		h.recordOutcome(r, Outcome{
			UpstreamAttempted:  true,
			UpstreamStatus:     Optional[int]{Value: resp.StatusCode, Set: true},
			Provenance:         ProvenanceUpstreamHTTP,
			DownstreamComplete: true,
		})
	}
}

// checkDecodedRequestSize rejects decoded requests that amplify beyond the
// decoded-request body limit (merge gate 19: the decoded limit is separate
// from the accepted raw-body limit). The typed error renders as 413 in the
// client dialect, never the generic conversion 400.
func (h *TranscodeHandler) checkDecodedRequestSize(rendered []byte) error {
	limit := h.cfg.BodyLimits.DecodedRequestBytes
	if limit <= 0 {
		return nil
	}
	if int64(len(rendered)) > limit {
		return errDecodedRequestTooLarge
	}
	return nil
}

// conversionProvenance classifies a response-conversion error: corrupt
// upstream wire data (UpstreamWireError) is an upstream body failure; valid
// source features the transcoder does not support (UnsupportedFeatureError),
// loss-policy rejections, and target-render failures stay local (review-k
// finding 3).
func conversionProvenance(err error) ExchangeProvenance {
	if _, ok := errors.AsType[*UpstreamWireError](err); ok {
		return ProvenanceUpstreamBodyError
	}
	return ProvenanceLocalResponseConversionError
}

// convertResponse decodes the upstream JSON response into the canonical IR
// and renders the client response envelope. The output dialect is determined
// by the CLIENT protocol, never by the upstream protocol.
func (h *TranscodeHandler) convertResponse(
	r *http.Request,
	body []byte,
	context *ExchangeContext,
) ([]byte, ExchangeProvenance, error) {
	switch h.cfg.Mapping.ClientProtocol {
	case ClientResponses:
		switch h.cfg.Mapping.UpstreamProtocol {
		case UpstreamChatCompletions:
			response, decodeReport, err := DecodeChatResponseWithPolicy(
				body,
				h.cfg.Mapping.ChatCapabilities,
				h.cfg.Mapping.LossPolicy,
			)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			converted, report, err := RenderResponsesResponse(response, context)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			// The decode's provider reasoning drop is part of the same
			// exchange log as the render's losses (request-side merge
			// precedent).
			report.Losses = append(report.Losses, decodeReport.Losses...)
			logConversionReport(report, r)
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
			response, decodeReport, err := DecodeChatResponseWithPolicy(
				body,
				h.cfg.Mapping.ChatCapabilities,
				h.cfg.Mapping.LossPolicy,
			)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			converted, report, err := RenderMessagesResponse(response, context)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			// The decode's provider reasoning drop is part of the same
			// exchange log as the render's losses (request-side merge
			// precedent).
			report.Losses = append(report.Losses, decodeReport.Losses...)
			logConversionReport(report, r)
			return converted, ProvenanceLocalResponseConversionError, nil

		case UpstreamResponses:
			response, err := DecodeResponsesResponse(body)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			converted, report, err := RenderMessagesResponse(response, context)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			logConversionReport(report, r)
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
	receivedAt time.Time,
) {
	converter, err := h.newFrameConverter(context)
	if err != nil {
		// writeLocalError -> writeDialectHTTPError records the outcome
		// exactly once.
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
	// io.ErrShortWrite, and the writer's first error surfaces as WriterErr
	// (review-k finding 7).
	downstreamComplete := observation.WriterErr == nil && observation.SealErr == nil

	// Classify the outcome from explicit provenance. The classification is
	// recorded on the outcome (review-08 additional 8) so downstream sinks
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
		// when already expired (review-z commit 4).
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
	case streamOutcomeDownstreamFailure:
		outcome.Provenance = ProvenanceDownstreamWriteError
	default:
		outcome.UpstreamAttempted = true
		outcome.UpstreamStatus = Optional[int]{Value: resp.StatusCode, Set: true}
		outcome.Provenance = ProvenanceUpstreamHTTP
		outcome.DownstreamComplete = downstreamComplete
	}
	h.recordOutcome(r, outcome)
	// Response-side approved losses are logged with the same fidelity as
	// request-side losses (review-j findings 7 and 10).
	logConversionReport(*converter.ConversionReport(), r)
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

	case client == ClientMessages && upstream == UpstreamResponses:
		state := newAnthropicResponsesStreamState(
			context,
			h.cfg.Mapping.LossPolicy,
			context.IDs.New("msg_"),
			model,
			createdAt,
		)
		return newResponsesToAnthropicConverter(state), nil

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
	// arrived (review-08 blocker 9).
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
		// Set=false when no hold was signaled (review-z commit 4).
		RetryAfter: Optional[time.Duration]{
			Value: retryAfter,
			Set:   resp.Header.Get("Retry-After") != "",
		},
	}
	client := ClientProtocol(h.cfg.Mapping.ClientProtocol)
	apiErr.Message = h.boundErrorMessage(apiErr.Message)
	if err := WriteDialectHTTPError(w, client, apiErr); err != nil {
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
// of the status — a truncated 400 is never a healthy non-failure (review-08
// blocker 8). A failed downstream write changes the provenance exactly like
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
	writeErr := WriteDialectHTTPError(w, client, apiErr)
	retryAfter := circuitbreaker.ParseRetryAfter(resp.Header, receivedAt, time.Now())
	outcome := Outcome{
		UpstreamAttempted: true,
		UpstreamStatus:    Optional[int]{Value: apiErr.Status, Set: true},
		Provenance:        ProvenanceUpstreamBodyError,
		UpstreamFailure:   true,
		// The upstream's Retry-After is a header fact that survives the
		// failed body transfer: the rate-signalled hold must be recorded
		// even when the error body could not be read, anchored at header
		// receipt (review-08 blockers 8 and 9). Set=true whenever the
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
// (review-j finding 11). A failed downstream write changes the provenance
// exactly like the other error writers.
func (h *TranscodeHandler) writeUpstreamSemanticFailure(
	r *http.Request,
	w http.ResponseWriter,
	apiErr CanonicalAPIError,
	upstreamStatus int,
) {
	client := ClientProtocol(h.cfg.Mapping.ClientProtocol)
	apiErr.Message = h.boundErrorMessage(apiErr.Message)
	writeErr := WriteDialectHTTPError(w, client, apiErr)
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
	// ErrorMessageBytes bound (review-z commit 3).
	apiErr.Message = h.boundErrorMessage(apiErr.Message)
	writeErr := WriteDialectHTTPError(w, client, apiErr)
	upstreamFailure := false
	switch provenance {
	case ProvenanceUpstreamTransportError:
		upstreamFailure = true
	case ProvenanceUpstreamHTTP, ProvenanceUpstreamBodyError:
		// Match the proxy's breaker classification (IsFailureStatus /
		// IsFailureStatusWithHeaders): 429 and 5xx are failures, and 403 is
		// a failure only when it carries a rate-limit signal (Retry-After).
		//
		// Disposition (autopsy 04 rec 2, no change): the upstream-failure
		// classification of body errors is pinned review-k design — a body
		// violating the wire contract IS an upstream health signal, and a
		// consistently-poisonous upstream SHOULD open the breaker
		// (TestHandlerCorruptUpstreamResponseIsUpstreamFailure). The
		// field-observed poison sources are eliminated at the decode layer
		// (autopsy tasks 13-15), not by widening breaker tolerance here.
		upstreamFailure = apiErr.Status == http.StatusTooManyRequests ||
			(apiErr.Status >= 500 && apiErr.Status < 600) ||
			(apiErr.Status == http.StatusForbidden && apiErr.RetryAfter != "")
	}
	outcome := Outcome{
		// The attempt fact reflects whether an upstream request was
		// dispatched: local REQUEST conversion and signing errors never
		// are; upstream HTTP/body/transport outcomes and local RESPONSE
		// conversion errors are (review-z commit 4).
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
func (h *TranscodeHandler) recordOutcome(r *http.Request, outcome Outcome) {
	// The synchronous per-request sink records exactly once (review-z
	// commit 4): there is no non-blocking path that can silently lose
	// provenance.
	if r != nil {
		if sink := OutcomeSinkFromContext(r.Context()); sink != nil {
			sink.Record(outcome)
		}
	}
	if h.outcomeFn != nil {
		h.outcomeFn(outcome)
	}
}

// copyResponseHeaders copies the upstream response headers a transcoded
// route is allowed to expose to the client-facing origin: the request-id
// family and the provider rate-limit informational headers (review-08
// blocker 10). Everything else — Set-Cookie and other control headers,
// entity metadata, and provider extension headers — is stripped: the
// translated response is a new representation and the client's origin must
// not receive cross-provider control signals.
func (h *TranscodeHandler) copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		if !isAllowedResponseHeader(name) {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

// isAllowedResponseHeader reports whether an upstream response header may be
// exposed on a transcoded route. The allowed set is the request-id family and
// the rate-limit informational headers of the pinned providers (OpenAI
// x-ratelimit-*, Anthropic anthropic-ratelimit-*), plus Retry-After.
func isAllowedResponseHeader(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "x-request-id", "request-id", "x-amzn-requestid",
		"x-amz-request-id", "x-amz-id-2", "retry-after":
		return true
	}
	return strings.HasPrefix(lower, "x-ratelimit-") ||
		strings.HasPrefix(lower, "anthropic-ratelimit-")
}

// isEventStreamMediaType reports whether the media type is exactly
// text/event-stream, shared by the Accept parsing and the response
// Content-Type matching (review-j findings 6 and 15).
func isEventStreamMediaType(mediaType string) bool {
	return mediaType == "text/event-stream"
}

// acceptRange is one parsed media range of an Accept header.
type acceptRange struct {
	mediaType string  // lowercased
	q         float64 // 0..1
	order     int     // position in the header; the first range is 0
}

// splitAcceptRanges splits an Accept header into its media ranges on commas
// that are outside quoted strings. RFC 9110 quoted-string parameter values
// may contain commas (and backslash-escaped characters); splitting naively
// on every comma would destroy such a range.
func splitAcceptRanges(accept string) []string {
	var parts []string
	start := 0
	inQuotes := false
	escaped := false
	for i := 0; i < len(accept); i++ {
		switch c := accept[i]; {
		case escaped:
			escaped = false
		case inQuotes && c == '\\':
			escaped = true
		case c == '"':
			inQuotes = !inQuotes
		case c == ',' && !inQuotes:
			parts = append(parts, accept[start:i])
			start = i + 1
		}
	}
	return append(parts, accept[start:])
}

// parseAcceptRanges parses every media range of an Accept header. Ranges
// that fail to parse, or whose q value is outside 0..1, are skipped: a
// malformed range expresses no usable preference and is ignored — it never
// decides and never disturbs the ordering of the ranges around it.
func parseAcceptRanges(accept string) []acceptRange {
	var ranges []acceptRange
	for order, part := range splitAcceptRanges(accept) {
		mediaType, params, err := mime.ParseMediaType(part)
		if err != nil {
			continue
		}
		r := acceptRange{mediaType: mediaType, q: 1, order: order}
		if raw, ok := params["q"]; ok {
			q, err := strconv.ParseFloat(raw, 64)
			// math.IsNaN rejects NaN, which passes the numeric bounds check
			// (NaN compares false against everything) and would otherwise
			// poison same-specificity selection as an undefeatable exclusion.
			if err != nil || math.IsNaN(q) || q < 0 || q > 1 {
				continue
			}
			r.q = q
		}
		ranges = append(ranges, r)
	}
	return ranges
}

// acceptPreference is the client's preference for one representation derived
// from an Accept header.
type acceptPreference struct {
	acceptable  bool    // the representation is not excluded (see below)
	q           float64 // effective quality of the deciding range
	specificity int     // specificity of the deciding range
	order       int     // position of the deciding range
}

// effectiveAcceptPreference derives the client's preference for mediaType
// from parsed Accept ranges, matched by the exact type, typeWildcard, or
// */*. The most specific applicable range determines the representation's
// quality (RFC 9110 §12.5.1): a q=0 exact exclusion wins over a later */*,
// and a less specific range never dilutes the most specific decision. Among
// equally specific ranges the highest q wins, and the earliest such range
// decides order ties. A representation with no applicable range is
// acceptable by default (RFC 9110: unmentioned representations are
// acceptable); it is excluded only when an applicable range exists and the
// most specific tier's quality is 0.
func effectiveAcceptPreference(ranges []acceptRange, mediaType, typeWildcard string) acceptPreference {
	var best acceptPreference
	best.specificity = -1
	for _, r := range ranges {
		specificity := -1
		switch r.mediaType {
		case mediaType:
			specificity = 2
		case typeWildcard:
			specificity = 1
		case "*/*":
			specificity = 0
		}
		if specificity < 0 {
			continue
		}
		if specificity > best.specificity ||
			specificity == best.specificity &&
				(r.q > best.q || r.q == best.q && r.order < best.order) {
			best = acceptPreference{
				q:           r.q,
				specificity: specificity,
				order:       r.order,
			}
		}
	}
	best.acceptable = !(best.specificity >= 0 && best.q == 0)
	return best
}

// acceptIsEventStream reports whether the client's Accept header selects
// text/event-stream as the most-preferred acceptable representation over
// application/json. Every media range is parsed (media type and q); a q=0
// range excludes the representation it names (an unmentioned representation
// remains acceptable by default); malformed ranges are ignored. When exactly
// one representation is acceptable it is selected; otherwise the documented
// precedence applies: (1) effective quality, where the most specific
// applicable range determines a representation's quality; (2) specificity
// (exact type over type/* over */*); (3) order of appearance in the header.
// A tie that remains (e.g. Accept: */*, or no acceptable representation at
// all) defaults to application/json, the non-streaming representation.
func acceptIsEventStream(accept string) bool {
	ranges := parseAcceptRanges(accept)
	sse := effectiveAcceptPreference(ranges, "text/event-stream", "text/*")
	jsonPref := effectiveAcceptPreference(ranges, "application/json", "application/*")
	if !sse.acceptable && !jsonPref.acceptable {
		return false
	}
	if !jsonPref.acceptable {
		return true
	}
	if !sse.acceptable {
		return false
	}
	if sse.q != jsonPref.q {
		return sse.q > jsonPref.q
	}
	if sse.specificity != jsonPref.specificity {
		return sse.specificity > jsonPref.specificity
	}
	if sse.order != jsonPref.order {
		return sse.order < jsonPref.order
	}
	return false
}

// isEventStream reports whether the response is an SSE stream, matched by
// media type exactly — text/event-streaming and other lookalikes are not
// streams (review-j finding 15).
func isEventStream(resp *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && isEventStreamMediaType(mediaType)
}

// isJSON reports whether the response is JSON: the application/json media
// type or a structured-syntax-suffix member of the JSON family
// (application/*+json). application/notjson and other lookalikes are not
// JSON (review-j finding 15). The structured-syntax suffix "+json" matches
// only within the application tree (review-08 additional 7):
// text/example+json is NOT JSON, per RFC 6839 — the suffix is only defined
// for application/*.
func isJSON(resp *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	if mediaType == "application/json" {
		return true
	}
	base, _, ok := strings.Cut(mediaType, "/")
	if !ok || base != "application" {
		return false
	}
	return strings.HasSuffix(mediaType, "+json")
}

// isUpgradeRequest reports whether the request carries an Upgrade token.
func isUpgradeRequest(r *http.Request) bool {
	if len(r.Header.Values("Upgrade")) > 0 {
		return true
	}
	for _, value := range r.Header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// isContextCancellationError reports whether the error is cancellation-derived
// (context.Canceled or context.DeadlineExceeded, possibly wrapped). Client
// abort suppression applies only to such errors: an unrelated transport or
// body failure racing with a client cancellation must win as its true
// classification (review-08 blocker 8).
func isContextCancellationError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// nowUnix returns the current unix time in seconds as a float64: the
// Responses contract's created_at is float64, so the generated stream
// identity timestamps share the type end-to-end (review-z commit 1).
func nowUnix() float64 {
	return float64(time.Now().Unix())
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

// boundErrorMessage truncates a client-visible error message to the
// configured ErrorMessageBytes bound (review-z commit 3). Every error
// writing path goes through it, so no upstream error body can amplify
// client-visible text beyond the bound.
func (h *TranscodeHandler) boundErrorMessage(message string) string {
	max := h.cfg.BodyLimits.ErrorMessageBytes
	if max <= 0 {
		return message
	}
	if len(message) <= max {
		return message
	}
	// The ellipsis must not push the message past the configured bound.
	if max > 3 {
		return message[:max-3] + "…"
	}
	return message[:max]
}

// logRequestError logs a local failure with its detail (never the client
// message): local construction and conversion failures are logged,
// sanitized, and reported neutrally (review-j finding 14; autopsy 04 rec 3
// pins the conversion-failure path for operator observability).
func (h *TranscodeHandler) logRequestError(r *http.Request, err error) {
	log.Printf("transcode: %s %s: %v", r.Method, r.URL.Path, err)
}
