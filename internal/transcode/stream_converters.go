package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Stream converter contracts:
//
// https://platform.claude.com/docs/en/build-with-claude/streaming
// https://platform.openai.com/docs/api-reference/responses-streaming
// https://github.com/openai/openai-go/blob/main/responses/response.go

// errChatStreamDone marks the [DONE] sentinel of a Chat stream.
var errChatStreamDone = errors.New("chat stream [DONE] sentinel")

// pendingToolCall buffers Chat tool-call fragments until call ID and function
// name are both known, per the review: do not emit an incomplete
// function_call output_item.added and hope a later delta supplies identity.
type pendingToolCall struct {
	itemID      string
	outputIndex int64
	callID      string
	name        string

	// pending holds argument fragments not yet emitted as deltas.
	pending strings.Builder
	// complete accumulates every fragment for the final .done arguments.
	complete strings.Builder
	started  bool
}

// openResponsesItem tracks one open Responses output item during Chat→
// Responses streaming.
type openResponsesItem struct {
	outputIndex int64
	item        ResponsesOutputItem

	// openPartIndex is the content index of the currently open content part,
	// or -1 when no part is open.
	openPartIndex int64
}

// chatResponsesStreamState converts Chat stream chunks into typed Responses
// SSE events with the full item lifecycle. Function-call starts are buffered
// until call ID and name are known; argument fragments are emitted with delta
// (never arguments); the completion event always carries name and arguments
// ("{}" for empty). Provider reasoning deltas are dropped with a documented
// loss — they are never synthesized into reasoning items.
type chatResponsesStreamState struct {
	ctx     *ExchangeContext
	policy  LossPolicy
	builder ResponsesEventBuilder

	responseID string
	model      string
	createdAt  int64

	// Envelope echo from the original request.
	echo *ResponsesRequestEcho

	started   bool
	sawFinish bool

	items        []openResponsesItem
	itemIndex    int64
	pendingCalls map[int]*pendingToolCall // keyed by chat tool fragment index

	usage *ResponsesUsage

	report ConversionReport

	// heldTerminal is the terminal batch built on finish_reason and released
	// by the [DONE] frame or FinalizeEOF.
	heldTerminal []ResponsesSSEEvent
}

func newChatResponsesStreamState(
	ctx *ExchangeContext,
	policy LossPolicy,
	responseID string,
	model string,
	createdAt int64,
	echo *ResponsesRequestEcho,
) *chatResponsesStreamState {
	return &chatResponsesStreamState{
		ctx:          ctx,
		policy:       policy,
		responseID:   responseID,
		model:        model,
		createdAt:    createdAt,
		echo:         echo,
		pendingCalls: make(map[int]*pendingToolCall),
	}
}

