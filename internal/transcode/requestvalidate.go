package transcode

// Request-side IR validation. It is separate from the IR declarations because
// it is the single gate every request renderer must pass, and the rules it
// enforces are product contracts rather than type shape: a role may not carry a
// part its dialect never places there, a retained-turn count must be a real
// prefix, the thinking union must be one of its three arms, and thinking may
// only cross the boundary as proxy-synthesized reasoning.

import "fmt"

// ValidateCanonicalRequest checks the IR invariants before rendering:
// every turn role is one of the canonical values, every turn has content,
// every part is non-nil, RetainedTurns is a prefix of the turn list, tool calls
// carry non-empty identity (item/call id and name) with object-shaped
// arguments, and tool results carry a non-empty call id.
func ValidateCanonicalRequest(request CanonicalRequest) error {
	// RetainedTurns decides which turns were reconstructed by the proxy rather
	// than authored by the client, so an out-of-range value would silently
	// reclassify client input as proxy history (or exempt nothing at all).
	if request.RetainedTurns < 0 {
		return fmt.Errorf("retained turn count %d is negative", request.RetainedTurns)
	}
	if request.RetainedTurns > len(request.Turns) {
		return fmt.Errorf(
			"retained turn count %d exceeds the %d request turns",
			request.RetainedTurns,
			len(request.Turns),
		)
	}
	// The thinking union is validated here for the same reason RetainedTurns is:
	// decode enforces the Anthropic contract, so a violation can only arrive on a
	// hand-built CanonicalRequest. Without this check a bogus type fell through
	// both Chat's and Responses' switch silently, and a display on a non-adaptive
	// type rendered an invalid Messages union beside budget_tokens. enabled
	// WITHOUT a budget stays valid — the renderers map it to a named Note.
	if thinking := request.Thinking; thinking != nil {
		switch thinking.Type {
		case "enabled", "disabled", "adaptive":
		default:
			return fmt.Errorf(
				"thinking type %q is invalid (want enabled, disabled, or adaptive)",
				thinking.Type,
			)
		}
		if thinking.Type != "adaptive" && thinking.Display != "" {
			return fmt.Errorf(
				"thinking type %s cannot carry display (it belongs to adaptive)",
				thinking.Type,
			)
		}
		if thinking.Type != "enabled" && thinking.BudgetTokens != nil {
			return fmt.Errorf(
				"thinking type %s cannot carry budget_tokens (it belongs to enabled)",
				thinking.Type,
			)
		}
		// A budget that is present must be usable. The Messages decoder
		// rejects a non-positive one, and the Messages renderer copies it
		// straight onto the wire, so a hand-built request would otherwise
		// render a budget_tokens the upstream refuses.
		if thinking.BudgetTokens != nil && *thinking.BudgetTokens <= 0 {
			return fmt.Errorf(
				"thinking budget_tokens must be positive, got %d",
				*thinking.BudgetTokens,
			)
		}
	}
	for turnIndex, turn := range request.Turns {
		switch turn.Role {
		case CanonicalUser, CanonicalAssistant, CanonicalSystem, CanonicalDeveloper:
		default:
			return fmt.Errorf("turn %d has invalid role %q", turnIndex, turn.Role)
		}
		if len(turn.Parts) == 0 {
			return fmt.Errorf("turn %d has no content parts", turnIndex)
		}
		for partIndex, part := range turn.Parts {
			if part == nil {
				return fmt.Errorf(
					"turn %d part %d is nil",
					turnIndex,
					partIndex,
				)
			}
			// Central role semantics: function calls only
			// in assistant turns, function results only in user turns, and
			// refusal only on assistant messages — enforced once here, not
			// per-renderer.
			switch part.(type) {
			case CanonicalFunctionCall:
				if turn.Role != CanonicalAssistant {
					return fmt.Errorf(
						"turn %d part %d: function call in %q turn",
						turnIndex,
						partIndex,
						turn.Role,
					)
				}
			case CanonicalFunctionResult:
				if turn.Role != CanonicalUser {
					return fmt.Errorf(
						"turn %d part %d: function result in %q turn",
						turnIndex,
						partIndex,
						turn.Role,
					)
				}
			case CanonicalRefusal:
				if turn.Role != CanonicalAssistant {
					return fmt.Errorf(
						"turn %d part %d: refusal in %q turn",
						turnIndex,
						partIndex,
						turn.Role,
					)
				}
			}
			if err := validateCanonicalPart(part); err != nil {
				return fmt.Errorf("turn %d part %d: %w", turnIndex, partIndex, err)
			}
		}
	}
	return nil
}
