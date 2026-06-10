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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// rtFunc is a RoundTripper implemented by a function.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func body(s string) io.ReadCloser {
	return io.NopCloser(bytes.NewBufferString(s))
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
