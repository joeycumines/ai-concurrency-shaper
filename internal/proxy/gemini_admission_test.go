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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// geminiAdmissionUpstream is a scripted fake upstream for the Gemini
// admission tests. It records in-flight concurrency and answers per path:
// chat/responses/messages upstreams get a minimal valid reply, the Gemini
// upstream gets a GenerateContent reply; SSE variants answer a small SSE
// script. A status script (e.g. 500) overrides the reply.
type geminiAdmissionUpstream struct {
	mu       sync.Mutex
	inflight int
	maxSeen  int
	paths    []string
	status   int // when non-zero, every request gets this status
	// holdStream keeps the first SSE exchange open after its first frame
	// until the request context is cancelled, so a client abort lands after
	// the upstream exchange started.
	holdStream bool
	cancelled  chan struct{}
}

func (u *geminiAdmissionUpstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	streaming := strings.Contains(r.URL.Path, ":streamGenerateContent") ||
		strings.Contains(r.URL.RawQuery, "alt=sse") ||
		strings.Contains(string(body), `"stream":true`)
	u.mu.Lock()
	u.inflight++
	if u.inflight > u.maxSeen {
		u.maxSeen = u.inflight
	}
	u.paths = append(u.paths, r.Method+" "+r.URL.Path)
	status := u.status
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.inflight--
		u.mu.Unlock()
	}()

	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"code":500,"message":"boom","status":"INTERNAL"}}`)
		return
	}

	switch {
	case streaming:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		frame := func(data string) {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			// Chat Completions upstream stream -> Gemini client SSE.
			frame(`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gemini-x","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`)
			if u.holdStream && len(u.paths) == 1 {
				<-r.Context().Done()
				select {
				case u.cancelled <- struct{}{}:
				default:
				}
				return
			}
			frame(`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gemini-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
			frame(`[DONE]`)
		case "/v1/responses":
			// Responses upstream stream -> Gemini client SSE.
			frame(`{"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"gemini-x","output":[]}}`)
			frame(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}}`)
			frame(`{"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`)
			frame(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hi"}`)
			frame(`{"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"hi"}`)
			frame(`{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hi"}}`)
			frame(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}}`)
			frame(`{"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gemini-x","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
		case "/v1/messages":
			// Messages upstream stream -> Gemini client SSE.
			frame(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"gemini-x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`)
			frame(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			frame(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
			frame(`{"type":"content_block_stop","index":0}`)
			frame(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`)
			frame(`{"type":"message_stop"}`)
		default:
			// Gemini upstream stream (R2G / M2G / native-stream).
			frame(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"index":0}]}`)
			frame(`{"candidates":[{"finishReason":"STOP","index":0}]}`)
			frame(`{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
		}
		return
	case r.URL.Path == "/v1/chat/completions":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	case r.URL.Path == "/v1/responses":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	case r.URL.Path == "/v1/messages":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	default: // gemini upstream JSON
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"wire-gemini"}`)
	}
}

func (u *geminiAdmissionUpstream) maxConcurrency() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.maxSeen
}

func (u *geminiAdmissionUpstream) setStatus(status int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = status
}

// geminiDirection describes one client->upstream direction under test.
type geminiDirection struct {
	name string
	// clientPath is the request path on the proxy (with {model} concrete).
	clientPath string
	// clientBody is the request body.
	clientBody string
	// stream reports whether the exchange is SSE.
	stream bool
	// mapping is the transcode mapping, or nil for the native route.
	mapping *TranscodeMapping
	// native, when true, registers a native Gemini route instead of a mapping.
	native bool
}

// geminiAdmissionDirections lists the six Gemini directions, JSON and SSE
// each, driven off the same fake upstream.
func geminiAdmissionDirections() []geminiDirection {
	g2c := TranscodeMapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1beta/models/{model}:generateContent"},
		ClientProtocol:   transcode.ClientGemini,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		ModelMap:         transcode.ModelMap{AllowIdentity: true},
		Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		LossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureMetadata:                          {},
			transcode.FeatureParallelToolCalls:                 {},
			transcode.FeatureGeminiClientQuery:                 {},
			transcode.FeatureGeminiThoughtSignaturePlaceholder: {},
			transcode.FeatureGeminiGenerationControls:          {},
			transcode.FeatureGeminiSafetySettings:              {},
			transcode.FeatureGeminiRequestControls:             {},
			transcode.FeatureGeminiThoughtSignature:            {},
			transcode.FeatureGeminiPartMetadata:                {},
			transcode.FeatureGeminiToolStrictness:              {},
			transcode.FeatureGeminiImageDetail:                 {},
			transcode.FeatureUsageUnknown:                      {},
			transcode.FeatureUsageCacheReadUnknown:             {},
			transcode.FeatureUsageCacheWriteUnknown:            {},
			transcode.FeatureUsageReasoningUnknown:             {},
		}},
	}
	g2r := g2c
	g2r.UpstreamProtocol = transcode.UpstreamResponses
	g2r.UpstreamPath = "/v1/responses"
	g2r.LossPolicy = transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureToolSchemaStrictness:              {},
		transcode.FeatureMetadata:                          {},
		transcode.FeatureParallelToolCalls:                 {},
		transcode.FeatureGeminiClientQuery:                 {},
		transcode.FeatureGeminiThoughtSignaturePlaceholder: {},
		transcode.FeatureGeminiGenerationControls:          {},
		transcode.FeatureGeminiSafetySettings:              {},
		transcode.FeatureGeminiRequestControls:             {},
		transcode.FeatureGeminiThoughtSignature:            {},
		transcode.FeatureGeminiPartMetadata:                {},
		transcode.FeatureGeminiToolStrictness:              {},
		transcode.FeatureGeminiImageDetail:                 {},
		transcode.FeatureUsageUnknown:                      {},
		transcode.FeatureUsageCacheReadUnknown:             {},
		transcode.FeatureUsageCacheWriteUnknown:            {},
		transcode.FeatureUsageReasoningUnknown:             {},
	}}
	g2m := g2c
	g2m.UpstreamProtocol = transcode.UpstreamMessages
	g2m.UpstreamPath = "/v1/messages"
	r2g := TranscodeMapping{
		ClientRoute:      transcode.RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
		ClientProtocol:   transcode.ClientResponses,
		UpstreamProtocol: transcode.UpstreamGemini,
		UpstreamPath:     "/v1beta",
		ModelMap:         transcode.ModelMap{AllowIdentity: true},
		Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
		LossPolicy:       g2c.LossPolicy,
	}
	m2g := TranscodeMapping{
		ClientRoute:        transcode.RouteKey{Method: http.MethodPost, Path: "/v1/messages"},
		ClientProtocol:     transcode.ClientMessages,
		UpstreamProtocol:   transcode.UpstreamGemini,
		UpstreamPath:       "/v1beta",
		ModelMap:           transcode.ModelMap{AllowIdentity: true},
		AllowedClientQuery: map[string]struct{}{"beta": {}},
		Auth:               transcode.AuthPolicy{Mode: transcode.AuthNone},
		LossPolicy:         g2c.LossPolicy,
	}
	geminiBody := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":64}}`
	responsesBody := `{"model":"m","input":"hi"}`
	messagesBody := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	return []geminiDirection{
		{name: "G2C", clientPath: "/v1beta/models/gemini-x:generateContent", clientBody: geminiBody, mapping: &g2c},
		{name: "G2C-stream", clientPath: "/v1beta/models/gemini-x:streamGenerateContent?alt=sse", clientBody: geminiBody, stream: true, mapping: &g2c},
		{name: "G2R", clientPath: "/v1beta/models/gemini-x:generateContent", clientBody: geminiBody, mapping: &g2r},
		{name: "G2R-stream", clientPath: "/v1beta/models/gemini-x:streamGenerateContent?alt=sse", clientBody: geminiBody, stream: true, mapping: &g2r},
		{name: "G2M", clientPath: "/v1beta/models/gemini-x:generateContent", clientBody: geminiBody, mapping: &g2m},
		{name: "G2M-stream", clientPath: "/v1beta/models/gemini-x:streamGenerateContent?alt=sse", clientBody: geminiBody, stream: true, mapping: &g2m},
		{name: "R2G", clientPath: "/v1/responses", clientBody: responsesBody, mapping: &r2g},
		{name: "R2G-stream", clientPath: "/v1/responses", clientBody: `{"model":"m","input":"hi","stream":true}`, stream: true, mapping: &r2g},
		{name: "M2G", clientPath: "/v1/messages?beta=true", clientBody: messagesBody, mapping: &m2g},
		{name: "M2G-stream", clientPath: "/v1/messages?beta=true", clientBody: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":true}`, stream: true, mapping: &m2g},
		{name: "native", clientPath: "/v1beta/models/fake-gemini:generateContent", clientBody: geminiBody, native: true},
		{name: "native-stream", clientPath: "/v1beta/models/fake-gemini:streamGenerateContent?alt=sse", clientBody: geminiBody, stream: true, native: true},
	}
}

