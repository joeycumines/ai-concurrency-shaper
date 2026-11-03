package transcode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGoogleStatusForHTTP(t *testing.T) {
	tests := map[int]string{
		http.StatusBadRequest:              "INVALID_ARGUMENT",
		http.StatusUnauthorized:            "UNAUTHENTICATED",
		http.StatusForbidden:               "PERMISSION_DENIED",
		http.StatusNotFound:                "NOT_FOUND",
		http.StatusRequestTimeout:          "DEADLINE_EXCEEDED",
		http.StatusConflict:                "ABORTED",
		http.StatusRequestEntityTooLarge:   "INVALID_ARGUMENT",
		http.StatusUnsupportedMediaType:    "INVALID_ARGUMENT",
		http.StatusTooManyRequests:         "RESOURCE_EXHAUSTED",
		499:                                "CANCELLED",
		http.StatusUnprocessableEntity:     "FAILED_PRECONDITION",
		http.StatusInternalServerError:     "INTERNAL",
		http.StatusNotImplemented:          "UNIMPLEMENTED",
		http.StatusBadGateway:              "UNAVAILABLE",
		http.StatusServiceUnavailable:      "UNAVAILABLE",
		http.StatusGatewayTimeout:          "DEADLINE_EXCEEDED",
		http.StatusHTTPVersionNotSupported: "INTERNAL",
	}
	for status, want := range tests {
		if got := googleStatusForHTTP(status); got != want {
			t.Errorf("googleStatusForHTTP(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestGeminiClientErrorStatus(t *testing.T) {
	tests := []struct {
		name string
		err  CanonicalAPIError
		want string
	}{
		{
			name: "http status decides when the code is not a google status",
			err:  CanonicalAPIError{Status: http.StatusTooManyRequests, Message: "slow down", Code: "rate_limit_exceeded"},
			want: `{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED"}}`,
		},
		{
			name: "a google status string from upstream is passed through",
			err:  CanonicalAPIError{Status: http.StatusInternalServerError, Message: "busy", Code: "UNAVAILABLE"},
			want: `{"error":{"code":500,"message":"busy","status":"UNAVAILABLE"}}`,
		},
		{
			name: "a lower-case code is not a google status",
			err:  CanonicalAPIError{Status: http.StatusNotFound, Message: "gone", Code: "not_found"},
			want: `{"error":{"code":404,"message":"gone","status":"NOT_FOUND"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(geminiClientError(normalizeCanonicalError(tt.err)))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("envelope = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestReadCanonicalUpstreamErrorGemini(t *testing.T) {
	t.Run("envelope message and status are parsed", func(t *testing.T) {
		header := http.Header{}
		header.Set("Retry-After", "7")
		header.Set("x-request-id", "req-1")
		resp := responseWithBody(http.StatusTooManyRequests, "application/json",
			`{"error":{"code":429,"message":"quota exceeded","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"7s"}]}}`,
			header)
		got, err := ReadCanonicalUpstreamError(resp, UpstreamGemini, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != http.StatusTooManyRequests {
			t.Errorf("Status = %d, want 429", got.Status)
		}
		if got.Message != "quota exceeded" {
			t.Errorf("Message = %q, want the envelope message", got.Message)
		}
		if got.Code != "RESOURCE_EXHAUSTED" {
			t.Errorf("Code = %q, want the envelope status", got.Code)
		}
		if got.Type != "rate_limit_error" {
			t.Errorf("Type = %q, want rate_limit_error", got.Type)
		}
		if got.RetryAfter != "7" || got.RequestID != "req-1" {
			t.Errorf("RetryAfter = %q, RequestID = %q; headers must carry through", got.RetryAfter, got.RequestID)
		}
		if strings.Contains(got.Message, "RetryInfo") {
			t.Errorf("details leaked into the message: %q", got.Message)
		}
	})

	t.Run("html body falls back to bounded text without a status code", func(t *testing.T) {
		body := "<html><body>gateway down</body></html>"
		resp := responseWithBody(http.StatusServiceUnavailable, "text/html", body, http.Header{})
		got, err := ReadCanonicalUpstreamError(resp, UpstreamGemini, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.Message, "<html>") {
			t.Errorf("Message = %q, want the sanitized body text", got.Message)
		}
		if got.Code != codeForStatus(http.StatusServiceUnavailable) {
			t.Errorf("Code = %q, want the status-derived code: the fallback must not invent a google status", got.Code)
		}
	})

	t.Run("envelope without an error object falls back", func(t *testing.T) {
		resp := responseWithBody(http.StatusBadRequest, "application/json", `{"status":"INVALID_ARGUMENT"}`, http.Header{})
		got, err := ReadCanonicalUpstreamError(resp, UpstreamGemini, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != codeForStatus(http.StatusBadRequest) {
			t.Errorf("Code = %q, want the status-derived code", got.Code)
		}
	})
}

func TestReadCanonicalUpstreamErrorGeminiEmptyMessageNeverCopiesDetails(t *testing.T) {
	body := `{"error":{"code":400,"message":"","status":"INVALID_ARGUMENT","details":[{"@type":"x","reason":"SECRETDETAIL"}]}}`
	resp := responseWithBody(http.StatusBadRequest, "application/json", body, http.Header{})
	got, err := ReadCanonicalUpstreamError(resp, UpstreamGemini, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Message, "SECRETDETAIL") || strings.Contains(got.Message, "details") {
		t.Fatalf("Message = %q copied the upstream details", got.Message)
	}
	if got.Message != "INVALID_ARGUMENT" {
		t.Fatalf("Message = %q, want the google status when the message is empty", got.Message)
	}

	resp = responseWithBody(http.StatusServiceUnavailable, "application/json",
		`{"error":{"code":503,"message":"","status":"x-unknown-status"}}`, http.Header{})
	got, err = ReadCanonicalUpstreamError(resp, UpstreamGemini, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Message != http.StatusText(http.StatusServiceUnavailable) {
		t.Fatalf("Message = %q, want the HTTP status text for an unrecognized status", got.Message)
	}
}

func TestWriteDialectHTTPErrorGemini(t *testing.T) {
	rec := httptest.NewRecorder()
	apiErr := CanonicalAPIError{Status: http.StatusBadRequest, Message: "unsupported field", RetryAfter: ""}
	if err := WriteDialectHTTPError(rec, ClientGemini, apiErr); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	want := `{"error":{"code":400,"message":"unsupported field","status":"INVALID_ARGUMENT"}}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(want)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(want))
	}
}

func TestWriteDialectHTTPErrorGeminiRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	apiErr := CanonicalAPIError{Status: http.StatusTooManyRequests, Message: "slow", RetryAfter: "3"}
	if err := WriteDialectHTTPError(rec, ClientGemini, apiErr); err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want 3", got)
	}
}
