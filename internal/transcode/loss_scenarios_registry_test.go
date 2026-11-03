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
	)
}
