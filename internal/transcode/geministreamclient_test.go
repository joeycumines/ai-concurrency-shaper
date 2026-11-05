package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// geminiSSEBody encodes typed Responses events as an upstream SSE body.
func geminiSSEBody(t *testing.T, events ...ResponsesSSEEvent) string {
	t.Helper()
	var b strings.Builder
	for _, event := range events {
		data, err := MarshalResponsesEvent(event)
		if err != nil {
			t.Fatalf("marshal %s: %v", event.EventType(), err)
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", event.EventType(), data)
	}
	return b.String()
}

// geminiStreamHandler serves a streamGenerateContent exchange over the given
// upstream protocol and SSE body.
func geminiStreamHandler(t *testing.T, upstream UpstreamProtocol, sse string) http.Handler {
	t.Helper()
	key, err := NewRouteKey(http.MethodPost, "/v1beta/models/{model}:generateContent")
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/chat/completions"
	if upstream == UpstreamResponses {
		path = "/v1/responses"
	}
	mapping := Mapping{
		ClientRoute:      key,
		ClientProtocol:   ClientGemini,
		UpstreamProtocol: upstream,
		UpstreamPath:     path,
		ModelMap:         ModelMap{AllowIdentity: true},
		Auth:             AuthPolicy{Mode: AuthNone},
		ChatCapabilities: defaultChatCapabilities(),
		LossPolicy: LossPolicy{Allowed: map[Feature]struct{}{
			FeatureUsageUnknown:           {},
			FeatureUsageCacheReadUnknown:  {},
			FeatureUsageCacheWriteUnknown: {},
			FeatureUsageReasoningUnknown:  {},
			FeatureToolSchemaStrictness:   {},
			FeatureProviderReasoningText:  {},
			FeatureRequestReasoning:       {},
		}},
	}
	return NewTranscodeHandler(
		HandlerConfig{
			Mapping:  mapping,
			Upstream: mustParseURL(t, "https://upstream.example"),
			BodyLimits: BodyLimits{
				AcceptedRequestBytes:    1 << 20,
				SuccessfulResponseBytes: 1 << 20,
			},
		},
		func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		},
		nil,
	)
}

// geminiStreamFrames splits a Gemini stream body into its data payloads. Every
// frame must be data-only: an event line or a comment line fails the test.
func geminiStreamFrames(t *testing.T, body string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for block := range strings.SplitSeq(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		if block == "" {
			continue
		}
		for line := range strings.SplitSeq(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				t.Fatalf("frame line %q is not a data line; body %q", line, body)
			}
		}
		payload := strings.TrimPrefix(block, "data: ")
		var doc map[string]any
		if err := json.Unmarshal([]byte(payload), &doc); err != nil {
			t.Fatalf("frame is not JSON: %v\n%s", err, payload)
		}
		frames = append(frames, doc)
	}
	return frames
}

// geminiFrameText concatenates the text parts of the frames.
func geminiFrameText(frames []map[string]any) string {
	var out strings.Builder
	for _, frame := range frames {
		candidates, _ := frame["candidates"].([]any)
		for _, c := range candidates {
			content, _ := c.(map[string]any)["content"].(map[string]any)
			parts, _ := content["parts"].([]any)
			for _, p := range parts {
				if text, ok := p.(map[string]any)["text"].(string); ok {
					out.WriteString(text)
				}
			}
		}
	}
	return out.String()
}

func geminiFrameFinish(frame map[string]any) string {
	candidates, _ := frame["candidates"].([]any)
	if len(candidates) == 0 {
		return ""
	}
	finish, _ := candidates[0].(map[string]any)["finishReason"].(string)
	return finish
}

func serveGeminiStream(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	return serveGeminiStreamBody(t, handler, target, geminiSimpleBody)
}

func serveGeminiStreamBody(t *testing.T, handler http.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// assertSingleTerminal requires exactly one frame that ends the exchange: a
// finish reason on success or an error object on failure.
func assertSingleTerminal(t *testing.T, frames []map[string]any) {
	t.Helper()
	terminals := 0
	for _, frame := range frames {
		if geminiFrameFinish(frame) != "" {
			terminals++
		}
		if _, ok := frame["error"]; ok {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("%d terminal frames, want exactly one; frames %v", terminals, frames)
	}
}

// responsesFunctionCallSSE is a complete function-call output item followed by
// the completed envelope. The call has the id call_1 and arguments {"q":"x"}.
const responsesFunctionCallSSE = "" +
	"event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","call_id":"call_1","name":"lookup","arguments":""}}` + "\n\n" +
	"event: response.function_call_arguments.delta\n" +
	`data: {"type":"response.function_call_arguments.delta","sequence_number":3,"item_id":"fc_1","output_index":0,"delta":"{\"q\":"}` + "\n\n" +
	"event: response.function_call_arguments.delta\n" +
	`data: {"type":"response.function_call_arguments.delta","sequence_number":4,"item_id":"fc_1","output_index":0,"delta":"\"x\"}"}` + "\n\n" +
	"event: response.function_call_arguments.done\n" +
	`data: {"type":"response.function_call_arguments.done","sequence_number":5,"item_id":"fc_1","output_index":0,"arguments":"{\"q\":\"x\"}"}` + "\n\n" +
	"event: response.output_item.done\n" +
	`data: {"type":"response.output_item.done","sequence_number":6,"output_index":0,"item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}}` + "\n\n"

// responsesReasoningSSE is a completed reasoning item with one summary text
// "think", followed by the completed envelope.
const responsesReasoningSSE = "" +
	"event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress","summary":[]}}` + "\n\n" +
	"event: response.reasoning_summary_part.added\n" +
	`data: {"type":"response.reasoning_summary_part.added","sequence_number":3,"item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}` + "\n\n" +
	"event: response.reasoning_summary_text.delta\n" +
	`data: {"type":"response.reasoning_summary_text.delta","sequence_number":4,"item_id":"rs_1","output_index":0,"summary_index":0,"delta":"think"}` + "\n\n" +
	"event: response.reasoning_summary_text.done\n" +
	`data: {"type":"response.reasoning_summary_text.done","sequence_number":5,"item_id":"rs_1","output_index":0,"summary_index":0,"text":"think"}` + "\n\n" +
	"event: response.reasoning_summary_part.done\n" +
	`data: {"type":"response.reasoning_summary_part.done","sequence_number":6,"item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"think"}}` + "\n\n" +
	"event: response.output_item.done\n" +
	`data: {"type":"response.output_item.done","sequence_number":7,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"think"}]}}` + "\n\n"

const geminiThinkingBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
	`"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`

func TestGeminiStreamResponsesFunctionCall(t *testing.T) {
	sse := geminiSSEBody(t, fsmCreated(1)) + responsesFunctionCallSSE + geminiSSEBody(t, fsmComplete(7))
	rec := serveGeminiStream(t, geminiStreamHandler(t, UpstreamResponses, sse),
		"/v1beta/models/gemini-x:streamGenerateContent?alt=sse")
	frames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, frames)
	var call map[string]any
	for _, frame := range frames {
		for _, c := range frame["candidates"].([]any) {
			parts, _ := c.(map[string]any)["content"].(map[string]any)["parts"].([]any)
			for _, p := range parts {
				if fc, ok := p.(map[string]any)["functionCall"].(map[string]any); ok {
					call = fc
				}
			}
		}
	}
	if call == nil {
		t.Fatalf("no functionCall frame; frames %v", frames)
	}
	if call["id"] != "call_1" || call["name"] != "lookup" {
		t.Fatalf("functionCall identity %v, want id call_1 name lookup", call)
	}
	if args, _ := call["args"].(map[string]any); args["q"] != "x" {
		t.Fatalf("functionCall args %v, want q=x", call["args"])
	}
	if geminiFrameFinish(frames[len(frames)-1]) != "STOP" {
		t.Fatalf("final frame %v, want STOP", frames[len(frames)-1])
	}
}

