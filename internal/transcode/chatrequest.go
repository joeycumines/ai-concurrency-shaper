package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
)

func RenderChatRequest(
	request CanonicalRequest,
	context *ExchangeContext,
	capabilities ChatCapabilities,
) ([]byte, ConversionReport, error) {
	var report ConversionReport
	if err := ValidateCanonicalRequest(request); err != nil {
		return nil, report, err
	}
	if err := RequirePortableArtifacts(
		request,
		UpstreamChatCompletions,
		context.lossPolicy(),
		&report,
	); err != nil {
		return nil, report, err
	}

	model := request.ClientModel
	if context != nil && context.UpstreamModel != "" {
		model = context.UpstreamModel
	}

	out := ChatRequest{
		Model:               model,
		Temperature:         request.Temperature,
		TopP:                request.TopP,
		MaxCompletionTokens: request.MaxOutputTokens,
		Stream:              new(request.Stream),
		N:                   new(1),
	}
	if len(request.Metadata) > 0 {
		if !capabilities.Metadata {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureMetadata,
				"metadata",
				"request metadata is not forwarded to the configured chat provider",
			); err != nil {
				return nil, report, err
			}
		} else {
			out.Metadata = anyMap(request.Metadata)
		}
	}
	// The official stream protocol sends a final usage-only chunk before
	// [DONE] when include_usage is requested; the state machine consumes it
	// into the terminal envelope's usage. It is
	// requested whenever the exchange streams, so Messages clients receive
	// real totals instead of fabricated zero usage.
	if request.Stream {
		out.StreamOptions = &ChatStreamOptions{IncludeUsage: new(true)}
	}

	// Responses request fields that Chat can express are rendered; the rest
	// must be rejected or approved as losses, never silently dropped (the
	// strict-subset invariant).
	if echo := context.OriginalResponsesRequest; echo != nil {
		out.User = echo.User
		out.Store = echo.Store
		// Opt-in continuity (statefulness decision: OFF by default). When
		// the store resolved this request's previous_response_id, the
		// reconstructed history is already prepended to the rendered turns,
		// so the field is consumed, not lost: skip the existing observable
		// loss (the hit Note at decode already records the fact). A miss
		// (RequestDepth 0) falls through to the loss, never a failure.
		if echo.PreviousResponseID != nil && context.RequestDepth == 0 {
			if err := report.Lose(
				context.lossPolicy(),
				FeaturePreviousResponseID,
				"request.previous_response_id",
				"the Responses previous_response_id field is not portable to a chat upstream",
			); err != nil {
				return nil, report, err
			}
		}
		if echo.TopLogprobs != nil {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureRequestTopLogprobs,
				"request.top_logprobs",
				"the Responses top_logprobs field is not portable to a chat upstream",
			); err != nil {
				return nil, report, err
			}
		}
		if echo.ServiceTier != nil {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureRequestServiceTier,
				"request.service_tier",
				"the Responses service_tier field is not portable to a chat upstream",
			); err != nil {
				return nil, report, err
			}
		}
		if echo.Truncation != nil {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureRequestTruncation,
				"request.truncation",
				"the Responses truncation field is not portable to a chat upstream",
			); err != nil {
				return nil, report, err
			}
		}
	}

	// Developer role: preserved when supported, otherwise an approved loss.
	for _, turn := range request.Turns {
		if turn.Role != CanonicalDeveloper {
			continue
		}
		if !capabilities.DeveloperRole {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureDeveloperRole,
				"messages[].role",
				"developer role is not supported by the configured chat provider",
			); err != nil {
				return nil, report, err
			}
		}
	}

	// Messages: system/developer turns become system/developer messages; user
	// turns carry text and tool results; assistant turns carry text, refusal,
	// and tool calls.
	//
	// System-channel placement: open-weights chat templates
	// (Qwen/Llama/DeepSeek Jinja) reject ANY role:system message after
	// index 0 — including a second leading one. Unless the configured
	// upstream declared the system_anywhere capability, system-channel turns
	// (CanonicalSystem, plus CanonicalDeveloper when the developer-role
	// capability is off) consolidate into exactly one leading system
	// message: a turn following dialog turns is the policy-gated
	// mid_conversation_system position loss, and a leading-only merge of
	// multiple system turns is a sanctioned note under the same key.
	rendered := make([]ChatMessage, 0, len(request.Turns))
	systemChannel := make([]bool, 0, len(request.Turns))
	// The namespace qualifier a replayed call carries is resolved for THIS
	// exchange: the canonical part keeps client-sent data, and retained
	// history never passes through decode, so the decision cannot live there.
	replay := newNamespaceReplayResolve(context.toolNames(), request.Tools, context.lossPolicy(), &report)
	// retainedChannel marks each rendered message as reconstructed from the
	// continuity store rather than authored by this request, so a request-side
	// gate can tell the two apart. The retained turns are a prefix of Turns.
	retainedChannel := make([]bool, 0, len(request.Turns))
	turnIndex := 0
	for _, turn := range request.Turns {
		retained := turnIndex < request.RetainedTurns
		turnPath := fmt.Sprintf("turns[%d]", turnIndex)
		turnIndex++
		switch turn.Role {
		case CanonicalSystem:
			message, err := canonicalTextTurnToChatMessage(
				turn,
				ChatMessageRoleSystem,
				context.lossPolicy(),
				&report,
			)
			if err != nil {
				return nil, report, err
			}
			rendered = append(rendered, message)
			systemChannel = append(systemChannel, true)
			retainedChannel = append(retainedChannel, retained)

		case CanonicalDeveloper:
			role := ChatMessageRoleDeveloper
			channel := false
			if !capabilities.DeveloperRole {
				role = ChatMessageRoleSystem
				channel = true
			}
			message, err := canonicalTextTurnToChatMessage(
				turn,
				role,
				context.lossPolicy(),
				&report,
			)
			if err != nil {
				return nil, report, err
			}
			rendered = append(rendered, message)
			systemChannel = append(systemChannel, channel)
			retainedChannel = append(retainedChannel, retained)

		case CanonicalUser:
			messages, err := canonicalUserTurnToChatMessages(
				turn,
				capabilities,
				&report,
				context.lossPolicy(),
			)
			if err != nil {
				return nil, report, err
			}
			for _, message := range messages {
				rendered = append(rendered, message)
				systemChannel = append(systemChannel, false)
				retainedChannel = append(retainedChannel, retained)
			}

		case CanonicalAssistant:
			message, err := canonicalAssistantTurnToChatMessage(turn, turnPath, replay)
			if err != nil {
				return nil, report, err
			}
			rendered = append(rendered, message)
			systemChannel = append(systemChannel, false)
			retainedChannel = append(retainedChannel, retained)
		}
	}

	if capabilities.SystemAnywhere {
		// Fidelity mode for upstreams that accept system messages
		// anywhere: positional rendering exactly as decoded, no report
		// entries.
		out.Messages = rendered
	} else {
		var (
			leading   []ChatMessage // system-channel messages before any dialog turn
			midDialog []ChatMessage // system-channel messages after a dialog turn
			dialog    []ChatMessage
		)
		sawDialog := false
		// midDialogAllRetained tracks whether every mid-dialog system message
		// came from the continuity store rather than this request. A
		// request-side gate polices CLIENT input, so a position the client's
		// own request never created must not be charged to it: a retained
		// system turn after dialog turns is the proxy's own history, already
		// consolidated once when the chain was first rendered. The encoding is
		// identical; only the policy decision differs, so it is recorded as a
		// Note (visible, never silent) instead of a loss that can reject a
		// request the client authored cleanly.
		midDialogAllRetained := true
		for i, message := range rendered {
			if !systemChannel[i] {
				// Only a turn the CLIENT authored may move a later client
				// system turn out of the leading position. Retained history
				// sits in front of the client's own turns, so counting it
				// would silently demote a client's instructions from index 0
				// to a mid-conversation system turn and then charge the
				// client for the position the proxy created.
				if !retainedChannel[i] {
					sawDialog = true
				}
				dialog = append(dialog, message)
				continue
			}
			if sawDialog {
				midDialog = append(midDialog, message)
				if !retainedChannel[i] {
					midDialogAllRetained = false
				}
			} else {
				leading = append(leading, message)
			}
		}
		switch {
		case len(midDialog) > 0 && midDialogAllRetained:
			if err := report.Note(
				FeatureMidConversationSystem,
				"messages[]",
				"retained conversation history carried system turns after dialog turns; they consolidate into the leading system message (the position was already lost when the chain was first rendered, so it is not re-charged to this request)",
			); err != nil {
				return nil, report, err
			}
			out.Messages = append(out.Messages, joinChatSystemMessages(leading, midDialog))
		case len(midDialog) > 0:
			// The position/timing of system-channel content following
			// dialog turns cannot survive; the consolidation is the
			// approved loss.
			if err := report.Lose(
				context.lossPolicy(),
				FeatureMidConversationSystem,
				"messages[]",
				"mid-conversation system turns cannot keep their position in a chat request; system-channel turns consolidate into one leading system message (position/timing lost, content and authority preserved)",
			); err != nil {
				return nil, report, err
			}
			out.Messages = append(out.Messages, joinChatSystemMessages(leading, midDialog))
		case len(leading) > 1:
			// Leading-only consolidation: open-weights templates reject a
			// second system message even at index 1, so the merge is
			// unconditional — but sanctioned and observable, never silent.
			if err := report.Note(
				FeatureMidConversationSystem,
				"messages[]",
				"multiple leading system turns consolidate into one leading system message",
			); err != nil {
				return nil, report, err
			}
			out.Messages = append(out.Messages, joinChatSystemMessages(leading, nil))
		case len(leading) == 1:
			// The common single-leading-system case renders exactly one
			// system message at index 0 with its original blocks.
			out.Messages = append(out.Messages, leading[0])
		}
		out.Messages = append(out.Messages, dialog...)
	}

	// A source request with no Chat-representable messages is a
	// client-dialect invalid-request error before any upstream request:
	// messages:null or an invented empty user prompt are never emitted.
	// A source request with no Chat-representable messages is a
	// client-dialect invalid-request error before any upstream request.
	// A message whose content is a single EMPTY text block counts as
	// non-representable: it arises only when every part of a system turn was
	// dropped under an approved loss, and admitting it would send the
	// upstream a conversation with no user turn at all. This keeps the
	// Messages-to-Chat direction in agreement with the Responses target,
	// which rejects the same source shape for the same reason.
	isDegenerate := func(messages []ChatMessage) bool {
		// Degenerate means EVERY message is a single EMPTY text block - the
		// exact shape the empty-block synthesis produces when all of a
		// system turn's parts were dropped. An assistant or tool message
		// carrying real content is representable and must NOT be rejected
		// just because the conversation has no user turn.
		for _, m := range messages {
			if m.Content == nil {
				return false
			}
			blocks := m.Content.ContentBlocks
			if len(blocks) != 1 || blocks[0].Type != ChatContentBlockTypeText {
				return false
			}
			if blocks[0].Text == nil || *blocks[0].Text != "" {
				return false
			}
		}
		return true
	}
	if len(out.Messages) == 0 || isDegenerate(out.Messages) {
		return nil, report, errors.New(
			"the source request has no Chat-representable messages",
		)
	}

	// The reminder is only truthful if the request actually declares the
	// protocol it describes. Without this gate an operator could prime a
	// mount whose request declares no sub-agent tools at all, and the model
	// would be told to call spawn_agent against an upstream that was never
	// sent it. Gate on the FLAT names this request will actually render, so
	// the check stays true after namespace flattening: a top-level tool
	// spelled like one of the five, or any child of the multi_agent_v1
	// namespace (already flattened to its flat name at decode).
	if capabilities.MultiAgentPriming && declaresMultiAgentTools(request.Tools, context) {
		primed, err := applyMultiAgentPriming(out.Messages, &report)
		if err != nil {
			return nil, report, err
		}
		out.Messages = primed
	}

	// Tools. The canonical schema is passed through byte-exact: it was
	// validated as exactly one JSON object at decode, so no
	// decode-and-remarshal round trip can corrupt its numbers. An absent schema is omitted, matching the source's
	// presence.
	boundsNoted := false
	for _, tool := range request.Tools {
		description := tool.Description
		parameters := tool.JSONSchema
		if !capabilities.Float64Bounds {
			rewritten, changed, err := dropVacuousNumericBounds(parameters)
			if err != nil {
				return nil, report, fmt.Errorf("tool %q schema: %w", tool.Name, err)
			}
			if changed {
				parameters = rewritten
				if !boundsNoted {
					boundsNoted = true
					if err := report.Note(
						FeatureVacuousSchemaBounds,
						"tools[].function.parameters",
						"numeric bounds at ±float64 max (which admit every finite number) are removed from tool schemas",
					); err != nil {
						return nil, report, err
					}
				}
			}
		}
		out.Tools = append(out.Tools, ChatTool{
			Type: ChatToolTypeFunction,
			Function: &ChatToolFunction{
				Name:        tool.Name,
				Description: &description,
				Parameters:  parameters,
				Strict:      tool.Strict,
			},
		})
	}

	// Tool choice.
	if request.ToolChoice != nil {
		choice, err := canonicalToolChoiceToChat(*request.ToolChoice)
		if err != nil {
			return nil, report, err
		}
		out.ToolChoice = &choice
	}

	// Parallel tool calls.
	if request.ParallelTools != nil {
		if !capabilities.ParallelToolCalls {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureParallelToolCalls,
				"parallel_tool_calls",
				"parallel tool calls are not supported by the configured chat provider",
			); err != nil {
				return nil, report, err
			}
		} else {
			out.ParallelToolCalls = request.ParallelTools
		}
	}

	// Stop sequences.
	if len(request.StopSequences) > 0 {
		if !capabilities.StopSequences {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureStopSequences,
				"stop_sequences",
				"stop sequences are not supported by the configured chat provider",
			); err != nil {
				return nil, report, err
			}
		} else {
			out.Stop = &ChatStop{Strs: request.StopSequences}
		}
	}

	// Structured output.
	if request.StructuredOutput != nil {
		if !capabilities.StructuredOutputs {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureStructuredOutput,
				"text.format",
				"structured output is not supported by the configured chat provider",
			); err != nil {
				return nil, report, err
			}
		} else {
			// The canonical schema is passed through byte-exact (validated
			// as exactly one JSON object at decode).
			out.ResponseFormat = &ChatResponseFormat{
				Type: ChatResponseFormatJSONSchema,
				JSONSchema: &ChatJSONSchemaFormat{
					Name:        request.StructuredOutput.Name,
					Description: &request.StructuredOutput.Description,
					Schema:      request.StructuredOutput.Schema,
					Strict:      new(request.StructuredOutput.Strict),
				},
			}
		}
	}

	// Reasoning effort (Responses source only).
	if context != nil && context.OriginalResponsesRequest != nil &&
		context.OriginalResponsesRequest.Reasoning != nil &&
		context.OriginalResponsesRequest.Reasoning.Effort != nil {
		if !capabilities.ReasoningEffort {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureRequestReasoning,
				"reasoning.effort",
				"reasoning effort is not supported by the configured chat provider",
			); err != nil {
				return nil, report, err
			}
		} else {
			out.ReasoningEffort = context.OriginalResponsesRequest.Reasoning.Effort
		}
	}
	// The reasoning summary style has no Chat representation: only the
	// effort is portable, so the summary request is a loss/reject decision.
	if context != nil && context.OriginalResponsesRequest != nil &&
		context.OriginalResponsesRequest.Reasoning != nil &&
		context.OriginalResponsesRequest.Reasoning.Summary != nil {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureReasoningSummary,
			"reasoning.summary",
			"the reasoning summary style cannot be reproduced in a chat request",
		); err != nil {
			return nil, report, err
		}
	}

	// Anthropic Messages thinking (Messages source only): the client's
	// thinking request maps to the chat reasoning_effort parameter when the
	// chat provider supports it (ReasoningEffort capability). The mapping is
	// explicit and documented — never silent:
	//
	//  - "adaptive"   the client delegated the thinking decision to the
	//                 model; chat's absent reasoning_effort is the exact
	//                 semantic (the provider applies its own default).
	//  - "disabled"   no thinking requested; nothing is emitted.
	//  - "enabled"    the explicit budget_tokens maps through the
	//                 documented deterministic threshold table in
	//                 thinkingBudgetToEffort; the mapping is reported as a
	//                 named Note so it is observable on every exchange.
	//
	// Without the ReasoningEffort capability an enabled budget is a
	// loss/reject decision (request_reasoning), never a silent drop.
	if request.Thinking != nil {
		switch request.Thinking.Type {
		case "enabled":
			if request.Thinking.BudgetTokens == nil {
				// An "enabled" thinking request without a budget is provider
				// intent the chat target cannot reproduce. Decode rejects
				// this shape today; a hand-built CanonicalRequest or a future
				// decode relaxation must never pass it silently (the never-silent invariant).
				if err := report.Note(
					FeatureRequestReasoning,
					"thinking",
					"thinking enabled without a budget maps to the chat provider's default reasoning effort",
				); err != nil {
					return nil, report, err
				}
			} else if !capabilities.ReasoningEffort {
				if err := report.Lose(
					context.lossPolicy(),
					FeatureRequestReasoning,
					"thinking",
					"the thinking budget cannot be reproduced by the configured chat provider",
				); err != nil {
					return nil, report, err
				}
			} else {
				effort := thinkingBudgetToEffort(*request.Thinking.BudgetTokens)
				out.ReasoningEffort = &effort
				if err := report.Note(
					FeatureRequestReasoning,
					"thinking",
					fmt.Sprintf(
						"Anthropic thinking budget %d mapped to chat reasoning_effort %q",
						*request.Thinking.BudgetTokens,
						effort,
					),
				); err != nil {
					return nil, report, err
				}
			}
		case "adaptive":
			if err := report.Note(
				FeatureRequestReasoning,
				"thinking",
				"adaptive thinking maps to the chat provider's default reasoning effort",
			); err != nil {
				return nil, report, err
			}
		case "disabled":
			// An explicit client-asserted no-thinking is observable, like
			// adaptive: the elision maps to the absence of chat
			// reasoning_effort and is reported.
			if err := report.Note(
				FeatureRequestReasoning,
				"thinking",
				"thinking disabled maps to the absence of chat reasoning_effort",
			); err != nil {
				return nil, report, err
			}
		}
	}

	// Profile-resolved reasoning tier: when the client specified neither a
	// reasoning effort nor a thinking control, the profile's tier fills the
	// gap. The gate is on the client's INTENT, not on whether an effort
	// happened to be materialized — thinking "disabled" and "adaptive" are
	// recorded as notes that deliberately emit no effort, and materializing
	// one here would contradict the note and override the client's explicit
	// choice. Applied only when the capability is granted; otherwise the tier
	// is inert (the upstream cannot honor it and no client-requested feature
	// is being dropped).
	if context != nil && context.ResolvedReasoningTier != "" &&
		request.Thinking == nil &&
		capabilities.ReasoningEffort && out.ReasoningEffort == nil {
		effort := context.ResolvedReasoningTier
		out.ReasoningEffort = &effort
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, report, err
	}
	return body, report, nil
}

