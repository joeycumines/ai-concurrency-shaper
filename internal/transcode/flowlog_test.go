package transcode

// The -transcode-flowlog-dir diagnostic recorder is inert when unset and
// writes exactly one JSON record per transcoded exchange when set. These
// tests also pin the recorder's writer-wrapping contract: flush must keep
// flowing to the wrapped writer through http.ResponseController even when
// that writer implements FlushError + Unwrap rather than http.Flusher (the
// proxy's statusRecorder shape), so enabling the recorder can never degrade
// live streaming delivery or flush-failure accounting.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// flushRecorder mimics the proxy's statusRecorder: FlushError + Unwrap, no
// http.Flusher.
type flushRecorder struct {
	http.ResponseWriter
	flushes int
}

func (f *flushRecorder) Unwrap() http.ResponseWriter { return f.ResponseWriter }

func (f *flushRecorder) FlushError() error {
	f.flushes++
	return http.NewResponseController(f.ResponseWriter).Flush()
}

// flushCountingWriter counts Flush calls on the base writer.
type flushCountingWriter struct {
	http.ResponseWriter
	flushes int
}

func (f *flushCountingWriter) Flush() { f.flushes++ }

func TestFlowRecorderFlushReachesWrappedWriter(t *testing.T) {
	fl := &flowCapture{dir: t.TempDir()}
	base := &flushCountingWriter{ResponseWriter: httptest.NewRecorder()}
	wrapped := fl.wrapWriter(&flushRecorder{ResponseWriter: base})

	if err := http.NewResponseController(wrapped).Flush(); err != nil {
		t.Fatalf("flush through the recorder tee failed: %v", err)
	}
	if base.flushes != 1 {
		t.Fatalf("base writer flushes = %d, want 1 (the recorder tee swallowed the flush)", base.flushes)
	}
	// The direct http.Flusher arm must reach the same delegate: callers that
	// type-assert Flusher bypass ResponseController entirely.
	flusher, ok := wrapped.(http.Flusher)
	if !ok {
		t.Fatal("recorder tee must keep exposing http.Flusher")
	}
	flusher.Flush()
	if base.flushes != 2 {
		t.Fatalf("base writer flushes = %d after the Flusher arm, want 2", base.flushes)
	}
}

