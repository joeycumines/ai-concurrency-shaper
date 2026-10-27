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
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

const chainSecret = "chain-secret"

// chainTypedWrapper is an ordinary wrapper error whose type errors.As can
// match.
type chainTypedWrapper struct{ err error }

func (w *chainTypedWrapper) Error() string { return "typed: " + w.err.Error() }
func (w *chainTypedWrapper) Unwrap() error { return w.err }

func chainURLError() *url.Error {
	return &url.Error{
		Op:  "Get",
		URL: "https://upstream.example/v1?key=" + chainSecret,
		Err: errors.New("connection refused"),
	}
}

// requireChainRedacted fails when the sanitized error, or the *url.Error it
// exposes, still carries the credential.
func requireChainRedacted(t *testing.T, err error) {
	t.Helper()
	if strings.Contains(err.Error(), chainSecret) {
		t.Fatalf("sanitized message leaks the credential: %v", err)
	}
	if u, ok := errors.AsType[*url.Error](err); ok && (strings.Contains(u.URL, chainSecret) || strings.Contains(u.Error(), chainSecret)) {
		t.Fatalf("reachable *url.Error leaks the credential: %v", u)
	}
}

// A sentinel reachable below a URL-bearing text must stay matchable: the
// message is scrubbed, the chain is not severed.
func TestSanitizeURLErrorKeepsSentinelBehindURLText(t *testing.T) {
	outer := &url.Error{
		Op:  "Get",
		URL: "https://upstream.example/v1?key=" + chainSecret,
		Err: fmt.Errorf("dialing https://upstream.example/v1?key=%s: %w", chainSecret, context.Canceled),
	}
	got := SanitizeURLError(outer)
	requireChainRedacted(t, got)
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("errors.Is(context.Canceled) = false for %v", got)
	}
}

// A joined sibling must stay reachable next to the URL-bearing error.
func TestSanitizeURLErrorKeepsJoinedSibling(t *testing.T) {
	got := SanitizeURLError(errors.Join(chainURLError(), context.Canceled))
	requireChainRedacted(t, got)
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("errors.Is(context.Canceled) = false for joined sibling in %v", got)
	}
}

// A branch that holds no URL is returned as it was, so its wrapper type is
// still matchable through errors.As.
func TestSanitizeURLErrorReturnsURLFreeBranchUnchanged(t *testing.T) {
	typed := &chainTypedWrapper{err: context.DeadlineExceeded}
	got := SanitizeURLError(errors.Join(chainURLError(), typed))
	requireChainRedacted(t, got)
	var found *chainTypedWrapper
	if !errors.As(got, &found) || found != typed {
		t.Fatalf("errors.As(*chainTypedWrapper) did not return the URL-free branch: %v", got)
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(context.DeadlineExceeded) = false for %v", got)
	}
}

// A bare *url.Error input yields a *url.Error, whatever its text looks like.
func TestSanitizeURLErrorBareInputStaysURLError(t *testing.T) {
	bare := &url.Error{
		Op:  "Get",
		URL: "https://upstream.example/v1?key=" + chainSecret,
		Err: fmt.Errorf("refused at https://upstream.example/v1?key=%s: %w", chainSecret, context.Canceled),
	}
	got := SanitizeURLError(bare)
	if _, ok := got.(*url.Error); !ok {
		t.Fatalf("SanitizeURLError(*url.Error) returned %T, want *url.Error", got)
	}
	requireChainRedacted(t, got)
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("errors.Is(context.Canceled) = false for %v", got)
	}
}
