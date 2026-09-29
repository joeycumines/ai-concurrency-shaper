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

// The concurrency contract of the handoff body itself: what its mutex
// serializes, and what Close is deliberately allowed to do without it. The
// release-on-exhaustion and replay-ownership rules these depend on are covered
// in suiteadmission_test.go.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// blockingWriteSink accepts a Write and then parks inside it, so the copy is
// provably still in flight when the test closes the body.
type blockingWriteSink struct {
	entered chan struct{}
	release *hold
	once    sync.Once
}

func (s *blockingWriteSink) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.entered) })
	s.release.wait()
	return len(p), nil
}

// TestCatalogSuiteHandoffBodyClosesWhileWriteInFlight pins the lock scope of
// WriteTo. A catalog target is an arbitrary http.Handler, and copying a body
// on one goroutine while closing it on another is an ordinary use of an
// io.ReadCloser. Holding the body mutex across the destination write makes
// that Close wait for the copy, so a sink that is itself waiting on the
// closer deadlocks both. The close must therefore return while a Write is
// still blocked, and the admission token must still come back.
func TestCatalogSuiteHandoffBodyClosesWhileWriteInFlight(t *testing.T) {
	admission := oneSlotAdmission()
	failures := make(targetFailures, 4)
	payload := `{"model":"m1","data":"payload"}`

	sinkRelease := newHold()
	t.Cleanup(sinkRelease.release)
	closed := make(chan struct{})
	copied := newHold()
	t.Cleanup(copied.release)

	var req2Hits atomic.Int64
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeOpenAI,
		Limits:       transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		// Short so a leaked token surfaces as a prompt 503 rather than the
		// full default wait. The first request is finished by the time the
		// second starts, so an admitted slot is immediate.
		InspectionTimeout: 2 * time.Second,
		Admission:         admission,
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sink := &blockingWriteSink{entered: make(chan struct{}), release: sinkRelease}
				copyDone := make(chan struct{})
				go func() {
					defer close(copyDone)
					// The body is a writer-to, so this reaches WriteTo and
					// parks inside the destination write.
					if _, err := io.Copy(sink, r.Body); err != nil {
						failures.send("copy body: %v", err)
					}
				}()
				<-sink.entered

				// Close from this goroutine while the copy is still parked
				// inside Write. A body mutex held across the destination
				// write blocks here until the sink is released, which only
				// happens after this close returns.
				if err := r.Body.Close(); err != nil {
					failures.send("close body: %v", err)
				}
				close(closed)

				copied.wait()
				<-copyDone
				w.WriteHeader(http.StatusOK)
			}), SupportedRoutes: allSuiteRoutes()},
			{Model: "m2", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req2Hits.Add(1)
				w.WriteHeader(http.StatusOK)
			}), SupportedRoutes: allSuiteRoutes()},
		},
	})

	req1Done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		suite.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload)))
		req1Done <- rec
	}()

	waitFor(t, "the target to close the body while the copy is in flight", closed, 5*time.Second)

	// The close returned without waiting for the parked write, so the token
	// is back and a second request is served instead of 503-ing.
	rec2 := httptest.NewRecorder()
	suite.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m2"}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("status after a close during an in-flight write = %d, want 200: the close must not deadlock on the copy: %s",
			rec2.Code, rec2.Body.String())
	}
	if req2Hits.Load() != 1 {
		t.Fatalf("req2 target hits = %d, want 1", req2Hits.Load())
	}

	// Only now let the parked write finish, so the copy goroutine cannot
	// outlive the test.
	sinkRelease.release()
	copied.release()
	if rec1 := <-req1Done; rec1.Code != http.StatusOK {
		t.Fatalf("req1 status = %d, want 200: %s", rec1.Code, rec1.Body.String())
	}
	failures.check(t)
}

