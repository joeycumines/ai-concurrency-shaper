package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

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
	// feature, never a silent elision.
	itemIdentityNoted := false
	noteItemIdentity := func() error {
		if itemIdentityNoted {
			return nil
		}
		if err := report.Note(
			FeaturePreviousResponseID,
			"input[].id",
			"input item ids are not forwarded (the target dialect has no item identity)",
		); err != nil {
			return err
		}
		itemIdentityNoted = true
		return nil
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
			argsStr := value.Arguments
			if strings.TrimSpace(argsStr) == "" {
				argsStr = "{}"
			}
			arguments, err := decodeJSONObject(argsStr)
			if err != nil {
				return nil, fmt.Errorf(
					"input item %d: function call arguments: %w",
					i,
					err,
				)
			}
			raw, err := rawMessage(arguments)
			if err != nil {
				return nil, fmt.Errorf("input item %d: function call arguments: %w", i, err)
			}
			// Replayed history keeps the qualifier the client sent. Whether
			// this exchange can map it is decided by the target renderer, not
			// here: the qualifier is client-sent data a Responses target
			// carries natively, and history reconstructed by the continuity
			// store never passes through this decode, so a decision written
			// into the canonical part would be missing or stale for the
			// exchange that actually renders it.
			part := CanonicalFunctionCall{
				CallID:    value.CallID,
				Name:      value.Name,
				Namespace: value.Namespace,
				Arguments: raw,
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
		// Assistant easy-message history turns carry output-type parts;
		// they map exactly like
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
// reinterpreted. A non-data URL returns empty media
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
