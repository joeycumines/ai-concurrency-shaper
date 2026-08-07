package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Event contracts:
//
// Complete Responses stream union:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L10042-L10120
//
// Function arguments delta / done:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L3609-L3669
//
// Reasoning summary part added / done:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L9755-L9862
//
// Content part added / done:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L2703-L2913
//
// Output text delta / done:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L10781-L10945
//
// Refusal delta / done:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L9929-L9995
//
// Error event:
// https://github.com/openai/openai-go/blob/main/responses/response.go#L2991-L3020

// ResponsesSSEEvent is one event-specific Responses stream event. The JSON
// type tag always equals the SSE event name.
type ResponsesSSEEvent interface {
	EventType() string
	Validate() error
}

// responsesEventBase is embedded by every event. SequenceNumber is required
// and always emitted, including zero.
type responsesEventBase struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number"`
}

func (b responsesEventBase) validate(want string) error {
	if b.Type != want {
		return fmt.Errorf("event type = %q, want %q", b.Type, want)
	}
	if b.SequenceNumber < 0 {
		return fmt.Errorf("negative sequence number %d", b.SequenceNumber)
	}
	return nil
}

// ResponseCreatedEvent is emitted when the response is created.
type ResponseCreatedEvent struct {
	responsesEventBase
	Response ResponseEnvelope `json:"response"`
}

func (e ResponseCreatedEvent) EventType() string { return e.Type }
func (e ResponseCreatedEvent) Validate() error {
	if err := e.responsesEventBase.validate("response.created"); err != nil {
		return err
	}
	return e.Response.Validate()
}

// ResponseInProgressEvent is emitted when response generation starts.
type ResponseInProgressEvent struct {
	responsesEventBase
	Response ResponseEnvelope `json:"response"`
}

func (e ResponseInProgressEvent) EventType() string { return e.Type }
func (e ResponseInProgressEvent) Validate() error {
	if err := e.responsesEventBase.validate("response.in_progress"); err != nil {
		return err
	}
	return e.Response.Validate()
}

// ResponseOutputItemAddedEvent is emitted when an output item is added.
type ResponseOutputItemAddedEvent struct {
	responsesEventBase
	OutputIndex int64               `json:"output_index"`
	Item        ResponsesOutputItem `json:"item"`
}

func (e ResponseOutputItemAddedEvent) EventType() string { return e.Type }
func (e ResponseOutputItemAddedEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.output_item.added",
	); err != nil {
		return err
	}
	if e.OutputIndex < 0 {
		return errors.New("negative output index")
	}
	if e.Item == nil {
		return errors.New("output_item.added item is nil")
	}
	return e.Item.Validate()
}

// UnmarshalJSON decodes the event, dispatching the item through the tagged
// output item union.
func (e *ResponseOutputItemAddedEvent) UnmarshalJSON(data []byte) error {
	var shadow struct {
		Type           string          `json:"type"`
		SequenceNumber int64           `json:"sequence_number"`
		OutputIndex    int64           `json:"output_index"`
		Item           json.RawMessage `json:"item"`
	}
	if err := strictDecode(data, &shadow); err != nil {
		return err
	}
	item, err := DecodeResponsesOutputItem(shadow.Item)
	if err != nil {
		return fmt.Errorf("output_item.added: %w", err)
	}
	*e = ResponseOutputItemAddedEvent{
		responsesEventBase: responsesEventBase{
			Type:           shadow.Type,
			SequenceNumber: shadow.SequenceNumber,
		},
		OutputIndex: shadow.OutputIndex,
		Item:        item,
	}
	return e.Validate()
}

// ResponseOutputItemDoneEvent is emitted when an output item is completed.
type ResponseOutputItemDoneEvent struct {
	responsesEventBase
	OutputIndex int64               `json:"output_index"`
	Item        ResponsesOutputItem `json:"item"`
}

func (e ResponseOutputItemDoneEvent) EventType() string { return e.Type }
func (e ResponseOutputItemDoneEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.output_item.done",
	); err != nil {
		return err
	}
	if e.OutputIndex < 0 {
		return errors.New("negative output index")
	}
	if e.Item == nil {
		return errors.New("output_item.done item is nil")
	}
	return e.Item.Validate()
}

