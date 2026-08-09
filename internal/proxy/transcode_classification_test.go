package proxy

// J2 regression tests for the unified exchange classification (review-j
// findings 1 and 2): one immutable exchange result drives slot holds, breaker
// resolution, completion counters, and journal finalization for both
// admission paths.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// j2Breaker returns a breaker with a small base penalty so holds are
// measurable: PenaltyDuration is nonzero even at zero consecutive failures.
func j2Breaker(t *testing.T) *circuitbreaker.Breaker {
	t.Helper()
	breaker, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(100),
		circuitbreaker.WithBasePenalty(500*time.Millisecond),
		circuitbreaker.WithMaxPenalty(60*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	return breaker
}

// j2LimitedProxy builds a proxy with a limited POST /v1/responses route, a
// slot limiter of capacity 1, and the given breaker (nil allowed).
func j2LimitedProxy(t *testing.T, upstream *httptest.Server, breaker *circuitbreaker.Breaker) *Proxy {
	t.Helper()
	pattern, err := route.Parse("POST /v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream.URL)
	options := []Option{
		WithUpstream(u),
		WithMatcher(route.NewMatcher([]route.Pattern{pattern})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
	}
	if breaker != nil {
		options = append(options, WithBreaker(breaker))
	}
	p, err := New(options...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProxyTranscodeLocalConversion502NoPhantomHold proves that a local
// response conversion failure (downstream 502, UpstreamFailure=false) does
// NOT hold the limiter slot, even though PenaltyDuration is nonzero at zero
// consecutive failures (review-j finding 1, false-penalty direction). The
// fixture is a VALID Chat response whose finish_reason is outside the
// supported subset — a known-but-unsupported feature stays local (review-k
// finding 3).
func TestProxyTranscodeLocalConversion502NoPhantomHold(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"weird","message":{"role":"assistant","content":"x"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)
	p := j2LimitedProxy(t, upstream, breaker)

	// PenaltyDuration is nonzero at zero consecutive failures: the base
	// penalty alone would hold the slot if the local 502 were misclassified.
	if got := breaker.Stats().CurrentPenalty; got != 500*time.Millisecond {
		t.Fatalf("PenaltyDuration at zero consecutive failures = %v, want 500ms", got)
	}

	req1 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec1 := httptest.NewRecorder()
	p.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusBadGateway {
		t.Fatalf("request 1 status = %d, want 502", rec1.Code)
	}

	// The second request must NOT wait out a phantom penalty: the slot is
	// released immediately because the exchange result says the local 502 is
	// not an upstream failure.
	start := time.Now()
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"y"}`),
	)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	wait := time.Since(start)
	t.Logf("second request wait after local conversion 502: %v", wait)
	if wait >= 400*time.Millisecond {
		t.Fatalf("phantom penalty applied to local conversion 502: wait %v", wait)
	}
}

// TestProxyTranscodeCorruptUpstreamJSONAppliesPhantomHold proves the
// review-k finding-3 counterexample: a 200 response that is not a valid
// instance of the supported Chat subset (an object that is not a chat
// completion at all) is corrupt upstream wire — an upstream failure — and
// DOES hold the limiter slot (review-k finding 3).
func TestProxyTranscodeCorruptUpstreamJSONAppliesPhantomHold(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"not":"chat"}`))
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)
	p := j2LimitedProxy(t, upstream, breaker)

	// PenaltyDuration is nonzero at zero consecutive failures: the base
	// penalty alone holds the slot when the corrupt wire is correctly
	// classified as an upstream failure.
	if got := breaker.Stats().CurrentPenalty; got != 500*time.Millisecond {
		t.Fatalf("PenaltyDuration at zero consecutive failures = %v, want 500ms", got)
	}

	req1 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec1 := httptest.NewRecorder()
	p.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusBadGateway {
		t.Fatalf("request 1 status = %d, want 502", rec1.Code)
	}

	// The second request MUST wait out the penalty: corrupt upstream wire is
	// an upstream failure, so the slot is held.
	start := time.Now()
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"y"}`),
	)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	wait := time.Since(start)
	t.Logf("second request wait after corrupt upstream wire 502: %v", wait)
	if wait < 400*time.Millisecond {
		t.Fatalf("upstream wire failure did not hold the slot: wait %v", wait)
	}
}

// TestProxyTranscodeTruncatedStreamAppliesPhantomHold proves that a 200
// stream truncated by the upstream — an explicit upstream failure — DOES hold
// the limiter slot (review-j finding 1, missed-penalty direction).
func TestProxyTranscodeTruncatedStreamAppliesPhantomHold(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// One valid chunk, then the stream ends without a terminal condition.
		_, _ = io.WriteString(
			w,
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
		)
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)
	before := breaker.Stats()
	p := j2LimitedProxy(t, upstream, breaker)

	req1 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	)
	rec1 := httptest.NewRecorder()
	p.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", rec1.Code)
	}
	if got := breaker.Stats().TotalFailures; got != before.TotalFailures+1 {
		t.Fatalf("truncated stream not recorded as upstream failure: before=%d after=%d", before.TotalFailures, got)
	}

	// The truncated stream is an upstream failure, so the slot must be held
	// for the penalty (the missed phantom hold is repaired).
	start := time.Now()
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"y","stream":true}`),
	)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	wait := time.Since(start)
	t.Logf("second request wait after truncated stream: %v", wait)
	if wait < 400*time.Millisecond {
		t.Fatalf("phantom hold not applied to truncated stream: wait %v", wait)
	}
}

