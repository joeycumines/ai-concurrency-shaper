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
	turnPath string,
	role ChatMessageRole,
	policy LossPolicy,
	report *ConversionReport,
) (ChatMessage, error) {
	var blocks []ChatContentBlock
	dropped := 0
	for i, part := range turn.Parts {
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
				fmt.Sprintf("%s.parts[%d]", turnPath, i),
				"non-text system content cannot be carried by a chat system message",
			); err != nil {
				return ChatMessage{}, err
			}
			dropped++
		case CanonicalThinkingPart:
			// Marker-signature thinking is proxy-synthesized reasoning: the
			// request path scrubs it from replayed history before upstream
			// rendering, so it never reaches a Chat upstream. Any other
			// signature was already refused by ValidateCanonicalRequest, which
			// is the only place the scrub can be made provably safe.
			//
			// It counts as dropped so an all-scrubbed system or developer turn
			// takes the same path as an all-dropped one (an empty text block
			// the caller omits) instead of being refused as "empty".
			dropped++
			continue
		default:
			// Defensive: no request path reaches this arm today, because
			// ValidateCanonicalRequest pre-empts refusal/call/result in a
			// system turn and CanonicalThinkingPart is built only on a
			// response path. It names the offending part type and the turn
			// role so that IF it is ever reached the operator can diagnose
			// it - a message that merely repeated the path would not.
			return ChatMessage{}, &UnsupportedFeatureError{
				Protocol: "chat",
				Path:     fmt.Sprintf("%s.parts[%d]", turnPath, i),
				Feature:  fmt.Sprintf("non-text %T in %s message", part, role),
			}
		}
	}
	// A system/developer turn is text-only; omit content when there are no
	// parts rather than emitting an invalid empty union.
	if len(blocks) == 0 {
		if dropped > 0 {
			// Every part was dropped under an approved loss or scrubbed as
			// proxy-synthesized thinking. Emit an empty text block so the
			// message stays a valid content union; the caller then omits it,
			// because an empty prompt is proxy-invented text the model would
			// read as an instruction the client never wrote. An all-dropped
			// conversation is still refused by the caller's own
			// len(out.Messages) == 0 guard, matching the Responses target.
			empty := ""
			blocks = append(blocks, ChatContentBlock{
				Type: ChatContentBlockTypeText,
				Text: &empty,
			})
		} else {
			// Defensive: a turn with no parts at all is refused by
			// ValidateCanonicalRequest, which every renderer passes first, so
			// no request path reaches this arm.
			return ChatMessage{}, errors.New("empty system or developer turn")
		}
	}
	return ChatMessage{Role: role, Content: &ChatMessageContent{ContentBlocks: blocks}}, nil
}

