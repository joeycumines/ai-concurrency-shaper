// Chat Completions stream -> Responses stream conversion. The stateful
// accumulator translates each chat chunk into zero or more responses events,
// tracking item lifecycles (reasoning, text, tool calls), output indices, and
// sequence numbers.

package transcode

import (
	"encoding/json"
	"strings"
	"time"
)

// =============================================================================
// Chat stream -> Responses stream
// =============================================================================

// ChatResponsesStreamState accumulates a chat completions SSE stream and
// converts each chunk into zero or more responses SSE events. The zero value
// is ready to use; the state must not be shared across streams.
type ChatResponsesStreamState struct {
	messageID  string
	model      string
	createdAt  int64
	sequence   int
	nextOutput int
	started    bool
	finished   bool

	reasoningOpen    bool
	reasoningClosed  bool
	reasoningID      string
	reasoningOutput  int
	reasoning        strings.Builder
	reasoningDetails []ChatReasoningDetails

	textOpen   bool
	textID     string
	textOutput int
	text       strings.Builder

	refusalOpen   bool
	refusalID     string
	refusalOutput int
	refusal       strings.Builder

	toolCalls     []*chatStreamToolCall
	items         []ResponsesMessage
	pendingUsage  *ResponsesResponseUsage
	terminalEvent []ResponsesStreamResponse
}

// chatStreamToolCall tracks one in-flight tool call of the chat stream. The
// chat index anchors deltas that carry only an index; the item id is the
// responses item id, distinct from the chat call id.
type chatStreamToolCall struct {
	index     int
	callID    string
	id        string
	name      string
	output    int
	arguments strings.Builder
}

