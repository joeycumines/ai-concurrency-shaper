package transcode

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

// DecodeMessagesResponse decodes an Anthropic Messages response into the
// canonical response. The envelope decodes tolerantly, so provider extension
// fields are skipped, while each content block decodes strictly through its
// per-arm decoder. Thinking and server-side blocks are approved losses under
// policy or rejections. A usage object that is absent makes every count
// unknown; a usage object that is present is known.
func DecodeMessagesResponse(
	body []byte,
	policy LossPolicy,
) (CanonicalResponse, ConversionReport, error) {
	var report ConversionReport
	var resp anthropicmessages.Response
	if err := wire.DecodeTolerant(body, &resp); err != nil {
		return CanonicalResponse{}, report, upstreamWireError(UpstreamMessages, http.StatusOK, err)
	}
	if resp.Type != "message" {
		return CanonicalResponse{}, report, upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			fmt.Errorf("anthropic response type %q is not message", resp.Type),
		)
	}
	if resp.Role != string(anthropicmessages.RoleAssistant) {
		return CanonicalResponse{}, report, upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			fmt.Errorf("anthropic response role %q is not assistant", resp.Role),
		)
	}

	status, stop, incomplete, err := messagesStop(resp)
	if err != nil {
		return CanonicalResponse{}, report, err
	}
	items, err := messagesItems(resp.Content, policy, &report)
	if err != nil {
		return CanonicalResponse{}, report, err
	}

	out := CanonicalResponse{
		ID:               resp.ID,
		Model:            resp.Model,
		Status:           status,
		Stop:             stop,
		Items:            items,
		Usage:            messagesUsage(resp.Usage),
		IncompleteReason: incomplete,
	}
	if err := ValidateCanonicalResponse(out); err != nil {
		return CanonicalResponse{}, report, upstreamWireError(UpstreamMessages, http.StatusOK, err)
	}
	return out, report, nil
}

// messagesStop maps the stop reason to the canonical status and stop. A
// max_tokens stop is an incomplete response. pause_turn asks the client to
// resend the conversation, which a single response cannot express.
func messagesStop(resp anthropicmessages.Response) (
	CanonicalResponseStatus,
	CanonicalStop,
	string,
	error,
) {
	if resp.StopReason == nil {
		return "", CanonicalStop{}, "", upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			errors.New("anthropic final response has no stop_reason"),
		)
	}
	switch *resp.StopReason {
	case anthropicmessages.StopReasonEndTurn:
		return CanonicalResponseCompleted, CanonicalStop{Reason: CanonicalStopEndTurn}, "", nil
	case anthropicmessages.StopReasonStopSequence:
		sequence := ""
		if resp.StopSequence != nil {
			sequence = *resp.StopSequence
		}
		return CanonicalResponseCompleted, CanonicalStop{
			Reason:   CanonicalStopStopSequence,
			Sequence: sequence,
		}, "", nil
	case anthropicmessages.StopReasonToolUse:
		return CanonicalResponseCompleted, CanonicalStop{Reason: CanonicalStopToolUse}, "", nil
	case anthropicmessages.StopReasonMaxTokens:
		return CanonicalResponseIncomplete, CanonicalStop{Reason: CanonicalStopMaxTokens}, "max_output_tokens", nil
	case anthropicmessages.StopReasonRefusal:
		// A filtered response is incomplete with reason content_filter, as
		// the streaming decode and the Gemini decode represent it.
		return CanonicalResponseIncomplete, CanonicalStop{Reason: CanonicalStopRefusal}, "content_filter", nil
	case anthropicmessages.StopReasonPauseTurn:
		return "", CanonicalStop{}, "", &UnrepresentableError{
			Protocol: "messages",
			Path:     "stop_reason",
			Detail:   "pause_turn asks the client to resend the conversation and has no single-response form",
		}
	default:
		return "", CanonicalStop{}, "", upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			fmt.Errorf("anthropic stop_reason %q is unknown", *resp.StopReason),
		)
	}
}

