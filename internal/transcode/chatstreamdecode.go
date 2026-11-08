package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// errChatStreamDone marks the [DONE] sentinel of a Chat stream.
var errChatStreamDone = errors.New("chat stream [DONE] sentinel")

// errChatDoneBeforeTerminal builds the typed upstream wire error returned
// when the [DONE] sentinel arrives before any terminal condition. The
// sentinel itself decides the result: the exchange is an upstream body
// failure and the reader must stop immediately rather than wait on an
// upstream that keeps the connection open after [DONE].
func errChatDoneBeforeTerminal() error {
	return upstreamWireError(
		UpstreamChatCompletions,
		http.StatusOK,
		errors.New("chat stream [DONE] before a terminal condition"),
	)
}

// parseLegacyFunctionCallFragment parses one legacy function_call fragment:
// explicit nulls are illegal (the tool_calls spelling rejects null arguments
// at the wire level), malformed JSON is corrupt wire, and a fragment with
// neither a name nor arguments content carries nothing to accumulate.
// When both members are present but carry empty strings (a benign terminal
// marker emitted by some upstreams before [DONE]), emptyFragment is true
// and the caller should treat it as a no-op equivalent to null.
func parseLegacyFunctionCallFragment(raw json.RawMessage) (name *string, args *string, hasContent bool, emptyFragment bool, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil, false, false, errors.New("legacy function_call is null")
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &present); err != nil {
		return nil, nil, false, false, err
	}
	if v, ok := present["name"]; ok {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, nil, false, false, errors.New("legacy function_call name is null")
		}
	}
	if v, ok := present["arguments"]; ok {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, nil, false, false, errors.New("legacy function_call arguments is null")
		}
	}
	var frag struct {
		Name      *string `json:"name"`
		Arguments *string `json:"arguments"`
	}
	if err := json.Unmarshal(trimmed, &frag); err != nil {
		return nil, nil, false, false, err
	}
	hasName := frag.Name != nil && *frag.Name != ""
	hasArgs := frag.Arguments != nil && *frag.Arguments != ""
	if !hasName && !hasArgs {
		// Both members present but empty strings: a benign terminal marker,
		// not corrupt wire. The caller treats this as a no-op.
		_, namePresent := present["name"]
		_, argsPresent := present["arguments"]
		if namePresent && argsPresent {
			return nil, nil, false, true, nil
		}
	}
	return frag.Name, frag.Arguments, hasName || hasArgs, false, nil
}

