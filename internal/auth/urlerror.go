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
	"cmp"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// redactedQueryPlaceholder is the escaped "[REDACTED]" RedactSensitiveURL
// substitutes for a credential value.
const redactedQueryPlaceholder = "%5BREDACTED%5D"

// SanitizeURLError rewrites a transport error so that no URL carried by a
// *url.Error reachable through its chain can echo a credential query value
// into a log line. net/http wraps transport failures in *url.Error values
// carrying the full outbound URL, and redirect handling nests further
// *url.Error values, sometimes behind ordinary %w wrappers and sometimes
// rendered into wrapper text.
//
// Guarantees:
//   - Error() is the original message with every URL found in the chain
//     replaced by its redacted form, raw and %q-quoted spellings alike. The
//     longest URL is replaced first, so a URL that is a prefix of another can
//     never leave the longer one's tail behind.
//   - The chain keeps its shape: every member reachable through Unwrap() error
//     or Unwrap() []error stays reachable, so errors.Is keeps matching
//     sentinels such as context errors and joined siblings stay reachable. A
//     branch that carries no URL is returned as it was.
//   - Every *url.Error reachable through Unwrap is a clone whose URL is
//     redacted and whose Err is itself sanitized. A bare *url.Error input
//     yields a *url.Error. A leaf whose own message carries a URL is
//     replaced by a sanitized leaf: leaving the original reachable would
//     re-expose the URL through Unwrap().Error(), so its concrete type is
//     deliberately not preserved.
//   - A typed-nil *url.Error anywhere in the chain is a leaf with nothing
//     to redact; it is kept as it is and never rendered or unwrapped. An
//     error whose own rendering panics (for example errors.Join over a
//     typed-nil *url.Error) panics when logged anyway, and stays out of
//     scope.
//   - A URL that cannot be parsed is still redacted: its whole query and
//     fragment are replaced, since its credential parameters cannot be told
//     apart.
//
// Scope: the walk is Unwrap only, shared by detection, collection, and
// rewriting. A URL exposed only through a custom As method cannot be
// enumerated, so it is not covered and the error is returned unchanged.
// Wrappers on the path to a URL are rebuilt as generic sanitized wrappers,
// so their concrete types are not preserved.
//
// Errors carrying no *url.Error are returned unchanged.
func SanitizeURLError(err error) error {
	if !hasURLError(err) {
		return err
	}
	pairs := map[string]string{}
	collectURLs(err, pairs)
	olds := make([]string, 0, len(pairs))
	for old := range pairs {
		olds = append(olds, old)
	}
	// strings.Replacer tries patterns in argument order at each position, so
	// the longest pattern must come first (ties broken for determinism).
	slices.SortFunc(olds, func(a, b string) int {
		if c := cmp.Compare(len(b), len(a)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	args := make([]string, 0, 2*len(olds))
	for _, old := range olds {
		args = append(args, old, pairs[old])
	}
	return sanitizeWith(err, strings.NewReplacer(args...))
}

// hasURLError reports whether a *url.Error is reachable from err through
// the same traversal collectURLs and sanitizeWith walk: direct type
// assertion, then Unwrap() []error, then Unwrap() error. A URL exposed
// only through a custom As method is out of scope by construction, so
// detection and rewriting can never disagree about whether the chain
// carries something to redact.
func hasURLError(err error) bool {
	switch w := err.(type) {
	case *url.Error:
		return w != nil
	case interface{ Unwrap() []error }:
		if slices.ContainsFunc(w.Unwrap(), hasURLError) {
			return true
		}
	case interface{ Unwrap() error }:
		return hasURLError(w.Unwrap())
	}
	return false
}

// sanitizeWith rebuilds err with r applied to every rendering. A *url.Error
// becomes a clone. A subtree that holds no *url.Error and whose rendering r
// leaves unchanged is returned as it is, so sentinels keep their identity and
// URL-free branches keep their types. Every other node is rebuilt with each of
// its children sanitized, so no member of the chain is dropped.
func sanitizeWith(err error, r *strings.Replacer) error {
	if err == nil {
		return nil
	}
	if direct, ok := err.(*url.Error); ok {
		if direct == nil {
			// A typed-nil carries no URL, and rendering it would panic.
			return err
		}
		return cloneURLError(direct, r)
	}
	msg := r.Replace(err.Error())
	if msg == err.Error() && !hasURLError(err) {
		return err
	}
	switch w := err.(type) {
	case interface{ Unwrap() []error }:
		children := w.Unwrap()
		sanitized := make([]error, len(children))
		for i, child := range children {
			sanitized[i] = sanitizeWith(child, r)
		}
		return &sanitizedJoinError{msg: msg, children: sanitized}
	case interface{ Unwrap() error }:
		return &sanitizedError{msg: msg, child: sanitizeWith(w.Unwrap(), r)}
	}
	return &sanitizedError{msg: msg}
}

// cloneURLError returns a copy of e with its URL redacted and its Err
// sanitized, so the clone is safe to render and to unwrap.
func cloneURLError(e *url.Error, r *strings.Replacer) *url.Error {
	clone := *e
	clone.URL = redactRawURL(e.URL)
	clone.Err = sanitizeWith(e.Err, r)
	return &clone
}

// collectURLs records the raw and quoted old/new spellings of every
// *url.Error URL in the chain.
func collectURLs(err error, pairs map[string]string) {
	if err == nil {
		return
	}
	if u, ok := err.(*url.Error); ok {
		if u == nil {
			// A typed-nil is a leaf: its Unwrap would dereference it.
			return
		}
		if u.URL != "" {
			if redacted := redactRawURL(u.URL); redacted != u.URL {
				pairs[u.URL] = redacted
				pairs[strconv.Quote(u.URL)] = strconv.Quote(redacted)
			}
		}
	}
	switch w := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range w.Unwrap() {
			collectURLs(child, pairs)
		}
	case interface{ Unwrap() error }:
		collectURLs(w.Unwrap(), pairs)
	}
}

// redactRawURL redacts one URL string for logging.
func redactRawURL(raw string) string {
	if parsed, err := url.Parse(raw); err == nil {
		return RedactSensitiveURL(parsed).String()
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		return raw[:i] + "?" + redactedQueryPlaceholder
	}
	return raw
}

// sanitizedError carries a scrubbed rendering of a wrapper with one child.
type sanitizedError struct {
	msg   string
	child error
}

func (e *sanitizedError) Error() string { return e.msg }
func (e *sanitizedError) Unwrap() error { return e.child }

// sanitizedJoinError carries a scrubbed rendering of a wrapper with several
// children, as errors.Join produces.
type sanitizedJoinError struct {
	msg      string
	children []error
}

func (e *sanitizedJoinError) Error() string   { return e.msg }
func (e *sanitizedJoinError) Unwrap() []error { return e.children }
