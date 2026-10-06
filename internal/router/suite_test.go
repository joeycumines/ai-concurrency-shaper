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

package router_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func suiteRouteSet(paths ...string) map[transcode.RouteKey]struct{} {
	routes := make(map[transcode.RouteKey]struct{}, len(paths))
	for _, p := range paths {
		routes[transcode.RouteKey{Method: http.MethodPost, Path: p}] = struct{}{}
	}
	return routes
}

func allSuiteRoutes() map[transcode.RouteKey]struct{} {
	return suiteRouteSet("/v1/responses", "/v1/messages")
}

func TestCatalogSuite_DiscoveryAndRouting(t *testing.T) {
	var countA, countB atomic.Int64
	var lastModelA, lastModelB atomic.Value

	handlerA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		countA.Add(1)
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe)
		lastModelA.Store(probe.Model)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"provider":"A","model":` + strconvQuote(probe.Model) + `}`))
	})

	handlerB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		countB.Add(1)
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe)
		lastModelB.Store(probe.Model)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"provider":"B","model":` + strconvQuote(probe.Model) + `}`))
	})

	catHandler, err := transcode.NewCatalogHandler(transcode.CatalogConfig{
		ProviderName: "suite-1",
		Models: []transcode.CatalogModel{
			{Surrogate: "model-a", Provider: "provA"},
			{Surrogate: "model-b", Provider: "provB"},
		},
		DefaultShape: transcode.CatalogShapeAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:           "suite-1",
		Prefix:         "/suite",
		CatalogHandler: catHandler,
		DefaultShape:   transcode.CatalogShapeAnthropic,
		ModelRoutes: []router.ModelRoute{
			{Model: "model-a", Provider: "provA", Handler: handlerA, SupportedRoutes: allSuiteRoutes()},
			{Model: "model-b", Provider: "provB", Handler: handlerB, SupportedRoutes: allSuiteRoutes()},
		},
	})

	rtr, err := router.New([]router.Provider{
		{Name: "suite-1", Prefix: "/suite", Proxy: suite},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Catalog discovery: GET /suite/v1/models returns Anthropic shape
	{
		req := httptest.NewRequest(http.MethodGet, "/suite/v1/models", nil)
		rec := httptest.NewRecorder()
		rtr.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("catalog status %d: %s", rec.Code, rec.Body.String())
		}
		var doc struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if len(doc.Data) != 2 {
			t.Fatalf("expected 2 models, got %d", len(doc.Data))
		}
		if doc.Data[0]["id"] != "model-a" || doc.Data[1]["id"] != "model-b" {
			t.Errorf("unexpected models: %+v", doc.Data)
		}
	}

	// 2. Single-model query: GET /suite/v1/models/model-a
	{
		req := httptest.NewRequest(http.MethodGet, "/suite/v1/models/model-a", nil)
		rec := httptest.NewRecorder()
		rtr.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("single model status %d: %s", rec.Code, rec.Body.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc["id"] != "model-a" {
			t.Errorf("expected id model-a, got %v", doc["id"])
		}
	}

	// 3. Single-model query not found: GET /suite/v1/models/nonexistent
	{
		req := httptest.NewRequest(http.MethodGet, "/suite/v1/models/nonexistent", nil)
		rec := httptest.NewRecorder()
		rtr.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	}

	// 4. Completion route POST /suite/v1/responses with model-a -> routes to handlerA
	{
		req := httptest.NewRequest(http.MethodPost, "/suite/v1/responses", bytes.NewBufferString(`{"model":"model-a"}`))
		rec := httptest.NewRecorder()
		rtr.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("completion status %d: %s", rec.Code, rec.Body.String())
		}
		if countA.Load() != 1 {
			t.Errorf("countA = %d, want 1", countA.Load())
		}
		if lastModelA.Load() != "model-a" {
			t.Errorf("lastModelA = %v, want model-a", lastModelA.Load())
		}
	}

	// 5. Completion route POST /suite/v1/messages with model-b -> routes to handlerB
	{
		req := httptest.NewRequest(http.MethodPost, "/suite/v1/messages", bytes.NewBufferString(`{"model":"model-b"}`))
		rec := httptest.NewRecorder()
		rtr.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("completion status %d: %s", rec.Code, rec.Body.String())
		}
		if countB.Load() != 1 {
			t.Errorf("countB = %d, want 1", countB.Load())
		}
		if lastModelB.Load() != "model-b" {
			t.Errorf("lastModelB = %v, want model-b", lastModelB.Load())
		}
	}

	// 5b. An unmatched path answers in the suite's own dialect, never Go's
	// bare text/plain 404 page. Every other error this handler emits is
	// dialect-shaped, and a suite that advertises an Anthropic catalog must
	// not answer a stray probe in net/http's error page.
	{
		for _, tc := range []struct {
			name   string
			method string
			path   string
			want   string
		}{
			{"post unknown path", http.MethodPost, "/suite/v1/nonsense", `"type":"error"`},
			{"get unknown path", http.MethodGet, "/suite/nope", `"error"`},
		} {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(`{}`))
			rec := httptest.NewRecorder()
			rtr.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s: expected 404, got %d: %s", tc.name, rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("%s: Content-Type = %q, want JSON", tc.name, ct)
			}
			if strings.Contains(rec.Body.String(), "page not found") {
				t.Fatalf("%s: body = %q, want a dialect error, not Go's 404 page", tc.name, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("%s: body = %q, want the dialect envelope %s", tc.name, rec.Body.String(), tc.want)
			}
		}
	}

	// 6. Unknown model -> 404 in client dialect
	{
		req := httptest.NewRequest(http.MethodPost, "/suite/v1/messages", bytes.NewBufferString(`{"model":"model-unknown"}`))
		rec := httptest.NewRecorder()
		rtr.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
		}
		var errDoc struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &errDoc); err != nil {
			t.Fatalf("unmarshal error doc: %v", err)
		}
		if errDoc.Type != "error" || errDoc.Error.Type != "not_found_error" {
			t.Errorf("unexpected error doc: %+v", errDoc)
		}
	}
}

