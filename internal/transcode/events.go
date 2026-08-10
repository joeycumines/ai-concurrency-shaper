package transcode

// The OpenAI Responses wire definitions live in the pinned wire package
// wire/openairesponses (see contracts.lock.json); this file re-exports them
// under the package's historical names so consumers compile unchanged. New
// code should prefer the wire package names.

import (
	"encoding/json"
	"errors"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// ResponsesSSEEvent is one event-specific Responses stream event. The JSON
// type tag always equals the SSE event name.
type ResponsesSSEEvent = openairesponses.Event

// EventBase is the embedded base of every Responses stream event: the type
// tag and the required sequence number.
type EventBase = openairesponses.EventBase

// ResponseCreatedEvent is emitted when the response is created.
type ResponseCreatedEvent = openairesponses.CreatedEvent

// ResponseInProgressEvent is emitted when response generation starts.
type ResponseInProgressEvent = openairesponses.InProgressEvent

// ResponseOutputItemAddedEvent is emitted when an output item is added.
type ResponseOutputItemAddedEvent = openairesponses.OutputItemAddedEvent

// ResponseOutputItemDoneEvent is emitted when an output item is completed.
type ResponseOutputItemDoneEvent = openairesponses.OutputItemDoneEvent

// ResponsesSummaryTextPart is the summary part payload of reasoning summary
// events.
type ResponsesSummaryTextPart = openairesponses.SummaryTextPart

// ResponseReasoningSummaryPartAddedEvent is emitted when a reasoning summary
// part is added.
type ResponseReasoningSummaryPartAddedEvent = openairesponses.ReasoningSummaryPartAddedEvent

// ResponseReasoningSummaryTextDeltaEvent is emitted when reasoning summary
// text is added.
type ResponseReasoningSummaryTextDeltaEvent = openairesponses.ReasoningSummaryTextDeltaEvent

// ResponseReasoningSummaryTextDoneEvent is emitted when reasoning summary
// text is finalized.
type ResponseReasoningSummaryTextDoneEvent = openairesponses.ReasoningSummaryTextDoneEvent

// ResponseReasoningSummaryPartDoneEvent is emitted when a reasoning summary
// part is completed.
type ResponseReasoningSummaryPartDoneEvent = openairesponses.ReasoningSummaryPartDoneEvent

// ResponsesStreamContentPart is the part payload of content part events.
type ResponsesStreamContentPart = openairesponses.StreamContentPart

// ResponsesStreamOutputTextPart is an output_text part in a stream event.
type ResponsesStreamOutputTextPart = openairesponses.StreamOutputTextPart

// ResponsesStreamRefusalPart is a refusal part in a stream event.
type ResponsesStreamRefusalPart = openairesponses.StreamRefusalPart

// ResponseContentPartAddedEvent is emitted when a content part is added.
type ResponseContentPartAddedEvent = openairesponses.ContentPartAddedEvent

// ResponseTextDeltaEvent is emitted when output text is added.
type ResponseTextDeltaEvent = openairesponses.TextDeltaEvent

// ResponseTextDoneEvent is emitted when output text is finalized.
type ResponseTextDoneEvent = openairesponses.TextDoneEvent

// ResponseContentPartDoneEvent is emitted when a content part is completed.
type ResponseContentPartDoneEvent = openairesponses.ContentPartDoneEvent

// ResponseFunctionCallArgumentsDeltaEvent carries a partial function-call
// arguments delta.
type ResponseFunctionCallArgumentsDeltaEvent = openairesponses.FunctionCallArgumentsDeltaEvent

// ResponseFunctionCallArgumentsDoneEvent carries the finalized arguments of
// a function call.
type ResponseFunctionCallArgumentsDoneEvent = openairesponses.FunctionCallArgumentsDoneEvent

// ResponseRefusalDeltaEvent carries a partial refusal delta.
type ResponseRefusalDeltaEvent = openairesponses.RefusalDeltaEvent

// ResponseRefusalDoneEvent carries the finalized refusal text.
type ResponseRefusalDoneEvent = openairesponses.RefusalDoneEvent

// ResponseCompletedEvent is the success terminal event.
type ResponseCompletedEvent = openairesponses.CompletedEvent

// ResponseIncompleteEvent is the incomplete terminal event.
type ResponseIncompleteEvent = openairesponses.IncompleteEvent

// ResponseFailedEvent is the failure terminal event.
type ResponseFailedEvent = openairesponses.FailedEvent

// ResponseErrorEvent is the error event. Code, message, and param are all
// required at the top level.
type ResponseErrorEvent = openairesponses.ErrorEvent

// ResponsesEventBuilder allocates sequence numbers. Each event gets exactly
// one sequence number, allocated at construction.
type ResponsesEventBuilder = openairesponses.EventBuilder

// MarshalResponsesEvent validates and marshals one event.
func MarshalResponsesEvent(event ResponsesSSEEvent) ([]byte, error) {
	if event == nil {
		return nil, errors.New("nil Responses event")
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(event)
}
