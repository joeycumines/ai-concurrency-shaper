package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
)

// TestProxy_QueueCommentsUpstream429EmitsFailedCloseAndRecordsFailure pins the behaviour:
// When upstream returns a non-200 status (e.g. 429) after queue comments committed the
// streaming 200 representation, the client must receive a failed-close comment (: queue-wait failed: upstream)
// followed by a closed stream (never raw JSON), and the breaker must record an upstream failure
// (never a false success).
func TestProxy_QueueCommentsUpstream429EmitsFailedCloseAndRecordsFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "15")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited by upstream"}}`))
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)

	pat, _ := route.Parse("POST /messages:1")
	met := metrics.NewCollector()
	breaker, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(2),
		circuitbreaker.WithOpenTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithBreaker(breaker),
		WithQueueComments(20*time.Millisecond),
		WithQueueTimeout(10*time.Second),
		WithMaxRetries(0),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := string(bodyBytes)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (committed by early SSE commit)", resp.StatusCode)
	}
	if !strings.Contains(body, ": queue-wait elapsed=0") {
		t.Fatalf("expected early queue comment, got: %q", body)
	}
	if !strings.Contains(body, ": queue-wait failed: upstream") {
		t.Fatalf("expected failed close comment ': queue-wait failed: upstream', got: %q", body)
	}
	if strings.Contains(body, "rate limited by upstream") {
		t.Fatalf("raw upstream JSON must not be written into committed stream: %q", body)
	}

	// Same barrier as the half-open probe test below: the breaker is resolved
	// after the handler returns, so a read straight after the body is drained
	// asserts a post-condition that is not yet guaranteed. Reading to EOF
	// narrows that window but does not close it.
	stats := waitForOutcome(t, breaker, 1)
	if stats.Failures != 1 || stats.ConsecutiveFailures != 1 {
		t.Fatalf("expected 1 failure on breaker, got failures=%d consecutive=%d", stats.Failures, stats.ConsecutiveFailures)
	}
}

// TestProxy_QueueCommentsHalfOpenProbeFailureDoesNotCloseBreaker pins the probe handling:
// A HALF_OPEN probe under queue-comments that receives an upstream failure must NOT record
// success or close the circuit.
func TestProxy_QueueCommentsHalfOpenProbeFailureDoesNotCloseBreaker(t *testing.T) {
	// The upstream handler runs on its own goroutine and reads this between
	// requests, so an atomic makes the hand-off explicit instead of relying on
	// a happens-before edge that runs through the network stack, which the
	// race detector does not promise to model. (It has not actually reported
	// this as a race, so this is correctness and clarity, not a caught bug.)
	var statusCode atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(statusCode.Load()))
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)

	pat, _ := route.Parse("POST /messages:1")
	met := metrics.NewCollector()
	breaker, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithOpenTimeout(50*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithBreaker(breaker),
		WithQueueComments(20*time.Millisecond),
		WithQueueTimeout(10*time.Second),
		WithMaxRetries(0),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	// Step 1: Initial failure trips breaker from CLOSED to OPEN.
	statusCode.Store(http.StatusInternalServerError)
	req1, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{}`))
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp1.Body.Close()

	// The proxy resolves the breaker from a defer that runs AFTER the inner
	// handler returns, which is after the client already holds the response.
	// Reading the state here without waiting asserts a post-condition that is
	// not yet guaranteed, and measurably failed ~7% of runs on an idle machine
	// (see waitForOutcome below). Wait for the recording, then assert.
	first := waitForOutcome(t, breaker, 1)
	if first.TotalFailures != 1 {
		t.Fatalf("recorded failures = %d after the 500, want 1", first.TotalFailures)
	}
	if first.State != circuitbreaker.Open {
		t.Fatalf("breaker state = %v after the 500, want OPEN", first.State)
	}

	// Step 2: Wait for openTimeout so breaker transitions to HALF_OPEN.
	time.Sleep(75 * time.Millisecond)

	// Step 3: Probe request arrives with Accept: text/event-stream under queue comments.
	statusCode.Store(http.StatusBadGateway)
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{}`))
	req2.Header.Set("Accept", "text/event-stream")
	req2.Header.Set("Content-Type", "application/json")

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	bodyBytes, _ := io.ReadAll(resp2.Body)
	body := string(bodyBytes)

	if !strings.Contains(body, ": queue-wait failed: upstream") {
		t.Fatalf("expected failed close comment in probe stream, got: %q", body)
	}

	// Step 4: Verify the breaker is NOT closed (it must remain OPEN/re-open on
	// failed probe). Wait for the probe's outcome to be recorded first,
	// otherwise the assertion can pass on the state left by step 1 and never
	// examine the probe at all.
	after := waitForOutcome(t, breaker, 2)
	if after.TotalFailures != 2 {
		t.Fatalf("recorded failures = %d after the failed probe, want 2", after.TotalFailures)
	}
	if after.State == circuitbreaker.Closed {
		t.Fatalf("breaker must NOT close on failed probe under queue comments (was falsely closed by committed 200)")
	}
	if after.State != circuitbreaker.Open {
		t.Fatalf("breaker state = %v after the failed probe, want OPEN (re-opened)", after.State)
	}
}

