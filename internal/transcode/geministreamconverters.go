package transcode

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/geminigeneratecontent"
)

// geminiToResponsesConverter adapts geminiResponsesStreamState to frameConverter,
// marshaling typed Responses events into frames. Gemini streams are data-only
// (no event name); a non-empty event name or a [DONE] sentinel is handled here.
type geminiToResponsesConverter struct {
	deliveryGate

	state *geminiResponsesStreamState

	// streamError latches the converter's own failure — a frame it could not
	// decode or convert, or a marshal that failed — so a caller that ignores
	// the returned error cannot still obtain a clean terminal from [DONE] or
	// EOF.
	streamError error
}

func newGeminiToResponsesConverter(state *geminiResponsesStreamState) *geminiToResponsesConverter {
	return &geminiToResponsesConverter{state: state}
}

// Convert processes one upstream Gemini SSE frame. The first failure of any
// kind latches: every later call, and FinalizeEOF, propagates it.
func (c *geminiToResponsesConverter) Convert(frame SSEEvent) (convertedBatch, error) {
	if c.streamError != nil {
		return convertedBatch{}, c.streamError
	}
	mark := c.state.beginCall()
	batch, err := c.convertFrame(frame)
	if err == nil {
		batch, err = c.gate(batch)
	}
	if err != nil {
		c.state.rollbackCall(mark)
		c.streamError = err
		return convertedBatch{}, err
	}
	c.state.commitCall()
	return batch, nil
}

