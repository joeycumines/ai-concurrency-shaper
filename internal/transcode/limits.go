package transcode

import "fmt"

// Package defaults for the BodyLimits fields. A zero value in a programmatic
// BodyLimits selects the field's default; the effective limits are computed
// once at handler construction so zero never reaches handler logic (review-k
// finding 8). The decoded-request default carries headroom over the
// accepted-request default for decode amplification, matching the CLI ratio.
const (
	DefaultAcceptedRequestBytes    int64 = 32 << 20
	DefaultDecodedRequestBytes     int64 = 32 << 22
	DefaultRetryReplayBytes        int64 = 32 << 20
	DefaultSuccessfulResponseBytes int64 = 32 << 20
	DefaultErrorResponseBytes      int64 = 1 << 20
	DefaultSSELineBytes            int   = 1 << 20
	DefaultSSEFrameBytes           int   = 1 << 20
)

// BodyLimits holds the independent body limits of a transcoded route. The
// limits are deliberately separate: the accepted-request limit bounds inbound
// payloads, the retry-replay limit bounds what the retry transport may replay,
// and the buffered-response limit bounds non-streaming upstream JSON reads.
//
// RetryReplayBytes contract: a NONZERO declared value must equal the proxy's
// retry transport body cap (cfg.maxBodyBytes) with retries enabled, or the
// proxy fails construction naming the route and both values — a declared
// bound that silently differs from the actual replay cap is a configuration
// error (review-k finding 8). A zero value declares no bound: the transport
// cap governs replay eligibility and no equality check applies.
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

// WithDefaults returns the body limits with every zero field replaced by its
// package default, so a programmatic all-zero BodyLimits enforces real bounds
// instead of leaving some fields unlimited (review-k finding 8). The handler
// applies this normalization once at construction; the handler-side per-use
// fallbacks remain as defense-in-depth. The proxy's RetryReplayBytes
// fail-fast check reads the raw declared value, not the defaulted one.
func (l BodyLimits) WithDefaults() BodyLimits {
	if l.AcceptedRequestBytes <= 0 {
		l.AcceptedRequestBytes = DefaultAcceptedRequestBytes
	}
	if l.DecodedRequestBytes <= 0 {
		l.DecodedRequestBytes = DefaultDecodedRequestBytes
	}
	if l.RetryReplayBytes <= 0 {
		l.RetryReplayBytes = DefaultRetryReplayBytes
	}
	if l.SuccessfulResponseBytes <= 0 {
		l.SuccessfulResponseBytes = DefaultSuccessfulResponseBytes
	}
	if l.ErrorResponseBytes <= 0 {
		l.ErrorResponseBytes = DefaultErrorResponseBytes
	}
	if l.SSELineBytes <= 0 {
		l.SSELineBytes = DefaultSSELineBytes
	}
	if l.SSEFrameBytes <= 0 {
		l.SSEFrameBytes = DefaultSSEFrameBytes
	}
	return l
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