// transcodeToolResult applies the tool_result.is_error decision when
// rendering a canonical tool result to a target that cannot carry the error
// status. It is rejected by default (FeatureToolResultErrorStatus); a permissive
// policy may encode the status into visible content with the named
// "error_status_prefix" encoding, which is reported because it invents text
func transcodeToolResult(
	result CanonicalFunctionResult,
	policy LossPolicy,
	report *ConversionReport,
	target string,
	path string,
) ([]CanonicalPart, error) {
	if !result.IsError {
		return result.Parts, nil
	}
	if err := report.Lose(
		policy,
		FeatureToolResultErrorStatus,
		path,
		"the tool result error status cannot be reproduced in the "+target+
			" dialect; the permissive encoding is the visible error_status_prefix text",
	); err != nil {
		return nil, err
	}
	return append(
		[]CanonicalPart{CanonicalText{Text: "[tool_result_error]"}},
		result.Parts...,
	), nil
}

// thinkingBudgetToEffort maps an Anthropic Messages thinking budget_tokens
// value to the reasoning_effort vocabulary. The thresholds are the
// documented midpoints of the classic Claude Code effort budgets
// (minimal ~ 256, low ~ 1024, medium ~ 4096, high ~ 16384); the mapping is
// deterministic, capped at "high" (the canonical vocabulary includes xhigh
// and max, but no documented budget maps above high, so the projection never
// synthesizes one), and reported as a named Note on every mapped exchange.
func thinkingBudgetToEffort(budget int) string {
	switch {
	case budget < 1024:
		return "minimal"
	case budget < 4096:
		return "low"
	case budget < 16384:
		return "medium"
	default:
		return "high"
	}
}

// anyMap converts a string map to an any-typed map for wire rendering.
func anyMap(m map[string]string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for key, value := range m {
		out[key] = value
	}
	return out
}