func TestCatalogSuite_ZeroProviders(t *testing.T) {
	catHandler, err := transcode.NewCatalogHandler(transcode.CatalogConfig{
		ProviderName: "suite-zero",
		Models:       nil,
		DefaultShape: transcode.CatalogShapeOpenAI,
	})
	if err != nil {
		t.Fatal(err)
	}

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:           "suite-zero",
		Prefix:         "/zero",
		CatalogHandler: catHandler,
		DefaultShape:   transcode.CatalogShapeOpenAI,
		ModelRoutes:    nil,
	})

	rtr, err := router.New([]router.Provider{
		{Name: "suite-zero", Prefix: "/zero", Proxy: suite},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Catalog discovery returns empty list
	req := httptest.NewRequest(http.MethodGet, "/zero/v1/models", nil)
	rec := httptest.NewRecorder()
	rtr.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status %d", rec.Code)
	}

	// Completion returns 503 (service unavailable / no provider configured)
	req2 := httptest.NewRequest(http.MethodPost, "/zero/v1/responses", bytes.NewBufferString(`{"model":"any"}`))
	rec2 := httptest.NewRecorder()
	rtr.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestCatalogSuite_TrailingSlashAndErrorFormat(t *testing.T) {
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:         "suite-empty",
		Prefix:       "/empty",
		DefaultShape: transcode.CatalogShapeOpenAI,
	})

	// /v1/models/ with trailing slash on empty suite must return 200 OK list, not 404
	req := httptest.NewRequest(http.MethodGet, "/v1/models/", nil)
	rec := httptest.NewRecorder()
	suite.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /v1/models/, got %d: %s", rec.Code, rec.Body.String())
	}
	var listDoc struct {
		Object string `json:"object"`
		Data   []any  `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listDoc); err != nil {
		t.Fatal(err)
	}
	if listDoc.Object != "list" {
		t.Errorf("expected object=list, got %q", listDoc.Object)
	}

	// /v1/models/nonexistent on empty suite must return 404 with invalid_request_error and model_not_found
	req404 := httptest.NewRequest(http.MethodGet, "/v1/models/nonexistent", nil)
	rec404 := httptest.NewRecorder()
	suite.ServeHTTP(rec404, req404)
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec404.Code, rec404.Body.String())
	}
	var errDoc struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec404.Body.Bytes(), &errDoc); err != nil {
		t.Fatal(err)
	}
	if errDoc.Error.Type != "invalid_request_error" {
		t.Errorf("expected error type invalid_request_error, got %q", errDoc.Error.Type)
	}
	if errDoc.Error.Code != "model_not_found" {
		t.Errorf("expected error code model_not_found, got %q", errDoc.Error.Code)
	}

	// Unmapped model on a suite with models should return 404 with invalid_request_error
	dummyTarget := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	suiteWithModel := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:         "suite-one",
		Prefix:       "/one",
		DefaultShape: transcode.CatalogShapeOpenAI,
		ModelRoutes: []router.ModelRoute{
			{Model: "known-model", Handler: dummyTarget, SupportedRoutes: allSuiteRoutes()},
		},
	})
	reqCompl404 := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"unknown-model"}`))
	recCompl404 := httptest.NewRecorder()
	suiteWithModel.ServeHTTP(recCompl404, reqCompl404)
	if recCompl404.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", recCompl404.Code, recCompl404.Body.String())
	}
	var complErrDoc struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recCompl404.Body.Bytes(), &complErrDoc); err != nil {
		t.Fatal(err)
	}
	if complErrDoc.Error.Type != "invalid_request_error" {
		t.Errorf("expected completion error type invalid_request_error, got %q", complErrDoc.Error.Type)
	}
}

