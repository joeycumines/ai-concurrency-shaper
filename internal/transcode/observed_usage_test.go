package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

func rewriteObservedUsage(t *testing.T, data []byte, stream bool, field string, usage any) []byte {
	t.Helper()
	rewrite := func(data []byte) []byte {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(data, &envelope); err != nil {
			t.Fatal(err)
		}
		if _, exists := envelope[field]; exists {
			if usage == nil {
				delete(envelope, field)
			} else {
				value, err := json.Marshal(usage)
				if err != nil {
					t.Fatal(err)
				}
				envelope[field] = value
			}
		}
		// Anthropic message_start nests its initial usage in message.
		if nested, ok := envelope["message"]; ok {
			var message map[string]json.RawMessage
			if err := json.Unmarshal(nested, &message); err != nil {
				t.Fatal(err)
			}
			if _, ok := message[field]; ok && usage != nil {
				message[field], _ = json.Marshal(usage)
				envelope["message"], _ = json.Marshal(message)
			}
		}
		if nested, ok := envelope["response"]; ok {
			var response map[string]json.RawMessage
			if err := json.Unmarshal(nested, &response); err != nil {
				t.Fatal(err)
			}
			if _, ok := response[field]; ok && usage != nil {
				response[field], _ = json.Marshal(usage)
				envelope["response"], _ = json.Marshal(response)
			}
		}
		out, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if !stream {
		return rewrite(data)
	}
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("data: ")) && !bytes.Equal(line, []byte("data: [DONE]")) {
			lines[i] = append([]byte("data: "), rewrite(line[6:])...)
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

func TestObservationKnownZeroAcrossEverySourceJSONAndSSE(t *testing.T) {
	gemini := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7},"modelVersion":"m","responseId":"g1"}`)
	for _, tc := range []struct {
		name            string
		client          ClientProtocol
		upstream        UpstreamProtocol
		field, request  string
		batch, streamed []byte
		zero            map[string]any
		total           bool
	}{
		{"chat", ClientResponses, UpstreamChatCompletions, "usage", `{"model":"m","input":"hi"}`, testcorpus.ChatCompletionsResponseJSON(), testcorpus.ChatCompletionsStreamSSE(), map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}, true},
		{"responses", ClientMessages, UpstreamResponses, "usage", `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`, testcorpus.ResponsesResponseJSON(), testcorpus.ResponsesStreamSSE(), map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0, "input_tokens_details": map[string]int{"cached_tokens": 0}, "output_tokens_details": map[string]int{"reasoning_tokens": 0}}, true},
		{"messages", ClientGemini, UpstreamMessages, "usage", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":100}}`, testcorpus.AnthropicMessagesResponseJSON(), testcorpus.AnthropicMessagesStreamSSE(), map[string]any{"input_tokens": 0, "output_tokens": 0}, false},
		{"gemini", ClientResponses, UpstreamGemini, "usageMetadata", `{"model":"m","input":"hi"}`, gemini, append(append([]byte("data: "), gemini...), []byte("\n\n")...), map[string]any{"promptTokenCount": 0, "candidatesTokenCount": 0, "totalTokenCount": 0}, true},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				data, request := tc.batch, tc.request
				if stream {
					data = tc.streamed
					if tc.client != ClientGemini {
						request = strings.TrimSuffix(request, "}") + `,"stream":true}`
					}
				}
				data = rewriteObservedUsage(t, data, stream, tc.field, tc.zero)
				_, fact, out, rec, _ := observationExchange(t, diagnosticMapping(tc.client, tc.upstream), []byte(request), data, stream, BodyLimits{})
				if !out.DownstreamComplete || out.LocalFailure || out.UpstreamFailure || fact.Usage == nil {
					t.Fatalf("valid zero usage failed: %+v usage=%+v body=%s", out, fact.Usage, rec.Body.String())
				}
				u := fact.Usage
				if !u.InputKnown || !u.OutputKnown || u.TotalKnown != tc.total || u.Input != 0 || u.Output != 0 || u.Total != 0 {
					t.Fatalf("zero became unknown or derived: %+v", u)
				}
			})
		}
	}
}

func TestObservationAbsentAndDerivedUsageRemainUnknown(t *testing.T) {
	for _, stream := range []bool{false, true} {
		data, request := testcorpus.ChatCompletionsResponseJSON(), `{"model":"m","input":"hi"}`
		if stream {
			data = testcorpus.ChatCompletionsStreamSSE()
			request = `{"model":"m","input":"hi","stream":true}`
		}
		data = rewriteObservedUsage(t, data, stream, "usage", nil)
		_, fact, out, _, _ := observationExchange(t, diagnosticMapping(ClientResponses, UpstreamChatCompletions), []byte(request), data, stream, BodyLimits{})
		if !out.DownstreamComplete || fact.Usage != nil {
			t.Fatalf("absent usage fabricated: stream=%v %+v %+v", stream, out, fact.Usage)
		}
	}
	for _, absent := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		t.Run(absent, func(t *testing.T) {
			u := map[string]int{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}
			delete(u, absent)
			data := rewriteObservedUsage(t, testcorpus.ChatCompletionsStreamSSE(), true, "usage", u)
			_, fact, out, _, _ := observationExchange(t, diagnosticMapping(ClientResponses, UpstreamChatCompletions), []byte(`{"model":"m","input":"hi","stream":true}`), data, true, BodyLimits{})
			if !out.DownstreamComplete || fact.Usage == nil {
				t.Fatalf("failed %+v", out)
			}
			if fact.Usage.InputKnown != (absent != "prompt_tokens") || fact.Usage.OutputKnown != (absent != "completion_tokens") || fact.Usage.TotalKnown != (absent != "total_tokens") {
				t.Fatalf("derived field became reported: %+v", fact.Usage)
			}
		})
	}
}

func TestObservationDeliveryFailureDoesNotPublishFinalUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		data, request := testcorpus.ChatCompletionsResponseJSON(), `{"model":"m","input":"hi"}`
		if stream {
			data = testcorpus.ChatCompletionsStreamSSE()
			request = `{"model":"m","input":"hi","stream":true}`
		}
		evidence, fact, out, _, _ := observationExchange(t, diagnosticMapping(ClientResponses, UpstreamChatCompletions), []byte(request), data, stream, BodyLimits{}, func(http.ResponseWriter) http.ResponseWriter { return &shortWriteResponseWriter{} })
		if out.DownstreamComplete || fact.Usage != nil || len(evidence.Failures) == 0 {
			t.Fatalf("failed delivery published final usage: stream=%v %+v %+v", stream, out, fact.Usage)
		}
	}
}

func TestFlowRecorderJoinsLogicalRequestIdentity(t *testing.T) {
	hub := observation.New(observation.Config{Providers: []string{"provider"}, ManualTicks: true})
	defer hub.Close(context.Background())
	ctx, request := hub.Begin(context.Background(), observation.BeginInfo{Provider: "provider"})
	dir := t.TempDir()
	fl := newFlowCapture(dir)
	fl.setClientRequest(httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx))
	fl.seal()
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("flow record missing %v", err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record flowRecord
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.RequestID != request.ID() || record.ID == "" {
		t.Fatalf("missing logical/flow ID join %+v", record)
	}
}
