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
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// redactedQueryPlaceholder is the escaped "[REDACTED]" RedactSensitiveURL
// substitutes for a credential value.
const redactedQueryPlaceholder = "%5BREDACTED%5D"

// SanitizeURLError rewrites a transport error so that no *url.Error URL
// anywhere in its chain can echo a credential query value into a log line.
// net/http wraps transport failures in *url.Error carrying the full outbound
// URL, and redirect handling nests further *url.Error values, sometimes
// behind ordinary %w wrappers and sometimes rendered into wrapper text.
//
// Guarantees:
//   - Error() is the original message with every URL found in the chain
//     replaced by its redacted form, raw and %q-quoted spellings alike. The
//     longest URL is replaced first, so a URL that is a prefix of another
//     can never leave the longer one's tail behind.
//   - Every *url.Error reachable through Unwrap/errors.As is a clone whose
//     URL is redacted and whose Err is itself sanitized, so no raw URL is
//     reachable programmatically either. A bare *url.Error input yields a
//     *url.Error.
//   - A URL that cannot be parsed is still redacted: its whole query and
//     fragment are replaced, since its credential parameters cannot be told
//     apart.
//
// Errors carrying no *url.Error are returned unchanged.
func SanitizeURLError(err error) error {
	if _, ok := errors.AsType[*url.Error](err); !ok {
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

// sanitizeWith rebuilds err with r applied to every rendering and every
// reachable *url.Error cloned.
func sanitizeWith(err error, r *strings.Replacer) error {
	if err == nil {
		return nil
	}
	msg := r.Replace(err.Error())
	if direct, ok := err.(*url.Error); ok {
		clone := cloneURLError(direct, r)
		if clone.Error() == msg {
			return clone
		}
		return &sanitizedURLError{msg: msg, redacted: clone}
	}
	if outer, ok := errors.AsType[*url.Error](err); ok {
		return &sanitizedURLError{msg: msg, redacted: cloneURLError(outer, r)}
	}
	if msg == err.Error() {
		// No URL below and nothing to scrub: keep the original value, so
		// errors.Is keeps matching sentinels such as context errors.
		return err
	}
	return &sanitizedURLError{msg: msg}
}

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
	if u, ok := err.(*url.Error); ok && u.URL != "" {
		if redacted := redactRawURL(u.URL); redacted != u.URL {
			pairs[u.URL] = redacted
			pairs[strconv.Quote(u.URL)] = strconv.Quote(redacted)
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

// sanitizedURLError carries a scrubbed rendering of a wrapped error chain.
// Unwrap exposes only the sanitized clone of the chain's first *url.Error
// (nil when the scrubbed value held none), never the original.
type sanitizedURLError struct {
	msg      string
	redacted error
}

func (e *sanitizedURLError) Error() string { return e.msg }

func (e *sanitizedURLError) Unwrap() error { return e.redacted }
