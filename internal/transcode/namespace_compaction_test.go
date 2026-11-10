package transcode

// A Codex auto-compaction exchange declares no tools and asks the model to
// summarize the thread, while its input still replays the thread's
// function_call history. The conversation heard on the wire: the client
// stopped declaring tools, but the history still names the namespace each
// call belonged to (multi_agent_v1 and the MCP groups). With no declared
// tool there is nothing to map the qualifier onto, so the bare child name is
// the only truthful representation the upstream can receive. The approved
// loss must therefore reach the chat renderer as a bare call: a replayed
// namespace that the request does not declare is recorded once and cleared,
// never re-refused after the policy has decided it.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const compactionRequestBody = `{
	"model": "m",
	"tools": [],
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

func TestCompactionRequestReplaysNamespacedCallsAsBareNames(t *testing.T) {
	t.Run("decode records one loss and clears the qualifier", func(t *testing.T) {
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
		if calls[0].Name != "spawn_agent" || calls[0].Namespace != "" {
			t.Fatalf("replayed call = name %q namespace %q, want the bare name with the qualifier cleared",
				calls[0].Name, calls[0].Namespace)
		}
		var losses []ConversionLoss
		for _, loss := range result.Report.Losses {
			if loss.Feature == FeatureNamespaceReplayUndeclared {
				losses = append(losses, loss)
			}
		}
		if len(losses) != 1 || losses[0].Kind != LossRecord {
			t.Fatalf("namespace losses = %+v, want exactly one approved loss", losses)
		}
		if losses[0].Path != "input[1].namespace" {
			t.Fatalf("loss path = %q, want input[1].namespace", losses[0].Path)
		}
	})

	t.Run("strict policy still names the refused feature", func(t *testing.T) {
		_, _, err := DecodeResponsesRequest([]byte(compactionRequestBody), StrictLossPolicy())
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
		_, _, err := DecodeResponsesRequest([]byte(body), compactionLossPolicy())
		if err == nil {
			t.Fatal("want a refusal: the bare name belongs to a surviving tool, so the replay would be re-pointed")
		}
		if !strings.Contains(err.Error(), "already used by another declared tool") {
			t.Fatalf("error = %q, want it to name the collision", err)
		}
	})

	t.Run("every flattening renderer carries the bare name", func(t *testing.T) {
		result, _, err := DecodeResponsesRequest([]byte(compactionRequestBody), compactionLossPolicy())
		if err != nil {
			t.Fatalf("decode: %v", err)
		}

		geminiContext := testExchangeContext()
		geminiContext.LossPolicy = permissivePolicy()
		geminiContext.GeminiSignatures = NewGeminiSignatureStore(GeminiSignatureConfig{})
		geminiBody, _, err := RenderGeminiRequest(result.Request, geminiContext)
		if err != nil {
			t.Fatalf("gemini render: %v", err)
		}
		if !strings.Contains(string(geminiBody), `"name":"spawn_agent"`) ||
			strings.Contains(string(geminiBody), "multi_agent_v1") {
			t.Fatalf("gemini render did not carry the bare call name: %s", geminiBody)
		}

		maxOutput := 64
		result.Request.MaxOutputTokens = &maxOutput
		messagesContext := testExchangeContext()
		messagesContext.LossPolicy = permissivePolicy()
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
			t.Fatalf("unmarshal chat request: %v", err)
		}
		found := false
		for _, message := range chat.Messages {
			for _, call := range message.ToolCalls {
				if call.Function.Name == "spawn_agent" {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("the replayed call is missing from the chat history: %s", upstreamBody)
		}
		if !strings.Contains(rec.Body.String(), `"type":"message"`) {
			t.Fatalf("downstream body is not a completed Responses message: %s", rec.Body.String())
		}
	})
}
