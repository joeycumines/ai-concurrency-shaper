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

package proxy

// The early queue-comment commit replaces a request's whole response with a
// 200 event stream. These tests pin the consequences of choosing that
// representation for a response that cannot be a body-bearing stream, and pin
// that the choice never changes admission.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// newLimitAllProxy builds a blanket-limited proxy (every route queued) whose
// upstream answers with the supplied handler.
func newLimitAllProxy(t *testing.T, upstream http.Handler, opts ...Option) *Proxy {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := []Option{
		WithUpstream(u),
		WithMatcher(route.NewMatcher(nil)),
		WithLimitAll(true),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(10 * time.Second),
		WithMaxRetries(0),
	}
	p, err := New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p
}

// rawRequest writes a request verbatim over a TCP connection and returns the
// parsed response. It is how a client can vary framing details (upgrade
// headers, CONNECT) that http.Client would normalise.
func rawRequest(t *testing.T, proxyURL, request string) (*http.Response, []byte) {
	t.Helper()
	addr := strings.TrimPrefix(proxyURL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, strings.ReplaceAll(request, "{addr}", addr)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	resp.Body.Close()
	return resp, body
}

// TestProxyQueueCommentsHeadPreservesUpstreamStatus pins that a HEAD request
// under -queue-comments reaches the upstream without having its response
// representation replaced: net/http discards HEAD body writes, so the commit
// would have produced a bodiless 200 event-stream and dropped the upstream
// status and Retry-After the client is asking about.
func TestProxyQueueCommentsHeadPreservesUpstreamStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			p := newLimitAllProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.Header().Set("X-Upstream-Status", fmt.Sprint(status))
				w.WriteHeader(status)
			}), WithQueueComments(10*time.Millisecond))
			srv := httptest.NewServer(p)
			t.Cleanup(srv.Close)

			req, err := http.NewRequest(http.MethodHead, srv.URL+"/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Accept", "text/event-stream")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("HEAD: %v", err)
			}
			resp.Body.Close()

			if resp.StatusCode != status {
				t.Fatalf("HEAD status = %d, want the upstream's %d", resp.StatusCode, status)
			}
			if got := resp.Header.Get("Retry-After"); got != "7" {
				t.Fatalf("Retry-After = %q, want the upstream's 7", got)
			}
			if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
				t.Fatalf("HEAD Content-Type = %q, want the upstream representation", ct)
			}
			if got := resp.Header.Get("X-Upstream-Status"); got != fmt.Sprint(status) {
				t.Fatalf("X-Upstream-Status = %q, want the upstream's status header", got)
			}
		})
	}
}

// TestProxyQueueCommentsConnectPreservesError pins that a CONNECT request is
// not given the committed event stream. ReverseProxy rejects CONNECT locally,
// so a commit would present a synthetic 200 stream instead of the real error.
func TestProxyQueueCommentsConnectPreservesError(t *testing.T) {
	var upstreamHits atomic.Int64
	p := newLimitAllProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamHits.Add(1)
	}), WithQueueComments(10*time.Millisecond))
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	resp, body := rawRequest(t, srv.URL, "CONNECT example.com:443 HTTP/1.1\r\nHost: {addr}\r\nAccept: text/event-stream\r\n\r\n")
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("CONNECT status = 200 body=%q, want the real error instead of a committed stream", string(body))
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		t.Fatalf("CONNECT Content-Type = %q, want no committed event stream", ct)
	}
	if strings.Contains(string(body), "queue-wait") {
		t.Fatalf("CONNECT body = %q, want no queue-comment bytes", string(body))
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("upstream hits = %d, want 0: ReverseProxy refuses CONNECT locally", got)
	}
}