func TestGeminiStreamResponsesThinkingWithIncludeThoughts(t *testing.T) {
	sse := geminiSSEBody(t, fsmCreated(1)) + responsesReasoningSSE + geminiSSEBody(t, fsmComplete(8))
	rec := serveGeminiStreamBody(t, geminiStreamHandler(t, UpstreamResponses, sse),
		"/v1beta/models/gemini-x:streamGenerateContent?alt=sse", geminiThinkingBody)
	frames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, frames)
	thought := false
	for _, frame := range frames {
		for _, c := range frame["candidates"].([]any) {
			parts, _ := c.(map[string]any)["content"].(map[string]any)["parts"].([]any)
			for _, p := range parts {
				part := p.(map[string]any)
				if part["thought"] == true && part["text"] == "think" {
					thought = true
				}
			}
		}
	}
	if !thought {
		t.Fatalf("includeThoughts stream carried no thought part; frames %v", frames)
	}
}

func TestGeminiStreamResponsesThinkingWithoutIncludeThoughtsIsNotRendered(t *testing.T) {
	sse := geminiSSEBody(t, fsmCreated(1)) + responsesReasoningSSE + geminiSSEBody(t, fsmComplete(8))
	rec := serveGeminiStream(t, geminiStreamHandler(t, UpstreamResponses, sse),
		"/v1beta/models/gemini-x:streamGenerateContent?alt=sse")
	frames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, frames)
	if strings.Contains(rec.Body.String(), "think") || strings.Contains(rec.Body.String(), `"thought"`) {
		t.Fatalf("reasoning text leaked without includeThoughts: %s", rec.Body.String())
	}
}

func TestGeminiStreamChatUsageAbsentOmitsUsageMetadata(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gemini-x","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gemini-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rec := serveGeminiStream(t, geminiStreamHandler(t, UpstreamChatCompletions, sse),
		"/v1beta/models/gemini-x:streamGenerateContent?alt=sse")
	frames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, frames)
	if _, ok := frames[len(frames)-1]["usageMetadata"]; ok {
		t.Fatalf("usage the upstream never reported was rendered: %v", frames[len(frames)-1])
	}
}

func TestGeminiStreamMalformedUpstreamFrameNeverSucceeds(t *testing.T) {
	sse := geminiSSEBody(t,
		fsmCreated(1),
		fsmMessageAdded(2, 0, "msg_1"),
		fsmTextPartAdded(3, 0, 0, "msg_1"),
		fsmTextDelta(4, 0, 0, "msg_1", "hi"),
	) + "event: response.output_text.delta\ndata: {\"type\": oops\n\n" + geminiSSEBody(t, fsmComplete(5))
	rec := serveGeminiStream(t, geminiStreamHandler(t, UpstreamResponses, sse),
		"/v1beta/models/gemini-x:streamGenerateContent?alt=sse")
	frames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, frames)
	if _, ok := frames[len(frames)-1]["error"]; !ok {
		t.Fatalf("malformed upstream frame did not end in an error frame: %v", frames)
	}
	for _, frame := range frames {
		if geminiFrameFinish(frame) != "" {
			t.Fatalf("malformed upstream frame reported success: %v", frame)
		}
	}
}

func TestGeminiStreamOversizedUpstreamFrameReportsError(t *testing.T) {
	huge := strings.Repeat("a", 1<<20+16)
	sse := geminiSSEBody(t,
		fsmCreated(1),
		fsmMessageAdded(2, 0, "msg_1"),
		fsmTextPartAdded(3, 0, 0, "msg_1"),
		fsmTextDelta(4, 0, 0, "msg_1", huge),
	) + geminiSSEBody(t, fsmComplete(5))
	rec := serveGeminiStream(t, geminiStreamHandler(t, UpstreamResponses, sse),
		"/v1beta/models/gemini-x:streamGenerateContent?alt=sse")
	frames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, frames)
	if _, ok := frames[len(frames)-1]["error"]; !ok {
		t.Fatalf("oversized upstream frame did not end in an error frame")
	}
}

