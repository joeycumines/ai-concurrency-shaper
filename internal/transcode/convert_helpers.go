package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

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

// canonicalToolChoiceToResponses renders the canonical tool choice into the
// Responses union.
func canonicalToolChoiceToResponses(
	choice CanonicalToolChoice,
) (ResponsesToolChoice, error) {
	switch choice.Mode {
	case "none", "auto", "required":
		mode := choice.Mode
		return ResponsesToolChoice{Str: &mode}, nil
	case "named":
		if choice.Name == "" {
			return ResponsesToolChoice{}, errors.New("named tool choice has no name")
		}
		return ResponsesToolChoice{Named: &ResponsesToolChoiceNamed{
			Type: "function",
			Name: choice.Name,
		}}, nil
	default:
		return ResponsesToolChoice{}, fmt.Errorf(
			"unknown tool choice mode %q",
			choice.Mode,
		)
	}
}

func canonicalToolChoiceToChat(
	choice CanonicalToolChoice,
) (ChatToolChoice, error) {
	switch choice.Mode {
	case "none", "auto", "required":
		mode := choice.Mode
		return ChatToolChoice{Str: &mode}, nil
	case "named":
		if choice.Name == "" {
			return ChatToolChoice{}, errors.New("named tool choice has no name")
		}
		return ChatToolChoice{Struct: &ChatToolChoiceStruct{
			Type: "function",
			Function: &ChatToolChoiceFunction{
				Name: choice.Name,
			},
		}}, nil
	default:
		return ChatToolChoice{}, fmt.Errorf(
			"unknown tool choice mode %q",
			choice.Mode,
		)
	}
}

// canonicalTextTurnToChatMessage renders a system or developer turn into a
// Chat message with the given role. Only text content is portable to a Chat
// system/developer message.
func canonicalTextTurnToChatMessage(
	turn CanonicalTurn,
	role ChatMessageRole,
) (ChatMessage, error) {
	var blocks []ChatContentBlock
	for _, part := range turn.Parts {
		switch value := part.(type) {
		case CanonicalText:
			text := value.Text
			blocks = append(blocks, ChatContentBlock{
				Type: ChatContentBlockTypeText,
				Text: &text,
			})
		default:
			return ChatMessage{}, &UnsupportedFeatureError{
				Protocol: "chat",
				Path:     "messages[].content",
				Feature:  fmt.Sprintf("non-text %T in %s message", part, role),
			}
		}
	}
	// A system/developer turn is text-only; omit content when there are no
	// parts rather than emitting an invalid empty union.
	if len(blocks) == 0 {
		return ChatMessage{}, errors.New("empty system or developer turn")
	}
	return ChatMessage{Role: role, Content: &ChatMessageContent{ContentBlocks: blocks}}, nil
}

// joinChatSystemMessages consolidates rendered system-channel messages into
// one leading system message, joining their text with "\n\n" in encounter
// order (autopsy 02). Every input message is text-only (canonicalTextTurnTo
// ChatMessage rejects anything else), so the join cannot lose content.
func joinChatSystemMessages(leading, midDialog []ChatMessage) ChatMessage {
	texts := make([]string, 0, len(leading)+len(midDialog))
	for _, group := range [][]ChatMessage{leading, midDialog} {
		for _, message := range group {
			for _, block := range message.Content.ContentBlocks {
				if block.Text != nil {
					texts = append(texts, *block.Text)
				}
			}
		}
	}
	joined := strings.Join(texts, "\n\n")
	return ChatMessage{
		Role: ChatMessageRoleSystem,
		Content: &ChatMessageContent{
			ContentBlocks: []ChatContentBlock{{
				Type: ChatContentBlockTypeText,
				Text: &joined,
			}},
		},
	}
}

