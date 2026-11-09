package transcode

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/geminigeneratecontent"
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
	// streamError latches the first failure of the stream, whatever its
	// cause: the exchange ended on a failure, not on a finish, so every later
	// call and FinalizeEOF propagate it instead of releasing a success
	// terminal.
	streamError error
	// sawCandidate records that any earlier chunk carried a candidate, and
	// promptBlocked that an earlier chunk was prompt-blocked, so the
	// blockReason+candidates contradiction is rejected even when split
	// across chunks in either order.
	sawCandidate  bool
	promptBlocked bool

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
	// usageClampNotes gates the usage-clamp notes to once per stream per
	// key, like the chat stream state: a long stream may repeat the same
	// arithmetic inconsistency in several chunks.
	usageClampNotes usageClampNotes

	// thoughtBufs accumulates streamed thought text per reasoning item.
	thoughtBufs map[geminiPartKey]*strings.Builder

	// totalAccumulated bounds the exchange-wide sum of accumulated semantic
	// bytes, the same per-state bound the chat state applies.
	totalAccumulated int64

	// Per-stream once-gates for the metadata losses.
	safetyLossRecorded         bool
	promptSafetyLossRecorded   bool
	groundingLossRecorded      bool
	logprobsLossRecorded       bool
	tokenCountLossRecorded     bool
	usageBreakdownLossRecorded bool
	serviceTierLossRecorded    bool
	modelStatusLossRecorded    bool
	thoughtTextNoted           bool
	thoughtThinkingNoted       bool
	thoughtLossNoted           bool
	partMetadataLossRecorded   bool
	partSignatureLossRecorded  bool
	callIDSynthesizedNoted     bool

	// pendingSignatures holds the thought signatures the current converter call
	// has accepted; commitCall writes them once the whole call succeeds.
	pendingSignatures []pendingSignature
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
// semantic bytes, mirroring the chat state's bound. It runs BEFORE the added
// bytes are written, so the per-buffer bound includes them.
func (s *geminiResponsesStreamState) checkAccumulated(builder *strings.Builder, added int, what string) error {
	// The bounds are checked BEFORE the caller writes the delta into the
	// builder: a rejected delta must not mutate the accumulated state (or
	// pay the allocation) at all.
	if builder.Len()+added > maxStreamAccumulatedBytes {
		return s.wireError(fmt.Errorf(
			"gemini stream accumulated %s exceeds %d bytes",
			what,
			maxStreamAccumulatedBytes,
		))
	}
	if err := s.checkTotal(added, what); err != nil {
		return err
	}
	s.totalAccumulated += int64(added)
	return nil
}

// checkDeltaBounds validates one delta before any item or part is opened: a
// delta that cannot fit the per-part and exchange-wide bounds must not leave
// output behind.
func (s *geminiResponsesStreamState) checkDeltaBounds(n int, what string) error {
	if n > maxStreamAccumulatedBytes {
		return s.wireError(fmt.Errorf(
			"gemini stream accumulated %s exceeds %d bytes",
			what,
			maxStreamAccumulatedBytes,
		))
	}
	return s.checkTotal(n, what)
}

// checkTotal reports whether n more retained semantic bytes fit the
// exchange-wide accumulated total. It never mutates the total, so a rejected
// charge leaves no state behind.
func (s *geminiResponsesStreamState) checkTotal(n int, what string) error {
	if s.totalAccumulated+int64(n) > maxStreamTotalAccumulatedBytes {
		return s.wireError(fmt.Errorf(
			"gemini stream accumulated %s exceeds the exchange total of %d bytes",
			what,
			maxStreamTotalAccumulatedBytes,
		))
	}
	return nil
}

// chargeTotal adds n retained semantic bytes (text, refusal, tool arguments,
// tool-call identity) to the exchange-wide accumulated total. Everything
// charged here is repeated in done events and the terminal envelope, so the
// release bounds derive from this total. The check runs before the add.
func (s *geminiResponsesStreamState) chargeTotal(n int, what string) error {
	if err := s.checkTotal(n, what); err != nil {
		return err
	}
	s.totalAccumulated += int64(n)
	return nil
}

