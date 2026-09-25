package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"strings"
)

// Response-direction conversions. Each upstream response decodes exactly once
// into the canonical IR and the client response renders directly from it.
// Chat responses are constrained to a single choice (n=1) both at request
// render time and at decode time.

// chatResponseShadow is the presence-aware decode shadow of ChatResponse:
// every presence-sensitive field is a pointer so absent-vs-zero is
// distinguishable, while the full surface is modeled (reusing the wire types
// for non-presence-sensitive payloads) so the tolerant decode's modeled
// surface covers the full pinned contract. The shadow enforces the pinned
// required fields of the Chat response contract and its
// usage presence is consumed by the usage Known-flag decode.
type chatResponseShadow struct {
	ID                string             `json:"id"`
	Object            *string            `json:"object"`
	Created           int64              `json:"created"`
	Model             *string            `json:"model"`
	ServiceTier       *string            `json:"service_tier,omitempty"`
	SystemFingerprint string             `json:"system_fingerprint,omitempty"`
	Choices           []chatChoiceShadow `json:"choices"`
	Usage             *chatUsageShadow   `json:"usage,omitempty"`

	// Opaque provider-extension fields present on real chat responses
	// (e.g. the yolo gateway's prompt_token_ids/prompt_text): decoded so
	// strict wire decoding never fails on a current provider; never
	// forwarded.
	PromptTokenIDs any     `json:"prompt_token_ids,omitempty"`
	PromptText     *string `json:"prompt_text,omitempty"`

	// CacheCost is an opaque provider extension (the Verboo gateway's
	// billing field). Modeled as an opaque provider extension; captured as
	// raw JSON and never forwarded (the envelope decode is tolerant regardless).
	CacheCost json.RawMessage `json:"cache_cost,omitempty"`

	// CompletionCost is an opaque provider extension (the LiteLLM gateway's
	// cost-accounting field). Modeled as an opaque provider extension; captured
	// as raw JSON and never forwarded (the envelope decode is tolerant regardless).
	CompletionCost json.RawMessage `json:"completion_cost,omitempty"`
}

type chatChoiceShadow struct {
	Index        *int64              `json:"index"`
	FinishReason *string             `json:"finish_reason"`
	LogProbs     *ChatChoiceLogprobs `json:"logprobs"`
	Message      *chatMessageShadow  `json:"message"`
	Delta        *ChatStreamDelta    `json:"delta,omitempty"`

	TokenIDs      any     `json:"token_ids,omitempty"`
	RoutedExperts any     `json:"routed_experts,omitempty"`
	StopReason    *string `json:"stop_reason,omitempty"`
	MatchedStop   any     `json:"matched_stop,omitempty"`
}

// chatMessageShadow mirrors ChatMessage with a pointer role so an absent
// role is distinguishable from a present one.
type chatMessageShadow struct {
	Name    *string             `json:"name,omitempty"`
	Role    *ChatMessageRole    `json:"role,omitempty"`
	Content *ChatMessageContent `json:"content,omitempty"`

	ToolCallID *string              `json:"tool_call_id,omitempty"`
	Refusal    *string              `json:"refusal,omitempty"`
	ToolCalls  []chatToolCallShadow `json:"tool_calls,omitempty"`
	Reasoning  *string              `json:"reasoning,omitempty"`

	// FunctionCall is the legacy non-tool_calls tool-call spelling (a KNOWN
	// official field, pinned in pins.md). It is modeled so a message carrying
	// it is structurally REJECTED — never silently dropped (a silent drop
	// would leave the client with a tool_use stop reason and no tool call).
	FunctionCall json.RawMessage `json:"function_call,omitempty"`
	// ReasoningContent mirrors the wire ChatAssistantMessage extension (the
	// DeepSeek/Qwen spelling); shadow-mirrors-wire pattern.
	ReasoningContent *string `json:"reasoning_content,omitempty"`

	TokenIDs      any     `json:"token_ids,omitempty"`
	RoutedExperts any     `json:"routed_experts,omitempty"`
	StopReason    *string `json:"stop_reason,omitempty"`
	// MatchedStop mirrors the wire ChatAssistantMessage extension
	// (matched_stop is observed at choice level; the message-level mirror
	// is defensive — the captured error text is level-ambiguous);
	// shadow-mirrors-wire pattern.
	MatchedStop any `json:"matched_stop,omitempty"`
}

// chatToolCallShadow mirrors ChatMessageToolCall with a pointer arguments
// field so a missing arguments field is distinguishable from an empty one.
type chatToolCallShadow struct {
	Type     *string                    `json:"type,omitempty"`
	ID       *string                    `json:"id,omitempty"`
	Function chatToolCallFunctionShadow `json:"function"`
}

type chatToolCallFunctionShadow struct {
	Name      *string `json:"name"`
	Arguments *string `json:"arguments"`
}

