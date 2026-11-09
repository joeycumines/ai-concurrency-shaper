package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (s *anthropicResponsesStreamState) outputItemAdded(
	event ResponseOutputItemAddedEvent,
) ([]AnthropicStreamEvent, error) {
	// Item identities are unique across the stream: a duplicate
	// output_item.added for the same item id is corrupt upstream wire.
	itemType := itemTypeName(event.Item)
	itemID := responsesOutputItemID(event.Item)
	if _, exists := s.addedItems[itemID]; exists {
		return nil, s.wireError(fmt.Errorf(
			"duplicate output item %q (%s)",
			itemID,
			itemType,
		))
	}
	if err := s.budget.addItem(); err != nil {
		return nil, s.wireError(err)
	}
	if err := s.budget.addStateEntries(1); err != nil {
		return nil, s.wireError(err)
	}
	s.addedItems[itemID] = anthropicAddedItem{
		outputIndex: event.OutputIndex,
		itemType:    itemType,
	}

	switch item := event.Item.(type) {
	case *ResponsesOutputMessage:
		// Message items open their content blocks via content_part.added.
		// The output-message phase has no Messages representation: it
		// enters the loss decision.
		if err := s.losePhaseOnce(item); err != nil {
			return nil, err
		}
		return nil, nil

	case *ResponsesReasoningOutputItem:
		// With the provider_reasoning_thinking capability, the chat state
		// routes provider reasoning here as a reasoning item whose summary
		// deltas render as native thinking blocks (see the
		// reasoning-summary handlers). Without the capability the item is
		// dropped with the documented loss.
		if !s.capabilities.ProviderReasoningThinking {
			return nil, s.loseReasoningOnce()
		}
		return nil, nil

	case *ResponsesFunctionCallOutputItem:
		// Buffer the tool block start until call ID and name are known.
		if err := s.budget.addToolCall(); err != nil {
			return nil, s.wireError(err)
		}
		if err := s.budget.addStateEntries(1); err != nil {
			return nil, s.wireError(err)
		}
		// Tool-call identity renders into the generated tool_use block start
		// and the terminal reconciliation, so it is semantic state charged
		// against the exchange total exactly like the chat direction's
		// identity (the direct
		// Responses→Anthropic path left identity uncharged).
		if err := s.chargeIdentity(len(item.CallID) + len(item.Name)); err != nil {
			return nil, err
		}
		pending := &pendingToolBlock{
			blockIndex:  s.blockIndex,
			outputIndex: event.OutputIndex,
			itemID:      item.ID,
			callID:      item.CallID,
			name:        item.Name,
		}
		s.blockIndex++
		s.pendingToolStart[item.ID] = pending
		return s.maybeStartToolBlock(pending)

	default:
		return nil, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     "stream[].item.type",
			Feature:  itemTypeName(item),
		}
	}
}

// maybeStartToolBlock starts the tool_use block once the call identity is
// complete — but only when no other content block is open: the Anthropic
// dialect carries exactly one open content block, so a block whose turn has
// not come is deferred with its argument fragments and drained when the open
// block closes. Argument fragments received meanwhile are replayed as
// input_json_delta by the drain (never by later events), so a started block
// never replays anything here.
func (s *anthropicResponsesStreamState) maybeStartToolBlock(
	pending *pendingToolBlock,
) ([]AnthropicStreamEvent, error) {
	if pending.started || pending.callID == "" || pending.name == "" {
		return nil, nil
	}
	if s.anyContentBlockOpen() {
		if err := s.deferToolBlock(pending); err != nil {
			return nil, err
		}
		return nil, nil
	}
	pending.started = true
	s.sawToolUse = true
	return []AnthropicStreamEvent{s.toolBlockStartEvent(pending)}, nil
}