func TestCatalogSuite_SubresourceNotCatalog(t *testing.T) {
	// When a path has descendant segments beyond single model (e.g. /v1/models/m1/extra),
	// it should NOT be handled as a catalog endpoint.
	fallbackHit := false
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fallback ok"))
	})
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:           "suite-sub",
		Prefix:         "/sub",
		DefaultShape:   transcode.CatalogShapeOpenAI,
		Fallback:       fallback,
		FallbackRoutes: allSuiteRoutes(),
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models/m1/extra", nil)
	rec := httptest.NewRecorder()
	suite.ServeHTTP(rec, req)
	if !fallbackHit {
		t.Fatalf("expected fallback handler to be called for /v1/models/m1/extra, got code %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCatalogSuite_ServeCompletion_GetBodyAndTransferEncoding(t *testing.T) {
	var targetReq *http.Request
	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetReq = r
		w.WriteHeader(http.StatusOK)
	})

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:         "test-suite",
		Prefix:       "/suite",
		DefaultShape: transcode.CatalogShapeOpenAI,
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: target, SupportedRoutes: allSuiteRoutes()},
		},
	})

	bodyStr := `{"model":"m1","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(bodyStr))
	req.TransferEncoding = []string{"chunked"}
	rec := httptest.NewRecorder()

	suite.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if targetReq == nil {
		t.Fatal("target handler was not called")
	}

	// Verify r.GetBody is set and can be called multiple times to read the exact body
	if targetReq.GetBody == nil {
		t.Fatal("targetReq.GetBody is nil, want non-nil func for ReverseProxy compatibility")
	}
	for i := range 3 {
		rc, err := targetReq.GetBody()
		if err != nil {
			t.Fatalf("GetBody() call %d error: %v", i, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read GetBody() call %d: %v", i, err)
		}
		if string(data) != bodyStr {
			t.Fatalf("GetBody() call %d = %q, want %q", i, string(data), bodyStr)
		}
	}

	// Verify TransferEncoding is cleared and ContentLength is set
	if len(targetReq.TransferEncoding) != 0 {
		t.Errorf("targetReq.TransferEncoding = %v, want empty/nil", targetReq.TransferEncoding)
	}
	if targetReq.ContentLength != int64(len(bodyStr)) {
		t.Errorf("targetReq.ContentLength = %d, want %d", targetReq.ContentLength, len(bodyStr))
	}
}

func TestCatalogSuite_ServeCompletion_AcceptedRequestBytesLimit(t *testing.T) {
	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// 1. Default limits (AcceptedRequestBytes = 32MB): an 11MB payload must succeed (> 10MB old limit)
	suiteDefault := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:         "test-default",
		Prefix:       "/suite",
		DefaultShape: transcode.CatalogShapeOpenAI,
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: target, SupportedRoutes: allSuiteRoutes()},
		},
	})

	// Create a payload > 10MB (e.g. 11MB) with {"model":"m1","pad":"..."}
	padSize := 11 << 20
	var buf bytes.Buffer
	buf.WriteString(`{"model":"m1","pad":"`)
	buf.Grow(padSize + 100)
	buf.Write(bytes.Repeat([]byte("a"), padSize))
	buf.WriteString(`"}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", &buf)
	rec := httptest.NewRecorder()
	suiteDefault.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for 11MB body with default 32MB limit, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Custom limit (e.g. AcceptedRequestBytes = 1MB): a 2MB payload must be rejected with 413
	suiteCustom := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:         "test-custom",
		Prefix:       "/suite",
		DefaultShape: transcode.CatalogShapeOpenAI,
		Limits: transcode.BodyLimits{
			AcceptedRequestBytes: 1 << 20,
		},
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: target, SupportedRoutes: allSuiteRoutes()},
		},
	})

	var buf2 bytes.Buffer
	buf2.WriteString(`{"model":"m1","pad":"`)
	buf2.Write(bytes.Repeat([]byte("b"), 2<<20))
	buf2.WriteString(`"}`)

	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", &buf2)
	rec2 := httptest.NewRecorder()
	suiteCustom.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 Request Entity Too Large, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestCatalogSuite_StrictEnforcementAndFallbackIsolation(t *testing.T) {
	var fallbackHits atomic.Int64
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fallback response"))
	})

	dummyTarget := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("target response"))
	})

	// 1. Strict suite: Strict: true
	strictSuite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:           "strict-suite",
		Prefix:         "/strict",
		DefaultShape:   transcode.CatalogShapeOpenAI,
		Strict:         true,
		Fallback:       fallback,
		FallbackRoutes: allSuiteRoutes(),
		ModelRoutes: []router.ModelRoute{
			{Model: "allowed-model", Handler: dummyTarget, SupportedRoutes: allSuiteRoutes()},
		},
	})

	// 1a. Unmapped model completion request: must return 404 and NOT call fallback
	reqUnmapped := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"unmapped-model"}`))
	recUnmapped := httptest.NewRecorder()
	strictSuite.ServeHTTP(recUnmapped, reqUnmapped)
	if recUnmapped.Code != http.StatusNotFound {
		t.Fatalf("strict unmapped completion code = %d, want 404", recUnmapped.Code)
	}
	if fallbackHits.Load() != 0 {
		t.Fatalf("strict unmapped completion called fallback %d times, want 0", fallbackHits.Load())
	}

	// 1b. Non-completion route: must return 404 and NOT call fallback
	reqNonCompl := httptest.NewRequest(http.MethodGet, "/v1/models/allowed-model/extra", nil)
	recNonCompl := httptest.NewRecorder()
	strictSuite.ServeHTTP(recNonCompl, reqNonCompl)
	if recNonCompl.Code != http.StatusNotFound {
		t.Fatalf("strict non-completion code = %d, want 404", recNonCompl.Code)
	}
	if fallbackHits.Load() != 0 {
		t.Fatalf("strict non-completion called fallback %d times, want 0", fallbackHits.Load())
	}

	// 2. Non-strict suite: Strict: false
	nonStrictSuite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:           "non-strict-suite",
		Prefix:         "/non-strict",
		DefaultShape:   transcode.CatalogShapeOpenAI,
		Strict:         false,
		Fallback:       fallback,
		FallbackRoutes: allSuiteRoutes(),
		ModelRoutes: []router.ModelRoute{
			{Model: "allowed-model", Handler: dummyTarget, SupportedRoutes: allSuiteRoutes()},
		},
	})

	// 2a. Unmapped model completion request: must fall back to fallback handler
	reqUnmapped2 := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"unmapped-model"}`))
	recUnmapped2 := httptest.NewRecorder()
	nonStrictSuite.ServeHTTP(recUnmapped2, reqUnmapped2)
	if recUnmapped2.Code != http.StatusOK || !strings.Contains(recUnmapped2.Body.String(), "fallback response") {
		t.Fatalf("non-strict unmapped completion code = %d: %s", recUnmapped2.Code, recUnmapped2.Body.String())
	}

	// 2b. Non-completion route: must fall back to fallback handler
	reqNonCompl2 := httptest.NewRequest(http.MethodGet, "/v1/models/allowed-model/extra", nil)
	recNonCompl2 := httptest.NewRecorder()
	nonStrictSuite.ServeHTTP(recNonCompl2, reqNonCompl2)
	if recNonCompl2.Code != http.StatusOK || !strings.Contains(recNonCompl2.Body.String(), "fallback response") {
		t.Errorf("non-strict non-completion code = %d: %s", recNonCompl2.Code, recNonCompl2.Body.String())
	}
}

