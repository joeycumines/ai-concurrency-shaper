package transcode

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// chargeIdentity adds n bytes of tool-call identity (call id, function name)
// to the exchange accumulated total, mirroring the chat direction's charge:
// the identity renders into the generated tool_use block start and the
// terminal reconciliation.
func (s *anthropicResponsesStreamState) chargeIdentity(n int) error {
	if err := chargeStreamIdentity(&s.totalAccumulated, n, "responses"); err != nil {
		return s.wireError(err)
	}
	return nil
}

// wireError marks a conversion error as corrupt upstream Responses wire
// data: the exchange classifies as an upstream failure, never a local
// conversion failure.
func (s *anthropicResponsesStreamState) wireError(err error) error {
	return upstreamWireError(UpstreamResponses, http.StatusOK, err)
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
	// stream. -1 accepts any nonnegative first
	// sequence.
	lastSequence int64

	// Upstream response identity, pinned from the response.created envelope
	// and verified on every later envelope-bearing event: id, model, and
	// created_at must be stable across the stream.
	// created_at is float64 end-to-end.
	upstreamID        string
	upstreamModel     string
	upstreamCreatedAt float64

	// addedItems records every output item observed via output_item.added:
	// its output index and type. Duplicate identities, content parts for
	// unknown items, and output-index mismatches are wire errors.
	addedItems map[string]anthropicAddedItem

	// doneItems records every output item closed by output_item.done: a
	// duplicate done for any item type is corrupt upstream wire.
	doneItems map[string]struct{}

	// partsSeen records every content part observed via content_part.added
	// (keyed by owning item and content index) with its wire part type.
	// Duplicate parts and type-mismatched content_part.done are wire errors.
	partsSeen map[responsePartKey]string

	// partCounts tracks the number of parts observed per item so the
	// per-item part bound and the done/envelope count checks are O(1)
	// instead of scanning partsSeen.
	partCounts map[string]int

	// textBufs and refusalBufs accumulate streamed text and refusal per
	// content part so the done events and the terminal envelope reconcile
	// against the incrementally observed content.
	textBufs    map[responsePartKey]*strings.Builder
	refusalBufs map[responsePartKey]*strings.Builder

	// thinkingBufs accumulates streamed thinking text per open thinking
	// block, bounded per block and per exchange by checkAccumulated exactly
	// like text and refusal.
	thinkingBufs map[responsePartKey]*strings.Builder

	// tool blocks buffered until call identity is complete.
	pendingToolStart map[string]*pendingToolBlock // keyed by item_id

	// deferredTools is the FIFO of reserved tool blocks that could not start
	// because another content block was open, in block-index order. They are
	// drained one at a time as each open block closes, so exactly one
	// tool_use block is open at any moment.
	deferredTools []*pendingToolBlock
	// bufferedToolFragments holds, per deferred item id, the argument
	// fragments received while the block was deferred, replayed in arrival
	// order when the block starts.
	bufferedToolFragments map[string][]string

	// closedToolCalls records every function call closed by output_item.done:
	// the terminal envelope's function items must reconcile against it.
	closedToolCalls map[string]anthropicClosedToolCall

	// partBlocks maps the Responses content part — keyed by owning item and
	// content index — to the Anthropic block index it opened. The composed chat->anthropic
	// direction keeps text and refusal parts open simultaneously, so deltas
	// must target their own block, never the lowest open one.
	partBlocks map[responsePartKey]int64

	responseID string
	model      string
	createdAt  float64

	// budget is the seven-dimension total exchange budget: every event and every state allocation charges it BEFORE the
	// mutation.
	budget streamBudget

	// fsm is the single lifecycle validator every event passes before
	// interpretation.
	fsm *responsesStreamFSM

	sawTerminal   bool
	sawErrorEvent bool
	sawToolUse    bool

	// reasoningLossRecorded ensures the reasoning loss is recorded exactly
	// once per stream.
	reasoningLossRecorded bool

	// usageComponentsLossRecorded gates the required-usage-component loss:
	// the Messages wire requires breakdown fields the Responses source never
	// provides, and the decision is recorded exactly once per stream.
	usageComponentsLossRecorded bool

	// usageClampNotes gates the usage-clamp notes (usage_cache_exceeds_input,
	// usage_negative_counts) to once per stream per key.
	usageClampNotes usageClampNotes

	// totalAccumulated bounds the exchange-wide sum of accumulated semantic
	// bytes (text, refusal, tool arguments).
	totalAccumulated int64

	// capabilities gates the native thinking rendering
	// (provider_reasoning_thinking): when enabled, reasoning-summary events
	// render as thinking blocks carrying the marker signature.
	capabilities ChatCapabilities

	// reasoningBlockIndex is the content-block index of the OPEN thinking
	// block (nil when none is open).
	reasoningBlockIndex *int

	// phaseGated tracks the output items whose phase already entered the
	// loss decision, so a phase-bearing item seen both in output_item.added
	// and in the terminal envelope is gated exactly once.
	phaseGated map[string]struct{}

	// controlsGated ensures the envelope-controls loss is recorded exactly
	// once per stream.
	controlsGated bool

	usage *AnthropicUsage

	// report accumulates approved losses of the conversion; it is surfaced
	// through the frame converter so the handler can log them.
	report ConversionReport

	// Accumulated content blocks for the final message envelope.
	message AnthropicMessageResponse

	// streamError latches the first failure Convert returned, so a caller
	// that ignores it can never report the terminal as clean afterwards.
	streamError error
}

