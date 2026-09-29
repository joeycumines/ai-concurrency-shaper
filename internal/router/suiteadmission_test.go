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

// targetFailures carries an assertion made on a handler goroutine back to the
// test goroutine. t.Fatal calls runtime.Goexit, which on a handler goroutine
// skips the cleanup the test registered and surfaces later as a different
// failure, so handlers report and the test judges.
type targetFailures chan error

func (f targetFailures) send(format string, args ...any) {
	select {
	case f <- fmt.Errorf(format, args...):
	default:
	}
}

func (f targetFailures) check(t *testing.T) {
	t.Helper()
	for {
		select {
		case err := <-f:
			t.Error(err)
		default:
			return
		}
	}
}

// hold is an idempotent release for a channel a held request blocks on. A test
// that fails before releasing it still unblocks the goroutine it started,
// because the release is registered with t.Cleanup the moment the hold exists.
type hold struct {
	ch   chan struct{}
	once sync.Once
}

func newHold() *hold {
	h := &hold{ch: make(chan struct{})}
	return h
}

func (h *hold) wait() { <-h.ch }

func (h *hold) release() { h.once.Do(func() { close(h.ch) }) }

// waitFor fails the test if ch is not closed within d.
func waitFor(t *testing.T, what string, ch <-chan struct{}, d time.Duration) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("timed out after %s waiting for %s", d, what)
	}
}

// oneSlotAdmission is a pool with exactly one slot, so a second concurrent
// request can only be served if the first one gives the slot back.
func oneSlotAdmission() *router.CatalogSuiteAdmission {
	return router.NewCatalogSuiteAdmission(transcode.BodyLimits{AcceptedRequestBytes: 128 << 20})
}

// TestCatalogSuiteBufferAdmissionIsGloballyBounded proves the pool is shared:
// while one suite's target still holds an unspent body, a second suite sharing
// the pool waits for a slot instead of being turned away — the client blocks
// until the proxy can admit it, and is never told to go retry.
func TestCatalogSuiteBufferAdmissionIsGloballyBounded(t *testing.T) {
	admission := oneSlotAdmission()
	entered := make(chan struct{})
	hold := newHold()
	t.Cleanup(hold.release)

	unreadTarget := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		// The body is never read and never closed: the catalog's buffering
		// window is still open, so the slot must still be held.
		hold.wait()
		w.WriteHeader(http.StatusOK)
	})
	var secondHits atomic.Int64
	secondTarget := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	first := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeCodex,
		Limits:       transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		Admission:    admission,
		ModelRoutes:  []router.ModelRoute{{Model: "m1", Handler: unreadTarget, SupportedRoutes: allSuiteRoutes()}},
	})
	second := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeCodex,
		Limits:       transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		Admission:    admission,
		ModelRoutes:  []router.ModelRoute{{Model: "m2", Handler: secondTarget, SupportedRoutes: allSuiteRoutes()}},
	})

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		first.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m1"}`)))
		firstDone <- rec
	}()
	waitFor(t, "the first target to be entered", entered, 5*time.Second)

	// The shared pool has no free slot. This request must WAIT: it is neither
	// answered nor allowed to reach its provider. Its own context is the only
	// thing that ends the wait, which is what bounds this observation.
	waitingCtx, cancelWaiting := context.WithCancel(context.Background())
	waitingDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		second.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"m2"}`)).WithContext(waitingCtx))
		waitingDone <- rec
	}()
	select {
	case rec := <-waitingDone:
		t.Fatalf("a request was answered with %d while the shared pool was exhausted; it must wait for a slot: %s",
			rec.Code, rec.Body.String())
	case <-time.After(250 * time.Millisecond):
	}
	if secondHits.Load() != 0 {
		t.Fatalf("second target hits = %d, want 0: an unspent body must hold the shared slot", secondHits.Load())
	}

	// A waiter whose client is gone leaves without reaching a provider.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledRec := httptest.NewRecorder()
	second.ServeHTTP(canceledRec,
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m2"}`)).WithContext(ctx))
	if secondHits.Load() != 0 {
		t.Fatalf("a canceled waiter reached the second target: hits = %d", secondHits.Load())
	}

	// The waiting request leaves with its client rather than hanging forever.
	cancelWaiting()
	<-waitingDone

	hold.release()
	if rec1 := <-firstDone; rec1.Code != http.StatusOK {
		t.Fatalf("first suite status = %d, want 200", rec1.Code)
	}

	// With the slot free again the next request is admitted and served.
	rec3 := httptest.NewRecorder()
	second.ServeHTTP(rec3, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m2"}`)))
	if rec3.Code != http.StatusOK {
		t.Fatalf("status once the slot was free = %d, want 200: %s", rec3.Code, rec3.Body.String())
	}
	if secondHits.Load() != 1 {
		t.Fatalf("second target hits = %d, want 1", secondHits.Load())
	}
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

// deadlineRecorder tracks the read deadline the catalog arms through
// http.NewResponseController. httptest.ResponseRecorder reports deadlines as
// unsupported, so a live server would be the only other way to observe this
// and the connection-level detail would be invisible; the recording writer
// stands in for the connection.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu    sync.Mutex
	armed bool
	ever  bool
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed = !t.IsZero()
	if d.armed {
		d.ever = true
	}
	return nil
}

