package transcode

// A Responses client can send request-envelope fields only a Responses upstream
// understands. Every renderer that cannot carry them must record the same
// observable decision under the same feature key and path, whether the target
// is Chat, Gemini, or Messages; before the shared helper existed only the Chat
// renderer did so, and the Gemini and Messages renderers dropped the same
// fields silently (a reachable Responses-to-Gemini route broke the
// strict-subset invariant).

import (
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// responsesEchoRequest is the envelope under test: one user turn so the render
// has a conversation and no field-specific refusals of its own.
func responsesEchoRequest() CanonicalRequest {
	return CanonicalRequest{
		ClientModel:     "m",
		MaxOutputTokens: new(64),
		Turns: []CanonicalTurn{{
			Role:  CanonicalUser,
			Parts: []CanonicalPart{CanonicalText{Text: "hi"}},
		}},
	}
}

// responsesEchoFields lists each non-portable envelope field with the feature
// key and path the shared decision must record for it, plus a builder that
// populates exactly that field so a strict run refuses on it alone.
var responsesEchoFields = []struct {
	feature Feature
	path    string
	apply   func(*ResponsesRequestEcho)
}{
	{
		feature: FeatureRequestTruncation,
		path:    "request.truncation",
		apply: func(echo *ResponsesRequestEcho) {
			truncation := "auto"
			echo.Truncation = &truncation
		},
	},
	{
		feature: FeatureRequestServiceTier,
		path:    "request.service_tier",
		apply: func(echo *ResponsesRequestEcho) {
			tier := "auto"
			echo.ServiceTier = &tier
		},
	},
	{
		feature: FeatureRequestTopLogprobs,
		path:    "request.top_logprobs",
		apply: func(echo *ResponsesRequestEcho) {
			top := int64(2)
			echo.TopLogprobs = &top
		},
	},
	{
		feature: FeatureRequestReasoning,
		path:    "request.reasoning.effort",
		apply: func(echo *ResponsesRequestEcho) {
			effort := "high"
			echo.Reasoning = &ResponsesEnvelopeReasoning{Effort: &effort}
		},
	},
	{
		feature: FeatureReasoningSummary,
		path:    "request.reasoning.summary",
		apply: func(echo *ResponsesRequestEcho) {
			summary := "auto"
			echo.Reasoning = &ResponsesEnvelopeReasoning{Summary: &summary}
		},
	},
}

// responsesEchoContext returns a context whose echo carries exactly the field
// selected by apply.
func responsesEchoContext(policy LossPolicy, apply func(*ResponsesRequestEcho)) *ExchangeContext {
	echo := &ResponsesRequestEcho{}
	apply(echo)
	return &ExchangeContext{
		IDs:                      NewExchangeIDs(),
		LossPolicy:               policy,
		OriginalResponsesRequest: echo,
	}
}

// responsesEchoRenderers maps each target that consumes a Responses source to
// its render entry point.
func responsesEchoRenderers() map[string]func(*ExchangeContext) (ConversionReport, error) {
	return map[string]func(*ExchangeContext) (ConversionReport, error){
		"chat": func(context *ExchangeContext) (ConversionReport, error) {
			_, report, err := RenderChatRequest(responsesEchoRequest(), context, ChatCapabilities{})
			return report, err
		},
		"gemini": func(context *ExchangeContext) (ConversionReport, error) {
			_, report, err := RenderGeminiRequest(responsesEchoRequest(), context)
			return report, err
		},
		"messages": func(context *ExchangeContext) (ConversionReport, error) {
			_, report, err := RenderMessagesRequest(responsesEchoRequest(), context)
			return report, err
		},
	}
}

// reportHasAt reports whether the report carries a loss for the feature at the
// given path.
func reportHasAt(report ConversionReport, feature Feature, path string) bool {
	for _, loss := range report.Losses {
		if loss.Feature == feature && loss.Path == path {
			return true
		}
	}
	return false
}

// TestResponsesEchoLossesAreTargetIndependent proves the Gemini and Messages
// renderers record the same envelope decisions the Chat renderer records: one
// loss per field, under the same key and path, and a strict policy refuses each
// field with that key named.
func TestResponsesEchoLossesAreTargetIndependent(t *testing.T) {
	renderers := responsesEchoRenderers()
	for name, render := range renderers {
		for _, field := range responsesEchoFields {
			t.Run(name+"/"+string(field.feature), func(t *testing.T) {
				permissive := LossPolicy{Allowed: map[Feature]struct{}{field.feature: {}}}
				report, err := render(responsesEchoContext(permissive, field.apply))
				if err != nil {
					t.Fatalf("permissive render: %v", err)
				}
				if !reportHasAt(report, field.feature, field.path) {
					t.Fatalf("report lacks %s at %s: %+v", field.feature, field.path, report.Losses)
				}

				_, err = render(responsesEchoContext(StrictLossPolicy(), field.apply))
				if err == nil {
					t.Fatalf("strict %s render accepted %s", name, field.feature)
				}
				if !strings.Contains(err.Error(), string(field.feature)) {
					t.Errorf("err = %v, want it to name %s", err, field.feature)
				}
			})
		}
	}
}

// fullResponsesEcho returns an echo populating every request-envelope field the
// pinned Responses create-request wire models.
func fullResponsesEcho() *ResponsesRequestEcho {
	previous := "resp_prev"
	store := false
	user := "user_1"
	truncation := "auto"
	tier := "flex"
	top := int64(3)
	effort := "high"
	summary := "auto"
	return &ResponsesRequestEcho{
		PreviousResponseID: &previous,
		Store:              &store,
		User:               &user,
		Truncation:         &truncation,
		ServiceTier:        &tier,
		TopLogprobs:        &top,
		Reasoning:          &ResponsesEnvelopeReasoning{Effort: &effort, Summary: &summary},
	}
}

// TestResponsesEchoLossesLeaveAResponsesTargetAlone proves the Responses target
// never charges itself for an envelope its own wire carries, and — the part a
// report-only assertion could not prove — actually RENDERS every echoed field.
// The direction is not constructible from configuration (Mapping.Validate
// admits no same-protocol direction), so this is the unit-level contract that
// keeps the renderer honest for the wire it targets.
func TestResponsesEchoLossesLeaveAResponsesTargetAlone(t *testing.T) {
	context := &ExchangeContext{
		IDs:                      NewExchangeIDs(),
		LossPolicy:               StrictLossPolicy(),
		OriginalResponsesRequest: fullResponsesEcho(),
	}
	rendered, report, err := RenderResponsesRequest(responsesEchoRequest(), context)
	if err != nil {
		t.Fatalf("responses render: %v", err)
	}
	for _, feature := range []Feature{
		FeatureRequestReasoning,
		FeatureReasoningSummary,
		FeaturePreviousResponseID,
		FeatureResponsesControls,
		FeatureRequestTruncation,
		FeatureRequestServiceTier,
		FeatureRequestTopLogprobs,
	} {
		if reportHasFeature(report, feature) {
			t.Fatalf("%s charged to the Responses target, which carries it: %+v", feature, report.Losses)
		}
	}

	var out openairesponses.Request
	if err := strictDecode(rendered, &out); err != nil {
		t.Fatalf("rendered responses: %v\n%s", err, rendered)
	}
	if out.PreviousResponseID == nil || *out.PreviousResponseID != "resp_prev" {
		t.Errorf("previous_response_id = %v, want the echo value", out.PreviousResponseID)
	}
	if out.Store == nil || *out.Store {
		t.Errorf("store = %v, want the echoed false", out.Store)
	}
	if out.User == nil || *out.User != "user_1" {
		t.Errorf("user = %v, want the echo value", out.User)
	}
	if out.Truncation == nil || *out.Truncation != "auto" {
		t.Errorf("truncation = %v, want the echo value", out.Truncation)
	}
	if out.ServiceTier == nil || *out.ServiceTier != "flex" {
		t.Errorf("service_tier = %v, want the echo value", out.ServiceTier)
	}
	if out.TopLogprobs == nil || *out.TopLogprobs != 3 {
		t.Errorf("top_logprobs = %v, want the echo value", out.TopLogprobs)
	}
	if out.Reasoning == nil {
		t.Fatalf("reasoning = nil, want the echoed effort and summary")
	}
	if out.Reasoning.Effort == nil || *out.Reasoning.Effort != "high" {
		t.Errorf("reasoning.effort = %v, want the echo value", out.Reasoning.Effort)
	}
	if out.Reasoning.Summary == nil || *out.Reasoning.Summary != "auto" {
		t.Errorf("reasoning.summary = %v, want the echo value", out.Reasoning.Summary)
	}
}

// TestResponsesEchoPreviousResponseIDIsNotForwardedDownstreamOfResolution proves
// a resolved chain is not continued a second time: the store already prepended
// the conversation, so forwarding the id would make the upstream append it
// again. The other echo fields are still carried.
func TestResponsesEchoPreviousResponseIDIsNotForwardedDownstreamOfResolution(t *testing.T) {
	context := &ExchangeContext{
		IDs:                      NewExchangeIDs(),
		LossPolicy:               StrictLossPolicy(),
		RequestDepth:             2,
		OriginalResponsesRequest: fullResponsesEcho(),
	}
	rendered, report, err := RenderResponsesRequest(responsesEchoRequest(), context)
	if err != nil {
		t.Fatalf("responses render: %v", err)
	}
	var out openairesponses.Request
	if err := strictDecode(rendered, &out); err != nil {
		t.Fatalf("rendered responses: %v\n%s", err, rendered)
	}
	if out.PreviousResponseID != nil {
		t.Errorf("previous_response_id = %v, want it omitted after the chain was resolved", *out.PreviousResponseID)
	}
	if out.User == nil || out.Store == nil || out.Truncation == nil || out.ServiceTier == nil ||
		out.TopLogprobs == nil || out.Reasoning == nil || out.Reasoning.Summary == nil {
		t.Errorf("a resolved id must not suppress the rest of the echo: %s", rendered)
	}
	if reportHasFeature(report, FeaturePreviousResponseID) {
		t.Errorf("a consumed previous_response_id is not a loss: %+v", report.Losses)
	}
}

// TestResponsesEchoStoreFalseIsSatisfiedOnAStatelessTarget proves the same
// principle the round applied to `strict:false` and to the parallel-tools value:
// an assertion the target already satisfies is not a loss. A stateless upstream
// keeps nothing, so `store:false` asks for exactly what happens anyway; only
// `store:true` is a real, refusable loss.
func TestResponsesEchoStoreFalseIsSatisfiedOnAStatelessTarget(t *testing.T) {
	store := false
	apply := func(echo *ResponsesRequestEcho) { echo.Store = &store }

	// Strict policy: nothing is approved, yet store:false must not refuse.
	for _, name := range []string{"gemini", "messages"} {
		render := responsesEchoRenderers()[name]
		report, err := render(responsesEchoContext(StrictLossPolicy(), apply))
		if err != nil {
			t.Fatalf("%s: store:false refused a stateless target: %v", name, err)
		}
		if reportHasFeature(report, FeatureResponsesControls) {
			t.Fatalf("%s: store:false recorded a loss on a stateless target: %+v", name, report.Losses)
		}
	}

	// store:true IS a real loss, so the same target refuses it under strict.
	storeTrue := true
	applyTrue := func(echo *ResponsesRequestEcho) { echo.Store = &storeTrue }
	for _, name := range []string{"gemini", "messages"} {
		render := responsesEchoRenderers()[name]
		if _, err := render(responsesEchoContext(StrictLossPolicy(), applyTrue)); err == nil {
			t.Fatalf("%s: store:true was accepted though the target keeps nothing", name)
		}
		report, err := render(responsesEchoContext(LossPolicy{
			Allowed: map[Feature]struct{}{FeatureResponsesControls: {}},
		}, applyTrue))
		if err != nil {
			t.Fatalf("%s approved store:true: %v", name, err)
		}
		if !reportHasAt(report, FeatureResponsesControls, "request.store") {
			t.Fatalf("%s: report = %+v, want the store decision", name, report.Losses)
		}
	}
}

// TestResponsesEchoUserAndStoreAreDecidedPerTarget proves the two envelope
// fields the shared helper used to omit are never dropped silently: Gemini and
// Messages cannot carry them, so both record the shared responses_controls
// decision, while Chat models both fields and records nothing.
func TestResponsesEchoUserAndStoreAreDecidedPerTarget(t *testing.T) {
	user := "user_1"
	// store:TRUE is the value that is a real loss on a target with no storage;
	// store:false is already satisfied there and is covered separately.
	store := true
	apply := func(echo *ResponsesRequestEcho) {
		echo.User = &user
		echo.Store = &store
	}
	permissive := LossPolicy{Allowed: map[Feature]struct{}{FeatureResponsesControls: {}}}
	for _, name := range []string{"gemini", "messages"} {
		render := responsesEchoRenderers()[name]
		report, err := render(responsesEchoContext(permissive, apply))
		if err != nil {
			t.Fatalf("%s permissive render: %v", name, err)
		}
		if !reportHasAt(report, FeatureResponsesControls, "request.user") {
			t.Errorf("%s report lacks the user decision: %+v", name, report.Losses)
		}
		if !reportHasAt(report, FeatureResponsesControls, "request.store") {
			t.Errorf("%s report lacks the store decision: %+v", name, report.Losses)
		}
		if _, err := render(responsesEchoContext(StrictLossPolicy(), apply)); err == nil {
			t.Errorf("%s strict render accepted user and store", name)
		}
	}

	// Chat renders both natively, so neither is a loss there.
	report, err := responsesEchoRenderers()["chat"](responsesEchoContext(StrictLossPolicy(), apply))
	if err != nil {
		t.Fatalf("chat render: %v", err)
	}
	if reportHasFeature(report, FeatureResponsesControls) {
		t.Errorf("chat charged user/store it carries: %+v", report.Losses)
	}
}
