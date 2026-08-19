package transcode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"net/http"
	"strings"
)

// Stream converter contracts:
//
// https://platform.claude.com/docs/en/build-with-claude/streaming
// https://platform.openai.com/docs/api-reference/responses-streaming
// https://github.com/openai/openai-go/blob/main/responses/response.go

// errChatStreamDone marks the [DONE] sentinel of a Chat stream.
var errChatStreamDone = errors.New("chat stream [DONE] sentinel")

// errChatDoneBeforeTerminal builds the typed upstream wire error returned
// when the [DONE] sentinel arrives before any terminal condition. The
// sentinel itself decides the result: the exchange is an upstream body
// failure and the reader must stop immediately rather than wait on an
// upstream that keeps the connection open after [DONE] (review-k finding 1).
func errChatDoneBeforeTerminal() error {
	return upstreamWireError(
		UpstreamChatCompletions,
		http.StatusOK,
		errors.New("chat stream [DONE] before a terminal condition"),
	)
}

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

// chatPartKey identifies one content part of one open message item by the
// item's output index and the part's content index.
type chatPartKey struct {
	outputIndex  int64
	contentIndex int64
}

// chatResponsesStreamState converts Chat stream chunks into typed Responses
// SSE events with the full item lifecycle. Function-call starts are buffered
// until call ID and name are known; argument fragments are emitted with delta
// (never arguments); the completion event carries the arguments ("{}" for
// empty) and no name (review-08 blocker 5). Provider reasoning deltas are
// dropped with a documented loss, or mapped to ordinary text under the
// ProviderReasoningText capability — they are never synthesized into
// reasoning items.
type chatResponsesStreamState struct {
	ctx          *ExchangeContext
	policy       LossPolicy
	capabilities ChatCapabilities
	builder      ResponsesEventBuilder

	responseID string
	model      string
	createdAt  float64

	// Envelope echo from the original request.
	echo *ResponsesRequestEcho

	started   bool
	sawFinish bool

	// chunkID and chunkModel pin the chunk identity from the first chunk;
	// later chunks with a different id or model are an upstream protocol
	// error (review-j finding 6).
	chunkID    string
	chunkModel string

	// finishReason is recorded when the finish chunk arrives; the terminal
	// envelope is built lazily at release so the optional usage tail is
	// reflected in its usage (review-j finding 6).
	finishReason string

	items        []openResponsesItem
	itemIndex    int64
	pendingCalls map[int]*pendingToolCall // keyed by chat tool fragment index

	// textBufs and refusalBufs accumulate streamed text and refusal per
	// content part in strings.Builder, avoiding quadratic re-copying of the
	// whole accumulated string on every delta (review-k finding 9). The
	// final strings are materialized into the message parts once at
	// finish(); the parts stay empty while the stream is in progress, so
	// the emitted deltas remain the only per-chunk representation.
	textBufs    map[chatPartKey]*strings.Builder
	refusalBufs map[chatPartKey]*strings.Builder

	// serviceTierLossRecorded and logprobsLossRecorded gate the per-chunk
	// envelope losses to exactly once per stream (review-k finding 9).
	serviceTierLossRecorded bool
	logprobsLossRecorded    bool

	// reasoningReportRecorded gates the provider-reasoning report entry
	// (loss or note) to exactly once per stream (review-08 blocker 7).
	reasoningReportRecorded bool

	// totalAccumulated bounds the exchange-wide sum of accumulated semantic
	// bytes (review-08 blocker 7).
	totalAccumulated int64

	// budget is the seven-dimension total exchange budget (review-z commit
	// 3): every event and every state allocation charges it BEFORE the
	// mutation.
	budget streamBudget

	usage *ResponsesUsage

	report ConversionReport

	// heldTerminal holds the item-closing events built on finish_reason; the
	// terminal envelope is appended at release by the [DONE] sentinel (the
	// only release path; review-08 blocker 2). A finish that opened no items
	// leaves this nil — the release signal is terminalReleased, never this
	// slice's nil-ness.
	heldTerminal []ResponsesSSEEvent

	// terminalReleased is set when the held terminal was released by the
	// [DONE] sentinel. It is the release signal, NOT heldTerminal's nil-ness:
	// a finish that opened no items holds a nil slice, which must remain
	// distinguishable from "no terminal condition reached" (review-08
	// blocker 2).
	terminalReleased bool

	// usageComponentsLossRecorded gates the required-usage-component loss
	// (review-k finding 6): the pinned Responses usage requires the breakdown
	// detail objects the Chat source may not provide, and the decision is
	// recorded exactly once per stream.
	usageComponentsLossRecorded bool
}

// wireError marks a conversion error as corrupt upstream Chat wire data: the
// exchange classifies as an upstream failure, never a local conversion
// failure (review-k finding 3).
func (s *chatResponsesStreamState) wireError(err error) error {
	return upstreamWireError(UpstreamChatCompletions, http.StatusOK, err)
}

// checkAccumulated rejects accumulated semantic state (text, refusal, tool
// arguments) beyond the per-item cumulative bound and the exchange total:
// individually bounded SSE frames could otherwise accumulate without limit
// and be emitted as one generated downstream frame, and a corrupt upstream
// can amplify memory without bound (review-k finding 9, review-08 blocker
// 7). The typed upstream wire error classifies the exchange as an upstream
// failure.
func (s *chatResponsesStreamState) checkAccumulated(builder *strings.Builder, added int, what string) error {
	if builder.Len() > maxStreamAccumulatedBytes {
		return s.wireError(fmt.Errorf(
			"chat stream accumulated %s exceeds %d bytes",
			what,
			maxStreamAccumulatedBytes,
		))
	}
	s.totalAccumulated += int64(added)
	if s.totalAccumulated > maxStreamTotalAccumulatedBytes {
		return s.wireError(fmt.Errorf(
			"chat stream accumulated %s exceeds the exchange total of %d bytes",
			what,
			maxStreamTotalAccumulatedBytes,
		))
	}
	return nil
}

// loseServiceTierOnce records the service-tier loss exactly once per stream:
// the tier is a chunk-envelope attribute whose presence on any chunk enters
// the decision once, not once per chunk (review-k finding 9).
func (s *chatResponsesStreamState) loseServiceTierOnce() error {
	if s.serviceTierLossRecorded {
		return nil
	}
	s.serviceTierLossRecorded = true
	return s.report.Lose(
		s.policy,
		FeatureResponseServiceTier,
		"service_tier",
		"the upstream chat service tier actually served cannot be reproduced in the client dialect",
	)
}

// loseLogprobsOnce records the log-probabilities loss exactly once per
// stream (review-k finding 9).
func (s *chatResponsesStreamState) loseLogprobsOnce() error {
	if s.logprobsLossRecorded {
		return nil
	}
	s.logprobsLossRecorded = true
	return s.report.Lose(
		s.policy,
		FeatureLogprobs,
		"choices[].logprobs",
		"chat response logprobs cannot be reproduced in the client dialect",
	)
}

// wireError marks a conversion error as corrupt upstream Responses wire
// data: the exchange classifies as an upstream failure, never a local
// conversion failure (review-k finding 3).
func (s *anthropicResponsesStreamState) wireError(err error) error {
	return upstreamWireError(UpstreamResponses, http.StatusOK, err)
}

func newChatResponsesStreamState(
	ctx *ExchangeContext,
	policy LossPolicy,
	capabilities ChatCapabilities,
	responseID string,
	model string,
	createdAt float64,
	echo *ResponsesRequestEcho,
) *chatResponsesStreamState {
	return &chatResponsesStreamState{
		ctx:          ctx,
		policy:       policy,
		capabilities: capabilities,
		responseID:   responseID,
		model:        model,
		createdAt:    createdAt,
		echo:         echo,
		budget:       newStreamBudget(),
		pendingCalls: make(map[int]*pendingToolCall),
		textBufs:     make(map[chatPartKey]*strings.Builder),
		refusalBufs:  make(map[chatPartKey]*strings.Builder),
	}
}

// loseUnknownUsageComponentsOnce records the usage-timing loss exactly once
// per stream when a wire-required Responses usage component is unknown on
// the Chat source: the pinned Responses usage requires the breakdown detail
// objects, and they are unknown when the chunk omits them. Zeros are never
// emitted silently (review-k finding 6).
func (s *chatResponsesStreamState) loseUnknownUsageComponentsOnce(usage *ChatLLMUsage) error {
	if s.usageComponentsLossRecorded || usage == nil {
		return nil
	}
	s.usageComponentsLossRecorded = true
	if usage.PromptTokensDetails == nil {
		if err := s.report.Lose(
			s.policy,
			FeatureUsageCacheReadUnknown,
			"usage",
			"the upstream response did not provide input_tokens_details.cached_tokens; the required Responses usage breakdown cannot be reproduced",
		); err != nil {
			return err
		}
	}
	if usage.CompletionTokensDetails == nil {
		if err := s.report.Lose(
			s.policy,
			FeatureUsageReasoningUnknown,
			"usage",
			"the upstream response did not provide output_tokens_details.reasoning_tokens; the required Responses usage breakdown cannot be reproduced",
		); err != nil {
			return err
		}
	}
	return nil
}