// responsePartKey identifies a Responses content part by its owning output
// item and content index. Content indices are scoped to an output item, so
// the item id is part of the identity.
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

	// deferred is set while the reserved block waits in deferredTools because
	// another content block was open when its identity completed.
	deferred bool
	// done is set when the item's done arrived while the block was deferred:
	// its stop (and the reconciled done-suffix) is emitted by the drain.
	done bool
	// doneSuffix holds reconciled snapshot bytes to emit as one
	// input_json_delta at drain, after the buffered fragments.
	doneSuffix string
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
	capabilities ChatCapabilities,
	responseID string,
	model string,
	createdAt float64,
) *anthropicResponsesStreamState {
	return &anthropicResponsesStreamState{
		ctx:                   ctx,
		policy:                policy,
		capabilities:          capabilities,
		responseID:            responseID,
		model:                 model,
		budget:                newStreamBudget(),
		fsm:                   newResponsesStreamFSM(),
		createdAt:             createdAt,
		lastSequence:          -1,
		addedItems:            make(map[string]anthropicAddedItem),
		doneItems:             make(map[string]struct{}),
		partsSeen:             make(map[responsePartKey]string),
		partCounts:            make(map[string]int),
		textBufs:              make(map[responsePartKey]*strings.Builder),
		refusalBufs:           make(map[responsePartKey]*strings.Builder),
		thinkingBufs:          make(map[responsePartKey]*strings.Builder),
		pendingToolStart:      make(map[string]*pendingToolBlock),
		bufferedToolFragments: make(map[string][]string),
		closedToolCalls:       make(map[string]anthropicClosedToolCall),
		partBlocks:            make(map[responsePartKey]int64),
		phaseGated:            make(map[string]struct{}),
	}
}

