package transcode

import (
	"errors"
	"fmt"
)

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
// content_block_stop — and the envelope's output
// items must match the incrementally observed output.
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
	// item left open at the terminal is corrupt upstream wire.
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
// closed done snapshots. Observed items MAY be absent
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
		case *ResponsesReasoningOutputItem:
			// The thinking-rendering reasoning item: the summary text was
			// streamed as thinking deltas (provider_reasoning_thinking);
			// identity and type already reconciled above, and the done
			// event marked it observed.
			if _, done := s.doneItems[itemID]; !done {
				return s.wireError(fmt.Errorf(
					"terminal envelope reasoning item %q was never closed by output_item.done",
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
				// output, never corrupt upstream wire.
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
// blocks, so nothing is synthesized here.
func (s *anthropicResponsesStreamState) terminalEvents(
	stop CanonicalStopReason,
) ([]AnthropicStreamEvent, error) {
	return []AnthropicStreamEvent{
		{
			Type: AnthropicStreamEventTypeMessageDelta,
			Delta: &AnthropicStreamDelta{
				StopReason: new(stopReasonToAnthropic(stop)),
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
	// failed envelope before the created envelope is a corrupt lifecycle.
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
	s.message.StopReason = new(stopReasonToAnthropic(stop))
	converted, clamp, err := responsesUsageToAnthropicUsage(usage, responsesUsagePresence(usage))
	if err != nil {
		// The only remaining failure is a count too large for this
		// platform's int while rendering the target dialect: a local
		// rendering impossibility, never a failed exchange on the
		// source's own arithmetic (that is clamped and noted).
		return err
	}
	if converted != nil {
		if err := s.usageClampNotes.note(&s.report, "usage", clamp); err != nil {
			return err
		}
		// The required Messages breakdown components the source did not
		// provide enter the loss decision before the zeros are emitted;
		// gated once per stream.
		if err := s.loseUnknownUsageComponentsOnce(usage); err != nil {
			return err
		}
		s.message.Usage = converted
		s.usage = converted
	}
	if s.usage == nil {
		// message_delta.usage is required on the wire. The terminal envelope
		// provided no usage: zeros would fabricate facts, so the omission is
		// an explicit loss/reject decision.
		if err := s.report.Lose(
			s.policy,
			FeatureUsageUnknown,
			"usage",
			"the upstream response did not provide terminal token usage; the required Messages usage cannot be reproduced",
		); err != nil {
			return err
		}
		// The Messages wire requires output_tokens_details on the usage
		// object: the zeros are emitted only after the approved loss above.
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

// finalizeEOF reports the truncation when no terminal was emitted, leaving
// failure latching to FinalizeEOF.
func (s *anthropicResponsesStreamState) finalizeEOF() ([]AnthropicStreamEvent, error) {
	if s.sawTerminal {
		// The terminal was emitted at response.completed (or as an error
		// event); nothing more to emit.
		return nil, nil
	}
	return nil, errors.New(
		"responses stream ended before a terminal condition",
	)
}

// responsesUsagePresence reports which counts a Responses usage actually
// provided, so the clamp's details never present a defaulted zero as a source
// fact. The pinned Responses contract requires all three totals, and the
// non-streaming decode relies on the same assumption (DecodeResponsesResponse
// marks them known whenever usage is present); the breakdown presence is exact
// (the detail objects are optional pointers).
func responsesUsagePresence(usage *ResponsesUsage) usagePresence {
	if usage == nil {
		return usagePresence{}
	}
	return usagePresence{
		input:      true,
		output:     true,
		total:      true,
		cacheRead:  usage.InputTokensDetails != nil,
		cacheWrite: usage.CreatedCacheTokens != nil,
		reasoning:  usage.OutputTokensDetails != nil,
	}
}

// responsesUsageToAnthropicUsage converts a Responses usage into the
// Anthropic form with the pinned semantics: input_tokens +
// cache_creation_input_tokens + cache_read_input_tokens = total, so the
// uncached input is the total minus the cached breakdown with checked
// nonnegative arithmetic. The in-memory
// created_cache_tokens carrier (never a wire field; set by the composed Chat
// source) supplies the cache-creation component when present, matching the
// non-streaming decode. A nil source usage returns (nil, nil): the caller
// decides the required-wire-usage loss instead of fabricating zeros.
// presence is the caller's account of which counts the source provided (see
// responsesUsagePresence).
func responsesUsageToAnthropicUsage(usage *ResponsesUsage, presence usagePresence) (*AnthropicUsage, usageClamp, error) {
	if usage == nil {
		return nil, usageClamp{}, nil
	}
	// The upstream usage is a subject-to-change provider value: an
	// arithmetically inconsistent source (a cached breakdown exceeding the
	// input total, negative counts) is CLAMPED into the Messages invariants
	// and recorded by the caller as an ungated note naming the source
	// numbers, instead of failing the exchange (the pre-fix failure was a
	// 502 reading "source usage is arithmetically inconsistent").
	clamp := clampResponsesUsage(usage, presence)
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
	// A total that is not the exact sum of input + output is an
	// observability fact, not a rejection: the emitted values are relayed and
	// the clamp carries the mismatch detail (naming the source numbers where
	// a clamp correction changed them); the callers owning the report record
	// it.
	// Checked, architecture-independent int64-to-int conversion before
	// rendering Messages usage: a count that cannot be represented on this
	// platform (32-bit builds) is a typed error, never a silent overflow.
	uncached, err := checkedInt64ToInt(usage.InputTokens - cached - cacheWrite)
	if err != nil {
		return nil, clamp, &UsageArithmeticError{Detail: "input tokens: " + err.Error()}
	}
	readCached, err := checkedInt64ToInt(cached)
	if err != nil {
		return nil, clamp, &UsageArithmeticError{Detail: "cached tokens: " + err.Error()}
	}
	created, err := checkedInt64ToInt(cacheWrite)
	if err != nil {
		return nil, clamp, &UsageArithmeticError{Detail: "cache-creation tokens: " + err.Error()}
	}
	output, err := checkedInt64ToInt(usage.OutputTokens)
	if err != nil {
		return nil, clamp, &UsageArithmeticError{Detail: "output tokens: " + err.Error()}
	}
	thinking, err := checkedInt64ToInt(reasoning)
	if err != nil {
		return nil, clamp, &UsageArithmeticError{Detail: "reasoning tokens: " + err.Error()}
	}
	return &AnthropicUsage{
		InputTokens:              uncached,
		CacheCreationInputTokens: created,
		CacheReadInputTokens:     readCached,
		OutputTokens:             output,
		OutputTokensDetails: &AnthropicOutputTokensDetails{
			ThinkingTokens: thinking,
		},
	}, clamp, nil
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
