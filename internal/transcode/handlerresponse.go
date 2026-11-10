package transcode

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

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
		// — never a local conversion failure.
		if _, ok := errors.AsType[*UpstreamSemanticFailureError](err); ok {
			h.writeUpstreamSemanticFailure(r, w, apiErr, resp.StatusCode)
			return
		}
		// The conversion failure is logged server-side (
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
	// without a partial response escaping.
	if int64(len(converted)) > h.cfg.BodyLimits.GeneratedResponseBytes {
		h.writeDialectHTTPError(r, w, CanonicalAPIError{
			Status:  http.StatusBadGateway,
			Type:    "api_error",
			Code:    "response_conversion_error",
			Message: "generated response exceeds the configured limit",
		}, ProvenanceLocalResponseConversionError)
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
	// handler must never record it as a clean completion.
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

// conversionProvenance classifies a response-conversion error: corrupt
// upstream wire data (UpstreamWireError) is an upstream body failure; valid
// source features the transcoder does not support (UnsupportedFeatureError),
// loss-policy rejections, and target-render failures stay local.
func conversionProvenance(err error) ExchangeProvenance {
	if _, ok := errors.AsType[*UpstreamWireError](err); ok {
		return ProvenanceUpstreamBodyError
	}
	return ProvenanceLocalResponseConversionError
}

// storeGeminiCallSignatures retains the thought signatures a Gemini upstream
// attached to its function calls, keyed by the client-visible call id, so a
// later turn replays them through RenderGeminiRequest. A nil store is an
// empty store: nothing is retained and the renderer falls back to the
// documented placeholder.
func (h *TranscodeHandler) storeGeminiCallSignatures(context *ExchangeContext, response CanonicalResponse) {
	if h.cfg.GeminiSignatures == nil || len(response.Source.GeminiCallSignatures) == 0 {
		return
	}
	for callID, signature := range response.Source.GeminiCallSignatures {
		h.cfg.GeminiSignatures.Put(h.cfg.GeminiSignatureScope, callID, signature)
	}
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
				context.ToolNames,
			)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			converted, report, err := RenderResponsesResponse(response, context)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			// Opt-in continuity: retain the canonical conversation that
			// produced the emitted id, so a follow-up carrying it as
			// previous_response_id can be reconstructed. The envelope id
			// is minted inside the render (context.IDs), so it is read
			// back from the converted bytes — response.ID is the upstream
			// chat id, never the client-visible id. Nothing is retained
			// when the store is nil (default) or the client sent store:false.
			h.recordContinuityFromEnvelope(r, context, response, converted)
			// The decode's provider reasoning drop is part of the same
			// exchange log as the render's losses (request-side merge
			// precedent).
			report.Losses = append(report.Losses, decodeReport.Losses...)
			h.logConversionReport(report, r, "response")
			return converted, ProvenanceLocalResponseConversionError, nil
		case UpstreamGemini:
			response, decodeReport, err := DecodeGeminiResponse(
				body,
				h.cfg.Mapping.ChatCapabilities,
				h.cfg.Mapping.LossPolicy,
				context.IDs,
			)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			converted, report, err := RenderResponsesResponse(response, context)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			// Retain the thought signatures the upstream attached to its
			// function calls, keyed by the client-visible call id, so a
			// later turn replays them (Gemini 3 rejects calls without one).
			h.storeGeminiCallSignatures(context, response)
			h.recordContinuityFromEnvelope(r, context, response, converted)
			report.Losses = append(report.Losses, decodeReport.Losses...)
			h.logConversionReport(report, r, "response")
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
				context.ToolNames,
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
			h.logConversionReport(report, r, "response")
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
			h.logConversionReport(report, r, "response")
			return converted, ProvenanceLocalResponseConversionError, nil

		case UpstreamGemini:
			response, decodeReport, err := DecodeGeminiResponse(
				body,
				h.cfg.Mapping.ChatCapabilities,
				h.cfg.Mapping.LossPolicy,
				context.IDs,
			)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			converted, report, err := RenderMessagesResponse(response, context)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			h.storeGeminiCallSignatures(context, response)
			report.Losses = append(report.Losses, decodeReport.Losses...)
			h.logConversionReport(report, r, "response")
			return converted, ProvenanceLocalResponseConversionError, nil

		default:
			return nil, ProvenanceLocalResponseConversionError, fmt.Errorf(
				"unsupported upstream protocol %q for messages client",
				h.cfg.Mapping.UpstreamProtocol,
			)
		}

	case ClientGemini:
		switch h.cfg.Mapping.UpstreamProtocol {
		case UpstreamChatCompletions:
			response, decodeReport, err := DecodeChatResponseWithPolicy(
				body,
				h.cfg.Mapping.ChatCapabilities,
				h.cfg.Mapping.LossPolicy,
				context.ToolNames,
			)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			converted, report, err := RenderGeminiResponse(response, context)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			report.Losses = append(report.Losses, decodeReport.Losses...)
			h.logConversionReport(report, r, "response")
			return converted, ProvenanceLocalResponseConversionError, nil
		case UpstreamResponses:
			response, err := DecodeResponsesResponse(body)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			converted, report, err := RenderGeminiResponse(response, context)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			h.logConversionReport(report, r, "response")
			return converted, ProvenanceLocalResponseConversionError, nil
		case UpstreamMessages:
			response, decodeReport, err := DecodeMessagesResponse(body, h.cfg.Mapping.LossPolicy)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			converted, report, err := RenderGeminiResponse(response, context)
			if err != nil {
				return nil, conversionProvenance(err), err
			}
			report.Losses = append(report.Losses, decodeReport.Losses...)
			h.logConversionReport(report, r, "response")
			return converted, ProvenanceLocalResponseConversionError, nil
		default:
			return nil, ProvenanceLocalResponseConversionError, fmt.Errorf(
				"unsupported upstream protocol %q for gemini client",
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

// copyResponseHeaders copies the upstream response headers a transcoded
// route is allowed to expose to the client-facing origin: the request-id
// family and the provider rate-limit informational headers. Everything else — Set-Cookie and other control headers,
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

// isEventStream reports whether the response is an SSE stream, matched by
// media type exactly — text/event-streaming and other lookalikes are not
// streams.
func isEventStream(resp *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && isEventStreamMediaType(mediaType)
}

// isJSON reports whether the response is JSON: the application/json media
// type or a structured-syntax-suffix member of the JSON family
// (application/*+json). application/notjson and other lookalikes are not
// JSON. The structured-syntax suffix "+json" matches
// only within the application tree:
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
