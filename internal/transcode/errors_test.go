package transcode

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func responseWithBody(status int, contentType string, body string, header http.Header) *http.Response {
	resp := &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
	if contentType != "" {
		resp.Header.Set("Content-Type", contentType)
	}
	for name, values := range header {
		for _, value := range values {
			resp.Header.Add(name, value)
		}
	}
	return resp
}

func TestReadCanonicalUpstreamErrorOpenAI(t *testing.T) {
	resp := responseWithBody(429, "application/json", `{
		"error": {"message":"Rate limited","type":"rate_limit_error","param":null,"code":"rate_limit_exceeded"}
	}`, http.Header{
		"X-Request-Id": []string{"req_123"},
		"Retry-After":  []string{"5"},
	})
	apiErr, err := ReadCanonicalUpstreamError(resp, UpstreamResponses, 0)
	if err != nil {
		t.Fatal(err)
	}
	if apiErr.Status != 429 {
		t.Fatalf("status = %d", apiErr.Status)
	}
	if apiErr.Message != "Rate limited" || apiErr.Type != "rate_limit_error" || apiErr.Code != "rate_limit_exceeded" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if apiErr.RequestID != "req_123" || apiErr.RetryAfter != "5" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
}

func TestReadCanonicalUpstreamErrorAnthropic(t *testing.T) {
	resp := responseWithBody(529, "application/json", `{
		"type":"error",
		"error":{"type":"overloaded_error","message":"Overloaded"},
		"request_id":"req_xyz"
	}`, http.Header{})
	apiErr, err := ReadCanonicalUpstreamError(resp, UpstreamMessages, 0)
	if err != nil {
		t.Fatal(err)
	}
	if apiErr.Status != 529 {
		t.Fatalf("status = %d", apiErr.Status)
	}
	if apiErr.Message != "Overloaded" || apiErr.Type != "overloaded_error" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if apiErr.RequestID != "req_xyz" {
		t.Fatalf("request id = %q", apiErr.RequestID)
	}
}

func TestReadCanonicalUpstreamErrorHTMLSanitized(t *testing.T) {
	// Raw provider HTML must not be forwarded verbatim; the message is
	// whitespace-normalized and bounded.
	resp := responseWithBody(502, "text/html", "<html><body>\n  Bad Gateway with a very long message\n</body></html>", http.Header{})
	apiErr, err := ReadCanonicalUpstreamError(resp, UpstreamResponses, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(apiErr.Message, "\n") {
		t.Fatalf("raw HTML whitespace leaked: %q", apiErr.Message)
	}
	if apiErr.Type == "" || apiErr.Code == "" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
}

func TestReadCanonicalUpstreamErrorEmptyBody(t *testing.T) {
	resp := responseWithBody(500, "", "", http.Header{})
	apiErr, err := ReadCanonicalUpstreamError(resp, UpstreamMessages, 0)
	if err != nil {
		t.Fatal(err)
	}
	if apiErr.Message == "" {
		t.Fatal("empty message")
	}
}

func TestReadCanonicalUpstreamErrorBounded(t *testing.T) {
	// An oversized error body is rejected rather than parsed.
	huge := strings.Repeat("x", int(maxUpstreamErrorBodyBytes)+1024)
	resp := responseWithBody(500, "text/plain", huge, http.Header{})
	if _, err := ReadCanonicalUpstreamError(resp, UpstreamResponses, 0); err == nil {
		t.Fatal("oversized error body accepted")
	}

	// A custom limit is honored.
	resp2 := responseWithBody(500, "text/plain", strings.Repeat("x", 4096), http.Header{})
	if _, err := ReadCanonicalUpstreamError(resp2, UpstreamResponses, 1024); err == nil {
		t.Fatal("error body exceeding the configured limit accepted")
	}

	// Within the limit, the parse succeeds.
	apiErr, err := ReadCanonicalUpstreamError(resp, UpstreamResponses, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(apiErr.Message) > int(maxUpstreamErrorBodyBytes) {
		t.Fatalf("message not bounded: %d", len(apiErr.Message))
	}
}

func TestWriteDialectHTTPErrorResponses(t *testing.T) {
	rec := httptest.NewRecorder()
	apiErr := CanonicalAPIError{
		Status:    429,
		Type:      "rate_limit_error",
		Code:      "rate_limit_exceeded",
		Message:   "slow down",
		Param:     "model",
		RequestID: "req_1",
	}
	if err := WriteDialectHTTPError(rec, ClientResponses, apiErr); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 429 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("X-Request-Id") != "req_1" {
		t.Fatalf("request id = %q", rec.Header().Get("X-Request-Id"))
	}
	var body struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Param   *string `json:"param"`
			Code    string  `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Message != "slow down" || body.Error.Type != "rate_limit_error" ||
		body.Error.Code != "rate_limit_exceeded" || body.Error.Param == nil || *body.Error.Param != "model" {
		t.Fatalf("body = %+v", body)
	}
}

func TestWriteDialectHTTPErrorMessages(t *testing.T) {
	rec := httptest.NewRecorder()
	apiErr := CanonicalAPIError{
		Status:    413,
		Message:   "too big",
		RequestID: "req_2",
	}
	if err := WriteDialectHTTPError(rec, ClientMessages, apiErr); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 413 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Request-Id") != "req_2" {
		t.Fatalf("request id = %q", rec.Header().Get("Request-Id"))
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "error" || body.Error.Type != "request_too_large" || body.Error.Message != "too big" {
		t.Fatalf("body = %+v", body)
	}
}

func TestWriteDialectHTTPErrorPreservesRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	apiErr := CanonicalAPIError{
		Status:     429,
		Message:    "slow",
		RetryAfter: "30",
	}
	if err := WriteDialectHTTPError(rec, ClientResponses, apiErr); err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q", got)
	}
}

func TestSanitizeErrorText(t *testing.T) {
	if got := sanitizeErrorText([]byte("  hello\n  world  ")); got != "hello world" {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeErrorText([]byte("")); got == "" {
		t.Fatal("empty input must produce a message")
	}
	long := strings.Repeat("a", 5000)
	got := sanitizeErrorText([]byte(long))
	if len([]rune(got)) > 2049 { // 2048 chars + ellipsis
		t.Fatalf("not bounded: %d runes", len([]rune(got)))
	}
}

func TestRawScalarString(t *testing.T) {
	if got := rawScalarString(json.RawMessage(`"hello"`)); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := rawScalarString(json.RawMessage(`42`)); got != "42" {
		t.Fatalf("got %q", got)
	}
	if got := rawScalarString(json.RawMessage(`null`)); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := rawScalarString(json.RawMessage(``)); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestTypeForStatus(t *testing.T) {
	tests := map[int]string{
		400: "invalid_request_error",
		401: "authentication_error",
		403: "permission_error",
		429: "rate_limit_error",
		529: "overloaded_error",
		500: "api_error",
		502: "api_error",
	}
	for status, want := range tests {
		if got := typeForStatus(status); got != want {
			t.Errorf("typeForStatus(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestCodeForStatus(t *testing.T) {
	if got := codeForStatus(413); got != "request_too_large" {
		t.Fatalf("got %q", got)
	}
	if got := codeForStatus(429); got != "rate_limit_exceeded" {
		t.Fatalf("got %q", got)
	}
	if got := codeForStatus(529); got != "overloaded" {
		t.Fatalf("got %q", got)
	}
	if got := codeForStatus(500); got != "internal_server_error" {
		t.Fatalf("got %q", got)
	}
}
