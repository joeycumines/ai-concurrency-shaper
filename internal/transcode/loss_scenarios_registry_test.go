package transcode

import (
	"slices"
	"testing"
)

// lossScenariosReachable aggregates the per-area loss-key reachability
// scenarios. The gemini_* keys are covered by loss_scenarios_gemini_test.go,
// which lands together with the Gemini conversion directions.
func lossScenariosReachable(t *testing.T) []lossScenario {
	return slices.Concat(
		lossScenariosRequestConversion(t),
		lossScenariosMessagesUsage(t),
		lossScenariosReasoning(t),
		lossScenariosRequestFidelity(t),
		lossScenariosResponseProvenance(t),
		lossScenariosStreamQuirks(t),
		lossScenariosImageMetadata(t),
		lossScenariosGemini(t),
		lossScenariosAnthropicToolChanges(t),
	)
}

// lossScenariosAnthropicToolChanges covers the mid-conversation tool-change
// key. A tool_definition is hoisted (a Note, so its scenario lists no
// permission); a withdrawal is the policy-gated loss, driven here so the key
// is reachable on a strict-refusing path and is recorded only by its own
// permission.
func lossScenariosAnthropicToolChanges(t *testing.T) []lossScenario {
	t.Helper()
	return []lossScenario{
		{
			key:  FeatureAnthropicToolChanges,
			perm: []Feature{FeatureAnthropicToolChanges},
			run: func(policy LossPolicy) (ConversionReport, error) {
				result, err := DecodeMessagesRequest([]byte(`{"model":"m","max_tokens":16,`+
					`"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],`+
					`"messages":[{"role":"user","content":"hi"},`+
					`{"role":"system","content":[{"type":"tool_removal",`+
					`"tool":{"type":"tool_reference","name":"get_weather"}}]},`+
					`{"role":"user","content":"go"}]}`), policy)
				return result.Report, err
			},
		},
	}
}
