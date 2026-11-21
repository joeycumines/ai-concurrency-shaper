package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/auth"
	"github.com/joeycumines/ai-concurrency-shaper/internal/control"
	"github.com/joeycumines/ai-concurrency-shaper/internal/diagnostic"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func observedFixture(t *testing.T, opts ...Option) (*Proxy, *observation.Hub, *control.Controller, *queue.Limiter) {
	t.Helper()
	h := observation.New(observation.Config{Providers: []string{"primary"}, ManualTicks: true})
	c := control.New()
	l := queue.NewLimiterWithCooldown(1, 0)
	if err := c.RegisterProvider("primary", map[string]*queue.Limiter{"default": l}); err != nil {
		t.Fatal(err)
	}
	c.SetCanceller(h.Cancel)
	t.Cleanup(func() {
		c.Close()
		if err := h.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	target, _ := url.Parse("http://upstream.invalid")
	limited, _ := route.Parse("POST /v1/messages")
	unlimited, _ := route.Parse("GET /unlimited:unlimited")
	options := []Option{WithUpstream(target), WithMatcher(route.NewMatcher([]route.Pattern{unlimited, limited})), WithLimiter(l), WithMetrics(metrics.NewCollector()), WithJournal(journal.New(32, 4096)), WithObservation(h, "primary"), WithRuntimeControl(c, "primary")}
	options = append(options, opts...)
	p, err := New(options...)
	if err != nil {
		t.Fatal(err)
	}
	return p, h, c, l
}
func controlApply(t *testing.T, c *control.Controller, cmd control.Command) {
	t.Helper()
	v, err := c.Preview(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Apply(v.Token); err != nil {
		t.Fatal(err)
	}
}
func observedFacts(t *testing.T, h *observation.Hub, live bool) []observation.RequestFact {
	t.Helper()
	r, err := h.Requests(context.Background(), observation.RequestQuery{Limit: 100, LiveOnly: live})
	if err != nil {
		t.Fatal(err)
	}
	return r.Requests
}
func primaryTotals(t *testing.T, h *observation.Hub) observation.ProviderSnapshot {
	t.Helper()
	for _, s := range h.Snapshot().Providers {
		if s.Provider == "primary" {
			return s
		}
	}
	t.Fatal("provider missing")
	return observation.ProviderSnapshot{}
}
func untilObserved(t *testing.T, fn func() bool) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("observation timed out")
}
func okResponse(req *http.Request) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: req}
}

type countedRequestBody struct {
	reads  atomic.Int64
	reader io.Reader
}

func (b *countedRequestBody) Read(p []byte) (int, error) { b.reads.Add(1); return b.reader.Read(p) }
func (b *countedRequestBody) Close() error               { return nil }

func TestRuntimePauseBeforeBodyAndBreakerThenResume(t *testing.T) {
	var attempts atomic.Int64
	b := halfOpenBreaker(t)
	breakerBefore := b.Stats()
	p, h, c, _ := observedFixture(t, WithBreaker(b), WithTransport(transportchainRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		_, _ = io.ReadAll(r.Body)
		return okResponse(r), nil
	})))
	controlApply(t, c, control.Command{Action: control.Pause, Provider: "primary"})
	body := &countedRequestBody{reader: strings.NewReader("payload")}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { p.ServeHTTP(rr, req); close(done) }()
	untilObserved(t, func() bool { return primaryTotals(t, h).Queued == 1 })
	if body.reads.Load() != 0 || attempts.Load() != 0 {
		t.Fatal("paused request read its body or reached upstream")
	}
	if s := b.Stats(); s.TotalSuccesses != breakerBefore.TotalSuccesses || s.TotalFailures != breakerBefore.TotalFailures {
		t.Fatalf("pause changed breaker history: %+v", s)
	}
	controlApply(t, c, control.Command{Action: control.Resume, Provider: "primary"})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("resume did not admit")
	}
	if rr.Code != 200 || attempts.Load() != 1 || body.reads.Load() == 0 {
		t.Fatalf("response=%d attempts=%d reads=%d", rr.Code, attempts.Load(), body.reads.Load())
	}
	facts := observedFacts(t, h, false)
	if len(facts) != 1 || facts[0].QueueNS <= 0 || facts[0].Outcome != observation.OutcomeCompleted {
		t.Fatalf("facts %+v", facts)
	}
	if s := primaryTotals(t, h); s.Admission.Count != 1 || s.Queued != 0 || s.Active != 0 {
		t.Fatalf("queue accounting %+v", s)
	}
}

