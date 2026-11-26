package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/anthropicmessages"
)

// anthropicUpstreamResponsesStreamState converts Anthropic Messages stream
// events into the typed Responses events the Gemini client state consumes.
// Each visible content block becomes one output item, in block order.
// Discarded blocks retain their kind and lifecycle state: omitting their
// payload does not permit an invalid delta or an unclosed block. The Gemini
// state also validates the emitted sequence with the Responses lifecycle
// machine, so an ordering defect fails the stream instead of reaching the client.
//
// Usage is reported only when both the message_start and the final
// message_delta carried a usage object. Otherwise the terminal envelope has no
// usage, and the Gemini renderer records every count as unknown.
type anthropicUpstreamResponsesStreamState struct {
	responseID string
	model      string
	createdAt  float64
	policy     LossPolicy
	report     ConversionReport
	budget     streamBudget

	started      bool
	stopped      bool
	sequence     int64
	nextOutput   int64
	blocks       map[int64]*anthropicStreamBlock
	startUsage   *anthropicmessages.Usage
	finalUsage   *anthropicmessages.Usage
	stopReason   anthropicmessages.StopReason
	sawStopValue bool
	// The content phase ends at the first message_delta, including one that
	// carries only usage metadata. Later metadata cannot reopen content.
	contentPhaseEnded bool
	// startStopReason records a stop reason already present on the
	// message_start envelope. Start metadata is authoritative: a later
	// terminal under a different reason contradicts it and is malformed
	// wire, so the refusal cannot be silently discarded by a success.
	startStopReason   *anthropicmessages.StopReason
	sawStartStopValue bool
	// streamError latches the first failure of any kind: an upstream error
	// event, a refused conversion, any error the conversion returns. Every
	// later call and FinalizeEOF propagate it, so a failed stream can never
	// report a clean terminal.
	streamError error
	// authenticatedThinkingLossRecorded, serverToolsLossRecorded and
	// toolChangesLossRecorded gate the losses for content blocks a Gemini
	// stream cannot carry to once per stream: a provider may repeat the same
	// kind of block many times.
	authenticatedThinkingLossRecorded bool
	serverToolsLossRecorded           bool
	toolChangesLossRecorded           bool
}

// anthropicStreamBlock tracks every content block until the stream ends. A
// visible block retains its output identity and accumulated text or arguments.
// An ignored block retains only its kind and closure; the same lifecycle checks
// apply even though it contributes no client-visible output.
type anthropicStreamBlock struct {
	kind        anthropicmessages.ContentBlockType
	ignored     bool
	outputIndex int64
	itemID      string
	callID      string
	name        string
	text        strings.Builder
	args        strings.Builder
	closed      bool
}

// wireError types a stream failure as an upstream wire error, so a budget
// violation caused by the upstream's stream is classified as an upstream
// fault rather than a local conversion failure.
func (s *anthropicUpstreamResponsesStreamState) wireError(err error) error {
	return upstreamWireError(UpstreamMessages, http.StatusOK, err)
}

func newAnthropicUpstreamResponsesStreamState(
	responseID string,
	model string,
	policy LossPolicy,
) *anthropicUpstreamResponsesStreamState {
	return &anthropicUpstreamResponsesStreamState{
		responseID: responseID,
		model:      model,
		createdAt:  float64(time.Now().Unix()),
		policy:     policy,
		budget:     newStreamBudget(),
		blocks:     make(map[int64]*anthropicStreamBlock),
	}
}

// Convert processes one decoded Anthropic event into zero or more typed
// Responses events. The first failure of any kind latches: every later call
// propagates it, so a stream that failed can never report a clean terminal.
func (s *anthropicUpstreamResponsesStreamState) Convert(
	event anthropicmessages.StreamEvent,
) ([]ResponsesSSEEvent, error) {
	if s.streamError != nil {
		return nil, s.streamError
	}
	events, err := s.convertEvent(event)
	if err != nil {
		s.streamError = err
		return nil, err
	}
	return events, nil
}

// convertEvent converts one event, leaving failure latching to Convert.
func (s *anthropicUpstreamResponsesStreamState) convertEvent(
	event anthropicmessages.StreamEvent,
) ([]ResponsesSSEEvent, error) {
	if s.stopped {
		return nil, upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			fmt.Errorf("anthropic stream event %q after the stream ended", event.Type),
		)
	}
	if err := s.budget.addEvent(); err != nil {
		return nil, s.wireError(err)
	}
	if err := validateAnthropicEventPayload(event); err != nil {
		return nil, s.wireError(err)
	}
	switch event.Type {
	case anthropicmessages.StreamEventTypeMessageStart:
		return s.messageStart(event)
	case anthropicmessages.StreamEventTypeContentBlockStart:
		return s.blockStart(event)
	case anthropicmessages.StreamEventTypeContentBlockDelta:
		return s.blockDelta(event)
	case anthropicmessages.StreamEventTypeContentBlockStop:
		return s.blockStop(event)
	case anthropicmessages.StreamEventTypeMessageDelta:
		return s.messageDelta(event)
	case anthropicmessages.StreamEventTypeMessageStop:
		return s.messageStop()
	case anthropicmessages.StreamEventTypePing:
		return nil, nil
	case anthropicmessages.StreamEventTypeError:
		if event.Error == nil {
			return nil, upstreamWireError(
				UpstreamMessages,
				http.StatusOK,
				errors.New("anthropic error event has no error object"),
			)
		}
		message := event.Error.Message
		if message == "" {
			message = fmt.Sprintf(
				"the upstream sent an error event (type %s)", event.Error.Type,
			)
		}
		s.stopped = true
		// The error event returns the client-visible error event with a nil
		// error, so it latches itself; the wrapper never sees it.
		s.streamError = upstreamStreamFailure(message)
		return []ResponsesSSEEvent{ResponseErrorEvent{
			Type:           "error",
			SequenceNumber: s.nextSequence(),
			Code:           event.Error.Type,
			Message:        message,
		}}, nil
	default:
		return nil, upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			fmt.Errorf("anthropic stream event type %q is unknown", event.Type),
		)
	}
}

