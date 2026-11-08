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

// convertPart converts one Gemini stream part into Responses events.
func (s *geminiResponsesStreamState) convertPart(
	part geminigeneratecontent.Part, partIndex int,
) ([]ResponsesSSEEvent, error) {
	path := fmt.Sprintf("candidates[].content.parts[%d]", partIndex)

	// Unsupported payloads are unrepresentable in the Responses stream. Raw
	// member payloads are read with RawMemberPresent, matching Part.Validate
	// and the non-streaming decode: an explicit JSON null is not a payload.
	switch {
	case part.InlineData != nil, part.FileData != nil,
		geminigeneratecontent.RawMemberPresent(part.ExecutableCode),
		geminigeneratecontent.RawMemberPresent(part.CodeExecutionResult),
		geminigeneratecontent.RawMemberPresent(part.ToolCall),
		geminigeneratecontent.RawMemberPresent(part.ToolResponse),
		geminigeneratecontent.RawMemberPresent(part.AudioTranscription):
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

	// Part modifiers, a signature on a non-call part, and a dropped thought
	// are observable losses, once per stream. Every policy gate the part
	// needs is decided BEFORE any of its payload commits, so a part refused
	// for one loss leaves no other entry, no stored signature, and no events
	// behind. Each loss is recorded only AFTER the payload committed, so a
	// part whose payload is refused (bounds, an invalid function call) leaves
	// none. Signature-only and empty-thought parts commit no payload, yet the
	// metadata and signature they carry are still losses of that exact part:
	// the non-streaming decode records both on every part shape.
	partMetadata := geminigeneratecontent.RawMemberPresent(part.VideoMetadata) ||
		geminigeneratecontent.RawMemberPresent(part.SpeechMetadata) ||
		geminigeneratecontent.RawMemberPresent(part.PartMetadata) ||
		geminigeneratecontent.RawMemberPresent(part.MediaResolution) ||
		part.MediaProcessing != nil
	thought := part.Thought != nil && *part.Thought
	dropsThought := thought && part.Text != nil && *part.Text != "" &&
		!s.capabilities.ProviderReasoningThinking && !s.capabilities.ProviderReasoningText
	if err := s.decidePartLosses(part, path, partMetadata, dropsThought); err != nil {
		return nil, err
	}

	if part.FunctionCall != nil {
		events, err := s.convertFunctionCall(part, path)
		if err != nil {
			return nil, err
		}
		if err := s.losePartMetadata(partMetadata, path); err != nil {
			return nil, err
		}
		return events, nil
	}

	if part.Text == nil || (thought && *part.Text == "") {
		// A signature-only part, or an empty thought (the empty-delta short
		// circuit in emitThoughtText applies), carries no payload and emits
		// nothing, and records no reasoning note or loss for an encoding that
		// produced nothing.
		return nil, s.losePartModifiers(part, path, partMetadata)
	}
	if thought {
		// Same capability rules as the non-streaming decode: thought text is
		// provider reasoning; mapped under the capability or an approved
		// loss, each recorded once per stream. The encoding commits first, so
		// a rejected delta leaves no note behind.
		var events []ResponsesSSEEvent
		switch {
		case s.capabilities.ProviderReasoningThinking:
			var err error
			events, err = s.emitThoughtText(*part.Text, path)
			if err != nil {
				return nil, err
			}
		case s.capabilities.ProviderReasoningText:
			var err error
			events, err = s.emitTextDelta(*part.Text)
			if err != nil {
				return nil, err
			}
		default:
			// The dropped thought commits as an approved loss, and its
			// modifiers and signature remain observable losses of the part.
			if err := s.loseThought(path); err != nil {
				return nil, err
			}
			return nil, s.losePartModifiers(part, path, partMetadata)
		}
		if err := s.losePartModifiers(part, path, partMetadata); err != nil {
			return nil, err
		}
		if s.capabilities.ProviderReasoningThinking {
			if !s.thoughtThinkingNoted {
				s.thoughtThinkingNoted = true
				if err := s.report.Note(
					FeatureProviderReasoningText,
					path,
					"provider reasoning mapped to a native thinking block (provider_reasoning_thinking encoding)",
				); err != nil {
					return nil, err
				}
			}
		} else if !s.thoughtTextNoted {
			s.thoughtTextNoted = true
			if err := s.report.Note(
				FeatureProviderReasoningText,
				path,
				"provider reasoning text mapped to ordinary text (provider_reasoning_text capability)",
			); err != nil {
				return nil, err
			}
		}
		return events, nil
	}

	events, err := s.emitTextDelta(*part.Text)
	if err != nil {
		return nil, err
	}
	if err := s.losePartModifiers(part, path, partMetadata); err != nil {
		return nil, err
	}
	return events, nil
}

// decidePartLosses peeks every policy gate the part's losses need — a dropped
// thought, part modifier metadata, a non-call signature — before any of the
// part's payload commits. The losses themselves are recorded afterwards by
// loseThought and losePartModifiers, which then cannot be refused.
func (s *geminiResponsesStreamState) decidePartLosses(
	part geminigeneratecontent.Part, path string, partMetadata, dropsThought bool,
) error {
	if dropsThought {
		if err := s.policy.refusal(FeatureProviderReasoningText, path); err != nil {
			return err
		}
	}
	if partMetadata {
		if err := s.policy.refusal(FeatureGeminiPartMetadata, path); err != nil {
			return err
		}
	}
	if hasNonCallSignature(part) {
		if err := s.policy.refusal(FeatureGeminiResponseThoughtSignature, path); err != nil {
			return err
		}
	}
	return nil
}

// losePartModifiers records the part's metadata and non-call signature
// losses, both already decided by decidePartLosses.
func (s *geminiResponsesStreamState) losePartModifiers(
	part geminigeneratecontent.Part, path string, partMetadata bool,
) error {
	if err := s.losePartMetadata(partMetadata, path); err != nil {
		return err
	}
	return s.loseNonCallSignature(part, path)
}

// losePartMetadata records the approved loss of part modifier metadata, once
// per stream. Its policy gate was decided before the part's payload committed.
func (s *geminiResponsesStreamState) losePartMetadata(present bool, path string) error {
	if !present || s.partMetadataLossRecorded {
		return nil
	}
	s.partMetadataLossRecorded = true
	return s.report.Lose(
		s.policy,
		FeatureGeminiPartMetadata,
		path,
		"part modifier metadata cannot be reproduced in the target response",
	)
}

// loseThought records the approved loss of thought text under no reasoning
// capability, once per stream. Its policy gate was decided before the part's
// payload committed.
func (s *geminiResponsesStreamState) loseThought(path string) error {
	if s.thoughtLossNoted {
		return nil
	}
	s.thoughtLossNoted = true
	return s.report.Lose(
		s.policy, FeatureProviderReasoningText, path,
		"provider reasoning text cannot be reproduced in the target response",
	)
}

// hasNonCallSignature reports whether the part carries a thought signature
// that no function call retains. An explicit empty signature carries none.
func hasNonCallSignature(part geminigeneratecontent.Part) bool {
	return part.ThoughtSignature != nil && *part.ThoughtSignature != "" && part.FunctionCall == nil
}

// loseNonCallSignature records the approved loss of a thought signature on a
// non-function-call part, once per stream. Its policy gate was decided before
// the part's payload committed.
func (s *geminiResponsesStreamState) loseNonCallSignature(part geminigeneratecontent.Part, path string) error {
	if !hasNonCallSignature(part) || s.partSignatureLossRecorded {
		return nil
	}
	s.partSignatureLossRecorded = true
	return s.report.Lose(
		s.policy,
		FeatureGeminiResponseThoughtSignature,
		path,
		"a thought signature on a non-function-call stream part cannot be reproduced in the target response",
	)
}

// emitTextDelta opens the message item and content part on first text and
// emits one response.output_text.delta per chunk.
func (s *geminiResponsesStreamState) emitTextDelta(delta string) ([]ResponsesSSEEvent, error) {
	if delta == "" {
		return nil, nil
	}
	// The bounds are checked before the item and part are opened, so a delta
	// that cannot fit leaves no output behind.
	if err := s.checkDeltaBounds(len(delta), "text delta"); err != nil {
		return nil, err
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
	// The bounds are checked before the reasoning item is opened, so a delta
	// that cannot fit leaves no output behind.
	if err := s.checkDeltaBounds(len(delta), "thought delta"); err != nil {
		return nil, err
	}
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
	// The added event carries a detached snapshot of the item at creation:
	// the live item gains parts and is completed later, possibly within the
	// same chunk, before the event is marshaled.
	snapshot := *message
	snapshot.Content = ResponsesOutputContentParts{}
	return append(events, s.builder.OutputItemAdded(outputIndex, &snapshot)), nil
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
	// A detached creation snapshot, as for message items.
	snapshot := *reasoning
	snapshot.Summary = []ResponsesReasoningSummary{}
	return append(events, s.builder.OutputItemAdded(outputIndex, &snapshot)), nil
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
	synthesizedCallID := false
	callID := call.ID
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
	if callID == "" {
		callID = s.ctx.IDs.New("gcall_")
		synthesizedCallID = true
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
	if synthesizedCallID && !s.callIDSynthesizedNoted {
		// The note is recorded only once per stream, after the synthesized
		// identity passed the exchange-total charge.
		s.callIDSynthesizedNoted = true
		if err := s.report.Note(
			FeatureGeminiCallIDSynthesized, path,
			"the upstream function call carried no id; a proxy-minted call id was emitted",
		); err != nil {
			return nil, err
		}
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
	// The added event announces the call in progress with empty arguments,
	// which then arrive through the delta, as on every other stream path.
	added := *item
	added.Status = ResponsesItemInProgress
	added.Arguments = ""
	return append(events,
		s.builder.OutputItemAdded(outputIndex, &added),
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
	// The refusal text comes from the upstream (finishMessage), so it has the
	// same per-part bound as streamed text. The bounds are checked before any
	// item or part is opened.
	if err := s.checkDeltaBounds(len(text), "refusal"); err != nil {
		return nil, err
	}
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

// rejectContinuationToken refuses a candidate carrying an unrepresentable
// continuation token. It is pure, so callers run it before committing output.
func rejectContinuationToken(candidate *geminigeneratecontent.Candidate) error {
	if candidate.ContinuationToken == nil || *candidate.ContinuationToken == "" {
		return nil
	}
	return &UnrepresentableError{
		Protocol: string(UpstreamGemini),
		Path:     "candidates[].continuationToken",
		Detail:   "a continuation token cannot be represented in the target dialect",
	}
}

// recordCandidateMetadata records the per-chunk candidate metadata losses once
// per stream.
func (s *geminiResponsesStreamState) recordCandidateMetadata(candidate *geminigeneratecontent.Candidate) error {
	// A trailing chunk carrying a continuation token is still unrepresentable;
	// the non-trailing path already rejected it before committing output.
	if err := rejectContinuationToken(candidate); err != nil {
		return err
	}
	if len(candidate.SafetyRatings) > 0 && !s.safetyLossRecorded {
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
	if err := s.usageClampNotes.note(&s.report, "usageMetadata", clamp); err != nil {
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
// ends before any finish reason, or that failed at any point, is never a
// success.
func (s *geminiResponsesStreamState) FinalizeEOF() ([]ResponsesSSEEvent, error) {
	if s.streamError != nil {
		// The stream failed earlier: propagate the failure; a failed exchange
		// is never a successful completion.
		return nil, s.streamError
	}
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
	batch, err := c.convertFrame(frame)
	if err != nil {
		c.streamError = err
		return convertedBatch{}, err
	}
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
	batch, err := c.convertFrame(frame)
	if err != nil {
		c.streamError = err
		return convertedBatch{}, err
	}
	return batch, nil
}

// convertFrame converts one frame, leaving failure latching to Convert.
func (c *geminiToAnthropicConverter) convertFrame(frame SSEEvent) (convertedBatch, error) {
	if c.gemini.streamError != nil {
		return convertedBatch{}, c.gemini.streamError
	}
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
// The Anthropic state's latch is checked as well as the converter's (the
// Gemini state's is checked by its own FinalizeEOF): a failure fed to it
// directly after the release must not be washed out by an empty clean batch.
func (c *geminiToAnthropicConverter) FinalizeEOF() (convertedBatch, error) {
	if c.streamError != nil {
		return convertedBatch{}, c.streamError
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
	return c.gemini.continuityTurns(requestTurns, depth)
}