// UnmarshalJSON decodes the event, dispatching the item through the tagged
// output item union.
func (e *ResponseOutputItemDoneEvent) UnmarshalJSON(data []byte) error {
	var shadow struct {
		Type           string          `json:"type"`
		SequenceNumber int64           `json:"sequence_number"`
		OutputIndex    int64           `json:"output_index"`
		Item           json.RawMessage `json:"item"`
	}
	if err := strictDecode(data, &shadow); err != nil {
		return err
	}
	item, err := DecodeResponsesOutputItem(shadow.Item)
	if err != nil {
		return fmt.Errorf("output_item.done: %w", err)
	}
	*e = ResponseOutputItemDoneEvent{
		responsesEventBase: responsesEventBase{
			Type:           shadow.Type,
			SequenceNumber: shadow.SequenceNumber,
		},
		OutputIndex: shadow.OutputIndex,
		Item:        item,
	}
	return e.Validate()
}

// ResponsesSummaryTextPart is the summary part payload of reasoning summary
// events.
type ResponsesSummaryTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponseReasoningSummaryPartAddedEvent struct {
	responsesEventBase
	ItemID       string                   `json:"item_id"`
	OutputIndex  int64                    `json:"output_index"`
	SummaryIndex int64                    `json:"summary_index"`
	Part         ResponsesSummaryTextPart `json:"part"`
}

func (e ResponseReasoningSummaryPartAddedEvent) EventType() string {
	return e.Type
}
func (e ResponseReasoningSummaryPartAddedEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.reasoning_summary_part.added",
	); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("reasoning summary item_id is empty")
	}
	if e.OutputIndex < 0 || e.SummaryIndex < 0 {
		return errors.New("negative reasoning summary index")
	}
	if e.Part.Type != "summary_text" {
		return fmt.Errorf("reasoning summary part type = %q", e.Part.Type)
	}
	return nil
}

// ResponseReasoningSummaryTextDeltaEvent is emitted when reasoning summary
// text is added.
type ResponseReasoningSummaryTextDeltaEvent struct {
	responsesEventBase
	ItemID       string `json:"item_id"`
	OutputIndex  int64  `json:"output_index"`
	SummaryIndex int64  `json:"summary_index"`
	Delta        string `json:"delta"`
}

func (e ResponseReasoningSummaryTextDeltaEvent) EventType() string {
	return e.Type
}
func (e ResponseReasoningSummaryTextDeltaEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.reasoning_summary_text.delta",
	); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("reasoning summary delta item_id is empty")
	}
	return nil
}

// ResponseReasoningSummaryTextDoneEvent is emitted when reasoning summary
// text is finalized.
type ResponseReasoningSummaryTextDoneEvent struct {
	responsesEventBase
	ItemID       string `json:"item_id"`
	OutputIndex  int64  `json:"output_index"`
	SummaryIndex int64  `json:"summary_index"`
	Text         string `json:"text"`
}

func (e ResponseReasoningSummaryTextDoneEvent) EventType() string {
	return e.Type
}
func (e ResponseReasoningSummaryTextDoneEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.reasoning_summary_text.done",
	); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("reasoning summary done item_id is empty")
	}
	return nil
}

// ResponseReasoningSummaryPartDoneEvent is emitted when a reasoning summary
// part is completed.
type ResponseReasoningSummaryPartDoneEvent struct {
	responsesEventBase
	ItemID       string                   `json:"item_id"`
	OutputIndex  int64                    `json:"output_index"`
	SummaryIndex int64                    `json:"summary_index"`
	Part         ResponsesSummaryTextPart `json:"part"`
	Status       string                   `json:"status,omitempty"`
}

func (e ResponseReasoningSummaryPartDoneEvent) EventType() string {
	return e.Type
}
func (e ResponseReasoningSummaryPartDoneEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.reasoning_summary_part.done",
	); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("reasoning summary part done item_id is empty")
	}
	if e.Part.Type != "summary_text" {
		return fmt.Errorf("reasoning summary done part type = %q", e.Part.Type)
	}
	if e.Status != "" && e.Status != "incomplete" {
		return fmt.Errorf("invalid reasoning summary part status %q", e.Status)
	}
	return nil
}

// ResponsesStreamContentPart is the part payload of content part events.
type ResponsesStreamContentPart interface {
	isResponsesStreamContentPart()
	Validate() error
}

