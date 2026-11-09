package transcode

import (
	"errors"
	"net/http"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// finish closes open items and builds the terminal event batch.
func (s *chatResponsesStreamState) finish(
	finishReason string,
) ([]ResponsesSSEEvent, error) {
	var events []ResponsesSSEEvent

	// Close every pending tool call: arguments done (arguments, "{}" for
	// empty, no name — ) then output_item.done. A
	// fragment that never received an identity (id or name) is malformed
	// upstream data: silently dropping it would hide the corruption behind a
	// successful completion. The slice order is fragment-index order, so the
	// terminal is deterministic (a map walk would randomize the done events
	// and with them the downstream adapter's deferral order).
	for _, pending := range s.pendingSlice() {
		if !pending.started {
			return nil, s.wireError(errors.New(
				"chat tool call fragment ended without an id and name",
			))
		}
		// Model-generated arguments are preserved byte-exact: the Responses
		// function_call arguments field is a string, and invalid model
		// output is never an upstream defect. Only the
		// snapshot-vs-accumulated identity check remains wire-corrupt.
		events = append(events, s.toolCallClosure(pending)...)
	}

	// Close open message items: content parts done, then output_item.done.
	// The accumulated text and refusal strings are materialized into the
	// parts ONCE here — the parts stayed empty while the stream was in
	// progress — so the done events and the terminal
	// envelope carry the full content.
	for i := range s.items {
		item := &s.items[i]
		message, ok := item.item.(*ResponsesOutputMessage)
		if !ok {
			continue
		}
		if message.Status == ResponsesItemCompleted {
			// Already closed inline at a message→tool transition
			// (closeOpenMessageItem): emitting the done events twice would
			// duplicate a part.done on the wire.
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

	// Close open reasoning items (provider_reasoning_thinking): the summary
	// done event, then output_item.done — the anthropic state machine
	// reconciles the terminal envelope against these observations.
	for i := range s.items {
		item := &s.items[i]
		reasoning, ok := item.item.(*ResponsesReasoningOutputItem)
		if !ok {
			continue
		}
		if reasoning.Status == ResponsesItemCompleted {
			// Already closed inline at a reasoning→content/tool
			// transition (closeOpenReasoningItem): its done events were
			// emitted there — emitting them twice is a duplicate
			// part.done on the wire and a duplicate reconciliation in
			// the anthropic state.
			continue
		}
		reasoning.Status = ResponsesItemCompleted
		// Materialize the accumulated summary text from the per-item builder,
		// exactly as the message parts are materialized above.
		summaryText := ""
		if buf := s.reasoningBufs[chatPartKey{outputIndex: item.outputIndex, contentIndex: 0}]; buf != nil {
			summaryText = buf.String()
		}
		if len(reasoning.Summary) == 0 {
			reasoning.Summary = []ResponsesReasoningSummary{{Type: "summary_text", Text: summaryText}}
		} else {
			reasoning.Summary[0].Text = summaryText
		}
		events = append(events,
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
		)
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

// continuityTurns rebuilds the canonical request turns the completed stream
// continues: the request turns the exchange decoded (already
// continuity-resolved, so the chain includes reconstructed history) followed
// by the assistant turns the terminal envelope rendered. It mirrors
// responseItemsToTurns over the terminal output items (message items become
// assistant turns with call folding; result/reasoning items skipped), using
// the state's own text/refusal accumulators would double-count: the terminal
// items already carry the materialized content at release. Call with the
// exchange's request turns; nil when the terminal never released.
// It also returns how many tool calls were omitted because their arguments did not
// decode as a JSON object, so the caller can report the omission rather than
// leave the next request to fail over proxy-held content.
func (s *chatResponsesStreamState) continuityTurns(requestTurns []CanonicalTurn, depth int) (string, []CanonicalTurn, int, int, bool) {
	if s.streamError != nil || !s.terminalReleased {
		return "", nil, 0, 0, false
	}
	items := s.finalOutputItems()
	turns := make([]CanonicalTurn, 0, len(requestTurns)+2)
	turns = append(turns, requestTurns...)
	omitted := 0
	for _, item := range items {
		switch value := item.(type) {
		case *ResponsesOutputMessage:
			parts, err := responsesOutputContentToCanonical(value.Content)
			if err != nil || len(parts) == 0 {
				continue
			}
			// Streamed reasoning items materialize as message items whose
			// parts may include synthesized thinking: thinking is ephemeral
			// model reasoning, not conversation content, and no Chat render
			// carries it — drop it so the retained chain always renders.
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
			// Retained history is re-validated by the REQUEST-side renderer,
			// which requires object-shaped arguments. The streamed arguments
			// are raw accumulated model output, so a call that did not decode
			// as a JSON object cannot be replayed; carrying it verbatim minted
			// a part the renderer rejects and failed the FOLLOW-UP request
			// over content the client never sent. Carry the normalized object
			// and omit the calls that have none - the same rule the
			// non-streaming record path applies, so both retain the same turns.
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
	for _, group := range [][]*pendingToolCall{s.pendingSlice(), s.closedCalls} {
		for _, pending := range group {
			if !pending.started {
				continue
			}
			arguments := pending.complete.String()
			if arguments == "" {
				arguments = "{}"
			}
			callName, callNamespace := s.ctx.ToolNames.clientCallName(pending.name)
			ordered = append(ordered, indexed{
				index: pending.outputIndex,
				item: &ResponsesFunctionCallOutputItem{
					ID:        pending.itemID,
					Type:      "function_call",
					Status:    ResponsesItemCompleted,
					CallID:    pending.callID,
					Name:      callName,
					Arguments: arguments,
					Namespace: callNamespace,
				},
			})
		}
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

// releaseTerminal returns the held terminal batch — the item-closing events
// plus the terminal envelope — and whether this call performed the release.
// The envelope is built at release time so the optional usage tail is
// reflected in its usage. A finish that opened no items
// still releases a one-event batch (just the terminal envelope): the [DONE]
// sentinel must be able to release a zero-output completion.
func (s *chatResponsesStreamState) releaseTerminal() ([]ResponsesSSEEvent, bool) {
	if s.terminalReleased {
		return nil, false
	}
	s.terminalReleased = true
	held := s.heldTerminal
	s.heldTerminal = nil
	return append(held, s.terminalEnvelope()), true
}

// FinalizeEOF releases a terminal held after a finishing chunk when the
// upstream ended its stream without the [DONE] sentinel (some gateways omit
// it): every semantic terminal already arrived, so the completion is real
// and the client can be told so truthfully, with the provider quirk
// recorded as an ungated note. A stream that ends WITHOUT any
// finish_reason is still a typed truncation, never a fabricated success. A
// zero-output finish releases the same way: the terminal was reached.
func (s *chatResponsesStreamState) FinalizeEOF() ([]ResponsesSSEEvent, error) {
	if s.streamError != nil {
		return nil, s.streamError
	}
	events, err := s.finalizeEOF()
	if err != nil {
		s.streamError = err
		return nil, err
	}
	return events, nil
}

// finalizeEOF releases the held terminal at EOF, leaving failure latching to
// FinalizeEOF.
func (s *chatResponsesStreamState) finalizeEOF() ([]ResponsesSSEEvent, error) {
	if s.sawFinish && !s.terminalReleased {
		// The release is the commit; the note follows it, so it describes a
		// completion that was actually released.
		held, _ := s.releaseTerminal()
		if err := s.noteMissingSentinel(); err != nil {
			return nil, err
		}
		return held, nil
	}
	if s.sawFinish {
		// The finish chunk was consumed and the [DONE] sentinel released the
		// terminal; nothing more to emit.
		return nil, nil
	}
	return nil, upstreamWireError(UpstreamChatCompletions, http.StatusOK, errStreamTruncated)
}

// noteMissingSentinel records the missing-[DONE] provider quirk as an
// ungated note: the EOF release is truthful (the finish already arrived),
// and the quirk staying visible in the conversion report is what matters.
func (s *chatResponsesStreamState) noteMissingSentinel() error {
	return s.report.Note(
		FeatureMissingStreamSentinel,
		"chat[].stream",
		"the upstream stream ended after a finishing chunk without the [DONE] sentinel; the completion was released on EOF",
	)
}
