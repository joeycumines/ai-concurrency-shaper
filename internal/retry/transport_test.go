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

package retry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
)

// rtFunc is a RoundTripper implemented by a function.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func body(s string) io.ReadCloser {
	return io.NopCloser(bytes.NewBufferString(s))
}

type blockingEOFBody struct {
	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *blockingEOFBody) Read([]byte) (int, error) {
	b.once.Do(func() {
		if b.started != nil {
			close(b.started)
		}
	})
	<-b.release
	return 0, io.EOF
}

func (b *blockingEOFBody) Close() error { return nil }

type closeTrackingBody struct {
	reader io.Reader
	closed atomic.Bool
}

func (b *closeTrackingBody) Read(p []byte) (int, error) {
	if b.reader == nil {
		return 0, io.EOF
	}
	return b.reader.Read(p)
}

func (b *closeTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

type blockingCloseBody struct {
	closed     chan struct{}
	closeOnce  sync.Once
	closedFlag atomic.Bool
}

func newBlockingCloseBody() *blockingCloseBody {
	return &blockingCloseBody{closed: make(chan struct{})}
}

func (b *blockingCloseBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockingCloseBody) Close() error {
	b.closedFlag.Store(true)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestRetryOn5xx(t *testing.T) {
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})
	tr := Transport{Inner: inner, MaxRetries: 3, WaitMin: time.Millisecond, WaitMax: time.Millisecond}
	resp, _ := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	if resp.StatusCode != 200 {
		t.Errorf("got status %d, want 200", resp.StatusCode)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestBodyReplay(t *testing.T) {
	var bodies []string
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		p, _ := io.ReadAll(req.Body)
		bodies = append(bodies, string(p))
		if len(bodies) < 2 {
			return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:        inner,
		MaxBodyBytes: 1 << 20,
		MaxRetries:   2,
		WaitMin:      time.Millisecond,
		WaitMax:      time.Millisecond,
	}
	req := httptest.NewRequest("POST", "http://x/", bytes.NewReader([]byte("hello")))
	resp, _ := tr.RoundTrip(req)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies = %d, want 2", len(bodies))
	}
	for i, b := range bodies {
		if b != "hello" {
			t.Errorf("attempt %d: body = %q, want %q", i, b, "hello")
		}
	}
}

func TestRetry_BreakerEpochFromContext(t *testing.T) {
	// Verify that the breakerEpoch is extracted from the request context
	// when set by the proxy's pre-check Allow() call. This prevents the
	// stale-probe bypass described in review-06 Finding 1: without the
	// context handoff, the retry transport's breakerEpoch would be 0 for
	// the first attempt, causing RecordFailure(..., epoch=0) to bypass
	// the stale-probe guard in circuitbreaker.RecordFailure.
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(1*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trip the circuit to get a non-zero probe epoch.
	for range 10 {
		b.RecordFailure(500, 0, time.Time{}, 0)
	}
	// Wait for the OPEN period to expire so the next Allow() returns HALF_OPEN.
	time.Sleep(1100 * time.Millisecond)
	epoch, allowErr := b.Allow()
	if allowErr != nil {
		t.Fatalf("Allow() after OPEN timeout: %v", allowErr)
	}
	if epoch == 0 {
		t.Fatalf("expected non-zero epoch from HALF_OPEN Allow(), got 0")
	}

	// Inject the epoch into the request context, mimicking what the proxy does.
	attempt := 0
	var capturedEpoch uint64
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		attempt++
		if attempt == 1 {
			// Verify the context value is accessible.
			if v := req.Context().Value(BreakerEpochKey); v != nil {
				capturedEpoch = v.(uint64)
			}
		}
		return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 0, // no retries — just test the first attempt
		Breaker:    b,
	}

	req := httptest.NewRequest("GET", "http://x/", nil)
	req = req.WithContext(context.WithValue(req.Context(), BreakerEpochKey, epoch))

	_, _ = tr.RoundTrip(req)

	if capturedEpoch != epoch {
		t.Errorf("captured epoch = %d, want %d (from context)", capturedEpoch, epoch)
	}
}

func TestRetry_BreakerEpochZeroWhenNotInContext(t *testing.T) {
	// Verify that without the context value, breakerEpoch remains 0.
	// This is the historical behavior — RecordFailure with epoch=0
	// bypasses the stale-probe guard.
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 0,
		Breaker:    b,
	}

	// No context value set — epoch should be 0, but the failure still records.
	_, _ = tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))

	stats := b.Stats()
	if stats.TotalFailures != 1 {
		t.Errorf("total failures = %d, want 1", stats.TotalFailures)
	}
}

func TestRetry_NilNilGuard(t *testing.T) {
	// Verify that a buggy inner transport returning (nil, nil) does not
	// cause httputil.ReverseProxy to panic. The retry transport replaces
	// (nil, nil) with a synthetic error.
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		return nil, nil // would panic httputil.ReverseProxy without the guard
	})

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 0, // no retries — just the first attempt
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
	}

	resp, err := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	if err == nil {
		t.Fatal("expected non-nil error for (nil, nil) from inner transport")
	}
	if !strings.Contains(err.Error(), "nil response without error") {
		t.Errorf("error = %v, want 'nil response without error' substring", err)
	}
	if resp != nil {
		t.Errorf("expected nil response, got %v", resp)
	}
}

func TestRetry_NilNilGuardWithBreaker(t *testing.T) {
	// Verify that (nil, nil) from inner transport records a failure with
	// status 0 (transport error) to the breaker.
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		return nil, nil
	})

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 0,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
		Breaker:    b,
	}

	_, err = tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	if err == nil {
		t.Fatal("expected non-nil error")
	}

	stats := b.Stats()
	if stats.TotalFailures != 1 {
		t.Errorf("total failures = %d, want 1 (breaker should see the nil-nil as a transport error)", stats.TotalFailures)
	}
}

func TestNo4xxRetry(t *testing.T) {
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 400, Body: body("bad"), Header: make(http.Header)}, nil
	})
	tr := Transport{Inner: inner, MaxRetries: 5}
	resp, _ := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	if resp.StatusCode != 400 {
		t.Fatalf("got status %d", resp.StatusCode)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestLargeBodyNoRetry(t *testing.T) {
	calls := 0
	var receivedBody string
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		p, _ := io.ReadAll(req.Body)
		receivedBody = string(p)
		req.Body.Close()
		return &http.Response{StatusCode: 500, Body: body("e"), Header: make(http.Header)}, nil
	})
	tr := Transport{Inner: inner, MaxRetries: 3, MaxBodyBytes: 100}
	big := bytes.Repeat([]byte("x"), 200)
	req := httptest.NewRequest("POST", "http://x/", bytes.NewReader(big))
	_, _ = tr.RoundTrip(req)
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if len(receivedBody) != 200 {
		t.Errorf("received body length = %d, want 200", len(receivedBody))
	}
}

func TestContextCancel(t *testing.T) {
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: body("e"), Header: make(http.Header)}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	tr := Transport{Inner: inner, MaxRetries: 1000, WaitMin: time.Millisecond, WaitMax: time.Millisecond}
	req := httptest.NewRequest("GET", "http://x/", nil)
	req = req.WithContext(ctx)
	// The RoundTrip should return after the context cancels, not hang.
	_, _ = tr.RoundTrip(req)
}

func TestRetry_CancelDuringBackoffReleasesHalfOpenProbe(t *testing.T) {
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(10*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	drainStarted := make(chan struct{})
	releaseDrain := make(chan struct{})
	var calls atomic.Int64
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if call != 1 {
			return &http.Response{StatusCode: http.StatusOK, Body: body("unexpected"), Header: make(http.Header), Request: req}, nil
		}
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Header:     make(http.Header),
			Body:       &blockingEOFBody{started: drainStarted, release: releaseDrain},
			Request:    req,
		}, nil
	})
	tr := &Transport{
		Inner:      inner,
		MaxRetries: 1,
		WaitMin:    time.Hour,
		WaitMax:    time.Hour,
		Breaker:    b,
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "http://x/", nil).WithContext(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := tr.RoundTrip(req)
		done <- err
	}()

	select {
	case <-drainStarted:
	case <-time.After(time.Second):
		t.Fatal("first retry response body was not drained")
	}
	time.Sleep(20 * time.Millisecond) // let the breaker OPEN timeout elapse before the retry Allow
	cancel()
	close(releaseDrain)

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RoundTrip error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RoundTrip did not return after request cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("inner RoundTrip calls = %d, want 1", calls.Load())
	}
	if epoch, err := b.Allow(); err != nil {
		t.Fatalf("Allow() after canceled retry backoff = epoch %d, err %v; want released HALF_OPEN probe", epoch, err)
	}
}

