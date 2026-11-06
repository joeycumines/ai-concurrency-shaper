package transcode

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
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
	// particular, a zero DecodedRequestBytes is never treated as unlimited.
	// The handler-side per-use fallbacks remain as
	// defense-in-depth for any future construction path that bypasses this
	// normalization.
	cfg.BodyLimits = cfg.BodyLimits.WithDefaults()
	// The configuration is frozen at construction: the mapping and upstream
	// URL are deep-copied once into the private configuration, so
	// programmatic callers can never mutate live configuration. The mapping's Secret source is resolved ONCE here into a
	// static source (when not the inbound-forwarding mode): a mutable
	// custom SecretSource can no longer change live behavior or fail
	// mid-exchange — the same freeze contract the provider auth policy
	// satisfies through auth.FreezeAuthPolicy, including its one exception:
	// a rotating auth.DynamicSecretSource is resolved but kept live.
	cfg.Mapping = cloneMapping(cfg.Mapping)
	if !cfg.Mapping.Auth.Inbound && cfg.Mapping.Auth.Secret != nil {
		frozen, err := freezeSecretSource(context.Background(), cfg.Mapping.Auth.Secret)
		if err != nil {
			panic(fmt.Sprintf("transcode: resolve mapping auth secret: %v", err))
		}
		cfg.Mapping.Auth.Secret = frozen
	}
	upstream := *cfg.Upstream
	cfg.Upstream = &upstream
	// Startup observability (operator choice: aggregate + startup
	// summary): log the route's approved-loss profile once at construction,
	// so the operator sees what this route will observably lose. The
	// per-request approved-loss line (logConversionReport) is the
	// request-specific signal; this is the route-level reference. A strict
	// route (empty Allowed) logs 'none (strict)' so the operator knows no
	// loss is approved.
	keys := make([]Feature, 0, len(cfg.Mapping.LossPolicy.Allowed))
	for k := range cfg.Mapping.LossPolicy.Allowed {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if len(keys) == 0 {
		log.Printf(
			"transcode: %s %s: loss policy approves: none (strict)",
			cfg.Mapping.ClientRoute.Method,
			cfg.Mapping.ClientRoute.Path,
		)
	} else {
		var b strings.Builder
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(string(k))
			if d := LossKeyDescription(k); d != "" {
				fmt.Fprintf(&b, ": %s", d)
			}
		}
		log.Printf(
			"transcode: %s %s: loss policy approves: %s",
			cfg.Mapping.ClientRoute.Method,
			cfg.Mapping.ClientRoute.Path,
			b.String(),
		)
	}
	if cfg.FlowLogDir != "" {
		info, err := os.Stat(cfg.FlowLogDir)
		if err != nil || !info.IsDir() {
			panic(fmt.Sprintf("transcode: invalid flow log directory %q: not an existing directory", cfg.FlowLogDir))
		}
		log.Printf(
			"transcode: %s %s: flow recorder enabled: writing unredacted full exchange records to %q",
			cfg.Mapping.ClientRoute.Method,
			cfg.Mapping.ClientRoute.Path,
			cfg.FlowLogDir,
		)
	}
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
	if m.ProfileMap.Profiles != nil {
		profiles := make(map[string]ProfileMapping, len(m.ProfileMap.Profiles))
		maps.Copy(profiles, m.ProfileMap.Profiles)
		m.ProfileMap.Profiles = profiles
	}
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
// missing outcome.

