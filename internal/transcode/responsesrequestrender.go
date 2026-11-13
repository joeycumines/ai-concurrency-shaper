package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// RenderResponsesRequest renders the canonical IR into a Responses request
// body. System and developer turns become the instructions field; user and
// assistant turns become input items; function calls and results become their
// own items. Stop sequences have no Responses representation and are a loss
// or an error depending on policy.
func RenderResponsesRequest(
	request CanonicalRequest,
	context *ExchangeContext,
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

	var instructions *string
	input := make(ResponsesInputItems, 0)

	// Split turns into instructions (system/developer) and input items,
	// remembering each system-channel turn's position and whether a dialog
	// turn preceded it: both the position loss and the multi-turn decision are
	// adjudicated per turn below.
	type systemChannelTurn struct {
		turn      CanonicalTurn
		index     int
		midDialog bool
		developer bool
	}
	var systemTurns []systemChannelTurn
	// conversationTurns pairs each dialog turn with its CANONICAL index, so a
	// refusal or report entry can name the turn the operator actually sent
	// rather than a position in a filtered slice.
	var conversationTurns []struct {
		turn  CanonicalTurn
		index int
	}
	// sawClientDialog and sawRetainedDialog are tracked separately: a
	// system-channel turn the CLIENT authored is mid-dialog only once the
	// client's own request opened dialog, because retained history physically
	// precedes the client's turns and demoting a client's instructions for the
	// proxy's own history would charge the client for a position it never
	// created. A retained system turn is mid-dialog as soon as any dialog
	// precedes it.
	sawClientDialog := false
	sawRetainedDialog := false
	for i, turn := range request.Turns {
		retained := i < request.RetainedTurns
		switch turn.Role {
		case CanonicalSystem, CanonicalDeveloper:
			midDialog := sawClientDialog
			if retained {
				midDialog = sawRetainedDialog || sawClientDialog
			}
			systemTurns = append(systemTurns, systemChannelTurn{
				turn:      turn,
				index:     i,
				midDialog: midDialog,
				developer: turn.Role == CanonicalDeveloper,
			})
		default:
			if retained {
				sawRetainedDialog = true
			} else {
				sawClientDialog = true
			}
			conversationTurns = append(conversationTurns, struct {
				turn  CanonicalTurn
				index int
			}{turn: turn, index: i})
		}
	}

	// The create-request instructions is a plain string (the pinned
	// ResponseNewParams shape). Every system-channel turn's text joins into
	// that one string in encounter order, exactly as the Chat target joins its
	// system messages: a target that holds a single system field must not
	// discard the prompt because the source happened to split it. A non-text
	// part (an image or a document) takes the system_non_text_content
	// decision individually, and a mid-conversation or developer turn records
	// its own position decision, so nothing moves silently.
	var instructionsBuilder strings.Builder
	textParts := 0
	// contributing holds the index of every system-channel turn that actually
	// contributed text. A turn whose parts were all dropped (a non-text part
	// under an approved system_non_text_content) did not contribute, so it must
	// not make the join decision describe a merge that never happened.
	var contributing []int
	for _, channel := range systemTurns {
		turnPath := fmt.Sprintf("turns[%d]", channel.index)
		if channel.midDialog {
			if err := chargePosition(
				request,
				channel.index,
				FeatureMidConversationSystem,
				turnPath,
				"a system turn after a dialog turn moves into the single instructions string; its position is lost (content and authority preserved)",
				&report,
				context.lossPolicy(),
			); err != nil {
				return nil, report, err
			}
		}
		if channel.developer {
			if err := chargePosition(
				request,
				channel.index,
				FeatureDeveloperRole,
				turnPath,
				"the create-request instructions string is role-neutral; the developer-role distinction is lost",
				&report,
				context.lossPolicy(),
			); err != nil {
				return nil, report, err
			}
		}
		turnTextParts := 0
		for _, part := range channel.turn.Parts {
			text, ok := part.(CanonicalText)
			if !ok {
				if _, thinking := part.(CanonicalThinkingPart); thinking {
					// Marker-signature thinking is proxy-synthesized reasoning
					// the request path scrubs in every turn role; any other
					// signature was already refused by
					// ValidateCanonicalRequest.
					continue
				}
				if err := loseSystemPart(context.lossPolicy(), &report, part); err != nil {
					return nil, report, err
				}
				continue
			}
			// An empty text part carries no bytes: skipping it cannot lose
			// content, and joining it would invent a leading or doubled
			// separator that the model would see as prompt text.
			if text.Text == "" {
				continue
			}
			if instructionsBuilder.Len() > 0 {
				instructionsBuilder.WriteByte('\n')
			}
			instructionsBuilder.WriteString(text.Text)
			textParts++
			turnTextParts++
		}
		if turnTextParts > 0 {
			contributing = append(contributing, channel.index)
		}
	}
	// More than one CONTRIBUTING system turn cannot keep its own boundary in
	// one instructions string. The decision is the shared multiple_system_turns
	// one every target takes for this encoding: a policy-gated loss when the
	// client authored more than one contributing turn, a Note otherwise (the
	// surplus came from the continuity store's reconstructed prefix, already
	// decided when the chain was first rendered).
	if err := chargeSystemTurnJoin(
		request,
		contributing,
		"instructions",
		"multiple system turns cannot keep their boundaries in a single instructions string; their text joins in order",
		&report,
		context.lossPolicy(),
	); err != nil {
		return nil, report, err
	}
	if len(contributing) == 1 && textParts > 1 {
		if err := report.Note(
			FeatureMultipleSystemTurns,
			"instructions",
			"multiple system text parts join into one instructions string (instructions_text_join encoding)",
		); err != nil {
			return nil, report, err
		}
	}
	if instructionsBuilder.Len() > 0 {
		rendered := instructionsBuilder.String()
		instructions = &rendered
	}

	// Render user/assistant turns into input items, preserving order. A
	// turn's content parts become one easy message; function calls and
	// results become their own items after it.
	//
	// A source request with no conversation turns renders "input":[] — the
	// pinned Responses contract requires input, and an empty item list is
	// not a legal substitute for a missing conversation. This matches the
	// Messages→Chat direction, which rejects the same source shape
	// (the directions disagreed).
	if len(conversationTurns) == 0 {
		return nil, report, errors.New(
			"the source request has no turn that can be represented in the Responses input",
		)
	}
	for _, entry := range conversationTurns {
		turn := entry.turn
		// turnPath names the canonical turn the operator sent, matching the
		// path convention of every other render-side entry.
		turnPath := fmt.Sprintf("turns[%d]", entry.index)
		var contentParts []CanonicalPart
		for _, part := range turn.Parts {
			switch value := part.(type) {
			case CanonicalFunctionCall:
				if len(contentParts) > 0 {
					item, err := canonicalTurnPartsToEasyMessage(
						turn.Role,
						contentParts,
					)
					if err != nil {
						return nil, report, err
					}
					input = append(input, item)
					contentParts = nil
				}
				arguments := string(value.Arguments)
				if arguments == "" {
					arguments = "{}"
				}
				input = append(input, &ResponsesFunctionCallInput{
					Type:      "function_call",
					CallID:    value.CallID,
					Name:      value.Name,
					Namespace: value.Namespace,
					Arguments: arguments,
				})

			case CanonicalFunctionResult:
				if len(contentParts) > 0 {
					item, err := canonicalTurnPartsToEasyMessage(
						turn.Role,
						contentParts,
					)
					if err != nil {
						return nil, report, err
					}
					input = append(input, item)
					contentParts = nil
				}
				parts, err := transcodeToolResult(
					value,
					context.lossPolicy(),
					&report,
					"responses",
					"input[].function_call_output",
				)
				if err != nil {
					return nil, report, err
				}
				output, err := canonicalPartsToFunctionOutput(parts)
				if err != nil {
					return nil, report, err
				}
				input = append(input, &ResponsesFunctionCallOutputInput{
					Type:   "function_call_output",
					CallID: value.CallID,
					Output: output,
				})

			default:
				contentParts = append(contentParts, part)
			}
		}
		if len(contentParts) > 0 {
			item, err := canonicalTurnPartsToEasyMessage(turn.Role, contentParts)
			if err != nil {
				return nil, report, err
			}
			if item.Content.Text == nil && len(item.Content.Parts) == 0 {
				// Every part was scrubbed (marker-signature thinking is the only
				// part type this renderer drops), so the item would carry a
				// contentless union. Emitting it fails deep in the wire marshal
				// with "no selected variant", which describes a marshaller bug
				// rather than the request the operator sent; inventing an empty
				// text part would put prompt text on the wire the client never
				// wrote. The Messages decoder synthesizes an empty text part for
				// exactly this shape, so a decoder-produced request never
				// reaches here — this is the hand-built case, refused honestly
				// and at the canonical path.
				return nil, report, fmt.Errorf(
					"%s has no content that the Responses input can carry after scrubbing proxy-synthesized thinking",
					turnPath,
				)
			}
			input = append(input, item)
		}
	}

	out := openairesponses.Request{
		Model:             model,
		Instructions:      instructions,
		Input:             &ResponsesInput{Items: input},
		MaxOutputTokens:   request.MaxOutputTokens,
		ParallelToolCalls: request.ParallelTools,
		Temperature:       request.Temperature,
		TopP:              request.TopP,
		Metadata:          request.Metadata,
		// The stream field is emitted only when true, matching the
		// pre-contract behavior (a non-streaming request body omits it).
		Stream: wire.Field[bool]{Value: true, Present: request.Stream},
	}

	// Top-k sampling has no Responses representation: only a Messages or
	// Gemini target carries the cutoff.
	if request.TopK != nil {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureTopK,
			"top_k",
			"top-k sampling is not supported by a Responses upstream",
		); err != nil {
			return nil, report, err
		}
	}

	// Stop sequences are not representable in Responses.
	if len(request.StopSequences) > 0 {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureStopSequences,
			"stop_sequences",
			"Responses has no stop-sequence field",
		); err != nil {
			return nil, report, err
		}
	}

	// Tools. The pinned Responses function-tool contract requires an
	// explicit strict on the wire. A source tool that carries no strictness
	// semantic (Messages tools have none; Chat tools may omit it) cannot
	// provide one: under strict policy the conversion is rejected, and under
	// the tool_schema_strictness permission it emits explicit strict:false —
	// the non-tightening value — never an omitted or guessed strict.
	for _, tool := range request.Tools {
		parameters := tool.JSONSchema
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object"}`)
		}
		strict, err := responsesToolStrictField(context, tool, &report)
		if err != nil {
			return nil, report, err
		}
		out.Tools = append(out.Tools, ResponsesTool{
			Type:        "function",
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  parameters,
			Strict:      strict,
		})
	}

	// Tool choice.
	if request.ToolChoice != nil {
		choice, err := canonicalToolChoiceToResponses(*request.ToolChoice)
		if err != nil {
			return nil, report, err
		}
		out.ToolChoice = &choice
	}

	// Structured output.
	if request.StructuredOutput != nil {
		schema := request.StructuredOutput.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out.Text = &ResponsesEnvelopeText{
			Format: &ResponsesEnvelopeTextFormat{
				Type:        "json_schema",
				Name:        request.StructuredOutput.Name,
				Description: request.StructuredOutput.Description,
				Schema:      schema,
				Strict:      new(request.StructuredOutput.Strict),
			},
		}
	}

	// The Responses request-envelope echo. This renderer targets the very wire
	// the echo was decoded from, so every field that wire models is restored
	// exactly: dropping one would silently change what the client asked for
	// (a new conversation instead of a continued one, no store, no truncation
	// policy, no logprob detail, no reasoning-summary style). No same-protocol
	// direction is constructible from configuration today, so this is the
	// renderer's own contract rather than a live route, and the unit tests
	// assert the RENDERED body rather than the absence of a report entry.
	//
	// previous_response_id is the one exception: a resolved chain was already
	// prepended by the continuity store (RequestDepth > 0), so forwarding the
	// id would make the upstream continue the conversation a second time. An
	// UNRESOLVED id is forwarded as the client sent it.
	if context != nil && context.OriginalResponsesRequest != nil {
		echo := context.OriginalResponsesRequest
		out.Store = echo.Store
		out.User = echo.User
		out.Truncation = echo.Truncation
		out.ServiceTier = echo.ServiceTier
		out.TopLogprobs = echo.TopLogprobs
		if context.RequestDepth == 0 {
			out.PreviousResponseID = echo.PreviousResponseID
		}
		if echo.Reasoning != nil {
			out.Reasoning = &ResponsesEnvelopeReasoning{
				Effort:  echo.Reasoning.Effort,
				Summary: echo.Reasoning.Summary,
			}
		}
	}

	// Anthropic Messages thinking configuration (Messages source only): the
	// client's thinking request maps to Responses reasoning.effort when the
	// exchange grants the ReasoningEffort capability (mapping
	// ExchangeContext.Capabilities). The mapping is explicit and documented —
	// never silent:
	//
	//  - "adaptive"  the client delegated the thinking decision to the
	//                model; the absence of reasoning.effort is the exact
	//                semantic (the upstream applies its own default).
	//  - "disabled"  no thinking requested; nothing is emitted.
	//  - "enabled"   the explicit budget_tokens maps through the documented
	//                deterministic threshold table thinkingBudgetToEffort
	//                and is reported as a named Note.
	//
	// Without the ReasoningEffort capability an enabled budget is a
	// loss/reject decision (request_reasoning), never a silent drop — the
	// same policy as the chat renderer.
	if request.Thinking != nil {
		var capabilities ChatCapabilities
		if context != nil {
			capabilities = context.Capabilities
		}
		// The client's thinking-visibility preference is a Messages-native
		// field with no Responses representation.
		if request.Thinking.Display != "" {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureRequestReasoning,
				"thinking.display",
				"the thinking display preference cannot be reproduced by a Responses request",
			); err != nil {
				return nil, report, err
			}
		}
		switch request.Thinking.Type {
		case "enabled":
			if request.Thinking.BudgetTokens == nil {
				// An "enabled" thinking request without a budget is provider
				// intent the responses target cannot reproduce. Decode rejects
				// this shape today; a hand-built CanonicalRequest or a future
				// decode relaxation must never pass it silently (the never-silent invariant).
				if err := report.Note(
					FeatureRequestReasoning,
					"thinking",
					"thinking enabled without a budget maps to the upstream provider's default reasoning effort",
				); err != nil {
					return nil, report, err
				}
			} else if !capabilities.ReasoningEffort {
				if err := report.Lose(
					context.lossPolicy(),
					FeatureRequestReasoning,
					"thinking",
					"the thinking budget cannot be reproduced by the configured upstream provider",
				); err != nil {
					return nil, report, err
				}
			} else {
				if out.Reasoning == nil {
					out.Reasoning = &ResponsesEnvelopeReasoning{}
				}
				effort := thinkingBudgetToEffort(*request.Thinking.BudgetTokens)
				out.Reasoning.Effort = &effort
				if err := report.Note(
					FeatureRequestReasoning,
					"thinking",
					fmt.Sprintf(
						"Anthropic thinking budget %d mapped to Responses reasoning.effort %q",
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
				"adaptive thinking maps to the upstream provider's default reasoning effort",
			); err != nil {
				return nil, report, err
			}
		case "disabled":
			if err := report.Note(
				FeatureRequestReasoning,
				"thinking",
				"thinking disabled maps to the absence of Responses reasoning.effort",
			); err != nil {
				return nil, report, err
			}
		}
	}

	// Profile-resolved reasoning tier: when the client specified neither a
	// reasoning effort nor a thinking control, the profile's tier fills the
	// gap. The gate is on the client's INTENT, not on whether an effort
	// happened to be materialized: thinking "disabled" and "adaptive" are
	// recorded as notes that deliberately emit no effort, and letting the
	// profile tier materialize one here would contradict the note just
	// written (thinking disabled would still reach the upstream as
	// reasoning_effort) and would override the client's explicit choice.
	// The Responses upstream always accepts reasoning.effort, so no
	// capability gate is needed here.
	if context != nil && context.ResolvedReasoningTier != "" && request.Thinking == nil {
		if out.Reasoning == nil {
			out.Reasoning = &ResponsesEnvelopeReasoning{}
		}
		if out.Reasoning.Effort == nil {
			effort := context.ResolvedReasoningTier
			out.Reasoning.Effort = &effort
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, report, err
	}
	return body, report, nil
}

// responsesToolStrictField renders the required strict field of a Responses
// function tool. A source strict value is preserved exactly. A source tool
// without a strictness semantic is an explicit loss decision: rejected under
// strict policy, and emitted as explicit strict:false under the
// tool_schema_strictness permission.
func responsesToolStrictField(
	context *ExchangeContext,
	tool CanonicalTool,
	report *ConversionReport,
) (wire.Field[bool], error) {
	if tool.Strict != nil {
		return wire.Field[bool]{Value: *tool.Strict, Present: true}, nil
	}
	if err := report.Lose(
		context.lossPolicy(),
		FeatureToolSchemaStrictness,
		"tools[].strict",
		"the source tool schema has no strictness semantic; emitting explicit strict:false",
	); err != nil {
		return wire.Field[bool]{}, err
	}
	return wire.Field[bool]{Value: false, Present: true}, nil
}

// loseSystemPart applies the loss/reject decision for a system prompt part
// that cannot be expressed in the string-only create-request instructions.
// An image or document follows the system_non_text_content loss decision
// (approved drop, else typed rejection); any other non-text part is a stable
// typed rejection — never a leaked Go type name, matching the chat-side
// decision.
func loseSystemPart(
	policy LossPolicy,
	report *ConversionReport,
	part CanonicalPart,
) error {
	switch part.(type) {
	case CanonicalImage, CanonicalDocument:
	default:
		// Defensive: no request path reaches this arm today - the create-request
		// render only ever passes an image or a document here. It names the
		// offending part type so that IF it is ever reached the operator can
		// diagnose it; a message that merely repeated the path would not.
		return &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     "instructions",
			Feature:  fmt.Sprintf("system prompt part %T cannot be expressed in the create-request instructions string", part),
		}
	}
	return report.Lose(
		policy,
		FeatureSystemNonTextContent,
		"instructions",
		"the system prompt part cannot be expressed in the string-only create-request instructions",
	)
}

