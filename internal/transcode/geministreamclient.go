package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// geminiStreamClientState converts typed Responses events into Gemini
// GenerateContent stream chunks. Every event passes the Responses lifecycle
// validator and the stream budget before it converts, and no chunk is emitted
// after the terminal chunk. The chunks carry no event name and no comment:
// the Gemini SDK reads data-only frames.
type geminiStreamClientState struct {
	ctx *ExchangeContext
	// upstream is the protocol whose stream produced the events. Every
	// event derives from upstream bytes, so a bound violation here is an
	// upstream fault and is typed as that upstream's wire error.
	upstream   UpstreamProtocol
	policy     LossPolicy
	report     ConversionReport
	responseID string
	model      string

	fsm          *responsesStreamFSM
	budget       streamBudget
	lastSequence int64

	// sentPart records that at least one content part was emitted, so the
	// terminal chunk can supply the empty text part a Gemini stream needs when
	// the upstream produced no content at all.
	sentPart bool
	// thoughtLossNoted and refusalNoted keep the once-per-stream records.
	thoughtLossNoted bool
	refusalNoted     bool
	// phaseLossNoted and reasoningLossNoted keep the once-per-stream records
	// of the output metadata a Gemini stream cannot carry, matching the
	// non-streaming render.
	phaseLossNoted     bool
	reasoningLossNoted bool
	// outputItemBoundariesLossNoted and logprobsLossNoted keep the
	// once-per-stream records of conversation-state output items and text
	// logprobs, neither of which a Gemini stream carries.
	outputItemBoundariesLossNoted bool
	logprobsLossNoted             bool
	// streamedSummary accumulates, per reasoning item id, the summary text
	// that arrived as summary deltas: only that text was rendered as thoughts
	// or recorded as provider reasoning loss, so only an exact content match
	// proves the item's final summary covered. The retained copy is charged
	// against the semantic-bytes budget like any other retained text.
	streamedSummary map[string]*strings.Builder

	sawTerminal bool
	// streamError latches the first failure, including a failure terminal
	// (response.failed, response.error): every later call and FinalizeEOF
	// propagate it, so a failed stream can never report a clean terminal.
	streamError error
}

func newGeminiStreamClientState(
	ctx *ExchangeContext,
	upstream UpstreamProtocol,
	policy LossPolicy,
	responseID string,
	model string,
) *geminiStreamClientState {
	return &geminiStreamClientState{
		ctx:          ctx,
		upstream:     upstream,
		policy:       policy,
		responseID:   responseID,
		model:        model,
		fsm:          newResponsesStreamFSM(),
		budget:       newStreamBudget(),
		lastSequence: -1,
	}
}

// upstreamStreamFailure reports a failure the upstream sent inside a
// successful stream body. The HTTP status was 200, so the failure is an upstream
// body failure: the stream reader must classify the exchange as an upstream
// failure, which it does only for upstream provenance.
func upstreamStreamFailure(message string) error {
	return &StreamConversionError{
		Cause:      &UpstreamSemanticFailureError{Message: message},
		Provenance: ProvenanceUpstreamBodyError,
	}
}

// wireError types a stream failure as a wire error of the upstream whose
// stream produced the events.
func (s *geminiStreamClientState) wireError(err error) error {
	return upstreamWireError(s.upstream, http.StatusOK, err)
}