// newGeminiAdmissionProxy builds a proxy for one direction with a route
// limit of 1 (plus the global limiter of 1) and the given extra options.
func newGeminiAdmissionProxy(t *testing.T, up *geminiAdmissionUpstream, dir geminiDirection, extra ...Option) *Proxy {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	upstreamURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	opts := []Option{
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(route.DefaultPatterns())),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithMaxRetries(0),
	}
	if dir.native {
		opts = append(opts, WithNativeRoutes(NativeRoute{
			RouteKey: transcode.RouteKey{Method: http.MethodPost, Path: "/v1beta/models/{model}:generateContent"},
			Protocol: transcode.NativeGemini,
			Provider: "testprov",
			ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
				"fake-gemini": {
					ClientModel:         "fake-gemini",
					UpstreamModel:       "wire-gemini",
					ClientResponseModel: "fake-gemini",
					Via:                 transcode.NativeGemini,
				},
			}},
		}))
	} else if dir.mapping != nil {
		opts = append(opts, WithTranscodeMapping(*dir.mapping))
	}
	opts = append(opts, extra...)
	p, err := New(opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// doGemini sends one request for the direction and returns the recorder.
func doGemini(t *testing.T, p *Proxy, dir geminiDirection) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, dir.clientPath, strings.NewReader(dir.clientBody))
	req.Header.Set("Content-Type", "application/json")
	if dir.stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestGeminiAdmissionSlotHeldForWholeExchange proves, for every Gemini