// canonicalTurnPartsToEasyMessage renders canonical content parts into a
// Responses easy input message.
func canonicalTurnPartsToEasyMessage(
	role CanonicalRole,
	parts []CanonicalPart,
) (*ResponsesEasyInputMessage, error) {
	message := &ResponsesEasyInputMessage{
		Role: responsesInputRoleFromCanonical(role),
	}
	content, err := canonicalPartsToResponsesContent(parts)
	if err != nil {
		return nil, err
	}
	message.Content = content
	return message, nil
}

// responsesInputRoleFromCanonical maps a canonical role to a Responses input
// role.
func responsesInputRoleFromCanonical(role CanonicalRole) ResponsesInputRole {
	switch role {
	case CanonicalAssistant:
		return ResponsesInputRoleAssistant
	case CanonicalSystem:
		return ResponsesInputRoleSystem
	case CanonicalDeveloper:
		return ResponsesInputRoleDeveloper
	default:
		return ResponsesInputRoleUser
	}
}

// canonicalPartsToResponsesContent renders canonical content parts into the
// Responses message content union.
func canonicalPartsToResponsesContent(
	parts []CanonicalPart,
) (ResponsesInputMessageContent, error) {
	var wireParts []ResponsesInputContentPart
	for i, part := range parts {
		switch value := part.(type) {
		case CanonicalText:
			wireParts = append(wireParts, &ResponsesInputText{
				Type: "input_text",
				Text: value.Text,
			})

		case CanonicalImage:
			image, err := canonicalImageToResponsesInputImage(value)
			if err != nil {
				return ResponsesInputMessageContent{}, fmt.Errorf(
					"content part %d: %w",
					i,
					err,
				)
			}
			wireParts = append(wireParts, image)

		case CanonicalDocument:
			file, err := canonicalDocumentToResponsesInputFile(value)
			if err != nil {
				return ResponsesInputMessageContent{}, fmt.Errorf(
					"content part %d: %w",
					i,
					err,
				)
			}
			wireParts = append(wireParts, file)

		case CanonicalRefusal:
			return ResponsesInputMessageContent{}, &UnsupportedFeatureError{
				Protocol: "responses",
				Path:     "input[].content",
				Feature:  "refusal input content",
			}

		case CanonicalFunctionCall, CanonicalFunctionResult:
			return ResponsesInputMessageContent{}, errors.New(
				"function calls and results must be rendered as separate input items",
			)

		case CanonicalThinkingPart:
			// Marker-signature thinking is proxy-synthesized reasoning: the
			// request path scrubs it from replayed history before upstream
			// rendering, so it never reaches a Responses upstream. Any other
			// signature was refused by ValidateCanonicalRequest, which is the
			// only gate that inspects turn parts (the artifact gate covers
			// request.Artifacts).
			continue

		default:
			return ResponsesInputMessageContent{}, fmt.Errorf(
				"content part %d: unknown canonical part %T",
				i,
				part,
			)
		}
	}
	return ResponsesInputMessageContent{Parts: wireParts}, nil
}

