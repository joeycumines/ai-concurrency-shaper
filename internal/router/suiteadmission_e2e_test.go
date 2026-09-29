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

// End-to-end composition: the catalog suite's admission pool in front of a
// real proxy.Proxy with its own limiter, which is the only place the
// admission ceiling the pool used to impose is observable end to end.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// TestCatalogSuiteConcurrencyReachesTheProviderLimit is the end-to-end form of
// the same claim. A catalog suite with the default two slots fronts a real
// proxy whose own limiter is four, and eight requests are issued at once: the
// upstream must see four simultaneous exchanges. The catalog's pool is two
// bodies deep, so a provider that saw only two would mean the pool, not the
// provider's limiter, was setting the ceiling.
func TestCatalogSuiteConcurrencyReachesTheProviderLimit(t *testing.T) {
	const (
		providerLimit = 4
		numRequests   = 8
	)
	hold := newHold()

	var inUpstream atomic.Int64
	var maxInUpstream atomic.Int64
	limitReached := make(chan struct{})
	var signalOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := inUpstream.Add(1)
		defer inUpstream.Add(-1)
		for {
			old := maxInUpstream.Load()
			if current <= old || maxInUpstream.CompareAndSwap(old, current) {
				break
			}
		}
		if current >= providerLimit {
			signalOnce.Do(func() { close(limitReached) })
		}
		_, _ = io.ReadAll(r.Body)
		hold.wait()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	limited, err := route.Parse("POST /v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	p, err := proxy.New(
		proxy.WithUpstream(upstreamURL),
		proxy.WithMatcher(route.NewMatcher([]route.Pattern{limited})),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(providerLimit, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:            true,
		DefaultShape:      transcode.CatalogShapeOpenAI,
		InspectionTimeout: 30 * time.Second,
		ModelRoutes:       []router.ModelRoute{{Model: "m", Handler: p, SupportedRoutes: allSuiteRoutes()}},
	})
	server := httptest.NewServer(suite)
	t.Cleanup(server.Close)
	// Registered last so it runs first: a failing assertion must unblock the
	// parked upstream exchanges before either test server waits on them.
	t.Cleanup(hold.release)

	codes := make([]int, numRequests)
	var wg sync.WaitGroup
	for i := range numRequests {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"m","req":%d}`, id)
			resp, err := http.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(body))
			if err != nil {
				t.Errorf("request %d: %v", id, err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			codes[id] = resp.StatusCode
		}(i)
	}

	select {
	case <-limitReached:
	case <-time.After(20 * time.Second):
		t.Fatalf("upstream never reached the provider's own limit of %d; the catalog admission pool reached it first (max observed %d)",
			providerLimit, maxInUpstream.Load())
	}
	if got := maxInUpstream.Load(); got != providerLimit {
		t.Fatalf("max concurrent upstream exchanges = %d, want exactly the provider limit %d", got, providerLimit)
	}

	hold.release()
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d status = %d, want 200", i, code)
		}
	}
}

// TestCatalogSuiteSaturatedProviderNeverTurnsAClientAway is the end-to-end
// form of the blocking-admission contract. Here the DOWNSTREAM limiter is the
// scarcer resource: the pool has two slots and the provider admits two at a
// time, so most requests are waiting on a provider slot while still holding
// their buffered body. Every one of them must wait for its turn and then be
// served. None may be answered with an error, because a client told to retry
// is exactly the backoff the proxy exists to absorb.
func TestCatalogSuiteSaturatedProviderNeverTurnsAClientAway(t *testing.T) {
	const (
		providerLimit = 2
		numRequests   = 8
	)
	hold := newHold()

	var inUpstream atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inUpstream.Add(1)
		defer inUpstream.Add(-1)
		_, _ = io.ReadAll(r.Body)
		hold.wait()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	limited, err := route.Parse("POST /v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	p, err := proxy.New(
		proxy.WithUpstream(upstreamURL),
		proxy.WithMatcher(route.NewMatcher([]route.Pattern{limited})),
		proxy.WithLimiter(queue.NewLimiterWithCooldown(providerLimit, 0)),
		proxy.WithMetrics(metrics.NewCollector()),
	)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// A SHORT inspection timeout, and the test then watches for several times
	// it. That is the discriminator: if admission still gave up and answered
	// an error once this elapsed, the client would be told to retry. It must
	// not, so the wait is genuinely unbounded and only the provider's own
	// progress ends it.
	suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
		Strict:            true,
		DefaultShape:      transcode.CatalogShapeOpenAI,
		InspectionTimeout: 300 * time.Millisecond,
		ModelRoutes:       []router.ModelRoute{{Model: "m", Handler: p, SupportedRoutes: allSuiteRoutes()}},
	})
	server := httptest.NewServer(suite)
	t.Cleanup(server.Close)
	// Registered last so it runs first: a failing assertion must unblock the
	// parked upstream exchanges before either test server waits on them.
	t.Cleanup(hold.release)

	codes := make([]int, numRequests)
	var wg sync.WaitGroup
	for i := range numRequests {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"m","req":%d}`, id)
			resp, err := http.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(body))
			if err != nil {
				t.Errorf("request %d: %v", id, err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			codes[id] = resp.StatusCode
		}(i)
	}

	// The provider is saturated and holding. Every request must still be
	// waiting: none answered with a success it could not have had, and none
	// turned away with an error.
	saturatedFor := time.After(3 * time.Second)
	settled := false
	for !settled {
		select {
		case <-saturatedFor:
			settled = true
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	for i, code := range codes {
		if code != 0 {
			t.Fatalf("request %d was answered with %d while every provider slot was busy: "+
				"a saturated provider must make the client wait, never answer", i, code)
		}
	}
	if got := inUpstream.Load(); got > providerLimit {
		t.Fatalf("upstream concurrency = %d, want at most the provider limit %d", got, providerLimit)
	}

	// Let the provider drain. Every request must now be served, in waves.
	hold.release()
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d status = %d, want 200: a saturated provider must never turn a client away", i, code)
		}
	}
}