func (s *anthropicUpstreamResponsesStreamState) messageStart(
	event anthropicmessages.StreamEvent,
) ([]ResponsesSSEEvent, error) {
	if s.started {
		return nil, upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			errors.New("anthropic message_start repeated"),
		)
	}
	if event.Message == nil {
		return nil, upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			errors.New("anthropic message_start has no message"),
		)
	}
	if len(event.Message.Content) > 0 {
		return nil, s.wireError(errors.New("anthropic message_start carries content; content must arrive through content_block events"))
	}
	s.started = true
	s.startUsage = event.Message.Usage
	if event.Message.StopReason != nil {
		reason := *event.Message.StopReason
		s.startStopReason = &reason
		s.sawStartStopValue = true
	}
	return []ResponsesSSEEvent{ResponseCreatedEvent{
		Type:           "response.created",
		SequenceNumber: s.nextSequence(),
		Response:       s.envelope("in_progress", nil, nil),
	}}, nil
}

func (s *anthropicUpstreamResponsesStreamState) blockStart(
	event anthropicmessages.StreamEvent,
) ([]ResponsesSSEEvent, error) {
	if err := s.requireOpenable(event); err != nil {
		return nil, err
	}
	index := int64(*event.Index)
	block := event.ContentBlock
	switch block.Type {
	case anthropicmessages.ContentBlockTypeText:
		if len(block.Citations) > 0 {
			return nil, &UnrepresentableError{
				Protocol: "messages",
				Path:     "content_block.citations",
				Detail:   "text citations have no Gemini response form",
			}
		}
		if err := s.budget.addItem(); err != nil {
			return nil, s.wireError(err)
		}
		if err := s.budget.addPart(); err != nil {
			return nil, s.wireError(err)
		}
		tracked := &anthropicStreamBlock{
			kind:        block.Type,
			outputIndex: s.nextOutput,
			itemID:      fmt.Sprintf("msg_%d", s.nextOutput),
		}
		s.nextOutput++
		s.blocks[index] = tracked
		events := []ResponsesSSEEvent{
			ResponseOutputItemAddedEvent{
				Type:           "response.output_item.added",
				SequenceNumber: s.nextSequence(),
				OutputIndex:    tracked.outputIndex,
				Item: &ResponsesOutputMessage{
					ID: tracked.itemID, Type: "message", Role: "assistant",
					Status: ResponsesItemInProgress, Content: ResponsesOutputContentParts{},
				},
			},
			ResponseContentPartAddedEvent{
				Type:           "response.content_part.added",
				SequenceNumber: s.nextSequence(),
				ItemID:         tracked.itemID,
				OutputIndex:    tracked.outputIndex,
				ContentIndex:   0,
				Part: &ResponsesStreamOutputTextPart{
					Type: "output_text", Text: "", Annotations: []ResponsesAnnotation{},
				},
			},
		}
		if text := derefString(block.Text); text != "" {
			delta, err := s.textDelta(tracked, text)
			if err != nil {
				return nil, err
			}
			events = append(events, delta)
		}
		return events, nil

	case anthropicmessages.ContentBlockTypeToolUse:
		if input := strings.TrimSpace(string(block.Input)); input != "" && input != "{}" {
			return nil, upstreamWireError(
				UpstreamMessages,
				http.StatusOK,
				errors.New("anthropic tool_use start carries input; arguments must arrive as input_json_delta"),
			)
		}
		if err := s.budget.addItem(); err != nil {
			return nil, s.wireError(err)
		}
		if err := s.budget.addToolCall(); err != nil {
			return nil, s.wireError(err)
		}
		callID, name := derefString(block.ID), derefString(block.Name)
		// The call identity is repeated in the done events and the terminal
		// envelope, so it is retained semantic state like the arguments.
		if err := s.budget.addSemanticBytes(int64(len(callID) + len(name))); err != nil {
			return nil, s.wireError(err)
		}
		tracked := &anthropicStreamBlock{
			kind:        block.Type,
			outputIndex: s.nextOutput,
			itemID:      fmt.Sprintf("fc_%d", s.nextOutput),
			callID:      callID,
			name:        name,
		}
		s.nextOutput++
		s.blocks[index] = tracked
		return []ResponsesSSEEvent{ResponseOutputItemAddedEvent{
			Type:           "response.output_item.added",
			SequenceNumber: s.nextSequence(),
			OutputIndex:    tracked.outputIndex,
			Item: &ResponsesFunctionCallOutputItem{
				ID: tracked.itemID, Type: "function_call", Status: ResponsesItemInProgress,
				CallID: tracked.callID, Name: tracked.name, Arguments: "",
			},
		}}, nil

	case anthropicmessages.ContentBlockTypeThinking,
		anthropicmessages.ContentBlockTypeRedactedThinking:
		if err := s.budget.addStateEntries(1); err != nil {
			return nil, s.wireError(err)
		}
		if !s.authenticatedThinkingLossRecorded {
			s.authenticatedThinkingLossRecorded = true
			if err := s.report.Lose(
				s.policy,
				FeatureAuthenticatedThinking,
				"content_block",
				"the provider's authenticated thinking block cannot be reproduced in a Gemini stream",
			); err != nil {
				return nil, err
			}
		}
		s.blocks[index] = &anthropicStreamBlock{kind: block.Type, ignored: true}
		return nil, nil

	case anthropicmessages.ContentBlockTypeMCPToolListing:
		// The MCP connector's fetched tool list (mcp-client-2026-09-15). It
		// pins provider-side tool-search state, carries no conversation
		// content, and was never requested by the client, so the omission is a
		// Note rather than an operator-refusable loss.
		if err := s.budget.addStateEntries(1); err != nil {
			return nil, s.wireError(err)
		}
		if !s.toolChangesLossRecorded {
			s.toolChangesLossRecorded = true
			if err := s.report.Note(
				FeatureAnthropicToolChanges,
				"content_block",
				"an MCP tool listing pins the provider's tool search state and cannot be reproduced in a client stream",
			); err != nil {
				return nil, err
			}
		}
		s.blocks[index] = &anthropicStreamBlock{kind: block.Type, ignored: true}
		return nil, nil

	case anthropicmessages.ContentBlockTypeToolAddition,
		anthropicmessages.ContentBlockTypeToolRemoval:
		// Request-only blocks: their presence on the response surface is
		// contradictory upstream wire, never a feature to absorb.
		return nil, s.wireError(fmt.Errorf(
			"content block %d is a request-only tool change block", index,
		))

	case anthropicmessages.ContentBlockTypeServerToolUse,
		anthropicmessages.ContentBlockTypeWebSearchToolResult,
		anthropicmessages.ContentBlockTypeMCPToolUse,
		anthropicmessages.ContentBlockTypeMCPToolResult,
		anthropicmessages.ContentBlockTypeCodeExecution,
		anthropicmessages.ContentBlockTypeCodeExecutionResult,
		anthropicmessages.ContentBlockTypeContainerUpload:
		if err := s.budget.addStateEntries(1); err != nil {
			return nil, s.wireError(err)
		}
		if !s.serverToolsLossRecorded {
			s.serverToolsLossRecorded = true
			if err := s.report.Lose(
				s.policy,
				FeatureAnthropicServerTools,
				"content_block",
				"server-side tool output cannot be reproduced in a Gemini stream",
			); err != nil {
				return nil, err
			}
		}
		s.blocks[index] = &anthropicStreamBlock{kind: block.Type, ignored: true}
		return nil, nil

	default:
		return nil, upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			fmt.Errorf("anthropic content block type %q is unknown", block.Type),
		)
	}
}

