package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/geminigeneratecontent"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// continuityTurnSource is implemented by a stream converter whose underlying
// state can rebuild the canonical conversation a completed stream continues,
// for the opt-in continuity store. streamResponse records continuity for any
// converter implementing it when the client protocol is Responses, instead of
// type-asserting one concrete converter.
type continuityTurnSource interface {
	continuityTurns(requestTurns []CanonicalTurn, depth int) (string, []CanonicalTurn, int, int, bool)
}

// geminiResponsesStreamState converts Gemini upstream stream chunks (data-only
// SSE frames carrying GenerateContentResponse JSON) into typed Responses SSE
// events. Text parts are deltas; a functionCall part arrives complete and is
// emitted as one output_item.added + arguments delta + arguments done +
// output_item.done sequence, storing its thoughtSignature at that moment. A
// chunk carrying finishReason is terminal: the terminal events are held until
// EOF (Gemini has no [DONE] sentinel) so a trailing usage-only chunk is
// reflected in the terminal envelope's usage.
type geminiResponsesStreamState struct {
	ctx          *ExchangeContext
	policy       LossPolicy
	capabilities ChatCapabilities
	builder      ResponsesEventBuilder

	responseID string
	model      string
	createdAt  float64

	// echo is the original Responses request, when the client is Responses.
	echo *ResponsesRequestEcho

	budget streamBudget
	report ConversionReport

	started bool
	sawTerm bool

	// items tracks the open/closed output items in output order.
	items []openResponsesItem
	// textBufs accumulates the streamed text per content part.
	textBufs map[geminiPartKey]*strings.Builder

	// finishReason is recorded when the finish chunk arrives; the terminal
	// envelope is built lazily at release so a trailing usage chunk is
	// reflected.
	finishReason string
	// refused is set when the stream ended in a provider refusal (a blocked
	// prompt or a refusal finishReason); the terminal is then rendered as
	// incomplete with reason content_filter, the Responses contract's
	// representation of a filtered response.
	refused       bool
	heldTerminal  []ResponsesSSEEvent
	terminalDone  bool
	usage         *ResponsesUsage
	usagePresence usagePresence

	// thoughtBufs accumulates streamed thought text per reasoning item.
	thoughtBufs map[geminiPartKey]*strings.Builder

	// totalAccumulated bounds the exchange-wide sum of accumulated semantic
	// bytes, the same per-state bound the chat state applies.
	totalAccumulated int64

	// Per-stream once-gates for the metadata losses.
	safetyLossRecorded         bool
	groundingLossRecorded      bool
	logprobsLossRecorded       bool
	tokenCountLossRecorded     bool
	usageBreakdownLossRecorded bool
	serviceTierLossRecorded    bool
	modelStatusLossRecorded    bool
	thoughtTextNoted           bool
	thoughtLossNoted           bool
}

type geminiPartKey struct {
	outputIndex  int64
	contentIndex int64
}

// isGeminiReasoningItem reports whether the item is a reasoning item.
func isGeminiReasoningItem(item *openResponsesItem) bool {
	_, ok := item.item.(*ResponsesReasoningOutputItem)
	return ok
}

func newGeminiResponsesStreamState(
	ctx *ExchangeContext,
	policy LossPolicy,
	capabilities ChatCapabilities,
	responseID string,
	model string,
	createdAt float64,
	echo *ResponsesRequestEcho,
) *geminiResponsesStreamState {
	return &geminiResponsesStreamState{
		ctx:          ctx,
		policy:       policy,
		capabilities: capabilities,
		responseID:   responseID,
		model:        model,
		createdAt:    createdAt,
		echo:         echo,
		budget:       newStreamBudget(),
		textBufs:     make(map[geminiPartKey]*strings.Builder),
		thoughtBufs:  make(map[geminiPartKey]*strings.Builder),
	}
}

func (s *geminiResponsesStreamState) wireError(err error) error {
	return upstreamWireError(UpstreamGemini, http.StatusOK, err)
}

// checkAccumulated bounds the per-buffer and exchange-wide accumulated
// semantic bytes, mirroring the chat state's bound.
func (s *geminiResponsesStreamState) checkAccumulated(builder *strings.Builder, added int, what string) error {
	if builder.Len() > maxStreamAccumulatedBytes {
		return s.wireError(fmt.Errorf(
			"gemini stream accumulated %s exceeds %d bytes",
			what,
			maxStreamAccumulatedBytes,
		))
	}
	return s.chargeTotal(added, what)
}

// chargeTotal adds n retained semantic bytes (text, refusal, tool arguments,
// tool-call identity) to the exchange-wide accumulated total. Everything
// charged here is repeated in done events and the terminal envelope, so the
// release bounds derive from this total.
func (s *geminiResponsesStreamState) chargeTotal(n int, what string) error {
	s.totalAccumulated += int64(n)
	if s.totalAccumulated > maxStreamTotalAccumulatedBytes {
		return s.wireError(fmt.Errorf(
			"gemini stream accumulated %s exceeds the exchange total of %d bytes",
			what,
			maxStreamTotalAccumulatedBytes,
		))
	}
	return nil
}

