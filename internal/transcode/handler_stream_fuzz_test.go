package transcode_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// FuzzTranscodeHandlerMalformedOrTruncatedStream drives the full streaming
// handler with malformed and truncated upstream SSE. A 200 stream must
// terminate explicitly: truncated or malformed input is represented by a
// client-dialect error event, never a silent clean EOF.
func FuzzTranscodeHandlerMalformedOrTruncatedStream(f *testing.F) {
	f.Add(
		[]byte(`data: {"id":"x","choices":[]}`+"\n\n"),
		uint16(0),
		uint8(1),
	)
	f.Add(
		[]byte("data: not-json\n\ndata: [DONE]\n\n"),
		uint16(8),
		uint8(2),
	)
	f.Add(
		[]byte("event: response.completed\ndata: {"),
		uint16(12),
		uint8(3),
	)

	f.Fuzz(func(
		t *testing.T,
		upstreamBytes []byte,
		cutAt uint16,
		chunkSize uint8,
	) {
		if len(upstreamBytes) > 128<<10 {
			t.Skip()
		}

		cut := min(int(cutAt), len(upstreamBytes))
		wire := upstreamBytes[:cut]

		upstream := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)

				size := int(chunkSize%32) + 1
				for len(wire) != 0 {
					n := min(size, len(wire))
					_, _ = w.Write(wire[:n])
					wire = wire[n:]
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
				}
			},
		))
		defer upstream.Close()

		handler := newStrictResponsesToChatTestHandler(t, upstream.URL)

		ctx, cancel := context.WithTimeout(
			context.Background(),
			2*time.Second,
		)
		defer cancel()

		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			bytes.NewBufferString(`{"model":"m","input":"hello","stream":true}`),
		).WithContext(ctx)
		recorder := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			defer close(done)
			handler.ServeHTTP(recorder, request)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			// The deadline bounds the exchange, but the handler may take a
			// few more milliseconds to abort and return (especially under
			// the race detector with large inputs). Only fail if it does
			// not terminate after a grace period.
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("handler failed to terminate: %v", ctx.Err())
			}
		}

		if recorder.Code != http.StatusOK {
			assertResponsesHTTPErrorShape(
				t,
				recorder.Code,
				recorder.Body.Bytes(),
			)
			return
		}

		// When the exchange was aborted by the client deadline, the
		// interrupted stream legitimately ends without a terminal: the
		// abort itself is the terminal condition (the proxy classifies it
		// as a client abort and emits an error event when it can). Only
		// streams that ended while the client was still waiting must carry
		// an explicit terminal or error event.
		if ctx.Err() != nil {
			return
		}

		events, trailing, err := parseCompleteSSE(
			recorder.Body.Bytes(),
		)
		if err != nil {
			t.Fatalf("downstream emitted malformed SSE: %v", err)
		}
		if len(trailing) != 0 {
			t.Fatalf("downstream emitted trailing partial bytes: %q", trailing)
		}

		var sawTerminal, sawError bool
		for index, event := range events {
			var typeProbe struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(event.Data, &typeProbe); err != nil {
				t.Fatalf(
					"downstream event %d has invalid JSON: %v",
					index,
					err,
				)
			}
			if event.Event != typeProbe.Type {
				t.Fatalf(
					"downstream event %d name/type mismatch: %q/%q",
					index,
					event.Event,
					typeProbe.Type,
				)
			}

			switch typeProbe.Type {
			case "response.completed",
				"response.incomplete",
				"response.failed":
				if sawTerminal || sawError {
					t.Fatalf("duplicate/conflicting terminal at event %d", index)
				}
				sawTerminal = true

			case "error":
				if sawTerminal || sawError {
					t.Fatalf("duplicate/conflicting error at event %d", index)
				}
				sawError = true
			}

			if (sawTerminal || sawError) && index != len(events)-1 {
				t.Fatalf("event emitted after terminal/error")
			}
		}

		// A 200 stream must terminate explicitly. Truncated or malformed input
		// is represented by an error event, never a silent clean EOF.
		if !sawTerminal && !sawError {
			t.Fatal("stream ended without terminal or error event")
		}
	})
}

// newStrictResponsesToChatTestHandler builds a Responses->Chat streaming
// handler that forwards to the given upstream through the default transport.
func newStrictResponsesToChatTestHandler(t *testing.T, upstreamURL string) *transcode.TranscodeHandler {
	t.Helper()
	upstream, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	key, err := transcode.NewRouteKey(http.MethodPost, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	return transcode.NewTranscodeHandler(
		transcode.HandlerConfig{
			Mapping: transcode.Mapping{
				ClientRoute:        key,
				ClientProtocol:     transcode.ClientResponses,
				UpstreamProtocol:   transcode.UpstreamChatCompletions,
				UpstreamPath:       "/v1/chat/completions",
				Auth:               transcode.AuthPolicy{Mode: transcode.AuthNone},
				ModelMap:           transcode.ModelMap{AllowIdentity: true},
				LossPolicy:         transcode.StrictLossPolicy(),
				ChatCapabilities:   transcode.ChatCapabilities{ParallelToolCalls: true, ReasoningEffort: true},
				AllowedClientQuery: map[string]struct{}{},
			},
			Upstream: upstream,
			BodyLimits: transcode.BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
				ErrorResponseBytes:      1 << 20,
				// Mirrors the package parser bounds (maxSSELineBytes,
				// maxSSEFrameBytes).
				SSELineBytes:  1 << 20,
				SSEFrameBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			return http.DefaultTransport.RoundTrip(req)
		},
		nil,
	)
}

// sseEvent is one parsed downstream frame.
type sseEvent struct {
	Event string
	Data  []byte
}

// parseCompleteSSE parses complete SSE frames from a byte buffer. It returns
// the parsed events and any trailing partial bytes (a frame without its
// terminating blank line).
func parseCompleteSSE(data []byte) ([]sseEvent, []byte, error) {
	var events []sseEvent
	i := 0
	for {
		// Find the next blank line (frame boundary).
		rel := bytes.Index(data[i:], []byte("\n\n"))
		if rel < 0 {
			return events, data[i:], nil
		}
		frame := data[i : i+rel]
		i += rel + 2

		var event sseEvent
		for line := range bytes.SplitSeq(frame, []byte("\n")) {
			switch {
			case bytes.HasPrefix(line, []byte("event:")):
				event.Event = strings.TrimSpace(strings.TrimPrefix(string(line), "event:"))
			case bytes.HasPrefix(line, []byte("data:")):
				payload := bytes.TrimPrefix(line, []byte("data:"))
				payload = bytes.TrimPrefix(payload, []byte(" "))
				if len(event.Data) > 0 {
					event.Data = append(event.Data, '\n')
				}
				event.Data = append(event.Data, payload...)
			}
		}
		if event.Data == nil {
			return nil, nil, io.ErrUnexpectedEOF
		}
		events = append(events, event)
	}
}

func assertResponsesHTTPErrorShape(
	t *testing.T,
	status int,
	body []byte,
) {
	t.Helper()
	if status < 400 {
		t.Fatalf("unexpected error status %d", status)
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("invalid Responses error JSON: %v", err)
	}
	if envelope.Error.Message == "" || envelope.Error.Type == "" {
		t.Fatalf("incomplete Responses error: %s", body)
	}
}
