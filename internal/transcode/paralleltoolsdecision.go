package transcode

// The parallel-tool-calls setting, decided once.
//
// A client's explicit parallel-tool-calls setting is a real intent, but whether
// losing it costs the client anything depends on the VALUE and on whether a tool
// call can happen at all. The shape of the decision is shared; only the "can
// this target carry it" fact is per target:
//
//   - parallel ALLOWED (true) costs nothing on any target: every dialect's
//     default is "parallel allowed", so omitting the field asserts the same
//     thing. Charging it would refuse a request that loses nothing.
//   - parallel DISABLED (false) is a real loss wherever the target cannot
//     express it, because the upstream may then emit parallel calls the client
//     forbade. That is a POLICY-GATED loss, so an operator can refuse it — a
//     Note would leave them no control at all.
//   - Two conditions make the setting inert regardless: the request declares no
//     tools, or its tool_choice forbids every call. No tool call can happen, so
//     the client's setting is already satisfied and there is nothing to lose.
//     Those are Notes: observable, never refusable, because refusing an
//     already-satisfied assertion would be an availability failure.
//
// This mirrors the reasoning already applied to `strict:false` and to the
// Messages wire's own disable flag.

// chargeParallelTools records the decision for a request's explicit
// parallel-tool-calls setting, given whether the TARGET can express it. The
// request is the one being rendered, so its tool list and choice are the ones
// the upstream will actually see. A nil return with carriage true means the
// caller should emit the field itself.
func chargeParallelTools(
	request CanonicalRequest,
	carriage bool,
	path string,
	report *ConversionReport,
	policy LossPolicy,
) error {
	if request.ParallelTools == nil {
		return nil
	}
	// An explicit true asserts the dialect default, so every target already
	// satisfies it by omitting the field.
	if *request.ParallelTools {
		return nil
	}
	// Inert first: no tool call can happen, so nothing is lost no matter what
	// the target can express. Note it rather than charge a key that is not
	// default-approved.
	inert, err := chargeParallelToolsInert(request, path, report)
	if err != nil || inert {
		return err
	}
	if !carriage {
		return report.Lose(
			policy,
			FeatureParallelToolCalls,
			path,
			"the target cannot disable parallel tool calls, so the upstream may emit calls the client forbade",
		)
	}
	return nil
}

// chargeParallelToolsInert records the Note for an inert parallel-tools setting
// and reports whether the setting was inert. A false result means the request
// COULD emit a tool call, so the caller decides between a loss and native
// carriage.
func chargeParallelToolsInert(
	request CanonicalRequest,
	path string,
	report *ConversionReport,
) (bool, error) {
	switch {
	case len(request.Tools) == 0:
		return true, report.Note(
			FeatureParallelToolCalls,
			path,
			"no tools are declared, so parallel tool calls are already impossible; the client's parallel-call setting needs no wire field",
		)
	case request.ToolChoice != nil && request.ToolChoice.Mode == "none":
		return true, report.Note(
			FeatureParallelToolCalls,
			path,
			"the tool_choice forbids every tool call, so parallel tool calls are already impossible; the client's parallel-call setting needs no wire field",
		)
	}
	return false, nil
}