// TestProxyQueueCommentsUpgradeHeaderRefusalIsPlainHTTP pins the classifier
// boundary: any request the native or transcode handler refuses as an upgrade
// must not have its refusal turned into a 200 error stream by the commit.
func TestProxyQueueCommentsUpgradeHeaderRefusalIsPlainHTTP(t *testing.T) {
	t.Run("transcoded route", func(t *testing.T) {
		p, err := New(
			WithUpstream(mustURL(t, "http://upstream.invalid")),
			WithMatcher(route.NewMatcher([]route.Pattern{mustRoutePattern(t, "POST /v1/responses")})),
			WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
			WithMetrics(metrics.NewCollector()),
			WithQueueComments(10*time.Millisecond),
			WithQueueTimeout(10*time.Second),
			WithMaxRetries(0),
			WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
		)
		if err != nil {
			t.Fatalf("proxy.New: %v", err)
		}
		srv := httptest.NewServer(p)
		t.Cleanup(srv.Close)

		body := `{"model":"m","input":"x","stream":true}`
		resp, got := rawRequest(t, srv.URL,
			"POST /v1/responses HTTP/1.1\r\nHost: {addr}\r\nAccept: text/event-stream\r\nUpgrade: websocket\r\n"+
				"Content-Type: application/json\r\nContent-Length: "+fmt.Sprint(len(body))+"\r\n\r\n"+body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d body=%q, want a plain 400 for an upgrade request on a transcoded route", resp.StatusCode, string(got))
		}
		if strings.Contains(string(got), "event: error") {
			t.Fatalf("body = %q, want an HTTP error, not an in-stream error event", string(got))
		}
	})

	t.Run("native route", func(t *testing.T) {
		key, err := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
		if err != nil {
			t.Fatal(err)
		}
		p, err := New(
			WithUpstream(mustURL(t, "http://upstream.invalid")),
			WithMatcher(route.NewMatcher([]route.Pattern{mustRoutePattern(t, "POST /v1/messages")})),
			WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
			WithMetrics(metrics.NewCollector()),
			WithQueueComments(10*time.Millisecond),
			WithQueueTimeout(10*time.Second),
			WithMaxRetries(0),
			WithNativeRoutes(NativeRoute{
				RouteKey: key,
				Protocol: transcode.NativeMessages,
				ModelMap: transcode.ModelMap{AllowIdentity: true},
			}),
		)
		if err != nil {
			t.Fatalf("proxy.New: %v", err)
		}
		srv := httptest.NewServer(p)
		t.Cleanup(srv.Close)

		body := `{"model":"m","max_tokens":8,"messages":[]}`
		resp, got := rawRequest(t, srv.URL,
			"POST /v1/messages HTTP/1.1\r\nHost: {addr}\r\nAccept: text/event-stream\r\nUpgrade: websocket\r\n"+
				"Content-Type: application/json\r\nContent-Length: "+fmt.Sprint(len(body))+"\r\n\r\n"+body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d body=%q, want a plain 400 for an upgrade request on a native route", resp.StatusCode, string(got))
		}
		if strings.Contains(string(got), "event: error") {
			t.Fatalf("body = %q, want an HTTP error, not an in-stream error event", string(got))
		}
	})
}

// TestProxyQueueCommentsUpgradeHeaderTransparentPassthrough pins that the
// broader upgrade classifier costs only comment bytes on a route that forwards
// the request transparently: the request is still forwarded, and its response
// is untouched.
func TestProxyQueueCommentsUpgradeHeaderTransparentPassthrough(t *testing.T) {
	var hits atomic.Int64
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	p := newLimitAllProxy(t, upstream, WithQueueComments(10*time.Millisecond))
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	resp, body := rawRequest(t, srv.URL,
		"POST /v1/responses HTTP/1.1\r\nHost: {addr}\r\nAccept: text/event-stream\r\nUpgrade: websocket\r\nContent-Length: 0\r\n\r\n")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%q, want the upstream's 200", resp.StatusCode, string(body))
	}
	if got := string(body); got != `{"ok":true}` {
		t.Fatalf("body = %q, want the upstream's own body with no comment bytes", got)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
}