// ConvertChatResponseResponsesStreamResponse converts one chat stream chunk
// into responses SSE events. Chunks after the terminal finish chunk are
// ignored.
func (s *ChatResponsesStreamState) ConvertChatResponseResponsesStreamResponse(chunk *ChatStreamResponse) []ResponsesStreamResponse {
	if chunk == nil {
		return nil
	}
	// A trailing usage-only chunk (empty choices) carries the final usage
	// and releases a held-back terminal event.
	if len(chunk.Choices) == 0 {
		if chunk.Usage != nil {
			s.pendingUsage = chatLLMUsageResponsesUsage(chunk.Usage)
			return s.emitTerminal()
		}
		return nil
	}
	if s.finished {
		return nil
	}
	if s.messageID == "" && chunk.ID != "" {
		s.messageID = chunk.ID
	}
	if s.model == "" && chunk.Model != "" {
		s.model = chunk.Model
	}
	if s.createdAt == 0 {
		// Prefer the upstream chunk timestamp; synthesize only when absent.
		if chunk.Created > 0 {
			s.createdAt = chunk.Created
		} else {
			s.createdAt = time.Now().Unix()
		}
	}
	if len(chunk.Choices) == 0 || chunk.Choices[0].Delta == nil {
		return nil
	}
	delta := chunk.Choices[0].Delta
	var events []ResponsesStreamResponse

	if !s.started {
		// The response envelope is emitted on the first meaningful chunk, not
		// only on a role-carrying delta: providers may omit the role or send
		// content first, and downstream converters initialize their message
		// state from the created event.
		s.started = true
		events = append(events,
			s.responseEvent(ResponsesStreamResponseTypeCreated, ResponsesStreamResponse{Response: s.envelope("in_progress")}),
			s.responseEvent(ResponsesStreamResponseTypeInProgress, ResponsesStreamResponse{Response: s.envelope("in_progress")}),
		)
	}

	hasReasoning := delta.Reasoning != nil && *delta.Reasoning != ""
	hasReasoningDetails := len(delta.ReasoningDetails) > 0
	if (hasReasoning || hasReasoningDetails) && !s.reasoningClosed && !s.textOpen {
		if !s.reasoningOpen {
			s.reasoningID = newItemID("rs_")
			s.reasoningOutput = s.nextOutput
			item := ResponsesMessage{
				ID:      new(s.reasoningID),
				Type:    new(ResponsesMessageTypeReasoning),
				Role:    new(ResponsesMessageRoleAssistant),
				Content: &ResponsesMessageContent{ContentBlocks: []ResponsesMessageContentBlock{}},
			}
			events = append(events,
				s.responseEvent(ResponsesStreamResponseTypeOutputItemAdded, ResponsesStreamResponse{
					OutputIndex: new(s.reasoningOutput),
					Item:        &item,
				}),
				s.responseEvent(ResponsesStreamResponseTypeReasoningSummaryPartAdded, ResponsesStreamResponse{
					ItemID:       new(s.reasoningID),
					OutputIndex:  new(s.reasoningOutput),
					SummaryIndex: new(0),
				}),
			)
			s.nextOutput++
			s.reasoningOpen = true
		}
		if hasReasoning {
			events = append(events, s.responseEvent(ResponsesStreamResponseTypeReasoningSummaryTextDelta, ResponsesStreamResponse{
				ItemID:       new(s.reasoningID),
				OutputIndex:  new(s.reasoningOutput),
				SummaryIndex: new(0),
				Delta:        delta.Reasoning,
			}))
			s.reasoning.WriteString(*delta.Reasoning)
		}
		if hasReasoningDetails {
			s.reasoningDetails = append(s.reasoningDetails, delta.ReasoningDetails...)
		}
	}

	// An image delta branch is structurally impossible: ChatStreamDelta.Content
	// is *string, so image data cannot appear as a content delta.
	if delta.Content != nil && *delta.Content != "" {
		if s.reasoningOpen {
			events = append(events, s.closeReasoning("completed")...)
		}
		if s.refusalOpen {
			events = append(events, s.closeRefusal("completed")...)
		}
		if !s.textOpen {
			s.textID = newItemID("msg_")
			s.textOutput = s.nextOutput
			item := ResponsesMessage{
				ID:      new(s.textID),
				Type:    new(ResponsesMessageTypeMessage),
				Role:    new(ResponsesMessageRoleAssistant),
				Status:  new("in_progress"),
				Content: &ResponsesMessageContent{ContentBlocks: []ResponsesMessageContentBlock{}},
			}
			events = append(events,
				s.responseEvent(ResponsesStreamResponseTypeOutputItemAdded, ResponsesStreamResponse{
					OutputIndex: new(s.textOutput),
					Item:        &item,
				}),
				s.responseEvent(ResponsesStreamResponseTypeContentPartAdded, ResponsesStreamResponse{
					ItemID:       new(s.textID),
					OutputIndex:  new(s.textOutput),
					ContentIndex: new(0),
					Part:         &ResponsesMessageContentBlock{Type: ResponsesMessageContentBlockTypeOutputText},
				}),
			)
			s.nextOutput++
			s.textOpen = true
		}
		events = append(events, s.responseEvent(ResponsesStreamResponseTypeOutputTextDelta, ResponsesStreamResponse{
			ItemID:       new(s.textID),
			OutputIndex:  new(s.textOutput),
			ContentIndex: new(0),
			Delta:        delta.Content,
		}))
		s.text.WriteString(*delta.Content)
	}

	// A single chat delta may carry several tool calls (each with its own
	// index); every one must be streamed, not just the first. Calls are
	// anchored by their chat call id or, failing that, their stream index —
	// never by the slice position, so out-of-order arrivals cannot corrupt
	// the mapping.
	for i := range delta.ToolCalls {
		call := &delta.ToolCalls[i]
		callID := derefStr(call.ID)
		if callID == "" && call.Index == nil {
			// No way to identify or anchor this call: skip it without
			// aborting the rest of the chunk.
			continue
		}
		var tracked *chatStreamToolCall
		if callID != "" {
			tracked = s.lookupToolCallID(callID)
		}
		if tracked == nil && call.Index != nil {
			tracked = s.lookupToolCallIndex(*call.Index)
		}
		if tracked == nil {
			if s.reasoningOpen {
				events = append(events, s.closeReasoning("completed")...)
			}
			if s.textOpen {
				events = append(events, s.closeText("completed")...)
			}
			if s.refusalOpen {
				events = append(events, s.closeRefusal("completed")...)
			}
			tracked = &chatStreamToolCall{id: newItemID("fc_"), output: s.nextOutput}
			if call.Index != nil {
				tracked.index = *call.Index
			}
			if callID != "" {
				tracked.callID = callID
			}
			if call.Function.Name != nil && *call.Function.Name != "" {
				tracked.name = *call.Function.Name
			}
			s.toolCalls = append(s.toolCalls, tracked)
			item := ResponsesMessage{
				ID:     new(tracked.id),
				Type:   new(ResponsesMessageTypeFunctionCall),
				Status: new("in_progress"),
				ResponsesToolMessage: &ResponsesToolMessage{
					CallID:    nonEmptyPtr(tracked.callID),
					Name:      nonEmptyPtr(tracked.name),
					Arguments: new(""),
				},
			}
			events = append(events, s.responseEvent(ResponsesStreamResponseTypeOutputItemAdded, ResponsesStreamResponse{
				OutputIndex: new(tracked.output),
				Item:        &item,
			}))
			s.nextOutput++
		} else {
			// A later delta of a known call: pick up the call id and name if
			// they arrive after the call was first seen.
			if callID != "" && tracked.callID == "" {
				tracked.callID = callID
			}
			if call.Function.Name != nil && *call.Function.Name != "" {
				tracked.name = *call.Function.Name
			}
		}
		if call.Function.Arguments != "" {
			events = append(events, s.responseEvent(ResponsesStreamResponseTypeFunctionCallArgumentsDelta, ResponsesStreamResponse{
				ItemID:      new(tracked.id),
				OutputIndex: new(tracked.output),
				Arguments:   new(call.Function.Arguments),
			}))
			tracked.arguments.WriteString(call.Function.Arguments)
		}
	}

	// Refusal text streams as a message item with a refusal content part;
	// the item lifecycle (output_item.added + content_part.added) must
	// precede the refusal.delta events, mirroring the text path.
	// Only Choices[0] is streamed because the Responses API has no
	// multi-choice concept.
	if delta.Refusal != nil && *delta.Refusal != "" {
		if s.reasoningOpen {
			events = append(events, s.closeReasoning("completed")...)
		}
		if s.textOpen {
			events = append(events, s.closeText("completed")...)
		}
		if !s.refusalOpen {
			s.refusalID = newItemID("msg_")
			s.refusalOutput = s.nextOutput
			item := ResponsesMessage{
				ID:      new(s.refusalID),
				Type:    new(ResponsesMessageTypeRefusal),
				Role:    new(ResponsesMessageRoleAssistant),
				Status:  new("in_progress"),
				Content: &ResponsesMessageContent{ContentBlocks: []ResponsesMessageContentBlock{}},
			}
			events = append(events,
				s.responseEvent(ResponsesStreamResponseTypeOutputItemAdded, ResponsesStreamResponse{
					OutputIndex: new(s.refusalOutput),
					Item:        &item,
				}),
				s.responseEvent(ResponsesStreamResponseTypeContentPartAdded, ResponsesStreamResponse{
					ItemID:       new(s.refusalID),
					OutputIndex:  new(s.refusalOutput),
					ContentIndex: new(0),
					Part:         &ResponsesMessageContentBlock{Type: ResponsesMessageContentBlockTypeRefusal},
				}),
			)
			s.nextOutput++
			s.refusalOpen = true
		}
		events = append(events, s.responseEvent(ResponsesStreamResponseTypeRefusalDelta, ResponsesStreamResponse{
			ItemID:       new(s.refusalID),
			OutputIndex:  new(s.refusalOutput),
			ContentIndex: new(0),
			Delta:        delta.Refusal,
		}))
		s.refusal.WriteString(*delta.Refusal)
	}

	if chunk.Choices[0].FinishReason != nil {
		terminal := ResponsesStreamResponseTypeCompleted
		status := "completed"
		reason := ""
		switch *chunk.Choices[0].FinishReason {
		case "length":
			terminal = ResponsesStreamResponseTypeIncomplete
			status = "incomplete"
			reason = "max_output_tokens"
		case "content_filter":
			// A safety-filtered completion is not a success: report it as
			// incomplete with the content_filter reason.
			terminal = ResponsesStreamResponseTypeIncomplete
			status = "incomplete"
			reason = "content_filter"
		}
		if s.reasoningOpen {
			events = append(events, s.closeReasoning(status)...)
		}
		if s.textOpen {
			events = append(events, s.closeText(status)...)
		}
		if s.refusalOpen {
			events = append(events, s.closeRefusal(status)...)
		}
		for _, tc := range s.toolCalls {
			if tc.arguments.Len() > 0 {
				events = append(events, s.responseEvent(ResponsesStreamResponseTypeFunctionCallArgumentsDone, ResponsesStreamResponse{
					ItemID:      new(tc.id),
					OutputIndex: new(tc.output),
					Arguments:   new(tc.arguments.String()),
				}))
			}
			item := ResponsesMessage{
				ID:     new(tc.id),
				Type:   new(ResponsesMessageTypeFunctionCall),
				Status: new(status),
				ResponsesToolMessage: &ResponsesToolMessage{
					CallID:    nonEmptyPtr(tc.callID),
					Name:      nonEmptyPtr(tc.name),
					Arguments: nonEmptyPtr(tc.arguments.String()),
				},
			}
			events = append(events, s.responseEvent(ResponsesStreamResponseTypeOutputItemDone, ResponsesStreamResponse{
				OutputIndex: new(tc.output),
				Item:        &item,
			}))
			s.items = append(s.items, item)
		}
		envelope := s.envelope(status)
		if reason != "" {
			envelope.IncompleteDetails = json.RawMessage(`{"reason":"` + reason + `"}`)
		}
		envelope.Output = s.items
		if chunk.Usage != nil {
			envelope.Usage = chatLLMUsageResponsesUsage(chunk.Usage)
		}
		// Hold the terminal event back: a trailing usage-only chunk may still
		// carry the final usage. It is released by a subsequent usage chunk or
		// by Terminal at upstream EOF.
		s.terminalEvent = []ResponsesStreamResponse{s.responseEvent(terminal, ResponsesStreamResponse{Response: envelope})}
	}

	return events
}