// TestProxyTranscodeRateLimit403HoldAndBreakerFailure proves that an upstream
// 403 carrying only x-ratelimit-* signals is classified as an upstream
// failure and holds the slot, even though the rendered client error carries
// none of the signals (review-j finding 1, missed rate-ban direction).
func TestProxyTranscodeRateLimit403HoldAndBreakerFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A rate-limit signal from the breaker's vocabulary; the rendered
		// client error must not carry it, so the legacy recorder-based
		// classification would miss the failure.
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"banned","type":"permission_error"}}`))
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)
	before := breaker.Stats()
	p := j2LimitedProxy(t, upstream, breaker)

	req1 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec1 := httptest.NewRecorder()
	p.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusForbidden {
		t.Fatalf("request 1 status = %d, want 403", rec1.Code)
	}
	// The rendered client error must not carry the upstream rate-limit
	// signal (that is what makes the recorder-based classification miss it).
	for name := range rec1.Header() {
		if strings.HasPrefix(strings.ToLower(name), "x-ratelimit") {
			t.Fatalf("rendered client error leaks upstream rate-limit header %q", name)
		}
	}
	if got := breaker.Stats().TotalFailures; got != before.TotalFailures+1 {
		t.Fatalf("rate-signalled 403 not recorded as breaker failure: before=%d after=%d", before.TotalFailures, got)
	}

	// The rate-signalled 403 must hold the slot even though the recorder's
	// (rendered) response headers carry no signal.
	start := time.Now()
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"y"}`),
	)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	wait := time.Since(start)
	t.Logf("second request wait after rate-signalled 403: %v", wait)
	if wait < 400*time.Millisecond {
		t.Fatalf("rate-signalled 403 did not hold the slot: wait %v", wait)
	}
}

// failWriter fails every Write, simulating a disconnected downstream client.
type failWriter struct {
	header http.Header
	status int
}

func (w *failWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failWriter) WriteHeader(status int) { w.status = status }

func (w *failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestProxyTranscodeUpstreamErrorFrameWriteFailureNotClean proves that a
// genuine upstream error frame (a streamed response.failed) whose downstream
// write fails is still an upstream failure (breaker failure recorded) AND is
// not counted as a clean completion: the exchange is aborted because the
// translated error was never delivered (review-j finding 2, write-failure
// accounting).
func TestProxyTranscodeUpstreamErrorFrameWriteFailureNotClean(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(
			w,
			"data: {\"type\":\"response.failed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"failed\",\"model\":\"m\",\"output\":[],\"error\":{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\n",
		)
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)
	before := breaker.Stats()
	collector := metrics.NewCollector()

	pattern, err := route.Parse("POST /v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream.URL)
	p, err := New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher([]route.Pattern{pattern})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(collector),
		WithBreaker(breaker),
		WithTranscodeMapping(transcodeMapping(testMessagesResponsesMapping(t))),
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	)
	writer := &failWriter{}
	p.ServeHTTP(writer, req)

	if writer.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", writer.status)
	}
	if got := breaker.Stats().TotalFailures; got != before.TotalFailures+1 {
		t.Fatalf("upstream error frame not recorded as failure: before=%d after=%d", before.TotalFailures, got)
	}
	snapshot := collector.Snapshot()
	if snapshot.TotalProxied != 0 {
		t.Fatalf("undelivered error exchange counted as clean completion: TotalProxied=%d", snapshot.TotalProxied)
	}
	if snapshot.TotalAborted != 1 {
		t.Fatalf("undelivered error exchange not recorded as aborted: TotalAborted=%d", snapshot.TotalAborted)
	}
}

