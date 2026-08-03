// Chat Completions <-> Responses conversions (non-streaming): request and
// response translation, message/item aggregation, content block mapping,
// tool and tool choice conversion, and usage mapping. Semantics mirror the
// Bifrost reference implementation.

package transcode

import (
	"encoding/json"
	"strings"
)

// =============================================================================
// Responses request <-> Chat request
// =============================================================================

// ConvertResponsesRequestChatRequest converts a responses API request to a
// chat completions request. Instructions become a leading system message;
// reasoning items are attached to the next assistant message; function call
// items become assistant tool calls; function call outputs become tool
// messages.
func ConvertResponsesRequestChatRequest(req *ResponsesRequest) *ChatRequest {
	if req == nil {
		return &ChatRequest{}
	}
	out := &ChatRequest{
		Model:               req.Model,
		MaxCompletionTokens: req.MaxOutputTokens,
		Temperature:         req.Temperature,
		TopP:                req.TopP,
		Stream:              req.Stream,
		ParallelToolCalls:   req.ParallelToolCalls,
		Metadata:            req.Metadata,
		User:                req.User,
		Store:               req.Store,
	}
	if req.StreamOptions != nil {
		out.StreamOptions = &ChatStreamOptions{IncludeUsage: req.StreamOptions.IncludeUsage}
	}
	if req.Instructions != nil {
		out.Messages = append(out.Messages, ChatMessage{
			Role:    ChatMessageRoleSystem,
			Content: &ChatMessageContent{ContentStr: req.Instructions},
		})
	}
	out.Messages = append(out.Messages, responsesMessagesChatMessages(req.Input)...)
	if req.Reasoning != nil {
		out.Reasoning = &ChatReasoning{Effort: req.Reasoning.Effort}
	}
	for i := range req.Tools {
		if tool := responsesToolChatTool(&req.Tools[i]); tool != nil {
			out.Tools = append(out.Tools, *tool)
		}
	}
	out.ToolChoice = responsesToolChoiceChatToolChoice(req.ToolChoice)
	// A function-named tool choice is only valid when the named tool exists;
	// otherwise the choice is dropped, mirroring the reference fallback
	// sanitization.
	if out.ToolChoice != nil && out.ToolChoice.Struct != nil {
		found := false
		for i := range out.Tools {
			if out.Tools[i].Function.Name == out.ToolChoice.Struct.Function.Name {
				found = true
				break
			}
		}
		if !found {
			out.ToolChoice = nil
		}
	}
	return out
}