func TestCatalogSuiteRejectsDuplicateModelKeys(t *testing.T) {
	var targetHits atomic.Int64
	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeCodex,
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: target, SupportedRoutes: allSuiteRoutes()},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"unknown","model":"m1"}`))
	rec := httptest.NewRecorder()
	suite.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate model status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if targetHits.Load() != 0 {
		t.Fatalf("duplicate-key request reached target %d times", targetHits.Load())
	}
}

func TestCatalogSuiteUnknownModelErrorIsBounded(t *testing.T) {
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeCodex,
		ModelRoutes: []router.ModelRoute{
			{Model: "known", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), SupportedRoutes: allSuiteRoutes()},
		},
	})
	model := strings.Repeat("x", 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+model+`"}`))
	rec := httptest.NewRecorder()
	suite.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if rec.Body.Len() > transcode.DefaultErrorMessageBytes+512 {
		t.Fatalf("error body = %d bytes, exceeds bounded family", rec.Body.Len())
	}
}

func TestCatalogSuiteRejectsUnsupportedProviderRoute(t *testing.T) {
	var targetHits atomic.Int64
	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeAnthropic,
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: target, SupportedRoutes: suiteRouteSet("/v1/responses")},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m1"}`))
	rec := httptest.NewRecorder()
	suite.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unsupported route status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if targetHits.Load() != 0 {
		t.Fatal("unsupported route reached provider target")
	}
}

