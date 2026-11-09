package transcode

import (
	"errors"
	"fmt"
	"strings"
)

func (s *anthropicResponsesStreamState) contentPartAdded(
	event ResponseContentPartAddedEvent,
) ([]AnthropicStreamEvent, error) {
	// A content part must be owned by a MESSAGE item observed via
	// output_item.added — function-call and reasoning items own no content
	// parts — and each (item, content index) identity is unique.
	added, ok := s.addedItems[event.ItemID]
	if !ok {
		return nil, s.wireError(fmt.Errorf(
			"content part for unknown item %q",
			event.ItemID,
		))
	}
	if added.itemType != "message" {
		return nil, s.wireError(fmt.Errorf(
			"content part for non-message item %q (%s)",
			event.ItemID,
			added.itemType,
		))
	}
	if event.OutputIndex != added.outputIndex {
		return nil, s.wireError(fmt.Errorf(
			"content part output index = %d, want %d (observed at output_item.added)",
			event.OutputIndex,
			added.outputIndex,
		))
	}
	key := responsePartKey{ItemID: event.ItemID, ContentIndex: event.ContentIndex}
	if _, exists := s.partsSeen[key]; exists {
		return nil, s.wireError(fmt.Errorf(
			"duplicate content part %q at index %d",
			event.ItemID,
			event.ContentIndex,
		))
	}
	if s.partCounts[event.ItemID] >= maxStreamPartsPerItem {
		return nil, s.wireError(fmt.Errorf(
			"responses stream content parts per item exceed the exchange bound of %d",
			maxStreamPartsPerItem,
		))
	}
	var events []AnthropicStreamEvent
	switch event.Part.(type) {
	case *ResponsesStreamOutputTextPart, *ResponsesStreamRefusalPart:
		// Refusal becomes ordinary text content.
		events = append(events, AnthropicStreamEvent{
			Type:  AnthropicStreamEventTypeContentBlockStart,
			Index: new(int(s.blockIndex)),
			ContentBlock: &AnthropicContentBlock{
				Type: AnthropicContentBlockTypeText,
				Text: new(""),
			},
		})
		if err := s.budget.addPart(); err != nil {
			return nil, s.wireError(err)
		}
		if err := s.budget.addStateEntries(2); err != nil {
			return nil, s.wireError(err)
		}
		s.partBlocks[key] = s.blockIndex
		s.partsSeen[key] = partTypeName(event.Part)
		s.partCounts[event.ItemID]++
		s.blockIndex++
	default:
		return nil, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     "stream[].part.type",
			Feature:  partTypeName(event.Part),
		}
	}
	return events, nil
}

func (s *anthropicResponsesStreamState) textDelta(
	event ResponseTextDeltaEvent,
) ([]AnthropicStreamEvent, error) {
	key := responsePartKey{ItemID: event.ItemID, ContentIndex: event.ContentIndex}
	index, ok := s.partBlocks[key]
	if !ok {
		return nil, s.wireError(errors.New("text delta with no open content block"))
	}
	if err := s.checkPartOutputIndex(key, event.OutputIndex); err != nil {
		return nil, err
	}
	// A text delta must target a text part: forwarding it to a refusal block
	// would drift the emitted block from every snapshot.
	if seenType := s.partsSeen[key]; seenType != "output_text" {
		return nil, s.wireError(fmt.Errorf(
			"text delta for part of type %q",
			seenType,
		))
	}
	builder := s.textBufs[key]
	if builder == nil {
		builder = &strings.Builder{}
		s.textBufs[key] = builder
	}
	if err := s.checkAccumulated(builder, len(event.Delta), "text"); err != nil {
		return nil, err
	}
	builder.WriteString(event.Delta)
	text := event.Delta
	return []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockDelta,
		Index: new(int(index)),
		Delta: &AnthropicStreamDelta{
			Type: AnthropicStreamDeltaTypeTextDelta,
			Text: &text,
		},
	}}, nil
}

