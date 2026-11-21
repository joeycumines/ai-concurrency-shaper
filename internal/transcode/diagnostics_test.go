package transcode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/diagnostic"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/testcorpus"
)

// observationExchange runs the real handler with ingress and wire taps. The
// scripted transport consumes exactly the rendered body and returns source wire
// fixtures; the same path is used by the HTTP gateway.
func observationExchange(t *testing.T, m Mapping, clientBody, upstreamBody []byte, stream bool, limits BodyLimits, wrappers ...func(http.ResponseWriter) http.ResponseWriter) (*diagnostic.Snapshot, observation.RequestFact, Outcome, *httptest.ResponseRecorder, int) {
	t.Helper()
	m.ModelMap = ModelMap{AllowIdentity: true}
	m.LossPolicy = LossPolicy{Allowed: allLossKeys()}
	m.Auth = AuthPolicy{Mode: AuthNone}
	m.ChatCapabilities = defaultChatCapabilities()
	hub := observation.New(observation.Config{Providers: []string{"test"}, ManualTicks: true})
	defer hub.Close(context.Background())
	ctx, observed := hub.Begin(context.Background(), observation.BeginInfo{Provider: "test", Method: "POST", Path: m.ClientRoute.Path})
	dc := diagnostic.New(observed.ID(), 1<<16)
	ctx = diagnostic.WithContext(ctx, dc)
	var outcome Outcome
	calls := 0
	handler := NewTranscodeHandler(HandlerConfig{Mapping: m, Upstream: mustParseURL(t, "https://upstream.example"), BodyLimits: limits}, func(r *http.Request) (*http.Response, error) {
		calls++
		rendered, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(rendered) == 0 {
			t.Fatal("empty rendered request")
		}
		ct := "application/json"
		if stream {
			ct = "text/event-stream"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(bytes.NewReader(upstreamBody))}, nil
	}, func(v Outcome) { outcome = v })
	path := m.ClientRoute.Path
	if m.ClientProtocol == ClientGemini {
		path = "/v1beta/models/m:generateContent"
		if stream {
			path = "/v1beta/models/m:streamGenerateContent?alt=sse"
		}
	}
	r := httptest.NewRequest("POST", path, bytes.NewReader(clientBody)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	if stream {
		r.Header.Set("Accept", "text/event-stream")
	}
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Set("X-Stainless-Package-Version", "9.9.9")
	r.Body = dc.WrapBody(diagnostic.ClientRequest, r.Body, "application/json", 0)
	rec := httptest.NewRecorder()
	var base http.ResponseWriter = rec
	for _, wrap := range wrappers {
		base = wrap(base)
	}
	w := diagnostic.WrapWriter(base, dc)
	handler.ServeHTTP(w, r)
	return dc.Seal(), observed.Snapshot(), outcome, rec, calls
}
func diagnosticMapping(client ClientProtocol, upstream UpstreamProtocol) Mapping {
	path := "/v1/responses"
	if client == ClientMessages {
		path = "/v1/messages"
	}
	if client == ClientGemini {
		path = "/v1beta/models/{model}:generateContent"
	}
	upath := "/v1/chat/completions"
	switch upstream {
	case UpstreamResponses:
		upath = "/v1/responses"
	case UpstreamMessages:
		upath = "/v1/messages"
	case UpstreamGemini:
		upath = "/v1beta"
	}
	return Mapping{ClientRoute: RouteKey{Method: "POST", Path: path}, ClientProtocol: client, UpstreamProtocol: upstream, UpstreamPath: upath}
}
func TestObservationAllConversionDirectionsJSONAndSSE(t *testing.T) {
	directions := []struct {
		client   ClientProtocol
		upstream UpstreamProtocol
	}{{ClientResponses, UpstreamChatCompletions}, {ClientResponses, UpstreamGemini}, {ClientMessages, UpstreamChatCompletions}, {ClientMessages, UpstreamResponses}, {ClientMessages, UpstreamGemini}, {ClientGemini, UpstreamChatCompletions}, {ClientGemini, UpstreamResponses}, {ClientGemini, UpstreamMessages}}
	for _, d := range directions {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-%s/stream=%v", d.client, d.upstream, stream), func(t *testing.T) {
				request := `{"model":"m","input":"hi"}`
				switch d.client {
				case ClientMessages:
					request = `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
				case ClientGemini:
					request = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":100}}`
				}
				if stream && d.client != ClientGemini {
					request = strings.TrimSuffix(request, "}") + `,"stream":true}`
				}
				var payload []byte
				var input, output, total int64
				totalKnown := true
				switch d.upstream {
				case UpstreamChatCompletions:
					payload = testcorpus.ChatCompletionsResponseJSON()
					input, output, total = 42, 18, 60
					if stream {
						payload = testcorpus.ChatCompletionsStreamSSE()
					}
				case UpstreamResponses:
					payload = testcorpus.ResponsesResponseJSON()
					input, output, total = 45, 25, 70
					if stream {
						payload = testcorpus.ResponsesStreamSSE()
					}
				case UpstreamMessages:
					payload = testcorpus.AnthropicMessagesResponseJSON()
					input, output = 45, 25
					totalKnown = false
					if stream {
						payload = testcorpus.AnthropicMessagesStreamSSE()
					}
				case UpstreamGemini:
					input, output, total = 5, 2, 7
					payload = []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7},"modelVersion":"m","responseId":"g1"}`)
					if stream {
						payload = append(append([]byte("data: "), payload...), []byte("\n\n")...)
					}
				}
				evidence, fact, out, rec, calls := observationExchange(t, diagnosticMapping(d.client, d.upstream), []byte(request), payload, stream, BodyLimits{})
				if calls != 1 || rec.Code != 200 || !out.DownstreamComplete || out.LocalFailure || out.UpstreamFailure {
					t.Fatalf("failed exchange calls=%d status=%d outcome=%+v body=%s", calls, rec.Code, out, rec.Body.String())
				}
				if fact.Usage == nil {
					t.Fatalf("no source usage: %s", rec.Body.String())
				}
				u := fact.Usage
				if !u.InputKnown || !u.OutputKnown || u.TotalKnown != totalKnown || u.Input != input || u.Output != output || u.Total != total {
					t.Fatalf("usage=%+v want=%d/%d/%d totalKnown=%v", u, input, output, total, totalKnown)
				}
				if evidence.RequestID != fact.ID || evidence.ClientProtocol != string(d.client) || evidence.UpstreamProtocol != string(d.upstream) || len(evidence.Contracts) < 4 {
					t.Fatalf("missing join or contract evidence %+v", evidence)
				}
				for _, leg := range evidence.Legs {
					if !leg.Observed || leg.ObservedBytes <= 0 || leg.CapturedBytes <= 0 {
						t.Fatalf("missing %s evidence", leg.Name)
					}
				}
				if !bytes.Equal(evidence.Legs[0].Body, []byte(request)) || !bytes.Equal(evidence.Legs[3].Body, rec.Body.Bytes()) {
					t.Fatal("client/accepted downstream evidence differs from wire")
				}
			})
		}
	}
}
func TestObservationNewAnthropicFieldFailsLocallyWithEvidence(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"future_client_field":{"SECRET":"value"}}`)
	evidence, fact, out, rec, calls := observationExchange(t, diagnosticMapping(ClientMessages, UpstreamResponses), body, nil, false, BodyLimits{})
	if calls != 0 || rec.Code != 400 || out.Provenance != ProvenanceLocalRequestConversionError {
		t.Fatalf("unexpected forwarding calls=%d status=%d %+v", calls, rec.Code, out)
	}
	if len(evidence.Failures) != 1 || evidence.Failures[0].Kind != "unknown_field" || !strings.Contains(evidence.Failures[0].Path, "future_client_field") || fact.Fingerprint == "" {
		t.Fatalf("missing diagnostic: %+v", evidence.Failures)
	}
	if evidence.Legs[1].Observed || evidence.Legs[2].Observed {
		t.Fatal("fabricated upstream evidence")
	}
	if fact.Usage != nil {
		t.Fatal("fabricated usage")
	}
}
func TestObservationTruncatedStreamAndGeneratedLimitNeverPublishUsage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
		limits  BodyLimits
	}{
		{"truncated", []byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"}}]}\n\n"), BodyLimits{}},
		{"output-bound", testcorpus.ChatCompletionsStreamSSE(), BodyLimits{GeneratedSSEBatchBytes: 1024}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence, fact, out, _, _ := observationExchange(t, diagnosticMapping(ClientResponses, UpstreamChatCompletions), []byte(`{"model":"m","input":"hi","stream":true}`), tc.payload, true, tc.limits)
			if fact.Usage != nil {
				t.Fatal("failed stream published final usage")
			}
			if out.Provenance == ProvenanceUpstreamHTTP || len(evidence.Failures) == 0 {
				t.Fatalf("failure missing %+v %+v", out, evidence.Failures)
			}
		})
	}
}

