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

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// What the client RECEIVES from a natively served route. The proxy's contract
// on this path is that it rewrites the model identifier and forwards the
// document otherwise byte-identically, so these tests assert on emitted bytes
// rather than on decoded fields: a whole-document re-encode to change one
// value is semantically invisible to a JSON decoder and still a violation.
// Routing and request-side validation are in native_test.go.

// TestProxyNativeStructuralErrorIsBounded covers the third local rejection,
// the one that quotes a key name straight out of the request: the tolerant
// decode reports a duplicate key as `duplicate JSON key %q`, so that message
// text is client-controlled and the dialect writer escapes it. Every local
// rejection on this path has to be bounded, not just the ones that quote the
// model identifier.
func TestProxyNativeStructuralErrorIsBounded(t *testing.T) {
	up := &nativeUpstream{response: `{}`}
	p, _ := newNativeProxy(t, up, nativeRoute(t, transcode.NativeMessages, "/v1/messages"))

	// A duplicated key whose name is the client-controlled payload.
	huge := strings.Repeat("<", 200<<10)
	body := `{"model":"msg-model","` + huge + `":1,"` + huge + `":2}`
	rec := postNative(t, p, "/v1/messages", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: a duplicate key must be refused: %s", rec.Code, rec.Body.String())
	}
	if got, limit := rec.Body.Len(), 32<<10; got > limit {
		t.Fatalf("error body is %d bytes for a %d byte request (limit %d): "+
			"the structural rejection reflected an unbounded client string", got, len(body), limit)
	}
	if !strings.Contains(rec.Body.String(), `duplicate JSON key`) {
		t.Fatalf("body = %s, want it to name the duplicate key", rec.Body.String())
	}
}

