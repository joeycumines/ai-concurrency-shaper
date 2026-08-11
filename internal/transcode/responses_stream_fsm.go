package transcode

// The reusable Responses stream lifecycle FSM (review-z commit 3): every
// stream event passes through ONE validator before any protocol conversion.
// The FSM owns the phase model (item/part/call phases), sequence and identity
// pinning, output-index uniqueness, accumulated text/refusal/arguments
// buffers, the reasoning lifecycle, the terminal state, snapshot
// reconciliation, and the global budget. Every rejection class below is a
// typed UpstreamWireError with upstream-failure classification.
//
// The Responses-to-Messages converter calls Validate before interpreting
// every event; the render-side state machines keep their own bookkeeping for
// the target dialect, but the lifecycle authority is here.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// itemPhase is the lifecycle phase of one output item.
type itemPhase uint8

const (
	itemPhaseAdded itemPhase = iota + 1
	itemPhaseDone
)

// partPhase is the lifecycle phase of one content part.
type partPhase uint8

const (
	partPhaseAdded partPhase = iota + 1
	partPhaseValueDone
	partPhaseDone
)

// callPhase is the lifecycle phase of one function call.
type callPhase uint8

const (
	callPhaseAdded callPhase = iota + 1
	callPhaseArgumentsDone
	callPhaseDone
)

// fsmItem is the FSM's per-item state. Accumulated text/refusal/arguments
// use strings.Builder to avoid quadratic re-copying on every delta (the
// review-k finding 9 antipattern the render machine already avoids).
type fsmItem struct {
	outputIndex int64
	kind        string // message, function_call, function_call_output, reasoning
	itemPhase   itemPhase
	parts       map[int64]fsmPart // content index -> part state
	calls       map[string]callPhase
	text        map[int64]*strings.Builder
	refusal     map[int64]*strings.Builder
	arguments   strings.Builder
	reasoning   reasoningPhase
}

// fsmPart is the FSM's per-part state.
type fsmPart struct {
	kind      string // output_text, refusal
	partPhase partPhase
}

// reasoningPhase is the lifecycle phase of one reasoning summary part.
type reasoningPhase uint8

const (
	reasoningNone reasoningPhase = iota
	reasoningPartAdded
	reasoningValueDone
	reasoningPartDone
)

// responsesStreamFSM validates one Responses stream lifecycle.
type responsesStreamFSM struct {
	// budget is the seven-dimension total exchange budget: every event and
	// every state allocation charges it BEFORE the mutation.
	budget streamBudget

	started           bool
	terminal          bool
	lastSeq           int64
	identityID        string
	identityModel     string
	identityCreatedAt float64

	items   map[string]*fsmItem
	indexes map[int64]string
}

// newResponsesStreamFSM returns a fresh FSM with the package-default budget.
func newResponsesStreamFSM() *responsesStreamFSM {
	return &responsesStreamFSM{
		budget:  newStreamBudget(),
		lastSeq: -1, // any nonnegative first sequence is accepted
		items:   make(map[string]*fsmItem),
		indexes: make(map[int64]string),
	}
}