// responsesMessagesChatMessages converts input/output items into chat
// messages. Reasoning is buffered and attached to the next assistant message;
// consecutive function call items merge into one assistant message carrying
// tool calls.
func responsesMessagesChatMessages(items []ResponsesMessage) []ChatMessage {
	var messages []ChatMessage
	var pendingReasoning *string
	var pendingDetails []ChatReasoningDetails
	var toolCalls []ChatAssistantMessageToolCall

	flushToolCalls := func() {
		if len(toolCalls) == 0 {
			return
		}
		msg := ChatMessage{
			Role:                 ChatMessageRoleAssistant,
			ChatAssistantMessage: &ChatAssistantMessage{ToolCalls: toolCalls},
		}
		msg.Reasoning = trimmedReasoning(pendingReasoning)
		msg.ReasoningDetails = pendingDetails
		pendingReasoning, pendingDetails = nil, nil
		messages = append(messages, msg)
		toolCalls = nil
	}

	for i := range items {
		item := &items[i]
		// Input items may omit the type field; the API infers message items
		// from the role.
		itemType := ResponsesMessageTypeMessage
		if item.Type != nil {
			itemType = *item.Type
		}
		switch itemType {
		case ResponsesMessageTypeReasoning:
			flushToolCalls()
			if item.ResponsesReasoning != nil && item.Summary != nil {
				for j := range item.Summary {
					text := strings.TrimSpace(item.Summary[j].Text)
					if text == "" {
						continue
					}
					if pendingReasoning == nil {
						pendingReasoning = new("")
					}
					*pendingReasoning += item.Summary[j].Text + "\n"
					pendingDetails = append(pendingDetails, ChatReasoningDetails{
						Index:   len(pendingDetails),
						Type:    ChatReasoningDetailsTypeSummary,
						Summary: new(text),
					})
				}
			}
			if item.ResponsesReasoning != nil && item.EncryptedContent != nil {
				pendingDetails = append(pendingDetails, ChatReasoningDetails{
					Index: len(pendingDetails),
					Type:  ChatReasoningDetailsTypeEncrypted,
					Data:  item.EncryptedContent,
				})
			}
			if item.Content != nil {
				for j := range item.Content.ContentBlocks {
					block := &item.Content.ContentBlocks[j]
					if block.Type != ResponsesMessageContentBlockTypeReasoning || block.Text == nil {
						continue
					}
					if pendingReasoning == nil {
						pendingReasoning = new("")
					}
					*pendingReasoning += *block.Text + "\n"
					pendingDetails = append(pendingDetails, ChatReasoningDetails{
						Index:     len(pendingDetails),
						Type:      ChatReasoningDetailsTypeText,
						Text:      block.Text,
						Signature: block.Signature,
					})
				}
			}

		case ResponsesMessageTypeFunctionCall:
			if item.ResponsesToolMessage == nil || (item.CallID == nil && item.Name == nil) {
				continue
			}
			toolCalls = append(toolCalls, ChatAssistantMessageToolCall{
				Type: new("function"),
				ID:   item.CallID,
				Function: ChatAssistantMessageToolCallFunction{
					Name:      item.Name,
					Arguments: derefStr(item.Arguments),
				},
			})

		case ResponsesMessageTypeFunctionCallOutput:
			flushToolCalls()
			if item.ResponsesToolMessage == nil || item.CallID == nil {
				continue
			}
			var content *ChatMessageContent
			if item.Output != nil {
				if item.Output.Str != nil {
					content = &ChatMessageContent{ContentStr: item.Output.Str}
				} else if item.Output.Blocks != nil {
					content = &ChatMessageContent{ContentBlocks: responsesContentBlocksChatContentBlocks(item.Output.Blocks)}
				}
			}
			messages = append(messages, ChatMessage{
				Role:            ChatMessageRoleTool,
				Content:         content,
				ChatToolMessage: &ChatToolMessage{ToolCallID: item.CallID},
			})

		case ResponsesMessageTypeRefusal:
			flushToolCalls()
			msg := ChatMessage{Role: ChatMessageRoleAssistant, ChatAssistantMessage: &ChatAssistantMessage{}}
			if item.Content != nil {
				if item.Content.ContentStr != nil {
					msg.Refusal = item.Content.ContentStr
				} else if len(item.Content.ContentBlocks) == 1 && item.Content.ContentBlocks[0].Type == ResponsesMessageContentBlockTypeRefusal {
					msg.Refusal = item.Content.ContentBlocks[0].Text
				}
			}
			msg.Reasoning = trimmedReasoning(pendingReasoning)
			msg.ReasoningDetails = pendingDetails
			pendingReasoning, pendingDetails = nil, nil
			messages = append(messages, msg)

		case ResponsesMessageTypeMessage:
			flushToolCalls()
			role := responsesRoleChatRole(item.Role)
			msg := ChatMessage{Role: role}
			if role == ChatMessageRoleAssistant {
				msg.ChatAssistantMessage = &ChatAssistantMessage{}
				msg.Reasoning = trimmedReasoning(pendingReasoning)
				msg.ReasoningDetails = pendingDetails
				pendingReasoning, pendingDetails = nil, nil
			}
			if item.Content != nil {
				if item.Content.ContentStr != nil {
					msg.Content = &ChatMessageContent{ContentStr: item.Content.ContentStr}
				} else if item.Content.ContentBlocks != nil {
					blocks := responsesContentBlocksChatContentBlocks(item.Content.ContentBlocks)
					// A single text block collapses to a plain content string,
					// matching the reference behavior.
					if len(blocks) == 1 && blocks[0].Type == ChatContentBlockTypeText {
						msg.Content = &ChatMessageContent{ContentStr: blocks[0].Text}
					} else if len(blocks) > 0 {
						msg.Content = &ChatMessageContent{ContentBlocks: blocks}
					}
				}
			}
			messages = append(messages, msg)

		default:
			flushToolCalls()
		}
	}
	flushToolCalls()
	return messages
}

