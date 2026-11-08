package transcode

import (
	"fmt"
	"net/http"
	"strings"
)

// Stream converter contracts:
//
// https://platform.claude.com/docs/en/build-with-claude/streaming
// https://platform.openai.com/docs/api-reference/responses-streaming
// https://github.com/openai/openai-go/blob/main/responses/response.go

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
// empty) and no name. Provider reasoning deltas are
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
	// error.
	chunkID    string
	chunkModel string

	// finishReason is recorded when the finish chunk arrives; the terminal
	// envelope is built lazily at release so the optional usage tail is
	// reflected in its usage.
	finishReason string

	items        []openResponsesItem
	itemIndex    int64
	pendingCalls map[int]*pendingToolCall // keyed by chat tool fragment index
	// closedCalls holds tool calls sealed inline at a transition where the
	// next content item had to open. They are already closed on the wire,
	// so finish must not close them again, but the terminal envelope still
	// carries them.
	closedCalls []*pendingToolCall

	// textBufs and refusalBufs accumulate streamed text and refusal per
	// content part in strings.Builder, avoiding quadratic re-copying of the
	// whole accumulated string on every delta. The
	// final strings are materialized into the message parts once at
	// finish(); the parts stay empty while the stream is in progress, so
	// the emitted deltas remain the only per-chunk representation.
	textBufs    map[chatPartKey]*strings.Builder
	refusalBufs map[chatPartKey]*strings.Builder

	// serviceTierLossRecorded and logprobsLossRecorded gate the per-chunk
	// envelope losses to exactly once per stream.
	serviceTierLossRecorded bool
	logprobsLossRecorded    bool

	// reasoningReportRecorded gates the provider-reasoning report entry
	// (loss or note) to exactly once per stream.
	reasoningReportRecorded bool

	// legacyFunctionCallNoted gates the legacy function_call synthesis note
	// to exactly once per stream.
	legacyFunctionCallNoted bool

	// totalAccumulated bounds the exchange-wide sum of accumulated semantic
	// bytes.
	totalAccumulated int64

	// budget is the seven-dimension total exchange budget: every event and every state allocation charges it BEFORE the
	// mutation.
	budget streamBudget

	usage *ResponsesUsage

	report ConversionReport

	// heldTerminal holds the item-closing events built on finish_reason; the
	// terminal envelope is appended at release by the [DONE] sentinel (the
	// only release path). A finish that opened no items
	// leaves this nil — the release signal is terminalReleased, never this
	// slice's nil-ness.
	heldTerminal []ResponsesSSEEvent

	// terminalReleased is set when the held terminal was released by the
	// [DONE] sentinel. It is the release signal, NOT heldTerminal's nil-ness:
	// a finish that opened no items holds a nil slice, which must remain
	// distinguishable from "no terminal condition reached".
	terminalReleased bool

	// usageComponentsLossRecorded gates the required-usage-component loss
	// the pinned Responses usage requires the breakdown
	// detail objects the Chat source may not provide, and the decision is
	// recorded exactly once per stream.
	usageComponentsLossRecorded bool

	// usageClampNotes gates the usage-clamp notes (usage_cache_exceeds_input,
	// usage_negative_counts) to once per stream per key.
	usageClampNotes usageClampNotes

	// usageTotalDerivedNoted gates the usage_total_derived note PER DERIVED
	// KEY, not once per stream. A stream can derive a total in one phase and
	// the completion in another; a single per-stream gate then reported the
	// first derivation for the whole exchange, so the log named a key that was
	// not the one delivered and stayed silent about the key that was. A
	// per-key gate still collapses the repeated identical chunks of a hot
	// stream to one line.
	usageTotalDerivedNoted map[string]bool

	// usageMergeNoted gates the usage_total_merged note to once per stream.
	// The event is binary - a redelivery replaced the recorded accounting, or
	// it did not - so a per-key gate would be wrong, and an ungated note let a
	// gateway that repeats the terminal many times push real entries out of a
	// saturated report (4096) for a single exchange.
	usageMergeNoted bool

	// streamError latches the first failure Convert returned, so a caller
	// that ignores it can never release the held terminal afterwards.
	streamError error
}