func (s *anthropicResponsesStreamState) textDone(
	event ResponseTextDoneEvent,
) ([]AnthropicStreamEvent, error) {
	key := responsePartKey{ItemID: event.ItemID, ContentIndex: event.ContentIndex}
	if _, ok := s.partBlocks[key]; !ok {
		return nil, s.wireError(errors.New("text done with no open content block"))
	}
	if err := s.checkPartOutputIndex(key, event.OutputIndex); err != nil {
		return nil, err
	}
	if seenType := s.partsSeen[key]; seenType != "output_text" {
		return nil, s.wireError(fmt.Errorf(
			"text done for part of type %q",
			seenType,
		))
	}
	// The done text must match the accumulated deltas: a mismatch means the
	// upstream's fragments and its final snapshot disagree. A part that accumulated nothing reconciles with the empty
	// string.
	if accumulated := s.accumulatedText(key); accumulated != event.Text {
		return nil, s.wireError(errors.New(
			"text done does not match the accumulated text",
		))
	}
	return nil, nil
}

func (s *anthropicResponsesStreamState) contentPartDone(
	event ResponseContentPartDoneEvent,
) ([]AnthropicStreamEvent, error) {
	key := responsePartKey{ItemID: event.ItemID, ContentIndex: event.ContentIndex}
	index, ok := s.partBlocks[key]
	if !ok {
		return nil, s.wireError(errors.New("content part done with no open block"))
	}
	if err := s.checkPartOutputIndex(key, event.OutputIndex); err != nil {
		return nil, err
	}
	// The done part must match the opened part's type.
	if got := partTypeName(event.Part); got != s.partsSeen[key] {
		return nil, s.wireError(fmt.Errorf(
			"content part done type = %q, want %q (observed at content_part.added)",
			got,
			s.partsSeen[key],
		))
	}
	delete(s.partBlocks, key)
	events := []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockStop,
		Index: new(int(index)),
	}}
	// The closed part may have freed the wire for the next deferred block.
	drain, err := s.drainDeferredTools()
	if err != nil {
		return nil, err
	}
	return append(events, drain...), nil
}

// checkPartOutputIndex verifies an event targeting a content part carries
// the owning item's output index.
func (s *anthropicResponsesStreamState) checkPartOutputIndex(
	key responsePartKey,
	outputIndex int64,
) error {
	added, ok := s.addedItems[key.ItemID]
	if !ok {
		return s.wireError(fmt.Errorf(
			"event for unknown item %q",
			key.ItemID,
		))
	}
	if outputIndex != added.outputIndex {
		return s.wireError(fmt.Errorf(
			"event output index = %d, want %d (observed at output_item.added)",
			outputIndex,
			added.outputIndex,
		))
	}
	return nil
}

func (s *anthropicResponsesStreamState) functionArgumentsDelta(
	event ResponseFunctionCallArgumentsDeltaEvent,
) ([]AnthropicStreamEvent, error) {
	pending, ok := s.pendingToolStart[event.ItemID]
	if !ok {
		return nil, s.wireError(fmt.Errorf("arguments delta for unknown item %q", event.ItemID))
	}
	if event.OutputIndex != pending.outputIndex {
		return nil, s.wireError(fmt.Errorf(
			"arguments delta output index = %d, want %d (observed at output_item.added)",
			event.OutputIndex,
			pending.outputIndex,
		))
	}
	// Ensure the block started before emitting input_json_delta.
	startEvents, err := s.maybeStartToolBlock(pending)
	if err != nil {
		return nil, err
	}
	// The accumulated arguments are emitted as one generated downstream
	// frame at output_item.done: bound their cumulative size and the exchange-wide total BEFORE the write.
	if err := s.checkAccumulated(&pending.arguments, len(event.Delta), "tool arguments"); err != nil {
		return nil, err
	}
	pending.arguments.WriteString(event.Delta)
	// A block that never started (the added item lacked call identity, which
	// is corrupt wire rejected at output_item.done) must not receive an
	// input_json_delta without a content_block_start: the bytes are still
	// accumulated so the eventual rejection is consistent. A deferred block
	// buffers each fragment (charged against the state-entry budget) for the
	// drain to replay in arrival order.
	if !pending.started {
		if pending.deferred {
			if err := s.budget.addStateEntries(1); err != nil {
				return nil, s.wireError(err)
			}
			s.bufferedToolFragments[event.ItemID] = append(s.bufferedToolFragments[event.ItemID], event.Delta)
		}
		return startEvents, nil
	}
	partial := event.Delta
	out := []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockDelta,
		Index: new(int(pending.blockIndex)),
		Delta: &AnthropicStreamDelta{
			Type:        AnthropicStreamDeltaTypeInputJSONDelta,
			PartialJSON: &partial,
		},
	}}
	return append(startEvents, out...), nil
}