// Convert processes one Gemini stream chunk into typed Responses events. The
// first failure is latched: every later call, and FinalizeEOF, returns it, so
// a stream that failed can never release a success terminal.
func (s *geminiResponsesStreamState) Convert(chunk geminigeneratecontent.GenerateContentResponse) ([]ResponsesSSEEvent, error) {
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

// convertChunk converts one chunk, leaving failure latching to Convert.
func (s *geminiResponsesStreamState) convertChunk(chunk geminigeneratecontent.GenerateContentResponse) ([]ResponsesSSEEvent, error) {
	if err := s.budget.addEvent(); err != nil {
		return nil, s.wireError(err)
	}

	// An in-stream error frame is an upstream failure terminal: the HTTP
	// status was 200, so the body carries the only truthful signal.
	if chunk.Error != nil {
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
		s.sawCandidate = true
	}

	blocked := chunk.PromptFeedback != nil && chunk.PromptFeedback.BlockReason != nil &&
		*chunk.PromptFeedback.BlockReason != "" &&
		*chunk.PromptFeedback.BlockReason != "BLOCK_REASON_UNSPECIFIED"
	// The API contract: when blockReason is set "the prompt was blocked and
	// no candidates are returned". A chunk carrying both contradicts itself,
	// and so does a block reason in any chunk after one that carried a
	// candidate (or a candidate in any chunk after a blocked one) — neither
	// half can be dropped silently, so it is corrupt wire.
	if blocked && (candidate != nil || s.sawCandidate) {
		return nil, s.wireError(errors.New(
			"gemini stream carries both promptFeedback.blockReason and a candidate",
		))
	}
	if s.promptBlocked && candidate != nil {
		return nil, s.wireError(errors.New(
			"gemini stream chunk carries a candidate after a promptFeedback.blockReason chunk",
		))
	}

	// Prompt-blocked with no candidates: a refusal.
	if candidate == nil {
		if blocked {
			if s.sawTerm {
				return nil, s.wireError(errors.New("gemini stream chunk after the terminal"))
			}
			s.sawTerm = true
			s.promptBlocked = true
			reason := *chunk.PromptFeedback.BlockReason
			events := s.startEvents()
			refusal, err := s.appendRefusal(fmt.Sprintf("the prompt was blocked by the provider (blockReason %s)", reason))
			if err != nil {
				return nil, err
			}
			events = append(events, refusal...)
			s.refused = true
			s.heldTerminal = s.closeAllAndHold()
			// The blocked-prompt chunk is usually the only chunk, so its
			// usage and metadata are recorded here, not lost with it.
			if err := s.recordChunkMetadata(chunk); err != nil {
				return nil, err
			}
			// The note describes the refusal encoding, so it is recorded only
			// after the encoding and the chunk metadata committed.
			if err := s.report.Note(
				FeatureGeminiPromptBlocked,
				"promptFeedback.blockReason",
				fmt.Sprintf("the prompt was blocked (blockReason %s) and the stream is rendered as a refusal", reason),
			); err != nil {
				return nil, err
			}
			return events, nil
		}
		// A chunk with no candidates and no block reason carries no content
		// (some providers emit empty keep-alives, or a usage-only chunk);
		// after the terminal it is absorbed as trailing accounting.
		if s.sawTerm {
			return s.absorbTrailing(chunk)
		}
		return nil, s.recordChunkMetadata(chunk)
	}

	if s.sawTerm {
		// After the finish chunk only accounting may follow.
		return s.absorbTrailing(chunk)
	}

	// A finish reason the target cannot represent rejects the chunk before any
	// part, item, or metadata is committed: a rejected chunk leaves no output.
	reason := ""
	if candidate.FinishReason != nil && *candidate.FinishReason != "" &&
		*candidate.FinishReason != "FINISH_REASON_UNSPECIFIED" {
		reason = *candidate.FinishReason
		if reason != "STOP" && reason != "MAX_TOKENS" && !refusalFinishReasons[reason] {
			return nil, &UnrepresentableError{
				Protocol: string(UpstreamGemini),
				Path:     "candidates[].finishReason",
				Detail:   fmt.Sprintf("the provider ended the stream with finishReason %s", reason),
			}
		}
	}
	// A continuation token is unrepresentable: reject the chunk before any
	// part or metadata is committed.
	if err := rejectContinuationToken(candidate); err != nil {
		return nil, err
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
	if err := s.recordChunkMetadata(chunk); err != nil {
		return nil, err
	}

	// A chunk carrying finishReason is terminal: hold the item-closing events
	// and the terminal envelope for release at EOF (or a late [DONE]).
	if reason != "" {
		s.sawTerm = true
		s.finishReason = reason
		if refusalFinishReasons[reason] {
			// The same rendering as the non-streaming decode: the stop is a
			// refusal, carried by a refusal part on the message item.
			text := fmt.Sprintf("the provider stopped the response (finishReason %s)", reason)
			if candidate.FinishMessage != nil && *candidate.FinishMessage != "" {
				text = *candidate.FinishMessage
			}
			refusal, err := s.appendRefusal(text)
			if err != nil {
				return nil, err
			}
			events = append(events, refusal...)
			s.refused = true
			// The note describes the refusal encoding, so it is recorded only
			// after the refusal passed its budget and bound checks.
			if err := s.report.Note(
				FeatureGeminiFinishRefusal,
				"candidates[].finishReason",
				fmt.Sprintf("finishReason %s rendered as a refusal (gemini_finish_refusal encoding)", reason),
			); err != nil {
				return nil, err
			}
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

// absorbTrailing handles a chunk after the terminal: only accounting and
// metadata may follow (the last usage wins); content parts are corrupt wire,
// and so is a candidate that repeats a finish reason, finish message, or a
// non-model role — the finish chunk already carried the terminal state. The
// candidate's remaining metadata is still recorded under the loss policy.
func (s *geminiResponsesStreamState) absorbTrailing(chunk geminigeneratecontent.GenerateContentResponse) ([]ResponsesSSEEvent, error) {
	if len(chunk.Candidates) > 0 {
		candidate := &chunk.Candidates[0]
		if candidate.Content != nil && len(candidate.Content.Parts) > 0 {
			return nil, s.wireError(errors.New(
				"gemini stream chunk with content parts after the finish chunk",
			))
		}
		if candidate.FinishReason != nil && *candidate.FinishReason != "" &&
			*candidate.FinishReason != "FINISH_REASON_UNSPECIFIED" {
			return nil, s.wireError(fmt.Errorf(
				"gemini stream chunk with finishReason %q after the finish chunk", *candidate.FinishReason,
			))
		}
		if candidate.FinishMessage != nil && *candidate.FinishMessage != "" {
			return nil, s.wireError(errors.New(
				"gemini stream chunk with finishMessage after the finish chunk",
			))
		}
		if candidate.Content != nil && candidate.Content.Role != "" && candidate.Content.Role != "model" {
			return nil, s.wireError(fmt.Errorf(
				"gemini stream candidate content role = %q after the finish chunk, want model or absent",
				candidate.Content.Role,
			))
		}
		if err := s.recordCandidateMetadata(candidate); err != nil {
			return nil, err
		}
	}
	return nil, s.recordChunkMetadata(chunk)
}

// recordChunkMetadata records the response-level fields of one chunk, on
// every path a chunk can take: usage (the last one wins), and the approved
// losses of the prompt safety ratings and the model status, each once per
// stream. The non-streaming decode gates the same fields.
func (s *geminiResponsesStreamState) recordChunkMetadata(chunk geminigeneratecontent.GenerateContentResponse) error {
	if chunk.UsageMetadata != nil {
		if err := s.recordUsage(chunk.UsageMetadata); err != nil {
			return err
		}
	}
	if chunk.PromptFeedback != nil && len(chunk.PromptFeedback.SafetyRatings) > 0 &&
		!s.promptSafetyLossRecorded {
		s.promptSafetyLossRecorded = true
		if err := s.report.Lose(
			s.policy,
			FeatureGeminiSafetyMetadata,
			"promptFeedback.safetyRatings",
			"prompt safety ratings cannot be reproduced in the target response",
		); err != nil {
			return err
		}
	}
	if chunk.ModelStatus != nil && !s.modelStatusLossRecorded {
		s.modelStatusLossRecorded = true
		if err := s.report.Lose(
			s.policy,
			FeatureGeminiModelStatus,
			"modelStatus",
			"the model status (lifecycle stage/retirement) cannot be reproduced in the target response",
		); err != nil {
			return err
		}
	}
	return nil
}