// direction in JSON and SSE form, that with a route limit of 1 a second
// request waits until the first exchange ends (plus the release cooldown),
// the global limiter is held together with the route slot, and the upstream
// never observes concurrency above 1.
func TestGeminiAdmissionSlotHeldForWholeExchange(t *testing.T) {
	for _, dir := range geminiAdmissionDirections() {
		t.Run(dir.name, func(t *testing.T) {
			up := &geminiAdmissionUpstream{}
			const cooldown = 150 * time.Millisecond
			p := newGeminiAdmissionProxy(t, up, dir,
				WithLimiter(queue.NewLimiterWithCooldown(1, cooldown)),
				WithGlobalLimiter(queue.NewLimiterWithCooldown(1, cooldown)),
			)

			rec1 := doGemini(t, p, dir)
			if rec1.Code != http.StatusOK {
				t.Fatalf("first request status = %d, want 200: %s", rec1.Code, rec1.Body.String())
			}
			start := time.Now()
			rec2 := doGemini(t, p, dir)
			wait := time.Since(start)
			if rec2.Code != http.StatusOK {
				t.Fatalf("second request status = %d, want 200: %s", rec2.Code, rec2.Body.String())
			}
			// The second request waited out at least the release cooldown.
			if wait < cooldown/2 {
				t.Fatalf("second request waited %v, want at least ~%v of release cooldown", wait, cooldown)
			}
			if got := up.maxConcurrency(); got > 1 {
				t.Fatalf("upstream observed concurrency %d, want <= 1", got)
			}
		})
	}
}

