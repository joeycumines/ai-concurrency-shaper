package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/config"
	"github.com/joeycumines/ai-concurrency-shaper/internal/control"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func ingressFixture(t *testing.T, upstream http.Handler) (*gatewayRuntime, *proxy.Proxy, http.Handler, *config.Config) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	cfg, err := config.Parse([]string{"-diagnostic-capture-bytes", "256", "--provider=primary", "-prefix", "/p", "-upstream", server.URL, "-concurrency", "1", "-release-cooldown", "0", "-failure-hold", "0", "-cancel-cooldown", "500ms", "-circuit-breaker=false", "-retry", "0"})
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.ResolveAndValidate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cfg.Close() })
	rt, err := newGatewayRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rt.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	p, _, _, err := buildProvider(cfg.Providers[0], proxy.WithObservation(rt.hub, "primary"), proxy.WithRuntimeControl(rt.controls, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{Name: "shared", Prefix: "/s", Strict: true, DefaultShape: transcode.CatalogShapeAnthropic, ModelRoutes: []router.ModelRoute{{Model: "model-a", Provider: "primary", Handler: p, SupportedRoutes: map[transcode.RouteKey]struct{}{key: {}}}}})
	dispatch, err := router.New([]router.Provider{{Name: "primary", Prefix: "/p", Proxy: p}, {Name: "shared", Prefix: "/s", Proxy: suite}})
	if err != nil {
		t.Fatal(err)
	}
	return rt, p, rt.observeIngress(dispatch), cfg
}
func ingressApply(t *testing.T, rt *gatewayRuntime, cmd control.Command) {
	t.Helper()
	preview, err := rt.controls.Preview(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rt.controls.Apply(preview.Token); err != nil {
		t.Fatal(err)
	}
}
func ingressFacts(t *testing.T, rt *gatewayRuntime, live bool) []observation.RequestFact {
	t.Helper()
	r, err := rt.hub.Requests(context.Background(), observation.RequestQuery{Limit: 100, LiveOnly: live})
	if err != nil {
		t.Fatal(err)
	}
	return r.Requests
}
func ingressWait(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("ingress state did not settle")
}

type ingressCountBody struct {
	reads  atomic.Int64
	bytes  atomic.Int64
	reader io.Reader
}

func (b *ingressCountBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	n, err := b.reader.Read(p)
	b.bytes.Add(int64(n))
	return n, err
}
func (b *ingressCountBody) Close() error { return nil }

func TestIngressSuiteIdentitySurvivesPausedProvider(t *testing.T) {
	var attempts atomic.Int64
	rt, p, handler, _ := ingressFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "opaque upstream response")
	}))
	ingressApply(t, rt, control.Command{Action: control.Pause, Provider: "primary"})
	payload := `{"model":"model-a","messages":[{"role":"user","content":"hello"}]}`
	body := &ingressCountBody{reader: strings.NewReader(payload)}
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/s/v1/messages", body))
		close(done)
	}()
	var live observation.RequestFact
	ingressWait(t, func() bool {
		facts := ingressFacts(t, rt, true)
		if len(facts) == 1 && facts[0].Queued {
			live = facts[0]
			return true
		}
		return false
	})
	if live.ReceivedProvider != observation.UnresolvedProvider || live.Provider != "primary" || live.Path != "/s/v1/messages" || live.Route == "" {
		t.Fatalf("suite handoff corrupted identity: %+v", live)
	}
	if attempts.Load() != 0 || body.bytes.Load() != int64(len(payload)) {
		t.Fatalf("suite inspection vs provider gate: attempts=%d bodybytes=%d", attempts.Load(), body.bytes.Load())
	}
	ingressApply(t, rt, control.Command{Action: control.Resume, Provider: "primary"})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("suite request remained paused")
	}
	facts := ingressFacts(t, rt, false)
	if len(facts) != 1 || facts[0].ID != live.ID || facts[0].Outcome != observation.OutcomeCompleted || facts[0].AttemptCount != 1 {
		t.Fatalf("terminal fact %+v", facts)
	}
	entry := p.Journal().FindRequestID(live.ID)
	if entry == nil || entry.Diagnostic == nil || entry.Diagnostic.Legs[0].ObservedBytes != int64(len(payload)) {
		t.Fatal("original client body was lost or counted twice during suite handoff")
	}
	received := map[string]uint64{}
	terminal := map[string]uint64{}
	for _, s := range rt.hub.Snapshot().Providers {
		received[s.Provider] = s.Totals.Received
		terminal[s.Provider] = s.Totals.Terminal
	}
	if received[observation.UnresolvedProvider] != 1 || received["primary"] != 0 || terminal["primary"] != 1 || terminal[observation.UnresolvedProvider] != 0 {
		t.Fatalf("request counted twice: received=%v terminal=%v", received, terminal)
	}
}

