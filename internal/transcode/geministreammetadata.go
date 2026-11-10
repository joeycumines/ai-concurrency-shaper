package transcode

import (
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/geminigeneratecontent"
)

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
