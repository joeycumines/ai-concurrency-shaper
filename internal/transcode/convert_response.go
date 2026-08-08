package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Response-direction conversions. Each upstream response decodes exactly once
// into the canonical IR and the client response renders directly from it.
// Chat responses are constrained to a single choice (n=1) both at request
// render time and at decode time.

// DecodeChatResponse decodes a non-streaming Chat Completions response into
// the canonical IR. A response with more than one choice is rejected rather
// than silently taking choice zero. Provider plaintext reasoning is handled
// only when ChatCapabilities.ProviderReasoningText is enabled.
func DecodeChatResponse(
	body []byte,
	capabilities ChatCapabilities,
) (CanonicalResponse, error) {
	var wire ChatResponse
	if err := strictDecode(body, &wire); err != nil {
		return CanonicalResponse{}, fmt.Errorf("chat response: %w", err)
	}
	if len(wire.Choices) == 0 {
		return CanonicalResponse{}, errors.New(
			"chat response has no choices",
		)
	}
	if len(wire.Choices) > 1 {
		return CanonicalResponse{}, errors.New(
			"chat response has more than one choice; the transcoder requires n=1",
		)
	}

	choice := wire.Choices[0]
	message := choice.Message
	if message == nil {
		return CanonicalResponse{}, errors.New("chat response choice has no message")
	}
	if err := message.Validate(); err != nil {
		return CanonicalResponse{}, fmt.Errorf("chat response message: %w", err)
	}

	response := CanonicalResponse{
		ID:        wire.ID,
		Model:     wire.Model,
		CreatedAt: wire.Created,
		Status:    CanonicalResponseCompleted,
	}
	if wire.ServiceTier != nil {
		response.ChatServiceTier = *wire.ServiceTier
	}
	if choice.LogProbs != nil {
		response.ChatLogProbs = true
	}
	if wire.Usage != nil {
		response.Usage = CanonicalUsage{
			InputTokens:     int64(wire.Usage.PromptTokens),
			OutputTokens:    int64(wire.Usage.CompletionTokens),
			InputKnown:      true,
			OutputKnown:     true,
			CacheReadKnown:  wire.Usage.PromptTokensDetails != nil,
			ReasoningKnown:  wire.Usage.CompletionTokensDetails != nil,
			CacheWriteKnown: false, // cache-write tokens are not part of the pinned Chat contract
		}
		if wire.Usage.PromptTokensDetails != nil {
			response.Usage.CacheReadTokens = int64(wire.Usage.PromptTokensDetails.CachedTokens)
		}
		if wire.Usage.CompletionTokensDetails != nil {
			response.Usage.ReasoningTokens = int64(wire.Usage.CompletionTokensDetails.ReasoningTokens)
		}
	}

	switch derefStr(choice.FinishReason) {
	case "", "stop":
		response.StopReason = CanonicalStopEndTurn
	case "length":
		response.StopReason = CanonicalStopMaxTokens
		response.Status = CanonicalResponseIncomplete
		response.IncompleteReason = "max_output_tokens"
	case "tool_calls", "function_call":
		response.StopReason = CanonicalStopToolUse
	case "content_filter":
		// The official Responses contract represents a filtered response as
		// status incomplete with reason content_filter; the Anthropic
		// dialect renders the refusal stop reason.
		response.StopReason = CanonicalStopRefusal
		response.Status = CanonicalResponseIncomplete
		response.IncompleteReason = "content_filter"
	default:
		return CanonicalResponse{}, &UnsupportedFeatureError{
			Protocol: "chat",
			Path:     "choices[].finish_reason",
			Feature:  *choice.FinishReason,
		}
	}

	parts, err := chatMessageToCanonicalParts(message, capabilities)
	if err != nil {
		return CanonicalResponse{}, err
	}
	response.Turns = []CanonicalTurn{{
		Role:  CanonicalAssistant,
		Parts: parts,
	}}
	if err := ValidateCanonicalResponse(response); err != nil {
		return CanonicalResponse{}, err
	}
	return response, nil
}

