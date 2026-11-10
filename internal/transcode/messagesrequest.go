package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

// RenderMessagesRequest renders a canonical request as an Anthropic Messages
// request for an Anthropic Messages upstream. The model is the exchange
// context's upstream model, or the client model when none is resolved.
//
// Rendering is positional. Each user or assistant turn becomes one message in
// the same order and is never merged with its neighbors. Leading system turns
// fill the top-level system field. Messages requires tool_result blocks before
// any other block of a user message, so a text or media part that precedes a
// function result is rejected rather than reordered.
func RenderMessagesRequest(
	request CanonicalRequest,
	context *ExchangeContext,
) ([]byte, ConversionReport, error) {
	var report ConversionReport
	if err := ValidateCanonicalRequest(request); err != nil {
		return nil, report, err
	}
	if err := RequirePortableArtifacts(
		request,
		UpstreamMessages,
		context.lossPolicy(),
		&report,
	); err != nil {
		return nil, report, err
	}

	model := request.ClientModel
	if context != nil && context.UpstreamModel != "" {
		model = context.UpstreamModel
	}
	maxTokens, err := messagesMaxTokens(request, context, model, &report)
	if err != nil {
		return nil, report, err
	}

	out := anthropicmessages.Request{
		Model:         model,
		MaxTokens:     maxTokens,
		Temperature:   request.Temperature,
		TopP:          request.TopP,
		TopK:          request.TopK,
		StopSequences: request.StopSequences,
	}
	if request.Stream {
		out.Stream = new(true)
	}
	if err := renderMessagesTurns(request, context, &out, &report); err != nil {
		return nil, report, err
	}
	if err := renderMessagesTools(request, context, &out, &report); err != nil {
		return nil, report, err
	}
	if err := renderMessagesMetadata(request, context, &out, &report); err != nil {
		return nil, report, err
	}
	if request.Thinking != nil {
		out.Thinking = &anthropicmessages.ThinkingConfig{
			Type:         request.Thinking.Type,
			BudgetTokens: request.Thinking.BudgetTokens,
		}
	}
	if request.StructuredOutput != nil {
		if err := report.Lose(
			context.lossPolicy(),
			FeatureStructuredOutput,
			"generationConfig.responseSchema",
			"structured output cannot be enforced by an Anthropic Messages upstream",
		); err != nil {
			return nil, report, err
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, report, err
	}
	return body, report, nil
}

// messagesMaxTokens resolves the required max_tokens. A request that omits its
// output limit takes the model table's max_output, recorded as a note, and is
// rejected when the table declares none.
func messagesMaxTokens(
	request CanonicalRequest,
	context *ExchangeContext,
	model string,
	report *ConversionReport,
) (int, error) {
	if request.MaxOutputTokens != nil {
		return *request.MaxOutputTokens, nil
	}
	if context != nil && context.ModelMaxOutputTokens != nil {
		if err := report.Note(
			FeatureGeminiMaxTokensFromModelTable,
			"generationConfig.maxOutputTokens",
			"the Anthropic Messages upstream requires max_tokens; the model table's max_output supplies it",
		); err != nil {
			return 0, err
		}
		return *context.ModelMaxOutputTokens, nil
	}
	name := model
	if context != nil && context.RequestedClientModel != "" {
		name = context.RequestedClientModel
	}
	return 0, fmt.Errorf(
		"maxOutputTokens is required for this model: the Anthropic Messages upstream requires max_tokens and the model table declares no max_output for %q",
		name,
	)
}

// renderMessagesTurns fills the system field and the message list. System and
// developer turns that precede the first dialog turn become the system field.
// A system-channel turn after dialog turns moves to the front, which is the
// approved mid_conversation_system loss. Developer turns are the approved
// developer_role loss, rendered as system content.
func renderMessagesTurns(
	request CanonicalRequest,
	context *ExchangeContext,
	out *anthropicmessages.Request,
	report *ConversionReport,
) error {
	var system []anthropicmessages.ContentBlock
	systemTurns := 0
	sawDialog := false
	for i, turn := range request.Turns {
		path := fmt.Sprintf("messages[%d]", i)
		switch turn.Role {
		case CanonicalSystem, CanonicalDeveloper:
			if turn.Role == CanonicalDeveloper {
				if err := report.Lose(
					context.lossPolicy(),
					FeatureDeveloperRole,
					"messages[].role",
					"developer role is not supported by the Anthropic Messages upstream; developer content renders as system content",
				); err != nil {
					return err
				}
			}
			if sawDialog {
				if err := report.Lose(
					context.lossPolicy(),
					FeatureMidConversationSystem,
					"messages[]",
					"mid-conversation system turns cannot keep their position in a Messages request; they consolidate into the leading system field",
				); err != nil {
					return err
				}
			}
			blocks, err := messagesSystemBlocks(turn, path)
			if err != nil {
				return err
			}
			system = append(system, blocks...)
			systemTurns++

		case CanonicalUser, CanonicalAssistant:
			sawDialog = true
			message, err := messagesTurn(turn, path, context.ToolNames)
			if err != nil {
				return err
			}
			out.Messages = append(out.Messages, message)

		default:
			return fmt.Errorf("%s: canonical role %q has no Messages form", path, turn.Role)
		}
	}
	if systemTurns > 1 && !sawDialogAfterSystem(request) {
		if err := report.Note(
			FeatureMidConversationSystem,
			"messages[]",
			"multiple leading system turns consolidate into one system field",
		); err != nil {
			return err
		}
	}
	if len(system) == 1 && system[0].Type == anthropicmessages.ContentBlockTypeText {
		text := *system[0].Text
		out.System = &anthropicmessages.Content{ContentStr: &text}
	} else if len(system) > 0 {
		out.System = &anthropicmessages.Content{ContentBlocks: system}
	}
	if len(out.Messages) == 0 {
		return errors.New("the source request has no Messages-representable messages")
	}
	return nil
}

// sawDialogAfterSystem reports whether any system-channel turn follows a dialog
// turn. Such a turn is already recorded as the mid_conversation_system loss, so
// the leading-consolidation note applies only when no such turn exists.
func sawDialogAfterSystem(request CanonicalRequest) bool {
	sawDialog := false
	for _, turn := range request.Turns {
		switch turn.Role {
		case CanonicalUser, CanonicalAssistant:
			sawDialog = true
		case CanonicalSystem, CanonicalDeveloper:
			if sawDialog {
				return true
			}
		}
	}
	return false
}

// messagesSystemBlocks renders a system or developer turn as text blocks. Only
// text is representable in the system field.
func messagesSystemBlocks(turn CanonicalTurn, path string) ([]anthropicmessages.ContentBlock, error) {
	var blocks []anthropicmessages.ContentBlock
	for j, part := range turn.Parts {
		text, ok := part.(CanonicalText)
		if !ok {
			return nil, &UnrepresentableError{
				Protocol: "messages",
				Path:     fmt.Sprintf("%s.parts[%d]", path, j),
				Detail:   fmt.Sprintf("the system field carries only text, not %T", part),
			}
		}
		value := text.Text
		blocks = append(blocks, anthropicmessages.ContentBlock{
			Type: anthropicmessages.ContentBlockTypeText,
			Text: &value,
		})
	}
	return blocks, nil
}

// messagesTurn renders one user or assistant turn as one message. A user turn's
// tool results must come before any other part, because Messages requires
// tool_result blocks first in a user message.
func messagesTurn(turn CanonicalTurn, path string, names *ToolNames) (anthropicmessages.Message, error) {
	role := anthropicmessages.RoleUser
	if turn.Role == CanonicalAssistant {
		role = anthropicmessages.RoleAssistant
	}
	var blocks []anthropicmessages.ContentBlock
	sawOther := false
	for j, part := range turn.Parts {
		partPath := fmt.Sprintf("%s.parts[%d]", path, j)
		switch value := part.(type) {
		case CanonicalFunctionResult:
			if turn.Role != CanonicalUser {
				return anthropicmessages.Message{}, &UnrepresentableError{
					Protocol: "messages",
					Path:     partPath,
					Detail:   "a function result must belong to a user turn",
				}
			}
			if sawOther {
				return anthropicmessages.Message{}, errors.New(
					"functionResponse parts must precede other parts in a user content for this upstream",
				)
			}
			block, err := messagesToolResultBlock(value, partPath)
			if err != nil {
				return anthropicmessages.Message{}, err
			}
			blocks = append(blocks, block)

		case CanonicalFunctionCall:
			if turn.Role != CanonicalAssistant {
				return anthropicmessages.Message{}, &UnrepresentableError{
					Protocol: "messages",
					Path:     partPath,
					Detail:   "a function call must belong to an assistant turn",
				}
			}
			sawOther = true
			id := value.CallID
			// The canonical part carries the BARE name plus the namespace
			// this exchange declared; the Messages wire carries only the flat
			// name, re-derived from both via ToolNames.flatName. An approved
			// undeclared replay arrives with the qualifier already cleared by
			// the decode.
			name := value.Name
			if value.Namespace != "" {
				flat, ok := names.flatName(ToolNameRef{Namespace: value.Namespace, Name: value.Name})
				if !ok {
					return anthropicmessages.Message{}, fmt.Errorf(
						"%s: call %q references namespace %q, which this request does not declare",
						partPath, value.Name, value.Namespace,
					)
				}
				name = flat
			}
			blocks = append(blocks, anthropicmessages.ContentBlock{
				Type:  anthropicmessages.ContentBlockTypeToolUse,
				ID:    &id,
				Name:  &name,
				Input: value.Arguments,
			})

		case CanonicalText:
			sawOther = true
			text := value.Text
			blocks = append(blocks, anthropicmessages.ContentBlock{
				Type: anthropicmessages.ContentBlockTypeText,
				Text: &text,
			})

		case CanonicalImage, CanonicalDocument:
			sawOther = true
			block, err := messagesMediaBlock(value, partPath)
			if err != nil {
				return anthropicmessages.Message{}, err
			}
			blocks = append(blocks, block)

		default:
			return anthropicmessages.Message{}, &UnrepresentableError{
				Protocol: "messages",
				Path:     partPath,
				Detail:   fmt.Sprintf("the part %T has no Messages request form", part),
			}
		}
	}
	if len(blocks) == 0 {
		return anthropicmessages.Message{}, fmt.Errorf("%s has no content to send", path)
	}
	return anthropicmessages.Message{
		Role:    role,
		Content: anthropicmessages.Content{ContentBlocks: blocks},
	}, nil
}

// messagesToolResultBlock renders a function result as a tool_result block. The
// error status is carried natively, so no loss is recorded for it.
func messagesToolResultBlock(result CanonicalFunctionResult, path string) (anthropicmessages.ContentBlock, error) {
	id := result.CallID
	content, err := messagesResultContent(result.Parts, path)
	if err != nil {
		return anthropicmessages.ContentBlock{}, err
	}
	block := anthropicmessages.ContentBlock{
		Type:      anthropicmessages.ContentBlockTypeToolResult,
		ToolUseID: &id,
		Content:   &content,
	}
	if result.IsError {
		block.IsError = new(true)
	}
	return block, nil
}

// messagesResultContent renders tool result parts. An empty result is an
// empty string, the form the API accepts for a result with no content.
func messagesResultContent(parts []CanonicalPart, path string) (anthropicmessages.Content, error) {
	if len(parts) == 0 {
		empty := ""
		return anthropicmessages.Content{ContentStr: &empty}, nil
	}
	blocks := make([]anthropicmessages.ContentBlock, 0, len(parts))
	for j, part := range parts {
		partPath := fmt.Sprintf("%s.content[%d]", path, j)
		switch value := part.(type) {
		case CanonicalText:
			text := value.Text
			blocks = append(blocks, anthropicmessages.ContentBlock{
				Type: anthropicmessages.ContentBlockTypeText,
				Text: &text,
			})
		case CanonicalImage, CanonicalDocument:
			block, err := messagesMediaBlock(value, partPath)
			if err != nil {
				return anthropicmessages.Content{}, err
			}
			blocks = append(blocks, block)
		default:
			return anthropicmessages.Content{}, &UnrepresentableError{
				Protocol: "messages",
				Path:     partPath,
				Detail:   fmt.Sprintf("the tool result part %T has no Messages form", part),
			}
		}
	}
	return anthropicmessages.Content{ContentBlocks: blocks}, nil
}

// messagesMediaBlock renders an image or document as a base64 or url source.
// A file identifier has no Messages form and is rejected. A url source must
// carry its media type, because the wire form always emits media_type.
func messagesMediaBlock(part CanonicalPart, path string) (anthropicmessages.ContentBlock, error) {
	switch value := part.(type) {
	case CanonicalImage:
		source, err := messagesSource(value.MediaType, value.URL, value.Base64, path)
		if err != nil {
			return anthropicmessages.ContentBlock{}, err
		}
		return anthropicmessages.ContentBlock{
			Type:   anthropicmessages.ContentBlockTypeImage,
			Source: &source,
		}, nil
	case CanonicalDocument:
		if value.FileID != "" {
			return anthropicmessages.ContentBlock{}, &UnrepresentableError{
				Protocol: "messages",
				Path:     path,
				Detail:   "a file identifier document has no Messages form",
			}
		}
		source, err := messagesSource(value.MediaType, value.URL, value.Base64, path)
		if err != nil {
			return anthropicmessages.ContentBlock{}, err
		}
		return anthropicmessages.ContentBlock{
			Type:   anthropicmessages.ContentBlockTypeDocument,
			Source: &source,
		}, nil
	default:
		return anthropicmessages.ContentBlock{}, fmt.Errorf("%s: %T is not media", path, part)
	}
}

func messagesSource(mediaType, url, data, path string) (anthropicmessages.Source, error) {
	switch {
	case data != "" && url == "":
		return anthropicmessages.Source{
			Type:      anthropicmessages.SourceTypeBase64,
			MediaType: mediaType,
			Data:      data,
		}, nil
	case url != "" && data == "":
		if mediaType == "" {
			return anthropicmessages.Source{}, &UnrepresentableError{
				Protocol: "messages",
				Path:     path,
				Detail:   "a url source without a media type cannot be rendered",
			}
		}
		return anthropicmessages.Source{
			Type:      anthropicmessages.SourceTypeURL,
			MediaType: mediaType,
			URL:       url,
		}, nil
	default:
		return anthropicmessages.Source{}, &UnrepresentableError{
			Protocol: "messages",
			Path:     path,
			Detail:   "a media part must carry exactly one of inline data or a url",
		}
	}
}

// renderMessagesTools renders function tools and the tool choice. A disabled
// parallel-call request is carried by disable_parallel_tool_use on the choice.
func renderMessagesTools(
	request CanonicalRequest,
	context *ExchangeContext,
	out *anthropicmessages.Request,
	report *ConversionReport,
) error {
	for i, tool := range request.Tools {
		if tool.Strict != nil {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureToolSchemaStrictness,
				fmt.Sprintf("tools[%d].strict", i),
				"tool strictness cannot be enforced by an Anthropic Messages upstream",
			); err != nil {
				return err
			}
		}
		rendered := anthropicmessages.Tool{
			Name:        tool.Name,
			InputSchema: tool.JSONSchema,
		}
		if tool.Description != "" {
			description := tool.Description
			rendered.Description = &description
		}
		out.Tools = append(out.Tools, rendered)
	}
	if request.ToolChoice != nil {
		choice, err := messagesToolChoice(*request.ToolChoice)
		if err != nil {
			return err
		}
		out.ToolChoice = &choice
	}
	if request.ParallelTools != nil && !*request.ParallelTools && len(request.Tools) > 0 {
		choice := anthropicmessages.ToolChoice{Type: "auto"}
		if out.ToolChoice != nil {
			choice = *out.ToolChoice
		}
		choice.DisableParallelToolUse = new(true)
		out.ToolChoice = &choice
	}
	return nil
}