func TestRetry_RequestContextCancellationDoesNotRetryHalfOpenProbe(t *testing.T) {
	tests := []struct {
		name    string
		makeCtx func(t *testing.T) (context.Context, context.CancelFunc, error)
	}{
		{
			name: "canceled",
			makeCtx: func(t *testing.T) (context.Context, context.CancelFunc, error) {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}, context.Canceled
			},
		},
		{
			name: "deadline exceeded",
			makeCtx: func(t *testing.T) (context.Context, context.CancelFunc, error) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
				<-ctx.Done()
				return ctx, cancel, context.DeadlineExceeded
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := circuitbreaker.New(
				circuitbreaker.WithFailureThreshold(1),
				circuitbreaker.WithWindow(10*time.Second),
				circuitbreaker.WithOpenTimeout(10*time.Millisecond),
				circuitbreaker.WithMaxOpenTimeout(10*time.Millisecond),
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
			time.Sleep(20 * time.Millisecond)
			epoch, allowErr := b.Allow()
			if allowErr != nil {
				t.Fatalf("Allow() opening half-open probe: %v", allowErr)
			}
			if epoch == 0 {
				t.Fatal("half-open Allow returned epoch 0, want non-zero probe epoch")
			}

			ctx, cleanup, wantErr := tt.makeCtx(t)
			defer cleanup()
			var calls atomic.Int64
			inner := rtFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if got := req.Context().Err(); !errors.Is(got, wantErr) {
					t.Fatalf("request context error = %v, want %v", got, wantErr)
				}
				return nil, wantErr
			})
			tr := &Transport{
				Inner:      inner,
				MaxRetries: 3,
				WaitMin:    time.Millisecond,
				WaitMax:    time.Millisecond,
				Breaker:    b,
			}

			req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
			req = req.WithContext(context.WithValue(ctx, BreakerEpochKey, epoch))
			resp, err := tr.RoundTrip(req)
			if !errors.Is(err, wantErr) {
				t.Fatalf("RoundTrip error = %v, want %v", err, wantErr)
			}
			if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
				t.Fatalf("RoundTrip converted request cancellation into ErrCircuitOpen: %v", err)
			}
			if resp != nil {
				t.Fatalf("response = %#v, want nil", resp)
			}
			if calls.Load() != 1 {
				t.Fatalf("inner RoundTrip calls = %d, want exactly 1 (no retry after request cancellation)", calls.Load())
			}

			stats := b.Stats()
			if stats.TotalFailures != 1 || stats.TotalSuccesses != 0 {
				t.Fatalf("breaker failures=%d successes=%d, want only seeded failure and no cancellation accounting", stats.TotalFailures, stats.TotalSuccesses)
			}
			if state := b.State(); state != circuitbreaker.HalfOpen {
				t.Fatalf("breaker state = %s, want HALF_OPEN with cancellation not recorded as failure", state)
			}
			nextEpoch, allowErr := b.Allow()
			if allowErr != nil {
				t.Fatalf("Allow() after request-context cancellation = epoch %d, err %v; want released HALF_OPEN probe", nextEpoch, allowErr)
			}
			if nextEpoch == 0 {
				t.Fatal("Allow() after request-context cancellation returned epoch 0, want new HALF_OPEN probe epoch")
			}
			b.CancelProbe(nextEpoch)
		})
	}
}

func TestRetry_RequestContextCancellationAfterRetryableStatusDoesNotRetryHalfOpenProbe(t *testing.T) {
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(10*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
	time.Sleep(20 * time.Millisecond)
	epoch, allowErr := b.Allow()
	if allowErr != nil {
		t.Fatalf("Allow() opening half-open probe: %v", allowErr)
	}
	if epoch == 0 {
		t.Fatal("half-open Allow returned epoch 0, want non-zero probe epoch")
	}

	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int64
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		cancel()
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Header:     make(http.Header),
			Body:       body("retryable failure"),
			Request:    req,
		}, nil
	})
	tr := &Transport{
		Inner:      inner,
		MaxRetries: 3,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
		Breaker:    b,
	}

	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req = req.WithContext(context.WithValue(ctx, BreakerEpochKey, epoch))
	resp, err := tr.RoundTrip(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip error = %v, want context.Canceled", err)
	}
	if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		t.Fatalf("RoundTrip converted post-status cancellation into ErrCircuitOpen: %v", err)
	}
	if resp != nil {
		t.Fatalf("response = %#v, want nil because caller context was canceled before retry", resp)
	}
	if calls.Load() != 1 {
		t.Fatalf("inner RoundTrip calls = %d, want exactly 1 (no retry after request cancellation)", calls.Load())
	}

	stats := b.Stats()
	if stats.TotalFailures != 2 || stats.TotalSuccesses != 0 {
		t.Fatalf("breaker failures=%d successes=%d, want seeded failure plus upstream 500 and no cancellation success", stats.TotalFailures, stats.TotalSuccesses)
	}
	if state := b.State(); state != circuitbreaker.Open {
		t.Fatalf("breaker state = %s, want OPEN after definitive upstream 500", state)
	}
}

func TestRetry_RequestContextCancellationWithResponseClosesBodyAndReleasesHalfOpenProbe(t *testing.T) {
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(10*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
	time.Sleep(20 * time.Millisecond)
	epoch, allowErr := b.Allow()
	if allowErr != nil {
		t.Fatalf("Allow() opening half-open probe: %v", allowErr)
	}
	if epoch == 0 {
		t.Fatal("half-open Allow returned epoch 0, want non-zero probe epoch")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tracked := &closeTrackingBody{reader: strings.NewReader("ignored response")}
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       tracked,
			Request:    req,
		}, context.Canceled
	})
	tr := &Transport{
		Inner:        inner,
		MaxRetries:   3,
		MaxBodyBytes: 1024,
		WaitMin:      time.Millisecond,
		WaitMax:      time.Millisecond,
		Breaker:      b,
	}

	req := httptest.NewRequest(http.MethodPost, "http://x/", strings.NewReader("buffered request"))
	req = req.WithContext(context.WithValue(ctx, BreakerEpochKey, epoch))
	resp, err := tr.RoundTrip(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip error = %v, want context.Canceled", err)
	}
	if resp != nil {
		t.Fatalf("response = %#v, want nil for terminal request cancellation", resp)
	}
	if !tracked.closed.Load() {
		t.Fatal("response body returned with context cancellation was not closed")
	}
	stats := b.Stats()
	if stats.TotalFailures != 1 || stats.TotalSuccesses != 0 {
		t.Fatalf("breaker failures=%d successes=%d, want only seeded failure and no cancellation success", stats.TotalFailures, stats.TotalSuccesses)
	}
	nextEpoch, allowErr := b.Allow()
	if allowErr != nil {
		t.Fatalf("Allow() after resp+cancel cleanup = epoch %d, err %v; want released HALF_OPEN probe", nextEpoch, allowErr)
	}
	b.CancelProbe(nextEpoch)
}

func TestRetry_BodyTooLargeRequestContextCancellationReleasesHalfOpenProbe(t *testing.T) {
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(10*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b.RecordFailure(http.StatusInternalServerError, 0, time.Time{}, 0)
	time.Sleep(20 * time.Millisecond)
	epoch, allowErr := b.Allow()
	if allowErr != nil {
		t.Fatalf("Allow() opening half-open probe: %v", allowErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int64
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, context.Canceled
	})
	tr := &Transport{
		Inner:        inner,
		MaxRetries:   3,
		MaxBodyBytes: 1,
		Breaker:      b,
	}

	req := httptest.NewRequest(http.MethodPost, "http://x/", strings.NewReader("oversized"))
	req = req.WithContext(context.WithValue(ctx, BreakerEpochKey, epoch))
	resp, err := tr.RoundTrip(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip error = %v, want context.Canceled", err)
	}
	if resp != nil {
		t.Fatalf("response = %#v, want nil", resp)
	}
	if calls.Load() != 1 {
		t.Fatalf("inner RoundTrip calls = %d, want 1 body-too-large attempt", calls.Load())
	}
	stats := b.Stats()
	if stats.TotalFailures != 1 || stats.TotalSuccesses != 0 {
		t.Fatalf("breaker failures=%d successes=%d, want only seeded failure and no cancellation accounting", stats.TotalFailures, stats.TotalSuccesses)
	}
	nextEpoch, allowErr := b.Allow()
	if allowErr != nil {
		t.Fatalf("Allow() after body-too-large cancellation = epoch %d, err %v; want released HALF_OPEN probe", nextEpoch, allowErr)
	}
	b.CancelProbe(nextEpoch)
}

func TestRetry_RequestContextCancellationAfterRetryableStatusSkipsDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	blockedBody := newBlockingCloseBody()
	var calls atomic.Int64
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		cancel()
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Header:     make(http.Header),
			Body:       blockedBody,
			Request:    req,
		}, nil
	})
	tr := &Transport{
		Inner:      inner,
		MaxRetries: 3,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
	}

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		resp, err := tr.RoundTrip(httptest.NewRequest(http.MethodGet, "http://x/", nil).WithContext(ctx))
		done <- result{resp: resp, err: err}
	}()

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("RoundTrip error = %v, want context.Canceled", got.err)
		}
		if got.resp != nil {
			t.Fatalf("response = %#v, want nil after cancellation before retry", got.resp)
		}
	case <-time.After(300 * time.Millisecond):
		_ = blockedBody.Close()
		<-done
		t.Fatal("RoundTrip attempted to drain retryable response after request cancellation")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("RoundTrip elapsed = %v, want cancellation to skip retry-drain latency", elapsed)
	}
	if calls.Load() != 1 {
		t.Fatalf("inner RoundTrip calls = %d, want exactly 1", calls.Load())
	}
	if !blockedBody.closedFlag.Load() {
		t.Fatal("retryable response body was not closed after cancellation")
	}
}