// responsesContentBlocksChatContentBlocks maps responses content blocks to
// chat content blocks. input_text/output_text/reasoning_text become text;
// input_image becomes image_url (URL form only); refusal is passed through;
// unmodeled block types are dropped.
func responsesContentBlocksChatContentBlocks(blocks []ResponsesMessageContentBlock) []ChatContentBlock {
	var out []ChatContentBlock
	for i := range blocks {
		block := &blocks[i]
		switch block.Type {
		case ResponsesMessageContentBlockTypeInputText, ResponsesMessageContentBlockTypeOutputText, ResponsesMessageContentBlockTypeReasoning:
			if block.Text != nil {
				out = append(out, ChatContentBlock{Type: ChatContentBlockTypeText, Text: block.Text})
			}
		case ResponsesMessageContentBlockTypeInputImage:
			if block.ImageURL != nil && *block.ImageURL != "" {
				out = append(out, ChatContentBlock{Type: ChatContentBlockTypeImage, ImageURL: &ChatInputImage{URL: *block.ImageURL}})
			} else if len(block.ImageData) > 0 {
				// Base64 image payloads (e.g. from an anthropic image block)
				// map to the chat image_url data URI form.
				if url := chatImageDataURI(block.ImageData); url != "" {
					out = append(out, ChatContentBlock{Type: ChatContentBlockTypeImage, ImageURL: &ChatInputImage{URL: url}})
				}
			}
		case ResponsesMessageContentBlockTypeRefusal:
			out = append(out, ChatContentBlock{Type: ChatContentBlockTypeRefusal, Refusal: block.Text})
		}
	}
	return out
}

