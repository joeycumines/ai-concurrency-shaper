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
	// strict-subset invariant). A nil context is the ordinary strict render,
	// exactly as for the other three request renderers.
	var echo *ResponsesRequestEcho
	if context != nil {
		echo = context.OriginalResponsesRequest
	}
	if echo != nil {
		out.User = echo.User
		out.Store = echo.Store
	}
	// Every non-portable envelope field takes the shared decision, so Chat,
	// Gemini and Messages cannot disagree about the key or the path. The
	// previous_response_id loss is skipped by the helper when the continuity
	// store resolved it: the reconstructed history is already prepended, so
	// the field is consumed rather than lost, and the hit Note at decode
	// already records the fact. Chat models user and store natively, so they
	// are rendered above rather than charged.
	if err := responsesEchoLosses(context, &report, echoCarriage{
		Effort: capabilities.ReasoningEffort,
		User:   true,
		Store:  true,
	}); err != nil {
		return nil, report, err
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
	// multiple system turns is the shared join decision.
	rendered := make([]ChatMessage, 0, len(request.Turns))
	// instructionChannel marks each rendered message as an INSTRUCTION (a
	// system or developer turn) rather than dialog. It is the placement fact
	// used for emptiness filtering and join contribution; it is NOT the
	// provenance fact, which retainedChannel owns.
	instructionChannel := make([]bool, 0, len(request.Turns))
	// consolidates marks the instruction messages that join the ONE leading
	// system message: a system turn always does, and a developer turn does
	// only when the developer-role capability forces it onto the system role.
	// With the capability granted the developer message is carried in place,
	// so the client's encounter order survives.
	consolidates := make([]bool, 0, len(request.Turns))
	// The namespace qualifier a replayed call carries is resolved for THIS
	// exchange: the canonical part keeps client-sent data, and retained
	// history never passes through decode, so the decision cannot live there.
	replay := newNamespaceReplayResolve(context.toolNames(), request.Tools, context.lossPolicy(), &report)
	// retainedChannel marks each rendered message as reconstructed from the
	// continuity store rather than authored by this request, so a request-side
	// gate can tell the two apart. The retained turns are a prefix of Turns.
	retainedChannel := make([]bool, 0, len(request.Turns))
	// sourceTurn records, per rendered message, the canonical turn index it
	// came from. One canonical turn can render as several messages (a user
	// turn splits around its tool results), so the report path must name the
	// turn, never the rendered position.
	sourceTurn := make([]int, 0, len(request.Turns))
	turnIndex := 0
	for _, turn := range request.Turns {
		retained := turnIndex < request.RetainedTurns
		turnPath := fmt.Sprintf("turns[%d]", turnIndex)
		index := turnIndex
		turnIndex++
		switch turn.Role {
		case CanonicalSystem:
			message, err := canonicalTextTurnToChatMessage(
				turn,
				turnPath,
				ChatMessageRoleSystem,
				context.lossPolicy(),
				&report,
			)
			if err != nil {
				return nil, report, err
			}
			rendered = append(rendered, message)
			instructionChannel = append(instructionChannel, true)
			consolidates = append(consolidates, true)
			retainedChannel = append(retainedChannel, retained)
			sourceTurn = append(sourceTurn, index)

		case CanonicalDeveloper:
			role := ChatMessageRoleDeveloper
			// A developer turn is an INSTRUCTION either way; only the role it
			// renders under depends on the capability.
			joinsSystem := false
			if !capabilities.DeveloperRole {
				role = ChatMessageRoleSystem
				joinsSystem = true
			}
			message, err := canonicalTextTurnToChatMessage(
				turn,
				turnPath,
				role,
				context.lossPolicy(),
				&report,
			)
			if err != nil {
				return nil, report, err
			}
			// The contribution comes FIRST: a developer turn that renders no
			// text occupies no role and no position, so charging it would
			// describe a collapse that never happened.
			if joinsSystem && !chatMessageTextIsEmpty(message) {
				// The role distinction is dropped for EVERY developer turn,
				// retained or not; only the policy decision differs, and
				// chargePosition owns that distinction so a retained turn
				// cannot reject a cleanly authored request.
				if err := chargePosition(
					request,
					index,
					FeatureDeveloperRole,
					turnPath,
					"developer role is not supported by the configured chat provider; the turn renders with the system role",
					&report,
					context.lossPolicy(),
				); err != nil {
					return nil, report, err
				}
			}
			rendered = append(rendered, message)
			instructionChannel = append(instructionChannel, true)
			consolidates = append(consolidates, joinsSystem)
			retainedChannel = append(retainedChannel, retained)
			sourceTurn = append(sourceTurn, index)

		case CanonicalUser:
			messages, err := canonicalUserTurnToChatMessages(
				turn,
				turnPath,
				capabilities,
				&report,
				context.lossPolicy(),
			)
			if err != nil {
				return nil, report, err
			}
			for _, message := range messages {
				rendered = append(rendered, message)
				instructionChannel = append(instructionChannel, false)
				consolidates = append(consolidates, false)
				retainedChannel = append(retainedChannel, retained)
				sourceTurn = append(sourceTurn, index)
			}

		case CanonicalAssistant:
			message, err := canonicalAssistantTurnToChatMessage(turn, turnPath, replay)
			if err != nil {
				return nil, report, err
			}
			rendered = append(rendered, message)
			instructionChannel = append(instructionChannel, false)
			consolidates = append(consolidates, false)
			retainedChannel = append(retainedChannel, retained)
			sourceTurn = append(sourceTurn, index)
		}
	}

	if capabilities.SystemAnywhere {
		// Fidelity mode for upstreams that accept system messages
		// anywhere: positional rendering exactly as decoded, no report
		// entries. The one thing this mode does NOT preserve is an
		// INSTRUCTION message that carries no bytes at all: that shape is
		// proxy-invented (the renderer synthesizes an empty text block for a
		// turn whose parts were all dropped or scrubbed), and emitting it
		// would send the upstream an empty system or developer prompt the
		// client never wrote — the same rule the consolidated path and the
		// other three targets follow.
		filtered := make([]ChatMessage, 0, len(rendered))
		for i, message := range rendered {
			if instructionChannel[i] && chatMessageTextIsEmpty(message) {
				continue
			}
			filtered = append(filtered, message)
		}
		out.Messages = filtered
	} else {
		var (
			// systemMessages holds every consolidating instruction message in
			// ENCOUNTER order. The consolidated leading message joins their
			// text in that order, so separating the leading and mid-dialog
			// groups can never reorder the prompt the model sees.
			systemMessages []ChatMessage
			// contributing records the canonical index of every consolidating
			// instruction turn that actually produced text, which is what the
			// shared join decision counts.
			contributing []int
			// consolidatedAt is the index in rendered at which the consolidated
			// system message belongs: the first CONTRIBUTING consolidating
			// turn. -1 means nothing consolidates, so nothing is inserted.
			consolidatedAt = -1
		)
		sawClientDialog := false
		// sawRetainedDialog tracks dialog turns the continuity store
		// reconstructed. A request-side gate polices CLIENT input, so a
		// position the client's own request never created must not be charged
		// to it.
		sawRetainedDialog := false
		// keptSinceContributor records that an instruction message which renders
		// IN PLACE (a developer turn the capability lets us keep) has appeared
		// after the first contributing system turn. A later contributor's text
		// then jumps over that message into the consolidated prompt, which is a
		// real positional move. Two leading system turns with nothing surviving
		// between them are NOT charged a position: their merge is the
		// multiple_system_turns decision, which is what says the boundaries are
		// lost.
		keptSinceContributor := false
		for i, message := range rendered {
			if !instructionChannel[i] {
				if retainedChannel[i] {
					sawRetainedDialog = true
				} else {
					sawClientDialog = true
				}
				continue
			}
			if !consolidates[i] {
				// A developer turn carried in place under the developer-role
				// capability is an instruction but not dialog: it must not open
				// dialog, and it does not join the consolidated prompt because
				// it renders at its own position.
				if consolidatedAt >= 0 {
					keptSinceContributor = true
				}
				continue
			}
			// A turn that rendered nothing occupies no position and contributes
			// to no join: charging it would describe a move or a merge that
			// never happened.
			if chatMessageTextIsEmpty(message) {
				continue
			}
			firstContributor := consolidatedAt < 0
			systemMessages = append(systemMessages, message)
			contributing = append(contributing, sourceTurn[i])
			if firstContributor {
				consolidatedAt = i
			}
			// A client-authored instruction turn is mid-dialog only when the
			// CLIENT's own request has already opened dialog: retained history
			// sits in front of the client's turns, and letting it demote the
			// client's instructions would charge the client for a position the
			// proxy created. A retained instruction turn is mid-dialog when any
			// dialog precedes it, either the proxy's or the client's.
			mid := sawClientDialog
			if retainedChannel[i] {
				mid = sawRetainedDialog || sawClientDialog
			}
			if !mid && !keptSinceContributor {
				continue
			}
			// Each mid-dialog instruction turn is charged on its OWN
			// provenance, so a retained turn and a client turn in the same
			// conversation cannot be folded into one decision that hides which
			// one paid.
			if err := chargePosition(
				request,
				sourceTurn[i],
				FeatureMidConversationSystem,
				fmt.Sprintf("turns[%d]", sourceTurn[i]),
				"mid-conversation system turns cannot keep their position in a chat request; system-channel turns consolidate into one leading system message (position/timing lost, content and authority preserved)",
				&report,
				context.lossPolicy(),
			); err != nil {
				return nil, report, err
			}
		}
		if err := chargeSystemTurnJoin(
			request,
			contributing,
			"messages[0]",
			"multiple system turns cannot keep their boundaries in one leading chat system message; their text joins in order",
			&report,
			context.lossPolicy(),
		); err != nil {
			return nil, report, err
		}
		// Emit in encounter order. Consolidating instruction messages vanish
		// into the consolidated prompt, which takes the place of the first one
		// that contributed; every other message — including a developer message
		// the capability lets us keep, and including a dialog turn that
		// physically precedes the first contributing system turn — keeps the
		// position it rendered at.
		for i, message := range rendered {
			if i == consolidatedAt {
				out.Messages = append(out.Messages, consolidateChatSystemMessages(systemMessages))
				continue
			}
			if instructionChannel[i] && consolidates[i] {
				continue
			}
			// A developer message carried in place is still proxy-invented
			// when every part of its turn was dropped or scrubbed.
			if instructionChannel[i] && chatMessageTextIsEmpty(message) {
				continue
			}
			out.Messages = append(out.Messages, message)
		}
		if consolidatedAt < 0 && len(systemMessages) > 0 {
			// Unreachable while consolidatedAt is assigned above on every
			// contribution; kept honest rather than silently dropping the
			// prompt if that assignment ever changes.
			out.Messages = append([]ChatMessage{consolidateChatSystemMessages(systemMessages)}, out.Messages...)
		}
	}

	// A source request with no Chat-representable messages is a
	// client-dialect invalid-request error before any upstream request:
	// messages:null or an invented empty user prompt are never emitted. Every
	// contentless instruction message has already been filtered out, so an
	// empty list means the source had nothing a chat message could carry — the
	// same shape the Responses target rejects. A dialog turn the Messages
	// decoder preserved with an empty text part is DELIBERATELY kept: the turn
	// boundary is the point of that synthesis, and refusing it would delete a
	// turn the model is expected to see.
	if len(out.Messages) == 0 {
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
	if capabilities.MultiAgentPriming && declaresMultiAgentTools(request.Tools) {
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

	// Parallel tool calls, decided once by the shared helper. The capability
	// decides whether chat can CARRY the field at all; the helper decides
	// whether it should be emitted (a real setting with a call that can happen)
	// or only noted (an inert one, because no field may be written without
	// tools), and charges a refusable loss for a disable chat cannot express.
	if request.ParallelTools != nil {
		emit, err := chargeParallelTools(
			request,
			capabilities.ParallelToolCalls,
			"parallel_tool_calls",
			&report,
			context.lossPolicy(),
		)
		if err != nil {
			return nil, report, err
		}
		if emit {
			out.ParallelToolCalls = request.ParallelTools
		}
	}

	// Top-k sampling has no Chat representation: only a Messages or Gemini
	// target carries the cutoff, so a Messages-sourced top_k is a loss here.
	if request.TopK != nil {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureTopK,
			"top_k",
			"top-k sampling is not supported by the configured chat provider",
		); err != nil {
			return nil, report, err
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
			// The decision names the canonical feature: the loss is the
			// structured output, and the field that happens to carry it in one
			// dialect is not a path a Gemini-sourced schema would ever have.
			if err := report.Lose(
				context.lossPolicy(),
				FeatureStructuredOutput,
				"structured_output",
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

	// Reasoning effort (Responses source only). The loss decision for a target
	// that cannot reproduce it was taken by responsesEchoLosses above; here
	// the capability grant decides whether the value is emitted.
	if context != nil && context.OriginalResponsesRequest != nil &&
		context.OriginalResponsesRequest.Reasoning != nil &&
		context.OriginalResponsesRequest.Reasoning.Effort != nil &&
		capabilities.ReasoningEffort {
		out.ReasoningEffort = context.OriginalResponsesRequest.Reasoning.Effort
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
		// The client's thinking-visibility preference is a Messages-native
		// field with no Chat representation.
		if request.Thinking.Display != "" {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureRequestReasoning,
				"thinking.display",
				"the thinking display preference cannot be reproduced by a chat request",
			); err != nil {
				return nil, report, err
			}
		}
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
// value to the reasoning_effort vocabulary. The thresholds ARE the classic
// Claude Code effort budgets (low 1024, medium 4096, high 16384): a budget
// below the next tier's budget projects to the lower tier, so the mapping is a
// deterministic upper-bound projection rather than a midpoint search. It is
// capped at "high" (the canonical vocabulary includes xhigh and max, but no
// documented budget maps above high, so the projection never synthesizes one),
// and reported as a named Note on every mapped exchange.
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
