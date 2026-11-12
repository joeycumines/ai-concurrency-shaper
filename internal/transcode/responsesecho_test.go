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
		path:    "reasoning.effort",
		apply: func(echo *ResponsesRequestEcho) {
			effort := "high"
			echo.Reasoning = &ResponsesEnvelopeReasoning{Effort: &effort}
		},
	},
	{
		feature: FeatureReasoningSummary,
		path:    "reasoning.summary",
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

// TestResponsesEchoLossesLeaveAResponsesTargetAlone proves the Responses target
// never charges itself for a reasoning configuration its own wire carries, so a
// future shared call there must pass effortPortable=true and must not charge the
// summary style either.
func TestResponsesEchoLossesLeaveAResponsesTargetAlone(t *testing.T) {
	effort := "high"
	summary := "auto"
	context := &ExchangeContext{
		IDs:        NewExchangeIDs(),
		LossPolicy: StrictLossPolicy(),
		OriginalResponsesRequest: &ResponsesRequestEcho{
			Reasoning: &ResponsesEnvelopeReasoning{Effort: &effort, Summary: &summary},
		},
	}
	_, report, err := RenderResponsesRequest(responsesEchoRequest(), context)
	if err != nil {
		t.Fatalf("responses render: %v", err)
	}
	for _, feature := range []Feature{FeatureRequestReasoning, FeatureReasoningSummary} {
		if reportHasFeature(report, feature) {
			t.Fatalf("%s charged to the Responses target, which carries it: %+v", feature, report.Losses)
		}
	}
}