func TestRuntimeDrainRejectsAllNewTrafficButKeepsCatalog(t *testing.T) {
	var calls atomic.Int64
	p, h, c, _ := observedFixture(t, WithLimitAll(true), WithModelCatalog(testCatalogConfig(128000)), WithTransport(transportchainRoundTripperFunc(func(r *http.Request) (*http.Response, error) { calls.Add(1); return okResponse(r), nil })))
	controlApply(t, c, control.Command{Action: control.Drain, Provider: "primary"})
	for _, path := range []string{"/v1/messages", "/unlimited", "/health"} {
		method := http.MethodGet
		if path == "/v1/messages" {
			method = http.MethodPost
		}
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, httptest.NewRequest(method, path, nil))
		if rr.Code != 503 {
			t.Fatalf("%s=%d", path, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rr.Code != 200 || calls.Load() != 0 {
		t.Fatalf("catalog=%d upstreamcalls=%d", rr.Code, calls.Load())
	}
	s := primaryTotals(t, h)
	if s.Totals.Rejected != 3 || s.Totals.Completed != 1 || s.Totals.Attempts != 0 || s.Totals.HTTP[1] != [5]uint64{} {
		t.Fatalf("local statuses fabricated upstream facts: %+v", s.Totals)
	}
}

func TestRuntimePauseKeepsOriginalQueueDeadline(t *testing.T) {
	var calls atomic.Int64
	p, h, c, l := observedFixture(t, WithQueueTimeout(120*time.Millisecond), WithTransport(transportchainRoundTripperFunc(func(r *http.Request) (*http.Response, error) { calls.Add(1); return okResponse(r), nil })))
	release, _ := l.Acquire(context.Background())
	defer release()
	controlApply(t, c, control.Command{Action: control.Pause, Provider: "primary"})
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	start := time.Now()
	go func() { p.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/messages", nil)); close(done) }()
	untilObserved(t, func() bool { return primaryTotals(t, h).Queued == 1 })
	time.Sleep(80 * time.Millisecond)
	controlApply(t, c, control.Command{Action: control.Resume, Provider: "primary"})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("deadline not honored")
	}
	if rr.Code != 504 || calls.Load() != 0 {
		t.Fatalf("status=%d upstream=%d", rr.Code, calls.Load())
	}
	if elapsed := time.Since(start); elapsed >= 195*time.Millisecond {
		t.Fatalf("pause restarted queue deadline: %v", elapsed)
	}
	if s := primaryTotals(t, h); s.Admission.Count != 1 || s.Totals.Attempts != 0 {
		t.Fatalf("phase union %+v", s)
	}
}

func TestRuntimeCancelExactLiveRequestPreservesHold(t *testing.T) {
	started := make(chan struct{})
	p, h, c, l := observedFixture(t, WithCancelCooldown(120*time.Millisecond), WithTransport(transportchainRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})))
	done := make(chan struct{})
	go func() {
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		close(done)
	}()
	<-started
	facts := observedFacts(t, h, true)
	if len(facts) != 1 {
		t.Fatalf("livefacts %+v", facts)
	}
	id := facts[0].ID
	controlApply(t, c, control.Command{Action: control.Cancel, Provider: "primary", RequestID: id})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not reach request")
	}
	if l.Stats().Occupied != 1 {
		t.Fatal("cancel bypassed upstream accounting hold")
	}
	facts = observedFacts(t, h, false)
	if len(facts) != 1 || facts[0].ID != id || !facts[0].Cancelled || facts[0].Outcome != observation.OutcomeAborted {
		t.Fatalf("cancelled fact %+v", facts)
	}
	untilObserved(t, func() bool { return l.Stats().Occupied == 0 })
}

