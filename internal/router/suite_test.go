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
