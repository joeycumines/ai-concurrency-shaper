package transcode

// Replayed function_call history names the Responses tool namespace its call
// belonged to. That qualifier is client-sent data: a Responses target carries
// it natively, so the canonical part keeps it and each flattening renderer
// decides for ITS exchange whether the qualifier can be mapped. A qualifier the
// exchange does not declare is the namespace_replay_undeclared loss when the
// resulting bare name is unowned; when a surviving tool owns that name the
// exchange is refused, because forwarding it would re-point a client-sent call
// at a different tool.
//
// The per-exchange placement is what makes proxy-reconstructed history work:
// the continuity store retains canonical turns and never re-decodes them, so a
// decision written into a part at decode time is either missing (a follow-up
// that declares no tools) or stale (a later exchange that declares the
// qualifier again).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// compactionRequestBody is the shape a Codex auto-compaction request carries:
// no tools at all while the thread's namespaced function calls replay in the
// history, and tool_choice left at the client's "auto". It stands for observed
// wire from the 2026-11-10 capture; the top-level controls the capture carried
// that a chat request cannot express are covered by their own keys.
const compactionRequestBody = `{
	"model": "m",
	"tools": [],
	"tool_choice": "auto",
	"store": false,
	"input": [
		{"type": "message", "role": "user",
		 "content": [{"type": "input_text", "text": "delegate the review"}]},
		{"type": "function_call", "call_id": "call-1", "name": "spawn_agent",
		 "namespace": "multi_agent_v1", "arguments": "{\"message\":\"review it\"}"},
		{"type": "function_call_output", "call_id": "call-1", "output": "agent-1"},
		{"type": "message", "role": "user",
		 "content": [{"type": "input_text",
		  "text": "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task."}]}
	]
}`

func compactionLossPolicy() LossPolicy {
	return LossPolicy{Allowed: map[Feature]struct{}{
		FeatureNamespaceReplayUndeclared: {},
	}}
}

// namespacedToolRequest declares the multi_agent_v1 namespace with one child,
// so a replay of that child resolves to its flat wire name.
const namespacedToolRequest = `{
	"model": "m",
	"input": "delegate",
	"tools": [
		{"type": "namespace", "name": "multi_agent_v1", "description": "agents", "tools": [
			{"type": "function", "name": "spawn_agent", "description": "s", "strict": false,
			 "parameters": {"type": "object", "properties": {"message": {"type": "string"}}}}]}
	]
}`

// withReplay appends a replayed namespaced call (and its result) to a
// namespaced tool request.
func withReplay(body, namespace string) string {
	replay := `[
		{"type": "function_call", "call_id": "call-1", "name": "spawn_agent",
		 "namespace": ` + strconv.Quote(namespace) + `, "arguments": "{\"message\":\"x\"}"},
		{"type": "function_call_output", "call_id": "call-1", "output": "ok"}
	]`
	var request map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		panic(err)
	}
	request["input"] = json.RawMessage(replay)
	merged, err := json.Marshal(request)
	if err != nil {
		panic(err)
	}
	return string(merged)
}

// chatHistoryName extracts the tool-call name the chat history carries for the
// replayed call.
func chatHistoryName(t *testing.T, upstreamBody string) string {
	t.Helper()
	var chat struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(upstreamBody), &chat); err != nil {
		t.Fatalf("unmarshal chat request: %v\n%s", err, upstreamBody)
	}
	for _, message := range chat.Messages {
		for _, call := range message.ToolCalls {
			if call.Function.Name != "" {
				return call.Function.Name
			}
		}
	}
	return ""
}

