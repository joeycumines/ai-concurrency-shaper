package config

import (
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func TestParseTranscodeRouteGemini(t *testing.T) {
	t.Run("explicit route with a model template", func(t *testing.T) {
		route, err := parseTranscodeRoute("gemini@/v1beta/models/{model}:generateContent=chat-completions@/v1/chat/completions")
		if err != nil {
			t.Fatalf("parseTranscodeRoute: %v", err)
		}
		m := route.Mapping
		if m.ClientProtocol != transcode.ClientGemini || m.UpstreamProtocol != transcode.UpstreamChatCompletions {
			t.Fatalf("direction %q -> %q", m.ClientProtocol, m.UpstreamProtocol)
		}
		if m.ClientRoute.Path != "/v1beta/models/{model}:generateContent" {
			t.Fatalf("client route %q", m.ClientRoute.Path)
		}
	})

	t.Run("route prefix before the models segment is kept", func(t *testing.T) {
		route, err := parseTranscodeRoute("gemini@/proxy/v1beta/models/{model}:generateContent=responses@/v1/responses")
		if err != nil {
			t.Fatalf("parseTranscodeRoute: %v", err)
		}
		if route.Mapping.ClientRoute.Path != "/proxy/v1beta/models/{model}:generateContent" {
			t.Fatalf("client route %q", route.Mapping.ClientRoute.Path)
		}
	})

	t.Run("a path without the model template is rejected", func(t *testing.T) {
		_, err := parseTranscodeRoute("gemini@/v1beta/models/gemini-pro:generateContent=chat-completions@/v1/chat/completions")
		if err == nil || !strings.Contains(err.Error(), "invalid transcode route") {
			t.Fatalf("error = %v, want an invalid route", err)
		}
	})

	t.Run("chat completions stays upstream-only as a client", func(t *testing.T) {
		_, err := parseTranscodeRoute("chat-completions@/v1/chat/completions=chat-completions@/v1/chat/completions")
		if err == nil {
			t.Fatalf("chat completions accepted as a client protocol")
		}
	})
}

func TestBuildTranscodeMappingsGeminiPresets(t *testing.T) {
	build := func(t *testing.T, flags geminiPresetFlags, strictDefaults bool) []transcode.Mapping {
		t.Helper()
		mappings, err := buildTranscodeMappings(nil, false, false, false, transcodeCLIOptions{
			negatedLosses:  map[transcode.Feature]struct{}{},
			negatedQuery:   map[string]struct{}{},
			strictDefaults: strictDefaults,
			geminiPresets:  flags,
		})
		if err != nil {
			t.Fatalf("buildTranscodeMappings: %v", err)
		}
		out := make([]transcode.Mapping, 0, len(mappings))
		for _, m := range mappings {
			out = append(out, m.Mapping)
		}
		return out
	}

	t.Run("no presets add no mappings", func(t *testing.T) {
		if got := build(t, geminiPresetFlags{}, false); len(got) != 0 {
			t.Fatalf("got %d mappings, want none", len(got))
		}
	})

	t.Run("chat preset serves the Gemini route over chat completions", func(t *testing.T) {
		got := build(t, geminiPresetFlags{GeminiChat: true}, false)
		if len(got) != 1 {
			t.Fatalf("got %d mappings, want one", len(got))
		}
		m := got[0]
		if m.ClientProtocol != transcode.ClientGemini || m.UpstreamProtocol != transcode.UpstreamChatCompletions {
			t.Fatalf("direction %q -> %q", m.ClientProtocol, m.UpstreamProtocol)
		}
		if m.ClientRoute.Path != "/v1beta/models/{model}:generateContent" || m.UpstreamPath != "/v1/chat/completions" {
			t.Fatalf("route %q upstream %q", m.ClientRoute.Path, m.UpstreamPath)
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("preset mapping does not validate: %v", err)
		}
	})

	t.Run("responses preset defaults to the explicit strictness loss", func(t *testing.T) {
		got := build(t, geminiPresetFlags{GeminiResponses: true}, false)
		if len(got) != 1 {
			t.Fatalf("got %d mappings, want one", len(got))
		}
		if !got[0].LossPolicy.Allows(transcode.FeatureToolSchemaStrictness) {
			t.Fatalf("Gemini-to-Responses default does not approve tool_schema_strictness")
		}
		if err := got[0].Validate(); err != nil {
			t.Fatalf("preset mapping does not validate: %v", err)
		}
	})

	t.Run("strict defaults withhold the strictness loss", func(t *testing.T) {
		got := build(t, geminiPresetFlags{GeminiResponses: true}, true)
		if len(got) != 1 {
			t.Fatalf("got %d mappings, want one", len(got))
		}
		if got[0].LossPolicy.Allows(transcode.FeatureToolSchemaStrictness) {
			t.Fatalf("strict defaults approved tool_schema_strictness")
		}
	})

	t.Run("messages preset serves the Gemini route over the Anthropic Messages upstream", func(t *testing.T) {
		got := build(t, geminiPresetFlags{GeminiMessages: true}, false)
		if len(got) != 1 {
			t.Fatalf("got %d mappings, want one", len(got))
		}
		m := got[0]
		if m.ClientProtocol != transcode.ClientGemini || m.UpstreamProtocol != transcode.UpstreamMessages {
			t.Fatalf("direction %q -> %q", m.ClientProtocol, m.UpstreamProtocol)
		}
		if m.UpstreamPath != "/v1/messages" {
			t.Fatalf("upstream path %q", m.UpstreamPath)
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("preset mapping does not validate: %v", err)
		}
	})

	t.Run("responses-gemini preset serves /v1/responses over the Gemini upstream", func(t *testing.T) {
		got := build(t, geminiPresetFlags{ResponsesGemini: true}, false)
		if len(got) != 1 {
			t.Fatalf("got %d mappings, want one", len(got))
		}
		m := got[0]
		if m.ClientProtocol != transcode.ClientResponses || m.UpstreamProtocol != transcode.UpstreamGemini {
			t.Fatalf("direction %q -> %q", m.ClientProtocol, m.UpstreamProtocol)
		}
		if m.ClientRoute.Path != "/v1/responses" || m.UpstreamPath != "/v1beta" {
			t.Fatalf("route %q upstream %q", m.ClientRoute.Path, m.UpstreamPath)
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("preset mapping does not validate: %v", err)
		}
	})

	t.Run("messages-gemini preset serves /v1/messages over the Gemini upstream", func(t *testing.T) {
		got := build(t, geminiPresetFlags{MessagesGemini: true}, false)
		if len(got) != 1 {
			t.Fatalf("got %d mappings, want one", len(got))
		}
		m := got[0]
		if m.ClientProtocol != transcode.ClientMessages || m.UpstreamProtocol != transcode.UpstreamGemini {
			t.Fatalf("direction %q -> %q", m.ClientProtocol, m.UpstreamProtocol)
		}
		if m.ClientRoute.Path != "/v1/messages" || m.UpstreamPath != "/v1beta" {
			t.Fatalf("route %q upstream %q", m.ClientRoute.Path, m.UpstreamPath)
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("preset mapping does not validate: %v", err)
		}
	})

	t.Run("gemini-upstream mappings get metadata and parallel_tool_calls injected", func(t *testing.T) {
		got := build(t, geminiPresetFlags{ResponsesGemini: true, MessagesGemini: true}, false)
		if len(got) != 2 {
			t.Fatalf("got %d mappings, want two", len(got))
		}
		for _, m := range got {
			if m.UpstreamProtocol != transcode.UpstreamGemini {
				t.Fatalf("mapping upstream %q, want gemini", m.UpstreamProtocol)
			}
			if !m.LossPolicy.Allows(transcode.FeatureMetadata) {
				t.Fatalf("mapping does not approve metadata")
			}
			if !m.LossPolicy.Allows(transcode.FeatureParallelToolCalls) {
				t.Fatalf("mapping does not approve parallel_tool_calls")
			}
		}
	})

	t.Run("explicit gemini-upstream route gets the same injection", func(t *testing.T) {
		route, err := parseTranscodeRoute("responses@/v1/responses=gemini@/v1beta")
		if err != nil {
			t.Fatalf("parseTranscodeRoute: %v", err)
		}
		mappings, err := buildTranscodeMappings([]proxy.TranscodeMapping{route}, false, false, false, transcodeCLIOptions{
			negatedLosses: map[transcode.Feature]struct{}{},
			negatedQuery:  map[string]struct{}{},
		})
		if err != nil {
			t.Fatalf("buildTranscodeMappings: %v", err)
		}
		if len(mappings) != 1 {
			t.Fatalf("got %d mappings, want one", len(mappings))
		}
		m := mappings[0].Mapping
		if !m.LossPolicy.Allows(transcode.FeatureMetadata) || !m.LossPolicy.Allows(transcode.FeatureParallelToolCalls) {
			t.Fatalf("explicit route lacks the metadata/parallel_tool_calls injection: %v", m.LossPolicy.Allowed)
		}
	})

	t.Run("negated losses remove the injection", func(t *testing.T) {
		got := build(t, geminiPresetFlags{MessagesGemini: true}, false)
		_ = got
		mappings, err := buildTranscodeMappings(nil, false, false, false, transcodeCLIOptions{
			negatedLosses: map[transcode.Feature]struct{}{
				transcode.FeatureMetadata: {},
			},
			negatedQuery:  map[string]struct{}{},
			geminiPresets: geminiPresetFlags{MessagesGemini: true},
		})
		if err != nil {
			t.Fatalf("buildTranscodeMappings: %v", err)
		}
		if mappings[0].Mapping.LossPolicy.Allows(transcode.FeatureMetadata) {
			t.Fatal("negated metadata still approved")
		}
		if !mappings[0].Mapping.LossPolicy.Allows(transcode.FeatureParallelToolCalls) {
			t.Fatal("parallel_tool_calls missing although not negated")
		}
	})

	t.Run("strict defaults withhold the gemini injection", func(t *testing.T) {
		got := build(t, geminiPresetFlags{MessagesGemini: true}, true)
		if len(got) != 1 {
			t.Fatalf("got %d mappings, want one", len(got))
		}
		if got[0].LossPolicy.Allows(transcode.FeatureMetadata) || got[0].LossPolicy.Allows(transcode.FeatureParallelToolCalls) {
			t.Fatalf("strict defaults approved the gemini injection: %v", got[0].LossPolicy.Allowed)
		}
	})
}

func TestParseTranscodeRouteMessagesUpstream(t *testing.T) {
	t.Run("gemini client may target the messages upstream", func(t *testing.T) {
		route, err := parseTranscodeRoute("gemini@/v1beta/models/{model}:generateContent=messages@/v1/messages")
		if err != nil {
			t.Fatalf("parseTranscodeRoute: %v", err)
		}
		if err := route.Mapping.Validate(); err != nil {
			t.Fatalf("mapping does not validate: %v", err)
		}
	})

	t.Run("a responses client targeting the messages upstream is refused", func(t *testing.T) {
		_, err := parseTranscodeRoute("responses@/v1/responses=messages@/v1/messages")
		if err == nil || !strings.Contains(err.Error(), "unsupported transcode direction") {
			t.Fatalf("parseTranscodeRoute = %v, want unsupported transcode direction", err)
		}
	})
}

// TestResolveAndValidate_GeminiMultiTargetReachability pins the generalized
// startup check on a Gemini client route: a route with a gemini-client chat and
// messages mapping where no model has via=messages fails startup naming
// messages.
func TestResolveAndValidate_GeminiMultiTargetReachability(t *testing.T) {
	bad, err := Parse([]string{
		"-upstream", "https://api.example.com",
		"-transcode-gemini-chat",
		"-transcode-gemini-messages",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = bad.ResolveAndValidate()
	if err == nil {
		t.Fatal("ResolveAndValidate: nil error, want the unreachable messages target refused")
	}
	for _, want := range []string{"POST /v1beta/models/{model}:generateContent", "unreachable", "messages", "-model-table"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	}

	// A via=messages model makes the messages target selectable, and a via=chat
	// model keeps the chat target selectable under the table-projected map.
	ok, err := Parse([]string{
		"-model-table", "g@example=wire-g;via=messages",
		"-model-table", "c@example=wire-c;via=chat",
		"-upstream", "https://api.example.com",
		"-transcode-gemini-chat",
		"-transcode-gemini-messages",
	})
	if err != nil {
		t.Fatalf("Parse with both vias: %v", err)
	}
	if err := ok.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate with both vias: %v", err)
	}

	// Without a via=chat entry (and the table disabling identity), the chat
	// target becomes unreachable and the refusal names chat.
	noChat, err := Parse([]string{
		"-model-table", "g@example=wire-g;via=messages",
		"-upstream", "https://api.example.com",
		"-transcode-gemini-chat",
		"-transcode-gemini-messages",
	})
	if err != nil {
		t.Fatalf("Parse no-chat: %v", err)
	}
	err = noChat.ResolveAndValidate()
	if err == nil || !strings.Contains(err.Error(), "chat") {
		t.Fatalf("ResolveAndValidate = %v, want the unreachable chat target named", err)
	}
}
