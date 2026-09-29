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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func TestCatalogSuiteBufferAdmissionIsGloballyBounded(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var targetHits atomic.Int64
	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if targetHits.Add(1) == 1 {
			close(entered)
		}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	admission := router.NewCatalogSuiteAdmission(transcode.BodyLimits{AcceptedRequestBytes: 128 << 20})
	firstSuite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeCodex,
		Limits:       transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		Admission:    admission,
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: target, SupportedRoutes: allSuiteRoutes()},
		},
	})
	secondSuite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeCodex,
		Limits:       transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		Admission:    admission,
		ModelRoutes: []router.ModelRoute{
			{Model: "m2", Handler: target, SupportedRoutes: allSuiteRoutes()},
		},
	})

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m1"}`))
		rec := httptest.NewRecorder()
		firstSuite.ServeHTTP(rec, req)
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m2"}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	secondSuite.ServeHTTP(rec, req)
	if targetHits.Load() != 1 {
		t.Fatalf("canceled waiter reached target; hits = %d, want 1", targetHits.Load())
	}
	close(release)
	<-firstDone
}

func TestCatalogSuiteStalledBodyHitsRealReadDeadline(t *testing.T) {
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:            true,
		DefaultShape:      transcode.CatalogShapeCodex,
		Limits:            transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		InspectionTimeout: 50 * time.Millisecond,
	})
	server := httptest.NewServer(suite)
	defer server.Close()

	bodyReader, bodyWriter := io.Pipe()
	defer bodyWriter.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	type result struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		resultCh <- result{resp: resp, err: err}
	}()
	if _, err := bodyWriter.Write([]byte(`{"model":`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.resp.Body.Close()
		if got.resp.StatusCode != http.StatusRequestTimeout {
			t.Fatalf("stalled body status = %d, want 408", got.resp.StatusCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stalled request body did not hit inspection deadline")
	}
}

func TestCatalogSuiteHandoffReleasesAdmissionOnBodyDrain(t *testing.T) {
	// Configure an admission pool with exactly 1 slot so a second concurrent
	// request MUST wait unless the first request's slot is released upon body drain.
	admission := router.NewCatalogSuiteAdmission(transcode.BodyLimits{AcceptedRequestBytes: 128 << 20})

	req1BodyRead := make(chan struct{})
	finishReq1 := make(chan struct{})

	target1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("req1 read body error: %v", err)
		}
		if !strings.Contains(string(body), "model-1") {
			t.Errorf("req1 body unexpected: %s", string(body))
		}
		_ = r.Body.Close()
		close(req1BodyRead) // signal that req1 has fully drained bucket 2's buffer

		// Hold target1 open to simulate a slow upstream completion / streaming response
		<-finishReq1
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"req1 done"}`))
	})

	var req2Hits atomic.Int64
	target2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req2Hits.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("req2 read body error: %v", err)
		}
		if !strings.Contains(string(body), "model-2") {
			t.Errorf("req2 body unexpected: %s", string(body))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"req2 done"}`))
	})

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:            true,
		DefaultShape:      transcode.CatalogShapeOpenAI,
		Limits:            transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		InspectionTimeout: 500 * time.Millisecond,
		Admission:         admission,
		ModelRoutes: []router.ModelRoute{
			{Model: "model-1", Handler: target1, SupportedRoutes: allSuiteRoutes()},
			{Model: "model-2", Handler: target2, SupportedRoutes: allSuiteRoutes()},
		},
	})

	req1Done := make(chan struct{})
	var rec1 *httptest.ResponseRecorder
	go func() {
		defer close(req1Done)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model-1","data":"payload"}`))
		rec1 = httptest.NewRecorder()
		suite.ServeHTTP(rec1, req)
	}()

	// Wait until req1 has drained its body
	select {
	case <-req1BodyRead:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for req1 to drain body")
	}

	// While req1 is STILL ACTIVE inside target1 (finishReq1 is not closed),
	// send req2. Because req1's body drain released the admission slot,
	// req2 must acquire the slot immediately and succeed.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model-2","data":"payload2"}`))
	rec2 := httptest.NewRecorder()
	suite.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("req2 status = %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
	if req2Hits.Load() != 1 {
		t.Fatalf("req2Hits = %d, want 1", req2Hits.Load())
	}

	// Now finish req1
	close(finishReq1)
	<-req1Done

	if rec1.Code != http.StatusOK {
		t.Fatalf("req1 status = %d, want 200: %s", rec1.Code, rec1.Body.String())
	}
}