// TestGeminiAdmissionQueueBounds proves queue depth and queue timeout reject
// a waiting request with the documented responses, for a Gemini path.
func TestGeminiAdmissionQueueBounds(t *testing.T) {
	dir := geminiAdmissionDirections()[0] // G2C JSON

	t.Run("queue depth", func(t *testing.T) {
		up := &geminiAdmissionUpstream{}
		// Hold the single route slot directly via the limiter so the next
		// request must queue, then exceed the queue depth of 1.
		lim := queue.NewLimiterWithCooldown(1, 0)
		p := newGeminiAdmissionProxy(t, up, dir, WithLimiter(lim), WithQueueDepthLimit(1))
		stop := fillQueue(t, lim)
		defer stop()
		rec := doGemini(t, p, dir)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 (queue depth): %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("queue timeout", func(t *testing.T) {
		up := &geminiAdmissionUpstream{}
		lim := queue.NewLimiterWithCooldown(1, 0)
		p := newGeminiAdmissionProxy(t, up, dir,
			WithLimiter(lim),
			WithQueueTimeout(100*time.Millisecond),
		)
		stop := fillQueue(t, lim)
		defer stop()
		start := time.Now()
		rec := doGemini(t, p, dir)
		wait := time.Since(start)
		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("status = %d, want 504 (queue timeout): %s", rec.Code, rec.Body.String())
		}
		if wait > 2*time.Second {
			t.Fatalf("queue timeout wait = %v, want ~100ms", wait)
		}
	})
}

// TestGeminiAdmissionBreakerOpenRejectsWithoutStateChange proves an open
// breaker rejects a Gemini request before admission and does not change the
// breaker epoch.
func TestGeminiAdmissionBreakerOpenRejectsWithoutStateChange(t *testing.T) {
	dir := geminiAdmissionDirections()[0]
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(time.Hour),
		circuitbreaker.WithMaxOpenTimeout(time.Hour),
	)
	up := &geminiAdmissionUpstream{}
	p := newGeminiAdmissionProxy(t, up, dir, WithBreaker(b))

	// Drive the breaker open with one definitive upstream failure.
	up.setStatus(500)
	rec := doGemini(t, p, dir)
	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want the 500 to surface")
	}
	if got := b.State(); got != circuitbreaker.Open {
		t.Fatalf("breaker state = %s, want OPEN", got)
	}
	statsBefore := b.Stats()

	up.setStatus(0)
	rec = doGemini(t, p, dir)
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want a breaker rejection (503/429): %s", rec.Code, rec.Body.String())
	}
	statsAfter := b.Stats()
	if statsAfter.TotalFailures != statsBefore.TotalFailures || statsAfter.TotalSuccesses != statsBefore.TotalSuccesses {
		t.Fatalf("breaker accounting changed on a rejected request: before %+v after %+v", statsBefore, statsAfter)
	}
}

// TestGeminiAdmissionUpstreamFailureHoldAndBreaker proves a definitive
// upstream 500 on a Gemini path records exactly one breaker failure and
// applies the failure hold (the slot is not reusable immediately), even
// when the breaker is off.
func TestGeminiAdmissionUpstreamFailureHoldAndBreaker(t *testing.T) {
	for _, withBreaker := range []bool{true, false} {
		t.Run(fmt.Sprintf("breaker=%v", withBreaker), func(t *testing.T) {
			dir := geminiAdmissionDirections()[0]
			var b *circuitbreaker.Breaker
			var extra []Option
			if withBreaker {
				b = testBreaker(t)
				extra = append(extra, WithBreaker(b))
			}
			extra = append(extra, WithFailureHold(200*time.Millisecond))
			up := &geminiAdmissionUpstream{}
			p := newGeminiAdmissionProxy(t, up, dir, extra...)

			up.setStatus(500)
			rec1 := doGemini(t, p, dir)
			if rec1.Code == http.StatusOK {
				t.Fatalf("first status = 200, want the 500")
			}
			if withBreaker {
				if got := b.Stats().TotalFailures; got != 1 {
					t.Fatalf("breaker failures = %d, want exactly 1", got)
				}
			}
			// The failure hold keeps the slot: a second request must wait.
			up.setStatus(0)
			start := time.Now()
			rec2 := doGemini(t, p, dir)
			wait := time.Since(start)
			if rec2.Code != http.StatusOK {
				t.Fatalf("second status = %d, want 200: %s", rec2.Code, rec2.Body.String())
			}
			if wait < 150*time.Millisecond {
				t.Fatalf("no failure hold: second request waited only %v", wait)
			}
		})
	}
}