// ResponsesStreamOutputTextPart is an output_text part in a stream event.
type ResponsesStreamOutputTextPart struct {
	Type        string                 `json:"type"`
	Text        string                 `json:"text"`
	Annotations []ResponsesAnnotation  `json:"annotations"`
	Logprobs    []ResponsesTextLogprob `json:"logprobs,omitempty"`
}

func (*ResponsesStreamOutputTextPart) isResponsesStreamContentPart() {}
func (p *ResponsesStreamOutputTextPart) Validate() error {
	if p.Type != "output_text" {
		return fmt.Errorf("stream output text type = %q", p.Type)
	}
	if p.Annotations == nil {
		return errors.New("stream output_text annotations must be an array")
	}
	return nil
}

// ResponsesStreamRefusalPart is a refusal part in a stream event.
type ResponsesStreamRefusalPart struct {
	Type    string `json:"type"`
	Refusal string `json:"refusal"`
}

func (*ResponsesStreamRefusalPart) isResponsesStreamContentPart() {}
func (p *ResponsesStreamRefusalPart) Validate() error {
	if p.Type != "refusal" {
		return fmt.Errorf("stream refusal part type = %q", p.Type)
	}
	return nil
}

// ResponseContentPartAddedEvent is emitted when a content part is added.
type ResponseContentPartAddedEvent struct {
	responsesEventBase
	ItemID       string                     `json:"item_id"`
	OutputIndex  int64                      `json:"output_index"`
	ContentIndex int64                      `json:"content_index"`
	Part         ResponsesStreamContentPart `json:"part"`
}

func (e ResponseContentPartAddedEvent) EventType() string { return e.Type }
func (e ResponseContentPartAddedEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.content_part.added",
	); err != nil {
		return err
	}
	if e.ItemID == "" || e.Part == nil {
		return errors.New("content_part.added requires item_id and part")
	}
	return e.Part.Validate()
}

// UnmarshalJSON decodes the event, dispatching the part through the stream
// content part union.
func (e *ResponseContentPartAddedEvent) UnmarshalJSON(data []byte) error {
	var shadow struct {
		Type           string          `json:"type"`
		SequenceNumber int64           `json:"sequence_number"`
		ItemID         string          `json:"item_id"`
		OutputIndex    int64           `json:"output_index"`
		ContentIndex   int64           `json:"content_index"`
		Part           json.RawMessage `json:"part"`
	}
	if err := strictDecode(data, &shadow); err != nil {
		return err
	}
	part, err := decodeResponsesStreamContentPart(shadow.Part)
	if err != nil {
		return fmt.Errorf("content_part.added: %w", err)
	}
	*e = ResponseContentPartAddedEvent{
		responsesEventBase: responsesEventBase{
			Type:           shadow.Type,
			SequenceNumber: shadow.SequenceNumber,
		},
		ItemID:       shadow.ItemID,
		OutputIndex:  shadow.OutputIndex,
		ContentIndex: shadow.ContentIndex,
		Part:         part,
	}
	return e.Validate()
}

// ResponseTextDeltaEvent is emitted when output text is added.
type ResponseTextDeltaEvent struct {
	responsesEventBase
	ItemID       string                 `json:"item_id"`
	OutputIndex  int64                  `json:"output_index"`
	ContentIndex int64                  `json:"content_index"`
	Delta        string                 `json:"delta"`
	Logprobs     []ResponsesTextLogprob `json:"logprobs"`
}

func (e ResponseTextDeltaEvent) EventType() string { return e.Type }
func (e ResponseTextDeltaEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.output_text.delta",
	); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("output_text.delta item_id is empty")
	}
	if e.Logprobs == nil {
		return errors.New("output_text.delta logprobs must be an array")
	}
	return nil
}

// ResponseTextDoneEvent is emitted when output text is finalized.
type ResponseTextDoneEvent struct {
	responsesEventBase
	ItemID       string                 `json:"item_id"`
	OutputIndex  int64                  `json:"output_index"`
	ContentIndex int64                  `json:"content_index"`
	Text         string                 `json:"text"`
	Logprobs     []ResponsesTextLogprob `json:"logprobs"`
}

func (e ResponseTextDoneEvent) EventType() string { return e.Type }
func (e ResponseTextDoneEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.output_text.done",
	); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("output_text.done item_id is empty")
	}
	if e.Logprobs == nil {
		return errors.New("output_text.done logprobs must be an array")
	}
	return nil
}

