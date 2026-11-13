package transcode

// The Responses `user` field is an identifier. An empty one carries nothing, so a
// target with no user field already satisfies it — the same reasoning that makes
// store:false satisfied on a target with no storage. Charging it would refuse a
// request that loses nothing.

import (
	"testing"
)

// TestResponsesEchoEmptyUserIsSatisfiedOnAStatelessTarget proves an empty user
// string records no decision and never refuses, while a real one still does.
func TestResponsesEchoEmptyUserIsSatisfiedOnAStatelessTarget(t *testing.T) {
	empty := ""
	applyEmpty := func(echo *ResponsesRequestEcho) { echo.User = &empty }
	permissive := LossPolicy{Allowed: map[Feature]struct{}{FeatureResponsesControls: {}}}
	for _, name := range []string{"gemini", "messages"} {
		render := responsesEchoRenderers()[name]
		// Strict: nothing is approved, and an empty user loses nothing.
		report, err := render(responsesEchoContext(StrictLossPolicy(), applyEmpty))
		if err != nil {
			t.Fatalf("%s: an empty user refused a strict render: %v", name, err)
		}
		if reportHasAt(report, FeatureResponsesControls, "request.user") {
			t.Fatalf("%s: an empty user recorded a decision: %+v", name, report.Losses)
		}
		// A real identifier is still a decision.
		real := "user_1"
		applyReal := func(echo *ResponsesRequestEcho) { echo.User = &real }
		report, err = render(responsesEchoContext(permissive, applyReal))
		if err != nil {
			t.Fatalf("%s permissive render: %v", name, err)
		}
		if !reportHasAt(report, FeatureResponsesControls, "request.user") {
			t.Fatalf("%s: a real user recorded no decision: %+v", name, report.Losses)
		}
	}
}
