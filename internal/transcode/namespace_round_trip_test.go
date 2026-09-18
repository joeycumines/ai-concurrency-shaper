package transcode

// The Responses namespace-tool round trip: a client that declares
// namespace tools (the Codex CLI's multi_agent_v1 / MCP groupings) gets the
// children flattened into flat chat function tools, and a model call to a
// flattened child comes back as a function_call carrying the bare child name
// plus its separate namespace qualifier.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

const namespaceToolRequestBody = `{
	"model": "m",
	"input": "spawn a subagent",
	"tools": [
		{"type": "function", "name": "exec_command", "description": "run", "strict": false,
		 "parameters": {"type": "object", "properties": {}, "additionalProperties": false}},
		{"type": "namespace", "name": "multi_agent_v1",
		 "description": "Tools for spawning and managing sub-agents.",
		 "tools": [
			{"type": "function", "name": "spawn_agent", "description": "spawn", "strict": false,
			 "parameters": {"type": "object", "properties": {"message": {"type": "string"}}, "additionalProperties": false}}
		 ]}
	]
}`

func TestNamespaceToolRoundTripNonStreaming(t *testing.T) {
	mapping := responsesMapping(t)
	var upstreamBody string
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		upstreamBody = string(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"spawn_agent","arguments":"{\"message\":\"x\"}"}}]}}]}`,
			)),
		}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(namespaceToolRequestBody))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(upstreamBody, `"name":"spawn_agent"`) {
		t.Fatalf("upstream chat request lacks the flattened child: %s", upstreamBody)
	}
	if strings.Contains(upstreamBody, "namespace") {
		t.Fatalf("namespace grouping leaked into the chat request: %s", upstreamBody)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"type":"function_call"`) {
		t.Fatalf("missing function_call item: %s", out)
	}
	if !strings.Contains(out, `"name":"spawn_agent"`) || !strings.Contains(out, `"namespace":"multi_agent_v1"`) {
		t.Fatalf("function_call lacks the bare name + namespace qualifier: %s", out)
	}
}

func TestNamespaceToolRoundTripStreaming(t *testing.T) {
	mapping := responsesMapping(t)
	stream := "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"spawn_agent\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"message\\\":\\\"x\\\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":\"tool_calls\",\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
		"data: [DONE]\n\n"
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(stream)),
		}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(strings.Replace(namespaceToolRequestBody, `"input": "spawn a subagent"`, `"input": "spawn a subagent", "stream": true`, 1)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"namespace":"multi_agent_v1"`) {
		t.Fatalf("streamed function_call lacks the namespace qualifier: %s", out)
	}
	if !strings.Contains(out, `"name":"spawn_agent"`) {
		t.Fatalf("streamed function_call lacks the bare child name: %s", out)
	}
	if strings.Count(out, `"type":"function_call"`) != 3 {
		t.Fatalf("want the function_call on output_item.added, .done, and the terminal envelope: %s", out)
	}
	if strings.Count(out, `"namespace":"multi_agent_v1"`) != 3 {
		t.Fatalf("every function_call shape must carry the namespace qualifier: %s", out)
	}
}

func TestNamespaceToolCollisionQualifiesChild(t *testing.T) {
	mapping := responsesMapping(t)
	var upstreamBody string
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		upstreamBody = string(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"ns2__search","arguments":"{}"}}]}}]}`,
			)),
		}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model": "m",
		"input": "search",
		"tools": [
			{"type": "function", "name": "search", "description": "plain", "strict": false,
			 "parameters": {"type": "object", "properties": {}}},
			{"type": "namespace", "name": "ns1", "description": "one", "tools": [
				{"type": "function", "name": "search", "description": "ns1 search", "strict": false,
				 "parameters": {"type": "object", "properties": {}}}]},
			{"type": "namespace", "name": "ns2", "description": "two", "tools": [
				{"type": "function", "name": "search", "description": "ns2 search", "strict": false,
				 "parameters": {"type": "object", "properties": {}}}]}
		]
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"name":"search"`, `"name":"ns1__search"`, `"name":"ns2__search"`} {
		if !strings.Contains(upstreamBody, want) {
			t.Fatalf("upstream tools lack %s: %s", want, upstreamBody)
		}
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"type":"function_call","status":"completed","call_id":"call-1","name":"search","arguments":"{}","namespace":"ns2"`) {
		t.Fatalf("collision call must render the BARE child name plus the qualifier: %s", out)
	}
	if strings.Contains(out, "ns2__search") {
		t.Fatalf("the flattened chat name leaked to the client: %s", out)
	}
}