// TestClassifyTranscodeExchangeSuppressibleAbort pins the transcode
// suppressible-abort decision: a client abort suppresses the phantom hold
// only when the outcome carries no definitive upstream failure — a
// rate-signalled 403 classified from the ORIGINAL upstream headers stays a
// failure even when the client disconnects while the translated error is
// written (native parity).
func TestClassifyTranscodeExchangeSuppressibleAbort(t *testing.T) {
	makeRec := func(outcome transcode.Outcome) *statusRecorder {
		outcomeCopy := outcome
		return &statusRecorder{
			ResponseWriter:   httptest.NewRecorder(),
			transcodeOutcome: &outcomeCopy,
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	now := time.Now()

	// Rate-signalled 403 + client abort: NOT suppressible (the retained
	// upstream failure drives the hold and breaker failure).
	rec := makeRec(transcode.Outcome{
		Status:          http.StatusForbidden,
		Provenance:      transcode.ProvenanceClientAbort,
		UpstreamFailure: true,
		ClientAborted:   true,
	})
	result := classifyTranscodeExchange(rec, req, true, now)
	if result.suppressibleAbort {
		t.Fatal("rate-signalled 403 with client abort is suppressible; the definitive upstream failure must be recorded")
	}
	if !result.upstreamFailure {
		t.Fatal("rate-signalled 403 lost its upstream failure classification")
	}

	// Plain client abort (no upstream response): suppressible.
	rec = makeRec(transcode.Outcome{
		Provenance:    transcode.ProvenanceClientAbort,
		ClientAborted: true,
	})
	result = classifyTranscodeExchange(rec, req, true, now)
	if !result.suppressibleAbort {
		t.Fatal("plain client abort is not suppressible")
	}

	// Local conversion failure + client abort: suppressible (not an upstream
	// failure).
	rec = makeRec(transcode.Outcome{
		Status:          http.StatusBadGateway,
		Provenance:      transcode.ProvenanceLocalResponseConversionError,
		ClientAborted:   true,
		UpstreamFailure: false,
	})
	result = classifyTranscodeExchange(rec, req, true, now)
	if !result.suppressibleAbort {
		t.Fatal("local conversion failure with client abort is not suppressible")
	}

	// 429 + client abort: NOT suppressible.
	rec = makeRec(transcode.Outcome{
		Status:          http.StatusTooManyRequests,
		Provenance:      transcode.ProvenanceClientAbort,
		UpstreamFailure: true,
		ClientAborted:   true,
	})
	result = classifyTranscodeExchange(rec, req, true, now)
	if result.suppressibleAbort {
		t.Fatal("429 with client abort is suppressible; the definitive upstream failure must be recorded")
	}
}

// TestProxyTranscodeOversizedSSEFatalNotShortenedSuccess proves that an
// oversized SSE frame (part of a tool call) followed by a valid terminal
// frame terminates the exchange with a client-dialect error event classified
// as an upstream body/protocol failure: breaker failure recorded, phantom
// hold applied, and the terminal frame can never turn the stream into a
// shortened success (review-j finding 3).
func TestProxyTranscodeOversizedSSEFatalNotShortenedSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Part of a tool call in a data line beyond the configured line
		// bound, followed by a valid terminal chunk and [DONE].
		_, _ = io.WriteString(
			w,
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"f\",\"arguments\":\""+
				strings.Repeat("x", 4096)+
				"\"}}]}}]}\n\n"+
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
				"data: [DONE]\n\n",
		)
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)
	before := breaker.Stats()

	pattern, err := route.Parse("POST /v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream.URL)
	mapping := testResponsesMapping(t)
	p, err := New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher([]route.Pattern{pattern})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(breaker),
		WithTranscodeMapping(TranscodeMapping{
			Mapping: mapping,
			BodyLimits: transcode.BodyLimits{
				SSELineBytes:  1024,
				SSEFrameBytes: 1024,
			},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	req1 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	)
	rec1 := httptest.NewRecorder()
	p.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", rec1.Code)
	}
	body1 := rec1.Body.String()
	if !strings.Contains(body1, `"type":"error"`) {
		t.Fatalf("downstream stream has no error event: %q", body1)
	}
	if !strings.Contains(body1, "SSE line exceeds") {
		t.Fatalf("error event does not report the size violation: %q", body1)
	}
	if strings.Contains(body1, "response.completed") {
		t.Fatalf("oversized frame was skipped and the stream ended as a shortened success: %q", body1)
	}
	if got := breaker.Stats().TotalFailures; got != before.TotalFailures+1 {
		t.Fatalf("size violation not recorded as upstream failure: before=%d after=%d", before.TotalFailures, got)
	}

	// The upstream body/protocol failure holds the slot (phantom hold).
	start := time.Now()
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"y","stream":true}`),
	)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	wait := time.Since(start)
	t.Logf("second request wait after SSE size violation: %v", wait)
	if wait < 400*time.Millisecond {
		t.Fatalf("phantom hold not applied to SSE size violation: wait %v", wait)
	}
}

// TestProxyTranscodeAbortNotCleanCompletion proves that a client-aborted
// transcode exchange is never counted as a clean completion: IncProxied does
// not fire, the journal marks the entry aborted without ResponseComplete, and
// the aborted-request metric is recorded (review-j finding 2).
func TestProxyTranscodeAbortNotCleanCompletion(t *testing.T) {
	upstreamStarted := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(
			w,
			"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
		)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)
	breakerBefore := breaker.Stats()
	collector := metrics.NewCollector()
	j := journal.New(64, 1<<20)

	pattern, err := route.Parse("POST /v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream.URL)
	p, err := New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher([]route.Pattern{pattern})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(collector),
		WithBreaker(breaker),
		WithJournal(j),
		WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	).WithContext(ctx)

	writer := newReturnGuardWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(writer, req)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for writer.flushCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if writer.flushCount() == 0 {
		t.Fatal("no downstream flush observed")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not return after cancellation")
	}
	<-upstreamStarted

	snapshot := collector.Snapshot()
	if snapshot.TotalProxied != 0 {
		t.Fatalf("aborted exchange counted as clean completion: TotalProxied=%d", snapshot.TotalProxied)
	}
	if snapshot.TotalAborted != 1 {
		t.Fatalf("aborted exchange not recorded as aborted: TotalAborted=%d", snapshot.TotalAborted)
	}
	after := breaker.Stats()
	if after.TotalSuccesses != breakerBefore.TotalSuccesses ||
		after.TotalFailures != breakerBefore.TotalFailures {
		t.Fatalf("aborted exchange mutated breaker health: before(s=%d,f=%d) after(s=%d,f=%d)",
			breakerBefore.TotalSuccesses, breakerBefore.TotalFailures,
			after.TotalSuccesses, after.TotalFailures)
	}

	entries := j.Entries()
	if len(entries) != 1 {
		t.Fatalf("journal entries = %d, want 1", len(entries))
	}
	if !entries[0].Aborted {
		t.Fatal("journal entry not marked aborted")
	}
	if !entries[0].Timing.ResponseComplete.IsZero() {
		t.Fatal("journal entry records ResponseComplete for an aborted exchange")
	}
}

// TestProxyTranscodeShortWriteNotCleanCompletion proves a transcode exchange
// whose downstream write is short (a non-conforming ResponseWriter returning
// (n < len(b), nil)) is never counted as a clean completion: the recorder's
// independent write-failure observation is preserved by the monotonic
// aborted assignment, so IncProxied does not fire, the journal marks the
// entry aborted without ResponseComplete, and the breaker records neither
// success nor failure (review-k finding 7).
func TestProxyTranscodeShortWriteNotCleanCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(
			w,
			`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`,
		)
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)
	breakerBefore := breaker.Stats()
	collector := metrics.NewCollector()
	j := journal.New(64, 1<<20)

	pattern, err := route.Parse("POST /v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream.URL)
	p, err := New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher([]route.Pattern{pattern})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(collector),
		WithBreaker(breaker),
		WithJournal(j),
		WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	writer := &shortWriteResponseWriter{}
	p.ServeHTTP(writer, req)

	snapshot := collector.Snapshot()
	if snapshot.TotalProxied != 0 {
		t.Fatalf("short-write exchange counted as clean completion: TotalProxied=%d", snapshot.TotalProxied)
	}
	if snapshot.TotalAborted != 1 {
		t.Fatalf("short-write exchange not recorded as aborted: TotalAborted=%d", snapshot.TotalAborted)
	}
	after := breaker.Stats()
	if after.TotalSuccesses != breakerBefore.TotalSuccesses ||
		after.TotalFailures != breakerBefore.TotalFailures {
		t.Fatalf("short-write exchange mutated breaker health: before(s=%d,f=%d) after(s=%d,f=%d)",
			breakerBefore.TotalSuccesses, breakerBefore.TotalFailures,
			after.TotalSuccesses, after.TotalFailures)
	}
	entries := j.Entries()
	if len(entries) != 1 {
		t.Fatalf("journal entries = %d, want 1", len(entries))
	}
	if !entries[0].Aborted {
		t.Fatal("journal entry not marked aborted")
	}
	if !entries[0].Timing.ResponseComplete.IsZero() {
		t.Fatal("journal entry records ResponseComplete for a short-write exchange")
	}
}

// shortWriteResponseWriter violates the io.Writer contract by returning
// (n < len(b), nil) from Write.
type shortWriteResponseWriter struct {
	header http.Header
	status int
}

func (w *shortWriteResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *shortWriteResponseWriter) WriteHeader(status int) { w.status = status }

func (w *shortWriteResponseWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	return len(b) - 1, nil
}

// TestProxyTranscodeInBandChatErrorFrameIsUpstreamFailure proves an in-band
// Chat error frame is classified as an upstream failure: breaker failure
// recorded, phantom hold applied, client-dialect error event emitted — never
// a local conversion failure (review-j finding 11).
func TestProxyTranscodeInBandChatErrorFrameIsUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(
			w,
			"data: {\"error\":{\"message\":\"upstream exploded\",\"type\":\"server_error\"}}\n\n",
		)
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)
	before := breaker.Stats()
	p := j2LimitedProxy(t, upstream, breaker)

	req1 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	)
	rec1 := httptest.NewRecorder()
	p.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", rec1.Code)
	}
	if !strings.Contains(rec1.Body.String(), `"type":"error"`) {
		t.Fatalf("client must receive an error event: %q", rec1.Body.String())
	}
	if got := breaker.Stats().TotalFailures; got != before.TotalFailures+1 {
		t.Fatalf("in-band error frame not recorded as upstream failure: before=%d after=%d", before.TotalFailures, got)
	}

	// The upstream failure holds the slot.
	start := time.Now()
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"y","stream":true}`),
	)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	wait := time.Since(start)
	t.Logf("second request wait after in-band error frame: %v", wait)
	if wait < 400*time.Millisecond {
		t.Fatalf("phantom hold not applied to in-band error frame: wait %v", wait)
	}
}

