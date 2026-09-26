package transcode

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Legacy function_call streaming accumulates exactly like tool_calls
// fragments: the name arrives first, arguments fragments follow, and the
// finish_reason function_call closes one tool call with byte-exact args.
func TestLegacyFunctionCallStreamAccumulation(t *testing.T) {
	frames := []string{
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","function_call":{"name":"get_weather","arguments":""}},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"function_call":{"arguments":"{\"city\":"}},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"function_call":{"arguments":"\"Tokyo\"}"}},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}`,
	}
	state := newChatResponsesStreamState(
		testStreamContext(),
		StrictLossPolicy(),
		ChatCapabilities{},
		"resp_1",
		"m",
		1,
		nil,
	)
	var all []ResponsesSSEEvent
	for i, f := range frames {
		chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(f)})
		if err != nil {
			t.Fatalf("frame %d rejected: %v", i, err)
		}
		events, err := state.Convert(chunk)
		if err != nil {
			t.Fatalf("frame %d convert: %v", i, err)
		}
		all = append(all, events...)
	}
	held, ok := state.releaseTerminal()
	if !ok {
		t.Fatal("no held terminal after function_call finish")
	}
	all = append(all, held...)
	var done *ResponseFunctionCallArgumentsDoneEvent
	for _, e := range all {
		if d, ok := e.(ResponseFunctionCallArgumentsDoneEvent); ok {
			c := d
			done = &c
		}
	}
	if done == nil {
		t.Fatalf("no arguments done in %d events", len(all))
	}
	if done.Arguments != `{"city":"Tokyo"}` {
		t.Fatalf("done arguments = %q, want full object", done.Arguments)
	}
	if !reportHasFeature(state.report, FeatureLegacyFunctionCall) {
		t.Fatalf("report lacks legacy_function_call note: %+v", state.report)
	}
}

// The same legacy stream through the Chat->Anthropic composition emits one
// tool_use block with the full input, never an error terminal.
func TestLegacyFunctionCallToAnthropicStream(t *testing.T) {
	frames := []string{
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","function_call":{"name":"f","arguments":""}},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"function_call":{"arguments":"{\"x\":1}"}},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}`,
	}
	chat := newChatResponsesStreamState(
		testStreamContext(), StrictLossPolicy(), ChatCapabilities{},
		"resp_1", "m", 1, nil,
	)
	anthropic := newAnthropicResponsesStreamState(
		testStreamContext(), j6PermissivePolicy(), ChatCapabilities{},
		"msg_1", "m", 1,
	)
	converter := newChatToAnthropicConverter(chat, anthropic)
	var text strings.Builder
	for i, f := range frames {
		batch, err := converter.Convert(SSEEvent{Data: []byte(f)})
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		for _, e := range batch.Events {
			text.WriteString(string(e.Data))
		}
	}
	batch, err := converter.Convert(SSEEvent{Data: []byte("[DONE]")})
	if err != nil {
		t.Fatalf("DONE: %v", err)
	}
	for _, e := range batch.Events {
		text.WriteString(string(e.Data))
	}
	out := text.String()
	if !strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("missing tool_use block: %q", out)
	}
	if strings.Contains(out, `"type":"error"`) {
		t.Fatalf("error terminal in legacy stream: %q", out)
	}
}