// Convert processes one Chat stream chunk into Responses events.
func (s *chatResponsesStreamState) Convert(
	chunk ChatStreamResponse,
) ([]ResponsesSSEEvent, error) {
	if s.sawFinish {
		return nil, errors.New("chat stream chunk after finish_reason")
	}

	var events []ResponsesSSEEvent

	if !s.started {
		envelope := s.baseEnvelope("in_progress")
		events = append(events,
			s.builder.Created(envelope),
			s.builder.InProgress(envelope),
		)
		s.started = true
		if chunk.Created != 0 {
			s.createdAt = chunk.Created
		}
	}

	if chunk.Usage != nil {
		s.usage = chatUsageToResponsesUsage(chunk.Usage)
	}

	// n=1: a chunk with more than one choice is an upstream protocol error.
	if len(chunk.Choices) > 1 {
		return nil, errors.New("chat stream chunk has more than one choice; n=1 required")
	}

	for _, choice := range chunk.Choices {
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
			// Hold the terminal batch; it is released by [DONE] or EOF.
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
		textPart := message.Content[contentIndex].(*ResponsesOutputText)
		textPart.Text += *delta.Content
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
		refusalPart := message.Content[contentIndex].(*ResponsesOutputRefusal)
		refusalPart.Refusal += *delta.Refusal
	}

	if delta.Reasoning != nil && *delta.Reasoning != "" {
		// Provider plaintext reasoning has no portable representation in a
		// Responses stream and must never be synthesized into reasoning
		// items. It is dropped with a documented loss.
		if err := s.report.Lose(
			s.policy,
			FeatureProviderReasoning,
			"choices[].delta.reasoning",
			"provider reasoning is dropped during chat-to-responses streaming",
		); err != nil {
			return nil, err
		}
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
	// Open a new message item when the last item is not a message.
	if len(s.items) == 0 || !s.items[len(s.items)-1].isMessage() {
		added, err := s.openMessageItem()
		if err != nil {
			return nil, nil, err
		}
		// The output_item.added event is returned through the caller so it
		// is emitted before the content_part.added of the first part.
		return s.openMessageItemForPartWithEvents(partType, added)
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

	// Otherwise open a new content part.
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

// convertToolCall buffers or emits one Chat tool-call fragment. Fragments are
// keyed by their Chat index; when the id arrives, an id alias is registered
// so later id-addressed fragments resolve to the same pending call.
func (s *chatResponsesStreamState) convertToolCall(
	call ChatAssistantMessageToolCall,
) ([]ResponsesSSEEvent, error) {
	var events []ResponsesSSEEvent

	// Resolve the pending call: by id alias first (an id makes the fragment
	// attributable even without an index), then by fragment index.
	var pending *pendingToolCall
	ok := false
	if call.ID != nil && *call.ID != "" {
		for _, existing := range s.pendingCalls {
			if existing.callID == *call.ID {
				pending = existing
				ok = true
				break
			}
		}
	}
	if !ok && call.Index != nil {
		pending, ok = s.pendingCalls[*call.Index]
		if ok && call.ID != nil && *call.ID != "" &&
			pending.callID != "" && pending.callID != *call.ID {
			return nil, fmt.Errorf(
				"chat tool call fragment index %d carries id %q but the pending call is %q",
				*call.Index,
				*call.ID,
				pending.callID,
			)
		}
	}
	if !ok && call.Index == nil && call.ID == nil {
		// An index-less, id-less continuation fragment is attributable only
		// when exactly one pending call exists; with several, the fragment
		// is ambiguous and must not merge into an unrelated call.
		switch len(s.pendingCalls) {
		case 0:
			// First fragment: create below.
		case 1:
			for _, existing := range s.pendingCalls {
				pending = existing
			}
			ok = true
		default:
			return nil, errors.New(
				"chat tool call fragment without index or id is ambiguous",
			)
		}
	}
	if !ok {
		// New pending call. An index-less fragment that arrives alongside
		// existing calls gets a synthetic negative key so it cannot collide
		// with a real fragment index.
		index := 0
		if call.Index != nil {
			index = *call.Index
		} else {
			index = -1
			for {
				if _, taken := s.pendingCalls[index]; !taken {
					break
				}
				index--
			}
		}
		pending = &pendingToolCall{
			outputIndex: s.itemIndex,
		}
		s.itemIndex++
		s.pendingCalls[index] = pending
	}

	if call.ID != nil && *call.ID != "" {
		pending.callID = *call.ID
	}
	if call.Function.Name != nil && *call.Function.Name != "" {
		pending.name = *call.Function.Name
	}
	if call.Function.Arguments != "" {
		pending.pending.WriteString(call.Function.Arguments)
		pending.complete.WriteString(call.Function.Arguments)
	}

	// Emit the output_item.added only once identity (call ID + name) is
	// complete, then replay buffered argument fragments.
	if !pending.started && pending.callID != "" && pending.name != "" {
		pending.itemID = s.ctx.IDs.New("fc_")
		pending.started = true
		events = append(events, s.builder.OutputItemAdded(
			pending.outputIndex,
			&ResponsesFunctionCallOutputItem{
				ID:        pending.itemID,
				Type:      "function_call",
				Status:    ResponsesItemInProgress,
				CallID:    pending.callID,
				Name:      pending.name,
				Arguments: "{}",
			},
		))
	}

	if pending.started && pending.pending.Len() > 0 {
		events = append(events, s.builder.FunctionArgumentsDelta(
			pending.itemID,
			pending.outputIndex,
			pending.pending.String(),
		))
		pending.pending.Reset()
	}

	return events, nil
}

// openMessageItem opens a new message output item.
// openMessageItem opens a new message output item and returns the
// output_item.added event that must be emitted before any part event.
func (s *chatResponsesStreamState) openMessageItem() ([]ResponsesSSEEvent, error) {
	item := openResponsesItem{
		outputIndex:   s.itemIndex,
		openPartIndex: -1,
		item: &ResponsesOutputMessage{
			ID:      s.ctx.IDs.New("msg_"),
			Type:    "message",
			Role:    "assistant",
			Status:  ResponsesItemInProgress,
			Content: ResponsesOutputContentParts{},
		},
	}
	s.itemIndex++
	s.items = append(s.items, item)
	return []ResponsesSSEEvent{
		s.builder.OutputItemAdded(
			item.outputIndex,
			item.item,
		),
	}, nil
}

// finish closes open items and builds the terminal event batch.
func (s *chatResponsesStreamState) finish(
	finishReason string,
) ([]ResponsesSSEEvent, error) {
	var events []ResponsesSSEEvent

	// Close every pending tool call: arguments done (name + arguments,
	// "{}" for empty) then output_item.done. A fragment that never received
	// an identity (id or name) is malformed upstream data: silently dropping
	// it would hide the corruption behind a successful completion.
	for _, pending := range s.pendingCalls {
		if !pending.started {
			return nil, errors.New(
				"chat tool call fragment ended without an id and name",
			)
		}
		arguments := pending.complete.String()
		if arguments == "" {
			arguments = "{}"
		}
		// A done event must never carry truncated or malformed arguments:
		// emitting invalid JSON would poison the client's tool call.
		if !json.Valid([]byte(arguments)) {
			return nil, fmt.Errorf(
				"tool call %q arguments are not valid JSON: %q",
				pending.callID,
				arguments,
			)
		}
		events = append(events,
			s.builder.FunctionArgumentsDone(
				pending.itemID,
				pending.outputIndex,
				pending.name,
				arguments,
			),
			s.builder.OutputItemDone(
				pending.outputIndex,
				&ResponsesFunctionCallOutputItem{
					ID:        pending.itemID,
					Type:      "function_call",
					Status:    ResponsesItemCompleted,
					CallID:    pending.callID,
					Name:      pending.name,
					Arguments: arguments,
				},
			),
		)
	}

	// Close open message items: content parts done, then output_item.done.
	for i := range s.items {
		item := &s.items[i]
		message, ok := item.item.(*ResponsesOutputMessage)
		if !ok {
			continue
		}
		for contentIndex, part := range message.Content {
			switch value := part.(type) {
			case *ResponsesOutputText:
				events = append(events,
					s.builder.TextDone(
						message.ID,
						item.outputIndex,
						int64(contentIndex),
						value.Text,
					),
					s.builder.ContentPartDone(
						message.ID,
						item.outputIndex,
						int64(contentIndex),
						&ResponsesStreamOutputTextPart{
							Type:        "output_text",
							Text:        value.Text,
							Annotations: []ResponsesAnnotation{},
						},
					),
				)
			case *ResponsesOutputRefusal:
				events = append(events,
					s.builder.RefusalDone(
						message.ID,
						item.outputIndex,
						int64(contentIndex),
						value.Refusal,
					),
					s.builder.ContentPartDone(
						message.ID,
						item.outputIndex,
						int64(contentIndex),
						&ResponsesStreamRefusalPart{
							Type:    "refusal",
							Refusal: value.Refusal,
						},
					),
				)
			}
		}
		message.Status = ResponsesItemCompleted
		events = append(events, s.builder.OutputItemDone(
			item.outputIndex,
			message,
		))
	}

	// Final envelope: all output items (message items and function calls)
	// ordered by output index. Unknown finish reasons are rejected, matching
	// the non-streaming decode: they must never silently become a successful
	// completion.
	status := "completed"
	reason := ""
	switch finishReason {
	case "length", "max_tokens":
		status = "incomplete"
		reason = "max_output_tokens"
	case "content_filter":
		status = "incomplete"
		reason = "content_filter"
	case "stop", "tool_calls", "function_call":
	default:
		return nil, &UnsupportedFeatureError{
			Protocol: "chat",
			Path:     "choices[].finish_reason",
			Feature:  finishReason,
		}
	}
	envelope := s.baseEnvelope(status)
	envelope.Output = s.finalOutputItems()
	envelope.Usage = s.usage
	if status == "incomplete" {
		// The incomplete reason is part of the official envelope contract.
		envelope.IncompleteDetails = &ResponsesIncompleteDetails{
			Reason: reason,
		}
		events = append(events, s.builder.Incomplete(envelope))
	} else {
		events = append(events, s.builder.Completed(envelope))
	}

	return events, nil
}

// finalOutputItems returns every completed output item ordered by output
// index: message items plus completed function calls.
func (s *chatResponsesStreamState) finalOutputItems() []ResponsesOutputItem {
	type indexed struct {
		index int64
		item  ResponsesOutputItem
	}
	ordered := make([]indexed, 0, len(s.items)+len(s.pendingCalls))
	for i := range s.items {
		ordered = append(ordered, indexed{index: s.items[i].outputIndex, item: s.items[i].item})
	}
	for _, pending := range s.pendingCalls {
		if !pending.started {
			continue
		}
		arguments := pending.complete.String()
		if arguments == "" {
			arguments = "{}"
		}
		ordered = append(ordered, indexed{
			index: pending.outputIndex,
			item: &ResponsesFunctionCallOutputItem{
				ID:        pending.itemID,
				Type:      "function_call",
				Status:    ResponsesItemCompleted,
				CallID:    pending.callID,
				Name:      pending.name,
				Arguments: arguments,
			},
		})
	}
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j].index < ordered[j-1].index; j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	out := make([]ResponsesOutputItem, 0, len(ordered))
	for _, entry := range ordered {
		out = append(out, entry.item)
	}
	return out
}

// baseEnvelope builds the response envelope with the request echo.
func (s *chatResponsesStreamState) baseEnvelope(status string) ResponseEnvelope {
	envelope := ResponseEnvelope{
		ID:        s.responseID,
		Object:    "response",
		CreatedAt: s.createdAt,
		Status:    status,
		Model:     s.model,
		Output:    []ResponsesOutputItem{},
	}
	if s.echo != nil {
		envelope.Instructions = s.echo.Instructions
		if s.echo.MaxOutputTokens != nil {
			value := int64(*s.echo.MaxOutputTokens)
			envelope.MaxOutputTokens = &value
		}
		envelope.ParallelToolCalls = s.echo.ParallelToolCalls
		envelope.PreviousResponseID = s.echo.PreviousResponseID
		envelope.Store = s.echo.Store
		envelope.Temperature = s.echo.Temperature
		envelope.TopP = s.echo.TopP
		envelope.Truncation = s.echo.Truncation
		envelope.User = s.echo.User
		envelope.Metadata = s.echo.Metadata
		envelope.Tools = s.echo.Tools
		envelope.ToolChoice = s.echo.ToolChoice
		envelope.Reasoning = s.echo.Reasoning
		envelope.Text = s.echo.Text
		envelope.ServiceTier = s.echo.ServiceTier
		envelope.TopLogprobs = s.echo.TopLogprobs
	}
	return envelope
}

// releaseTerminal returns the held terminal batch and whether it exists.
func (s *chatResponsesStreamState) releaseTerminal() ([]ResponsesSSEEvent, bool) {
	if s.heldTerminal == nil {
		return nil, false
	}
	held := s.heldTerminal
	s.heldTerminal = nil
	return held, true
}

// FinalizeEOF releases the held terminal or reports a truncation error. A
// stream that ended without a terminal condition is never reported as
// success.
func (s *chatResponsesStreamState) FinalizeEOF() ([]ResponsesSSEEvent, error) {
	if held, ok := s.releaseTerminal(); ok {
		return held, nil
	}
	if s.sawFinish {
		// The finish chunk was consumed and released; nothing more to emit.
		return nil, nil
	}
	return nil, errors.New(
		"chat stream ended before a terminal condition",
	)
}

// isMessage reports whether the open item is a message item.
func (o *openResponsesItem) isMessage() bool {
	_, ok := o.item.(*ResponsesOutputMessage)
	return ok
}

func chatUsageToResponsesUsage(usage *ChatLLMUsage) *ResponsesUsage {
	if usage == nil {
		return nil
	}
	out := &ResponsesUsage{
		InputTokens:  int64(usage.PromptTokens),
		OutputTokens: int64(usage.CompletionTokens),
		TotalTokens:  int64(usage.TotalTokens),
		InputTokensDetails: &UsageInputTokensDetails{
			CachedTokens: 0,
		},
		OutputTokensDetails: &UsageOutputTokensDetails{
			ReasoningTokens: 0,
		},
	}
	if usage.PromptTokensDetails != nil {
		out.InputTokensDetails.CachedTokens = int64(usage.PromptTokensDetails.CachedTokens)
	}
	if usage.CompletionTokensDetails != nil {
		out.OutputTokensDetails.ReasoningTokens = int64(usage.CompletionTokensDetails.ReasoningTokens)
	}
	return out
}

// chatStreamChunkFromSSE parses one upstream SSE frame into a Chat stream
// chunk, reporting the [DONE] sentinel via errChatStreamDone. An in-band
// error frame is surfaced as an error: it must never be silently ignored
// while the stream continues (and potentially ends as clean success).
func chatStreamChunkFromSSE(frame SSEEvent) (ChatStreamResponse, error) {
	data := bytes.TrimSpace(frame.Data)
	if bytes.Equal(data, []byte("[DONE]")) {
		return ChatStreamResponse{}, errChatStreamDone
	}
	var probe struct {
		Error *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ChatStreamResponse{}, fmt.Errorf("chat stream chunk: %w", err)
	}
	if probe.Error != nil {
		var detail struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(*probe.Error, &detail)
		message := detail.Message
		if message == "" {
			message = "chat stream error frame"
		}
		return ChatStreamResponse{}, fmt.Errorf("chat stream chunk: %s", message)
	}
	var chunk ChatStreamResponse
	if err := json.Unmarshal(data, &chunk); err != nil {
		return ChatStreamResponse{}, fmt.Errorf("chat stream chunk: %w", err)
	}
	return chunk, nil
}

// anthropicResponsesStreamState converts typed Responses SSE events into
// Anthropic stream events. Failed Responses streams become Anthropic error
// events and never end_turn; error events carry the nested Anthropic error
// object; refusal becomes ordinary text content; OpenAI reasoning is never
// synthesized as thinking.
type anthropicResponsesStreamState struct {
	ctx *ExchangeContext

	messageSent bool
	blockIndex  int64

	// tool blocks buffered until call identity is complete.
	pendingToolStart map[string]*pendingToolBlock // keyed by item_id

	// partBlocks maps the Responses content part index to the Anthropic
	// block index it opened. The composed chat->anthropic direction keeps
	// text and refusal parts open simultaneously, so deltas must target
	// their own block, never the lowest open one.
	partBlocks map[int64]int64

	responseID string
	model      string
	createdAt  int64

	sawTerminal   bool
	sawErrorEvent bool
	sawToolUse    bool

	usage *AnthropicUsage

	// Accumulated content blocks for the final message envelope.
	message AnthropicMessageResponse
}

type pendingToolBlock struct {
	blockIndex int64
	itemID     string
	callID     string
	name       string
	arguments  strings.Builder
	started    bool
}

func newAnthropicResponsesStreamState(
	ctx *ExchangeContext,
	responseID string,
	model string,
	createdAt int64,
) *anthropicResponsesStreamState {
	return &anthropicResponsesStreamState{
		ctx:              ctx,
		responseID:       responseID,
		model:            model,
		createdAt:        createdAt,
		pendingToolStart: make(map[string]*pendingToolBlock),
		partBlocks:       make(map[int64]int64),
	}
}

// Convert processes one typed Responses event into Anthropic events.
func (s *anthropicResponsesStreamState) Convert(
	event ResponsesSSEEvent,
) ([]AnthropicStreamEvent, error) {
	if s.sawTerminal {
		return nil, errors.New("responses stream event after terminal")
	}

	switch value := event.(type) {
	case ResponseCreatedEvent:
		return s.messageStart(value.Response)

	case ResponseInProgressEvent:
		return nil, nil

	case ResponseOutputItemAddedEvent:
		return s.outputItemAdded(value)

	case ResponseOutputItemDoneEvent:
		return s.outputItemDone(value)

	case ResponseContentPartAddedEvent:
		return s.contentPartAdded(value)

	case ResponseTextDeltaEvent:
		return s.textDelta(value)

	case ResponseTextDoneEvent:
		return nil, nil

	case ResponseContentPartDoneEvent:
		return s.contentPartDone(value)

	case ResponseFunctionCallArgumentsDeltaEvent:
		return s.functionArgumentsDelta(value)

	case ResponseFunctionCallArgumentsDoneEvent:
		return s.functionArgumentsDone(value)

	case ResponseRefusalDeltaEvent:
		return s.refusalDelta(value)

	case ResponseRefusalDoneEvent:
		return s.refusalDone(value)

	case ResponseReasoningSummaryPartAddedEvent,
		ResponseReasoningSummaryTextDeltaEvent,
		ResponseReasoningSummaryTextDoneEvent,
		ResponseReasoningSummaryPartDoneEvent:
		// OpenAI reasoning is never synthesized as Anthropic thinking; these
		// events are dropped.
		return nil, nil

	case ResponseCompletedEvent:
		return s.completed(value.Response)

	case ResponseIncompleteEvent:
		return s.incomplete(value.Response)

	case ResponseFailedEvent:
		return s.failed(value.Response)

	case ResponseErrorEvent:
		return s.errorEvent(value)

	default:
		return nil, &UnsupportedFeatureError{
			Protocol: "responses",
			Path:     "stream[].type",
			Feature:  event.EventType(),
		}
	}
}

func (s *anthropicResponsesStreamState) messageStart(
	envelope ResponseEnvelope,
) ([]AnthropicStreamEvent, error) {
	if s.messageSent {
		return nil, errors.New("duplicate message_start")
	}
	s.messageSent = true
	s.message = AnthropicMessageResponse{
		ID:         s.responseID,
		Type:       "message",
		Role:       "assistant",
		Model:      s.model,
		Content:    []AnthropicContentBlock{},
		StopReason: AnthropicStopReasonEndTurn,
		Usage:      &AnthropicUsage{},
	}
	if envelope.Usage != nil {
		s.message.Usage = responsesUsageToAnthropicUsage(envelope.Usage)
		s.usage = s.message.Usage
	}
	return []AnthropicStreamEvent{{
		Type:    AnthropicStreamEventTypeMessageStart,
		Message: &s.message,
	}}, nil
}

func (s *anthropicResponsesStreamState) outputItemAdded(
	event ResponseOutputItemAddedEvent,
) ([]AnthropicStreamEvent, error) {
	switch item := event.Item.(type) {
	case *ResponsesOutputMessage:
		// Message items open their content blocks via content_part.added.
		return nil, nil

	case *ResponsesReasoningOutputItem:
		// OpenAI reasoning is never synthesized as Anthropic thinking; the
		// reasoning item is dropped.
		return nil, nil

	case *ResponsesFunctionCallOutputItem:
		// Buffer the tool block start until call ID and name are known.
		pending := &pendingToolBlock{
			blockIndex: s.blockIndex,
			itemID:     item.ID,
			callID:     item.CallID,
			name:       item.Name,
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

// maybeStartToolBlock emits the tool_use content_block_start once the call
// identity is complete, replaying nothing (argument fragments are emitted as
// input_json_delta by later events).
func (s *anthropicResponsesStreamState) maybeStartToolBlock(
	pending *pendingToolBlock,
) ([]AnthropicStreamEvent, error) {
	if pending.started || pending.callID == "" || pending.name == "" {
		return nil, nil
	}
	pending.started = true
	s.sawToolUse = true
	callID := pending.callID
	name := pending.name
	return []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockStart,
		Index: intPtr(int(pending.blockIndex)),
		ContentBlock: &AnthropicContentBlock{
			Type:  AnthropicContentBlockTypeToolUse,
			ID:    &callID,
			Name:  &name,
			Input: json.RawMessage("{}"),
		},
	}}, nil
}

func (s *anthropicResponsesStreamState) outputItemDone(
	event ResponseOutputItemDoneEvent,
) ([]AnthropicStreamEvent, error) {
	var events []AnthropicStreamEvent

	switch event.Item.(type) {
	case *ResponsesOutputMessage:
		// Message items are closed by content_part.done.
		return nil, nil
	case *ResponsesReasoningOutputItem:
		// Reasoning items were dropped on add.
		return nil, nil
	}

	call, ok := event.Item.(*ResponsesFunctionCallOutputItem)
	if !ok {
		return nil, nil
	}

	pending, ok := s.pendingToolStart[call.ID]
	if !ok {
		return nil, fmt.Errorf("tool block for item %q was never started", call.ID)
	}
	startEvents, err := s.maybeStartToolBlock(pending)
	if err != nil {
		return nil, err
	}
	events = append(events, startEvents...)

	// Final arguments must parse as exactly one JSON object.
	arguments := pending.arguments.String()
	if arguments == "" {
		arguments = "{}"
	}
	if err := validateFinalToolInput(arguments); err != nil {
		return nil, fmt.Errorf("tool block for item %q: %w", call.ID, err)
	}

	events = append(events, AnthropicStreamEvent{
		Type:  AnthropicStreamEventTypeContentBlockStop,
		Index: intPtr(int(pending.blockIndex)),
	})
	// The block is now closed; the terminal must not stop it again.
	delete(s.pendingToolStart, call.ID)

	// The message envelope's content stays empty: content blocks arrive via
	// content_block_start events (the official contract); message_start is
	// already marshaled with an empty content array.
	return events, nil
}

func (s *anthropicResponsesStreamState) contentPartAdded(
	event ResponseContentPartAddedEvent,
) ([]AnthropicStreamEvent, error) {
	var events []AnthropicStreamEvent
	switch event.Part.(type) {
	case *ResponsesStreamOutputTextPart, *ResponsesStreamRefusalPart:
		// Refusal becomes ordinary text content.
		events = append(events, AnthropicStreamEvent{
			Type:  AnthropicStreamEventTypeContentBlockStart,
			Index: intPtr(int(s.blockIndex)),
			ContentBlock: &AnthropicContentBlock{
				Type: AnthropicContentBlockTypeText,
				Text: stringPtr(""),
			},
		})
		s.partBlocks[event.ContentIndex] = s.blockIndex
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
	index, ok := s.partBlocks[event.ContentIndex]
	if !ok {
		return nil, errors.New("text delta with no open content block")
	}
	text := event.Delta
	return []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockDelta,
		Index: intPtr(int(index)),
		Delta: &AnthropicStreamDelta{
			Type: AnthropicStreamDeltaTypeTextDelta,
			Text: &text,
		},
	}}, nil
}

func (s *anthropicResponsesStreamState) contentPartDone(
	event ResponseContentPartDoneEvent,
) ([]AnthropicStreamEvent, error) {
	index, ok := s.partBlocks[event.ContentIndex]
	if !ok {
		return nil, errors.New("content part done with no open block")
	}
	delete(s.partBlocks, event.ContentIndex)
	return []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockStop,
		Index: intPtr(int(index)),
	}}, nil
}

