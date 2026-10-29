package transcode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

func TestStreamOutcomeRetainsUpstreamStatusOnClientAbort(t *testing.T) {
	mapping := responsesMapping(t)
	ctx, cancel := context.WithCancel(context.Background())
	handler, outcomes := outcomeCaptureHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(string(testcorpus.ChatCompletionsStreamSSE()))),
		}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`)).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	outcome := <-outcomes
	if !outcome.UpstreamAttempted || !outcome.UpstreamStatus.Set ||
		outcome.UpstreamStatus.Value != http.StatusOK {
		t.Fatalf("client-aborted stream lost the received upstream status: %+v", outcome)
	}
}

func TestStreamOutcomeRetainsUpstreamStatusOnDownstreamWriteFailure(t *testing.T) {
	mapping := responsesMapping(t)
	handler, outcomes := outcomeCaptureHandler(t, mapping, func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(string(testcorpus.ChatCompletionsStreamSSE()))),
		}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`))
	req.Header.Set("Accept", "text/event-stream")
	handler.ServeHTTP(&failingResponseWriter{}, req)

	outcome := <-outcomes
	if outcome.Provenance != ProvenanceDownstreamWriteError {
		t.Fatalf("provenance = %s, want downstream_write_error", outcome.Provenance)
	}
	if !outcome.UpstreamAttempted || !outcome.UpstreamStatus.Set ||
		outcome.UpstreamStatus.Value != http.StatusOK {
		t.Fatalf("downstream-write failure lost the received upstream status: %+v", outcome)
	}
}