// Member-level nulls and the identity-less `{}` fragment are corrupt wire,
// never silently defaulted (the benign BOTH-members-empty fragment is the
// separate no-op case): these reject, matching the tool_calls spelling
// (null arguments) and the non-streaming legacy validation.
func TestLegacyFunctionCallCorruptFragmentsRejected(t *testing.T) {
	for _, body := range []string{
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"function_call":{"name":"f","arguments":null}},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"function_call":{"name":null,"arguments":"{}"}},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"function_call":{}},"finish_reason":null}]}`,
	} {
		if _, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(body)}); err == nil {
			t.Fatalf("streaming fragment accepted: %s", body)
		}
	}
	for _, body := range []string{
		`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"function_call","message":{"role":"assistant","content":null,"function_call":{"name":"f","arguments":null}}}]} `,
		`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"function_call","message":{"role":"assistant","content":null,"function_call":{}}}]} `,
	} {
		if _, _, err := DecodeChatResponseWithPolicy([]byte(body), ChatCapabilities{}, StrictLossPolicy()); err == nil {
			t.Fatalf("non-streaming fragment accepted: %s", body)
		}
	}
}

// The full Messages handler renders a legacy upstream stream as one tool_use
// block with byte-exact input, never the legacy-spelling error terminal.
func TestLegacyFunctionCallMessagesHandlerStream(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	sse := "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"function_call\":{\"name\":\"get_weather\",\"arguments\":\"\"}},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"function_call\":{\"arguments\":\"{\\\"city\\\":\\\"Tokyo\\\"}\"}},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"function_call\"}]}\n\n" +
		"data: [DONE]\n\n"
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"get_weather","description":"weather","input_schema":{"type":"object"}}],"stream":true}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "legacy function_call spelling") {
		t.Fatalf("legacy error terminal: %q", body)
	}
	if !strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("missing tool_use: %q", body)
	}
	if !strings.Contains(body, `Tokyo`) {
		t.Fatalf("missing arguments: %q", body)
	}
}

// The full Responses handler renders the same legacy stream as one
// function_call item, never the legacy-spelling error terminal.
func TestLegacyFunctionCallResponsesHandlerStream(t *testing.T) {
	mapping := responsesMapping(t)
	sse := "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"function_call\":{\"name\":\"f\",\"arguments\":\"\"}},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"function_call\":{\"arguments\":\"{\\\"x\\\":1}\"}},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"function_call\"}]}\n\n" +
		"data: [DONE]\n\n"
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"m","input":"hi","stream":true}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "legacy function_call spelling") {
		t.Fatalf("legacy error terminal: %q", body)
	}
	if !strings.Contains(body, "function_call") {
		t.Fatalf("missing function_call item: %q", body)
	}
}

// A terminal chunk carrying a legacy function_call fragment where both
// members are present but empty strings is a benign upstream marker (not
// corrupt wire): it carries no tool-call content to accumulate and must
// not kill an otherwise-complete stream. The parser treats it as a no-op
// equivalent to the already-accepted null fragment.
func TestLegacyFunctionCallEmptyStringFragmentAccepted(t *testing.T) {
	_, _, hasContent, emptyFragment, err := parseLegacyFunctionCallFragment(
		[]byte(`{"arguments":"","name":""}`),
	)
	if err != nil {
		t.Fatalf("empty-string fragment rejected: %v", err)
	}
	if hasContent {
		t.Fatal("empty-string fragment reported hasContent=true")
	}
	if !emptyFragment {
		t.Fatal("empty-string fragment did not set emptyFragment=true")
	}
}

// The full verboo glm-5.3-flash terminal shape: a chunk with
// delta.function_call {"arguments":"","name":""}, finish_reason stop,
// full usage, followed by a usage-only tail and [DONE]. The stream must
// complete without an error terminal.
func TestLegacyFunctionCallEmptyStringTerminalStream(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	sse := "data: {\"id\":\"chatcmpl-v1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"42\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-v1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{\"function_call\":{\"arguments\":\"\",\"name\":\"\"}},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":10,\"prompt_tokens\":20,\"total_tokens\":30}}\n\n" +
		"data: {\"id\":\"chatcmpl-v1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.3-flash\",\"choices\":[],\"usage\":{\"completion_tokens\":10,\"prompt_tokens\":20,\"total_tokens\":30}}\n\n" +
		"data: [DONE]\n\n"
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"glm-5.3-flash","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `"type":"error"`) {
		t.Fatalf("error terminal in empty-string fragment stream: %q", body)
	}
	if !strings.Contains(body, `"type":"message_stop"`) {
		t.Fatalf("missing message_stop: %q", body)
	}
	if !strings.Contains(body, `"type":"text"`) || !strings.Contains(body, "42") {
		t.Fatalf("missing text content: %q", body)
	}
}

// The optional usage-only tail chunk (stream_options.include_usage) is a
// pure accounting frame; some gateways (observed on the verboo glm stream)
// omit the envelope identity on it. The tail decodes without id, model, or
// created, while a content-bearing chunk missing its identity still rejects.
func TestUsageOnlyTailIdentityOptionalDecode(t *testing.T) {
	chunk, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(`{"choices":[],"object":"chat.completion.chunk","usage":{"completion_tokens":124,"prompt_tokens":26,"total_tokens":209}}`)})
	if err != nil {
		t.Fatalf("identity-less usage-only tail rejected: %v", err)
	}
	if chunk.Usage == nil || chunk.Usage.TotalTokens != 209 {
		t.Fatalf("tail usage lost: %+v", chunk.Usage)
	}

	for _, body := range []string{
		// content-bearing chunk (one choice) with no id still rejects
		`{"object":"chat.completion.chunk","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"x"}}]}`,
		// a frame with neither usage nor identity is not a tail and still rejects
		`{"object":"chat.completion.chunk","model":"m","created":1,"choices":[]}`,
	} {
		if _, err := chatStreamChunkFromSSE(SSEEvent{Data: []byte(body)}); err == nil {
			t.Fatalf("non-tail fragment accepted: %s", body)
		}
	}
}

// The full streaming lifecycle with an identity-less usage-only tail
// completes to exactly one message_stop terminal: content chunk, finish
// chunk, tail without id/model/created, then [DONE].
func TestStreamLifecycleIdentityLessUsageTail(t *testing.T) {
	mapping := messagesMapping(t, UpstreamChatCompletions)
	mapping.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
		FeatureUsageUnknown:           {},
	}}
	sse := "data: {\"id\":\"chatcmpl-t\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"42\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-t\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"object\":\"chat.completion.chunk\",\"usage\":{\"completion_tokens\":5,\"prompt_tokens\":7,\"total_tokens\":12}}\n\n" +
		"data: [DONE]\n\n"
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `"type":"error"`) {
		t.Fatalf("error terminal in identity-less tail stream: %q", body)
	}
	if strings.Count(body, `"type":"message_stop"`) != 1 {
		t.Fatalf("message_stop count != 1: %q", body)
	}
	if !strings.Contains(body, `"type":"text"`) || !strings.Contains(body, "42") {
		t.Fatalf("missing text content: %q", body)
	}
}