// chatMessageToCanonicalParts converts a Chat assistant message into canonical
// parts: text content, refusal, tool calls, and (when the capability is
// enabled) provider plaintext reasoning.
func chatMessageToCanonicalParts(
	message *ChatMessage,
	capabilities ChatCapabilities,
) ([]CanonicalPart, error) {
	var parts []CanonicalPart

	if message.Content != nil {
		switch {
		case message.Content.ContentStr != nil:
			parts = append(parts, CanonicalText{Text: *message.Content.ContentStr})
		case message.Content.ContentBlocks != nil:
			for i, block := range message.Content.ContentBlocks {
				switch block.Type {
				case ChatContentBlockTypeText:
					if block.Text != nil {
						parts = append(parts, CanonicalText{Text: *block.Text})
					}
				case ChatContentBlockTypeImage:
					return nil, &UnsupportedFeatureError{
						Protocol: "chat",
						Path:     "choices[].message.content[].type",
						Feature:  "image_url",
					}
				default:
					return nil, fmt.Errorf(
						"chat message content block %d: unknown type %q",
						i,
						block.Type,
					)
				}
			}
		}
	}

	if message.ChatAssistantMessage != nil {
		if message.Refusal != nil {
			parts = append(parts, CanonicalRefusal{Text: *message.Refusal})
		}

		if message.Reasoning != nil {
			if !capabilities.ProviderReasoningText {
				return nil, &UnsupportedFeatureError{
					Protocol: "chat",
					Path:     "choices[].message.reasoning",
					Feature:  "provider reasoning text",
				}
			}
			// Provider plaintext reasoning is mapped to ordinary text only.
			parts = append(parts, CanonicalText{Text: *message.Reasoning})
		}

		for i, call := range message.ToolCalls {
			if call.ID == nil || *call.ID == "" {
				return nil, fmt.Errorf("chat tool call %d has no id", i)
			}
			name := ""
			if call.Function.Name != nil {
				name = *call.Function.Name
			}
			arguments := call.Function.Arguments
			if strings.TrimSpace(arguments) == "" {
				// Empty arguments are valid on the wire and represent the
				// empty object (the Responses form of an empty argument set).
				arguments = "{}"
			}
			decoded, err := decodeJSONObject(arguments)
			if err != nil {
				return nil, fmt.Errorf("chat tool call %d arguments: %w", i, err)
			}
			parts = append(parts, CanonicalFunctionCall{
				CallID:    *call.ID,
				Name:      name,
				Arguments: mustRawMessage(decoded),
			})
		}
	}

	return parts, nil
}