// requireOpenable checks the preconditions shared by block starts: the stream
// has begun, the event names a block index, the block is present, and the
// index is not reused.
func (s *anthropicUpstreamResponsesStreamState) requireOpenable(event anthropicmessages.StreamEvent) error {
	if !s.started {
		return upstreamWireError(UpstreamMessages, http.StatusOK, errors.New("anthropic content block before message_start"))
	}
	if s.contentPhaseEnded {
		return s.wireError(errors.New("anthropic content block after message_delta"))
	}
	if event.Index == nil || event.ContentBlock == nil {
		return upstreamWireError(UpstreamMessages, http.StatusOK, errors.New("anthropic content_block_start has no index or block"))
	}
	index := int64(*event.Index)
	if index < 0 {
		return s.wireError(fmt.Errorf("anthropic content block index %d is negative", index))
	}
	if _, reused := s.blocks[index]; reused {
		return upstreamWireError(UpstreamMessages, http.StatusOK, fmt.Errorf("anthropic content block index %d reused", index))
	}
	return nil
}

func (s *anthropicUpstreamResponsesStreamState) blockDelta(
	event anthropicmessages.StreamEvent,
) ([]ResponsesSSEEvent, error) {
	if !s.started {
		return nil, upstreamWireError(UpstreamMessages, http.StatusOK, errors.New("anthropic content delta before message_start"))
	}
	if s.contentPhaseEnded {
		return nil, s.wireError(errors.New("anthropic content delta after message_delta"))
	}
	if event.Index == nil || event.Delta == nil {
		return nil, upstreamWireError(UpstreamMessages, http.StatusOK, errors.New("anthropic content_block_delta has no index or delta"))
	}
	index := int64(*event.Index)
	tracked, ok := s.blocks[index]
	if !ok || tracked.closed {
		return nil, upstreamWireError(UpstreamMessages, http.StatusOK, fmt.Errorf("anthropic delta for unopened block %d", index))
	}
	delta := event.Delta
	if err := validateAnthropicBlockDelta(tracked.kind, *delta); err != nil {
		return nil, s.wireError(fmt.Errorf("content block %d: %w", index, err))
	}
	if tracked.ignored {
		// The frame/event bounds apply to discarded deltas. Their payloads
		// are not retained, so they consume no accumulated semantic bytes.
		return nil, nil
	}
	switch delta.Type {
	case anthropicmessages.StreamDeltaTypeTextDelta:
		out, err := s.textDelta(tracked, derefString(delta.Text))
		if err != nil {
			return nil, err
		}
		return []ResponsesSSEEvent{out}, nil

	case anthropicmessages.StreamDeltaTypeInputJSONDelta:
		partial := derefString(delta.PartialJSON)
		if err := s.budget.addSemanticBytes(int64(len(partial))); err != nil {
			return nil, s.wireError(err)
		}
		tracked.args.WriteString(partial)
		return []ResponsesSSEEvent{ResponseFunctionCallArgumentsDeltaEvent{
			Type:           "response.function_call_arguments.delta",
			SequenceNumber: s.nextSequence(),
			ItemID:         tracked.itemID,
			OutputIndex:    tracked.outputIndex,
			Delta:          partial,
		}}, nil

	case anthropicmessages.StreamDeltaTypeCitationsDelta:
		return nil, &UnrepresentableError{
			Protocol: "messages",
			Path:     "content_block_delta.citation",
			Detail:   "text citations have no Gemini response form",
		}

	default:
		return nil, upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			fmt.Errorf("anthropic delta type %q is not valid on a visible block", delta.Type),
		)
	}
}

