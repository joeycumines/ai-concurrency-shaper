package transcode

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// chatUsageKeyIsNull reports whether the named key inside the `usage` object
// of a raw chat chunk is present and explicitly null. Absent keys, absent
// usage, non-object usage, and invalid JSON report false (the pointer totals
// decide those cases).
func chatUsageKeyIsNull(chunkData json.RawMessage, key string) bool {
	if len(chunkData) == 0 {
		return false
	}
	var outer struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(chunkData, &outer); err != nil {
		return false
	}
	if len(outer.Usage) == 0 {
		return false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(outer.Usage, &probe); err != nil {
		return false
	}
	value, ok := probe[key]
	if !ok {
		return false
	}
	return string(bytes.TrimSpace(value)) == "null"
}

// noteDerivedUsageTotal records the usage_total_derived note exactly once per
// stream when the decoder derived a missing total from the two present ones.
// It is called from every usage-absorbing path (pre-finish and post-finish) so
// the derivation stays observable wherever the accounting arrives.
func (s *chatResponsesStreamState) noteDerivedUsageTotal(chunk ChatStreamResponse) error {
	if chunk.DerivedUsageTotal == "" || s.usageTotalDerivedNoted[chunk.DerivedUsageTotal] {
		return nil
	}
	if s.usageTotalDerivedNoted == nil {
		s.usageTotalDerivedNoted = make(map[string]bool, 1)
	}
	s.usageTotalDerivedNoted[chunk.DerivedUsageTotal] = true
	return s.report.Note(
		FeatureUsageTotalDerived,
		"usage",
		fmt.Sprintf(
			"the upstream usage omitted %s; it was derived from the other two totals (never defaulted to zero)",
			chunk.DerivedUsageTotal,
		),
	)
}

// isRepeatedTerminalChunk reports whether chunk re-emits the recorded
// terminal: exactly one index-0 choice carrying the same finish reason with
// an insubstantial delta. Role-only deltas and empty-string content,
// refusal, and reasoning fields carry no output; any tool-call fragment or
// raw legacy function-call value other than an explicit null or the empty
// string is substantive, as is any finish reason other than the recorded
// one. (The wire decoder already normalizes the benign empty
// `function_call` fragment to nil before this predicate sees it; the raw
// check here keeps the predicate safe on its own terms.)
func isRepeatedTerminalChunk(chunk ChatStreamResponse, finishReason string) bool {
	if len(chunk.Choices) != 1 {
		return false
	}
	choice := chunk.Choices[0]
	if choice.Index != 0 {
		return false
	}
	if choice.FinishReason == nil || *choice.FinishReason != finishReason {
		return false
	}
	if d := choice.Delta; d != nil {
		if (d.Content != nil && *d.Content != "") ||
			(d.Refusal != nil && *d.Refusal != "") ||
			(d.Reasoning != nil && *d.Reasoning != "") ||
			(d.ReasoningContent != nil && *d.ReasoningContent != "") ||
			len(d.ToolCalls) > 0 {
			return false
		}
		if trimmed := bytes.TrimSpace(d.FunctionCall); len(trimmed) > 0 &&
			!bytes.Equal(trimmed, []byte("null")) {
			return false
		}
	}
	return true
}

// absorbPhase2Accounting folds the accounting carried by one post-finish
// chunk into the terminal envelope's usage: stable chunk identity (enforced
// only when the chunk carries it — gateways may omit id/model on pure
// accounting frames), the service-tier loss decision, per-choice logprobs,
// and the usage totals when present. It emits no events: accounting frames
// never render downstream.
func (s *chatResponsesStreamState) absorbPhase2Accounting(chunk ChatStreamResponse) error {
	if chunk.ID != "" && chunk.ID != s.chunkID {
		return s.wireError(fmt.Errorf(
			"chat stream chunk id %q does not match the first chunk id %q",
			chunk.ID,
			s.chunkID,
		))
	}
	if chunk.Model != "" && chunk.Model != s.chunkModel {
		return s.wireError(fmt.Errorf(
			"chat stream chunk model %q does not match the first chunk model %q",
			chunk.Model,
			s.chunkModel,
		))
	}
	if chunk.ServiceTier != nil {
		if err := s.loseServiceTierOnce(); err != nil {
			return err
		}
	}
	for _, choice := range chunk.Choices {
		if choice.LogProbs != nil {
			if err := s.loseLogprobsOnce(); err != nil {
				return err
			}
		}
	}
	if chunk.Usage == nil {
		return nil
	}
	if err := s.noteDerivedUsageTotal(chunk); err != nil {
		return err
	}
	if err := s.loseUnknownUsageComponentsOnce(chunk.Usage); err != nil {
		return err
	}
	converted, clamp := chatUsageToResponsesUsage(chunk.Usage, chunk.DerivedUsageTotal)
	if err := s.usageClampNotes.note(&s.report, "usage", clamp); err != nil {
		return err
	}
	// A repeated terminal REPLACES the recorded accounting with whatever this
	// frame carries. That is a merge of two source values, so it must be
	// observable: without a note, a gateway that redelivers a small tail over a
	// real total silently overwrites the client's token counts and the exchange
	// is still reported as a clean success. Recorded once per stream, and only
	// when the values actually differ, so a benign exact repeat stays quiet.
	//
	// DECISION (last-wins, deliberate): the later frame is the upstream's
	// latest word on the exchange's totals — the official protocol itself
	// sends the authoritative usage tail AFTER the finish chunk, so
	// first-wins would freeze the accounting at the earliest (often empty)
	// reading and report stale billing counts as current. The divergent-
	// redelivery note is the protection against an adversarial shrink: the
	// replacement is observable in the per-request log, never silent.
	if s.usage != nil && !s.usageMergeNoted && !sameUsageAccounting(*s.usage, *converted) {
		if err := s.report.Note(
			FeatureUsageTotalMerged,
			"usage",
			"a repeated terminal frame re-delivered the accounting; the recorded usage was "+
				"replaced with the values in that frame",
		); err != nil {
			return err
		}
		s.usageMergeNoted = true // set after the note commits
	}
	s.usage = converted
	return nil
}

// sameUsageAccounting reports whether two converted usage snapshots agree on
// the counts a client can observe. The breakdown counts are compared too: a
// redelivery that keeps the three totals but zeroes cached_tokens has still
// replaced a client-observable value, and the rationale for the merge note
// applies to it exactly as it does to the totals. The optional carriers are
// compared BY VALUE and nil-safely, never by pointer identity - two snapshots
// carrying the same count in different allocations are the same accounting.
func sameUsageAccounting(a, b ResponsesUsage) bool {
	return a.InputTokens == b.InputTokens &&
		a.OutputTokens == b.OutputTokens &&
		a.TotalTokens == b.TotalTokens &&
		sameCreatedCacheTokens(a.CreatedCacheTokens, b.CreatedCacheTokens) &&
		sameCachedTokens(a.InputTokensDetails, b.InputTokensDetails) &&
		sameReasoningTokens(a.OutputTokensDetails, b.OutputTokensDetails)
}

// sameCachedTokens compares two optional input breakdowns by value, treating
// two absent carriers as equal — the same nil-safe convention as
// sameCreatedCacheTokens below.
func sameCachedTokens(a, b *UsageInputTokensDetails) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.CachedTokens == b.CachedTokens
	}
}

