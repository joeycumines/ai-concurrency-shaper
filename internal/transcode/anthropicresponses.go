// Anthropic Messages <-> Responses conversions (non-streaming): request
// translation (system, content blocks, tools, tool choice), response
// translation (content blocks, stop reason, usage), and their helpers.

package transcode

import (
	"encoding/json"
	"strings"
)

// =============================================================================
// Anthropic request -> Responses request
// =============================================================================

// ConvertAnthropicRequestResponsesRequest converts an Anthropic messages
// request to a responses API request. The system prompt becomes a leading
// system message item; thinking blocks become reasoning items; tool use and
// tool result blocks become function call and function call output items.
func ConvertAnthropicRequestResponsesRequest(req *AnthropicMessageRequest) *ResponsesRequest {
	if req == nil {
		return &ResponsesRequest{}
	}
	out := &ResponsesRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		Metadata:    req.Metadata,
	}
	// Anthropic's disable_parallel_tool_use maps to the responses
	// parallel_tool_calls control.
	if req.ToolChoice != nil && req.ToolChoice.DisableParallelToolUse != nil {
		out.ParallelToolCalls = new(!*req.ToolChoice.DisableParallelToolUse)
	}
	if req.MaxTokens > 0 {
		out.MaxOutputTokens = new(req.MaxTokens)
	}
	if req.System != nil {
		if req.System.ContentStr != nil {
			out.Input = append(out.Input, ResponsesMessage{
				ID:      new(newItemID("msg_")),
				Type:    new(ResponsesMessageTypeMessage),
				Role:    new(ResponsesMessageRoleSystem),
				Content: &ResponsesMessageContent{ContentStr: req.System.ContentStr},
			})
		} else if len(req.System.ContentBlocks) > 0 {
			if blocks := anthropicSystemResponsesContentBlocks(req.System.ContentBlocks); len(blocks) > 0 {
				out.Input = append(out.Input, ResponsesMessage{
					ID:      new(newItemID("msg_")),
					Type:    new(ResponsesMessageTypeMessage),
					Role:    new(ResponsesMessageRoleSystem),
					Content: &ResponsesMessageContent{ContentBlocks: blocks},
				})
			}
		}
	}
	out.Input = append(out.Input, anthropicMessagesResponsesMessages(req.Messages)...)
	for i := range req.Tools {
		if tool := anthropicToolResponsesTool(&req.Tools[i]); tool != nil {
			out.Tools = append(out.Tools, *tool)
		}
	}
	out.ToolChoice = anthropicToolChoiceResponsesToolChoice(req.ToolChoice)
	return out
}

// anthropicSystemResponsesContentBlocks maps system content blocks to
// responses content blocks (text blocks only).
func anthropicSystemResponsesContentBlocks(blocks []AnthropicContentBlock) []ResponsesMessageContentBlock {
	var out []ResponsesMessageContentBlock
	for i := range blocks {
		if blocks[i].Type == AnthropicContentBlockTypeText && blocks[i].Text != nil {
			out = append(out, ResponsesMessageContentBlock{Type: ResponsesMessageContentBlockTypeInputText, Text: blocks[i].Text})
		}
	}
	return out
}