// toolBlockStartEvent builds the content_block_start for a tool block.
func (s *anthropicResponsesStreamState) toolBlockStartEvent(pending *pendingToolBlock) AnthropicStreamEvent {
	callID := pending.callID
	name := pending.name
	return AnthropicStreamEvent{
		Type:  AnthropicStreamEventTypeContentBlockStart,
		Index: new(int(pending.blockIndex)),
		ContentBlock: &AnthropicContentBlock{
			Type:  AnthropicContentBlockTypeToolUse,
			ID:    &callID,
			Name:  &name,
			Input: json.RawMessage("{}"),
		},
	}
}

// anyContentBlockOpen reports whether any content block is currently open:
// a message part (text or refusal), a thinking block, or another started tool
// block.
func (s *anthropicResponsesStreamState) anyContentBlockOpen() bool {
	if len(s.partBlocks) > 0 || s.reasoningBlockIndex != nil {
		return true
	}
	for _, other := range s.pendingToolStart {
		if other.started {
			return true
		}
	}
	return false
}

// deferToolBlock queues a reserved block whose start must wait, charging the
// queue entry against the stream budget exactly once.
func (s *anthropicResponsesStreamState) deferToolBlock(pending *pendingToolBlock) error {
	if pending.deferred {
		return nil
	}
	if err := s.budget.addStateEntries(1); err != nil {
		return s.wireError(err)
	}
	pending.deferred = true
	s.deferredTools = append(s.deferredTools, pending)
	return nil
}

// drainDeferredTools starts the next deferred tool block when no content block
// is open, replaying its buffered fragments and any reconciled done-suffix as
// input_json_delta. When the item was already done while deferred, its stop is
// emitted here and the call is recorded as closed, exactly as the direct path
// would have. A drain cascades while deferred blocks can start and finish in
// the same batch; a block that is not yet done stays open and ends it.
func (s *anthropicResponsesStreamState) drainDeferredTools() ([]AnthropicStreamEvent, error) {
	var events []AnthropicStreamEvent
	// Cascade: a deferred block whose item already finished starts AND stops
	// in this batch, freeing the wire for the next deferred block, so the
	// loop continues while blocks can be drained. A block that is not yet
	// done stays open and ends the cascade.
	for len(s.deferredTools) > 0 && !s.anyContentBlockOpen() {
		pending := s.deferredTools[0]
		s.deferredTools = s.deferredTools[1:]
		pending.deferred = false
		pending.started = true
		s.sawToolUse = true

		events = append(events, s.toolBlockStartEvent(pending))
		for _, fragment := range s.bufferedToolFragments[pending.itemID] {
			partial := fragment
			events = append(events, AnthropicStreamEvent{
				Type:  AnthropicStreamEventTypeContentBlockDelta,
				Index: new(int(pending.blockIndex)),
				Delta: &AnthropicStreamDelta{
					Type:        AnthropicStreamDeltaTypeInputJSONDelta,
					PartialJSON: &partial,
				},
			})
		}
		delete(s.bufferedToolFragments, pending.itemID)
		if pending.doneSuffix != "" {
			partial := pending.doneSuffix
			events = append(events, AnthropicStreamEvent{
				Type:  AnthropicStreamEventTypeContentBlockDelta,
				Index: new(int(pending.blockIndex)),
				Delta: &AnthropicStreamDelta{
					Type:        AnthropicStreamDeltaTypeInputJSONDelta,
					PartialJSON: &partial,
				},
			})
		}
		if !pending.done {
			break
		}
		events = append(events, AnthropicStreamEvent{
			Type:  AnthropicStreamEventTypeContentBlockStop,
			Index: new(int(pending.blockIndex)),
		})
		delete(s.pendingToolStart, pending.itemID)
		arguments := pending.arguments.String()
		if arguments == "" {
			arguments = "{}"
		}
		s.closedToolCalls[pending.itemID] = anthropicClosedToolCall{
			outputIndex: pending.outputIndex,
			callID:      pending.callID,
			name:        pending.name,
			arguments:   arguments,
		}
		s.doneItems[pending.itemID] = struct{}{}
	}
	return events, nil
}