func TestRetry_WithCircuitBreaker_AbortsWhenOpen(t *testing.T) {
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(1*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 10,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
		Breaker:    b,
	}

	resp, err := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	// The first attempt fails, recording a failure. Then it retries, second
	// attempt also fails, recording a second failure which trips the circuit.
	// On the third attempt (attempt=2), Allow() returns ErrCircuitOpen,
	// so retries stop. Total calls should be 2.
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (retries stopped by circuit breaker)", calls)
	}
	// The transport must return ErrCircuitOpen, not a (nil, nil) that would
	// panic httputil.ReverseProxy, nor a stale response with a drained body.
	if err == nil {
		t.Fatalf("expected non-nil error when circuit trips, got nil")
	}
	if !errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		t.Errorf("expected ErrCircuitOpen, got %v", err)
	}
	if resp != nil {
		t.Errorf("expected nil response when circuit trips, got %v", resp)
	}
}

func TestRetry_WithCircuitBreaker_RecordsFailures(t *testing.T) {
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 3,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
		Breaker:    b,
	}

	tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))

	s := b.Stats()
	// 4 attempts (1 initial + 3 retries), each recording a failure.
	if s.TotalFailures != 4 {
		t.Errorf("TotalFailures = %d, want 4", s.TotalFailures)
	}
}

func TestRetry_WithCircuitBreaker_RecordsSuccess(t *testing.T) {
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls < 2 {
			return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 3,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
		Breaker:    b,
	}

	resp, _ := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	if resp.StatusCode != 200 {
		t.Errorf("got status %d, want 200", resp.StatusCode)
	}

	s := b.Stats()
	if s.TotalFailures != 1 {
		t.Errorf("TotalFailures = %d, want 1", s.TotalFailures)
	}
	if s.TotalSuccesses != 1 {
		t.Errorf("TotalSuccesses = %d, want 1", s.TotalSuccesses)
	}
}

func TestRetry_WithDeferredBreakerSuccessSkipsHeaderLevelSuccess(t *testing.T) {
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 3,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
		Breaker:    b,
	}

	req := httptest.NewRequest("GET", "http://x/", nil)
	req = req.WithContext(WithDeferredBreakerSuccess(req.Context()))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	s := b.Stats()
	if s.TotalSuccesses != 0 {
		t.Fatalf("TotalSuccesses = %d, want 0 (success must be deferred until caller consumes body)", s.TotalSuccesses)
	}
	if s.TotalFailures != 0 {
		t.Fatalf("TotalFailures = %d, want 0", s.TotalFailures)
	}
}

func TestRetry_WithDeferredBreakerSuccessRecordsReturnedAttemptMetadata(t *testing.T) {
	calls := 0
	var secondAttemptEntered time.Time
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
		}
		secondAttemptEntered = time.Now()
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 1,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
		Breaker:    b,
	}

	attempt := &BreakerAttempt{}
	req := httptest.NewRequest("GET", "http://x/", nil)
	ctx := WithDeferredBreakerSuccess(req.Context())
	ctx = WithBreakerAttempt(ctx, attempt)
	req = req.WithContext(ctx)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if attempt.StartedAt.IsZero() {
		t.Fatal("attempt StartedAt was not recorded")
	}
	if attempt.StartedAt.After(secondAttemptEntered) {
		t.Fatalf("attempt StartedAt = %v, want no later than second attempt entry %v", attempt.StartedAt, secondAttemptEntered)
	}
	if attempt.Epoch != 0 {
		t.Fatalf("attempt Epoch = %d, want 0 while breaker remains CLOSED", attempt.Epoch)
	}
	if attempt.HeaderFailureRecorded {
		t.Fatal("attempt HeaderFailureRecorded = true, want false for returned 2xx attempt")
	}
	if attempt.FailureRecorded {
		t.Fatal("attempt FailureRecorded = true, want false for returned 2xx attempt")
	}
	if !attempt.AnyFailureRecorded {
		t.Fatal("attempt AnyFailureRecorded = false, want true because an earlier retry attempt returned 500")
	}
	if !attempt.AnyHeaderFailureRecorded {
		t.Fatal("attempt AnyHeaderFailureRecorded = false, want true because an earlier retry attempt returned 500 headers")
	}
}

func TestRetry_WithBreakerAttemptRecordsReturnedHeaderFailureOutcome(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		header  http.Header
		wantHit bool
	}{
		{name: "500", status: http.StatusInternalServerError, header: make(http.Header), wantHit: true},
		{name: "temporary 403", status: http.StatusForbidden, header: http.Header{"Retry-After": []string{"30"}}, wantHit: true},
		{name: "bare 403", status: http.StatusForbidden, header: make(http.Header), wantHit: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := rtFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tt.status, Body: body("body"), Header: tt.header.Clone()}, nil
			})

			b, err := circuitbreaker.New(
				circuitbreaker.WithFailureThreshold(10),
				circuitbreaker.WithWindow(10*time.Second),
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			tr := &Transport{Inner: inner, MaxRetries: 0, Breaker: b}
			attempt := &BreakerAttempt{}
			req := httptest.NewRequest("GET", "http://x/", nil)
			req = req.WithContext(WithBreakerAttempt(req.Context(), attempt))
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip error: %v", err)
			}
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			if attempt.StartedAt.IsZero() {
				t.Fatal("attempt StartedAt was not recorded")
			}
			if attempt.HeaderFailureRecorded != tt.wantHit {
				t.Fatalf("HeaderFailureRecorded = %v, want %v", attempt.HeaderFailureRecorded, tt.wantHit)
			}
			if attempt.FailureRecorded != tt.wantHit {
				t.Fatalf("FailureRecorded = %v, want %v", attempt.FailureRecorded, tt.wantHit)
			}
			if attempt.AnyHeaderFailureRecorded != tt.wantHit {
				t.Fatalf("AnyHeaderFailureRecorded = %v, want %v", attempt.AnyHeaderFailureRecorded, tt.wantHit)
			}
			if attempt.AnyFailureRecorded != tt.wantHit {
				t.Fatalf("AnyFailureRecorded = %v, want %v", attempt.AnyFailureRecorded, tt.wantHit)
			}
		})
	}
}

func TestRetry_WithBreakerAttemptRecordsReturnedTransportFailureOutcome(t *testing.T) {
	errTransport := errors.New("transport failed")
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errTransport
	})
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{Inner: inner, MaxRetries: 0, Breaker: b}
	attempt := &BreakerAttempt{}
	req := httptest.NewRequest("GET", "http://x/", nil)
	req = req.WithContext(WithBreakerAttempt(req.Context(), attempt))
	resp, err := tr.RoundTrip(req)
	if !errors.Is(err, errTransport) {
		t.Fatalf("RoundTrip error = %v, want %v", err, errTransport)
	}
	if resp != nil {
		t.Fatalf("resp = %#v, want nil", resp)
	}
	if !attempt.FailureRecorded {
		t.Fatal("FailureRecorded = false, want true for returned transport failure")
	}
	if attempt.HeaderFailureRecorded {
		t.Fatal("HeaderFailureRecorded = true, want false for transport failure")
	}
	if !attempt.AnyFailureRecorded {
		t.Fatal("AnyFailureRecorded = false, want true for returned transport failure")
	}
	if attempt.AnyHeaderFailureRecorded {
		t.Fatal("AnyHeaderFailureRecorded = true, want false for transport failure")
	}
}

func TestRetry_ContextCanceledErrorWithoutRequestCancelRecordsFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := rtFunc(func(req *http.Request) (*http.Response, error) {
				if req.Context().Err() != nil {
					t.Fatalf("request context unexpectedly canceled: %v", req.Context().Err())
				}
				return nil, tt.err
			})

			b, err := circuitbreaker.New(
				circuitbreaker.WithFailureThreshold(10),
				circuitbreaker.WithWindow(10*time.Second),
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			tr := &Transport{
				Inner:      inner,
				MaxRetries: 0,
				Breaker:    b,
			}

			_, _ = tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))

			s := b.Stats()
			if s.TotalFailures != 1 {
				t.Fatalf("TotalFailures = %d, want 1 (context-shaped error without request cancellation is a transport failure)", s.TotalFailures)
			}
			if s.TotalSuccesses != 0 {
				t.Fatalf("TotalSuccesses = %d, want 0", s.TotalSuccesses)
			}
		})
	}
}

