package transcode

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

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
		// Stream intent precedence: the request body's
		// stream field, when explicitly present, is authoritative; only when
		// absent does the client Accept header select the representation
		// (parsed below with full media-range and q-value semantics — q=0
		// excludes, malformed ranges are ignored). The response media type
		// must agree with the resolved intent.
		StreamIntent: AcceptIsEventStream(r.Header.Get("Accept")),
	}

	// Resolve the client model through the mapping once the decoded request
	// reveals it. The client-facing alias is returned in the response; the
	// upstream model is used on the outbound request. Profile-aware
	// resolution: if the client model matches a profile name, use the
	// profile's model and tier; otherwise fall through to ModelMap.Resolve.
	resolveModel := func(clientModel string, report *ConversionReport) error {
		// Try profile resolution first.
		if profileModel, profileTier, ok := h.cfg.Mapping.ProfileMap.ResolveProfile(clientModel); ok {
			// Profile found. Resolve the profile's target model through ModelMap.
			mappingModel, err := h.cfg.Mapping.ModelMap.Resolve(profileModel)
			if err != nil {
				return fmt.Errorf("profile %q targets unmapped model %q: %w", clientModel, profileModel, err)
			}
			// Profile model is mapped. Record Note if tier collapsed.
			if profileTier != "" && mappingModel.ReasoningTier != "" && profileTier != mappingModel.ReasoningTier {
				if err := report.Note(
					FeatureProfileRouting,
					"model",
					fmt.Sprintf("profile %q requested tier %q but model mapping pins tier %q", clientModel, profileTier, mappingModel.ReasoningTier),
				); err != nil {
					return err
				}
				context.ResolvedReasoningTier = mappingModel.ReasoningTier
			} else if profileTier != "" {
				context.ResolvedReasoningTier = profileTier
			} else {
				context.ResolvedReasoningTier = mappingModel.ReasoningTier
			}
			context.RequestedClientModel = clientModel
			context.UpstreamModel = mappingModel.UpstreamModel
			return nil
		}
		// No profile match. Fall through to ModelMap.Resolve.
		mappingModel, err := h.cfg.Mapping.ModelMap.Resolve(clientModel)
		if err != nil {
			return err
		}
		context.RequestedClientModel = mappingModel.ClientResponseModel
		context.UpstreamModel = mappingModel.UpstreamModel
		context.ResolvedReasoningTier = mappingModel.ReasoningTier
		return nil
	}

	switch mapping.ClientProtocol {
	case ClientResponses:
		result, echo, err := DecodeResponsesRequest(body, policy)
		if err != nil {
			return nil, nil, err
		}
		// The request body's stream field, when explicitly present, is
		// authoritative over the Accept header: an
		// explicit stream:false is never merged into streaming.
		if result.StreamSet {
			context.StreamIntent = result.Request.Stream
		}
		// Write the resolved intent back into the canonical request so the
		// upstream renderer emits the stream value matching the negotiated
		// mode: an Accept-only stream request must not
		// ask the upstream for JSON while the handler expects SSE.
		result.Request.Stream = context.StreamIntent
		if err := resolveModel(result.Request.ClientModel, &result.Report); err != nil {
			return nil, nil, err
		}
		context.OriginalResponsesRequest = echo
		context.ToolNames = result.ToolNames
		result.Request.ClientModel = context.UpstreamModel

		// Opt-in continuity (statefulness decision: OFF by default — a nil
		// store skips this entirely and previous_response_id keeps its
		// existing observable loss at render). When the store is set and
		// the request carries a previous_response_id, resolve the retained
		// conversation and prepend it ahead of the request's own turns; a
		// miss (unknown, expired, or foreign id) falls through to the
		// existing loss, never a failure.
		if h.cfg.Continuity != nil && echo.PreviousResponseID != nil &&
			*echo.PreviousResponseID != "" {
			if prior, depth, ok := resolveContinuityChain(
				h.cfg.Continuity, h.cfg.ContinuityKey, *echo.PreviousResponseID,
			); ok {
				merged := make([]CanonicalTurn, 0, len(prior)+len(result.Request.Turns))
				merged = append(merged, prior...)
				merged = append(merged, result.Request.Turns...)
				result.Request.Turns = merged
				// Record how much of the prefix is proxy-reconstructed so the
				// render's request-side gates judge only the turns this
				// request actually authored.
				result.Request.RetainedTurns = len(prior)
				context.RequestDepth = depth
				if err := continuityNote(&result.Report, depth, *echo.PreviousResponseID); err != nil {
					return nil, nil, err
				}
			}
		}
		// The retained chain must include the reconstructed history, so the
		// record call in convertResponse stores the post-merge turns.
		context.RequestTurns = result.Request.Turns

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
		h.logConversionReport(report, r, "request")
		return rendered, context, nil

	case ClientMessages:
		result, err := DecodeMessagesRequest(body, policy)
		if err != nil {
			return nil, nil, err
		}
		// The request body's stream field, when explicitly present, is
		// authoritative over the Accept header: an
		// explicit stream:false is never merged into streaming.
		if result.StreamSet {
			context.StreamIntent = result.Request.Stream
		}
		// Write the resolved intent back into the canonical request so the
		// upstream renderer emits the stream value matching the negotiated
		// mode: an Accept-only stream request must not
		// ask the upstream for JSON while the handler expects SSE.
		result.Request.Stream = context.StreamIntent
		if err := resolveModel(result.Request.ClientModel, &result.Report); err != nil {
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
		h.logConversionReport(report, r, "request")
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
	// sent is forwarded unless it is on the list.
	// Cookies, forwarding headers, range/conditional controls, idempotency
	// keys, source-provider controls, and unknown credential or extension
	// headers never reach the target provider; hop-by-hop and
	// transformed-representation metadata are structurally absent.
	headers := http.Header{}
	// The outbound Accept is rewritten to the representation actually
	// requested upstream: text/event-stream when the exchange streams,
	// otherwise application/json. The client's Accept describes its own
	// preferences, not the mode of the converted request, and must never
	// reach the target provider unnormalized.
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

	// Extract the inbound credential from the CLIENT's original headers when
	// the policy requires inbound credentials, then strip and re-apply per policy.
	var inbound Credential
	if h.cfg.Mapping.Auth.Inbound && h.cfg.Mapping.Auth.Mode != AuthNone {
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
// loss-policy rejections, and target-render failures stay local.

var errRequestBodyTooLarge = errors.New("request body too large")

// errDecodedRequestTooLarge marks a decoded/rendered request that amplified
// beyond the decoded-request body limit. It renders as 413 RequestEntityTooLarge
// in the client dialect, not the generic conversion 400.

var errDecodedRequestTooLarge = errors.New("decoded request exceeds the decoded-request body limit")

// errEchoTooLarge marks a Responses request whose echo exceeds the echo
// bound (checkEchoSize): the echo renders into every response envelope at up
// to 6x JSON escaping, so it is bounded as a fail-closed resource limit. It
// renders as 413 like the decoded-request bound.

var errEchoTooLarge = errors.New("responses request echo exceeds the echo body limit")
