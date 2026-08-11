package transcode

import "fmt"

// maxStreamAccumulatedBytes bounds the cumulative semantic state a stream
// may accumulate per item or part (text, refusal, tool-call arguments)
// before the exchange is rejected as corrupt upstream wire: individually
// bounded SSE frames could otherwise accumulate without limit and be emitted
// as one generated downstream frame, amplifying memory without bound
// (review-k finding 9). Aligned with the default SSE frame bound.
const maxStreamAccumulatedBytes = 1 << 20

// Exchange-level stream budgets (review-08 blocker 7): the per-item/part
// bounds above limit a single accumulator; these budgets bound the whole
// exchange so a corrupt upstream cannot grow memory without limit across
// many items, parts, tool calls, or report entries, and cannot amplify the
// generated downstream output (JSON escaping, terminal repetition) without
// bound. Every violation terminates the exchange as corrupt upstream wire.
const (
	// maxStreamTotalAccumulatedBytes bounds the sum of all accumulated
	// semantic bytes (text, refusal, tool arguments) across every item,
	// part, and tool call of one exchange.
	maxStreamTotalAccumulatedBytes = 4 << 20
	// maxStreamOutputItems bounds the output items one exchange may open.
	maxStreamOutputItems = 4096
	// maxStreamPartsPerItem bounds the content parts one output item may
	// open.
	maxStreamPartsPerItem = 4096
	// maxStreamToolCalls bounds the tool calls one exchange may open.
	maxStreamToolCalls = 4096
	// maxStreamTerminalBatchBytes bounds the terminal batch buffered in the
	// converting reader: the released item-closing events and the terminal
	// envelope repeat the accumulated content several times, and JSON
	// escaping can amplify each copy.
	maxStreamTerminalBatchBytes = 32 << 20
	// maxStreamConversionReportEntries bounds the accumulated losses and
	// notes of one exchange.
	maxStreamConversionReportEntries = 4096
	// maxStreamErrorTextBytes bounds the error text embedded in a
	// client-dialect error frame; longer messages are truncated.
	maxStreamErrorTextBytes = 4 << 10
)

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
	// DefaultGeneratedResponseBytes bounds the complete rendered non-stream
	// JSON response body, applied after conversion and BEFORE headers
	// commit (review-z commit 3).
	DefaultGeneratedResponseBytes int64 = 32 << 20
	// DefaultErrorMessageBytes bounds every client-visible error message
	// (stream error frames and dialect error bodies).
	DefaultErrorMessageBytes int = 4 << 10
	// DefaultGeneratedSSEFrameBytes bounds one generated downstream SSE
	// frame (the outbound counterpart of SSEFrameBytes).
	DefaultGeneratedSSEFrameBytes int = 1 << 20
	// DefaultGeneratedSSEBatchBytes bounds one generated terminal batch
	// (the released item-closing events plus the terminal envelope).
	DefaultGeneratedSSEBatchBytes int = 32 << 20
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
	// Output-side limits (review-z commit 3). SSELineBytes and SSEFrameBytes
	// bound the INBOUND upstream stream; the Generated* fields bound what the
	// transcoder emits.
	GeneratedResponseBytes int64
	ErrorMessageBytes      int
	GeneratedSSEFrameBytes int
	GeneratedSSEBatchBytes int
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
	if l.GeneratedResponseBytes <= 0 {
		l.GeneratedResponseBytes = DefaultGeneratedResponseBytes
	}
	if l.ErrorMessageBytes <= 0 {
		l.ErrorMessageBytes = DefaultErrorMessageBytes
	}
	if l.GeneratedSSEFrameBytes <= 0 {
		l.GeneratedSSEFrameBytes = DefaultGeneratedSSEFrameBytes
	}
	if l.GeneratedSSEBatchBytes <= 0 {
		l.GeneratedSSEBatchBytes = DefaultGeneratedSSEBatchBytes
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
	if l.GeneratedResponseBytes < 0 {
		return fmt.Errorf("GeneratedResponseBytes must be nonnegative, got %d", l.GeneratedResponseBytes)
	}
	if l.ErrorMessageBytes < 0 {
		return fmt.Errorf("ErrorMessageBytes must be nonnegative, got %d", l.ErrorMessageBytes)
	}
	if l.GeneratedSSEFrameBytes < 0 {
		return fmt.Errorf("GeneratedSSEFrameBytes must be nonnegative, got %d", l.GeneratedSSEFrameBytes)
	}
	if l.GeneratedSSEBatchBytes < 0 {
		return fmt.Errorf("GeneratedSSEBatchBytes must be nonnegative, got %d", l.GeneratedSSEBatchBytes)
	}
	return nil
}