func TestObservedRetriesKeepOneLogicalIDAndJournalJoin(t *testing.T) {
	var count atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte("retry"))
			return
		}
		_, _ = w.Write([]byte("complete"))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	p, h, _, _ := observedFixture(t, WithUpstream(target), WithMaxRetries(1), WithMaxBodyBytes(4096), WithRetryWaitMin(time.Millisecond), WithRetryWaitMax(time.Millisecond))
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("body")))
	facts := observedFacts(t, h, false)
	if len(facts) != 1 {
		t.Fatalf("facts=%d", len(facts))
	}
	f := facts[0]
	if rr.Code != 200 || f.AttemptCount != 2 || f.Attempts[0].Kind != observation.AttemptInitial || f.Attempts[1].Kind != observation.AttemptRetry || f.Outcome != observation.OutcomeCompleted {
		t.Fatalf("status=%d fact=%+v", rr.Code, f)
	}
	e := p.Journal().FindRequestID(f.ID)
	if e == nil || e.ID != f.JournalID {
		t.Fatal("exact journal join missing at fact publication")
	}
	totals := primaryTotals(t, h).Totals
	if totals.Received != 1 || totals.Terminal != 1 || totals.Attempts != 2 || totals.Retries != 1 || totals.Upstream5xx != 1 || totals.HTTP[0][1] != 1 || totals.HTTP[1][1] != 1 || totals.HTTP[1][4] != 1 {
		t.Fatalf("counts %+v", totals)
	}
	if primaryTotals(t, h).Admission.Count != 0 {
		t.Fatal("idle request fabricated a queue wait")
	}
}

func TestObservedHeaderSuccessDoesNotHideBodyFailureOrCancel(t *testing.T) {
	for _, cancelDuringWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-failure", true: "read-failure-and-cancel"}[cancelDuringWrite], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p, h, _, _ := observedFixture(t, WithTransport(transportchainRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &readChunkWithErrorBody{chunk: []byte("partial"), err: io.ErrUnexpectedEOF}, Request: r}, nil
			})))
			var w http.ResponseWriter = httptest.NewRecorder()
			if cancelDuringWrite {
				w = &cancelingErrorResponseWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, err: io.ErrClosedPipe}
			}
			func() {
				defer func() {
					if v := recover(); v != nil && v != http.ErrAbortHandler {
						panic(v)
					}
				}()
				p.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx))
			}()
			f := observedFacts(t, h, false)[0]
			if f.Status != 200 || f.Outcome != observation.OutcomeFailed || f.Stage != observation.StageUpstream || f.Attempts[0].Outcome != observation.OutcomeFailed {
				t.Fatalf("body failure lost: %+v", f)
			}
			if primaryTotals(t, h).Totals.HTTP[1][1] != 1 {
				t.Fatal("observed upstream200 lost")
			}
		})
	}
}

func TestObservedAuthResendCountedAfterSigningOnly(t *testing.T) {
	for _, failSigner := range []bool{false, true} {
		t.Run(map[bool]string{false: "resend", true: "signer-fails"}[failSigner], func(t *testing.T) {
			h := observation.New(observation.Config{Providers: []string{"primary"}, ManualTicks: true})
			defer h.Close(context.Background())
			ctx, request := h.Begin(context.Background(), observation.BeginInfo{Provider: "primary"})
			ctx = context.WithValue(ctx, observationStateKey{}, &transportObservationState{})
			if failSigner {
				ctx = transcode.WithRequestSigner(ctx, &failingResendSigner{})
			}
			req := httptest.NewRequest(http.MethodGet, "http://upstream.invalid/", nil).WithContext(ctx)
			source := &rotatingSecretSource{current: "stale"}
			auth.AttachReauthentication(req, source, "stale", "Authorization", "Bearer ")
			var calls int
			transport := buildUpstreamTransport(&proxyConfig{transport: transportchainRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				resp := okResponse(r)
				if calls == 1 {
					resp.StatusCode = 401
				}
				return resp, nil
			})})
			resp, err := transport.RoundTrip(req)
			if resp != nil {
				_, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}
			request.Finish(observation.FinishInfo{Outcome: observation.OutcomeCompleted, Status: 200})
			want := uint64(2)
			if failSigner {
				want = 1
				if err == nil {
					t.Fatal("expected signer error")
				}
			}
			f := observedFacts(t, h, false)[0]
			if f.AttemptCount != want || uint64(calls) != want {
				t.Fatalf("actual attempts=%d facts=%+v", calls, f)
			}
			if !failSigner && (f.Attempts[1].Kind != observation.AttemptAuthResend || primaryTotals(t, h).Totals.AuthResends != 1) {
				t.Fatalf("auth resend classification %+v", f)
			}
		})
	}
}

