package transcode

import (
	"encoding/json"
	"errors"
)

// frameConverters adapt the typed stream state machines to the
// frameConverter interface consumed by the convertingReader. Each direction
// has one adapter; the chat→anthropic direction composes the two state
// machines strictly in memory — typed events flow between them and are never
// re-serialized through another dialect.

// chatToResponsesConverter adapts chatResponsesStreamState to frameConverter,
// marshaling typed Responses events into frames.
type chatToResponsesConverter struct {
	state *chatResponsesStreamState
}

func newChatToResponsesConverter(state *chatResponsesStreamState) *chatToResponsesConverter {
	return &chatToResponsesConverter{state: state}
}

// Convert processes one upstream Chat SSE frame.
func (c *chatToResponsesConverter) Convert(
	frame SSEEvent,
) (convertedBatch, error) {
	chunk, err := chatStreamChunkFromSSE(frame)
	if errors.Is(err, errChatStreamDone) {
		// [DONE] releases the held terminal batch and stops the reader.
		held, ok := c.state.releaseTerminal()
		if !ok {
			return convertedBatch{}, nil
		}
		batch, err := marshalResponsesEvents(held)
		batch.Terminal = true
		return batch, err
	}
	if err != nil {
		return convertedBatch{}, err
	}

	events, err := c.state.Convert(chunk)
	if err != nil {
		return convertedBatch{}, err
	}
	return marshalResponsesEvents(events)
}

// FinalizeEOF releases a held terminal or reports a truncation error. A
// stream that ended without a terminal condition is never reported as
// success.
func (c *chatToResponsesConverter) FinalizeEOF() (convertedBatch, error) {
	events, err := c.state.FinalizeEOF()
	if err != nil {
		return convertedBatch{}, err
	}
	batch, err := marshalResponsesEvents(events)
	batch.Terminal = true
	return batch, err
}

// marshalResponsesEvents validates and marshals typed Responses events into
// frames.
func marshalResponsesEvents(
	events []ResponsesSSEEvent,
) (convertedBatch, error) {
	batch := convertedBatch{}
	for _, event := range events {
		data, err := MarshalResponsesEvent(event)
		if err != nil {
			return convertedBatch{}, err
		}
		batch.Events = append(batch.Events, frameEvent{
			Type: event.EventType(),
			Data: data,
		})
	}
	return batch, nil
}

// responsesToAnthropicConverter adapts anthropicResponsesStreamState to
// frameConverter. Upstream Responses SSE frames are decoded into typed
// Responses events, converted, and marshaled into Anthropic frames.
type responsesToAnthropicConverter struct {
	state *anthropicResponsesStreamState
}

func newResponsesToAnthropicConverter(
	state *anthropicResponsesStreamState,
) *responsesToAnthropicConverter {
	return &responsesToAnthropicConverter{state: state}
}

// Convert processes one upstream Responses SSE frame.
func (c *responsesToAnthropicConverter) Convert(
	frame SSEEvent,
) (convertedBatch, error) {
	data := trimJSONSpace(frame.Data)
	if isResponsesDoneSentinel(data) {
		// The Responses protocol has no [DONE] sentinel; this is defensive
		// tolerance for a chat-style upstream. The terminal was already
		// emitted at response.completed; a [DONE] before any terminal is a
		// truncation and must not end the stream cleanly.
		if c.state.sawTerminal {
			return convertedBatch{Terminal: true}, nil
		}
		return convertedBatch{}, errors.New(
			"responses stream [DONE] before a terminal condition",
		)
	}

	event, err := decodeResponsesSSEEvent(data)
	if err != nil {
		return convertedBatch{}, err
	}
	// Validate the SSE event name equals the JSON type.
	if err := validateEventNameMatchesJSONType(frame); err != nil {
		return convertedBatch{}, err
	}

	anthropicEvents, err := c.state.Convert(event)
	if err != nil {
		return convertedBatch{}, err
	}
	batch, err := marshalAnthropicEvents(anthropicEvents)
	if err != nil {
		return convertedBatch{}, err
	}
	// Any terminal condition stops the reader immediately: the Responses
	// protocol has no [DONE] sentinel, so the success terminal (message_delta
	// + message_stop) is emitted at response.completed, not held until EOF.
	batch.Terminal = c.state.sawTerminal
	return batch, nil
}

// FinalizeEOF releases a held terminal or reports a truncation error.
func (c *responsesToAnthropicConverter) FinalizeEOF() (convertedBatch, error) {
	events, err := c.state.FinalizeEOF()
	if err != nil {
		return convertedBatch{}, err
	}
	batch, err := marshalAnthropicEvents(events)
	batch.Terminal = true
	return batch, err
}

