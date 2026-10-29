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

package proxy

import (
	"net/http"
	"strings"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
)

// reauthTransport recovers from an upstream rejection of a rotating
// credential. A request whose credential came from an
// auth.DynamicSecretSource (marked via auth.AttachReauthentication) that is
// answered 401, or 403 without rate-limit signals, invalidates the
// credential; when the body can be replayed and the source yields a
// different value, the request is re-sent exactly once with it. Every
// attempt is sent with the source's current value, so retries after a
// refresh carry the fresh credential.
//
// Requests without the marker (static env:/file: credentials, inbound
// forwarding, no auth) pass through untouched, so their retry behavior is
// exactly that of the surrounding retry transport.
type reauthTransport struct {
	inner http.RoundTripper
}

func (t *reauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reauth, ok := auth.ReauthenticationFromContext(req.Context())
	if !ok {
		return t.inner.RoundTrip(req)
	}

	// The retry layer re-sends the same request on every attempt, carrying
	// the credential applied when it was built. Each attempt is sent with
	// the source's current value instead, so an attempt after a refresh
	// never repeats a credential the upstream already rejected.
	send, used := req, reauth.Secret
	if current, err := reauth.Source.Secret(req.Context()); err == nil {
		if current = strings.TrimSpace(current); current != "" && current != used {
			send = req.Clone(req.Context())
			reauth.Apply(send.Header, current)
			used = current
		}
	}

	resp, err := t.inner.RoundTrip(send)
	if err != nil || resp == nil || !credentialRejected(resp) {
		return resp, err
	}
	reauth.Source.Invalidate(used)

	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	if !replayable {
		return resp, nil
	}
	fresh, serr := reauth.Source.Secret(req.Context())
	fresh = strings.TrimSpace(fresh)
	if serr != nil || fresh == "" || fresh == used {
		// No different credential to try (the refresh is rate-limited,
		// failed, or returned the same value): the rejection stands.
		return resp, nil
	}

	resend := req.Clone(req.Context())
	if req.GetBody != nil {
		body, berr := req.GetBody()
		if berr != nil {
			return resp, nil
		}
		resend.Body = body
	}
	reauth.Apply(resend.Header, fresh)

	// Closed without draining: reading the rejected body could block on a
	// slow upstream, and this path is rare enough that losing the
	// connection for reuse costs nothing.
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	return t.inner.RoundTrip(resend)
}

// Unwrap exposes the inner transport to the retry/breaker detection walk.
func (t *reauthTransport) Unwrap() http.RoundTripper {
	if t == nil {
		return nil
	}
	return t.inner
}

// credentialRejected reports whether resp rejects the request's credential:
// any 401, or a 403 that is not a rate-limit ban (a 403 carrying
// Retry-After or rate-limit headers is the circuit breaker's temporary-ban
// signal, not an authentication failure).
func credentialRejected(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return true
	case http.StatusForbidden:
		now := time.Now()
		return !circuitbreaker.IsTemporaryBanResponse(resp.Header, now, now)
	default:
		return false
	}
}
