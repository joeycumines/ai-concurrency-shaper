package transcode

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
)

// readRequestBody reads and bounds the client request body.
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
		StreamIntent:         AcceptIsEventStream(r.Header.Get("Accept")),
		GeminiSignatures:     h.cfg.GeminiSignatures,
		GeminiSignatureScope: h.cfg.GeminiSignatureScope,
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
			context.ModelMaxOutputTokens = mappingModel.MaxOutput
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
		context.ModelMaxOutputTokens = mappingModel.MaxOutput
		return nil
	}

	// Gemini-upstream routes never forward client query parameters: the
	// Gemini API rejects unknown parameters, so an allowlisted one is an
	// approved loss and any other is a client fault. Computed once here and
	// merged into each branch's request-stage report, so the exchange still
	// logs at most one request-side line.
	queryReport, err := h.geminiClientQueryLosses(r)
	if err != nil {
		return nil, nil, err
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
		case UpstreamGemini:
			rendered, report, err = RenderGeminiRequest(result.Request, context)
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
		report.Losses = append(report.Losses, queryReport.Losses...)
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
		case UpstreamGemini:
			rendered, report, err = RenderGeminiRequest(result.Request, context)
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
		report.Losses = append(report.Losses, queryReport.Losses...)
		h.logConversionReport(report, r, "request")
		return rendered, context, nil

	case ClientGemini:
		match, ok := MatchGeminiRequestPath(r.Method, r.URL.Path)
		if !ok {
			return nil, nil, errors.New("request path does not match a Gemini generateContent route")
		}
		if err := geminiClientQuery(match, r.URL.RawQuery); err != nil {
			return nil, nil, err
		}
		result, facts, err := DecodeGeminiRequest(body, policy, mapping.UpstreamProtocol)
		if err != nil {
			return nil, nil, err
		}
		if err := geminiBodyModel(facts, match.Model, &result.Report); err != nil {
			return nil, nil, err
		}
		// The route alone selects the representation: streamGenerateContent
		// streams and generateContent returns JSON, whatever the Accept header
		// says.
		stream := match.Operation == GeminiStream
		context.StreamIntent = stream
		result.Request.Stream = stream
		if err := resolveModel(match.Model, &result.Report); err != nil {
			return nil, nil, err
		}
		context.GeminiIncludeThoughts = facts.IncludeThoughts
		context.RequestTurns = result.Request.Turns
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
		case UpstreamMessages:
			rendered, report, err = RenderMessagesRequest(result.Request, context)
		default:
			return nil, nil, fmt.Errorf(
				"unsupported upstream protocol %q for gemini client",
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
		report.Losses = append(report.Losses, queryReport.Losses...)
		h.logConversionReport(report, r, "request")
		return rendered, context, nil

	default:
		return nil, nil, fmt.Errorf("unknown client protocol %q", mapping.ClientProtocol)
	}
}

// sortedQueryNames returns the query parameter names in sorted order, so the
// per-parameter loss report and the unallowed-parameter rejection are
// deterministic regardless of map iteration order.
func sortedQueryNames(query url.Values) []string {
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// geminiClientQueryLosses applies the client-query rules of a Gemini-bound
// route: the Gemini GenerateContent API rejects unknown query parameters, so
// client query parameters are never forwarded. A parameter the allowlist
// accepted is recorded as an approved loss at path ?<name>; an unallowed one
// is a client fault (errClientQueryParameter), exactly like BuildMappedURL.
func (h *TranscodeHandler) geminiClientQueryLosses(r *http.Request) (ConversionReport, error) {
	var report ConversionReport
	if h.cfg.Mapping.UpstreamProtocol != UpstreamGemini {
		return report, nil
	}
	clientQuery := upstreamClientQuery(r, h.cfg.Mapping.ClientProtocol)
	for _, name := range sortedQueryNames(clientQuery) {
		if _, allowed := h.cfg.Mapping.AllowedClientQuery[name]; allowed {
			if err := report.Lose(
				h.cfg.Mapping.LossPolicy,
				FeatureGeminiClientQuery,
				"?"+name,
				"the Gemini GenerateContent API rejects unknown query parameters, so this allowed client query parameter is not forwarded",
			); err != nil {
				return report, err
			}
		} else {
			return report, fmt.Errorf(
				"%w %q is not allowed on transcoded routes",
				errClientQueryParameter,
				name,
			)
		}
	}
	return report, nil
}

// buildUpstreamRequest builds the outbound request: hop-by-hop sanitization,
// source auth stripping, target auth application, recomputed Content-Length,
// the outbound Accept rewritten to the negotiated stream mode, and the client
// context carried so downstream cancellation aborts upstream. The exchange
// context selects the URL shape (a Gemini upstream carries the model and
// operation in the path) and receives the dropped-client-query loss report.
func (h *TranscodeHandler) buildUpstreamRequest(
	r *http.Request,
	body []byte,
	context *ExchangeContext,
) (*http.Request, error) {
	var targetURL *url.URL
	if h.cfg.Mapping.UpstreamProtocol == UpstreamGemini {
		// The Gemini GenerateContent API carries the model and operation in
		// the path; client query parameters were already validated and
		// recorded by geminiClientQueryLosses in convertRequest.
		var err error
		targetURL, err = BuildGeminiURL(h.cfg.Upstream, h.cfg.Mapping.UpstreamPath, context.UpstreamModel, context.StreamIntent)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		targetURL, err = BuildMappedURL(
			h.cfg.Upstream,
			h.cfg.Mapping.UpstreamPath,
			upstreamClientQuery(r, h.cfg.Mapping.ClientProtocol),
			h.cfg.Mapping.AllowedClientQuery,
		)
		if err != nil {
			return nil, err
		}
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
	if context.StreamIntent {
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

// checkDecodedRequestSize rejects decoded requests that amplify beyond the
// decoded-request body limit (the decoded limit is separate
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
