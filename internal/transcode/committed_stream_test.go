package transcode

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/circuitbreaker"
)

func TestHandler_CommittedStreamUpstreamHTTPErrorEmitsErrorEvent(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"Retry-After":  []string{"30"},
			},
			Body: io.NopCloser(strings.NewReader(`{"error":{"message":"rate limit exceeded"}}`)),
		}, nil
	})

	const reqBody = `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	ctx, sink := WithOutcomeSink(req.Context())
	ctx = WithCommittedStreamContext(ctx)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	outcome, ok := sink.Load()
	if !ok {
		t.Fatal("no outcome recorded")
	}
	if !outcome.UpstreamFailure {
		t.Fatalf("expected UpstreamFailure=true, got false")
	}
	if !outcome.UpstreamStatus.Set || outcome.UpstreamStatus.Value != http.StatusTooManyRequests {
		t.Fatalf("expected UpstreamStatus=429, got %v", outcome.UpstreamStatus)
	}
	if !outcome.RetryAfter.Set || outcome.RetryAfter.Value <= 0 || outcome.RetryAfter.Value > 30*time.Second {
		t.Fatalf("expected RetryAfter around 30s, got %v", outcome.RetryAfter)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: error\n") {
		t.Fatalf("expected event: error frame in committed stream, got: %q", body)
	}
	if !strings.Contains(body, "rate limit exceeded") {
		t.Fatalf("expected rate limit error message in event frame, got: %q", body)
	}
	if strings.Contains(body, `{"error":{"message":"rate limit exceeded"}}`) {
		t.Fatalf("raw upstream JSON must not be written into committed stream: %q", body)
	}
}

func TestHandler_CommittedStreamUpstreamBodyErrorEmitsErrorEvent(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		_ = pw.CloseWithError(io.ErrUnexpectedEOF)
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body: pr,
		}, nil
	})

	const reqBody = `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	ctx, sink := WithOutcomeSink(req.Context())
	ctx = WithCommittedStreamContext(ctx)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	outcome, ok := sink.Load()
	if !ok {
		t.Fatal("no outcome recorded")
	}
	if !outcome.UpstreamFailure {
		t.Fatalf("expected UpstreamFailure=true, got false")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: error\n") {
		t.Fatalf("expected event: error frame in committed stream, got: %q", body)
	}
}

func TestHandler_CommittedStreamSemanticFailureEmitsErrorEvent(t *testing.T) {
	mapping := messagesMapping(t, UpstreamResponses)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return nil, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx, sink := WithOutcomeSink(req.Context())
	ctx = WithCommittedStreamContext(ctx)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.writeUpstreamSemanticFailure(req, rec, CanonicalAPIError{
		Status:  http.StatusBadGateway,
		Type:    "api_error",
		Code:    "response_conversion_error",
		Message: "convert response: upstream semantic failure",
	}, http.StatusOK)

	outcome, ok := sink.Load()
	if !ok {
		t.Fatal("no outcome recorded")
	}
	if !outcome.UpstreamFailure {
		t.Fatalf("expected UpstreamFailure=true for semantic failure, got false")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: error\n") {
		t.Fatalf("expected event: error frame in committed stream, got: %q", body)
	}
	if !strings.Contains(body, "upstream semantic failure") {
		t.Fatalf("expected semantic error message in event frame, got: %q", body)
	}
}

func TestHandler_CommittedStreamStreamFalseRejected(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	roundTripCalled := false
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		roundTripCalled = true
		return nil, nil
	})

	const reqBody = `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	ctx, sink := WithOutcomeSink(req.Context())
	ctx = WithCommittedStreamContext(ctx)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if roundTripCalled {
		t.Fatal("upstream round trip must not be called when stream:false violates committed stream")
	}

	outcome, ok := sink.Load()
	if !ok {
		t.Fatal("no outcome recorded")
	}
	if !outcome.LocalFailure {
		t.Fatalf("expected LocalFailure=true, got false")
	}
	if outcome.UpstreamAttempted {
		t.Fatalf("expected UpstreamAttempted=false, got true")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: error\n") {
		t.Fatalf("expected dialect event: error frame, got: %q", body)
	}
	if !strings.Contains(body, "stream:false is incompatible with committed event-stream representation") {
		t.Fatalf("expected incompatible stream error message, got: %q", body)
	}
}

func TestHandler_ErrCircuitOpenRenders503(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return nil, circuitbreaker.ErrCircuitOpen
	})

	t.Run("non-committed stream renders 503 JSON", func(t *testing.T) {
		const reqBody = `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")

		ctx, sink := WithOutcomeSink(req.Context())
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		outcome, ok := sink.Load()
		if !ok {
			t.Fatal("no outcome recorded")
		}
		if !outcome.CircuitRejected {
			t.Fatalf("expected CircuitRejected=true, got false")
		}
		if !outcome.LocalFailure {
			t.Fatalf("expected LocalFailure=true, got false")
		}
		if outcome.UpstreamFailure {
			t.Fatalf("expected UpstreamFailure=false, got true")
		}
		if outcome.UpstreamAttempted {
			t.Fatalf("expected UpstreamAttempted=false, got true")
		}
		body := rec.Body.String()
		if !strings.Contains(body, "circuit open") {
			t.Fatalf("expected 'circuit open' in body, got: %q", body)
		}
		if !strings.Contains(body, `"type":"api_error"`) {
			t.Fatalf("expected Anthropic api_error, got: %q", body)
		}
	})

	t.Run("committed stream renders event error frame", func(t *testing.T) {
		const reqBody = `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":true}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")

		ctx, sink := WithOutcomeSink(req.Context())
		ctx = WithCommittedStreamContext(ctx)
		req = req.WithContext(ctx)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		outcome, ok := sink.Load()
		if !ok {
			t.Fatal("no outcome recorded")
		}
		if !outcome.CircuitRejected {
			t.Fatalf("expected CircuitRejected=true, got false")
		}
		body := rec.Body.String()
		if !strings.Contains(body, "event: error\n") {
			t.Fatalf("expected event: error frame in committed stream, got: %q", body)
		}
		if !strings.Contains(body, "circuit open") {
			t.Fatalf("expected 'circuit open' in error event, got: %q", body)
		}
	})
}