// Convert processes one typed Responses event into zero or more chunks.
func (s *geminiStreamClientState) Convert(event ResponsesSSEEvent) ([]geminiResponse, error) {
	if s.streamError != nil {
		return nil, s.streamError
	}
	if s.sawTerminal {
		return nil, s.wireError(errors.New("responses stream event after terminal"))
	}
	if err := s.fsm.Validate(event); err != nil {
		return nil, err
	}
	if err := s.budget.addEvent(); err != nil {
		return nil, s.wireError(err)
	}
	if sequence := event.Sequence(); sequence <= s.lastSequence {
		return nil, s.wireError(fmt.Errorf(
			"responses stream sequence number %d is not strictly increasing (last %d)",
			sequence, s.lastSequence,
		))
	} else {
		s.lastSequence = sequence
	}

	switch value := event.(type) {
	case ResponseTextDeltaEvent:
		if len(value.Logprobs) > 0 && !s.logprobsLossNoted {
			s.logprobsLossNoted = true
			if err := s.report.Lose(
				s.policy, FeatureLogprobs, "output_text.delta.logprobs",
				"text logprobs cannot be reproduced in a Gemini stream",
			); err != nil {
				return nil, err
			}
		}
		return s.textChunk(value.Delta, false)
	case ResponseRefusalDeltaEvent:
		if !s.refusalNoted {
			s.refusalNoted = true
			if err := s.report.Note(
				FeatureGeminiRefusalText, "candidates[0].content.parts[].text",
				"the refusal is rendered as ordinary text",
			); err != nil {
				return nil, err
			}
		}
		return s.textChunk(value.Delta, false)
	case ResponseReasoningSummaryTextDeltaEvent:
		if !s.ctx.GeminiIncludeThoughts {
			// The loss decision comes first: a refused event must not leave
			// retained state or budget charges behind.
			if !s.thoughtLossNoted {
				s.thoughtLossNoted = true
				if err := s.report.Lose(
					s.policy, FeatureProviderReasoningText, "candidates[0].content.parts[].thought",
					"provider reasoning text is not returned unless the client requests thoughts",
				); err != nil {
					return nil, err
				}
			}
		}
		if value.Delta != "" {
			// Retain the streamed text for the coverage proof in
			// loseReasoning: the retained copy is semantic state, so it is
			// charged like rendered text (textChunk charges its own copy when
			// thoughts are included — retention and emission are separate).
			if err := s.budget.addSemanticBytes(int64(len(value.Delta))); err != nil {
				return nil, s.wireError(err)
			}
			if s.streamedSummary == nil {
				s.streamedSummary = make(map[string]*strings.Builder)
			}
			buf := s.streamedSummary[value.ItemID]
			if buf == nil {
				if err := s.budget.addStateEntries(1); err != nil {
					return nil, s.wireError(err)
				}
				buf = &strings.Builder{}
				s.streamedSummary[value.ItemID] = buf
			}
			buf.WriteString(value.Delta)
		}
		if !s.ctx.GeminiIncludeThoughts {
			return nil, nil
		}
		return s.textChunk(value.Delta, true)
	case ResponseOutputItemDoneEvent:
		switch item := value.Item.(type) {
		case *ResponsesFunctionCallOutputItem:
			return s.functionCallChunk(item)
		case *ResponsesOutputMessage:
			return nil, s.losePhase(item)
		case *ResponsesReasoningOutputItem:
			return nil, s.loseReasoning(item)
		case *ResponsesFunctionCallOutputResultItem:
			// A tool result echoed in the upstream output is conversation
			// state, not model output; the non-streaming render records it
			// under the same policy-gated loss rather than dropping it.
			if s.outputItemBoundariesLossNoted {
				return nil, nil
			}
			s.outputItemBoundariesLossNoted = true
			return nil, s.report.Lose(
				s.policy, FeatureOutputItemBoundaries, "output[].function_call_output",
				"tool results are conversation-state output items, not part of the model response",
			)
		}
		return nil, nil
	case ResponseCompletedEvent:
		return s.terminalChunk(value.Response, "completed")
	case ResponseIncompleteEvent:
		return s.terminalChunk(value.Response, "incomplete")
	case ResponseFailedEvent:
		s.sawTerminal = true
		s.streamError = upstreamStreamFailure("the upstream response failed")
		return nil, s.streamError
	case ResponseErrorEvent:
		s.sawTerminal = true
		s.streamError = upstreamStreamFailure(value.Message)
		return nil, s.streamError
	default:
		// Lifecycle events with no client-visible content (created, in
		// progress, part and item boundaries) produce no chunk.
		return nil, nil
	}
}

