package transcode

import (
	"encoding/json"
	"fmt"
	"strings"
)

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
// user-message renderer. Exact text results stay exact
// text. Image, document, or mixed results render as multipart content blocks
// (text and image_url parts, order preserved) so a vision model can see the
// image when the ToolResultImages capability is enabled. Otherwise they fall
// back to the observable encoding: the deterministic JSON text envelope
// (transcode_version 1), gated by the tool_result_multimodal_content and
// tool_result_json_envelope permissions and recorded in the conversion
// report. An error is always one of the policy refusals those keys report or a
// typed local unrepresentable shape — never an upstream wire error.
func renderChatToolResult(
	result CanonicalFunctionResult,
	capabilities ChatCapabilities,
	policy LossPolicy,
	report *ConversionReport,
) (ChatMessage, error) {
	parts := result.Parts
	if result.IsError {
		// The error status cannot be carried by a Chat tool message; the
		// permissive encoding is the visible error_status_prefix text.
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
	// message content union. A single text part renders byte-exact; a
	// multi-part all-text result is joined with '\n' and recorded as a
	// sanctioned Note — the join preserves every content byte (the part
	// boundaries are structural metadata the chat dialect cannot carry),
	// so it is an observable sanctioned encoding, never a policy-gated
	// loss and never silent (acceptance: a
	// Note, NOT a policy-gated loss — default-rejecting the join made
	// multi-part tool results a live availability failure with Claude
	// Code). The count is over the client-supplied parts only — the
	// synthetic error_status_prefix part is not a client part boundary
	// (its own encoding is reported under tool_result_error_status).
	if allTextParts(parts) {
		if len(result.Parts) > 1 {
			if err := report.Note(
				FeatureToolResultTextJoin,
				"messages[].tool_result.content",
				"multi-part all-text tool result joined into one '\\n'-separated string (the chat tool message cannot carry part boundaries; every content byte is preserved)",
			); err != nil {
				return ChatMessage{}, err
			}
		}
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

	// The upstream accepts image parts inside a tool message: render the
	// multimodal content as multipart blocks (text and image_url, order
	// preserved). This is a truthful lossless encoding of the client's
	// content, so it is preferred whenever the capability is on; only the
	// image parts require the ImageInput rendering vocabulary.
	if capabilities.ToolResultImages {
		toolMessage, ok, err := renderChatToolResultMultipart(result, parts, capabilities, policy, report)
		if err != nil {
			return ChatMessage{}, err
		}
		if ok {
			return toolMessage, nil
		}
	}

	// Multimodal content: the content-shape loss and the sanctioned
	// encoding loss gate the deterministic JSON text envelope. The policy error
	// from Lose is returned as-is: it is already local, typed, and names the
	// exact feature key that refused, which a generic UnrepresentableError
	// would hide from the operator.
	if err := report.Lose(
		policy,
		FeatureToolResultMultimodalContent,
		"messages[].tool_result.content",
		"the multimodal tool-result content cannot be carried exactly by a Chat tool message",
	); err != nil {
		return ChatMessage{}, err
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
		return ChatMessage{}, err
	}
	callID := result.CallID
	return ChatMessage{
		Role:            ChatMessageRoleTool,
		Content:         &ChatMessageContent{ContentStr: &envelopeText},
		ChatToolMessage: &ChatToolMessage{ToolCallID: &callID},
	}, nil
}

// renderChatToolResultMultipart renders multimodal tool-result content as
// multipart Chat tool-message content blocks: text parts as text blocks and
// image parts as image_url blocks (data URLs), order preserved. It reports
// ok=false when a part cannot be carried as multipart content (a document,
// or an image whose media type is outside the Chat vocabulary), leaving the
// caller to take its loss/reject decision. Image parts require the
// ImageInput rendering vocabulary in addition to ToolResultImages.
func renderChatToolResultMultipart(
	result CanonicalFunctionResult,
	parts []CanonicalPart,
	capabilities ChatCapabilities,
	policy LossPolicy,
	report *ConversionReport,
) (ChatMessage, bool, error) {
	if !capabilities.ImageInput {
		return ChatMessage{}, false, nil
	}
	// The attempt is TRANSACTIONAL. This renderer may DECLINE a shape the
	// multipart form cannot represent (a document, an image media type the
	// encoder rejects, no blocks) and the caller then falls back to the
	// envelope, which carries no detail field and never renders a part.
	// Recording into the caller's report as we go would leave notes and
	// policy-gated losses describing content that never reached the wire, so a
	// scratch report is collected and committed only on success.
	//
	// A decision the POLICY already took is NOT a decline. Propagating it is the
	// only honest outcome:
	//   - a denied image_detail_original means the operator refused the
	//     downgrade to "high". The envelope has no detail field at all, so
	//     falling back would apply a total drop — strictly stronger than the
	//     downgrade that was refused.
	//   - an invalid detail value is malformed client input. Declining would
	//     serve it with the value silently re-encoded away, while the same value
	//     on a user-message image hard-fails: the content location must not
	//     decide the verdict.
	scratch := &ConversionReport{}
	var blocks []ChatContentBlock
	for _, part := range parts {
		switch value := part.(type) {
		case CanonicalText:
			text := value.Text
			blocks = append(blocks, ChatContentBlock{
				Type: ChatContentBlockTypeText,
				Text: &text,
			})

		case CanonicalImage:
			url := value.URL
			if url == "" {
				var err error
				url, err = imageDataURL(value.MediaType, value.Base64)
				if err != nil {
					return ChatMessage{}, false, nil
				}
			}
			detail, err := chatImageDetail(value.Detail, "messages[].tool_result.content", policy, scratch)
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

		default:
			// A document (or any other part) is not carried by the
			// multipart tool-message form: fall back to the JSON envelope,
			// which can represent every part shape.
			return ChatMessage{}, false, nil
		}
	}
	if len(blocks) == 0 {
		return ChatMessage{}, false, nil
	}
	report.absorb(*scratch)
	callID := result.CallID
	return ChatMessage{
		Role:            ChatMessageRoleTool,
		Content:         &ChatMessageContent{ContentBlocks: blocks},
		ChatToolMessage: &ChatToolMessage{ToolCallID: &callID},
	}, true, nil
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
			if value.FileID != "" {
				// The Chat envelope has no file-id arm: it can carry inline
				// data or a url, but an id-only document names content the
				// envelope cannot express. Saying so — and naming the
				// identifier, which is the only handle the operator has —
				// is the honest outcome; routing it through encodeDataURL
				// would fail with "empty data", which misdescribes the
				// problem entirely.
				return "", &UnrepresentableError{
					Protocol: "chat",
					Path:     "messages[].tool_result.content",
					Detail:   fmt.Sprintf("a file identifier document (%q) has no Chat tool-result form", value.FileID),
				}
			}
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