// anthropicMessagesResponsesMessages converts anthropic messages into
// responses input items. Thinking blocks are buffered so the reasoning item
// precedes the text or tool items of the same message.
func anthropicMessagesResponsesMessages(messages []AnthropicMessage) []ResponsesMessage {
	var items []ResponsesMessage
	var pendingReasoning *ResponsesMessage
	for i := range messages {
		msg := &messages[i]
		var blocks []AnthropicContentBlock
		if msg.Content.ContentStr != nil {
			blocks = []AnthropicContentBlock{{Type: AnthropicContentBlockTypeText, Text: msg.Content.ContentStr}}
		} else {
			blocks = msg.Content.ContentBlocks
		}
		role := anthropicRoleResponsesRole(msg.Role)
		for j := range blocks {
			block := &blocks[j]
			switch block.Type {
			case AnthropicContentBlockTypeThinking, AnthropicContentBlockTypeRedactedThinking:
				if pendingReasoning == nil {
					pendingReasoning = &ResponsesMessage{
						ID:                 new(newItemID("rs_")),
						Type:               new(ResponsesMessageTypeReasoning),
						Role:               new(ResponsesMessageRoleAssistant),
						Status:             new("completed"),
						ResponsesReasoning: &ResponsesReasoning{},
					}
				}
				if block.Type == AnthropicContentBlockTypeRedactedThinking {
					if block.Data != nil {
						pendingReasoning.EncryptedContent = block.Data
					}
					continue
				}
				if block.Thinking != nil {
					var contentBlocks []ResponsesMessageContentBlock
					if pendingReasoning.Content != nil {
						contentBlocks = pendingReasoning.Content.ContentBlocks
					}
					pendingReasoning.Content = &ResponsesMessageContent{
						ContentBlocks: append(contentBlocks, ResponsesMessageContentBlock{
							Type:      ResponsesMessageContentBlockTypeReasoning,
							Text:      block.Thinking,
							Signature: block.Signature,
						}),
					}
				}

			case AnthropicContentBlockTypeToolUse:
				if pendingReasoning != nil {
					items = append(items, *pendingReasoning)
					pendingReasoning = nil
				}
				item := ResponsesMessage{
					ID:     new(newItemID("fc_")),
					Type:   new(ResponsesMessageTypeFunctionCall),
					Role:   new(ResponsesMessageRoleAssistant),
					Status: new("completed"),
					ResponsesToolMessage: &ResponsesToolMessage{
						CallID: block.ID,
						Name:   block.Name,
						// An empty tool input is still a valid argument
						// payload: represent it as an empty object rather
						// than dropping the arguments field.
						Arguments: new(toolInputArguments(block.Input)),
					},
				}
				items = append(items, item)

			case AnthropicContentBlockTypeToolResult:
				if pendingReasoning != nil {
					items = append(items, *pendingReasoning)
					pendingReasoning = nil
				}
				if block.ToolUseID == nil {
					continue
				}
				item := ResponsesMessage{
					ID:                   new(newItemID("fc_")),
					Type:                 new(ResponsesMessageTypeFunctionCallOutput),
					Status:               new("completed"),
					ResponsesToolMessage: &ResponsesToolMessage{CallID: block.ToolUseID},
				}
				if block.IsError != nil && *block.IsError {
					item.Status = new("incomplete")
				}
				if block.Content != nil {
					if block.Content.ContentStr != nil {
						item.Output = &ResponsesToolMessageOutput{Str: block.Content.ContentStr}
					} else if len(block.Content.ContentBlocks) > 0 {
						item.Output = &ResponsesToolMessageOutput{Blocks: anthropicContentBlocksResponsesContentBlocks(block.Content.ContentBlocks)}
					}
				}
				items = append(items, item)

			default:
				// text, image, document
				if pendingReasoning != nil {
					items = append(items, *pendingReasoning)
					pendingReasoning = nil
				}
				if block := anthropicContentBlockResponsesContentBlock(block, role); block != nil {
					items = append(items, ResponsesMessage{
						ID:      new(newItemID("msg_")),
						Type:    new(ResponsesMessageTypeMessage),
						Role:    new(role),
						Status:  new("completed"),
						Content: &ResponsesMessageContent{ContentBlocks: []ResponsesMessageContentBlock{*block}},
					})
				}
			}
		}
	}
	if pendingReasoning != nil {
		items = append(items, *pendingReasoning)
	}
	return items
}

// anthropicContentBlockResponsesContentBlock maps a single anthropic content
// block to a responses content block. Text becomes input_text or output_text
// by role; image becomes input_image with its URL or base64 data payload.
func anthropicContentBlockResponsesContentBlock(block *AnthropicContentBlock, role ResponsesMessageRoleType) *ResponsesMessageContentBlock {
	switch block.Type {
	case AnthropicContentBlockTypeText:
		if block.Text == nil {
			return nil
		}
		blockType := ResponsesMessageContentBlockTypeInputText
		if role == ResponsesMessageRoleAssistant {
			blockType = ResponsesMessageContentBlockTypeOutputText
		}
		return &ResponsesMessageContentBlock{Type: blockType, Text: block.Text}
	case AnthropicContentBlockTypeImage:
		if block.Source == nil {
			return nil
		}
		out := &ResponsesMessageContentBlock{Type: ResponsesMessageContentBlockTypeInputImage}
		if block.Source.URL != nil && *block.Source.URL != "" {
			out.ImageURL = block.Source.URL
		} else if block.Source.Data != nil {
			imageData := struct {
				Type      string  `json:"type"`
				Data      string  `json:"data"`
				MediaType *string `json:"media_type,omitempty"`
			}{Type: "base64", Data: *block.Source.Data, MediaType: block.Source.MediaType}
			encoded, err := json.Marshal(imageData)
			if err != nil {
				return nil
			}
			out.ImageData = encoded
		}
		if out.ImageURL == nil && len(out.ImageData) == 0 {
			return nil
		}
		return out
	}
	return nil
}