func (s *anthropicResponsesStreamState) functionArgumentsDelta(
	event ResponseFunctionCallArgumentsDeltaEvent,
) ([]AnthropicStreamEvent, error) {
	pending, ok := s.pendingToolStart[event.ItemID]
	if !ok {
		return nil, fmt.Errorf("arguments delta for unknown item %q", event.ItemID)
	}
	// Ensure the block started before emitting input_json_delta.
	startEvents, err := s.maybeStartToolBlock(pending)
	if err != nil {
		return nil, err
	}
	pending.arguments.WriteString(event.Delta)
	partial := event.Delta
	out := []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockDelta,
		Index: intPtr(int(pending.blockIndex)),
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
		return nil, fmt.Errorf("arguments done for unknown item %q", event.ItemID)
	}
	// Record the final name (superset over the official event) and arguments.
	if pending.name == "" && event.Name != "" {
		pending.name = event.Name
	}
	if event.Arguments != "" {
		pending.arguments.Reset()
		pending.arguments.WriteString(event.Arguments)
	}
	return s.maybeStartToolBlock(pending)
}

func (s *anthropicResponsesStreamState) refusalDelta(
	event ResponseRefusalDeltaEvent,
) ([]AnthropicStreamEvent, error) {
	index, ok := s.partBlocks[event.ContentIndex]
	if !ok {
		return nil, errors.New("refusal delta with no open content block")
	}
	text := event.Delta
	return []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockDelta,
		Index: intPtr(int(index)),
		Delta: &AnthropicStreamDelta{
			Type: AnthropicStreamDeltaTypeTextDelta,
			Text: &text,
		},
	}}, nil
}

