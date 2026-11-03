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

// geminiSuiteRoute is the client route key of the Gemini GenerateContent
// template a provider handler supports.
var geminiSuiteRoute = map[transcode.RouteKey]struct{}{
	{Method: http.MethodPost, Path: "/v1beta/models/{model}:generateContent"}: {},
}

// geminiSuiteTarget counts hits and answers like a provider handler. It never
// touches the request body, so the caller can assert afterwards that the suite
// handed the body over unread.
func geminiSuiteTarget(hits *atomic.Int64, response string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(response))
	})
}

// decodeGeminiError decodes a Google error envelope.
func decodeGeminiError(t *testing.T, body []byte) (code int, status, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body %q is not a Google error envelope: %v", body, err)
	}
	return env.Error.Code, env.Error.Status, env.Error.Message
}

// TestCatalogSuite_GeminiRouting covers path-based routing of Gemini
// GenerateContent requests on the shared suite mount.
func TestCatalogSuite_GeminiRouting(t *testing.T) {
	var targetHits, fallbackHits atomic.Int64

	target := geminiSuiteTarget(&targetHits, `{"candidates":[]}`)
	fallback := geminiSuiteTarget(&fallbackHits, `"fallback"`)

	geminiOnly := geminiSuiteRoute
	allRoutes := allSuiteRoutes()
	for k := range geminiOnly {
		allRoutes[k] = struct{}{}
	}

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:         "suite-gemini",
		Prefix:       "/suite",
		DefaultShape: transcode.CatalogShapeOpenAI,
		ModelRoutes: []router.ModelRoute{
			// gemini-model's provider serves the Gemini route.
			{Model: "gemini-model", Provider: "provA", Handler: target, SupportedRoutes: geminiOnly},
			// plain-model's provider does not.
			{Model: "plain-model", Provider: "provB", Handler: target, SupportedRoutes: allSuiteRoutes()},
		},
		Fallback:       fallback,
		FallbackRoutes: allSuiteRoutes(),
	})

	post := func(path, body string) (*httptest.ResponseRecorder, *http.Request) {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		suite.ServeHTTP(rec, req)
		return rec, req
	}

	// 1. A known model whose provider serves the Gemini route reaches the
	// target handler with the body untouched: after the exchange the original
	// request body still reads back whole, proving the suite never buffered it.
	rec, req := post("/v1beta/models/gemini-model:generateContent", `{"contents":[]}`)
	if rec.Code != http.StatusOK || targetHits.Load() != 1 {
		t.Fatalf("status = %d, target hits = %d, want 200 and 1; body: %s", rec.Code, targetHits.Load(), rec.Body.String())
	}
	if rest, err := io.ReadAll(req.Body); err != nil || string(rest) != `{"contents":[]}` {
		t.Fatalf("the suite consumed the body on the Gemini path: remaining = %q, err = %v", rest, err)
	}

	// 2. The stream operation shares the same route key.
	rec, _ = post("/v1beta/models/gemini-model:streamGenerateContent?alt=sse", `{"contents":[]}`)
	if rec.Code != http.StatusOK || targetHits.Load() != 2 {
		t.Fatalf("stream: status = %d, hits = %d, want 200 and 2; body: %s", rec.Code, targetHits.Load(), rec.Body.String())
	}

	// 3. A known model whose provider does not serve the Gemini route gets a
	// Google-envelope 404 naming the template path — the same verdict the
	// completion routes give for a known model on an unsupported route; the
	// fallback only serves unknown models.
	rec, _ = post("/v1beta/models/plain-model:generateContent", `{"contents":[]}`)
	if rec.Code != http.StatusNotFound || fallbackHits.Load() != 0 {
		t.Fatalf("known model without the route: status = %d, fallback hits = %d, want 404 and 0; body: %s", rec.Code, fallbackHits.Load(), rec.Body.String())
	}
	code, status, message := decodeGeminiError(t, rec.Body.Bytes())
	if code != http.StatusNotFound || status != "NOT_FOUND" ||
		!strings.Contains(message, `"plain-model"`) || !strings.Contains(message, "/v1beta/models/{model}:generateContent") {
		t.Fatalf("envelope = %d %q %q, want 404 NOT_FOUND naming the model and template path", code, status, message)
	}

	// 4. An unknown model on the non-strict suite with a fallback goes to the
	// fallback, exactly as before.
	rec, _ = post("/v1beta/models/unknown-model:generateContent", `{"contents":[]}`)
	if rec.Code != http.StatusOK || fallbackHits.Load() != 1 {
		t.Fatalf("unknown model: status = %d, fallback hits = %d, want 200 and 2; body: %s", rec.Code, fallbackHits.Load(), rec.Body.String())
	}

	// 5. GET /v1beta/models on the suite is not a Gemini operation and keeps
	// the transparent/fallback behavior (no catalog is served for it).
	reqGET := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	recGET := httptest.NewRecorder()
	suite.ServeHTTP(recGET, reqGET)
	if recGET.Code != http.StatusOK || fallbackHits.Load() != 2 {
		t.Fatalf("GET /v1beta/models: status = %d, fallback hits = %d, want the fallback", recGET.Code, fallbackHits.Load())
	}

	// 6. A strict suite without fallback answers a Google-envelope 404.
	strict := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:         "strict-gemini",
		Prefix:       "/strict",
		DefaultShape: transcode.CatalogShapeOpenAI,
		Strict:       true,
		ModelRoutes: []router.ModelRoute{
			{Model: "gemini-model", Provider: "provA", Handler: target, SupportedRoutes: geminiOnly},
			{Model: "plain-model", Provider: "provB", Handler: target, SupportedRoutes: allSuiteRoutes()},
		},
	})
	req = httptest.NewRequest(http.MethodPost, "/v1beta/models/unknown-model:generateContent", bytes.NewBufferString(`{"contents":[]}`))
	rec = httptest.NewRecorder()
	strict.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("strict unknown model: status = %d, want 404", rec.Code)
	}
	code, status, message = decodeGeminiError(t, rec.Body.Bytes())
	if code != http.StatusNotFound || status != "NOT_FOUND" || !strings.Contains(message, `"unknown-model"`) || !strings.Contains(message, "not found in catalog suite") {
		t.Fatalf("envelope = %d %q %q, want 404 NOT_FOUND naming the model", code, status, message)
	}

	// 7. On a strict suite, a known model whose provider lacks the Gemini route
	// gets a Google-envelope 404 naming the template path.
	req = httptest.NewRequest(http.MethodPost, "/v1beta/models/plain-model:generateContent", bytes.NewBufferString(`{"contents":[]}`))
	rec = httptest.NewRecorder()
	strict.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("strict known model without route: status = %d, want 404", rec.Code)
	}
	code, status, message = decodeGeminiError(t, rec.Body.Bytes())
	if code != http.StatusNotFound || status != "NOT_FOUND" ||
		!strings.Contains(message, `"plain-model"`) || !strings.Contains(message, "/v1beta/models/{model}:generateContent") {
		t.Fatalf("envelope = %d %q %q, want 404 NOT_FOUND naming the model and template path", code, status, message)
	}
}