// losePhase records the approved loss of an output message phase, once per
// stream: a Gemini response has no phase.
func (s *geminiStreamClientState) losePhase(message *ResponsesOutputMessage) error {
	if message.Phase == "" || s.phaseLossNoted {
		return nil
	}
	s.phaseLossNoted = true
	return s.report.Lose(
		s.policy, FeatureOutputPhase, "output[].phase",
		"the output message phase cannot be reproduced in a Gemini stream",
	)
}

// loseReasoning records the approved loss of reasoning output the stream did
// not render, once per stream. Summary text that arrived as summary deltas
// was rendered as thoughts or recorded as provider reasoning loss on its own;
// only a final summary whose text exactly matches the accumulated streamed
// text is proven covered — a byte-count match is not a content match.
// Whatever the item carries beyond that (summary text that never streamed,
// encrypted content, reasoning text content, or an item with no summary text
// at all, as the non-streaming render treats it) is lost here.
func (s *geminiStreamClientState) loseReasoning(item *ResponsesReasoningOutputItem) error {
	if s.reasoningLossNoted {
		return nil
	}
	var summaryText strings.Builder
	for _, summary := range item.Summary {
		summaryText.WriteString(summary.Text)
	}
	summarized := summaryText.Len() > 0 && s.streamedSummary[item.ID] != nil &&
		s.streamedSummary[item.ID].String() == summaryText.String()
	if summarized && item.EncryptedContent == "" && len(item.Content) == 0 {
		return nil
	}
	s.reasoningLossNoted = true
	return s.report.Lose(
		s.policy, FeatureReasoningSummary, "output[].reasoning",
		"Responses reasoning output cannot be reproduced in a Gemini stream",
	)
}

// textChunk renders one text delta. An empty delta carries nothing and is
// dropped rather than sent as an empty part.
func (s *geminiStreamClientState) textChunk(delta string, thought bool) ([]geminiResponse, error) {
	if delta == "" {
		return nil, nil
	}
	if err := s.budget.addSemanticBytes(int64(len(delta))); err != nil {
		return nil, s.wireError(err)
	}
	text := delta
	part := geminiPart{Text: &text}
	if thought {
		flag := true
		part.Thought = &flag
	}
	s.sentPart = true
	return []geminiResponse{s.chunk([]geminiPart{part}, "", nil)}, nil
}

// functionCallChunk renders one completed function call. Its arguments must
// be a JSON object, because Gemini functionCall.args is an object.
func (s *geminiStreamClientState) functionCallChunk(item *ResponsesFunctionCallOutputItem) ([]geminiResponse, error) {
	if err := s.budget.addToolCall(); err != nil {
		return nil, s.wireError(err)
	}
	args := ParseToolArguments(item.Arguments)
	if !args.IsObject {
		return nil, &UnrepresentableError{
			Protocol: "gemini",
			Path:     "candidates[0].content.parts[].functionCall.args",
			Detail:   "the model-generated tool arguments are not a JSON object and cannot be represented as functionCall.args",
		}
	}
	call := &geminiFunctionCall{
		ID:   item.CallID,
		Name: item.Name,
		Args: json.RawMessage(args.Raw),
	}
	s.sentPart = true
	return []geminiResponse{s.chunk([]geminiPart{{FunctionCall: call}}, "", nil)}, nil
}