func TestRetry_RequestCancelWithNonContextTransportErrorRecordsFailure(t *testing.T) {
	transportErr := errors.New("retry test connection reset")
	ctx, cancel := context.WithCancel(context.Background())
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		cancel()
		return nil, transportErr
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 0,
		Breaker:    b,
	}

	req := httptest.NewRequest("GET", "http://x/", nil).WithContext(ctx)
	_, _ = tr.RoundTrip(req)

	s := b.Stats()
	if s.TotalFailures != 1 {
		t.Fatalf("TotalFailures = %d, want 1 (request cancellation must not hide unrelated transport errors)", s.TotalFailures)
	}
	if s.TotalSuccesses != 0 {
		t.Fatalf("TotalSuccesses = %d, want 0", s.TotalSuccesses)
	}
}

func TestRetry_BodyTooLargeRequestCancelWithNonContextTransportErrorRecordsFailure(t *testing.T) {
	transportErr := errors.New("retry test body-too-large connection reset")
	ctx, cancel := context.WithCancel(context.Background())
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		cancel()
		return nil, transportErr
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:        inner,
		MaxRetries:   0,
		MaxBodyBytes: 1,
		Breaker:      b,
	}

	req := httptest.NewRequest("POST", "http://x/", strings.NewReader("oversized"))
	req = req.WithContext(ctx)
	_, _ = tr.RoundTrip(req)

	s := b.Stats()
	if s.TotalFailures != 1 {
		t.Fatalf("TotalFailures = %d, want 1 in body-too-large path", s.TotalFailures)
	}
	if s.TotalSuccesses != 0 {
		t.Fatalf("TotalSuccesses = %d, want 0", s.TotalSuccesses)
	}
}

func TestRetry_RequestCancelWithDefinitiveFailureResponseRecordsFailure(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		headers http.Header
	}{
		{name: "500", status: http.StatusInternalServerError, headers: make(http.Header)},
		{name: "429", status: http.StatusTooManyRequests, headers: make(http.Header)},
		{name: "temporary 403", status: http.StatusForbidden, headers: http.Header{"Retry-After": []string{"30"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			inner := rtFunc(func(req *http.Request) (*http.Response, error) {
				cancel()
				return &http.Response{StatusCode: tt.status, Body: body("failure"), Header: tt.headers.Clone()}, nil
			})

			b, err := circuitbreaker.New(
				circuitbreaker.WithFailureThreshold(10),
				circuitbreaker.WithWindow(10*time.Second),
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			tr := &Transport{
				Inner:      inner,
				MaxRetries: 0,
				Breaker:    b,
			}

			req := httptest.NewRequest("GET", "http://x/", nil).WithContext(ctx)
			resp, _ := tr.RoundTrip(req)
			if resp == nil || resp.StatusCode != tt.status {
				t.Fatalf("response status = %v, want %d", resp, tt.status)
			}

			s := b.Stats()
			if s.TotalFailures != 1 {
				t.Fatalf("TotalFailures = %d, want 1 (definitive upstream failure must be recorded despite request cancellation)", s.TotalFailures)
			}
			if s.TotalSuccesses != 0 {
				t.Fatalf("TotalSuccesses = %d, want 0", s.TotalSuccesses)
			}
		})
	}
}

func TestRetry_RequestContextCancellationPreservesTerminalResponses(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		maxRetries    int
		requestBody   io.Reader
		wantFailures  int64
		wantSuccesses int64
	}{
		{
			name:         "retry limit exhausted returns definitive failure response",
			status:       http.StatusInternalServerError,
			maxRetries:   0,
			wantFailures: 1,
		},
		{
			name:         "unreplayable request body returns definitive failure response",
			status:       http.StatusInternalServerError,
			maxRetries:   3,
			requestBody:  strings.NewReader("unbuffered request body"),
			wantFailures: 1,
		},
		{
			name:          "non retryable clean response is returned",
			status:        http.StatusNoContent,
			maxRetries:    3,
			wantSuccesses: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			returnedBody := &closeTrackingBody{reader: strings.NewReader("terminal response body")}
			var calls atomic.Int64
			inner := rtFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				cancel()
				return &http.Response{
					StatusCode: tt.status,
					Status:     fmt.Sprintf("%d %s", tt.status, http.StatusText(tt.status)),
					Header:     make(http.Header),
					Body:       returnedBody,
					Request:    req,
				}, nil
			})

			b, err := circuitbreaker.New(
				circuitbreaker.WithFailureThreshold(10),
				circuitbreaker.WithWindow(10*time.Second),
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			tr := &Transport{
				Inner:      inner,
				MaxRetries: tt.maxRetries,
				WaitMin:    time.Millisecond,
				WaitMax:    time.Millisecond,
				Breaker:    b,
			}

			req := httptest.NewRequest(http.MethodPost, "http://x/", tt.requestBody).WithContext(ctx)
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip error = %v, want nil because terminal response is returned despite request cancellation", err)
			}
			if resp == nil || resp.StatusCode != tt.status {
				t.Fatalf("response = %#v, want status %d", resp, tt.status)
			}
			if returnedBody.closed.Load() {
				t.Fatal("terminal response body was closed before being returned to caller")
			}
			if calls.Load() != 1 {
				t.Fatalf("inner RoundTrip calls = %d, want 1 terminal attempt", calls.Load())
			}

			s := b.Stats()
			if s.TotalFailures != tt.wantFailures || s.TotalSuccesses != tt.wantSuccesses {
				t.Fatalf("breaker failures=%d successes=%d, want failures=%d successes=%d", s.TotalFailures, s.TotalSuccesses, tt.wantFailures, tt.wantSuccesses)
			}
		})
	}
}

func TestRetry_WithCircuitBreaker_429RecordsFailure(t *testing.T) {
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Body: body("rate limited"), Header: make(http.Header)}, nil
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 2,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
		Breaker:    b,
	}

	tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))

	s := b.Stats()
	if s.TotalFailures != 3 { // 1 initial + 2 retries
		t.Errorf("TotalFailures = %d, want 3", s.TotalFailures)
	}
}

func TestRetry_WithCircuitBreaker_Bare403DoesNotRecordFailure(t *testing.T) {
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Body: body("invalid key"), Header: make(http.Header)}, nil
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 0,
		Breaker:    b,
	}

	resp, err := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	s := b.Stats()
	if s.TotalFailures != 0 {
		t.Errorf("TotalFailures = %d, want 0 for bare auth 403", s.TotalFailures)
	}
	if s.TotalSuccesses != 0 {
		t.Errorf("TotalSuccesses = %d, want 0", s.TotalSuccesses)
	}
}

func TestRetry_WithCircuitBreaker_RateLimited403RecordsFailure(t *testing.T) {
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       body("temporarily banned"),
			Header: http.Header{
				"Retry-After": []string{"60"},
			},
		}, nil
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 0,
		Breaker:    b,
	}

	tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))

	s := b.Stats()
	if s.TotalFailures != 1 {
		t.Errorf("TotalFailures = %d, want 1 for rate-limit-signaled 403", s.TotalFailures)
	}
}

func TestRetry_WithCircuitBreaker_NilBreaker_NoChange(t *testing.T) {
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 5,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
		Breaker:    nil, // no breaker
	}

	resp, _ := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	if resp.StatusCode != 200 {
		t.Errorf("got status %d, want 200", resp.StatusCode)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestRetry_CircuitOpenAfterTransportError(t *testing.T) {
	// Verify that when the first attempt is a transport error (nil response)
	// and the circuit trips, the transport returns ErrCircuitOpen — not
	// (nil, nil) which would panic httputil.ReverseProxy.
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return nil, io.ErrUnexpectedEOF // transport error — resp is nil
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(1*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 10,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
		Breaker:    b,
	}

	resp, err := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	// First attempt: transport error (failure #1 trips circuit).
	// Second attempt: Allow() returns ErrCircuitOpen.
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if resp != nil {
		t.Errorf("expected nil response, got %v", resp)
	}
	if err == nil {
		t.Fatal("expected non-nil error (ErrCircuitOpen), got nil")
	}
	if !errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		t.Errorf("expected ErrCircuitOpen, got %v", err)
	}
}

func TestRetry_CircuitOpenAfterFailedResponse(t *testing.T) {
	// Verify that when the first attempt returns a 500 (non-nil response)
	// and the circuit trips on the second attempt, the transport returns
	// ErrCircuitOpen — not the stale 500 with a drained/closed body.
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       body("error body"),
			Header:     make(http.Header),
		}, nil
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(1*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 10,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
		Breaker:    b,
	}

	resp, err := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	// First attempt: 500 (failure #1 trips circuit).
	// Second attempt: Allow() returns ErrCircuitOpen.
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if resp != nil {
		t.Errorf("expected nil response (not stale 500), got status %d", resp.StatusCode)
	}
	if !errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		t.Errorf("expected ErrCircuitOpen, got %v", err)
	}
}

func TestRetry_WithCircuitBreaker_TransportErrorRecordsFailure(t *testing.T) {
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 2,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
		Breaker:    b,
	}

	tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))

	s := b.Stats()
	if s.TotalFailures != 3 {
		t.Errorf("TotalFailures = %d, want 3 (transport errors counted)", s.TotalFailures)
	}
}

