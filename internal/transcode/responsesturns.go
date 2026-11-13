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
	// replayedLogprobsNoted dedupes the replayed-logprob Note across the WHOLE
	// exchange, exactly like the item identity note: the same fact on several
	// replayed parts is one line.
	replayedLogprobsNoted := false
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
			// An easy message can carry output-type parts (an assistant
			// history turn replayed without an item id), so its annotations are
			// the same citation metadata as the previous-output arm's.
			if err := annotatedInputPartLosses(value.Content.Parts, fmt.Sprintf("input[%d].content", i), policy, report, &replayedLogprobsNoted); err != nil {
				return nil, err
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
			if err := annotatedInputLosses(value.Content, i, policy, report, &replayedLogprobsNoted); err != nil {
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
		// Assistant easy-message history turns carry output-type parts; they
		// map exactly like responsesOutputContentToCanonical — the text
		// reaches the IR losslessly. Annotations are citation metadata the
		// target request cannot reproduce; they are recorded at the item level
		// by the caller (see annotatedInputLosses) rather than dropped silently,
		// matching the Anthropic decoder's request_citations decision.
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

// replayedOutputTextDecisions records the decisions for one replayed output_text
// part at path: citation STRUCTURE is client-visible content, so its loss is
// policy-gated under request_citations; token log-probabilities are scoring
// metadata the client is relaying, so they are one deduped Note per exchange
// under their own request-side key. Both walkers below are this function, so the
// two arms cannot drift.
func replayedOutputTextDecisions(
	text *ResponsesOutputText,
	path string,
	policy LossPolicy,
	report *ConversionReport,
	noteLogprobs *bool,
) error {
	if len(text.Annotations) > 0 {
		if err := report.Lose(
			policy,
			FeatureRequestCitations,
			path+".annotations",
			"request citations on replayed output text cannot be reproduced in the target request",
		); err != nil {
			return err
		}
	}
	if len(text.Logprobs) > 0 && !*noteLogprobs {
		*noteLogprobs = true
		if err := report.Note(
			FeatureRequestLogprobsReplay,
			path+".logprobs",
			"token log-probabilities on replayed output text are provider scoring metadata the client is relaying; they are not forwarded (the source supplied them for the previous response, not as a request for new ones)",
		); err != nil {
			return err
		}
	}
	return nil
}

// annotatedInputLosses records the replayed-output decisions for a previous
// output message replayed as input. The Anthropic decoder records the same
// citation key for equivalent citation metadata, so the two source decoders
// agree.
func annotatedInputLosses(
	content ResponsesOutputContentParts,
	index int,
	policy LossPolicy,
	report *ConversionReport,
	noteLogprobs *bool,
) error {
	for partIndex, part := range content {
		text, ok := part.(*ResponsesOutputText)
		if !ok {
			continue
		}
		base := fmt.Sprintf("input[%d].content[%d]", index, partIndex)
		if err := replayedOutputTextDecisions(text, base, policy, report, noteLogprobs); err != nil {
			return err
		}
	}
	return nil
}

// annotatedInputPartLosses is annotatedInputLosses for the arms that carry the
// INPUT content union rather than the output content union: an easy message
// routes its parts through responsesInputContentPartToCanonical, whose
// *ResponsesOutputText arm drops the annotation and logprob structures.
// basePath names the item's content array, so the entry points at the part the
// caller can find on the wire.
//
// A function_call_output needs no equivalent: the pinned wire contract rejects
// an output-typed part there (FunctionOutput.Validate), so no annotation or
// logprob can reach that arm.
func annotatedInputPartLosses(
	parts []ResponsesInputContentPart,
	basePath string,
	policy LossPolicy,
	report *ConversionReport,
	noteLogprobs *bool,
) error {
	for partIndex, part := range parts {
		text, ok := part.(*ResponsesOutputText)
		if !ok {
			continue
		}
		base := fmt.Sprintf("%s[%d]", basePath, partIndex)
		if err := replayedOutputTextDecisions(text, base, policy, report, noteLogprobs); err != nil {
			return err
		}
	}
	return nil
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