// Convert processes one typed Responses event into Anthropic events. The
// first failure of any kind latches: every later call, and FinalizeEOF,
// propagates it, so a stream that failed can never report a clean terminal.
func (s *anthropicResponsesStreamState) Convert(
	event ResponsesSSEEvent,
) ([]AnthropicStreamEvent, error) {
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
func (s *anthropicResponsesStreamState) convertEvent(
	event ResponsesSSEEvent,
) ([]AnthropicStreamEvent, error) {
	if s.sawTerminal {
		return nil, errors.New("responses stream event after terminal")
	}
	// Every event passes the single lifecycle validator FIRST; the render-side checks below remain as defense.
	if err := s.fsm.Validate(event); err != nil {
		return nil, err
	}
	if err := s.budget.addEvent(); err != nil {
		return nil, s.wireError(err)
	}
	// The wire requires strictly increasing, unique sequence numbers across
	// the stream.
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
	// before the created envelope is a corrupt lifecycle. The stream error event is the one exception: it is an abort
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
		// With the provider_reasoning_thinking capability, reasoning
		// summary events render as NATIVE Anthropic thinking blocks
		// carrying the proxy's marker signature (operator-adjudicated
		// design, 2026-09-07); the request path scrubs marker-signature
		// blocks from replayed history, so the synthetic signature never
		// reaches an upstream. Without the capability the events are
		// dropped with the documented loss.
		if !s.capabilities.ProviderReasoningThinking {
			return nil, s.loseReasoningOnce()
		}
		switch value := value.(type) {
		case ResponseReasoningSummaryPartAddedEvent:
			return s.reasoningPartAdded(value)
		case ResponseReasoningSummaryTextDeltaEvent:
			return s.reasoningTextDelta(value)
		case ResponseReasoningSummaryTextDoneEvent:
			return nil, nil
		case ResponseReasoningSummaryPartDoneEvent:
			return s.reasoningPartDone(value)
		}
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

// loseUnknownUsageComponentsOnce records the usage-timing loss exactly once
// per stream when a wire-required Messages usage component is unknown on the
// source: cache_read_input_tokens / thinking_tokens are unknown when their
// detail objects are absent, and cache_creation_input_tokens is unknown
// unless the composed Chat source carried the created_cache_tokens provider
// extension through the in-memory usage carrier (it is never part of the
// pinned Responses wire contract). Zeros are never emitted silently
func (s *anthropicResponsesStreamState) loseUnknownUsageComponentsOnce(usage *ResponsesUsage) error {
	if s.usageComponentsLossRecorded || usage == nil {
		return nil
	}
	// The once-per-stream gate is set only after every component decision
	// below commits (decide before commit, record after commit): a refused
	// component must not latch the gate and silence the remaining components.
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
	s.usageComponentsLossRecorded = true
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
	// created_at.
	s.upstreamID = envelope.ID
	s.upstreamModel = envelope.Model
	s.upstreamCreatedAt = envelope.CreatedAt
	if err := s.loseControlsOnce(envelope); err != nil {
		return nil, err
	}
	// The stream-start message serializes stop_reason: null and
	// stop_sequence: null: generation has not finished,
	// and the stop fields are assigned only in message_delta.
	s.message = AnthropicMessageResponse{
		ID:      s.responseID,
		Type:    "message",
		Role:    "assistant",
		Model:   s.model,
		Content: []AnthropicContentBlock{},
	}
	usage, clamp, err := responsesUsageToAnthropicUsage(envelope.Usage, responsesUsagePresence(envelope.Usage))
	if err != nil {
		// The only remaining failure is a count too large for this
		// platform's int while rendering the target dialect: a local
		// rendering impossibility, never a failed exchange on the
		// source's own arithmetic (that is clamped and noted).
		return nil, err
	}
	if usage != nil {
		if err := s.usageClampNotes.note(&s.report, "usage", clamp); err != nil {
			return nil, err
		}
		// The required Messages breakdown components the source did not
		// provide enter the loss decision before the zeros are emitted.
		if err := s.loseUnknownUsageComponentsOnce(envelope.Usage); err != nil {
			return nil, err
		}
		s.message.Usage = usage
		s.usage = usage
	} else {
		// The created envelope cannot provide usage yet (Responses streams
		// deliver it at completion). The Messages contract requires usage on
		// message_start; emitting zeros would fabricate facts, so the early
		// usage is an explicit loss/reject decision.
		if err := s.report.Lose(
			s.policy,
			FeatureUsageUnknown,
			"message_start.usage",
			"the source cannot provide early token usage; the required message_start usage is a known loss",
		); err != nil {
			return nil, err
		}
		// The Messages wire requires output_tokens_details on the usage
		// object: the zeros are emitted only after the approved loss above.
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
// wire.
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
