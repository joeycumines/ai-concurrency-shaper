package proxy

// The gateway-hosted model catalog: a mount-scoped, locally rendered discovery
// document served in the unlimited admission class.

import (
	"encoding/json"
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

func testCatalogConfig(context int) transcode.CatalogConfig {
	return transcode.CatalogConfig{
		ProviderName:      "TestProv",
		ServesResponses:   true,
		ParallelToolCalls: true,
		StructuredOutputs: true,
		Models: []transcode.CatalogModel{
			{Surrogate: "alpha", Context: &context},
			{Surrogate: "beta"},
		},
	}
}

// TestCatalogServedBeforeAdmission proves the listing answers while every
// limited slot is held, and counts as passthrough.
func TestCatalogServedBeforeAdmission(t *testing.T) {
	release1 := make(chan struct{})
	release2 := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			if r.Header.Get("X-Probe") == "1" {
				<-release1
			} else {
				<-release2
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(release1); close(release2) })
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	limited, _ := route.Parse("POST /messages:2")
	met := metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{limited})),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(met),
		WithLimitAll(true),
		WithQueueTimeout(2*time.Second),
		WithModelCatalog(testCatalogConfig(128000)),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	proxiedBefore := met.Snapshot().TotalProxied

	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			req.Header.Set("X-Probe", map[bool]string{true: "1", false: "0"}[i == 0])
			p.ServeHTTP(httptest.NewRecorder(), req)
		})
	}
	waitSnapshot(t, met, func(s metrics.Snapshot) bool { return s.Active == 2 })

	start := time.Now()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d: %s", rec.Code, rec.Body.String())
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("catalog took %v — it queued behind the saturated pool", elapsed)
	}
	if !strings.Contains(rec.Body.String(), `"models"`) {
		t.Fatalf("codex-shaped document expected: %s", rec.Body.String())
	}

	waitSnapshot(t, met, func(s metrics.Snapshot) bool { return s.TotalPassThrough >= 1 })
	if got := met.Snapshot().TotalProxied; got != proxiedBefore {
		t.Fatalf("TotalProxied = %d, want unchanged %d (the catalog is never shaped)", got, proxiedBefore)
	}
}

// TestCatalogBypassesGlobalLimiter proves the catalog answers behind a held
// global slot.
func TestCatalogBypassesGlobalLimiter(t *testing.T) {
	release := make(chan struct{})
	var closeOnce sync.Once
	safeClose := func() { closeOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(safeClose)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	limited, _ := route.Parse("POST /messages:1")
	met := metrics.NewCollector()
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher([]route.Pattern{limited})),
		WithLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithMetrics(met),
		WithGlobalLimiter(queue.NewLimiterWithCooldown(1, 0)),
		WithLimitAll(true),
		WithQueueTimeout(2*time.Second),
		WithModelCatalog(testCatalogConfig(128000)),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	holderDone := make(chan int, 1)
	go func() {
		holderDone <- func() int {
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
			return rec.Code
		}()
	}()
	waitSnapshot(t, met, func(s metrics.Snapshot) bool { return s.Active == 1 })

	start := time.Now()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("catalog took %v behind the held global slot", elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d", rec.Code)
	}
	safeClose()
	if code := <-holderDone; code != http.StatusOK {
		t.Fatalf("holder status = %d", code)
	}
}