// wireError marks a conversion error as corrupt upstream Chat wire data: the
// exchange classifies as an upstream failure, never a local conversion
// failure.
func (s *chatResponsesStreamState) wireError(err error) error {
	return upstreamWireError(UpstreamChatCompletions, http.StatusOK, err)
}

// checkAccumulated rejects accumulated semantic state (text, refusal, tool
// arguments) beyond the per-item cumulative bound and the exchange total:
// individually bounded SSE frames could otherwise accumulate without limit
// and be emitted as one generated downstream frame, and a corrupt upstream
// can amplify memory without bound. The typed upstream wire error classifies the exchange as an upstream
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

// chargeStreamIdentity adds n bytes of tool-call identity (call id, function
// name) to the exchange accumulated total: identity renders into the terminal
// envelope and the done events, so it is part of the repeated semantic state
// the release bounds derive from. It is the single accounting rule behind
// both directions' chargeIdentity methods; direction names the stream for the
// error text. The caller wraps the error in its own wireError so the exchange
// classification is unchanged.
func chargeStreamIdentity(accumulated *int64, n int, direction string) error {
	*accumulated += int64(n)
	if *accumulated > maxStreamTotalAccumulatedBytes {
		return fmt.Errorf(
			"%s stream accumulated tool-call identity exceeds the exchange total of %d bytes",
			direction,
			maxStreamTotalAccumulatedBytes,
		)
	}
	return nil
}

// chargeIdentity adds n bytes of tool-call identity (call id, function name)
// to the exchange accumulated total: identity renders into the terminal
// envelope and the done events, so it is part of the repeated semantic
// state the release bounds derive from.
func (s *chatResponsesStreamState) chargeIdentity(n int) error {
	if err := chargeStreamIdentity(&s.totalAccumulated, n, "chat"); err != nil {
		return s.wireError(err)
	}
	return nil
}

// loseServiceTierOnce records the service-tier loss exactly once per stream:
// the tier is a chunk-envelope attribute whose presence on any chunk enters
// the decision once, not once per chunk.
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
// stream.
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
// emitted silently.
func (s *chatResponsesStreamState) loseUnknownUsageComponentsOnce(usage *ChatLLMUsage) error {
	if s.usageComponentsLossRecorded || usage == nil {
		return nil
	}
	s.usageComponentsLossRecorded = true
	// A component is unknown only when NO signal exists anywhere: the top-level provider extensions make the same components known
	// as the pinned detail objects.
	if usage.PromptTokensDetails == nil &&
		usage.CachedTokens == nil &&
		usage.PromptCacheHitTokens == nil {
		if err := s.report.Lose(
			s.policy,
			FeatureUsageCacheReadUnknown,
			"usage",
			"the upstream response did not provide input_tokens_details.cached_tokens; the required Responses usage breakdown cannot be reproduced",
		); err != nil {
			return err
		}
	}
	if usage.CompletionTokensDetails == nil && usage.ReasoningTokens == nil {
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
// The stream lifecycle has explicit phases:
//
//  1. normal chunks — content, tool-call fragments, and the finish chunk;
//  2. after the finish reason — accounting redeliveries that carry the final
//     usage: either the usage-only tail chunk (choices: []/empty, usage
//     present) that the official protocol sends before [DONE] when
//     include_usage is requested, or a repeated terminal chunk (the same
//     single choice with the same finish reason and an insubstantial delta)
//     on which some gateways piggyback the usage instead of sending the
//     bare tail;
//  3. terminal — built at release by the [DONE] frame only, with the final usage.
func (s *chatResponsesStreamState) Convert(
	chunk ChatStreamResponse,
) ([]ResponsesSSEEvent, error) {
	if s.streamError != nil {
		return nil, s.streamError
	}
	events, err := s.convertChunk(chunk)
	if err != nil {
		s.streamError = err
		return nil, err
	}
	return events, nil
}

// isReasoningItem reports whether the open item is a reasoning item
// (the provider_reasoning_thinking accumulation carrier).
func (s *chatResponsesStreamState) isReasoningItem(item *openResponsesItem) bool {
	_, ok := item.item.(*ResponsesReasoningOutputItem)
	return ok
}