func (s *anthropicResponsesStreamState) refusalDone(
	event ResponseRefusalDoneEvent,
) ([]AnthropicStreamEvent, error) {
	// The refusal text block is closed by the subsequent content_part.done.
	return nil, nil
}

func (s *anthropicResponsesStreamState) completed(
	envelope ResponseEnvelope,
) ([]AnthropicStreamEvent, error) {
	// The stop reason mirrors the non-streaming render: a completed
	// response whose output carries function calls ends with tool_use. The
	// state's own knowledge covers upstreams whose completed envelope omits
	// the output items.
	stop := CanonicalStopEndTurn
	if outputHasFunctionCalls(envelope.Output) || s.sawToolUse {
		stop = CanonicalStopToolUse
	}
	s.finalizeMessage(stop, envelope.Usage)
	s.sawTerminal = true
	return s.terminalEvents(stop)
}

func (s *anthropicResponsesStreamState) incomplete(
	envelope ResponseEnvelope,
) ([]AnthropicStreamEvent, error) {
	// The incomplete reason drives the stop reason, matching the
	// non-streaming render: content_filter becomes a refusal stop, anything
	// else max_tokens.
	stop := CanonicalStopMaxTokens
	if envelope.IncompleteDetails != nil && envelope.IncompleteDetails.Reason == "content_filter" {
		stop = CanonicalStopRefusal
	}
	s.finalizeMessage(stop, envelope.Usage)
	s.sawTerminal = true
	return s.terminalEvents(stop)
}

