package transcode

import (
	"errors"
	"fmt"
)

// boolPtr returns a pointer to v.
func boolPtr(v bool) *bool { return &v }

// intPtr returns a pointer to v.
func intPtr(v int) *int { return &v }

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
			content, err := canonicalPartsToChatMessageContent(
				value.Parts,
				capabilities,
				report,
				policy,
			)
			if err != nil {
				return nil, err
			}
			callID := value.CallID
			messages = append(messages, ChatMessage{
				Role:            ChatMessageRoleTool,
				Content:         &content,
				ChatToolMessage: &ChatToolMessage{ToolCallID: &callID},
			})

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
			return ChatMessage{}, &UnsupportedFeatureError{
				Protocol: "chat",
				Path:     "messages[].content",
				Feature:  string(FeatureDocumentInput),
			}

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

// canonicalPartsToChatMessageContent renders canonical parts into a Chat
// message content union, collapsing a single text part into the string arm.
func canonicalPartsToChatMessageContent(
	parts []CanonicalPart,
	capabilities ChatCapabilities,
	report *ConversionReport,
	policy LossPolicy,
) (ChatMessageContent, error) {
	if len(parts) == 1 {
		if text, ok := parts[0].(CanonicalText); ok {
			return ChatMessageContent{ContentStr: &text.Text}, nil
		}
	}
	message, err := canonicalContentPartsToChatUserMessage(
		parts,
		capabilities,
		report,
		policy,
	)
	if err != nil {
		return ChatMessageContent{}, err
	}
	if message.Content == nil {
		return ChatMessageContent{}, errors.New("empty tool result content")
	}
	return *message.Content, nil
}

// canonicalAssistantTurnToChatMessage renders an assistant turn into a Chat
// message with content, refusal, and tool calls.
func canonicalAssistantTurnToChatMessage(
	turn CanonicalTurn,
) (ChatMessage, error) {
	var blocks []ChatContentBlock
	var toolCalls []ChatAssistantMessageToolCall
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
			index := i
			callID := value.CallID
			name := value.Name
			arguments := string(value.Arguments)
			if arguments == "" {
				arguments = "{}"
			}
			toolCalls = append(toolCalls, ChatAssistantMessageToolCall{
				Index: &index,
				Type:  stringPtr("function"),
				ID:    &callID,
				Function: ChatAssistantMessageToolCallFunction{
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

// stringPtr returns a pointer to s.
func stringPtr(s string) *string { return &s }

// derefStr returns the value of s, or "" when s is nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