// Validate processes one event against the lifecycle. A violation is a typed
// UpstreamWireError (upstream-failure classification). The error event is the
// one event that may arrive at any point before a terminal.
func (f *responsesStreamFSM) Validate(event ResponsesSSEEvent) error {
	if err := f.budget.addEvent(); err != nil {
		return upstreamWireError(UpstreamResponses, http.StatusOK, err)
	}

	// 17: any event after a terminal event.
	if f.terminal {
		return upstreamWireError(UpstreamResponses, http.StatusOK, errors.New(
			"responses stream event after the terminal event",
		))
	}

	// 3: duplicate or decreasing sequence numbers.
	sequence := event.Sequence()
	if sequence <= f.lastSeq {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"responses stream sequence number %d is not strictly increasing (last %d)",
			sequence,
			f.lastSeq,
		))
	}
	f.lastSeq = sequence

	// 1: any event before response.created, except a terminal error event.
	if !f.started && event.EventType() != "response.created" &&
		event.EventType() != "error" {
		return upstreamWireError(UpstreamResponses, http.StatusOK, errors.New(
			"responses stream event before response.created",
		))
	}

	switch value := event.(type) {
	case ResponseCreatedEvent:
		return f.created(value)
	case ResponseInProgressEvent:
		return nil
	case ResponseOutputItemAddedEvent:
		return f.itemAdded(value)
	case ResponseOutputItemDoneEvent:
		return f.itemDone(value)
	case ResponseContentPartAddedEvent:
		return f.partAdded(value)
	case ResponseContentPartDoneEvent:
		return f.partDone(value)
	case ResponseTextDeltaEvent:
		return f.textDelta(value)
	case ResponseTextDoneEvent:
		return f.textDone(value)
	case ResponseRefusalDeltaEvent:
		return f.refusalDelta(value)
	case ResponseRefusalDoneEvent:
		return f.refusalDone(value)
	case ResponseFunctionCallArgumentsDeltaEvent:
		return f.argumentsDelta(value)
	case ResponseFunctionCallArgumentsDoneEvent:
		return f.argumentsDone(value)
	case ResponseReasoningSummaryPartAddedEvent:
		return f.reasoningAdded(value)
	case ResponseReasoningSummaryTextDeltaEvent:
		return f.reasoningDelta(value)
	case ResponseReasoningSummaryTextDoneEvent:
		return f.reasoningTextDone(value)
	case ResponseReasoningSummaryPartDoneEvent:
		return f.reasoningDone(value)
	case ResponseCompletedEvent:
		return f.terminalEvent(value.Response, "completed")
	case ResponseIncompleteEvent:
		return f.terminalEvent(value.Response, "incomplete")
	case ResponseFailedEvent:
		return f.terminalEvent(value.Response, "failed")
	case ResponseErrorEvent:
		return nil
	default:
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"responses stream event of unknown type %T",
			event,
		))
	}
}

// created pins the response identity.
func (f *responsesStreamFSM) created(event ResponseCreatedEvent) error {
	// 2: duplicate response.created.
	if f.started {
		return upstreamWireError(UpstreamResponses, http.StatusOK, errors.New(
			"duplicate response.created",
		))
	}
	f.started = true
	f.identityID = event.Response.ID
	f.identityModel = event.Response.Model
	f.identityCreatedAt = event.Response.CreatedAt
	return nil
}

// itemAdded registers the item and its output index.
func (f *responsesStreamFSM) itemAdded(event ResponseOutputItemAddedEvent) error {
	itemID := responsesOutputItemID(event.Item)
	// 4: duplicate item IDs.
	if _, exists := f.items[itemID]; exists {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"duplicate output item %q",
			itemID,
		))
	}
	// 5: duplicate output indexes.
	if prior, exists := f.indexes[event.OutputIndex]; exists {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"duplicate output index %d (items %q and %q)",
			event.OutputIndex,
			prior,
			itemID,
		))
	}
	if err := f.budget.addItem(); err != nil {
		return upstreamWireError(UpstreamResponses, http.StatusOK, err)
	}
	if err := f.budget.addStateEntries(1); err != nil {
		return upstreamWireError(UpstreamResponses, http.StatusOK, err)
	}
	f.items[itemID] = &fsmItem{
		outputIndex: event.OutputIndex,
		kind:        itemTypeName(event.Item),
		itemPhase:   itemPhaseAdded,
		parts:       make(map[int64]fsmPart),
		calls:       make(map[string]callPhase),
		text:        make(map[int64]*strings.Builder),
		refusal:     make(map[int64]*strings.Builder),
	}
	if itemTypeName(event.Item) == "function_call" {
		if err := f.budget.addToolCall(); err != nil {
			return upstreamWireError(UpstreamResponses, http.StatusOK, err)
		}
	}
	f.indexes[event.OutputIndex] = itemID
	return nil
}