// terminalEvents emits content_block_stop for any tool block left open by a
// non-conformant upstream (output_item.done omitted), then the terminal
// message_delta + message_stop. The stream trace invariant requires every
// opened block to stop before message_delta.
func (s *anthropicResponsesStreamState) terminalEvents(
	stop CanonicalStopReason,
) ([]AnthropicStreamEvent, error) {
	var events []AnthropicStreamEvent
	// Close any tool block left open by a non-conformant upstream
	// (output_item.done omitted).
	for _, pending := range s.pendingToolStart {
		if !pending.started {
			continue
		}
		events = append(events, AnthropicStreamEvent{
			Type:  AnthropicStreamEventTypeContentBlockStop,
			Index: intPtr(int(pending.blockIndex)),
		})
		delete(s.pendingToolStart, pending.itemID)
	}
	// Close any text/refusal block left open by a non-conformant upstream
	// (content_part.done omitted).
	for contentIndex, blockIndex := range s.partBlocks {
		events = append(events, AnthropicStreamEvent{
			Type:  AnthropicStreamEventTypeContentBlockStop,
			Index: intPtr(int(blockIndex)),
		})
		delete(s.partBlocks, contentIndex)
	}
	events = append(events,
		AnthropicStreamEvent{
			Type: AnthropicStreamEventTypeMessageDelta,
			Delta: &AnthropicStreamDelta{
				StopReason: anthropicStopReasonPtr(stopReasonToAnthropic(stop)),
			},
			Usage: s.usage,
		},
		AnthropicStreamEvent{Type: AnthropicStreamEventTypeMessageStop},
	)
	return events, nil
}