func (h *TranscodeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Diagnostic full-flow tap (-transcode-flowlog-dir): captures the
	// complete contents of every transcoded exchange to one JSON file each.
	// Nil when disabled; every hook below is a nil-guarded no-op then.
	fl := newFlowCapture(h.cfg.FlowLogDir)
	if fl != nil {
		fl.setClientRequest(r)
		fl.setCommitted(CommittedStreamFromContext(r.Context()))
		r = r.WithContext(withFlowCapture(r.Context(), fl))
		w = fl.wrapWriter(w)
		defer fl.seal()
	}
	defer func() {
		if sink := OutcomeSinkFromContext(r.Context()); sink != nil {
			if _, recorded := sink.Load(); !recorded {
				// recordOutcome records to the same sink (once-only) and
				// carries the outcome to the diagnostic tap.
				h.recordOutcome(r, LocalFailureOutcome())
			}
		}
	}()
	// Reject Upgrade requests on transcoded routes: a 101 Switching
	// Protocols response cannot be meaningfully schema-transcoded.
	if isUpgradeRequest(r) {
		h.logRequestError(r, fmt.Errorf("[%s] upgrade requests are not supported on transcoded routes", ProvenanceLocalRequestConversionError))
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
		h.logRequestError(r, fmt.Errorf("[%s] content-encoding is not supported on transcoded routes", ProvenanceLocalRequestConversionError))
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
			h.logRequestError(r, fmt.Errorf("[%s] request body too large", ProvenanceLocalRequestConversionError))
			h.writeLocalError(r, w,
				http.StatusRequestEntityTooLarge, "request body too large",
				ProvenanceLocalRequestConversionError)
			return
		}
		h.logRequestError(r, fmt.Errorf("[%s] read request body: %s", ProvenanceLocalRequestConversionError, h.boundErrorMessage(err.Error())))
		h.writeLocalError(r, w,
			http.StatusBadRequest, "read request body: "+h.boundErrorMessage(err.Error()),
			ProvenanceLocalRequestConversionError)
		return
	}
	fl.setClientBody(body)

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
		// body limit — or whose echo exceeds the echo bound — is a 413, not a
		// generic conversion 400.
		status := http.StatusBadRequest
		if errors.Is(err, errDecodedRequestTooLarge) || errors.Is(err, errEchoTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		h.logRequestError(r, fmt.Errorf("[%s] convert request: %s", ProvenanceLocalRequestConversionError, h.boundErrorMessage(err.Error())))
		h.writeLocalError(r, w,
			status, "convert request: "+h.boundErrorMessage(err.Error()),
			ProvenanceLocalRequestConversionError)
		return
	}
	fl.setUpstreamRequest(upstreamBody, context.StreamIntent)

	if CommittedStreamFromContext(r.Context()) && !context.StreamIntent {
		h.logRequestError(r, fmt.Errorf("[%s] stream:false is incompatible with committed event-stream representation", ProvenanceLocalRequestConversionError))
		h.writeDialectHTTPError(r, w, CanonicalAPIError{
			Status:  http.StatusBadRequest,
			Type:    "invalid_request_error",
			Code:    "stream_intent_mismatch",
			Message: "stream:false is incompatible with committed event-stream representation",
		}, ProvenanceLocalRequestConversionError)
		return
	}

	outReq, err := h.buildUpstreamRequest(r, upstreamBody, context)
	if err != nil {
		// An unallowed client query parameter or an invalid inbound
		// credential is a client fault (400); other request-construction
		// failures are internal (500). Internal construction errors (secret
		// resolution, signing) never leak details such as file paths into
		// the client message; the detail is logged.
		status := http.StatusInternalServerError
		message := "build upstream request: internal error"
		if errors.Is(err, errClientQueryParameter) ||
			errors.Is(err, errAuthInboundCredential) {
			status = http.StatusBadRequest
			h.logRequestError(r, fmt.Errorf("[%s] build upstream request: %s", ProvenanceLocalRequestConversionError, h.boundErrorMessage(err.Error())))
			message = "build upstream request: " + h.boundErrorMessage(err.Error())
		} else {
			h.logRequestError(r, fmt.Errorf("[%s] build upstream request: %s", ProvenanceLocalRequestConversionError, h.boundErrorMessage(err.Error())))
		}
		h.writeLocalError(r, w,
			status, message,
			ProvenanceLocalRequestConversionError)
		return
	}
	fl.setUpstreamTarget(outReq)

	if preset := h.cfg.Mapping.Opencode; preset.Enabled {
		// First-party headers go on after authentication (applied
		// inside buildUpstreamRequest): the mock never clobbers
		// credentials. The conversation key derives from the client
		// model so the same conversation keeps one key whether it is
		// served natively or converted; without user text it degrades to
		// the client-derived key.
		keyModel := context.RequestedClientModel
		if keyModel == "" {
			keyModel = context.UpstreamModel
		}
		key := DeriveClientKey(preset.Provider, r.RemoteAddr)
		if firstText := FirstUserText(clientProtocolToNative(h.cfg.Mapping.ClientProtocol), body); firstText != "" {
			key = DeriveConversationKey(preset.Provider, keyModel, firstText)
		}
		preset.ApplyHeaders(outReq.Header, r.Header, preset.ResolveSession(r.Header, key))
	}

	resp, err := h.roundTrip(outReq)
	// The response headers arrived: anchor Retry-After and the 403
	// rate-signal classification here so body-read time is excluded from the
	// recorded hold.
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
		// as the upstream transport failure it is.
		if r.Context().Err() != nil && isContextCancellationError(err) {
			h.recordOutcome(r, Outcome{Provenance: ProvenanceClientAbort, ClientAborted: true})
			// A cancelled client context means the exchange is already over.
			// Return normally: the proxy classifies the abort from the
			// recorded outcome, and the net/http server tolerates a handler
			// that returns without writing to a dead connection.
			return
		}
		// A signer error is a local construction/auth failure (neutral),
		// never an upstream transport failure.
		if signingErr, ok := errors.AsType[*SigningError](err); ok {
			// The signer failure is logged with detail; the client message
			// is sanitized. The code is request-side: the failure
			// happened building the upstream request, before any response
			// existed.
			h.logRequestError(r, signingErr)
			h.writeDialectHTTPError(r, w, CanonicalAPIError{
				Status:  http.StatusBadGateway,
				Type:    "api_error",
				Code:    "request_conversion_error",
				Message: "build upstream request: internal error",
			}, ProvenanceLocalRequestConversionError)
			return
		}
		if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
			h.logRequestError(r, sanitizeUpstreamTransportError(err))
			h.writeCircuitOpenError(r, w)
			return
		}
		// The transport error may embed the full outbound URL (a custom
		// http.Client-based transport produces *url.Error with the complete
		// request URL, including a credential-bearing upstream base query).
		// The detail is logged server-side with sensitive URL query values
		// redacted; the client message is neutral, exactly as the native
		// passthrough path sends a fixed body.
		h.logRequestError(r, sanitizeUpstreamTransportError(err))
		h.writeDialectHTTPError(r, w, CanonicalAPIError{
			Status:  http.StatusBadGateway,
			Type:    "api_error",
			Code:    "upstream_transport_error",
			Message: "upstream request failed",
		}, ProvenanceUpstreamTransportError)
		return
	}
	defer resp.Body.Close()
	resp.Body = fl.wrapUpstreamBody(resp.Body, resp)

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
	// A streaming request cannot be answered with JSON and
	// a non-streaming request cannot be answered with an SSE stream.
	switch {
	case isEventStream(resp):
		if !context.StreamIntent {
			// The upstream returned the wrong representation for the
			// negotiated mode: corrupt upstream wire, an upstream failure
			// that fails a half-open probe and applies the failure hold.
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

func (h *TranscodeHandler) recordOutcome(r *http.Request, outcome Outcome) {
	// Diagnostic tap: stash the outcome on the per-exchange flow capture.
	if r != nil {
		flowFromContext(r.Context()).setOutcome(outcome)
	}
	// The synchronous per-request sink records exactly once: there is no non-blocking path that can silently lose
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
// family and the provider rate-limit informational headers. Everything else — Set-Cookie and other control headers,
// entity metadata, and provider extension headers — is stripped: the
// translated response is a new representation and the client's origin must
// not receive cross-provider control signals.

type HandlerConfig struct {
	// Mapping is the validated route mapping.
	Mapping Mapping

	// Upstream is the upstream base URL.
	Upstream *url.URL

	// BodyLimits are the independent request/response body limits.
	BodyLimits BodyLimits

	// FlowLogDir, when non-empty, is an existing directory that receives one
	// JSON record per transcoded exchange capturing the full flow (client
	// request, converted upstream request, upstream response, downstream
	// response, conversion reports, outcome). The directory is validated by
	// the constructing caller (the proxy), never created here, and the
	// records are unredacted: treat the directory as secret-bearing.
	FlowLogDir string

	// Continuity, when non-nil, is the opt-in bounded per-conversation
	// store behind the statefulness decision (OFF by default: nil means no
	// retention, and previous_response_id keeps its existing observable
	// loss). When set, the handler records the canonical conversation that
	// produced each emitted Responses id and resolves a later request's
	// previous_response_id against it; a miss degrades to the existing
	// loss, never a failure. ContinuityKey scopes retained ids to the
	// provider mapping that emitted them.
	Continuity    *ContinuityStore
	ContinuityKey string

	// GeminiSignatures, when non-nil, is the bounded per-provider store of
	// Gemini function-call thought signatures, used by renderers whose
	// upstream is Gemini to restore signatures on replayed calls.
	// GeminiSignatureScope scopes the store to the provider. A nil store
	// behaves as an empty store.
	GeminiSignatures     *GeminiSignatureStore
	GeminiSignatureScope string
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
// mid-exchange.