// The record filename must never escape the capture directory: both
// platform separators and other filesystem-significant characters in the
// client-controlled method/path become '_'.
func TestFlowRecorderSlugSanitized(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"/v1/messages", "v1_messages"},
		{`/v1/../../evil`, "v1_.._.._evil"},
		{`C:\windows\evil`, "C__windows_evil"},
		{"/", "root"},
		{"", "root"},
		{"..", "root"},
	} {
		if got := flowSlug(tc.in); got != tc.want {
			t.Errorf("flowSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// A sanitized path cannot traverse: exactly one record lands inside the
	// capture directory, and nothing lands in its parent.
	dir := t.TempDir()
	fl := &flowCapture{dir: dir, seq: 1, start: time.Now()}
	fl.path = `/v1/../../escape`
	fl.method = "POST"
	fl.seal()
	inside, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(inside) != 1 {
		t.Fatalf("records inside the capture directory = %v (err %v), want exactly 1", inside, err)
	}
	if name := filepath.Base(inside[0]); strings.ContainsAny(name, `/\`) {
		t.Fatalf("record name contains a separator: %q", name)
	}
	outside, err := filepath.Glob(filepath.Join(filepath.Dir(dir), "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range outside {
		if filepath.Dir(path) != filepath.Dir(dir) {
			continue
		}
		if _, err := os.Stat(path); err == nil && filepath.Base(path) == filepath.Base(inside[0]) {
			t.Fatalf("record also appeared outside the capture directory: %s", path)
		}
	}
}

// A streaming request answered with an upstream JSON error never reaches the
// stream classification: the record must not claim stream_outcome=success.
func TestFlowRecorderStreamOutcomeRequiresClassification(t *testing.T) {
	dir := t.TempDir()
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{}
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
			FlowLogDir: dir,
		},
		func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"boom","type":"server_error"}}`)),
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("record files = %v (err %v), want exactly 1", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Outcome map[string]any `json:"outcome"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if got, ok := record.Outcome["stream_outcome"]; ok {
		t.Fatalf("unclassified streaming failure claimed stream_outcome=%v", got)
	}
	if got, _ := record.Outcome["upstream_failure"].(bool); !got {
		t.Fatalf("upstream 500 not classified as an upstream failure: %v", record.Outcome)
	}
}

// Long client paths must stay well under any platform NAME_MAX while
// remaining unique.
func TestFlowRecorderPathSlugBounded(t *testing.T) {
	long := "/v1/" + strings.Repeat("segment/", 60) + "leaf"
	slug := flowPathSlug(long)
	if len(slug) > 96 {
		t.Fatalf("slug length %d exceeds the bound: %q", len(slug), slug)
	}
	other := flowPathSlug("/v1/" + strings.Repeat("segment/", 60) + "leaf2")
	if slug == other {
		t.Fatal("distinct long paths collapsed onto one slug")
	}
	// A long multi-byte path must not be split mid-rune.
	multibyte := "/v1/" + strings.Repeat("セグメント/", 30) + "終"
	mbSlug := flowPathSlug(multibyte)
	if len(mbSlug) > 128 {
		t.Fatalf("multibyte slug length %d exceeds the bound", len(mbSlug))
	}
	if !utf8.ValidString(mbSlug) {
		t.Fatalf("multibyte slug is not valid UTF-8: %q", mbSlug)
	}
}

// cappedBuffer must never fail its writer (io.TeeReader depends on it),
// must flag truncation at the cap, and must report the source total; the
// JSON body must carry base64 for non-UTF-8 bytes.
func TestFlowRecorderBodyEncodingAndCap(t *testing.T) {
	buf := &cappedBuffer{cap: 8}
	payload := []byte("0123456789abcdef")
	if n, err := buf.Write(payload); n != len(payload) || err != nil {
		t.Fatalf("cappedBuffer.Write = (%d, %v), want (%d, nil)", n, err, len(payload))
	}
	body := buf.snapshot()
	if !body.Truncated {
		t.Fatal("over-cap write did not flag truncation")
	}
	if body.Bytes != len(payload) {
		t.Fatalf("body.Bytes = %d, want the source total %d", body.Bytes, len(payload))
	}
	if body.Text != "01234567" {
		t.Fatalf("retained prefix = %q", body.Text)
	}
	if n, err := buf.Write(nil); n != 0 || err != nil {
		t.Fatalf("zero-length write = (%d, %v)", n, err)
	}

	binary := newFlowBody([]byte{0xff, 0xfe, 0x00}, false)
	if binary.Base64 == "" || binary.Text != "" {
		t.Fatalf("non-UTF-8 body not base64-encoded: %+v", binary)
	}
	if binary.Bytes != 3 {
		t.Fatalf("binary body bytes = %d", binary.Bytes)
	}
}

// A non-streaming exchange is captured too (including the upstream error
// body), and the recorded outcome must not claim a stream classification.
func TestFlowRecorderNonStreamingUpstreamErrorCaptured(t *testing.T) {
	dir := t.TempDir()
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{}
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
			FlowLogDir: dir,
		},
		func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"upstream exploded","type":"server_error"}}`)),
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("record files = %v (err %v), want exactly 1", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		StreamIntent     bool           `json:"stream_intent"`
		UpstreamRespBody map[string]any `json:"upstream_resp_body"`
		DownstreamBody   map[string]any `json:"downstream_body"`
		Outcome          map[string]any `json:"outcome"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.StreamIntent {
		t.Fatal("non-streaming request recorded as streaming")
	}
	if text, _ := record.UpstreamRespBody["text"].(string); !strings.Contains(text, "upstream exploded") {
		t.Fatalf("upstream error body not captured verbatim: %v", record.UpstreamRespBody)
	}
	if text, _ := record.DownstreamBody["text"].(string); !strings.Contains(text, "upstream exploded") {
		t.Fatalf("downstream error body not captured: %v", record.DownstreamBody)
	}
	if _, ok := record.Outcome["stream_outcome"]; ok {
		t.Fatalf("non-streaming exchange claimed a stream outcome: %v", record.Outcome)
	}
}

// The recorded downstream body must contain only the bytes the wrapped
// writer accepted, and a failed write must not be hidden.
func TestFlowRecorderWriteAccounting(t *testing.T) {
	fl := &flowCapture{dir: t.TempDir()}
	short := &shortWriter{limit: 3, header: http.Header{}}
	wrapped := fl.wrapWriter(short)
	if _, err := wrapped.Write([]byte("abcdef")); err == nil {
		t.Fatal("short write did not surface an error")
	}
	status, _, body := fl.downTee.snapshot()
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if body.Text != "abc" || body.Bytes != 3 {
		t.Fatalf("recorded downstream body = %q (%d bytes), want the accepted 3 bytes", body.Text, body.Bytes)
	}
}

// shortWriter accepts at most limit bytes and then fails, mirroring a
// downstream connection that died mid-write.
type shortWriter struct {
	limit  int
	used   int
	header http.Header
}

func (w *shortWriter) Header() http.Header { return w.header }

func (w *shortWriter) WriteHeader(int) {}

func (w *shortWriter) Write(p []byte) (int, error) {
	room := w.limit - w.used
	if room <= 0 {
		return 0, io.ErrShortWrite
	}
	if len(p) > room {
		w.used += room
		return room, io.ErrShortWrite
	}
	w.used += len(p)
	return len(p), nil
}

// Flush failure must reach the caller through the recorder tee.
func TestFlowRecorderFlushFailurePropagates(t *testing.T) {
	fl := &flowCapture{dir: t.TempDir()}
	wrapped := fl.wrapWriter(&failingFlushRecorder{header: http.Header{}})
	if err := http.NewResponseController(wrapped).Flush(); err == nil {
		t.Fatal("flush failure was swallowed by the recorder tee")
	}
}

type failingFlushRecorder struct {
	header http.Header
}

func (w *failingFlushRecorder) Header() http.Header { return w.header }

func (w *failingFlushRecorder) WriteHeader(int) {}

func (w *failingFlushRecorder) Write(p []byte) (int, error) { return len(p), nil }

func (w *failingFlushRecorder) FlushError() error { return io.ErrUnexpectedEOF }

// A taken record name gains a numeric suffix and never replaces the
// existing file; the suffix keeps the .json extension.
func TestFlowRecorderNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	writeFlowRecord(dir, "000001-1-POST-v1_messages.json", []byte("first"))
	writeFlowRecord(dir, "000001-1-POST-v1_messages.json", []byte("second"))
	first, err := os.ReadFile(filepath.Join(dir, "000001-1-POST-v1_messages.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "first" {
		t.Fatalf("existing record was replaced: %q", first)
	}
	second, err := os.ReadFile(filepath.Join(dir, "000001-1-POST-v1_messages.1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != "second" {
		t.Fatalf("retry record = %q", second)
	}
}

func TestFlowRecorderDisabledIsInert(t *testing.T) {
	if fl := newFlowCapture(""); fl != nil {
		t.Fatal("capture started with the tap disabled")
	}
	if w := (*flowCapture)(nil).wrapWriter(httptest.NewRecorder()); w == nil {
		t.Fatal("nil capture must pass the writer through")
	}
}

// A committed-stream exchange records the committed flag.
func TestFlowRecorderCommittedStreamFlag(t *testing.T) {
	dir := t.TempDir()
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{}
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
			FlowLogDir: dir,
		},
		func(req *http.Request) (*http.Response, error) {
			t.Error("upstream must not be reached for a stream:false committed exchange")
			return nil, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":false}`),
	)
	ctx := WithCommittedStreamContext(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("record files = %v (err %v), want exactly 1", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		CommittedStream bool `json:"committed_stream"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if !record.CommittedStream {
		t.Fatal("committed-stream exchange did not record the committed flag")
	}
}

func TestFlowRecorderWritesFullExchangeJSON(t *testing.T) {
	dir := t.TempDir()

	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{}
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	sse := "data: {\"id\":\"chatcmpl-f\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"42\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-f\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"chatcmpl-f\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
			FlowLogDir: dir,
		},
		func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			if !strings.Contains(string(body), `"model":"m"`) {
				t.Errorf("upstream body = %s", body)
			}
			if req.Header.Get("Authorization") != "" {
				t.Errorf("unexpected auth header %q", req.Header.Get("Authorization"))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	)
	req.Header.Set("Authorization", "Bearer junk-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("record files = %v (err %v), want exactly 1", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		ClientMethod  string              `json:"client_method"`
		ClientPath    string              `json:"client_path"`
		ClientQuery   string              `json:"client_query"`
		ClientHeaders map[string][]string `json:"client_headers"`
		ClientBody    struct {
			Text string `json:"text"`
		} `json:"client_body"`
		UpstreamMethod string `json:"upstream_method"`
		UpstreamURL    string `json:"upstream_url"`
		UpstreamBody   struct {
			Text string `json:"text"`
		} `json:"upstream_body"`
		UpstreamStatus   int `json:"upstream_status"`
		UpstreamRespBody struct {
			Text string `json:"text"`
		} `json:"upstream_resp_body"`
		DownstreamStatus int `json:"downstream_status"`
		DownstreamBody   struct {
			Text string `json:"text"`
		} `json:"downstream_body"`
		RequestReport  []map[string]any `json:"request_report"`
		ResponseReport []map[string]any `json:"response_report"`
		Outcome        map[string]any   `json:"outcome"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("record is not valid JSON: %v", err)
	}
	if record.ClientMethod != http.MethodPost || record.ClientPath != "/v1/messages" {
		t.Fatalf("client identity = %s %s", record.ClientMethod, record.ClientPath)
	}
	if !strings.Contains(record.ClientBody.Text, `"messages"`) {
		t.Fatalf("client body not captured verbatim: %q", record.ClientBody.Text)
	}
	// NO REDACTION (operator directive): the inbound credential is recorded.
	if got := record.ClientHeaders["Authorization"]; len(got) != 1 || got[0] != "Bearer junk-token" {
		t.Fatalf("authorization header not captured verbatim: %v", record.ClientHeaders)
	}
	if record.UpstreamMethod != http.MethodPost || !strings.Contains(record.UpstreamURL, "/v1/chat-completions") {
		t.Fatalf("upstream target = %s %s", record.UpstreamMethod, record.UpstreamURL)
	}
	if !strings.Contains(record.UpstreamBody.Text, `"stream":true`) {
		t.Fatalf("converted upstream body not captured: %q", record.UpstreamBody.Text)
	}
	if record.UpstreamStatus != http.StatusOK {
		t.Fatalf("upstream status = %d", record.UpstreamStatus)
	}
	if !strings.Contains(record.UpstreamRespBody.Text, `"usage"`) {
		t.Fatalf("raw upstream response body not captured: %q", record.UpstreamRespBody.Text)
	}
	if record.DownstreamStatus != http.StatusOK {
		t.Fatalf("downstream status = %d", record.DownstreamStatus)
	}
	if record.DownstreamBody.Text != rec.Body.String() {
		t.Fatalf("downstream body mismatch:\nrecorded %q\nactual   %q", record.DownstreamBody.Text, rec.Body.String())
	}
	if len(record.ResponseReport) == 0 {
		t.Fatal("response conversion report not captured")
	}
	if record.Outcome == nil {
		t.Fatal("outcome not captured")
	}
	// The outcome is rendered for reading: enum fields are their names.
	if got, _ := record.Outcome["provenance"].(string); got != "upstream_http" {
		t.Fatalf("outcome provenance = %v, want upstream_http", record.Outcome["provenance"])
	}
	if got, _ := record.Outcome["stream_outcome"].(string); got != "success" {
		t.Fatalf("outcome stream_outcome = %v, want success", record.Outcome["stream_outcome"])
	}
	// Only an exchange that actually ran the stream classification may claim
	// a stream outcome: the zero value would read as a false "success".
	unclassified := flowOutcomeOf(Outcome{}, false)
	if unclassified.StreamOutcome != "" {
		t.Fatalf("unclassified outcome reported stream_outcome=%q", unclassified.StreamOutcome)
	}
	if classified := flowOutcomeOf(Outcome{}, true); classified.StreamOutcome != "success" {
		t.Fatalf("classified outcome stream_outcome = %q", classified.StreamOutcome)
	}
}

// An exchange rejected before its body is read (413) still records the
// client identity: the filename and record must identify the request.
func TestFlowRecorderCapturesIdentityOnEarlyRejection(t *testing.T) {
	dir := t.TempDir()

	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.AllowedClientQuery = map[string]struct{}{}
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    16,
				SuccessfulResponseBytes: 1 << 20,
			},
			FlowLogDir: dir,
		},
		func(req *http.Request) (*http.Response, error) {
			t.Error("upstream must not be reached")
			return nil, nil
		},
		nil,
	)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"`+strings.Repeat("x", 64)+`"}]}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("record files = %v (err %v), want exactly 1", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		ClientMethod string         `json:"client_method"`
		ClientPath   string         `json:"client_path"`
		Outcome      map[string]any `json:"outcome"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.ClientMethod != http.MethodPost || record.ClientPath != "/v1/messages" {
		t.Fatalf("early rejection lost the client identity: %s %s", record.ClientMethod, record.ClientPath)
	}
	if record.Outcome == nil {
		t.Fatal("early rejection recorded no outcome")
	}
}