// outputHasFunctionCalls reports whether the response output contains
// function call items (the driver of the tool_use stop reason).
func outputHasFunctionCalls(output []ResponsesOutputItem) bool {
	for _, item := range output {
		if _, ok := item.(*ResponsesFunctionCallOutputItem); ok {
			return true
		}
	}
	return false
}

func (s *anthropicResponsesStreamState) failed(
	envelope ResponseEnvelope,
) ([]AnthropicStreamEvent, error) {
	// A failed Responses stream must become an Anthropic error event, never
	// end_turn.
	s.sawTerminal = true
	s.sawErrorEvent = true
	message := "upstream response failed"
	code := ""
	if envelope.Error != nil {
		if envelope.Error.Message != "" {
			message = envelope.Error.Message
		}
		code = envelope.Error.Code
	}
	return s.buildErrorEvent(code, message), nil
}

func (s *anthropicResponsesStreamState) errorEvent(
	event ResponseErrorEvent,
) ([]AnthropicStreamEvent, error) {
	s.sawTerminal = true
	s.sawErrorEvent = true
	return s.buildErrorEvent(event.Code, event.Message), nil
}

// buildErrorEvent constructs the Anthropic error event with the nested error
// object required by the official stream contract.
func (s *anthropicResponsesStreamState) buildErrorEvent(
	errorType string,
	message string,
) []AnthropicStreamEvent {
	errorType = anthropicErrorTypeFromCode(errorType)
	return []AnthropicStreamEvent{{
		Type: AnthropicStreamEventTypeError,
		Error: &AnthropicStreamError{
			Type:    errorType,
			Message: message,
		},
	}}
}

