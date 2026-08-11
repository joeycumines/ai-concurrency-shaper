package transcode

import "testing"

// TestBodyLimitsValidate proves negative limits fail startup validation
// (review-j finding 14).
func TestBodyLimitsValidate(t *testing.T) {
	if err := (BodyLimits{}).Validate(); err != nil {
		t.Fatalf("zero limits must be valid: %v", err)
	}
	for name, limits := range map[string]BodyLimits{
		"accepted":            {AcceptedRequestBytes: -1},
		"decoded":             {DecodedRequestBytes: -1},
		"retry replay":        {RetryReplayBytes: -1},
		"successful":          {SuccessfulResponseBytes: -1},
		"error":               {ErrorResponseBytes: -1},
		"sse line":            {SSELineBytes: -1},
		"sse frame":           {SSEFrameBytes: -1},
		"generated response":  {GeneratedResponseBytes: -1},
		"error message":       {ErrorMessageBytes: -1},
		"generated sse frame": {GeneratedSSEFrameBytes: -1},
		"generated sse batch": {GeneratedSSEBatchBytes: -1},
	} {
		if err := limits.Validate(); err == nil {
			t.Fatalf("%s: negative limit accepted", name)
		}
	}
}

// TestBodyLimitsWithDefaults proves every zero field selects its package
// default and explicit values are preserved (review-k finding 8): an
// all-zero BodyLimits enforces real bounds on every field, never unlimited.
func TestBodyLimitsWithDefaults(t *testing.T) {
	effective := (BodyLimits{}).WithDefaults()
	want := BodyLimits{
		AcceptedRequestBytes:    DefaultAcceptedRequestBytes,
		DecodedRequestBytes:     DefaultDecodedRequestBytes,
		RetryReplayBytes:        DefaultRetryReplayBytes,
		SuccessfulResponseBytes: DefaultSuccessfulResponseBytes,
		ErrorResponseBytes:      DefaultErrorResponseBytes,
		SSELineBytes:            DefaultSSELineBytes,
		SSEFrameBytes:           DefaultSSEFrameBytes,
		GeneratedResponseBytes:  DefaultGeneratedResponseBytes,
		ErrorMessageBytes:       DefaultErrorMessageBytes,
		GeneratedSSEFrameBytes:  DefaultGeneratedSSEFrameBytes,
		GeneratedSSEBatchBytes:  DefaultGeneratedSSEBatchBytes,
	}
	if effective != want {
		t.Fatalf("effective = %+v, want %+v", effective, want)
	}

	explicit := BodyLimits{
		AcceptedRequestBytes:    1,
		DecodedRequestBytes:     2,
		RetryReplayBytes:        3,
		SuccessfulResponseBytes: 4,
		ErrorResponseBytes:      5,
		SSELineBytes:            6,
		SSEFrameBytes:           7,
	}.WithDefaults()
	if explicit != (BodyLimits{
		AcceptedRequestBytes:    1,
		DecodedRequestBytes:     2,
		RetryReplayBytes:        3,
		SuccessfulResponseBytes: 4,
		ErrorResponseBytes:      5,
		SSELineBytes:            6,
		SSEFrameBytes:           7,
		GeneratedResponseBytes:  DefaultGeneratedResponseBytes,
		ErrorMessageBytes:       DefaultErrorMessageBytes,
		GeneratedSSEFrameBytes:  DefaultGeneratedSSEFrameBytes,
		GeneratedSSEBatchBytes:  DefaultGeneratedSSEBatchBytes,
	}) {
		t.Fatalf("explicit values changed: %+v", explicit)
	}

	// Per-field defaulting: only the zero fields are replaced.
	partial := (BodyLimits{DecodedRequestBytes: 42}).WithDefaults()
	if partial.DecodedRequestBytes != 42 ||
		partial.AcceptedRequestBytes != DefaultAcceptedRequestBytes {
		t.Fatalf("partial = %+v", partial)
	}
}