func TestGeminiStreamResponsesUpstreamTextAndTerminal(t *testing.T) {
	sse := geminiSSEBody(t,
		fsmCreated(1),
		fsmMessageAdded(2, 0, "msg_1"),
		fsmTextPartAdded(3, 0, 0, "msg_1"),
		fsmTextDelta(4, 0, 0, "msg_1", "hel"),
		fsmTextDelta(5, 0, 0, "msg_1", "lo"),
		fsmTextDone(6, 0, 0, "msg_1", "hello"),
		fsmTextPartDone(7, 0, 0, "msg_1"),
		fsmItemDone(8, 0, "msg_1"),
		fsmComplete(9),
	)
	rec := serveGeminiStream(t, geminiStreamHandler(t, UpstreamResponses, sse),
		"/v1beta/models/gemini-x:streamGenerateContent?alt=sse")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type %q", ct)
	}
	frames := geminiStreamFrames(t, rec.Body.String())
	if got := geminiFrameText(frames); got != "hello" {
		t.Fatalf("streamed text %q, want hello", got)
	}
	last := frames[len(frames)-1]
	if geminiFrameFinish(last) != "STOP" {
		t.Fatalf("final frame finish %q, want STOP; frames %v", geminiFrameFinish(last), frames)
	}
	terminals := 0
	for _, frame := range frames {
		if geminiFrameFinish(frame) != "" {
			terminals++
		}
		if frame["responseId"] == nil || frame["modelVersion"] != "gemini-x" {
			t.Fatalf("frame lacks identity: %v", frame)
		}
	}
	if terminals != 1 {
		t.Fatalf("%d terminal frames, want exactly one", terminals)
	}
}

func TestGeminiStreamTruncatedUpstreamReportsError(t *testing.T) {
	// The upstream stops after a text delta and never sends a terminal.
	sse := geminiSSEBody(t,
		fsmCreated(1),
		fsmMessageAdded(2, 0, "msg_1"),
		fsmTextPartAdded(3, 0, 0, "msg_1"),
		fsmTextDelta(4, 0, 0, "msg_1", "partial"),
	)
	rec := serveGeminiStream(t, geminiStreamHandler(t, UpstreamResponses, sse),
		"/v1beta/models/gemini-x:streamGenerateContent?alt=sse")
	frames := geminiStreamFrames(t, rec.Body.String())
	if len(frames) == 0 {
		t.Fatalf("no frames; body %q", rec.Body.String())
	}
	last := frames[len(frames)-1]
	if _, ok := last["error"]; !ok {
		t.Fatalf("truncated stream did not end with an error frame: %v", last)
	}
	for _, frame := range frames {
		if geminiFrameFinish(frame) != "" {
			t.Fatalf("truncated stream reported a finish reason: %v", frame)
		}
	}
}

func TestGeminiStreamUpstreamErrorEventReportsError(t *testing.T) {
	sse := geminiSSEBody(t,
		fsmCreated(1),
		fsmMessageAdded(2, 0, "msg_1"),
		fsmTextPartAdded(3, 0, 0, "msg_1"),
		fsmTextDelta(4, 0, 0, "msg_1", "partial"),
	) + "event: error\ndata: {\"type\":\"error\",\"sequence_number\":5,\"code\":\"server_error\",\"message\":\"overloaded\",\"param\":\"\"}\n\n"
	rec := serveGeminiStream(t, geminiStreamHandler(t, UpstreamResponses, sse),
		"/v1beta/models/gemini-x:streamGenerateContent?alt=sse")
	frames := geminiStreamFrames(t, rec.Body.String())
	last := frames[len(frames)-1]
	if _, ok := last["error"]; !ok {
		t.Fatalf("upstream error event did not end with an error frame: %v", last)
	}
	for _, frame := range frames {
		if geminiFrameFinish(frame) != "" {
			t.Fatalf("failed stream reported success: %v", frame)
		}
	}
}

func TestGeminiStreamChatUpstreamTextAndTerminal(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gemini-x","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gemini-x","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gemini-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rec := serveGeminiStream(t, geminiStreamHandler(t, UpstreamChatCompletions, sse),
		"/v1beta/models/gemini-x:streamGenerateContent?alt=sse")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	frames := geminiStreamFrames(t, rec.Body.String())
	if got := geminiFrameText(frames); got != "hello" {
		t.Fatalf("streamed text %q, want hello", got)
	}
	last := frames[len(frames)-1]
	if geminiFrameFinish(last) != "STOP" {
		t.Fatalf("final frame %v, want STOP", last)
	}
	usage, _ := last["usageMetadata"].(map[string]any)
	if usage["totalTokenCount"] != float64(5) {
		t.Fatalf("usage %v, want totalTokenCount 5", usage)
	}
	for _, frame := range frames[:len(frames)-1] {
		if geminiFrameFinish(frame) != "" {
			t.Fatalf("non-final frame carries a finish reason: %v", frame)
		}
	}
}

func TestGeminiStreamWithoutAltIsRefusedBeforeUpstream(t *testing.T) {
	var calls int
	handler := geminiChatHandler(t, nil, &calls)
	rec := serveGeminiStream(t, handler, "/v1beta/models/gemini-x:streamGenerateContent")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "requires alt=sse") {
		t.Fatalf("status %d body %s, want a 400 requiring alt=sse", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("refused stream reached the upstream %d times", calls)
	}
}

// geminiResponsesConverterReader drives Responses SSE through the Gemini
// client converter, the same way the handler composes it.
func geminiResponsesConverterReader(sse string) *convertingReader {
	state := newGeminiStreamClientState(testStreamContext(), UpstreamResponses, StrictLossPolicy(), "resp_1", "gemini-x")
	return newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(sse), 0, 0),
		newResponsesToGeminiConverter(state), 0, 0, 0,
	)
}

// assertGeminiFailedStream checks the observable contract of a stream that
// failed: a single data-only error frame, no success terminal, and an
// upstream failure classification.
func assertGeminiFailedStream(t *testing.T, reader *convertingReader, output string, readErr error) {
	t.Helper()
	if reader.SawTerminal() {
		t.Fatal("failed stream must not report a success terminal")
	}
	if strings.Contains(output, "finishReason") {
		t.Fatalf("failed stream reported a finish reason: %q", output)
	}
	if !strings.Contains(output, `"error"`) {
		t.Fatalf("failed stream carried no error frame: %q", output)
	}
	if strings.Contains(output, "event:") {
		t.Fatalf("Gemini stream output carries an event line: %q", output)
	}
	if got := classifyStreamObservation(streamObservation{
		ReaderErr:             readErr,
		SawErrorEvent:         reader.SawErrorEvent(),
		SawTerminal:           reader.SawTerminal(),
		SawUpstreamErrorFrame: reader.SawUpstreamErrorFrame(),
		UpstreamBodyError:     reader.UpstreamBodyError(),
	}); got != streamOutcomeUpstreamFailure {
		t.Fatalf("classification = %v, want upstream failure", got)
	}
}

