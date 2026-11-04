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

func TestGeminiStreamClientBudgetIsUpstreamFault(t *testing.T) {
	state := newGeminiStreamClientState(testStreamContext(), UpstreamMessages, StrictLossPolicy(), "resp_1", "gemini-x")
	state.budget.semanticBytes = state.budget.semanticBytesBudget
	_, err := state.textChunk("x", false)
	if !isUpstreamConversionError(err) {
		t.Fatalf("semantic budget: err = %T %v, want an upstream conversion error", err, err)
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