// sameReasoningTokens compares two optional output breakdowns by value,
// treating two absent carriers as equal.
func sameReasoningTokens(a, b *UsageOutputTokensDetails) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.ReasoningTokens == b.ReasoningTokens
	}
}

// sameCreatedCacheTokens compares two optional counts by value, treating two
// absent carriers as equal.
func sameCreatedCacheTokens(a, b *int64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// chatUsageToResponsesUsage converts a Chat chunk usage into the Responses
// form. The pinned Responses contract requires the breakdown detail objects;
// the call sites gate the unknown components through
// loseUnknownUsageComponentsOnce before this runs, so the zeros below are
// emitted only after the explicit usage-timing loss.
// The top-level provider extensions feed the same breakdowns with details
// winning. The created_cache_tokens provider extension rides
// the in-memory Responses usage carrier (json:"-"): the composed
// Messages←Chat stream can then know the cache-write component exactly like
// the non-streaming decode, without emitting wire bytes the Responses
// contract does not define.
//
// derivedTotal is the total the stream decoder synthesized from the other two
// ("" when the source carried all three). It only affects the clamp's PRESENCE
// bookkeeping - which counts are source facts - never the emitted numbers.
func chatUsageToResponsesUsage(
	usage *ChatLLMUsage,
	derivedTotal string,
) (*ResponsesUsage, usageClamp) {
	if usage == nil {
		return nil, usageClamp{}
	}
	prompt := int64(usage.PromptTokens)
	completion := int64(usage.CompletionTokens)
	total := int64(usage.TotalTokens)
	cached := int64(0)
	reasoning := int64(0)
	switch {
	case usage.PromptTokensDetails != nil:
		cached = int64(usage.PromptTokensDetails.CachedTokens)
	case usage.CachedTokens != nil:
		cached = int64(*usage.CachedTokens)
	case usage.PromptCacheHitTokens != nil:
		cached = int64(*usage.PromptCacheHitTokens)
	}
	switch {
	case usage.CompletionTokensDetails != nil:
		reasoning = int64(usage.CompletionTokensDetails.ReasoningTokens)
	case usage.ReasoningTokens != nil:
		reasoning = int64(*usage.ReasoningTokens)
	}
	out := &ResponsesUsage{
		InputTokens:  prompt,
		OutputTokens: completion,
		TotalTokens:  total,
		InputTokensDetails: &UsageInputTokensDetails{
			CachedTokens: 0,
		},
		OutputTokensDetails: &UsageOutputTokensDetails{
			ReasoningTokens: 0,
		},
	}
	out.InputTokensDetails.CachedTokens = cached
	out.OutputTokensDetails.ReasoningTokens = reasoning
	if usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CreatedCacheTokens != nil {
		created := int64(*usage.PromptTokensDetails.CreatedCacheTokens)
		out.CreatedCacheTokens = &created
	}
	// The source counts are clamped into the Responses invariants (nonnegative
	// counts, a cached breakdown no larger than the input total, a total that
	// matches the emitted components) and the corrections are returned for the
	// caller to record as ungated notes: an arithmetically inconsistent
	// upstream usage is a subject-to-change provider value, never an exchange
	// failure. The helper itself cannot fail.
	//
	// derivedTotal names the total this decoder DERIVED from the other two
	// (DerivedUsageTotal, "" when the source carried all three). The clamp
	// builds its detail from what the source REPORTED, so a derived component
	// must be declared absent: otherwise the detail relays an invented number
	// as a source observation and the operator cannot tell which count the
	// proxy made up. The breakdown presence is exact (each source spelling is
	// an optional pointer).
	presence := usagePresence{
		input:      derivedTotal != "prompt_tokens",
		output:     derivedTotal != "completion_tokens",
		total:      derivedTotal != "total_tokens",
		cacheRead:  usage.PromptTokensDetails != nil || usage.CachedTokens != nil || usage.PromptCacheHitTokens != nil,
		cacheWrite: usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CreatedCacheTokens != nil,
		reasoning:  usage.CompletionTokensDetails != nil || usage.ReasoningTokens != nil,
	}
	return out, clampResponsesUsage(out, presence)
}