// anthropicContentBlocksResponsesContentBlocks maps tool result content
// blocks to responses content blocks.
func anthropicContentBlocksResponsesContentBlocks(blocks []AnthropicContentBlock) []ResponsesMessageContentBlock {
	var out []ResponsesMessageContentBlock
	for i := range blocks {
		if block := anthropicContentBlockResponsesContentBlock(&blocks[i], ResponsesMessageRoleUser); block != nil {
			out = append(out, *block)
		}
	}
	return out
}

// anthropicToolResponsesTool converts an anthropic tool to a responses
// function tool.
func anthropicToolResponsesTool(tool *AnthropicTool) *ResponsesTool {
	if tool == nil || strings.TrimSpace(tool.Name) == "" {
		return nil
	}
	return &ResponsesTool{
		Type:        ResponsesToolTypeFunction,
		Name:        new(tool.Name),
		Description: tool.Description,
		Parameters:  tool.InputSchema,
	}
}

// anthropicToolChoiceResponsesToolChoice converts an anthropic tool choice to
// a responses tool choice.
func anthropicToolChoiceResponsesToolChoice(choice *AnthropicToolChoice) *ResponsesToolChoice {
	if choice == nil {
		return nil
	}
	switch choice.Type {
	case "auto", "none":
		return &ResponsesToolChoice{Str: new(choice.Type)}
	case "any":
		// Anthropic's "any" forces tool use; the responses API equivalent is
		// "required" — passing "any" through would be rejected upstream.
		return &ResponsesToolChoice{Str: new("required")}
	case "tool":
		if choice.Name == "" {
			return &ResponsesToolChoice{Str: new("auto")}
		}
		return &ResponsesToolChoice{Struct: &ResponsesToolChoiceStruct{Type: "function", Name: new(choice.Name)}}
	}
	return &ResponsesToolChoice{Str: new("auto")}
}

// =============================================================================
// Responses response -> Anthropic response
// =============================================================================

