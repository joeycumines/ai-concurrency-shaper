package transcode

// Acceptance tests: every granular loss key is reachable
// (a real conversion records it), rejected under strict policy, and allowed
// only by its own permission; invalid model-generated tool arguments convert
// byte-exact to Chat and Responses, produce a client-dialect unrepresentable
// error to Messages, and never contribute to upstream-failure accounting.

import (
	"slices"
	"strings"
	"testing"
)

// legacyLossNames are the REMOVED broad permission names: none may be
// accepted anywhere.
// legacyLossNames are the REMOVED broad permission names: none may be
// accepted anywhere. Names that
// survived as granular keys in their own right (image_input, top_k, ...) are
// not legacy and remain accepted.
var legacyLossNames = []string{
	"conversation_state", "provider_reasoning", "service_tier",
	"usage_timing", "tool_result_error", "phase", "reasoning_summary_request",
}

// TestParseLossFeaturesRejectsLegacyNames proves the removed broad names are
// rejected by the CLI parser (no deprecated aliases, no expansion).
func TestParseLossFeaturesRejectsLegacyNames(t *testing.T) {
	for _, name := range legacyLossNames {
		if _, err := ParseLossFeatures(name); err == nil {
			t.Fatalf("legacy loss name %q accepted", name)
		}
	}
}

type lossScenario struct {
	key  Feature
	perm []Feature // permissions the scenario's render path needs
	run  func(policy LossPolicy) (ConversionReport, error)
	// note marks a Note-recorded key (a sanctioned encoding, not a
	// policy decision): the strict-policy rejection assertion is
	// skipped, since Notes record under every policy.
	note bool
}

// TestLossKeysReachableAndStrictRejected proves every registered loss key is
// reachable: a concrete conversion records it under a permissive policy, and
// the same conversion is rejected under the strict policy. The scenario
// builders return the report that must carry the key.
func TestLossKeysReachableAndStrictRejected(t *testing.T) {
	permissive := func(keys ...Feature) LossPolicy {
		allowed := map[Feature]struct{}{}
		for _, key := range keys {
			allowed[key] = struct{}{}
		}
		return LossPolicy{Allowed: allowed}
	}
	strict := StrictLossPolicy()

	scenarios := lossScenariosReachable(t)
	// The scenario matrix must cover every registered key exactly once.
	registered := allLossKeys()
	// The gemini_* keys are exercised by loss_scenarios_gemini_test.go, which
	// lands with the Gemini conversion directions; skip them here until then.
	for key := range registered {
		if strings.HasPrefix(string(key), "gemini_") {
			delete(registered, key)
		}
	}
	seen := map[Feature]bool{}
	for _, s := range scenarios {
		if seen[s.key] {
			t.Fatalf("scenario for %q registered twice", s.key)
		}
		seen[s.key] = true
		delete(registered, s.key)
	}
	for key := range registered {
		t.Fatalf("no reachability scenario for registered key %q", key)
	}

	for _, s := range scenarios {
		t.Run(string(s.key), func(t *testing.T) {
			// The perm list is what makes the own-permission run below a
			// Note-vs-loss discriminator, so its shape is part of the contract:
			// a GATED loss must be granted by its own perm list (that is what
			// makes strict reject and this run accept), and a NOTE must NOT be
			// (a note consults no policy, so listing its key there would hide a
			// misclassification - the scenario would pass as a loss and as a
			// note alike). A note's perm list holds only the SIBLING losses the
			// exchange separately needs.
			grantsSelf := slices.Contains(s.perm, s.key)
			if s.note && grantsSelf {
				t.Fatalf(
					"%q is a note but its perm list grants the key itself; the "+
						"own-permission run can no longer distinguish a note from a loss",
					s.key,
				)
			}
			if !s.note && !grantsSelf {
				t.Fatalf(
					"%q is a gated loss but its perm list does not grant the key; "+
						"the own-permission run would reject for the wrong reason",
					s.key,
				)
			}
			// Notes are sanctioned encodings, not policy decisions: they record
			// under every policy including strict. Gated losses must reject
			// under strict.
			if !s.note {
				if _, err := s.run(strict); err == nil {
					t.Fatal("strict policy accepted the loss")
				}
			}
			// A policy allowing exactly this scenario's own permissions must
			// complete the scenario and record the key. For a note that policy
			// does NOT name the key, so this run is also the proof that the key
			// was recorded without any policy decision.
			other := permissive(s.perm...)
			report, err := s.run(other)
			if err != nil || !reportHasFeature(report, s.key) {
				t.Fatalf(
					"own-permission run: err = %v, report lacks %q: %+v",
					err,
					s.key,
					report,
				)
			}
			// A policy allowing everything succeeds and records the key.
			all := permissive(RegisteredLossKeys()...)
			report, err = s.run(all)
			if err != nil {
				t.Fatalf("permissive run failed: %v", err)
			}
			if !reportHasFeature(report, s.key) {
				t.Fatalf("report lacks %q: %+v", s.key, report)
			}
		})
	}
}