// marshalAnthropicEvents marshals typed Anthropic events into frames. The
// SSE event name equals the JSON type, matching the manual conformance
// validator.
func marshalAnthropicEvents(
	events []AnthropicStreamEvent,
) (convertedBatch, error) {
	batch := convertedBatch{}
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			return convertedBatch{}, err
		}
		batch.Events = append(batch.Events, frameEvent{
			Type: string(event.Type),
			Data: data,
		})
	}
	return batch, nil
}

// chatToAnthropicConverter composes the Chat→Responses and Responses→
// Anthropic state machines strictly in memory. Typed Responses events flow
// from one state machine into the other without intermediate JSON.
type chatToAnthropicConverter struct {
	chat      *chatResponsesStreamState
	anthropic *anthropicResponsesStreamState
}

func newChatToAnthropicConverter(
	chat *chatResponsesStreamState,
	anthropic *anthropicResponsesStreamState,
) *chatToAnthropicConverter {
	return &chatToAnthropicConverter{
		chat:      chat,
		anthropic: anthropic,
	}
}

// Convert processes one upstream Chat SSE frame through both state machines.
func (c *chatToAnthropicConverter) Convert(
	frame SSEEvent,
) (convertedBatch, error) {
	chunk, err := chatStreamChunkFromSSE(frame)
	if errors.Is(err, errChatStreamDone) {
		return c.releaseTerminals()
	}
	if err != nil {
		return convertedBatch{}, err
	}

	responsesEvents, err := c.chat.Convert(chunk)
	if err != nil {
		return convertedBatch{}, err
	}
	return c.convertResponsesEvents(responsesEvents)
}

// convertResponsesEvents feeds typed Responses events into the Anthropic
// state machine in memory.
func (c *chatToAnthropicConverter) convertResponsesEvents(
	responsesEvents []ResponsesSSEEvent,
) (convertedBatch, error) {
	var anthropicEvents []AnthropicStreamEvent
	for _, event := range responsesEvents {
		converted, err := c.anthropic.Convert(event)
		if err != nil {
			return convertedBatch{}, err
		}
		anthropicEvents = append(anthropicEvents, converted...)
	}
	batch, err := marshalAnthropicEvents(anthropicEvents)
	if err != nil {
		return convertedBatch{}, err
	}
	batch.Terminal = c.anthropic.sawTerminal
	return batch, nil
}

// releaseTerminals releases the held terminal from the Chat state machine,
// flowing it through the Anthropic state machine (which emits message_delta +
// message_stop immediately at response.completed).
func (c *chatToAnthropicConverter) releaseTerminals() (convertedBatch, error) {
	var batch convertedBatch
	if held, ok := c.chat.releaseTerminal(); ok {
		converted, err := c.convertResponsesEvents(held)
		if err != nil {
			return convertedBatch{}, err
		}
		batch.Events = append(batch.Events, converted.Events...)
		batch.Terminal = converted.Terminal
	}
	return batch, nil
}

// FinalizeEOF releases any held terminals or reports a truncation error.
func (c *chatToAnthropicConverter) FinalizeEOF() (convertedBatch, error) {
	batch, err := c.releaseTerminals()
	if err != nil {
		return convertedBatch{}, err
	}
	if batch.Terminal || len(batch.Events) > 0 {
		batch.Terminal = true
		return batch, nil
	}
	if c.chat.sawFinish || c.anthropic.sawTerminal {
		return convertedBatch{Terminal: true}, nil
	}
	return convertedBatch{}, errors.New(
		"chat stream ended before a terminal condition",
	)
}