// bodyReadingSigner is a signer that consumes the body it is asked to sign,
// the way a payload-hashing signature scheme must.
type bodyReadingSigner struct{ calls atomic.Int64 }

func (s *bodyReadingSigner) Sign(_ context.Context, req *http.Request) error {
	s.calls.Add(1)
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}
	return nil
}

// headerOnlySigner is a signer that signs headers alone and never reads the
// body it is asked to sign.
type headerOnlySigner struct{ calls atomic.Int64 }

func (s *headerOnlySigner) Sign(_ context.Context, req *http.Request) error {
	s.calls.Add(1)
	req.Header.Set("X-Signed", "yes")
	return nil
}

// TestCatalogSuiteSignedRouteReleasesAdmissionBeforeUpstream pins the release
// contract on a route whose upstream request is signed per attempt. Signing
// rewrites the outbound body from a replay copy, so the question is whether
// that replay can leave the catalog holding its admission token for a whole
// upstream exchange — which would restore the very ceiling the handoff
// removes. Both signer shapes are covered: one that reads the body and one
// that never touches it.
func TestCatalogSuiteSignedRouteReleasesAdmissionBeforeUpstream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signer transcode.RequestSigner
		calls  func(transcode.RequestSigner) *atomic.Int64
	}{
		{"body-reading signer", &bodyReadingSigner{}, func(s transcode.RequestSigner) *atomic.Int64 { return &s.(*bodyReadingSigner).calls }},
		{"header-only signer", &headerOnlySigner{}, func(s transcode.RequestSigner) *atomic.Int64 { return &s.(*headerOnlySigner).calls }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hold := newHold()
			failures := make(targetFailures, 4)
			upstreamEntered := make(chan struct{}, 2)
			var upstreamCalls atomic.Int64

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := upstreamCalls.Add(1)
				upstreamEntered <- struct{}{}
				if call == 1 {
					// The first exchange stays in flight so the catalog's
					// token must already be back in the pool for the second
					// request to reach the upstream at all.
					hold.wait()
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			t.Cleanup(upstream.Close)

			upstreamURL, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			mapping := helperResponsesToChatMapping(t)
			mapping.Auth = transcode.AuthPolicy{Mode: transcode.AuthExternalSigner, Signer: tc.signer}
			// A single slot: a token held across the first exchange would
			// make the second request 503 rather than reach the upstream.
			p, err := proxy.New(
				proxy.WithUpstream(upstreamURL),
				proxy.WithMatcher(route.NewMatcher(nil)),
				proxy.WithLimiter(queue.NewLimiterWithCooldown(4, 0)),
				proxy.WithMetrics(metrics.NewCollector()),
				proxy.WithTranscodeMapping(proxy.TranscodeMapping{Mapping: mapping}),
			)
			if err != nil {
				t.Fatalf("proxy.New: %v", err)
			}

			suite := router.NewCatalogSuiteHandler(router.SuiteConfig{
				Strict:            true,
				DefaultShape:      transcode.CatalogShapeOpenAI,
				Limits:            transcode.BodyLimits{AcceptedRequestBytes: 128 << 20},
				Admission:         oneSlotAdmission(),
				InspectionTimeout: 2 * time.Second,
				ModelRoutes:       []router.ModelRoute{{Model: "m", Handler: p, SupportedRoutes: allSuiteRoutes()}},
			})
			server := httptest.NewServer(suite)
			t.Cleanup(server.Close)
			// Registered last so it runs first: a failing assertion must
			// unblock the parked upstream exchange before either test server
			// waits on it.
			t.Cleanup(hold.release)

			post := func(id int) chan int {
				done := make(chan int, 1)
				go func() {
					body := `{"model":"m","input":"` + strconv.Itoa(id) + `"}`
					req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
					if err != nil {
						failures.send("request %d: %v", id, err)
						done <- 0
						return
					}
					req.Header.Set("Content-Type", "application/json")
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						failures.send("request %d: %v", id, err)
						done <- 0
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					done <- resp.StatusCode
				}()
				return done
			}

			first := post(1)
			waitFor(t, "the first request to reach the upstream", upstreamEntered, 10*time.Second)

			second := post(2)
			waitFor(t, "the second request to reach the upstream", upstreamEntered, 10*time.Second)

			hold.release()
			if code := <-first; code != http.StatusOK {
				t.Errorf("first request status = %d, want 200", code)
			}
			if code := <-second; code != http.StatusOK {
				t.Errorf("second request status = %d, want 200", code)
			}
			if got := tc.calls(tc.signer).Load(); got != 2 {
				t.Errorf("signer invocations = %d, want 2 (every attempt is signed)", got)
			}
			failures.check(t)
		})
	}
}