// terminalChunk renders the final chunk: the finish reason and usage, plus the
// empty text part when no content was ever sent. The stream is finished after
// it.
func (s *geminiStreamClientState) terminalChunk(response ResponseEnvelope, status string) ([]geminiResponse, error) {
	stop := CanonicalStopEndTurn
	if status == "incomplete" {
		// An incomplete response must map to a truthful finish reason: a
		// missing or unrecognized incomplete_details.reason must never fall
		// through to STOP, which would report the incomplete response as a
		// normal completion.
		if response.IncompleteDetails == nil || response.IncompleteDetails.Reason == "" {
			return nil, s.wireError(errors.New(
				"responses incomplete event carries no incomplete_details.reason",
			))
		}
		switch response.IncompleteDetails.Reason {
		case "max_output_tokens":
			stop = CanonicalStopMaxTokens
		case "content_filter":
			stop = CanonicalStopRefusal
		default:
			return nil, &UnrepresentableError{
				Protocol: string(s.upstream),
				Path:     "incomplete_details.reason",
				Detail: fmt.Sprintf(
					"the upstream ended the response incomplete with reason %q, which has no Gemini finishReason mapping",
					response.IncompleteDetails.Reason,
				),
			}
		}
	}
	canonical := CanonicalResponse{
		Stop:  CanonicalStop{Reason: stop},
		Usage: responsesEnvelopeUsage(response.Usage),
	}
	finish, err := geminiFinishReason(canonical, &s.report)
	if err != nil {
		return nil, err
	}
	usage, err := geminiResponseUsage(canonical, s.ctx, &s.report)
	if err != nil {
		return nil, err
	}
	var parts []geminiPart
	if !s.sentPart {
		empty := ""
		parts = []geminiPart{{Text: &empty}}
		if err := s.report.Note(
			FeatureGeminiEmptyContent, "candidates[0].content.parts",
			"the response had no content; one empty text part keeps SDK text accessors working",
		); err != nil {
			return nil, err
		}
	}
	s.sawTerminal = true
	return []geminiResponse{s.chunk(parts, finish, usage)}, nil
}

// chunk assembles one stream chunk around the response identity.
func (s *geminiStreamClientState) chunk(parts []geminiPart, finish string, usage *geminiUsage) geminiResponse {
	return geminiResponse{
		Candidates: []geminiCandidate{{
			Content:      geminiContent{Role: "model", Parts: parts},
			FinishReason: finish,
			Index:        0,
		}},
		UsageMetadata: usage,
		ModelVersion:  s.model,
		ResponseID:    s.responseID,
	}
}

// responsesEnvelopeUsage reads the Responses terminal envelope usage into the
// canonical form. An absent usage object is entirely unknown.
func responsesEnvelopeUsage(usage *ResponsesUsage) CanonicalUsage {
	if usage == nil {
		return CanonicalUsage{}
	}
	out := CanonicalUsage{
		InputTokens:    usage.InputTokens,
		OutputTokens:   usage.OutputTokens,
		TotalTokens:    usage.TotalTokens,
		InputKnown:     true,
		OutputKnown:    true,
		TotalKnown:     true,
		CacheReadKnown: usage.InputTokensDetails != nil,
		ReasoningKnown: usage.OutputTokensDetails != nil,
	}
	if usage.InputTokensDetails != nil {
		out.CacheReadTokens = usage.InputTokensDetails.CachedTokens
	}
	if usage.OutputTokensDetails != nil {
		out.ReasoningTokens = usage.OutputTokensDetails.ReasoningTokens
	}
	return out
}

// marshalGeminiChunks marshals chunks into data-only frames under the same
// frame bound the other dialects apply.
func marshalGeminiChunks(chunks []geminiResponse) (convertedBatch, error) {
	batch := convertedBatch{}
	for _, chunk := range chunks {
		data, err := json.Marshal(chunk)
		if err != nil {
			return convertedBatch{}, err
		}
		if len(data) > maxGeneratedSSEFrameBytes {
			return convertedBatch{}, &SSEBoundError{Bound: maxGeneratedSSEFrameBytes}
		}
		batch.Events = append(batch.Events, frameEvent{Data: data})
	}
	return batch, nil
}