func (s *anthropicResponsesStreamState) outputItemDone(
	event ResponseOutputItemDoneEvent,
) ([]AnthropicStreamEvent, error) {
	var events []AnthropicStreamEvent

	// The done item must reconcile with the observed added item: known
	// identity, matching type, and matching output index. A duplicate done for any item type is corrupt upstream wire.
	itemID := responsesOutputItemID(event.Item)
	if _, done := s.doneItems[itemID]; done {
		return nil, s.wireError(fmt.Errorf(
			"duplicate output item done for %q",
			itemID,
		))
	}
	added, ok := s.addedItems[itemID]
	if !ok {
		return nil, s.wireError(fmt.Errorf(
			"output item done for unknown item %q",
			itemID,
		))
	}
	if got := itemTypeName(event.Item); got != added.itemType {
		return nil, s.wireError(fmt.Errorf(
			"output item done type = %q, want %q (observed at output_item.added)",
			got,
			added.itemType,
		))
	}
	if event.OutputIndex != added.outputIndex {
		return nil, s.wireError(fmt.Errorf(
			"output item done output index = %d, want %d (observed at output_item.added)",
			event.OutputIndex,
			added.outputIndex,
		))
	}

	switch item := event.Item.(type) {
	case *ResponsesOutputMessage:
		// The done message's content must match the incrementally observed
		// parts: same parts, same types, same accumulated text.
		for contentIndex, part := range item.Content {
			key := responsePartKey{ItemID: item.ID, ContentIndex: int64(contentIndex)}
			seenType, seen := s.partsSeen[key]
			if !seen {
				return nil, s.wireError(fmt.Errorf(
					"output item done message part %d was never opened",
					contentIndex,
				))
			}
			partType := outputPartTypeName(part)
			if partType != seenType {
				return nil, s.wireError(fmt.Errorf(
					"output item done message part %d type = %q, want %q",
					contentIndex,
					partType,
					seenType,
				))
			}
			switch value := part.(type) {
			case *ResponsesOutputText:
				if accumulated := s.accumulatedText(key); accumulated != value.Text {
					return nil, s.wireError(fmt.Errorf(
						"output item done message part %d text does not match the accumulated text",
						contentIndex,
					))
				}
			case *ResponsesOutputRefusal:
				if accumulated := s.accumulatedRefusal(key); accumulated != value.Refusal {
					return nil, s.wireError(fmt.Errorf(
						"output item done message part %d refusal does not match the accumulated refusal",
						contentIndex,
					))
				}
			}
		}
		if len(item.Content) != s.partCounts[item.ID] {
			return nil, s.wireError(fmt.Errorf(
				"output item done message content count does not match the observed parts",
			))
		}
		s.doneItems[item.ID] = struct{}{}
		return nil, nil
	case *ResponsesReasoningOutputItem:
		// Reasoning items were dropped on add; identity and type already
		// reconciled above.
		s.doneItems[item.ID] = struct{}{}
		return nil, nil
	}

	call, ok := event.Item.(*ResponsesFunctionCallOutputItem)
	if !ok {
		return nil, nil
	}

	pending, ok := s.pendingToolStart[call.ID]
	if !ok {
		return nil, s.wireError(fmt.Errorf("tool block for item %q was never started", call.ID))
	}
	// Identity comes from the item-added lifecycle: the added item must have
	// carried the call identity, and the done snapshot must not drift from it.
	if pending.callID == "" || pending.name == "" {
		return nil, s.wireError(fmt.Errorf(
			"tool block for item %q was added without call identity",
			call.ID,
		))
	}
	if call.CallID != pending.callID {
		return nil, s.wireError(fmt.Errorf(
			"output item done call id = %q, want %q (observed at output_item.added)",
			call.CallID,
			pending.callID,
		))
	}
	if call.Name != pending.name {
		return nil, s.wireError(fmt.Errorf(
			"output item done name = %q, want %q (observed at output_item.added)",
			call.Name,
			pending.name,
		))
	}
	// The done item's status is optional on the pinned wire (the official
	// fixture omits it); a PRESENT status other than completed is a
	// contradiction with the done snapshot.
	if call.Status != "" && call.Status != ResponsesItemCompleted {
		return nil, s.wireError(fmt.Errorf(
			"output item done status = %q, want completed",
			call.Status,
		))
	}
	startEvents, err := s.maybeStartToolBlock(pending)
	if err != nil {
		return nil, err
	}
	events = append(events, startEvents...)

	// Reconcile the done snapshot's arguments (a JSON object, enforced) with
	// the accumulated argument buffer: an empty or partial buffer is
	// completed by the snapshot — never substituted with "{}" — and a
	// conflicting snapshot is corrupt upstream wire. The reconciled
	// arguments are delivered to the client as input_json_delta fragments
	// so the tool input can never collapse to an empty object or drift from
	// the done snapshot.
	arguments, suffix, err := s.reconcileToolArguments(pending, call.Arguments)
	if err != nil {
		if _, ok := errors.AsType[*UnrepresentableError](err); ok {
			return nil, err
		}
		return nil, s.wireError(fmt.Errorf("tool block for item %q: %w", call.ID, err))
	}
	if err := validateFinalToolInput(arguments); err != nil {
		// Anthropic tool_use.input requires an object: invalid
		// model-generated arguments are a LOCAL unrepresentable output,
		// never corrupt upstream wire.
		return nil, &UnrepresentableError{
			Protocol: "anthropic",
			Path:     "content_block.input",
			Detail:   fmt.Sprintf("tool block for item %q: %v", call.ID, err),
		}
	}

	if pending.deferred && !pending.started {
		// The block is still waiting for another open block to close: hold
		// the reconciled bytes and the done fact; the drain emits the start,
		// the buffered fragments, the suffix and the stop in order.
		pending.done = true
		pending.doneSuffix += suffix
		return events, nil
	}

	if suffix != "" {
		partial := suffix
		events = append(events, AnthropicStreamEvent{
			Type:  AnthropicStreamEventTypeContentBlockDelta,
			Index: new(int(pending.blockIndex)),
			Delta: &AnthropicStreamDelta{
				Type:        AnthropicStreamDeltaTypeInputJSONDelta,
				PartialJSON: &partial,
			},
		})
	}

	events = append(events, AnthropicStreamEvent{
		Type:  AnthropicStreamEventTypeContentBlockStop,
		Index: new(int(pending.blockIndex)),
	})
	// The block is now closed; the terminal must not stop it again.
	delete(s.pendingToolStart, call.ID)
	s.closedToolCalls[call.ID] = anthropicClosedToolCall{
		outputIndex: pending.outputIndex,
		callID:      pending.callID,
		name:        pending.name,
		arguments:   arguments,
	}
	s.doneItems[call.ID] = struct{}{}

	// The closed block may have freed the wire for the next deferred block.
	drain, err := s.drainDeferredTools()
	if err != nil {
		return nil, err
	}
	events = append(events, drain...)

	// The message envelope's content stays empty: content blocks arrive via
	// content_block_start events (the official contract); message_start is
	// already marshaled with an empty content array.
	return events, nil
}