// Convert processes one Gemini stream chunk into typed Responses events.
func (s *geminiResponsesStreamState) Convert(chunk geminigeneratecontent.GenerateContentResponse) ([]ResponsesSSEEvent, error) {
	if err := s.budget.addEvent(); err != nil {
		return nil, s.wireError(err)
	}

	// An in-stream error frame is an upstream failure terminal: the HTTP
	// status was 200, so the body carries the only truthful signal.
	if chunk.Error != nil {
		s.sawTerm = true
		message := chunk.Error.Message
		if message == "" {
			message = fmt.Sprintf("the upstream sent an error frame (code %d, status %s)", chunk.Error.Code, chunk.Error.Status)
		}
		return nil, upstreamStreamFailure(message)
	}

	// Candidate shape: the gateway never requests more than one candidate, so
	// a second one (or a non-zero index) is corrupt upstream wire.
	if len(chunk.Candidates) > 1 {
		return nil, s.wireError(errors.New(
			"gemini stream chunk carries more than one candidate",
		))
	}
	var candidate *geminigeneratecontent.Candidate
	if len(chunk.Candidates) == 1 {
		c := chunk.Candidates[0]
		if c.Index != nil && *c.Index != 0 {
			return nil, s.wireError(fmt.Errorf(
				"gemini stream candidate index = %d, want 0", *c.Index,
			))
		}
		candidate = &c
	}

	// Prompt-blocked with no candidates: a completed refusal.
	if candidate == nil {
		if chunk.PromptFeedback != nil && chunk.PromptFeedback.BlockReason != nil &&
			*chunk.PromptFeedback.BlockReason != "" &&
			*chunk.PromptFeedback.BlockReason != "BLOCK_REASON_UNSPECIFIED" {
			if s.sawTerm {
				return nil, s.wireError(errors.New("gemini stream chunk after the terminal"))
			}
			s.sawTerm = true
			reason := *chunk.PromptFeedback.BlockReason
			if err := s.report.Note(
				FeatureGeminiPromptBlocked,
				"promptFeedback.blockReason",
				fmt.Sprintf("the prompt was blocked (blockReason %s) and the stream is rendered as a refusal", reason),
			); err != nil {
				return nil, err
			}
			events := s.startEvents()
			refusal, err := s.appendRefusal(fmt.Sprintf("the prompt was blocked by the provider (blockReason %s)", reason))
			if err != nil {
				return nil, err
			}
			events = append(events, refusal...)
			s.refused = true
			s.heldTerminal = s.closeAllAndHold()
			return events, nil
		}
		// A chunk with no candidates and no block reason mid-stream carries
		// nothing (some providers emit empty keep-alives); after the terminal
		// it is a usage-only chunk and absorbed below.
		if s.sawTerm {
			return s.absorbTrailing(chunk)
		}
		return nil, nil
	}

	if s.sawTerm {
		// After the finish chunk only accounting may follow.
		return s.absorbTrailing(chunk)
	}

	events := s.startEvents()

	// Content parts.
	if candidate.Content != nil {
		if candidate.Content.Role != "" && candidate.Content.Role != "model" {
			return nil, s.wireError(fmt.Errorf(
				"gemini stream candidate content role = %q, want model or absent",
				candidate.Content.Role,
			))
		}
		for i, part := range candidate.Content.Parts {
			partEvents, err := s.convertPart(part, i)
			if err != nil {
				return nil, err
			}
			events = append(events, partEvents...)
		}
	}

	// Metadata losses recorded per chunk, gated to once per stream.
	if err := s.recordCandidateMetadata(candidate); err != nil {
		return nil, err
	}
	if chunk.UsageMetadata != nil {
		if err := s.recordUsage(chunk.UsageMetadata); err != nil {
			return nil, err
		}
	}
	if chunk.ModelStatus != nil {
		if !s.modelStatusLossRecorded {
			s.modelStatusLossRecorded = true
			if err := s.report.Lose(
				s.policy,
				FeatureGeminiModelStatus,
				"modelStatus",
				"the model status (lifecycle stage/retirement) cannot be reproduced in the target response",
			); err != nil {
				return nil, err
			}
		}
	}

	// A chunk carrying finishReason is terminal: hold the item-closing events
	// and the terminal envelope for release at EOF (or a late [DONE]).
	if candidate.FinishReason != nil && *candidate.FinishReason != "" &&
		*candidate.FinishReason != "FINISH_REASON_UNSPECIFIED" {
		reason := *candidate.FinishReason
		switch {
		case reason == "STOP", reason == "MAX_TOKENS":
			// mapped below at release
		case refusalFinishReasons[reason]:
			// rendered as a refusal at release
		default:
			return nil, &UnrepresentableError{
				Protocol: string(UpstreamGemini),
				Path:     "candidates[].finishReason",
				Detail:   fmt.Sprintf("the provider ended the stream with finishReason %s", reason),
			}
		}
		s.sawTerm = true
		s.finishReason = reason
		if refusalFinishReasons[reason] {
			// The same rendering as the non-streaming decode: the stop is a
			// refusal, carried by a refusal part on the message item.
			text := fmt.Sprintf("the provider stopped the response (finishReason %s)", reason)
			if candidate.FinishMessage != nil && *candidate.FinishMessage != "" {
				text = *candidate.FinishMessage
			}
			if err := s.report.Note(
				FeatureGeminiFinishRefusal,
				"candidates[].finishReason",
				fmt.Sprintf("finishReason %s rendered as a refusal (gemini_finish_refusal encoding)", reason),
			); err != nil {
				return nil, err
			}
			refusal, err := s.appendRefusal(text)
			if err != nil {
				return nil, err
			}
			events = append(events, refusal...)
			s.refused = true
		}
		s.heldTerminal = s.closeAllAndHold()
		return events, nil
	}

	return events, nil
}

// startEvents returns response.created and response.in_progress the first
// time it is called, and nothing afterwards: every path that emits an output
// item calls it first, so the lifecycle always opens with the response.
func (s *geminiResponsesStreamState) startEvents() []ResponsesSSEEvent {
	if s.started {
		return nil
	}
	s.started = true
	envelope := s.baseEnvelope("in_progress")
	return []ResponsesSSEEvent{
		s.builder.Created(envelope),
		s.builder.InProgress(envelope),
	}
}