// Convert processes one Chat stream chunk into Responses events.
//
// The stream lifecycle has explicit phases (review-j finding 6):
//
//  1. normal chunks — content, tool-call fragments, and the finish chunk;
//  2. after the finish reason — the optional usage-only tail chunk
//     (choices: []/empty, usage present) that the official protocol sends
//     before [DONE] when include_usage is requested;
//  3. terminal — built at release by the [DONE] frame only (review-08
//     blocker 2), with the final usage.
func (s *chatResponsesStreamState) Convert(
	chunk ChatStreamResponse,
) ([]ResponsesSSEEvent, error) {
	if err := s.budget.addEvent(); err != nil {
		return nil, s.wireError(err)
	}
	if s.sawFinish {
		// Phase 2: accept only the usage-only tail chunk and fold its totals
		// into the terminal envelope's usage. The tail is still part of the
		// stream: chunk identity must remain stable, and a service tier on
		// the tail enters the same loss/reject decision as on any other
		// chunk.
		if chunk.Usage != nil && len(chunk.Choices) == 0 {
			// The strict chunk decode guarantees id and model are always
			// present, so a mismatch is an upstream protocol error (review-j
			// finding 6).
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
			if chunk.ServiceTier != nil {
				if err := s.loseServiceTierOnce(); err != nil {
					return nil, err
				}
			}
			if err := s.loseUnknownUsageComponentsOnce(chunk.Usage); err != nil {
				return nil, err
			}
			converted, err := chatUsageToResponsesUsage(chunk.Usage)
			if err != nil {
				return nil, s.wireError(err)
			}
			s.usage = converted
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
		// upstream protocol error (review-j finding 6).
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
		if err := s.loseUnknownUsageComponentsOnce(chunk.Usage); err != nil {
			return nil, err
		}
		converted, err := chatUsageToResponsesUsage(chunk.Usage)
		if err != nil {
			return nil, s.wireError(err)
		}
		s.usage = converted
	}

	// The chunk envelope's service tier and per-choice token log
	// probabilities cannot be reproduced in the client dialect: their
	// presence enters the explicit loss/reject decision instead of being
	// silently dropped (review-j finding 4), recorded exactly once per
	// stream (review-k finding 9).
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
		// n=1: the single choice must be index 0 (review-j finding 6).
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
			// usage. Released ONLY by the [DONE] sentinel (review-08 blocker
			// 2); EOF after finish_reason without [DONE] is a truncation.
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
		key := chatPartKey{outputIndex: item.outputIndex, contentIndex: contentIndex}
		builder := s.textBufs[key]
		if builder == nil {
			builder = &strings.Builder{}
			s.textBufs[key] = builder
		}
		builder.WriteString(*delta.Content)
		if err := s.checkAccumulated(builder, len(*delta.Content), "text"); err != nil {
			return nil, err
		}
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
		builder.WriteString(*delta.Refusal)
		if err := s.checkAccumulated(builder, len(*delta.Refusal), "refusal"); err != nil {
			return nil, err
		}
	}

	if delta.Reasoning != nil && *delta.Reasoning != "" {
		// Provider plaintext reasoning is mapped to ordinary text only when
		// the capability is enabled (a named, reported encoding); otherwise
		// it is dropped with a documented loss (review-j finding 10). It
		// must never be synthesized into reasoning items. Either report
		// entry is recorded exactly once per stream, never once per delta
		// (review-08 blocker 7).
		if !s.capabilities.ProviderReasoningText {
			if !s.reasoningReportRecorded {
				s.reasoningReportRecorded = true
				if err := s.report.Lose(
					s.policy,
					FeatureProviderReasoningText,
					"choices[].delta.reasoning",
					"provider reasoning is dropped during chat-to-responses streaming",
				); err != nil {
					return nil, err
				}
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
				*delta.Reasoning,
			))
			key := chatPartKey{outputIndex: item.outputIndex, contentIndex: contentIndex}
			builder := s.textBufs[key]
			if builder == nil {
				builder = &strings.Builder{}
				s.textBufs[key] = builder
			}
			builder.WriteString(*delta.Reasoning)
			if err := s.checkAccumulated(builder, len(*delta.Reasoning), "text"); err != nil {
				return nil, err
			}
			// The encoding note is recorded exactly once per stream, never
			// once per delta (review-08 blocker 7).
			if !s.reasoningReportRecorded {
				s.reasoningReportRecorded = true
				s.report.Note(
					FeatureProviderReasoningText,
					"choices[].delta.reasoning",
					"provider reasoning maps to ordinary text (provider_reasoning_text encoding)",
				)
			}
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

// convertToolCall buffers or emits one Chat tool-call fragment. Fragments are
// keyed by their Chat index; when the id arrives, an id alias is registered
// so later id-addressed fragments resolve to the same pending call.
func (s *chatResponsesStreamState) convertToolCall(
	call ChatToolCallDelta,
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
			return nil, s.wireError(fmt.Errorf(
				"chat tool call fragment index %d carries id %q but the pending call is %q",
				*call.Index,
				*call.ID,
				pending.callID,
			))
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
			return nil, s.wireError(errors.New(
				"chat tool call fragment without index or id is ambiguous",
			))
		}
	}
	if !ok {
		// New pending call. An index-less fragment that arrives alongside
		// existing calls gets a synthetic negative key so it cannot collide
		// with a real fragment index.
		if len(s.pendingCalls) >= maxStreamToolCalls {
			return nil, s.wireError(fmt.Errorf(
				"chat stream tool calls exceed the exchange bound of %d",
				maxStreamToolCalls,
			))
		}
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
	} else if call.Index != nil {
		// The fragment resolved by id (or the single-pending fallback) but
		// also carries an index: the index must resolve to the same pending
		// call, never silently select another (review-08 blocker 2).
		if other, exists := s.pendingCalls[*call.Index]; exists && other != pending {
			return nil, s.wireError(fmt.Errorf(
				"chat tool call fragment index %d resolves to call %q but the fragment id %q resolves to call %q",
				*call.Index,
				other.callID,
				derefStr(call.ID),
				pending.callID,
			))
		}
	}

	// Identity immutability (review-08 blocker 2): once the output_item.added
	// event was announced, a later fragment changing the call NAME is corrupt
	// upstream wire — the client already saw the announced identity and the
	// done events must never drift from it. A conflicting id cannot reach
	// this point: id resolution matches exactly, and an index resolution
	// whose stored id differs is rejected above.
	if pending.started && call.Function.Name != nil && *call.Function.Name != "" &&
		pending.name != *call.Function.Name {
		return nil, s.wireError(fmt.Errorf(
			"chat tool call name changed from %q to %q after the added event",
			pending.name,
			*call.Function.Name,
		))
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
		// The complete buffer is emitted as one generated downstream frame
		// at finish: bound its cumulative size (review-k finding 9).
		if err := s.checkAccumulated(&pending.complete, len(call.Function.Arguments), "tool arguments"); err != nil {
			return nil, err
		}
	}

	// Emit the output_item.added only once identity (call ID + name) is
	// complete, then replay buffered argument fragments. The added event
	// carries an EMPTY argument string — never an invented complete object
	// (review-j finding 6): the arguments arrive through deltas and the done
	// event.
	//
	// The emitted item is a fresh value constructed here, never the pending
	// call: later fragments mutate only the pendingToolCall (callID, name,
	// builders), so the added event is already a detached snapshot and needs
	// no copy (review-k finding 1, function-call arm).
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
				Arguments: "",
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

// openMessageItem opens a new message output item and returns the
// output_item.added event that must be emitted before any part event. The
// event carries a DETACHED snapshot of the message as it exists at creation:
// the live item accumulates content and status through later deltas, and
// sharing the pointer would leak that mutation into the already-emitted
// output_item.added frame (review-k finding 1).
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

// finish closes open items and builds the terminal event batch.
func (s *chatResponsesStreamState) finish(
	finishReason string,
) ([]ResponsesSSEEvent, error) {
	var events []ResponsesSSEEvent

	// Close every pending tool call: arguments done (arguments, "{}" for
	// empty, no name — review-08 blocker 5) then output_item.done. A
	// fragment that never received an identity (id or name) is malformed
	// upstream data: silently dropping it would hide the corruption behind a
	// successful completion.
	for _, pending := range s.pendingCalls {
		if !pending.started {
			return nil, s.wireError(errors.New(
				"chat tool call fragment ended without an id and name",
			))
		}
		arguments := pending.complete.String()
		if arguments == "" {
			arguments = "{}"
		}
		// Model-generated arguments are preserved byte-exact: the Responses
		// function_call arguments field is a string, and invalid model
		// output is never an upstream defect (review-z commit 2). Only the
		// snapshot-vs-accumulated identity check remains wire-corrupt.
		events = append(events,
			s.builder.FunctionArgumentsDone(
				pending.itemID,
				pending.outputIndex,
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
	// The accumulated text and refusal strings are materialized into the
	// parts ONCE here — the parts stayed empty while the stream was in
	// progress (review-k finding 9) — so the done events and the terminal
	// envelope carry the full content.
	for i := range s.items {
		item := &s.items[i]
		message, ok := item.item.(*ResponsesOutputMessage)
		if !ok {
			continue
		}
		for contentIndex, part := range message.Content {
			key := chatPartKey{outputIndex: item.outputIndex, contentIndex: int64(contentIndex)}
			switch value := part.(type) {
			case *ResponsesOutputText:
				if builder := s.textBufs[key]; builder != nil {
					value.Text = builder.String()
				}
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
				if builder := s.refusalBufs[key]; builder != nil {
					value.Refusal = builder.String()
				}
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

	// The terminal envelope is NOT built here: the optional usage tail may
	// still arrive and must be reflected in the terminal's usage. The
	// finish reason is recorded and the envelope is built at release.
	// Unknown finish reasons are rejected, matching the non-streaming
	// decode: they must never silently become a successful completion.
	switch finishReason {
	case "length", "max_tokens", "content_filter", "stop", "tool_calls", "function_call":
		s.finishReason = finishReason
	default:
		return nil, &UnsupportedFeatureError{
			Protocol: "chat",
			Path:     "choices[].finish_reason",
			Feature:  finishReason,
		}
	}

	return events, nil
}

// terminalEnvelope builds the terminal envelope with the final usage (which
// the optional usage tail may have updated). The finish reason was validated
// at finish(); the switch is total over the six valid reasons.
func (s *chatResponsesStreamState) terminalEnvelope() ResponsesSSEEvent {
	status := "completed"
	reason := ""
	switch s.finishReason {
	case "length", "max_tokens":
		status = "incomplete"
		reason = "max_output_tokens"
	case "content_filter":
		status = "incomplete"
		reason = "content_filter"
	}
	envelope := s.baseEnvelope(status)
	envelope.Output = s.finalOutputItems()
	envelope.Usage = s.usage
	if status == "incomplete" {
		// The incomplete reason is part of the official envelope contract.
		envelope.IncompleteDetails = &ResponsesIncompleteDetails{
			Reason: reason,
		}
		return s.builder.Incomplete(envelope)
	}
	return s.builder.Completed(envelope)
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
		envelope.ParallelToolCalls = boolPtr(s.echo.ParallelToolCalls)
		envelope.PreviousResponseID = s.echo.PreviousResponseID
		envelope.Store = s.echo.Store
		envelope.Temperature = &s.echo.Temperature
		envelope.TopP = &s.echo.TopP
		envelope.Truncation = s.echo.Truncation
		envelope.User = s.echo.User
		envelope.Metadata = s.echo.Metadata
		envelope.Tools = s.echo.Tools
		envelope.ToolChoice = &s.echo.ToolChoice
		envelope.Reasoning = s.echo.Reasoning
		envelope.Text = s.echo.Text
		envelope.ServiceTier = s.echo.ServiceTier
		envelope.TopLogprobs = s.echo.TopLogprobs
	}
	return envelope
}

// releaseTerminal returns the held terminal batch — the item-closing events
// plus the terminal envelope — and whether this call performed the release.
// The envelope is built at release time so the optional usage tail is
// reflected in its usage (review-j finding 6). A finish that opened no items
// still releases a one-event batch (just the terminal envelope): the [DONE]
// sentinel must be able to release a zero-output completion (review-08
// blocker 2).
func (s *chatResponsesStreamState) releaseTerminal() ([]ResponsesSSEEvent, bool) {
	if s.terminalReleased {
		return nil, false
	}
	s.terminalReleased = true
	held := s.heldTerminal
	s.heldTerminal = nil
	return append(held, s.terminalEnvelope()), true
}

// FinalizeEOF reports a truncation error unless the stream terminated
// correctly. The held terminal is released ONLY by the [DONE] sentinel
// (review-08 blocker 2): a stream that ends after finish_reason without
// [DONE] is a truncated stream — the usage tail never arrived — and is a
// typed upstream truncation, never a released clean terminal. A zero-output
// finish is pinned the same way: the terminal was reached but not released.
func (s *chatResponsesStreamState) FinalizeEOF() ([]ResponsesSSEEvent, error) {
	if s.sawFinish && !s.terminalReleased {
		return nil, s.wireError(errors.New(
			"chat stream ended after finish_reason without the [DONE] sentinel",
		))
	}
	if s.sawFinish {
		// The finish chunk was consumed and the [DONE] sentinel released the
		// terminal; nothing more to emit.
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

// chatUsageToResponsesUsage converts a Chat chunk usage into the Responses
// form. The pinned Responses contract requires the breakdown detail objects;
// the call sites gate the unknown components through
// loseUnknownUsageComponentsOnce before this runs, so the zeros below are
// emitted only after the explicit usage-timing loss (review-k finding 6).
// The created_cache_tokens provider extension rides the in-memory Responses
// usage carrier (json:"-"): the composed Messages←Chat stream can then know
// the cache-write component exactly like the non-streaming decode, without
// emitting wire bytes the Responses contract does not define.
func chatUsageToResponsesUsage(usage *ChatLLMUsage) (*ResponsesUsage, error) {
	if usage == nil {
		return nil, nil
	}
	prompt := int64(usage.PromptTokens)
	completion := int64(usage.CompletionTokens)
	total := int64(usage.TotalTokens)
	cached := int64(0)
	reasoning := int64(0)
	if usage.PromptTokensDetails != nil {
		cached = int64(usage.PromptTokensDetails.CachedTokens)
	}
	if usage.CompletionTokensDetails != nil {
		reasoning = int64(usage.CompletionTokensDetails.ReasoningTokens)
	}
	if prompt < 0 || completion < 0 || total < 0 || cached < 0 || reasoning < 0 {
		return nil, errors.New(
			"chat usage has negative token counts",
		)
	}
	// The chat contract defines total_tokens as prompt + completion
	// EXACTLY: a known total that is not their exact sum is corrupt
	// upstream wire (review-z commit 5).
	if prompt+completion < prompt || prompt+completion != total {
		return nil, &UsageArithmeticError{
			Detail: fmt.Sprintf(
				"chat usage total %d is not the exact sum of prompt %d + completion %d",
				total, prompt, completion,
			),
			SourceMismatch: true,
			Input:          prompt,
			Output:         completion,
			Total:          total,
		}
	}
	out := &ResponsesUsage{
		InputTokens:  prompt,
		OutputTokens: completion,
		TotalTokens:  total,
		InputTokensDetails: &UsageInputTokensDetails{
			CachedTokens: 0,
		},
		OutputTokensDetails: &UsageOutputTokensDetails{
			ReasoningTokens: 0,
		},
	}
	out.InputTokensDetails.CachedTokens = cached
	out.OutputTokensDetails.ReasoningTokens = reasoning
	if usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CreatedCacheTokens != nil {
		created := int64(*usage.PromptTokensDetails.CreatedCacheTokens)
		out.CreatedCacheTokens = &created
	}
	return out, nil
}

// chatStreamChunkShadow is the presence-aware strict decode shadow of a Chat
// streaming chunk (review-08 blocker 2): the pinned envelope fields (object,
// id, model, created) and the choice fields (index, delta) are required and
// must be explicitly present — absent fields are corrupt upstream wire,
// never zero-defaulted. The non-streaming message arm is outside the
// streaming surface, so a chunk carrying it is rejected as an unknown field.
// The delta reuses ChatStreamDelta (already pointer-based); usage reuses
// chatUsageShadow so an omitted total is distinguishable from zero (the
// pinned contract requires all three totals).
type chatStreamChunkShadow struct {
	ID                string                   `json:"id"`
	Object            *string                  `json:"object"`
	Created           *int64                   `json:"created"`
	Model             string                   `json:"model"`
	ServiceTier       *string                  `json:"service_tier,omitempty"`
	SystemFingerprint string                   `json:"system_fingerprint,omitempty"`
	Choices           []chatStreamChoiceShadow `json:"choices"`
	Usage             *chatUsageShadow         `json:"usage,omitempty"`

	// Opaque provider-extension fields present on real chat streams (e.g.
	// the yolo gateway's prompt_token_ids/prompt_text): decoded so strict
	// wire decoding never fails on a current provider; never forwarded.
	PromptTokenIDs any     `json:"prompt_token_ids,omitempty"`
	PromptText     *string `json:"prompt_text,omitempty"`
}

// chatStreamChoiceShadow mirrors the pinned streaming choice: index and
// delta are required, finish_reason is a present-or-absent nullable string
// (no terminal is enforced here), logprobs is nullable, and a message arm
// would be an unknown-field rejection. Choices absent from the chunk are not
// distinguished from an empty list: a usage-only tail chunk legitimately
// carries choices: [].
type chatStreamChoiceShadow struct {
	Index        *int64              `json:"index"`
	FinishReason *string             `json:"finish_reason"`
	LogProbs     *ChatChoiceLogprobs `json:"logprobs"`
	Delta        *ChatStreamDelta    `json:"delta"`

	TokenIDs      any     `json:"token_ids,omitempty"`
	RoutedExperts any     `json:"routed_experts,omitempty"`
	StopReason    *string `json:"stop_reason,omitempty"`
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
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			fmt.Errorf("chat stream chunk: %w", err),
		)
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
		// An in-band Chat error frame is real upstream data: the typed
		// error carries upstream provenance so the exchange classifies as
		// an upstream failure, never a local conversion failure (review-j
		// finding 11).
		return ChatStreamResponse{}, &StreamConversionError{
			Cause:      fmt.Errorf("chat stream chunk: %s", message),
			Provenance: ProvenanceUpstreamBodyError,
			Status:     http.StatusOK,
		}
	}
	// Presence-aware strict shadow decode: the pinned envelope and choice
	// fields must be explicitly present, unknown fields are rejected, and
	// the message arm is outside the streaming surface (review-08 blocker
	// 2). Every violation is corrupt upstream wire.
	var shadow chatStreamChunkShadow
	if err := wire.Decode(data, &shadow); err != nil {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			fmt.Errorf("chat stream chunk: %w", err),
		)
	}
	if shadow.Object == nil || *shadow.Object != "chat.completion.chunk" {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			fmt.Errorf(
				"chat stream chunk object = %q, want \"chat.completion.chunk\"",
				derefStr(shadow.Object),
			),
		)
	}
	if shadow.ID == "" {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			errors.New("chat stream chunk id is empty"),
		)
	}
	if shadow.Model == "" {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			errors.New("chat stream chunk model is empty"),
		)
	}
	if shadow.Created == nil {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			errors.New("chat stream chunk created is missing"),
		)
	}
	// n=1: a chunk with more than one choice is an upstream protocol error.
	if len(shadow.Choices) > 1 {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			errors.New("chat stream chunk has more than one choice; n=1 required"),
		)
	}
	for i := range shadow.Choices {
		choice := &shadow.Choices[i]
		if choice.Index == nil {
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				fmt.Errorf("chat stream chunk choice %d has no index", i),
			)
		}
		if *choice.Index != 0 {
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				fmt.Errorf(
					"chat stream chunk choice index = %d; n=1 requires index 0",
					*choice.Index,
				),
			)
		}
		if choice.Delta == nil {
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				errors.New("chat stream chunk choice has no delta"),
			)
		}
		// A present delta role must be assistant: a non-assistant role is
		// corrupt upstream wire, never relabeled as assistant output
		// (review-08 blocker 2).
		if choice.Delta.Role != nil && *choice.Delta.Role != "assistant" {
			return ChatStreamResponse{}, upstreamWireError(
				UpstreamChatCompletions,
				http.StatusOK,
				fmt.Errorf(
					"chat stream chunk delta role = %q, want assistant",
					*choice.Delta.Role,
				),
			)
		}
		// The pinned streaming tool-call fragment requires an explicit
		// index; a present type must be function (the pinned contract marks
		// type optional, so an absent type is accepted).
		for callIndex, call := range choice.Delta.ToolCalls {
			if call.Index == nil {
				return ChatStreamResponse{}, upstreamWireError(
					UpstreamChatCompletions,
					http.StatusOK,
					fmt.Errorf("chat stream chunk tool call %d has no index", callIndex),
				)
			}
			if call.Type != nil && *call.Type != "function" {
				return ChatStreamResponse{}, upstreamWireError(
					UpstreamChatCompletions,
					http.StatusOK,
					fmt.Errorf(
						"chat stream chunk tool call %d type = %q, want function",
						callIndex,
						*call.Type,
					),
				)
			}
		}
	}
	// The pinned CompletionUsage requires all three totals: an omitted total
	// must never become a factual zero (review-08 blocker 2). The breakdown
	// components remain optional and enter the loss/reject decision.
	if shadow.Usage != nil &&
		(shadow.Usage.PromptTokens == nil ||
			shadow.Usage.CompletionTokens == nil ||
			shadow.Usage.TotalTokens == nil) {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			errors.New(
				"chat stream chunk usage must carry prompt_tokens, completion_tokens, and total_tokens",
			),
		)
	}
	// The wire decode cannot fail after the shadow succeeded: the shadow
	// models the full streaming surface with the same strictness (where the
	// shadow used a pointer and the wire a value, a null is silently ignored
	// by the value field, never a decode error), and the shadow's presence
	// checks above reject every null that matters before the conversion path
	// runs.
	var chunk ChatStreamResponse
	if err := json.Unmarshal(data, &chunk); err != nil {
		return ChatStreamResponse{}, upstreamWireError(
			UpstreamChatCompletions,
			http.StatusOK,
			fmt.Errorf("chat stream chunk: %w", err),
		)
	}
	return chunk, nil
}