// itemDone closes the item; open children are a violation (12).
func (f *responsesStreamFSM) itemDone(event ResponseOutputItemDoneEvent) error {
	itemID := responsesOutputItemID(event.Item)
	item, ok := f.items[itemID]
	if !ok {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"output item done for unknown item %q",
			itemID,
		))
	}
	if item.outputIndex != event.OutputIndex {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"output item done output index = %d, want %d (observed at output_item.added)",
			event.OutputIndex,
			item.outputIndex,
		))
	}
	if got := itemTypeName(event.Item); got != item.kind {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"output item done type = %q, want %q (observed at output_item.added)",
			got,
			item.kind,
		))
	}
	if item.itemPhase == itemPhaseDone {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"duplicate output item done for %q",
			itemID,
		))
	}
	for _, part := range item.parts {
		if part.partPhase == partPhaseAdded {
			return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
				"output item done for %q while a content part is still streaming",
				responsesOutputItemID(event.Item),
			))
		}
	}
	for _, phase := range item.calls {
		if phase == callPhaseAdded {
			return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
				"output item done for %q while a function call is still streaming",
				responsesOutputItemID(event.Item),
			))
		}
	}
	item.itemPhase = itemPhaseDone
	return nil
}

// partAdded opens a content part on a message item.
func (f *responsesStreamFSM) partAdded(event ResponseContentPartAddedEvent) error {
	item, err := f.messageItem(event.ItemID, event.OutputIndex, event.ContentIndex)
	if err != nil {
		return err
	}
	// 7: duplicate content indexes within an item.
	if _, exists := item.parts[event.ContentIndex]; exists {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"duplicate content part %q at index %d",
			event.ItemID,
			event.ContentIndex,
		))
	}
	if err := f.budget.addPart(); err != nil {
		return upstreamWireError(UpstreamResponses, http.StatusOK, err)
	}
	if err := f.budget.addStateEntries(1); err != nil {
		return upstreamWireError(UpstreamResponses, http.StatusOK, err)
	}
	item.parts[event.ContentIndex] = fsmPart{
		kind:      partTypeName(event.Part),
		partPhase: partPhaseAdded,
	}
	return nil
}

// partDone closes a content part; value-done must precede it (11).
func (f *responsesStreamFSM) partDone(event ResponseContentPartDoneEvent) error {
	item, err := f.messageItem(event.ItemID, event.OutputIndex, event.ContentIndex)
	if err != nil {
		return err
	}
	part, exists := item.parts[event.ContentIndex]
	if !exists {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"content_part.done for unopened part %q at index %d",
			event.ItemID,
			event.ContentIndex,
		))
	}
	if part.kind != partTypeName(event.Part) {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"content_part.done type = %q, want %q",
			partTypeName(event.Part),
			part.kind,
		))
	}
	if part.partPhase == partPhaseDone {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"duplicate content_part.done for %q at index %d",
			event.ItemID,
			event.ContentIndex,
		))
	}
	if part.partPhase != partPhaseValueDone {
		// 11: content_part.done before value-done.
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"content_part.done for %q at index %d before the value done event",
			event.ItemID,
			event.ContentIndex,
		))
	}
	part.partPhase = partPhaseDone
	item.parts[event.ContentIndex] = part
	return nil
}

// textDelta accumulates output text.
func (f *responsesStreamFSM) textDelta(event ResponseTextDeltaEvent) error {
	item, part, err := f.partForDelta(event.ItemID, event.OutputIndex, event.ContentIndex, "output_text")
	if err != nil {
		return err
	}
	if part.partPhase != partPhaseAdded {
		// 10: deltas after the corresponding done event.
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"output_text.delta after done for %q at index %d",
			event.ItemID,
			event.ContentIndex,
		))
	}
	if err := f.budget.addSemanticBytes(int64(len(event.Delta))); err != nil {
		return upstreamWireError(UpstreamResponses, http.StatusOK, err)
	}
	builder, ok := item.text[event.ContentIndex]
	if !ok {
		builder = &strings.Builder{}
		item.text[event.ContentIndex] = builder
	}
	builder.WriteString(event.Delta)
	return nil
}