// TestProxyQueueCommentsGateNeverChangesAdmission proves the comment gate is
// cosmetic under -limit-all: with the single slot held, HEAD, CONNECT, and an
// upgrade request all wait for release, and the depth bound still rejects
// them. The same assertions run with comments enabled and disabled.
func TestProxyQueueCommentsGateNeverChangesAdmission(t *testing.T) {
	for _, comments := range []time.Duration{0, 10 * time.Millisecond} {
		t.Run("comments="+comments.String(), func(t *testing.T) {
			release := make(chan struct{})
			var reached atomic.Int64
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached.Add(1)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.WriteHeader(http.StatusOK)
			})
			p := newLimitAllProxy(t, upstream,
				WithQueueComments(comments),
				WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
			)
			srv := httptest.NewServer(p)
			t.Cleanup(srv.Close)

			// Occupy the single slot.
			holderDone := make(chan struct{})
			go func() {
				defer close(holderDone)
				resp, err := http.Post(srv.URL+"/v1/messages", "application/json", nil)
				if err == nil {
					resp.Body.Close()
				}
			}()
			deadline := time.Now().Add(5 * time.Second)
			for reached.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if reached.Load() == 0 {
				t.Fatal("holder request never reached the upstream")
			}

			// Each queued connection must outlive close(release) so its
			// response can be read after the slot is freed: the conns are
			// closed at the outer level, after the post-release assertions.
			var conns []net.Conn
			defer func() {
				for _, c := range conns {
					_ = c.Close()
				}
			}()

			for _, tc := range []struct {
				name    string
				request string
			}{
				{name: "head", request: "HEAD /v1/messages HTTP/1.1\r\nHost: {addr}\r\nAccept: text/event-stream\r\n\r\n"},
				{name: "connect", request: "CONNECT example.com:443 HTTP/1.1\r\nHost: {addr}\r\nAccept: text/event-stream\r\n\r\n"},
				{name: "upgrade", request: "POST /v1/messages HTTP/1.1\r\nHost: {addr}\r\nAccept: text/event-stream\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nContent-Length: 0\r\n\r\n"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					addr := strings.TrimPrefix(srv.URL, "http://")
					conn, err := net.Dial("tcp", addr)
					if err != nil {
						t.Fatalf("dial: %v", err)
					}
					conns = append(conns, conn)
					if _, err := io.WriteString(conn, strings.ReplaceAll(tc.request, "{addr}", addr)); err != nil {
						t.Fatalf("write: %v", err)
					}
					if err := conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond)); err != nil {
						t.Fatalf("set deadline: %v", err)
					}
					// Nothing may come back while the only slot is held.
					if _, err := http.ReadResponse(bufio.NewReader(conn), nil); err == nil {
						t.Fatal("request was answered while the only slot was held: the gate must not affect admission")
					}
					if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
						t.Fatalf("set deadline: %v", err)
					}
				})
			}

			close(release)
			<-holderDone

			// With the slot released, every queued request must actually be
			// admitted and answered — the gate never changed admission, so
			// none of these reads may time out.
			for i, tc := range []string{"head", "connect", "upgrade"} {
				resp, err := http.ReadResponse(bufio.NewReader(conns[i]), nil)
				if err != nil {
					t.Fatalf("%s: no response after slot release (10s deadline): %v", tc, err)
				}
				_, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode < 200 || resp.StatusCode >= 600 {
					t.Fatalf("%s: invalid status %d after release", tc, resp.StatusCode)
				}
			}
		})
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustRoutePattern(t *testing.T, spec string) route.Pattern {
	t.Helper()
	pat, err := route.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	return pat
}

// TestProxyQueueCommentsGateRespectsQueueDepth pins that the gate does not
// exempt an excluded request from the bounded-queue rejection: with the slot
// held and the depth bound full, a HEAD still gets the documented 429.
func TestProxyQueueCommentsGateRespectsQueueDepth(t *testing.T) {
	for _, comments := range []time.Duration{0, 10 * time.Millisecond} {
		t.Run("comments="+comments.String(), func(t *testing.T) {
			release := make(chan struct{})
			var reached atomic.Int64
			p := newLimitAllProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached.Add(1)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.WriteHeader(http.StatusOK)
			}),
				WithQueueComments(comments),
				WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
				WithQueueDepthLimit(1),
			)
			srv := httptest.NewServer(p)
			t.Cleanup(srv.Close)

			holderDone := make(chan struct{})
			go func() {
				defer close(holderDone)
				resp, err := http.Post(srv.URL+"/v1/messages", "application/json", nil)
				if err == nil {
					resp.Body.Close()
				}
			}()
			deadline := time.Now().Add(5 * time.Second)
			for reached.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}

			// One waiter fills the depth bound. Poll the limiter's waiter
			// count instead of sleeping a fixed interval.
			waiterDone := make(chan struct{})
			go func() {
				defer close(waiterDone)
				resp, err := http.Post(srv.URL+"/v1/messages", "application/json", nil)
				if err == nil {
					resp.Body.Close()
				}
			}()
			deadline = time.Now().Add(5 * time.Second)
			for p.limiter.Stats().Waiters < 1 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if p.limiter.Stats().Waiters < 1 {
				t.Fatal("waiter never registered with the limiter")
			}

			req, err := http.NewRequest(http.MethodHead, srv.URL+"/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Accept", "text/event-stream")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("HEAD: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("HEAD status = %d, want 429 from the queue-depth bound", resp.StatusCode)
			}

			close(release)
			<-holderDone
			<-waiterDone
		})
	}
}

