package diagnostic

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

func TestCaptureFourLegBudgetAndImmutableSeal(t *testing.T) {
	for _, budget := range []int{0, 1, 3, 4, 7, 257, MaxCaptureBytes, MaxCaptureBytes + 1} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			c := New("epoch/1", budget)
			data := bytes.Repeat([]byte("x"), MaxCaptureBytes/4+1)
			for _, leg := range []LegName{ClientRequest, UpstreamRequest, UpstreamResponse, DownstreamResponse} {
				c.SetBody(leg, data, "application/json")
			}
			s := c.Seal()
			sum := 0
			for _, l := range s.Legs {
				sum += len(l.Body)
				if !l.Complete || !l.Truncated || l.ObservedBytes != int64(len(data)) {
					t.Fatalf("untruthful leg: %+v", l)
				}
			}
			if sum != min(budget, MaxCaptureBytes) {
				t.Fatalf("retained=%d budget=%d", sum, budget)
			}
			if len(s.Legs[0].Body) > 0 {
				s.Legs[0].Body[0] = '!'
			}
			s.Decisions = append(s.Decisions, Decision{Feature: "mutated"})
			c.Append(ClientRequest, []byte("late"))
			c.Failure("late", "late", "", "", "")
			again := c.Snapshot()
			if len(again.Decisions) != 0 || len(again.Failures) != 0 || again.Legs[0].ObservedBytes != int64(len(data)) {
				t.Fatal("sealed capture mutated")
			}
			if len(again.Legs[0].Body) > 0 && again.Legs[0].Body[0] != 'x' {
				t.Fatal("snapshot aliases retained body")
			}
		})
	}
}
func TestCaptureConcurrentBoundsAndSnapshots(t *testing.T) {
	c := New("epoch/2", 1024)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 100 {
				c.Append(ClientRequest, []byte("abcdef"))
				c.Stage("decode")
				c.Decision(Decision{Stage: "request", Feature: "unsupported", Path: string(rune(i))})
				c.Failure("request", "unknown_field", "private", "", "secret")
				_ = c.Snapshot()
			}
		})
	}
	wg.Wait()
	s := c.Seal()
	if len(s.Stages) != MaxStages || len(s.Failures) != MaxFailures || len(s.Decisions) != 100 || len(s.Legs[0].Body) != 256 || s.Legs[0].ObservedBytes != 4800 {
		t.Fatalf("bounds or counts wrong: stages=%d failures=%d decisions=%d bytes=%d/%d", len(s.Stages), len(s.Failures), len(s.Decisions), len(s.Legs[0].Body), s.Legs[0].ObservedBytes)
	}
}
func TestFailureFingerprintDoesNotEncodeSecrets(t *testing.T) {
	c := New("one", 0)
	c.Protocols("messages", "responses", nil, nil, nil)
	a := c.Failure("request", "unknown_field", "$.SECRET-A", "", "bodysecretA")
	b := c.Failure("request", "unknown_field", "$.SECRET-B", "", "bodysecretB")
	if a != b || len(a) != 24 {
		t.Fatalf("unstable grouping %q %q", a, b)
	}
	if a == c.Failure("response", "unknown_field", "$.SECRET-A", "", "bodysecretA") {
		t.Fatal("stage not represented")
	}
}
func TestBodyCompletionIsEOFNotClose(t *testing.T) {
	c := New("one", 64)
	r := c.WrapBody(UpstreamResponse, io.NopCloser(bytes.NewBufferString("123")), "application/json", 200)
	b := make([]byte, 1)
	_, _ = r.Read(b)
	_ = r.Close()
	if c.Snapshot().Legs[2].Complete {
		t.Fatal("unread close is not complete")
	}
	c = New("two", 64)
	r = c.WrapBody(UpstreamResponse, io.NopCloser(bytes.NewBufferString("123")), "application/json", 200)
	_, _ = io.ReadAll(r)
	if s := c.Snapshot().Legs[2]; !s.Complete || s.ObservedBytes != 3 {
		t.Fatalf("bad complete %+v", s)
	}
}

type shortWriter struct {
	header http.Header
	status int
}

func (w *shortWriter) Header() http.Header         { return w.header }
func (w *shortWriter) WriteHeader(n int)           { w.status = n }
func (w *shortWriter) Write(p []byte) (int, error) { return min(2, len(p)), nil }
func (w *shortWriter) FlushError() error           { return errors.New("flush failed") }
func TestWriterAcceptedBytesShortWriteAndFlush(t *testing.T) {
	c := New("one", 100)
	under := &shortWriter{header: make(http.Header)}
	w := WrapWriter(under, c)
	w.WriteHeader(103)
	if w.Status != 0 {
		t.Fatal("informational status committed")
	}
	w.Header().Set("Content-Type", "application/json")
	n, err := w.Write([]byte("abcdef"))
	if n != 2 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("write=%d,%v", n, err)
	}
	if s := c.Snapshot().Legs[3]; s.ObservedBytes != 2 || string(s.Body) != "ab" || s.Status != 200 {
		t.Fatalf("offered bytes mislabeled accepted: %+v", s)
	}
	if err := http.NewResponseController(w).Flush(); err == nil {
		t.Fatal("flush failure lost")
	}
	w.WriteHeader(500)
	if w.Status != 200 {
		t.Fatal("status changed after commit")
	}
}
func TestWriterRealHTTPStreamingAndUnwrap(t *testing.T) {
	c := New("one", 1024)
	under := httptest.NewRecorder()
	w := WrapWriter(under, c)
	w.WriteHeader(201)
	_, _ = w.Write([]byte("one\n\n"))
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Fatal(err)
	}
	if !under.Flushed || under.Code != 201 || under.Body.String() != "one\n\n" || w.Unwrap() != under {
		t.Fatal("writer changed HTTP behavior")
	}
}