func TestGeminiStreamResponsesEOFBeforeTerminalIsUpstreamFailure(t *testing.T) {
	sse := geminiSSEBody(t,
		fsmCreated(1),
		fsmMessageAdded(2, 0, "msg_1"),
		fsmTextPartAdded(3, 0, 0, "msg_1"),
		fsmTextDelta(4, 0, 0, "msg_1", "partial"),
	)
	reader := geminiResponsesConverterReader(sse)
	output, readErr := drainReader(t, reader)
	assertGeminiFailedStream(t, reader, output, readErr)
}

func TestGeminiStreamResponsesErrorEventIsUpstreamFailure(t *testing.T) {
	sse := geminiSSEBody(t,
		fsmCreated(1),
		fsmMessageAdded(2, 0, "msg_1"),
		fsmTextPartAdded(3, 0, 0, "msg_1"),
		fsmTextDelta(4, 0, 0, "msg_1", "partial"),
	) + "event: error\ndata: {\"type\":\"error\",\"sequence_number\":5,\"code\":\"server_error\",\"message\":\"overloaded\",\"param\":\"\"}\n\n"
	reader := geminiResponsesConverterReader(sse)
	output, readErr := drainReader(t, reader)
	assertGeminiFailedStream(t, reader, output, readErr)
}

func TestGeminiStreamResponsesMalformedFrameIsUpstreamFailure(t *testing.T) {
	sse := geminiSSEBody(t,
		fsmCreated(1),
		fsmMessageAdded(2, 0, "msg_1"),
		fsmTextPartAdded(3, 0, 0, "msg_1"),
	) + "event: response.output_text.delta\ndata: {\"type\": oops\n\n"
	reader := geminiResponsesConverterReader(sse)
	output, readErr := drainReader(t, reader)
	assertGeminiFailedStream(t, reader, output, readErr)
}

// usageOnlyPolicy approves only the usage losses a fixture without usage
// incurs, so a stream it fails is failed by the feature under test.
func usageOnlyPolicy(extra ...Feature) LossPolicy {
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageUnknown:           {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	for _, feature := range extra {
		policy.Allowed[feature] = struct{}{}
	}
	return policy
}

// assertGeminiLossRefused fails unless the stream was refused by a loss of
// the given feature the policy did not approve.
func assertGeminiLossRefused(t *testing.T, reader *convertingReader, output string, err error, feature Feature) {
	t.Helper()
	if reader.SawTerminal() || strings.Contains(output, "finishReason") {
		t.Fatalf("policy without %s reported success: %q", feature, output)
	}
	if err == nil || !strings.Contains(err.Error(), string(feature)) {
		t.Fatalf("err = %v, want a refusal naming %s", err, feature)
	}
}

// geminiClientStateReader converts the SSE body through a Responses->Gemini
// state with the given policy, returning the reader and the state.
func geminiClientStateReader(sse string, policy LossPolicy) (*convertingReader, *geminiStreamClientState) {
	ctx := testStreamContext()
	ctx.LossPolicy = policy
	state := newGeminiStreamClientState(ctx, UpstreamResponses, policy, "resp_1", "gemini-x")
	return newConvertingReaderWithLimits(
		NewSSEReaderWithLimits(strings.NewReader(sse), 0, 0),
		newResponsesToGeminiConverter(state), 0, 0, 0,
	), state
}

func TestGeminiStreamResponsesUnrenderedOutputMetadataIsGated(t *testing.T) {
	encryptedReasoning := "" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress","summary":[]}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[],"encrypted_content":"opaque"}}` + "\n\n"
	phasedMessage := "" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","phase":"final_answer","content":[]}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","phase":"final_answer","content":[]}}` + "\n\n"
	tests := []struct {
		name    string
		items   string
		feature Feature
	}{
		{name: "encrypted reasoning", items: encryptedReasoning, feature: FeatureReasoningSummary},
		{name: "message phase", items: phasedMessage, feature: FeatureOutputPhase},
	}
	for _, tt := range tests {
		sse := geminiSSEBody(t, fsmCreated(1)) + tt.items + geminiSSEBody(t, fsmComplete(4))
		t.Run(tt.name+" approved", func(t *testing.T) {
			reader, state := geminiClientStateReader(sse, usageOnlyPolicy(tt.feature))
			output, err := drainReader(t, reader)
			if !errors.Is(err, io.EOF) || !reader.SawTerminal() {
				t.Fatalf("stream did not complete: err %v; output %q", err, output)
			}
			if !reportHasFeature(state.report, tt.feature) {
				t.Fatalf("report lacks %s: %+v", tt.feature, state.report.Losses)
			}
		})
		t.Run(tt.name+" not approved", func(t *testing.T) {
			reader, _ := geminiClientStateReader(sse, usageOnlyPolicy())
			output, err := drainReader(t, reader)
			assertGeminiLossRefused(t, reader, output, err, tt.feature)
		})
	}
}

func TestGeminiStreamResponsesUnstreamedSummaryIsLost(t *testing.T) {
	sse := geminiSSEBody(t, fsmCreated(1)) +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress","summary":[]}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"never streamed"}]}}` + "\n\n" +
		geminiSSEBody(t, fsmComplete(4))
	reader, _ := geminiClientStateReader(sse, usageOnlyPolicy())
	output, err := drainReader(t, reader)
	assertGeminiLossRefused(t, reader, output, err, FeatureReasoningSummary)
}