// TestProxyNativeResponseRewritesOnlyTheModelValue is the response-side twin
// of the request-side byte-fidelity test. Restoring the alias must replace the
// model value and nothing else: the upstream's own key order, spacing, and
// string escaping are the client's to see, and re-encoding the document to
// change one field rewrites all of them. A '<', '>' or '&' in ordinary model
// output is what exposes a re-encode, because every re-encoder escapes them.
func TestProxyNativeResponseRewritesOnlyTheModelValue(t *testing.T) {
	// Deliberately unsorted keys, non-compact spacing, and characters a
	// re-encoder would escape, so any whole-document rewrite shows up.
	upstream := `{"zzz" : {"model" : "nested"} , "type":"message" , "model":"wire-msg", "content":"a < b & c > d"}`
	want := `{"zzz" : {"model" : "nested"} , "type":"message" , "model":"msg-model", "content":"a < b & c > d"}`

	up := &nativeUpstream{response: upstream}
	p, _ := newNativeProxy(t, up, nativeRoute(t, transcode.NativeMessages, "/v1/messages"))

	rec := postNative(t, p, "/v1/messages",
		`{"model":"msg-model","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != want {
		t.Fatalf("downstream body =\n%q\nwant\n%q", got, want)
	}
	// The declared length must describe exactly what was emitted.
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(want)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(want))
	}
}

// TestProxyNativeResponseIsNotAmplified pins the size property directly. The
// probe is bounded, so the bytes emitted to the client must stay bounded too:
// a rewrite that re-encodes the document inflates every escaped character six
// fold, and the amplification is the client's bill and the proxy's buffers.
func TestProxyNativeResponseIsNotAmplified(t *testing.T) {
	// Enough '<' to make re-encoding six times the original.
	content := strings.Repeat("<", 4096)
	upstream := `{"model":"wire-msg","content":"` + content + `"}`
	up := &nativeUpstream{response: upstream}
	p, _ := newNativeProxy(t, up, nativeRoute(t, transcode.NativeMessages, "/v1/messages"))

	rec := postNative(t, p, "/v1/messages",
		`{"model":"msg-model","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// The only size change a model-value swap may make is the difference
	// between the two values. Re-encoding would instead escape each of the
	// 4096 '<' into six bytes, adding about 20 KiB.
	want := len(upstream) - len(`"wire-msg"`) + len(`"msg-model"`)
	if got := rec.Body.Len(); got != want {
		t.Fatalf("downstream body is %d bytes, want %d: the alias restore re-encoded the "+
			"document instead of rewriting the model value", got, want)
	}
	if !strings.Contains(rec.Body.String(), content) {
		t.Fatal("the upstream's own content did not reach the client unescaped")
	}
}

// TestProxyNativeLocalErrorBoundsTheClientModel proves a local rejection does
// not reflect an unbounded client-controlled model identifier. The dialect
// writer escapes the message, so echoing a large model string inflates the
// error document about six fold; the accepted request body is tens of
// megabytes, so an unbounded echo would let a client make the proxy emit a
// hundredfold larger response than the request it sent.
func TestProxyNativeLocalErrorBoundsTheClientModel(t *testing.T) {
	up := &nativeUpstream{response: `{}`}
	route := nativeRoute(t, transcode.NativeMessages, "/v1/messages")
	p, _ := newNativeProxy(t, up, route)

	// A model the map does not know, so the resolution failure names the
	// model identifier the client sent.
	huge := strings.Repeat("<", 200<<10)
	rec := postNative(t, p, "/v1/messages",
		`{"model":"`+huge+`","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: the unmapped model must fail resolution locally", rec.Code)
	}
	// The message is clamped to ErrorMessageBytes (4 KiB) before rendering, and
	// the dialect writer escapes up to six fold, so the document must land
	// under 24 KiB. Without the clamp it is six times the 200 KiB identifier.
	if got, limit := rec.Body.Len(), 32<<10; got > limit {
		t.Fatalf("error body is %d bytes for a %d byte model identifier (limit %d): "+
			"the local rejection reflected an unbounded client string", got, len(huge), limit)
	}
	if !strings.Contains(rec.Body.String(), `\u003c`) {
		t.Fatalf("body = %s, want the escaped client model to be present but bounded", rec.Body.String())
	}
}

// TestProxyNativeLocalErrors proves strict violations and unknown models
// are local client errors that never reach the upstream.
func TestProxyNativeLocalErrors(t *testing.T) {
	up := &nativeUpstream{response: `{}`}
	p, _ := newNativeProxy(t, up, nativeRoute(t, transcode.NativeMessages, "/v1/messages"))

	cases := []struct {
		name string
		body string
	}{
		{"unknown model", `{"model":"nope","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`},
		// Semantic completeness beyond the model identifier (e.g.
		// max_tokens) is the upstream's job on a verbatim path, so a
		// wellformed document without it forwards instead of failing.
		{"missing model", `{"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`},
		{"malformed", `{"model":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postNative(t, p, "/v1/messages", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if path, _, _ := up.got(); path != "" {
				t.Fatalf("upstream reached at %q, want no upstream contact", path)
			}
		})
	}
}

// TestProxyNativeStreamByteIdentical proves streaming responses pass
// through byte-identical with the wire model intact.
func TestProxyNativeStreamByteIdentical(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1710000000,\"model\":\"wire-chat\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	up := &nativeUpstream{sse: sse}
	p, _ := newNativeProxy(t, up, nativeRoute(t, transcode.NativeChat, "/v1/chat/completions"))

	rec := postNative(t, p, "/v1/chat/completions",
		`{"model":"chat-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, model, _ := up.got(); model != "wire-chat" {
		t.Fatalf("upstream model = %q, want wire-chat", model)
	}
	if got := rec.Body.String(); got != sse {
		t.Fatalf("downstream stream = %q, want byte-identical %q", got, sse)
	}
}

// TestProxyNativeResponseWithDuplicateModelIsUntouched proves an ambiguous
// document is left alone rather than half-rewritten. A client reads whichever
// duplicate its parser resolves to, so replacing only the first could write
// the client alias into a slot nobody reads while the value the client does
// read still carries the upstream model — the exact leak the restore exists to
// prevent. Forwarding the document intact is the honest outcome.
func TestProxyNativeResponseWithDuplicateModelIsUntouched(t *testing.T) {
	upstream := `{"model":"other","type":"message","model":"wire-msg","content":"hi"}`
	up := &nativeUpstream{response: upstream}
	p, _ := newNativeProxy(t, up, nativeRoute(t, transcode.NativeMessages, "/v1/messages"))

	rec := postNative(t, p, "/v1/messages",
		`{"model":"msg-model","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != upstream {
		t.Fatalf("body = %q, want the ambiguous document forwarded byte-identical: %q", got, upstream)
	}
}

// TestProxyNativeResponseBoundIsExact pins the edge of the alias inspection
// bound. A body of exactly the bound is still inspected, so the alias is
// restored; one byte more is not inspected at all and passes through
// untouched. The far-over case is covered separately — this is about the
// boundary itself, which is where an off-by-one would hide.
func TestProxyNativeResponseBoundIsExact(t *testing.T) {
	// Sized so the document is exactly `bound` bytes long.
	const bound = 64
	head := `{"model":"wire-msg","c":"`
	tail := `"}`
	exact := head + strings.Repeat("x", bound-len(head)-len(tail)) + tail
	over := head + strings.Repeat("x", bound-len(head)-len(tail)+1) + tail
	if len(exact) != bound {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(exact), bound)
	}

	// declared controls whether the upstream advertises a Content-Length. With
	// one, the response is skipped on the declared-length check and never
	// reaches the probe; without one the probe is what decides. Both paths
	// have to be pinned at the boundary, because they are separate arithmetic.
	//
	// Merely omitting the header is not enough: net/http buffers a small
	// handler response and sets Content-Length itself, so the chunked case has
	// to flush the headers first to actually commit to chunked encoding.
	newCappedProxy := func(t *testing.T, response string, declared bool) *Proxy {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			if declared {
				w.Header().Set("Content-Length", strconv.Itoa(len(response)))
			} else {
				w.WriteHeader(http.StatusOK)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			_, _ = w.Write([]byte(response))
		}))
		t.Cleanup(srv.Close)
		upstreamURL, _ := url.Parse(srv.URL)
		key, _ := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
		p, err := New(
			WithUpstream(upstreamURL),
			WithMatcher(route.NewMatcher(nil)),
			WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
			WithMetrics(metrics.NewCollector()),
			WithNativeRoutes(NativeRoute{
				RouteKey: key,
				Protocol: transcode.NativeMessages,
				ModelMap: nativeTestModelMap(),
				BodyLimits: transcode.BodyLimits{
					AcceptedRequestBytes:    1 << 20,
					DecodedRequestBytes:     1 << 20,
					SuccessfulResponseBytes: bound,
				},
			}),
		)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	request := `{"model":"msg-model","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`

	t.Run("exactly at the bound is inspected", func(t *testing.T) {
		p := newCappedProxy(t, exact, true)
		rec := postNative(t, p, "/v1/messages", request)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"model":"msg-model"`) {
			t.Fatalf("body = %s, want the alias restored on a body at the bound", rec.Body.String())
		}
	})

	t.Run("one byte over is untouched", func(t *testing.T) {
		p := newCappedProxy(t, over, true)
		rec := postNative(t, p, "/v1/messages", request)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if rec.Body.String() != over {
			t.Fatalf("body = %q, want it forwarded byte-identical over the bound", rec.Body.String())
		}
	})

	t.Run("chunked at the bound is inspected", func(t *testing.T) {
		p := newCappedProxy(t, exact, false)
		rec := postNative(t, p, "/v1/messages", request)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"model":"msg-model"`) {
			t.Fatalf("body = %s, want the alias restored on a chunked body at the bound", rec.Body.String())
		}
	})

	t.Run("chunked one byte over is untouched", func(t *testing.T) {
		p := newCappedProxy(t, over, false)
		rec := postNative(t, p, "/v1/messages", request)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if rec.Body.String() != over {
			t.Fatalf("body = %q, want it forwarded byte-identical over the bound", rec.Body.String())
		}
	})
}

// TestProxyNativeOverCapResponseUntouched proves responses over the alias
// inspection bound pass through byte-identical instead of truncating,
// with both declared and chunked lengths.
func TestProxyNativeOverCapResponseUntouched(t *testing.T) {
	big := `{"type":"message","model":"wire-msg","content":"` + strings.Repeat("x", 256) + `"}`
	newCappedProxy := func(t *testing.T, handler http.HandlerFunc) *Proxy {
		t.Helper()
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		upstreamURL, _ := url.Parse(srv.URL)
		key, _ := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
		p, err := New(
			WithUpstream(upstreamURL),
			WithMatcher(route.NewMatcher(nil)),
			WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
			WithMetrics(metrics.NewCollector()),
			WithNativeRoutes(NativeRoute{
				RouteKey: key,
				Protocol: transcode.NativeMessages,
				ModelMap: nativeTestModelMap(),
				BodyLimits: transcode.BodyLimits{
					AcceptedRequestBytes:    1 << 20,
					DecodedRequestBytes:     1 << 20,
					SuccessfulResponseBytes: 64,
				},
			}),
		)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	body := `{"model":"msg-model","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`

	t.Run("declared length", func(t *testing.T) {
		p := newCappedProxy(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(big)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(big))
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Body.String(); got != big {
			t.Fatalf("downstream = %d bytes, want byte-identical %d bytes", len(got), len(big))
		}
	})

	t.Run("chunked", func(t *testing.T) {
		p := newCappedProxy(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			_, _ = w.Write([]byte(big))
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Body.String(); got != big {
			t.Fatalf("downstream = %d bytes, want byte-identical %d bytes", len(got), len(big))
		}
	})
}