// Codec report saturation and the smaller evidence view are two distinct
// bounds. Neither may disguise unclassified omissions as approved losses.
func TestObservationConversionOverflowReachesFactsAndEvidence(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprintf("capture=%v", capture), func(t *testing.T) {
			hub := observation.New(observation.Config{Providers: []string{"test"}, ManualTicks: true})
			defer hub.Close(context.Background())
			ctx, observed := hub.Begin(context.Background(), observation.BeginInfo{Provider: "test", Method: "POST", Path: "/v1/messages"})
			var dc *diagnostic.Capture
			if capture {
				dc = diagnostic.New(observed.ID(), 0)
				ctx = diagnostic.WithContext(ctx, dc)
			}
			var report ConversionReport
			for i := range maxStreamConversionReportEntries + 17 {
				if err := report.Note(FeatureToolResultTextJoin, fmt.Sprintf("$.items[%d]", i), "sanctioned encoding"); err != nil {
					t.Fatal(err)
				}
			}
			if report.Dropped != 17 || len(report.Losses) != maxStreamConversionReportEntries+1 {
				t.Fatalf("fixture did not exercise codec saturation: retained=%d dropped=%d", len(report.Losses), report.Dropped)
			}
			r := httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
			new(TranscodeHandler).diagnosticReport(r, "request_encode", report)
			fact := observed.Snapshot()
			if fact.ConversionLosses != 0 || fact.ConversionNotes != uint64(maxStreamConversionReportEntries+1) || fact.ConversionDropped != 17 {
				t.Fatalf("omissions reclassified or lost: %+v", fact)
			}
			var totals observation.Counters
			for _, p := range hub.Snapshot().Providers {
				if p.Provider == "test" {
					totals = p.Totals
				}
			}
			if totals.ConversionLosses != 0 || totals.ConversionNotes != fact.ConversionNotes || totals.ConversionDropped != 17 {
				t.Fatalf("canonical totals differ from request fact: %+v", totals)
			}
			if capture {
				evidence := dc.Seal()
				if len(evidence.Decisions) != diagnostic.MaxDecisions || evidence.DroppedDecisions != uint64(17+len(report.Losses)-diagnostic.MaxDecisions) {
					t.Fatalf("evidence omitted a bound: retained=%d dropped=%d", len(evidence.Decisions), evidence.DroppedDecisions)
				}
				if len(evidence.Failures) != 0 {
					t.Fatal("telemetry saturation invented an exchange failure")
				}
			}
		})
	}
}
