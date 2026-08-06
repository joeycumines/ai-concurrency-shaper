package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Direct source-to-IR-to-target conversion. There must not be an
// implementation that chains wire serializers:
//
//	Messages JSON -> Responses JSON -> Chat JSON
//
// because every intermediate serializer loses source semantics and invents
// fields that were never supplied. Each source decodes exactly once into the
// canonical IR and each target renders directly from that IR.

// responsesRequestEnvelope is the strict request envelope decoded from a
// Responses request body. Unknown fields are rejected; recognized-but-
// unsupported fields produce an UnsupportedFeatureError before decoding.
type responsesRequestEnvelope struct {
	Model              string                      `json:"model"`
	Input              *ResponsesInput             `json:"input,omitempty"`
	Instructions       *ResponsesInput             `json:"instructions,omitempty"`
	MaxOutputTokens    *int                        `json:"max_output_tokens,omitempty"`
	ParallelToolCalls  *bool                       `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID *string                     `json:"previous_response_id,omitempty"`
	Store              *bool                       `json:"store,omitempty"`
	Temperature        *float64                    `json:"temperature,omitempty"`
	TopP               *float64                    `json:"top_p,omitempty"`
	Truncation         *string                     `json:"truncation,omitempty"`
	User               *string                     `json:"user,omitempty"`
	Metadata           map[string]string           `json:"metadata,omitempty"`
	Tools              []ResponsesTool             `json:"tools,omitempty"`
	ToolChoice         *ResponsesToolChoice        `json:"tool_choice,omitempty"`
	Reasoning          *ResponsesEnvelopeReasoning `json:"reasoning,omitempty"`
	Text               *ResponsesEnvelopeText      `json:"text,omitempty"`
	ServiceTier        *string                     `json:"service_tier,omitempty"`
	TopLogprobs        *int64                      `json:"top_logprobs,omitempty"`
	Stream             bool                        `json:"stream,omitempty"`
}

// probeUnsupportedResponsesFields reports the first recognized-but-unsupported
// Responses request field, or "" when none is present. These fields are
// conversation-state and background controls that the transcoder deliberately
// does not implement; their presence is a typed unsupported-feature error,
// never a silent drop.
func probeUnsupportedResponsesFields(data []byte) (string, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", err
	}
	for _, name := range []string{
		"include",
		"prompt",
		"background",
		"max_tool_calls",
		"safety_identifier",
		"prompt_cache_key",
		"status",
	} {
		if _, ok := probe[name]; ok {
			return name, nil
		}
	}
	return "", nil
}

// DecodeResponsesRequest decodes a Responses request body into the canonical
// IR and captures the request echo required to reconstruct the client
// response envelope.
func DecodeResponsesRequest(
	body []byte,
	policy LossPolicy,
) (DecodeResult, *ResponsesRequestEcho, error) {
	if name, err := probeUnsupportedResponsesFields(body); err != nil {
		return DecodeResult{}, nil, err
	} else if name != "" {
		return DecodeResult{}, nil, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     name,
			Feature:  name,
		}
	}

	var envelope responsesRequestEnvelope
	if err := strictDecode(body, &envelope); err != nil {
		return DecodeResult{}, nil, fmt.Errorf("responses request: %w", err)
	}
	if envelope.Model == "" {
		return DecodeResult{}, nil, errors.New("responses request model is empty")
	}
	if envelope.Truncation != nil {
		switch *envelope.Truncation {
		case "auto", "disabled":
		default:
			return DecodeResult{}, nil, fmt.Errorf(
				"invalid truncation %q",
				*envelope.Truncation,
			)
		}
	}
	if envelope.TopLogprobs != nil && *envelope.TopLogprobs < 0 {
		return DecodeResult{}, nil, errors.New("negative top_logprobs")
	}

	var result DecodeResult
	result.Request.ClientModel = envelope.Model
	result.Request.Stream = envelope.Stream
	result.Request.MaxOutputTokens = envelope.MaxOutputTokens
	result.Request.ParallelTools = envelope.ParallelToolCalls
	result.Request.Temperature = envelope.Temperature
	result.Request.TopP = envelope.TopP
	result.Request.Metadata = envelope.Metadata

	// Tools. Only function tools are supported; built-in tools are a typed
	// unsupported feature.
	for i, tool := range envelope.Tools {
		if err := tool.Validate(); err != nil {
			return DecodeResult{}, nil, fmt.Errorf("responses tools[%d]: %w", i, err)
		}
		result.Request.Tools = append(result.Request.Tools, CanonicalTool{
			Name:        tool.Name,
			Description: tool.Description,
			JSONSchema:  tool.Parameters,
			Strict:      tool.Strict,
		})
	}

	// Tool choice.
	if envelope.ToolChoice != nil {
		choice, err := canonicalizeResponsesToolChoice(*envelope.ToolChoice)
		if err != nil {
			return DecodeResult{}, nil, err
		}
		result.Request.ToolChoice = choice
	}

	// Structured output from text.format.
	if envelope.Text != nil && envelope.Text.Format != nil {
		format := envelope.Text.Format
		switch format.Type {
		case "", "text":
			// No structured output requested.
		case "json_schema":
			if len(format.Schema) == 0 {
				return DecodeResult{}, nil, errors.New(
					"responses text.format json_schema has no schema",
				)
			}
			strict := format.Strict != nil && *format.Strict
			result.Request.StructuredOutput = &CanonicalStructuredOutput{
				Name:        format.Name,
				Description: format.Description,
				Schema:      format.Schema,
				Strict:      strict,
			}
		case "json_object":
			if err := result.Report.Lose(
				policy,
				FeatureStructuredOutput,
				"text.format.type",
				"json_object mode cannot be reproduced in every target",
			); err != nil {
				return DecodeResult{}, nil, err
			}
		default:
			return DecodeResult{}, nil, &UnsupportedFeatureError{
				Protocol: "responses",
				Path:     "text.format.type",
				Feature:  format.Type,
			}
		}
	}

	// Turns from instructions and input.
	if envelope.Instructions != nil {
		if err := envelope.Instructions.Validate(); err != nil {
			return DecodeResult{}, nil, fmt.Errorf("responses instructions: %w", err)
		}
		turn, err := responsesInstructionsToTurns(*envelope.Instructions)
		if err != nil {
			return DecodeResult{}, nil, err
		}
		result.Request.Turns = append(result.Request.Turns, turn...)
	}
	if envelope.Input != nil {
		if err := envelope.Input.Validate(); err != nil {
			return DecodeResult{}, nil, fmt.Errorf("responses input: %w", err)
		}
		turns, err := responsesInputToTurns(
			*envelope.Input,
			policy,
			&result.Report,
			&result.Request.Artifacts,
		)
		if err != nil {
			return DecodeResult{}, nil, err
		}
		result.Request.Turns = append(result.Request.Turns, turns...)
	}

	// Echo for the client envelope reconstruction.
	echo := &ResponsesRequestEcho{
		Instructions:       envelope.Instructions,
		MaxOutputTokens:    envelope.MaxOutputTokens,
		ParallelToolCalls:  envelope.ParallelToolCalls,
		PreviousResponseID: envelope.PreviousResponseID,
		Store:              envelope.Store,
		Temperature:        envelope.Temperature,
		TopP:               envelope.TopP,
		Truncation:         envelope.Truncation,
		User:               envelope.User,
		Metadata:           envelope.Metadata,
		Tools:              envelope.Tools,
		ToolChoice:         envelope.ToolChoice,
		Reasoning:          envelope.Reasoning,
		Text:               envelope.Text,
		ServiceTier:        envelope.ServiceTier,
		TopLogprobs:        envelope.TopLogprobs,
		Stream:             envelope.Stream,
	}

	return result, echo, nil
}

// responsesInstructionsToTurns maps the Responses instructions union to
// system turns.
func responsesInstructionsToTurns(
	instructions ResponsesInput,
) ([]CanonicalTurn, error) {
	if instructions.Text != nil {
		return []CanonicalTurn{{
			Role:  CanonicalSystem,
			Parts: []CanonicalPart{CanonicalText{Text: *instructions.Text}},
		}}, nil
	}
	turn := CanonicalTurn{Role: CanonicalSystem}
	for i, item := range instructions.Items {
		switch value := item.(type) {
		case *ResponsesEasyInputMessage:
			parts, err := responsesInputContentToParts(value.Content)
			if err != nil {
				return nil, fmt.Errorf("instructions item %d: %w", i, err)
			}
			turn.Parts = append(turn.Parts, parts...)
		default:
			return nil, &UnsupportedFeatureError{
				Protocol: "responses",
				Path:     "instructions[]",
				Feature:  "non-message instructions item",
			}
		}
	}
	return []CanonicalTurn{turn}, nil
}

// responsesInputToTurns maps the Responses input item list to canonical turns,
// preserving turn boundaries and content order. Function calls and their
// results are folded into the adjacent assistant and user turns so identity
// is preserved.
func responsesInputToTurns(
	input ResponsesInput,
	policy LossPolicy,
	report *ConversionReport,
	artifacts *SourceArtifacts,
) ([]CanonicalTurn, error) {
	if input.Text != nil {
		return []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: *input.Text}},
		}}, nil
	}

	var turns []CanonicalTurn
	for i, item := range input.Items {
		switch value := item.(type) {
		case *ResponsesEasyInputMessage:
			role := canonicalRoleFromResponsesInputRole(value.Role)
			parts, err := responsesInputContentToParts(value.Content)
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", i, err)
			}
			turns = append(turns, CanonicalTurn{Role: role, Parts: parts})

		case *ResponsesPreviousOutputMessage:
			parts, err := responsesOutputContentToCanonical(value.Content)
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", i, err)
			}
			turns = append(turns, CanonicalTurn{
				Role:  CanonicalAssistant,
				Parts: parts,
			})

		case *ResponsesFunctionCallInput:
			arguments, err := decodeJSONObject(value.Arguments)
			if err != nil {
				return nil, fmt.Errorf(
					"input item %d: function call arguments: %w",
					i,
					err,
				)
			}
			part := CanonicalFunctionCall{
				CallID:    value.CallID,
				Name:      value.Name,
				Arguments: mustRawMessage(arguments),
			}
			turns = appendFunctionCallTurn(turns, part)

		case *ResponsesFunctionCallOutputInput:
			parts, err := responsesFunctionOutputToCanonical(value.Output)
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", i, err)
			}
			turns = appendFunctionResultTurn(turns, CanonicalFunctionResult{
				CallID:  value.CallID,
				IsError: false,
				Parts:   parts,
			})

		case *ResponsesReasoningInput:
			// Reasoning items are source-specific artifacts. They may pass
			// through unchanged only to a Responses target; crossing
			// protocols is decided by RequirePortableArtifacts.
			raw, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", i, err)
			}
			artifacts.ResponsesReasoningItems = append(
				artifacts.ResponsesReasoningItems,
				raw,
			)

		case *ResponsesItemReferenceInput:
			if err := report.Lose(
				policy,
				FeatureConversationState,
				"input[].item_reference",
				"item references are Responses-specific conversation state",
			); err != nil {
				return nil, fmt.Errorf("input item %d: %w", i, err)
			}

		default:
			return nil, fmt.Errorf("input item %d: unknown item type %T", i, item)
		}
	}
	return turns, nil
}

// appendFunctionCallTurn appends a function call part to the last assistant
// turn, or opens a new assistant turn when the last turn is not assistant.
func appendFunctionCallTurn(turns []CanonicalTurn, part CanonicalFunctionCall) []CanonicalTurn {
	if len(turns) > 0 && turns[len(turns)-1].Role == CanonicalAssistant {
		turns[len(turns)-1].Parts = append(turns[len(turns)-1].Parts, part)
		return turns
	}
	return append(turns, CanonicalTurn{
		Role:  CanonicalAssistant,
		Parts: []CanonicalPart{part},
	})
}

// appendFunctionResultTurn appends a function result part to the last user
// turn, or opens a new user turn when the last turn is not user.
func appendFunctionResultTurn(turns []CanonicalTurn, part CanonicalFunctionResult) []CanonicalTurn {
	if len(turns) > 0 && turns[len(turns)-1].Role == CanonicalUser {
		turns[len(turns)-1].Parts = append(turns[len(turns)-1].Parts, part)
		return turns
	}
	return append(turns, CanonicalTurn{
		Role:  CanonicalUser,
		Parts: []CanonicalPart{part},
	})
}

// canonicalRoleFromResponsesInputRole maps a Responses input role to the
// canonical role.
func canonicalRoleFromResponsesInputRole(role ResponsesInputRole) CanonicalRole {
	switch role {
	case ResponsesInputRoleAssistant:
		return CanonicalAssistant
	case ResponsesInputRoleSystem:
		return CanonicalSystem
	case ResponsesInputRoleDeveloper:
		return CanonicalDeveloper
	default:
		return CanonicalUser
	}
}

// responsesInputContentToParts maps Responses input message content to
// canonical parts.
func responsesInputContentToParts(
	content ResponsesInputMessageContent,
) ([]CanonicalPart, error) {
	if content.Text != nil {
		return []CanonicalPart{CanonicalText{Text: *content.Text}}, nil
	}
	var parts []CanonicalPart
	for i, part := range content.Parts {
		converted, err := responsesInputContentPartToCanonical(part)
		if err != nil {
			return nil, fmt.Errorf("content part %d: %w", i, err)
		}
		parts = append(parts, converted)
	}
	return parts, nil
}

// responsesInputContentPartToCanonical maps one input content part.
func responsesInputContentPartToCanonical(
	part ResponsesInputContentPart,
) (CanonicalPart, error) {
	switch value := part.(type) {
	case *ResponsesInputText:
		return CanonicalText{Text: value.Text}, nil

	case *ResponsesInputImage:
		// The official image_url field carries either a fully qualified URL
		// or a base64 data URL; file_id images have no portable equivalent in
		// the target dialects and are rejected.
		if value.FileID != "" {
			return nil, &UnsupportedFeatureError{
				Protocol: "responses",
				Path:     "input[].content[].file_id",
				Feature:  "image file_id input",
			}
		}
		mediaType, base64Data, err := splitImageDataURL(value.ImageURL)
		if err != nil {
			return nil, fmt.Errorf("input image: %w", err)
		}
		if base64Data != "" {
			return CanonicalImage{
				MediaType: mediaType,
				Base64:    base64Data,
				Detail:    value.Detail,
			}, nil
		}
		return CanonicalImage{
			URL:    value.ImageURL,
			Detail: value.Detail,
		}, nil

	case *ResponsesInputFile:
		return CanonicalDocument{
			MediaType: "",
			URL:       value.FileURL,
			Base64:    value.FileData,
			FileID:    value.FileID,
			Filename:  value.Filename,
		}, nil

	default:
		return nil, fmt.Errorf("unknown input content part type %T", part)
	}
}

// responsesOutputContentToCanonical maps output message content parts (used
// when a previous output message is reused as input) to canonical parts.
func responsesOutputContentToCanonical(
	content ResponsesOutputContentParts,
) ([]CanonicalPart, error) {
	if err := content.Validate(); err != nil {
		return nil, err
	}
	var parts []CanonicalPart
	for i, part := range content {
		switch value := part.(type) {
		case *ResponsesOutputText:
			parts = append(parts, CanonicalText{Text: value.Text})
		case *ResponsesOutputRefusal:
			parts = append(parts, CanonicalRefusal{Text: value.Refusal})
		default:
			return nil, fmt.Errorf("output content part %d: unknown type %T", i, part)
		}
	}
	return parts, nil
}

// responsesFunctionOutputToCanonical maps a function_call_output payload to
// canonical parts.
func responsesFunctionOutputToCanonical(
	output ResponsesFunctionOutput,
) ([]CanonicalPart, error) {
	if output.Text != nil {
		return []CanonicalPart{CanonicalText{Text: *output.Text}}, nil
	}
	var parts []CanonicalPart
	for i, part := range output.Parts {
		converted, err := responsesInputContentPartToCanonical(part)
		if err != nil {
			return nil, fmt.Errorf("function output part %d: %w", i, err)
		}
		parts = append(parts, converted)
	}
	return parts, nil
}

// splitImageDataURL parses a base64 data URL of the form
// data:<media-type>;base64,<data>. A non-data URL returns empty media type
// and data.
func splitImageDataURL(url string) (mediaType string, base64Data string, err error) {
	const prefix = "data:"
	if !bytes.HasPrefix([]byte(url), []byte(prefix)) {
		return "", "", nil
	}
	rest := url[len(prefix):]
	semicolon := bytes.IndexByte([]byte(rest), ';')
	comma := bytes.IndexByte([]byte(rest), ',')
	if semicolon < 0 || comma < 0 || semicolon > comma {
		return "", "", errors.New("malformed data URL")
	}
	mediaType = rest[:semicolon]
	base64Data = rest[comma+1:]
	if mediaType == "" || base64Data == "" {
		return "", "", errors.New("data URL missing media type or data")
	}
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return "", "", fmt.Errorf("unsupported image media type %q", mediaType)
	}
	return mediaType, base64Data, nil
}

// canonicalizeResponsesToolChoice maps the Responses tool_choice union to the
// canonical form. Unknown string values are rejected rather than silently
// defaulted to auto.
func canonicalizeResponsesToolChoice(
	choice ResponsesToolChoice,
) (*CanonicalToolChoice, error) {
	if err := choice.Validate(); err != nil {
		return nil, fmt.Errorf("responses tool_choice: %w", err)
	}
	if choice.Named != nil {
		return &CanonicalToolChoice{
			Mode: "named",
			Name: choice.Named.Name,
		}, nil
	}
	mode := *choice.Str
	if mode == "required" {
		// Responses has no "required"; the closest portable semantics is
		// auto. The strict contract rejects it instead of weakening caller
		// intent.
		return nil, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     "tool_choice",
			Feature:  mode,
		}
	}
	return &CanonicalToolChoice{Mode: mode}, nil
}

// mustRawMessage marshals a map into a raw JSON object. The map is already
// validated JSON by decodeJSONObject, so marshaling cannot fail.
func mustRawMessage(value map[string]json.RawMessage) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

// messagesRequestEnvelope is the strict request envelope decoded from an
// Anthropic Messages request body.
type messagesRequestEnvelope struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	Messages      []AnthropicMessage   `json:"messages"`
	System        *AnthropicContent    `json:"system,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	TopK          *int                 `json:"top_k,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        *bool                `json:"stream,omitempty"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	Metadata      map[string]any       `json:"metadata,omitempty"`
}

// DecodeMessagesRequest decodes an Anthropic Messages request body into the
// canonical IR.
func DecodeMessagesRequest(
	body []byte,
	policy LossPolicy,
) (DecodeResult, error) {
	var envelope messagesRequestEnvelope
	if err := strictDecode(body, &envelope); err != nil {
		return DecodeResult{}, fmt.Errorf("messages request: %w", err)
	}
	if envelope.Model == "" {
		return DecodeResult{}, errors.New("messages request model is empty")
	}
	if envelope.MaxTokens <= 0 {
		return DecodeResult{}, errors.New("messages request max_tokens must be positive")
	}

	var result DecodeResult
	result.Request.ClientModel = envelope.Model
	result.Request.MaxOutputTokens = &envelope.MaxTokens
	result.Request.Temperature = envelope.Temperature
	result.Request.TopP = envelope.TopP
	result.Request.StopSequences = envelope.StopSequences
	result.Request.Metadata = strMap(envelope.Metadata)
	if envelope.Stream != nil {
		result.Request.Stream = *envelope.Stream
	}

	if envelope.TopK != nil {
		if err := result.Report.Lose(
			policy,
			FeatureTopK,
			"top_k",
			"top_k has no portable equivalent in the target protocols",
		); err != nil {
			return DecodeResult{}, err
		}
	}

	// System prompt becomes a system turn.
	if envelope.System != nil {
		if err := envelope.System.Validate(); err != nil {
			return DecodeResult{}, fmt.Errorf("messages system: %w", err)
		}
		parts, err := anthropicContentToCanonical(*envelope.System, policy, &result.Report, &result.Request.Artifacts)
		if err != nil {
			return DecodeResult{}, fmt.Errorf("messages system: %w", err)
		}
		result.Request.Turns = append(result.Request.Turns, CanonicalTurn{
			Role:  CanonicalSystem,
			Parts: parts,
		})
	}

	// Conversation messages.
	for i, message := range envelope.Messages {
		if err := message.Validate(); err != nil {
			return DecodeResult{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		role := CanonicalUser
		if message.Role == AnthropicMessageRoleAssistant {
			role = CanonicalAssistant
		}
		parts, err := anthropicContentToCanonical(message.Content, policy, &result.Report, &result.Request.Artifacts)
		if err != nil {
			return DecodeResult{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		result.Request.Turns = append(result.Request.Turns, CanonicalTurn{
			Role:  role,
			Parts: parts,
		})
	}

	// Tools.
	for i, tool := range envelope.Tools {
		if err := tool.Validate(); err != nil {
			return DecodeResult{}, fmt.Errorf("messages tools[%d]: %w", i, err)
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return DecodeResult{}, fmt.Errorf("messages tools[%d]: %w", i, err)
		}
		description := ""
		if tool.Description != nil {
			description = *tool.Description
		}
		result.Request.Tools = append(result.Request.Tools, CanonicalTool{
			Name:        tool.Name,
			Description: description,
			JSONSchema:  schema,
		})
	}

	// Tool choice: Anthropic "auto"/"none"/"any"/named tool.
	if envelope.ToolChoice != nil {
		choice, err := canonicalizeAnthropicToolChoice(*envelope.ToolChoice)
		if err != nil {
			return DecodeResult{}, err
		}
		result.Request.ToolChoice = choice
	}

	return result, nil
}

// canonicalizeAnthropicToolChoice maps the Anthropic tool_choice union to the
// canonical form.
func canonicalizeAnthropicToolChoice(
	choice AnthropicToolChoice,
) (*CanonicalToolChoice, error) {
	switch choice.Type {
	case "auto":
		return &CanonicalToolChoice{Mode: "auto"}, nil
	case "none":
		return &CanonicalToolChoice{Mode: "none"}, nil
	case "any":
		return &CanonicalToolChoice{Mode: "required"}, nil
	case "tool":
		if choice.Name == "" {
			return nil, errors.New("messages tool_choice type tool requires name")
		}
		return &CanonicalToolChoice{Mode: "named", Name: choice.Name}, nil
	default:
		return nil, &UnsupportedFeatureError{
			Protocol: "messages",
			Path:     "tool_choice.type",
			Feature:  choice.Type,
		}
	}
}

// anthropicContentToCanonical maps Anthropic message content (string or
// blocks) to canonical parts.
func anthropicContentToCanonical(
	content AnthropicContent,
	policy LossPolicy,
	report *ConversionReport,
	artifacts *SourceArtifacts,
) ([]CanonicalPart, error) {
	if content.ContentStr != nil {
		return []CanonicalPart{CanonicalText{Text: *content.ContentStr}}, nil
	}
	var parts []CanonicalPart
	for i, block := range content.ContentBlocks {
		if err := block.Validate(); err != nil {
			return nil, fmt.Errorf("content block %d: %w", i, err)
		}
		switch block.Type {
		case AnthropicContentBlockTypeText:
			parts = append(parts, CanonicalText{Text: *block.Text})

		case AnthropicContentBlockTypeImage:
			if block.Source == nil {
				return nil, fmt.Errorf("content block %d: image has no source", i)
			}
			parts = append(parts, CanonicalImage{
				MediaType: block.Source.MediaType,
				URL:       block.Source.URL,
				Base64:    block.Source.Data,
			})

		case AnthropicContentBlockTypeDocument:
			if block.Source == nil {
				return nil, fmt.Errorf("content block %d: document has no source", i)
			}
			parts = append(parts, CanonicalDocument{
				MediaType: block.Source.MediaType,
				URL:       block.Source.URL,
				Base64:    block.Source.Data,
			})

		case AnthropicContentBlockTypeToolUse:
			arguments, err := decodeJSONObject(string(block.Input))
			if err != nil {
				return nil, fmt.Errorf("content block %d: tool_use input: %w", i, err)
			}
			parts = append(parts, CanonicalFunctionCall{
				CallID:    *block.ID,
				Name:      *block.Name,
				Arguments: mustRawMessage(arguments),
			})

		case AnthropicContentBlockTypeToolResult:
			resultParts, err := anthropicContentToCanonical(
				*block.Content,
				policy,
				report,
				artifacts,
			)
			if err != nil {
				return nil, fmt.Errorf("content block %d: tool_result: %w", i, err)
			}
			isError := block.IsError != nil && *block.IsError
			parts = append(parts, CanonicalFunctionResult{
				CallID:  *block.ToolUseID,
				IsError: isError,
				Parts:   resultParts,
			})

		case AnthropicContentBlockTypeThinking,
			AnthropicContentBlockTypeRedactedThinking:
			// Thinking blocks are source-authenticated artifacts. They may
			// pass through unchanged only to a Messages target; crossing
			// protocols is decided by RequirePortableArtifacts.
			raw, err := json.Marshal(block)
			if err != nil {
				return nil, fmt.Errorf("content block %d: %w", i, err)
			}
			artifacts.AnthropicThinkingBlocks = append(
				artifacts.AnthropicThinkingBlocks,
				raw,
			)

		default:
			return nil, fmt.Errorf("content block %d: unknown type %q", i, block.Type)
		}
	}
	return parts, nil
}

// strMap converts a map with any value type to a string map, dropping
// non-string values. Messages metadata values are strings on the wire.
func strMap(m map[string]any) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for key, value := range m {
		if s, ok := value.(string); ok {
			out[key] = s
		}
	}
	return out
}

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

	var instructions *ResponsesInput
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

	if len(systemTurns) > 0 {
		// A single text-only system turn renders as the string arm; anything
		// richer renders as easy-message items.
		if len(systemTurns) == 1 && len(systemTurns[0].Parts) == 1 {
			if text, ok := systemTurns[0].Parts[0].(CanonicalText); ok {
				instructions = &ResponsesInput{Text: &text.Text}
			}
		}
		if instructions == nil {
			var items ResponsesInputItems
			for _, turn := range systemTurns {
				item, err := canonicalTurnPartsToEasyMessage(turn.Role, turn.Parts)
				if err != nil {
					return nil, report, err
				}
				items = append(items, item)
			}
			instructions = &ResponsesInput{Items: items}
		}
	}

	// Render user/assistant turns into input items, preserving order. A
	// turn's content parts become one easy message; function calls and
	// results become their own items after it.
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
				output, err := canonicalPartsToFunctionOutput(value.Parts)
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

	out := responsesRequestEnvelope{
		Model:             model,
		Instructions:      instructions,
		Input:             &ResponsesInput{Items: input},
		MaxOutputTokens:   request.MaxOutputTokens,
		ParallelToolCalls: request.ParallelTools,
		Temperature:       request.Temperature,
		TopP:              request.TopP,
		Metadata:          request.Metadata,
		Stream:            request.Stream,
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

	// Tools.
	for _, tool := range request.Tools {
		parameters := tool.JSONSchema
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, ResponsesTool{
			Type:        "function",
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  parameters,
			Strict:      tool.Strict,
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
				Strict:      boolPtr(request.StructuredOutput.Strict),
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

	body, err := json.Marshal(out)
	if err != nil {
		return nil, report, err
	}
	return body, report, nil
}

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
		Stream:              boolPtr(request.Stream),
		Metadata:            anyMap(request.Metadata),
		N:                   intPtr(1),
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
	for _, turn := range request.Turns {
		switch turn.Role {
		case CanonicalSystem:
			message, err := canonicalTextTurnToChatMessage(turn, ChatMessageRoleSystem)
			if err != nil {
				return nil, report, err
			}
			out.Messages = append(out.Messages, message)

		case CanonicalDeveloper:
			role := ChatMessageRoleDeveloper
			if !capabilities.DeveloperRole {
				role = ChatMessageRoleSystem
			}
			message, err := canonicalTextTurnToChatMessage(turn, role)
			if err != nil {
				return nil, report, err
			}
			out.Messages = append(out.Messages, message)

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
			out.Messages = append(out.Messages, messages...)

		case CanonicalAssistant:
			message, err := canonicalAssistantTurnToChatMessage(turn)
			if err != nil {
				return nil, report, err
			}
			out.Messages = append(out.Messages, message)
		}
	}

	// Tools.
	for _, tool := range request.Tools {
		var parameters map[string]any
		if len(tool.JSONSchema) > 0 {
			if err := json.Unmarshal(tool.JSONSchema, &parameters); err != nil {
				return nil, report, fmt.Errorf("tool %q parameters: %w", tool.Name, err)
			}
		}
		description := tool.Description
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
			schema := request.StructuredOutput.Schema
			var schemaMap map[string]any
			if len(schema) > 0 {
				if err := json.Unmarshal(schema, &schemaMap); err != nil {
					return nil, report, fmt.Errorf("structured output schema: %w", err)
				}
			}
			out.ResponseFormat = &ChatResponseFormat{
				Type: ChatResponseFormatJSONSchema,
				JSONSchema: &ChatJSONSchemaFormat{
					Name:        request.StructuredOutput.Name,
					Description: &request.StructuredOutput.Description,
					Schema:      schemaMap,
					Strict:      boolPtr(request.StructuredOutput.Strict),
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
				FeatureProviderReasoning,
				"reasoning.effort",
				"reasoning effort is not supported by the configured chat provider",
			); err != nil {
				return nil, report, err
			}
		} else {
			out.ReasoningEffort = context.OriginalResponsesRequest.Reasoning.Effort
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, report, err
	}
	return body, report, nil
}
