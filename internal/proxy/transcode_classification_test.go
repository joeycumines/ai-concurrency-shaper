package proxy

// J2 regression tests for the unified exchange classification (review-j
// findings 1 and 2): one immutable exchange result drives slot holds, breaker
// resolution, completion counters, and journal finalization for both
// admission paths.

import (
	"context"
	"io"
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
// consecutive failures (review-j finding 1, false-penalty direction).
func TestProxyTranscodeLocalConversion502NoPhantomHold(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"not":"chat"}`))
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
