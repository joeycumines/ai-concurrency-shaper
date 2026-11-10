package geminigeneratecontent

import (
	"encoding/json"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// ErrorBody is the Google error payload, shared by an HTTP error envelope
// and the in-stream error frame.
type ErrorBody struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Status  string          `json:"status"`
	Details json.RawMessage `json:"details,omitempty"`
}

// ErrorEnvelope is the HTTP error body returned by the Gemini API.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// DecodeErrorEnvelope tolerantly decodes a Google error envelope so unknown
// provider extensions never hide the error itself. A body whose error member
// is absent or null is a missing-required rejection, not a zero ErrorBody.
func DecodeErrorEnvelope(body []byte) (ErrorEnvelope, error) {
	var envelope struct {
		Error *ErrorBody `json:"error"`
	}
	if err := wire.DecodeTolerant(body, &envelope); err != nil {
		return ErrorEnvelope{}, err
	}
	if envelope.Error == nil {
		return ErrorEnvelope{}, &wire.DecodeError{
			Kind:    wire.DecodeMissingRequired,
			Path:    "error",
			Message: "error envelope has no error object",
		}
	}
	return ErrorEnvelope{Error: *envelope.Error}, nil
}
