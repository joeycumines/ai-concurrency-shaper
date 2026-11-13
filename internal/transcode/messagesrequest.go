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
		context.lossPolicy(),
		&report,
	); err != nil {
		return nil, report, err
	}
	// Responses request fields the Messages wire cannot reproduce take the
	// shared envelope decision, so a Responses-sourced exchange never drops a
	// client field silently. Messages has no reasoning-effort control, no user
	// field (metadata.user_id is not the same semantic), and no store.
	if err := responsesEchoLosses(context, &report, echoCarriage{}); err != nil {
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
	// An enabled thinking request with no budget is provider intent this wire
	// cannot express: its enabled arm requires budget_tokens. Chat and Responses
	// note that shape and emit nothing, so this target does the same rather than
	// rendering a union the upstream rejects. Decode rejects it today; a
	// hand-built CanonicalRequest or a future decode relaxation must never pass
	// it silently.
	if request.Thinking != nil && request.Thinking.Type == "enabled" && request.Thinking.BudgetTokens == nil {
		if err := report.Note(
			FeatureRequestReasoning,
			"thinking",
			"thinking enabled without a budget maps to the Anthropic Messages upstream's own default; the enabled arm requires budget_tokens, so no thinking config is sent",
		); err != nil {
			return nil, report, err
		}
	} else if request.Thinking != nil {
		out.Thinking = &anthropicmessages.ThinkingConfig{
			Type:         request.Thinking.Type,
			BudgetTokens: request.Thinking.BudgetTokens,
		}
		// The Messages target carries the client's thinking display
		// preference natively.
		if request.Thinking.Display != "" {
			display := request.Thinking.Display
			out.Thinking.Display = &display
		}
	}
	if request.StructuredOutput != nil {
		// The decision names the canonical feature: the loss is the structured
		// output, and the field that happens to carry it in one dialect is not
		// a path a Gemini-sourced schema would ever have.
		if err := report.Lose(
			context.lossPolicy(),
			FeatureStructuredOutput,
			"structured_output",
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
			FeatureMessagesMaxTokensFromModelTable,
			"max_tokens",
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
	replay := newNamespaceReplayResolve(context.toolNames(), request.Tools, context.lossPolicy(), report)
	var system []anthropicmessages.ContentBlock
	// contributing holds the canonical index of every system-channel turn that
	// actually produced system content. The join decision counts only these: a
	// turn whose parts were all dropped contributed nothing, so it must not
	// produce a note claiming a merge that never happened.
	var contributing []int
	// sawClientDialog and sawRetainedDialog are tracked separately for the same
	// reason the Chat renderer tracks them: a client-authored system turn that
	// physically follows RETAINED dialog is a position the PROXY created, so it
	// must not be charged to the client. A retained system turn, by contrast,
	// is mid-dialog as soon as any dialog precedes it.
	sawClientDialog := false
	sawRetainedDialog := false
	for i, turn := range request.Turns {
		// Paths name the CANONICAL turn, matching the Chat, Responses and
		// Gemini renderers: out.Messages receives only user and assistant
		// turns, so a wire index would disagree with the canonical one as soon
		// as a system turn precedes. Content that renders under the system
		// field is reported under system.parts, never under messages.
		path := fmt.Sprintf("turns[%d]", i)
		retained := i < request.RetainedTurns
		switch turn.Role {
		case CanonicalSystem, CanonicalDeveloper:
			// Render the turn's blocks FIRST: a turn whose parts all took a
			// drop decision contributes nothing, so it occupies no position and
			// takes no role decision. Charging before knowing the contribution
			// would bill the client for a move that never happened.
			blocks, err := messagesSystemBlocks(turn, path, context.lossPolicy(), report)
			if err != nil {
				return err
			}
			if len(blocks) == 0 {
				continue
			}
			if turn.Role == CanonicalDeveloper {
				if err := chargePosition(
					request,
					i,
					FeatureDeveloperRole,
					path,
					"developer role is not supported by the Anthropic Messages upstream; developer content renders as system content",
					report,
					context.lossPolicy(),
				); err != nil {
					return err
				}
			}
			midDialog := sawClientDialog
			if retained {
				midDialog = sawRetainedDialog || sawClientDialog
			}
			if midDialog {
				// Only a position this request created may be charged; a
				// retained mid-conversation turn is the proxy's own history,
				// already consolidated once when the chain was first rendered.
				if err := chargePosition(
					request,
					i,
					FeatureMidConversationSystem,
					path,
					"mid-conversation system turns cannot keep their position in a Messages request; they consolidate into the leading system field",
					report,
					context.lossPolicy(),
				); err != nil {
					return err
				}
			}
			system = append(system, blocks...)
			contributing = append(contributing, i)

		case CanonicalUser, CanonicalAssistant:
			if retained {
				sawRetainedDialog = true
			} else {
				sawClientDialog = true
			}
			message, err := messagesTurn(turn, path, replay)
			if err != nil {
				return err
			}
			out.Messages = append(out.Messages, message)

		default:
			return fmt.Errorf("%s: canonical role %q has no Messages form", path, turn.Role)
		}
	}
	// The join decision is the shared one every target takes when several
	// system turns must share one system field: a sanctioned ENCODING (content
	// is joined, never dropped), so it consults the policy only for the CLIENT's
	// own surplus. Keeping it shared is what lets one policy control the same
	// encoding on every target.
	if err := chargeSystemTurnJoin(
		request,
		contributing,
		"system",
		"multiple system turns consolidate into one Messages system field",
		report,
		context.lossPolicy(),
	); err != nil {
		return err
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

// messagesSystemBlocks renders a system or developer turn as text blocks. Only
// text is representable in the system field, so a non-text part (an image or a
// document) takes the same system_non_text_content decision the Chat, Responses
// and Gemini targets apply: dropped observably when the operator approves the
// loss, and refused with the feature key otherwise — never a harder rule than
// the other targets. turnPath names the canonical turn the part belongs to,
// which is where the operator can find it; a part that is dropped never lands in
// the system field, so naming that field would point at content it never
// entered. An empty text part carries no bytes, so it is skipped rather than
// emitted as an empty block: the system field must never carry proxy-invented
// empty prompt text, matching the Responses target, which omits its instructions
// string for the identical shape.
func messagesSystemBlocks(
	turn CanonicalTurn,
	turnPath string,
	policy LossPolicy,
	report *ConversionReport,
) ([]anthropicmessages.ContentBlock, error) {
	var blocks []anthropicmessages.ContentBlock
	for j, part := range turn.Parts {
		text, ok := part.(CanonicalText)
		if !ok {
			if _, thinking := part.(CanonicalThinkingPart); thinking {
				// Marker-signature thinking is proxy-synthesized reasoning the
				// request path scrubs from replayed history; every renderer
				// drops it in every turn role rather than charging the client
				// for losing content it never wrote.
				continue
			}
			if err := report.Lose(
				policy,
				FeatureSystemNonTextContent,
				fmt.Sprintf("%s.parts[%d]", turnPath, j),
				"non-text system content cannot be carried by the Anthropic Messages system field",
			); err != nil {
				return nil, err
			}
			continue
		}
		if text.Text == "" {
			continue
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
func messagesTurn(turn CanonicalTurn, path string, replay namespaceReplayResolve) (anthropicmessages.Message, error) {
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
					"tool_result blocks must precede other blocks in a Messages user message",
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
			// The call's namespace qualifier is resolved for THIS exchange:
			// the Messages wire carries only the flat name.
			name, err := replay.wireName(value, partPath)
			if err != nil {
				return anthropicmessages.Message{}, err
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

		case CanonicalThinkingPart:
			// Marker-signature thinking is proxy-synthesized reasoning: the
			// request path scrubs it from replayed history before upstream
			// rendering, so it never reaches a Messages upstream. Any other
			// signature was refused by ValidateCanonicalRequest, which is the
			// only gate that inspects turn parts (the artifact gate covers
			// request.Artifacts).
			continue

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
		// strict:false asserts NO enforcement, which is exactly what the
		// Messages wire does, so only a truthy strict is a loss: gating on
		// presence would refuse every Responses-sourced tool while losing
		// nothing.
		if tool.Strict != nil && *tool.Strict {
			if err := report.Lose(
				context.lossPolicy(),
				FeatureToolSchemaStrictness,
				fmt.Sprintf("tools[%d].strict", i),
				"tool strictness cannot be enforced by an Anthropic Messages upstream",
			); err != nil {
				return err
			}
		}
		// The Anthropic tool contract requires input_schema. A source tool may
		// omit its parameters (a Gemini function declaration need not carry a
		// schema), and an absent one means "an object with no declared
		// properties", not "no schema": emitting none is a malformed request.
		schema := tool.JSONSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		rendered := anthropicmessages.Tool{
			Name:        tool.Name,
			InputSchema: schema,
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
	// disable_parallel_tool_use rides on the choice, and the setting is a boolean
	// in BOTH directions (an explicit false asserts that parallel calls are
	// allowed). The two inert conditions — no tools declared, or a choice that
	// forbids every call — are Notes from the shared helper, so they are never
	// policy decisions: a default-deny key there would refuse a request that
	// loses nothing, and no field may be written when no call can happen.
	//
	// A choice the client WROTE carries the value as sent. When the client wrote
	// no choice, only a real disable justifies synthesizing one: carrying an
	// "allowed" needs no field at all, because every dialect's default allows
	// parallel calls, so inventing a tool_choice for it would put a field on the
	// wire the client never asked for.
	if request.ParallelTools != nil {
		inert, err := chargeParallelToolsInert(request, "parallel_tool_calls", report)
		if err != nil {
			return err
		}
		if !inert {
			disabled := !*request.ParallelTools
			switch {
			case out.ToolChoice != nil:
				choice := *out.ToolChoice
				choice.DisableParallelToolUse = &disabled
				out.ToolChoice = &choice
			case disabled:
				out.ToolChoice = &anthropicmessages.ToolChoice{
					Type:                   "auto",
					DisableParallelToolUse: &disabled,
				}
			}
		}
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