// anthropicResponsesStreamState converts typed Responses SSE events into
// Anthropic stream events. Failed Responses streams become Anthropic error
// events and never end_turn; error events carry the nested Anthropic error
// object; refusal becomes ordinary text content; OpenAI reasoning is never
// synthesized as thinking.
type anthropicResponsesStreamState struct {
	ctx    *ExchangeContext
	policy LossPolicy

	messageSent bool
	blockIndex  int64

	// lastSequence is the sequence number of the last processed event; the
	// wire requires strictly increasing, unique sequence numbers across the
	// stream (review-08 blocker 3). -1 accepts any nonnegative first
	// sequence.
	lastSequence int64

	// Upstream response identity, pinned from the response.created envelope
	// and verified on every later envelope-bearing event: id, model, and
	// created_at must be stable across the stream (review-08 blocker 3).
	// created_at is float64 end-to-end (review-z commit 1).
	upstreamID        string
	upstreamModel     string
	upstreamCreatedAt float64

	// addedItems records every output item observed via output_item.added:
	// its output index and type. Duplicate identities, content parts for
	// unknown items, and output-index mismatches are wire errors (review-08
	// blocker 3).
	addedItems map[string]anthropicAddedItem

	// doneItems records every output item closed by output_item.done: a
	// duplicate done for any item type is corrupt upstream wire (review-08
	// blocker 3).
	doneItems map[string]struct{}

	// partsSeen records every content part observed via content_part.added
	// (keyed by owning item and content index) with its wire part type.
	// Duplicate parts and type-mismatched content_part.done are wire errors.
	partsSeen map[responsePartKey]string

	// partCounts tracks the number of parts observed per item so the
	// per-item part bound and the done/envelope count checks are O(1)
	// instead of scanning partsSeen (review-08 blocker 7).
	partCounts map[string]int

	// textBufs and refusalBufs accumulate streamed text and refusal per
	// content part so the done events and the terminal envelope reconcile
	// against the incrementally observed content (review-08 blocker 4).
	textBufs    map[responsePartKey]*strings.Builder
	refusalBufs map[responsePartKey]*strings.Builder

	// tool blocks buffered until call identity is complete.
	pendingToolStart map[string]*pendingToolBlock // keyed by item_id

	// closedToolCalls records every function call closed by output_item.done:
	// the terminal envelope's function items must reconcile against it
	// (review-08 blocker 4).
	closedToolCalls map[string]anthropicClosedToolCall

	// partBlocks maps the Responses content part — keyed by owning item and
	// content index (review-j finding 7: content indices are scoped to an
	// item, so two message items may both have content index 0) — to the
	// Anthropic block index it opened. The composed chat->anthropic
	// direction keeps text and refusal parts open simultaneously, so deltas
	// must target their own block, never the lowest open one.
	partBlocks map[responsePartKey]int64

	responseID string
	model      string
	createdAt  float64

	// budget is the seven-dimension total exchange budget (review-z commit
	// 3): every event and every state allocation charges it BEFORE the
	// mutation.
	budget streamBudget

	// fsm is the single lifecycle validator every event passes before
	// interpretation (review-z commit 3).
	fsm *responsesStreamFSM

	sawTerminal   bool
	sawErrorEvent bool
	sawToolUse    bool

	// reasoningLossRecorded ensures the reasoning loss is recorded exactly
	// once per stream (review-j finding 7).
	reasoningLossRecorded bool

	// usageComponentsLossRecorded gates the required-usage-component loss
	// (review-k finding 6): the Messages wire requires breakdown fields the
	// Responses source never provides, and the decision is recorded exactly
	// once per stream.
	usageComponentsLossRecorded bool

	// totalAccumulated bounds the exchange-wide sum of accumulated semantic
	// bytes (text, refusal, tool arguments; review-08 blocker 7).
	totalAccumulated int64

	// phaseGated tracks the output items whose phase already entered the
	// loss decision, so a phase-bearing item seen both in output_item.added
	// and in the terminal envelope is gated exactly once (review-j finding
	// 10).
	phaseGated map[string]struct{}

	// controlsGated ensures the envelope-controls loss is recorded exactly
	// once per stream (review-j finding 13).
	controlsGated bool

	usage *AnthropicUsage

	// report accumulates approved losses of the conversion; it is surfaced
	// through the frame converter so the handler can log them.
	report ConversionReport

	// Accumulated content blocks for the final message envelope.
	message AnthropicMessageResponse
}