// ResponseContentPartDoneEvent is emitted when a content part is completed.
type ResponseContentPartDoneEvent struct {
	responsesEventBase
	ItemID       string                     `json:"item_id"`
	OutputIndex  int64                      `json:"output_index"`
	ContentIndex int64                      `json:"content_index"`
	Part         ResponsesStreamContentPart `json:"part"`
}

func (e ResponseContentPartDoneEvent) EventType() string { return e.Type }
func (e ResponseContentPartDoneEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.content_part.done",
	); err != nil {
		return err
	}
	if e.ItemID == "" || e.Part == nil {
		return errors.New("content_part.done requires item_id and part")
	}
	return e.Part.Validate()
}

// UnmarshalJSON decodes the event, dispatching the part through the stream
// content part union.
func (e *ResponseContentPartDoneEvent) UnmarshalJSON(data []byte) error {
	var shadow struct {
		Type           string          `json:"type"`
		SequenceNumber int64           `json:"sequence_number"`
		ItemID         string          `json:"item_id"`
		OutputIndex    int64           `json:"output_index"`
		ContentIndex   int64           `json:"content_index"`
		Part           json.RawMessage `json:"part"`
	}
	if err := strictDecode(data, &shadow); err != nil {
		return err
	}
	part, err := decodeResponsesStreamContentPart(shadow.Part)
	if err != nil {
		return fmt.Errorf("content_part.done: %w", err)
	}
	*e = ResponseContentPartDoneEvent{
		responsesEventBase: responsesEventBase{
			Type:           shadow.Type,
			SequenceNumber: shadow.SequenceNumber,
		},
		ItemID:       shadow.ItemID,
		OutputIndex:  shadow.OutputIndex,
		ContentIndex: shadow.ContentIndex,
		Part:         part,
	}
	return e.Validate()
}

// decodeResponsesStreamContentPart dispatches the part through the stream
// content part union.
func decodeResponsesStreamContentPart(
	data []byte,
) (ResponsesStreamContentPart, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	var part ResponsesStreamContentPart
	switch probe.Type {
	case "output_text":
		part = &ResponsesStreamOutputTextPart{}
	case "refusal":
		part = &ResponsesStreamRefusalPart{}
	default:
		return nil, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     "stream[].part.type",
			Feature:  probe.Type,
		}
	}
	if err := strictDecode(data, part); err != nil {
		return nil, err
	}
	if err := part.Validate(); err != nil {
		return nil, err
	}
	return part, nil
}

// ResponseFunctionCallArgumentsDeltaEvent carries a partial function-call
// arguments delta. The official field is delta, never arguments.
type ResponseFunctionCallArgumentsDeltaEvent struct {
	responsesEventBase
	ItemID      string `json:"item_id"`
	OutputIndex int64  `json:"output_index"`
	Delta       string `json:"delta"`
}

func (e ResponseFunctionCallArgumentsDeltaEvent) EventType() string {
	return e.Type
}
func (e ResponseFunctionCallArgumentsDeltaEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.function_call_arguments.delta",
	); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("function arguments delta item_id is empty")
	}
	return nil
}

// ResponseFunctionCallArgumentsDoneEvent carries the finalized arguments of a
// function call, together with the function name. The emitted name is a
// superset over the current official event (which requires arguments but not
// name); official clients ignore unknown fields and the review mandates name
// on completion so call identity never depends on a later event.
type ResponseFunctionCallArgumentsDoneEvent struct {
	responsesEventBase
	ItemID      string `json:"item_id"`
	OutputIndex int64  `json:"output_index"`
	Name        string `json:"name"`
	Arguments   string `json:"arguments"`
}

func (e ResponseFunctionCallArgumentsDoneEvent) EventType() string {
	return e.Type
}
func (e ResponseFunctionCallArgumentsDoneEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.function_call_arguments.done",
	); err != nil {
		return err
	}
	if e.ItemID == "" || e.Name == "" {
		return errors.New("function arguments done requires item_id and name")
	}
	if !json.Valid([]byte(e.Arguments)) {
		return errors.New("final function arguments are invalid JSON")
	}
	return nil
}

// ResponseRefusalDeltaEvent carries a partial refusal delta.
type ResponseRefusalDeltaEvent struct {
	responsesEventBase
	ItemID       string `json:"item_id"`
	OutputIndex  int64  `json:"output_index"`
	ContentIndex int64  `json:"content_index"`
	Delta        string `json:"delta"`
}