// absorbTrailing handles a chunk after the terminal: only usageMetadata may
// carry new information (the last one wins); content parts are corrupt wire.
func (s *geminiResponsesStreamState) absorbTrailing(chunk geminigeneratecontent.GenerateContentResponse) ([]ResponsesSSEEvent, error) {
	if len(chunk.Candidates) > 0 && chunk.Candidates[0].Content != nil &&
		len(chunk.Candidates[0].Content.Parts) > 0 {
		return nil, s.wireError(errors.New(
			"gemini stream chunk with content parts after the finish chunk",
		))
	}
	if chunk.UsageMetadata != nil {
		if err := s.recordUsage(chunk.UsageMetadata); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// convertPart converts one Gemini stream part into Responses events.
func (s *geminiResponsesStreamState) convertPart(
	part geminigeneratecontent.Part, partIndex int,
) ([]ResponsesSSEEvent, error) {
	path := fmt.Sprintf("candidates[].content.parts[%d]", partIndex)

	// Unsupported payloads are unrepresentable in the Responses stream.
	switch {
	case part.InlineData != nil, part.FileData != nil,
		part.ExecutableCode != nil, part.CodeExecutionResult != nil,
		part.ToolCall != nil, part.ToolResponse != nil, part.AudioTranscription != nil:
		return nil, &UnrepresentableError{
			Protocol: string(UpstreamGemini),
			Path:     path,
			Detail:   "the part payload cannot be represented in the target dialect",
		}
	case part.FunctionResponse != nil:
		return nil, &UnrepresentableError{
			Protocol: string(UpstreamGemini),
			Path:     path,
			Detail:   "a functionResponse part cannot appear in a model response stream",
		}
	}

	// Part modifiers are an observable loss.
	if geminigeneratecontent.RawMemberPresent(part.VideoMetadata) ||
		geminigeneratecontent.RawMemberPresent(part.SpeechMetadata) ||
		geminigeneratecontent.RawMemberPresent(part.PartMetadata) ||
		geminigeneratecontent.RawMemberPresent(part.MediaResolution) ||
		part.MediaProcessing != nil {
		if err := s.report.Lose(
			s.policy,
			FeatureGeminiPartMetadata,
			path,
			"part modifier metadata cannot be reproduced in the target response",
		); err != nil {
			return nil, err
		}
	}
	// A signature on a non-call part is an observable loss.
	if part.ThoughtSignature != nil && part.FunctionCall == nil {
		if err := s.report.Lose(
			s.policy,
			FeatureGeminiResponseThoughtSignature,
			path,
			"a thought signature on a non-function-call stream part cannot be reproduced in the target response",
		); err != nil {
			return nil, err
		}
	}

	if part.FunctionCall != nil {
		return s.convertFunctionCall(part, path)
	}

	if part.Text == nil {
		// A signature-only part carries no payload and emits nothing.
		return nil, nil
	}
	thought := part.Thought != nil && *part.Thought
	if thought {
		// Same capability rules as the non-streaming decode: thought text is
		// provider reasoning; mapped under the capability or an approved loss.
		switch {
		case s.capabilities.ProviderReasoningThinking:
			if err := s.report.Note(
				FeatureProviderReasoningText,
				path,
				"provider reasoning mapped to a native thinking block (provider_reasoning_thinking encoding)",
			); err != nil {
				return nil, err
			}
			events, err := s.emitThoughtText(*part.Text, path)
			if err != nil {
				return nil, err
			}
			return events, nil
		case s.capabilities.ProviderReasoningText:
			if !s.thoughtTextNoted {
				s.thoughtTextNoted = true
				if err := s.report.Note(
					FeatureProviderReasoningText,
					path,
					"provider reasoning text mapped to ordinary text (provider_reasoning_text capability)",
				); err != nil {
					return nil, err
				}
			}
		default:
			if !s.thoughtLossNoted {
				s.thoughtLossNoted = true
				if err := s.report.Lose(
					s.policy, FeatureProviderReasoningText, path,
					"provider reasoning text cannot be reproduced in the target response",
				); err != nil {
					return nil, err
				}
			}
			return nil, nil
		}
	}
	return s.emitTextDelta(*part.Text)
}

// emitTextDelta opens the message item and content part on first text and
// emits one response.output_text.delta per chunk.
func (s *geminiResponsesStreamState) emitTextDelta(delta string) ([]ResponsesSSEEvent, error) {
	if delta == "" {
		return nil, nil
	}
	events, err := s.ensureMessageOpen()
	if err != nil {
		return nil, err
	}
	item := &s.items[len(s.items)-1]
	message := item.item.(*ResponsesOutputMessage)
	var contentIndex int64
	var key geminiPartKey
	if item.openPartIndex >= 0 && len(message.Content) > int(item.openPartIndex) {
		// Continue the open content part: Gemini streams one text part as
		// several chunks.
		contentIndex = item.openPartIndex
		key = geminiPartKey{outputIndex: item.outputIndex, contentIndex: contentIndex}
	} else {
		if err := s.budget.addPart(); err != nil {
			return nil, s.wireError(err)
		}
		contentIndex = int64(len(message.Content))
		key = geminiPartKey{outputIndex: item.outputIndex, contentIndex: contentIndex}
		s.textBufs[key] = &strings.Builder{}
		message.Content = append(message.Content, &ResponsesOutputText{
			Type:        "output_text",
			Text:        "",
			Annotations: []ResponsesAnnotation{},
		})
		item.openPartIndex = contentIndex
		events = append(events, s.builder.ContentPartAdded(
			message.ID,
			item.outputIndex,
			contentIndex,
			&ResponsesStreamOutputTextPart{
				Type:        "output_text",
				Text:        "",
				Annotations: []ResponsesAnnotation{},
			},
		))
	}
	buf := s.textBufs[key]
	if err := s.checkAccumulated(buf, len(delta), "text delta"); err != nil {
		return nil, err
	}
	buf.WriteString(delta)
	events = append(events, s.builder.TextDelta(
		message.ID, item.outputIndex, contentIndex, delta,
	))
	return events, nil
}

// emitThoughtText renders thought text as a reasoning summary delta when the
// capability maps it to thinking; the summary is a reasoning item.
func (s *geminiResponsesStreamState) emitThoughtText(delta, path string) ([]ResponsesSSEEvent, error) {
	if delta == "" {
		return nil, nil
	}
	_ = path
	events, err := s.ensureReasoningOpen()
	if err != nil {
		return nil, err
	}
	item := &s.items[len(s.items)-1]
	reasoning := item.item.(*ResponsesReasoningOutputItem)
	buf := s.thoughtBufs[geminiPartKey{outputIndex: item.outputIndex}]
	if buf == nil {
		buf = &strings.Builder{}
		s.thoughtBufs[geminiPartKey{outputIndex: item.outputIndex}] = buf
		if len(reasoning.Summary) == 0 {
			if err := s.budget.addPart(); err != nil {
				return nil, s.wireError(err)
			}
			reasoning.Summary = []ResponsesReasoningSummary{{Type: "summary_text", Text: ""}}
			events = append(events, s.builder.ReasoningSummaryPartAdded(
				reasoning.ID, item.outputIndex, 0, openairesponses.SummaryTextPart{
					Type: "summary_text", Text: "",
				},
			))
		}
	}
	if err := s.checkAccumulated(buf, len(delta), "thought delta"); err != nil {
		return nil, err
	}
	buf.WriteString(delta)
	events = append(events, s.builder.ReasoningSummaryTextDelta(
		reasoning.ID, item.outputIndex, 0, delta,
	))
	return events, nil
}

// ensureMessageOpen opens a new output message item when the last item is not
// an open message.
func (s *geminiResponsesStreamState) ensureMessageOpen() ([]ResponsesSSEEvent, error) {
	events := s.closeOpenReasoning()
	if n := len(s.items); n > 0 {
		if item := &s.items[n-1]; item.isMessage() && item.item.(*ResponsesOutputMessage).Status != ResponsesItemCompleted {
			return events, nil
		}
	}
	if err := s.budget.addItem(); err != nil {
		return nil, s.wireError(err)
	}
	outputIndex := int64(len(s.items))
	message := &ResponsesOutputMessage{
		ID:      s.ctx.IDs.New("msg_"),
		Type:    "message",
		Role:    "assistant",
		Status:  ResponsesItemInProgress,
		Content: []ResponsesOutputContentPart{},
	}
	s.items = append(s.items, openResponsesItem{outputIndex: outputIndex, item: message, openPartIndex: -1})
	return append(events, s.builder.OutputItemAdded(outputIndex, message)), nil
}

// ensureReasoningOpen opens a reasoning item when none is open.
func (s *geminiResponsesStreamState) ensureReasoningOpen() ([]ResponsesSSEEvent, error) {
	if n := len(s.items); n > 0 {
		if item := &s.items[n-1]; isGeminiReasoningItem(item) &&
			item.item.(*ResponsesReasoningOutputItem).Status != ResponsesItemCompleted {
			return nil, nil
		}
	}
	events := s.closeOpenMessage()
	if err := s.budget.addItem(); err != nil {
		return nil, s.wireError(err)
	}
	outputIndex := int64(len(s.items))
	reasoning := &ResponsesReasoningOutputItem{
		ID:      s.ctx.IDs.New("rs_"),
		Type:    "reasoning",
		Status:  ResponsesItemInProgress,
		Summary: []ResponsesReasoningSummary{},
	}
	s.items = append(s.items, openResponsesItem{outputIndex: outputIndex, item: reasoning, openPartIndex: -1})
	return append(events, s.builder.OutputItemAdded(outputIndex, reasoning)), nil
}

// convertFunctionCall emits one complete function call: close any open item,
// then output_item.added, one arguments delta with the whole arguments string,
// arguments done, output_item.done. The thoughtSignature is stored in the
// signature store at this moment.
func (s *geminiResponsesStreamState) convertFunctionCall(
	part geminigeneratecontent.Part, path string,
) ([]ResponsesSSEEvent, error) {
	call := part.FunctionCall
	if call.Name == "" {
		return nil, s.wireError(fmt.Errorf("gemini stream functionCall at %s has no name", path))
	}
	args := ""
	if len(call.Args) > 0 {
		trimmed := strings.TrimSpace(string(call.Args))
		if trimmed != "" && trimmed != "null" {
			if !json.Valid(call.Args) {
				return nil, s.wireError(fmt.Errorf(
					"gemini stream functionCall at %s carries malformed argument JSON", path,
				))
			}
			args = trimmed
		}
	}
	if args == "" {
		args = "{}"
	}
	if len(args) > maxStreamAccumulatedBytes {
		return nil, s.wireError(fmt.Errorf(
			"gemini stream functionCall arguments at %s exceed %d bytes",
			path,
			maxStreamAccumulatedBytes,
		))
	}
	if err := s.chargeTotal(len(args), "tool arguments"); err != nil {
		return nil, err
	}
	if err := s.budget.addItem(); err != nil {
		return nil, s.wireError(err)
	}
	if err := s.budget.addToolCall(); err != nil {
		return nil, s.wireError(err)
	}
	outputIndex := int64(len(s.items))
	itemID := s.ctx.IDs.New("fc_")
	callID := call.ID
	if callID == "" {
		callID = s.ctx.IDs.New("gcall_")
		if err := s.report.Note(
			FeatureGeminiCallIDSynthesized, path,
			"the upstream function call carried no id; a proxy-minted call id was emitted",
		); err != nil {
			return nil, err
		}
	}
	name := call.Name
	if s.ctx.ToolNames != nil {
		if clientName, ns := s.ctx.ToolNames.clientCallName(call.Name); ns != "" {
			name = clientName
		}
	}
	// The call identity renders into the done events and the terminal
	// envelope, so it is retained semantic state like the arguments.
	if err := s.chargeTotal(len(callID)+len(name), "tool-call identity"); err != nil {
		return nil, err
	}
	events := s.closeOpenMessage()
	events = append(events, s.closeOpenReasoning()...)
	item := &ResponsesFunctionCallOutputItem{
		ID:        itemID,
		Type:      "function_call",
		Status:    ResponsesItemCompleted,
		CallID:    callID,
		Name:      name,
		Arguments: args,
	}
	s.items = append(s.items, openResponsesItem{outputIndex: outputIndex, item: item})
	// Retain the thought signature for the next turn, keyed by the
	// client-visible call id.
	if part.ThoughtSignature != nil && *part.ThoughtSignature != "" &&
		s.ctx.GeminiSignatures != nil {
		s.ctx.GeminiSignatures.Put(s.ctx.GeminiSignatureScope, callID, *part.ThoughtSignature)
	}
	return append(events,
		s.builder.OutputItemAdded(outputIndex, item),
		s.builder.FunctionArgumentsDelta(itemID, outputIndex, args),
		s.builder.FunctionArgumentsDone(itemID, outputIndex, args),
		s.builder.OutputItemDone(outputIndex, item),
	), nil
}

// appendRefusal adds a refusal content part to the open message item, opening
// one when none is open (the same placement as the non-streaming decode). An
// open text part is closed first, so at most one part is ever open. The
// refusal text is streamed as one delta so the done event reconciles with it;
// the part stays open until closeAllAndHold.
func (s *geminiResponsesStreamState) appendRefusal(text string) ([]ResponsesSSEEvent, error) {
	events, err := s.ensureMessageOpen()
	if err != nil {
		return nil, err
	}
	if err := s.budget.addPart(); err != nil {
		return nil, s.wireError(err)
	}
	if err := s.chargeTotal(len(text), "refusal"); err != nil {
		return nil, err
	}
	item := &s.items[len(s.items)-1]
	message := item.item.(*ResponsesOutputMessage)
	events = append(events, s.closeOpenPart(item)...)
	contentIndex := int64(len(message.Content))
	message.Content = append(message.Content, &ResponsesOutputRefusal{Type: "refusal", Refusal: text})
	item.openPartIndex = contentIndex
	events = append(events,
		s.builder.ContentPartAdded(message.ID, item.outputIndex, contentIndex,
			&ResponsesStreamRefusalPart{Type: "refusal", Refusal: ""}),
		s.builder.RefusalDelta(message.ID, item.outputIndex, contentIndex, text),
	)
	return events, nil
}

// closeOpenPart emits the done events of the message item's open content part
// (text or refusal done, then content_part.done), materializing the streamed
// text into the part. It does nothing when no part is open.
func (s *geminiResponsesStreamState) closeOpenPart(item *openResponsesItem) []ResponsesSSEEvent {
	message, ok := item.item.(*ResponsesOutputMessage)
	if !ok || item.openPartIndex < 0 || int(item.openPartIndex) >= len(message.Content) {
		item.openPartIndex = -1
		return nil
	}
	contentIndex := item.openPartIndex
	item.openPartIndex = -1
	switch part := message.Content[contentIndex].(type) {
	case *ResponsesOutputText:
		if buf := s.textBufs[geminiPartKey{outputIndex: item.outputIndex, contentIndex: contentIndex}]; buf != nil {
			part.Text = buf.String()
		}
		return []ResponsesSSEEvent{
			s.builder.TextDone(message.ID, item.outputIndex, contentIndex, part.Text),
			s.builder.ContentPartDone(message.ID, item.outputIndex, contentIndex,
				&ResponsesStreamOutputTextPart{
					Type:        "output_text",
					Text:        part.Text,
					Annotations: []ResponsesAnnotation{},
				}),
		}
	case *ResponsesOutputRefusal:
		return []ResponsesSSEEvent{
			s.builder.RefusalDone(message.ID, item.outputIndex, contentIndex, part.Refusal),
			s.builder.ContentPartDone(message.ID, item.outputIndex, contentIndex,
				&ResponsesStreamRefusalPart{Type: "refusal", Refusal: part.Refusal}),
		}
	}
	return nil
}

// closeOpenMessage closes the last item when it is an open message: its open
// content part's done events, then output_item.done with status completed.
func (s *geminiResponsesStreamState) closeOpenMessage() []ResponsesSSEEvent {
	n := len(s.items)
	if n == 0 {
		return nil
	}
	item := &s.items[n-1]
	message, ok := item.item.(*ResponsesOutputMessage)
	if !ok || message.Status == ResponsesItemCompleted {
		return nil
	}
	events := s.closeOpenPart(item)
	message.Status = ResponsesItemCompleted
	return append(events, s.builder.OutputItemDone(item.outputIndex, message))
}

// closeOpenReasoning closes the last item when it is an open reasoning item:
// the summary text and part done events, then output_item.done with status
// completed.
func (s *geminiResponsesStreamState) closeOpenReasoning() []ResponsesSSEEvent {
	n := len(s.items)
	if n == 0 {
		return nil
	}
	item := &s.items[n-1]
	reasoning, ok := item.item.(*ResponsesReasoningOutputItem)
	if !ok || reasoning.Status == ResponsesItemCompleted {
		return nil
	}
	text := ""
	if buf := s.thoughtBufs[geminiPartKey{outputIndex: item.outputIndex}]; buf != nil {
		text = buf.String()
	}
	if len(reasoning.Summary) == 0 {
		reasoning.Summary = []ResponsesReasoningSummary{{Type: "summary_text", Text: text}}
	} else {
		reasoning.Summary[0].Text = text
	}
	reasoning.Status = ResponsesItemCompleted
	return []ResponsesSSEEvent{
		s.builder.ReasoningSummaryTextDone(reasoning.ID, item.outputIndex, 0, text),
		s.builder.ReasoningSummaryPartDone(reasoning.ID, item.outputIndex, 0, openairesponses.SummaryTextPart{
			Type: "summary_text", Text: text,
		}),
		s.builder.OutputItemDone(item.outputIndex, reasoning),
	}
}

// closeAllAndHold closes the open item and returns its closing events, which
// the caller holds with the terminal: the terminal envelope is appended at
// release so a trailing usage chunk is reflected. Function-call items are
// complete when emitted, and every earlier item was closed when the next one
// opened, so only the last item can still be open.
func (s *geminiResponsesStreamState) closeAllAndHold() []ResponsesSSEEvent {
	return append(s.closeOpenMessage(), s.closeOpenReasoning()...)
}

// recordCandidateMetadata records the per-chunk candidate metadata losses once
// per stream.
func (s *geminiResponsesStreamState) recordCandidateMetadata(candidate *geminigeneratecontent.Candidate) error {
	if candidate.SafetyRatings != nil && !s.safetyLossRecorded {
		s.safetyLossRecorded = true
		if err := s.report.Lose(
			s.policy,
			FeatureGeminiSafetyMetadata,
			"candidates[].safetyRatings",
			"Gemini safety ratings cannot be reproduced in the target response",
		); err != nil {
			return err
		}
	}
	if (geminigeneratecontent.RawMemberPresent(candidate.CitationMetadata) ||
		geminigeneratecontent.RawMemberPresent(candidate.GroundingMetadata) ||
		geminigeneratecontent.RawMemberPresent(candidate.GroundingAttributions) ||
		geminigeneratecontent.RawMemberPresent(candidate.URLContextMetadata)) && !s.groundingLossRecorded {
		s.groundingLossRecorded = true
		if err := s.report.Lose(
			s.policy,
			FeatureGeminiGroundingMetadata,
			"candidates[].groundingMetadata",
			"Gemini citation/grounding/url-context metadata cannot be reproduced in the target response",
		); err != nil {
			return err
		}
	}
	if (candidate.AvgLogprobs != nil || geminigeneratecontent.RawMemberPresent(candidate.LogprobsResult)) &&
		!s.logprobsLossRecorded {
		s.logprobsLossRecorded = true
		if err := s.report.Lose(
			s.policy,
			FeatureLogprobs,
			"candidates[].avgLogprobs",
			"token log-probabilities cannot be reproduced in the target",
		); err != nil {
			return err
		}
	}
	if candidate.TokenCount != nil && !s.tokenCountLossRecorded {
		s.tokenCountLossRecorded = true
		if err := s.report.Lose(
			s.policy,
			FeatureGeminiUsageBreakdown,
			"candidates[].tokenCount",
			"the candidate token count breakdown cannot be reproduced in the target response",
		); err != nil {
			return err
		}
	}
	if candidate.ContinuationToken != nil && *candidate.ContinuationToken != "" {
		return &UnrepresentableError{
			Protocol: string(UpstreamGemini),
			Path:     "candidates[].continuationToken",
			Detail:   "a continuation token cannot be represented in the target dialect",
		}
	}
	return nil
}

// recordUsage folds a usage chunk into the terminal usage (the last one wins).
func (s *geminiResponsesStreamState) recordUsage(u *geminigeneratecontent.UsageMetadata) error {
	deref := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	input := deref(u.PromptTokenCount) + deref(u.ToolUsePromptTokenCount)
	cached := deref(u.CachedContentTokenCount)
	output := deref(u.CandidatesTokenCount) + deref(u.ThoughtsTokenCount)
	reasoning := deref(u.ThoughtsTokenCount)
	total := deref(u.TotalTokenCount)
	s.usage = &ResponsesUsage{
		InputTokens:  input,
		OutputTokens: output,
		TotalTokens:  total,
		InputTokensDetails: &UsageInputTokensDetails{
			CachedTokens: cached,
		},
		OutputTokensDetails: &UsageOutputTokensDetails{
			ReasoningTokens: reasoning,
		},
	}
	s.usagePresence = usagePresence{
		input:      u.PromptTokenCount != nil || u.ToolUsePromptTokenCount != nil,
		output:     u.CandidatesTokenCount != nil || u.ThoughtsTokenCount != nil,
		total:      u.TotalTokenCount != nil,
		cacheRead:  u.CachedContentTokenCount != nil,
		cacheWrite: false,
		reasoning:  u.ThoughtsTokenCount != nil,
	}
	clamp := clampResponsesUsage(s.usage, s.usagePresence)
	if err := clamp.record(&s.report, "usageMetadata"); err != nil {
		return err
	}
	if len(u.PromptTokensDetails) > 0 || len(u.CacheTokensDetails) > 0 ||
		len(u.CandidatesTokensDetails) > 0 || len(u.ToolUsePromptTokensDetails) > 0 {
		if !s.usageBreakdownLossRecorded {
			s.usageBreakdownLossRecorded = true
			if err := s.report.Lose(
				s.policy,
				FeatureGeminiUsageBreakdown,
				"usageMetadata.*TokensDetails",
				"the per-modality token breakdowns cannot be reproduced in the target response",
			); err != nil {
				return err
			}
		}
	}
	if u.ServiceTier != nil && *u.ServiceTier != "" {
		if !s.serviceTierLossRecorded {
			s.serviceTierLossRecorded = true
			if err := s.report.Lose(
				s.policy,
				FeatureResponseServiceTier,
				"usageMetadata.serviceTier",
				"the upstream service tier actually served cannot be reproduced in the target",
			); err != nil {
				return err
			}
		}
	}
	return nil
}

// baseEnvelope builds the Responses envelope for lifecycle/terminal events.
func (s *geminiResponsesStreamState) baseEnvelope(status string) ResponseEnvelope {
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
		envelope.ParallelToolCalls = new(s.echo.ParallelToolCalls)
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

// finalOutputItems returns the completed output items in output order.
func (s *geminiResponsesStreamState) finalOutputItems() []ResponsesOutputItem {
	out := make([]ResponsesOutputItem, 0, len(s.items))
	for i := range s.items {
		out = append(out, s.items[i].item)
	}
	return out
}

// terminalEnvelope builds the terminal envelope with the final usage.
func (s *geminiResponsesStreamState) terminalEnvelope() ResponsesSSEEvent {
	// A refusal (blocked prompt or refusal finishReason) is incomplete with
	// reason content_filter, the Responses contract's filtered-response
	// representation, which the Anthropic composition renders as the refusal
	// stop reason. MAX_TOKENS is incomplete with max_output_tokens.
	incompleteReason := ""
	switch {
	case s.refused:
		incompleteReason = "content_filter"
	case s.finishReason == "MAX_TOKENS":
		incompleteReason = "max_output_tokens"
	}
	if incompleteReason != "" {
		envelope := s.baseEnvelope("incomplete")
		envelope.Output = s.finalOutputItems()
		envelope.Usage = s.usage
		envelope.IncompleteDetails = &ResponsesIncompleteDetails{Reason: incompleteReason}
		return s.builder.Incomplete(envelope)
	}
	envelope := s.baseEnvelope("completed")
	envelope.Output = s.finalOutputItems()
	envelope.Usage = s.usage
	return s.builder.Completed(envelope)
}

// releaseTerminal returns the held item-closing events plus the terminal
// envelope, once.
func (s *geminiResponsesStreamState) releaseTerminal() ([]ResponsesSSEEvent, bool) {
	if s.terminalDone {
		return nil, false
	}
	s.terminalDone = true
	held := s.heldTerminal
	s.heldTerminal = nil
	return append(held, s.terminalEnvelope()), true
}

// FinalizeEOF releases the held terminal when the stream ended after a finish
// chunk (Gemini has no [DONE] sentinel, so EOF is the release). A stream that
// ends before any finish reason is a truncation, never a success.
func (s *geminiResponsesStreamState) FinalizeEOF() ([]ResponsesSSEEvent, error) {
	if s.sawTerm {
		if s.terminalDone {
			return nil, nil
		}
		held, _ := s.releaseTerminal()
		return held, nil
	}
	return nil, upstreamWireError(UpstreamGemini, http.StatusOK, errStreamTruncated)
}

// continuityTurns implements continuityTurnSource with the same semantics as
// (*chatResponsesStreamState).continuityTurns: the request turns followed by
// the terminal assistant turns.
func (s *geminiResponsesStreamState) continuityTurns(
	requestTurns []CanonicalTurn, depth int,
) (string, []CanonicalTurn, int, int, bool) {
	if !s.terminalDone {
		return "", nil, 0, 0, false
	}
	turns := make([]CanonicalTurn, 0, len(requestTurns)+2)
	turns = append(turns, requestTurns...)
	omitted := 0
	for _, item := range s.finalOutputItems() {
		switch value := item.(type) {
		case *ResponsesOutputMessage:
			parts, err := responsesOutputContentToCanonical(value.Content)
			if err != nil || len(parts) == 0 {
				continue
			}
			kept := parts[:0]
			for _, part := range parts {
				if _, ok := part.(CanonicalThinkingPart); ok {
					continue
				}
				kept = append(kept, part)
			}
			if len(kept) == 0 {
				continue
			}
			turns = append(turns, CanonicalTurn{Role: CanonicalAssistant, Parts: kept})
		case *ResponsesFunctionCallOutputItem:
			arguments := ParseToolArguments(value.Arguments)
			if !arguments.IsObject {
				omitted++
				continue
			}
			part := CanonicalFunctionCall{
				CallID:    value.CallID,
				Name:      value.Name,
				Arguments: arguments.Object,
			}
			if len(turns) > 0 && turns[len(turns)-1].Role == CanonicalAssistant {
				turns[len(turns)-1].Parts = append(turns[len(turns)-1].Parts, part)
			} else {
				turns = append(turns, CanonicalTurn{Role: CanonicalAssistant, Parts: []CanonicalPart{part}})
			}
		default:
			continue
		}
	}
	if len(turns) == 0 {
		return "", nil, 0, 0, false
	}
	return s.responseID, turns, depth, omitted, true
}

// geminiToResponsesConverter adapts geminiResponsesStreamState to frameConverter,
// marshaling typed Responses events into frames. Gemini streams are data-only
// (no event name); a non-empty event name or a [DONE] sentinel is handled here.
type geminiToResponsesConverter struct {
	state *geminiResponsesStreamState
}

func newGeminiToResponsesConverter(state *geminiResponsesStreamState) *geminiToResponsesConverter {
	return &geminiToResponsesConverter{state: state}
}

// Convert processes one upstream Gemini SSE frame.
func (c *geminiToResponsesConverter) Convert(frame SSEEvent) (convertedBatch, error) {
	if frame.Event != "" {
		return convertedBatch{}, upstreamWireError(
			UpstreamGemini, http.StatusOK,
			errors.New("gemini stream frame carries an SSE event name; Gemini frames are data-only"),
		)
	}
	data := trimJSONSpace(frame.Data)
	if string(data) == "[DONE]" {
		// Google never sends [DONE]. After the terminal it releases the held
		// terminal; before it, the stream is truncated.
		if !c.state.sawTerm {
			return convertedBatch{}, upstreamWireError(
				UpstreamGemini, http.StatusOK,
				errors.New("gemini stream [DONE] before a terminal condition"),
			)
		}
		held, ok := c.state.releaseTerminal()
		if !ok {
			return convertedBatch{}, c.state.wireError(errors.New(
				"gemini stream [DONE] after the terminal was released",
			))
		}
		batch, err := marshalResponsesEvents(held)
		batch.Terminal = true
		return batch, err
	}
	chunk, err := geminigeneratecontent.DecodeResponse(frame.Data)
	if err != nil {
		return convertedBatch{}, upstreamWireError(
			UpstreamGemini, http.StatusOK,
			fmt.Errorf("gemini stream chunk: %w", err),
		)
	}
	events, err := c.state.Convert(chunk)
	if err != nil {
		return convertedBatch{}, err
	}
	return marshalResponsesEvents(events)
}

// FinalizeEOF releases the held terminal on upstream EOF.
func (c *geminiToResponsesConverter) FinalizeEOF() (convertedBatch, error) {
	events, err := c.state.FinalizeEOF()
	if err != nil {
		return convertedBatch{}, err
	}
	batch, err := marshalResponsesEvents(events)
	batch.Terminal = true
	return batch, err
}

// ConversionReport returns the accumulated report of the state.
func (c *geminiToResponsesConverter) ConversionReport() *ConversionReport {
	return &c.state.report
}

// ErrorEvent builds a client-dialect Responses error event frame.
func (c *geminiToResponsesConverter) ErrorEvent(err error) (frameEvent, bool) {
	return responsesErrorFrame(err, c.state.builder.NextSequenceNumber())
}

// continuityTurns delegates to the state (continuityTurnSource).
func (c *geminiToResponsesConverter) continuityTurns(
	requestTurns []CanonicalTurn, depth int,
) (string, []CanonicalTurn, int, int, bool) {
	return c.state.continuityTurns(requestTurns, depth)
}

// geminiToAnthropicConverter composes the Gemini->Responses and
// Responses->Anthropic state machines strictly in memory, like
// chatToAnthropicConverter.
type geminiToAnthropicConverter struct {
	gemini    *geminiResponsesStreamState
	anthropic *anthropicResponsesStreamState
}

func newGeminiToAnthropicConverter(
	gemini *geminiResponsesStreamState,
	anthropic *anthropicResponsesStreamState,
) *geminiToAnthropicConverter {
	return &geminiToAnthropicConverter{gemini: gemini, anthropic: anthropic}
}

// Convert processes one upstream Gemini SSE frame through both state machines.
func (c *geminiToAnthropicConverter) Convert(frame SSEEvent) (convertedBatch, error) {
	if frame.Event != "" {
		return convertedBatch{}, upstreamWireError(
			UpstreamGemini, http.StatusOK,
			errors.New("gemini stream frame carries an SSE event name; Gemini frames are data-only"),
		)
	}
	data := trimJSONSpace(frame.Data)
	if string(data) == "[DONE]" {
		if !c.gemini.sawTerm {
			return convertedBatch{}, upstreamWireError(
				UpstreamGemini, http.StatusOK,
				errors.New("gemini stream [DONE] before a terminal condition"),
			)
		}
		held, ok := c.gemini.releaseTerminal()
		if !ok {
			return convertedBatch{}, c.gemini.wireError(errors.New(
				"gemini stream [DONE] after the terminal was released",
			))
		}
		return c.convertResponsesEvents(held, true)
	}
	chunk, err := geminigeneratecontent.DecodeResponse(frame.Data)
	if err != nil {
		return convertedBatch{}, upstreamWireError(
			UpstreamGemini, http.StatusOK,
			fmt.Errorf("gemini stream chunk: %w", err),
		)
	}
	responsesEvents, err := c.gemini.Convert(chunk)
	if err != nil {
		return convertedBatch{}, err
	}
	return c.convertResponsesEvents(responsesEvents, false)
}

func (c *geminiToAnthropicConverter) convertResponsesEvents(
	responsesEvents []ResponsesSSEEvent, terminal bool,
) (convertedBatch, error) {
	var anthropicEvents []AnthropicStreamEvent
	for _, event := range responsesEvents {
		converted, err := c.anthropic.Convert(event)
		if err != nil {
			return convertedBatch{}, err
		}
		anthropicEvents = append(anthropicEvents, converted...)
	}
	batch, err := marshalAnthropicEvents(anthropicEvents)
	if err != nil {
		return convertedBatch{}, err
	}
	batch.Terminal = terminal || c.anthropic.sawTerminal
	return batch, nil
}

// FinalizeEOF releases the held Gemini terminal through the Anthropic state.
func (c *geminiToAnthropicConverter) FinalizeEOF() (convertedBatch, error) {
	events, err := c.gemini.FinalizeEOF()
	if err != nil {
		return convertedBatch{}, err
	}
	return c.convertResponsesEvents(events, true)
}

// ConversionReport merges both states' reports like chatToAnthropicConverter.
func (c *geminiToAnthropicConverter) ConversionReport() *ConversionReport {
	merged := ConversionReport{
		Dropped: c.gemini.report.Dropped + c.anthropic.report.Dropped,
	}
	seen := make(map[string]struct{}, len(c.gemini.report.Losses)+len(c.anthropic.report.Losses))
	for _, losses := range [][]ConversionLoss{c.gemini.report.Losses, c.anthropic.report.Losses} {
		for _, loss := range losses {
			key := fmt.Sprintf("%s at %s", loss.Feature, loss.Path)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			merged.Losses = append(merged.Losses, loss)
		}
	}
	return &merged
}

// ErrorEvent builds a client-dialect Anthropic error event frame.
func (c *geminiToAnthropicConverter) ErrorEvent(err error) (frameEvent, bool) {
	return anthropicErrorFrame(err)
}

// continuityTurns delegates to the Gemini state (continuityTurnSource).
func (c *geminiToAnthropicConverter) continuityTurns(
	requestTurns []CanonicalTurn, depth int,
) (string, []CanonicalTurn, int, int, bool) {
	return c.gemini.continuityTurns(requestTurns, depth)
}