// geminiStreamErrorFrame renders a stream failure as the Gemini error frame.
// The failure is always reported as an upstream failure of the stream.
func geminiStreamErrorFrame(err error) (frameEvent, bool) {
	apiErr := normalizeCanonicalError(CanonicalAPIError{
		Status:  http.StatusBadGateway,
		Message: boundedErrorMessage(err),
	})
	payload, marshalErr := json.Marshal(geminiClientError(apiErr))
	if marshalErr != nil {
		return frameEvent{}, false
	}
	return frameEvent{Data: payload}, true
}

// chatToGeminiConverter composes the Chat-to-Responses state with the
// Responses-to-Gemini state strictly in memory, like chatToAnthropicConverter.
type chatToGeminiConverter struct {
	chat   *chatResponsesStreamState
	gemini *geminiStreamClientState
}

func newChatToGeminiConverter(
	chat *chatResponsesStreamState,
	gemini *geminiStreamClientState,
) *chatToGeminiConverter {
	return &chatToGeminiConverter{chat: chat, gemini: gemini}
}

// Convert processes one upstream Chat SSE frame through both state machines.
func (c *chatToGeminiConverter) Convert(frame SSEEvent) (convertedBatch, error) {
	if c.gemini.streamError != nil {
		return convertedBatch{}, c.gemini.streamError
	}
	chunk, err := chatStreamChunkFromSSE(frame)
	if errors.Is(err, errChatStreamDone) {
		if !c.chat.sawFinish {
			return convertedBatch{}, errChatDoneBeforeTerminal()
		}
		batch, releaseErr := c.releaseTerminals()
		if releaseErr != nil {
			return convertedBatch{}, releaseErr
		}
		if batch.Terminal || len(batch.Events) > 0 {
			return batch, nil
		}
		return convertedBatch{}, errChatDoneBeforeTerminal()
	}
	if err != nil {
		return convertedBatch{}, err
	}
	responsesEvents, err := c.chat.Convert(chunk)
	if err != nil {
		return convertedBatch{}, err
	}
	return c.convertResponsesEvents(responsesEvents)
}

func (c *chatToGeminiConverter) convertResponsesEvents(events []ResponsesSSEEvent) (convertedBatch, error) {
	var chunks []geminiResponse
	for _, event := range events {
		converted, err := c.gemini.Convert(event)
		if err != nil {
			return convertedBatch{}, err
		}
		chunks = append(chunks, converted...)
	}
	batch, err := marshalGeminiChunks(chunks)
	if err != nil {
		return convertedBatch{}, err
	}
	batch.Terminal = c.gemini.sawTerminal
	return batch, nil
}

func (c *chatToGeminiConverter) releaseTerminals() (convertedBatch, error) {
	var batch convertedBatch
	if held, ok := c.chat.releaseTerminal(); ok {
		converted, err := c.convertResponsesEvents(held)
		if err != nil {
			return convertedBatch{}, err
		}
		batch.Events = append(batch.Events, converted.Events...)
		batch.Terminal = converted.Terminal
	}
	return batch, nil
}

// FinalizeEOF releases the held Chat terminal when the upstream ended after a
// finishing chunk without the [DONE] sentinel. A stream that ends without any
// finish reason is a truncation, never a released terminal.
func (c *chatToGeminiConverter) FinalizeEOF() (convertedBatch, error) {
	if c.gemini.streamError != nil {
		return convertedBatch{}, c.gemini.streamError
	}
	if c.chat.sawFinish && !c.chat.terminalReleased {
		if err := c.chat.noteMissingSentinel(); err != nil {
			return convertedBatch{}, err
		}
		batch, err := c.releaseTerminals()
		if err != nil {
			return convertedBatch{}, err
		}
		batch.Terminal = true
		return batch, nil
	}
	if c.chat.sawFinish || c.gemini.sawTerminal {
		return convertedBatch{Terminal: true}, nil
	}
	return convertedBatch{}, upstreamWireError(
		UpstreamChatCompletions,
		http.StatusOK,
		errors.New("chat stream ended before a terminal condition"),
	)
}

