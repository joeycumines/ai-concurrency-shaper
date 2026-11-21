package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/config"
	"github.com/joeycumines/ai-concurrency-shaper/internal/history"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

func shutdownRuntimeFixture(t *testing.T) (*gatewayRuntime, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shutdown-history.db")
	cfg, err := config.Parse([]string{"-history-db", path, "--provider=primary", "-prefix", "/p", "-upstream", "http://127.0.0.1:1", "-release-cooldown", "0", "-circuit-breaker=false"})
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.ResolveAndValidate(); err != nil {
		t.Fatal(err)
	}
	rt, err := newGatewayRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := rt.Close(ctx); err != nil {
			t.Error(err)
		}
		cfg.Close()
	})
	return rt, path
}
func persistedShutdownFact(t *testing.T, path string) observation.RequestFact {
	t.Helper()
	store, err := history.Open(path, history.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(context.Background())
	result, err := store.Requests(context.Background(), observation.RequestQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || len(result.Requests) != 1 {
		t.Fatalf("terminal fact count=%+v", result)
	}
	fact := result.Requests[0]
	if fact.Live || fact.Outcome != observation.OutcomeAborted || !fact.Cancelled || fact.JournalID == 0 || fact.AttemptCount != 0 {
		t.Fatalf("shutdown terminal fact=%+v", fact)
	}
	return fact
}
func TestShutdownGraceExpiryPersistsFinalIngressFact(t *testing.T) {
	rt, path := shutdownRuntimeFixture(t)
	entered := make(chan struct{})
	handlerReturned := make(chan struct{})
	srv := &http.Server{Handler: rt.observeIngress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(handlerReturned)
	}))}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	defer srv.Close()
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ln) }()
	clientDone := make(chan error, 1)
	client := &http.Client{Timeout: 3 * time.Second}
	go func() {
		response, err := client.Get("http://" + ln.Addr().String() + "/slow")
		if response != nil {
			response.Body.Close()
		}
		clientDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	if err = shutdownServers(20*time.Millisecond, srv); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("grace expiry was hidden: %v", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = rt.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handlerReturned:
	default:
		t.Fatal("history sealed before handler returned")
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("force close did not release client")
	}
	select {
	case err := <-serveDone:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener not closed")
	}
	fact := persistedShutdownFact(t, path)
	if fact.DurationNS <= 0 {
		t.Fatal("final duration missing")
	}
	if rt.ingressJournal.FindRequestID(fact.ID) == nil {
		t.Fatal("persisted fact preceded exact journal publication")
	}
}
func TestRuntimeCloseTimeoutLeavesRecorderOpenUntilIngressJoined(t *testing.T) {
	rt, path := shutdownRuntimeFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	done := make(chan struct{})
	handler := rt.observeIngress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	go func() {
		defer close(done)
		defer func() {
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				t.Errorf("handler panic: %v", v)
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ignores-context", nil))
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	err := rt.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("noncooperative handler close=%v", err)
	}
	live, err := rt.hub.Requests(context.Background(), observation.RequestQuery{LiveOnly: true})
	if err != nil || len(live.Requests) != 1 {
		t.Fatalf("recorder prematurely closed: %+v %v", live, err)
	}
	late := httptest.NewRecorder()
	handler.ServeHTTP(late, httptest.NewRequest(http.MethodGet, "/late", nil))
	if late.Code != http.StatusServiceUnavailable {
		t.Fatalf("late ingress=%d", late.Code)
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("released handler did not finalize")
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer closeCancel()
	if err = rt.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	fact := persistedShutdownFact(t, path)
	if fact.ID != live.Requests[0].ID {
		t.Fatal("shutdown changed request identity")
	}
}