// validateAnthropicBlockDelta keeps the delta union consistent with the block
// that receives it. Thinking and server-tool arguments may be discarded under
// an approved loss, but their deltas still need the correct kind and payload.
// Blocks whose complete payload arrives in content_block_start accept no delta.
func validateAnthropicBlockDelta(
	kind anthropicmessages.ContentBlockType,
	delta anthropicmessages.StreamDelta,
) error {
	remaining := delta
	remaining.Type = ""
	var field string
	var present bool
	switch delta.Type {
	case anthropicmessages.StreamDeltaTypeTextDelta:
		if kind == anthropicmessages.ContentBlockTypeText {
			field, present = "text", delta.Text != nil
			remaining.Text = nil
		}
	case anthropicmessages.StreamDeltaTypeInputJSONDelta:
		switch kind {
		case anthropicmessages.ContentBlockTypeToolUse,
			anthropicmessages.ContentBlockTypeServerToolUse,
			anthropicmessages.ContentBlockTypeMCPToolUse,
			anthropicmessages.ContentBlockTypeCodeExecution:
			// code_execution is an admitted compatibility spelling for an
			// invocation; the documented API uses server_tool_use with that
			// name. Keep its argument deltas while checking their shape.
			field, present = "partial_json", delta.PartialJSON != nil
			remaining.PartialJSON = nil
		}
	case anthropicmessages.StreamDeltaTypeThinkingDelta:
		if kind == anthropicmessages.ContentBlockTypeThinking {
			field, present = "thinking", delta.Thinking != nil
			remaining.Thinking = nil
		}
	case anthropicmessages.StreamDeltaTypeSignatureDelta:
		if kind == anthropicmessages.ContentBlockTypeThinking {
			field, present = "signature", delta.Signature != nil
			remaining.Signature = nil
		}
	case anthropicmessages.StreamDeltaTypeCitationsDelta:
		if kind == anthropicmessages.ContentBlockTypeText {
			field, present = "citation", delta.Citation != nil
			remaining.Citation = nil
		}
	}
	if field == "" {
		return fmt.Errorf("anthropic delta type %q is not valid on a %q block", delta.Type, kind)
	}
	if !present {
		return fmt.Errorf("anthropic %s has no %s", delta.Type, field)
	}
	if remaining != (anthropicmessages.StreamDelta{}) {
		return fmt.Errorf("anthropic %s carries fields from another delta arm", delta.Type)
	}
	return nil
}

// textDelta records one text fragment and returns its delta event.
func (s *anthropicUpstreamResponsesStreamState) textDelta(
	tracked *anthropicStreamBlock,
	text string,
) (ResponsesSSEEvent, error) {
	if err := s.budget.addSemanticBytes(int64(len(text))); err != nil {
		return nil, s.wireError(err)
	}
	tracked.text.WriteString(text)
	return ResponseTextDeltaEvent{
		Type:           "response.output_text.delta",
		SequenceNumber: s.nextSequence(),
		ItemID:         tracked.itemID,
		OutputIndex:    tracked.outputIndex,
		ContentIndex:   0,
		Delta:          text,
		Logprobs:       []ResponsesTextLogprob{},
	}, nil
}

