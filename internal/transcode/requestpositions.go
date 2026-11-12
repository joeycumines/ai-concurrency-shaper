package transcode

// Position decisions shared by the request renderers.
//
// A request-side portability gate exists to police CLIENT input, so it must not
// charge the client for a position the client's own request did not create.
// CanonicalRequest.RetainedTurns marks the leading turns the opt-in continuity
// store reconstructed; the encoding a renderer applies is identical for
// authored and retained turns, only the policy decision differs. Keeping the
// two branches in one place stops the four renderers from drifting apart: the
// Chat renderer implemented this rule while the other three re-charged the
// client, and a default-deny key could then reject a cleanly authored request.

// chargePosition records the decision for a turn that cannot keep its
// position in the target request. A turn the CLIENT authored is a
// policy-gated loss (it can reject the request); a turn the continuity store
// reconstructed is a Note, because the position was already decided once when
// the chain was first rendered. index is the turn's index in
// request.Turns; detail describes the encoding, and the retained branch appends
// its own explanation so the log never claims a loss the policy never saw.
func chargePosition(
	request CanonicalRequest,
	index int,
	feature Feature,
	path string,
	detail string,
	report *ConversionReport,
	policy LossPolicy,
) error {
	if index >= 0 && index < request.RetainedTurns {
		return report.Note(
			feature,
			path,
			detail+" (the turn came from the proxy's retained continuity history, not this request, and the position was already decided when the chain was first rendered)",
		)
	}
	return report.Lose(policy, feature, path, detail)
}