// chatUsageShadow mirrors ChatLLMUsage with pointer totals so explicit
// presence is distinguishable from omitted (the wire fields are omitempty;
// the Known flags are decoded from this shadow). The four
// provider-extension pointers mirror the wire LLMUsage extensions exactly
// (shadow-mirrors-wire pattern): the shared struct covers both
// the non-streaming response and the streaming chunk shadows.
type chatUsageShadow struct {
	PromptTokens            *int                         `json:"prompt_tokens,omitempty"`
	CompletionTokens        *int                         `json:"completion_tokens,omitempty"`
	TotalTokens             *int                         `json:"total_tokens,omitempty"`
	ReasoningTokens         *int                         `json:"reasoning_tokens,omitempty"`
	CachedTokens            *int                         `json:"cached_tokens,omitempty"`
	PromptCacheHitTokens    *int                         `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens   *int                         `json:"prompt_cache_miss_tokens,omitempty"`
	PromptTokensDetails     *ChatPromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *ChatCompletionTokensDetails `json:"completion_tokens_details,omitempty"`

	// CacheCost is an opaque provider extension (the Verboo gateway's
	// billing field). Modeled as an opaque provider extension; captured as
	// raw JSON and never forwarded (the envelope decode is tolerant regardless).
	CacheCost json.RawMessage `json:"cache_cost,omitempty"`

	// CompletionCost is an opaque provider extension (the LiteLLM gateway's
	// cost-accounting field). Modeled as an opaque provider extension; captured
	// as raw JSON and never forwarded (the envelope decode is tolerant regardless).
	CompletionCost json.RawMessage `json:"completion_cost,omitempty"`
}

// DecodeChatResponseWithPolicy decodes a non-streaming Chat Completions
// response into the canonical IR, applying the exchange loss policy to the
// provider plaintext reasoning decision. The decode enforces the pinned wire
// contract's semantic presence strictly (the upstream envelope is a
// subject-to-change contract, so unknown provider-extension fields are
// TOLERATED, but): the required fields (object, one choice, choice index 0,
// finish_reason, message with role assistant, and complete tool-call identity)
// must be explicitly present — absent or null is rejected, never defaulted;
// corrupt upstream wire is an upstream failure. Provider
// plaintext reasoning is the one field that instead follows the loss policy:
// mapped to ordinary text with the capability, an approved loss with the
// reasoning dropped when the policy allows losing provider_reasoning_text, or
// an UnsupportedFeatureError otherwise — never a silent drop (stream
// disposition parity).
func DecodeChatResponseWithPolicy(
	body []byte,
	capabilities ChatCapabilities,
	policy LossPolicy,
) (CanonicalResponse, ConversionReport, error) {
	// Presence-aware shadow decode: absent-vs-zero is distinguishable.
	var shadow chatResponseShadow
	if err := wire.DecodeTolerant(body, &shadow); err != nil {
		// A decode failure — malformed JSON or a type-corrupt modeled value —
		// is corrupt upstream wire, an
		// upstream failure. Valid features the
		// transcoder knows but does not support are rejected as
		// UnsupportedFeatureError (local) instead.
		return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
			UpstreamChatCompletions,
			0,
			fmt.Errorf("chat response: %w", err),
		)
	}

	// The pinned Chat response contract (openai-go v1.12.0 chatcompletion.go):
	// object, choices, created, model, finish_reason, index, message, and the
	// message role are required fields. Every violation is corrupt upstream
	// wire.
	if shadow.Object == nil || *shadow.Object != "chat.completion" {
		return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
			UpstreamChatCompletions,
			0,
			fmt.Errorf("chat response object = %q, want \"chat.completion\"", derefStr(shadow.Object)),
		)
	}
	// model is a required field of the pinned Chat response contract: absent
	// or empty refuses the exchange as a LOCAL conversion error, never an
	// upstream failure. A provider that omits the field is sloppy, not
	// poisonous, and must not open the circuit breaker for every route.
	if shadow.Model == nil || *shadow.Model == "" {
		return CanonicalResponse{}, ConversionReport{}, errors.New("chat response has no model")
	}
	if len(shadow.Choices) == 0 {
		return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
			UpstreamChatCompletions,
			0,
			errors.New("chat response has no choices"),
		)
	}
	if len(shadow.Choices) > 1 {
		return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
			UpstreamChatCompletions,
			0,
			errors.New("chat response has more than one choice; the transcoder requires n=1"),
		)
	}
	shadowChoice := shadow.Choices[0]
	if shadowChoice.Index == nil || *shadowChoice.Index != 0 {
		return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
			UpstreamChatCompletions,
			0,
			fmt.Errorf("chat response choice index = %v; n=1 requires index 0", indexOrZero(shadowChoice.Index)),
		)
	}
	// A non-streaming chat.completion choice carrying the streaming-only
	// delta arm is corrupt upstream wire: the non-streaming surface carries
	// only message, not delta. This is a KNOWN field, not a provider
	// extension, so rejecting it does not weaken the envelope's
	// unknown-field tolerance, and it prevents the delta content from being
	// silently dropped (GAP-012 parity).
	if shadowChoice.Delta != nil {
		return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
			UpstreamChatCompletions,
			0,
			errors.New("chat response choice carries a streaming delta arm; the non-streaming surface carries only message"),
		)
	}
	if shadowChoice.FinishReason == nil {
		return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
			UpstreamChatCompletions,
			0,
			errors.New("chat response choice has no finish_reason"),
		)
	}
	if shadowChoice.Message == nil {
		return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
			UpstreamChatCompletions,
			0,
			errors.New("chat response choice has no message"),
		)
	}
	if shadowChoice.Message.Role == nil || *shadowChoice.Message.Role != ChatMessageRoleAssistant {
		return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
			UpstreamChatCompletions,
			0,
			fmt.Errorf("chat response message role = %q, want assistant", derefRole(shadowChoice.Message.Role)),
		)
	}
	// The legacy non-tool_calls function_call spelling is a KNOWN official
	// field (pinned in pins.md): the single invocation maps to one
	// canonical tool call with a synthesized id, never a silent drop (which
	// would leave the client with a tool_use stop reason and no tool call).
	// A message carrying both spellings at once is a contradictory union.
	if len(shadowChoice.Message.FunctionCall) > 0 {
		trimmed := bytes.TrimSpace(shadowChoice.Message.FunctionCall)
		if !bytes.Equal(trimmed, []byte("null")) {
			if len(shadowChoice.Message.ToolCalls) > 0 {
				return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
					UpstreamChatCompletions,
					0,
					errors.New("chat response message carries both tool_calls and the legacy function_call spelling"),
				)
			}
			var frag struct {
				Name      *string `json:"name"`
				Arguments *string `json:"arguments"`
			}
			if err := json.Unmarshal(trimmed, &frag); err != nil {
				return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
					UpstreamChatCompletions,
					0,
					fmt.Errorf("chat response legacy function_call: %w", err),
				)
			}
			if frag.Name == nil || *frag.Name == "" {
				return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
					UpstreamChatCompletions,
					0,
					errors.New("chat response legacy function_call has no name"),
				)
			}
			if frag.Arguments == nil {
				return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
					UpstreamChatCompletions,
					0,
					errors.New("chat response legacy function_call has no arguments"),
				)
			}
		}
	}
	// tool_call_id is a tool-only field: on an assistant response message it
	// would otherwise be silently dropped. A message
	// carrying another role's fields is a contradictory union — a typed
	// decode rejection.
	if shadowChoice.Message.ToolCallID != nil {
		return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
			UpstreamChatCompletions,
			0,
			&wire.DecodeError{
				Kind:    wire.DecodeContradictoryUnion,
				Path:    "choices[].message.tool_call_id",
				Message: "chat response assistant message carries tool_call_id",
			},
		)
	}
	for i, call := range shadowChoice.Message.ToolCalls {
		if call.Type == nil || *call.Type != "function" {
			return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
				UpstreamChatCompletions,
				0,
				fmt.Errorf("chat response tool call %d type = %q, want function", i, derefStr(call.Type)),
			)
		}
		if call.ID == nil || *call.ID == "" {
			return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
				UpstreamChatCompletions,
				0,
				fmt.Errorf("chat response tool call %d has no id", i),
			)
		}
		if call.Function.Name == nil || *call.Function.Name == "" {
			return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
				UpstreamChatCompletions,
				0,
				fmt.Errorf("chat response tool call %d has no function name", i),
			)
		}
		if call.Function.Arguments == nil {
			return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
				UpstreamChatCompletions,
				0,
				fmt.Errorf("chat response tool call %d has no arguments", i),
			)
		}
	}

	// The wire decode may reject what the shadow accepted: the shadow's
	// pointer fields tolerate nulls that the wire decode rejects as illegal
	// (e.g. usage.total_tokens:null into a plain value field). Both decode
	// the same modeled surface otherwise; the shadow's presence checks above
	// reject every null that matters before the conversion path runs, and
	// any rejection here is corrupt upstream wire either way. Both passes
	// are tolerant to unknown provider-extension fields on the envelope.
	var chat ChatResponse
	if err := wire.DecodeTolerant(body, &chat); err != nil {
		return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
			UpstreamChatCompletions,
			0,
			fmt.Errorf("chat response: %w", err),
		)
	}
	// The shadow enforced exactly one choice with an explicit message; the
	// wire decode sees the same bytes.
	choice := chat.Choices[0]
	message := choice.Message

	response := CanonicalResponse{
		ID:        chat.ID,
		Model:     chat.Model,
		CreatedAt: float64(chat.Created),
		Status:    CanonicalResponseCompleted,
	}
	if chat.ServiceTier != nil {
		response.Source.ChatServiceTier = *chat.ServiceTier
	}
	if choice.LogProbs != nil {
		response.Source.ChatLogProbs = true
	}
	// Usage. The Known flags reflect the shadow's explicit presence: the
	// Chat usage totals are modeled omitempty (defensively — the pinned
	// contract marks them required, so a conforming upstream always sends
	// them, but presence is distinguishable only through the probe;
	// ). Cache-write tokens come from the
	// created_cache_tokens provider extension: a provider that reports it
	// makes the Messages cache-creation component known; one that does not
	// leaves the loss-gated unknown decision.
	//
	// Top-level provider extensions: a reasoning signal from
	// EITHER the pinned detail object OR the top-level reasoning_tokens
	// makes the component known (details win); a cache-read signal comes
	// from prompt_tokens_details.cached_tokens, then cached_tokens, then
	// prompt_cache_hit_tokens (DeepSeek hit = cached-read semantics).
	// prompt_cache_miss_tokens has no canonical home — the miss is the
	// derivable uncached prompt.
	var report ConversionReport
	if shadow.Usage != nil {
		response.Usage = CanonicalUsage{
			CacheReadKnown: shadow.Usage.PromptTokensDetails != nil ||
				shadow.Usage.CachedTokens != nil ||
				shadow.Usage.PromptCacheHitTokens != nil,
			ReasoningKnown: shadow.Usage.CompletionTokensDetails != nil ||
				shadow.Usage.ReasoningTokens != nil,
			CacheWriteKnown: shadow.Usage.PromptTokensDetails != nil &&
				shadow.Usage.PromptTokensDetails.CreatedCacheTokens != nil,
		}
		if shadow.Usage.PromptTokens != nil {
			response.Usage.InputTokens = int64(*shadow.Usage.PromptTokens)
			response.Usage.InputKnown = true
		}
		if shadow.Usage.CompletionTokens != nil {
			response.Usage.OutputTokens = int64(*shadow.Usage.CompletionTokens)
			response.Usage.OutputKnown = true
		}
		if shadow.Usage.TotalTokens != nil {
			response.Usage.TotalTokens = int64(*shadow.Usage.TotalTokens)
			response.Usage.TotalKnown = true
		}
		// A total that is not the exact sum of prompt + completion is an
		// observability fact (real gateways emit it), never an exchange
		// failure. The mismatch is recorded by the renderer's usage clamp on
		// the EMITTED counts: recording it here, pre-clamp, would name source
		// numbers the clamp may then correct.
		switch {
		case shadow.Usage.PromptTokensDetails != nil:
			response.Usage.CacheReadTokens = int64(shadow.Usage.PromptTokensDetails.CachedTokens)
			if shadow.Usage.PromptTokensDetails.CreatedCacheTokens != nil {
				response.Usage.CacheWriteTokens = int64(*shadow.Usage.PromptTokensDetails.CreatedCacheTokens)
			}
		case shadow.Usage.CachedTokens != nil:
			response.Usage.CacheReadTokens = int64(*shadow.Usage.CachedTokens)
		case shadow.Usage.PromptCacheHitTokens != nil:
			response.Usage.CacheReadTokens = int64(*shadow.Usage.PromptCacheHitTokens)
		}
		switch {
		case shadow.Usage.CompletionTokensDetails != nil:
			response.Usage.ReasoningTokens = int64(shadow.Usage.CompletionTokensDetails.ReasoningTokens)
		case shadow.Usage.ReasoningTokens != nil:
			response.Usage.ReasoningTokens = int64(*shadow.Usage.ReasoningTokens)
		}
	}

	// The shadow enforced finish_reason presence; unknown values are a
	// known-but-unsupported feature (local), never a defaulted success.
	switch derefStr(choice.FinishReason) {
	case "stop":
		response.Stop.Reason = CanonicalStopEndTurn
	case "length":
		response.Stop.Reason = CanonicalStopMaxTokens
		response.Status = CanonicalResponseIncomplete
		response.IncompleteReason = "max_output_tokens"
	case "tool_calls", "function_call":
		response.Stop.Reason = CanonicalStopToolUse
	case "content_filter":
		// The official Responses contract represents a filtered response as
		// status incomplete with reason content_filter; the Anthropic
		// dialect renders the refusal stop reason.
		response.Stop.Reason = CanonicalStopRefusal
		response.Status = CanonicalResponseIncomplete
		response.IncompleteReason = "content_filter"
	default:
		return CanonicalResponse{}, ConversionReport{}, &UnsupportedFeatureError{
			Protocol: "chat",
			Path:     "choices[].finish_reason",
			Feature:  *choice.FinishReason,
		}
	}

	// Map the legacy function_call to one tool call with a synthesized id
	// derived from the response id. The shadow validation above guarantees
	// the fragment is well-formed and alone; the wire decode discards it
	// (the wire type models only tool_calls), so inject the synthesized
	// call here and record the synthesis as an ungated note.
	if len(shadowChoice.Message.FunctionCall) > 0 && !bytes.Equal(bytes.TrimSpace(shadowChoice.Message.FunctionCall), []byte("null")) {
		var frag struct {
			Name      *string `json:"name"`
			Arguments *string `json:"arguments"`
		}
		// Validated above; a failure here is corrupt wire all the same.
		if err := json.Unmarshal(bytes.TrimSpace(shadowChoice.Message.FunctionCall), &frag); err != nil {
			return CanonicalResponse{}, ConversionReport{}, upstreamWireError(
				UpstreamChatCompletions,
				0,
				fmt.Errorf("chat response legacy function_call: %w", err),
			)
		}
		synthID := chat.ID + ":legacy-function-call-0"
		args := ""
		if frag.Arguments != nil {
			args = *frag.Arguments
		}
		legacyCall := ChatMessageToolCall{
			Type: "function",
			ID:   &synthID,
			Function: ChatToolCallFunction{
				Name:      frag.Name,
				Arguments: args,
			},
		}
		if message.ChatAssistantMessage == nil {
			message.ChatAssistantMessage = &ChatAssistantMessage{}
		}
		message.ToolCalls = append(message.ToolCalls, legacyCall)
		if err := report.Note(
			FeatureLegacyFunctionCall,
			"choices[].message.function_call",
			"legacy function_call mapped to one tool call with id synthesized from the response id ("+synthID+")",
		); err != nil {
			return CanonicalResponse{}, ConversionReport{}, err
		}
	}

	// The assistant message becomes one message item; tool calls become
	// their own function-call items, preserving function-call identity and
	// the model-generated arguments byte-exact. The
	// answer-content parts render even while provider reasoning is dropped
	// under an approved loss, so the client still receives a rendered response.
	parts, calls, err := chatMessageToCanonicalParts(message, capabilities, policy, &report)
	if err != nil {
		return CanonicalResponse{}, report, err
	}
	items := make([]CanonicalResponseItem, 0, 1+len(calls))
	if len(parts) > 0 {
		items = append(items, &CanonicalMessageItem{
			Role:  CanonicalAssistant,
			Parts: parts,
		})
	}
	for _, call := range calls {
		items = append(items, call)
	}
	response.Items = items
	if err := ValidateCanonicalResponse(response); err != nil {
		return CanonicalResponse{}, ConversionReport{}, err
	}
	return response, report, nil
}

// indexOrZero dereferences a presence pointer, defaulting to zero.
func indexOrZero(index *int64) int64 {
	if index == nil {
		return 0
	}
	return *index
}

// derefRole dereferences a role presence pointer for error messages.
func derefRole(role *ChatMessageRole) string {
	if role == nil {
		return ""
	}
	return string(*role)
}

// chatMessageToCanonicalParts converts a Chat assistant message into
// canonical output: content parts (text, refusal, and — when the capability
// is enabled — provider plaintext reasoning mapped to ordinary text) and
// separate function-call items. Tool-call arguments are model-generated and
// preserved byte-exact in ToolArguments; invalid model output is never an
// upstream defect. Provider plaintext reasoning without
// the capability follows the loss policy: an approved
// provider_reasoning_text loss with the reasoning dropped when the policy
// allows it, an error otherwise (the stream disposition, sharing the same
// loss key and detail). The report is non-nil.
func chatMessageToCanonicalParts(
	message *ChatMessage,
	capabilities ChatCapabilities,
	policy LossPolicy,
	report *ConversionReport,
) ([]CanonicalPart, []*CanonicalFunctionCallItem, error) {
	var parts []CanonicalPart
	var calls []*CanonicalFunctionCallItem

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
					return nil, nil, &UnsupportedFeatureError{
						Protocol: "chat",
						Path:     "choices[].message.content[].type",
						Feature:  "image_url",
					}
				default:
					// An unknown content block type is outside the modeled
					// surface: corrupt wire (an upstream failure). Known
					// features the transcoder does not support — image_url
					// above, unknown finish_reason values in DecodeChatResponse
					// — are typed UnsupportedFeatureError and stay local.
					return nil, nil, upstreamWireError(
						UpstreamChatCompletions,
						0,
						fmt.Errorf(
							"chat message content block %d: unknown type %q",
							i,
							block.Type,
						),
					)
				}
			}
		}
	}

	if message.ChatAssistantMessage != nil {
		if message.Refusal != nil {
			parts = append(parts, CanonicalRefusal{Text: *message.Refusal})
		}

		// Provider plaintext reasoning arrives in two provider spellings:
		// `reasoning` (OpenRouter style) and `reasoning_content` (the
		// DeepSeek/Qwen convention real open-weights gateways emit). They
		// are one logical field; a body carrying two non-empty spellings at
		// once is contradictory upstream wire, never an ordered merge. The
		// resolved text follows Reasoning's semantics exactly: capability on
		// maps to ordinary text, capability off is a typed rejection naming
		// the actual field.
		reasoningText, reasoningPath, both := resolveChatReasoningSpelling(
			message.Reasoning,
			message.ReasoningContent,
			"choices[].message.reasoning",
			"choices[].message.reasoning_content",
		)
		if both {
			return nil, nil, upstreamWireError(
				UpstreamChatCompletions,
				0,
				errors.New(chatProviderReasoningBothDetail),
			)
		}
		if reasoningText != "" {
			if capabilities.ProviderReasoningThinking {
				// The provider_reasoning_thinking capability maps provider
				// plaintext reasoning to a NATIVE thinking part carrying the
				// proxy's marker signature (the request path scrubs
				// marker-signature blocks from replayed history, so the
				// synthetic signature never reaches an upstream). Thinking
				// takes precedence over the ordinary-text mapping when both
				// capabilities are enabled — it is the more faithful
				// rendering, and Claude Code displays it with the native
				// thinking UI.
				parts = append(parts, CanonicalThinkingPart{
					Text:      reasoningText,
					Signature: SyntheticThinkingSignature,
				})
				if err := report.Note(
					FeatureProviderReasoningText,
					reasoningPath,
					"provider reasoning mapped to a native thinking block (provider_reasoning_thinking encoding)",
				); err != nil {
					return nil, nil, err
				}
			} else if capabilities.ProviderReasoningText {
				// Provider plaintext reasoning is mapped to ordinary text only.
				// The mapping is the named provider_reasoning_text encoding and
				// is recorded exactly once, sharing the stream surface's note
				// detail so a capability-on exchange is observable through both
				// conversion paths.
				parts = append(parts, CanonicalText{Text: reasoningText})
				if err := report.Note(
					FeatureProviderReasoningText,
					reasoningPath,
					chatProviderReasoningMappedDetail,
				); err != nil {
					return nil, nil, err
				}
			} else {
				// Neither capability: an approved loss with the reasoning
				// dropped (the content parts still render) or an
				// UnsupportedFeatureError under the strict policy — never a
				// silent drop (stream disposition parity).
				if err := report.Lose(
					policy,
					FeatureProviderReasoningText,
					reasoningPath,
					chatProviderReasoningDroppedDetail,
				); err != nil {
					return nil, nil, err
				}
			}
		}

		for i, call := range message.ToolCalls {
			if call.ID == nil || *call.ID == "" {
				return nil, nil, upstreamWireError(
					UpstreamChatCompletions,
					0,
					fmt.Errorf("chat tool call %d has no id", i),
				)
			}
			name := ""
			if call.Function.Name != nil {
				name = *call.Function.Name
			}
			// The shadow enforced arguments presence; the raw string is
			// preserved byte-exact — invalid model output is preserved, not
			// rejected. The empty-string-to-"{}"
			// substitution exists only in the render direction.
			calls = append(calls, &CanonicalFunctionCallItem{
				CallID:    *call.ID,
				Name:      name,
				Arguments: ParseToolArguments(call.Function.Arguments),
			})
		}
	}

	return parts, calls, nil
}

// DecodeResponsesResponse decodes a non-streaming Responses response into the
// canonical IR. Reasoning output items are carried as source artifacts; their
// loss or rejection is decided at render time against the target protocol.
func DecodeResponsesResponse(
	body []byte,
) (CanonicalResponse, error) {
	var envelope ResponseEnvelope
	if err := wire.DecodeTolerant(body, &envelope); err != nil {
		// A decode failure — malformed JSON or a type-corrupt modeled value —
		// is corrupt upstream wire, an
		// upstream failure. Valid features the
		// transcoder knows but does not support are rejected as
		// UnsupportedFeatureError (local) instead: the wire layer reports
		// them as wire.UnsupportedTypeError and the boundary translates
		// before the upstream-wire guard, so they can never be misclassified.
		return CanonicalResponse{}, upstreamWireError(
			UpstreamResponses,
			0,
			fmt.Errorf(
				"responses response: %w",
				wireUnsupportedToFeature(err),
			),
		)
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
			response.Stop.Reason = CanonicalStopRefusal
		} else {
			response.Stop.Reason = CanonicalStopMaxTokens
		}
	case "failed":
		response.Status = CanonicalResponseFailed
		if envelope.Error != nil {
			response.ErrorMessage = envelope.Error.Message
		}
	case "":
		// An absent or empty status is a missing required semantic field: the
		// exchange is refused as a LOCAL conversion error, never an upstream
		// failure, so a provider that omits it cannot open the circuit
		// breaker. A PRESENT but unknown status stays an unsupported feature
		// (also local) below.
		return CanonicalResponse{}, errors.New("responses response has no status")
	default:
		return CanonicalResponse{}, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     "status",
			Feature:  envelope.Status,
		}
	}

	for _, control := range []struct {
		name    string
		present bool
	}{
		{"background", envelope.Background != nil},
		{"max_tool_calls", envelope.MaxToolCalls != nil},
		{"prompt", envelope.Prompt != nil},
		{"prompt_cache_key", envelope.PromptCacheKey != ""},
		{"safety_identifier", envelope.SafetyIdentifier != ""},
	} {
		if control.present {
			response.Source.ResponsesControls = append(response.Source.ResponsesControls, control.name)
		}
	}
	if envelope.ServiceTier != nil {
		response.Source.ResponsesServiceTier = *envelope.ServiceTier
	}

	if envelope.Usage != nil {
		response.Usage = CanonicalUsage{
			InputTokens:     envelope.Usage.InputTokens,
			OutputTokens:    envelope.Usage.OutputTokens,
			TotalTokens:     envelope.Usage.TotalTokens,
			InputKnown:      true,
			OutputKnown:     true,
			TotalKnown:      true,
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

	// Output items decode one-to-one into canonical items: item boundaries,
	// output ordering, phases, reasoning artifacts, function calls, and
	// conversation-state items survive until the target renderer.
	sawToolUse := false
	for i, item := range envelope.Output {
		switch value := item.(type) {
		case *ResponsesOutputMessage:
			var parts []CanonicalPart
			for _, content := range value.Content {
				switch part := content.(type) {
				case *ResponsesOutputText:
					parts = append(parts, CanonicalText{Text: part.Text})
				case *ResponsesOutputRefusal:
					parts = append(parts, CanonicalRefusal{Text: part.Refusal})
				default:
					return CanonicalResponse{}, fmt.Errorf(
						"output item %d: unknown content part %T",
						i,
						content,
					)
				}
			}
			response.Items = append(response.Items, &CanonicalMessageItem{
				ID:    value.ID,
				Role:  CanonicalAssistant,
				Phase: Optional[string]{Value: value.Phase, Set: value.Phase != ""},
				Parts: parts,
			})

		case *ResponsesFunctionCallOutputItem:
			// Model-generated arguments are preserved byte-exact; invalid
			// model output is never an upstream defect.
			response.Items = append(response.Items, &CanonicalFunctionCallItem{
				ItemID:    value.ID,
				CallID:    value.CallID,
				Name:      value.Name,
				Arguments: ParseToolArguments(value.Arguments),
			})
			sawToolUse = true

		case *ResponsesFunctionCallOutputResultItem:
			outputParts, err := responsesFunctionOutputToCanonical(value.Output)
			if err != nil {
				return CanonicalResponse{}, fmt.Errorf(
					"output item %d function call output: %w",
					i,
					err,
				)
			}
			response.Items = append(response.Items, &CanonicalFunctionResultItem{
				ItemID: value.ID,
				CallID: value.CallID,
				Parts:  outputParts,
			})

		case *ResponsesReasoningOutputItem:
			raw, err := json.Marshal(value)
			if err != nil {
				return CanonicalResponse{}, fmt.Errorf("output item %d: %w", i, err)
			}
			response.Items = append(response.Items, &CanonicalReasoningItem{Raw: raw})

		default:
			return CanonicalResponse{}, fmt.Errorf(
				"output item %d: unknown item type %T",
				i,
				item,
			)
		}
	}

	switch response.Status {
	case CanonicalResponseCompleted:
		if sawToolUse {
			response.Stop.Reason = CanonicalStopToolUse
		} else {
			response.Stop.Reason = CanonicalStopEndTurn
		}
	case CanonicalResponseIncomplete:
		// The decode already recorded the reason-specific stop reason
		// (content_filter -> refusal, anything else max_tokens); keep it.
		if response.Stop.Reason == "" {
			response.Stop.Reason = CanonicalStopMaxTokens
		}
	case CanonicalResponseFailed:
		response.Stop.Reason = CanonicalStopEndTurn
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
// conversion.
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
	// rejection per the exchange policy — never a silent drop.
	var report ConversionReport
	if response.Source.ChatLogProbs {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureLogprobs,
			"choices[].logprobs",
			"chat response logprobs cannot be reproduced in a Responses response",
		); err != nil {
			return nil, report, err
		}
	}
	if response.Source.ChatServiceTier != "" {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureResponseServiceTier,
			"service_tier",
			"the upstream chat service tier actually served cannot be reproduced in a Responses response",
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

	// Output items render one-to-one from the canonical items; item
	// boundaries and identities are preserved.
	for _, item := range response.Items {
		switch value := item.(type) {
		case *CanonicalMessageItem:
			message := &ResponsesOutputMessage{
				ID:      context.IDs.New("msg_"),
				Type:    "message",
				Role:    "assistant",
				Status:  ResponsesItemCompleted,
				Content: ResponsesOutputContentParts{},
			}
			if value.Phase.Set {
				message.Phase = value.Phase.Value
			}
			for _, part := range value.Parts {
				switch partValue := part.(type) {
				case CanonicalText:
					message.Content = append(message.Content, &ResponsesOutputText{
						Type:        "output_text",
						Text:        partValue.Text,
						Annotations: []ResponsesAnnotation{},
					})
				case CanonicalThinkingPart:
					// A thinking part renders as a native Responses reasoning
					// output item: the reasoning summary carries the provider
					// reasoning text (the Responses dialect has no thinking
					// blocks; this is its native reasoning carrier).
					envelope.Output = append(envelope.Output, &ResponsesReasoningOutputItem{
						ID:     context.IDs.New("rs_"),
						Type:   "reasoning",
						Status: ResponsesItemCompleted,
						Summary: []ResponsesReasoningSummary{{
							Type: "summary_text",
							Text: partValue.Text,
						}},
					})
				case CanonicalRefusal:
					message.Content = append(message.Content, &ResponsesOutputRefusal{
						Type:    "refusal",
						Refusal: partValue.Text,
					})
				default:
					return nil, report, fmt.Errorf(
						"response message item: unknown canonical part %T",
						part,
					)
				}
			}
			envelope.Output = append(envelope.Output, message)

		case *CanonicalFunctionCallItem:
			// The Responses function_call arguments field is a string:
			// the model-generated raw text is preserved byte-exact
			//.
			callName, callNamespace := context.ToolNames.clientCallName(value.Name)
			envelope.Output = append(envelope.Output, &ResponsesFunctionCallOutputItem{
				ID:        context.IDs.New("fc_"),
				Type:      "function_call",
				Status:    ResponsesItemCompleted,
				CallID:    value.CallID,
				Name:      callName,
				Arguments: value.Arguments.Raw,
				Namespace: callNamespace,
			})

		case *CanonicalFunctionResultItem:
			return nil, report, &UnsupportedFeatureError{
				Protocol: "responses",
				Path:     "output[]",
				Feature:  "function result in model output",
			}

		case *CanonicalReasoningItem:
			var reasoning ResponsesReasoningOutputItem
			if err := json.Unmarshal(value.Raw, &reasoning); err != nil {
				return nil, report, fmt.Errorf("response reasoning item: %w", err)
			}
			envelope.Output = append(envelope.Output, &reasoning)

		default:
			return nil, report, fmt.Errorf(
				"response item: unknown canonical item type %T",
				item,
			)
		}
	}

	// Usage.
	// Usage is emitted only when the source provided it: unknown usage is
	// never fabricated as zero facts. The pinned
	// Responses contract requires the breakdown detail objects on the usage
	// object (openai-go v1.12.0 response.go): a component the source did not
	// provide is a usage-timing loss (approved or rejected per the exchange
	// policy), never a silent zero — omitting the required field would just
	// move the fabricated zero into the client's defaulting. The total is the source's own when provided, otherwise
	// derived from the parts. The Responses wire does not require the usage
	// object itself, so a source without usage renders without one — but
	// the omission is recorded as a Note (a sanctioned elision, not a
	// policy-gated loss: nothing the source sent was dropped), so the
	// exchange's usage provenance stays observable (
	// the omission was silent).
	if response.Usage.Unknown() {
		if err := report.Note(
			FeatureUsageUnknown,
			"usage",
			"the source response provided no token usage; the Responses usage object is omitted (the target wire does not require it)",
		); err != nil {
			return nil, report, err
		}
	} else {
		// The upstream usage is a subject-to-change provider value: an
		// arithmetically inconsistent source (a cached breakdown exceeding
		// the input total, negative counts) is CLAMPED into the Responses
		// invariants and recorded as an ungated note naming the source
		// numbers, instead of failing the exchange (the same clamp the
		// Messages renderer applies).
		usage := response.Usage
		if clamp := clampCanonicalUsage(&usage); !clamp.empty() {
			if err := clamp.record(&report, "usage"); err != nil {
				return nil, report, err
			}
		}
		// The total is the source's own when provided, otherwise derived from
		// the parts with checked arithmetic: a derived sum that cannot be
		// represented is saturated and the saturation recorded, never a silent
		// wrap into a negative total.
		total := usage.TotalTokens
		if !usage.TotalKnown {
			var saturation string
			total, saturation = derivedUsageTotal(usage.InputTokens, usage.OutputTokens)
			if saturation != "" {
				if err := report.Note(FeatureUsageTotalMismatch, "usage", saturation); err != nil {
					return nil, report, err
				}
			}
		}
		envelope.Usage = &ResponsesUsage{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			TotalTokens:  total,
		}
		// Each wire-required component the source did not provide is its own
		// granular loss decision.
		components := []struct {
			feature Feature
			name    string
			known   bool
		}{
			{FeatureUsageCacheReadUnknown, "input_tokens_details.cached_tokens", response.Usage.CacheReadKnown},
			{FeatureUsageReasoningUnknown, "output_tokens_details.reasoning_tokens", response.Usage.ReasoningKnown},
			{FeatureUsageUnknown, "input_tokens or output_tokens", response.Usage.InputKnown && response.Usage.OutputKnown},
		}
		for _, component := range components {
			if component.known {
				continue
			}
			if err := report.Lose(
				context.lossPolicy(),
				component.feature,
				"usage",
				"the upstream response did not provide "+component.name+"; the required Responses usage cannot be reproduced",
			); err != nil {
				return nil, report, err
			}
		}
		envelope.Usage.InputTokensDetails = &UsageInputTokensDetails{
			CachedTokens: usage.CacheReadTokens,
		}
		envelope.Usage.OutputTokensDetails = &UsageOutputTokensDetails{
			ReasoningTokens: usage.ReasoningTokens,
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

	// Request echo. The effective values were normalized at request decode
	// with the pinned API defaults, so the envelope echo always carries a
	// complete, valid value for the required fields.
	if echo := context.OriginalResponsesRequest; echo != nil {
		envelope.Instructions = echo.Instructions
		if echo.MaxOutputTokens != nil {
			value := int64(*echo.MaxOutputTokens)
			envelope.MaxOutputTokens = &value
		}
		envelope.ParallelToolCalls = new(echo.ParallelToolCalls)
		envelope.PreviousResponseID = echo.PreviousResponseID
		envelope.Store = echo.Store
		envelope.Temperature = &echo.Temperature
		envelope.TopP = &echo.TopP
		envelope.Truncation = echo.Truncation
		envelope.User = echo.User
		envelope.Metadata = echo.Metadata
		envelope.Tools = echo.Tools
		envelope.ToolChoice = &echo.ToolChoice
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
		// response.failed classification.
		return nil, report, &UpstreamSemanticFailureError{
			Message: response.ErrorMessage,
		}
	}
	if context == nil || context.IDs == nil {
		return nil, report, errors.New("render messages response requires an exchange context")
	}
	// Chat response attributes the Messages response cannot reproduce
	// (token log-probabilities and the tier actually served) are a loss or a
	// rejection per the exchange policy — never a silent drop.
	if response.Source.ChatLogProbs {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureLogprobs,
			"choices[].logprobs",
			"chat response logprobs cannot be reproduced in a Messages response",
		); err != nil {
			return nil, report, err
		}
	}
	if response.Source.ChatServiceTier != "" {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureResponseServiceTier,
			"service_tier",
			"the upstream chat service tier actually served cannot be reproduced in a Messages response",
		); err != nil {
			return nil, report, err
		}
	}
	// The pinned Responses envelope controls cannot be reproduced in a
	// Messages response.
	if len(response.Source.ResponsesControls) > 0 {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureResponsesControls,
			"output",
			"the Responses envelope controls "+strings.Join(response.Source.ResponsesControls, ", ")+
				" cannot be reproduced in a Messages response",
		); err != nil {
			return nil, report, err
		}
	}
	// The Responses source's service tier enters the same loss/reject
	// decision as the chat source's tier (the
	// Responses→Messages drop was silent).
	if response.Source.ResponsesServiceTier != "" {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureResponseServiceTier,
			"service_tier",
			"the upstream service tier actually served cannot be reproduced in a Messages response",
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

	// Phase and reasoning losses are recorded once per response, before the
	// items render.
	phaseLossRecorded := false
	reasoningLossRecorded := false
	for _, item := range response.Items {
		switch value := item.(type) {
		case *CanonicalMessageItem:
			if value.Phase.Set && !phaseLossRecorded {
				phaseLossRecorded = true
				if err := report.Lose(
					context.lossPolicy(),
					FeatureOutputPhase,
					"output[].phase",
					"the output message phase cannot be reproduced in a Messages response",
				); err != nil {
					return nil, report, err
				}
			}
			// Anthropic ordering: thinking blocks precede text. The chat
			// message walk appends the reasoning part AFTER the content
			// parts (the reasoning_content field follows content on the
			// chat wire), so the parts are emitted in two passes: thinking
			// first, then text/refusal.
			for _, part := range value.Parts {
				if thinkingPart, ok := part.(CanonicalThinkingPart); ok {
					thinking := thinkingPart.Text
					signature := thinkingPart.Signature
					out.Content = append(out.Content, AnthropicContentBlock{
						Type:      AnthropicContentBlockTypeThinking,
						Thinking:  &thinking,
						Signature: &signature,
					})
				}
			}
			for _, part := range value.Parts {
				switch partValue := part.(type) {
				case CanonicalText:
					text := partValue.Text
					out.Content = append(out.Content, AnthropicContentBlock{
						Type: AnthropicContentBlockTypeText,
						Text: &text,
					})
				case CanonicalRefusal:
					text := partValue.Text
					out.Content = append(out.Content, AnthropicContentBlock{
						Type: AnthropicContentBlockTypeText,
						Text: &text,
					})
				case CanonicalThinkingPart:
					// Already emitted in the thinking-first pass above.
				default:
					return nil, report, fmt.Errorf(
						"response message item: unknown canonical part %T",
						part,
					)
				}
			}

		case *CanonicalFunctionCallItem:
			// Anthropic tool_use.input requires an object. Model-generated
			// arguments that are not an object are a LOCAL unrepresentable
			// output — never corrupt upstream wire.
			if !value.Arguments.IsObject {
				return nil, report, &UnrepresentableError{
					Protocol: "messages",
					Path:     "content[].tool_use.input",
					Detail:   "the model-generated tool arguments are not a JSON object and cannot be represented as tool_use.input",
				}
			}
			callID := value.CallID
			name := value.Name
			out.Content = append(out.Content, AnthropicContentBlock{
				Type:  AnthropicContentBlockTypeToolUse,
				ID:    &callID,
				Name:  &name,
				Input: value.Arguments.Object,
			})

		case *CanonicalFunctionResultItem:
			// Conversation-state echoes (tool results in the upstream output)
			// belong to the next request, not the current Messages response.
			// They are an approved loss or a rejection per the exchange policy.
			if err := report.Lose(
				context.lossPolicy(),
				FeatureOutputItemBoundaries,
				"output[].function_call_output",
				"tool results are conversation-state output items, not part of the model response",
			); err != nil {
				return nil, report, err
			}

		case *CanonicalReasoningItem:
			if !reasoningLossRecorded {
				reasoningLossRecorded = true
				if err := report.Lose(
					context.lossPolicy(),
					FeatureReasoningSummary,
					"output[].reasoning",
					"Responses reasoning output cannot be reproduced in a Messages response",
				); err != nil {
					return nil, report, err
				}
			}

		default:
			return nil, report, fmt.Errorf(
				"response item: unknown canonical item type %T",
				item,
			)
		}
	}

	// stop_reason and stop_sequence are always present on the wire: the
	// completed response carries the real values (stop_sequence is null
	// unless a custom sequence was used).
	switch response.Stop.Reason {
	case CanonicalStopEndTurn:
		out.StopReason = new(AnthropicStopReasonEndTurn)
	case CanonicalStopMaxTokens:
		out.StopReason = new(AnthropicStopReasonMaxTokens)
	case CanonicalStopStopSequence:
		out.StopReason = new(AnthropicStopReasonStopSequence)
		out.StopSequence = &response.Stop.Sequence
	case CanonicalStopToolUse:
		out.StopReason = new(AnthropicStopReasonToolUse)
	case CanonicalStopRefusal:
		out.StopReason = new(AnthropicStopReasonRefusal)
		out.StopDetails = &AnthropicStopDetails{Type: "refusal"}
	default:
		out.StopReason = new(AnthropicStopReasonEndTurn)
	}

	// Anthropic usage semantics: input_tokens + cache_creation_input_tokens
	// + cache_read_input_tokens = total. The uncached input is the total
	// minus the cached breakdown, with checked nonnegative arithmetic
	//. Unknown usage is never fabricated as zero facts:
	// it is an explicit loss/reject decision.
	if response.Usage.Unknown() {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureUsageUnknown,
			"usage",
			"the upstream response did not provide token usage; the required Messages usage cannot be reproduced",
		); err != nil {
			return nil, report, err
		}
		// The Messages wire contract requires the usage object AND its
		// output_tokens_details; under an approved loss the zeros are a
		// documented approximation, never a silent fabrication.
		out.Usage = &AnthropicUsage{
			OutputTokensDetails: &AnthropicOutputTokensDetails{},
		}
	} else {
		// The Messages wire requires cache_creation_input_tokens,
		// cache_read_input_tokens, and output_tokens_details.thinking_tokens
		// on the usage object: every component the source did not provide is
		// a usage-timing loss (approved or rejected per the exchange policy),
		// never a silent zero. Cache-write tokens are
		// not part of the pinned Chat/Responses contract; the chat source
		// knows them only through the created_cache_tokens provider
		// extension, and the Responses source never does.
		// Each wire-required component the source did not provide is its own
		// granular loss decision.
		components := []struct {
			feature Feature
			name    string
			known   bool
		}{
			{FeatureUsageUnknown, "input_tokens", response.Usage.InputKnown},
			{FeatureUsageCacheReadUnknown, "cache_read_input_tokens", response.Usage.CacheReadKnown},
			{FeatureUsageCacheWriteUnknown, "cache_creation_input_tokens", response.Usage.CacheWriteKnown},
			{FeatureUsageUnknown, "output_tokens", response.Usage.OutputKnown},
			{FeatureUsageReasoningUnknown, "output_tokens_details.thinking_tokens", response.Usage.ReasoningKnown},
		}
		for _, component := range components {
			if component.known {
				continue
			}
			if err := report.Lose(
				context.lossPolicy(),
				component.feature,
				"usage",
				"the upstream response did not provide "+component.name+"; the required Messages usage cannot be reproduced",
			); err != nil {
				return nil, report, err
			}
		}
		// The upstream usage is a subject-to-change provider value: an
		// arithmetically inconsistent source (a cached breakdown exceeding
		// the input total, negative counts) is CLAMPED into the Messages
		// invariants and recorded as an ungated note naming the source
		// numbers, instead of failing the exchange (the pre-fix failure was a
		// 502 reading "source usage is arithmetically inconsistent").
		usage := response.Usage
		if clamp := clampCanonicalUsage(&usage); !clamp.empty() {
			if err := clamp.record(&report, "usage"); err != nil {
				return nil, report, err
			}
		}
		inputTokens := usage.InputTokens
		cached := usage.CacheReadTokens + usage.CacheWriteTokens
		// Checked, architecture-independent int64-to-int conversion before
		// rendering Messages usage: a count that cannot be represented on
		// this platform (32-bit builds) is a typed error, never a silent
		// overflow.
		uncached, err := checkedInt64ToInt(inputTokens - cached)
		if err != nil {
			return nil, report, &UsageArithmeticError{Detail: "input tokens: " + err.Error()}
		}
		cacheWrite, err := checkedInt64ToInt(usage.CacheWriteTokens)
		if err != nil {
			return nil, report, &UsageArithmeticError{Detail: "cache-creation tokens: " + err.Error()}
		}
		cacheRead, err := checkedInt64ToInt(usage.CacheReadTokens)
		if err != nil {
			return nil, report, &UsageArithmeticError{Detail: "cache-read tokens: " + err.Error()}
		}
		output, err := checkedInt64ToInt(usage.OutputTokens)
		if err != nil {
			return nil, report, &UsageArithmeticError{Detail: "output tokens: " + err.Error()}
		}
		thinking, err := checkedInt64ToInt(usage.ReasoningTokens)
		if err != nil {
			return nil, report, &UsageArithmeticError{Detail: "reasoning tokens: " + err.Error()}
		}
		// output_tokens_details is required on the wire for known usage; the
		// thinking breakdown is zero only after the loss decision above,
		// matching the stream path.
		out.Usage = &AnthropicUsage{
			InputTokens:              uncached,
			CacheCreationInputTokens: cacheWrite,
			CacheReadInputTokens:     cacheRead,
			OutputTokens:             output,
			OutputTokensDetails: &AnthropicOutputTokensDetails{
				ThinkingTokens: thinking,
			},
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, report, err
	}
	return body, report, nil
}