func TestIngressOperatorCancelBeforeHeadersDoesNotReturnSuccessfulHTTP(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCancelled := make(chan struct{})
	rt, _, handler, cfg := ingressFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCancelled)
	}))
	front := httptest.NewServer(handler)
	defer front.Close()
	client := front.Client()
	client.Timeout = 3 * time.Second
	type result struct {
		status int
		err    error
	}
	answer := make(chan result, 1)
	go func() {
		resp, err := client.Post(front.URL+"/s/v1/messages", "application/json", strings.NewReader(`{"model":"model-a"}`))
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			answer <- result{resp.StatusCode, err}
			return
		}
		answer <- result{err: err}
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request missing")
	}
	live := ingressFacts(t, rt, true)
	if len(live) != 1 {
		t.Fatalf("live %+v", live)
	}
	ingressApply(t, rt, control.Command{Action: control.Cancel, Provider: live[0].Provider, RequestID: live[0].ID})
	select {
	case got := <-answer:
		if got.err == nil {
			t.Fatalf("operator-cancelled pre-header request returned HTTP%d instead of aborting", got.status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not reach waiting client")
	}
	select {
	case <-upstreamCancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop upstream transport")
	}
	facts := ingressFacts(t, rt, false)
	if len(facts) != 1 || facts[0].Status != 0 || facts[0].Outcome != observation.OutcomeAborted || !facts[0].Cancelled {
		t.Fatalf("cancel fact %+v", facts)
	}
	if cfg.Providers[0].DefaultLimiter().Stats().Occupied != 1 {
		t.Fatal("operator cancellation skipped accounting hold")
	}
	ingressWait(t, func() bool { return cfg.Providers[0].DefaultLimiter().Stats().Occupied == 0 })
}

type ingressBlockedBody struct {
	started              chan struct{}
	closed               chan struct{}
	startOnce, closeOnce sync.Once
}

func (b *ingressBlockedBody) Read([]byte) (int, error) {
	b.startOnce.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("read interrupted by close")
}
func (b *ingressBlockedBody) Close() error { b.closeOnce.Do(func() { close(b.closed) }); return nil }

func TestIngressCancelUnresolvedSuiteBodyRead(t *testing.T) {
	var attempts atomic.Int64
	rt, _, handler, _ := ingressFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts.Add(1); w.WriteHeader(200) }))
	body := &ingressBlockedBody{started: make(chan struct{}), closed: make(chan struct{})}
	defer body.Close()
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				panic(v)
			}
		}()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/s/v1/messages", body))
	}()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("suite did not inspect body")
	}
	live := ingressFacts(t, rt, true)
	if len(live) != 1 || live[0].Provider != observation.UnresolvedProvider {
		t.Fatalf("unresolved live fact %+v", live)
	}
	ingressApply(t, rt, control.Command{Action: control.Cancel, Provider: observation.UnresolvedProvider, RequestID: live[0].ID})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt unresolved request-body read")
	}
	if attempts.Load() != 0 {
		t.Fatal("cancelled unresolved body reached provider")
	}
	facts := ingressFacts(t, rt, false)
	if len(facts) != 1 || !facts[0].Cancelled || facts[0].AttemptCount != 0 {
		t.Fatalf("cancelled unresolved fact %+v", facts)
	}
}

func TestIngressUnwrittenResponseStatusFollowsActualBoundary(t *testing.T) {
	rt, _, _, _ := ingressFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	normal := rt.observeIngress(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	normal.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/implicit", nil))
	broken := rt.observeIngress(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	func() {
		defer func() {
			if v := recover(); v != http.ErrAbortHandler {
				t.Errorf("panic=%v", v)
			}
		}()
		broken.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
	}()
	facts := ingressFacts(t, rt, false)
	if len(facts) != 2 {
		t.Fatalf("facts %+v", facts)
	}
	for _, f := range facts {
		switch f.Path {
		case "/implicit":
			if f.Status != 200 || f.Outcome != observation.OutcomeCompleted {
				t.Fatalf("normal implicit status %+v", f)
			}
		case "/panic":
			if f.Status != 0 || f.Outcome != observation.OutcomeFailed || f.Stage != observation.StageProxy {
				t.Fatalf("panic fabricated HTTP status %+v", f)
			}
		}
	}
}

func TestIngressNormalCompletionRetainsKeepalive(t *testing.T) {
	rt, _, handler, _ := ingressFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	front := httptest.NewUnstartedServer(handler)
	var connections atomic.Int64
	front.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	front.Start()
	defer front.Close()
	transport := &http.Transport{MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	for range 12 {
		resp, err := client.Post(front.URL+"/s/v1/messages", "application/json", strings.NewReader(`{"model":"model-a"}`))
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("status=%d read=%v", resp.StatusCode, err)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("ordinary Finish cancellation disrupted keepalive: frontend connections=%d", connections.Load())
	}
	if facts := ingressFacts(t, rt, false); len(facts) != 12 {
		t.Fatalf("terminal requests=%d", len(facts))
	}
}

func TestIngressSuiteRefusalRetainsInvestigableEvidence(t *testing.T) {
	var attempts atomic.Int64
	rt, _, handler, _ := ingressFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts.Add(1); w.WriteHeader(200) }))
	payload := `{"model":"not-configured"}`
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/s/v1/messages", strings.NewReader(payload)))
	facts := ingressFacts(t, rt, false)
	if len(facts) != 1 {
		t.Fatalf("facts %+v", facts)
	}
	f := facts[0]
	if rr.Code != 404 || f.Status != 404 || f.Outcome != observation.OutcomeRejected || f.Provider != observation.UnresolvedProvider || f.AttemptCount != 0 || attempts.Load() != 0 {
		t.Fatalf("suite refusal fabricated upstream activity: %+v HTTP%d", f, rr.Code)
	}
	entry := rt.ingressJournal.FindRequestID(f.ID)
	if entry == nil || entry.ID != f.JournalID || entry.Diagnostic == nil || f.Fingerprint == "" {
		t.Fatal("suite failure lost its request evidence")
	}
	if entry.Diagnostic.Legs[0].ObservedBytes != int64(len(payload)) || string(entry.Diagnostic.Legs[0].Body) != payload || entry.Diagnostic.Legs[2].Observed {
		t.Fatalf("partial routing evidence %+v", entry.Diagnostic.Legs)
	}
	if entry.Aborted || entry.Timing.ResponseComplete.IsZero() {
		t.Fatal("fully delivered local error was labelled incomplete")
	}
}