func (s *anthropicUpstreamResponsesStreamState) blockStop(
	event anthropicmessages.StreamEvent,
) ([]ResponsesSSEEvent, error) {
	if !s.started {
		return nil, upstreamWireError(UpstreamMessages, http.StatusOK, errors.New("anthropic content_block_stop before message_start"))
	}
	if s.contentPhaseEnded {
		return nil, s.wireError(errors.New("anthropic content_block_stop after message_delta"))
	}
	if event.Index == nil {
		return nil, upstreamWireError(UpstreamMessages, http.StatusOK, errors.New("anthropic content_block_stop has no index"))
	}
	index := int64(*event.Index)
	tracked, ok := s.blocks[index]
	if !ok || tracked.closed {
		return nil, upstreamWireError(UpstreamMessages, http.StatusOK, fmt.Errorf("anthropic stop for unopened block %d", index))
	}
	tracked.closed = true
	if tracked.ignored {
		return nil, nil
	}
	switch tracked.kind {
	case anthropicmessages.ContentBlockTypeText:
		text := tracked.text.String()
		return []ResponsesSSEEvent{
			ResponseTextDoneEvent{
				Type:           "response.output_text.done",
				SequenceNumber: s.nextSequence(),
				ItemID:         tracked.itemID,
				OutputIndex:    tracked.outputIndex,
				ContentIndex:   0,
				Text:           text,
				Logprobs:       []ResponsesTextLogprob{},
			},
			ResponseContentPartDoneEvent{
				Type:           "response.content_part.done",
				SequenceNumber: s.nextSequence(),
				ItemID:         tracked.itemID,
				OutputIndex:    tracked.outputIndex,
				ContentIndex:   0,
				Part: &ResponsesStreamOutputTextPart{
					Type: "output_text", Text: text, Annotations: []ResponsesAnnotation{},
				},
			},
			ResponseOutputItemDoneEvent{
				Type:           "response.output_item.done",
				SequenceNumber: s.nextSequence(),
				OutputIndex:    tracked.outputIndex,
				Item: &ResponsesOutputMessage{
					ID: tracked.itemID, Type: "message", Role: "assistant",
					Status: ResponsesItemCompleted, Content: ResponsesOutputContentParts{},
				},
			},
		}, nil

	case anthropicmessages.ContentBlockTypeToolUse:
		arguments := tracked.args.String()
		if arguments == "" {
			// A tool call without argument deltas has an empty object as its input.
			arguments = "{}"
		}
		return []ResponsesSSEEvent{
			ResponseFunctionCallArgumentsDoneEvent{
				Type:           "response.function_call_arguments.done",
				SequenceNumber: s.nextSequence(),
				ItemID:         tracked.itemID,
				OutputIndex:    tracked.outputIndex,
				Arguments:      arguments,
			},
			ResponseOutputItemDoneEvent{
				Type:           "response.output_item.done",
				SequenceNumber: s.nextSequence(),
				OutputIndex:    tracked.outputIndex,
				Item: &ResponsesFunctionCallOutputItem{
					ID: tracked.itemID, Type: "function_call", Status: ResponsesItemCompleted,
					CallID: tracked.callID, Name: tracked.name, Arguments: arguments,
				},
			},
		}, nil
	}
	return nil, nil
}

func (s *anthropicUpstreamResponsesStreamState) messageDelta(
	event anthropicmessages.StreamEvent,
) ([]ResponsesSSEEvent, error) {
	if !s.started {
		return nil, upstreamWireError(UpstreamMessages, http.StatusOK, errors.New("anthropic message_delta before message_start"))
	}
	if !s.contentPhaseEnded {
		// The first message_delta ends the content phase whether or not it
		// carries a stop reason; the closure check runs once at that
		// transition, not per repeated delta.
		if err := s.requireClosedBlocks(event.Type); err != nil {
			return nil, err
		}
		s.contentPhaseEnded = true
	}
	if event.Delta != nil && event.Delta.StopReason != nil {
		if s.sawStopValue && s.stopReason != *event.Delta.StopReason {
			// A contradictory second stop reason is malformed wire: the
			// later value could overwrite a refusal into a success terminal.
			// A repeated identical reason is allowed (providers send a second
			// message_delta for cumulative usage), so only the conflicting
			// case fails the exchange.
			return nil, s.wireError(fmt.Errorf(
				"anthropic message_delta stop_reason %q contradicts earlier %q",
				*event.Delta.StopReason, s.stopReason,
			))
		}
		if s.sawStartStopValue && *s.startStopReason != *event.Delta.StopReason {
			// The message_start already declared this stop reason; a delta
			// claiming a different one contradicts the start metadata.
			return nil, s.wireError(fmt.Errorf(
				"anthropic message_delta stop_reason %q contradicts message_start %q",
				*event.Delta.StopReason, *s.startStopReason,
			))
		}
		s.stopReason = *event.Delta.StopReason
		s.sawStopValue = true
	}
	if event.Usage != nil {
		s.finalUsage = event.Usage
	}
	return nil, nil
}

// requireClosedBlocks checks visible and discarded blocks alike. The first
// message_delta follows every block's stop, independently of when a stop reason
// arrives. Once that phase starts, the closed block set cannot change.
func (s *anthropicUpstreamResponsesStreamState) requireClosedBlocks(event anthropicmessages.StreamEventType) error {
	// A map's iteration order is random; report the earliest unclosed block so
	// the diagnostic is stable across runs when several blocks are open.
	var first int64 = -1
	for index, block := range s.blocks {
		if !block.closed && (first < 0 || index < first) {
			first = index
		}
	}
	if first >= 0 {
		return s.wireError(fmt.Errorf("anthropic %s before content block %d stopped", event, first))
	}
	return nil
}