// anthropicErrorTypeFromCode maps a Responses error code to an Anthropic
// error type.
func anthropicErrorTypeFromCode(code string) string {
	switch code {
	case "rate_limit_exceeded":
		return "rate_limit_error"
	case "overloaded":
		return "overloaded_error"
	case "authentication_error":
		return "authentication_error"
	case "permission_error":
		return "permission_error"
	case "request_too_large":
		return "request_too_large"
	case "invalid_request_error":
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

func (s *anthropicResponsesStreamState) finalizeMessage(
	stop CanonicalStopReason,
	usage *ResponsesUsage,
) {
	s.message.StopReason = stopReasonToAnthropic(stop)
	if usage != nil {
		s.message.Usage = responsesUsageToAnthropicUsage(usage)
		s.usage = s.message.Usage
	}
	if s.usage == nil {
		// message_delta.usage is required on the wire; emit a zero usage when
		// the upstream did not provide one.
		s.usage = &AnthropicUsage{}
	}
}

// FinalizeEOF reports a truncation error when the stream ended without a
// terminal condition. A stream that ended without a terminal condition is
// never reported as success.
func (s *anthropicResponsesStreamState) FinalizeEOF() ([]AnthropicStreamEvent, error) {
	if s.sawTerminal {
		// The terminal was emitted at response.completed (or as an error
		// event); nothing more to emit.
		return nil, nil
	}
	return nil, errors.New(
		"responses stream ended before a terminal condition",
	)
}

func responsesUsageToAnthropicUsage(usage *ResponsesUsage) *AnthropicUsage {
	if usage == nil {
		return &AnthropicUsage{}
	}
	cached := int64(0)
	if usage.InputTokensDetails != nil {
		cached = usage.InputTokensDetails.CachedTokens
	}
	reasoning := int64(0)
	if usage.OutputTokensDetails != nil {
		reasoning = usage.OutputTokensDetails.ReasoningTokens
	}
	return &AnthropicUsage{
		InputTokens:              int(usage.InputTokens),
		CacheCreationInputTokens: 0,
		CacheReadInputTokens:     int(cached),
		OutputTokens:             int(usage.OutputTokens),
		OutputTokensDetails: &AnthropicOutputTokensDetails{
			ThinkingTokens: int(reasoning),
		},
	}
}

func stopReasonToAnthropic(stop CanonicalStopReason) AnthropicStopReason {
	switch stop {
	case CanonicalStopMaxTokens:
		return AnthropicStopReasonMaxTokens
	case CanonicalStopStopSequence:
		return AnthropicStopReasonStopSequence
	case CanonicalStopToolUse:
		return AnthropicStopReasonToolUse
	case CanonicalStopRefusal:
		return AnthropicStopReasonRefusal
	default:
		return AnthropicStopReasonEndTurn
	}
}

// validateFinalToolInput enforces that the completed tool input parses as
// exactly one JSON object.
func validateFinalToolInput(arguments string) error {
	if _, err := decodeJSONObject(arguments); err != nil {
		return fmt.Errorf("final tool input is not a JSON object: %w", err)
	}
	return nil
}

// itemTypeName returns a stable type name for an output item.
func itemTypeName(item ResponsesOutputItem) string {
	switch item.(type) {
	case *ResponsesOutputMessage:
		return "message"
	case *ResponsesFunctionCallOutputItem:
		return "function_call"
	case *ResponsesReasoningOutputItem:
		return "reasoning"
	default:
		return fmt.Sprintf("%T", item)
	}
}

// partTypeName returns a stable type name for a stream content part.
func partTypeName(part ResponsesStreamContentPart) string {
	switch part.(type) {
	case *ResponsesStreamOutputTextPart:
		return "output_text"
	case *ResponsesStreamRefusalPart:
		return "refusal"
	default:
		return fmt.Sprintf("%T", part)
	}
}

// anthropicStopReasonPtr returns a pointer to the stop reason.
func anthropicStopReasonPtr(reason AnthropicStopReason) *AnthropicStopReason {
	return &reason
}