func (e ResponseRefusalDeltaEvent) EventType() string { return e.Type }
func (e ResponseRefusalDeltaEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.refusal.delta",
	); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("refusal delta item_id is empty")
	}
	return nil
}

// ResponseRefusalDoneEvent carries the finalized refusal text. This event
// exists in the official stream and must be emitted when a refusal completes.
type ResponseRefusalDoneEvent struct {
	responsesEventBase
	ItemID       string `json:"item_id"`
	OutputIndex  int64  `json:"output_index"`
	ContentIndex int64  `json:"content_index"`
	Refusal      string `json:"refusal"`
}

func (e ResponseRefusalDoneEvent) EventType() string { return e.Type }
func (e ResponseRefusalDoneEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.refusal.done",
	); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("refusal done item_id is empty")
	}
	return nil
}

// ResponseCompletedEvent is the success terminal event.
type ResponseCompletedEvent struct {
	responsesEventBase
	Response ResponseEnvelope `json:"response"`
}

func (e ResponseCompletedEvent) EventType() string { return e.Type }
func (e ResponseCompletedEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.completed",
	); err != nil {
		return err
	}
	if e.Response.Status != "completed" {
		return fmt.Errorf("completed response status = %q", e.Response.Status)
	}
	return e.Response.Validate()
}

// ResponseIncompleteEvent is the incomplete terminal event.
type ResponseIncompleteEvent struct {
	responsesEventBase
	Response ResponseEnvelope `json:"response"`
}

func (e ResponseIncompleteEvent) EventType() string { return e.Type }
func (e ResponseIncompleteEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.incomplete",
	); err != nil {
		return err
	}
	if e.Response.Status != "incomplete" {
		return fmt.Errorf("incomplete response status = %q", e.Response.Status)
	}
	return e.Response.Validate()
}

// ResponseFailedEvent is the failure terminal event. It must never be rendered
// as a successful completion in any target dialect.
type ResponseFailedEvent struct {
	responsesEventBase
	Response ResponseEnvelope `json:"response"`
}

func (e ResponseFailedEvent) EventType() string { return e.Type }
func (e ResponseFailedEvent) Validate() error {
	if err := e.responsesEventBase.validate(
		"response.failed",
	); err != nil {
		return err
	}
	if e.Response.Status != "failed" {
		return fmt.Errorf("failed response status = %q", e.Response.Status)
	}
	return e.Response.Validate()
}

// ResponseErrorEvent is the error event. Code, message, and param are all
// required at the top level.
type ResponseErrorEvent struct {
	responsesEventBase
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param"`
}

func (e ResponseErrorEvent) EventType() string { return e.Type }
func (e ResponseErrorEvent) Validate() error {
	if err := e.responsesEventBase.validate("error"); err != nil {
		return err
	}
	if e.Code == "" || e.Message == "" {
		return errors.New("error event requires code and message")
	}
	return nil
}

// ResponsesEventBuilder allocates sequence numbers. Each event gets exactly
// one sequence number, allocated at construction.
type ResponsesEventBuilder struct {
	nextSequence int64
}

func (b *ResponsesEventBuilder) base(eventType string) responsesEventBase {
	base := responsesEventBase{
		Type:           eventType,
		SequenceNumber: b.nextSequence,
	}
	b.nextSequence++
	return base
}

// NextSequenceNumber returns the sequence number the next event will
// receive. Error frames generated outside the builder (stream abort paths)
// use it so the error terminal keeps the stream's sequence monotonic.
func (b *ResponsesEventBuilder) NextSequenceNumber() int64 {
	return b.nextSequence
}

// Created builds a response.created event.
func (b *ResponsesEventBuilder) Created(
	response ResponseEnvelope,
) ResponseCreatedEvent {
	return ResponseCreatedEvent{
		responsesEventBase: b.base("response.created"),
		Response:           response,
	}
}

// InProgress builds a response.in_progress event.
func (b *ResponsesEventBuilder) InProgress(
	response ResponseEnvelope,
) ResponseInProgressEvent {
	return ResponseInProgressEvent{
		responsesEventBase: b.base("response.in_progress"),
		Response:           response,
	}
}