func TestObservedUpgradeDoesNotWrapBidirectionalBody(t *testing.T) {
	h := observation.New(observation.Config{Providers: []string{"primary"}, ManualTicks: true})
	defer h.Close(context.Background())
	ctx, r := h.Begin(context.Background(), observation.BeginInfo{Provider: "primary"})
	ctx = context.WithValue(ctx, observationStateKey{}, &transportObservationState{})
	body := &upgradeObservationBody{}
	resp, err := observeAttempt(httptest.NewRequest(http.MethodGet, "http://upstream.invalid/", nil).WithContext(ctx), transportchainRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 101, Body: body, Header: make(http.Header)}, nil
	}))
	if err != nil || resp.Body != body {
		t.Fatalf("upgraded body was wrapped: %T %v", resp.Body, err)
	}
	r.Finish(observation.FinishInfo{Outcome: observation.OutcomeCompleted, Status: 101})
	f := observedFacts(t, h, false)[0]
	if f.Attempts[0].Outcome != observation.OutcomeCompleted {
		t.Fatalf("handshake outcome %+v", f)
	}
}

type upgradeObservationBody struct{}

func (*upgradeObservationBody) Read([]byte) (int, error)    { return 0, io.EOF }
func (*upgradeObservationBody) Write(p []byte) (int, error) { return len(p), nil }
func (*upgradeObservationBody) Close() error                { return nil }
func (*upgradeObservationBody) CloseWrite() error           { return nil }

func TestObservedIngressIDSurvivesProviderAndDiagnosticJournal(t *testing.T) {
	p, h, _, _ := observedFixture(t, WithTransport(transportchainRoundTripperFunc(func(r *http.Request) (*http.Response, error) { return okResponse(r), nil })))
	ctx, logical := h.Begin(context.Background(), observation.BeginInfo{Provider: observation.UnresolvedProvider, Method: "GET", Path: "/health"})
	cap := diagnostic.New(logical.ID(), 64)
	ctx = diagnostic.WithContext(ctx, cap)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil).WithContext(ctx))
	f := observedFacts(t, h, false)[0]
	if f.ID != logical.ID() || f.ReceivedProvider != observation.UnresolvedProvider || f.Provider != "primary" {
		t.Fatalf("provider handoff changed identity: %+v", f)
	}
	e := p.Journal().FindRequestID(f.ID)
	if e == nil || e.Diagnostic == nil || e.Diagnostic.RequestID != f.ID {
		t.Fatal("diagnostic join missing")
	}
	if primaryTotals(t, h).Totals.Received != 0 {
		t.Fatal("provider handoff counted second received request")
	}
}

func TestObservedLocalSignerFailureHasNoUpstreamHTTPStatus(t *testing.T) {
	h := observation.New(observation.Config{Providers: []string{"primary"}, ManualTicks: true})
	defer h.Close(context.Background())
	ctx, r := h.Begin(context.Background(), observation.BeginInfo{Provider: "primary"})
	ctx = context.WithValue(ctx, observationStateKey{}, &transportObservationState{})
	signCtx, cancel := context.WithCancel(ctx)
	req := httptest.NewRequest(http.MethodGet, "http://upstream.invalid/", nil).WithContext(transcode.WithRequestSigner(signCtx, cancelingRequestSigner{cancel: cancel}))
	called := false
	_, err := buildUpstreamTransport(&proxyConfig{transport: transportchainRoundTripperFunc(func(*http.Request) (*http.Response, error) { called = true; return nil, errors.New("should not send") })}).RoundTrip(req)
	if err == nil || called {
		t.Fatal("signer failure reached transport")
	}
	r.Finish(observation.FinishInfo{Status: 502, Outcome: observation.OutcomeFailed, Stage: observation.StageAuthentication})
	s := primaryTotals(t, h)
	if s.Totals.Attempts != 0 || s.Totals.HTTP[1] != [5]uint64{} {
		t.Fatalf("invented upstream request %+v", s.Totals)
	}
}