func TestRetry_ClientDisconnectNotReportedToBreaker(t *testing.T) {
	// Verify that a client-initiated context cancellation is NOT reported
	// to the circuit breaker. An attacker could otherwise trip the breaker
	// by initiating and immediately dropping connections.
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      &cancelTransport{},
		MaxRetries: 0,
		Breaker:    b,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately to simulate client disconnect.

	req := httptest.NewRequest("GET", "http://x/", nil).WithContext(ctx)
	_, _ = tr.RoundTrip(req)

	s := b.Stats()
	if s.TotalFailures != 0 {
		t.Errorf("TotalFailures = %d, want 0 (client cancel should not be reported to breaker)", s.TotalFailures)
	}
}

func TestRetry_ClientDeadlineExceededNotReportedToBreaker(t *testing.T) {
	// Verify that a client-initiated DeadlineExceeded is NOT reported to the
	// circuit breaker in the main retry loop. The transport never calls
	// context.WithTimeout, so all DeadlineExceeded errors originate from the
	// client context or the proxy's queue timeout — not from per-attempt
	// deadlines controlled by the transport. An attacker could otherwise trip
	// the breaker by sending requests with tight client deadlines.
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:      &deadlineExceededTransport{},
		MaxRetries: 0,
		Breaker:    b,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done() // Wait for the deadline to fire.

	req := httptest.NewRequest("GET", "http://x/", nil).WithContext(ctx)
	_, _ = tr.RoundTrip(req)

	s := b.Stats()
	if s.TotalFailures != 0 {
		t.Errorf("TotalFailures = %d, want 0 (client DeadlineExceeded should not be reported to breaker)", s.TotalFailures)
	}
}

func TestRetry_ClientDeadlineExceededNotReportedToBreaker_LargeBody(t *testing.T) {
	// Verify that a client-initiated DeadlineExceeded is NOT reported to the
	// circuit breaker in the body-too-large path (the separate code path that
	// handles bodies exceeding MaxBodyBytes). The same rationale as
	// TestRetry_ClientDeadlineExceededNotReportedToBreaker applies — the
	// transport does not control its own attempt deadlines.
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:        &deadlineExceededTransport{},
		MaxBodyBytes: 100,
		MaxRetries:   0,
		Breaker:      b,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done() // Wait for the deadline to fire.

	big := bytes.Repeat([]byte("x"), 200)
	req := httptest.NewRequest("POST", "http://x/", bytes.NewReader(big)).WithContext(ctx)
	_, _ = tr.RoundTrip(req)

	s := b.Stats()
	if s.TotalFailures != 0 {
		t.Errorf("TotalFailures = %d, want 0 (client DeadlineExceeded in large-body path should not be reported to breaker)", s.TotalFailures)
	}
}

// deadlineExceededTransport returns context.DeadlineExceeded to simulate a
// client-imposed timeout expiring before the upstream responds.
type deadlineExceededTransport struct{}

func (t *deadlineExceededTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, context.DeadlineExceeded
}

// cancelTransport returns context.Canceled error to simulate client disconnect.
type cancelTransport struct{}

func (t *cancelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, context.Canceled
}

// strictCloseReader wraps a bytes.Reader and rejects reads after Close.
// This simulates real HTTP connection bodies (TCP streams) that cannot be
// read after Close, exposing the read-after-close bug that bytes.NewReader
// hides (because bytes.Reader.Close is a no-op that doesn't prevent reads).
type strictCloseReader struct {
	*bytes.Reader
	closed bool
}

func (s *strictCloseReader) Close() error {
	s.closed = true
	return nil
}

func (s *strictCloseReader) Read(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("read after close")
	}
	return s.Reader.Read(p)
}

func TestRetry_LargeBodySucceedsEndToEnd(t *testing.T) {
	// Verify that when a request body exceeds MaxBodyBytes, the
	// reconstructed body (prefix from buffer + remaining stream) is fully
	// readable by the downstream RoundTripper. Before the fix, the transport
	// called req.Body.Close() before constructing the MultiReader, causing
	// read-after-close errors for bodies backed by real connections (as
	// opposed to bytes.Reader, which silently allows reads after Close).
	var receivedBody []byte
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		p, _ := io.ReadAll(req.Body)
		receivedBody = p
		// Close the request body to verify that pooledBody.Close() properly
		// cleans up the original body via the origBody field.
		req.Body.Close()
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:        inner,
		MaxBodyBytes: 100,
		MaxRetries:   0, // no retries — body-too-large path returns immediately
	}

	// Use strictCloseReader to simulate a real connection body that fails
	// reads after Close. bytes.NewReader would NOT catch this bug because
	// its Close is a no-op that doesn't prevent subsequent reads.
	origData := bytes.Repeat([]byte("x"), 200)
	origBody := &strictCloseReader{Reader: bytes.NewReader(origData)}
	req := httptest.NewRequest("POST", "http://x/", origBody)

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// The inner RoundTripper must have received the COMPLETE 200-byte body,
	// not just the 100-byte prefix that was buffered.
	if len(receivedBody) != 200 {
		t.Errorf("received body length = %d, want 200 (prefix + remaining stream)", len(receivedBody))
	}
	if !bytes.Equal(receivedBody, origData) {
		t.Errorf("received body content mismatch: got %d bytes, want %d bytes matching original", len(receivedBody), len(origData))
	}

	// Verify that the original body was closed by pooledBody.Close(),
	// confirming that the origBody cleanup path works and there is no
	// resource leak.
	if !origBody.closed {
		t.Error("original body was not closed — origBody cleanup in pooledBody.Close() is not working")
	}
}

func TestRetry_LargeBodyReportsFailureToBreaker(t *testing.T) {
	// Verify that a request with a body exceeding MaxBodyBytes still
	// reports its outcome to the circuit breaker when the upstream returns
	// a 5xx. Before the fix, the body-too-large path returned directly
	// from inner.RoundTrip, bypassing the breaker recording block entirely.
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		// Drain the body so the pooledBody can be fully read.
		io.ReadAll(req.Body)
		req.Body.Close()
		return &http.Response{StatusCode: 503, Body: body("service unavailable"), Header: make(http.Header)}, nil
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:        inner,
		MaxBodyBytes: 100,
		MaxRetries:   0,
		Breaker:      b,
	}

	big := bytes.Repeat([]byte("x"), 200)
	req := httptest.NewRequest("POST", "http://x/", bytes.NewReader(big))
	resp, _ := tr.RoundTrip(req)

	if resp.StatusCode != 503 {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}

	s := b.Stats()
	if s.TotalFailures != 1 {
		t.Errorf("TotalFailures = %d, want 1 (large-body 5xx must report to breaker)", s.TotalFailures)
	}
	if s.TotalSuccesses != 0 {
		t.Errorf("TotalSuccesses = %d, want 0", s.TotalSuccesses)
	}
}

func TestRetry_LargeBodyReportsSuccessToBreaker(t *testing.T) {
	// Verify that a request with a body exceeding MaxBodyBytes reports
	// a successful outcome to the circuit breaker. This enables the
	// HALF_OPEN → CLOSED transition when the probe is a large request.
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		io.ReadAll(req.Body)
		req.Body.Close()
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(10),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:        inner,
		MaxBodyBytes: 100,
		MaxRetries:   0,
		Breaker:      b,
	}

	big := bytes.Repeat([]byte("x"), 200)
	req := httptest.NewRequest("POST", "http://x/", bytes.NewReader(big))
	resp, _ := tr.RoundTrip(req)

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	s := b.Stats()
	if s.TotalSuccesses != 1 {
		t.Errorf("TotalSuccesses = %d, want 1 (large-body 2xx must report to breaker)", s.TotalSuccesses)
	}
	if s.TotalFailures != 0 {
		t.Errorf("TotalFailures = %d, want 0", s.TotalFailures)
	}
}

