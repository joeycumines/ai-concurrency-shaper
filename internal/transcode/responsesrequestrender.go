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
		UpstreamResponses,
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

	// Split turns into instructions (system/developer) and input items.
	var systemTurns, conversationTurns []CanonicalTurn
	for _, turn := range request.Turns {
		switch turn.Role {
		case CanonicalSystem, CanonicalDeveloper:
			systemTurns = append(systemTurns, turn)
		default:
			conversationTurns = append(conversationTurns, turn)
		}
	}

	// The create-request instructions is a plain string (the pinned
	// ResponseNewParams shape). Text-only system prompts render as that
	// string; non-text parts (images, documents) or multiple system turns
	// cannot be expressed in the string and are a loss/reject decision —
	// never an illegal items array.
	if len(systemTurns) > 1 {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureMultipleSystemTurns,
			"instructions",
			"multiple system turns cannot be expressed in a single instructions string",
		); err != nil {
			return nil, report, err
		}
	}
	if len(systemTurns) == 1 {
		var builder strings.Builder
		for _, part := range systemTurns[0].Parts {
			text, ok := part.(CanonicalText)
			if !ok {
				if err := loseSystemPart(context.lossPolicy(), &report, part); err != nil {
					return nil, report, err
				}
				continue
			}
			if builder.Len() > 0 {
				builder.WriteByte('\n')
			}
			builder.WriteString(text.Text)
		}
		if builder.Len() > 0 {
			if len(systemTurns[0].Parts) > 1 {
				if err := report.Note(
					FeatureMultipleSystemTurns,
					"instructions",
					"multiple system text parts join into one instructions string (instructions_text_join encoding)",
				); err != nil {
					return nil, report, err
				}
			}
			rendered := builder.String()
			instructions = &rendered
		}
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
			"the source request has no Messages-representable conversation turns for the Responses input",
		)
	}
	for _, turn := range conversationTurns {
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

	// Reasoning effort from the original request echo.
	if context != nil && context.OriginalResponsesRequest != nil &&
		context.OriginalResponsesRequest.Reasoning != nil &&
		context.OriginalResponsesRequest.Reasoning.Effort != nil {
		out.Reasoning = &ResponsesEnvelopeReasoning{
			Effort: context.OriginalResponsesRequest.Reasoning.Effort,
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
