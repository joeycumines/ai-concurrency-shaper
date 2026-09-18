package transcode

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamedResponseLocalFailureIsLogged(t *testing.T) {
	mapping := responsesMapping(t)
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"deep think\"}}]}\n\n" +
					"data: [DONE]\n\n")),
		}, nil
	})

	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","input":"x","stream":true}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("client must receive the dialect error event: %s", body)
	}

	logged := buf.String()
	if got := strings.Count(logged, "[local_response_conversion_error]"); got != 1 {
		t.Fatalf("operator lines marked as a local response failure = %d, want exactly 1: %q", got, logged)
	}
	if !strings.Contains(logged, "transcode: POST /v1/responses: ") {
		t.Fatalf("operator line lacks the method and path attribution: %q", logged)
	}
	if !strings.Contains(logged, "convert stream response: ") {
		t.Fatalf("operator line lacks the failing stage: %q", logged)
	}
	if !strings.Contains(logged, "provider_reasoning_text") {
		t.Fatalf("operator line does not name the rejected feature, so the cause is not visible: %q", logged)
	}
	if strings.Contains(logged, "convert request") {
		t.Fatalf("a response-side failure must not be reported as a request-side one: %q", logged)
	}
}