// consolidateChatSystemMessages turns the consolidating instruction messages
// into the single leading system message: one message's blocks are carried
// verbatim (so a lone system turn keeps its multi-block content and any image
// numbering), and several are joined into one text block.
func consolidateChatSystemMessages(messages []ChatMessage) ChatMessage {
	if len(messages) == 1 {
		return messages[0]
	}
	return joinChatSystemMessages(messages)
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
		if message.Content == nil {
			// A contentless message contributes no text; its own caller has
			// already decided not to emit it.
			continue
		}
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

// chatMessageTextIsEmpty reports whether a rendered system-channel message
// carries no text bytes at all. Such a message is proxy-invented: it exists only
// because canonicalTextTurnToChatMessage synthesizes an empty text block for a
// turn whose parts were all dropped, and emitting it would send the upstream an
// empty system prompt the client never wrote.
func chatMessageTextIsEmpty(message ChatMessage) bool {
	if message.Content == nil {
		return false
	}
	for _, block := range message.Content.ContentBlocks {
		if block.Text != nil && *block.Text != "" {
			return false
		}
	}
	return true
}

// canonicalUserTurnToChatMessages renders a user turn into Chat messages:
// function results become tool-role messages and the remaining content
// becomes one user message. Image input requires the configured capability.
//
// A segment that rendered no blocks is SKIPPED rather than refused: the turn
// is not empty, it has already produced the tool messages around that segment,
// and refusing it would reject a turn whose only "empty" part was proxy
// history the renderer itself scrubbed. The turn's parts are numbered by their
// canonical index, so a part's report path survives the segmenting.
func canonicalUserTurnToChatMessages(
	turn CanonicalTurn,
	turnPath string,
	capabilities ChatCapabilities,
	report *ConversionReport,
	policy LossPolicy,
) ([]ChatMessage, error) {
	var (
		contentParts   []CanonicalPart
		contentIndexes []int
		messages       []ChatMessage
	)
	flush := func() error {
		message, produced, err := canonicalContentPartsToChatUserMessage(
			contentParts,
			contentIndexes,
			turnPath,
			capabilities,
			report,
			policy,
		)
		if err != nil {
			return err
		}
		if produced {
			messages = append(messages, message)
		}
		contentParts = nil
		contentIndexes = nil
		return nil
	}
	for i, part := range turn.Parts {
		if value, ok := part.(CanonicalFunctionResult); ok {
			if len(contentParts) > 0 {
				if err := flush(); err != nil {
					return nil, err
				}
			}
			// Tool messages use the dedicated tool-result renderer — never
			// the user-message renderer.
			toolMessage, err := renderChatToolResult(
				value,
				fmt.Sprintf("%s.parts[%d]", turnPath, i),
				capabilities,
				policy,
				report,
			)
			if err != nil {
				return nil, err
			}
			messages = append(messages, toolMessage)
			continue
		}
		contentParts = append(contentParts, part)
		contentIndexes = append(contentIndexes, i)
	}
	if len(contentParts) > 0 {
		if err := flush(); err != nil {
			return nil, err
		}
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
// configured capability. indexes holds each part's canonical index in the
// turn, so a drop names the part the operator actually sent. produced is false
// when every part scrubbed or dropped and nothing remains to send.
func canonicalContentPartsToChatUserMessage(
	parts []CanonicalPart,
	indexes []int,
	turnPath string,
	capabilities ChatCapabilities,
	report *ConversionReport,
	policy LossPolicy,
) (ChatMessage, bool, error) {
	var blocks []ChatContentBlock
	for i, part := range parts {
		partPath := fmt.Sprintf("%s.parts[%d]", turnPath, indexes[i])
		switch value := part.(type) {
		case CanonicalText:
			// A client-authored empty text part is forwarded verbatim. That is
			// deliberate and differs from the SYSTEM-channel rule: there, an
			// empty block is proxy-invented filler and is dropped. Here it can
			// be the Messages decoder's own synthesis for a dialog turn whose
			// blocks were all proxy-synthesized thinking (see
			// messagesrequestdecode), which exists precisely to PRESERVE the
			// turn boundary. Dropping it would either delete an assistant turn
			// the model is expected to see or refuse the request outright, so
			// preserving the boundary wins over suppressing zero bytes.
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
					partPath,
					"image input is not supported by the configured chat provider",
				); err != nil {
					return ChatMessage{}, false, err
				}
				continue
			}
			url := value.URL
			if url == "" {
				var err error
				url, err = imageDataURL(value.MediaType, value.Base64)
				if err != nil {
					return ChatMessage{}, false, fmt.Errorf("content part %d: %w", i, err)
				}
			}
			detail, err := chatImageDetail(value.Detail, partPath, policy, report)
			if err != nil {
				return ChatMessage{}, false, err
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
				partPath,
				"document input is not supported by the configured chat provider",
			); err != nil {
				return ChatMessage{}, false, err
			}
			continue

		case CanonicalRefusal:
			return ChatMessage{}, false, &UnsupportedFeatureError{
				Protocol: "chat",
				Path:     partPath,
				Feature:  "refusal input content",
			}

		case CanonicalThinkingPart:
			// A marker-signature thinking part is proxy-synthesized reasoning
			// the request path scrubs in every turn role; anything else was
			// already refused by ValidateCanonicalRequest.
			continue

		default:
			return ChatMessage{}, false, fmt.Errorf(
				"content part %d: unknown canonical part %T",
				i,
				part,
			)
		}
	}
	if len(blocks) == 0 {
		return ChatMessage{}, false, nil
	}
	return ChatMessage{Role: ChatMessageRoleUser, Content: &ChatMessageContent{ContentBlocks: blocks}}, true, nil
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
			// rendering, so it never reaches a Chat upstream. Any other
			// signature was refused by ValidateCanonicalRequest, which is the
			// only gate that inspects turn parts (the artifact gate covers
			// request.Artifacts).
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
	// A turn whose every part was scrubbed (only marker-signature thinking can
	// be) would become a bodiless assistant message: no content, no tool calls,
	// no refusal. The upstream would have to interpret a turn carrying nothing,
	// and the operator would see a message the client never wrote. Refuse it
	// locally instead; the Messages decoder synthesizes an empty text part for
	// exactly this shape, so a decoder-produced request never reaches here.
	if len(blocks) == 0 && len(toolCalls) == 0 && refusal == nil {
		return ChatMessage{}, &UnrepresentableError{
			Protocol: "chat",
			Path:     turnPath,
			Detail:   "the turn has no content that a chat assistant message can carry after scrubbing proxy-synthesized thinking",
		}
	}
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