// TestProxyCancelCooldownCoversAbandonedUpstreamHandoff pins that a local
// upgrade handoff failure, where the upstream already answered 101 and the
// proxy closed that connection, holds the slot for the cancel cooldown even
// when -release-cooldown is 0. The limiter's own post-release cooldown must not
// be the only thing covering the upstream's teardown.
func TestProxyCancelCooldownCoversAbandonedUpstreamHandoff(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cancelCooldown time.Duration
		wantHold       bool
	}{
		{name: "cancel cooldown holds the slot", cancelCooldown: 300 * time.Millisecond, wantHold: true},
		{name: "zero cancel cooldown releases immediately", cancelCooldown: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int64
			p, err := New(
				WithUpstream(mustURL(t, "http://upstream.invalid")),
				WithMatcher(route.NewMatcher([]route.Pattern{mustRoutePattern(t, "POST /v1/messages")})),
				// Release cooldown off: only the cancel-cooldown branch can hold.
				WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
				WithMetrics(metrics.NewCollector()),
				WithQueueTimeout(10*time.Second),
				WithMaxRetries(0),
				WithCancelCooldown(tc.cancelCooldown),
				WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if hits.Add(1) == 1 {
						return &http.Response{
							StatusCode:    http.StatusSwitchingProtocols,
							Status:        "101 Switching Protocols",
							Header:        http.Header{"Connection": {"Upgrade"}, "Upgrade": {"testproto"}},
							Body:          &closeTrackingReadWriteCloser{},
							ContentLength: -1,
							Request:       req,
						}, nil
					}
					return &http.Response{
						StatusCode:    http.StatusOK,
						Status:        "200 OK",
						Header:        http.Header{"Content-Type": {"application/json"}},
						Body:          io.NopCloser(strings.NewReader(`{}`)),
						ContentLength: 2,
						Request:       req,
					}, nil
				})),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			// A writer that cannot hijack forces the pre-hijack local failure:
			// the upstream's 101 is closed by the proxy and the client is told
			// to retry with a 502.
			upgrade, err := http.NewRequest(http.MethodPost, "http://proxy.invalid/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			upgrade.Header.Set("Connection", "Upgrade")
			upgrade.Header.Set("Upgrade", "testproto")
			rw := &failedHijackResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: http.ErrNotSupported}
			p.ServeHTTP(rw, upgrade)
			if rw.Code != http.StatusBadGateway {
				t.Fatalf("upgrade status = %d, want the local 502", rw.Code)
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("upstream hits = %d, want exactly 1 before the local handoff failure", got)
			}

			start := time.Now()
			next := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			p.ServeHTTP(httptest.NewRecorder(), next)
			waited := time.Since(start)

			if tc.wantHold {
				if waited < tc.cancelCooldown/2 {
					t.Fatalf("follow-up admitted after %v, want a hold near %v while the upstream tears down the abandoned 101", waited, tc.cancelCooldown)
				}
			} else if waited > 150*time.Millisecond {
				t.Fatalf("follow-up admitted after %v with a zero cancel cooldown", waited)
			}
		})
	}
}

// TestProxyCancelCooldownCoversAbandonedUpstreamHandoffPassthrough pins that
// the same hold applies to the global-limiter slot on a passthrough route:
// both admission paths consult one predicate, so the two cannot drift.
func TestProxyCancelCooldownCoversAbandonedUpstreamHandoffPassthrough(t *testing.T) {
	var hits atomic.Int64
	p, err := New(
		WithUpstream(mustURL(t, "http://upstream.invalid")),
		// No limited pattern: the request is passthrough and takes only the
		// global limiter's slot.
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithQueueTimeout(10*time.Second),
		WithMaxRetries(0),
		WithCancelCooldown(300*time.Millisecond),
		WithTransport(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if hits.Add(1) == 1 {
				return &http.Response{
					StatusCode:    http.StatusSwitchingProtocols,
					Status:        "101 Switching Protocols",
					Header:        http.Header{"Connection": {"Upgrade"}, "Upgrade": {"testproto"}},
					Body:          &closeTrackingReadWriteCloser{},
					ContentLength: -1,
					Request:       req,
				}, nil
			}
			return &http.Response{
				StatusCode:    http.StatusOK,
				Status:        "200 OK",
				Header:        http.Header{"Content-Type": {"application/json"}},
				Body:          io.NopCloser(strings.NewReader(`{}`)),
				ContentLength: 2,
				Request:       req,
			}, nil
		})),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	upgrade, err := http.NewRequest(http.MethodGet, "http://proxy.invalid/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "testproto")
	rw := &failedHijackResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: http.ErrNotSupported}
	p.ServeHTTP(rw, upgrade)
	if rw.Code != http.StatusBadGateway {
		t.Fatalf("upgrade status = %d, want the local 502", rw.Code)
	}

	start := time.Now()
	p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Fatalf("follow-up admitted after %v, want the global slot held for the cancel cooldown", waited)
	}
}
