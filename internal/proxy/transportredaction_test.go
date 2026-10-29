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

// Package proxy implements a concurrency-bounded reverse proxy.
package proxy

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// reachableURLErrors returns every *url.Error reachable from err through
// Unwrap() error and Unwrap() []error.
func reachableURLErrors(err error) []*url.Error {
	var out []*url.Error
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if u, ok := e.(*url.Error); ok {
			out = append(out, u)
		}
		switch w := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range w.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(w.Unwrap())
		}
	}
	walk(err)
	return out
}

// TestSanitizeTransportErrorRedactsEveryURLInTheChain pins that no secret
// query value survives sanitization, whatever the chain looks like: an outer
// URL that is a prefix of an inner URL rendered unquoted, a nested *url.Error
// behind a non-url.Error wrapper reached through Unwrap/errors.As, and an
// outer URL that cannot be parsed.
func TestSanitizeTransportErrorRedactsEveryURLInTheChain(t *testing.T) {
	const outerURL = "https://upstream.example/v1?key=outer-secret"
	prefixInner := &url.Error{Op: "Get", URL: outerURL + "&token=inner-secret", Err: errors.New("connection refused")}
	for name, input := range map[string]error{
		"outer prefixes inner": &url.Error{
			Op:  "Post",
			URL: outerURL,
			Err: fmt.Errorf("redirect to %s: %w", prefixInner.URL, prefixInner),
		},
		"wrapped nested chain": fmt.Errorf("round trip: %w", &url.Error{
			Op:  "Post",
			URL: outerURL,
			Err: fmt.Errorf("redirect detail: %w", &url.Error{
				Op: "Get", URL: "https://upstream.example/v2?token=inner-secret", Err: errors.New("refused"),
			}),
		}),
		"unparseable outer": &url.Error{
			Op:  "Post",
			URL: "https://upstream.example/v1\x7f?key=outer-secret",
			Err: &url.Error{Op: "Get", URL: "https://upstream.example/v2?token=inner-secret", Err: errors.New("refused")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := sanitizeTransportError(input)
			for _, secret := range []string{"outer-secret", "inner-secret"} {
				if strings.Contains(got.Error(), secret) {
					t.Errorf("sanitized message leaks %q: %s", secret, got)
				}
				for _, u := range reachableURLErrors(got) {
					if strings.Contains(u.URL, secret) || strings.Contains(u.Error(), secret) {
						t.Errorf("reachable *url.Error leaks %q: %s", secret, u)
					}
				}
			}
			if _, ok := errors.AsType[*url.Error](got); !ok {
				t.Fatalf("errors.As(%T) found no *url.Error", got)
			}
		})
	}
}

func TestSanitizeTransportErrorFindsWrappedNestedURLErrors(t *testing.T) {
	inner := &url.Error{
		Op:  "Get",
		URL: "https://upstream.example/v1?token=inner-secret",
		Err: errors.New("connection refused"),
	}
	middle := fmt.Errorf("redirect detail: %w", inner)
	outer := &url.Error{
		Op:  "Post",
		URL: "https://upstream.example/v1?key=outer-secret",
		Err: middle,
	}
	wrapped := fmt.Errorf("round trip to %q failed: %w", outer.URL, outer)

	for _, input := range []error{outer, wrapped} {
		got := sanitizeTransportError(input)
		for _, secret := range []string{"inner-secret", "outer-secret"} {
			if strings.Contains(got.Error(), secret) {
				t.Fatalf("sanitized error leaks %q: %v", secret, got)
			}
		}
	}
}