func TestCatalogSuite_ChatCompletionRouted(t *testing.T) {
	var hits atomic.Int64
	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	catHandler, err := transcode.NewCatalogHandler(transcode.CatalogConfig{
		ProviderName: "suite-chat",
		Models:       []transcode.CatalogModel{{Surrogate: "chat-model", Provider: "provC"}},
		DefaultShape: transcode.CatalogShapeOpenAI,
	})
	if err != nil {
		t.Fatal(err)
	}
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:           "suite-chat",
		Prefix:         "/suite",
		CatalogHandler: catHandler,
		ModelRoutes: []router.ModelRoute{{
			Model:    "chat-model",
			Provider: "provC",
			Handler:  target,
			SupportedRoutes: map[transcode.RouteKey]struct{}{
				{Method: http.MethodPost, Path: "/v1/chat/completions"}: {},
			},
		}},
		Strict: true,
	})
	rtr, err := router.New([]router.Provider{
		{Name: "suite-chat", Prefix: "/suite", Proxy: suite},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Routed: POST /suite/v1/chat/completions with a covered model.
	req := httptest.NewRequest(http.MethodPost, "/suite/v1/chat/completions",
		bytes.NewBufferString(`{"model":"chat-model","messages":[]}`))
	rec := httptest.NewRecorder()
	rtr.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat completion status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("target hits = %d, want 1", hits.Load())
	}

	// Unknown model on the chat route: 404 in the OpenAI dialect.
	req = httptest.NewRequest(http.MethodPost, "/suite/v1/chat/completions",
		bytes.NewBufferString(`{"model":"nope","messages":[]}`))
	rec = httptest.NewRecorder()
	rtr.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("error body: %v", err)
	}
	if _, ok := doc["error"]; !ok {
		t.Fatalf("body = %s, want OpenAI error envelope", rec.Body.String())
	}
}

// TestCatalogSuiteChatErrorShapeIsOpenAI proves a chat error is rendered
// in the OpenAI envelope even when the suite catalog defaults to the
// Anthropic shape: the route's dialect wins over the suite default.
func TestCatalogSuiteChatErrorShapeIsOpenAI(t *testing.T) {
	catHandler, err := transcode.NewCatalogHandler(transcode.CatalogConfig{
		ProviderName: "suite-chat-shape",
		Models:       []transcode.CatalogModel{{Surrogate: "chat-model", Provider: "provC"}},
		DefaultShape: transcode.CatalogShapeAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:           "suite-chat-shape",
		Prefix:         "/suite",
		CatalogHandler: catHandler,
		DefaultShape:   transcode.CatalogShapeAnthropic,
		ModelRoutes: []router.ModelRoute{{
			Model:           "chat-model",
			Provider:        "provC",
			Handler:         http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
			SupportedRoutes: suiteRouteSet("/v1/chat/completions"),
		}},
		Strict: true,
	})
	rtr, err := router.New([]router.Provider{{Name: "suite-chat-shape", Prefix: "/suite", Proxy: suite}})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/suite/v1/chat/completions",
		bytes.NewBufferString(`{"model":"unknown","messages":[]}`))
	rec := httptest.NewRecorder()
	rtr.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("error body not JSON: %v: %s", err, rec.Body.String())
	}
	if _, ok := doc["error"]; !ok {
		t.Fatalf("body = %s, want the OpenAI error envelope", rec.Body.String())
	}
	if _, anthropic := doc["type"]; anthropic {
		t.Fatalf("body = %s, want no Anthropic top-level type on the chat route", rec.Body.String())
	}
}