// messageStop ends the response. Every block must be closed and a stop reason
// must have arrived, because the terminal event reports the stop reason.
func (s *anthropicUpstreamResponsesStreamState) messageStop() ([]ResponsesSSEEvent, error) {
	if !s.started {
		return nil, upstreamWireError(UpstreamMessages, http.StatusOK, errors.New("anthropic message_stop before message_start"))
	}
	if err := s.requireClosedBlocks(anthropicmessages.StreamEventTypeMessageStop); err != nil {
		return nil, err
	}
	if !s.sawStopValue {
		return nil, upstreamWireError(UpstreamMessages, http.StatusOK, errors.New("anthropic message_stop without a stop_reason"))
	}
	if s.sawStartStopValue && *s.startStopReason != s.stopReason {
		// The start envelope declared a different stop reason; committing a
		// terminal under another reason would discard the start metadata.
		return nil, s.wireError(fmt.Errorf(
			"anthropic message_stop stop_reason %q contradicts message_start %q",
			s.stopReason, *s.startStopReason,
		))
	}

	// The stop reason decides first: a stop the terminal cannot carry refuses
	// the stream before any terminal state or note commits.
	var incompleteReason string
	switch s.stopReason {
	case anthropicmessages.StopReasonEndTurn,
		anthropicmessages.StopReasonStopSequence,
		anthropicmessages.StopReasonToolUse:
	case anthropicmessages.StopReasonMaxTokens:
		incompleteReason = "max_output_tokens"
	case anthropicmessages.StopReasonRefusal:
		incompleteReason = "content_filter"
	case anthropicmessages.StopReasonPauseTurn:
		return nil, &UnrepresentableError{
			Protocol: "messages",
			Path:     "stop_reason",
			Detail:   "pause_turn asks the client to resend the conversation and has no single-response form",
		}
	default:
		return nil, upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			fmt.Errorf("anthropic stop_reason %q is unknown", s.stopReason),
		)
	}

	s.stopped = true
	usage := s.responsesUsage()
	if usage != nil {
		// The note describes a terminal that committed, so it follows the
		// stop-reason decision.
		if err := s.report.Note(
			FeatureUsageTotalDerived,
			"usage.total_tokens",
			"the Anthropic stream reports no total; the total is the input plus the output",
		); err != nil {
			return nil, err
		}
	}
	if incompleteReason == "" {
		return []ResponsesSSEEvent{ResponseCompletedEvent{
			Type:           "response.completed",
			SequenceNumber: s.nextSequence(),
			Response:       s.envelope("completed", usage, nil),
		}}, nil
	}
	return []ResponsesSSEEvent{ResponseIncompleteEvent{
		Type:           "response.incomplete",
		SequenceNumber: s.nextSequence(),
		Response: s.envelope("incomplete", usage, &ResponsesIncompleteDetails{
			Reason: incompleteReason,
		}),
	}}, nil
}

// responsesUsage builds the terminal envelope usage. It is present only when
// both usage objects arrived, because a Responses usage object has no unknown
// count. The output count is the final cumulative value.
func (s *anthropicUpstreamResponsesStreamState) responsesUsage() *ResponsesUsage {
	if s.startUsage == nil || s.finalUsage == nil {
		return nil
	}
	cacheRead := int64(s.startUsage.CacheReadInputTokens)
	cacheWrite := int64(s.startUsage.CacheCreationInputTokens)
	input := int64(s.startUsage.InputTokens) + cacheRead + cacheWrite
	output := int64(s.finalUsage.OutputTokens)
	usage := &ResponsesUsage{
		InputTokens:        input,
		InputTokensDetails: &UsageInputTokensDetails{CachedTokens: cacheRead},
		OutputTokens:       output,
		TotalTokens:        input + output,
	}
	if s.finalUsage.OutputTokensDetails != nil {
		usage.OutputTokensDetails = &UsageOutputTokensDetails{
			ReasoningTokens: int64(s.finalUsage.OutputTokensDetails.ThinkingTokens),
		}
	}
	return usage
}

func (s *anthropicUpstreamResponsesStreamState) envelope(
	status string,
	usage *ResponsesUsage,
	incomplete *ResponsesIncompleteDetails,
) ResponseEnvelope {
	return ResponseEnvelope{
		ID:                s.responseID,
		Object:            "response",
		CreatedAt:         s.createdAt,
		Status:            status,
		Model:             s.model,
		Output:            []ResponsesOutputItem{},
		Usage:             usage,
		IncompleteDetails: incomplete,
	}
}

func (s *anthropicUpstreamResponsesStreamState) nextSequence() int64 {
	sequence := s.sequence
	s.sequence++
	return sequence
}

// messagesToGeminiConverter composes the Anthropic-to-Responses state with the
// Gemini client state in memory, like the other composed converters.
type messagesToGeminiConverter struct {
	deliveryGate

	upstream *anthropicUpstreamResponsesStreamState
	gemini   *geminiStreamClientState

	// streamError latches the converter's own failure — a frame it could not
	// decode or convert — so a caller that ignores the returned error cannot
	// still obtain a clean terminal from a later frame or EOF.
	streamError error
}

func newMessagesToGeminiConverter(
	upstream *anthropicUpstreamResponsesStreamState,
	gemini *geminiStreamClientState,
) *messagesToGeminiConverter {
	return &messagesToGeminiConverter{upstream: upstream, gemini: gemini}
}

// Convert processes one upstream Anthropic SSE frame. The first failure of
// any kind latches: every later call, and FinalizeEOF, propagates it.
func (c *messagesToGeminiConverter) Convert(frame SSEEvent) (convertedBatch, error) {
	if c.streamError != nil {
		return convertedBatch{}, c.streamError
	}
	upstreamMark, geminiMark := c.upstream.beginCall(), c.gemini.beginCall()
	batch, err := c.convertFrame(frame)
	if err == nil {
		batch, err = c.gate(batch)
	}
	if err != nil {
		c.upstream.rollbackCall(upstreamMark)
		c.gemini.rollbackCall(geminiMark)
		c.streamError = err
		return convertedBatch{}, err
	}
	return batch, nil
}

