// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package transcode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// bodyConsumingSigner is a signer that must see the body to sign it, the way a
// payload-hashing signature scheme does.
type bodyConsumingSigner struct{ calls int }

func (s *bodyConsumingSigner) Sign(_ context.Context, req *http.Request) error {
	s.calls++
	if req.Body != nil {
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return err
		}
	}
	return nil
}

// headerOnlySigner signs headers alone and never reads the body.
type headerOnlySigner struct{ calls int }

func (s *headerOnlySigner) Sign(_ context.Context, req *http.Request) error {
	s.calls++
	req.Header.Set("X-Signed", "yes")
	return nil
}

// recordingTransport captures the body the inner transport is finally given,
// as both the bytes and the object itself: a wrapper that leaked onto the
// wire would carry the same bytes as the caller's body, so only identity can
// tell the two apart.
type recordingTransport struct {
	calls   int
	sent    string
	gotBody io.ReadCloser
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	t.gotBody = req.Body
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		t.sent = string(body)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     http.Header{},
	}, nil
}

// closeSpy is a body that records whether it was closed, so the RoundTripper
// contract — the body is closed on every return, errors included — is
// observable.
type closeSpy struct {
	io.Reader
	closes int
}

func (c *closeSpy) Close() error {
	c.closes++
	return nil
}

// substitutingSigner installs a body of its own, then optionally fails. The
// transport keeps such a body because that is what the signer signed and
// wants sent, but when the attempt is abandoned nothing else holds a reference
// to it, so the transport is the only thing that can close it.
type substitutingSigner struct {
	body io.ReadCloser
	err  error
}

func (s *substitutingSigner) Sign(_ context.Context, req *http.Request) error {
	req.Body = s.body
	return s.err
}

// TestSigningTransportClosesASubstitutedBodyItAbandons covers both paths where
// a signer-installed body is dropped rather than sent: the signer failing, and
// the rebuildable path replacing it with a fresh GetBody reader. Neither leaks.
func TestSigningTransportClosesASubstitutedBodyItAbandons(t *testing.T) {
	const body = `{"model":"m","input":"hello"}`

	t.Run("signer fails", func(t *testing.T) {
		spy := &closeSpy{Reader: strings.NewReader(body)}
		inner := &recordingTransport{}
		transport := &SigningTransport{Inner: inner}

		req := signingRequest(body, true)
		req = req.WithContext(WithRequestSigner(req.Context(), &substitutingSigner{
			body: spy, err: errors.New("signer exploded"),
		}))
		if _, err := transport.RoundTrip(req); err == nil {
			t.Fatal("RoundTrip succeeded, want the signer's error")
		}
		if spy.closes != 1 {
			t.Fatalf("substituted body closed %d times, want 1: a body only the transport holds must not leak", spy.closes)
		}
		if inner.calls != 0 {
			t.Fatalf("inner transport calls = %d, want 0", inner.calls)
		}
	})

	t.Run("replaced by a rebuild", func(t *testing.T) {
		spy := &closeSpy{Reader: strings.NewReader(body)}
		inner := &recordingTransport{}
		transport := &SigningTransport{Inner: inner}

		req := signingRequest(body, true)
		req = req.WithContext(WithRequestSigner(req.Context(), &substitutingSigner{body: spy}))
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		_ = resp.Body.Close()
		if spy.closes != 1 {
			t.Fatalf("substituted body closed %d times, want 1: it was replaced on the request, so nobody else will close it", spy.closes)
		}
		if inner.sent != body {
			t.Fatalf("upstream body = %q, want the rebuilt payload", inner.sent)
		}
	})
}

func signingRequest(body string, withGetBody bool) *http.Request {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://upstream.example/v1/chat/completions", bytes.NewReader([]byte(body)))
	if err != nil {
		panic(err)
	}
	// http.NewRequest derives a GetBody for a *bytes.Reader; clear it first so
	// the flag below is the only thing that decides whether the request can be
	// rebuilt, which is the whole subject of these tests.
	req.GetBody = nil
	if withGetBody {
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader([]byte(body))), nil
		}
	}
	return req.WithContext(WithRequestSigner(req.Context(), nil))
}