func TestRuntimeDrainPreservesPreviouslyPermittedQueue(t *testing.T) {
	var calls atomic.Int64
	p, h, c, l := observedFixture(t, WithTransport(transportchainRoundTripperFunc(func(r *http.Request) (*http.Response, error) { calls.Add(1); return okResponse(r), nil })))
	release, _ := l.Acquire(context.Background())
	done := make(chan struct{})
	rr := httptest.NewRecorder()
	go func() { p.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/messages", nil)); close(done) }()
	untilObserved(t, func() bool { return l.Stats().Waiters == 1 })
	controlApply(t, c, control.Command{Action: control.Drain, Provider: "primary"})
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drain revoked an existing admission permit")
	}
	if rr.Code != 200 || calls.Load() != 1 {
		t.Fatalf("queuedrequest status=%d attempts=%d", rr.Code, calls.Load())
	}
	rejected := httptest.NewRecorder()
	p.ServeHTTP(rejected, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if rejected.Code != 503 || calls.Load() != 1 {
		t.Fatal("drain admitted new arrival")
	}
	if s := primaryTotals(t, h); s.Totals.Completed != 1 || s.Totals.Rejected != 1 || s.Admission.Count != 1 {
		t.Fatalf("cohorts %+v", s)
	}
}

func TestObservedDownstreamWriteFailureIsDistinctFromCancellation(t *testing.T) {
	p, h, _, _ := observedFixture(t, WithTransport(transportchainRoundTripperFunc(func(r *http.Request) (*http.Response, error) { return okResponse(r), nil })))
	w := &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: io.ErrClosedPipe}
	func() {
		defer func() {
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				panic(v)
			}
		}()
		p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	}()
	f := observedFacts(t, h, false)[0]
	if f.Outcome != observation.OutcomeFailed || f.Stage != observation.StageDownstream || f.Cancelled {
		t.Fatalf("write failure classified inaccurately: %+v", f)
	}
	if s := primaryTotals(t, h); s.Totals.Failures[4] != 1 || s.Totals.Completed != 0 {
		t.Fatalf("failure stage not exported %+v", s.Totals)
	}
}

func TestObservedUpstreamFailureSurvivesDownstreamCancel(t *testing.T) {
	p, h, _, _ := observedFixture(t, WithTransport(transportchainRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		resp := okResponse(r)
		resp.StatusCode = 503
		return resp, nil
	})))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &cancelingErrorResponseWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, err: io.ErrClosedPipe}
	func() {
		defer func() {
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				panic(v)
			}
		}()
		p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil).WithContext(ctx))
	}()
	f := observedFacts(t, h, false)[0]
	if f.Outcome != observation.OutcomeFailed || f.Stage != observation.StageUpstream || !f.Cancelled {
		t.Fatalf("known status failure lost to cancel: %+v", f)
	}
}

