package transcode

import (
	"bytes"
	"errors"
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

// TestOperatorLogLineCannotBeForgedByClientText proves a client cannot inject
// a newline and fabricate an operator log line. Error text routinely embeds
// raw client JSON: encoding/json puts the offending KEY into
// UnmarshalTypeError, and a decoded key may contain a newline. The
// client-facing body escapes it (JSON), but the operator log is plain text -
// and the TUI log ring splits writes on "\n", so an unsanitized detail would
// become its own dashboard entry. Every client-influenced byte on the line is
// escaped, and the field is length-bounded so one request cannot flood the log.
func TestOperatorLogLineCannotBeForgedByClientText(t *testing.T) {
	forged := "transcode: POST /v1/responses: [local_request_conversion_error] FORGED"

	for _, tc := range []struct {
		name    string
		detail  string
		wantOne bool
	}{
		{
			name:    "newline in client-supplied json key",
			detail:  "responses request: wire: malformed: json: cannot unmarshal number into Go struct field Request.metadata.a\n" + forged + "\nb of type string",
			wantOne: true,
		},
		{
			name:    "carriage return and other control bytes",
			detail:  "boom\r\r\x00\x1b[31m\x7f " + forged,
			wantOne: true,
		},
		{
			name:    "tab",
			detail:  "a\tb",
			wantOne: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The whole line must stay on ONE physical line.
			if got := logSafeText(tc.detail); strings.ContainsAny(got, "\n\r") {
				t.Fatalf("logSafeText left a line break in %q", got)
			}
			// And the field is bounded, so a huge client-supplied detail
			// cannot flood the log.
			if got := logSafeText(strings.Repeat("A", 100000)); len(got) > 1024 {
				t.Errorf("logSafeText output = %d bytes, want a bound of ~512", len(got))
			}
		})
	}

	// End-to-end through the real logger: a detail carrying the forged line
	// must not produce a second physical line.
	logged := captureLog(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","input":"x"}`))
	h := &TranscodeHandler{}
	h.logRequestError(req, errors.New("convert request: a\n"+forged+"\nb"))
	out := strings.TrimRight(logged.String(), "\n")
	if lines := strings.Split(out, "\n"); len(lines) != 1 {
		t.Fatalf("log has %d physical lines, want exactly 1:\n%s", len(lines), out)
	}
	// The attacker's text may still appear INLINE (it is evidence), but it must
	// no longer be able to START a line: only the real prefix may precede it.
	before, _, ok := strings.Cut(out, forged)
	if ok {
		prefix := before
		if strings.HasPrefix(strings.TrimSpace(prefix), "transcode:") && len(prefix) < 2 {
			t.Errorf("attacker text begins a second operator line:\n%s", out)
		}
		if !strings.HasSuffix(prefix, `\n`) {
			t.Errorf("attacker text is not visibly escaped inline:\n%s", out)
		}
	}
	if !strings.Contains(out, `\n`) {
		t.Errorf("expected the embedded newline to be escaped as \\n in:\n%s", out)
	}
}