// canonicalUserTurnToChatMessages renders a user turn into Chat messages:
// function results become tool-role messages and the remaining content
// becomes one user message. Image input requires the configured capability.
func canonicalUserTurnToChatMessages(
	turn CanonicalTurn,
	capabilities ChatCapabilities,
	report *ConversionReport,
	policy LossPolicy,
) ([]ChatMessage, error) {
	var contentParts []CanonicalPart
	var messages []ChatMessage
	for _, part := range turn.Parts {
		switch value := part.(type) {
		case CanonicalFunctionResult:
			if len(contentParts) > 0 {
				message, err := canonicalContentPartsToChatUserMessage(
					contentParts,
					capabilities,
					report,
					policy,
				)
				if err != nil {
					return nil, err
				}
				messages = append(messages, message)
				contentParts = nil
			}
			// Tool messages use the dedicated tool-result renderer — never
			// the user-message renderer (review-z commit 2).
			toolMessage, err := renderChatToolResult(value, policy, report)
			if err != nil {
				return nil, err
			}
			messages = append(messages, toolMessage)

		default:
			contentParts = append(contentParts, part)
		}
	}
	if len(contentParts) > 0 {
		message, err := canonicalContentPartsToChatUserMessage(
			contentParts,
			capabilities,
			report,
			policy,
		)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

// canonicalContentPartsToChatUserMessage renders text and image parts into a
// user message. Image input is rendered as image_url blocks and requires the
// configured capability.
func canonicalContentPartsToChatUserMessage(
	parts []CanonicalPart,
	capabilities ChatCapabilities,
	report *ConversionReport,
	policy LossPolicy,
) (ChatMessage, error) {
	var blocks []ChatContentBlock
	for i, part := range parts {
		switch value := part.(type) {
		case CanonicalText:
			text := value.Text
			blocks = append(blocks, ChatContentBlock{
				Type: ChatContentBlockTypeText,
				Text: &text,
			})

		case CanonicalImage:
			if !capabilities.ImageInput {
				if err := report.Lose(
					policy,
					FeatureImageInput,
					"messages[].content",
					"image input is not supported by the configured chat provider",
				); err != nil {
					return ChatMessage{}, err
				}
				continue
			}
			url := value.URL
			if url == "" {
				var err error
				url, err = imageDataURL(value.MediaType, value.Base64)
				if err != nil {
					return ChatMessage{}, fmt.Errorf("content part %d: %w", i, err)
				}
			}
			detail := value.Detail
			if detail == "" {
				// The official Chat image detail defaults to auto; an empty
				// value is not part of the wire enum.
				detail = "auto"
			}
			blocks = append(blocks, ChatContentBlock{
				Type: ChatContentBlockTypeImage,
				ImageURL: &ChatInputImage{
					URL:    url,
					Detail: &detail,
				},
			})

		case CanonicalDocument:
			// Document input cannot be rendered by the chat provider; it is
			// an approved loss (the part is dropped) or a rejection, mirroring
			// the image-input decision (review-z commit 2).
			if err := report.Lose(
				policy,
				FeatureDocumentInput,
				"messages[].content",
				"document input is not supported by the configured chat provider",
			); err != nil {
				return ChatMessage{}, err
			}
			continue

		case CanonicalRefusal:
			return ChatMessage{}, &UnsupportedFeatureError{
				Protocol: "chat",
				Path:     "messages[].content",
				Feature:  "refusal input content",
			}

		default:
			return ChatMessage{}, fmt.Errorf(
				"content part %d: unknown canonical part %T",
				i,
				part,
			)
		}
	}
	if len(blocks) == 0 {
		return ChatMessage{}, errors.New("user turn has no portable content")
	}
	return ChatMessage{Role: ChatMessageRoleUser, Content: &ChatMessageContent{ContentBlocks: blocks}}, nil
}

// canonicalAssistantTurnToChatMessage renders an assistant turn into a Chat
// message with content, refusal, and tool calls.
func canonicalAssistantTurnToChatMessage(
	turn CanonicalTurn,
) (ChatMessage, error) {
	var blocks []ChatContentBlock
	var toolCalls []ChatMessageToolCall
	var refusal *string
	for i, part := range turn.Parts {
		switch value := part.(type) {
		case CanonicalText:
			text := value.Text
			blocks = append(blocks, ChatContentBlock{
				Type: ChatContentBlockTypeText,
				Text: &text,
			})

		case CanonicalRefusal:
			// Refusal is a top-level assistant message field in Chat.
			refusal = &value.Text

		case CanonicalFunctionCall:
			callID := value.CallID
			name := value.Name
			arguments := string(value.Arguments)
			if arguments == "" {
				arguments = "{}"
			}
			// The official non-stream assistant tool-call shape carries id,
			// function, and type with NO index (review-j finding 5). The
			// type field is required on the wire and always emitted as
			// "function" (review-z commit 1) — never omitted.
			toolCalls = append(toolCalls, ChatMessageToolCall{
				Type: "function",
				ID:   &callID,
				Function: ChatToolCallFunction{
					Name:      &name,
					Arguments: arguments,
				},
			})

		default:
			return ChatMessage{}, fmt.Errorf(
				"assistant message part %d: unknown canonical part %T",
				i,
				part,
			)
		}
	}
	content := ChatMessageContent{ContentBlocks: blocks}
	message := ChatMessage{
		Role: ChatMessageRoleAssistant,
	}
	// Content is omitted when the turn carries only tool calls or a refusal
	// (the official Chat shape for tool-call messages).
	if len(blocks) > 0 {
		message.Content = &content
	}
	if len(toolCalls) > 0 || refusal != nil {
		message.ChatAssistantMessage = &ChatAssistantMessage{
			ToolCalls: toolCalls,
			Refusal:   refusal,
		}
	}
	return message, nil
}

// derefStr returns the value of s, or "" when s is nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// toolResultEnvelopeContent is one entry of the deterministic
// tool_result_json_envelope JSON text envelope (transcode_version 1).
type toolResultEnvelopeContent struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	URL       string `json:"url,omitempty"`
}

// toolResultEnvelope is the deterministic JSON text envelope carrying
// multimodal tool-result content inside a Chat tool message
// (tool_result_json_envelope, transcode_version 1).
type toolResultEnvelope struct {
	TranscodeVersion int                         `json:"transcode_version"`
	Content          []toolResultEnvelopeContent `json:"content"`
}

// renderChatToolResult renders a function result into a complete Chat
// tool-role message — the dedicated tool-result renderer, never the
// user-message renderer (review-z commit 2). Exact text results stay exact
// text. Image, document, or mixed results are rejected under strict policy
// (UnrepresentableError — local, never corrupt wire) and, under the
// tool_result_multimodal_content and tool_result_json_envelope permissions,
// encoded as ONE deterministic JSON text envelope (transcode_version 1) that
// is recorded in the conversion report. Invalid wire (image_url blocks
// inside a tool-role message) is never emitted.
func renderChatToolResult(
	result CanonicalFunctionResult,
	policy LossPolicy,
	report *ConversionReport,
) (ChatMessage, error) {
	parts := result.Parts
	if result.IsError {
		// The error status cannot be carried by a Chat tool message; the
		// permissive encoding is the visible error_status_prefix text
		// (review-j finding 10).
		if err := report.Lose(
			policy,
			FeatureToolResultErrorStatus,
			"messages[].tool_result.is_error",
			"the tool result error status cannot be reproduced in the chat dialect; the permissive encoding is the visible error_status_prefix text",
		); err != nil {
			return ChatMessage{}, err
		}
		parts = append(
			[]CanonicalPart{CanonicalText{Text: "[tool_result_error]"}},
			parts...,
		)
	}

	// Exact text results stay exact text: the string arm of the Chat
	// message content union.
	if allTextParts(parts) {
		var builder strings.Builder
		for i, part := range parts {
			if i > 0 {
				builder.WriteByte('\n')
			}
			builder.WriteString(part.(CanonicalText).Text)
		}
		text := builder.String()
		callID := result.CallID
		return ChatMessage{
			Role:            ChatMessageRoleTool,
			Content:         &ChatMessageContent{ContentStr: &text},
			ChatToolMessage: &ChatToolMessage{ToolCallID: &callID},
		}, nil
	}

	// Multimodal content: the content-shape loss and the sanctioned
	// encoding loss gate the deterministic JSON text envelope.
	if err := report.Lose(
		policy,
		FeatureToolResultMultimodalContent,
		"messages[].tool_result.content",
		"the multimodal tool-result content cannot be carried exactly by a Chat tool message",
	); err != nil {
		return ChatMessage{}, &UnrepresentableError{
			Protocol: "chat",
			Path:     "messages[].tool_result.content",
			Detail:   "the multimodal tool-result content cannot be represented as a Chat tool message",
		}
	}
	envelopeText, err := encodeToolResultEnvelope(parts)
	if err != nil {
		return ChatMessage{}, err
	}
	if err := report.Lose(
		policy,
		FeatureToolResultJSONEnvelope,
		"messages[].tool_result.content",
		"the multimodal tool-result content is encoded as the deterministic transcode JSON text envelope (tool_result_json_envelope, transcode_version 1)",
	); err != nil {
		return ChatMessage{}, &UnrepresentableError{
			Protocol: "chat",
			Path:     "messages[].tool_result.content",
			Detail:   "the multimodal tool-result content cannot be represented as a Chat tool message",
		}
	}
	callID := result.CallID
	return ChatMessage{
		Role:            ChatMessageRoleTool,
		Content:         &ChatMessageContent{ContentStr: &envelopeText},
		ChatToolMessage: &ChatToolMessage{ToolCallID: &callID},
	}, nil
}

// allTextParts reports whether every part is ordinary text.
func allTextParts(parts []CanonicalPart) bool {
	for _, part := range parts {
		if _, ok := part.(CanonicalText); !ok {
			return false
		}
	}
	return true
}

// encodeToolResultEnvelope renders the deterministic tool_result_json_envelope
// JSON text (transcode_version 1): text, image, and document parts in order,
// with base64 images/documents carried as data URLs.
func encodeToolResultEnvelope(parts []CanonicalPart) (string, error) {
	envelope := toolResultEnvelope{TranscodeVersion: 1}
	for i, part := range parts {
		switch value := part.(type) {
		case CanonicalText:
			envelope.Content = append(envelope.Content, toolResultEnvelopeContent{
				Type: "text",
				Text: value.Text,
			})
		case CanonicalImage:
			url := value.URL
			if url == "" {
				var err error
				url, err = imageDataURL(value.MediaType, value.Base64)
				if err != nil {
					return "", fmt.Errorf("tool result part %d: %w", i, err)
				}
			}
			envelope.Content = append(envelope.Content, toolResultEnvelopeContent{
				Type:      "image",
				MediaType: value.MediaType,
				URL:       url,
			})
		case CanonicalDocument:
			url := value.URL
			if url == "" {
				var err error
				url, err = encodeDataURL(value.MediaType, value.Base64)
				if err != nil {
					return "", fmt.Errorf("tool result part %d: %w", i, err)
				}
			}
			envelope.Content = append(envelope.Content, toolResultEnvelopeContent{
				Type:      "document",
				MediaType: value.MediaType,
				URL:       url,
			})
		default:
			return "", fmt.Errorf(
				"tool result part %d: cannot encode canonical part %T in the tool_result_json_envelope",
				i,
				part,
			)
		}
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// nonEmpty reports whether a presence-aware string pointer carries a
// non-empty value: the provider reasoning spellings (reasoning and
// reasoning_content) treat present-but-empty exactly like absent, matching
// the stream delta idiom.
func nonEmpty(s *string) bool {
	return s != nil && *s != ""
}