func TestCatalogSuiteHandoffReleasesAdmissionOnGetBodyDrain(t *testing.T) {
	// 1 slot admission pool
	admission := router.NewCatalogSuiteAdmission(transcode.BodyLimits{AcceptedRequestBytes: 128 << 20})

	req1BodyRead := make(chan struct{})
	finishReq1 := make(chan struct{})

	target1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.GetBody == nil {
			t.Fatal("expected r.GetBody to be set")
		}
		rc, err := r.GetBody()
		if err != nil {
			t.Fatalf("GetBody failed: %v", err)
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Errorf("req1 read GetBody error: %v", err)
		}
		if !strings.Contains(string(body), "model-1") {
			t.Errorf("req1 body unexpected: %s", string(body))
		}
		close(req1BodyRead) // signal that req1 has fully drained via GetBody

		<-finishReq1
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"req1 done"}`))
	})

	var req2Hits atomic.Int64
	target2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req2Hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:            true,
		DefaultShape:      transcode.CatalogShapeOpenAI,
		Limits:            transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		InspectionTimeout: 500 * time.Millisecond,
		Admission:         admission,
		ModelRoutes: []router.ModelRoute{
			{Model: "model-1", Handler: target1, SupportedRoutes: allSuiteRoutes()},
			{Model: "model-2", Handler: target2, SupportedRoutes: allSuiteRoutes()},
		},
	})

	req1Done := make(chan struct{})
	var rec1 *httptest.ResponseRecorder
	go func() {
		defer close(req1Done)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model-1"}`))
		rec1 = httptest.NewRecorder()
		suite.ServeHTTP(rec1, req)
	}()

	select {
	case <-req1BodyRead:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for req1 to drain GetBody")
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model-2"}`))
	rec2 := httptest.NewRecorder()
	suite.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("req2 status = %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
	if req2Hits.Load() != 1 {
		t.Fatalf("req2Hits = %d, want 1", req2Hits.Load())
	}

	close(finishReq1)
	<-req1Done

	if rec1.Code != http.StatusOK {
		t.Fatalf("req1 status = %d, want 200: %s", rec1.Code, rec1.Body.String())
	}
}

func TestCatalogSuiteConcurrentPassthroughExceedsBufferSlots(t *testing.T) {
	// Default limits -> slots = max(1, 64MiB / 32MiB) = 2 slots.
	var concurrentInTarget atomic.Int64
	var maxConcurrentObserved atomic.Int64
	var totalSuccess atomic.Int64

	target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := concurrentInTarget.Add(1)
		defer concurrentInTarget.Add(-1)
		for {
			old := maxConcurrentObserved.Load()
			if cur <= old || maxConcurrentObserved.CompareAndSwap(old, cur) {
				break
			}
		}

		// Drain body immediately like a reverse proxy sending to upstream
		_, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()

		// Simulate upstream latency
		time.Sleep(50 * time.Millisecond)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:            true,
		DefaultShape:      transcode.CatalogShapeOpenAI,
		InspectionTimeout: 100 * time.Millisecond,
		ModelRoutes: []router.ModelRoute{
			{Model: "pass-model", Handler: target, SupportedRoutes: allSuiteRoutes()},
		},
	})

	const numRequests = 8
	var wg sync.WaitGroup
	errCh := make(chan error, numRequests)

	for i := range numRequests {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"pass-model","req":`+strconv.Itoa(id)+`}`))
			rec := httptest.NewRecorder()
			suite.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				errCh <- fmt.Errorf("request %d failed with status %d: %s", id, rec.Code, rec.Body.String())
				return
			}
			totalSuccess.Add(1)
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}

	if totalSuccess.Load() != numRequests {
		t.Fatalf("totalSuccess = %d, want %d", totalSuccess.Load(), numRequests)
	}
	if maxConcurrentObserved.Load() <= 2 {
		t.Logf("observed concurrency in target: %d (slots were 2)", maxConcurrentObserved.Load())
	}
}

func TestCatalogSuiteSafetyReleaseOnEarlyReturn(t *testing.T) {
	// 1 slot
	admission := router.NewCatalogSuiteAdmission(transcode.BodyLimits{AcceptedRequestBytes: 128 << 20})

	targetEarlyReturn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return 400 without reading or closing r.Body
		w.WriteHeader(http.StatusBadRequest)
	})

	var req2Hit bool
	targetNormal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req2Hit = true
		w.WriteHeader(http.StatusOK)
	})

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeOpenAI,
		Limits:       transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		Admission:    admission,
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: targetEarlyReturn, SupportedRoutes: allSuiteRoutes()},
			{Model: "m2", Handler: targetNormal, SupportedRoutes: allSuiteRoutes()},
		},
	})

	req1 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m1"}`))
	rec1 := httptest.NewRecorder()
	suite.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusBadRequest {
		t.Fatalf("req1 status = %d, want 400", rec1.Code)
	}

	// Slot must be available for req2
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m2"}`))
	rec2 := httptest.NewRecorder()
	suite.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("req2 status = %d, want 200", rec2.Code)
	}
	if !req2Hit {
		t.Fatal("req2 target was not reached")
	}
}