// geminiReasoningSummarySSE builds a reasoning item whose summary arrives as
// deltas (the given text) and whose output_item.done repeats the done text.
func geminiReasoningSummarySSE(streamed, done string) string {
	return "" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress","summary":[]}}` + "\n\n" +
		"event: response.reasoning_summary_part.added\n" +
		`data: {"type":"response.reasoning_summary_part.added","sequence_number":3,"item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}` + "\n\n" +
		"event: response.reasoning_summary_text.delta\n" +
		`data: {"type":"response.reasoning_summary_text.delta","sequence_number":4,"item_id":"rs_1","output_index":0,"summary_index":0,"delta":` + jsonString(streamed) + `}` + "\n\n" +
		"event: response.reasoning_summary_text.done\n" +
		`data: {"type":"response.reasoning_summary_text.done","sequence_number":5,"item_id":"rs_1","output_index":0,"summary_index":0,"text":` + jsonString(streamed) + `}` + "\n\n" +
		"event: response.reasoning_summary_part.done\n" +
		`data: {"type":"response.reasoning_summary_part.done","sequence_number":6,"item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":` + jsonString(streamed) + `}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","sequence_number":7,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":` + jsonString(done) + `}]}}` + "\n\n"
}

func jsonString(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func TestGeminiStreamResponsesSummaryCoverageProvesContentNotBytes(t *testing.T) {
	// The streamed summary and the final summary are both 3 bytes but differ:
	// the final text was never rendered, so the loss must be recorded.
	sameLengthDifferent := geminiSSEBody(t, fsmCreated(1)) +
		geminiReasoningSummarySSE("aaa", "bbb") +
		geminiSSEBody(t, fsmComplete(8))
	reader, _ := geminiClientStateReader(sameLengthDifferent, usageOnlyPolicy(FeatureProviderReasoningText))
	output, err := drainReader(t, reader)
	assertGeminiLossRefused(t, reader, output, err, FeatureReasoningSummary)

	// The final summary matches the streamed deltas exactly: no loss.
	exact := geminiSSEBody(t, fsmCreated(1)) +
		geminiReasoningSummarySSE("think", "think") +
		geminiSSEBody(t, fsmComplete(8))
	reader, state := geminiClientStateReader(exact, usageOnlyPolicy(FeatureProviderReasoningText))
	output, err = drainReader(t, reader)
	if !errors.Is(err, io.EOF) || !reader.SawTerminal() {
		t.Fatalf("exact-match summary stream did not complete: err %v; output %q", err, output)
	}
	if reportHasFeature(state.report, FeatureReasoningSummary) {
		t.Fatalf("exactly streamed summary recorded a loss: %+v", state.report.Losses)
	}

	// The done event omits the repeated summary after deltas streamed: the
	// summary is not proven covered, so the loss must be recorded.
	omitted := geminiSSEBody(t, fsmCreated(1)) +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress","summary":[]}}` + "\n\n" +
		"event: response.reasoning_summary_part.added\n" +
		`data: {"type":"response.reasoning_summary_part.added","sequence_number":3,"item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}` + "\n\n" +
		"event: response.reasoning_summary_text.delta\n" +
		`data: {"type":"response.reasoning_summary_text.delta","sequence_number":4,"item_id":"rs_1","output_index":0,"summary_index":0,"delta":"think"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[]}}` + "\n\n" +
		geminiSSEBody(t, fsmComplete(6))
	reader, _ = geminiClientStateReader(omitted, usageOnlyPolicy(FeatureProviderReasoningText))
	output, err = drainReader(t, reader)
	assertGeminiLossRefused(t, reader, output, err, FeatureReasoningSummary)
}

func TestGeminiStreamClientIncompleteReasonMustBeKnown(t *testing.T) {
	// An incomplete terminal with a missing or unrecognized reason must never
	// fall through to a STOP completion: that would report an incomplete
	// response as a normal finish.
	incompleteEvent := func(reason *string) ResponseIncompleteEvent {
		env := fsmEnvelope("resp_1", "incomplete")
		if reason != nil {
			env.IncompleteDetails = &ResponsesIncompleteDetails{Reason: *reason}
		}
		return ResponseIncompleteEvent{
			Type: "response.incomplete", SequenceNumber: 2, Response: env,
		}
	}
	known := func(reason string) bool {
		return reason == "max_output_tokens" || reason == "content_filter"
	}
	nilReason := (*string)(nil)
	for _, tc := range []struct {
		name   string
		event  ResponseIncompleteEvent
		wantOK bool
	}{
		{name: "missing incomplete_details", event: incompleteEvent(nilReason)},
		{name: "empty reason", event: incompleteEvent(new(""))},
		{name: "unknown reason", event: incompleteEvent(new("pause_turn"))},
		{name: "max_output_tokens", event: incompleteEvent(new("max_output_tokens")), wantOK: true},
		{name: "content_filter", event: incompleteEvent(new("content_filter")), wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := newGeminiStreamClientState(testStreamContext(), UpstreamResponses, usageOnlyPolicy(), "resp_1", "gemini-x")
			state.ctx.LossPolicy = state.policy
			if _, err := state.Convert(fsmCreated(1)); err != nil {
				t.Fatalf("created: %v", err)
			}
			chunks, err := state.Convert(tc.event)
			if tc.wantOK {
				if err != nil || len(chunks) != 1 {
					t.Fatalf("known reason: chunks=%v err=%v, want one terminal chunk", chunks, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("incomplete with reason %v: accepted with chunks %v, want an error", tc.event.Response.IncompleteDetails, chunks)
			}
			var unrep *UnrepresentableError
			if !known("") && errors.As(err, &unrep) {
				// unknown non-empty reason: unrepresentable
			} else if !isUpstreamConversionError(err) && !errors.As(err, &unrep) {
				t.Fatalf("err = %T %v, want a wire error or UnrepresentableError", err, err)
			}
			if len(chunks) != 0 || strings.Contains(fmt.Sprint(chunks), `"STOP"`) {
				t.Fatalf("failed incomplete emitted chunks %v, want none", chunks)
			}
		})
	}
}

func TestGeminiStreamClientOutputItemDoneCoversModeledItems(t *testing.T) {
	// Every output item type the wire union models must be handled on
	// output_item.done: function_call_output is a policy-gated loss (never a
	// silent drop), and text deltas carrying logprobs are loss-gated once.
	callOutputDone := func(seq int64) ResponseOutputItemDoneEvent {
		return ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: seq, OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputResultItem{
				ID: "r1", Type: "function_call_output", Status: ResponsesItemCompleted,
				CallID: "call_1", Output: ResponsesFunctionOutput{Text: new("ok")},
			},
		}
	}
	callOutputAdded := func(seq int64) ResponseOutputItemAddedEvent {
		return ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: seq, OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputResultItem{
				ID: "r1", Type: "function_call_output", Status: ResponsesItemInProgress,
				CallID: "call_1", Output: ResponsesFunctionOutput{Text: new("ok")},
			},
		}
	}
	callOutput := func(s *geminiStreamClientState, seq int64) error {
		t.Helper()
		if _, err := s.Convert(callOutputAdded(seq)); err != nil {
			return err
		}
		_, err := s.Convert(callOutputDone(seq + 1))
		return err
	}
	logprobsDelta := func(seq int64) ResponseTextDeltaEvent {
		return ResponseTextDeltaEvent{
			Type: "response.output_text.delta", SequenceNumber: seq,
			ItemID: "msg_1", OutputIndex: 0, ContentIndex: 0, Delta: "hi",
			Logprobs: []openairesponses.TextLogprob{{Token: "hi", Logprob: -0.1}},
		}
	}
	textOpen := func(s *geminiStreamClientState) {
		t.Helper()
		if _, err := s.Convert(fsmCreated(1)); err != nil {
			t.Fatalf("created: %v", err)
		}
		if _, err := s.Convert(fsmMessageAdded(2, 0, "msg_1")); err != nil {
			t.Fatalf("message added: %v", err)
		}
		if _, err := s.Convert(fsmTextPartAdded(3, 0, 0, "msg_1")); err != nil {
			t.Fatalf("part added: %v", err)
		}
	}

	// function_call_output under a strict policy: refused with the
	// output_item_boundaries loss, not silently dropped.
	s := newGeminiStreamClientState(testStreamContext(), UpstreamResponses, StrictLossPolicy(), "resp_1", "gemini-x")
	if _, err := s.Convert(fsmCreated(1)); err != nil {
		t.Fatalf("created: %v", err)
	}
	err := callOutput(s, 2)
	var unsupported *UnsupportedFeatureError
	if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureOutputItemBoundaries) {
		t.Fatalf("function_call_output: err = %T %v, want UnsupportedFeatureError(%s)", err, err, FeatureOutputItemBoundaries)
	}

	// Under an allowing policy the stream completes and the loss is recorded.
	s = newGeminiStreamClientState(testStreamContext(), UpstreamResponses,
		usageOnlyPolicy(FeatureOutputItemBoundaries), "resp_1", "gemini-x")
	if _, err := s.Convert(fsmCreated(1)); err != nil {
		t.Fatalf("created: %v", err)
	}
	if err := callOutput(s, 2); err != nil {
		t.Fatalf("allowed function_call_output: %v", err)
	}
	if !reportHasFeature(s.report, FeatureOutputItemBoundaries) {
		t.Fatalf("function_call_output loss not recorded: %+v", s.report.Losses)
	}

	// Text deltas with logprobs: gated once per stream under FeatureLogprobs.
	s = newGeminiStreamClientState(testStreamContext(), UpstreamResponses, StrictLossPolicy(), "resp_1", "gemini-x")
	textOpen(s)
	if _, err := s.Convert(logprobsDelta(4)); err == nil {
		t.Fatal("logprobs delta under strict policy: accepted")
	} else if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureLogprobs) {
		t.Fatalf("logprobs delta: err = %T %v, want UnsupportedFeatureError(%s)", err, err, FeatureLogprobs)
	}
	s = newGeminiStreamClientState(testStreamContext(), UpstreamResponses,
		usageOnlyPolicy(FeatureLogprobs), "resp_1", "gemini-x")
	textOpen(s)
	for _, seq := range []int64{4, 5} {
		if _, err := s.Convert(logprobsDelta(seq)); err != nil {
			t.Fatalf("allowed logprobs delta %d: %v", seq, err)
		}
	}
	if n := countReportFeature(s.report, FeatureLogprobs); n != 1 {
		t.Fatalf("logprobs loss recorded %d times for two deltas, want 1: %+v", n, s.report.Losses)
	}
}

