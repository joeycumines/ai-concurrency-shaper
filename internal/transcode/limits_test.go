package transcode

import "testing"

// TestBodyLimitsValidate proves negative limits fail startup validation
// (review-j finding 14).
func TestBodyLimitsValidate(t *testing.T) {
	if err := (BodyLimits{}).Validate(); err != nil {
		t.Fatalf("zero limits must be valid: %v", err)
	}
	for name, limits := range map[string]BodyLimits{
		"accepted":     {AcceptedRequestBytes: -1},
		"decoded":      {DecodedRequestBytes: -1},
		"retry replay": {RetryReplayBytes: -1},
		"successful":   {SuccessfulResponseBytes: -1},
		"error":        {ErrorResponseBytes: -1},
		"sse line":     {SSELineBytes: -1},
		"sse frame":    {SSEFrameBytes: -1},
	} {
		if err := limits.Validate(); err == nil {
			t.Fatalf("%s: negative limit accepted", name)
		}
	}
}