// ConvertResponsesResponseAnthropicResponse converts a responses API response
// to an Anthropic messages response. Message items become text blocks,
// reasoning items become thinking blocks, and function call items become
// tool use blocks. Function call output items are input echoes and are
// dropped.
func ConvertResponsesResponseAnthropicResponse(resp *ResponsesResponse) *AnthropicMessageResponse {
	if resp == nil {
		return &AnthropicMessageResponse{}
	}
	out := &AnthropicMessageResponse{
		ID:    resp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: resp.Model,
	}
	var sawToolUse bool
	for i := range resp.Output {
		item := &resp.Output[i]
		if item.Type == nil {
			continue
		}
		switch *item.Type {
		case ResponsesMessageTypeRefusal:
			// Refusal items become text blocks; the response stop
			// reason is refusal with stop_details passthrough.
			if item.Content != nil {
				if item.Content.ContentStr != nil {
					out.Content = append(out.Content, AnthropicContentBlock{Type: AnthropicContentBlockTypeText, Text: item.Content.ContentStr})
				} else {
					for j := range item.Content.ContentBlocks {
						block := &item.Content.ContentBlocks[j]
						if block.Type == ResponsesMessageContentBlockTypeRefusal && block.Text != nil {
							out.Content = append(out.Content, AnthropicContentBlock{Type: AnthropicContentBlockTypeText, Text: block.Text})
						}
						// output_text blocks inside a refusal item are
						// also mapped to text blocks.
						if block.Type == ResponsesMessageContentBlockTypeOutputText && block.Text != nil {
							out.Content = append(out.Content, AnthropicContentBlock{Type: AnthropicContentBlockTypeText, Text: block.Text})
						}
					}
				}
			}
			if item.Status != nil && *item.Status == "incomplete" {
				out.StopReason = AnthropicStopReasonMaxTokens
			} else {
				out.StopReason = AnthropicStopReasonRefusal
			}

		case ResponsesMessageTypeMessage:
			// A message item may carry several content blocks (multi-part
			// text, refusal); every output_text block becomes a text block.
			if item.Content != nil {
				if item.Content.ContentStr != nil {
					out.Content = append(out.Content, AnthropicContentBlock{Type: AnthropicContentBlockTypeText, Text: item.Content.ContentStr})
				} else {
					for j := range item.Content.ContentBlocks {
						block := &item.Content.ContentBlocks[j]
						if block.Type == ResponsesMessageContentBlockTypeOutputText && block.Text != nil {
							out.Content = append(out.Content, AnthropicContentBlock{Type: AnthropicContentBlockTypeText, Text: block.Text})
						}
					}
				}
			}

		case ResponsesMessageTypeReasoning:
			appended := len(out.Content)
			if item.Content != nil {
				// Every reasoning block becomes a thinking block, preserving
				// signatures (multi-block reasoning must not be truncated to
				// the first block).
				for j := range item.Content.ContentBlocks {
					block := &item.Content.ContentBlocks[j]
					if block.Type != ResponsesMessageContentBlockTypeReasoning {
						continue
					}
					out.Content = append(out.Content, AnthropicContentBlock{
						Type:      AnthropicContentBlockTypeThinking,
						Thinking:  block.Text,
						Signature: block.Signature,
					})
				}
			}
			if len(out.Content) == appended && item.ResponsesReasoning != nil && len(item.Summary) > 0 {
				var b strings.Builder
				for j := range item.Summary {
					b.WriteString(item.Summary[j].Text)
					b.WriteString("\n")
				}
				out.Content = append(out.Content, AnthropicContentBlock{
					Type:     AnthropicContentBlockTypeThinking,
					Thinking: new(strings.TrimRight(b.String(), "\n")),
				})
			}
			if item.ResponsesReasoning != nil && item.EncryptedContent != nil {
				out.Content = append(out.Content, AnthropicContentBlock{
					Type: AnthropicContentBlockTypeRedactedThinking,
					Data: item.EncryptedContent,
				})
			}

		case ResponsesMessageTypeFunctionCall:
			if item.ResponsesToolMessage == nil {
				continue
			}
			block := AnthropicContentBlock{Type: AnthropicContentBlockTypeToolUse}
			if item.CallID != nil {
				block.ID = item.CallID
			} else {
				// Anthropic requires a tool use id; synthesize one rather
				// than emitting a block the API would reject.
				block.ID = new(newItemID("toolu_"))
			}
			if item.Name == nil || *item.Name == "" {
				// A tool use without a name cannot be represented.
				continue
			}
			sawToolUse = true
			block.Name = item.Name
			if item.Arguments != nil && strings.TrimSpace(*item.Arguments) != "" {
				block.Input = json.RawMessage(*item.Arguments)
			} else {
				block.Input = json.RawMessage(`{}`)
			}
			out.Content = append(out.Content, block)
		}
	}
	switch {
	case resp.Status != nil && *resp.Status == "incomplete":
		out.StopReason = AnthropicStopReasonMaxTokens
	case sawToolUse:
		out.StopReason = AnthropicStopReasonToolUse
	default:
		out.StopReason = AnthropicStopReasonEndTurn
	}
	if resp.Usage != nil {
		out.Usage = responsesUsageAnthropicUsage(resp.Usage)
	}
	if resp.StopDetails != nil {
		out.StopDetails = &AnthropicStopDetails{
			Type:             resp.StopDetails.Type,
			Category:         resp.StopDetails.Category,
			Explanation:      resp.StopDetails.Explanation,
			RecommendedModel: resp.StopDetails.RecommendedModel,
		}
	}
	return out
}

// responsesUsageAnthropicUsage maps responses usage to anthropic usage.
// Cached input tokens move into the cache read field and are removed from
// the input token count, matching the anthropic accounting.
func responsesUsageAnthropicUsage(usage *ResponsesResponseUsage) *AnthropicUsage {
	out := &AnthropicUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
	}
	if usage.InputTokensDetails != nil && usage.InputTokensDetails.CachedTokens > 0 {
		out.CacheReadInputTokens = usage.InputTokensDetails.CachedTokens
		if out.InputTokens >= out.CacheReadInputTokens {
			out.InputTokens -= out.CacheReadInputTokens
		}
	}
	if usage.OutputTokensDetails != nil && usage.OutputTokensDetails.ReasoningTokens > 0 {
		out.OutputTokensDetails = &AnthropicOutputTokensDetails{ThinkingTokens: usage.OutputTokensDetails.ReasoningTokens}
	}
	return out
}