func TestCatalogSuiteCompletionSurfaceIsVersionedResponsesAndMessages(t *testing.T) {
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{Strict: true})
	for _, target := range []string{"/chat/completions", "/responses", "/messages"} {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"model":"m"}`))
		rec := httptest.NewRecorder()
		suite.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", target, rec.Code)
		}
	}
	// POST /v1/chat/completions is a completion route (natively served
	// chat models): on an empty strict suite it reports no provider,
	// exactly like its /v1/responses and /v1/messages siblings.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	rec := httptest.NewRecorder()
	suite.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/v1/chat/completions status = %d, want 503", rec.Code)
	}
}

// TestCatalogSuiteCountTokensIsDialectShaped proves the Claude Code token
// probe is answered in the Anthropic dialect (never a bare 404 page), both
// when the route is declared and when it is not.
func TestCatalogSuiteCountTokensIsDialectShaped(t *testing.T) {
	var hits atomic.Int64
	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"input_tokens":7}`))
	})
	catHandler, err := transcode.NewCatalogHandler(transcode.CatalogConfig{
		ProviderName: "suite-ct",
		Models:       []transcode.CatalogModel{{Surrogate: "msg-model", Provider: "provA"}},
		DefaultShape: transcode.CatalogShapeAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:           "suite-ct",
		Prefix:         "/suite",
		CatalogHandler: catHandler,
		DefaultShape:   transcode.CatalogShapeAnthropic,
		ModelRoutes: []router.ModelRoute{{
			Model:           "msg-model",
			Provider:        "provA",
			Handler:         target,
			SupportedRoutes: suiteRouteSet("/v1/messages", "/v1/messages/count_tokens"),
		}},
		Strict: true,
	})
	rtr, err := router.New([]router.Provider{{Name: "suite-ct", Prefix: "/suite", Proxy: suite}})
	if err != nil {
		t.Fatal(err)
	}

	// Declared route: served.
	req := httptest.NewRequest(http.MethodPost, "/suite/v1/messages/count_tokens",
		bytes.NewBufferString(`{"model":"msg-model","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	rtr.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("target hits = %d, want 1", hits.Load())
	}

	// Undeclared route: dialect-shaped error, not a bare page.
	req = httptest.NewRequest(http.MethodPost, "/suite/v1/messages/count_tokens",
		bytes.NewBufferString(`{"model":"unknown","messages":[]}`))
	rec = httptest.NewRecorder()
	rtr.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("error content type = %q, want JSON", ct)
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("error body not JSON: %v: %s", err, rec.Body.String())
	}
	if envelope.Type != "error" || envelope.Error.Message == "" {
		t.Fatalf("error envelope = %+v, want the Anthropic shape", envelope)
	}
}

func TestCatalogSuiteKnownOversizeContentLengthFailsBeforeProvider(t *testing.T) {
	var targetHits atomic.Int64
	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeCodex,
		Limits:       transcode.BodyLimits{AcceptedRequestBytes: 1},
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: target, SupportedRoutes: allSuiteRoutes()},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m1"}`))
	req.ContentLength = 2
	rec := httptest.NewRecorder()
	suite.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("known oversize status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if targetHits.Load() != 0 {
		t.Fatal("known oversize request reached provider target")
	}
}

