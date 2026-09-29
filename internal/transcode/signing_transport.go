package transcode

// Per-attempt external request signing: the signer is
// attached to the request context by ApplyTargetAuthentication and a signing
// transport inserted between the retry transport and the configured base
// transport signs EVERY actual attempt AFTER the retry layer rebuilt the
// body and finalized Content-Length. A signature is never reused across
// attempts and the original request is never mutated. A signer error is a
// local construction/auth failure (neutral), never an upstream failure.

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// The handler classifies it as a local error so the
// circuit breaker is never opened by a signer defect.
type SigningError struct {
	Cause error
}

func (e *SigningError) Error() string { return "signing: " + e.Cause.Error() }
func (e *SigningError) Unwrap() error { return e.Cause }

// IsNonRetryable marks signing failures as local non-retryable defects: the
// retry transport never retries them and never records them as breaker
// failures.
func (e *SigningError) IsNonRetryable() bool { return true }

// SigningTransport signs every outgoing request whose context carries a
// signer. It clones the incoming request, obtains a fresh body via GetBody
// (the retry layer's rebuilt body), signs the EXACT attempt, and sends the
// clone — the original request is never mutated and no signature is reused
// across attempts. Requests without a signer pass through untouched.
//
// Contract: body-carrying requests must supply GetBody (the retry transport
// does); a signer must not replace the request Body with a wrapper that
// cannot be re-fetched via GetBody. A request that breaks the contract is
// refused as a local, non-retryable SigningError rather than sent truncated —
// see errSignerConsumedUnrebuildableBody. That refusal watches the body this
// transport installed, so it holds for a signer that leaves the body it was
// given in place; a signer that substitutes a body of its own is taken at its
// word and whatever it installed is what gets sent, re-fetched through GetBody
// when the request is rebuildable.
type SigningTransport struct {
	Inner http.RoundTripper
}

// Unwrap exposes the inner transport so retry/breaker detection machinery
// can see through this transparent wrapper.
func (t *SigningTransport) Unwrap() http.RoundTripper {
	return t.Inner
}

// errSignerConsumedUnrebuildableBody is returned when a signer consumes the
// only body of a request that supplies no GetBody. The attempt cannot be
// rebuilt, and sending the spent reader would put a truncated request
// upstream that reads as a successful exchange — the one outcome this
// transport must never produce. A local, non-retryable failure is honest;
// a short body is not.
var errSignerConsumedUnrebuildableBody = errors.New(
	"signer consumed the request body but the request supplies no GetBody to rebuild it")

// signerAttemptBody records whether the signer consumed the body it was
// given. The flag is written while Sign runs and read immediately after, both
// on the RoundTrip goroutine, before the inner transport is handed anything —
// so it is never touched concurrently.
type signerAttemptBody struct {
	io.ReadCloser
	consumed bool
}

func (b *signerAttemptBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.consumed = true
	}
	return n, err
}

// Close counts as consumption: a signer that closed the body left nothing to
// send, whatever it managed to read first.
func (b *signerAttemptBody) Close() error {
	b.consumed = true
	return b.ReadCloser.Close()
}

// RoundTrip implements http.RoundTripper.
func (t *SigningTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	signer := RequestSignerFromContext(req.Context())
	if signer == nil {
		return t.Inner.RoundTrip(req)
	}
	// discardAttempt closes everything this attempt owns and reports the
	// local, non-retryable failure. The RoundTripper contract requires the
	// request body to be closed on every return, errors included, and a body
	// fetched from GetBody for an attempt that never reaches a transport must
	// not be leaked. discarded is the body this attempt built for itself, or
	// nil when it never built one.
	discardAttempt := func(discarded io.ReadCloser, cause error) error {
		if discarded != nil {
			_ = discarded.Close()
		}
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return &SigningError{Cause: cause}
	}

	clone := req.Clone(req.Context())
	// rebuilt is the body fetched for signing, if any; it is the attempt's own
	// and the inner transport never sees it.
	var rebuilt io.ReadCloser
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, discardAttempt(nil, fmt.Errorf("rebuild request body: %w", err))
		}
		rebuilt = body
		clone.Body = body
		clone.ContentLength = req.ContentLength
	}
	// A request with no GetBody has exactly one body, and the signer runs
	// before the send, so what the signer does to that body decides whether an
	// attempt is still sendable at all. Nothing else can tell, so watch it.
	var attempt *signerAttemptBody
	if req.GetBody == nil && clone.Body != nil {
		attempt = &signerAttemptBody{ReadCloser: clone.Body}
		clone.Body = attempt
	}
	// Sign the exact attempt after body reconstruction and Content-Length
	// finalization. GetBody returns a fresh reader positioned at the start, so
	// the body is already rewound for the signed send; a fresh body is fetched
	// again when the signer consumed it (GetBody is re-invocable).
	signErr := signer.Sign(req.Context(), clone)
	// A body the SIGNER installed is this transport's to clean up whenever the
	// attempt is abandoned or the body is replaced: nothing else holds a
	// reference to it, so nothing else will close it. On the send path it is
	// what goes on the wire, and the transport owns it from there.
	var substituted io.ReadCloser
	if clone.Body != rebuilt && !(attempt != nil && clone.Body == attempt) {
		substituted = clone.Body
	}
	closeSubstituted := func() {
		if substituted != nil {
			_ = substituted.Close()
		}
	}
	if signErr != nil {
		closeSubstituted()
		return nil, discardAttempt(rebuilt, signErr)
	}
	// Only the body the signer left in place is this transport's to judge. A
	// signer that put its own body on the request keeps it, exactly as the
	// rebuildable path keeps rebuilding over it: what it signed is what it
	// wants sent.
	if wrapper, ok := clone.Body.(*signerAttemptBody); attempt != nil && ok && wrapper == attempt {
		if attempt.consumed {
			return nil, discardAttempt(nil, errSignerConsumedUnrebuildableBody)
		}
		// Unconsumed, so the caller's own body goes on the wire and the
		// tracking wrapper stays out of the request entirely.
		clone.Body = attempt.ReadCloser
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			closeSubstituted()
			return nil, discardAttempt(rebuilt, fmt.Errorf("rebuild request body: %w", err))
		}
		if rebuilt != nil {
			_ = rebuilt.Close()
		}
		// Being replaced, so it is this transport's to close.
		closeSubstituted()
		clone.Body = body
	}
	return t.Inner.RoundTrip(clone)
}