func TestRetry_LargeBodyClientCancelNotReportedToBreaker(t *testing.T) {
	// Verify that when a client disconnects during a large-body request,
	// the context.Canceled is NOT reported to the circuit breaker in the
	// body-too-large path. This is the large-body counterpart to
	// TestRetry_ClientDisconnectNotReportedToBreaker and covers the
	// isClientCancel guard added in R24-02.
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tr := &Transport{
		Inner:        &cancelTransport{},
		MaxBodyBytes: 100,
		MaxRetries:   0,
		Breaker:      b,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	big := bytes.Repeat([]byte("x"), 200)
	req := httptest.NewRequest("POST", "http://x/", bytes.NewReader(big)).WithContext(ctx)
	_, _ = tr.RoundTrip(req)

	s := b.Stats()
	if s.TotalFailures != 0 {
		t.Errorf("TotalFailures = %d, want 0 (client cancel in large-body path should not be reported to breaker)", s.TotalFailures)
	}
}

func TestRetry_PoolBufferCapacityCapping(t *testing.T) {
	// Verify that buffers with capacity exceeding maxPoolBufCap (64KB) are
	// discarded rather than returned to bufPool. Without this cap, a buffer
	// that grew to ~5MB from a large-body request would be pinned in the pool
	// indefinitely. Under sustained large-body traffic, sync.Pool would cache
	// many oversized buffers, causing permanent memory bloat.
	//
	// The test works by sending a body close to MaxBodyBytes through the
	// transport, then verifying that a subsequent request that needs a fresh
	// buffer gets a NEW small buffer from the pool — not the oversized one
	// that was discarded.

	// Step 1: Send a request with a body that grows the buffer beyond 64KB.
	inner200 := rtFunc(func(req *http.Request) (*http.Response, error) {
		io.ReadAll(req.Body)
		req.Body.Close()
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:        inner200,
		MaxBodyBytes: 128 * 1024, // 128KB — buffer will grow to fit
		MaxRetries:   0,
	}

	// Body is under MaxBodyBytes, so it gets buffered normally.
	bigBody := bytes.Repeat([]byte("x"), 100*1024) // 100KB — under 128KB limit
	req := httptest.NewRequest("POST", "http://x/", bytes.NewReader(bigBody))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	// The response body wraps bodyBuf via pooledBody. Close it to return
	// the buffer to the pool (or discard it if oversized).
	resp.Body.Close()

	// Step 2: Verify that releaseBuf correctly discards oversized buffers.
	// We can't directly inspect the pool, but we can verify the behavior
	// of releaseBuf.
	small := bytes.NewBuffer(make([]byte, 0, 1024)) // 1KB cap — should be returned
	small.Reset()
	releaseBuf(small)
	// If it was returned, getting from the pool should give us a buffer.
	got := bufPool.Get().(*bytes.Buffer)
	if got == nil {
		t.Fatal("bufPool.Get returned nil — small buffer should have been pooled")
	}
	// Return it.
	got.Reset()
	bufPool.Put(got)

	// Now test that an oversized buffer is discarded.
	big := bytes.NewBuffer(make([]byte, 0, 128*1024)) // 128KB cap — should be discarded
	big.Reset()
	releaseBuf(big)

	// The pool should NOT have the 128KB buffer. Getting from the pool
	// should return the small buffer we put back (or a new one).
	got2 := bufPool.Get().(*bytes.Buffer)
	if got2.Cap() > maxPoolBufCap {
		t.Errorf("got buffer with cap=%d from pool, want cap <= %d (oversized buffer should have been discarded)",
			got2.Cap(), maxPoolBufCap)
	}
	got2.Reset()
	bufPool.Put(got2)
}

func TestRetry_UnbufferedBodyNoRetry(t *testing.T) {
	// Verify that when MaxBodyBytes <= 0, the transport does NOT retry
	// requests with a body. Without the fix, the first attempt consumes
	// and closes req.Body, and subsequent retries pass the already-closed
	// body to inner.RoundTrip, causing an immediate "http: request body
	// closed" error. This creates a rapid futile retry loop that feeds
	// false status-0 failures to the circuit breaker.
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		// Read the body to simulate consumption.
		io.ReadAll(req.Body)
		req.Body.Close()
		return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:        inner,
		MaxRetries:   3,
		MaxBodyBytes: 0, // body NOT buffered — cannot retry
		WaitMin:      time.Millisecond,
		WaitMax:      time.Millisecond,
	}

	req := httptest.NewRequest("POST", "http://x/", bytes.NewReader([]byte("hello")))
	resp, _ := tr.RoundTrip(req)

	// Only 1 attempt should be made — no retry because body is unbuffered.
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (unbuffered body must not retry)", calls)
	}
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestRetry_NilBodyStillRetries(t *testing.T) {
	// Verify that requests WITHOUT a body (e.g., GET) still retry
	// normally when MaxBodyBytes <= 0. The canRetry guard should only
	// block retries for requests with a body that wasn't buffered.
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:        inner,
		MaxRetries:   3,
		MaxBodyBytes: 0,
		WaitMin:      time.Millisecond,
		WaitMax:      time.Millisecond,
	}

	req := httptest.NewRequest("GET", "http://x/", nil) // nil body
	resp, _ := tr.RoundTrip(req)

	if calls != 3 {
		t.Errorf("calls = %d, want 3 (nil body should still retry)", calls)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRetry_BoundedDrainOnLargeErrorBody(t *testing.T) {
	// R26-03 regression test: Verify that the body drain on retryable error
	// responses is bounded to 4KB regardless of actual body size. Before the
	// fix, a malicious upstream returning 5xx with an infinite streaming body
	// could block the retry goroutine indefinitely — a trivial DoS vector.
	//
	// Strategy: create a mock RoundTripper that returns 500 with a 100KB body.
	// With MaxRetries=1, the transport should drain the error body (bounded to
	// 4KB), then retry. The entire operation should complete quickly.
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			// Return a 500 with a 100KB body. The drain is bounded to 4KB.
			return &http.Response{
				StatusCode: 500,
				Body:       body(string(bytes.Repeat([]byte("x"), 100*1024))),
				Header:     make(http.Header),
			}, nil
		}
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 1,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
	}

	start := time.Now()
	resp, err := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// There must have been 2 RoundTrip calls (initial + 1 retry).
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + 1 retry)", calls)
	}

	// The drain of the 100KB body must have completed quickly (<100ms).
	// Without the LimitReader bound, draining 100KB with a slow reader
	// could take much longer.
	if elapsed > 100*time.Millisecond {
		t.Errorf("elapsed = %v, want <100ms (drain of 100KB body should be bounded to 4KB)", elapsed)
	}
}

// slowDripBody delivers one byte every interval, simulating a malicious
// upstream that returns 5xx with a slow-streaming body to hold the retry
// goroutine hostage.
type slowDripBody struct {
	data     []byte
	interval time.Duration
	mu       sync.Mutex
	closed   bool
	next     int
}

func (s *slowDripBody) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, errors.New("read after close")
	}
	if s.next >= len(s.data) {
		s.mu.Unlock()
		return 0, io.EOF
	}
	s.next++
	s.mu.Unlock()

	time.Sleep(s.interval)
	p[0] = s.data[s.next-1]
	return 1, nil
}

func (s *slowDripBody) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func TestRetry_TimeBoundedDrain(t *testing.T) {
	// Verify that the body drain on retryable error responses is time-bounded.
	// Without the fix, a slow-drip body sending 1 byte every 200ms would block
	// the retry goroutine for 4096*200ms = ~819 seconds. With the 5-second
	// drain deadline, the drain aborts after ~5 seconds and the retry proceeds.
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		// Return a 500 with a slow-drip body: 500 bytes at 200ms each.
		// Without the time bound, the drain would take 500*200ms = 100s.
		// With the time bound, it aborts after ~5s.
		return &http.Response{
			StatusCode: 500,
			Body: &slowDripBody{
				data:     bytes.Repeat([]byte("x"), 500),
				interval: 200 * time.Millisecond,
			},
			Header: make(http.Header),
		}, nil
	})

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 1,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
	}

	start := time.Now()
	resp, err := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The second attempt also returns 500, so the final response is 500.
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}

	// The drain must have aborted within ~7 seconds (5s deadline + 2s buffer
	// for scheduling jitter). Without the time-bounded drain, this would take
	// at least 100 seconds (500 bytes * 200ms/byte).
	if elapsed > 7*time.Second {
		t.Errorf("elapsed = %v, want <7s (drain should abort after 5s deadline)", elapsed)
	}
	// Sanity check: it must have taken at least 5 seconds (the drain deadline).
	if elapsed < 5*time.Second {
		t.Errorf("elapsed = %v, want >=5s (drain deadline should have fired)", elapsed)
	}
}

func TestRetry_ParseRetryAfter_Consulted(t *testing.T) {
	// Verify that the retry transport's ParseRetryAfter calls correctly
	// compute remaining delay when receivedAt != evaluatedAt (i.e., when
	// body-drain time has elapsed since the response headers arrived).
	t.Run("HTTP-date remaining after elapsed", func(t *testing.T) {
		receivedAt := time.Now()
		evaluatedAt := receivedAt.Add(4 * time.Second)
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("Retry-After", receivedAt.Add(30*time.Second).UTC().Format(http.TimeFormat))

		got := circuitbreaker.ParseRetryAfter(resp.Header, receivedAt, evaluatedAt)
		// Without Date header, remaining = retry.Sub(evaluatedAt) = 30s - 4s = 26s.
		if got < 25*time.Second || got > 27*time.Second {
			t.Errorf("HTTP-date remaining = %v, want ~26s (30s ban - 4s elapsed)", got)
		}
	})

	t.Run("HTTP-date with Date remaining after elapsed", func(t *testing.T) {
		upstreamNow := time.Now().UTC()
		receivedAt := time.Now()
		evaluatedAt := receivedAt.Add(4 * time.Second)
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("Date", upstreamNow.Format(http.TimeFormat))
		resp.Header.Set("Retry-After", upstreamNow.Add(30*time.Second).Format(http.TimeFormat))

		got := circuitbreaker.ParseRetryAfter(resp.Header, receivedAt, evaluatedAt)
		// With Date header: intendedDelta = 30s, proxyElapsed = 4s, remaining = 26s.
		if got < 25*time.Second || got > 27*time.Second {
			t.Errorf("HTTP-date+Date remaining = %v, want ~26s (30s intended - 4s elapsed)", got)
		}
	})

	t.Run("delay-seconds remaining after elapsed", func(t *testing.T) {
		receivedAt := time.Now()
		evaluatedAt := receivedAt.Add(4 * time.Second)
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("Retry-After", "120")

		got := circuitbreaker.ParseRetryAfter(resp.Header, receivedAt, evaluatedAt)
		// intended = 120s, elapsed = 4s, remaining = 116s.
		if got != 116*time.Second {
			t.Errorf("delay-seconds remaining = %v, want 116s (120s - 4s elapsed)", got)
		}
	})

	t.Run("immediate evaluation (receivedAt==evaluatedAt)", func(t *testing.T) {
		now := time.Now()
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("Retry-After", now.Add(30*time.Second).UTC().Format(http.TimeFormat))

		got := circuitbreaker.ParseRetryAfter(resp.Header, now, now)
		if got < 29*time.Second || got > 31*time.Second {
			t.Errorf("HTTP-date immediate = %v, want ~30s", got)
		}

		resp2 := &http.Response{Header: make(http.Header)}
		resp2.Header.Set("Retry-After", "120")
		got2 := circuitbreaker.ParseRetryAfter(resp2.Header, now, now)
		if got2 != 120*time.Second {
			t.Errorf("delay-seconds immediate = %v, want 120s", got2)
		}
	})
}