// TestCatalogSuiteGenericErrorVerdicts pins the suite's own generic 4xx
// type/code table: completion-path 404s are generic envelopes (unknown model,
// unmatched route), whose wire verdicts predate the client-error unification
// and must not drift with the shared mapping. The single-model 404 pins live
// in TestCatalogSuite_TrailingSlashAndErrorFormat; the 413-type pin lives in
// TestCatalogSuiteCompletion413Type below.
func TestCatalogSuiteGenericErrorVerdicts(t *testing.T) {
	dummyTarget := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	newSuite := func() *router.CatalogSuiteHandler {
		return router.NewCatalogSuiteHandler(router.SuiteConfig{
			Name:         "suite-verdicts",
			Prefix:       "/verdicts",
			DefaultShape: transcode.CatalogShapeOpenAI,
			Strict:       true,
			ModelRoutes: []router.ModelRoute{
				{Model: "known-model", Handler: dummyTarget, SupportedRoutes: allSuiteRoutes()},
			},
		})
	}
	serve := func(suite *router.CatalogSuiteHandler, body string) (int, string, string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses",
			bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		suite.ServeHTTP(rec, req)
		var doc struct {
			Error struct {
				Type string `json:"type"`
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("unmarshal error envelope: %v: %s", err, rec.Body.String())
		}
		return rec.Code, doc.Error.Type, doc.Error.Code
	}
	// Unknown model on a completion route: generic 404 envelope.
	if code, errType, errCode := serve(newSuite(), `{"model":"unknown-model"}`); code != http.StatusNotFound ||
		errType != "invalid_request_error" || errCode != "model_not_found" {
		t.Errorf("unknown-model completion = %d %q/%q, want 404 invalid_request_error/model_not_found",
			code, errType, errCode)
	}
	// Known model on an unsupported route: generic 404 envelope.
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:         "suite-verdicts-route",
		Prefix:       "/verdicts-route",
		DefaultShape: transcode.CatalogShapeOpenAI,
		Strict:       true,
		ModelRoutes: []router.ModelRoute{
			{Model: "known-model", Handler: dummyTarget, SupportedRoutes: suiteRouteSet("/v1/messages")},
		},
	})
	if code, errType, errCode := serve(suite, `{"model":"known-model"}`); code != http.StatusNotFound ||
		errType != "invalid_request_error" || errCode != "model_not_found" {
		t.Errorf("unsupported-route completion = %d %q/%q, want 404 invalid_request_error/model_not_found",
			code, errType, errCode)
	}
	// Malformed body: generic 400 envelope.
	if code, errType, errCode := serve(newSuite(), `{"model":`); code != http.StatusBadRequest ||
		errType != "invalid_request_error" || errCode != "bad_request" {
		t.Errorf("malformed completion = %d %q/%q, want 400 invalid_request_error/bad_request",
			code, errType, errCode)
	}
	// Explicit null model: illegal null, generic 400 envelope.
	if code, errType, errCode := serve(newSuite(), `{"model":null}`); code != http.StatusBadRequest ||
		errType != "invalid_request_error" || errCode != "bad_request" {
		t.Errorf("null-model completion = %d %q/%q, want 400 invalid_request_error/bad_request",
			code, errType, errCode)
	}
	// Empty catalog: 503 service_unavailable envelope.
	empty := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:         "suite-verdicts-empty",
		Prefix:       "/verdicts-empty",
		DefaultShape: transcode.CatalogShapeOpenAI,
		Strict:       true,
	})
	if code, errType, errCode := serve(empty, `{"model":"any"}`); code != http.StatusServiceUnavailable ||
		errType != "api_error" || errCode != "service_unavailable" {
		t.Errorf("empty-suite completion = %d %q/%q, want 503 api_error/service_unavailable",
			code, errType, errCode)
	}
}

// TestCatalogSuiteCompletion413Type pins the suite's generic 413 verdict:
// an oversize completion body renders request_too_large as both type and
// code, not the shared mapping's invalid_request_error type.
func TestCatalogSuiteCompletion413Type(t *testing.T) {
	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeOpenAI,
		Limits:       transcode.BodyLimits{AcceptedRequestBytes: 1},
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: target, SupportedRoutes: allSuiteRoutes()},
		},
	})
	var buf bytes.Buffer
	buf.WriteString(`{"model":"m1","pad":"`)
	buf.Write(bytes.Repeat([]byte("b"), 2<<20))
	buf.WriteString(`"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", &buf)
	rec := httptest.NewRecorder()
	suite.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Error.Type != "request_too_large" || doc.Error.Code != "request_too_large" {
		t.Errorf("oversize envelope = %q/%q, want request_too_large/request_too_large",
			doc.Error.Type, doc.Error.Code)
	}
}