// TestSigningTransportSendsTheFullPayloadAfterTheSignerConsumesIt pins the
// normal contract across every signer/rebuild combination that can be
// served: whatever the signer does, the request that goes upstream carries
// the whole body. A body-consuming signer needs a rebuild to send anything at
// all; a header-only signer needs none. The fourth combination — a
// body-consuming signer with no rebuild — is not servable, and
// TestSigningTransportRefusesToSendAConsumedBodyItCannotRebuild owns it.
func TestSigningTransportSendsTheFullPayloadAfterTheSignerConsumesIt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		makeSigner  func() RequestSigner
		rebuildable bool
	}{
		{"body-consuming signer, rebuildable", func() RequestSigner { return &bodyConsumingSigner{} }, true},
		{"header-only signer, rebuildable", func() RequestSigner { return &headerOnlySigner{} }, true},
		{"header-only signer, no rebuild", func() RequestSigner { return &headerOnlySigner{} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const body = `{"model":"m","input":"hello"}`
			inner := &recordingTransport{}
			transport := &SigningTransport{Inner: inner}

			req := signingRequest(body, tc.rebuildable)
			req = req.WithContext(WithRequestSigner(req.Context(), tc.makeSigner()))
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			_ = resp.Body.Close()

			if inner.calls != 1 {
				t.Fatalf("inner transport calls = %d, want 1", inner.calls)
			}
			if inner.sent != body {
				t.Fatalf("upstream body = %q, want %q", inner.sent, body)
			}
			if !tc.rebuildable && inner.gotBody != req.Body {
				t.Errorf("inner transport received %T, want the caller's own body: the tracking wrapper must not reach the wire", inner.gotBody)
			}
		})
	}
}

// TestSigningTransportRefusesToSendAConsumedBodyItCannotRebuild is the case
// the transport cannot serve. A signer that consumes the body leaves nothing
// to send, and with no GetBody there is no way to rebuild it. Sending the
// spent reader would put a truncated request upstream and let it read as a
// successful exchange, so the attempt must fail locally and loudly instead.
func TestSigningTransportRefusesToSendAConsumedBodyItCannotRebuild(t *testing.T) {
	const body = `{"model":"m","input":"hello"}`
	inner := &recordingTransport{}
	transport := &SigningTransport{Inner: inner}

	req := signingRequest(body, false)
	spy := &closeSpy{Reader: strings.NewReader(body)}
	req.Body = spy
	req = req.WithContext(WithRequestSigner(req.Context(), &bodyConsumingSigner{}))
	resp, err := transport.RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("RoundTrip succeeded on a body it consumed and cannot rebuild; a truncated request went upstream")
	}

	var signingErr *SigningError
	if !errors.As(err, &signingErr) {
		t.Fatalf("error = %v, want a *SigningError so the attempt is classified local and non-retryable", err)
	}
	if !signingErr.IsNonRetryable() {
		t.Error("a signing failure must be non-retryable so the retry layer never repeats it")
	}
	if inner.calls != 0 {
		t.Fatalf("inner transport calls = %d, want 0: nothing may be sent", inner.calls)
	}
	if spy.closes == 0 {
		t.Error("the request body was not closed on the error return; the RoundTripper contract requires it")
	}
}

// refusingSigner refuses to sign, so the attempt never reaches a transport.
type refusingSigner struct{ err error }

func (s refusingSigner) Sign(context.Context, *http.Request) error { return s.err }

// TestSigningTransportClosesTheBodyWhenSigningFails covers the sign-error
// return with the tracking wrapper installed, the one path where the body this
// transport watched is also the body it must close.
func TestSigningTransportClosesTheBodyWhenSigningFails(t *testing.T) {
	const body = `{"model":"m","input":"hello"}`
	inner := &recordingTransport{}
	transport := &SigningTransport{Inner: inner}

	req := signingRequest(body, false)
	req.Body = &closeSpy{Reader: strings.NewReader(body)}
	req = req.WithContext(WithRequestSigner(req.Context(), refusingSigner{err: errors.New("no credentials")}))

	resp, err := transport.RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("RoundTrip succeeded despite the signer refusing")
	}
	if _, ok := errors.AsType[*SigningError](err); !ok {
		t.Fatalf("error = %v, want a *SigningError", err)
	}
	if inner.calls != 0 {
		t.Fatalf("inner transport calls = %d, want 0", inner.calls)
	}
	if spy := req.Body.(*closeSpy); spy.closes == 0 {
		t.Error("the request body was not closed on the sign-error return; the RoundTripper contract requires it")
	}
}

// TestSigningTransportPassesThroughWithoutASigner keeps the transparent path
// untouched: no signer means no clone, no body handling, no error.
func TestSigningTransportPassesThroughWithoutASigner(t *testing.T) {
	const body = `{"model":"m"}`
	inner := &recordingTransport{}
	transport := &SigningTransport{Inner: inner}

	req := signingRequest(body, true)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if inner.calls != 1 || inner.sent != body {
		t.Fatalf("inner calls = %d sent = %q, want 1 and %q", inner.calls, inner.sent, body)
	}
}
