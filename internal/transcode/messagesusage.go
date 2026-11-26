package transcode

import "github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"

// validateMessagesUsagePresence checks required totals before the value-based
// Usage wire struct can make an omitted count look like a measured zero. A
// response or message_start requires both totals; message_delta requires only
// output_tokens. An absent usage object retains the existing unknown-usage
// policy. The caller has already validated the complete wire shape, including
// illegal nulls and types; this probe adds only the per-event presence rules.
//
// https://github.com/anthropics/anthropic-sdk-python/blob/main/src/anthropic/types/usage.py
// https://github.com/anthropics/anthropic-sdk-python/blob/main/src/anthropic/types/message_delta_usage.py
func validateMessagesUsagePresence(envelope []byte, path string, requireInput bool) error {
	var probe struct {
		Usage *struct {
			InputTokens  *int `json:"input_tokens"`
			OutputTokens *int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := wire.DecodeTolerant(envelope, &probe); err != nil {
		return err
	}
	if probe.Usage == nil {
		return nil
	}
	missing := ""
	if requireInput && probe.Usage.InputTokens == nil {
		missing = "input_tokens"
	} else if probe.Usage.OutputTokens == nil {
		missing = "output_tokens"
	}
	if missing != "" {
		return &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    path + "." + missing,
			Message: "anthropic usage is missing " + missing,
		}
	}
	return nil
}