// convertFrame converts one frame, leaving failure latching to Convert.
func (c *messagesToGeminiConverter) convertFrame(frame SSEEvent) (convertedBatch, error) {
	if c.upstream.streamError != nil {
		return convertedBatch{}, c.upstream.streamError
	}
	if c.gemini.streamError != nil {
		return convertedBatch{}, c.gemini.streamError
	}
	event, err := decodeAnthropicStreamFrame(frame)
	if err != nil {
		return convertedBatch{}, err
	}
	events, err := c.upstream.Convert(event)
	if err != nil {
		return convertedBatch{}, err
	}
	batch, err := convertGeminiEvents(c.gemini, events)
	if err != nil && c.upstream.streamError != nil {
		// An upstream error event latched the upstream state and was
		// re-rendered by the Gemini state as its own failure: the upstream
		// instance is the root failure, so Convert and FinalizeEOF report the
		// same one.
		return convertedBatch{}, c.upstream.streamError
	}
	return batch, err
}

// FinalizeEOF reports a truncation when the upstream ended before message_stop.
func (c *messagesToGeminiConverter) FinalizeEOF() (convertedBatch, error) {
	if c.streamError != nil {
		return convertedBatch{}, c.streamError
	}
	upstreamMark, geminiMark := c.upstream.beginCall(), c.gemini.beginCall()
	batch, err := c.finalizeEOF()
	if err == nil {
		batch, err = c.gate(batch)
	}
	if err != nil {
		c.upstream.rollbackCall(upstreamMark)
		c.gemini.rollbackCall(geminiMark)
		c.streamError = err
		return convertedBatch{}, err
	}
	return batch, nil
}

// finalizeEOF reports a truncation unless both message_stop and the Gemini
// terminal it produces were committed, leaving failure latching to
// FinalizeEOF.
func (c *messagesToGeminiConverter) finalizeEOF() (convertedBatch, error) {
	if c.upstream.streamError != nil {
		return convertedBatch{}, c.upstream.streamError
	}
	if c.gemini.streamError != nil {
		return convertedBatch{}, c.gemini.streamError
	}
	if !c.upstream.stopped {
		return convertedBatch{}, upstreamWireError(
			UpstreamMessages, http.StatusOK, errStreamTruncated,
		)
	}
	// message_stop commits a terminal on the Gemini client; a stop without a
	// committed terminal is not a clean end of the stream.
	if !c.gemini.sawTerminal {
		return convertedBatch{}, upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			errors.New("messages stream stopped without a committed terminal"),
		)
	}
	return convertedBatch{Terminal: true}, nil
}

// ErrorEvent renders a failure as the Gemini data-only error frame.
func (c *messagesToGeminiConverter) ErrorEvent(err error) (frameEvent, bool) {
	return geminiStreamErrorFrame(err)
}

// ConversionReport returns the losses and notes of both states.
func (c *messagesToGeminiConverter) ConversionReport() *ConversionReport {
	return mergeConversionReports(&c.upstream.report, &c.gemini.report)
}

// decodeAnthropicStreamFrame decodes one upstream frame. The event envelope
// (including the message and usage objects) is the provider's
// subject-to-change surface, so it is decoded tolerantly: an unknown field
// such as usage.service_tier is skipped. Content stays strict: content blocks
// decode through their per-arm union decoder, and a content_block_delta's
// delta is decoded again strictly so an unknown field on content is still
// rejected. The SSE event name must match the JSON type when the frame names
// one.
func decodeAnthropicStreamFrame(frame SSEEvent) (anthropicmessages.StreamEvent, error) {
	var event anthropicmessages.StreamEvent
	data := trimJSONSpace(frame.Data)
	if err := wire.DecodeTolerant(data, &event); err != nil {
		return event, upstreamWireError(UpstreamMessages, http.StatusOK, err)
	}
	// Envelope extensions remain tolerant. Known payload members, however,
	// belong to particular event arms; accepting them elsewhere would silently
	// discard content before the block lifecycle can see it. Check presence
	// here as well as decoded values below so explicit null cannot mask a
	// contradictory arm.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return event, upstreamWireError(UpstreamMessages, http.StatusOK, err)
	}
	switch event.Type {
	case anthropicmessages.StreamEventTypeMessageStart:
		if event.Message != nil && event.Message.Usage != nil {
			if err := validateMessagesUsagePresence(raw["message"], "message.usage", true); err != nil {
				return event, upstreamWireError(UpstreamMessages, http.StatusOK, err)
			}
		}
	case anthropicmessages.StreamEventTypeMessageDelta:
		if event.Usage != nil {
			if err := validateMessagesUsagePresence(data, "usage", false); err != nil {
				return event, upstreamWireError(UpstreamMessages, http.StatusOK, err)
			}
		}
	}
	for field := range raw {
		if !anthropicEventFieldAllowed(event.Type, field) {
			return event, upstreamWireError(UpstreamMessages, http.StatusOK,
				fmt.Errorf("anthropic %s carries %s from another event arm", event.Type, field))
		}
	}
	if event.Type == anthropicmessages.StreamEventTypeContentBlockDelta && event.Delta != nil {
		if err := validateAnthropicDeltaFields(raw["delta"], event.Delta.Type); err != nil {
			return event, upstreamWireError(UpstreamMessages, http.StatusOK, fmt.Errorf("content_block_delta delta: %w", err))
		}
	}
	if event.Type == anthropicmessages.StreamEventTypeMessageDelta && event.Delta != nil {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw["delta"], &fields); err != nil {
			return event, upstreamWireError(UpstreamMessages, http.StatusOK, err)
		}
		for field := range fields {
			switch field {
			case "type", "text", "partial_json", "thinking", "signature", "citation":
				return event, upstreamWireError(UpstreamMessages, http.StatusOK,
					fmt.Errorf("anthropic message_delta carries content delta field %q", field))
			}
		}
	}
	if frame.Event != "" && frame.Event != string(event.Type) {
		return event, upstreamWireError(
			UpstreamMessages,
			http.StatusOK,
			fmt.Errorf("anthropic event name %q does not match its type %q", frame.Event, event.Type),
		)
	}
	return event, nil
}