func TestObservedFailureEvidencePublishedWithExactFact(t *testing.T) {
	for _, downstream := range []bool{false, true} {
		t.Run(map[bool]string{false: "upstream-body", true: "downstream-write"}[downstream], func(t *testing.T) {
			p, h, _, _ := observedFixture(t, WithTransport(transportchainRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
				resp := okResponse(r)
				if !downstream {
					resp.Body = &readChunkWithErrorBody{chunk: []byte("partial"), err: io.ErrUnexpectedEOF}
				}
				return resp, nil
			})))
			ctx, request := h.Begin(context.Background(), observation.BeginInfo{Provider: "primary", Method: "GET", Path: "/health"})
			capture := diagnostic.New(request.ID(), 64)
			ctx = diagnostic.WithContext(ctx, capture)
			var out http.ResponseWriter = httptest.NewRecorder()
			if downstream {
				out = &errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: io.ErrClosedPipe}
			}
			out = diagnostic.WrapWriter(out, capture)
			func() {
				defer func() {
					if v := recover(); v != nil && v != http.ErrAbortHandler {
						panic(v)
					}
				}()
				p.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/health", nil).WithContext(ctx))
			}()
			f := observedFacts(t, h, false)[0]
			e := p.Journal().FindRequestID(f.ID)
			if e == nil || e.Diagnostic == nil || f.Fingerprint == "" {
				t.Fatalf("failed exchange evidence missing: fact=%+v entry=%+v", f, e)
			}
			found := false
			for _, failure := range e.Diagnostic.Failures {
				if failure.Fingerprint == f.Fingerprint {
					found = true
				}
			}
			if !found {
				t.Fatal("fact fingerprint not attached to exact journal entry")
			}
			if e.Diagnostic.Legs[2].Status != 200 || !e.Diagnostic.Legs[2].Observed || e.Diagnostic.Legs[3].Complete {
				t.Fatalf("header/body facts wrong %+v", e.Diagnostic.Legs)
			}
			if downstream && e.Diagnostic.Legs[3].ObservedBytes != 0 {
				t.Fatal("counted offered bytes instead of accepted bytes")
			}
			if !downstream && (e.Diagnostic.Legs[2].ObservedBytes != 7 || !e.Diagnostic.Legs[2].BytesKnown) {
				t.Fatalf("selected native bytes missing: %+v", e.Diagnostic.Legs[2])
			}
			if !downstream && e.Diagnostic.Legs[3].ObservedBytes != 7 {
				t.Fatalf("accepted partial bytes=%d", e.Diagnostic.Legs[3].ObservedBytes)
			}
		})
	}
}

func TestObservedFailedResendSignerClassifiesLocalAuthentication(t *testing.T) {
	var calls atomic.Int64
	source := &rotatingSecretSource{current: "stale"}
	p, h, _, _ := observedFixture(t, WithAuthPolicy(&auth.AuthPolicy{Mode: auth.AuthBearer, Secret: source}), WithTransport(transportchainRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		resp := okResponse(req)
		resp.StatusCode = 401
		return resp, nil
	})))
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req = req.WithContext(transcode.WithRequestSigner(req.Context(), &failingResendSigner{}))
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	f := observedFacts(t, h, false)[0]
	if calls.Load() != 1 || f.AttemptCount != 1 || f.Stage != observation.StageAuthentication || f.Outcome != observation.OutcomeFailed {
		t.Fatalf("local signer failure conflated with prior upstream401: %+v calls=%d", f, calls.Load())
	}
	if s := primaryTotals(t, h); s.Totals.HTTP[1][3] != 1 || s.Totals.AuthResends != 0 || s.Totals.Failures[2] != 1 {
		t.Fatalf("signer failure counters %+v", s.Totals)
	}
}

func TestObservedConvertedSelectedBodyBytesCountedExactlyOnce(t *testing.T) {
	upstreamBody := `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`
	p, h, _, _ := observedFixture(t, WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))), WithTransport(transportchainRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(upstreamBody)), Request: req}, nil
	})))
	ctx, r := h.Begin(context.Background(), observation.BeginInfo{Provider: "primary"})
	c := diagnostic.New(r.ID(), 4096)
	ctx = diagnostic.WithContext(ctx, c)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`)).WithContext(ctx)
	req.Body = c.WrapBody(diagnostic.ClientRequest, req.Body, "application/json", 0)
	rr := httptest.NewRecorder()
	p.ServeHTTP(diagnostic.WrapWriter(rr, c), req)
	f := observedFacts(t, h, false)[0]
	e := p.Journal().FindRequestID(f.ID)
	if f.Outcome != observation.OutcomeCompleted || e == nil || e.Diagnostic == nil {
		t.Fatalf("conversion failed HTTP%d %+v", rr.Code, f)
	}
	leg := e.Diagnostic.Legs[2]
	if !leg.Complete || !leg.BytesKnown || leg.ObservedBytes != int64(len(upstreamBody)) || string(leg.Body) != upstreamBody {
		t.Fatalf("selected converted upstream response double-counted or altered: %+v", leg)
	}
}
