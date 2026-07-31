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

package policy

import (
	"net/http"
	"testing"
)

func TestCrushedClientBehavior_EnabledCrushed(t *testing.T) {
	b := NewCrushedClientBehavior()
	b.SetEnabled(true)

	// 403 should be retryable when crushed.
	resp := &http.Response{StatusCode: http.StatusForbidden}
	if !b.CheckRetry(nil, resp, nil) {
		t.Error("expected 403 to be retryable when enabled and crushed")
	}

	// 429 should be retryable.
	resp429 := &http.Response{StatusCode: http.StatusTooManyRequests}
	if !b.CheckRetry(nil, resp429, nil) {
		t.Error("expected 429 to be retryable when enabled and crushed")
	}

	// 500 should be retryable.
	resp500 := &http.Response{StatusCode: http.StatusInternalServerError}
	if !b.CheckRetry(nil, resp500, nil) {
		t.Error("expected 500 to be retryable when enabled and crushed")
	}

	// 502 should be retryable.
	resp502 := &http.Response{StatusCode: 502}
	if !b.CheckRetry(nil, resp502, nil) {
		t.Error("expected 502 to be retryable when enabled and crushed")
	}
}

func TestCrushedClientBehavior_EnabledNotCrushed(t *testing.T) {
	// The CheckRetry function doesn't check IsCrushed - it relies on
	// the proxy engine to only call this behavior for crushed clients.
	// When CheckRetry is called directly, it just checks its own enabled flag.
	b := NewCrushedClientBehavior()
	b.SetEnabled(true)

	// 403 is retryable regardless (the behavior itself doesn't check
	// the crushed status - that's the caller's responsibility).
	resp := &http.Response{StatusCode: http.StatusForbidden}
	if !b.CheckRetry(nil, resp, nil) {
		t.Error("expected 403 to be retryable when behavior is enabled")
	}
}

func TestCrushedClientBehavior_Disabled(t *testing.T) {
	b := NewCrushedClientBehavior()
	b.SetEnabled(false)

	// Disabled behavior always returns false.
	resp := &http.Response{StatusCode: http.StatusForbidden}
	if b.CheckRetry(nil, resp, nil) {
		t.Error("expected 403 NOT to be retryable when disabled")
	}
}

func TestCrushedClientBehavior_NilResponse(t *testing.T) {
	b := NewCrushedClientBehavior()
	b.SetEnabled(true)

	// nil response should not be retryable for CrushedClientBehavior.
	if b.CheckRetry(nil, nil, nil) {
		t.Error("expected nil response NOT to be retryable")
	}
}

func TestCrushedClientBehavior_NilResponseWithError(t *testing.T) {
	b := NewCrushedClientBehavior()
	b.SetEnabled(true)

	// Transport errors should be retryable.
	if !b.CheckRetry(nil, nil, makeError("connection lost")) {
		t.Error("expected transport error to be retryable")
	}
}

func TestCrushedClientBehavior_IsEnabledToggle(t *testing.T) {
	b := NewCrushedClientBehavior()

	if !b.IsEnabled() {
		t.Error("expected IsEnabled=true initial (enabled by default)")
	}

	b.SetEnabled(false)
	if b.IsEnabled() {
		t.Error("expected IsEnabled=false after SetEnabled(false)")
	}

	b.SetEnabled(true)
	if !b.IsEnabled() {
		t.Error("expected IsEnabled=true after SetEnabled(true)")
	}
}

func TestRetryOn403Behavior_NilResponse(t *testing.T) {
	b := NewRetryOn403Behavior()
	b.SetEnabled(true)

	// nil response should not be retryable.
	if b.CheckRetry(nil, nil, nil) {
		t.Error("expected nil response NOT to be retryable for RetryOn403")
	}
}

func TestRetryOn403Behavior_NilResponseWithError(t *testing.T) {
	b := NewRetryOn403Behavior()
	b.SetEnabled(true)

	// Transport errors should be retryable.
	if !b.CheckRetry(nil, nil, makeError("connection refused")) {
		t.Error("expected transport error to be retryable")
	}
}

func TestRetryOn403Behavior_200Response(t *testing.T) {
	b := NewRetryOn403Behavior()
	b.SetEnabled(true)

	// 200 should not be retryable.
	resp := &http.Response{StatusCode: http.StatusOK}
	if b.CheckRetry(nil, resp, nil) {
		t.Error("expected 200 NOT to be retryable")
	}
}

type assertError struct {
	msg string
}

func (e *assertError) Error() string { return e.msg }

func makeError(msg string) error {
	return &assertError{msg: msg}
}
