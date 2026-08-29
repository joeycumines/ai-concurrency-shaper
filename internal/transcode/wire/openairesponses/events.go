package openairesponses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// Event is one event-specific Responses stream event. The JSON type tag
// always equals the SSE event name.
type Event interface {
	EventType() string
	Validate() error

	// Sequence returns the event's wire sequence number. The stream state
	// machines enforce strictly increasing, unique sequence numbers across
	// the stream.
	Sequence() int64
}

// EventBase is embedded by every event. SequenceNumber is required and
// always emitted, including zero.
type EventBase struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number"`
}

// Sequence returns the wire sequence number of the event.
func (b EventBase) Sequence() int64 {
	return b.SequenceNumber
}

func (b EventBase) validate(want string) error {
	if b.Type != want {
		return fmt.Errorf("event type = %q, want %q", b.Type, want)
	}
	if b.SequenceNumber < 0 {
		return fmt.Errorf("negative sequence number %d", b.SequenceNumber)
	}
	return nil
}

// CreatedEvent is emitted when the response is created.
type CreatedEvent struct {
	EventBase
	Response Response `json:"response"`
}

func (e CreatedEvent) EventType() string { return e.Type }
func (e CreatedEvent) Validate() error {
	if err := e.EventBase.validate("response.created"); err != nil {
		return err
	}
	return e.Response.Validate()
}

// InProgressEvent is emitted when response generation starts.
type InProgressEvent struct {
	EventBase
	Response Response `json:"response"`
}

func (e InProgressEvent) EventType() string { return e.Type }
func (e InProgressEvent) Validate() error {
	if err := e.EventBase.validate("response.in_progress"); err != nil {
		return err
	}
	return e.Response.Validate()
}

// OutputItemAddedEvent is emitted when an output item is added.
type OutputItemAddedEvent struct {
	EventBase
	OutputIndex int64      `json:"output_index"`
	Item        OutputItem `json:"item"`
}

