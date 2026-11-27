package transcode

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessagesUsageHandlerReview(t *testing.T) {
	startZero := strings.Replace(anthropicStartSSE, `"input_tokens":3,"output_tokens":1`, `"input_tokens":0,"output_tokens":0`, 1)
	stopZero := strings.Replace(anthropicStopSSE("end_turn"), `"output_tokens":2`, `"output_tokens":0`, 1)
	for _, tc := range []struct {
		name, start, stop, text string
		fail, unknown           bool
	}{
		{"missing initial input", strings.Replace(anthropicStartSSE, `"input_tokens":3`, `"unrelated_metadata":99`, 1), anthropicStopSSE("end_turn"), "", true, false},
		{"missing initial output", strings.Replace(anthropicStartSSE, `"output_tokens":1`, `"unrelated_metadata":99`, 1), anthropicStopSSE("end_turn"), "", true, false},
		{"missing final output", anthropicStartSSE, strings.Replace(anthropicStopSSE("end_turn"), `"output_tokens":2`, `"unrelated_metadata":99`, 1), "hello", true, false},
		{"alias only final output", anthropicStartSSE, strings.Replace(anthropicStopSSE("end_turn"), `"output_tokens":2`, `"OUTPUT_TOKENS":99`, 1), "hello", true, false},
		{"explicit zeros and optional delta input absent", startZero, stopZero, "hello", false, false},
		{"explicit zeros with extensions", strings.Replace(startZero, `"input_tokens":0`, `"input_tokens":0,"INPUT_TOKENS":99`, 1), strings.Replace(stopZero, `"output_tokens":0`, `"output_tokens":0,"OUTPUT_TOKENS":99`, 1), "hello", false, false},
		{"start usage absent", strings.Replace(anthropicStartSSE, `,"usage":{"input_tokens":3,"output_tokens":1}`, "", 1), anthropicStopSSE("end_turn"), "hello", false, true},
		{"start usage null", strings.Replace(anthropicStartSSE, `"usage":{"input_tokens":3,"output_tokens":1}`, `"usage":null`, 1), anthropicStopSSE("end_turn"), "hello", false, true},
		{"final usage absent", anthropicStartSSE, strings.Replace(anthropicStopSSE("end_turn"), `,"usage":{"output_tokens":2}`, "", 1), "hello", false, true},
		{"final usage null", anthropicStartSSE, strings.Replace(anthropicStopSSE("end_turn"), `"usage":{"output_tokens":2}`, `"usage":null`, 1), "hello", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := geminiMessagesStreamHandler(t, tc.start+anthropicTextBlockSSE+tc.stop, nil)
			req := httptest.NewRequest(http.MethodPost, geminiMessagesStreamTarget, strings.NewReader(geminiMessagesBody))
			req.Header.Set("Content-Type", "application/json")
			ctx, sink := WithOutcomeSink(req.Context())
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req.WithContext(ctx))
			outcome, ok := sink.Load()
			if !ok || !outcome.UpstreamAttempted || !outcome.UpstreamStatus.Set || outcome.UpstreamStatus.Value != http.StatusOK || outcome.LocalFailure || outcome.UpstreamFailure != tc.fail {
				t.Fatalf("incorrect outcome: %+v (recorded=%v)", outcome, ok)
			}
			if tc.fail {
				if outcome.Provenance != ProvenanceUpstreamBodyError || outcome.StreamOutcome != streamOutcomeUpstreamFailure {
					t.Fatalf("malformed usage not classified as upstream wire failure: %+v", outcome)
				}
			} else if outcome.Provenance != ProvenanceUpstreamHTTP || outcome.StreamOutcome != streamOutcomeSuccess {
				t.Fatalf("valid usage not classified as success: %+v", outcome)
			}
			frames := geminiStreamFrames(t, rec.Body.String())
			assertSingleTerminal(t, frames)
			if text := geminiFrameText(frames); text != tc.text {
				t.Fatalf("text=%q, want %q: %s", text, tc.text, rec.Body.String())
			}
			last := frames[len(frames)-1]
			if tc.fail {
				if _, ok := last["error"]; !ok || geminiFrameFinish(last) != "" {
					t.Fatalf("malformed usage did not terminate in error: %s", rec.Body.String())
				}
				return
			}
			if geminiFrameFinish(last) != "STOP" {
				t.Fatalf("valid stream terminal=%s", rec.Body.String())
			}
			usage, present := last["usageMetadata"].(map[string]any)
			if tc.unknown {
				if present {
					t.Fatalf("unknown usage became measured counts: %+v", usage)
				}
			} else {
				for _, key := range []string{"promptTokenCount", "candidatesTokenCount", "totalTokenCount"} {
					if value, known := usage[key]; !known || value != float64(0) {
						t.Fatalf("explicit zero %s = %v (known=%v)", key, value, known)
					}
				}
			}
		})
	}
}
