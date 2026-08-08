package transcode

import "fmt"

// BodyLimits holds the independent body limits of a transcoded route. The
// limits are deliberately separate: the accepted-request limit bounds inbound
// payloads, the retry-replay limit bounds what the retry transport may replay,
// and the buffered-response limit bounds non-streaming upstream JSON reads.
//
// https://platform.claude.com/docs/en/api/overview
type BodyLimits struct {
	AcceptedRequestBytes int64
	DecodedRequestBytes  int64
	// RetryReplayBytes bounds the request bodies the proxy's retry transport
	// may buffer for replay. The handler does not enforce it: the retry
	// transport's own MaxBodyBytes (configured from the same value) governs
	// replay eligibility, keeping the replay cap separate from the
	// accepted-request cap that gates inbound payloads.
	RetryReplayBytes        int64
	SuccessfulResponseBytes int64
	ErrorResponseBytes      int64
	SSELineBytes            int
	SSEFrameBytes           int
}

// Validate checks the body limits are usable: every byte limit and the SSE
// line/frame bounds are nonnegative (review-j finding 14). Zero values mean
// "use the package default" and are valid.
func (l BodyLimits) Validate() error {
	for name, value := range map[string]int64{
		"AcceptedRequestBytes":    l.AcceptedRequestBytes,
		"DecodedRequestBytes":     l.DecodedRequestBytes,
		"RetryReplayBytes":        l.RetryReplayBytes,
		"SuccessfulResponseBytes": l.SuccessfulResponseBytes,
		"ErrorResponseBytes":      l.ErrorResponseBytes,
	} {
		if value < 0 {
			return fmt.Errorf("%s must be nonnegative, got %d", name, value)
		}
	}
	if l.SSELineBytes < 0 {
		return fmt.Errorf("SSELineBytes must be nonnegative, got %d", l.SSELineBytes)
	}
	if l.SSEFrameBytes < 0 {
		return fmt.Errorf("SSEFrameBytes must be nonnegative, got %d", l.SSEFrameBytes)
	}
	return nil
}
