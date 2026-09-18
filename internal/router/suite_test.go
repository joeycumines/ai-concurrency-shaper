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
			{Model: "model-a", Provider: "provA", Handler: handlerA},
			{Model: "model-b", Provider: "provB", Handler: handlerB},
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

	// 4. Completion route POST /suite/v1/chat/completions with model-a -> routes to handlerA
	{
		req := httptest.NewRequest(http.MethodPost, "/suite/v1/chat/completions", bytes.NewBufferString(`{"model":"model-a"}`))
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
	req2 := httptest.NewRequest(http.MethodPost, "/zero/v1/chat/completions", bytes.NewBufferString(`{"model":"any"}`))
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
			{Model: "known-model", Handler: dummyTarget},
		},
	})
	reqCompl404 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"unknown-model"}`))
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
		Name:         "suite-sub",
		Prefix:       "/sub",
		DefaultShape: transcode.CatalogShapeOpenAI,
		Fallback:     fallback,
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
			{Model: "m1", Handler: target},
		},
	})

	bodyStr := `{"model":"m1","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(bodyStr))
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
			{Model: "m1", Handler: target},
		},
	})

	// Create a payload > 10MB (e.g. 11MB) with {"model":"m1","pad":"..."}
	padSize := 11 << 20
	var buf bytes.Buffer
	buf.WriteString(`{"model":"m1","pad":"`)
	buf.Grow(padSize + 100)
	buf.Write(bytes.Repeat([]byte("a"), padSize))
	buf.WriteString(`"}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", &buf)
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
			{Model: "m1", Handler: target},
		},
	})

	var buf2 bytes.Buffer
	buf2.WriteString(`{"model":"m1","pad":"`)
	buf2.Write(bytes.Repeat([]byte("b"), 2<<20))
	buf2.WriteString(`"}`)

	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", &buf2)
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
		Name:         "strict-suite",
		Prefix:       "/strict",
		DefaultShape: transcode.CatalogShapeOpenAI,
		Strict:       true,
		Fallback:     fallback,
		ModelRoutes: []router.ModelRoute{
			{Model: "allowed-model", Handler: dummyTarget},
		},
	})

	// 1a. Unmapped model completion request: must return 404 and NOT call fallback
	reqUnmapped := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"unmapped-model"}`))
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
		Name:         "non-strict-suite",
		Prefix:       "/non-strict",
		DefaultShape: transcode.CatalogShapeOpenAI,
		Strict:       false,
		Fallback:     fallback,
		ModelRoutes: []router.ModelRoute{
			{Model: "allowed-model", Handler: dummyTarget},
		},
	})

	// 2a. Unmapped model completion request: must fall back to fallback handler
	reqUnmapped2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"unmapped-model"}`))
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
		t.Fatalf("non-strict non-completion code = %d: %s", recNonCompl2.Code, recNonCompl2.Body.String())
	}
}