func (s *anthropicResponsesStreamState) functionArgumentsDone(
	event ResponseFunctionCallArgumentsDoneEvent,
) ([]AnthropicStreamEvent, error) {
	pending, ok := s.pendingToolStart[event.ItemID]
	if !ok {
		return nil, s.wireError(fmt.Errorf("arguments done for unknown item %q", event.ItemID))
	}
	if event.OutputIndex != pending.outputIndex {
		return nil, s.wireError(fmt.Errorf(
			"arguments done output index = %d, want %d (observed at output_item.added)",
			event.OutputIndex,
			pending.outputIndex,
		))
	}
	// Reconcile the done snapshot with the accumulated buffer instead of
	// silently replacing it: a mismatch is corrupt upstream wire, and any
	// snapshot bytes the upstream never streamed as deltas are emitted as
	// one input_json_delta so the client's assembled input equals the
	// snapshot. Identity comes from the item-added
	// lifecycle.
	_, suffix, err := s.reconcileToolArguments(pending, event.Arguments)
	if err != nil {
		if _, ok := errors.AsType[*UnrepresentableError](err); ok {
			return nil, err
		}
		return nil, s.wireError(fmt.Errorf("tool block for item %q: %w", event.ItemID, err))
	}
	events, err := s.maybeStartToolBlock(pending)
	if err != nil {
		return nil, err
	}
	// A block that never started (the added item lacked call identity) must
	// not receive an input_json_delta without a content_block_start; the
	// suffix is dropped with the exchange, which the done reconciliation
	// rejects.
	if pending.started && suffix != "" {
		partial := suffix
		events = append(events, AnthropicStreamEvent{
			Type:  AnthropicStreamEventTypeContentBlockDelta,
			Index: new(int(pending.blockIndex)),
			Delta: &AnthropicStreamDelta{
				Type:        AnthropicStreamDeltaTypeInputJSONDelta,
				PartialJSON: &partial,
			},
		})
	} else if pending.deferred && suffix != "" {
		// The block has not started yet: hold the reconciled bytes for the
		// drain, which emits them after the buffered fragments.
		pending.doneSuffix += suffix
	}
	return events, nil
}

func (s *anthropicResponsesStreamState) refusalDelta(
	event ResponseRefusalDeltaEvent,
) ([]AnthropicStreamEvent, error) {
	key := responsePartKey{ItemID: event.ItemID, ContentIndex: event.ContentIndex}
	index, ok := s.partBlocks[key]
	if !ok {
		return nil, s.wireError(errors.New("refusal delta with no open content block"))
	}
	if err := s.checkPartOutputIndex(key, event.OutputIndex); err != nil {
		return nil, err
	}
	// A refusal delta must target a refusal part: forwarding it to a text
	// block would drift the emitted block from every snapshot.
	if seenType := s.partsSeen[key]; seenType != "refusal" {
		return nil, s.wireError(fmt.Errorf(
			"refusal delta for part of type %q",
			seenType,
		))
	}
	builder := s.refusalBufs[key]
	if builder == nil {
		builder = &strings.Builder{}
		s.refusalBufs[key] = builder
	}
	if err := s.checkAccumulated(builder, len(event.Delta), "refusal"); err != nil {
		return nil, err
	}
	builder.WriteString(event.Delta)
	text := event.Delta
	return []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockDelta,
		Index: new(int(index)),
		Delta: &AnthropicStreamDelta{
			Type: AnthropicStreamDeltaTypeTextDelta,
			Text: &text,
		},
	}}, nil
}