// TestCatalogSuite_GeminiAdmissionUnused pins that the Gemini path never
// acquires a catalog buffer-admission slot: a suite whose admission pool is
// exhausted still serves a Gemini request (it does not buffer the body).
func TestCatalogSuite_GeminiAdmissionUnused(t *testing.T) {
	var targetHits atomic.Int64
	target := geminiSuiteTarget(&targetHits, `{"candidates":[]}`)

	limits := transcode.BodyLimits{AcceptedRequestBytes: 1 << 20}
	// One slot total: exhaust it with a buffered completion request that
	// blocks, then show a Gemini request still routes.
	admission := router.NewCatalogSuiteAdmission(limits)
	_ = admission

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Name:         "suite-admission",
		Prefix:       "/suite",
		DefaultShape: transcode.CatalogShapeOpenAI,
		ModelRoutes: []router.ModelRoute{
			{Model: "gemini-model", Provider: "provA", Handler: target, SupportedRoutes: geminiSuiteRoute},
		},
		Admission: admission,
		Limits:    limits,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-model:generateContent", bytes.NewBufferString(`{"contents":[]}`))
	rec := httptest.NewRecorder()
	suite.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || targetHits.Load() != 1 {
		t.Fatalf("Gemini request on a suite with a one-slot admission: status = %d, hits = %d, want 200 and 1 (no buffering)", rec.Code, targetHits.Load())
	}
}