// DecodeResponsesResponse decodes a non-streaming Responses response into the
// canonical IR. Reasoning output items are carried as source artifacts; their
// loss or rejection is decided at render time against the target protocol.
func DecodeResponsesResponse(
	body []byte,
) (CanonicalResponse, error) {
	var envelope ResponseEnvelope
	if err := strictDecode(body, &envelope); err != nil {
		return CanonicalResponse{}, fmt.Errorf("responses response: %w", err)
	}

	response := CanonicalResponse{
		ID:        envelope.ID,
		Model:     envelope.Model,
		CreatedAt: envelope.CreatedAt,
	}
	switch envelope.Status {
	case "completed":
		response.Status = CanonicalResponseCompleted
	case "incomplete":
		response.Status = CanonicalResponseIncomplete
		if envelope.IncompleteDetails != nil {
			response.IncompleteReason = envelope.IncompleteDetails.Reason
		}
		// Match the streaming path: content_filter renders a refusal stop
		// reason, anything else max_tokens.
		if response.IncompleteReason == "content_filter" {
			response.StopReason = CanonicalStopRefusal
		} else {
			response.StopReason = CanonicalStopMaxTokens
		}
	case "failed":
		response.Status = CanonicalResponseFailed
		if envelope.Error != nil {
			response.ErrorMessage = envelope.Error.Message
		}
	default:
		return CanonicalResponse{}, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     "status",
			Feature:  envelope.Status,
		}
	}

	if envelope.Usage != nil {
		response.Usage = CanonicalUsage{
			InputTokens:     envelope.Usage.InputTokens,
			OutputTokens:    envelope.Usage.OutputTokens,
			InputKnown:      true,
			OutputKnown:     true,
			CacheReadKnown:  envelope.Usage.InputTokensDetails != nil,
			ReasoningKnown:  envelope.Usage.OutputTokensDetails != nil,
			CacheWriteKnown: false, // cache-write tokens are not part of the pinned Responses contract
		}
		if envelope.Usage.InputTokensDetails != nil {
			response.Usage.CacheReadTokens = envelope.Usage.InputTokensDetails.CachedTokens
		}
		if envelope.Usage.OutputTokensDetails != nil {
			response.Usage.ReasoningTokens = envelope.Usage.OutputTokensDetails.ReasoningTokens
		}
	}

	var turns []CanonicalTurn
	var assistantParts []CanonicalPart
	var resultParts []CanonicalPart
	sawToolUse := false

	// flushAssistantTurn emits the accumulated assistant parts as one turn.
	flushAssistantTurn := func() {
		if len(assistantParts) > 0 {
			turns = append(turns, CanonicalTurn{
				Role:  CanonicalAssistant,
				Parts: assistantParts,
			})
			assistantParts = nil
		}
	}
	// flushResultTurn emits the accumulated function results as one user turn.
	flushResultTurn := func() {
		if len(resultParts) > 0 {
			turns = append(turns, CanonicalTurn{
				Role:  CanonicalUser,
				Parts: resultParts,
			})
			resultParts = nil
		}
	}

	for i, item := range envelope.Output {
		switch value := item.(type) {
		case *ResponsesOutputMessage:
			flushResultTurn()
			if value.Phase != "" {
				response.ResponsesPhase = true
			}
			for _, content := range value.Content {
				switch part := content.(type) {
				case *ResponsesOutputText:
					assistantParts = append(assistantParts, CanonicalText{Text: part.Text})
				case *ResponsesOutputRefusal:
					assistantParts = append(assistantParts, CanonicalRefusal{Text: part.Refusal})
				default:
					return CanonicalResponse{}, fmt.Errorf(
						"output item %d: unknown content part %T",
						i,
						content,
					)
				}
			}

		case *ResponsesFunctionCallOutputItem:
			flushResultTurn()
			arguments, err := decodeJSONObject(value.Arguments)
			if err != nil {
				return CanonicalResponse{}, fmt.Errorf(
					"output item %d function call arguments: %w",
					i,
					err,
				)
			}
			assistantParts = append(assistantParts, CanonicalFunctionCall{
				CallID:    value.CallID,
				Name:      value.Name,
				Arguments: mustRawMessage(arguments),
			})
			sawToolUse = true

		case *ResponsesFunctionCallOutputResultItem:
			flushAssistantTurn()
			outputParts, err := responsesFunctionOutputToCanonical(value.Output)
			if err != nil {
				return CanonicalResponse{}, fmt.Errorf(
					"output item %d function call output: %w",
					i,
					err,
				)
			}
			resultParts = append(resultParts, CanonicalFunctionResult{
				CallID:  value.CallID,
				IsError: false,
				Parts:   outputParts,
			})

		case *ResponsesReasoningOutputItem:
			flushResultTurn()
			raw, err := json.Marshal(value)
			if err != nil {
				return CanonicalResponse{}, fmt.Errorf("output item %d: %w", i, err)
			}
			response.ReasoningItems = append(
				response.ReasoningItems,
				raw,
			)

		default:
			return CanonicalResponse{}, fmt.Errorf(
				"output item %d: unknown item type %T",
				i,
				item,
			)
		}
	}
	flushAssistantTurn()
	flushResultTurn()

	if len(turns) > 0 {
		response.Turns = turns
	}

	switch response.Status {
	case CanonicalResponseCompleted:
		if sawToolUse {
			response.StopReason = CanonicalStopToolUse
		} else {
			response.StopReason = CanonicalStopEndTurn
		}
	case CanonicalResponseIncomplete:
		// The decode already recorded the reason-specific stop reason
		// (content_filter -> refusal, anything else max_tokens); keep it.
		if response.StopReason == "" {
			response.StopReason = CanonicalStopMaxTokens
		}
	case CanonicalResponseFailed:
		response.StopReason = CanonicalStopEndTurn
	}

	if err := ValidateCanonicalResponse(response); err != nil {
		return CanonicalResponse{}, err
	}
	return response, nil
}