// textDone finalizes the accumulated text (9: duplicate done rejected).
func (f *responsesStreamFSM) textDone(event ResponseTextDoneEvent) error {
	item, part, err := f.partForDelta(event.ItemID, event.OutputIndex, event.ContentIndex, "output_text")
	if err != nil {
		return err
	}
	if part.partPhase != partPhaseAdded {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"duplicate or misplaced output_text.done for %q at index %d",
			event.ItemID,
			event.ContentIndex,
		))
	}
	// The snapshot must reconcile with the accumulated text (16).
	accumulated := ""
	if builder := item.text[event.ContentIndex]; builder != nil {
		accumulated = builder.String()
	}
	if accumulated != event.Text {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"output_text.done text does not match the accumulated text for %q at index %d",
			event.ItemID,
			event.ContentIndex,
		))
	}
	part.partPhase = partPhaseValueDone
	item.parts[event.ContentIndex] = part
	return nil
}

// refusalDelta accumulates refusal text.
func (f *responsesStreamFSM) refusalDelta(event ResponseRefusalDeltaEvent) error {
	item, part, err := f.partForDelta(event.ItemID, event.OutputIndex, event.ContentIndex, "refusal")
	if err != nil {
		return err
	}
	if part.partPhase != partPhaseAdded {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"refusal.delta after done for %q at index %d",
			event.ItemID,
			event.ContentIndex,
		))
	}
	if err := f.budget.addSemanticBytes(int64(len(event.Delta))); err != nil {
		return upstreamWireError(UpstreamResponses, http.StatusOK, err)
	}
	builder, ok := item.refusal[event.ContentIndex]
	if !ok {
		builder = &strings.Builder{}
		item.refusal[event.ContentIndex] = builder
	}
	builder.WriteString(event.Delta)
	return nil
}

// refusalDone finalizes the accumulated refusal.
func (f *responsesStreamFSM) refusalDone(event ResponseRefusalDoneEvent) error {
	item, part, err := f.partForDelta(event.ItemID, event.OutputIndex, event.ContentIndex, "refusal")
	if err != nil {
		return err
	}
	if part.partPhase != partPhaseAdded {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"duplicate or misplaced refusal.done for %q at index %d",
			event.ItemID,
			event.ContentIndex,
		))
	}
	accumulated := ""
	if builder := item.refusal[event.ContentIndex]; builder != nil {
		accumulated = builder.String()
	}
	if accumulated != event.Refusal {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"refusal.done text does not match the accumulated refusal for %q at index %d",
			event.ItemID,
			event.ContentIndex,
		))
	}
	part.partPhase = partPhaseValueDone
	item.parts[event.ContentIndex] = part
	return nil
}

// argumentsDelta accumulates function-call arguments.
func (f *responsesStreamFSM) argumentsDelta(event ResponseFunctionCallArgumentsDeltaEvent) error {
	item, err := f.functionCallItem(event.ItemID, event.OutputIndex)
	if err != nil {
		return err
	}
	phase := item.calls[event.ItemID]
	if phase != callPhaseAdded {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"function_call_arguments.delta after done for %q",
			event.ItemID,
		))
	}
	if err := f.budget.addSemanticBytes(int64(len(event.Delta))); err != nil {
		return upstreamWireError(UpstreamResponses, http.StatusOK, err)
	}
	item.arguments.WriteString(event.Delta)
	return nil
}

// argumentsDone finalizes the accumulated arguments (9: duplicate done
// rejected).
func (f *responsesStreamFSM) argumentsDone(event ResponseFunctionCallArgumentsDoneEvent) error {
	item, err := f.functionCallItem(event.ItemID, event.OutputIndex)
	if err != nil {
		return err
	}
	phase := item.calls[event.ItemID]
	if phase != callPhaseAdded {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"duplicate or misplaced function_call_arguments.done for %q",
			event.ItemID,
		))
	}
	// The done snapshot carries model-generated arguments preserved
	// byte-exact: any string is legal and invalid model output is never an
	// upstream defect (review-z commit 2). The render-side reconciliation
	// remains the snapshot-vs-accumulated authority (16).
	item.calls[event.ItemID] = callPhaseArgumentsDone
	return nil
}

