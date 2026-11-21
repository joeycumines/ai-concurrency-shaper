package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/config"
	"github.com/joeycumines/ai-concurrency-shaper/internal/history"
	"github.com/joeycumines/ai-concurrency-shaper/internal/investigation"
	"github.com/joeycumines/ai-concurrency-shaper/internal/journal"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
	"github.com/prometheus/client_golang/prometheus"
)

func TestGatewayObservationIntegration(t *testing.T) {
	var attempts atomic.Int64
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) == 1 {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, `{"error":{"message":"try later"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`)
	}))
	defer alpha.Close()
	beta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "opaque native data") }))
	defer beta.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "history # ü.db")
	cfg, err := config.Parse([]string{"-history-db", path, "-diagnostic-capture-bytes", "16384", "-export-dir", dir, "--provider=alpha", "-prefix", "/a", "-upstream", alpha.URL, "-retry", "1", "-retry-wait-min", "1ms", "-retry-wait-max", "1ms", "-retry-min-delay", "0", "-release-cooldown", "0", "-failure-hold", "0", "-circuit-breaker=false", "-transcode-responses-chat", "--provider=beta", "-prefix", "/b", "-upstream", beta.URL, "-retry", "0", "-release-cooldown", "0", "-circuit-breaker=false"})
	if err != nil {
		t.Fatal(err)
	}
	defer cfg.Close()
	if err = cfg.ResolveAndValidate(); err != nil {
		t.Fatal(err)
	}
	rt, err := newGatewayRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = rt.Close(context.Background())
		}
	}()
	var mounts []router.Provider
	var journals []*journal.Journal
	for _, p := range cfg.Providers {
		pr, _, j, err := buildProvider(p, proxy.WithObservation(rt.hub, p.EffectiveName()), proxy.WithRuntimeControl(rt.controls, p.EffectiveName()))
		if err != nil {
			t.Fatal(err)
		}
		journals = append(journals, j)
		mounts = append(mounts, router.Provider{Name: p.EffectiveName(), Prefix: p.Prefix, Proxy: pr})
	}
	dispatch, err := router.New(mounts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt.observeIngress(dispatch))
	defer srv.Close()
	start := time.Now().Add(-time.Second)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{{"POST", "/a/v1/responses", `{"model":"m","input":"hello"}`, 200}, {"POST", "/a/v1/responses", `{"model":"m","input":"hello","new_client_option":true}`, 400}, {"GET", "/b/native", "", 200}, {"GET", "/outside", "", 404}} {
		r, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		response, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != tc.status {
			t.Fatalf("%s status=%d", tc.path, response.StatusCode)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	var facts observation.RequestResult
	for {
		facts, err = rt.hub.Requests(context.Background(), observation.RequestQuery{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		terminal := 0
		for _, f := range facts.Requests {
			if !f.Live {
				terminal++
			}
		}
		if terminal == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal facts=%d", terminal)
		}
		time.Sleep(time.Millisecond)
	}
	if attempts.Load() != 2 {
		t.Fatalf("upstream attempts=%d", attempts.Load())
	}
	ids := map[string]bool{}
	var failed *journal.Entry
	for _, f := range facts.Requests {
		if ids[f.ID] || f.ID == "" {
			t.Fatal("duplicate/missing identity")
		}
		ids[f.ID] = true
		if f.Provider == observation.UnresolvedProvider {
			if f.Status != 404 || f.AttemptCount != 0 {
				t.Fatalf("unmatched request %+v", f)
			}
			continue
		}
		var entry *journal.Entry
		for _, j := range journals {
			if found := j.FindRequestID(f.ID); found != nil {
				entry = found
			}
		}
		if entry == nil || entry.ID != f.JournalID || entry.Diagnostic == nil {
			t.Fatalf("missing exact journal join %+v", f)
		}
		if f.Status == 400 {
			failed = entry
			if f.AttemptCount != 0 || f.Stage != observation.StageConversionRequest || f.Fingerprint == "" {
				t.Fatalf("failed conversion fact %+v", f)
			}
		}
		if f.Provider == "alpha" && f.Status == 200 {
			if f.AttemptCount != 2 || f.Usage == nil || f.Usage.Input != 3 || f.Usage.Output != 2 {
				t.Fatalf("retry/source usage wrong %+v", f)
			}
		}
	}
	if failed == nil || len(failed.Diagnostic.Failures) == 0 || !bytes.Contains(failed.Diagnostic.Legs[0].Body, []byte("new_client_option")) {
		t.Fatal("failed request evidence missing")
	}
	exported, err := investigation.Write(dir, investigation.Selection{Entries: []*journal.Entry{failed}}, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exported)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("new_client_option")) {
		t.Fatal("metadata export included client body/path")
	}
	snap := rt.hub.Snapshot()
	totals := map[string]observation.Counters{}
	for _, p := range snap.Providers {
		totals[p.Provider] = p.Totals
	}
	if totals["alpha"].Received != 2 || totals["alpha"].Terminal != 2 || totals["alpha"].Attempts != 2 || totals["alpha"].Retries != 1 || totals["beta"].Received != 1 || totals[observation.UnresolvedProvider].Received != 1 {
		t.Fatalf("ingress/attempt counts %+v", totals)
	}
	registry := prometheus.NewRegistry()
	if err = registry.Register(rt.hub.PrometheusCollector()); err != nil {
		t.Fatal(err)
	}
	a, err := registry.Gather()
	if err != nil || len(a) < 30 {
		t.Fatalf("Prometheus gather: %d %v", len(a), err)
	}
	b, err := registry.Gather()
	if err != nil || len(b) != len(a) {
		t.Fatal("scrape consumed observations")
	}
	srv.Close()
	end := time.Now().Add(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = rt.Close(ctx); err != nil {
		t.Fatal(err)
	}
	closed = true
	reopened, err := history.Open(path, history.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	persisted, err := reopened.Requests(context.Background(), observation.RequestQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Requests) != 4 {
		t.Fatalf("persisted facts=%d", len(persisted.Requests))
	}
	for _, f := range persisted.Requests {
		if !ids[f.ID] {
			t.Fatalf("reopened identity changed: %s", f.ID)
		}
	}
	intervals, _, truncated, err := reopened.Read(context.Background(), observation.Query{From: start, To: end, MaxPoints: 120}, observation.Exclusion{})
	if err != nil || truncated {
		t.Fatalf("reopened history %v truncated=%v", err, truncated)
	}
	var count uint64
	for _, v := range intervals {
		count += v.Counts.Terminal
		if v.ElapsedNS <= 0 {
			t.Fatal("native monotonic elapsed missing after JSON/SQLite serialization")
		}
	}
	if count != 4 {
		t.Fatalf("history counts=%d", count)
	}
}

func TestGatewayWithoutHistoryCreatesNoFiles(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Parse([]string{"-export-dir", dir, "-upstream", "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	defer cfg.Close()
	if err = cfg.ResolveAndValidate(); err != nil {
		t.Fatal(err)
	}
	rt, err := newGatewayRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rt.hub.Snapshot().History.Enabled {
		t.Fatal("history unexpectedly enabled")
	}
	if err = rt.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("unrequested files created: %v %v", files, err)
	}
}