func TestGeminiStreamClientBudgetIsUpstreamFault(t *testing.T) {
	state := newGeminiStreamClientState(testStreamContext(), UpstreamMessages, StrictLossPolicy(), "resp_1", "gemini-x")
	state.budget.semanticBytes = state.budget.semanticBytesBudget
	_, err := state.textChunk("x", false)
	if !isUpstreamConversionError(err) {
		t.Fatalf("semantic budget: err = %T %v, want an upstream conversion error", err, err)
	}
}

func TestGeminiStreamClientSummaryRetentionIsBudgetedAndGatedBeforeMutation(t *testing.T) {
	// A reasoning-summary delta that is retained for coverage proof must be
	// charged against the semantic-bytes budget even when thoughts are not
	// rendered, so an upstream cannot grow the retained copy without bound.
	reasoningOpen := func(s *geminiStreamClientState, seq int64) {
		t.Helper()
		if _, err := s.Convert(fsmCreated(1)); err != nil {
			t.Fatalf("created: %v", err)
		}
		if _, err := s.Convert(ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: seq,
			OutputIndex: 0,
			Item: &ResponsesReasoningOutputItem{
				ID: "rs_1", Type: "reasoning", Status: ResponsesItemInProgress,
				Summary: []ResponsesReasoningSummary{},
			},
		}); err != nil {
			t.Fatalf("item added: %v", err)
		}
		if _, err := s.Convert(ResponseReasoningSummaryPartAddedEvent{
			Type: "response.reasoning_summary_part.added", SequenceNumber: seq + 1,
			ItemID: "rs_1", OutputIndex: 0, SummaryIndex: 0,
			Part: openairesponses.SummaryTextPart{Type: "summary_text", Text: ""},
		}); err != nil {
			t.Fatalf("part added: %v", err)
		}
	}
	newSummaryState := func() *geminiStreamClientState {
		state := newGeminiStreamClientState(testStreamContext(), UpstreamResponses, usageOnlyPolicy(FeatureProviderReasoningText), "resp_1", "gemini-x")
		state.ctx.GeminiIncludeThoughts = false
		reasoningOpen(state, 2)
		return state
	}
	state := newSummaryState()
	state.budget.semanticBytes = state.budget.semanticBytesBudget
	_, err := state.Convert(ResponseReasoningSummaryTextDeltaEvent{
		Type: "response.reasoning_summary_text.delta", SequenceNumber: 5,
		ItemID: "rs_1", OutputIndex: 0, SummaryIndex: 0, Delta: "x",
	})
	if !isUpstreamConversionError(err) {
		t.Fatalf("retained summary delta: err = %T %v, want an upstream conversion error", err, err)
	}

	// When the policy disallows provider reasoning text, the loss is evaluated
	// before any state is retained: no summary entry may exist afterwards.
	state = newGeminiStreamClientState(testStreamContext(), UpstreamResponses, StrictLossPolicy(), "resp_1", "gemini-x")
	state.ctx.GeminiIncludeThoughts = false
	reasoningOpen(state, 2)
	_, err = state.Convert(ResponseReasoningSummaryTextDeltaEvent{
		Type: "response.reasoning_summary_text.delta", SequenceNumber: 5,
		ItemID: "rs_1", OutputIndex: 0, SummaryIndex: 0, Delta: "x",
	})
	var unsupported *UnsupportedFeatureError
	if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureProviderReasoningText) {
		t.Fatalf("strict policy: err = %T %v, want UnsupportedFeatureError(%s)", err, err, FeatureProviderReasoningText)
	}
	if len(state.streamedSummary) != 0 || state.budget.stateEntries != 0 {
		t.Fatalf("loss checked after mutation: streamedSummary=%v stateEntries=%d, want empty/0",
			state.streamedSummary, state.budget.stateEntries)
	}
}

