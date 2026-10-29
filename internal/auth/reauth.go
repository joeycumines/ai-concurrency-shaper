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

package auth

import (
	"context"
	"net/http"
)

// Reauthentication records how a request was authenticated from a
// DynamicSecretSource, so the transport that sends it can invalidate the
// credential on an upstream 401/403 and re-send the request once with a
// fresh one. It is attached only for dynamic sources: requests authenticated
// from a static source carry none, and their retry behavior is unchanged.
type Reauthentication struct {
	// Source issued Secret.
	Source DynamicSecretSource
	// Secret is the credential value the request was sent with.
	Secret string
	// Header and Scheme locate the credential: the header is set to
	// Scheme+secret (Scheme is "Bearer " for bearer auth, else empty).
	Header string
	Scheme string
}

// Apply sets the credential header on h to secret.
func (r Reauthentication) Apply(h http.Header, secret string) {
	h.Set(r.Header, r.Scheme+secret)
}

type reauthenticationKey struct{}

// withReauthentication returns ctx carrying r.
func withReauthentication(ctx context.Context, r Reauthentication) context.Context {
	return context.WithValue(ctx, reauthenticationKey{}, r)
}

// ReauthenticationFromContext returns the Reauthentication attached by
// AttachReauthentication, if any.
func ReauthenticationFromContext(ctx context.Context) (Reauthentication, bool) {
	r, ok := ctx.Value(reauthenticationKey{}).(Reauthentication)
	return r, ok && r.Source != nil
}

// AttachReauthentication marks req as authenticated by source when source
// is dynamic, after the credential header was set to scheme+secret; other
// sources attach nothing. The request is updated in place so callers holding
// the pointer (the Rewrite hook's ProxyRequest.Out) see the marked context.
func AttachReauthentication(req *http.Request, source SecretSource, secret, header, scheme string) {
	dynamic, ok := source.(DynamicSecretSource)
	if !ok {
		return
	}
	*req = *req.WithContext(withReauthentication(req.Context(), Reauthentication{
		Source: dynamic,
		Secret: secret,
		Header: header,
		Scheme: scheme,
	}))
}
