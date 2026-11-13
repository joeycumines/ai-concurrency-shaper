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

// echoCarriage reports the envelope fields a target can carry natively. A false
// member is the target's own limitation, so it takes the shared observable
// decision rather than depending on the target author remembering to add it.
type echoCarriage struct {
	// Effort reports that the target reproduces reasoning.effort (a
	// capability grant, since only some upstreams accept the knob).
	Effort bool
	// User and Store report that the target models the Responses user and
	// store fields. Chat does; Gemini and Messages have no such field.
	User  bool
	Store bool
}

// responsesEchoLosses records the portability decisions for the Responses
// request envelope. Each echoCarriage member reports whether the target can
// reproduce that field; every field a target cannot carry is recorded here so
// an operator reading the per-request log sees one vocabulary regardless of the
// target. A resolved previous_response_id is consumed by the continuity store,
// not lost, so it is charged only when the store did not resolve it.
func responsesEchoLosses(
	context *ExchangeContext,
	report *ConversionReport,
	carriage echoCarriage,
) error {
	if context == nil || context.OriginalResponsesRequest == nil {
		return nil
	}
	echo := context.OriginalResponsesRequest
	policy := context.lossPolicy()
	// previous_response_id is a REFERENCE to conversation state the target does
	// not keep. It is a loss when the continuity store did not resolve it, and
	// the loss is approved by default: an unresolved reference is the ordinary
	// shape of a stateless deployment (continuity is opt-in), so refusing it
	// would turn the proxy's own missing feature into a client-visible failure.
	// Deny it with -transcode-allow-loss '!previous_response_id' to restore the
	// refusal. A store MISS is the same case — nothing was reconstructed, so the
	// field is lost, never a failure.
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
	// user and store are Responses envelope controls: a target that does not
	// model them cannot reproduce what the client asked for, so the drop is
	// observable under the envelope-controls key rather than silent. store:FALSE
	// is the exception, because a target with no storage already satisfies it —
	// only store:true asks for something the target cannot do.
	if echo.User != nil && !carriage.User {
		if err := report.Lose(
			policy,
			FeatureResponsesControls,
			"request.user",
			"the Responses user field cannot be reproduced by this target",
		); err != nil {
			return err
		}
	}
	if echo.Store != nil && *echo.Store && !carriage.Store {
		if err := report.Lose(
			policy,
			FeatureResponsesControls,
			"request.store",
			"the Responses store field cannot be reproduced by this target",
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
	if echo.Reasoning != nil && echo.Reasoning.Effort != nil && !carriage.Effort {
		if err := report.Lose(
			policy,
			FeatureRequestReasoning,
			"request.reasoning.effort",
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
			"request.reasoning.summary",
			"the reasoning summary style cannot be reproduced by this target",
		); err != nil {
			return err
		}
	}
	return nil
}