// messagesItems converts the content blocks in order. Consecutive text blocks
// form one message item, and each tool call is its own item, so the order of
// text and calls is kept.
func messagesItems(
	blocks []anthropicmessages.ContentBlock,
	policy LossPolicy,
	report *ConversionReport,
) ([]CanonicalResponseItem, error) {
	var items []CanonicalResponseItem
	var message *CanonicalMessageItem
	for i := range blocks {
		block := &blocks[i]
		path := fmt.Sprintf("content[%d]", i)
		switch block.Type {
		case anthropicmessages.ContentBlockTypeText:
			if len(block.Citations) > 0 {
				return nil, &UnrepresentableError{
					Protocol: "messages",
					Path:     path + ".citations",
					Detail:   "text citations have no Gemini response form",
				}
			}
			if message == nil {
				message = &CanonicalMessageItem{Role: CanonicalAssistant}
				items = append(items, message)
			}
			message.Parts = append(message.Parts, CanonicalText{Text: derefString(block.Text)})

		case anthropicmessages.ContentBlockTypeToolUse:
			message = nil
			args := ParseToolArguments(string(block.Input))
			if !args.IsObject {
				return nil, upstreamWireError(
					UpstreamMessages,
					http.StatusOK,
					fmt.Errorf("%s tool_use input is not a JSON object", path),
				)
			}
			items = append(items, &CanonicalFunctionCallItem{
				CallID:    derefString(block.ID),
				Name:      derefString(block.Name),
				Arguments: args,
			})

		case anthropicmessages.ContentBlockTypeThinking,
			anthropicmessages.ContentBlockTypeRedactedThinking:
			message = nil
			if err := report.Lose(
				policy,
				FeatureAuthenticatedThinking,
				path,
				"the provider's authenticated thinking block cannot be reproduced in a Gemini response",
			); err != nil {
				return nil, err
			}

		case anthropicmessages.ContentBlockTypeServerToolUse,
			anthropicmessages.ContentBlockTypeWebSearchToolResult,
			anthropicmessages.ContentBlockTypeMCPToolUse,
			anthropicmessages.ContentBlockTypeMCPToolResult,
			anthropicmessages.ContentBlockTypeCodeExecution,
			anthropicmessages.ContentBlockTypeCodeExecutionResult,
			anthropicmessages.ContentBlockTypeContainerUpload:
			message = nil
			if err := report.Lose(
				policy,
				FeatureAnthropicServerTools,
				path,
				"server-side tool output cannot be reproduced in a Gemini response",
			); err != nil {
				return nil, err
			}

		default:
			return nil, upstreamWireError(
				UpstreamMessages,
				http.StatusOK,
				fmt.Errorf("%s has unknown content block type %q", path, block.Type),
			)
		}
	}
	return items, nil
}

// messagesUsage maps the Anthropic usage. The input total includes the cache
// reads and cache writes, which Anthropic reports beside input_tokens. The
// total is not provided by the API, so it stays unknown for the renderer to
// derive. A reasoning count is known only when the output details are present.
func messagesUsage(usage *anthropicmessages.Usage) CanonicalUsage {
	if usage == nil {
		return CanonicalUsage{}
	}
	cacheRead := int64(usage.CacheReadInputTokens)
	cacheWrite := int64(usage.CacheCreationInputTokens)
	out := CanonicalUsage{
		InputTokens:      int64(usage.InputTokens) + cacheRead + cacheWrite,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
		OutputTokens:     int64(usage.OutputTokens),
		InputKnown:       true,
		CacheReadKnown:   true,
		CacheWriteKnown:  true,
		OutputKnown:      true,
	}
	if usage.OutputTokensDetails != nil {
		out.ReasoningTokens = int64(usage.OutputTokensDetails.ThinkingTokens)
		out.ReasoningKnown = true
	}
	return out
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