// chatStreamChunkShadow is the presence-aware strict decode shadow of a Chat
// streaming chunk: the pinned envelope fields (object,
// id, model, created) and the choice fields (index, delta) are required and
// must be explicitly present — absent fields are corrupt upstream wire,
// never zero-defaulted. The upstream envelope is a subject-to-change
// contract, so an unknown field is TOLERATED (never a failure); the
// non-streaming message arm, by contrast, is a KNOWN field and a STRUCTURAL
// rejection (a streaming chunk carries only deltas). The delta reuses
// ChatStreamDelta (already pointer-based); usage reuses chatUsageShadow so an
// omitted total is distinguishable from zero (the pinned contract requires
// all three totals).
type chatStreamChunkShadow struct {
	ID                string                   `json:"id"`
	Object            *string                  `json:"object"`
	Created           *int64                   `json:"created"`
	Model             string                   `json:"model"`
	ServiceTier       *string                  `json:"service_tier,omitempty"`
	SystemFingerprint *string                  `json:"system_fingerprint,omitempty"`
	Choices           []chatStreamChoiceShadow `json:"choices"`
	Usage             *chatUsageShadow         `json:"usage,omitempty"`

	// derivedTotal names the usage total this decoder derived from the other
	// two (absent on the wire); it is recorded per exchange, never forwarded.
	derivedTotal string

	// Opaque provider-extension fields present on real chat streams (e.g.
	// the yolo gateway's prompt_token_ids/prompt_text): decoded so strict
	// wire decoding never fails on a current provider; never forwarded.
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

// chatStreamChoiceShadow mirrors the pinned streaming choice: index and
// delta are required, finish_reason is a present-or-absent nullable string
// (no terminal is enforced here), logprobs is nullable, and the non-streaming
// message arm is a STRUCTURAL rejection — the streaming surface carries only
// deltas, so a chunk carrying a message arm is corrupt upstream wire (a known
// field, not a provider extension, so this does not weaken the envelope's
// unknown-field tolerance). Choices absent from the chunk are not
// distinguished from an empty list: a usage-only tail chunk legitimately
// carries choices: [].
type chatStreamChoiceShadow struct {
	Index        *int64              `json:"index"`
	FinishReason *string             `json:"finish_reason"`
	LogProbs     *ChatChoiceLogprobs `json:"logprobs"`
	Delta        *ChatStreamDelta    `json:"delta"`

	// Message is the non-streaming message arm. It is modeled so a chunk
	// carrying it is rejected (the streaming surface carries only deltas),
	// rather than silently skipped and its content dropped.
	Message json.RawMessage `json:"message,omitempty"`

	TokenIDs      any     `json:"token_ids,omitempty"`
	RoutedExperts any     `json:"routed_experts,omitempty"`
	StopReason    *string `json:"stop_reason,omitempty"`
	// MatchedStop mirrors the wire Choice extension (2026-08-24);
	// delta level is unobserved — the raw-upstream probe places the field on
	// the choice of every chunk — so ChatStreamDelta deliberately carries no
	// twin: the placement was observed only on a live upstream probe.
	MatchedStop any `json:"matched_stop,omitempty"`
}

// chatStreamChunkFromSSE parses one upstream SSE frame into a Chat stream
// chunk, reporting the [DONE] sentinel via errChatStreamDone. An in-band
// error frame is surfaced as an error: it must never be silently ignored
// while the stream continues (and potentially ends as clean success).
func chatStreamChunkFromSSE(frame SSEEvent) (ChatStreamResponse, error) {
	data := bytes.TrimSpace(frame.Data)
	if bytes.Equal(data, []byte("[DONE]")) {
		return ChatStreamResponse{}, errChatStreamDone
	}
	var probe struct {
		Error *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			fmt.Errorf("chat stream chunk: %w", err),
		)
	}
	if probe.Error != nil {
		var detail struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(*probe.Error, &detail)
		message := detail.Message
		if message == "" {
			message = "chat stream error frame"
		}
		// An in-band Chat error frame is real upstream data: the typed
		// error carries upstream provenance so the exchange classifies as
		// an upstream failure, never a local conversion failure.
		return ChatStreamResponse{}, &StreamConversionError{
			Cause:      fmt.Errorf("chat stream chunk: %s", message),
			Provenance: ProvenanceUpstreamBodyError,
			Status:     http.StatusOK,
		}
	}
	// Presence-aware strict shadow decode: the pinned envelope and choice
	// fields must be explicitly present — absent fields are corrupt upstream
	// wire. The upstream envelope is a subject-to-change
	// contract, so the decode is tolerant to unknown provider-extension fields
	// (which are discarded), while the modeled presence checks below still
	// reject every semantic violation — including the non-streaming message
	// arm, which is a KNOWN field and a STRUCTURAL rejection (a streaming chunk
	// carries only deltas), never a silently-dropped extension.
	var shadow chatStreamChunkShadow
	if err := wire.DecodeTolerant(data, &shadow); err != nil {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			fmt.Errorf("chat stream chunk: %w", err),
		)
	}
	if shadow.Object == nil || *shadow.Object != "chat.completion.chunk" {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			fmt.Errorf(
				"chat stream chunk object = %q, want \"chat.completion.chunk\"",
				derefStr(shadow.Object),
			),
		)
	}
	// The optional usage-only tail chunk (stream_options.include_usage) carries
	// only usage accounting with an empty choices list; some providers omit its
	// id/model/created identity. Identity is meaningless on a content-less tail,
	// so only content-bearing chunks require it.
	usageOnlyTail := shadow.Usage != nil && len(shadow.Choices) == 0
	if !usageOnlyTail && shadow.ID == "" {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			errors.New("chat stream chunk id is empty"),
		)
	}
	if !usageOnlyTail && shadow.Model == "" {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			errors.New("chat stream chunk model is empty"),
		)
	}
	if !usageOnlyTail && shadow.Created == nil {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			errors.New("chat stream chunk created is missing"),
		)
	}
	// n=1: a chunk with more than one choice is an upstream protocol error.
	if len(shadow.Choices) > 1 {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			errors.New("chat stream chunk has more than one choice; n=1 required"),
		)
	}
	for i := range shadow.Choices {
		choice := &shadow.Choices[i]
		if choice.Index == nil {
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				fmt.Errorf("chat stream chunk choice %d has no index", i),
			)
		}
		if *choice.Index != 0 {
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				fmt.Errorf(
					"chat stream chunk choice index = %d; n=1 requires index 0",
					*choice.Index,
				),
			)
		}
		if choice.Delta == nil {
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				errors.New("chat stream chunk choice has no delta"),
			)
		}
		// A non-streaming message arm is a STRUCTURAL rejection: the
		// streaming surface carries only deltas, so a chunk carrying a
		// message arm is corrupt upstream wire. This is a KNOWN field (the
		// non-streaming arm), not a provider extension, so it does not
		// weaken the envelope's unknown-field tolerance. Rejecting it here
		// prevents the message-arm content from being silently dropped.
		if len(choice.Message) > 0 {
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				errors.New("chat stream chunk choice carries a non-streaming message arm; the streaming surface carries only deltas"),
			)
		}
		// A present delta role must be assistant: a non-assistant role is
		// corrupt upstream wire, never relabeled as assistant output.
		if choice.Delta.Role != nil && *choice.Delta.Role != "assistant" {
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				fmt.Errorf(
					"chat stream chunk delta role = %q, want assistant",
					*choice.Delta.Role,
				),
			)
		}
		// The pinned streaming tool-call fragment requires an explicit
		// index; a present type must be function (the pinned contract marks
		// type optional, so an absent type is accepted).
		for callIndex, call := range choice.Delta.ToolCalls {
			if call.Index == nil {
				return ChatStreamResponse{}, upstreamWireError(
					UpstreamChatCompletions,
					http.StatusOK,
					fmt.Errorf("chat stream chunk tool call %d has no index", callIndex),
				)
			}
			if call.Type != nil && *call.Type != "function" {
				return ChatStreamResponse{}, upstreamWireError(
					UpstreamChatCompletions,
					http.StatusOK,
					fmt.Errorf(
						"chat stream chunk tool call %d type = %q, want function",
						callIndex,
						*call.Type,
					),
				)
			}
		}
		// The legacy non-tool_calls function_call fragment spelling is a KNOWN
		// official field of the pinned revision: the single invocation maps to
		// one canonical tool call with a synthesized id, never a silent
		// drop (which would leave the client with a tool_use stop reason
		// and no tool call). A delta carrying both spellings at once is a
		// contradictory union.
		if len(choice.Delta.FunctionCall) > 0 {
			trimmed := bytes.TrimSpace(choice.Delta.FunctionCall)
			if !bytes.Equal(trimmed, []byte("null")) {
				if len(choice.Delta.ToolCalls) > 0 {
					return ChatStreamResponse{}, upstreamWireError(
						UpstreamChatCompletions,
						http.StatusOK,
						errors.New("chat stream chunk delta carries both tool_calls and the legacy function_call spelling"),
					)
				}
				if _, _, hasContent, emptyFragment, err := parseLegacyFunctionCallFragment(trimmed); err != nil {
					return ChatStreamResponse{}, upstreamWireError(
						UpstreamChatCompletions,
						http.StatusOK,
						fmt.Errorf("chat stream chunk legacy function_call: %w", err),
					)
				} else if !hasContent && !emptyFragment {
					return ChatStreamResponse{}, upstreamWireError(
						UpstreamChatCompletions,
						http.StatusOK,
						errors.New("chat stream chunk legacy function_call has neither name nor arguments"),
					)
				}
			}
		}
	}
	// The pinned CompletionUsage requires all three totals, but an honest
	// upstream sometimes omits exactly one on a tail. A single ABSENT total is
	// DERIVED when the other two are present (input + output = total, or
	// total - the present component), never defaulted to zero; the derivation
	// is recorded per exchange so it stays observable. An explicitly NULL
	// total is an illegal value for a modeled scalar and rejects, never
	// derived. Two or more absent totals cannot be derived truthfully and
	// remain a typed upstream wire error.
	if shadow.Usage != nil {
		// The pointer totals conflate null with absent, so the raw wire
		// bytes carry the null evidence: an explicit null is a malformed
		// modeled scalar and rejects before any derivation decision.
		for _, name := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
			if chatUsageKeyIsNull(data, name) {
				return ChatStreamResponse{}, upstreamWireError(
					UpstreamChatCompletions,
					http.StatusOK,
					fmt.Errorf("chat stream chunk usage %s must not be null", name),
				)
			}
		}
		prompt, completion, total := shadow.Usage.PromptTokens, shadow.Usage.CompletionTokens, shadow.Usage.TotalTokens
		switch {
		case prompt != nil && completion != nil && total == nil:
			derived, ok := checkedAdd(*prompt, *completion)
			if !ok {
				// The source's own arithmetic is never a failure: saturate and
				// record, exactly as usage_clamp.go's derivedUsageTotal does for
				// the non-streaming renderer. Rejecting here would make the same
				// upstream bytes succeed on one direction and 502 on the other.
				derived = saturatingUsageSum(*prompt, *completion)
			}
			shadow.Usage.TotalTokens = &derived
			shadow.derivedTotal = "total_tokens"
		case prompt != nil && completion == nil && total != nil:
			derived, ok := checkedSub(*total, *prompt)
			if !ok {
				derived = saturatingUsageDifference(*total, *prompt)
			}
			shadow.Usage.CompletionTokens = &derived
			shadow.derivedTotal = "completion_tokens"
		case prompt == nil && completion != nil && total != nil:
			derived, ok := checkedSub(*total, *completion)
			if !ok {
				derived = saturatingUsageDifference(*total, *completion)
			}
			shadow.Usage.PromptTokens = &derived
			shadow.derivedTotal = "prompt_tokens"
		case prompt == nil || completion == nil || total == nil:
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				errors.New("chat stream chunk usage must carry prompt_tokens, completion_tokens, and total_tokens"),
			)
		}
	}
	var chunk ChatStreamResponse
	if err := json.Unmarshal(data, &chunk); err != nil {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			fmt.Errorf("chat stream chunk: %w", err),
		)
	}
	// Carry the derived-total marker onto the decoded chunk (the second pass
	// re-decodes the wire bytes and cannot see a derived value): the stream
	// state records one note per exchange when it absorbs the usage.
	chunk.DerivedUsageTotal = shadow.derivedTotal
	if shadow.derivedTotal != "" && shadow.Usage != nil {
		// The wire re-decode left the derived component absent; write the
		// derived values back so downstream sees a complete usage object.
		if chunk.Usage == nil {
			chunk.Usage = &ChatLLMUsage{}
		}
		if shadow.Usage.PromptTokens != nil {
			chunk.Usage.PromptTokens = *shadow.Usage.PromptTokens
		}
		if shadow.Usage.CompletionTokens != nil {
			chunk.Usage.CompletionTokens = *shadow.Usage.CompletionTokens
		}
		if shadow.Usage.TotalTokens != nil {
			chunk.Usage.TotalTokens = *shadow.Usage.TotalTokens
		}
	}
	// Map the legacy function_call fragment to the tool_calls spelling so
	// the downstream state machine accumulates it exactly like a tool call
	// fragment. The id is synthesized deterministically from the chunk id
	// (stable across the stream); the conversion to canonical tool calls
	// records the synthesis as an ungated note.
	if len(chunk.Choices) > 0 && chunk.Choices[0].Delta != nil && len(chunk.Choices[0].Delta.FunctionCall) > 0 {
		raw := bytes.TrimSpace(chunk.Choices[0].Delta.FunctionCall)
		if bytes.Equal(raw, []byte("null")) {
			chunk.Choices[0].Delta.FunctionCall = nil
			return chunk, nil
		}
		name, argsPtr, hasContent, emptyFragment, err := parseLegacyFunctionCallFragment(raw)
		if err != nil {
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				fmt.Errorf("chat stream chunk legacy function_call: %w", err),
			)
		}
		if emptyFragment {
			chunk.Choices[0].Delta.FunctionCall = nil
			return chunk, nil
		}
		if !hasContent {
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				errors.New("chat stream chunk legacy function_call has neither name nor arguments"),
			)
		}
		if len(chunk.Choices[0].Delta.ToolCalls) > 0 {
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				errors.New("chat stream chunk delta carries both tool_calls and the legacy function_call spelling"),
			)
		}
		synthID := chunk.ID + ":legacy-function-call-0"
		idx := 0
		typ := "function"
		args := ""
		if argsPtr != nil {
			args = *argsPtr
		}
		chunk.Choices[0].Delta.ToolCalls = []ChatToolCallDelta{{
			Index: &idx,
			ID:    &synthID,
			Type:  &typ,
			Function: ChatToolCallFunction{
				Name:      name,
				Arguments: args,
			},
		}}
		chunk.Choices[0].Delta.FunctionCall = nil
	}
	return chunk, nil
}