// TestCatalogSuiteHandoffBodySerializesReadAgainstWrite pins the invariant
// WriteTo's offset arithmetic depends on: every operation that moves the
// reader offset is serialized, so a Read cannot land between a WriteTo taking
// the lock and finishing its write. If one could, the offset would advance
// against a base a concurrent Read had already moved, and a later read would
// run past the end of the buffer and panic inside the target.
//
// The assertion is that a read issued while a write is parked inside the
// destination does NOT complete. Completion means the lock was released across
// the write, which is the corruption; blocking is the serialization. The
// window below bounds that observation rather than standing in for one — the
// test still fails outright if the read is refused, if the copy stalls, or if
// the body cannot be walked afterwards.
func TestCatalogSuiteHandoffBodySerializesReadAgainstWrite(t *testing.T) {
	payload := `{"model":"m1","data":"payload-bytes"}`

	copyRelease := newHold()
	t.Cleanup(copyRelease.release)
	sinkEntered := make(chan struct{})

	failures := make(targetFailures, 8)
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:       true,
		DefaultShape: transcode.CatalogShapeOpenAI,
		Limits:       transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		ModelRoutes: []router.ModelRoute{{
			Model: "m1",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sink := &blockingWriteSink{entered: sinkEntered, release: copyRelease}
				copied := make(chan struct{})
				go func() {
					defer close(copied)
					_, _ = io.Copy(sink, r.Body)
				}()
				<-sinkEntered

				// Issue a read while the copy is parked inside Write.
				readDone := make(chan error, 1)
				go func() {
					head := make([]byte, 4)
					_, err := io.ReadFull(r.Body, head)
					readDone <- err
				}()
				select {
				case err := <-readDone:
					failures.send("a read completed while a write was still in flight (%v): the body offset is not serialized", err)
				case <-time.After(250 * time.Millisecond):
				}

				copyRelease.release()
				select {
				case <-copied:
				case <-time.After(5 * time.Second):
					failures.send("the copy never finished after its destination was released")
					w.WriteHeader(http.StatusOK)
					return
				}
				select {
				case err := <-readDone:
					if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
						failures.send("read after the copy: %v", err)
					}
				case <-time.After(5 * time.Second):
					failures.send("the read never returned after the copy finished")
				}

				// The body must still be walkable: a corrupted offset panics
				// here instead of returning the tail.
				func() {
					defer func() {
						if r := recover(); r != nil {
							failures.send("final copy panicked: %v", r)
						}
					}()
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						failures.send("final copy: %v", err)
					}
				}()
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

// TestCatalogSuiteHandoffBodyShortWriteLeavesBytes proves a destination that
// accepts fewer bytes than it was given does not spend the body: the unwritten
// tail stays readable, and — probed while the tail is still unread — the
// admission token stays held, so the release keys off exhaustion rather than
// off the return of WriteTo.
func TestCatalogSuiteHandoffBodyShortWriteLeavesBytes(t *testing.T) {
	admission := oneSlotAdmission()
	failures := make(targetFailures, 8)
	payload := `{"model":"m1","data":"0123456789"}`
	const shortCount = 4

	var copiedRemainder string
	var req2Hits atomic.Int64
	var shortWrote sync.WaitGroup
	shortWrote.Add(1)
	remainderRead := newHold()
	t.Cleanup(remainderRead.release)

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:            true,
		DefaultShape:      transcode.CatalogShapeOpenAI,
		Limits:            transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
		InspectionTimeout: 150 * time.Millisecond,
		Admission:         admission,
		ModelRoutes: []router.ModelRoute{
			{Model: "m1", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n, err := io.Copy(shortSink{}, r.Body)
				if int(n) != shortCount {
					failures.send("short write reported %d bytes, want %d", n, shortCount)
				}
				if !errors.Is(err, io.ErrShortWrite) {
					failures.send("short write error = %v, want io.ErrShortWrite", err)
				}
				shortWrote.Done()

				// Wait until the test has probed the pool while this body is
				// still unspent: the short write alone must not have released.
				remainderRead.wait()

				// The tail the destination refused must still be readable.
				rest, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					failures.send("read remainder: %v", readErr)
				}
				copiedRemainder = string(rest)
				w.WriteHeader(http.StatusOK)
			}), SupportedRoutes: allSuiteRoutes()},
			{Model: "m2", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req2Hits.Add(1)
				w.WriteHeader(http.StatusOK)
			}), SupportedRoutes: allSuiteRoutes()},
		},
	})

	req1Done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		suite.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload)))
		req1Done <- rec
	}()
	shortWrote.Wait()

	// The body still has an unread tail, so the token must still be held and a
	// second request must wait for it rather than displacing this one.
	blockedCtx, cancelBlocked := context.WithCancel(context.Background())
	blockedDone := make(chan struct{})
	go func() {
		defer close(blockedDone)
		suite.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"m2"}`)).WithContext(blockedCtx))
	}()
	select {
	case <-blockedDone:
		t.Fatal("a request was admitted while the first body still had an unread tail")
	case <-time.After(250 * time.Millisecond):
	}
	if req2Hits.Load() != 0 {
		t.Fatalf("req2 target hits = %d, want 0 while the tail is unread", req2Hits.Load())
	}
	cancelBlocked()
	<-blockedDone

	remainderRead.release()
	if rec1 := <-req1Done; rec1.Code != http.StatusOK {
		t.Fatalf("req1 status = %d, want 200: %s", rec1.Code, rec1.Body.String())
	}

	wantTail := payload[shortCount:]
	if copiedRemainder != wantTail {
		t.Fatalf("remainder after the short write = %q, want %q", copiedRemainder, wantTail)
	}

	// The tail has now been read, so the body is spent and the token is back.
	rec2 := httptest.NewRecorder()
	suite.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m2"}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("status after the remainder was read = %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
	if req2Hits.Load() != 1 {
		t.Fatalf("req2 target hits = %d, want 1", req2Hits.Load())
	}
}

// shortSink accepts at most n bytes and reports a short write, the way a
// destination that cannot take the whole buffer does.
type shortSink struct{ n int }

func (s shortSink) Write(p []byte) (int, error) {
	limit := s.n
	if limit == 0 {
		limit = 4
	}
	if len(p) <= limit {
		return len(p), nil
	}
	return limit, nil
}