// emitTerminal releases the held-back terminal event, attaching any usage
// accumulated from a trailing usage-only chunk.
func (s *ChatResponsesStreamState) emitTerminal() []ResponsesStreamResponse {
	if s.terminalEvent == nil {
		return nil
	}
	events := s.terminalEvent
	s.terminalEvent = nil
	s.finished = true
	if events[0].Response != nil && events[0].Response.Usage == nil && s.pendingUsage != nil {
		events[0].Response.Usage = s.pendingUsage
	}
	return events
}

// Terminal returns the held-back terminal event, or nil. It is called when
// the upstream stream ends without a further data frame.
func (s *ChatResponsesStreamState) Terminal() []ResponsesStreamResponse {
	return s.emitTerminal()
}

// lookupToolCallID returns the tracked tool call with the given chat call id.
func (s *ChatResponsesStreamState) lookupToolCallID(callID string) *chatStreamToolCall {
	for _, tc := range s.toolCalls {
		if tc.callID == callID {
			return tc
		}
	}
	return nil
}

// lookupToolCallIndex returns the tracked tool call with the given chat stream
// index, or nil.
func (s *ChatResponsesStreamState) lookupToolCallIndex(index int) *chatStreamToolCall {
	for _, tc := range s.toolCalls {
		if tc.index == index {
			return tc
		}
	}
	return nil
}