func (e OutputItemAddedEvent) EventType() string { return e.Type }
func (e OutputItemAddedEvent) Validate() error {
	if err := e.EventBase.validate("response.output_item.added"); err != nil {
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
func (e *OutputItemAddedEvent) UnmarshalJSON(data []byte) error {
	var shadow struct {
		Type           string          `json:"type"`
		SequenceNumber int64           `json:"sequence_number"`
		OutputIndex    int64           `json:"output_index"`
		Item           json.RawMessage `json:"item"`
	}
	if err := wire.Decode(data, &shadow); err != nil {
		return err
	}
	item, err := DecodeOutputItem(shadow.Item)
	if err != nil {
		return fmt.Errorf("output_item.added: %w", err)
	}
	*e = OutputItemAddedEvent{
		EventBase: EventBase{
			Type:           shadow.Type,
			SequenceNumber: shadow.SequenceNumber,
		},
		OutputIndex: shadow.OutputIndex,
		Item:        item,
	}
	return e.Validate()
}

// OutputItemDoneEvent is emitted when an output item is completed.
type OutputItemDoneEvent struct {
	EventBase
	OutputIndex int64      `json:"output_index"`
	Item        OutputItem `json:"item"`
}

func (e OutputItemDoneEvent) EventType() string { return e.Type }
func (e OutputItemDoneEvent) Validate() error {
	if err := e.EventBase.validate("response.output_item.done"); err != nil {
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
func (e *OutputItemDoneEvent) UnmarshalJSON(data []byte) error {
	var shadow struct {
		Type           string          `json:"type"`
		SequenceNumber int64           `json:"sequence_number"`
		OutputIndex    int64           `json:"output_index"`
		Item           json.RawMessage `json:"item"`
	}
	if err := wire.Decode(data, &shadow); err != nil {
		return err
	}
	item, err := DecodeOutputItem(shadow.Item)
	if err != nil {
		return fmt.Errorf("output_item.done: %w", err)
	}
	*e = OutputItemDoneEvent{
		EventBase: EventBase{
			Type:           shadow.Type,
			SequenceNumber: shadow.SequenceNumber,
		},
		OutputIndex: shadow.OutputIndex,
		Item:        item,
	}
	return e.Validate()
}

// SummaryTextPart is the summary part payload of reasoning summary events.
type SummaryTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ReasoningSummaryPartAddedEvent is emitted when a reasoning summary part is
// added.
type ReasoningSummaryPartAddedEvent struct {
	EventBase
	ItemID       string          `json:"item_id"`
	OutputIndex  int64           `json:"output_index"`
	SummaryIndex int64           `json:"summary_index"`
	Part         SummaryTextPart `json:"part"`
}

func (e ReasoningSummaryPartAddedEvent) EventType() string { return e.Type }
func (e ReasoningSummaryPartAddedEvent) Validate() error {
	if err := e.EventBase.validate("response.reasoning_summary_part.added"); err != nil {
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

// ReasoningSummaryTextDeltaEvent is emitted when reasoning summary text is
// added.
type ReasoningSummaryTextDeltaEvent struct {
	EventBase
	ItemID       string `json:"item_id"`
	OutputIndex  int64  `json:"output_index"`
	SummaryIndex int64  `json:"summary_index"`
	Delta        string `json:"delta"`
}

func (e ReasoningSummaryTextDeltaEvent) EventType() string { return e.Type }
func (e ReasoningSummaryTextDeltaEvent) Validate() error {
	if err := e.EventBase.validate("response.reasoning_summary_text.delta"); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("reasoning summary delta item_id is empty")
	}
	if e.OutputIndex < 0 || e.SummaryIndex < 0 {
		return errors.New("negative reasoning summary index")
	}
	return nil
}

// ReasoningSummaryTextDoneEvent is emitted when reasoning summary text is
// finalized.
type ReasoningSummaryTextDoneEvent struct {
	EventBase
	ItemID       string `json:"item_id"`
	OutputIndex  int64  `json:"output_index"`
	SummaryIndex int64  `json:"summary_index"`
	Text         string `json:"text"`
}

func (e ReasoningSummaryTextDoneEvent) EventType() string { return e.Type }
func (e ReasoningSummaryTextDoneEvent) Validate() error {
	if err := e.EventBase.validate("response.reasoning_summary_text.done"); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("reasoning summary done item_id is empty")
	}
	if e.OutputIndex < 0 || e.SummaryIndex < 0 {
		return errors.New("negative reasoning summary index")
	}
	return nil
}

// ReasoningSummaryPartDoneEvent is emitted when a reasoning summary part is
// completed.
type ReasoningSummaryPartDoneEvent struct {
	EventBase
	ItemID       string          `json:"item_id"`
	OutputIndex  int64           `json:"output_index"`
	SummaryIndex int64           `json:"summary_index"`
	Part         SummaryTextPart `json:"part"`
	Status       string          `json:"status,omitempty"`
}

func (e ReasoningSummaryPartDoneEvent) EventType() string { return e.Type }
func (e ReasoningSummaryPartDoneEvent) Validate() error {
	if err := e.EventBase.validate("response.reasoning_summary_part.done"); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("reasoning summary part done item_id is empty")
	}
	if e.Part.Type != "summary_text" {
		return fmt.Errorf("reasoning summary done part type = %q", e.Part.Type)
	}
	if e.OutputIndex < 0 || e.SummaryIndex < 0 {
		return errors.New("negative reasoning summary index")
	}
	if e.Status != "" && e.Status != "incomplete" {
		return fmt.Errorf("invalid reasoning summary part status %q", e.Status)
	}
	return nil
}

// StreamContentPart is the part payload of content part events.
type StreamContentPart interface {
	isStreamContentPart()
	Validate() error
}

// StreamOutputTextPart is an output_text part in a stream event.
type StreamOutputTextPart struct {
	Type        string        `json:"type"`
	Text        string        `json:"text"`
	Annotations []Annotation  `json:"annotations"`
	Logprobs    []TextLogprob `json:"logprobs,omitempty"`
}

func (*StreamOutputTextPart) isStreamContentPart() {}
func (p *StreamOutputTextPart) Validate() error {
	if p.Type != "output_text" {
		return fmt.Errorf("stream output text type = %q", p.Type)
	}
	if p.Annotations == nil {
		return errors.New("stream output_text annotations must be an array")
	}
	return nil
}

// StreamRefusalPart is a refusal part in a stream event.
type StreamRefusalPart struct {
	Type    string `json:"type"`
	Refusal string `json:"refusal"`
}

func (*StreamRefusalPart) isStreamContentPart() {}
func (p *StreamRefusalPart) Validate() error {
	if p.Type != "refusal" {
		return fmt.Errorf("stream refusal part type = %q", p.Type)
	}
	return nil
}

// ContentPartAddedEvent is emitted when a content part is added.
type ContentPartAddedEvent struct {
	EventBase
	ItemID       string            `json:"item_id"`
	OutputIndex  int64             `json:"output_index"`
	ContentIndex int64             `json:"content_index"`
	Part         StreamContentPart `json:"part"`
}

func (e ContentPartAddedEvent) EventType() string { return e.Type }
func (e ContentPartAddedEvent) Validate() error {
	if err := e.EventBase.validate("response.content_part.added"); err != nil {
		return err
	}
	if e.ItemID == "" || e.Part == nil {
		return errors.New("content_part.added requires item_id and part")
	}
	if e.OutputIndex < 0 || e.ContentIndex < 0 {
		return errors.New("negative content part index")
	}
	return e.Part.Validate()
}

// UnmarshalJSON decodes the event, dispatching the part through the stream
// content part union.
func (e *ContentPartAddedEvent) UnmarshalJSON(data []byte) error {
	var shadow struct {
		Type           string          `json:"type"`
		SequenceNumber int64           `json:"sequence_number"`
		ItemID         string          `json:"item_id"`
		OutputIndex    int64           `json:"output_index"`
		ContentIndex   int64           `json:"content_index"`
		Part           json.RawMessage `json:"part"`
	}
	if err := wire.Decode(data, &shadow); err != nil {
		return err
	}
	part, err := decodeStreamContentPart(shadow.Part)
	if err != nil {
		return fmt.Errorf("content_part.added: %w", err)
	}
	*e = ContentPartAddedEvent{
		EventBase: EventBase{
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

// TextDeltaEvent is emitted when output text is added.
type TextDeltaEvent struct {
	EventBase
	ItemID       string        `json:"item_id"`
	OutputIndex  int64         `json:"output_index"`
	ContentIndex int64         `json:"content_index"`
	Delta        string        `json:"delta"`
	Logprobs     []TextLogprob `json:"logprobs"`
}

func (e TextDeltaEvent) EventType() string { return e.Type }
func (e TextDeltaEvent) Validate() error {
	if err := e.EventBase.validate("response.output_text.delta"); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("output_text.delta item_id is empty")
	}
	if e.OutputIndex < 0 || e.ContentIndex < 0 {
		return errors.New("negative text index")
	}
	if e.Logprobs == nil {
		return errors.New("output_text.delta logprobs must be an array")
	}
	return nil
}

// TextDoneEvent is emitted when output text is finalized.
type TextDoneEvent struct {
	EventBase
	ItemID       string        `json:"item_id"`
	OutputIndex  int64         `json:"output_index"`
	ContentIndex int64         `json:"content_index"`
	Text         string        `json:"text"`
	Logprobs     []TextLogprob `json:"logprobs"`
}

func (e TextDoneEvent) EventType() string { return e.Type }
func (e TextDoneEvent) Validate() error {
	if err := e.EventBase.validate("response.output_text.done"); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("output_text.done item_id is empty")
	}
	if e.OutputIndex < 0 || e.ContentIndex < 0 {
		return errors.New("negative text index")
	}
	if e.Logprobs == nil {
		return errors.New("output_text.done logprobs must be an array")
	}
	return nil
}

// ContentPartDoneEvent is emitted when a content part is completed.
type ContentPartDoneEvent struct {
	EventBase
	ItemID       string            `json:"item_id"`
	OutputIndex  int64             `json:"output_index"`
	ContentIndex int64             `json:"content_index"`
	Part         StreamContentPart `json:"part"`
}

func (e ContentPartDoneEvent) EventType() string { return e.Type }
func (e ContentPartDoneEvent) Validate() error {
	if err := e.EventBase.validate("response.content_part.done"); err != nil {
		return err
	}
	if e.ItemID == "" || e.Part == nil {
		return errors.New("content_part.done requires item_id and part")
	}
	if e.OutputIndex < 0 || e.ContentIndex < 0 {
		return errors.New("negative content part index")
	}
	return e.Part.Validate()
}

// UnmarshalJSON decodes the event, dispatching the part through the stream
// content part union.
func (e *ContentPartDoneEvent) UnmarshalJSON(data []byte) error {
	var shadow struct {
		Type           string          `json:"type"`
		SequenceNumber int64           `json:"sequence_number"`
		ItemID         string          `json:"item_id"`
		OutputIndex    int64           `json:"output_index"`
		ContentIndex   int64           `json:"content_index"`
		Part           json.RawMessage `json:"part"`
	}
	if err := wire.Decode(data, &shadow); err != nil {
		return err
	}
	part, err := decodeStreamContentPart(shadow.Part)
	if err != nil {
		return fmt.Errorf("content_part.done: %w", err)
	}
	*e = ContentPartDoneEvent{
		EventBase: EventBase{
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

// decodeStreamContentPart dispatches the part through the stream content
// part union.
func decodeStreamContentPart(data []byte) (StreamContentPart, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	if probe.Type == "" {
		return nil, &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "type",
			Message: "stream content part requires a type tag",
		}
	}
	var part StreamContentPart
	switch probe.Type {
	case "output_text":
		part = &StreamOutputTextPart{}
	case "refusal":
		part = &StreamRefusalPart{}
	default:
		return nil, &wire.UnsupportedTypeError{
			Protocol: "responses",
			Path:     "stream[].part.type",
			Type:     probe.Type,
		}
	}
	if err := wire.Decode(data, part); err != nil {
		return nil, err
	}
	// Decode-side normalization (autopsy 01): real clients omit the
	// annotations key on output_text parts; a decoded absent array is the
	// same empty array. Validate itself stays strict for hand-built values.
	if text, ok := part.(*StreamOutputTextPart); ok && text.Annotations == nil {
		text.Annotations = []Annotation{}
	}
	if err := part.Validate(); err != nil {
		return nil, err
	}
	return part, nil
}

// FunctionCallArgumentsDeltaEvent carries a partial function-call arguments
// delta. The official field is delta, never arguments.
type FunctionCallArgumentsDeltaEvent struct {
	EventBase
	ItemID      string `json:"item_id"`
	OutputIndex int64  `json:"output_index"`
	Delta       string `json:"delta"`
}

func (e FunctionCallArgumentsDeltaEvent) EventType() string { return e.Type }
func (e FunctionCallArgumentsDeltaEvent) Validate() error {
	if err := e.EventBase.validate("response.function_call_arguments.delta"); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("function arguments delta item_id is empty")
	}
	if e.OutputIndex < 0 {
		return errors.New("negative function arguments index")
	}
	return nil
}

// FunctionCallArgumentsDoneEvent carries the finalized arguments of a
// function call. The official event requires arguments but carries NO name:
// call identity comes from the output_item.added lifecycle, never from a
// private superset field.
type FunctionCallArgumentsDoneEvent struct {
	EventBase
	ItemID      string `json:"item_id"`
	OutputIndex int64  `json:"output_index"`
	Arguments   string `json:"arguments"`
}

func (e FunctionCallArgumentsDoneEvent) EventType() string { return e.Type }
func (e FunctionCallArgumentsDoneEvent) Validate() error {
	if err := e.EventBase.validate("response.function_call_arguments.done"); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("function arguments done requires item_id")
	}
	if e.OutputIndex < 0 {
		return errors.New("negative function arguments index")
	}
	// arguments is optional on the wire: the official done event may carry
	// an empty string when nothing was accumulated. The payload is
	// model-generated output preserved byte-exact; any string is legal and
	// invalid model output is never an upstream defect (review-z commit 2).
	return nil
}

// RefusalDeltaEvent carries a partial refusal delta.
type RefusalDeltaEvent struct {
	EventBase
	ItemID       string `json:"item_id"`
	OutputIndex  int64  `json:"output_index"`
	ContentIndex int64  `json:"content_index"`
	Delta        string `json:"delta"`
}

func (e RefusalDeltaEvent) EventType() string { return e.Type }
func (e RefusalDeltaEvent) Validate() error {
	if err := e.EventBase.validate("response.refusal.delta"); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("refusal delta item_id is empty")
	}
	if e.OutputIndex < 0 || e.ContentIndex < 0 {
		return errors.New("negative refusal index")
	}
	return nil
}

// RefusalDoneEvent carries the finalized refusal text. This event exists in
// the official stream and must be emitted when a refusal completes.
type RefusalDoneEvent struct {
	EventBase
	ItemID       string `json:"item_id"`
	OutputIndex  int64  `json:"output_index"`
	ContentIndex int64  `json:"content_index"`
	Refusal      string `json:"refusal"`
}

func (e RefusalDoneEvent) EventType() string { return e.Type }
func (e RefusalDoneEvent) Validate() error {
	if err := e.EventBase.validate("response.refusal.done"); err != nil {
		return err
	}
	if e.ItemID == "" {
		return errors.New("refusal done item_id is empty")
	}
	if e.OutputIndex < 0 || e.ContentIndex < 0 {
		return errors.New("negative refusal index")
	}
	return nil
}

// CompletedEvent is the success terminal event.
type CompletedEvent struct {
	EventBase
	Response Response `json:"response"`
}

func (e CompletedEvent) EventType() string { return e.Type }
func (e CompletedEvent) Validate() error {
	if err := e.EventBase.validate("response.completed"); err != nil {
		return err
	}
	if e.Response.Status != "completed" {
		return fmt.Errorf("completed response status = %q", e.Response.Status)
	}
	return e.Response.Validate()
}

// IncompleteEvent is the incomplete terminal event.
type IncompleteEvent struct {
	EventBase
	Response Response `json:"response"`
}

func (e IncompleteEvent) EventType() string { return e.Type }
func (e IncompleteEvent) Validate() error {
	if err := e.EventBase.validate("response.incomplete"); err != nil {
		return err
	}
	if e.Response.Status != "incomplete" {
		return fmt.Errorf("incomplete response status = %q", e.Response.Status)
	}
	return e.Response.Validate()
}

// FailedEvent is the failure terminal event. It must never be rendered as a
// successful completion in any target dialect.
type FailedEvent struct {
	EventBase
	Response Response `json:"response"`
}

func (e FailedEvent) EventType() string { return e.Type }
func (e FailedEvent) Validate() error {
	if err := e.EventBase.validate("response.failed"); err != nil {
		return err
	}
	if e.Response.Status != "failed" {
		return fmt.Errorf("failed response status = %q", e.Response.Status)
	}
	return e.Response.Validate()
}

// ErrorEvent is the error event. Code, message, and param are all required
// at the top level.
type ErrorEvent struct {
	EventBase
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param"`
}

func (e ErrorEvent) EventType() string { return e.Type }
func (e ErrorEvent) Validate() error {
	if err := e.EventBase.validate("error"); err != nil {
		return err
	}
	if e.Code == "" {
		return &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "code",
			Message: "error event requires code",
		}
	}
	if e.Message == "" {
		return &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "message",
			Message: "error event requires message",
		}
	}
	return nil
}

// EventBuilder allocates sequence numbers. Each event gets exactly one
// sequence number, allocated at construction.
type EventBuilder struct {
	nextSequence int64
}

func (b *EventBuilder) base(eventType string) EventBase {
	base := EventBase{
		Type:           eventType,
		SequenceNumber: b.nextSequence,
	}
	b.nextSequence++
	return base
}

// NextSequenceNumber returns the sequence number the next event will
// receive. Error frames generated outside the builder (stream abort paths)
// use it so the error terminal keeps the stream's sequence monotonic.
func (b *EventBuilder) NextSequenceNumber() int64 {
	return b.nextSequence
}

// Created builds a response.created event.
func (b *EventBuilder) Created(response Response) CreatedEvent {
	return CreatedEvent{EventBase: b.base("response.created"), Response: response}
}

// InProgress builds a response.in_progress event.
func (b *EventBuilder) InProgress(response Response) InProgressEvent {
	return InProgressEvent{EventBase: b.base("response.in_progress"), Response: response}
}

// OutputItemAdded builds a response.output_item.added event.
func (b *EventBuilder) OutputItemAdded(outputIndex int64, item OutputItem) OutputItemAddedEvent {
	return OutputItemAddedEvent{
		EventBase:   b.base("response.output_item.added"),
		OutputIndex: outputIndex,
		Item:        item,
	}
}

// OutputItemDone builds a response.output_item.done event.
func (b *EventBuilder) OutputItemDone(outputIndex int64, item OutputItem) OutputItemDoneEvent {
	return OutputItemDoneEvent{
		EventBase:   b.base("response.output_item.done"),
		OutputIndex: outputIndex,
		Item:        item,
	}
}

// ReasoningSummaryPartAdded builds a reasoning summary part added event.
func (b *EventBuilder) ReasoningSummaryPartAdded(
	itemID string, outputIndex int64, summaryIndex int64, part SummaryTextPart,
) ReasoningSummaryPartAddedEvent {
	return ReasoningSummaryPartAddedEvent{
		EventBase:    b.base("response.reasoning_summary_part.added"),
		ItemID:       itemID,
		OutputIndex:  outputIndex,
		SummaryIndex: summaryIndex,
		Part:         part,
	}
}

// ReasoningSummaryTextDelta builds a reasoning summary text delta event.
func (b *EventBuilder) ReasoningSummaryTextDelta(
	itemID string, outputIndex int64, summaryIndex int64, delta string,
) ReasoningSummaryTextDeltaEvent {
	return ReasoningSummaryTextDeltaEvent{
		EventBase:    b.base("response.reasoning_summary_text.delta"),
		ItemID:       itemID,
		OutputIndex:  outputIndex,
		SummaryIndex: summaryIndex,
		Delta:        delta,
	}
}

// ReasoningSummaryTextDone builds a reasoning summary text done event.
func (b *EventBuilder) ReasoningSummaryTextDone(
	itemID string, outputIndex int64, summaryIndex int64, text string,
) ReasoningSummaryTextDoneEvent {
	return ReasoningSummaryTextDoneEvent{
		EventBase:    b.base("response.reasoning_summary_text.done"),
		ItemID:       itemID,
		OutputIndex:  outputIndex,
		SummaryIndex: summaryIndex,
		Text:         text,
	}
}

// ReasoningSummaryPartDone builds a reasoning summary part done event.
func (b *EventBuilder) ReasoningSummaryPartDone(
	itemID string, outputIndex int64, summaryIndex int64, part SummaryTextPart,
) ReasoningSummaryPartDoneEvent {
	return ReasoningSummaryPartDoneEvent{
		EventBase:    b.base("response.reasoning_summary_part.done"),
		ItemID:       itemID,
		OutputIndex:  outputIndex,
		SummaryIndex: summaryIndex,
		Part:         part,
	}
}

// ContentPartAdded builds a response.content_part.added event.
func (b *EventBuilder) ContentPartAdded(
	itemID string, outputIndex int64, contentIndex int64, part StreamContentPart,
) ContentPartAddedEvent {
	return ContentPartAddedEvent{
		EventBase:    b.base("response.content_part.added"),
		ItemID:       itemID,
		OutputIndex:  outputIndex,
		ContentIndex: contentIndex,
		Part:         part,
	}
}

// TextDelta builds a response.output_text.delta event with an empty non-nil
// logprobs array, as required by the wire contract.
func (b *EventBuilder) TextDelta(
	itemID string, outputIndex int64, contentIndex int64, delta string,
) TextDeltaEvent {
	return TextDeltaEvent{
		EventBase:    b.base("response.output_text.delta"),
		ItemID:       itemID,
		OutputIndex:  outputIndex,
		ContentIndex: contentIndex,
		Delta:        delta,
		Logprobs:     make([]TextLogprob, 0),
	}
}

// TextDone builds a response.output_text.done event with an empty non-nil
// logprobs array.
func (b *EventBuilder) TextDone(
	itemID string, outputIndex int64, contentIndex int64, text string,
) TextDoneEvent {
	return TextDoneEvent{
		EventBase:    b.base("response.output_text.done"),
		ItemID:       itemID,
		OutputIndex:  outputIndex,
		ContentIndex: contentIndex,
		Text:         text,
		Logprobs:     make([]TextLogprob, 0),
	}
}

// ContentPartDone builds a response.content_part.done event.
func (b *EventBuilder) ContentPartDone(
	itemID string, outputIndex int64, contentIndex int64, part StreamContentPart,
) ContentPartDoneEvent {
	return ContentPartDoneEvent{
		EventBase:    b.base("response.content_part.done"),
		ItemID:       itemID,
		OutputIndex:  outputIndex,
		ContentIndex: contentIndex,
		Part:         part,
	}
}

// FunctionArgumentsDelta builds a response.function_call_arguments.delta
// event. The partial payload is carried by delta.
func (b *EventBuilder) FunctionArgumentsDelta(
	itemID string, outputIndex int64, delta string,
) FunctionCallArgumentsDeltaEvent {
	return FunctionCallArgumentsDeltaEvent{
		EventBase:   b.base("response.function_call_arguments.delta"),
		ItemID:      itemID,
		OutputIndex: outputIndex,
		Delta:       delta,
	}
}

// FunctionArgumentsDone builds a response.function_call_arguments.done event
// carrying the finalized arguments (never a partial payload). The official
// event has no name field: call identity comes from the item-added
// lifecycle.
func (b *EventBuilder) FunctionArgumentsDone(
	itemID string, outputIndex int64, arguments string,
) FunctionCallArgumentsDoneEvent {
	return FunctionCallArgumentsDoneEvent{
		EventBase:   b.base("response.function_call_arguments.done"),
		ItemID:      itemID,
		OutputIndex: outputIndex,
		Arguments:   arguments,
	}
}

// RefusalDelta builds a response.refusal.delta event.
func (b *EventBuilder) RefusalDelta(
	itemID string, outputIndex int64, contentIndex int64, delta string,
) RefusalDeltaEvent {
	return RefusalDeltaEvent{
		EventBase:    b.base("response.refusal.delta"),
		ItemID:       itemID,
		OutputIndex:  outputIndex,
		ContentIndex: contentIndex,
		Delta:        delta,
	}
}

// RefusalDone builds a response.refusal.done event.
func (b *EventBuilder) RefusalDone(
	itemID string, outputIndex int64, contentIndex int64, refusal string,
) RefusalDoneEvent {
	return RefusalDoneEvent{
		EventBase:    b.base("response.refusal.done"),
		ItemID:       itemID,
		OutputIndex:  outputIndex,
		ContentIndex: contentIndex,
		Refusal:      refusal,
	}
}

// Completed builds a response.completed event.
func (b *EventBuilder) Completed(response Response) CompletedEvent {
	return CompletedEvent{EventBase: b.base("response.completed"), Response: response}
}

// Incomplete builds a response.incomplete event.
func (b *EventBuilder) Incomplete(response Response) IncompleteEvent {
	return IncompleteEvent{EventBase: b.base("response.incomplete"), Response: response}
}

// Failed builds a response.failed event.
func (b *EventBuilder) Failed(response Response) FailedEvent {
	return FailedEvent{EventBase: b.base("response.failed"), Response: response}
}

// Error builds an error event.
func (b *EventBuilder) Error(code string, message string, param string) ErrorEvent {
	return ErrorEvent{
		EventBase: b.base("error"),
		Code:      code,
		Message:   message,
		Param:     param,
	}
}

// DecodeEvent decodes one Responses stream frame into the typed event union.
// Unknown event types produce a wire.UnsupportedTypeError identifying the
// exact type.
func DecodeEvent(data []byte) (Event, error) {
	var probe struct {
		Type wire.Field[string] `json:"type"`
	}
	// The probe is lenient: it reads only the first JSON value, so a
	// trailing-value document is rejected with the typed trailing error by
	// the strict decode below, never with a raw syntax error here.
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&probe); err != nil {
		return nil, err
	}
	if probe.Type.Null {
		return nil, &wire.DecodeError{
			Kind:    wire.DecodeIllegalNull,
			Path:    "stream[].type",
			Message: "event type must not be null",
		}
	}
	if !probe.Type.Present || probe.Type.Value == "" {
		return nil, &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "stream[].type",
			Message: "event type is required",
		}
	}

	var event Event
	switch probe.Type.Value {
	case "response.created":
		event = &CreatedEvent{}
	case "response.in_progress":
		event = &InProgressEvent{}
	case "response.output_item.added":
		event = &OutputItemAddedEvent{}
	case "response.output_item.done":
		event = &OutputItemDoneEvent{}
	case "response.reasoning_summary_part.added":
		event = &ReasoningSummaryPartAddedEvent{}
	case "response.reasoning_summary_text.delta":
		event = &ReasoningSummaryTextDeltaEvent{}
	case "response.reasoning_summary_text.done":
		event = &ReasoningSummaryTextDoneEvent{}
	case "response.reasoning_summary_part.done":
		event = &ReasoningSummaryPartDoneEvent{}
	case "response.content_part.added":
		event = &ContentPartAddedEvent{}
	case "response.output_text.delta":
		event = &TextDeltaEvent{}
	case "response.output_text.done":
		event = &TextDoneEvent{}
	case "response.content_part.done":
		event = &ContentPartDoneEvent{}
	case "response.function_call_arguments.delta":
		event = &FunctionCallArgumentsDeltaEvent{}
	case "response.function_call_arguments.done":
		event = &FunctionCallArgumentsDoneEvent{}
	case "response.refusal.delta":
		event = &RefusalDeltaEvent{}
	case "response.refusal.done":
		event = &RefusalDoneEvent{}
	case "response.completed":
		event = &CompletedEvent{}
	case "response.incomplete":
		event = &IncompleteEvent{}
	case "response.failed":
		event = &FailedEvent{}
	case "error":
		event = &ErrorEvent{}
	default:
		return nil, &wire.UnsupportedTypeError{
			Protocol: "responses",
			Path:     "stream[].type",
			Type:     probe.Type.Value,
		}
	}

	if err := wire.Decode(data, event); err != nil {
		return nil, err
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return event, nil
}