// TestCatalogZeroModelsPassthrough proves a mount without a catalog keeps
// GET /v1/models a verbatim passthrough.
func TestCatalogZeroModelsPassthrough(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"upstream":true}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
	)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"upstream":true`) {
		t.Fatalf("zero-model mount must pass through: %d %s", rec.Code, rec.Body.String())
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits)
	}
}

// TestCatalogMethodGate proves only GET reaches the catalog renderer.
func TestCatalogMethodGate(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"upstream":true}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithModelCatalog(testCatalogConfig(128000)),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPost, http.MethodHead, http.MethodOptions, http.MethodDelete} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(method, "/v1/models", strings.NewReader(`{"any":"body"}`)))
		if strings.Contains(rec.Body.String(), `"slug"`) {
			t.Errorf("%s /v1/models was served by the catalog: %s", method, rec.Body.String())
		}
	}
	if hits != 4 {
		t.Fatalf("upstream hits = %d, want 4 (every non-GET falls through)", hits)
	}
}

// TestCatalogTrailingSlashParity pins trailing-slash canonicalization.
func TestCatalogTrailingSlashParity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" || r.URL.Path == "/v1/models/" {
			t.Errorf("upstream must not be contacted for the catalog list: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithModelCatalog(testCatalogConfig(128000)),
	)
	if err != nil {
		t.Fatal(err)
	}
	plain := httptest.NewRecorder()
	p.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	slashed := httptest.NewRecorder()
	p.ServeHTTP(slashed, httptest.NewRequest(http.MethodGet, "/v1/models/", nil))
	if plain.Code != http.StatusOK || slashed.Code != http.StatusOK {
		t.Fatalf("statuses = %d/%d", plain.Code, slashed.Code)
	}
	if plain.Body.String() != slashed.Body.String() {
		t.Fatalf("trailing slash changed the document:\n%s\n%s", plain.Body.String(), slashed.Body.String())
	}

	// A sub-resource is not the catalog: it passes through.
	sub := httptest.NewRecorder()
	p.ServeHTTP(sub, httptest.NewRequest(http.MethodGet, "/v1/models/alpha", nil))
	if sub.Code != http.StatusOK {
		t.Fatalf("sub-resource status = %d, want upstream passthrough", sub.Code)
	}
}

// TestCatalogUnknownQueryRejected proves unknown query keys are local 400s and
// never forwarded.
func TestCatalogUnknownQueryRejected(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithModelCatalog(testCatalogConfig(128000)),
	)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models?foo=1", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown query parameter") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if hits != 0 {
		t.Fatalf("upstream hits = %d, want 0 (never forwarded)", hits)
	}
	allowed := httptest.NewRecorder()
	p.ServeHTTP(allowed, httptest.NewRequest(http.MethodGet, "/v1/models?beta=true", nil))
	if allowed.Code != http.StatusOK {
		t.Fatalf("beta status = %d, want 200", allowed.Code)
	}
}

// TestCatalogResponseBounded proves the generated-response bound fails the
// exchange before any partial document escapes, and that a below-minimum bound
// is a construction error.
func TestCatalogResponseBounded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	context := 100000
	config := transcode.CatalogConfig{ProviderName: "TestProv"}
	for i := 0; i < 20; i++ {
		config.Models = append(config.Models, transcode.CatalogModel{
			Surrogate: "model-" + string(rune('a'+i)),
			Context:   &context,
		})
	}
	config.Limits.GeneratedResponseBytes = transcode.MinGeneratedResponseBytes

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithModelCatalog(config),
	)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for an over-bound document: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"models"`) {
		t.Fatalf("an over-bound document leaked: %s", rec.Body.String())
	}

	// Below the minimum legal bound is a startup error.
	config.Limits.GeneratedResponseBytes = transcode.MinGeneratedResponseBytes - 1
	if _, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithModelCatalog(config),
	); err == nil {
		t.Fatal("below-minimum GeneratedResponseBytes accepted")
	} else if !strings.Contains(err.Error(), "GeneratedResponseBytes") {
		t.Fatalf("error = %v, want it to name the bound", err)
	}
}

// TestCatalogDocumentShapeFromOption is a smoke check that a mount with a
// catalog serves JSON with an explicit content type.
func TestCatalogDocumentShapeFromOption(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithModelCatalog(testCatalogConfig(128000)),
	)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	var document struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(document.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(document.Models))
	}
}

// TestCatalogServedWithOpenBreaker proves the catalog is a local answer: a
// tripped breaker must not delay or reject it.
func TestCatalogServedWithOpenBreaker(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	breaker, err := circuitbreaker.New(
		circuitbreaker.WithFailureThreshold(1),
		circuitbreaker.WithWindow(time.Minute),
		circuitbreaker.WithOpenTimeout(time.Minute),
		circuitbreaker.WithMaxOpenTimeout(time.Minute),
		circuitbreaker.WithBasePenalty(time.Second),
		circuitbreaker.WithMaxPenalty(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := breaker.Allow()
	if err != nil {
		t.Fatal(err)
	}
	breaker.RecordFailure(http.StatusInternalServerError, 0, time.Now(), epoch)
	if _, err := breaker.Allow(); err == nil {
		t.Fatal("breaker did not open; the test cannot prove the bypass")
	}

	p, err := New(
		WithUpstream(upstreamURL),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(breaker),
		WithModelCatalog(testCatalogConfig(128000)),
	)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d behind an open breaker: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"models"`) {
		t.Fatalf("catalog document missing: %s", rec.Body.String())
	}
}
