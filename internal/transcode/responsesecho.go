package transcode

// The request-envelope decisions shared by the renderers that consume a
// Responses source request.
//
// A Responses client can send fields that only a Responses upstream
// understands. Every renderer that cannot carry them must record the same
// observable decision, under the same feature key and path, so an operator
// reading the per-request log sees one vocabulary regardless of the target. The
// Chat renderer implemented this set while the Gemini and Messages renderers
// dropped the same fields silently, which made a reachable Responses-to-Gemini
// route violate the strict-subset invariant.

// responsesEchoLosses records the portability decisions for the Responses
// request envelope. effortPortable reports whether the target can reproduce
// reasoning.effort (only after a capability grant); every other field here has
// no representation in any non-Responses target. A resolved
// previous_response_id is consumed by the continuity store, not lost, so it is
// charged only when the store did not resolve it.
func responsesEchoLosses(
	context *ExchangeContext,
	report *ConversionReport,
	effortPortable bool,
) error {
	if context == nil || context.OriginalResponsesRequest == nil {
		return nil
	}
	echo := context.OriginalResponsesRequest
	policy := context.lossPolicy()
	if echo.PreviousResponseID != nil && context.RequestDepth == 0 {
		if err := report.Lose(
			policy,
			FeaturePreviousResponseID,
			"request.previous_response_id",
			"the Responses previous_response_id field cannot be reproduced by this target, which keeps no conversation state",
		); err != nil {
			return err
		}
	}
	if echo.TopLogprobs != nil {
		if err := report.Lose(
			policy,
			FeatureRequestTopLogprobs,
			"request.top_logprobs",
			"the Responses top_logprobs field cannot be reproduced by this target",
		); err != nil {
			return err
		}
	}
	if echo.ServiceTier != nil {
		if err := report.Lose(
			policy,
			FeatureRequestServiceTier,
			"request.service_tier",
			"the Responses service_tier field cannot be reproduced by this target",
		); err != nil {
			return err
		}
	}
	if echo.Truncation != nil {
		if err := report.Lose(
			policy,
			FeatureRequestTruncation,
			"request.truncation",
			"the Responses truncation field cannot be reproduced by this target",
		); err != nil {
			return err
		}
	}
	if echo.Reasoning != nil && echo.Reasoning.Effort != nil && !effortPortable {
		if err := report.Lose(
			policy,
			FeatureRequestReasoning,
			"reasoning.effort",
			"the Responses reasoning effort cannot be reproduced by this target",
		); err != nil {
			return err
		}
	}
	// The reasoning summary style has no representation outside Responses:
	// only the effort is portable, so the summary request is its own decision.
	if echo.Reasoning != nil && echo.Reasoning.Summary != nil {
		if err := report.Lose(
			policy,
			FeatureReasoningSummary,
			"reasoning.summary",
			"the reasoning summary style cannot be reproduced by this target",
		); err != nil {
			return err
		}
	}
	return nil
}