// RenderResponsesResponse renders the canonical response into a Responses
// response envelope, reconstructed from the request echo. The client-facing
// model alias is returned; the actual upstream model is never leaked. The
// returned report carries every approved loss and named encoding of the
// conversion (review-j finding 10).
func RenderResponsesResponse(
	response CanonicalResponse,
	context *ExchangeContext,
) ([]byte, ConversionReport, error) {
	if err := ValidateCanonicalResponse(response); err != nil {
		return nil, ConversionReport{}, err
	}
	if context == nil || context.IDs == nil {
		return nil, ConversionReport{}, errors.New("render responses response requires an exchange context")
	}
	// Chat response attributes the Responses envelope cannot reproduce
	// (token log-probabilities and the tier actually served) are a loss or a
	// rejection per the exchange policy — never a silent drop (review-j
	// finding 4).
	var report ConversionReport
	if response.ChatLogProbs {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureLogprobs,
			"choices[].logprobs",
			"chat response logprobs cannot be reproduced in a Responses response",
		); err != nil {
			return nil, report, err
		}
	}
	if response.ChatServiceTier != "" {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureServiceTier,
			"service_tier",
			"the upstream chat service tier cannot be reproduced in a Responses response",
		); err != nil {
			return nil, report, err
		}
	}

	envelope := ResponseEnvelope{
		ID:        context.IDs.New("resp_"),
		Object:    "response",
		CreatedAt: response.CreatedAt,
		Status:    string(response.Status),
		Model:     requestedClientModelAlias(response, context),
		Output:    []ResponsesOutputItem{},
	}

	// Output items from the canonical turns.
	for _, turn := range response.Turns {
		if turn.Role != CanonicalAssistant {
			return nil, report, fmt.Errorf(
				"response turn role %q is not assistant",
				turn.Role,
			)
		}
		message := &ResponsesOutputMessage{
			ID:      context.IDs.New("msg_"),
			Type:    "message",
			Role:    "assistant",
			Status:  ResponsesItemCompleted,
			Content: ResponsesOutputContentParts{},
		}
		for _, part := range turn.Parts {
			switch value := part.(type) {
			case CanonicalText:
				message.Content = append(message.Content, &ResponsesOutputText{
					Type:        "output_text",
					Text:        value.Text,
					Annotations: []ResponsesAnnotation{},
				})
			case CanonicalRefusal:
				message.Content = append(message.Content, &ResponsesOutputRefusal{
					Type:    "refusal",
					Refusal: value.Text,
				})
			case CanonicalFunctionCall:
				// Only emit the message when it actually carries content; a
				// turn that starts with a tool call must not produce a
				// phantom empty message item before it.
				if len(message.Content) > 0 {
					envelope.Output = append(envelope.Output, message)
				}
				message = nil
				envelope.Output = append(envelope.Output, &ResponsesFunctionCallOutputItem{
					ID:        context.IDs.New("fc_"),
					Type:      "function_call",
					Status:    ResponsesItemCompleted,
					CallID:    value.CallID,
					Name:      value.Name,
					Arguments: string(value.Arguments),
				})
				message = &ResponsesOutputMessage{
					ID:      context.IDs.New("msg_"),
					Type:    "message",
					Role:    "assistant",
					Status:  ResponsesItemCompleted,
					Content: ResponsesOutputContentParts{},
				}
			case CanonicalFunctionResult:
				return nil, report, &UnsupportedFeatureError{
					Protocol: "responses",
					Path:     "output[]",
					Feature:  "function result in model output",
				}
			default:
				return nil, report, fmt.Errorf(
					"response turn: unknown canonical part %T",
					part,
				)
			}
		}
		if message != nil && len(message.Content) > 0 {
			envelope.Output = append(envelope.Output, message)
		}
	}

	// Usage.
	// Usage is emitted only when the source provided it: unknown usage is
	// never fabricated as zero facts (review-j finding 9). The cached and
	// reasoning breakdowns are mapped from the canonical fields.
	if !response.Usage.Unknown() {
		envelope.Usage = &ResponsesUsage{
			InputTokens:  response.Usage.InputTokens,
			OutputTokens: response.Usage.OutputTokens,
			TotalTokens:  response.Usage.InputTokens + response.Usage.OutputTokens,
			InputTokensDetails: &UsageInputTokensDetails{
				CachedTokens: response.Usage.CacheReadTokens,
			},
			OutputTokensDetails: &UsageOutputTokensDetails{
				ReasoningTokens: response.Usage.ReasoningTokens,
			},
		}
	}

	// Failed responses carry an error object.
	if response.Status == CanonicalResponseFailed {
		envelope.Error = &ResponsesEnvelopeError{
			Message: response.ErrorMessage,
		}
	}
	if response.Status == CanonicalResponseIncomplete {
		envelope.IncompleteDetails = &ResponsesIncompleteDetails{
			Reason: response.IncompleteReason,
		}
	}

	// Request echo.
	if echo := context.OriginalResponsesRequest; echo != nil {
		envelope.Instructions = echo.Instructions
		if echo.MaxOutputTokens != nil {
			value := int64(*echo.MaxOutputTokens)
			envelope.MaxOutputTokens = &value
		}
		envelope.ParallelToolCalls = echo.ParallelToolCalls
		envelope.PreviousResponseID = echo.PreviousResponseID
		envelope.Store = echo.Store
		envelope.Temperature = echo.Temperature
		envelope.TopP = echo.TopP
		envelope.Truncation = echo.Truncation
		envelope.User = echo.User
		envelope.Metadata = echo.Metadata
		envelope.Tools = echo.Tools
		envelope.ToolChoice = echo.ToolChoice
		envelope.Reasoning = echo.Reasoning
		envelope.Text = echo.Text
		envelope.ServiceTier = echo.ServiceTier
		envelope.TopLogprobs = echo.TopLogprobs
	}

	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, report, err
	}
	return body, report, nil
}