// OutputItemAdded builds a response.output_item.added event.
func (b *ResponsesEventBuilder) OutputItemAdded(
	outputIndex int64,
	item ResponsesOutputItem,
) ResponseOutputItemAddedEvent {
	return ResponseOutputItemAddedEvent{
		responsesEventBase: b.base("response.output_item.added"),
		OutputIndex:        outputIndex,
		Item:               item,
	}
}

// OutputItemDone builds a response.output_item.done event.
func (b *ResponsesEventBuilder) OutputItemDone(
	outputIndex int64,
	item ResponsesOutputItem,
) ResponseOutputItemDoneEvent {
	return ResponseOutputItemDoneEvent{
		responsesEventBase: b.base("response.output_item.done"),
		OutputIndex:        outputIndex,
		Item:               item,
	}
}

// ReasoningSummaryPartAdded builds a reasoning summary part added event.
func (b *ResponsesEventBuilder) ReasoningSummaryPartAdded(
	itemID string,
	outputIndex int64,
	summaryIndex int64,
	part ResponsesSummaryTextPart,
) ResponseReasoningSummaryPartAddedEvent {
	return ResponseReasoningSummaryPartAddedEvent{
		responsesEventBase: b.base("response.reasoning_summary_part.added"),
		ItemID:             itemID,
		OutputIndex:        outputIndex,
		SummaryIndex:       summaryIndex,
		Part:               part,
	}
}

// ReasoningSummaryTextDelta builds a reasoning summary text delta event.
func (b *ResponsesEventBuilder) ReasoningSummaryTextDelta(
	itemID string,
	outputIndex int64,
	summaryIndex int64,
	delta string,
) ResponseReasoningSummaryTextDeltaEvent {
	return ResponseReasoningSummaryTextDeltaEvent{
		responsesEventBase: b.base("response.reasoning_summary_text.delta"),
		ItemID:             itemID,
		OutputIndex:        outputIndex,
		SummaryIndex:       summaryIndex,
		Delta:              delta,
	}
}

// ReasoningSummaryTextDone builds a reasoning summary text done event.
func (b *ResponsesEventBuilder) ReasoningSummaryTextDone(
	itemID string,
	outputIndex int64,
	summaryIndex int64,
	text string,
) ResponseReasoningSummaryTextDoneEvent {
	return ResponseReasoningSummaryTextDoneEvent{
		responsesEventBase: b.base("response.reasoning_summary_text.done"),
		ItemID:             itemID,
		OutputIndex:        outputIndex,
		SummaryIndex:       summaryIndex,
		Text:               text,
	}
}

// ReasoningSummaryPartDone builds a reasoning summary part done event.
func (b *ResponsesEventBuilder) ReasoningSummaryPartDone(
	itemID string,
	outputIndex int64,
	summaryIndex int64,
	part ResponsesSummaryTextPart,
) ResponseReasoningSummaryPartDoneEvent {
	return ResponseReasoningSummaryPartDoneEvent{
		responsesEventBase: b.base("response.reasoning_summary_part.done"),
		ItemID:             itemID,
		OutputIndex:        outputIndex,
		SummaryIndex:       summaryIndex,
		Part:               part,
	}
}

// ContentPartAdded builds a response.content_part.added event.
func (b *ResponsesEventBuilder) ContentPartAdded(
	itemID string,
	outputIndex int64,
	contentIndex int64,
	part ResponsesStreamContentPart,
) ResponseContentPartAddedEvent {
	return ResponseContentPartAddedEvent{
		responsesEventBase: b.base("response.content_part.added"),
		ItemID:             itemID,
		OutputIndex:        outputIndex,
		ContentIndex:       contentIndex,
		Part:               part,
	}
}

// TextDelta builds a response.output_text.delta event with an empty non-nil
// logprobs array, as required by the wire contract.
func (b *ResponsesEventBuilder) TextDelta(
	itemID string,
	outputIndex int64,
	contentIndex int64,
	delta string,
) ResponseTextDeltaEvent {
	return ResponseTextDeltaEvent{
		responsesEventBase: b.base("response.output_text.delta"),
		ItemID:             itemID,
		OutputIndex:        outputIndex,
		ContentIndex:       contentIndex,
		Delta:              delta,
		Logprobs:           make([]ResponsesTextLogprob, 0),
	}
}

