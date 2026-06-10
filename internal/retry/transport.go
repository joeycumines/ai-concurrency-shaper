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

// Package retry implements a retrying HTTP transport.
package retry

import (
	"bytes"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// CheckRetry decides whether a failed attempt should be retried.
type CheckRetry func(resp *http.Response, err error) bool

// DefaultCheckRetry retries on 5xx, 429, and transport errors.
var DefaultCheckRetry CheckRetry = func(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
}

var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// Transport is an http.RoundTripper that retries transient failures.
type Transport struct {
	Inner        http.RoundTripper
	MaxRetries   int
	MaxBodyBytes int64
	CheckRetry   CheckRetry
	WaitMin      time.Duration
	WaitMax      time.Duration
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	inner := t.Inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	shouldRetry := t.CheckRetry
	if shouldRetry == nil {
		shouldRetry = DefaultCheckRetry
	}

	var bodyBuf *bytes.Buffer
	if req.Body != nil && t.MaxBodyBytes > 0 {
		buf := bufPool.Get().(*bytes.Buffer)
		buf.Reset()
		if _, err := io.Copy(buf, io.LimitReader(req.Body, t.MaxBodyBytes+1)); err != nil {
			req.Body.Close()
			bufPool.Put(buf)
			return nil, err
		}
		if int64(buf.Len()) > t.MaxBodyBytes {
			// Body too large to buffer; reconstruct with prefix + remaining stream.
			req.Body.Close()
			req.Body = &pooledBody{
				ReadCloser: io.NopCloser(io.MultiReader(buf, req.Body)),
				buf:        buf,
			}
			return inner.RoundTrip(req)
		}
		req.Body.Close()
		bodyBuf = buf
	}

	var lastResp *http.Response
	for attempt := 0; ; attempt++ {
		if bodyBuf != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBuf.Bytes()))
			req.ContentLength = int64(bodyBuf.Len())
			if req.Header != nil {
				req.Header.Set("Content-Length", strconv.Itoa(bodyBuf.Len()))
			}
		}

		if attempt > 0 {
			wait := calcWait(attempt, t.WaitMin, t.WaitMax)
			if ra := parseRetryAfter(lastResp); ra > 0 && ra > wait {
				wait = ra
			}
			timer := time.NewTimer(wait)
			select {
			case <-req.Context().Done():
				timer.Stop()
				if bodyBuf != nil {
					bodyBuf.Reset()
					bufPool.Put(bodyBuf)
				}
				return nil, req.Context().Err()
			case <-timer.C:
			}
		}

		resp, err := inner.RoundTrip(req)

		mustRetry := shouldRetry(resp, err)
		atLimit := t.MaxRetries >= 0 && attempt >= t.MaxRetries

		if !mustRetry || atLimit {
			if bodyBuf != nil {
				if resp != nil && resp.Body != nil {
					resp.Body = &pooledBody{ReadCloser: resp.Body, buf: bodyBuf}
				} else {
					bodyBuf.Reset()
					bufPool.Put(bodyBuf)
				}
			}
			return resp, err
		}

		// Drain the body so the connection can be reused.
		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		lastResp = resp
	}
}

func calcWait(attempt int, wMin, wMax time.Duration) time.Duration {
	if wMin <= 0 {
		wMin = 500 * time.Millisecond
	}
	if wMax <= 0 {
		wMax = 30 * time.Second
	}
	base := wMin
	for i := 1; i < attempt; i++ {
		base *= 2
		if base >= wMax {
			base = wMax
			break
		}
	}
	// ±25% jitter (rand.Int64N uses the global auto-seeded source, which is
	// safe for concurrent use and does not require manual seeding since Go 1.22).
	if j := int64(float64(base) * 0.25); j > 0 {
		base += time.Duration(rand.Int64N(2*int64(j)+1) - j)
	}
	if base < wMin {
		return wMin
	}
	if base > wMax {
		return wMax
	}
	return base
}

func parseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	sec, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return time.Duration(sec) * time.Second
}

type pooledBody struct {
	io.ReadCloser
	buf *bytes.Buffer
}

func (p *pooledBody) Close() error {
	err := p.ReadCloser.Close()
	if p.buf != nil {
		p.buf.Reset()
		bufPool.Put(p.buf)
		p.buf = nil
	}
	return err
}