// reconcileToolArguments reconciles the final arguments snapshot (a JSON
// object, enforced) with the accumulated argument buffer. The buffer must be
// a byte-prefix of the snapshot (an empty buffer is the degenerate prefix) —
// the accumulated deltas and the final snapshot then agree — or, when both
// parse as JSON objects, the two must be semantically equal (key order and
// whitespace ignored). The buffer is completed with the snapshot, and the
// returned suffix is the byte range the upstream never streamed as deltas:
// the caller must emit it as one input_json_delta so the client's assembled
// tool input equals the snapshot. A conflicting snapshot is an error:
// arguments can never be silently replaced.
func (s *anthropicResponsesStreamState) reconcileToolArguments(
	pending *pendingToolBlock,
	snapshot string,
) (string, string, error) {
	doneArgs, err := decodeJSONObject(snapshot)
	if err != nil {
		// The snapshot is the upstream's final claim about model-generated
		// arguments: a non-object is a LOCAL unrepresentable output, never
		// corrupt upstream wire. Callers pass this
		// typed error through unwrapped.
		return "", "", &UnrepresentableError{
			Protocol: "anthropic",
			Path:     "content_block.input",
			Detail:   fmt.Sprintf("final arguments are not a JSON object: %v", err),
		}
	}
	accumulated := pending.arguments.String()
	if strings.HasPrefix(snapshot, accumulated) {
		// The buffer is a byte-prefix of the snapshot (possibly equal): the
		// snapshot completes it. The suffix is the part the upstream never
		// streamed as a delta — and the snapshot bytes retained for the
		// exchange's lifetime must count against the exchange total like any
		// other accumulation.
		suffix := snapshot[len(accumulated):]
		s.totalAccumulated += int64(len(suffix))
		if s.totalAccumulated > maxStreamTotalAccumulatedBytes {
			return "", "", fmt.Errorf(
				"responses stream accumulated tool arguments exceed the exchange total of %d bytes",
				maxStreamTotalAccumulatedBytes,
			)
		}
		pending.arguments.Reset()
		pending.arguments.WriteString(snapshot)
		return snapshot, suffix, nil
	}
	// Not a byte-prefix: the buffer may still be semantically equal (e.g.
	// the upstream re-serialized the object with different whitespace).
	accArgs, accErr := decodeJSONObject(accumulated)
	if accErr == nil && jsonObjectsEqual(accArgs, doneArgs) {
		return accumulated, "", nil
	}
	return "", "", errors.New(
		"final arguments do not match the accumulated argument deltas",
	)
}

