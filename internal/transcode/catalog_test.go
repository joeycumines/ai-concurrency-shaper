package transcode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// catalogFixture builds the standard three-model snapshot used by the renderer
// tests: a fully-decorated model, a partially-decorated one, and a fact-free
// minimal one.
func catalogFixture(t *testing.T) *CatalogHandler {
	t.Helper()
	context1 := 262144
	maxOut1 := 32768
	context2 := 204800
	handler, err := NewCatalogHandler(CatalogConfig{
		ProviderName:      "TestProv",
		ServesResponses:   true,
		ParallelToolCalls: true,
		StructuredOutputs: true,
		Models: []CatalogModel{
			{Surrogate: "kimi-k3", Context: &context1, MaxOutput: &maxOut1, Efforts: []string{"high"}, Modalities: []string{"text", "image"}},
			{Surrogate: "glm-5.2", Context: &context2, Modalities: []string{"text"}},
			{Surrogate: "bare-model"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func catalogGet(t *testing.T, handler *CatalogHandler, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestCatalogCodexEntryGolden pins the Codex native document field for field.
func TestCatalogCodexEntryGolden(t *testing.T) {
	handler := catalogFixture(t)
	rec := catalogGet(t, handler, "/v1/models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var document struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Models) != 3 {
		t.Fatalf("models = %d, want 3", len(document.Models))
	}
	kimi := document.Models[0]
	want := map[string]any{
		"slug":                         "kimi-k3",
		"display_name":                 "TestProv kimi-k3",
		"supported_in_api":             true,
		"shell_type":                   "shell_command",
		"visibility":                   "list",
		"priority":                     float64(1),
		"support_verbosity":            true,
		"supports_parallel_tool_calls": true,
		"truncation_policy":            map[string]any{"mode": "tokens", "limit": float64(262144)},
		"experimental_supported_tools": []any{},
		"base_instructions":            "",
		"input_modalities":             []any{"text", "image"},
		"context_window":               float64(262144),
		"max_context_window":           float64(262144),
		"auto_compact_token_limit":     float64(249036),
		"supported_reasoning_levels": []any{
			map[string]any{"effort": "high", "description": "Thorough"},
		},
	}
	assertJSONObjectEqual(t, "kimi-k3", kimi, want)

	glm := document.Models[1]
	if policy, ok := glm["truncation_policy"].(map[string]any); !ok || policy["limit"] != float64(204800) {
		t.Errorf("glm-5.2 truncation_policy = %v, want the declared context", glm["truncation_policy"])
	}
	if levels, ok := glm["supported_reasoning_levels"].([]any); !ok || len(levels) != 0 {
		t.Errorf("glm-5.2 levels = %v, want empty", glm["supported_reasoning_levels"])
	}

	bare := document.Models[2]
	for _, absent := range []string{"truncation_policy", "context_window", "max_context_window", "auto_compact_token_limit"} {
		if _, ok := bare[absent]; ok {
			t.Errorf("bare-model carries %q without facts", absent)
		}
	}
	if bare["base_instructions"] != "" {
		t.Errorf("bare-model base_instructions = %v, want the explicit empty string", bare["base_instructions"])
	}
	if modalities, ok := bare["input_modalities"].([]any); !ok || len(modalities) != 1 || modalities[0] != "text" {
		t.Errorf("bare-model input_modalities = %v, want [text]", bare["input_modalities"])
	}
}

// TestCatalogCodexEntryOmitsMaxout proves the Codex shape has no max_output
// slot: the fact is served on the Anthropic shape, not invented here.
func TestCatalogCodexEntryOmitsMaxout(t *testing.T) {
	handler := catalogFixture(t)
	rec := catalogGet(t, handler, "/v1/models", nil)
	for _, banned := range []string{"max_output", "max_tokens"} {
		if strings.Contains(rec.Body.String(), banned) {
			t.Errorf("codex document contains %q:\n%s", banned, rec.Body.String())
		}
	}
}

// TestCatalogAnthropicEntryGolden pins the Anthropic messages-list document.
func TestCatalogAnthropicEntryGolden(t *testing.T) {
	handler := catalogFixture(t)
	rec := catalogGet(t, handler, "/v1/models", map[string]string{"Anthropic-Version": "2023-06-01"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var document struct {
		Data []struct {
			Type           string          `json:"type"`
			ID             string          `json:"id"`
			DisplayName    string          `json:"display_name"`
			CreatedAt      string          `json:"created_at"`
			MaxInputTokens *int            `json:"max_input_tokens"`
			MaxTokens      *int            `json:"max_tokens"`
			Capabilities   json.RawMessage `json:"capabilities"`
		} `json:"data"`
		FirstID *string `json:"first_id"`
		LastID  *string `json:"last_id"`
		HasMore bool    `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Data) != 3 {
		t.Fatalf("data = %d, want 3", len(document.Data))
	}
	if document.FirstID == nil || *document.FirstID != "kimi-k3" || document.LastID == nil || *document.LastID != "bare-model" {
		t.Errorf("first/last = %v/%v", document.FirstID, document.LastID)
	}
	if document.HasMore {
		t.Error("has_more = true, want false for the full list")
	}

	kimi := document.Data[0]
	if kimi.Type != "model" || kimi.ID != "kimi-k3" || kimi.DisplayName != "TestProv kimi-k3" || kimi.CreatedAt != "1970-01-01T00:00:00Z" {
		t.Errorf("kimi identity = %+v", kimi)
	}
	if kimi.MaxInputTokens == nil || *kimi.MaxInputTokens != 262144 || kimi.MaxTokens == nil || *kimi.MaxTokens != 32768 {
		t.Errorf("kimi tokens = %v/%v", kimi.MaxInputTokens, kimi.MaxTokens)
	}
	var capabilities map[string]any
	if err := json.Unmarshal(kimi.Capabilities, &capabilities); err != nil {
		t.Fatal(err)
	}
	effort, _ := capabilities["effort"].(map[string]any)
	if effort["supported"] != true {
		t.Errorf("effort.supported = %v", effort["supported"])
	}
	if leaf, _ := effort["high"].(map[string]any); leaf["supported"] != true {
		t.Errorf("high leaf = %v", effort["high"])
	}
	if leaf, _ := effort["low"].(map[string]any); leaf["supported"] != false {
		t.Errorf("low leaf = %v", effort["low"])
	}
	if _, ok := effort["minimal"]; ok {
		t.Error("effort carries a minimal leaf the Anthropic contract has no slot for")
	}
	if leaf, _ := capabilities["image_input"].(map[string]any); leaf["supported"] != true {
		t.Errorf("image_input = %v", capabilities["image_input"])
	}
	if leaf, _ := capabilities["pdf_input"].(map[string]any); leaf["supported"] != false {
		t.Errorf("pdf_input = %v", capabilities["pdf_input"])
	}
	for _, key := range []string{"batch", "citations", "code_execution", "context_management"} {
		if leaf, _ := capabilities[key].(map[string]any); leaf["supported"] != false {
			t.Errorf("%s = %v, want false", key, capabilities[key])
		}
	}
	thinking, _ := capabilities["thinking"].(map[string]any)
	if thinking["supported"] != true {
		t.Errorf("thinking = %v", capabilities["thinking"])
	}
	types, _ := thinking["types"].(map[string]any)
	if adaptive, _ := types["adaptive"].(map[string]any); adaptive["supported"] != false {
		t.Errorf("thinking.types.adaptive = %v", types["adaptive"])
	}

	glm := document.Data[1]
	if glm.MaxTokens != nil {
		t.Errorf("glm-5.2 max_tokens = %v, want null (key present)", glm.MaxTokens)
	}
	var glmCapabilities map[string]any
	if err := json.Unmarshal(glm.Capabilities, &glmCapabilities); err != nil {
		t.Fatal(err)
	}
	if effort, _ := glmCapabilities["effort"].(map[string]any); effort["supported"] != false {
		t.Errorf("glm-5.2 effort = %v, want supported false", glmCapabilities["effort"])
	}

	bare := document.Data[2]
	if string(bare.Capabilities) != "null" {
		t.Errorf("bare-model capabilities = %s, want null for a fact-free model", bare.Capabilities)
	}
	if bare.MaxInputTokens != nil || bare.MaxTokens != nil {
		t.Errorf("bare-model tokens = %v/%v, want null", bare.MaxInputTokens, bare.MaxTokens)
	}
}

// TestCatalogOpenAIEntryGolden pins the lean OpenAI list.
func TestCatalogOpenAIEntryGolden(t *testing.T) {
	handler := catalogFixture(t)
	rec := catalogGet(t, handler, "/v1/models?format=openai", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var document struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Object != "list" {
		t.Errorf("object = %q", document.Object)
	}
	if len(document.Data) != 3 {
		t.Fatalf("data = %d, want 3", len(document.Data))
	}
	for i, want := range []string{"kimi-k3", "glm-5.2", "bare-model"} {
		entry := document.Data[i]
		if entry.ID != want || entry.Object != "model" || entry.Created != 0 || entry.OwnedBy != "TestProv" {
			t.Errorf("data[%d] = %+v, want %q owned_by TestProv", i, entry, want)
		}
	}
	if strings.Contains(rec.Body.String(), "shutdown_date") {
		t.Errorf("lean document carries shutdown_date: %s", rec.Body.String())
	}
}

// TestCatalogMalformedEntrySkipped proves one malformed entry never fails the
// listing: the render loop is per-entry independent.
func TestCatalogMalformedEntrySkipped(t *testing.T) {
	badContext := 0
	badOut := -5
	cases := []struct {
		name  string
		model CatalogModel
	}{
		{"bad context", CatalogModel{Surrogate: "bad", Context: &badContext}},
		{"bad max_output", CatalogModel{Surrogate: "bad", MaxOutput: &badOut}},
		{"bad effort", CatalogModel{Surrogate: "bad", Efforts: []string{"bogus"}}},
		{"bad modality", CatalogModel{Surrogate: "bad", Modalities: []string{"video"}}},
	}
	context := 128000
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := NewCatalogHandler(CatalogConfig{
				ProviderName: "TestProv",
				Models: []CatalogModel{
					{Surrogate: "good", Context: &context},
					tc.model,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{"/v1/models", "/v1/models?format=anthropic", "/v1/models?format=openai"} {
				rec := catalogGet(t, handler, target, nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s status = %d: %s", target, rec.Code, rec.Body.String())
				}
				body := rec.Body.String()
				if !strings.Contains(body, `"good"`) {
					t.Errorf("%s lost the good entry: %s", target, body)
				}
				if strings.Contains(body, `"bad"`) {
					t.Errorf("%s served the malformed entry: %s", target, body)
				}
			}
		})
	}
}

// TestCatalogRoutableListed proves every served identifier resolves on the
// mount's projected map.
func TestCatalogRoutableListed(t *testing.T) {
	handler := catalogFixture(t)
	modelMap := ModelMap{
		Exact: map[string]ModelMapping{
			"kimi-k3":    {ClientModel: "kimi-k3", UpstreamModel: "wire-kimi", ClientResponseModel: "kimi-k3"},
			"glm-5.2":    {ClientModel: "glm-5.2", UpstreamModel: "wire-glm", ClientResponseModel: "glm-5.2"},
			"bare-model": {ClientModel: "bare-model", UpstreamModel: "wire-bare", ClientResponseModel: "bare-model"},
		},
		RequireExplicitMap: true,
	}
	for _, target := range []string{"/v1/models", "/v1/models?format=anthropic", "/v1/models?format=openai"} {
		rec := catalogGet(t, handler, target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", target, rec.Code)
		}
		var document map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		for _, slug := range catalogDocumentSlugs(t, document) {
			if _, err := modelMap.Resolve(slug); err != nil {
				t.Errorf("%s served unresolvable %q: %v", target, slug, err)
			}
		}
	}
}

// TestCatalogMissNamesServable proves an unknown cursor is a bounded local 400
// naming every servable surrogate.
func TestCatalogMissNamesServable(t *testing.T) {
	handler := catalogFixture(t)
	rec := catalogGet(t, handler, "/v1/models?format=anthropic&after_id=unknown", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"unknown after_id", "kimi-k3", "glm-5.2", "bare-model"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
	if len(body) > DefaultErrorMessageBytes+256 {
		t.Errorf("error body %d bytes exceeds the message bound family", len(body))
	}
}

// catalogDocumentSlugs extracts the slug/id values from any of the three
// document shapes.
func catalogDocumentSlugs(t *testing.T, document map[string]any) []string {
	t.Helper()
	raw, ok := document["data"].([]any)
	if !ok {
		raw, ok = document["models"].([]any)
		if !ok {
			t.Fatalf("document has neither data nor models: %v", document)
		}
	}
	var slugs []string
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("entry is not an object: %v", item)
		}
		for _, key := range []string{"slug", "id"} {
			if value, ok := entry[key].(string); ok {
				slugs = append(slugs, value)
				break
			}
		}
	}
	return slugs
}

// assertJSONObjectEqual compares two decoded JSON objects.
func assertJSONObjectEqual(t *testing.T, name string, got, want map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s has %d keys, want %d:\ngot  %v\nwant %v", name, len(got), len(want), got, want)
	}
	for key, wantValue := range want {
		gotValue, ok := got[key]
		if !ok {
			t.Errorf("%s missing key %q", name, key)
			continue
		}
		if !jsonEqual(gotValue, wantValue) {
			t.Errorf("%s[%q] = %#v, want %#v", name, key, gotValue, wantValue)
		}
	}
}

func jsonEqual(a, b any) bool {
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	return aerr == nil && berr == nil && string(ab) == string(bb)
}