// envelope builds the response envelope carried by lifecycle and terminal
// events.
func (s *ChatResponsesStreamState) envelope(status string) *ResponsesResponse {
	return &ResponsesResponse{
		ID:        s.messageID,
		Object:    "response",
		CreatedAt: s.createdAt,
		Status:    new(status),
		Model:     s.model,
	}
}

// responseEvent wraps an event with the next sequence number, starting at
// zero like the real API.
func (s *ChatResponsesStreamState) responseEvent(eventType ResponsesStreamResponseType, payload ResponsesStreamResponse) ResponsesStreamResponse {
	payload.Type = eventType
	payload.SequenceNumber = s.sequence
	s.sequence++
	return payload
}

// closeReasoning emits the terminal events of the open reasoning item: the
// summary text done and summary part done events, then the item done.
// The status parameter (completed/incomplete) is threaded from the
// finish-reason and transition call sites, matching closeText and closeRefusal.
func (s *ChatResponsesStreamState) closeReasoning(status string) []ResponsesStreamResponse {
	reasoning := s.reasoning.String()
	item := ResponsesMessage{
		ID:     new(s.reasoningID),
		Type:   new(ResponsesMessageTypeReasoning),
		Role:   new(ResponsesMessageRoleAssistant),
		Status: new(status),
	}

	var summary []ResponsesReasoningSummary
	var blocks []ResponsesMessageContentBlock
	var encryptedContent *string

	// Collect all reasoning details in a single pass.
	// Use summary-type details if present; otherwise fall back to
	// the accumulated reasoning text to avoid duplicate summaries.
	for j := range s.reasoningDetails {
		d := &s.reasoningDetails[j]
		switch d.Type {
		case ChatReasoningDetailsTypeSummary:
			if d.Summary != nil {
				summary = append(summary, ResponsesReasoningSummary{Type: "summary_text", Text: *d.Summary})
			}
		case ChatReasoningDetailsTypeEncrypted:
			encryptedContent = d.Data
		default:
			if d.Text != nil {
				blocks = append(blocks, ResponsesMessageContentBlock{Type: ResponsesMessageContentBlockTypeReasoning, Text: d.Text, Signature: d.Signature})
			}
		}
	}

	// If no summary-type details were found, use the accumulated reasoning text.
	if len(summary) == 0 && reasoning != "" {
		summary = append(summary, ResponsesReasoningSummary{Type: "summary_text", Text: reasoning})
	}

	if len(summary) > 0 || encryptedContent != nil {
		item.ResponsesReasoning = &ResponsesReasoning{Summary: summary, EncryptedContent: encryptedContent}
	}
	if len(blocks) > 0 {
		item.Content = &ResponsesMessageContent{ContentBlocks: blocks}
	}

	index := s.reasoningOutput
	s.reasoningOpen = false
	s.reasoningClosed = true
	s.reasoning.Reset()
	s.reasoningDetails = nil
	s.items = append(s.items, item)

	return []ResponsesStreamResponse{
		s.responseEvent(ResponsesStreamResponseTypeReasoningSummaryTextDone, ResponsesStreamResponse{
			ItemID:       new(s.reasoningID),
			OutputIndex:  new(index),
			SummaryIndex: new(0),
			Text:         new(reasoning),
		}),
		s.responseEvent(ResponsesStreamResponseTypeReasoningSummaryPartDone, ResponsesStreamResponse{
			ItemID:       new(s.reasoningID),
			OutputIndex:  new(index),
			SummaryIndex: new(0),
		}),
		s.responseEvent(ResponsesStreamResponseTypeOutputItemDone, ResponsesStreamResponse{
			OutputIndex: new(index),
			Item:        &item,
		}),
	}
}