// messagesToolChoice maps the canonical tool choice onto the Messages choice.
func messagesToolChoice(choice CanonicalToolChoice) (anthropicmessages.ToolChoice, error) {
	switch choice.Mode {
	case "none":
		return anthropicmessages.ToolChoice{Type: "none"}, nil
	case "auto":
		return anthropicmessages.ToolChoice{Type: "auto"}, nil
	case "required":
		return anthropicmessages.ToolChoice{Type: "any"}, nil
	case "named":
		return anthropicmessages.ToolChoice{Type: "tool", Name: choice.Name}, nil
	default:
		return anthropicmessages.ToolChoice{}, fmt.Errorf("tool choice mode %q has no Messages form", choice.Mode)
	}
}

// renderMessagesMetadata carries metadata.user_id, the only metadata field the
// Messages API accepts. Every other key is an approved metadata loss.
func renderMessagesMetadata(
	request CanonicalRequest,
	context *ExchangeContext,
	out *anthropicmessages.Request,
	report *ConversionReport,
) error {
	for _, key := range slices.Sorted(maps.Keys(request.Metadata)) {
		if key == "user_id" {
			if out.Metadata == nil {
				out.Metadata = map[string]any{}
			}
			out.Metadata["user_id"] = request.Metadata[key]
			continue
		}
		if err := report.Lose(
			context.lossPolicy(),
			FeatureMetadata,
			"metadata."+key,
			"the Anthropic Messages metadata field carries only user_id",
		); err != nil {
			return err
		}
	}
	return nil
}