// TestProxy_QueueCommentsExplicitStreamFalseDoesNotCommitQueueComments pins the behaviour:
// When a request sends Accept: text/event-stream but explicitly specifies "stream": false
// in the JSON request body, queue comments must NOT be committed. The request must complete
// cleanly as a standard non-streaming response.
func TestProxy_QueueCommentsExplicitStreamFalseDoesNotCommitQueueComments(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_123","model":"m","content":"hello"}`))
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)

	pat, _ := route.Parse("POST /messages:1")
	met := metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithQueueComments(20*time.Millisecond),
		WithQueueTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	const reqBody = `{"model":"m","max_tokens":10,"stream":false}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want application/json (stream:false was requested)", ct)
	}
	if strings.Contains(string(respBody), ": queue-wait") {
		t.Fatalf("queue comments must NOT be emitted when stream:false is requested: %q", respBody)
	}
	if !strings.Contains(string(respBody), `"content":"hello"`) {
		t.Fatalf("expected JSON body, got: %q", respBody)
	}
}

type errRoundTripper struct {
	err error
}

func (rt *errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, rt.err
}

// TestProxy_TranscodedRetryCircuitOpenRenders503AndIncrementsCounter pins the behaviour:
// When a transcoded route experiences ErrCircuitOpen (e.g. breaker opened between retries),
// it must render native 503 Service Unavailable with a dialect-correct "circuit open" error
// (never 502 upstream_transport_error), increment shaper_circuit_rejected_total, and avoid
// recording a breaker failure.
func TestProxy_TranscodedRetryCircuitOpenRenders503AndIncrementsCounter(t *testing.T) {
	pat, _ := route.Parse("POST /responses:1")
	met := metrics.NewCollector()
	breaker, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(5),
		circuitbreaker.WithOpenTimeout(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Transport returns ErrCircuitOpen
	customTransport := &errRoundTripper{err: circuitbreaker.ErrCircuitOpen}

	upstreamURL, _ := url.Parse("https://upstream.example")
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{pat})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithBreaker(breaker),
		WithTransport(customTransport),
		WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
	)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 Service Unavailable", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "circuit open") {
		t.Fatalf("expected 'circuit open' in body, got: %q", body)
	}
	if strings.Contains(body, "upstream_transport_error") {
		t.Fatalf("must NOT render 502 upstream_transport_error: %q", body)
	}

	snap := met.Snapshot()
	if snap.TotalCircuitRejected != 1 {
		t.Fatalf("TotalCircuitRejected = %d, want 1", snap.TotalCircuitRejected)
	}

	stats := breaker.Stats()
	if stats.Failures != 0 || stats.ConsecutiveFailures != 0 {
		t.Fatalf("circuit rejection must not record breaker failure: failures=%d consecutive=%d", stats.Failures, stats.ConsecutiveFailures)
	}
}

// waitForOutcome blocks until the breaker has recorded at least n failures and
// returns a consistent snapshot taken at that point.
//
// Why a wait is required: the proxy classifies an exchange and resolves the
// breaker in a defer that runs after the inner handler has returned, so the
// client's Do (and, for a body with a known Content-Length, even its read to
// EOF) can complete before the outcome is recorded. Measured on an idle
// machine, the gap between the client observing the response and the breaker
// recording the failure averaged ~6us and peaked at ~111us, which is why a
// bare read of State() intermittently saw CLOSED. Draining the body narrows
// that window but does not close it, so the wait is the only correct barrier.
func waitForOutcome(t *testing.T, b *circuitbreaker.Breaker, n int) circuitbreaker.Stats {
	t.Helper()
	deadline := time.Now().Add(waitForOutcomeTimeout)
	for {
		st := b.Stats()
		if st.TotalFailures >= int64(n) {
			return st
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("breaker recorded %d failures within %s, want %d", st.TotalFailures, waitForOutcomeTimeout, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForOutcomeTimeout bounds the wait. The recording is bookkeeping that
// runs immediately after the response is written, so this is generous; it only
// has to exceed scheduler delay on a saturated machine, and a breach means a
// real defect rather than a slow machine.
const waitForOutcomeTimeout = 10 * time.Second
