package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
	"github.com/joeycumines/ai-concurrency-shaper/internal/metrics"
	"github.com/joeycumines/ai-concurrency-shaper/internal/queue"
	"github.com/joeycumines/ai-concurrency-shaper/internal/route"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// FuzzTranscodeCancellationRaces cancels the client at every phase of a
// streaming transcode exchange and asserts:
//
//   - ServeHTTP returns promptly after cancellation;
//   - no downstream operation happens after ServeHTTP returned;
//   - a client abort is neither a breaker success nor a failure-streak reset;
//   - the upstream context/body is released.
func FuzzTranscodeCancellationRaces(f *testing.F) {
	// phase:
	//   0 = cancel before upstream
	//   1 = cancel after upstream handler starts
	//   2 = cancel after first downstream flush
	//   3 = cancel after source terminal frame
	//   4 = no cancellation
	for phase := byte(0); phase <= 4; phase++ {
		f.Add(phase, uint16(0), uint16(0))
	}

	f.Fuzz(func(
		t *testing.T,
		phase byte,
		upstreamDelayMS uint16,
		cancelDelayMS uint16,
	) {
		phase %= 5
		upstreamDelay := time.Duration(upstreamDelayMS%25) * time.Millisecond
		cancelDelay := time.Duration(cancelDelayMS%25) * time.Millisecond

		upstreamStarted := make(chan struct{})
		sourceTerminalWritten := make(chan struct{})
		upstreamCancelled := make(chan struct{})

		upstream := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				close(upstreamStarted)
				defer func() {
					select {
					case <-r.Context().Done():
						close(upstreamCancelled)
					default:
					}
				}()

				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)

				time.Sleep(upstreamDelay)

				_, _ = io.WriteString(
					w,
					"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
				)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}

				// Hold the stream open so a phase-2 cancellation
				// deterministically interrupts an incomplete exchange. The
				// fuzz delays are bounded well below this grace period, so
				// the terminal frames only arrive when the client cancels
				// (releasing the connection) or the stream completes
				// (phase 4).
				select {
				case <-r.Context().Done():
					return
				case <-time.After(250 * time.Millisecond):
				}

				_, _ = io.WriteString(
					w,
					"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
				)
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				close(sourceTerminalWritten)

				<-r.Context().Done()
			},
		))
		defer upstream.Close()

		breaker, err := circuitbreaker.New(
			circuitbreaker.WithFailureThreshold(100),
		)
		if err != nil {
			t.Fatal(err)
		}

		// Seed a streak. A non-terminal client abort must not reset it by being
		// recorded as a success.
		breaker.RecordFailure(500, 0, time.Time{}, 0)
		before := breaker.Stats()

		proxy := newStrictTranscodeProxyForCancellation(
			t,
			upstream.URL,
			breaker,
		)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel() // every path must release the request context
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(`{"model":"m","input":"x","stream":true}`),
		).WithContext(ctx)

		writer := newReturnGuardWriter()
		handlerDone := make(chan struct{})

		go func() {
			defer close(handlerDone)
			proxy.ServeHTTP(writer, request)
			writer.returned.Store(true)
		}()

		switch phase {
		case 0:
			cancel()

		case 1:
			select {
			case <-upstreamStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream did not start")
			}
			time.Sleep(cancelDelay)
			cancel()

		case 2:
			deadline := time.Now().Add(5 * time.Second)
			for writer.flushes.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if writer.flushes.Load() == 0 {
				t.Fatal("no downstream flush observed")
			}
			time.Sleep(cancelDelay)
			cancel()

		case 3:
			select {
			case <-sourceTerminalWritten:
			case <-time.After(5 * time.Second):
				t.Fatal("source terminal not written")
			}
			time.Sleep(cancelDelay)
			cancel()

		case 4:
			select {
			case <-sourceTerminalWritten:
			case <-time.After(5 * time.Second):
				t.Fatal("source terminal not written")
			}
			cancel()
		}

		select {
		case <-handlerDone:
		case <-time.After(2 * time.Second):
			t.Fatal("proxy did not return after cancellation")
		}

		// Allow any copy worker attempting a late operation to be observed.
		time.Sleep(10 * time.Millisecond)

		if got := writer.late.Load(); got != 0 {
			t.Fatalf("downstream operations after ServeHTTP returned: %d", got)
		}

		after := breaker.Stats()
		if phase <= 2 {
			if after.TotalSuccesses != before.TotalSuccesses {
				t.Fatalf(
					"client abort recorded success: before=%d after=%d phase=%d",
					before.TotalSuccesses,
					after.TotalSuccesses,
					phase,
				)
			}
			if after.ConsecutiveFailures != before.ConsecutiveFailures {
				t.Fatalf(
					"client abort reset failure streak: before=%d after=%d phase=%d totalFailures=%d writer.status=%d writer.late=%d",
					before.ConsecutiveFailures,
					after.ConsecutiveFailures,
					phase,
					after.TotalFailures,
					writer.status,
					writer.late.Load(),
				)
			}
		}

		if phase == 0 {
			// The request context was cancelled before the proxy contacted
			// the upstream: there is no connection to release. Assert the
			// upstream was never reached instead.
			select {
			case <-upstreamStarted:
				t.Fatal("upstream contacted despite pre-cancelled context")
			case <-time.After(50 * time.Millisecond):
			}
		} else {
			select {
			case <-upstreamCancelled:
			case <-time.After(time.Second):
				t.Fatal("upstream context/body was not released")
			}
		}
	})
}

// newStrictTranscodeProxyForCancellation builds a proxy with a strict
// Responses->Chat transcode mapping and the given breaker.
func newStrictTranscodeProxyForCancellation(
	t *testing.T,
	upstreamURL string,
	breaker *circuitbreaker.Breaker,
) *Proxy {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(
		WithUpstream(u),
		WithMatcher(route.NewMatcher(nil)),
		WithLimiter(queue.NewLimiterWithCooldown(2, 0)),
		WithMetrics(metrics.NewCollector()),
		WithBreaker(breaker),
		WithTranscodeMapping(transcodeMapping(testResponsesMapping(t))),
	)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var _ = transcode.ClientResponses