func (d *deadlineRecorder) state() (armed, ever bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.armed, d.ever
}

// TestCatalogSuiteInspectionDeadlineClearedBeforeDispatch proves the body
// inspection timeout does not outlive the inspection. A deadline left armed
// across target.handler.ServeHTTP puts a body-read timeout on a long upstream
// exchange: on HTTP/2 the stream deadline is a timer that fires on its own and
// resets the stream mid-response.
func TestCatalogSuiteInspectionDeadlineClearedBeforeDispatch(t *testing.T) {
	failures := make(targetFailures, 2)
	observed := make(chan bool, 1)

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:            true,
		DefaultShape:      transcode.CatalogShapeOpenAI,
		InspectionTimeout: 30 * time.Second,
		ModelRoutes: []router.ModelRoute{{
			Model: "m",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				rec := w.(*deadlineRecorder)
				armed, ever := rec.state()
				if !ever {
					failures.send("no read deadline was ever armed; the controller never reached the writer")
				}
				observed <- armed
				w.WriteHeader(http.StatusOK)
			}),
			SupportedRoutes: allSuiteRoutes(),
		}},
	})

	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	suite.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m"}`)))

	select {
	case armed := <-observed:
		if armed {
			t.Fatal("a read deadline was still armed while the target handler ran")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("target handler never ran")
	}
	failures.check(t)
}

// TestCatalogSuiteHandoffReleasesAdmissionOnBodyDrain proves the token comes
// back as soon as the target spends the body, while the target is still
// running its own exchange.
func TestCatalogSuiteHandoffReleasesAdmissionOnBodyDrain(t *testing.T) {
	admission := oneSlotAdmission()
	failures := make(targetFailures, 4)
	bodySpent := make(chan struct{})
	hold := newHold()
	t.Cleanup(hold.release)

	const payload = `{"model":"model-1","data":"payload-1"}`
	target1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			failures.send("req1 read body: %v", err)
			return
		}
		if string(body) != payload {
			failures.send("req1 body = %q, want %q", body, payload)
			return
		}
		close(bodySpent)
		// The upstream exchange is still running here. Holding the
		// admission token across it would cap the whole mount at one
		// request.
		hold.wait()
		w.WriteHeader(http.StatusOK)
	})
	var req2Hits atomic.Int64
	target2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req2Hits.Add(1)
		_, _ = io.ReadAll(r.Body)
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

	req1Done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		suite.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload)))
		req1Done <- rec
	}()
	waitFor(t, "req1 to spend its body", bodySpent, 5*time.Second)

	rec2 := httptest.NewRecorder()
	suite.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model-2"}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("req2 status = %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
	if req2Hits.Load() != 1 {
		t.Fatalf("req2 target hits = %d, want 1", req2Hits.Load())
	}

	hold.release()
	if rec1 := <-req1Done; rec1.Code != http.StatusOK {
		t.Fatalf("req1 status = %d, want 200: %s", rec1.Code, rec1.Body.String())
	}
	failures.check(t)
}

