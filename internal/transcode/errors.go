package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Anthropic HTTP error contract:
// https://platform.claude.com/docs/en/api/errors#error-shapes
//
// Anthropic SSE error contract:
// https://platform.claude.com/docs/en/build-with-claude/streaming#error-events
//
// Responses SSE error contract:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L2991-L3020
//
// OpenAI HTTP error guidance:
// https://platform.openai.com/docs/guides/error-codes/api-errors

// maxUpstreamErrorBodyBytes bounds the upstream error body read so a
// misbehaving provider cannot exhaust memory with an unbounded error page.
const maxUpstreamErrorBodyBytes int64 = 1 << 20

// CanonicalAPIError is the single internal form of every local and upstream
// error. The target renderer maps it into the client dialect.
type CanonicalAPIError struct {
	Status int

	Type    string
	Code    string
	Message string
	Param   string

	RequestID  string
	RetryAfter string
}

// openAIErrorEnvelope is the OpenAI HTTP error shape.
type openAIErrorEnvelope struct {
	Error struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Param   json.RawMessage `json:"param"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
}

// anthropicErrorEnvelope is the Anthropic HTTP error shape.
type anthropicErrorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

// ReadCanonicalUpstreamError parses a non-2xx upstream response into the
// canonical error form, preserving status, request ID, and Retry-After. Raw
// provider HTML error pages are never forwarded; only a bounded,
// whitespace-normalized message is retained. maxBytes bounds the error body
// read (0 selects the package default).
func ReadCanonicalUpstreamError(
	resp *http.Response,
	upstream UpstreamProtocol,
	maxBytes int64,
) (CanonicalAPIError, error) {
	if maxBytes <= 0 {
		maxBytes = maxUpstreamErrorBodyBytes
	}
	if resp == nil {
		return CanonicalAPIError{}, errors.New("nil upstream response")
	}

	body, err := io.ReadAll(
		io.LimitReader(resp.Body, maxBytes+1),
	)
	if err != nil {
		return CanonicalAPIError{}, err
	}
	if int64(len(body)) > maxBytes {
		return CanonicalAPIError{}, errors.New("upstream error body exceeds the configured limit")
	}

	result := CanonicalAPIError{
		Status: resp.StatusCode,
		RequestID: firstNonEmpty(
			resp.Header.Get("x-request-id"),
			resp.Header.Get("request-id"),
			resp.Header.Get("x-amzn-requestid"),
		),
		RetryAfter: resp.Header.Get("Retry-After"),
	}

	switch upstream {
	case UpstreamResponses, UpstreamChatCompletions:
		var envelope openAIErrorEnvelope
		if err := json.Unmarshal(body, &envelope); err == nil &&
			envelope.Error.Message != "" {
			result.Message = envelope.Error.Message
			result.Type = envelope.Error.Type
			result.Param = rawScalarString(envelope.Error.Param)
			result.Code = rawScalarString(envelope.Error.Code)
			return normalizeCanonicalError(result), nil
		}

	case UpstreamMessages:
		var envelope anthropicErrorEnvelope
		if err := json.Unmarshal(body, &envelope); err == nil &&
			envelope.Error.Message != "" {
			result.Message = envelope.Error.Message
			result.Type = envelope.Error.Type
			if result.RequestID == "" {
				result.RequestID = envelope.RequestID
			}
			return normalizeCanonicalError(result), nil
		}
	}

	// Never forward a raw provider HTML error page. Preserve a bounded,
	// whitespace-normalized message only.
	result.Message = sanitizeErrorText(body)
	result.Type = typeForStatus(result.Status)
	result.Code = codeForStatus(result.Status)
	return normalizeCanonicalError(result), nil
}

// normalizeCanonicalError fills defaults for empty fields.
func normalizeCanonicalError(e CanonicalAPIError) CanonicalAPIError {
	if e.Status == 0 {
		e.Status = http.StatusBadGateway
	}
	if e.Type == "" {
		e.Type = typeForStatus(e.Status)
	}
	if e.Code == "" {
		e.Code = codeForStatus(e.Status)
	}
	if e.Message == "" {
		e.Message = http.StatusText(e.Status)
	}
	return e
}

// typeForStatus maps a status to an OpenAI-style error type.
func typeForStatus(status int) string {
	switch status {
	case 400, 404, 409, 413, 422:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}

// codeForStatus maps a status to a stable error code.
func codeForStatus(status int) string {
	switch status {
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case 529:
		return "overloaded"
	default:
		return strings.ReplaceAll(
			strings.ToLower(http.StatusText(status)),
			" ",
			"_",
		)
	}
}

// WriteDialectHTTPError renders a canonical error in the client dialect.
func WriteDialectHTTPError(
	w http.ResponseWriter,
	target ClientProtocol,
	apiErr CanonicalAPIError,
) error {
	apiErr = normalizeCanonicalError(apiErr)

	RemoveHopByHopHeaders(w.Header())
	RemoveTransformedRepresentationHeaders(w.Header())
	w.Header().Set("Content-Type", "application/json")

	if apiErr.RetryAfter != "" {
		w.Header().Set("Retry-After", apiErr.RetryAfter)
	}

	switch target {
	case ClientResponses:
		if apiErr.RequestID != "" {
			w.Header().Set("X-Request-Id", apiErr.RequestID)
		}

		body := struct {
			Error struct {
				Message string  `json:"message"`
				Type    string  `json:"type"`
				Param   *string `json:"param"`
				Code    string  `json:"code"`
			} `json:"error"`
		}{}
		body.Error.Message = apiErr.Message
		body.Error.Type = apiErr.Type
		body.Error.Code = apiErr.Code
		if apiErr.Param != "" {
			body.Error.Param = &apiErr.Param
		}

		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(apiErr.Status)
		return writeAll(w, payload)

	case ClientMessages:
		if apiErr.RequestID != "" {
			w.Header().Set("Request-Id", apiErr.RequestID)
		}

		anthropicType := anthropicErrorType(apiErr)
		body := struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			RequestID string `json:"request_id,omitempty"`
		}{
			Type:      "error",
			RequestID: apiErr.RequestID,
		}
		body.Error.Type = anthropicType
		body.Error.Message = apiErr.Message

		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(apiErr.Status)
		return writeAll(w, payload)

	default:
		return fmt.Errorf("unknown client error dialect %q", target)
	}
}

func anthropicErrorType(e CanonicalAPIError) string {
	switch e.Status {
	case 400, 422:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// sanitizeErrorText bounds and whitespace-normalizes an opaque error body.
func sanitizeErrorText(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return "upstream returned an error without a response body"
	}

	text := strings.Join(strings.Fields(string(body)), " ")
	const max = 2048
	if len(text) > max {
		text = text[:max] + "…"
	}
	return text
}

// rawScalarString extracts a string from a raw JSON scalar, falling back to
// the raw bytes when the value is not a string.
func rawScalarString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}

	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// firstNonEmpty returns the first non-empty value.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// UpstreamWireError is returned when the SOURCE wire is malformed or
// self-contradictory — corrupt upstream protocol data: invalid JSON, an
// envelope violating the protocol contract, mismatched stream identity,
// illegal lifecycle transitions, or malformed tool arguments. Such an
// exchange is an upstream failure (breaker penalty and slot protection).
// Valid source features this transcoder does not support remain
// UnsupportedFeatureError — a local conversion result — and are never
// wrapped in this type (review-k finding 3).
type UpstreamWireError struct {
	// Protocol is the upstream protocol whose wire data is corrupt.
	Protocol UpstreamProtocol
	// Status is the upstream HTTP response status when known (streams
	// always carry 200; non-streaming decodes leave it zero and the handler
	// records the actual response status).
	Status int
	// Cause is the underlying decode or state-validation failure.
	Cause error
}

func (e *UpstreamWireError) Error() string {
	if e.Cause == nil {
		return "upstream wire error"
	}
	return e.Cause.Error()
}

func (e *UpstreamWireError) Unwrap() error { return e.Cause }

// UsageArithmeticError is the typed error for token-usage arithmetic that
// violates the pinned contracts (review-z commit 5): a known total that is
// not the exact sum of its components (chat and responses contracts define
// total_tokens as input + output exactly), or a component too large for the
// target wire's integer width (silent overflow on 32-bit builds is never
// acceptable). Classification: the decode sites wrap instances raised from
// a SOURCE total mismatch in upstreamWireError (corrupt upstream wire — an
// upstream failure); instances raised while RENDERING a target dialect
// (integer-width overflow) stay local. errors.As finds the typed error
// through either wrap; Detail names the violated invariant and Input,
// Output, Total carry the involved counts.
type UsageArithmeticError struct {
	// Detail names the violated invariant.
	Detail string
	// SourceMismatch discriminates the two raise sites: TRUE for a known
	// total that is not the exact sum of its components (corrupt upstream
	// wire — an upstream failure); FALSE for a count that is valid per
	// contract but unrepresentable on this platform while rendering a
	// target dialect (a local rendering impossibility). Classification
	// points must branch on this flag, never on the detail text.
	SourceMismatch bool
	// Input, Output, Total are the involved token counts (zero when not
	// applicable).
	Input  int64
	Output int64
	Total  int64
}

func (e *UsageArithmeticError) Error() string {
	if e.Detail == "" {
		return "usage arithmetic error"
	}
	return "usage arithmetic: " + e.Detail
}

// upstreamWireError wraps cause as corrupt upstream wire data. A cause that
// is or carries an UnsupportedFeatureError is NEVER wrapped: a valid source
// feature this transcoder knows but does not support is a local conversion
// result, and the typed error is passed through untouched so no classification
// point can mistake it for corrupt wire (review-k finding 3).
func upstreamWireError(protocol UpstreamProtocol, status int, cause error) error {
	if _, ok := errors.AsType[*UnsupportedFeatureError](cause); ok {
		return cause
	}
	return &UpstreamWireError{Protocol: protocol, Status: status, Cause: cause}
}

// StreamConversionError is a typed conversion error carrying provenance and
// the upstream response status, so the exchange classification never relies
// on error-string matching (review-j finding 11).
type StreamConversionError struct {
	Cause      error
	Provenance ExchangeProvenance
	Status     int
}

func (e *StreamConversionError) Error() string {
	if e.Cause == nil {
		return "stream conversion error"
	}
	return e.Cause.Error()
}

func (e *StreamConversionError) Unwrap() error { return e.Cause }

// UpstreamSemanticFailureError is returned when a non-stream upstream
// response itself reports a failure — a 2xx Responses envelope with status
// "failed". The exchange classifies as an upstream failure with the
// upstream's HTTP status, matching the streamed response.failed
// classification; it is never a local conversion error (review-j finding
// 11).
type UpstreamSemanticFailureError struct {
	Message string
}

func (e *UpstreamSemanticFailureError) Error() string {
	if e.Message == "" {
		return "upstream response failed"
	}
	return "upstream response failed: " + e.Message
}

// UnrepresentableError reports a valid source value that the target dialect
// cannot represent at all — e.g. model-generated tool arguments that are not
// a JSON object when the target requires an object. It is a LOCAL conversion
// result: invalid model output is never an upstream defect, never opens the
// circuit breaker, and the exchange records it neutral (review-z commit 2).
type UnrepresentableError struct {
	Protocol string
	Path     string
	Detail   string
}

func (e *UnrepresentableError) Error() string {
	if e.Detail == "" {
		return e.Protocol + " field " + e.Path + " is unrepresentable in the target dialect"
	}
	return e.Protocol + " field " + e.Path + ": " + e.Detail
}