func (s *anthropicResponsesStreamState) refusalDone(
	event ResponseRefusalDoneEvent,
) ([]AnthropicStreamEvent, error) {
	key := responsePartKey{ItemID: event.ItemID, ContentIndex: event.ContentIndex}
	if _, ok := s.partBlocks[key]; !ok {
		return nil, s.wireError(errors.New("refusal done with no open content block"))
	}
	if err := s.checkPartOutputIndex(key, event.OutputIndex); err != nil {
		return nil, err
	}
	if seenType := s.partsSeen[key]; seenType != "refusal" {
		return nil, s.wireError(fmt.Errorf(
			"refusal done for part of type %q",
			seenType,
		))
	}
	// The done refusal must match the accumulated deltas. A part that accumulated nothing reconciles with the empty string.
	if accumulated := s.accumulatedRefusal(key); accumulated != event.Refusal {
		return nil, s.wireError(errors.New(
			"refusal done does not match the accumulated refusal",
		))
	}
	return nil, nil
}

func (s *anthropicResponsesStreamState) gateEnvelopePhases(
	envelope ResponseEnvelope,
) error {
	for _, item := range envelope.Output {
		message, ok := item.(*ResponsesOutputMessage)
		if !ok {
			continue
		}
		if err := s.losePhaseOnce(message); err != nil {
			return err
		}
	}
	return nil
}

// loseControlsOnce records the pinned envelope-controls loss exactly once
// per stream: the created envelope, the completed envelope, or an
// incomplete envelope may each carry them, and the first sighting decides
// An envelope without controls never triggers a
// loss.
func (s *anthropicResponsesStreamState) loseControlsOnce(
	envelope ResponseEnvelope,
) error {
	if s.controlsGated {
		return nil
	}
	var present []string
	for _, control := range []struct {
		name    string
		present bool
	}{
		{"background", envelope.Background != nil},
		{"max_tool_calls", envelope.MaxToolCalls != nil},
		{"prompt", envelope.Prompt != nil},
		{"prompt_cache_key", envelope.PromptCacheKey != nil},
		{"safety_identifier", envelope.SafetyIdentifier != nil},
	} {
		if control.present {
			present = append(present, control.name)
		}
	}
	if len(present) == 0 {
		return nil
	}
	s.controlsGated = true
	return s.report.Lose(
		s.policy,
		FeatureResponsesControls,
		"response",
		"the Responses envelope controls "+strings.Join(present, ", ")+
			" cannot be reproduced in an Anthropic stream",
	)
}

// losePhaseOnce records the output-message phase loss exactly once per
// item.
func (s *anthropicResponsesStreamState) losePhaseOnce(
	message *ResponsesOutputMessage,
) error {
	if message == nil || message.Phase == "" {
		return nil
	}
	if _, gated := s.phaseGated[message.ID]; gated {
		return nil
	}
	s.phaseGated[message.ID] = struct{}{}
	return s.report.Lose(
		s.policy,
		FeatureOutputPhase,
		"output[].phase",
		"the output message phase cannot be reproduced in an Anthropic stream",
	)
}

// loseReasoningOnce records the reasoning loss exactly once per stream
// OpenAI reasoning is never synthesized as Anthropic
// thinking, and dropping it silently would bypass the loss policy that the
// non-streaming path consults.
func (s *anthropicResponsesStreamState) loseReasoningOnce() error {
	if s.reasoningLossRecorded {
		return nil
	}
	err := s.report.Lose(
		s.policy,
		FeatureReasoningSummary,
		"stream[].reasoning",
		"OpenAI reasoning cannot be reproduced in an Anthropic stream",
	)
	if err != nil {
		return err
	}
	s.reasoningLossRecorded = true
	return nil
}

