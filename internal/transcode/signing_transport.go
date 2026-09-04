package transcode

// Per-attempt external request signing (review-z commit 4): the signer is
// attached to the request context by ApplyTargetAuthentication and a signing
// transport inserted between the retry transport and the configured base
// transport signs EVERY actual attempt AFTER the retry layer rebuilt the
// body and finalized Content-Length. A signature is never reused across
// attempts and the original request is never mutated. A signer error is a
// local construction/auth failure (neutral), never an upstream failure.

import (
	"context"
	"fmt"
	"net/http"
)

// signerContextKey carries the request signer through the request context.
type signerContextKey struct{}

// WithRequestSigner attaches the signer to the request context.
func WithRequestSigner(ctx context.Context, signer RequestSigner) context.Context {
	return context.WithValue(ctx, signerContextKey{}, signer)
}

// RequestSignerFromContext returns the request's signer, or nil.
func RequestSignerFromContext(ctx context.Context) RequestSigner {
	if signer, _ := ctx.Value(signerContextKey{}).(RequestSigner); signer != nil {
		return signer
	}
	return nil
}

// SigningError is the typed error reported when per-attempt signing fails:
// a local construction/auth failure (neutral), never an upstream failure
// (review-z commit 4). The handler classifies it as a local error so the
// circuit breaker is never opened by a signer defect.
type SigningError struct {
	Cause error
}

func (e *SigningError) Error() string { return "signing: " + e.Cause.Error() }
func (e *SigningError) Unwrap() error { return e.Cause }

// IsNonRetryable marks signing failures as local non-retryable defects: the
// retry transport never retries them and never records them as breaker
// failures (review-z commit 4).
func (e *SigningError) IsNonRetryable() bool { return true }

// SigningTransport signs every outgoing request whose context carries a
// signer. It clones the incoming request, obtains a fresh body via GetBody
// (the retry layer's rebuilt body), signs the EXACT attempt, and sends the
// clone — the original request is never mutated and no signature is reused
// across attempts. Requests without a signer pass through untouched.
//
// Contract: body-carrying requests must supply GetBody (the retry transport
// does); a signer must not replace the request Body with a wrapper that
// cannot be re-fetched via GetBody.
type SigningTransport struct {
	Inner http.RoundTripper
}

// Unwrap exposes the inner transport so retry/breaker detection machinery
// can see through this transparent wrapper.
func (t *SigningTransport) Unwrap() http.RoundTripper {
	return t.Inner
}

// RoundTrip implements http.RoundTripper.
func (t *SigningTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	signer := RequestSignerFromContext(req.Context())
	if signer == nil {
		return t.Inner.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, &SigningError{Cause: fmt.Errorf("rebuild request body: %w", err)}
		}
		clone.Body = body
		clone.ContentLength = req.ContentLength
	}
	// Sign the exact attempt after body reconstruction and Content-Length
	// finalization. GetBody returns a fresh reader positioned at the start,
	// so the body is already rewound for the signed send; a fresh body is
	// fetched again when the signer consumed it (GetBody is re-invocable).
	if err := signer.Sign(req.Context(), clone); err != nil {
		return nil, &SigningError{Cause: err}
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, &SigningError{Cause: fmt.Errorf("rebuild request body: %w", err)}
		}
		clone.Body = body
	}
	return t.Inner.RoundTrip(clone)
}