// responsePartKey identifies a Responses content part by its owning output
// item and content index. Content indices are scoped to an output item, so
// the item id is part of the identity (review-j finding 7).
type responsePartKey struct {
	ItemID       string
	ContentIndex int64
}

type pendingToolBlock struct {
	blockIndex  int64
	outputIndex int64
	itemID      string
	callID      string
	name        string
	arguments   strings.Builder
	started     bool
}

// anthropicAddedItem records one output item observed via output_item.added.
type anthropicAddedItem struct {
	outputIndex int64
	itemType    string
}

// anthropicClosedToolCall records one function call closed by
// output_item.done, for terminal-envelope reconciliation.
type anthropicClosedToolCall struct {
	outputIndex int64
	callID      string
	name        string
	arguments   string
}

func newAnthropicResponsesStreamState(
	ctx *ExchangeContext,
	policy LossPolicy,
	responseID string,
	model string,
	createdAt float64,
) *anthropicResponsesStreamState {
	return &anthropicResponsesStreamState{
		ctx:              ctx,
		policy:           policy,
		responseID:       responseID,
		model:            model,
		budget:           newStreamBudget(),
		fsm:              newResponsesStreamFSM(),
		createdAt:        createdAt,
		lastSequence:     -1,
		addedItems:       make(map[string]anthropicAddedItem),
		doneItems:        make(map[string]struct{}),
		partsSeen:        make(map[responsePartKey]string),
		partCounts:       make(map[string]int),
		textBufs:         make(map[responsePartKey]*strings.Builder),
		refusalBufs:      make(map[responsePartKey]*strings.Builder),
		pendingToolStart: make(map[string]*pendingToolBlock),
		closedToolCalls:  make(map[string]anthropicClosedToolCall),
		partBlocks:       make(map[responsePartKey]int64),
		phaseGated:       make(map[string]struct{}),
	}
}