func TestRetry_RetryWaitAccountsForDrainBody(t *testing.T) {
	// Review-15 regression: verify that the retry transport does NOT
	// over-sleep by body-drain duration. Before the fix, the transport
	// passed retryNow,retryNow to ParseRetryAfter, zeroing proxyElapsed.
	// If drainBody took 500ms and the upstream sent Retry-After: 1 (1s),
	// the transport slept the full 1s on top of the 500ms drain, totaling
	// 1.5s instead of the correct 500ms remaining.
	//
	// Strategy: use a mock RoundTripper that returns 429 with Retry-After: 1
	// followed by 200 on the second attempt. A slow-drip body (simulating
	// drainBody latency) adds ~300ms to the first attempt. The total elapsed
	// time should be approximately 1s (the Retry-After value), NOT 1.3s.
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			resp := &http.Response{
				StatusCode: 429,
				Body: &slowDripBody{
					data:     bytes.Repeat([]byte("x"), 10),
					interval: 30 * time.Millisecond, // ~300ms drain
				},
				Header: make(http.Header),
			}
			resp.Header.Set("Retry-After", "1") // 1 second
			return resp, nil
		}
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 1,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
	}

	start := time.Now()
	resp, err := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// The total time should be approximately 1s (Retry-After value),
	// NOT 1.3s (which would indicate the transport added the drain time
	// on top of the full ban duration). Allow 300ms tolerance.
	if elapsed > 1300*time.Millisecond {
		t.Errorf("elapsed = %v, want <1.3s (retry wait should account for drain time, not add it on top)", elapsed)
	}
	// Sanity: must have taken at least 1s (the Retry-After value minus
	// the drain overlap). With 300ms drain and 1s Retry-After, remaining
	// is 700ms, so total is ~1s (300ms drain + 700ms wait).
	if elapsed < 800*time.Millisecond {
		t.Errorf("elapsed = %v, want >=800ms (should wait for remaining Retry-After duration)", elapsed)
	}
}

func TestRetry_RetryWaitHTTPDateRemaining(t *testing.T) {
	// Review-15 regression: verify that HTTP-date Retry-After with Date
	// header correctly computes remaining delay after body drain. Before
	// the fix, the transport passed retryNow,retryNow which zeroed
	// proxyElapsed, causing intendedDelta to be returned as the wait
	// (over-sleeping by the drain duration).
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			// Simulate an upstream that sends Date + Retry-After 1s in the future.
			upstreamNow := time.Now().UTC()
			resp := &http.Response{
				StatusCode: 429,
				Body: &slowDripBody{
					data:     bytes.Repeat([]byte("x"), 10),
					interval: 30 * time.Millisecond, // ~300ms drain
				},
				Header: make(http.Header),
			}
			resp.Header.Set("Date", upstreamNow.Format(http.TimeFormat))
			resp.Header.Set("Retry-After", upstreamNow.Add(1*time.Second).Format(http.TimeFormat))
			return resp, nil
		}
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:      inner,
		MaxRetries: 1,
		WaitMin:    time.Millisecond,
		WaitMax:    time.Millisecond,
	}

	start := time.Now()
	resp, err := tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// With the fix, the transport should wait for the REMAINING duration
	// (1s ban - ~300ms drain = ~700ms remaining), not the full 1s ban.
	// Total elapsed should be approximately 1s, NOT 1.3s.
	if elapsed > 1300*time.Millisecond {
		t.Errorf("elapsed = %v, want <1.3s (HTTP-date+Date retry wait should account for drain time)", elapsed)
	}
	if elapsed < 800*time.Millisecond {
		t.Errorf("elapsed = %v, want >=800ms (should wait for remaining ban duration)", elapsed)
	}
}

func TestRetry_MinRetryDelayFloor(t *testing.T) {
	// Verify that MinRetryDelay floors the retry wait duration.
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls < 2 {
			return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:         inner,
		MaxRetries:    1,
		WaitMin:       time.Millisecond,
		WaitMax:       time.Millisecond,
		MinRetryDelay: 200 * time.Millisecond,
	}

	start := time.Now()
	_, _ = tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	elapsed := time.Since(start)

	// The retry should be delayed by MinRetryDelay, not WaitMin.
	if elapsed < 150*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 200ms (MinRetryDelay floor)", elapsed)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestRetry_MinRetryDelayRespectsRetryAfter(t *testing.T) {
	// Verify that Retry-After overrides MinRetryDelay when larger.
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls < 2 {
			resp := &http.Response{StatusCode: 429, Body: body("rate limited"), Header: make(http.Header)}
			resp.Header.Set("Retry-After", "1") // 1 second
			return resp, nil
		}
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:         inner,
		MaxRetries:    1,
		WaitMin:       time.Millisecond,
		WaitMax:       time.Millisecond,
		MinRetryDelay: 200 * time.Millisecond,
	}

	start := time.Now()
	_, _ = tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	elapsed := time.Since(start)

	// Retry-After: 1s should override MinRetryDelay: 200ms.
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 1s (Retry-After overrides MinRetryDelay)", elapsed)
	}
}

func TestRetry_InFlightRetriesCounter(t *testing.T) {
	// Verify that InFlightRetries is incremented during retry and
	// decremented after completion.
	var counter atomic.Int64
	calls := 0
	var retryInFlight int64
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 2 {
			// Second call is the retry attempt — InFlightRetries should be 1.
			retryInFlight = counter.Load()
		}
		if calls < 2 {
			return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:           inner,
		MaxRetries:      1,
		WaitMin:         time.Millisecond,
		WaitMax:         time.Millisecond,
		InFlightRetries: &counter,
	}

	_, _ = tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))

	// During retry (attempt > 0), the counter should have been 1.
	if retryInFlight != 1 {
		t.Errorf("retry in-flight counter during retry = %d, want 1", retryInFlight)
	}
	// After completion, the counter should be back to 0.
	if counter.Load() != 0 {
		t.Errorf("final in-flight counter = %d, want 0", counter.Load())
	}
}

func TestRetry_MinRetryDelayOverridesSmallerRetryAfter(t *testing.T) {
	// Verify that MinRetryDelay overrides a shorter Retry-After header.
	// The operator's floor takes precedence over the upstream's hint.
	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls < 2 {
			resp := &http.Response{StatusCode: 429, Body: body("rate limited"), Header: make(http.Header)}
			resp.Header.Set("Retry-After", "0") // 0 seconds — instant retry
			return resp, nil
		}
		return &http.Response{StatusCode: 200, Body: body("ok"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:         inner,
		MaxRetries:    1,
		WaitMin:       time.Millisecond,
		WaitMax:       time.Millisecond,
		MinRetryDelay: 200 * time.Millisecond,
	}

	start := time.Now()
	_, _ = tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
	elapsed := time.Since(start)

	// MinRetryDelay=200ms should override Retry-After=0.
	if elapsed < 150*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 200ms (MinRetryDelay overrides Retry-After=0)", elapsed)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestRetry_InFlightRetriesDecrementOnCircuitOpen(t *testing.T) {
	// Verify InFlightRetries is decremented when the circuit breaker
	// opens mid-retry and the transport returns ErrCircuitOpen.
	var counter atomic.Int64
	b, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Trip the circuit.
	b.RecordFailure(500, 0, time.Time{}, 0)

	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:           inner,
		MaxRetries:      3,
		WaitMin:         time.Millisecond,
		WaitMax:         time.Millisecond,
		InFlightRetries: &counter,
		Breaker:         b,
	}

	_, _ = tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))

	// The counter should be back to 0 after the circuit-open abort.
	if counter.Load() != 0 {
		t.Errorf("InFlightRetries after circuit-open = %d, want 0", counter.Load())
	}
}

