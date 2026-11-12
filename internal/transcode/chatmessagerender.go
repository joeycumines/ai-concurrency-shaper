package transcode

import (
	"errors"
	"fmt"
	"strings"
)

// canonicalTextTurnToChatMessage renders a system or developer turn into a
// Chat message with the given role. Text is portable; a non-text part (an
// image or document) follows the same loss/reject decision the Responses
// target applies under system_non_text_content — it is either dropped
// observably (the part cannot be expressed as a system message and the
// upstream was told to accept the loss) or rejected with a stable,
// client-dialect feature name (never a leaked Go type). A drop never
// silently empties the turn: an all-non-text system turn becomes an empty
// text block so the message shape stays valid.
func canonicalTextTurnToChatMessage(
	turn CanonicalTurn,
	role ChatMessageRole,
	policy LossPolicy,
	report *ConversionReport,
) (ChatMessage, error) {
	var blocks []ChatContentBlock
	dropped := 0
	for _, part := range turn.Parts {
		switch value := part.(type) {
		case CanonicalText:
			text := value.Text
			blocks = append(blocks, ChatContentBlock{
				Type: ChatContentBlockTypeText,
				Text: &text,
			})
		case CanonicalImage, CanonicalDocument:
			if err := report.Lose(
				policy,
				FeatureSystemNonTextContent,
				"messages[].content",
				"non-text system content cannot be carried by a chat system message",
			); err != nil {
				return ChatMessage{}, err
			}
			dropped++
		default:
			// Defensive: no request path reaches this arm today, because
			// ValidateCanonicalRequest pre-empts refusal/call/result in a
			// system turn and CanonicalThinkingPart is built only on a
			// response path. It names the offending part type and the turn
			// role so that IF it is ever reached the operator can diagnose
			// it - a message that merely repeated the path would not.
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
		if dropped > 0 {
			// Every part was dropped under an approved loss. Emit an empty
			// text block so the system message stays a valid content union -
			// but ONLY when this turn is not the whole conversation. A request
			// whose entire conversation is a dropped system turn has no
			// representable content at all, and admitting it here would
			// disagree with the Responses target, which rejects exactly this
			// shape precisely to match. Callers reject a degenerate rendered
			// set; see the guard in RenderChatRequest.
			empty := ""
			blocks = append(blocks, ChatContentBlock{
				Type: ChatContentBlockTypeText,
				Text: &empty,
			})
		} else {
			return ChatMessage{}, errors.New("empty system or developer turn")
		}
	}
	return ChatMessage{Role: role, Content: &ChatMessageContent{ContentBlocks: blocks}}, nil
}

// joinChatSystemMessages consolidates rendered system-channel messages into
// one leading system message, joining their text with "\n\n" in encounter
// order. Every input message is text-only (canonicalTextTurnToChatMessage
// rejects anything else), so the join cannot lose content. An EMPTY text block
// carries no bytes and is skipped: joining it would invent a leading, trailing,
// or doubled separator that the model would read as prompt text, and it would
// also let an all-dropped system turn slip past the degenerate-conversation
// guard in RenderChatRequest.
func joinChatSystemMessages(messages []ChatMessage) ChatMessage {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		for _, block := range message.Content.ContentBlocks {
			if block.Text != nil && *block.Text != "" {
				texts = append(texts, *block.Text)
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
			// the user-message renderer.
			toolMessage, err := renderChatToolResult(value, capabilities, policy, report)
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

// chatImageDetail maps a canonical image detail to the Chat vocabulary
// (auto|low|high). Any other unexpected value is rejected rather than
// forwarded unvalidated. An empty value is the source dialect having no detail
// field (an Anthropic image block), and the documented "auto" default is
// chosen with the invention recorded as a note: that is PROXY-invented
// content, not a client request, so it takes no policy decision.
//
// The Responses-only "original" is different: the CLIENT asked for it, and
// mapping it to "high" downgrades a fidelity-only knob. AGENTS.md requires
// such a mapping to be an observable POLICY-GATED loss so an operator can
// refuse the downgrade, so it goes through Lose with the policy rather than
// Note (which never consults the policy and would leave the operator no say).
// The key is in defaultTranscodeLosses, so a default deployment keeps working
// while an operator can still reject it.
func chatImageDetail(
	detail string,
	path string,
	policy LossPolicy,
	report *ConversionReport,
) (string, error) {
	switch detail {
	case "":
		if err := report.Note(
			FeatureImageDetailInvented,
			path,
			"the source carried no image detail; the documented 'auto' default was chosen (the source dialect may have no detail field, or the client omitted the optional one)",
		); err != nil {
			return "", err
		}
		return "auto", nil
	case "auto", "low", "high":
		return detail, nil
	case "original":
		if err := report.Lose(
			policy,
			FeatureImageDetailOriginal,
			path,
			"the Responses-only image detail 'original' has no Chat equivalent; it was mapped to 'high' (the closest truthful semantic, but a downgrade the operator can refuse)",
		); err != nil {
			return "", err
		}
		return "high", nil
	default:
		return "", &UnsupportedFeatureError{
			Protocol: "chat",
			Path:     path,
			Feature:  "image detail " + detail,
		}
	}
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
			detail, err := chatImageDetail(value.Detail, "messages[].content", policy, report)
			if err != nil {
				return ChatMessage{}, err
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
			// the image-input decision.
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
// message with content, refusal, and tool calls. replay resolves each call's
// namespace qualifier for THIS exchange (see namespaceReplayResolve).
func canonicalAssistantTurnToChatMessage(
	turn CanonicalTurn,
	turnPath string,
	replay namespaceReplayResolve,
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

		case CanonicalThinkingPart:
			// Marker-signature thinking is proxy-synthesized reasoning: the
			// request path scrubs it from replayed history before upstream
			// rendering, so it never reaches a Chat upstream. A non-marker
			// thinking part is a source-authenticated artifact gated by
			// RequirePortableArtifacts, which runs before this loop.
			continue

		case CanonicalFunctionCall:
			callID := value.CallID
			name, err := replay.wireName(value, fmt.Sprintf("%s.parts[%d]", turnPath, i))
			if err != nil {
				return ChatMessage{}, err
			}
			arguments := string(value.Arguments)
			if arguments == "" {
				arguments = "{}"
			}
			// The official non-stream assistant tool-call shape carries id,
			// function, and type with NO index. The
			// type field is required on the wire and always emitted as
			// "function" — never omitted.
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
