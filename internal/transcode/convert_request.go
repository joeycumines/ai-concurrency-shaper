package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// Direct source-to-IR-to-target conversion. There must not be an
// implementation that chains wire serializers:
//
//	Messages JSON -> Responses JSON -> Chat JSON
//
// because every intermediate serializer loses source semantics and invents
// fields that were never supplied. Each source decodes exactly once into the
// canonical IR and each target renders directly from that IR.

// probeUnsupportedResponsesFields reports the first recognized-but-unsupported
// Responses request field, or "" when none is present. These fields are
// background and conversation-state controls the transcoder deliberately
// does not implement; their presence is a typed unsupported-feature error,
// never a silent drop. (include and prompt_cache_key are NOT probed here:
// include is a best-effort response-format preference and prompt_cache_key
// is the responses_controls loss decision, both handled in
// DecodeResponsesRequest.)
func probeUnsupportedResponsesFields(data []byte) (string, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", err
	}
	for _, name := range []string{
		"prompt",
		"background",
		"max_tool_calls",
		"safety_identifier",
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
// response envelope. The body is decoded through the pinned wire contract
// (wire/openairesponses.Request): the stream field is presence-aware so the
// handler can apply the documented precedence over the Accept header, and
// tool entries must carry an explicit strict (a missing or null strict is a
// malformed request, rejected client-dialect before any upstream request).
func DecodeResponsesRequest(
	body []byte,
	policy LossPolicy,
) (DecodeResult, *ResponsesRequestEcho, error) {
	var request openairesponses.Request
	if err := wire.Decode(body, &request); err != nil {
		// A valid-but-unsupported feature reported by a wire union dispatcher
		// (e.g. an input_audio part) surfaces as UnsupportedFeatureError —
		// client-dialect local — never as a malformed-request error. All six
		// malformed-JSON categories surface as typed wire.DecodeError.
		return DecodeResult{}, nil, fmt.Errorf(
			"responses request: %w",
			wireUnsupportedToFeature(err),
		)
	}
	// Recognized-but-unsupported request fields (background and
	// conversation-state controls) are a typed unsupported-feature error,
	// never a silent drop. The probe runs after the strict decode so a
	// malformed document is reported as malformed, not as an unsupported
	// field.
	if name, err := probeUnsupportedResponsesFields(body); err != nil {
		return DecodeResult{}, nil, err
	} else if name != "" {
		return DecodeResult{}, nil, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     name,
			Feature:  name,
		}
	}

	var result DecodeResult

	// Client-controlled request fields that a chat upstream cannot honor
	// are recorded, never silently dropped:
	//
	//   - include requests extra response fields (e.g.
	//     reasoning.encrypted_content). The chat upstream cannot produce
	//     them; the rendered response carries what the source actually
	//     provided (the reasoning content mapping under the
	//     provider_reasoning_text capability). Best-effort by nature.
	//   - client_metadata is pure client telemetry with no upstream
	//     semantics.
	//   - prompt_cache_key is a conversation-cache control; the
	//     responses_controls loss decision applies (approved by the CLI
	//     defaults, rejectable by policy).
	if len(request.Include) > 0 {
		if err := result.Report.Note(
			FeatureResponsesControls,
			"include",
			"the include response-format preference cannot be honored by a chat upstream; the rendered response carries what the source provides",
		); err != nil {
			return DecodeResult{}, nil, err
		}
	}
	if len(request.ClientMetadata) > 0 {
		if err := result.Report.Note(
			FeatureResponsesControls,
			"client_metadata",
			"client_metadata is client telemetry with no upstream semantics; it is not forwarded",
		); err != nil {
			return DecodeResult{}, nil, err
		}
	}
	if request.PromptCacheKey != "" {
		if err := result.Report.Lose(
			policy,
			FeatureResponsesControls,
			"prompt_cache_key",
			"the prompt cache key cannot be reproduced by a chat upstream",
		); err != nil {
			return DecodeResult{}, nil, err
		}
	}

	// Tool strictness is required on the wire for function tools: reject
	// before any value is consumed so a strict-less tool can never reach the
	// converters.
	for i, tool := range request.Tools {
		if err := tool.Validate(); err != nil {
			return DecodeResult{}, nil, fmt.Errorf("responses tools[%d]: %w", i, err)
		}
	}

	if request.Model == "" {
		return DecodeResult{}, nil, &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "model",
			Message: "responses request model is empty",
		}
	}
	if request.Truncation != nil {
		switch *request.Truncation {
		case "auto", "disabled":
		default:
			return DecodeResult{}, nil, fmt.Errorf(
				"invalid truncation %q",
				*request.Truncation,
			)
		}
	}
	if request.TopLogprobs != nil && *request.TopLogprobs < 0 {
		return DecodeResult{}, nil, errors.New("negative top_logprobs")
	}

	result.Request.ClientModel = request.Model
	result.Request.Stream = request.Stream.Value
	// An explicitly present body stream field (true or false, never null) is
	// recorded as present so the handler can apply the documented precedence
	// over the Accept header (review-08 blocker 1). An absent or null field
	// is treated as absent, consistent with the envelope's other pointer
	// fields.
	if request.Stream.Present && !request.Stream.Null {
		result.StreamSet = true
		result.Request.Stream = request.Stream.Value
	}
	result.Request.MaxOutputTokens = request.MaxOutputTokens
	result.Request.ParallelTools = request.ParallelToolCalls
	result.Request.Temperature = request.Temperature
	result.Request.TopP = request.TopP
	result.Request.Metadata = request.Metadata

	// Tools. Function tools are supported; namespace tools are flattened
	// into their nested function tools (the grouping is client-side
	// structure the chat target cannot express; the functions themselves
	// are fully portable); built-in tools (web_search, file_search,
	// code_interpreter, computer_use, ...) are the builtin_tools loss
	// decision — approved, they are dropped; rejected, the request fails.
	// The raw parameters schema is validated as exactly one JSON object at
	// this boundary; its bytes are preserved, never decoded and remarshaled
	// through a map, so large integers, decimals, and exponents survive
	// byte-exact (review-k finding 2).
	for i, tool := range request.Tools {
		switch tool.Type {
		case "namespace":
			flattened, err := flattenNamespaceTool(tool, &result.Report, policy)
			if err != nil {
				return DecodeResult{}, nil, fmt.Errorf(
					"responses tools[%d]: %w",
					i,
					err,
				)
			}
			for j, nested := range flattened {
				if len(nested.Parameters) > 0 {
					if _, err := decodeJSONObject(string(nested.Parameters)); err != nil {
						return DecodeResult{}, nil, fmt.Errorf(
							"responses tools[%d] namespace[%d] parameters: %w",
							i,
							j,
							err,
						)
					}
				}
				result.Request.Tools = append(result.Request.Tools, CanonicalTool{
					Name:        nested.Name,
					Description: nested.Description,
					JSONSchema:  nested.Parameters,
					Strict:      fieldBoolPtr(nested.Strict),
				})
			}
		case "function":
			if len(tool.Parameters) > 0 {
				if _, err := decodeJSONObject(string(tool.Parameters)); err != nil {
					return DecodeResult{}, nil, fmt.Errorf(
						"responses tools[%d] parameters: %w",
						i,
						err,
					)
				}
			}
			result.Request.Tools = append(result.Request.Tools, CanonicalTool{
				Name:        tool.Name,
				Description: tool.Description,
				JSONSchema:  tool.Parameters,
				Strict:      fieldBoolPtr(tool.Strict),
			})
		default:
			// Built-in and unknown tool types: the builtin_tools loss
			// decision (approved by the CLI defaults, rejectable by
			// policy).
			if err := result.Report.Lose(
				policy,
				FeatureBuiltinTools,
				fmt.Sprintf("tools[%d]", i),
				fmt.Sprintf(
					"the %q built-in tool cannot be reproduced in a chat request",
					tool.Type,
				),
			); err != nil {
				return DecodeResult{}, nil, err
			}
		}
	}

	// Tool choice, reconciled against the tools that survive conversion: an
	// approved built-in drop can leave "required" or a named function choice
	// pointing at tools that no longer exist, which would render an invalid
	// upstream request (review-12 finding 5).
	if request.ToolChoice != nil {
		choice, err := canonicalizeResponsesToolChoice(*request.ToolChoice)
		if err != nil {
			return DecodeResult{}, nil, err
		}
		choice, err = reconcileToolChoice(
			choice,
			len(request.Tools),
			&result.Request,
			&result.Report,
		)
		if err != nil {
			return DecodeResult{}, nil, err
		}
		result.Request.ToolChoice = choice
	}

	// Structured output from text.format.
	if request.Text != nil && request.Text.Format != nil {
		format := request.Text.Format
		switch format.Type {
		case "", "text":
			// No structured output requested.
		case "json_schema":
			if len(format.Schema) == 0 {
				return DecodeResult{}, nil, errors.New(
					"responses text.format json_schema has no schema",
				)
			}
			if _, err := decodeJSONObject(string(format.Schema)); err != nil {
				return DecodeResult{}, nil, fmt.Errorf(
					"responses text.format schema: %w",
					err,
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
	if request.Instructions != nil {
		result.Request.Turns = append(result.Request.Turns, CanonicalTurn{
			Role:  CanonicalSystem,
			Parts: []CanonicalPart{CanonicalText{Text: *request.Instructions}},
		})
	}
	if request.Input != nil {
		if err := request.Input.Validate(); err != nil {
			return DecodeResult{}, nil, fmt.Errorf("responses input: %w", err)
		}
		turns, err := responsesInputToTurns(
			*request.Input,
			policy,
			&result.Report,
			&result.Request.Artifacts,
		)
		if err != nil {
			return DecodeResult{}, nil, err
		}
		result.Request.Turns = append(result.Request.Turns, turns...)
	}

	// Echo for the client envelope reconstruction. The client's
	// instructions is a plain string; the response envelope echo renders
	// the string arm of the pinned instructions union. The effective values
	// (parallel_tool_calls, temperature, top_p, tool_choice) carry the
	// pinned API defaults — parallel_tool_calls defaults to true,
	// temperature to 1.0, top_p to 1.0, tool_choice to "auto" — so the
	// renderer can emit a complete response envelope without guessing later.
	echo := &ResponsesRequestEcho{}
	if request.Instructions != nil {
		echo.Instructions = &ResponsesInput{Text: request.Instructions}
	}
	echo.MaxOutputTokens = request.MaxOutputTokens
	echo.ParallelToolCalls = defaultBoolValue(request.ParallelToolCalls, true)
	echo.PreviousResponseID = request.PreviousResponseID
	echo.Store = request.Store
	echo.Temperature = defaultFloatValue(request.Temperature, 1.0)
	echo.TopP = defaultFloatValue(request.TopP, 1.0)
	echo.Truncation = request.Truncation
	echo.User = request.User
	echo.Metadata = request.Metadata
	echo.Tools = request.Tools
	echo.ToolChoice = defaultToolChoice(request.ToolChoice)
	echo.Reasoning = request.Reasoning
	echo.Text = request.Text
	echo.ServiceTier = request.ServiceTier
	echo.TopLogprobs = request.TopLogprobs

	return result, echo, nil
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
	// Input item identity (the id on an easy message, a previous output
	// message, a function call, or a function call output) is a Responses
	// conversation-state reference: the canonical IR tracks turn boundaries,
	// content order, and tool-call identity — not item ids — and every
	// renderer rebuilds items from turns. The drop is unconditional and
	// observable: one deduped note per exchange under the conversation-state
	// feature, never a silent elision (review-gate task-11 finding 3).
	itemIdentityNoted := false
	noteItemIdentity := func() error {
		if itemIdentityNoted {
			return nil
		}
		itemIdentityNoted = true
		return report.Note(
			FeaturePreviousResponseID,
			"input[].id",
			"input item ids are not forwarded (the target dialect has no item identity)",
		)
	}
	for i, item := range input.Items {
		switch value := item.(type) {
		case *ResponsesEasyInputMessage:
			if value.ID != "" {
				if err := noteItemIdentity(); err != nil {
					return nil, err
				}
			}
			if err := loseInputPhase(policy, report, value.Phase, i); err != nil {
				return nil, err
			}
			role := canonicalRoleFromResponsesInputRole(value.Role)
			parts, err := responsesInputContentToParts(value.Content)
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", i, err)
			}
			turns = append(turns, CanonicalTurn{Role: role, Parts: parts})

		case *ResponsesPreviousOutputMessage:
			if value.ID != "" {
				if err := noteItemIdentity(); err != nil {
					return nil, err
				}
			}
			if err := loseInputPhase(policy, report, value.Phase, i); err != nil {
				return nil, err
			}
			parts, err := responsesOutputContentToCanonical(value.Content)
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", i, err)
			}
			turns = append(turns, CanonicalTurn{
				Role:  CanonicalAssistant,
				Parts: parts,
			})

		case *ResponsesFunctionCallInput:
			if value.ID != "" {
				if err := noteItemIdentity(); err != nil {
					return nil, err
				}
			}
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
			if value.ID != "" {
				if err := noteItemIdentity(); err != nil {
					return nil, err
				}
			}
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
				FeaturePreviousResponseID,
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

	case *ResponsesOutputText:
		// Assistant easy-message history turns carry output-type parts
		// (autopsy 01 §3.3); they map exactly like
		// responsesOutputContentToCanonical — only .Text/.Refusal reach the
		// IR, annotations never do.
		return CanonicalText{Text: value.Text}, nil

	case *ResponsesOutputRefusal:
		return CanonicalRefusal{Text: value.Refusal}, nil

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
// data:<media-type>;<parameters>;base64,<data>. The parameters section must
// include the base64 parameter — anything else is rejected, never silently
// reinterpreted (review-j finding 15). A non-data URL returns empty media
// type and data.
func splitImageDataURL(url string) (mediaType string, base64Data string, err error) {
	const prefix = "data:"
	if !bytes.HasPrefix([]byte(url), []byte(prefix)) {
		return "", "", nil
	}
	rest := url[len(prefix):]
	comma := bytes.IndexByte([]byte(rest), ',')
	if comma < 0 {
		return "", "", errors.New("malformed data URL")
	}
	params := strings.Split(rest[:comma], ";")
	mediaType = params[0]
	base64Data = rest[comma+1:]
	if mediaType == "" || base64Data == "" {
		return "", "", errors.New("data URL missing media type or data")
	}
	foundBase64 := false
	for _, param := range params[1:] {
		if strings.EqualFold(param, "base64") {
			foundBase64 = true
			break
		}
	}
	if !foundBase64 {
		return "", "", errors.New("data URL parameters must include base64")
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
	// "required" is part of the official Responses contract (and the Chat
	// dialect renders it natively), so it is portable as-is.
	return &CanonicalToolChoice{Mode: *choice.Str}, nil
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

// flattenNamespaceTool returns the nested function tools of a namespace
// tool, recursing into nested namespaces. The grouping is client-side
// structure a chat request cannot express; the nested function tools are
// fully portable, so the flatten is reported as a named Note. A nested
// non-function tool type is the SAME builtin_tools loss decision as a
// top-level built-in tool (review-11 finding 2): approved, it drops
// observably; rejected, the request fails — never a different, harder rule
// than the top-level path.
func flattenNamespaceTool(
	tool openairesponses.Tool,
	report *ConversionReport,
	policy LossPolicy,
) ([]openairesponses.Tool, error) {
	var out []openairesponses.Tool
	for i, nested := range tool.Tools {
		switch nested.Type {
		case "namespace":
			inner, err := flattenNamespaceTool(nested, report, policy)
			if err != nil {
				return nil, fmt.Errorf("namespace %d: %w", i, err)
			}
			out = append(out, inner...)
		case "function":
			out = append(out, nested)
		default:
			if err := report.Lose(
				policy,
				FeatureBuiltinTools,
				fmt.Sprintf("tools[].tools[%d]", i),
				fmt.Sprintf(
					"the %q built-in tool nested in a namespace cannot be reproduced in a chat request",
					nested.Type,
				),
			); err != nil {
				return nil, err
			}
		}
	}
	if len(out) == 0 {
		// Every nested tool was a dropped built-in. The top-level rule for
		// an all-built-in tools list is accept-and-drop under the approval,
		// and the tool_choice reconciliation owns the no-tools-left case —
		// a namespace is never a different, harder rule than the top level
		// (review-11 finding 2).
		if err := report.Note(
			FeatureBuiltinTools,
			"tools[]",
			fmt.Sprintf(
				"namespace tool %q carried no portable function tools (all nested tools dropped under the builtin_tools approval)",
				tool.Name,
			),
		); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := report.Note(
		FeatureBuiltinTools,
		"tools[]",
		fmt.Sprintf(
			"namespace tool %q flattened into %d function tool(s) for the chat request",
			tool.Name,
			len(out),
		),
	); err != nil {
		return nil, err
	}
	return out, nil
}

// DecodeMessagesRequest decodes an Anthropic Messages request body into the
// canonical IR.
func DecodeMessagesRequest(
	body []byte,
	policy LossPolicy,
) (DecodeResult, error) {
	var envelope anthropicmessages.Request
	if err := wire.Decode(body, &envelope); err != nil {
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
	metadata, err := strMap(envelope.Metadata)
	if err != nil {
		return DecodeResult{}, err
	}
	result.Request.Metadata = metadata
	if envelope.Stream != nil {
		result.StreamSet = true
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

	// The modern Anthropic envelope controls are client-side semantics with
	// no target representation: output_config.budget_tokens is an output
	// budget whose effective cap is already carried by max_tokens, and
	// context_management directs server-side conversation trimming. They are
	// never silently stripped — each present control is an explicit
	// loss/reject decision under anthropic_controls (review-11 finding 1).
	if envelope.ContextManagement != nil {
		if err := result.Report.Lose(
			policy,
			FeatureAnthropicControls,
			"context_management",
			"context_management is a server-side conversation control the target cannot reproduce",
		); err != nil {
			return DecodeResult{}, err
		}
	}
	if envelope.OutputConfig != nil {
		if err := result.Report.Lose(
			policy,
			FeatureAnthropicControls,
			"output_config",
			"the output_config budget duplicates max_tokens, which the target already carries",
		); err != nil {
			return DecodeResult{}, err
		}
	}

	// cache_control is the Anthropic prompt-cache marker real clients attach
	// to text blocks, tools, and the system prompt. It is a pure performance
	// hint with no semantic content and no portable equivalent, so it is a
	// sanctioned observable elision — one deduped note per exchange, never a
	// policy gate and never a silent drop (analysis G3).
	cacheControlNoted := false
	noteCacheControl := func() error {
		if cacheControlNoted {
			return nil
		}
		cacheControlNoted = true
		return result.Report.Note(
			FeatureAnthropicControls,
			"cache_control",
			"cache_control performance hints are not forwarded (chat upstreams cache automatically)",
		)
	}
	if envelope.System != nil {
		for _, block := range envelope.System.ContentBlocks {
			if block.CacheControl != nil {
				if err := noteCacheControl(); err != nil {
					return DecodeResult{}, err
				}
			}
		}
	}
	for _, tool := range envelope.Tools {
		if tool.CacheControl != nil {
			if err := noteCacheControl(); err != nil {
				return DecodeResult{}, err
			}
		}
	}
	// Blocks nest one level: tool_result content carries its own block
	// array, and the marker is legal on those nested blocks too, so the
	// scan walks the nested content of every top-level block.
	var contentBlocksCarryCacheControl func(blocks []anthropicmessages.ContentBlock) error
	contentBlocksCarryCacheControl = func(blocks []anthropicmessages.ContentBlock) error {
		for _, block := range blocks {
			if block.CacheControl != nil {
				if err := noteCacheControl(); err != nil {
					return err
				}
			}
			if block.Content != nil {
				if err := contentBlocksCarryCacheControl(block.Content.ContentBlocks); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, message := range envelope.Messages {
		if err := contentBlocksCarryCacheControl(message.Content.ContentBlocks); err != nil {
			return DecodeResult{}, err
		}
	}

	// Thinking configuration. "enabled" requires an explicit budget; members
	// are validated per type against the official contract (enabled={type,
	// budget_tokens}, disabled={type}, adaptive={type,display}) so a
	// cross-type member is a malformed request, never silently ignored
	// (review-12 R12-L1). The strict wire decode already rejected unknown
	// fields inside the object.
	if envelope.Thinking != nil {
		switch envelope.Thinking.Type {
		case "enabled":
			if envelope.Thinking.Display != nil {
				return DecodeResult{}, errors.New(
					"messages thinking type enabled cannot carry display (it belongs to adaptive)",
				)
			}
			if envelope.Thinking.BudgetTokens == nil {
				return DecodeResult{}, errors.New(
					"messages thinking type enabled requires budget_tokens",
				)
			}
			if *envelope.Thinking.BudgetTokens <= 0 {
				return DecodeResult{}, errors.New(
					"messages thinking budget_tokens must be positive",
				)
			}
			result.Request.Thinking = &CanonicalThinking{
				Type:         "enabled",
				BudgetTokens: envelope.Thinking.BudgetTokens,
			}
		case "disabled", "adaptive":
			if envelope.Thinking.BudgetTokens != nil {
				return DecodeResult{}, fmt.Errorf(
					"messages thinking type %s cannot carry budget_tokens (it belongs to enabled)",
					envelope.Thinking.Type,
				)
			}
			result.Request.Thinking = &CanonicalThinking{
				Type: envelope.Thinking.Type,
			}
		default:
			return DecodeResult{}, fmt.Errorf(
				"messages thinking type %q is invalid (want enabled, disabled, or adaptive)",
				envelope.Thinking.Type,
			)
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

	// Conversation messages. The modern Anthropic wire admits inline
	// system-role messages alongside user/assistant turns; they map to the
	// canonical system role like the top-level system field.
	for i, message := range envelope.Messages {
		if err := message.Validate(); err != nil {
			return DecodeResult{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		var role CanonicalRole
		switch message.Role {
		case AnthropicMessageRoleUser:
			role = CanonicalUser
		case AnthropicMessageRoleAssistant:
			role = CanonicalAssistant
		case AnthropicMessageRoleSystem:
			role = CanonicalSystem
		default:
			return DecodeResult{}, fmt.Errorf(
				"messages[%d]: unknown anthropic message role %q",
				i,
				message.Role,
			)
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

	// Tools. The input_schema raw bytes are preserved — validated as exactly
	// one JSON object at this boundary, never decoded and remarshaled through
	// a map, so large integers, decimals, and exponents survive byte-exact
	// (review-k finding 2).
	for i, tool := range envelope.Tools {
		if err := tool.Validate(); err != nil {
			return DecodeResult{}, fmt.Errorf("messages tools[%d]: %w", i, err)
		}
		if _, err := decodeJSONObject(string(tool.InputSchema)); err != nil {
			return DecodeResult{}, fmt.Errorf(
				"messages tools[%d] input_schema: %w",
				i,
				err,
			)
		}
		description := ""
		if tool.Description != nil {
			description = *tool.Description
		}
		result.Request.Tools = append(result.Request.Tools, CanonicalTool{
			Name:        tool.Name,
			Description: description,
			JSONSchema:  tool.InputSchema,
		})
	}

	// Tool choice: Anthropic "auto"/"none"/"any"/named tool, reconciled
	// against the surviving tools so a dangling reference never reaches a
	// renderer (review-12 finding 5).
	if envelope.ToolChoice != nil {
		choice, err := canonicalizeAnthropicToolChoice(
			*envelope.ToolChoice,
			&result.Report,
			policy,
		)
		if err != nil {
			return DecodeResult{}, err
		}
		choice, err = reconcileToolChoice(
			choice,
			len(envelope.Tools),
			&result.Request,
			&result.Report,
		)
		if err != nil {
			return DecodeResult{}, err
		}
		result.Request.ToolChoice = choice
	}

	return result, nil
}

// canonicalizeAnthropicToolChoice maps the Anthropic tool_choice union to the
// canonical form. disable_parallel_tool_use is not portable to the target
// providers and is rejected or approved as a parallel-tool-calls loss.
func canonicalizeAnthropicToolChoice(
	choice AnthropicToolChoice,
	report *ConversionReport,
	policy LossPolicy,
) (*CanonicalToolChoice, error) {
	if choice.DisableParallelToolUse != nil && *choice.DisableParallelToolUse {
		if err := report.Lose(
			policy,
			FeatureParallelToolCalls,
			"tool_choice.disable_parallel_tool_use",
			"disable_parallel_tool_use is not portable to the target provider",
		); err != nil {
			return nil, err
		}
	}
	switch choice.Type {
	case "auto", "none", "any":
		// A name is only meaningful with type "tool": carrying one on
		// another mode is a contradictory union arm, rejected instead of
		// silently discarded (review-k finding 5).
		if choice.Name != "" {
			return nil, fmt.Errorf(
				"messages tool_choice type %s must not carry a name",
				choice.Type,
			)
		}
		mode := "auto"
		switch choice.Type {
		case "none":
			mode = "none"
		case "any":
			mode = "required"
		}
		return &CanonicalToolChoice{Mode: mode}, nil
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

// reconcileToolChoice checks a canonicalized tool choice against the tools
// that actually survive conversion (review-12 finding 5). A named choice must
// reference a surviving tool under every policy — a dangling reference is
// malformed no matter how many tools the client sent. A mode choice is only
// reconciled when the client DID send tools and the converter dropped them
// all (built-in-tool loss): "required" against zero tools is a client-dialect
// error (the converter would otherwise render an invalid upstream request),
// and "auto" is dropped with an observable note because an empty tool list
// leaves it meaningless. When the client sent no tools at all the choice
// passes through untouched — the upstream judges that incoherence, not the
// converter (TestDecodeResponsesRequestToolChoiceRequired pins it).
func reconcileToolChoice(
	choice *CanonicalToolChoice,
	requestToolCount int,
	request *CanonicalRequest,
	report *ConversionReport,
) (*CanonicalToolChoice, error) {
	if choice == nil {
		return nil, nil
	}
	if choice.Mode == "named" {
		for _, tool := range request.Tools {
			if tool.Name == choice.Name {
				return choice, nil
			}
		}
		return nil, fmt.Errorf(
			"tool_choice names tool %q which is not among the tools that survive conversion",
			choice.Name,
		)
	}
	if requestToolCount == 0 || len(request.Tools) > 0 {
		return choice, nil
	}
	switch choice.Mode {
	case "required":
		return nil, errors.New(
			"tool_choice required but no portable tools remain after the builtin_tools loss",
		)
	case "auto":
		if err := report.Note(
			FeatureBuiltinTools,
			"tool_choice",
			"tool_choice auto dropped with the last tool (no tools remain to choose among)",
		); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return choice, nil
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

// strMap converts a map with any value type to a string map. Messages
// metadata values are strings on the wire; any other value is an error
// (never silently dropped).
func strMap(m map[string]any) (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for key, value := range m {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("metadata field %q must be a string", key)
		}
		out[key] = s
	}
	return out, nil
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
	// never an illegal items array (review-j finding 13).
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

	body, err := json.Marshal(out)
	if err != nil {
		return nil, report, err
	}
	return body, report, nil
}

// thinkingBudgetToEffort maps an Anthropic Messages thinking budget_tokens
// value to the OpenAI chat reasoning_effort vocabulary. The thresholds are
// the documented midpoints of the classic Claude Code effort budgets
// (minimal ~ 256, low ~ 1024, medium ~ 4096, high ~ 16384); the mapping is
// deterministic, capped at "high" (the non-standard "xhigh" is never
// synthesized), and reported as a named Note on every mapped exchange.
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
		Metadata:            anyMap(request.Metadata),
		N:                   new(1),
	}
	// The official stream protocol sends a final usage-only chunk before
	// [DONE] when include_usage is requested; the state machine consumes it
	// into the terminal envelope's usage (review-j finding 6). It is
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
		if echo.PreviousResponseID != nil {
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

	// A source request with no Chat-representable messages is a
	// client-dialect invalid-request error before any upstream request:
	// messages:null or an invented empty user prompt are never emitted
	// (review-z commit 2).
	if len(out.Messages) == 0 {
		return nil, report, errors.New(
			"the source request has no Chat-representable messages",
		)
	}

	// Tools. The canonical schema is passed through byte-exact: it was
	// validated as exactly one JSON object at decode, so no
	// decode-and-remarshal round trip can corrupt its numbers (review-k
	// finding 2). An absent schema is omitted, matching the source's
	// presence.
	for _, tool := range request.Tools {
		description := tool.Description
		out.Tools = append(out.Tools, ChatTool{
			Type: ChatToolTypeFunction,
			Function: &ChatToolFunction{
				Name:        tool.Name,
				Description: &description,
				Parameters:  tool.JSONSchema,
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
			// as exactly one JSON object at decode; review-k finding 2).
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
	// effort is portable, so the summary request is a loss/reject decision
	// (review-j finding 10).
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
	//   - "adaptive"   the client delegated the thinking decision to the
	//                  model; chat's absent reasoning_effort is the exact
	//                  semantic (the provider applies its own default).
	//   - "disabled"   no thinking requested; nothing is emitted.
	//   - "enabled"    the explicit budget_tokens maps through the
	//                  documented deterministic threshold table in
	//                  thinkingBudgetToEffort; the mapping is reported as a
	//                  named Note so it is observable on every exchange.
	//
	// Without the ReasoningEffort capability an enabled budget is a
	// loss/reject decision (request_reasoning), never a silent drop.
	if request.Thinking != nil {
		switch request.Thinking.Type {
		case "enabled":
			if request.Thinking.BudgetTokens != nil {
				if !capabilities.ReasoningEffort {
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
			// reasoning_effort and is reported (analysis doc 05 §4 / G8).
			if err := report.Note(
				FeatureRequestReasoning,
				"thinking",
				"thinking disabled maps to the absence of chat reasoning_effort",
			); err != nil {
				return nil, report, err
			}
		}
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
// (review-j finding 10).
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

// loseInputPhase applies the input-message phase decision at decode time.
// The canonical IR cannot carry a phase and no target dialect can reproduce
// it, so its presence is a loss/reject decision regardless of the target
// (review-j finding 10).
func loseInputPhase(
	policy LossPolicy,
	report *ConversionReport,
	phase string,
	index int,
) error {
	if phase == "" {
		return nil
	}
	return report.Lose(
		policy,
		FeatureOutputPhase,
		fmt.Sprintf("input[%d].phase", index),
		"the input message phase cannot be reproduced in any target dialect",
	)
}

// loseSystemPart applies the loss/reject decision for a system prompt part
// that cannot be expressed in the string-only create-request instructions
// (review-j finding 13).
func loseSystemPart(
	policy LossPolicy,
	report *ConversionReport,
	part CanonicalPart,
) error {
	switch part.(type) {
	case CanonicalImage, CanonicalDocument:
	default:
		return fmt.Errorf(
			"system prompt part %T cannot be expressed in the create-request instructions string",
			part,
		)
	}
	return report.Lose(
		policy,
		FeatureSystemNonTextContent,
		"instructions",
		"the system prompt part cannot be expressed in the string-only create-request instructions",
	)
}

// fieldBoolPtr converts a presence-aware wire bool to a plain pointer:
// absent or explicit null becomes nil, everything else becomes the value.
func fieldBoolPtr(field wire.Field[bool]) *bool {
	if !field.Present || field.Null {
		return nil
	}
	value := field.Value
	return &value
}

// defaultBoolValue applies the pinned API default when the field is absent.
func defaultBoolValue(value *bool, def bool) bool {
	if value == nil {
		return def
	}
	return *value
}

// defaultFloatValue applies the pinned API default when the field is absent.
func defaultFloatValue(value *float64, def float64) float64 {
	if value == nil {
		return def
	}
	return *value
}

// defaultToolChoice applies the pinned tool_choice default ("auto") when the
// request carried no explicit choice, so the response envelope echo always
// carries a valid value.
func defaultToolChoice(choice *ResponsesToolChoice) ResponsesToolChoice {
	if choice != nil {
		return *choice
	}
	auto := "auto"
	return ResponsesToolChoice{Str: &auto}
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