// canonicalImageToResponsesInputImage renders a canonical image into an
// input_image content part, using image_url with a base64 data URL when the
// image is inline. Detail defaults to auto (the API default) when the source
// dialect did not specify one.
func canonicalImageToResponsesInputImage(
	image CanonicalImage,
) (*ResponsesInputImage, error) {
	detail := image.Detail
	if detail == "" {
		detail = "auto"
	}
	if image.URL != "" {
		return &ResponsesInputImage{
			Type:     "input_image",
			Detail:   detail,
			ImageURL: image.URL,
		}, nil
	}
	if image.Base64 != "" {
		url, err := imageDataURL(image.MediaType, image.Base64)
		if err != nil {
			return nil, err
		}
		return &ResponsesInputImage{
			Type:     "input_image",
			Detail:   detail,
			ImageURL: url,
		}, nil
	}
	return nil, errors.New("canonical image has neither URL nor base64 data")
}

// imageDataURL builds a base64 data URL for an image media type.
// encodeDataURL builds a data URL for any media type (documents included).
func encodeDataURL(mediaType, base64Data string) (string, error) {
	if base64Data == "" {
		return "", errors.New("empty data")
	}
	return "data:" + mediaType + ";base64," + base64Data, nil
}

func imageDataURL(mediaType, base64Data string) (string, error) {
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return "", fmt.Errorf("unsupported image media type %q", mediaType)
	}
	if base64Data == "" {
		return "", errors.New("empty image data")
	}
	return "data:" + mediaType + ";base64," + base64Data, nil
}

// canonicalDocumentToResponsesInputFile renders a canonical document into an
// input_file content part.
func canonicalDocumentToResponsesInputFile(
	document CanonicalDocument,
) (*ResponsesInputFile, error) {
	file := &ResponsesInputFile{
		Type:     "input_file",
		FileID:   document.FileID,
		FileURL:  document.URL,
		Filename: document.Filename,
	}
	if document.Base64 != "" {
		file.FileData = document.Base64
	}
	if err := file.Validate(); err != nil {
		return nil, err
	}
	return file, nil
}

// canonicalPartsToFunctionOutput renders canonical parts into a Responses
// function_call_output output union.
func canonicalPartsToFunctionOutput(
	parts []CanonicalPart,
) (ResponsesFunctionOutput, error) {
	// A single text part renders as the string arm; anything richer renders
	// as a part array.
	if len(parts) == 1 {
		if text, ok := parts[0].(CanonicalText); ok {
			return ResponsesFunctionOutput{Text: &text.Text}, nil
		}
	}
	content, err := canonicalPartsToResponsesContent(parts)
	if err != nil {
		return ResponsesFunctionOutput{}, err
	}
	return ResponsesFunctionOutput{Parts: content.Parts}, nil
}