// ErrorEvent renders a stream failure as the Gemini error frame.
func (c *chatToGeminiConverter) ErrorEvent(err error) (frameEvent, bool) {
	return geminiStreamErrorFrame(err)
}

// ConversionReport merges the losses of both states, keeping the first entry
// per feature and path, as the composed Messages converter does.
func (c *chatToGeminiConverter) ConversionReport() *ConversionReport {
	return mergeConversionReports(&c.chat.report, &c.gemini.report)
}

// responsesToGeminiConverter decodes upstream Responses SSE and feeds the typed
// events to the Gemini state, like responsesToAnthropicConverter.
type responsesToGeminiConverter struct {
	state *geminiStreamClientState

	// missingEventNameNoted gates the missing-event-name note to once per
	// stream.
	missingEventNameNoted bool
}

func newResponsesToGeminiConverter(state *geminiStreamClientState) *responsesToGeminiConverter {
	return &responsesToGeminiConverter{state: state}
}

// Convert processes one upstream Responses SSE frame.
func (c *responsesToGeminiConverter) Convert(frame SSEEvent) (convertedBatch, error) {
	if c.state.streamError != nil {
		return convertedBatch{}, c.state.streamError
	}
	data := trimJSONSpace(frame.Data)
	if isResponsesDoneSentinel(data) {
		if c.state.sawTerminal {
			return convertedBatch{Terminal: true}, nil
		}
		return convertedBatch{}, upstreamWireError(
			UpstreamResponses,
			http.StatusOK,
			errors.New("responses stream [DONE] before a terminal condition"),
		)
	}
	event, err := decodeResponsesSSEEvent(data)
	if err != nil {
		return convertedBatch{}, err
	}
	if frame.Event == "" {
		if !c.missingEventNameNoted {
			c.missingEventNameNoted = true
			if err := c.state.report.Note(
				FeatureMissingEventName,
				"responses[].stream",
				"the upstream stream omitted the SSE event: name; the event was routed by its JSON type",
			); err != nil {
				return convertedBatch{}, err
			}
		}
	} else if err := validateEventNameMatchesJSONType(frame); err != nil {
		return convertedBatch{}, upstreamWireError(UpstreamResponses, http.StatusOK, err)
	}
	chunks, err := c.state.Convert(event)
	if err != nil {
		return convertedBatch{}, err
	}
	batch, err := marshalGeminiChunks(chunks)
	if err != nil {
		return convertedBatch{}, err
	}
	batch.Terminal = c.state.sawTerminal
	return batch, nil
}

// FinalizeEOF reports a truncation when the upstream ended before a terminal
// condition; a stream that reached its terminal has nothing left to release.
func (c *responsesToGeminiConverter) FinalizeEOF() (convertedBatch, error) {
	if c.state.streamError != nil {
		return convertedBatch{}, c.state.streamError
	}
	if !c.state.sawTerminal {
		return convertedBatch{}, upstreamWireError(
			UpstreamResponses,
			http.StatusOK,
			errors.New("responses stream ended before a terminal condition"),
		)
	}
	return convertedBatch{Terminal: true}, nil
}

// ErrorEvent renders a stream failure as the Gemini error frame.
func (c *responsesToGeminiConverter) ErrorEvent(err error) (frameEvent, bool) {
	return geminiStreamErrorFrame(err)
}

// ConversionReport returns the losses of the Gemini state.
func (c *responsesToGeminiConverter) ConversionReport() *ConversionReport {
	return &c.state.report
}

// mergeConversionReports combines two state reports. Entries are kept once per
// feature and path, in order, and the dropped counts are summed so a saturated
// report stays visible.
func mergeConversionReports(a, b *ConversionReport) *ConversionReport {
	merged := ConversionReport{Dropped: a.Dropped + b.Dropped}
	seen := make(map[string]struct{}, len(a.Losses)+len(b.Losses))
	for _, losses := range [][]ConversionLoss{a.Losses, b.Losses} {
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