// reasoningAdded opens a reasoning summary part.
func (f *responsesStreamFSM) reasoningAdded(event ResponseReasoningSummaryPartAddedEvent) error {
	item, err := f.reasoningItem(event.ItemID, event.OutputIndex)
	if err != nil {
		return err
	}
	if item.reasoning != reasoningNone {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"duplicate reasoning summary part for %q",
			event.ItemID,
		))
	}
	if err := f.budget.addStateEntries(1); err != nil {
		return upstreamWireError(UpstreamResponses, http.StatusOK, err)
	}
	item.reasoning = reasoningPartAdded
	return nil
}

// reasoningDelta accumulates reasoning summary text.
func (f *responsesStreamFSM) reasoningDelta(event ResponseReasoningSummaryTextDeltaEvent) error {
	item, err := f.reasoningItem(event.ItemID, event.OutputIndex)
	if err != nil {
		return err
	}
	if item.reasoning != reasoningPartAdded {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"reasoning summary delta outside an open part for %q",
			event.ItemID,
		))
	}
	if err := f.budget.addSemanticBytes(int64(len(event.Delta))); err != nil {
		return upstreamWireError(UpstreamResponses, http.StatusOK, err)
	}
	return nil
}

// reasoningTextDone finalizes the reasoning summary text.
func (f *responsesStreamFSM) reasoningTextDone(event ResponseReasoningSummaryTextDoneEvent) error {
	item, err := f.reasoningItem(event.ItemID, event.OutputIndex)
	if err != nil {
		return err
	}
	if item.reasoning != reasoningPartAdded {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"reasoning summary text done outside an open part for %q",
			event.ItemID,
		))
	}
	item.reasoning = reasoningValueDone
	return nil
}

// reasoningDone closes the reasoning summary part.
func (f *responsesStreamFSM) reasoningDone(event ResponseReasoningSummaryPartDoneEvent) error {
	item, err := f.reasoningItem(event.ItemID, event.OutputIndex)
	if err != nil {
		return err
	}
	if item.reasoning != reasoningValueDone {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"reasoning summary part done before value done for %q",
			event.ItemID,
		))
	}
	item.reasoning = reasoningPartDone
	return nil
}

// terminalEvent validates the terminal envelope: identity drift (15), open
// items (14), and events after the terminal (17).
func (f *responsesStreamFSM) terminalEvent(envelope ResponseEnvelope, wantStatus string) error {
	if f.terminal {
		return upstreamWireError(UpstreamResponses, http.StatusOK, errors.New(
			"duplicate terminal event",
		))
	}
	// 15: terminal envelope identity drift.
	if envelope.ID != f.identityID || envelope.Model != f.identityModel ||
		envelope.CreatedAt != f.identityCreatedAt {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"terminal envelope identity %q/%q/%v does not match response.created %q/%q/%v",
			envelope.ID,
			envelope.Model,
			envelope.CreatedAt,
			f.identityID,
			f.identityModel,
			f.identityCreatedAt,
		))
	}
	if envelope.Status != wantStatus {
		return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"terminal envelope status = %q, want %q",
			envelope.Status,
			wantStatus,
		))
	}
	// 14: terminal SUCCESS while any item is open. Failed (abort) terminals
	// accept open items (an upstream abort may cut the stream at any point);
	// the render machine additionally rejects open items at its incomplete
	// terminal itself, so net enforcement holds there too.
	if wantStatus == "completed" {
		for itemID, item := range f.items {
			if item.itemPhase != itemPhaseDone {
				return upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
					"terminal event while item %q is still open (missing output_item.done)",
					itemID,
				))
			}
		}
	}
	f.terminal = true
	return nil
}

// messageItem resolves a message item for part events (6/13: unknown,
// non-message, or completed items).
func (f *responsesStreamFSM) messageItem(
	itemID string,
	outputIndex, contentIndex int64,
) (*fsmItem, error) {
	item, ok := f.items[itemID]
	if !ok {
		return nil, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"content part event for unknown item %q (no open block)",
			itemID,
		))
	}
	if item.outputIndex != outputIndex {
		return nil, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"content part output index = %d, want %d (observed at output_item.added)",
			outputIndex,
			item.outputIndex,
		))
	}
	if item.kind != "message" {
		return nil, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"content part event for non-message item %q (%s)",
			itemID,
			item.kind,
		))
	}
	if item.itemPhase == itemPhaseDone {
		return nil, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"content part event for completed item %q",
			itemID,
		))
	}
	return item, nil
}