// chatMessagesResponsesMessages converts chat messages into responses input
// items. Assistant reasoning becomes reasoning items; assistant tool calls
// become function call items; tool messages become function call output items.
func chatMessagesResponsesMessages(messages []ChatMessage) []ResponsesMessage {
	var items []ResponsesMessage
	for i := range messages {
		msg := &messages[i]
		switch msg.Role {
		case ChatMessageRoleAssistant:
			assistant := msg.ChatAssistantMessage
			if assistant == nil {
				assistant = &ChatAssistantMessage{}
			}
			if assistant.Refusal != nil {
				items = append(items, ResponsesMessage{
					ID:   new(newItemID("rs_")),
					Type: new(ResponsesMessageTypeRefusal),
					Role: new(ResponsesMessageRoleAssistant),
					Content: &ResponsesMessageContent{
						ContentBlocks: []ResponsesMessageContentBlock{{Type: ResponsesMessageContentBlockTypeRefusal, Text: assistant.Refusal}},
					},
				})
			}
			if assistant.Reasoning != nil || len(assistant.ReasoningDetails) > 0 {
				item := ResponsesMessage{
					ID:                 new(newItemID("rs_")),
					Type:               new(ResponsesMessageTypeReasoning),
					Role:               new(ResponsesMessageRoleAssistant),
					ResponsesReasoning: &ResponsesReasoning{},
				}
				var summary []ResponsesReasoningSummary
				var blocks []ResponsesMessageContentBlock
				for j := range assistant.ReasoningDetails {
					d := &assistant.ReasoningDetails[j]
					switch d.Type {
					case ChatReasoningDetailsTypeSummary:
						if d.Summary != nil {
							summary = append(summary, ResponsesReasoningSummary{Type: "summary_text", Text: *d.Summary})
						}
					case ChatReasoningDetailsTypeEncrypted:
						item.EncryptedContent = d.Data
					default:
						if d.Text != nil {
							// Preserve the signature so signed reasoning
							// round-trips into thinking blocks.
							blocks = append(blocks, ResponsesMessageContentBlock{Type: ResponsesMessageContentBlockTypeReasoning, Text: d.Text, Signature: d.Signature})
						}
					}
				}
				// A plain reasoning string is a single reasoning block, used
				// only when no structured details are present.
				if len(assistant.ReasoningDetails) == 0 && assistant.Reasoning != nil {
					blocks = append(blocks, ResponsesMessageContentBlock{Type: ResponsesMessageContentBlockTypeReasoning, Text: assistant.Reasoning})
				}
				if len(summary) > 0 {
					item.Summary = summary
				}
				if len(blocks) > 0 {
					item.Content = &ResponsesMessageContent{ContentBlocks: blocks}
				}
				if len(summary) > 0 || len(blocks) > 0 || item.EncryptedContent != nil {
					items = append(items, item)
				}
			}
			for j := range assistant.ToolCalls {
				call := &assistant.ToolCalls[j]
				item := ResponsesMessage{
					ID:     new(newItemID("fc_")),
					Type:   new(ResponsesMessageTypeFunctionCall),
					Role:   new(ResponsesMessageRoleAssistant),
					Status: new("completed"),
					ResponsesToolMessage: &ResponsesToolMessage{
						CallID:    call.ID,
						Name:      nonEmptyPtr(derefStr(call.Function.Name)),
						Arguments: nonEmptyPtr(call.Function.Arguments),
					},
				}
				if item.ResponsesToolMessage.CallID == nil && item.ResponsesToolMessage.Name == nil {
					continue
				}
				items = append(items, item)
			}
			if msg.Content != nil {
				assistantText := derefStr(msg.Content.ContentStr)
				if assistantText != "" || len(msg.Content.ContentBlocks) > 0 {
					var blocks []ResponsesMessageContentBlock
					if msg.Content.ContentStr != nil {
						blocks = []ResponsesMessageContentBlock{{Type: ResponsesMessageContentBlockTypeOutputText, Text: msg.Content.ContentStr}}
					} else {
						blocks = chatContentResponsesContentBlocks(msg.Content, true)
					}
					items = append(items, ResponsesMessage{
						ID:     new(newItemID("msg_")),
						Type:   new(ResponsesMessageTypeMessage),
						Role:   new(ResponsesMessageRoleAssistant),
						Status: new("completed"),
						Content: &ResponsesMessageContent{
							ContentBlocks: blocks,
						},
					})
				}
			}

		case ChatMessageRoleTool:
			if msg.ChatToolMessage == nil || msg.ToolCallID == nil {
				continue
			}
			item := ResponsesMessage{
				ID:                   new(newItemID("fc_")),
				Type:                 new(ResponsesMessageTypeFunctionCallOutput),
				Status:               new("completed"),
				ResponsesToolMessage: &ResponsesToolMessage{CallID: msg.ToolCallID},
			}
			if msg.Content != nil {
				if msg.Content.ContentStr != nil {
					item.Output = &ResponsesToolMessageOutput{Str: msg.Content.ContentStr}
				} else if len(msg.Content.ContentBlocks) > 0 {
					// Multi-block tool results (text, images) are preserved as
					// output content blocks rather than dropped.
					item.Output = &ResponsesToolMessageOutput{Blocks: chatContentResponsesContentBlocks(msg.Content, true)}
				}
			}
			items = append(items, item)

		default:
			// system, developer, user
			item := ResponsesMessage{
				ID:   new(newItemID("msg_")),
				Type: new(ResponsesMessageTypeMessage),
				Role: chatRoleResponsesRole(msg.Role),
			}
			if msg.Content != nil {
				if msg.Content.ContentStr != nil {
					item.Content = &ResponsesMessageContent{ContentStr: msg.Content.ContentStr}
				} else if len(msg.Content.ContentBlocks) > 0 {
					item.Content = &ResponsesMessageContent{ContentBlocks: chatContentResponsesContentBlocks(msg.Content, false)}
				}
			}
			items = append(items, item)
		}
	}
	return items
}

// chatContentResponsesContentBlocks maps chat content blocks to responses
// content blocks. Text becomes output_text for assistant output and
// input_text otherwise; image_url becomes input_image.
func chatContentResponsesContentBlocks(content *ChatMessageContent, assistant bool) []ResponsesMessageContentBlock {
	if content == nil || content.ContentStr != nil {
		return nil
	}
	textType := ResponsesMessageContentBlockTypeInputText
	if assistant {
		textType = ResponsesMessageContentBlockTypeOutputText
	}
	var out []ResponsesMessageContentBlock
	for i := range content.ContentBlocks {
		block := &content.ContentBlocks[i]
		switch block.Type {
		case ChatContentBlockTypeText:
			if block.Text != nil {
				out = append(out, ResponsesMessageContentBlock{Type: textType, Text: block.Text})
			}
		case ChatContentBlockTypeImage:
			if block.ImageURL != nil && block.ImageURL.URL != "" {
				out = append(out, ResponsesMessageContentBlock{Type: ResponsesMessageContentBlockTypeInputImage, ImageURL: new(block.ImageURL.URL)})
			}
		case ChatContentBlockTypeRefusal:
			out = append(out, ResponsesMessageContentBlock{Type: ResponsesMessageContentBlockTypeRefusal, Text: block.Refusal})
		}
	}
	return out
}

