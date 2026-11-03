package transcode

import (
	"net/http"
	"slices"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/geminigeneratecontent"
)

// googleRPCStatuses are the google.rpc status strings the Gemini API reports.
// A canonical error code already in this list is passed through unchanged;
// any other code is replaced by the status the HTTP code maps to.
var googleRPCStatuses = []string{
	"CANCELLED",
	"UNKNOWN",
	"INVALID_ARGUMENT",
	"DEADLINE_EXCEEDED",
	"NOT_FOUND",
	"ALREADY_EXISTS",
	"PERMISSION_DENIED",
	"RESOURCE_EXHAUSTED",
	"FAILED_PRECONDITION",
	"ABORTED",
	"OUT_OF_RANGE",
	"UNIMPLEMENTED",
	"INTERNAL",
	"UNAVAILABLE",
	"DATA_LOSS",
	"UNAUTHENTICATED",
}

// googleStatusForHTTP maps an HTTP status to the google.rpc status string the
// Gemini API reports for it.
func googleStatusForHTTP(status int) string {
	switch status {
	case 400, 413, 415:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 408, 504:
		return "DEADLINE_EXCEEDED"
	case 409:
		return "ABORTED"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 499:
		return "CANCELLED"
	case 501:
		return "UNIMPLEMENTED"
	case 502, 503:
		return "UNAVAILABLE"
	}
	if status >= 400 && status < 500 {
		return "FAILED_PRECONDITION"
	}
	return "INTERNAL"
}

// geminiUpstreamMessage chooses the client-facing message for a decodable
// upstream Google error. The envelope message is used when present; otherwise
// a recognized google.rpc status names the failure, and otherwise the HTTP
// status text does. The details member is never used.
func geminiUpstreamMessage(httpStatus int, body geminigeneratecontent.ErrorBody) string {
	if body.Message != "" {
		return body.Message
	}
	if slices.Contains(googleRPCStatuses, body.Status) {
		return body.Status
	}
	if text := http.StatusText(httpStatus); text != "" {
		return text
	}
	return "upstream returned an error without a message"
}

// geminiClientError renders a canonical error as the Google error envelope:
// {"error":{"code":<http status>,"message":...,"status":...}}.
func geminiClientError(apiErr CanonicalAPIError) geminigeneratecontent.ErrorEnvelope {
	status := googleStatusForHTTP(apiErr.Status)
	if slices.Contains(googleRPCStatuses, apiErr.Code) {
		status = apiErr.Code
	}
	return geminigeneratecontent.ErrorEnvelope{
		Error: geminigeneratecontent.ErrorBody{
			Code:    apiErr.Status,
			Message: apiErr.Message,
			Status:  status,
		},
	}
}