// TestCatalogSuiteHandoffBodyCopiesAndReleaseOwnership pins both halves of the
// handoff contract at once:
//
//   - every copy handed to the target is byte-identical and independently
//     re-readable, and
//   - only spending r.Body gives the admission token back. Draining a replay
//     body from r.GetBody is not evidence that the catalog's buffer is gone,
//     and releasing on it would let the pool admit more bodies than it exists
//     to bound.
//
// The primary body is spent with an exact-sized io.ReadFull and never closed:
// that read takes the last byte and reports (n, nil), so neither the EOF that
// bytes.Reader defers to a later read nor a Close ever arrives. Exhaustion is
// the only signal that can release the token here.
func TestCatalogSuiteHandoffBodyCopiesAndReleaseOwnership(t *testing.T) {
	admission := oneSlotAdmission()
	failures := make(targetFailures, 8)
	replaysDrained := make(chan struct{})
	spendPrimary := newHold()
	primarySpent := make(chan struct{})
	hold := newHold()
	t.Cleanup(hold.release)
	t.Cleanup(spendPrimary.release)

	const payload = `{"model":"model-1","data":"payload-1"}`
	target1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := range 2 {
			replay, err := r.GetBody()
			if err != nil {
				failures.send("GetBody %d: %v", i, err)
				return
			}
			got, err := io.ReadAll(replay)
			_ = replay.Close()
			if err != nil {
				failures.send("replay %d read: %v", i, err)
				return
			}
			if string(got) != payload {
				failures.send("replay %d body = %q, want %q", i, got, payload)
				return
			}
		}
		close(replaysDrained)

		spendPrimary.wait()
		buf := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			failures.send("primary read: %v", err)
			return
		}
		if string(buf) != payload {
			failures.send("primary body = %q, want %q", buf, payload)
			return
		}
		close(primarySpent)
		hold.wait()
		w.WriteHeader(http.StatusOK)
	})
	var secondHits atomic.Int64
	secondTarget := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	first := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:            true,
		DefaultShape:      transcode.CatalogShapeOpenAI,
		Limits:            transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		InspectionTimeout: 30 * time.Second,
		Admission:         admission,
		ModelRoutes:       []router.ModelRoute{{Model: "model-1", Handler: target1, SupportedRoutes: allSuiteRoutes()}},
	})
	second := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:            true,
		DefaultShape:      transcode.CatalogShapeOpenAI,
		Limits:            transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		Admission:         admission,
		InspectionTimeout: 150 * time.Millisecond,
		ModelRoutes:       []router.ModelRoute{{Model: "model-2", Handler: secondTarget, SupportedRoutes: allSuiteRoutes()}},
	})

	req1Done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		first.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload)))
		req1Done <- rec
	}()
	waitFor(t, "req1 to drain both replay bodies", replaysDrained, 5*time.Second)

	// Replay copies are spent; the catalog's own body is not. The pool must
	// still be exhausted, so this request waits rather than being answered.
	blockedCtx, cancelBlocked := context.WithCancel(context.Background())
	blockedDone := make(chan struct{})
	go func() {
		defer close(blockedDone)
		second.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"model-2"}`)).WithContext(blockedCtx))
	}()
	select {
	case <-blockedDone:
		t.Fatal("a request was answered after a replay-only drain: draining a replay body must not release the token")
	case <-time.After(250 * time.Millisecond):
	}
	if secondHits.Load() != 0 {
		t.Fatalf("second target hits = %d, want 0", secondHits.Load())
	}
	cancelBlocked()
	<-blockedDone

	spendPrimary.release()
	waitFor(t, "req1 to spend the primary body", primarySpent, 5*time.Second)

	rec2 := httptest.NewRecorder()
	second.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model-2"}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("status after the primary body was spent = %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
	if secondHits.Load() != 1 {
		t.Fatalf("second target hits = %d, want 1", secondHits.Load())
	}

	hold.release()
	if rec1 := <-req1Done; rec1.Code != http.StatusOK {
		t.Fatalf("req1 status = %d, want 200: %s", rec1.Code, rec1.Body.String())
	}
	failures.check(t)
}

// TestCatalogSuiteHandoffBodyForwardsInOneWrite keeps the single-Write copy
// path the pre-handoff body had: io.NopCloser over a *bytes.Reader is a
// writer-to, so a consumer that copies the body directly forwarded it in one
// Write, and a wrapper that only implements Read would silently chunk it
// through a 32 KiB buffer instead. (A net/http Transport sending a body of
// known length does not take that path — transferWriter wraps a sized body in
// an io.LimitReader — so this pins the direct-consumer contract, not the
// transport's.)
func TestCatalogSuiteHandoffBodyForwardsInOneWrite(t *testing.T) {
	failures := make(targetFailures, 4)
	payload := `{"model":"m","data":"` + strings.Repeat("x", 64<<10) + `"}`

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeOpenAI,
		ModelRoutes: []router.ModelRoute{{
			Model: "m",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, ok := r.Body.(io.WriterTo); !ok {
					failures.send("handoff body does not implement io.WriterTo")
				}
				var sink writeCountingSink
				n, err := io.Copy(&sink, r.Body)
				if err != nil {
					failures.send("copy body: %v", err)
					return
				}
				if int(n) != len(payload) {
					failures.send("copied %d bytes, want %d", n, len(payload))
				}
				if sink.writes != 1 {
					failures.send("forwarded the body in %d writes, want 1", sink.writes)
				}
				if sink.bytes != payload {
					failures.send("forwarded payload does not match the request body")
				}
				w.WriteHeader(http.StatusOK)
			}),
			SupportedRoutes: allSuiteRoutes(),
		}},
	})

	rec := httptest.NewRecorder()
	suite.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	failures.check(t)
}

type writeCountingSink struct {
	writes int
	bytes  string
}

func (s *writeCountingSink) Write(p []byte) (int, error) {
	s.writes++
	s.bytes += string(p)
	return len(p), nil
}

// TestCatalogSuiteHandoffReleasesAdmissionOnPanic proves the handler-return
// backstop covers a target that unwinds without ever touching the body.
func TestCatalogSuiteHandoffReleasesAdmissionOnPanic(t *testing.T) {
	admission := oneSlotAdmission()
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("target exploded")
	})
	var req2Hits atomic.Int64
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeOpenAI,
		Limits:       transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		// InspectionTimeout does NOT bound admission any more -- a request
		// that finds the pool full waits, and only a freed slot or the
		// client's context ends that. So a leaked token here shows up as the
		// follow-up request below hanging until the package timeout, not as a
		// prompt failure. The first request is already finished when the
		// second one starts, so an admitted slot is immediate.
		InspectionTimeout: 2 * time.Second,
		Admission:         admission,
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: panicking, SupportedRoutes: allSuiteRoutes()},
			{Model: "m2", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req2Hits.Add(1)
				w.WriteHeader(http.StatusOK)
			}), SupportedRoutes: allSuiteRoutes()},
		},
	})

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panicking target's panic did not propagate out of the suite")
			}
		}()
		suite.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m1"}`)))
	}()

	rec2 := httptest.NewRecorder()
	suite.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m2"}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("status after a panicking target = %d, want 200: a panic must not leak the token: %s",
			rec2.Code, rec2.Body.String())
	}
	if req2Hits.Load() != 1 {
		t.Fatalf("req2 target hits = %d, want 1", req2Hits.Load())
	}
}