func TestIngressSuiteBufferWaitIsObservedAndCancellable(t *testing.T) {
	var calls atomic.Int64
	rt, p, _, _ := ingressFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	key, _ := transcode.NewRouteKey(http.MethodPost, "/v1/messages")
	limits := transcode.BodyLimits{AcceptedRequestBytes: 128 << 20}
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{Name: "shared", Strict: true, DefaultShape: transcode.CatalogShapeAnthropic, Limits: limits, Admission: router.NewCatalogSuiteAdmission(limits), ModelRoutes: []router.ModelRoute{{Model: "model-a", Provider: "primary", Handler: p, SupportedRoutes: map[transcode.RouteKey]struct{}{key: {}}}}})
	dispatch, err := router.New([]router.Provider{{Name: "shared", Prefix: "/s", Proxy: suite}})
	if err != nil {
		t.Fatal(err)
	}
	handler := rt.observeIngress(dispatch)
	ingressApply(t, rt, control.Command{Action: control.Pause, Provider: "primary"})
	firstDone := make(chan struct{})
	firstResponse := httptest.NewRecorder()
	go func() {
		defer close(firstDone)
		handler.ServeHTTP(firstResponse, httptest.NewRequest(http.MethodPost, "/s/v1/messages", strings.NewReader(`{"model":"model-a"}`)))
	}()
	ingressWait(t, func() bool {
		facts := ingressFacts(t, rt, true)
		return len(facts) == 1 && facts[0].Provider == "primary" && facts[0].Queued
	})
	body := &ingressCountBody{reader: strings.NewReader(`{"model":"model-a"}`)}
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		defer func() {
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				panic(v)
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/s/v1/messages", body))
	}()
	var waitingID string
	ingressWait(t, func() bool {
		for _, f := range ingressFacts(t, rt, true) {
			if f.Provider == observation.UnresolvedProvider && f.Queued {
				waitingID = f.ID
				return true
			}
		}
		return false
	})
	if body.reads.Load() != 0 {
		t.Fatal("suite read a body before acquiring buffer admission")
	}
	ingressApply(t, rt, control.Command{Action: control.Cancel, Provider: observation.UnresolvedProvider, RequestID: waitingID})
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("cancel did not release suite buffer waiter")
	}
	ingressApply(t, rt, control.Command{Action: control.Resume, Provider: "primary"})
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("original buffered request did not complete")
	}
	if calls.Load() != 1 || firstResponse.Code != 200 {
		t.Fatalf("buffer lease corruption: attempts=%d status=%d", calls.Load(), firstResponse.Code)
	}
	facts := ingressFacts(t, rt, false)
	if len(facts) != 2 {
		t.Fatalf("facts=%d", len(facts))
	}
	for _, f := range facts {
		if f.QueueNS <= 0 || !f.Waited {
			t.Fatalf("admission phase not measured: %+v", f)
		}
		if f.ID == waitingID && (!f.Cancelled || f.AttemptCount != 0) {
			t.Fatalf("buffer cancellation provenance %+v", f)
		}
	}
	var waitSamples uint64
	for _, s := range rt.hub.Snapshot().Providers {
		waitSamples += s.Admission.Count
		if s.Queued != 0 {
			t.Fatalf("queued gauge leaked %+v", s)
		}
	}
	if waitSamples != 2 {
		t.Fatalf("admission union samples=%d, want one per waiting logical request", waitSamples)
	}
}