// TestProxyTranscodeNonStreamFailedEnvelopeIsUpstreamFailure proves a 200
// non-stream Responses envelope with status "failed" classifies as an
// upstream failure — identical to the streamed response.failed case — with
// the upstream's HTTP status driving the breaker (review-j finding 11).
func TestProxyTranscodeNonStreamFailedEnvelopeIsUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(
			w,
			`{"id":"resp_1","object":"response","created_at":1,"status":"failed","model":"m","output":[],"error":{"code":"server_error","message":"boom"}}`,
		)
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)
	before := breaker.Stats()
	pattern, err := route.Parse("POST /v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream.URL)
	p, err := New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher([]route.Pattern{pattern})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(breaker),
		WithTranscodeMapping(transcodeMapping(testMessagesResponsesMapping(t))),
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`),
	)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := breaker.Stats().TotalFailures; got != before.TotalFailures+1 {
		t.Fatalf("non-stream failed envelope not recorded as upstream failure: before=%d after=%d", before.TotalFailures, got)
	}
	if !strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("client error must carry the upstream message: %q", rec.Body.String())
	}

	// Parity: the streamed response.failed classifies identically.
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(
			w,
			"data: {\"type\":\"response.failed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"failed\",\"model\":\"m\",\"output\":[],\"error\":{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\n",
		)
	}))
	t.Cleanup(upstream2.Close)

	breaker2 := j2Breaker(t)
	before2 := breaker2.Stats()
	pattern2, err := route.Parse("POST /v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	u2, _ := url.Parse(upstream2.URL)
	p2, err := New(
		WithUpstream(u2),
		WithMatcher(route.NewMatcher([]route.Pattern{pattern2})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(breaker2),
		WithTranscodeMapping(transcodeMapping(testMessagesResponsesMapping(t))),
	)
	if err != nil {
		t.Fatal(err)
	}
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	)
	rec2 := httptest.NewRecorder()
	p2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("streamed status = %d, want 200", rec2.Code)
	}
	if got := breaker2.Stats().TotalFailures; got != before2.TotalFailures+1 {
		t.Fatalf("streamed failed envelope not recorded as upstream failure: before=%d after=%d", before2.TotalFailures, got)
	}
}

// TestProxyNewRetryReplayBytesContract proves the retry-replay contract
// (review-k finding 8): a declared RetryReplayBytes must equal the proxy
// retry body cap with retries enabled, or proxy.New fails naming the route
// and both values; equal values construct.
func TestProxyNewRetryReplayBytesContract(t *testing.T) {
	pattern, err := route.Parse("POST /v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://upstream.example")
	base := func() []Option {
		return []Option{
			WithUpstream(u),
			WithMatcher(route.NewMatcher([]route.Pattern{pattern})),
			WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
			WithMetrics(metrics.NewCollector()),
		}
	}

	// Mismatch: the declared replay bound (1 MiB) differs from the proxy
	// retry body cap (32 MiB). The error names the route and both values.
	mismatch := TranscodeMapping{
		Mapping:    testResponsesMapping(t),
		BodyLimits: transcode.BodyLimits{RetryReplayBytes: 1 << 20},
	}
	_, err = New(append(base(),
		WithMaxRetries(3),
		WithMaxBodyBytes(32<<20),
		WithTranscodeMapping(mismatch),
	)...)
	if err == nil {
		t.Fatal("mismatched RetryReplayBytes accepted")
	}
	if !strings.Contains(err.Error(), "POST /v1/responses") ||
		!strings.Contains(err.Error(), "RetryReplayBytes=1048576") ||
		!strings.Contains(err.Error(), "33554432") {
		t.Fatalf("error does not name the route and both values: %v", err)
	}

	// Retries disabled with a non-zero declared bound also fails.
	_, err = New(append(base(), WithTranscodeMapping(mismatch))...)
	if err == nil {
		t.Fatal("declared RetryReplayBytes with retries disabled accepted")
	}
	if !strings.Contains(err.Error(), "retries are disabled") {
		t.Fatalf("error does not report disabled retries: %v", err)
	}

	// Equal values construct.
	equal := TranscodeMapping{
		Mapping:    testResponsesMapping(t),
		BodyLimits: transcode.BodyLimits{RetryReplayBytes: 32 << 20},
	}
	if _, err := New(append(base(),
		WithMaxRetries(3),
		WithMaxBodyBytes(32<<20),
		WithTranscodeMapping(equal),
	)...); err != nil {
		t.Fatalf("equal values rejected: %v", err)
	}

	// An undeclared (zero) bound constructs without retries: no equality
	// check applies to a route that declares no replay bound.
	undeclared := TranscodeMapping{Mapping: testResponsesMapping(t)}
	if _, err := New(append(base(), WithTranscodeMapping(undeclared))...); err != nil {
		t.Fatalf("undeclared replay bound rejected: %v", err)
	}
}

// TestProxyNewRejectsMisconfiguredTranscodeRoutes proves a misconfigured
// route fails at proxy.New, never on the first request (review-j finding 14).
func TestProxyNewRejectsMisconfiguredTranscodeRoutes(t *testing.T) {
	pattern, err := route.Parse("POST /v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://upstream.example")
	base := func() []Option {
		return []Option{
			WithUpstream(u),
			WithMatcher(route.NewMatcher([]route.Pattern{pattern})),
			WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
			WithMetrics(metrics.NewCollector()),
		}
	}
	valid := TranscodeMapping{
		Mapping:    testResponsesMapping(t),
		BodyLimits: transcode.BodyLimits{},
	}

	tests := []struct {
		name    string
		mapping TranscodeMapping
	}{
		{
			name: "negative body limit",
			mapping: func() TranscodeMapping {
				m := valid
				m.BodyLimits = transcode.BodyLimits{AcceptedRequestBytes: -1}
				return m
			}(),
		},
		{
			name: "invalid allowed query key",
			mapping: func() TranscodeMapping {
				m := valid
				m.AllowedClientQuery = map[string]struct{}{"bad key!": {}}
				return m
			}(),
		},
		{
			name: "unknown auth mode",
			mapping: func() TranscodeMapping {
				m := valid
				m.Auth = transcode.AuthPolicy{Mode: transcode.AuthMode("bogus")}
				return m
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := append(base(), WithTranscodeMapping(tt.mapping))
			if _, err := New(options...); err == nil {
				t.Fatal("proxy.New accepted the misconfigured route")
			}
		})
	}
}

// TestProxyTranscodeMidStreamBodyErrorIsUpstreamFailure proves a raw
// non-EOF upstream body failure mid-stream (a connection reset) classifies
// as an upstream failure: breaker failure recorded, phantom hold applied,
// client error event emitted — matching the non-streaming path (review-j
// finding 1: a stream that truncates OR FAILS while its body is read).
func TestProxyTranscodeMidStreamBodyErrorIsUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Reset the connection mid-body with a RST so the client transport
		// observes a raw non-EOF read failure.
		if hijacker, ok := w.(http.Hijacker); ok {
			conn, _, err := hijacker.Hijack()
			if err != nil {
				return
			}
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = conn.Close()
		}
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)
	before := breaker.Stats()
	p := j2LimitedProxy(t, upstream, breaker)

	req1 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`),
	)
	rec1 := httptest.NewRecorder()
	p.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", rec1.Code)
	}
	if !strings.Contains(rec1.Body.String(), `"type":"error"`) {
		t.Fatalf("client must receive an error event: %q", rec1.Body.String())
	}
	if got := breaker.Stats().TotalFailures; got != before.TotalFailures+1 {
		t.Fatalf("mid-stream body failure not recorded as upstream failure: before=%d after=%d", before.TotalFailures, got)
	}

	// The upstream failure holds the slot.
	start := time.Now()
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"y","stream":true}`),
	)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	wait := time.Since(start)
	t.Logf("second request wait after mid-stream body failure: %v", wait)
	if wait < 400*time.Millisecond {
		t.Fatalf("phantom hold not applied to mid-stream body failure: wait %v", wait)
	}
}

// TestProxyTranscodeWrongMediaTypeAppliesPhantomHold proves a 2xx upstream
// response carrying the wrong representation for the negotiated stream mode
// is an UPSTREAM failure that holds the limiter slot: the old local
// classification cancelled a half-open probe and applied no hold, letting a
// broken provider pass for healthy (review-08 blocker 8).
func TestProxyTranscodeWrongMediaTypeAppliesPhantomHold(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
	}))
	t.Cleanup(upstream.Close)

	breaker := j2Breaker(t)
	before := breaker.Stats()
	p := j2LimitedProxy(t, upstream, breaker)

	// The non-streaming request is answered with a stream: the wrong
	// representation must fail the breaker.
	req1 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`),
	)
	rec1 := httptest.NewRecorder()
	p.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusBadGateway {
		t.Fatalf("request 1 status = %d, want 502", rec1.Code)
	}
	if got := breaker.Stats().TotalFailures; got != before.TotalFailures+1 {
		t.Fatalf("wrong media type not recorded as upstream failure: before=%d after=%d", before.TotalFailures, got)
	}

	// The second request MUST wait out the penalty: the slot is held.
	start := time.Now()
	req2 := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"y"}`),
	)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	wait := time.Since(start)
	t.Logf("second request wait after wrong media type 502: %v", wait)
	if wait < 400*time.Millisecond {
		t.Fatalf("wrong media type failure did not hold the slot: wait %v", wait)
	}
}