// convertFrame converts one frame, leaving failure latching to Convert.
func (c *geminiToResponsesConverter) convertFrame(frame SSEEvent) (convertedBatch, error) {
	if c.state.streamError != nil {
		return convertedBatch{}, c.state.streamError
	}
	if frame.Event != "" {
		return convertedBatch{}, upstreamWireError(
			UpstreamGemini, http.StatusOK,
			errors.New("gemini stream frame carries an SSE event name; Gemini frames are data-only"),
		)
	}
	data := trimJSONSpace(frame.Data)
	if string(data) == "[DONE]" {
		// Google never sends [DONE]. After the terminal it releases the held
		// terminal; before it, the stream is truncated. Deliberately not
		// errStreamTruncated: the sentinel is a protocol violation
		// (upstream-wire class), not an EOF truncation.
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
		if err != nil {
			return convertedBatch{}, err
		}
		batch.Terminal = true
		return batch, nil
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
	if c.streamError != nil {
		return convertedBatch{}, c.streamError
	}
	mark := c.state.beginCall()
	batch, err := c.finalizeEOF()
	if err == nil {
		batch, err = c.gate(batch)
	}
	if err != nil {
		c.state.rollbackCall(mark)
		c.streamError = err
		return convertedBatch{}, err
	}
	c.state.commitCall()
	return batch, nil
}

// finalizeEOF releases the held terminal, leaving failure latching to
// FinalizeEOF.
func (c *geminiToResponsesConverter) finalizeEOF() (convertedBatch, error) {
	events, err := c.state.FinalizeEOF()
	if err != nil {
		return convertedBatch{}, err
	}
	batch, err := marshalResponsesEvents(events)
	if err != nil {
		return convertedBatch{}, err
	}
	batch.Terminal = true
	return batch, nil
}

// ConversionReport returns the accumulated report of the state.
func (c *geminiToResponsesConverter) ConversionReport() *ConversionReport {
	return &c.state.report
}

// ErrorEvent builds a client-dialect Responses error event frame, numbered from
// the delivered events.
func (c *geminiToResponsesConverter) ErrorEvent(err error) (frameEvent, bool) {
	return responsesErrorFrame(err, c.nextDeliveredSequence())
}

// continuityTurns delegates to the state (continuityTurnSource).
func (c *geminiToResponsesConverter) continuityTurns(
	requestTurns []CanonicalTurn, depth int,
) (string, []CanonicalTurn, int, int, bool) {
	if c.streamError != nil {
		return "", nil, 0, 0, false
	}
	return c.state.continuityTurns(requestTurns, depth)
}

// geminiToAnthropicConverter composes the Gemini->Responses and
// Responses->Anthropic state machines strictly in memory, like
// chatToAnthropicConverter.
type geminiToAnthropicConverter struct {
	deliveryGate

	gemini    *geminiResponsesStreamState
	anthropic *anthropicResponsesStreamState

	// streamError latches the converter's own failure — a frame it could not
	// decode, a downstream conversion that failed, or a marshal that failed —
	// so a caller that ignores the returned error cannot still obtain a clean
	// terminal from [DONE] or EOF.
	streamError error
}

func newGeminiToAnthropicConverter(
	gemini *geminiResponsesStreamState,
	anthropic *anthropicResponsesStreamState,
) *geminiToAnthropicConverter {
	return &geminiToAnthropicConverter{gemini: gemini, anthropic: anthropic}
}

// Convert processes one upstream Gemini SSE frame through both state machines.
// The first failure of any kind latches: every later call, and FinalizeEOF,
// propagates it.
func (c *geminiToAnthropicConverter) Convert(frame SSEEvent) (convertedBatch, error) {
	if c.streamError != nil {
		return convertedBatch{}, c.streamError
	}
	geminiMark, anthropicMark := c.gemini.beginCall(), c.anthropic.beginCall()
	batch, err := c.convertFrame(frame)
	if err == nil {
		batch, err = c.gate(batch)
	}
	if err != nil {
		c.gemini.rollbackCall(geminiMark)
		c.anthropic.rollbackCall(anthropicMark)
		c.streamError = err
		return convertedBatch{}, err
	}
	c.gemini.commitCall()
	return batch, nil
}

// convertFrame converts one frame, leaving failure latching to Convert.
func (c *geminiToAnthropicConverter) convertFrame(frame SSEEvent) (convertedBatch, error) {
	if c.gemini.streamError != nil {
		return convertedBatch{}, c.gemini.streamError
	}
	if c.anthropic.streamError != nil {
		return convertedBatch{}, c.anthropic.streamError
	}
	if frame.Event != "" {
		return convertedBatch{}, upstreamWireError(
			UpstreamGemini, http.StatusOK,
			errors.New("gemini stream frame carries an SSE event name; Gemini frames are data-only"),
		)
	}
	data := trimJSONSpace(frame.Data)
	if string(data) == "[DONE]" {
		// Same as above: not errStreamTruncated — a [DONE] before a terminal
		// is a sentinel protocol violation, not an EOF truncation.
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
	if c.streamError != nil {
		return convertedBatch{}, c.streamError
	}
	geminiMark, anthropicMark := c.gemini.beginCall(), c.anthropic.beginCall()
	batch, err := c.finalizeEOF()
	if err == nil {
		batch, err = c.gate(batch)
	}
	if err != nil {
		c.gemini.rollbackCall(geminiMark)
		c.anthropic.rollbackCall(anthropicMark)
		c.streamError = err
		return convertedBatch{}, err
	}
	c.gemini.commitCall()
	return batch, nil
}

// finalizeEOF releases the Gemini terminal through the Anthropic state,
// leaving failure latching to FinalizeEOF. Both composed latches are checked in
// the order convertFrame uses, so a failure fed to either state directly is
// reported as the same instance on every entry point.
func (c *geminiToAnthropicConverter) finalizeEOF() (convertedBatch, error) {
	if c.gemini.streamError != nil {
		return convertedBatch{}, c.gemini.streamError
	}
	if c.anthropic.streamError != nil {
		return convertedBatch{}, c.anthropic.streamError
	}
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
	if c.streamError != nil || c.gemini.streamError != nil || c.anthropic.streamError != nil {
		return "", nil, 0, 0, false
	}
	return c.gemini.continuityTurns(requestTurns, depth)
}