// reasoningOutputIndex is removed: closeReasoning now uses s.reasoningOutput
// directly, which is set when the reasoning item is opened and is always
// correct at close time.

// closeRefusal emits the terminal events of the open refusal item. There is
// no refusal.done event in the responses stream schema (mirroring Bifrost),
// so the close is content_part.done + output_item.done.
func (s *ChatResponsesStreamState) closeRefusal(status string) []ResponsesStreamResponse {
	refusal := s.refusal.String()
	output := s.refusalOutput
	item := ResponsesMessage{
		ID:     new(s.refusalID),
		Type:   new(ResponsesMessageTypeRefusal),
		Role:   new(ResponsesMessageRoleAssistant),
		Status: new(status),
		Content: &ResponsesMessageContent{ContentBlocks: []ResponsesMessageContentBlock{{
			Type: ResponsesMessageContentBlockTypeRefusal,
			Text: new(refusal),
		}}},
	}
	s.refusalOpen = false
	s.refusal.Reset()
	s.items = append(s.items, item)
	return []ResponsesStreamResponse{
		s.responseEvent(ResponsesStreamResponseTypeContentPartDone, ResponsesStreamResponse{
			ItemID:       new(s.refusalID),
			OutputIndex:  new(output),
			ContentIndex: new(0),
		}),
		s.responseEvent(ResponsesStreamResponseTypeOutputItemDone, ResponsesStreamResponse{
			OutputIndex: new(output),
			Item:        &item,
		}),
	}
}

// closeText emits the terminal events of the open text item.
func (s *ChatResponsesStreamState) closeText(status string) []ResponsesStreamResponse {
	text := s.text.String()
	output := s.textOutput
	item := ResponsesMessage{
		ID:      new(s.textID),
		Type:    new(ResponsesMessageTypeMessage),
		Role:    new(ResponsesMessageRoleAssistant),
		Status:  new(status),
		Content: &ResponsesMessageContent{ContentBlocks: []ResponsesMessageContentBlock{{Type: ResponsesMessageContentBlockTypeOutputText, Text: new(text)}}},
	}
	s.textOpen = false
	s.text.Reset()
	s.items = append(s.items, item)
	return []ResponsesStreamResponse{
		s.responseEvent(ResponsesStreamResponseTypeOutputTextDone, ResponsesStreamResponse{
			ItemID:       new(s.textID),
			OutputIndex:  new(output),
			ContentIndex: new(0),
			Text:         new(text),
		}),
		s.responseEvent(ResponsesStreamResponseTypeContentPartDone, ResponsesStreamResponse{
			ItemID:       new(s.textID),
			OutputIndex:  new(output),
			ContentIndex: new(0),
		}),
		s.responseEvent(ResponsesStreamResponseTypeOutputItemDone, ResponsesStreamResponse{
			OutputIndex: new(output),
			Item:        &item,
		}),
	}
}