func TestGeminiStreamChatUpstreamLengthIsMaxTokens(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gemini-x","choices":[{"index":0,"delta":{"role":"assistant","content":"cut"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gemini-x","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rec := serveGeminiStream(t, geminiStreamHandler(t, UpstreamChatCompletions, sse),
		"/v1beta/models/gemini-x:streamGenerateContent?alt=sse")
	frames := geminiStreamFrames(t, rec.Body.String())
	assertSingleTerminal(t, frames)
	if finish := geminiFrameFinish(frames[len(frames)-1]); finish != "MAX_TOKENS" {
		t.Fatalf("finish %q, want MAX_TOKENS; body %s", finish, rec.Body.String())
	}
}

func TestGeminiStreamClientFailureEventsLatch(t *testing.T) {
	// A failed/error event is a failure terminal, not a success one: every
	// later call, [DONE], and EOF must propagate the latched failure instead
	// of reporting a clean terminal.
	tests := []struct {
		name  string
		event func(seq int64) ResponsesSSEEvent
	}{
		{
			name: "failed event",
			event: func(seq int64) ResponsesSSEEvent {
				return ResponseFailedEvent{
					Type: "response.failed", SequenceNumber: seq,
					Response: fsmEnvelope("resp_1", "failed"),
				}
			},
		},
		{
			name: "error event",
			event: func(seq int64) ResponsesSSEEvent {
				return ResponseErrorEvent{
					Type: "error", SequenceNumber: seq, Code: "server_error", Message: "boom",
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newGeminiStreamClientState(
				testStreamContext(), UpstreamResponses, usageOnlyPolicy(), "resp_1", "gemini-x",
			)
			if _, err := state.Convert(fsmCreated(0)); err != nil {
				t.Fatalf("created: %v", err)
			}
			chunks, failure := state.Convert(tt.event(1))
			if failure == nil || !isUpstreamConversionError(failure) {
				t.Fatalf("failure event: err = %T %v, want an upstream conversion error", failure, failure)
			}
			if len(chunks) != 0 {
				t.Fatalf("failure event produced chunks: %v", chunks)
			}
			if _, err := state.Convert(fsmMessageAdded(2, 0, "msg_1")); !errors.Is(err, failure) {
				t.Fatalf("event after failure: err = %v, want the latched %v", err, failure)
			}

			converter := newResponsesToGeminiConverter(state)
			batch, err := converter.Convert(SSEEvent{Data: []byte("[DONE]")})
			if !errors.Is(err, failure) {
				t.Fatalf("[DONE] after failure: err = %v, want the latched %v", err, failure)
			}
			if batch.Terminal || len(batch.Events) != 0 {
				t.Fatalf("[DONE] after failure released %+v", batch)
			}
			batch, err = converter.FinalizeEOF()
			if !errors.Is(err, failure) {
				t.Fatalf("FinalizeEOF after failure: err = %v, want the latched %v", err, failure)
			}
			if batch.Terminal || len(batch.Events) != 0 {
				t.Fatalf("FinalizeEOF after failure released %+v", batch)
			}
		})
	}
}

func TestGeminiStreamClientOutputItemAddedIsGated(t *testing.T) {
	// Metadata a Gemini stream cannot carry is decided when the item is
	// announced: an upstream must not get its text deltas through before the
	// done event reveals the incompatibility.
	newState := func(policy LossPolicy) *geminiStreamClientState {
		state := newGeminiStreamClientState(
			testStreamContext(), UpstreamResponses, policy, "resp_1", "gemini-x",
		)
		if _, err := state.Convert(fsmCreated(1)); err != nil {
			t.Fatalf("created: %v", err)
		}
		return state
	}
	messageAdded := func(seq int64, phase string) ResponseOutputItemAddedEvent {
		event := fsmMessageAdded(seq, 0, "msg_1")
		event.Item.(*ResponsesOutputMessage).Phase = phase
		return event
	}
	callOutputAdded := func(seq int64) ResponseOutputItemAddedEvent {
		return ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: seq, OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputResultItem{
				ID: "r1", Type: "function_call_output", Status: ResponsesItemInProgress,
				CallID: "call_1", Output: ResponsesFunctionOutput{Text: new("ok")},
			},
		}
	}
	callOutputDone := func(seq int64) ResponseOutputItemDoneEvent {
		return ResponseOutputItemDoneEvent{
			Type: "response.output_item.done", SequenceNumber: seq, OutputIndex: 0,
			Item: &ResponsesFunctionCallOutputResultItem{
				ID: "r1", Type: "function_call_output", Status: ResponsesItemCompleted,
				CallID: "call_1", Output: ResponsesFunctionOutput{Text: new("ok")},
			},
		}
	}
	reasoningAdded := func(seq int64, item *ResponsesReasoningOutputItem) ResponseOutputItemAddedEvent {
		return ResponseOutputItemAddedEvent{
			Type: "response.output_item.added", SequenceNumber: seq, OutputIndex: 0, Item: item,
		}
	}
	var unsupported *UnsupportedFeatureError
	wantRefused := func(t *testing.T, err error, feature Feature) {
		t.Helper()
		if !errors.As(err, &unsupported) || unsupported.Feature != string(feature) {
			t.Fatalf("err = %T %v, want UnsupportedFeatureError(%s)", err, err, feature)
		}
	}

	t.Run("phase refuses before the text", func(t *testing.T) {
		state := newState(StrictLossPolicy())
		wantRefused(t, convertErr(state, messageAdded(2, "final_answer")), FeatureOutputPhase)
		chunks, err := state.Convert(fsmTextDelta(3, 0, 0, "msg_1", "leak"))
		if err == nil || len(chunks) != 0 {
			t.Fatalf("delta after the refusal = %v, err %v; want no chunk", chunks, err)
		}
	})

	t.Run("tool result refuses at added", func(t *testing.T) {
		state := newState(StrictLossPolicy())
		wantRefused(t, convertErr(state, callOutputAdded(2)), FeatureOutputItemBoundaries)
	})

	t.Run("tool result is recorded once", func(t *testing.T) {
		state := newState(usageOnlyPolicy(FeatureOutputItemBoundaries))
		if err := convertErr(state, callOutputAdded(2)); err != nil {
			t.Fatalf("added: %v", err)
		}
		if err := convertErr(state, callOutputDone(3)); err != nil {
			t.Fatalf("done: %v", err)
		}
		if n := countReportFeature(state.report, FeatureOutputItemBoundaries); n != 1 {
			t.Fatalf("loss recorded %d times, want 1: %+v", n, state.report.Losses)
		}
	})

	t.Run("encrypted reasoning refuses at added", func(t *testing.T) {
		state := newState(StrictLossPolicy())
		wantRefused(t, convertErr(state, reasoningAdded(2, &ResponsesReasoningOutputItem{
			ID: "rs_1", Type: "reasoning", Status: ResponsesItemInProgress,
			Summary: []ResponsesReasoningSummary{}, EncryptedContent: "opaque",
		})), FeatureReasoningSummary)
	})

	t.Run("reasoning content refuses at added", func(t *testing.T) {
		state := newState(StrictLossPolicy())
		wantRefused(t, convertErr(state, reasoningAdded(2, &ResponsesReasoningOutputItem{
			ID: "rs_1", Type: "reasoning", Status: ResponsesItemInProgress,
			Summary: []ResponsesReasoningSummary{},
			Content: []openairesponses.ReasoningText{{Type: "reasoning_text", Text: "hidden"}},
		})), FeatureReasoningSummary)
	})

	t.Run("bare reasoning item is not lost at added", func(t *testing.T) {
		state := newState(StrictLossPolicy())
		if err := convertErr(state, reasoningAdded(2, &ResponsesReasoningOutputItem{
			ID: "rs_1", Type: "reasoning", Status: ResponsesItemInProgress,
			Summary: []ResponsesReasoningSummary{},
		})); err != nil {
			t.Fatalf("bare reasoning item: %v", err)
		}
		if reportHasFeature(state.report, FeatureReasoningSummary) {
			t.Fatalf("bare reasoning item recorded a loss: %+v", state.report.Losses)
		}
	})
}