// TextDone builds a response.output_text.done event with an empty non-nil
// logprobs array.
func (b *ResponsesEventBuilder) TextDone(
	itemID string,
	outputIndex int64,
	contentIndex int64,
	text string,
) ResponseTextDoneEvent {
	return ResponseTextDoneEvent{
		responsesEventBase: b.base("response.output_text.done"),
		ItemID:             itemID,
		OutputIndex:        outputIndex,
		ContentIndex:       contentIndex,
		Text:               text,
		Logprobs:           make([]ResponsesTextLogprob, 0),
	}
}

// ContentPartDone builds a response.content_part.done event.
func (b *ResponsesEventBuilder) ContentPartDone(
	itemID string,
	outputIndex int64,
	contentIndex int64,
	part ResponsesStreamContentPart,
) ResponseContentPartDoneEvent {
	return ResponseContentPartDoneEvent{
		responsesEventBase: b.base("response.content_part.done"),
		ItemID:             itemID,
		OutputIndex:        outputIndex,
		ContentIndex:       contentIndex,
		Part:               part,
	}
}

// FunctionArgumentsDelta builds a response.function_call_arguments.delta
// event. The partial payload is carried by delta.
func (b *ResponsesEventBuilder) FunctionArgumentsDelta(
	itemID string,
	outputIndex int64,
	delta string,
) ResponseFunctionCallArgumentsDeltaEvent {
	return ResponseFunctionCallArgumentsDeltaEvent{
		responsesEventBase: b.base(
			"response.function_call_arguments.delta",
		),
		ItemID:      itemID,
		OutputIndex: outputIndex,
		Delta:       delta,
	}
}

// FunctionArgumentsDone builds a response.function_call_arguments.done event
// carrying the name and the finalized arguments (never a partial payload).
func (b *ResponsesEventBuilder) FunctionArgumentsDone(
	itemID string,
	outputIndex int64,
	name string,
	arguments string,
) ResponseFunctionCallArgumentsDoneEvent {
	return ResponseFunctionCallArgumentsDoneEvent{
		responsesEventBase: b.base(
			"response.function_call_arguments.done",
		),
		ItemID:      itemID,
		OutputIndex: outputIndex,
		Name:        name,
		Arguments:   arguments,
	}
}

// RefusalDelta builds a response.refusal.delta event.
func (b *ResponsesEventBuilder) RefusalDelta(
	itemID string,
	outputIndex int64,
	contentIndex int64,
	delta string,
) ResponseRefusalDeltaEvent {
	return ResponseRefusalDeltaEvent{
		responsesEventBase: b.base("response.refusal.delta"),
		ItemID:             itemID,
		OutputIndex:        outputIndex,
		ContentIndex:       contentIndex,
		Delta:              delta,
	}
}

// RefusalDone builds a response.refusal.done event.
func (b *ResponsesEventBuilder) RefusalDone(
	itemID string,
	outputIndex int64,
	contentIndex int64,
	refusal string,
) ResponseRefusalDoneEvent {
	return ResponseRefusalDoneEvent{
		responsesEventBase: b.base("response.refusal.done"),
		ItemID:             itemID,
		OutputIndex:        outputIndex,
		ContentIndex:       contentIndex,
		Refusal:            refusal,
	}
}

// Completed builds a response.completed event.
func (b *ResponsesEventBuilder) Completed(
	response ResponseEnvelope,
) ResponseCompletedEvent {
	return ResponseCompletedEvent{
		responsesEventBase: b.base("response.completed"),
		Response:           response,
	}
}

// Incomplete builds a response.incomplete event.
func (b *ResponsesEventBuilder) Incomplete(
	response ResponseEnvelope,
) ResponseIncompleteEvent {
	return ResponseIncompleteEvent{
		responsesEventBase: b.base("response.incomplete"),
		Response:           response,
	}
}

// Failed builds a response.failed event.
func (b *ResponsesEventBuilder) Failed(
	response ResponseEnvelope,
) ResponseFailedEvent {
	return ResponseFailedEvent{
		responsesEventBase: b.base("response.failed"),
		Response:           response,
	}
}

// Error builds an error event.
func (b *ResponsesEventBuilder) Error(
	code string,
	message string,
	param string,
) ResponseErrorEvent {
	return ResponseErrorEvent{
		responsesEventBase: b.base("error"),
		Code:               code,
		Message:            message,
		Param:              param,
	}
}

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
