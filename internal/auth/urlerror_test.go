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

// asOnlyWrapper exposes a *url.Error through a custom As method with no
// Unwrap: it is outside the sanitizer's documented scope (only Unwrap-
// reachable URLs are scrubbed). The sanitizer must leave such an error
// completely alone - no rebuild, concrete type intact - because there is
// nothing on the walkable chain to redact.
type asOnlyWrapper struct{ inner *url.Error }

func (w *asOnlyWrapper) Error() string { return "as-only: " + w.inner.Error() }
func (w *asOnlyWrapper) As(target any) bool {
	if t, ok := target.(**url.Error); ok {
		*t = w.inner
		return true
	}
	return false
}

func TestSanitizeURLErrorAsOnlyWrapperReturnedUnchanged(t *testing.T) {
	w := &asOnlyWrapper{inner: chainURLError()}
	got := SanitizeURLError(w)
	if got != error(w) {
		t.Fatalf("SanitizeURLError returned %T, want the original error unchanged: an As-only URL is out of scope and there is nothing to redact on the Unwrap chain", got)
	}
}

// typedNilWrapper exposes a typed-nil *url.Error through Unwrap without
// rendering it, the way a real wrapper would carry a zero value it never
// formatted. The walker must tolerate it: no panic, wrapper preserved.
type typedNilWrapper struct{ inner error }

func (w *typedNilWrapper) Error() string { return "typed-nil carrier" }
func (w *typedNilWrapper) Unwrap() error { return w.inner }

func TestSanitizeURLErrorTypedNilURLErrorDoesNotPanic(t *testing.T) {
	w := &typedNilWrapper{inner: (*url.Error)(nil)}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SanitizeURLError panicked on a typed-nil *url.Error: %v", r)
		}
	}()
	got := SanitizeURLError(w)
	if !strings.Contains(got.Error(), "typed-nil carrier") {
		t.Fatalf("sanitized error %q lost the wrapper's text", got.Error())
	}
}

// selectiveJoin joins children but renders only the first, so a typed-nil
// child beside it never reaches its own (panicking) Error method.
type selectiveJoin struct{ children []error }

func (j *selectiveJoin) Error() string   { return j.children[0].Error() }
func (j *selectiveJoin) Unwrap() []error { return j.children }

// A typed-nil *url.Error beside or below a real one must not panic any
// walker once the real URL makes the chain eligible for rewriting: the
// typed-nil is a leaf with nothing to redact, and every real URL is still
// redacted. Each chain renders without panicking before sanitization.
func TestSanitizeURLErrorTypedNilBesideRealURLError(t *testing.T) {
	for name, build := range map[string]func() error{
		"joined sibling behind a wrapper": func() error {
			return errors.Join(chainURLError(), &typedNilWrapper{inner: (*url.Error)(nil)})
		},
		"Err of a real url.Error behind a wrapper": func() error {
			u := chainURLError()
			u.Err = &typedNilWrapper{inner: (*url.Error)(nil)}
			return u
		},
		"direct child of a selective join": func() error {
			return &selectiveJoin{children: []error{chainURLError(), (*url.Error)(nil)}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := build()
			_ = in.Error() // the original renders safely
			var got error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("SanitizeURLError panicked: %v", r)
					}
				}()
				got = SanitizeURLError(in)
			}()
			requireChainRedacted(t, got)
			if strings.Contains(name, "wrapper") {
				if _, ok := errors.AsType[*typedNilWrapper](got); !ok {
					t.Fatalf("the typed-nil carrier is no longer reachable: %v", got)
				}
			}
		})
	}
}