// decodeResponsesSSEEvent decodes one Responses stream frame into the typed
// event union. Events are returned by value so the state machines' value-type
// switches match.
func decodeResponsesSSEEvent(data []byte) (ResponsesSSEEvent, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}

	var event ResponsesSSEEvent
	switch probe.Type {
	case "response.created":
		event = &ResponseCreatedEvent{}
	case "response.in_progress":
		event = &ResponseInProgressEvent{}
	case "response.output_item.added":
		event = &ResponseOutputItemAddedEvent{}
	case "response.output_item.done":
		event = &ResponseOutputItemDoneEvent{}
	case "response.reasoning_summary_part.added":
		event = &ResponseReasoningSummaryPartAddedEvent{}
	case "response.reasoning_summary_text.delta":
		event = &ResponseReasoningSummaryTextDeltaEvent{}
	case "response.reasoning_summary_text.done":
		event = &ResponseReasoningSummaryTextDoneEvent{}
	case "response.reasoning_summary_part.done":
		event = &ResponseReasoningSummaryPartDoneEvent{}
	case "response.content_part.added":
		event = &ResponseContentPartAddedEvent{}
	case "response.output_text.delta":
		event = &ResponseTextDeltaEvent{}
	case "response.output_text.done":
		event = &ResponseTextDoneEvent{}
	case "response.content_part.done":
		event = &ResponseContentPartDoneEvent{}
	case "response.function_call_arguments.delta":
		event = &ResponseFunctionCallArgumentsDeltaEvent{}
	case "response.function_call_arguments.done":
		event = &ResponseFunctionCallArgumentsDoneEvent{}
	case "response.refusal.delta":
		event = &ResponseRefusalDeltaEvent{}
	case "response.refusal.done":
		event = &ResponseRefusalDoneEvent{}
	case "response.completed":
		event = &ResponseCompletedEvent{}
	case "response.incomplete":
		event = &ResponseIncompleteEvent{}
	case "response.failed":
		event = &ResponseFailedEvent{}
	case "error":
		event = &ResponseErrorEvent{}
	default:
		return nil, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     "stream[].type",
			Feature:  probe.Type,
		}
	}

	if err := strictDecode(data, event); err != nil {
		return nil, err
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}

	// Return by value so the value-type switches in the state machines match.
	switch probe.Type {
	case "response.created":
		return *(event.(*ResponseCreatedEvent)), nil
	case "response.in_progress":
		return *(event.(*ResponseInProgressEvent)), nil
	case "response.output_item.added":
		return *(event.(*ResponseOutputItemAddedEvent)), nil
	case "response.output_item.done":
		return *(event.(*ResponseOutputItemDoneEvent)), nil
	case "response.reasoning_summary_part.added":
		return *(event.(*ResponseReasoningSummaryPartAddedEvent)), nil
	case "response.reasoning_summary_text.delta":
		return *(event.(*ResponseReasoningSummaryTextDeltaEvent)), nil
	case "response.reasoning_summary_text.done":
		return *(event.(*ResponseReasoningSummaryTextDoneEvent)), nil
	case "response.reasoning_summary_part.done":
		return *(event.(*ResponseReasoningSummaryPartDoneEvent)), nil
	case "response.content_part.added":
		return *(event.(*ResponseContentPartAddedEvent)), nil
	case "response.output_text.delta":
		return *(event.(*ResponseTextDeltaEvent)), nil
	case "response.output_text.done":
		return *(event.(*ResponseTextDoneEvent)), nil
	case "response.content_part.done":
		return *(event.(*ResponseContentPartDoneEvent)), nil
	case "response.function_call_arguments.delta":
		return *(event.(*ResponseFunctionCallArgumentsDeltaEvent)), nil
	case "response.function_call_arguments.done":
		return *(event.(*ResponseFunctionCallArgumentsDoneEvent)), nil
	case "response.refusal.delta":
		return *(event.(*ResponseRefusalDeltaEvent)), nil
	case "response.refusal.done":
		return *(event.(*ResponseRefusalDoneEvent)), nil
	case "response.completed":
		return *(event.(*ResponseCompletedEvent)), nil
	case "response.incomplete":
		return *(event.(*ResponseIncompleteEvent)), nil
	case "response.failed":
		return *(event.(*ResponseFailedEvent)), nil
	case "error":
		return *(event.(*ResponseErrorEvent)), nil
	default:
		panic("unreachable: validated event type")
	}
}

// isResponsesDoneSentinel reports whether the frame is the Responses [DONE]
// sentinel.
func isResponsesDoneSentinel(data []byte) bool {
	return string(data) == "[DONE]"
}

// ErrorEvent builds a client-dialect Responses error event frame.
func (c *chatToResponsesConverter) ErrorEvent(err error) (frameEvent, bool) {
	return responsesErrorFrame(err)
}

// ErrorEvent builds a client-dialect Anthropic error event frame.
func (c *responsesToAnthropicConverter) ErrorEvent(err error) (frameEvent, bool) {
	return anthropicErrorFrame(err)
}

// ErrorEvent builds a client-dialect Anthropic error event frame (the
// composed stream's client is always Messages).
func (c *chatToAnthropicConverter) ErrorEvent(err error) (frameEvent, bool) {
	return anthropicErrorFrame(err)
}

// responsesErrorFrame marshals a Responses error event.
func responsesErrorFrame(err error) (frameEvent, bool) {
	event := ResponseErrorEvent{
		responsesEventBase: responsesEventBase{
			Type:           "error",
			SequenceNumber: 0,
		},
		Code:    "conversion_error",
		Message: err.Error(),
		Param:   "",
	}
	data, marshalErr := MarshalResponsesEvent(event)
	if marshalErr != nil {
		return frameEvent{}, false
	}
	return frameEvent{Type: "error", Data: data}, true
}

// anthropicErrorFrame marshals an Anthropic error event with the nested
// error object.
func anthropicErrorFrame(err error) (frameEvent, bool) {
	event := AnthropicStreamEvent{
		Type: AnthropicStreamEventTypeError,
		Error: &AnthropicStreamError{
			Type:    "api_error",
			Message: err.Error(),
		},
	}
	data, marshalErr := json.Marshal(event)
	if marshalErr != nil {
		return frameEvent{}, false
	}
	return frameEvent{Type: "error", Data: data}, true
}