func TestNamespaceHistoryReplayUsesFlatName(t *testing.T) {
	mapping := responsesMapping(t)
	var upstreamBody string
	handler := testHandler(t, mapping, func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		upstreamBody = string(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"done"}}]}`,
			)),
		}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model": "m",
		"tools": [
			{"type": "function", "name": "spawn_agent", "description": "plain", "strict": false,
			 "parameters": {"type": "object", "properties": {}}},
			{"type": "namespace", "name": "multi_agent_v1", "description": "agents", "tools": [
				{"type": "function", "name": "spawn_agent", "description": "ns child", "strict": false,
				 "parameters": {"type": "object", "properties": {}}}]}
		],
		"input": [
			{"type": "function_call", "call_id": "call-1", "name": "spawn_agent", "namespace": "multi_agent_v1", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call-1", "output": "ok"}
		]
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(upstreamBody, `"name":"multi_agent_v1__spawn_agent"`) {
		t.Fatalf("replayed history must use the flattened name for the namespaced child: %s", upstreamBody)
	}
}

func TestFieldCaptureCodexNamespaceRequestReplayed(t *testing.T) {
	policy := LossPolicy{Allowed: map[Feature]struct{}{
		FeatureBuiltinTools:           {},
		FeatureUsageUnknown:           {},
		FeatureUsageCacheReadUnknown:  {},
		FeatureUsageCacheWriteUnknown: {},
		FeatureUsageReasoningUnknown:  {},
	}}
	result, echo, err := DecodeResponsesRequest(testcorpus.FieldCodexNamespaceRequestJSON(), policy)
	if err != nil {
		t.Fatalf("decode captured Codex namespace request: %v", err)
	}
	found := false
	for _, tool := range result.Request.Tools {
		if tool.Name == "spawn_agent" {
			found = true
		}
	}
	if !found {
		t.Fatalf("spawn_agent was not flattened: %+v", result.Request.Tools)
	}
	if ns := result.ToolNames.FlatToRef["spawn_agent"].Namespace; ns != "multi_agent_v1" {
		t.Fatalf("spawn_agent namespace = %q, want multi_agent_v1", ns)
	}
	if ns := result.ToolNames.FlatToRef["_uninstall_app"].Namespace; ns != "mcp__codex_apps__plugin_management" {
		t.Fatalf("MCP child namespace = %q", ns)
	}

	context := testExchangeContext()
	context.LossPolicy = policy
	context.OriginalResponsesRequest = echo
	context.ToolNames = result.ToolNames
	context.RequestedClientModel = "glm-5.3-flash"
	context.UpstreamModel = "glm-5.3-flash"

	rendered, _, err := RenderChatRequest(result.Request, context, ChatCapabilities{})
	if err != nil {
		t.Fatalf("render chat request: %v", err)
	}
	if strings.Contains(string(rendered), "namespace") {
		t.Fatalf("namespace grouping leaked into the chat request: %s", rendered)
	}
	if !strings.Contains(string(rendered), `"name":"spawn_agent"`) {
		t.Fatalf("flattened child missing from the chat request: %s", rendered)
	}

	chat := `{"id":"c","object":"chat.completion","created":1,"model":"glm-5.3-flash","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"spawn_agent","arguments":"{\"message\":\"x\"}"}}]}}]}`
	response, _, err := DecodeChatResponseWithPolicy([]byte(chat), ChatCapabilities{}, policy)
	if err != nil {
		t.Fatalf("decode chat tool call: %v", err)
	}
	out, _, err := RenderResponsesResponse(response, context)
	if err != nil {
		t.Fatalf("render Responses response: %v", err)
	}
	if !strings.Contains(string(out), `"name":"spawn_agent"`) || !strings.Contains(string(out), `"namespace":"multi_agent_v1"`) {
		t.Fatalf("round-tripped tool call lacks bare name + namespace qualifier: %s", out)
	}
}

func TestNamespaceQualifiedNameCollisionStaysInvertible(t *testing.T) {
	var upstreamBody string
	handler := testHandler(t, responsesMapping(t), func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		upstreamBody = string(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"ns__search_2","arguments":"{}"}}]}}]}`,
			)),
		}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model": "m",
		"input": "search",
		"tools": [
			{"type": "function", "name": "search", "description": "plain", "strict": false,
			 "parameters": {"type": "object", "properties": {}}},
			{"type": "function", "name": "ns__search", "description": "plain qualified-looking", "strict": false,
			 "parameters": {"type": "object", "properties": {}}},
			{"type": "namespace", "name": "ns", "description": "g", "tools": [
				{"type": "function", "name": "search", "description": "child", "strict": false,
				 "parameters": {"type": "object", "properties": {}}}]}
		]
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(upstreamBody, `"name":"ns__search_2"`) {
		t.Fatalf("a qualified name owned by a plain tool must be disambiguated: %s", upstreamBody)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"type":"function_call","status":"completed","call_id":"call-1","name":"search","arguments":"{}","namespace":"ns"`) {
		t.Fatalf("disambiguated call did not map back to the bare child name: %s", out)
	}
}

func TestNestedNamespaceKeepsInnerQualifier(t *testing.T) {
	handler := testHandler(t, responsesMapping(t), func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"inner_tool","arguments":"{}"}}]}}]}`,
			)),
		}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model": "m",
		"input": "go",
		"tools": [
			{"type": "namespace", "name": "outer", "description": "outer", "tools": [
				{"type": "namespace", "name": "inner", "description": "inner", "tools": [
					{"type": "function", "name": "inner_tool", "description": "x", "strict": false,
					 "parameters": {"type": "object", "properties": {}}}]}]}
		]
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"type":"function_call","status":"completed","call_id":"call-1","name":"inner_tool","arguments":"{}","namespace":"inner"`) {
		t.Fatalf("nested child must restore its INNER namespace: %s", out)
	}
}

func TestUndeclaredNamespaceHistoryIsNoted(t *testing.T) {
	policy := StrictLossPolicy()
	result, _, err := DecodeResponsesRequest([]byte(`{
		"model": "m",
		"tools": [
			{"type": "namespace", "name": "declared", "description": "d", "tools": [
				{"type": "function", "name": "known", "description": "x", "strict": false,
				 "parameters": {"type": "object", "properties": {}}}]}
		],
		"input": [
			{"type": "function_call", "call_id": "call-1", "name": "unknown", "namespace": "undeclared", "arguments": "{}"}
		]
	}`), policy)
	if err != nil {
		t.Fatalf("an undeclared namespace history item must not fail the request: %v", err)
	}
	found := false
	for _, loss := range result.Report.Losses {
		if loss.Feature == FeatureOutputItemBoundaries && strings.Contains(loss.Detail, "undeclared") {
			found = true
		}
	}
	if !found {
		t.Fatalf("report = %+v, want an observable note for the undeclared namespace", result.Report.Losses)
	}
	var gotName string
	for _, turn := range result.Request.Turns {
		for _, part := range turn.Parts {
			if call, ok := part.(CanonicalFunctionCall); ok {
				gotName = call.Name
			}
		}
	}
	if gotName != "unknown" {
		t.Fatalf("history call name = %q, want the bare name forwarded", gotName)
	}
}

func TestNamespaceParallelCallsCarryTheirQualifiers(t *testing.T) {
	handler := testHandler(t, responsesMapping(t), func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"spawn_agent","arguments":"{}"}},{"id":"call-2","type":"function","function":{"name":"wait_agent","arguments":"{}"}}]}}]}`,
			)),
		}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model": "m",
		"input": "spawn then wait",
		"tools": [
			{"type": "namespace", "name": "multi_agent_v1", "description": "agents", "tools": [
				{"type": "function", "name": "spawn_agent", "description": "s", "strict": false,
				 "parameters": {"type": "object", "properties": {}}},
				{"type": "function", "name": "wait_agent", "description": "w", "strict": false,
				 "parameters": {"type": "object", "properties": {}}}]}
		]
	}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	first := strings.Index(out, `"type":"function_call","status":"completed","call_id":"call-1","name":"spawn_agent","arguments":"{}","namespace":"multi_agent_v1"`)
	second := strings.Index(out, `"type":"function_call","status":"completed","call_id":"call-2","name":"wait_agent","arguments":"{}","namespace":"multi_agent_v1"`)
	if first < 0 || second < 0 {
		t.Fatalf("parallel calls must each carry name + qualifier: %s", out)
	}
	if first > second {
		t.Fatalf("parallel calls lost their upstream order: %s", out)
	}
}
