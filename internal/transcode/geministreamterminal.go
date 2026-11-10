package transcode

import (
	"net/http"
)

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
// ends before any finish reason, or that failed at any point, is never a
// success.
func (s *geminiResponsesStreamState) FinalizeEOF() ([]ResponsesSSEEvent, error) {
	if s.streamError != nil {
		// The stream failed earlier: propagate the failure; a failed exchange
		// is never a successful completion.
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
func (s *geminiResponsesStreamState) finalizeEOF() ([]ResponsesSSEEvent, error) {
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
	if s.streamError != nil || !s.terminalDone {
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
				Namespace: value.Namespace,
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