// requestedClientModelAlias returns the stable client-facing model alias for
// the converted response: the requested client model when the context carries
// no resolution, otherwise the context's client model (which the handler sets
// from the mapping's ClientResponseModel).
func requestedClientModelAlias(response CanonicalResponse, context *ExchangeContext) string {
	if context.RequestedClientModel != "" {
		return context.RequestedClientModel
	}
	return response.Model
}

// RenderMessagesResponse renders the canonical response into an Anthropic
// Messages response. Refusal becomes ordinary text content with a refusal
// stop reason; function calls become tool_use blocks. Responses reasoning
// items cannot cross into Messages and are an approved loss or a rejection
// per the exchange loss policy.
func RenderMessagesResponse(
	response CanonicalResponse,
	context *ExchangeContext,
) ([]byte, ConversionReport, error) {
	var report ConversionReport
	if err := ValidateCanonicalResponse(response); err != nil {
		return nil, report, err
	}
	// A failed exchange must never be reported as a successful Messages
	// completion (merge gate 10). The upstream failure surfaces as a
	// client-dialect error, never as a message with a success stop reason.
	if response.Status == CanonicalResponseFailed {
		// A 2xx envelope reporting status "failed" is an upstream semantic
		// failure: the typed error classifies the exchange as an upstream
		// failure with the upstream HTTP status, matching the streamed
		// response.failed classification (review-j finding 11).
		return nil, report, &UpstreamSemanticFailureError{
			Message: response.ErrorMessage,
		}
	}
	if context == nil || context.IDs == nil {
		return nil, report, errors.New("render messages response requires an exchange context")
	}
	// Chat response attributes the Messages response cannot reproduce
	// (token log-probabilities and the tier actually served) are a loss or a
	// rejection per the exchange policy — never a silent drop (review-j
	// finding 4).
	if response.ChatLogProbs {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureLogprobs,
			"choices[].logprobs",
			"chat response logprobs cannot be reproduced in a Messages response",
		); err != nil {
			return nil, report, err
		}
	}
	if response.ChatServiceTier != "" {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureServiceTier,
			"service_tier",
			"the upstream chat service tier cannot be reproduced in a Messages response",
		); err != nil {
			return nil, report, err
		}
	}
	// A Responses output-message phase (commentary vs final_answer) has no
	// Messages representation (review-j finding 10).
	if response.ResponsesPhase {
		if err := report.Lose(
			context.lossPolicy(),
			FeaturePhase,
			"output[].phase",
			"the output message phase cannot be reproduced in a Messages response",
		); err != nil {
			return nil, report, err
		}
	}
	if len(response.ReasoningItems) > 0 {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureReasoningSummary,
			"output[].reasoning",
			"Responses reasoning output cannot be reproduced in a Messages response",
		); err != nil {
			return nil, report, err
		}
	}

	out := AnthropicMessageResponse{
		ID:      context.IDs.New("msg_"),
		Type:    "message",
		Role:    "assistant",
		Model:   requestedClientModelAlias(response, context),
		Content: []AnthropicContentBlock{},
	}

	for _, turn := range response.Turns {
		switch turn.Role {
		case CanonicalAssistant:
			for _, part := range turn.Parts {
				switch value := part.(type) {
				case CanonicalText:
					text := value.Text
					out.Content = append(out.Content, AnthropicContentBlock{
						Type: AnthropicContentBlockTypeText,
						Text: &text,
					})
				case CanonicalRefusal:
					text := value.Text
					out.Content = append(out.Content, AnthropicContentBlock{
						Type: AnthropicContentBlockTypeText,
						Text: &text,
					})
				case CanonicalFunctionCall:
					callID := value.CallID
					name := value.Name
					out.Content = append(out.Content, AnthropicContentBlock{
						Type:  AnthropicContentBlockTypeToolUse,
						ID:    &callID,
						Name:  &name,
						Input: value.Arguments,
					})
				case CanonicalFunctionResult:
					return nil, report, &UnsupportedFeatureError{
						Protocol: "messages",
						Path:     "content[]",
						Feature:  "function result in assistant output",
					}
				default:
					return nil, report, fmt.Errorf(
						"response turn: unknown canonical part %T",
						part,
					)
				}
			}

		case CanonicalUser:
			// Conversation-state echoes (tool results in the upstream output)
			// belong to the next request, not the current Messages response.
			// They are an approved loss or a rejection per the exchange policy.
			if err := report.Lose(
				context.lossPolicy(),
				FeatureConversationState,
				"output[].function_call_output",
				"tool results are conversation state, not part of the model response",
			); err != nil {
				return nil, report, err
			}

		default:
			return nil, report, fmt.Errorf(
				"response turn role %q is not assistant or user",
				turn.Role,
			)
		}
	}

	// stop_reason and stop_sequence are always present on the wire: the
	// completed response carries the real values (stop_sequence is null
	// unless a custom sequence was used).
	switch response.StopReason {
	case CanonicalStopEndTurn:
		out.StopReason = anthropicStopReasonPtr(AnthropicStopReasonEndTurn)
	case CanonicalStopMaxTokens:
		out.StopReason = anthropicStopReasonPtr(AnthropicStopReasonMaxTokens)
	case CanonicalStopStopSequence:
		out.StopReason = anthropicStopReasonPtr(AnthropicStopReasonStopSequence)
		out.StopSequence = &response.StopSequence
	case CanonicalStopToolUse:
		out.StopReason = anthropicStopReasonPtr(AnthropicStopReasonToolUse)
	case CanonicalStopRefusal:
		out.StopReason = anthropicStopReasonPtr(AnthropicStopReasonRefusal)
		out.StopDetails = &AnthropicStopDetails{Type: "refusal"}
	default:
		out.StopReason = anthropicStopReasonPtr(AnthropicStopReasonEndTurn)
	}

	// Anthropic usage semantics: input_tokens + cache_creation_input_tokens
	// + cache_read_input_tokens = total. The uncached input is the total
	// minus the cached breakdown, with checked nonnegative arithmetic
	// (review-j finding 9). Unknown usage is never fabricated as zero facts:
	// it is an explicit loss/reject decision.
	if response.Usage.Unknown() {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureUsageTiming,
			"usage",
			"the upstream response did not provide token usage; the required Messages usage cannot be reproduced",
		); err != nil {
			return nil, report, err
		}
		// The Messages wire contract requires the usage object; under an
		// approved loss the zeros are a documented approximation, never a
		// silent fabrication.
		out.Usage = &AnthropicUsage{}
	} else {
		inputTokens := response.Usage.InputTokens
		cached := response.Usage.CacheReadTokens + response.Usage.CacheWriteTokens
		if inputTokens < 0 || cached < 0 || inputTokens-cached < 0 {
			return nil, report, errors.New(
				"source usage is arithmetically inconsistent: nonnegative token counts required and cached tokens must not exceed the input total",
			)
		}
		// output_tokens_details is required on the wire for known usage; the
		// thinking breakdown is zero when the source did not provide it,
		// matching the stream path.
		out.Usage = &AnthropicUsage{
			InputTokens:              int(inputTokens - cached),
			CacheCreationInputTokens: int(response.Usage.CacheWriteTokens),
			CacheReadInputTokens:     int(response.Usage.CacheReadTokens),
			OutputTokens:             int(response.Usage.OutputTokens),
			OutputTokensDetails: &AnthropicOutputTokensDetails{
				ThinkingTokens: int(response.Usage.ReasoningTokens),
			},
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, report, err
	}
	return body, report, nil
}