// chatImageDataURI builds a chat image_url data URI from a responses
// input_image image_data payload: {"type":"base64","data":"...","media_type":
// "image/png"} becomes data:image/png;base64,... . Empty payloads yield "".
func chatImageDataURI(raw json.RawMessage) string {
	var payload struct {
		Data      string `json:"data"`
		MediaType string `json:"media_type"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Data == "" {
		return ""
	}
	return "data:" + payload.MediaType + ";base64," + payload.Data
}

// responsesToolChatTool converts a responses tool to a chat function tool.
// Only function tools with a non-empty name survive, mirroring the reference
// sanitization.
func responsesToolChatTool(tool *ResponsesTool) *ChatTool {
	if tool == nil || tool.Type != ResponsesToolTypeFunction || tool.Name == nil || strings.TrimSpace(*tool.Name) == "" {
		return nil
	}
	return &ChatTool{
		Type: ChatToolTypeFunction,
		Function: &ChatToolFunction{
			Name:        *tool.Name,
			Description: tool.Description,
			Parameters:  tool.Parameters,
			Strict:      tool.Strict,
		},
	}
}

// responsesToolChoiceChatToolChoice converts a responses tool choice to a
// chat tool choice. String forms pass through; a function-named struct form
// becomes a chat function choice; anything else is dropped.
func responsesToolChoiceChatToolChoice(choice *ResponsesToolChoice) *ChatToolChoice {
	if choice == nil {
		return nil
	}
	if choice.Str != nil {
		return &ChatToolChoice{Str: choice.Str}
	}
	if choice.Struct != nil && choice.Struct.Type == "function" && choice.Struct.Name != nil && *choice.Struct.Name != "" {
		return &ChatToolChoice{Struct: &ChatToolChoiceStruct{
			Type:     "function",
			Function: &ChatToolChoiceFunction{Name: *choice.Struct.Name},
		}}
	}
	return nil
}

// =============================================================================
// Responses response <-> Chat response
// =============================================================================

// ConvertChatResponseResponsesResponse converts a chat completions response
// to a responses API response. Each choice's message contributes output
// items; the finish reason of the first choice drives the status.
func ConvertChatResponseResponsesResponse(resp *ChatResponse) *ResponsesResponse {
	if resp == nil {
		return &ResponsesResponse{}
	}
	out := &ResponsesResponse{
		ID:        resp.ID,
		Object:    "response",
		CreatedAt: resp.Created,
		Model:     resp.Model,
	}
	var sawCompleted, sawIncomplete bool
	for i := range resp.Choices {
		choice := &resp.Choices[i]
		if choice.Message == nil {
			continue
		}
		out.Output = append(out.Output, chatMessagesResponsesMessages([]ChatMessage{*choice.Message})...)
		if choice.FinishReason != nil {
			switch *choice.FinishReason {
			case "stop", "tool_calls":
				sawCompleted = true
			case "length":
				sawIncomplete = true
			}
		}
	}
	// The incomplete status wins over completed; unmapped finish reasons are
	// ignored, mirroring the reference behavior.
	switch {
	case sawIncomplete:
		out.Status = new("incomplete")
		out.IncompleteDetails = json.RawMessage(`{"reason":"max_output_tokens"}`)
	case sawCompleted:
		out.Status = new("completed")
	}
	if resp.Usage != nil {
		out.Usage = chatLLMUsageResponsesUsage(resp.Usage)
	}
	return out
}

// chatLLMUsageResponsesUsage maps chat usage to responses usage.
func chatLLMUsageResponsesUsage(usage *ChatLLMUsage) *ResponsesResponseUsage {
	out := &ResponsesResponseUsage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.TotalTokens,
	}
	if usage.PromptTokensDetails != nil {
		out.InputTokensDetails = &ResponsesResponseInputTokens{CachedTokens: usage.PromptTokensDetails.CachedTokens}
	}
	if usage.CompletionTokensDetails != nil {
		out.OutputTokensDetails = &ResponsesResponseOutputTokens{ReasoningTokens: usage.CompletionTokensDetails.ReasoningTokens}
	}
	return out
}