// reasoningPartAdded opens the native thinking content block for a
// reasoning item (provider_reasoning_thinking). The block start carries the
// empty thinking text and empty signature; the signature delta (the marker)
// arrives immediately before content_block_stop per the Anthropic streaming
// contract.
func (s *anthropicResponsesStreamState) reasoningPartAdded(
	event ResponseReasoningSummaryPartAddedEvent,
) ([]AnthropicStreamEvent, error) {
	if s.reasoningBlockIndex != nil {
		// The Anthropic dialect cannot represent two concurrently-open
		// content blocks: the Responses FSM tracks reasoning phase per
		// item, so interleaved part lifecycles across items are FSM-legal
		// but unrenderable — reject as corrupt upstream wire rather than
		// misattribute deltas into the wrong block. Observed live: the
		// previous behavior panicked on the nil'd index once the first
		// part had closed.
		return nil, s.wireError(fmt.Errorf(
			"reasoning summary part added for %q while a thinking block is open",
			event.ItemID,
		))
	}
	if err := s.budget.addStateEntries(1); err != nil {
		return nil, s.wireError(err)
	}
	if err := s.budget.addPart(); err != nil {
		return nil, s.wireError(err)
	}
	thinking := ""
	s.reasoningBlockIndex = new(int(int(s.blockIndex)))
	s.blockIndex++
	emptySignature := ""
	return []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockStart,
		Index: new(int(*s.reasoningBlockIndex)),
		ContentBlock: &AnthropicContentBlock{
			Type:      AnthropicContentBlockTypeThinking,
			Thinking:  &thinking,
			Signature: &emptySignature,
		},
	}}, nil
}

// reasoningTextDelta streams one thinking_delta into the open thinking
// block.
func (s *anthropicResponsesStreamState) reasoningTextDelta(
	event ResponseReasoningSummaryTextDeltaEvent,
) ([]AnthropicStreamEvent, error) {
	if s.reasoningBlockIndex == nil {
		return nil, s.wireError(errors.New(
			"reasoning summary delta with no open thinking block",
		))
	}
	// The event budget was already charged once for this event in
	// convertEvent; charging it again here would halve the effective event
	// budget for thinking streams.
	key := responsePartKey{ItemID: event.ItemID, ContentIndex: event.SummaryIndex}
	builder := s.thinkingBufs[key]
	if builder == nil {
		builder = &strings.Builder{}
		s.thinkingBufs[key] = builder
	}
	if err := s.checkAccumulated(builder, len(event.Delta), "thinking text"); err != nil {
		return nil, err
	}
	builder.WriteString(event.Delta)
	delta := event.Delta
	return []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockDelta,
		Index: new(int(*s.reasoningBlockIndex)),
		Delta: &AnthropicStreamDelta{
			Type:     AnthropicStreamDeltaTypeThinkingDelta,
			Thinking: &delta,
		},
	}}, nil
}

// reasoningPartDone closes the thinking block: the signature delta (the
// marker signature) precedes content_block_stop exactly as the Anthropic
// streaming contract requires.
func (s *anthropicResponsesStreamState) reasoningPartDone(
	event ResponseReasoningSummaryPartDoneEvent,
) ([]AnthropicStreamEvent, error) {
	if s.reasoningBlockIndex == nil {
		return nil, s.wireError(errors.New(
			"reasoning summary part done with no open thinking block",
		))
	}
	signature := SyntheticThinkingSignature
	events := []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockDelta,
		Index: new(int(*s.reasoningBlockIndex)),
		Delta: &AnthropicStreamDelta{
			Type:      AnthropicStreamDeltaTypeSignatureDelta,
			Signature: &signature,
		},
	}, {
		Type:  AnthropicStreamEventTypeContentBlockStop,
		Index: new(int(*s.reasoningBlockIndex)),
	}}
	s.reasoningBlockIndex = nil
	// The closed thinking block may have freed the wire for the next
	// deferred block.
	drain, err := s.drainDeferredTools()
	if err != nil {
		return nil, err
	}
	return append(events, drain...), nil
}