// TestCompactionRequestReplaysNamespacedCallsAsBareNames pins the outcome the
// default deployment must serve: a tool-less request that replays the thread's
// namespaced calls converts to a chat request whose history carries the bare
// child name, the drop is recorded exactly once, and a target that carries the
// qualifier natively still receives it.
func TestCompactionRequestReplaysNamespacedCallsAsBareNames(t *testing.T) {
	t.Run("decode preserves the client-sent qualifier", func(t *testing.T) {
		result, _, err := DecodeResponsesRequest([]byte(compactionRequestBody), compactionLossPolicy())
		if err != nil {
			t.Fatalf("tool-less compaction replay must decode under the approved loss: %v", err)
		}
		var calls []CanonicalFunctionCall
		for _, turn := range result.Request.Turns {
			for _, part := range turn.Parts {
				if call, ok := part.(CanonicalFunctionCall); ok {
					calls = append(calls, call)
				}
			}
		}
		if len(calls) != 1 {
			t.Fatalf("function calls = %d, want the one replayed call", len(calls))
		}
		if calls[0].Name != "spawn_agent" || calls[0].Namespace != "multi_agent_v1" {
			t.Fatalf("replayed call = name %q namespace %q, want the bare name with its client-sent qualifier intact",
				calls[0].Name, calls[0].Namespace)
		}
		// The decision belongs to the render stage, so decode records nothing.
		for _, loss := range result.Report.Losses {
			if loss.Feature == FeatureNamespaceReplayUndeclared {
				t.Fatalf("decode recorded %+v, want the flattening render to own this decision", loss)
			}
		}
	})

	t.Run("chat render records one loss and carries the bare name", func(t *testing.T) {
		result, echo, err := DecodeResponsesRequest([]byte(compactionRequestBody), compactionLossPolicy())
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		context := testExchangeContext()
		context.LossPolicy = compactionLossPolicy()
		context.OriginalResponsesRequest = echo
		context.ToolNames = result.ToolNames
		body, report, err := RenderChatRequest(result.Request, context, ChatCapabilities{})
		if err != nil {
			t.Fatalf("chat render: %v", err)
		}
		if name := chatHistoryName(t, string(body)); name != "spawn_agent" {
			t.Fatalf("chat history call name = %q, want the bare child name\n%s", name, body)
		}
		if strings.Contains(string(body), "multi_agent_v1") {
			t.Fatalf("the undeclared namespace leaked into the chat request: %s", body)
		}
		var losses []ConversionLoss
		for _, loss := range report.Losses {
			if loss.Feature == FeatureNamespaceReplayUndeclared {
				losses = append(losses, loss)
			}
		}
		if len(losses) != 1 || losses[0].Kind != LossRecord {
			t.Fatalf("namespace losses = %+v, want exactly one approved loss", losses)
		}
		if !strings.Contains(losses[0].Path, ".parts[0]") {
			t.Fatalf("loss path = %q, want it to name the assistant turn part that carried the call", losses[0].Path)
		}
	})

	t.Run("responses render carries the qualifier", func(t *testing.T) {
		result, echo, err := DecodeResponsesRequest([]byte(compactionRequestBody), compactionLossPolicy())
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		context := testExchangeContext()
		context.LossPolicy = compactionLossPolicy()
		context.OriginalResponsesRequest = echo
		context.ToolNames = result.ToolNames
		body, _, err := RenderResponsesRequest(result.Request, context)
		if err != nil {
			t.Fatalf("responses render: %v", err)
		}
		var out struct {
			Input []struct {
				Type      string `json:"type"`
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"input"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("unmarshal responses request: %v\n%s", err, body)
		}
		found := false
		for _, item := range out.Input {
			if item.Type == "function_call" && item.Name == "spawn_agent" {
				found = true
				if item.Namespace != "multi_agent_v1" {
					t.Fatalf("function_call namespace = %q, want the client-sent qualifier carried through\n%s",
						item.Namespace, body)
				}
			}
		}
		if !found {
			t.Fatalf("replayed function_call missing from the responses request: %s", body)
		}
	})

	t.Run("strict policy still names the refused feature", func(t *testing.T) {
		result, echo, err := DecodeResponsesRequest([]byte(compactionRequestBody), compactionLossPolicy())
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		context := testExchangeContext() // StrictLossPolicy
		context.OriginalResponsesRequest = echo
		context.ToolNames = result.ToolNames
		_, _, err = RenderChatRequest(result.Request, context, ChatCapabilities{})
		if err == nil {
			t.Fatal("want the strict policy to refuse the unapproved qualifier loss")
		}
		if !strings.Contains(err.Error(), string(FeatureNamespaceReplayUndeclared)) {
			t.Fatalf("error = %q, want it to name %s", err, FeatureNamespaceReplayUndeclared)
		}
	})

	t.Run("owned bare name is still refused", func(t *testing.T) {
		body := strings.Replace(compactionRequestBody,
			`"tools": [],`,
			`"tools": [{"type": "function", "name": "spawn_agent", "description": "plain", "strict": false,`+
				` "parameters": {"type": "object", "properties": {}}}],`, 1)
		result, echo, err := DecodeResponsesRequest([]byte(body), compactionLossPolicy())
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		context := testExchangeContext()
		context.LossPolicy = compactionLossPolicy()
		context.OriginalResponsesRequest = echo
		context.ToolNames = result.ToolNames
		_, _, err = RenderChatRequest(result.Request, context, ChatCapabilities{})
		if err == nil {
			t.Fatal("want a refusal: the bare name belongs to a surviving tool, so the replay would be re-pointed")
		}
		if !strings.Contains(err.Error(), "already used by another declared tool") {
			t.Fatalf("error = %q, want it to name the collision", err)
		}
	})

	t.Run("every flattening renderer carries the bare name", func(t *testing.T) {
		result, echo, err := DecodeResponsesRequest([]byte(compactionRequestBody), compactionLossPolicy())
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		result.Request.MaxOutputTokens = new(64)

		geminiContext := testExchangeContext()
		geminiContext.LossPolicy = permissivePolicy()
		geminiContext.GeminiSignatures = NewGeminiSignatureStore(GeminiSignatureConfig{})
		geminiContext.ToolNames = result.ToolNames
		geminiContext.OriginalResponsesRequest = echo
		geminiBody, _, err := RenderGeminiRequest(result.Request, geminiContext)
		if err != nil {
			t.Fatalf("gemini render: %v", err)
		}
		if !strings.Contains(string(geminiBody), `"name":"spawn_agent"`) ||
			strings.Contains(string(geminiBody), "multi_agent_v1") {
			t.Fatalf("gemini render did not carry the bare call name: %s", geminiBody)
		}

		messagesContext := testExchangeContext()
		messagesContext.LossPolicy = permissivePolicy()
		messagesContext.ToolNames = result.ToolNames
		messagesContext.OriginalResponsesRequest = echo
		messagesContext.RequestedClientModel = "m"
		messagesContext.UpstreamModel = "claude-x"
		messagesBody, _, err := RenderMessagesRequest(result.Request, messagesContext)
		if err != nil {
			t.Fatalf("messages render: %v", err)
		}
		if !strings.Contains(string(messagesBody), `"name":"spawn_agent"`) ||
			strings.Contains(string(messagesBody), "multi_agent_v1") {
			t.Fatalf("messages render did not carry the bare call name: %s", messagesBody)
		}
	})

	t.Run("handler and chat render carry the bare name", func(t *testing.T) {
		mapping := responsesMapping(t)
		mapping.LossPolicy = compactionLossPolicy()
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
		req := httptest.NewRequest(http.MethodPost, "/v1/responses",
			strings.NewReader(compactionRequestBody))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(upstreamBody, "multi_agent_v1") {
			t.Fatalf("the undeclared namespace leaked into the chat request: %s", upstreamBody)
		}
		if name := chatHistoryName(t, upstreamBody); name != "spawn_agent" {
			t.Fatalf("the replayed call is missing from the chat history: %s", upstreamBody)
		}
		// The captured request carries tool_choice:"auto" with no tools. The
		// decode deliberately passes a client-sent choice through when the
		// CLIENT declared no tools (reconcileToolChoice only owns the
		// converter-caused emptiness), so the fixture is rendered faithfully
		// instead of being silently adjusted here.
		var chatRequest struct {
			ToolChoice json.RawMessage `json:"tool_choice"`
			Tools      json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal([]byte(upstreamBody), &chatRequest); err != nil {
			t.Fatalf("unmarshal chat request: %v\n%s", err, upstreamBody)
		}
		if string(chatRequest.ToolChoice) != `"auto"` {
			t.Fatalf("chat tool_choice = %s, want the client-sent \"auto\" passed through", chatRequest.ToolChoice)
		}
		if !strings.Contains(rec.Body.String(), `"type":"message"`) {
			t.Fatalf("downstream body is not a completed Responses message: %s", rec.Body.String())
		}
	})
}

// TestNamespaceReplayDecisionBelongsToTheRenderingExchange pins the placement
// that makes continuity possible: the same canonical part renders the declared
// flat name for an exchange that declares the qualifier and the bare name for
// one that does not, because the canonical part itself is never rewritten.
func TestNamespaceReplayDecisionBelongsToTheRenderingExchange(t *testing.T) {
	result, echo, err := DecodeResponsesRequest(
		[]byte(withReplay(namespacedToolRequest, "multi_agent_v1")),
		compactionLossPolicy(),
	)
	if err != nil {
		t.Fatalf("decode declared replay: %v", err)
	}

	render := func(t *testing.T, request CanonicalRequest, toolNames *ToolNames) (string, ConversionReport) {
		t.Helper()
		context := testExchangeContext()
		context.LossPolicy = compactionLossPolicy()
		context.OriginalResponsesRequest = echo
		context.ToolNames = toolNames
		body, report, err := RenderChatRequest(request, context, ChatCapabilities{})
		if err != nil {
			t.Fatalf("chat render: %v", err)
		}
		return string(body), report
	}

	// The exchange that declares the qualifier renders the flat child name and
	// records no loss.
	declaredBody, declaredReport := render(t, result.Request, result.ToolNames)
	if name := chatHistoryName(t, declaredBody); name != "spawn_agent" {
		t.Fatalf("declared render call name = %q, want the flat child name\n%s", name, declaredBody)
	}
	for _, loss := range declaredReport.Losses {
		if loss.Feature == FeatureNamespaceReplayUndeclared {
			t.Fatalf("declared render recorded %+v, want no loss", loss)
		}
	}

	// The same canonical turns rendered for an exchange that declares no tools
	// (the compaction follow-up) carry the bare name and record the loss.
	undeclared := result.Request
	undeclared.Tools = nil
	undeclared.ToolChoice = nil
	undeclaredBody, undeclaredReport := render(t, undeclared, nil)
	if name := chatHistoryName(t, undeclaredBody); name != "spawn_agent" {
		t.Fatalf("undeclared render call name = %q, want the bare child name\n%s", name, undeclaredBody)
	}
	counted := 0
	for _, loss := range undeclaredReport.Losses {
		if loss.Feature == FeatureNamespaceReplayUndeclared {
			counted++
		}
	}
	if counted != 1 {
		t.Fatalf("undeclared render recorded %d namespace losses, want exactly 1: %+v", counted, undeclaredReport.Losses)
	}

	// Rendering is pure: the canonical part still carries the qualifier, so a
	// later exchange that declares it again is unaffected.
	var namespaces []string
	for _, turn := range result.Request.Turns {
		for _, part := range turn.Parts {
			if call, ok := part.(CanonicalFunctionCall); ok {
				namespaces = append(namespaces, call.Namespace)
			}
		}
	}
	if len(namespaces) != 1 || namespaces[0] != "multi_agent_v1" {
		t.Fatalf("canonical call namespaces = %v, want the qualifier preserved across renders", namespaces)
	}
}

// TestContinuityRetainedNamespacedReplayRendersForEachExchange pins the
// cross-feature interaction: history retained by the continuity store keeps its
// qualifier, and a follow-up exchange decides it for itself. A tool-less
// compaction follow-up is served under the bare name; a follow-up that declares
// the qualifier renders the flat child name; neither decision mutates the
// retained chain.
func TestContinuityRetainedNamespacedReplayRendersForEachExchange(t *testing.T) {
	chatReply := func(text string) func(*http.Request) (*http.Response, error) {
		return func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"chatcmpl-1","object":"chat.completion","created":1710000000,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"spawn_agent","arguments":"{\"message\":\"x\"}"}}]}}]}`,
				)),
			}, nil
		}
	}

	var upstreamBodies []string
	mapping := responsesMapping(t)
	mapping.ModelMap = ModelMap{AllowIdentity: true}
	mapping.LossPolicy = compactionLossPolicy()
	mapping.Auth = AuthPolicy{Mode: AuthNone}
	mapping.ChatCapabilities = ChatCapabilities{ParallelToolCalls: true}
	store := NewContinuityStore(ContinuityConfig{Capacity: 8, TTL: time.Hour})
	handler := NewTranscodeHandler(
		HandlerConfig{
			Mapping:       mapping,
			Upstream:      mustParseURL(t, "https://upstream.example"),
			BodyLimits:    BodyLimits{AcceptedRequestBytes: 1 << 20, SuccessfulResponseBytes: 1 << 20},
			Continuity:    store,
			ContinuityKey: "test-map",
		},
		func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			upstreamBodies = append(upstreamBodies, string(body))
			return chatReply("answer")(req)
		},
		nil,
	)

	post := func(body string) (int, []byte) {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}

	code, first := post(namespacedToolRequest)
	if code != http.StatusOK {
		t.Fatalf("turn 1 status = %d: %s", code, first)
	}
	var firstEnvelope struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(first, &firstEnvelope); err != nil {
		t.Fatalf("turn 1 body: %v\n%s", err, first)
	}
	if firstEnvelope.ID == "" {
		t.Fatal("turn 1 emitted no response id")
	}

	// The retained chain holds the qualifier the exchange declared.
	retained, _, ok := resolveContinuityChain(store, "test-map", firstEnvelope.ID)
	if !ok {
		t.Fatal("turn 1 chain was not retained")
	}
	var retainedNamespaces []string
	for _, turn := range retained {
		for _, part := range turn.Parts {
			if call, ok := part.(CanonicalFunctionCall); ok {
				retainedNamespaces = append(retainedNamespaces, call.Namespace)
			}
		}
	}
	if len(retainedNamespaces) != 1 || retainedNamespaces[0] != "multi_agent_v1" {
		t.Fatalf("retained call namespaces = %v, want the declared qualifier preserved", retainedNamespaces)
	}

	// A tool-less follow-up (the compaction shape) must be served: the retained
	// qualifier is this follow-up's loss, recorded where it renders.
	compactionFollowUp := `{"model":"m","tools":[],"tool_choice":"auto","store":false,` +
		`"previous_response_id":` + strconv.Quote(firstEnvelope.ID) + `,` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text",` +
		`"text":"You are performing a CONTEXT CHECKPOINT COMPACTION."}]}]}`
	code, second := post(compactionFollowUp)
	if code != http.StatusOK {
		t.Fatalf("tool-less continuity follow-up status = %d, want the retained replay served: %s", code, second)
	}
	if len(upstreamBodies) != 2 {
		t.Fatalf("upstream saw %d exchanges, want 2", len(upstreamBodies))
	}
	if strings.Contains(upstreamBodies[1], "multi_agent_v1") {
		t.Fatalf("the undeclared namespace leaked into the follow-up chat request: %s", upstreamBodies[1])
	}
	if name := chatHistoryName(t, upstreamBodies[1]); name != "spawn_agent" {
		t.Fatalf("follow-up chat history call name = %q, want the bare child name\n%s", name, upstreamBodies[1])
	}

	// The retained chain is still intact, so a follow-up that declares the
	// namespace renders the flat child name rather than misattributing it.
	retainedAgain, _, ok := resolveContinuityChain(store, "test-map", firstEnvelope.ID)
	if !ok {
		t.Fatal("chain was dropped by the tool-less follow-up")
	}
	for _, turn := range retainedAgain {
		for _, part := range turn.Parts {
			if call, ok := part.(CanonicalFunctionCall); ok && call.Namespace != "multi_agent_v1" {
				t.Fatalf("retained call namespace = %q after the tool-less follow-up, want it unmutated", call.Namespace)
			}
		}
	}
}