// checkAccumulated bounds one accumulated semantic builder (text, refusal,
// or tool arguments) by the per-item bound and the exchange-wide total
// a corrupt upstream must not amplify memory without
// limit across many parts.
func (s *anthropicResponsesStreamState) checkAccumulated(builder *strings.Builder, added int, what string) error {
	// The bounds are checked BEFORE the caller writes the delta into the
	// builder: a rejected delta must not mutate the accumulated state (or
	// pay the allocation) at all.
	if builder.Len()+added > maxStreamAccumulatedBytes {
		return s.wireError(fmt.Errorf(
			"responses stream accumulated %s exceeds %d bytes",
			what,
			maxStreamAccumulatedBytes,
		))
	}
	if s.totalAccumulated+int64(added) > maxStreamTotalAccumulatedBytes {
		return s.wireError(fmt.Errorf(
			"responses stream accumulated %s exceeds the exchange total of %d bytes",
			what,
			maxStreamTotalAccumulatedBytes,
		))
	}
	s.totalAccumulated += int64(added)
	return nil
}

// accumulatedText returns the accumulated text of a content part, or the
// empty string when the part never accumulated a delta. The empty string is
// the reconciliation baseline: a zero-delta part reconciles with an empty
// done snapshot and contradicts any non-empty one.
func (s *anthropicResponsesStreamState) accumulatedText(key responsePartKey) string {
	if builder := s.textBufs[key]; builder != nil {
		return builder.String()
	}
	return ""
}

// accumulatedRefusal returns the accumulated refusal of a content part, or
// the empty string when the part never accumulated a delta.
func (s *anthropicResponsesStreamState) accumulatedRefusal(key responsePartKey) string {
	if builder := s.refusalBufs[key]; builder != nil {
		return builder.String()
	}
	return ""
}

// jsonObjectsEqual compares two decoded JSON objects semantically: key order
// and whitespace are ignored; values are compared byte-exactly.
func jsonObjectsEqual(a, b map[string]json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for key, aValue := range a {
		bValue, ok := b[key]
		if !ok || !bytes.Equal(aValue, bValue) {
			return false
		}
	}
	return true
}

// validateFinalToolInput enforces that the completed tool input parses as
// exactly one JSON object.
func validateFinalToolInput(arguments string) error {
	if _, err := decodeJSONObject(arguments); err != nil {
		return fmt.Errorf("final tool input is not a JSON object: %w", err)
	}
	return nil
}
