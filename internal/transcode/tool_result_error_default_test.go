package transcode

// Live compatibility failure (2026-09-07): Claude Code marks failed tool
// results with is_error:true, and the CLI default policy rejected the whole
// request. The default profile approves tool_result_error_status, so the
// permissive encoding ([tool_result_error] prefix + observable loss entry)
// applies out of the box. The strict programmatic policy still rejects.

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cliDefaultStylePolicy replicates the CLI default loss profile
// (internal/config defaultTranscodeLosses). The config package imports
// transcode, so the transcode tests cannot import config; keep in sync with
// the config-layer test.
func cliDefaultStylePolicy() LossPolicy {
	return LossPolicy{Allowed: map[Feature]struct{}{
		FeatureReasoningSummary:       {},
		FeatureAuthenticatedThinking:  {},
		FeatureMidConversationSystem:  {},
		FeatureResponsesControls:      {},
		FeatureAnthropicControls:      {},
		FeatureBuiltinTools:           {},
		FeatureUsageUnknown:           {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureRequestReasoning:       {},
		FeatureToolResultErrorStatus:  {},
	}}
}

func newIsErrorTestHandler(t *testing.T, policy LossPolicy, upstream func(*http.Request) (*http.Response, error)) *TranscodeHandler {
	t.Helper()
	key, err := NewRouteKey(http.MethodPost, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	mapping := Mapping{
		ClientRoute:      key,
		ClientProtocol:   ClientMessages,
		UpstreamProtocol: UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		Auth:             AuthPolicy{Mode: AuthNone},
		ModelMap:         ModelMap{AllowIdentity: true},
		LossPolicy:       policy,
	}
	return NewTranscodeHandler(
		HandlerConfig{Mapping: mapping, Upstream: mustParseURL(t, "https://upstream.example")},
		upstream,
		nil,
	)
}

func chatOKResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		)),
	}
}

func TestIsErrorToolResultConvertsUnderDefaultProfile(t *testing.T) {
	handler := newIsErrorTestHandler(t, cliDefaultStylePolicy(), func(req *http.Request) (*http.Response, error) {
		return chatOKResponse(), nil
	})
	var logs logBuffer
	prev := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prev)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"m","max_tokens":100,
		"messages":[
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[{"type":"text","text":"exit code 1"}]}]}
		]
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q — the CLI default profile must convert is_error tool results (Claude Code failure)", rec.Code, rec.Body.String())
	}
	// The decision is OBSERVABLE: exactly one request-stage loss line naming
	// the feature (REM-M acceptance).
	joined := logs.String()
	if !strings.Contains(joined, "tool_result_error_status at messages[].tool_result.is_error") {
		t.Fatalf("loss log lacks the tool_result_error_status entry: %q", joined)
	}
	if got := strings.Count(joined, "tool_result_error_status at messages[].tool_result.is_error"); got != 1 {
		t.Fatalf("tool_result_error_status loss line appears %d times, want exactly 1", got)
	}
}

// logBuffer collects log output.
type logBuffer struct {
	b strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) { return l.b.Write(p) }
func (l *logBuffer) String() string              { return l.b.String() }

func TestIsErrorToolResultStrictPolicyStillRejects(t *testing.T) {
	handler := newIsErrorTestHandler(t, StrictLossPolicy(), func(req *http.Request) (*http.Response, error) {
		t.Fatal("round trip must not be reached under strict policy")
		return nil, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"m","max_tokens":100,
		"messages":[
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[{"type":"text","text":"exit code 1"}]}]}
		]
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 under strict policy", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "tool_result_error_status") {
		t.Fatalf("body = %q, want the tool_result_error_status rejection", rec.Body.String())
	}
}

func TestNoErrorToolResultProducesNoRejection(t *testing.T) {
	for name, body := range map[string]string{
		"absent is_error": `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"ok"}]}]}]}`,
		"false is_error":  `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":[{"type":"text","text":"ok"}]}]}]}`,
	} {
		handler := newIsErrorTestHandler(t, cliDefaultStylePolicy(), func(req *http.Request) (*http.Response, error) {
			return chatOKResponse(), nil
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d body=%q", name, rec.Code, rec.Body.String())
		}
	}
}

// TestIsErrorPrefixRenderedInChatMessage pins the permissive encoding: the
// visible [tool_result_error] prefix precedes the result content in the
// rendered chat tool message.
func TestIsErrorPrefixRenderedInChatMessage(t *testing.T) {
	var captured string
	handler := newIsErrorTestHandler(t, cliDefaultStylePolicy(), func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		captured = string(body)
		return chatOKResponse(), nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"m","max_tokens":100,
		"messages":[
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[{"type":"text","text":"exit code 1"}]}]}
		]
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(captured, "[tool_result_error]") {
		t.Fatalf("rendered chat request lacks the error_status_prefix encoding: %s", captured)
	}
	if !strings.Contains(captured, "exit code 1") {
		t.Fatalf("rendered chat request lost the tool result content: %s", captured)
	}
}