// TestCatalogSuiteSafetyReleaseOnEarlyReturn proves a target that answers
// without ever reading or closing the body still returns the token when it
// returns.
func TestCatalogSuiteSafetyReleaseOnEarlyReturn(t *testing.T) {
	admission := oneSlotAdmission()
	var req2Hit bool
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeOpenAI,
		Limits:       transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		// InspectionTimeout does NOT bound admission any more -- a request
		// that finds the pool full waits, and only a freed slot or the
		// client's context ends that. So a leaked token here shows up as the
		// follow-up request below hanging until the package timeout, not as a
		// prompt failure. The first request is already finished when the
		// second one starts, so an admitted slot is immediate.
		InspectionTimeout: 2 * time.Second,
		Admission:         admission,
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
			}), SupportedRoutes: allSuiteRoutes()},
			{Model: "m2", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req2Hit = true
				w.WriteHeader(http.StatusOK)
			}), SupportedRoutes: allSuiteRoutes()},
		},
	})

	rec1 := httptest.NewRecorder()
	suite.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m1"}`)))
	if rec1.Code != http.StatusBadRequest {
		t.Fatalf("req1 status = %d, want 400", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	suite.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m2"}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("req2 status = %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
	if !req2Hit {
		t.Fatal("req2 target was not reached")
	}
}

// TestCatalogSuiteConcurrentRequestsExceedBufferSlots proves the admission
// pool is not a concurrency ceiling. Every target spends its body on entry and
// then parks, so the number of requests that ever reach a target at the same
// time is decided by the pool only if the pool is still being held. A pool
// that were released at handler return would admit exactly two and the wait
// below would expire.
func TestCatalogSuiteConcurrentRequestsExceedBufferSlots(t *testing.T) {
	// Default limits -> max(1, 64MiB / 32MiB) = 2 slots.
	const numRequests = 6
	entered := make(chan struct{}, numRequests)
	hold := newHold()
	t.Cleanup(hold.release)

	var inTarget atomic.Int64
	var maxInTarget atomic.Int64
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:            true,
		DefaultShape:      transcode.CatalogShapeOpenAI,
		InspectionTimeout: 30 * time.Second,
		ModelRoutes: []router.ModelRoute{{
			Model: "pass-model",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				current := inTarget.Add(1)
				for {
					old := maxInTarget.Load()
					if current <= old || maxInTarget.CompareAndSwap(old, current) {
						break
					}
				}
				defer inTarget.Add(-1)
				_, _ = io.ReadAll(r.Body)
				_ = r.Body.Close()
				entered <- struct{}{}
				hold.wait()
				w.WriteHeader(http.StatusOK)
			}),
			SupportedRoutes: allSuiteRoutes(),
		}},
	})

	codes := make([]int, numRequests)
	var wg sync.WaitGroup
	for i := range numRequests {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			body := `{"model":"pass-model","req":` + strconv.Itoa(id) + `}`
			suite.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
			codes[id] = rec.Code
		}(i)
	}

	// All six must be inside a target simultaneously. Each one gave its slot
	// back on the way in, so a two-slot pool is enough to get here and a pool
	// held to handler return is not.
	for range numRequests {
		waitFor(t, "a request to reach the target", entered, 10*time.Second)
	}
	if got := maxInTarget.Load(); got <= 2 {
		t.Fatalf("max concurrent requests in the target = %d, want more than the 2 admission slots", got)
	}

	hold.release()
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d status = %d, want 200", i, code)
		}
	}
}