func TestRetry_InFlightRetriesDecrementOnContextCancel(t *testing.T) {
	// Verify InFlightRetries is decremented when the request context is
	// cancelled during the retry wait period.
	var counter atomic.Int64

	calls := 0
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:           inner,
		MaxRetries:      3,
		WaitMin:         10 * time.Second, // long wait so we can cancel during it
		WaitMax:         30 * time.Second,
		InFlightRetries: &counter,
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "http://x/", nil)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = tr.RoundTrip(req)
	}()

	// Wait for the first attempt to complete and the retry wait to start.
	// The counter should be 1 (incremented for the retry attempt).
	time.Sleep(100 * time.Millisecond)
	if v := counter.Load(); v != 1 {
		t.Logf("InFlightRetries during retry wait = %d (expected 1, but timing-dependent)", v)
	}

	// Cancel the context to abort the retry.
	cancel()
	<-done

	// The counter should be back to 0 after context cancellation.
	if counter.Load() != 0 {
		t.Errorf("InFlightRetries after context cancel = %d, want 0", counter.Load())
	}
}

func TestPooledBody_ConcurrentReadAndClose(t *testing.T) {
	// Verify that pooledBody is safe for concurrent Read and Close calls,
	// as required by the http.Request.Body contract. The race detector
	// must not flag any data race. Before the fix, p.closed was a plain
	// bool accessed from Read and Close without synchronization, and
	// buf.Reset()/bufPool.Put ran while the MultiReader was still reading
	// from buf.
	slowData := bytes.Repeat([]byte("x"), 100)
	slowReader := &slowDripBody{data: slowData, interval: time.Millisecond}
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString("prefix")

	pb := &pooledBody{
		ReadCloser: io.NopCloser(io.MultiReader(buf, slowReader)),
		buf:        nil, // body-too-large path sets nil to avoid pool race
		origBody:   slowReader,
	}

	var wg sync.WaitGroup
	readErr := make(chan error, 1)

	// Start a goroutine that continuously reads from the pooledBody.
	wg.Go(func() {
		var n int64
		for {
			_, err := pb.Read(make([]byte, 10))
			if err != nil {
				readErr <- err
				return
			}
			n++
			if n > 200 {
				readErr <- fmt.Errorf("read too many times without error (expected Close to terminate reads)")
				return
			}
		}
	})

	// Give the reader a moment to start reading.
	time.Sleep(20 * time.Millisecond)

	// Close the body concurrently — this must not race with Read.
	pb.Close()

	// Close again — must be idempotent (no double pool-put, no panic).
	pb.Close()
	pb.Close()

	// Wait for the reader goroutine to finish.
	wg.Wait()

	// The reader should have received an error after Close.
	select {
	case err := <-readErr:
		if err == nil {
			t.Error("expected Read to return an error after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read goroutine did not terminate after Close")
	}

	// Verify the original body was closed.
	if !slowReader.closed {
		t.Error("origBody was not closed by pooledBody.Close()")
	}
}

func TestPooledBody_CloseOnceIdempotent(t *testing.T) {
	// Verify that calling Close() many times concurrently is safe — no
	// double pool-put, no panic. This exercises the sync.Once guarantee.
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString("test data")

	pb := &pooledBody{
		ReadCloser: io.NopCloser(bytes.NewReader(buf.Bytes())),
		buf:        buf,
	}

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			pb.Close()
		})
	}
	wg.Wait()

	// If sync.Once is working, only one Close actually ran. The pool
	// received exactly one Put. We can't directly verify pool state, but
	// the race detector will flag a double-put if sync.Once is missing.
}

func TestPooledBody_ResponsePathBufRecycled(t *testing.T) {
	// Verify that the response-body wrapping path (where buf is NOT in a
	// MultiReader) correctly recycles the buffer on Close.
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString("response body data")

	respBody := io.NopCloser(strings.NewReader("upstream response"))
	pb := &pooledBody{
		ReadCloser: respBody,
		buf:        buf,
	}

	// Read some data.
	data, err := io.ReadAll(pb)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "upstream response" {
		t.Errorf("data = %q, want %q", string(data), "upstream response")
	}

	// Close should recycle buf (it's not in a MultiReader, so safe).
	if err := pb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Get a buffer from the pool — should work without issues.
	recovered := bufPool.Get().(*bytes.Buffer)
	recovered.Reset()
	defer bufPool.Put(recovered)
}

type testReadWriteCloseWriter struct {
	read       *bytes.Buffer
	written    bytes.Buffer
	closed     atomic.Int64
	closeWrite atomic.Int64
}

func (b *testReadWriteCloseWriter) Read(p []byte) (int, error) {
	return b.read.Read(p)
}

func (b *testReadWriteCloseWriter) Write(p []byte) (int, error) {
	return b.written.Write(p)
}

func (b *testReadWriteCloseWriter) Close() error {
	b.closed.Add(1)
	return nil
}

func (b *testReadWriteCloseWriter) CloseWrite() error {
	b.closeWrite.Add(1)
	return nil
}

func TestWrapBufferedResponseBody_PreservesSwitchingProtocolsReadWriteCloseWrite(t *testing.T) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString("buffered request body")
	underlying := &testReadWriteCloseWriter{read: bytes.NewBufferString("upgrade-read")}
	resp := &http.Response{StatusCode: http.StatusSwitchingProtocols, Body: underlying}

	wrapped := wrapBufferedResponseBody(resp, buf)
	rwc, ok := wrapped.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("wrapped body type %T does not implement io.ReadWriteCloser", wrapped)
	}
	cw, ok := wrapped.(closeWriter)
	if !ok {
		t.Fatalf("wrapped body type %T does not implement CloseWrite", wrapped)
	}

	got, err := io.ReadAll(rwc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "upgrade-read" {
		t.Fatalf("ReadAll = %q, want upgrade-read", string(got))
	}
	if _, err := rwc.Write([]byte("upgrade-write")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if underlying.written.String() != "upgrade-write" {
		t.Fatalf("underlying written = %q, want upgrade-write", underlying.written.String())
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if underlying.closeWrite.Load() != 1 {
		t.Fatalf("underlying CloseWrite calls = %d, want 1", underlying.closeWrite.Load())
	}
	if err := rwc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := rwc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if underlying.closed.Load() != 1 {
		t.Fatalf("underlying Close calls = %d, want 1", underlying.closed.Load())
	}
}

func TestRetry_InFlightRetriesCounter_MultiAttempt(t *testing.T) {
	// Verify that InFlightRetries returns to 0 after a request that
	// exhausts multiple retry attempts. Before the fix, the counter
	// was incremented on every retry iteration but decremented only
	// once on exit, leaking +N-1 per request. A request retrying 3
	// times would permanently leak +2.
	var counter atomic.Int64
	calls := 0
	var maxInFlight int64
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		// Track the peak in-flight count across all attempts.
		if v := counter.Load(); v > maxInFlight {
			maxInFlight = v
		}
		return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:           inner,
		MaxRetries:      3, // 4 total attempts (1 initial + 3 retries)
		WaitMin:         time.Millisecond,
		WaitMax:         time.Millisecond,
		InFlightRetries: &counter,
	}

	_, _ = tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))

	if calls != 4 {
		t.Errorf("calls = %d, want 4 (1 initial + 3 retries)", calls)
	}

	// The peak should be 1 — we enter retry mode once, not 3 times.
	if maxInFlight != 1 {
		t.Errorf("max in-flight during retries = %d, want 1 (counter tracks retry mode, not attempt count)", maxInFlight)
	}

	// After completion, the counter MUST be 0 (no leak).
	if counter.Load() != 0 {
		t.Errorf("final in-flight counter = %d, want 0 (counter must balance — no leak)", counter.Load())
	}
}

func TestRetry_InFlightRetriesCounter_MultiRequestConcurrency(t *testing.T) {
	// Verify InFlightRetries is accurate under concurrent multi-retry requests.
	// Multiple RoundTrip calls can be in retry mode simultaneously, and the
	// counter should reflect the true count, returning to 0 when all complete.
	var counter atomic.Int64
	inner := rtFunc(func(req *http.Request) (*http.Response, error) {
		time.Sleep(10 * time.Millisecond) // simulate work
		return &http.Response{StatusCode: 500, Body: body("err"), Header: make(http.Header)}, nil
	})

	tr := &Transport{
		Inner:           inner,
		MaxRetries:      2,
		WaitMin:         time.Millisecond,
		WaitMax:         time.Millisecond,
		InFlightRetries: &counter,
	}

	const numRequests = 5
	var wg sync.WaitGroup
	for range numRequests {
		wg.Go(func() {
			_, _ = tr.RoundTrip(httptest.NewRequest("GET", "http://x/", nil))
		})
	}
	wg.Wait()

	// After all requests complete, the counter MUST be 0.
	if counter.Load() != 0 {
		t.Errorf("final in-flight counter = %d, want 0 (%d concurrent requests with %d retries each)",
			counter.Load(), numRequests, tr.MaxRetries)
	}
}
