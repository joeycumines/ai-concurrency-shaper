// Responses stream -> Anthropic stream conversion. The stateful accumulator
// translates each responses event into zero or more anthropic events,
// tracking content block indices and the terminal message lifecycle.

package transcode

import (
	"encoding/json"
)

// =============================================================================
// Responses stream -> Anthropic stream
// =============================================================================

// AnthropicResponsesStreamState accumulates a responses SSE stream and
// converts each event into zero or more Anthropic SSE events. The zero value
// is ready to use; the state must not be shared across streams.
type AnthropicResponsesStreamState struct {
	messageID       string
	model           string
	nextBlock       int
	blockIndex      map[string]int
	hasMessageDelta bool
	sawToolUse      bool
}

// ConvertResponsesStreamResponseAnthropicStreamEvent converts one responses
// stream event into Anthropic stream events. Events after the terminal
// message_stop are dropped, except ping and error.
func (s *AnthropicResponsesStreamState) ConvertResponsesStreamResponseAnthropicStreamEvent(event *ResponsesStreamResponse) []AnthropicStreamEvent {
	if event == nil || (s.hasMessageDelta && event.Type != ResponsesStreamResponseTypePing && event.Type != ResponsesStreamResponseTypeError) {
		return nil
	}
	if s.blockIndex == nil {
		s.blockIndex = make(map[string]int)
	}
	switch event.Type {
	case ResponsesStreamResponseTypeCreated:
		if event.Response == nil {
			return nil
		}
		s.messageID = event.Response.ID
		s.model = event.Response.Model
		usage := &AnthropicUsage{}
		if event.Response.Usage != nil {
			usage = responsesUsageAnthropicUsage(event.Response.Usage)
		}
		return []AnthropicStreamEvent{{
			Type: AnthropicStreamEventTypeMessageStart,
			Message: &AnthropicMessageResponse{
				ID:      s.messageID,
				Type:    "message",
				Role:    "assistant",
				Model:   s.model,
				Content: []AnthropicContentBlock{},
				Usage:   usage,
			},
		}}

	case ResponsesStreamResponseTypeInProgress:
		return nil

	case ResponsesStreamResponseTypeOutputItemAdded:
		if event.Item == nil || event.Item.Type == nil || event.Item.ID == nil {
			// An item without an id cannot be tracked for its deltas and
			// done events; skip it entirely so no orphaned content block
			// start is emitted.
			return nil
		}
		switch *event.Item.Type {
		case ResponsesMessageTypeRefusal:
			// Refusal items stream as text deltas through the open
			// text block; the refusal text is the content.
			index := s.nextBlock
			s.nextBlock++
			s.blockIndex[*event.Item.ID] = index
			block := AnthropicContentBlock{Type: AnthropicContentBlockTypeText, Text: new("")}
			return []AnthropicStreamEvent{{Type: AnthropicStreamEventTypeContentBlockStart, Index: new(index), ContentBlock: &block}}
		case ResponsesMessageTypeMessage:
			index := s.nextBlock
			s.nextBlock++
			s.blockIndex[*event.Item.ID] = index
			block := AnthropicContentBlock{Type: AnthropicContentBlockTypeText, Text: new("")}
			return []AnthropicStreamEvent{{Type: AnthropicStreamEventTypeContentBlockStart, Index: new(index), ContentBlock: &block}}
		case ResponsesMessageTypeReasoning:
			index := s.nextBlock
			s.nextBlock++
			s.blockIndex[*event.Item.ID] = index
			block := AnthropicContentBlock{Type: AnthropicContentBlockTypeThinking, Thinking: new("")}
			return []AnthropicStreamEvent{{Type: AnthropicStreamEventTypeContentBlockStart, Index: new(index), ContentBlock: &block}}
		case ResponsesMessageTypeFunctionCall:
			index := s.nextBlock
			s.nextBlock++
			s.blockIndex[*event.Item.ID] = index
			s.sawToolUse = true
			block := AnthropicContentBlock{Type: AnthropicContentBlockTypeToolUse, Input: json.RawMessage(`{}`)}
			if event.Item.ResponsesToolMessage != nil {
				if event.Item.CallID != nil {
					block.ID = event.Item.CallID
				}
				block.Name = event.Item.Name
			}
			return []AnthropicStreamEvent{{Type: AnthropicStreamEventTypeContentBlockStart, Index: new(index), ContentBlock: &block}}
		}
		// Unsupported item types consume no block index.
		return nil

	case ResponsesStreamResponseTypeContentPartAdded, ResponsesStreamResponseTypeContentPartDone:
		return nil

	case ResponsesStreamResponseTypeOutputTextDelta:
		if event.Delta == nil || event.ItemID == nil {
			return nil
		}
		index, ok := s.blockIndex[*event.ItemID]
		if !ok {
			return nil
		}
		return []AnthropicStreamEvent{{Type: AnthropicStreamEventTypeContentBlockDelta, Index: new(index), Delta: &AnthropicStreamDelta{Type: AnthropicStreamDeltaTypeTextDelta, Text: event.Delta}}}

	case ResponsesStreamResponseTypeReasoningSummaryTextDelta:
		if event.Delta == nil || event.ItemID == nil {
			return nil
		}
		index, ok := s.blockIndex[*event.ItemID]
		if !ok {
			return nil
		}
		return []AnthropicStreamEvent{{Type: AnthropicStreamEventTypeContentBlockDelta, Index: new(index), Delta: &AnthropicStreamDelta{Type: AnthropicStreamDeltaTypeThinkingDelta, Thinking: event.Delta}}}

	case ResponsesStreamResponseTypeFunctionCallArgumentsDelta:
		if event.Arguments == nil || event.ItemID == nil {
			return nil
		}
		index, ok := s.blockIndex[*event.ItemID]
		if !ok {
			return nil
		}
		return []AnthropicStreamEvent{{Type: AnthropicStreamEventTypeContentBlockDelta, Index: new(index), Delta: &AnthropicStreamDelta{Type: AnthropicStreamDeltaTypeInputJSONDelta, PartialJSON: event.Arguments}}}

	case ResponsesStreamResponseTypeRefusalDelta:
		// Anthropic has no refusal content block; stream the refusal text as
		// the open text block's delta so it is not dropped.
		if event.Delta == nil || event.ItemID == nil {
			return nil
		}
		index, ok := s.blockIndex[*event.ItemID]
		if !ok {
			return nil
		}
		return []AnthropicStreamEvent{{Type: AnthropicStreamEventTypeContentBlockDelta, Index: new(index), Delta: &AnthropicStreamDelta{Type: AnthropicStreamDeltaTypeTextDelta, Text: event.Delta}}}

	case ResponsesStreamResponseTypeOutputItemDone:
		var id string
		if event.Item != nil && event.Item.ID != nil {
			id = *event.Item.ID
		} else if event.ItemID != nil {
			id = *event.ItemID
		}
		if id == "" {
			return nil
		}
		index, ok := s.blockIndex[id]
		if !ok {
			return nil
		}
		return []AnthropicStreamEvent{{Type: AnthropicStreamEventTypeContentBlockStop, Index: new(index)}}

	case ResponsesStreamResponseTypeCompleted, ResponsesStreamResponseTypeIncomplete, ResponsesStreamResponseTypeFailed:
		if s.hasMessageDelta {
			return nil
		}
		s.hasMessageDelta = true
		stopReason := AnthropicStopReasonEndTurn
		if event.Type == ResponsesStreamResponseTypeIncomplete {
			stopReason = AnthropicStopReasonMaxTokens
		} else if s.sawToolUse {
			stopReason = AnthropicStopReasonToolUse
		}
		events := []AnthropicStreamEvent{{
			Type:  AnthropicStreamEventTypeMessageDelta,
			Delta: &AnthropicStreamDelta{StopReason: &stopReason},
		}}
		if event.Response != nil && event.Response.Usage != nil {
			events[0].Usage = responsesUsageAnthropicUsage(event.Response.Usage)
		}
		events = append(events, AnthropicStreamEvent{Type: AnthropicStreamEventTypeMessageStop})
		return events

	case ResponsesStreamResponseTypePing:
		return []AnthropicStreamEvent{{Type: AnthropicStreamEventTypePing}}

	case ResponsesStreamResponseTypeError:
		return []AnthropicStreamEvent{{Type: AnthropicStreamEventTypeError}}
	}
	return nil
}