// convertErr runs one typed event through the state and returns its error.
func convertErr(state *geminiStreamClientState, event ResponsesSSEEvent) error {
	_, err := state.Convert(event)
	return err
}

func TestGeminiStreamClientDoneOnlyLogprobsAreGated(t *testing.T) {
	// Logprobs carried only on the done events must not bypass the loss gate.
	logprob := []openairesponses.TextLogprob{{Token: "hi", Logprob: -0.1}}
	openText := func(t *testing.T, policy LossPolicy) *geminiStreamClientState {
		t.Helper()
		state := newGeminiStreamClientState(
			testStreamContext(), UpstreamResponses, policy, "resp_1", "gemini-x",
		)
		for _, event := range []ResponsesSSEEvent{
			fsmCreated(1),
			fsmMessageAdded(2, 0, "msg_1"),
			fsmTextPartAdded(3, 0, 0, "msg_1"),
			fsmTextDelta(4, 0, 0, "msg_1", "hi"),
		} {
			if _, err := state.Convert(event); err != nil {
				t.Fatalf("%s: %v", event.EventType(), err)
			}
		}
		return state
	}
	textDone := func(seq int64) ResponseTextDoneEvent {
		return ResponseTextDoneEvent{
			Type: "response.output_text.done", SequenceNumber: seq,
			ItemID: "msg_1", OutputIndex: 0, ContentIndex: 0, Text: "hi",
			Logprobs: logprob,
		}
	}
	partDone := func(seq int64) ResponseContentPartDoneEvent {
		return ResponseContentPartDoneEvent{
			Type: "response.content_part.done", SequenceNumber: seq,
			ItemID: "msg_1", OutputIndex: 0, ContentIndex: 0,
			Part: &ResponsesStreamOutputTextPart{
				Type: "output_text", Text: "hi",
				Annotations: []ResponsesAnnotation{}, Logprobs: logprob,
			},
		}
	}
	var unsupported *UnsupportedFeatureError

	state := openText(t, StrictLossPolicy())
	err := convertErr(state, textDone(5))
	if !errors.As(err, &unsupported) || unsupported.Feature != string(FeatureLogprobs) {
		t.Fatalf("done-only logprobs: err = %T %v, want UnsupportedFeatureError(%s)", err, err, FeatureLogprobs)
	}

	state = openText(t, usageOnlyPolicy(FeatureLogprobs))
	if err := convertErr(state, textDone(5)); err != nil {
		t.Fatalf("allowed text done: %v", err)
	}
	if err := convertErr(state, partDone(6)); err != nil {
		t.Fatalf("allowed part done: %v", err)
	}
	if n := countReportFeature(state.report, FeatureLogprobs); n != 1 {
		t.Fatalf("logprobs loss recorded %d times for the done events, want 1: %+v", n, state.report.Losses)
	}
}
