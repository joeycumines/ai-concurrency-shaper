package transcode

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
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
		// [DONE] is the protocol terminal: release the held terminal batch
		// and stop the reader. A [DONE] before any terminal condition is a
		// truncation the sentinel itself decides: the typed error is
		// returned so the exchange classifies as an upstream body failure
		// and the reader stops immediately — never an empty successful
		// batch that would wait on an upstream keeping the connection open
		// after [DONE] (review-k finding 1). The sawFinish guard is
		// required because a zero-output finish holds an EMPTY batch that
		// releaseTerminal still releases (review-08 blocker 2).
		if !c.state.sawFinish {
			return convertedBatch{}, errChatDoneBeforeTerminal()
		}
		held, ok := c.state.releaseTerminal()
		if !ok {
			// Defensive: the terminal was already released (the reader
			// stops at the first terminal batch, so a second [DONE] is
			// unreachable through the reader).
			return convertedBatch{}, c.state.wireError(errors.New(
				"chat stream [DONE] after the terminal was released",
			))
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

// FinalizeEOF reports a truncation error unless the stream terminated
// correctly. The held terminal (which may be an empty batch for a
// zero-output finish) is released ONLY by the [DONE] sentinel (review-08
// blocker 2).
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
// frames. A marshaled payload amplified beyond the frame bound (JSON
// escaping, terminal repetition) is a typed frame error (review-08 blocker
// 7): the generated downstream wire must be bounded like the input wire.
func marshalResponsesEvents(
	events []ResponsesSSEEvent,
) (convertedBatch, error) {
	batch := convertedBatch{}
	for _, event := range events {
		data, err := MarshalResponsesEvent(event)
		if err != nil {
			return convertedBatch{}, err
		}
		if len(data) > maxSSEFrameBytes {
			return convertedBatch{}, &SSEBoundError{Bound: maxSSEFrameBytes}
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
		return convertedBatch{}, upstreamWireError(
			UpstreamResponses,
			http.StatusOK,
			errors.New("responses stream [DONE] before a terminal condition"),
		)
	}

	event, err := decodeResponsesSSEEvent(data)
	if err != nil {
		return convertedBatch{}, err
	}
	// Validate the SSE event name equals the JSON type. Responses streams
	// require event: to be present and equal the JSON type tag (the
	// package's own rule, review-08 additional 12): an empty event name is
	// a wire error, not a silent pass.
	if frame.Event == "" {
		return convertedBatch{}, upstreamWireError(
			UpstreamResponses,
			http.StatusOK,
			errors.New("responses stream event has no event name"),
		)
	}
	if err := validateEventNameMatchesJSONType(frame); err != nil {
		return convertedBatch{}, upstreamWireError(
			UpstreamResponses,
			http.StatusOK,
			err,
		)
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
// validator. A marshaled payload amplified beyond the frame bound (JSON
// escaping, terminal repetition) is a typed frame error (review-08 blocker
// 7).
func marshalAnthropicEvents(
	events []AnthropicStreamEvent,
) (convertedBatch, error) {
	batch := convertedBatch{}
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			return convertedBatch{}, err
		}
		if len(data) > maxSSEFrameBytes {
			return convertedBatch{}, &SSEBoundError{Bound: maxSSEFrameBytes}
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
		// [DONE] is the protocol terminal: release the held terminal (which
		// flows through the Anthropic state machine) or fail immediately. A
		// [DONE] before any terminal condition is a truncation the sentinel
		// itself decides: the typed error is returned so the exchange
		// classifies as an upstream body failure and the reader stops
		// immediately — never an empty non-terminal batch that would wait
		// on an upstream keeping the connection open after [DONE] (review-k
		// finding 1). The sawFinish guard is required because a zero-output
		// finish holds an EMPTY batch that releaseTerminals still releases
		// (review-08 blocker 2).
		if !c.chat.sawFinish {
			return convertedBatch{}, errChatDoneBeforeTerminal()
		}
		batch, releaseErr := c.releaseTerminals()
		if releaseErr != nil {
			return convertedBatch{}, releaseErr
		}
		if batch.Terminal || len(batch.Events) > 0 {
			return batch, nil
		}
		return convertedBatch{}, errChatDoneBeforeTerminal()
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

// FinalizeEOF reports a truncation error unless the stream terminated
// correctly. The Chat held terminal (which may be an empty batch for a
// zero-output finish) is released ONLY by the [DONE] sentinel (review-08
// blocker 2): EOF after finish_reason without [DONE] is a typed upstream
// truncation, never a released terminal.
func (c *chatToAnthropicConverter) FinalizeEOF() (convertedBatch, error) {
	if c.chat.sawFinish && !c.chat.terminalReleased {
		return convertedBatch{}, c.chat.wireError(errors.New(
			"chat stream ended after finish_reason without the [DONE] sentinel",
		))
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
// switches match. The wire layer's unknown-event-type report is a valid-but-
// unsupported source feature (local), never corrupt wire; every other decode
// or validation failure is upstream wire corruption.
func decodeResponsesSSEEvent(data []byte) (ResponsesSSEEvent, error) {
	event, err := openairesponses.DecodeEvent(data)
	if err != nil {
		if unsupported, ok := errors.AsType[*wire.UnsupportedTypeError](err); ok {
			return nil, &UnsupportedFeatureError{
				Protocol: unsupported.Protocol,
				Path:     unsupported.Path,
				Feature:  unsupported.Type,
			}
		}
		return nil, upstreamWireError(UpstreamResponses, http.StatusOK, err)
	}

	// Return by value so the value-type switches in the state machines match.
	switch probe := event.(type) {
	case *openairesponses.CreatedEvent:
		return *probe, nil
	case *openairesponses.InProgressEvent:
		return *probe, nil
	case *openairesponses.OutputItemAddedEvent:
		return *probe, nil
	case *openairesponses.OutputItemDoneEvent:
		return *probe, nil
	case *openairesponses.ReasoningSummaryPartAddedEvent:
		return *probe, nil
	case *openairesponses.ReasoningSummaryTextDeltaEvent:
		return *probe, nil
	case *openairesponses.ReasoningSummaryTextDoneEvent:
		return *probe, nil
	case *openairesponses.ReasoningSummaryPartDoneEvent:
		return *probe, nil
	case *openairesponses.ContentPartAddedEvent:
		return *probe, nil
	case *openairesponses.TextDeltaEvent:
		return *probe, nil
	case *openairesponses.TextDoneEvent:
		return *probe, nil
	case *openairesponses.ContentPartDoneEvent:
		return *probe, nil
	case *openairesponses.FunctionCallArgumentsDeltaEvent:
		return *probe, nil
	case *openairesponses.FunctionCallArgumentsDoneEvent:
		return *probe, nil
	case *openairesponses.RefusalDeltaEvent:
		return *probe, nil
	case *openairesponses.RefusalDoneEvent:
		return *probe, nil
	case *openairesponses.CompletedEvent:
		return *probe, nil
	case *openairesponses.IncompleteEvent:
		return *probe, nil
	case *openairesponses.FailedEvent:
		return *probe, nil
	case *openairesponses.ErrorEvent:
		return *probe, nil
	default:
		panic("unreachable: validated event type")
	}
}

// isResponsesDoneSentinel reports whether the frame is the Responses [DONE]
// sentinel.
func isResponsesDoneSentinel(data []byte) bool {
	return string(data) == "[DONE]"
}

// ConversionReport returns the accumulated approved losses of the chat
// state.
func (c *chatToResponsesConverter) ConversionReport() *ConversionReport {
	return &c.state.report
}

// ErrorEvent builds a client-dialect Responses error event frame. The
// sequence number continues from the builder so a mid-stream error terminal
// never collides with response.created's zero.
func (c *chatToResponsesConverter) ErrorEvent(err error) (frameEvent, bool) {
	return responsesErrorFrame(err, c.state.builder.NextSequenceNumber())
}

// ConversionReport returns the accumulated approved losses of the
// Anthropic state.
func (c *responsesToAnthropicConverter) ConversionReport() *ConversionReport {
	return &c.state.report
}

// ErrorEvent builds a client-dialect Anthropic error event frame.
func (c *responsesToAnthropicConverter) ErrorEvent(err error) (frameEvent, bool) {
	return anthropicErrorFrame(err)
}

// ConversionReport returns the merged approved losses of both states of the
// composed conversion.
func (c *chatToAnthropicConverter) ConversionReport() *ConversionReport {
	merged := ConversionReport{}
	merged.Losses = append(merged.Losses, c.chat.report.Losses...)
	merged.Losses = append(merged.Losses, c.anthropic.report.Losses...)
	return &merged
}

// ErrorEvent builds a client-dialect Anthropic error event frame (the
// composed stream's client is always Messages).
func (c *chatToAnthropicConverter) ErrorEvent(err error) (frameEvent, bool) {
	return anthropicErrorFrame(err)
}

// boundedErrorMessage bounds the error text embedded in a client-dialect
// error frame: a conversion error may carry an entire corrupt upstream
// payload (e.g. an invalid tool-argument buffer), which must not amplify the
// downstream frame without bound (review-08 blocker 7).
func boundedErrorMessage(err error) string {
	return boundedErrorMessageLimit(err, maxStreamErrorTextBytes)
}

// boundedErrorMessageLimit bounds the error text embedded in a client-dialect
// error frame to max bytes: a conversion error may carry an entire corrupt
// upstream payload (e.g. an invalid tool-argument buffer), which must not
// amplify the downstream frame without bound (review-08 blocker 7; the
// configured ErrorMessageBytes is wired at the call sites, review-z commit
// 3).
func boundedErrorMessageLimit(err error, max int) string {
	message := err.Error()
	if len(message) <= max {
		return message
	}
	// The ellipsis must not push the text past the configured bound.
	if max > 3 {
		return message[:max-3] + "…"
	}
	return message[:max]
}

// responsesErrorFrame marshals a Responses error event.
func responsesErrorFrame(err error, sequence int64) (frameEvent, bool) {
	event := ResponseErrorEvent{
		Type:           "error",
		SequenceNumber: sequence,
		Code:           "conversion_error",
		Message:        boundedErrorMessage(err),
		Param:          "",
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
			Message: boundedErrorMessage(err),
		},
	}
	data, marshalErr := json.Marshal(event)
	if marshalErr != nil {
		return frameEvent{}, false
	}
	return frameEvent{Type: "error", Data: data}, true
}
