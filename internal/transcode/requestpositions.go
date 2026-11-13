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
			detail+" (the turn came from the proxy's retained continuity history, not this request, and the encoding was already decided once when the chain was first rendered)",
		)
	}
	return report.Lose(policy, feature, path, detail)
}

// chargeSystemTurnJoin records the decision for several system-channel turns
// that must share one system field in the target. Every dialect models at most
// one system turn, so the encoding joins their text in encounter order and the
// TURN BOUNDARY is what is lost — even on a target that emits one block or part
// per text part and so preserves every byte. Only the CLIENT can be made to pay
// for it: more than one client-authored contributing turn is a policy-gated
// loss, while a surplus that came from the retained prefix is a Note, because
// the proxy created those turns and their boundaries were already decided once.
//
// contributing holds the canonical index of each system-channel turn that
// actually contributed content. A turn whose parts were all dropped contributed
// nothing and must not count: that would make the decision describe a merge
// that never happened. One or zero contributing turns take no decision at all.
func chargeSystemTurnJoin(
	request CanonicalRequest,
	contributing []int,
	path string,
	detail string,
	report *ConversionReport,
	policy LossPolicy,
) error {
	if len(contributing) <= 1 {
		return nil
	}
	clientTurns := 0
	for _, index := range contributing {
		if index >= request.RetainedTurns {
			clientTurns++
		}
	}
	if clientTurns > 1 {
		return report.Lose(policy, FeatureMultipleSystemTurns, path, detail)
	}
	return report.Note(
		FeatureMultipleSystemTurns,
		path,
		detail+" (the surplus turns came from the proxy's retained continuity history, not this request)",
	)
}