// TestGeminiAdmissionLocalConversionErrorIsNotBreakerOutcome proves a local
// conversion error (unsupported field under strict policy) records no
// breaker outcome on a Gemini path.
func TestGeminiAdmissionLocalConversionErrorIsNotBreakerOutcome(t *testing.T) {
	// G2C under a strict loss policy: an unsupported request field (metadata)
	// is a client-dialect error before the upstream is touched.
	g2c := geminiAdmissionDirections()[0].mapping
	strict := *g2c
	strict.LossPolicy = transcode.LossPolicy{} // strict: nothing approved
	dir := geminiDirection{name: "G2C-strict", clientPath: "/v1beta/models/gemini-x:generateContent",
		clientBody: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"metadata":{"user_id":"u"}}`,
		mapping:    &strict}

	b := testBreaker(t)
	up := &geminiAdmissionUpstream{}
	p := newGeminiAdmissionProxy(t, up, dir, WithBreaker(b))

	rec := doGemini(t, p, dir)
	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want a local 4xx conversion error")
	}
	if got := b.Stats(); got.TotalFailures != 0 || got.TotalSuccesses != 0 {
		t.Fatalf("breaker recorded an outcome for a local error: %+v", got)
	}
	if got := len(up.paths); got != 0 {
		t.Fatalf("upstream saw %d requests, want 0 (local error never reaches upstream)", got)
	}
}

// TestGeminiAdmissionClientCancelAppliesCancelCooldown proves a client abort
// after the upstream started applies the cancel cooldown on a Gemini path.
func TestGeminiAdmissionClientCancelAppliesCancelCooldown(t *testing.T) {
	dir := geminiAdmissionDirections()[1] // G2C stream
	const cancelCooldown = 200 * time.Millisecond
	up := &geminiAdmissionUpstream{holdStream: true, cancelled: make(chan struct{}, 1)}
	p := newGeminiAdmissionProxy(t, up, dir, WithCancelCooldown(cancelCooldown))

	// Run the exchange over a real server so request cancellation propagates.
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+dir.clientPath, strings.NewReader(dir.clientBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		// Read the first frame, then cancel mid-stream.
		buf := make([]byte, 1024)
		if _, err := resp.Body.Read(buf); err != nil {
			return
		}
		cancel()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled exchange did not finish")
	}
	// The upstream must have observed the cancellation (the hold was released
	// by the request context), proving the abort happened after upstream start.
	select {
	case <-up.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never observed the client cancel")
	}

	// The next request must wait out the cancel cooldown.
	start := time.Now()
	rec := doGemini(t, p, dir)
	wait := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if wait < cancelCooldown/2 {
		t.Fatalf("no cancel cooldown: next request waited only %v, want >= ~%v", wait, cancelCooldown)
	}
}

// TestGeminiAdmissionHalfOpenProbeLifecycle proves a HALF_OPEN probe on a
// Gemini path is resolved by a success (closing the breaker).
func TestGeminiAdmissionHalfOpenProbeLifecycle(t *testing.T) {
	dir := geminiAdmissionDirections()[0]
	b := mustBreaker(t,
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(10*time.Second),
		circuitbreaker.WithOpenTimeout(30*time.Millisecond),
		circuitbreaker.WithMaxOpenTimeout(30*time.Millisecond),
	)
	up := &geminiAdmissionUpstream{}
	p := newGeminiAdmissionProxy(t, up, dir, WithBreaker(b))

	up.setStatus(500)
	doGemini(t, p, dir)
	if got := b.State(); got != circuitbreaker.Open {
		t.Fatalf("state = %s, want OPEN", got)
	}
	// Wait for the open timeout to lapse; the next request's Allow then
	// transitions to HALF_OPEN and admits it as the probe.
	time.Sleep(60 * time.Millisecond)
	up.setStatus(0)
	rec := doGemini(t, p, dir)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := b.State(); got != circuitbreaker.Closed {
		t.Fatalf("state = %s, want CLOSED after a successful probe", got)
	}
}