// Convert processes one typed Responses event into Anthropic events.
func (s *anthropicResponsesStreamState) Convert(
	event ResponsesSSEEvent,
) ([]AnthropicStreamEvent, error) {
	if s.sawTerminal {
		return nil, errors.New("responses stream event after terminal")
	}
	// Every event passes the single lifecycle validator FIRST (review-z
	// commit 3); the render-side checks below remain as defense.
	if err := s.fsm.Validate(event); err != nil {
		return nil, err
	}
	if err := s.budget.addEvent(); err != nil {
		return nil, s.wireError(err)
	}
	// The wire requires strictly increasing, unique sequence numbers across
	// the stream (review-08 blocker 3).
	if sequence := event.Sequence(); sequence <= s.lastSequence {
		return nil, s.wireError(fmt.Errorf(
			"responses stream sequence number %d is not strictly increasing (last %d)",
			sequence,
			s.lastSequence,
		))
	} else {
		s.lastSequence = sequence
	}
	// response.created must arrive first and exactly once: any other event
	// before the created envelope is a corrupt lifecycle (review-08 blocker
	// 3). The stream error event is the one exception: it is an abort
	// terminal that may arrive at any point.
	if !s.messageSent && event.EventType() != "response.created" &&
		event.EventType() != "error" {
		return nil, s.wireError(errors.New(
			"responses stream event before response.created",
		))
	}

	switch value := event.(type) {
	case ResponseCreatedEvent:
		return s.messageStart(value.Response)

	case ResponseInProgressEvent:
		// The in_progress envelope may carry the request-echo controls even
		// when the created envelope does not; gate them the same way. The
		// response identity must stay stable across the stream.
		if err := s.checkEnvelopeIdentity(value.Response); err != nil {
			return nil, err
		}
		if err := s.loseControlsOnce(value.Response); err != nil {
			return nil, err
		}
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
		return s.textDone(value)

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
		// events are dropped. The loss is recorded exactly once per stream
		// (review-j finding 7).
		return nil, s.loseReasoningOnce()

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

// loseUnknownUsageComponentsOnce records the usage-timing loss exactly once
// per stream when a wire-required Messages usage component is unknown on the
// source: cache_read_input_tokens / thinking_tokens are unknown when their
// detail objects are absent, and cache_creation_input_tokens is unknown
// unless the composed Chat source carried the created_cache_tokens provider
// extension through the in-memory usage carrier (it is never part of the
// pinned Responses wire contract). Zeros are never emitted silently
// (review-k finding 6).
func (s *anthropicResponsesStreamState) loseUnknownUsageComponentsOnce(usage *ResponsesUsage) error {
	if s.usageComponentsLossRecorded || usage == nil {
		return nil
	}
	s.usageComponentsLossRecorded = true
	if usage.InputTokensDetails == nil {
		if err := s.report.Lose(
			s.policy,
			FeatureUsageCacheReadUnknown,
			"usage",
			"the upstream response did not provide cache_read_input_tokens; the required Messages usage breakdown cannot be reproduced",
		); err != nil {
			return err
		}
	}
	// cache_creation_input_tokens is known only through the in-memory
	// created_cache_tokens carrier set by the composed Chat source.
	if usage.CreatedCacheTokens == nil {
		if err := s.report.Lose(
			s.policy,
			FeatureUsageCacheWriteUnknown,
			"usage",
			"the upstream response cannot provide cache_creation_input_tokens; the required Messages usage breakdown cannot be reproduced",
		); err != nil {
			return err
		}
	}
	if usage.OutputTokensDetails == nil {
		if err := s.report.Lose(
			s.policy,
			FeatureUsageReasoningUnknown,
			"usage",
			"the upstream response did not provide output_tokens_details.thinking_tokens; the required Messages usage breakdown cannot be reproduced",
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *anthropicResponsesStreamState) messageStart(
	envelope ResponseEnvelope,
) ([]AnthropicStreamEvent, error) {
	if s.messageSent {
		return nil, s.wireError(errors.New("duplicate message_start"))
	}
	s.messageSent = true
	// Pin the upstream response identity from the created envelope: every
	// later envelope-bearing event must carry the same id, model, and
	// created_at (review-08 blocker 3).
	s.upstreamID = envelope.ID
	s.upstreamModel = envelope.Model
	s.upstreamCreatedAt = envelope.CreatedAt
	if err := s.loseControlsOnce(envelope); err != nil {
		return nil, err
	}
	// The stream-start message serializes stop_reason: null and
	// stop_sequence: null (review-j finding 8): generation has not finished,
	// and the stop fields are assigned only in message_delta.
	s.message = AnthropicMessageResponse{
		ID:      s.responseID,
		Type:    "message",
		Role:    "assistant",
		Model:   s.model,
		Content: []AnthropicContentBlock{},
	}
	usage, err := responsesUsageToAnthropicUsage(envelope.Usage)
	if err != nil {
		// A source-total mismatch is corrupt upstream wire; an int-width
		// overflow while rendering stays local (review-z commit 5).
		var usageErr *UsageArithmeticError
		if errors.As(err, &usageErr) {
			if usageErr.SourceMismatch {
				return nil, s.wireError(fmt.Errorf("response usage: %w", err))
			}
			return nil, usageErr
		}
		return nil, s.wireError(fmt.Errorf("response usage: %w", err))
	}
	if usage != nil {
		// The required Messages breakdown components the source did not
		// provide enter the loss decision before the zeros are emitted
		// (review-k finding 6).
		if err := s.loseUnknownUsageComponentsOnce(envelope.Usage); err != nil {
			return nil, err
		}
		s.message.Usage = usage
		s.usage = usage
	} else {
		// The created envelope cannot provide usage yet (Responses streams
		// deliver it at completion). The Messages contract requires usage on
		// message_start; emitting zeros would fabricate facts, so the early
		// usage is an explicit loss/reject decision (review-j finding 9).
		if err := s.report.Lose(
			s.policy,
			FeatureUsageUnknown,
			"message_start.usage",
			"the source cannot provide early token usage; the required message_start usage is a known loss",
		); err != nil {
			return nil, err
		}
		// The Messages wire requires output_tokens_details on the usage
		// object: the zeros are emitted only after the approved loss above
		// (review-k finding 6).
		s.message.Usage = &AnthropicUsage{
			OutputTokensDetails: &AnthropicOutputTokensDetails{},
		}
	}
	return []AnthropicStreamEvent{{
		Type:    AnthropicStreamEventTypeMessageStart,
		Message: &s.message,
	}}, nil
}

// checkEnvelopeIdentity verifies a later envelope-bearing event (in_progress,
// completed, incomplete, failed) carries the response identity pinned from
// response.created: a drifted id, model, or created_at is corrupt upstream
// wire (review-08 blocker 3).
func (s *anthropicResponsesStreamState) checkEnvelopeIdentity(
	envelope ResponseEnvelope,
) error {
	switch {
	case envelope.ID != s.upstreamID:
		return s.wireError(fmt.Errorf(
			"responses stream response id = %q, want %q (pinned at response.created)",
			envelope.ID,
			s.upstreamID,
		))
	case envelope.Model != s.upstreamModel:
		return s.wireError(fmt.Errorf(
			"responses stream response model = %q, want %q (pinned at response.created)",
			envelope.Model,
			s.upstreamModel,
		))
	case envelope.CreatedAt != s.upstreamCreatedAt:
		return s.wireError(fmt.Errorf(
			"responses stream response created_at = %v, want %v (pinned at response.created)",
			envelope.CreatedAt,
			s.upstreamCreatedAt,
		))
	default:
		return nil
	}
}

// responsesOutputItemID returns the item id of a typed output item, or ""
// for item types that never appear on the stream.
func responsesOutputItemID(item ResponsesOutputItem) string {
	switch value := item.(type) {
	case *ResponsesOutputMessage:
		return value.ID
	case *ResponsesFunctionCallOutputItem:
		return value.ID
	case *ResponsesReasoningOutputItem:
		return value.ID
	default:
		return ""
	}
}

func (s *anthropicResponsesStreamState) outputItemAdded(
	event ResponseOutputItemAddedEvent,
) ([]AnthropicStreamEvent, error) {
	// Item identities are unique across the stream: a duplicate
	// output_item.added for the same item id is corrupt upstream wire
	// (review-08 blocker 3).
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
		// enters the loss decision (review-j finding 10).
		if err := s.losePhaseOnce(item); err != nil {
			return nil, err
		}
		return nil, nil

	case *ResponsesReasoningOutputItem:
		// OpenAI reasoning is never synthesized as Anthropic thinking; the
		// reasoning item is dropped. The loss is recorded exactly once per
		// stream (review-j finding 7).
		return nil, s.loseReasoningOnce()

	case *ResponsesFunctionCallOutputItem:
		// Buffer the tool block start until call ID and name are known.
		if err := s.budget.addToolCall(); err != nil {
			return nil, s.wireError(err)
		}
		if err := s.budget.addStateEntries(1); err != nil {
			return nil, s.wireError(err)
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

	// The done item must reconcile with the observed added item: known
	// identity, matching type, and matching output index (review-08 blocker
	// 3). A duplicate done for any item type is corrupt upstream wire.
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
		// parts: same parts, same types, same accumulated text (review-08
		// blocker 4).
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
	// carried the call identity, and the done snapshot must not drift from it
	// (review-08 blocker 4).
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
	// contradiction with the done snapshot (review-08 blocker 4).
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
	// the done snapshot (review-08 blocker 4).
	arguments, suffix, err := s.reconcileToolArguments(pending, call.Arguments)
	if err != nil {
		var unrepresentable *UnrepresentableError
		if errors.As(err, &unrepresentable) {
			return nil, err
		}
		return nil, s.wireError(fmt.Errorf("tool block for item %q: %w", call.ID, err))
	}
	if suffix != "" {
		partial := suffix
		events = append(events, AnthropicStreamEvent{
			Type:  AnthropicStreamEventTypeContentBlockDelta,
			Index: intPtr(int(pending.blockIndex)),
			Delta: &AnthropicStreamDelta{
				Type:        AnthropicStreamDeltaTypeInputJSONDelta,
				PartialJSON: &partial,
			},
		})
	}
	if err := validateFinalToolInput(arguments); err != nil {
		// Anthropic tool_use.input requires an object: invalid
		// model-generated arguments are a LOCAL unrepresentable output,
		// never corrupt upstream wire (review-z commit 2).
		return nil, &UnrepresentableError{
			Protocol: "anthropic",
			Path:     "content_block.input",
			Detail:   fmt.Sprintf("tool block for item %q: %v", call.ID, err),
		}
	}

	events = append(events, AnthropicStreamEvent{
		Type:  AnthropicStreamEventTypeContentBlockStop,
		Index: intPtr(int(pending.blockIndex)),
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
// arguments can never be silently replaced (review-08 blocker 4).
func (s *anthropicResponsesStreamState) reconcileToolArguments(
	pending *pendingToolBlock,
	snapshot string,
) (string, string, error) {
	doneArgs, err := decodeJSONObject(snapshot)
	if err != nil {
		// The snapshot is the upstream's final claim about model-generated
		// arguments: a non-object is a LOCAL unrepresentable output, never
		// corrupt upstream wire (review-z commit 2). Callers pass this
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
		// other accumulation (review-08 blocker 7).
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
// (review-08 blocker 7): a corrupt upstream must not amplify memory without
// limit across many parts.
func (s *anthropicResponsesStreamState) checkAccumulated(builder *strings.Builder, added int, what string) error {
	if builder.Len() > maxStreamAccumulatedBytes {
		return s.wireError(fmt.Errorf(
			"responses stream accumulated %s exceeds %d bytes",
			what,
			maxStreamAccumulatedBytes,
		))
	}
	s.totalAccumulated += int64(added)
	if s.totalAccumulated > maxStreamTotalAccumulatedBytes {
		return s.wireError(fmt.Errorf(
			"responses stream accumulated %s exceeds the exchange total of %d bytes",
			what,
			maxStreamTotalAccumulatedBytes,
		))
	}
	return nil
}

// accumulatedText returns the accumulated text of a content part, or the
// empty string when the part never accumulated a delta. The empty string is
// the reconciliation baseline: a zero-delta part reconciles with an empty
// done snapshot and contradicts any non-empty one (review-08 blocker 4).
func (s *anthropicResponsesStreamState) accumulatedText(key responsePartKey) string {
	if builder := s.textBufs[key]; builder != nil {
		return builder.String()
	}
	return ""
}

// accumulatedRefusal returns the accumulated refusal of a content part, or
// the empty string when the part never accumulated a delta (review-08
// blocker 4).
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

func (s *anthropicResponsesStreamState) contentPartAdded(
	event ResponseContentPartAddedEvent,
) ([]AnthropicStreamEvent, error) {
	// A content part must be owned by a MESSAGE item observed via
	// output_item.added — function-call and reasoning items own no content
	// parts — and each (item, content index) identity is unique (review-08
	// blocker 3).
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
			Index: intPtr(int(s.blockIndex)),
			ContentBlock: &AnthropicContentBlock{
				Type: AnthropicContentBlockTypeText,
				Text: stringPtr(""),
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
	// would drift the emitted block from every snapshot (review-08 blocker
	// 4).
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
	builder.WriteString(event.Delta)
	if err := s.checkAccumulated(builder, len(event.Delta), "text"); err != nil {
		return nil, err
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
	// upstream's fragments and its final snapshot disagree (review-08
	// blocker 4). A part that accumulated nothing reconciles with the empty
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
	// The done part must match the opened part's type (review-08 blocker
	// 4).
	if got := partTypeName(event.Part); got != s.partsSeen[key] {
		return nil, s.wireError(fmt.Errorf(
			"content part done type = %q, want %q (observed at content_part.added)",
			got,
			s.partsSeen[key],
		))
	}
	delete(s.partBlocks, key)
	return []AnthropicStreamEvent{{
		Type:  AnthropicStreamEventTypeContentBlockStop,
		Index: intPtr(int(index)),
	}}, nil
}

// checkPartOutputIndex verifies an event targeting a content part carries
// the owning item's output index (review-08 blocker 3).
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
	pending.arguments.WriteString(event.Delta)
	// The accumulated arguments are emitted as one generated downstream
	// frame at output_item.done: bound their cumulative size (review-k
	// finding 9) and the exchange-wide total (review-08 blocker 7).
	if err := s.checkAccumulated(&pending.arguments, len(event.Delta), "tool arguments"); err != nil {
		return nil, err
	}
	// A block that never started (the added item lacked call identity, which
	// is corrupt wire rejected at output_item.done) must not receive an
	// input_json_delta without a content_block_start: the bytes are still
	// accumulated so the eventual rejection is consistent (review-08 blocker
	// 3).
	if !pending.started {
		return startEvents, nil
	}
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
	// snapshot (review-08 blocker 4). Identity comes from the item-added
	// lifecycle.
	_, suffix, err := s.reconcileToolArguments(pending, event.Arguments)
	if err != nil {
		var unrepresentable *UnrepresentableError
		if errors.As(err, &unrepresentable) {
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
	// rejects (review-08 blocker 3).
	if pending.started && suffix != "" {
		partial := suffix
		events = append(events, AnthropicStreamEvent{
			Type:  AnthropicStreamEventTypeContentBlockDelta,
			Index: intPtr(int(pending.blockIndex)),
			Delta: &AnthropicStreamDelta{
				Type:        AnthropicStreamDeltaTypeInputJSONDelta,
				PartialJSON: &partial,
			},
		})
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
	// block would drift the emitted block from every snapshot (review-08
	// blocker 4).
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
	builder.WriteString(event.Delta)
	if err := s.checkAccumulated(builder, len(event.Delta), "refusal"); err != nil {
		return nil, err
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
	// The done refusal must match the accumulated deltas (review-08 blocker
	// 4). A part that accumulated nothing reconciles with the empty string.
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
// (review-j finding 13). An envelope without controls never triggers a
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
		{"prompt_cache_key", envelope.PromptCacheKey != ""},
		{"safety_identifier", envelope.SafetyIdentifier != ""},
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
// item (review-j finding 10).
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

func (s *anthropicResponsesStreamState) completed(
	envelope ResponseEnvelope,
) ([]AnthropicStreamEvent, error) {
	if err := s.gateTerminalEnvelope(envelope); err != nil {
		return nil, err
	}
	// The stop reason mirrors the non-streaming render: a completed
	// response whose output carries function calls ends with tool_use. The
	// state's own knowledge covers upstreams whose completed envelope omits
	// the output items.
	stop := CanonicalStopEndTurn
	if outputHasFunctionCalls(envelope.Output) || s.sawToolUse {
		stop = CanonicalStopToolUse
	}
	if err := s.gateEnvelopePhases(envelope); err != nil {
		return nil, err
	}
	if err := s.loseControlsOnce(envelope); err != nil {
		return nil, err
	}
	if err := s.finalizeMessage(stop, envelope.Usage); err != nil {
		return nil, err
	}
	s.sawTerminal = true
	return s.terminalEvents(stop)
}

func (s *anthropicResponsesStreamState) incomplete(
	envelope ResponseEnvelope,
) ([]AnthropicStreamEvent, error) {
	if err := s.gateTerminalEnvelope(envelope); err != nil {
		return nil, err
	}
	// The incomplete reason drives the stop reason, matching the
	// non-streaming render: content_filter becomes a refusal stop, anything
	// else max_tokens.
	stop := CanonicalStopMaxTokens
	if envelope.IncompleteDetails != nil && envelope.IncompleteDetails.Reason == "content_filter" {
		stop = CanonicalStopRefusal
	}
	if err := s.gateEnvelopePhases(envelope); err != nil {
		return nil, err
	}
	if err := s.loseControlsOnce(envelope); err != nil {
		return nil, err
	}
	if err := s.finalizeMessage(stop, envelope.Usage); err != nil {
		return nil, err
	}
	s.sawTerminal = true
	return s.terminalEvents(stop)
}

// gateTerminalEnvelope verifies a terminal envelope (completed or
// incomplete): the response identity must match the created envelope, the
// terminal may only follow response.created, every opened item, content
// part, and tool block must have been closed by its done events — an open
// block at the terminal is corrupt upstream wire, never a synthesized
// content_block_stop (review-08 blocker 3) — and the envelope's output
// items must match the incrementally observed output (review-08 blocker 4).
func (s *anthropicResponsesStreamState) gateTerminalEnvelope(
	envelope ResponseEnvelope,
) error {
	if !s.messageSent {
		return s.wireError(errors.New(
			"responses stream terminal before response.created",
		))
	}
	if err := s.checkEnvelopeIdentity(envelope); err != nil {
		return err
	}
	if len(s.partBlocks) > 0 || len(s.pendingToolStart) > 0 {
		return s.wireError(fmt.Errorf(
			"responses stream terminal with %d open content part(s) and %d open tool block(s); missing done events",
			len(s.partBlocks),
			len(s.pendingToolStart),
		))
	}
	// Every observed output item must have been closed by its
	// output_item.done before the terminal envelope: a message or reasoning
	// item left open at the terminal is corrupt upstream wire (review-08
	// blocker 3).
	for itemID := range s.addedItems {
		if _, done := s.doneItems[itemID]; !done {
			return s.wireError(fmt.Errorf(
				"responses stream terminal with the output item %q never closed by output_item.done",
				itemID,
			))
		}
	}
	return s.reconcileTerminalOutput(envelope)
}

// reconcileTerminalOutput verifies the terminal envelope's output items are
// consistent with the incrementally observed output: every envelope item
// must have been added with a matching type, message content must match the
// observed parts and accumulated text, and function calls must match their
// closed done snapshots (review-08 blocker 4). Observed items MAY be absent
// from the envelope: the published function-calling guide's completed
// envelope carries "output":[] (and the stop-reason logic already falls back
// to the state's own knowledge for such upstreams).
func (s *anthropicResponsesStreamState) reconcileTerminalOutput(
	envelope ResponseEnvelope,
) error {
	for _, item := range envelope.Output {
		itemID := responsesOutputItemID(item)
		added, ok := s.addedItems[itemID]
		if !ok {
			return s.wireError(fmt.Errorf(
				"terminal envelope output item %q was never observed",
				itemID,
			))
		}
		if got := itemTypeName(item); got != added.itemType {
			return s.wireError(fmt.Errorf(
				"terminal envelope output item %q type = %q, want %q",
				itemID,
				got,
				added.itemType,
			))
		}
		switch value := item.(type) {
		case *ResponsesOutputMessage:
			for contentIndex, part := range value.Content {
				key := responsePartKey{ItemID: itemID, ContentIndex: int64(contentIndex)}
				seenType, partSeen := s.partsSeen[key]
				if !partSeen {
					return s.wireError(fmt.Errorf(
						"terminal envelope message %q part %d was never observed",
						itemID,
						contentIndex,
					))
				}
				if got := outputPartTypeName(part); got != seenType {
					return s.wireError(fmt.Errorf(
						"terminal envelope message %q part %d type = %q, want %q",
						itemID,
						contentIndex,
						got,
						seenType,
					))
				}
				switch partValue := part.(type) {
				case *ResponsesOutputText:
					if accumulated := s.accumulatedText(key); accumulated != partValue.Text {
						return s.wireError(fmt.Errorf(
							"terminal envelope message %q part %d text does not match the accumulated text",
							itemID,
							contentIndex,
						))
					}
				case *ResponsesOutputRefusal:
					if accumulated := s.accumulatedRefusal(key); accumulated != partValue.Refusal {
						return s.wireError(fmt.Errorf(
							"terminal envelope message %q part %d refusal does not match the accumulated refusal",
							itemID,
							contentIndex,
						))
					}
				}
			}
			if len(value.Content) != s.partCounts[itemID] {
				return s.wireError(fmt.Errorf(
					"terminal envelope message %q content count does not match the observed parts",
					itemID,
				))
			}
		case *ResponsesFunctionCallOutputItem:
			closed, ok := s.closedToolCalls[itemID]
			if !ok {
				return s.wireError(fmt.Errorf(
					"terminal envelope function call %q was never closed by output_item.done",
					itemID,
				))
			}
			if value.CallID != closed.callID || value.Name != closed.name {
				return s.wireError(fmt.Errorf(
					"terminal envelope function call %q identity does not match the observed done snapshot",
					itemID,
				))
			}
			valueArgs, err := decodeJSONObject(value.Arguments)
			if err != nil {
				// Anthropic tool_use.input requires an object: invalid
				// model-generated arguments are a LOCAL unrepresentable
				// output, never corrupt upstream wire (review-z commit 2).
				return &UnrepresentableError{
					Protocol: "anthropic",
					Path:     "content_block.input",
					Detail: fmt.Sprintf(
						"terminal envelope function call %q arguments are not a JSON object: %v",
						itemID,
						err,
					),
				}
			}
			closedArgs, err := decodeJSONObject(closed.arguments)
			if err != nil || !jsonObjectsEqual(valueArgs, closedArgs) {
				return s.wireError(fmt.Errorf(
					"terminal envelope function call %q arguments do not match the observed done snapshot",
					itemID,
				))
			}
		}
	}
	return nil
}

// terminalEvents emits the terminal message_delta + message_stop. Every
// opened block must already be closed: gateTerminalEnvelope rejected open
// blocks, so nothing is synthesized here (review-08 blocker 3).
func (s *anthropicResponsesStreamState) terminalEvents(
	stop CanonicalStopReason,
) ([]AnthropicStreamEvent, error) {
	return []AnthropicStreamEvent{
		{
			Type: AnthropicStreamEventTypeMessageDelta,
			Delta: &AnthropicStreamDelta{
				StopReason: anthropicStopReasonPtr(stopReasonToAnthropic(stop)),
			},
			Usage: s.usage,
		},
		{Type: AnthropicStreamEventTypeMessageStop},
	}, nil
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
	// end_turn. The failure terminal may only follow response.created: a
	// failed envelope before the created envelope is a corrupt lifecycle
	// (review-08 blocker 3).
	if !s.messageSent {
		return nil, s.wireError(errors.New(
			"responses stream terminal before response.created",
		))
	}
	if err := s.checkEnvelopeIdentity(envelope); err != nil {
		return nil, err
	}
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
) error {
	s.message.StopReason = anthropicStopReasonPtr(stopReasonToAnthropic(stop))
	converted, err := responsesUsageToAnthropicUsage(usage)
	if err != nil {
		// A source-total mismatch is corrupt upstream wire; an int-width
		// overflow while rendering stays local (review-z commit 5).
		var usageErr *UsageArithmeticError
		if errors.As(err, &usageErr) {
			if usageErr.SourceMismatch {
				return s.wireError(fmt.Errorf("response usage: %w", err))
			}
			return usageErr
		}
		return s.wireError(fmt.Errorf("response usage: %w", err))
	}
	if converted != nil {
		// The required Messages breakdown components the source did not
		// provide enter the loss decision before the zeros are emitted
		// (review-k finding 6); gated once per stream.
		if err := s.loseUnknownUsageComponentsOnce(usage); err != nil {
			return err
		}
		s.message.Usage = converted
		s.usage = converted
	}
	if s.usage == nil {
		// message_delta.usage is required on the wire. The terminal envelope
		// provided no usage: zeros would fabricate facts, so the omission is
		// an explicit loss/reject decision (review-j finding 9).
		if err := s.report.Lose(
			s.policy,
			FeatureUsageUnknown,
			"usage",
			"the upstream response did not provide terminal token usage; the required Messages usage cannot be reproduced",
		); err != nil {
			return err
		}
		// The Messages wire requires output_tokens_details on the usage
		// object: the zeros are emitted only after the approved loss above
		// (review-k finding 6).
		s.usage = &AnthropicUsage{
			OutputTokensDetails: &AnthropicOutputTokensDetails{},
		}
	}
	return nil
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

// responsesUsageToAnthropicUsage converts a Responses usage into the
// Anthropic form with the pinned semantics: input_tokens +
// cache_creation_input_tokens + cache_read_input_tokens = total, so the
// uncached input is the total minus the cached breakdown with checked
// nonnegative arithmetic (review-j finding 9). The in-memory
// created_cache_tokens carrier (never a wire field; set by the composed Chat
// source) supplies the cache-creation component when present, matching the
// non-streaming decode. A nil source usage returns (nil, nil): the caller
// decides the required-wire-usage loss instead of fabricating zeros.
func responsesUsageToAnthropicUsage(usage *ResponsesUsage) (*AnthropicUsage, error) {
	if usage == nil {
		return nil, nil
	}
	cached := int64(0)
	if usage.InputTokensDetails != nil {
		cached = usage.InputTokensDetails.CachedTokens
	}
	cacheWrite := int64(0)
	if usage.CreatedCacheTokens != nil {
		cacheWrite = *usage.CreatedCacheTokens
	}
	reasoning := int64(0)
	if usage.OutputTokensDetails != nil {
		reasoning = usage.OutputTokensDetails.ReasoningTokens
	}
	if usage.InputTokens < 0 || cached < 0 || cacheWrite < 0 || usage.InputTokens-cached-cacheWrite < 0 ||
		usage.OutputTokens < 0 || reasoning < 0 {
		return nil, errors.New(
			"source usage is arithmetically inconsistent: nonnegative token counts required and cached tokens must not exceed the input total",
		)
	}
	// The responses contract defines total_tokens as input + output
	// EXACTLY: a known total that is not their exact sum is corrupt
	// upstream wire (review-z commit 5).
	if sum := usage.InputTokens + usage.OutputTokens; sum < usage.InputTokens ||
		sum != usage.TotalTokens {
		return nil, &UsageArithmeticError{
			Detail: fmt.Sprintf(
				"responses usage total %d is not the exact sum of input %d + output %d",
				usage.TotalTokens, usage.InputTokens, usage.OutputTokens,
			),
			SourceMismatch: true,
			Input:          usage.InputTokens,
			Output:         usage.OutputTokens,
			Total:          usage.TotalTokens,
		}
	}
	// Checked, architecture-independent int64-to-int conversion before
	// rendering Messages usage: a count that cannot be represented on this
	// platform (32-bit builds) is a typed error, never a silent overflow
	// (review-z commit 5).
	uncached, err := checkedInt64ToInt(usage.InputTokens - cached - cacheWrite)
	if err != nil {
		return nil, &UsageArithmeticError{Detail: "input tokens: " + err.Error()}
	}
	readCached, err := checkedInt64ToInt(cached)
	if err != nil {
		return nil, &UsageArithmeticError{Detail: "cached tokens: " + err.Error()}
	}
	created, err := checkedInt64ToInt(cacheWrite)
	if err != nil {
		return nil, &UsageArithmeticError{Detail: "cache-creation tokens: " + err.Error()}
	}
	output, err := checkedInt64ToInt(usage.OutputTokens)
	if err != nil {
		return nil, &UsageArithmeticError{Detail: "output tokens: " + err.Error()}
	}
	thinking, err := checkedInt64ToInt(reasoning)
	if err != nil {
		return nil, &UsageArithmeticError{Detail: "reasoning tokens: " + err.Error()}
	}
	return &AnthropicUsage{
		InputTokens:              uncached,
		CacheCreationInputTokens: created,
		CacheReadInputTokens:     readCached,
		OutputTokens:             output,
		OutputTokensDetails: &AnthropicOutputTokensDetails{
			ThinkingTokens: thinking,
		},
	}, nil
}

// loseReasoningOnce records the reasoning loss exactly once per stream
// (review-j finding 7): OpenAI reasoning is never synthesized as Anthropic
// thinking, and dropping it silently would bypass the loss policy that the
// non-streaming path consults.
func (s *anthropicResponsesStreamState) loseReasoningOnce() error {
	if s.reasoningLossRecorded {
		return nil
	}
	s.reasoningLossRecorded = true
	return s.report.Lose(
		s.policy,
		FeatureReasoningSummary,
		"stream[].reasoning",
		"OpenAI reasoning cannot be reproduced in an Anthropic stream",
	)
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

// outputPartTypeName returns a stable type name for a completed message
// content part (the envelope and output_item.done shapes).
func outputPartTypeName(part ResponsesOutputContentPart) string {
	switch part.(type) {
	case *ResponsesOutputText:
		return "output_text"
	case *ResponsesOutputRefusal:
		return "refusal"
	default:
		return fmt.Sprintf("%T", part)
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

// checkedInt64ToInt converts an int64 token count to int with an explicit
// range check: the Messages wire types use platform int, and a silent
// wrap on 32-bit builds would emit a corrupt negative count (review-z
// commit 5).
func checkedInt64ToInt(value int64) (int, error) {
	converted := int(value)
	if int64(converted) != value {
		return 0, fmt.Errorf("token count %d does not fit the platform int width", value)
	}
	return converted, nil
}