// partForDelta resolves the part targeted by a delta event, rejecting text
// events targeting refusal parts and vice versa (8).
func (f *responsesStreamFSM) partForDelta(
	itemID string,
	outputIndex, contentIndex int64,
	wantKind string,
) (*fsmItem, fsmPart, error) {
	if item, ok := f.items[itemID]; ok && item.outputIndex != outputIndex {
		return nil, fsmPart{}, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"content part output index = %d, want %d (observed at output_item.added)",
			outputIndex,
			item.outputIndex,
		))
	}
	item, err := f.messageItem(itemID, outputIndex, contentIndex)
	if err != nil {
		// A delta for an item the stream never opened has no open part.
		return nil, fsmPart{}, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"no open content block for %q at index %d",
			itemID,
			contentIndex,
		))
	}
	part, exists := item.parts[contentIndex]
	if !exists {
		return nil, fsmPart{}, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"no open content block for %q at index %d",
			itemID,
			contentIndex,
		))
	}
	if part.kind != wantKind {
		return nil, fsmPart{}, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"delta event of kind %q targets a %q part at %q index %d",
			wantKind,
			part.kind,
			itemID,
			contentIndex,
		))
	}
	return item, part, nil
}

// functionCallItem resolves a function-call item for arguments events.
func (f *responsesStreamFSM) functionCallItem(
	itemID string,
	outputIndex int64,
) (*fsmItem, error) {
	item, ok := f.items[itemID]
	if !ok {
		return nil, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"arguments event for unknown item %q",
			itemID,
		))
	}
	if item.outputIndex != outputIndex {
		return nil, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"arguments event output index = %d, want %d (observed at output_item.added)",
			outputIndex,
			item.outputIndex,
		))
	}
	if item.kind != "function_call" {
		return nil, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"arguments event for non-function item %q (%s)",
			itemID,
			item.kind,
		))
	}
	if item.itemPhase == itemPhaseDone {
		return nil, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"arguments event for completed item %q",
			itemID,
		))
	}
	if _, exists := item.calls[itemID]; !exists {
		item.calls[itemID] = callPhaseAdded
		if err := f.budget.addStateEntries(1); err != nil {
			return nil, upstreamWireError(UpstreamResponses, http.StatusOK, err)
		}
	}
	return item, nil
}

// reasoningItem resolves a reasoning item for summary-part events.
func (f *responsesStreamFSM) reasoningItem(
	itemID string,
	outputIndex int64,
) (*fsmItem, error) {
	item, ok := f.items[itemID]
	if !ok {
		// The reasoning events carry the item identity; the official
		// lifecycle sends output_item.added first, but reasoning items may
		// also register implicitly at their first summary event. The
		// output-index uniqueness invariant (5) still applies.
		if prior, exists := f.indexes[outputIndex]; exists && prior != itemID {
			return nil, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
				"duplicate output index %d (items %q and %q)",
				outputIndex,
				prior,
				itemID,
			))
		}
		if err := f.budget.addItem(); err != nil {
			return nil, upstreamWireError(UpstreamResponses, http.StatusOK, err)
		}
		if err := f.budget.addStateEntries(1); err != nil {
			return nil, upstreamWireError(UpstreamResponses, http.StatusOK, err)
		}
		item = &fsmItem{
			outputIndex: outputIndex,
			kind:        "reasoning",
			itemPhase:   itemPhaseDone,
			parts:       make(map[int64]fsmPart),
			calls:       make(map[string]callPhase),
			text:        make(map[int64]*strings.Builder),
			refusal:     make(map[int64]*strings.Builder),
		}
		f.items[itemID] = item
		f.indexes[outputIndex] = itemID
	}
	if item.outputIndex != outputIndex {
		return nil, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"reasoning event for item %q at the wrong output index",
			itemID,
		))
	}
	if item.kind != "reasoning" {
		return nil, upstreamWireError(UpstreamResponses, http.StatusOK, fmt.Errorf(
			"reasoning event for non-reasoning item %q (%s)",
			itemID,
			item.kind,
		))
	}
	return item, nil
}
