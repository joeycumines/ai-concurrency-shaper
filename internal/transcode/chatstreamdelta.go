package transcode

import (
	"errors"
	"fmt"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// convertChunk converts one chunk, leaving failure latching to Convert.
func (s *chatResponsesStreamState) convertChunk(
	chunk ChatStreamResponse,
) ([]ResponsesSSEEvent, error) {
	if err := s.budget.addEvent(); err != nil {
		return nil, s.wireError(err)
	}
	if s.sawFinish {
		// Phase 2: accept only accounting redeliveries and fold their
		// totals into the terminal envelope's usage. A redelivery is
		// still part of the stream: chunk identity must remain stable
		// (enforced when the chunk carries it), and a service tier on it
		// enters the same loss/reject decision as on any other chunk.
		if chunk.Usage != nil && len(chunk.Choices) == 0 {
			if err := s.absorbPhase2Accounting(chunk); err != nil {
				return nil, err
			}
			return nil, nil
		}
		// A repeated terminal chunk carries the recorded finish reason on
		// the same single choice with an insubstantial delta (role-only
		// or empty-string fields): it is the gateway's usage redelivery
		// in another envelope, never new output. Fold its accounting and
		// absorb it. Anything substantive after the terminal (content,
		// reasoning, refusal, tool calls, a different finish reason)
		// stays corrupt upstream wire.
		if isRepeatedTerminalChunk(chunk, s.finishReason) {
			if err := s.absorbPhase2Accounting(chunk); err != nil {
				return nil, err
			}
			return nil, nil
		}
		return nil, s.wireError(errors.New("chat stream chunk after finish_reason"))
	}

	var events []ResponsesSSEEvent

	if !s.started {
		// Pin the chunk identity and apply the upstream creation time BEFORE
		// building the created/in_progress envelopes, so response.created,
		// response.in_progress, and the terminal envelope share one
		// created_at.
		if chunk.Created != 0 {
			s.createdAt = float64(chunk.Created)
		}
		s.chunkID = chunk.ID
		s.chunkModel = chunk.Model
		envelope := s.baseEnvelope("in_progress")
		events = append(events,
			s.builder.Created(envelope),
			s.builder.InProgress(envelope),
		)
		s.started = true
	} else {
		// Stable chunk identity across the stream: the strict chunk decode
		// guarantees id and model are always present, so a mismatch is an
		// upstream protocol error.
		if chunk.ID != s.chunkID {
			return nil, s.wireError(fmt.Errorf(
				"chat stream chunk id %q does not match the first chunk id %q",
				chunk.ID,
				s.chunkID,
			))
		}
		if chunk.Model != s.chunkModel {
			return nil, s.wireError(fmt.Errorf(
				"chat stream chunk model %q does not match the first chunk model %q",
				chunk.Model,
				s.chunkModel,
			))
		}
	}

	if chunk.Usage != nil {
		if err := s.noteDerivedUsageTotal(chunk); err != nil {
			return nil, err
		}
		if err := s.loseUnknownUsageComponentsOnce(chunk.Usage); err != nil {
			return nil, err
		}
		converted, clamp := chatUsageToResponsesUsage(chunk.Usage, chunk.DerivedUsageTotal)
		if err := s.usageClampNotes.note(&s.report, "usage", clamp); err != nil {
			return nil, err
		}
		s.usage = converted
	}

	// The chunk envelope's service tier and per-choice token log
	// probabilities cannot be reproduced in the client dialect: their
	// presence enters the explicit loss/reject decision instead of being
	// silently dropped, recorded exactly once per
	// stream.
	if chunk.ServiceTier != nil {
		if err := s.loseServiceTierOnce(); err != nil {
			return nil, err
		}
	}

	// n=1: a chunk with more than one choice is an upstream protocol error.
	if len(chunk.Choices) > 1 {
		return nil, s.wireError(errors.New("chat stream chunk has more than one choice; n=1 required"))
	}

	for _, choice := range chunk.Choices {
		// n=1: the single choice must be index 0.
		if choice.Index != 0 {
			return nil, s.wireError(fmt.Errorf(
				"chat stream chunk choice index = %d; n=1 requires index 0",
				choice.Index,
			))
		}
		if choice.LogProbs != nil {
			if err := s.loseLogprobsOnce(); err != nil {
				return nil, err
			}
		}
		if choice.Delta != nil {
			deltaEvents, err := s.convertDelta(choice.Delta)
			if err != nil {
				return nil, err
			}
			events = append(events, deltaEvents...)
		}
		if choice.FinishReason != nil {
			terminal, err := s.finish(*choice.FinishReason)
			if err != nil {
				return nil, err
			}
			// Hold the item-closing events; the terminal envelope is built
			// at release so the optional usage tail is reflected in its
			// usage. Released ONLY by the [DONE] sentinel; EOF after finish_reason without [DONE] is a truncation.
			s.heldTerminal = terminal
			s.sawFinish = true
		}
	}

	return events, nil
}

// convertDelta converts one Chat delta into Responses events.
func (s *chatResponsesStreamState) convertDelta(
	delta *ChatStreamDelta,
) ([]ResponsesSSEEvent, error) {
	var events []ResponsesSSEEvent

	// Provider plaintext reasoning arrives in two provider spellings:
	// `reasoning` (OpenRouter style) and `reasoning_content` (the
	// DeepSeek/Qwen convention real open-weights gateways stream). They are
	// one logical field; a delta carrying two non-empty spellings at once is
	// contradictory upstream wire, never an ordered merge. The resolved text
	// runs the identical capability-gated block below, reported under the
	// actual field path.
	reasoningText, reasoningPath, both := resolveChatReasoningSpelling(
		delta.Reasoning,
		delta.ReasoningContent,
		"choices[].delta.reasoning",
		"choices[].delta.reasoning_content",
	)
	if both {
		return nil, s.wireError(errors.New(chatProviderReasoningBothDetail))
	}

	if reasoningText != "" {
		// Provider plaintext reasoning follows the capability ladder:
		// provider_reasoning_thinking maps it to native reasoning events
		// (the anthropic state renders them as thinking blocks carrying the
		// marker signature; operator-adjudicated design, 2026-09-07);
		// provider_reasoning_text maps it to ordinary text; neither is a
		// documented loss. Every report entry is recorded exactly once per
		// stream, never once per delta.
		if s.capabilities.ProviderReasoningThinking {
			// A reasoning item accumulates its own summary text; the
			// anthropic state machine owns the thinking-block lifecycle.
			// Thinking takes precedence over the ordinary-text mapping when
			// both capabilities are enabled (the more faithful rendering;
			// Claude Code displays it with the native thinking UI).
			//
			// A reasoning delta arriving while the LAST item is a message
			// (reasoning resumed after content/tool output) opens a NEW
			// reasoning item: the previous thinking block was sealed at the
			// transition, and appending into it would reconcile against a
			// sealed block. Each burst therefore renders as its own
			// thinking block with its own marker signature — the Anthropic
			// dialect's sequential-blocks contract permits exactly that.
			reasoningClosed := false
			if len(s.items) > 0 {
				if reasoning, ok := s.items[len(s.items)-1].item.(*ResponsesReasoningOutputItem); ok &&
					reasoning.Status == ResponsesItemCompleted {
					// The reasoning item was closed at a reasoning→content
					// transition; resumed reasoning renders as a NEW
					// thinking block (the previous one is sealed on the
					// wire).
					reasoningClosed = true
				}
			}
			if len(s.items) == 0 || !s.isReasoningItem(&s.items[len(s.items)-1]) || reasoningClosed {
				// A thinking block must not open while the preceding message's
				// content block is still open: seal the message item at the
				// transition, the way the reasoning item is sealed on the way
				// into content and tool output. A started tool block must be
				// sealed too — otherwise a tool_use block stays open while the
				// thinking block opens.
				closedMessage, err := s.closeOpenMessageItem()
				if err != nil {
					return nil, err
				}
				events = append(events, closedMessage...)
				events = append(events, s.closePendingToolCalls()...)
				if err := s.budget.addItem(); err != nil {
					return nil, s.wireError(err)
				}
				if err := s.budget.addStateEntries(1); err != nil {
					return nil, s.wireError(err)
				}
				reasoning := &ResponsesReasoningOutputItem{
					ID:      s.ctx.IDs.New("rs_"),
					Type:    "reasoning",
					Status:  ResponsesItemInProgress,
					Summary: []ResponsesReasoningSummary{},
				}
				item := openResponsesItem{
					outputIndex:   s.itemIndex,
					openPartIndex: -1,
					item:          reasoning,
				}
				s.itemIndex++
				s.items = append(s.items, item)
				// The standard lifecycle: output_item.added (a detached
				// snapshot; the live item accumulates summary text through
				// later deltas — ), then the summary
				// events. The anthropic state machine reconciles the
				// output_item.done against this observation.
				snapshot := *reasoning
				snapshot.Summary = []ResponsesReasoningSummary{}
				events = append(events,
					s.builder.OutputItemAdded(item.outputIndex, &snapshot),
					s.builder.ReasoningSummaryPartAdded(
						reasoning.ID,
						item.outputIndex,
						0,
						openairesponses.SummaryTextPart{Type: "summary_text", Text: ""},
					),
				)
			}
			item := &s.items[len(s.items)-1]
			reasoning := item.item.(*ResponsesReasoningOutputItem)
			events = append(events, s.builder.ReasoningSummaryTextDelta(
				reasoning.ID,
				item.outputIndex,
				0,
				reasoningText,
			))
			// The summary text accumulates in a per-item strings.Builder under
			// the same per-item and exchange-total bounds as text and refusal
			// (checkAccumulated), never via += on the summary string. The part
			// stays empty while the item is open; the accumulated text is
			// materialized into the summary when the item closes.
			key := chatPartKey{outputIndex: item.outputIndex, contentIndex: 0}
			buf := s.reasoningBufs[key]
			if buf == nil {
				buf = &strings.Builder{}
				s.reasoningBufs[key] = buf
			}
			// The bound is checked BEFORE the write: a rejected delta must
			// not mutate the accumulated state.
			if err := s.checkAccumulated(buf, len(reasoningText), "reasoning summary text"); err != nil {
				return nil, err
			}
			buf.WriteString(reasoningText)
			if len(reasoning.Summary) == 0 {
				reasoning.Summary = []ResponsesReasoningSummary{{Type: "summary_text", Text: ""}}
			}
			// The encoding note is recorded exactly once per stream, never
			// once per delta, and only after the delta's accumulated charge
			// has committed.
			// The once-per-stream flag is set only after the note commits
			// (decide before commit, record after commit).
			if !s.reasoningReportRecorded {
				if err := s.report.Note(
					FeatureProviderReasoningText,
					reasoningPath,
					"provider reasoning mapped to native thinking blocks (provider_reasoning_thinking encoding)",
				); err != nil {
					return nil, err
				}
				s.reasoningReportRecorded = true
			}
		} else if !s.capabilities.ProviderReasoningText {
			if !s.reasoningReportRecorded {
				if err := s.report.Lose(
					s.policy,
					FeatureProviderReasoningText,
					reasoningPath,
					chatProviderReasoningDroppedDetail,
				); err != nil {
					return nil, err
				}
				s.reasoningReportRecorded = true
			}
		} else {
			item, addedEvents, err := s.openMessageItemForPart("output_text")
			if err != nil {
				return nil, err
			}
			events = append(events, addedEvents...)
			message := item.item.(*ResponsesOutputMessage)
			contentIndex := item.openPartIndex
			events = append(events, s.builder.TextDelta(
				message.ID,
				item.outputIndex,
				contentIndex,
				reasoningText,
			))
			key := chatPartKey{outputIndex: item.outputIndex, contentIndex: contentIndex}
			builder := s.textBufs[key]
			if builder == nil {
				builder = &strings.Builder{}
				s.textBufs[key] = builder
			}
			if err := s.checkAccumulated(builder, len(reasoningText), "text"); err != nil {
				return nil, err
			}
			builder.WriteString(reasoningText)
			// The encoding note is recorded exactly once per stream, never
			// once per delta; the flag is set after the note commits.
			if !s.reasoningReportRecorded {
				if err := s.report.Note(
					FeatureProviderReasoningText,
					reasoningPath,
					chatProviderReasoningMappedDetail,
				); err != nil {
					return nil, err
				}
				s.reasoningReportRecorded = true
			}
		}
	}

	// Reasoning→content/tool transition (provider_reasoning_thinking): the
	// open reasoning item's summary lifecycle must close INLINE — before
	// the content/tool events — so the anthropic state emits
	// signature_delta + content_block_stop for the thinking block before
	// the next block opens. Holding the close until finish() scrambled the
	// order on the wire (text block closed before the thinking block's
	// signature). The reasoning block ABOVE ran first, so a single delta
	// carrying reasoning AND content/tool_calls appends the reasoning to
	// the (possibly newly opened) reasoning item first and only then seals
	// it: the wire order matches the delta's chronological order, and a
	// tool call never starts while a thinking block is still open.
	//
	// The close fires ONLY at a real transition — a delta that actually
	// carries content or tool output. A reasoning-only delta must NOT
	// close the item: sealing it here would fragment contiguous reasoning
	// into one thinking block per delta (one-block-per-delta, operator-
	// observed 2026-09-08 — Claude Code rendered one ∴ fragment per line).
	hasOutput := (delta.Content != nil && *delta.Content != "") ||
		(delta.Refusal != nil && *delta.Refusal != "") ||
		len(delta.ToolCalls) > 0
	if hasOutput {
		closeEvents, err := s.closeOpenReasoningItem()
		if err != nil {
			return nil, err
		}
		events = append(events, closeEvents...)
	}

	if delta.Content != nil && *delta.Content != "" {
		item, addedEvents, err := s.openMessageItemForPart("output_text")
		if err != nil {
			return nil, err
		}
		events = append(events, addedEvents...)
		message := item.item.(*ResponsesOutputMessage)
		contentIndex := item.openPartIndex
		events = append(events, s.builder.TextDelta(
			message.ID,
			item.outputIndex,
			contentIndex,
			*delta.Content,
		))
		key := chatPartKey{outputIndex: item.outputIndex, contentIndex: contentIndex}
		builder := s.textBufs[key]
		if builder == nil {
			builder = &strings.Builder{}
			s.textBufs[key] = builder
		}
		if err := s.checkAccumulated(builder, len(*delta.Content), "text"); err != nil {
			return nil, err
		}
		builder.WriteString(*delta.Content)
	}

	if delta.Refusal != nil && *delta.Refusal != "" {
		item, addedEvents, err := s.openMessageItemForPart("refusal")
		if err != nil {
			return nil, err
		}
		events = append(events, addedEvents...)
		message := item.item.(*ResponsesOutputMessage)
		contentIndex := item.openPartIndex
		events = append(events, s.builder.RefusalDelta(
			message.ID,
			item.outputIndex,
			contentIndex,
			*delta.Refusal,
		))
		key := chatPartKey{outputIndex: item.outputIndex, contentIndex: contentIndex}
		builder := s.refusalBufs[key]
		if builder == nil {
			builder = &strings.Builder{}
			s.refusalBufs[key] = builder
		}
		if err := s.checkAccumulated(builder, len(*delta.Refusal), "refusal"); err != nil {
			return nil, err
		}
		builder.WriteString(*delta.Refusal)
	}

	for _, call := range delta.ToolCalls {
		callEvents, err := s.convertToolCall(call)
		if err != nil {
			return nil, err
		}
		events = append(events, callEvents...)
	}

	return events, nil
}

// openMessageItemForPart returns the open message item, opening a new item or
// a new content part as needed. The returned events are the output_item.added
// and content_part.added events that must be emitted before the delta. The
// open part index on the returned item is the index the delta must target:
// the existing part when the type matches, otherwise a freshly added part.
func (s *chatResponsesStreamState) openMessageItemForPart(
	partType string,
) (*openResponsesItem, []ResponsesSSEEvent, error) {
	// Open a new message item when the last item is not a message, or when it
	// is a message already closed at a message→tool transition
	// (closeOpenMessageItem): content arriving after the tool call renders as
	// a NEW message item, exactly as resumed reasoning renders as a new
	// thinking block, because the closed item is already sealed on the wire.
	reuseTrailing := false
	if len(s.items) > 0 {
		if last := &s.items[len(s.items)-1]; last.isMessage() {
			if message, ok := last.item.(*ResponsesOutputMessage); !ok || message.Status != ResponsesItemCompleted {
				reuseTrailing = true
			}
		}
	}
	if !reuseTrailing {
		// A new content item must not open while a tool call's block is still
		// open: seal the pending calls at the transition.
		closed := s.closePendingToolCalls()
		added, err := s.openMessageItem()
		if err != nil {
			return nil, nil, err
		}
		// The output_item.added event is emitted before the
		// content_part.added of the first part.
		item, itemEvents, err := s.openMessageItemForPartWithEvents(partType, added)
		if err != nil {
			return nil, nil, err
		}
		return item, append(closed, itemEvents...), nil
	}
	return s.openMessageItemForPartWithEvents(partType, nil)
}

// openMessageItemForPartWithEvents continues the item/part bookkeeping,
// prepending any pre-existing added events to the returned batch.
func (s *chatResponsesStreamState) openMessageItemForPartWithEvents(
	partType string,
	prefix []ResponsesSSEEvent,
) (*openResponsesItem, []ResponsesSSEEvent, error) {
	item := &s.items[len(s.items)-1]
	message := item.item.(*ResponsesOutputMessage)

	// Continue the open part when its type matches.
	if item.openPartIndex >= 0 && item.openPartIndex < int64(len(message.Content)) {
		var existingType string
		switch message.Content[item.openPartIndex].(type) {
		case *ResponsesOutputText:
			existingType = "output_text"
		case *ResponsesOutputRefusal:
			existingType = "refusal"
		}
		if existingType == partType {
			return item, prefix, nil
		}
	}

	// Otherwise open a new content part. The Anthropic render carries
	// exactly one open content block, so a new part may open only after the
	// previous part was sealed: close the open part inline (value done +
	// part done) at the part-type transition, the same way the message item
	// is sealed at a message→tool transition. Without this the render would
	// receive two concurrent content_part.added events and emit two
	// interleaved content_block_start events, which the Messages dialect
	// forbids.
	if item.openPartIndex >= 0 && item.openPartIndex < int64(len(message.Content)) {
		events := s.closeOpenPart(item, message)
		prefix = append(prefix, events...)
	}
	if len(message.Content) >= maxStreamPartsPerItem {
		return nil, nil, s.wireError(fmt.Errorf(
			"chat stream content parts per item exceed the exchange bound of %d",
			maxStreamPartsPerItem,
		))
	}
	contentIndex := int64(len(message.Content))
	item.openPartIndex = contentIndex
	switch partType {
	case "output_text":
		message.Content = append(message.Content, &ResponsesOutputText{
			Type:        "output_text",
			Text:        "",
			Annotations: []ResponsesAnnotation{},
		})
		return item, append(prefix, s.builder.ContentPartAdded(
			message.ID,
			item.outputIndex,
			contentIndex,
			&ResponsesStreamOutputTextPart{
				Type:        "output_text",
				Text:        "",
				Annotations: []ResponsesAnnotation{},
			},
		)), nil
	case "refusal":
		message.Content = append(message.Content, &ResponsesOutputRefusal{
			Type:    "refusal",
			Refusal: "",
		})
		return item, append(prefix, s.builder.ContentPartAdded(
			message.ID,
			item.outputIndex,
			contentIndex,
			&ResponsesStreamRefusalPart{Type: "refusal", Refusal: ""},
		)), nil
	default:
		return nil, nil, fmt.Errorf("unknown content part type %q", partType)
	}
}

// openMessageItem opens a new message output item and returns the
// output_item.added event that must be emitted before any part event. The
// event carries a DETACHED snapshot of the message as it exists at creation:
// the live item accumulates content and status through later deltas, and
// sharing the pointer would leak that mutation into the already-emitted
// output_item.added frame.
func (s *chatResponsesStreamState) openMessageItem() ([]ResponsesSSEEvent, error) {
	if len(s.items) >= maxStreamOutputItems {
		return nil, s.wireError(fmt.Errorf(
			"chat stream output items exceed the exchange bound of %d",
			maxStreamOutputItems,
		))
	}
	message := &ResponsesOutputMessage{
		ID:      s.ctx.IDs.New("msg_"),
		Type:    "message",
		Role:    "assistant",
		Status:  ResponsesItemInProgress,
		Content: ResponsesOutputContentParts{},
	}
	item := openResponsesItem{
		outputIndex:   s.itemIndex,
		openPartIndex: -1,
		item:          message,
	}
	s.itemIndex++
	s.items = append(s.items, item)

	// The snapshot must never alias the live content slice: the live item's
	// Content is appended to by openMessageItemForPartWithEvents before the
	// batch is marshaled. The remaining scalar fields were copied by the
	// struct assignment above and never change for the lifetime of the item.
	snapshot := *message
	snapshot.Content = ResponsesOutputContentParts{}
	return []ResponsesSSEEvent{
		s.builder.OutputItemAdded(
			item.outputIndex,
			&snapshot,
		),
	}, nil
}

// closeOpenReasoningItem closes the trailing open reasoning item inline at a
// reasoning→content/tool transition (provider_reasoning_thinking): summary
// text.done, part.done, then output_item.done — the same event sequence
// finish() emits, but AT THE TRANSITION so the anthropic state seals the
// thinking block (signature_delta + content_block_stop) before the next
// content block opens. No-op when the last item is not an open reasoning
// item. The closed item stays in s.items with a completed status: the
// terminal reconciliation still sees it, and resumed reasoning opens a NEW
// item (a completed reasoning item is never appended to).
func (s *chatResponsesStreamState) closeOpenReasoningItem() ([]ResponsesSSEEvent, error) {
	if len(s.items) == 0 {
		return nil, nil
	}
	item := &s.items[len(s.items)-1]
	reasoning, ok := item.item.(*ResponsesReasoningOutputItem)
	if !ok || reasoning.Status != ResponsesItemInProgress {
		return nil, nil
	}
	reasoning.Status = ResponsesItemCompleted
	// Materialize the accumulated summary text from the per-item builder,
	// exactly as finish() does for text and refusal parts.
	summaryText := ""
	if buf := s.reasoningBufs[chatPartKey{outputIndex: item.outputIndex, contentIndex: 0}]; buf != nil {
		summaryText = buf.String()
	}
	if len(reasoning.Summary) == 0 {
		reasoning.Summary = []ResponsesReasoningSummary{{Type: "summary_text", Text: summaryText}}
	} else {
		reasoning.Summary[0].Text = summaryText
	}
	return []ResponsesSSEEvent{
		s.builder.ReasoningSummaryTextDone(
			reasoning.ID,
			item.outputIndex,
			0,
			summaryText,
		),
		s.builder.ReasoningSummaryPartDone(
			reasoning.ID,
			item.outputIndex,
			0,
			openairesponses.SummaryTextPart{
				Type: "summary_text",
				Text: summaryText,
			},
		),
		s.builder.OutputItemDone(
			item.outputIndex,
			reasoning,
		),
	}, nil
}

// closeOpenPart seals one open content part of a message item inline at a
// part-type transition (text → refusal or refusal → text): the value done
// event, then the part done event, exactly as finish() emits them, but AT THE
// TRANSITION so the next content_part.added never opens a second concurrent
// content block (the Anthropic dialect carries exactly one). The part stays
// in message.Content with its materialized value; the item is NOT closed.
func (s *chatResponsesStreamState) closeOpenPart(
	item *openResponsesItem,
	message *ResponsesOutputMessage,
) []ResponsesSSEEvent {
	contentIndex := item.openPartIndex
	if contentIndex < 0 || contentIndex >= int64(len(message.Content)) {
		return nil
	}
	key := chatPartKey{outputIndex: item.outputIndex, contentIndex: contentIndex}
	var events []ResponsesSSEEvent
	switch value := message.Content[contentIndex].(type) {
	case *ResponsesOutputText:
		if builder := s.textBufs[key]; builder != nil {
			value.Text = builder.String()
		}
		events = append(events,
			s.builder.TextDone(message.ID, item.outputIndex, contentIndex, value.Text),
			s.builder.ContentPartDone(message.ID, item.outputIndex, contentIndex,
				&ResponsesStreamOutputTextPart{
					Type:        "output_text",
					Text:        value.Text,
					Annotations: []ResponsesAnnotation{},
				}),
		)
	case *ResponsesOutputRefusal:
		if builder := s.refusalBufs[key]; builder != nil {
			value.Refusal = builder.String()
		}
		events = append(events,
			s.builder.RefusalDone(message.ID, item.outputIndex, contentIndex, value.Refusal),
			s.builder.ContentPartDone(message.ID, item.outputIndex, contentIndex,
				&ResponsesStreamRefusalPart{
					Type:    "refusal",
					Refusal: value.Refusal,
				}),
		)
	}
	if item.sealedParts == nil {
		item.sealedParts = make(map[int64]struct{})
	}
	item.sealedParts[contentIndex] = struct{}{}
	item.openPartIndex = -1
	// The sealed part's accumulator is released: the value was materialized
	// into the part above, and keeping the builder would leak its bytes if
	// the key were ever reused.
	delete(s.textBufs, key)
	delete(s.refusalBufs, key)
	return events
}

// closeOpenMessageItem closes the trailing open message item inline at a
// message→tool transition: each content part's done event, then
// output_item.done — the same sequence finish() emits, but AT THE TRANSITION
// so the next content block never opens before this one is sealed (anthropic
// requires every block to be closed before the next opens). No-op when the
// last item is not an open message item. The closed item stays in s.items
// with a completed status: the terminal reconciliation still sees it, and
// content arriving after the tool call opens a NEW message item (a completed
// message item is never appended to).
func (s *chatResponsesStreamState) closeOpenMessageItem() ([]ResponsesSSEEvent, error) {
	if len(s.items) == 0 {
		return nil, nil
	}
	item := &s.items[len(s.items)-1]
	message, ok := item.item.(*ResponsesOutputMessage)
	if !ok || message.Status != ResponsesItemInProgress {
		return nil, nil
	}
	var events []ResponsesSSEEvent
	for contentIndex, part := range message.Content {
		// A part already sealed inline at a part-type transition
		// (closeOpenPart) must not be closed twice here: emitting its done
		// events again would duplicate a part.done on the wire (the
		// Anthropic state fails on the second content_part.done), exactly
		// as finish() skips sealed parts.
		if _, sealed := item.sealedParts[int64(contentIndex)]; sealed {
			continue
		}
		key := chatPartKey{outputIndex: item.outputIndex, contentIndex: int64(contentIndex)}
		switch value := part.(type) {
		case *ResponsesOutputText:
			if builder := s.textBufs[key]; builder != nil {
				value.Text = builder.String()
			}
			events = append(events,
				s.builder.TextDone(message.ID, item.outputIndex, int64(contentIndex), value.Text),
				s.builder.ContentPartDone(message.ID, item.outputIndex, int64(contentIndex),
					&ResponsesStreamOutputTextPart{
						Type:        "output_text",
						Text:        value.Text,
						Annotations: []ResponsesAnnotation{},
					}),
			)
		case *ResponsesOutputRefusal:
			if builder := s.refusalBufs[key]; builder != nil {
				value.Refusal = builder.String()
			}
			events = append(events,
				s.builder.RefusalDone(message.ID, item.outputIndex, int64(contentIndex), value.Refusal),
				s.builder.ContentPartDone(message.ID, item.outputIndex, int64(contentIndex),
					&ResponsesStreamRefusalPart{
						Type:    "refusal",
						Refusal: value.Refusal,
					}),
			)
		}
	}
	if item.sealedParts == nil {
		item.sealedParts = make(map[int64]struct{})
	}
	for contentIndex := range message.Content {
		// The part's accumulator is released with its seal: the value was
		// materialized into the part above (or at the inline seal), and
		// keeping the builder would leak its bytes if the key were reused.
		key := chatPartKey{outputIndex: item.outputIndex, contentIndex: int64(contentIndex)}
		delete(s.textBufs, key)
		delete(s.refusalBufs, key)
		item.sealedParts[int64(contentIndex)] = struct{}{}
	}
	item.openPartIndex = -1
	message.Status = ResponsesItemCompleted
	events = append(events, s.builder.OutputItemDone(item.outputIndex, message))
	return events, nil
}
