package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A requested stop may exhaust the HTTP grace while a provider is still
// working. Forced closure must release the exchange and preserve its final
// evidence before the recorder closes, without converting that stop to exit 1.
// The identical fallback must retain an original fatal server/UI error.
func TestRunServerLifecycleGraceExpiryPersistsHistory(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		name := "operator_stop"
		if fatal {
			name = "fatal_error"
		}
		t.Run(name, func(t *testing.T) {
			rt, path := shutdownRuntimeFixture(t)
			entered := make(chan struct{})
			handlerReturned := make(chan struct{})
			srv := &http.Server{Handler: rt.observeIngress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-r.Context().Done()
				close(handlerReturned)
			}))}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(func() { cancel(); _ = srv.Close(); _ = ln.Close() })
			errCh := make(chan error, 2)
			lifecycleDone := make(chan error, 1)
			go func() { lifecycleDone <- runServerLifecycle(ctx, cancel, errCh, nil, nil, srv, ln) }()
			clientDone := make(chan error, 1)
			client := &http.Client{Timeout: 15 * time.Second}
			go func() {
				response, err := client.Get("http://" + ln.Addr().String() + "/unfinished")
				if response != nil {
					response.Body.Close()
				}
				clientDone <- err
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not start")
			}
			original := errors.New("server lifecycle failed")
			if fatal {
				errCh <- original
			} else {
				cancel()
			}
			select {
			case err := <-lifecycleDone:
				if fatal {
					if !errors.Is(err, original) || !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("fatal shutdown lost original error or grace diagnostic: %v", err)
					}
				} else if err != nil {
					t.Fatalf("intentional stop failed after successful forced close: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("HTTP shutdown did not finish after its grace")
			}
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer closeCancel()
			if err = rt.Close(closeCtx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-handlerReturned:
			default:
				t.Fatal("recorder closed before the handler returned")
			}
			select {
			case err := <-clientDone:
				if err == nil {
					t.Fatal("unfinished exchange became a successful response")
				}
			case <-time.After(time.Second):
				t.Fatal("forced close did not release the client")
			}
			fact := persistedShutdownFact(t, path)
			if fact.DurationNS <= 0 || rt.ingressJournal.FindRequestID(fact.ID) == nil {
				t.Fatalf("terminal duration or exact journal evidence missing: %+v", fact)
			}
			requireAddressReleased(t, ln.Addr().String())
		})
	}
}

// This listener closes its real socket but reports an actual close failure.
// Deadline-shaped listener errors must remain failures when the gateway's own
// grace has not expired; errors.Is(err, DeadlineExceeded) alone is insufficient.
type shutdownCloseErrorListener struct {
	net.Listener
	err error
}

func (l *shutdownCloseErrorListener) Close() error {
	_ = l.Listener.Close()
	return l.err
}

func TestRunServerLifecycleSignalPreservesListenerCloseError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"ordinary", errors.New("listener close failed")},
		{"deadline_value", context.DeadlineExceeded},
		{"wrapped_deadline", fmt.Errorf("listener failure: %w", context.DeadlineExceeded)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ln := &shutdownCloseErrorListener{Listener: base, err: tc.err}
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(func() { cancel(); _ = srv.Close(); _ = base.Close() })
			done := make(chan error, 1)
			go func() { done <- runServerLifecycle(ctx, cancel, make(chan error, 2), nil, nil, srv, ln) }()
			client := &http.Client{Timeout: 3 * time.Second}
			response, err := client.Get("http://" + ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if response.StatusCode != http.StatusNoContent || readErr != nil || closeErr != nil {
				t.Fatalf("initial exchange: status=%d read=%v close=%v", response.StatusCode, readErr, closeErr)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, tc.err) {
					t.Fatalf("operator stop hid listener close failure: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("idle shutdown did not return its listener error")
			}
			requireAddressReleased(t, ln.Addr().String())
		})
	}
}