// anthropicEventFieldAllowed restricts known payload fields to their event arm
// without rejecting unrelated provider extensions on the event envelope.
func anthropicEventFieldAllowed(kind anthropicmessages.StreamEventType, field string) bool {
	switch field {
	case "message":
		return kind == anthropicmessages.StreamEventTypeMessageStart
	case "index":
		return kind == anthropicmessages.StreamEventTypeContentBlockStart ||
			kind == anthropicmessages.StreamEventTypeContentBlockDelta ||
			kind == anthropicmessages.StreamEventTypeContentBlockStop
	case "content_block":
		return kind == anthropicmessages.StreamEventTypeContentBlockStart
	case "delta":
		return kind == anthropicmessages.StreamEventTypeContentBlockDelta ||
			kind == anthropicmessages.StreamEventTypeMessageDelta
	case "usage":
		return kind == anthropicmessages.StreamEventTypeMessageDelta
	case "error":
		return kind == anthropicmessages.StreamEventTypeError
	default:
		return true
	}
}

// validateAnthropicEventPayload also protects callers supplying decoded
// events directly. A message delta carries stop/usage metadata, never content
// fragments; unknown metadata is handled tolerantly by the wire decoder.
func validateAnthropicEventPayload(event anthropicmessages.StreamEvent) error {
	for _, field := range []struct {
		name    string
		present bool
	}{
		{"message", event.Message != nil},
		{"index", event.Index != nil},
		{"content_block", event.ContentBlock != nil},
		{"delta", event.Delta != nil},
		{"usage", event.Usage != nil},
		{"error", event.Error != nil},
	} {
		if field.present && !anthropicEventFieldAllowed(event.Type, field.name) {
			return fmt.Errorf("anthropic %s carries %s from another event arm", event.Type, field.name)
		}
	}
	if event.Type == anthropicmessages.StreamEventTypeMessageDelta && event.Delta != nil {
		remaining := *event.Delta
		remaining.StopReason, remaining.StopSequence = nil, nil
		if remaining != (anthropicmessages.StreamDelta{}) {
			return errors.New("anthropic message_delta carries content delta fields")
		}
	}
	return nil
}

// validateAnthropicDeltaFields decodes a content delta against its own arm.
// The broad StreamDelta struct also carries message_delta fields, so decoding
// against it alone would admit another arm's keys, including explicit nulls
// that a pointer-based semantic check cannot distinguish from absence.
func validateAnthropicDeltaFields(raw json.RawMessage, kind anthropicmessages.StreamDeltaType) error {
	switch kind {
	case anthropicmessages.StreamDeltaTypeTextDelta:
		var delta struct {
			Type anthropicmessages.StreamDeltaType `json:"type"`
			Text *string                           `json:"text"`
		}
		return wire.Decode(raw, &delta)
	case anthropicmessages.StreamDeltaTypeInputJSONDelta:
		var delta struct {
			Type        anthropicmessages.StreamDeltaType `json:"type"`
			PartialJSON *string                           `json:"partial_json"`
		}
		return wire.Decode(raw, &delta)
	case anthropicmessages.StreamDeltaTypeThinkingDelta:
		var delta struct {
			Type     anthropicmessages.StreamDeltaType `json:"type"`
			Thinking *string                           `json:"thinking"`
		}
		return wire.Decode(raw, &delta)
	case anthropicmessages.StreamDeltaTypeSignatureDelta:
		var delta struct {
			Type      anthropicmessages.StreamDeltaType `json:"type"`
			Signature *string                           `json:"signature"`
		}
		return wire.Decode(raw, &delta)
	case anthropicmessages.StreamDeltaTypeCitationsDelta:
		var delta struct {
			Type     anthropicmessages.StreamDeltaType `json:"type"`
			Citation *anthropicmessages.TextCitation   `json:"citation"`
		}
		return wire.Decode(raw, &delta)
	default:
		return fmt.Errorf("unknown anthropic content delta type %q", kind)
	}
}

// convertGeminiEvents feeds Responses events to the Gemini state and marshals
// the chunks it produces into one batch.
func convertGeminiEvents(gemini *geminiStreamClientState, events []ResponsesSSEEvent) (convertedBatch, error) {
	var chunks []geminiResponse
	for _, event := range events {
		converted, err := gemini.Convert(event)
		if err != nil {
			return convertedBatch{}, err
		}
		chunks = append(chunks, converted...)
	}
	batch, err := marshalGeminiChunks(chunks)
	if err != nil {
		return convertedBatch{}, err
	}
	batch.Terminal = gemini.sawTerminal
	return batch, nil
}
