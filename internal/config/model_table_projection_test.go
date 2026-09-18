// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package config

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// resolveModelTableArgs parses and resolves args, returning the config.
func resolveModelTableArgs(t *testing.T, args ...string) *Config {
	t.Helper()
	cfg, err := Parse(args)
	if err != nil {
		t.Fatalf("Parse(%q): %v", args, err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate(%q): %v", args, err)
	}
	return cfg
}

// assertModelTableSemanticError asserts the resolve failure text and that it is
// a semantic (exit 1) error, never a usage (exit 2) error.
func assertModelTableSemanticError(t *testing.T, args []string, want string) {
	t.Helper()
	cfg, err := Parse(args)
	if err != nil {
		t.Fatalf("Parse(%q): %v", args, err)
	}
	err = cfg.ResolveAndValidate()
	if err == nil {
		t.Fatalf("ResolveAndValidate(%q): want error %q, got nil", args, want)
	}
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if errors.Is(err, ErrUsage) {
		t.Errorf("model-table value failure must not be a usage error: %v", err)
	}
}

// TestModelTable_DuplicateSurrogateRejected rejects a surrogate declared twice
// in the global namespace, case-sensitively.
func TestModelTable_DuplicateSurrogateRejected(t *testing.T) {
	assertModelTableSemanticError(t, []string{
		"-upstream", "https://api.openai.com",
		"-model-table", "a@openai=b",
		"-model-table", "a@openai=c",
	}, `duplicate -model-table surrogate "a"`)

	// Case-sensitive: "a" and "A" are distinct surrogates.
	resolveModelTableArgs(t,
		"-upstream", "https://api.openai.com",
		"-model-table", "a@openai=b",
		"-model-table", "A@openai=c",
	)
}

// TestModelTable_DuplicateSurrogateAcrossProvidersRejected proves the surrogate
// namespace is global: two mounts cannot share one client-visible name.
func TestModelTable_DuplicateSurrogateAcrossProvidersRejected(t *testing.T) {
	assertModelTableSemanticError(t, []string{
		"-model-table", "x@anthropic=a",
		"-model-table", "x@openai=b",
		"--provider=anthropic",
		"-upstream", "https://api.anthropic.com",
		"-prefix", "/anthropic",
		"--provider=openai",
		"-upstream", "https://api.openai.com",
		"-prefix", "/openai",
	}, `duplicate -model-table surrogate "x"`)
}

// TestModelTable_TranscodeModelConflictRejected rejects the coexistence of the
// global table and any provider-scope -transcode-model, even on a provider with
// no transcode routes.
func TestModelTable_TranscodeModelConflictRejected(t *testing.T) {
	assertModelTableSemanticError(t, []string{
		"-upstream", "https://api.anthropic.com",
		"-transcode-model", "a=b",
		"-model-table", "c@anthropic=d",
	}, `-model-table and -transcode-model cannot be combined (provider "anthropic"): configure the global table only`)
}

// TestModelTable_UnknownProviderRejected rejects an entry naming a provider
// that does not resolve, listing the known providers.
func TestModelTable_UnknownProviderRejected(t *testing.T) {
	assertModelTableSemanticError(t, []string{
		"-upstream", "https://api.openai.com",
		"-model-table", "a@acme=b",
	}, `unknown -model-table provider "acme" in "a@acme=b": no provider named "acme" (known: openai)`)
}

// TestModelTable_CoverageRequiresEntryPerTranscodedProvider requires every
// provider with transcode routes to be named by at least one entry, so the
// projection never leaves a route with an empty explicit map.
func TestModelTable_CoverageRequiresEntryPerTranscodedProvider(t *testing.T) {
	assertModelTableSemanticError(t, []string{
		"-model-table", "a@anthropic=w",
		"--provider=anthropic",
		"-upstream", "https://api.anthropic.com",
		"-prefix", "/anthropic",
		"--provider=openai",
		"-upstream", "https://api.openai.com",
		"-prefix", "/openai",
		"-transcode-responses-chat",
	}, `provider "openai" has transcode routes but no -model-table entry names it`)
}

// TestModelTable_ProjectOntoExactMechanical proves the projection is a
// mechanical per-provider filter into the existing ModelMap.Exact, that
// identity fallback is off, and that unlisted models fail resolution.
func TestModelTable_ProjectOntoExactMechanical(t *testing.T) {
	cfg := resolveModelTableArgs(t,
		"-model-table", "s1@anthropic=w1",
		"-model-table", "s2@anthropic=w2",
		"-model-table", "s3@openai=w3",
		"--provider=anthropic",
		"-upstream", "https://api.anthropic.com",
		"-prefix", "/anthropic",
		"-transcode-responses-chat",
		"--provider=openai",
		"-upstream", "https://api.openai.com",
		"-prefix", "/openai",
		"-transcode-responses-chat",
	)
	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(cfg.Providers))
	}

	wantExact := []map[string]transcode.ModelMapping{
		{
			"s1": {ClientModel: "s1", UpstreamModel: "w1", ClientResponseModel: "s1"},
			"s2": {ClientModel: "s2", UpstreamModel: "w2", ClientResponseModel: "s2"},
		},
		{
			"s3": {ClientModel: "s3", UpstreamModel: "w3", ClientResponseModel: "s3"},
		},
	}
	for i, p := range cfg.Providers {
		mappings := p.TranscodeMappings()
		if len(mappings) != 1 {
			t.Fatalf("provider %d mappings = %d, want 1", i, len(mappings))
		}
		modelMap := mappings[0].Mapping.ModelMap
		if !reflect.DeepEqual(modelMap.Exact, wantExact[i]) {
			t.Errorf("provider %d Exact = %+v, want %+v", i, modelMap.Exact, wantExact[i])
		}
		if modelMap.AllowIdentity {
			t.Errorf("provider %d AllowIdentity = true, want false", i)
		}
		if !modelMap.RequireExplicitMap {
			t.Errorf("provider %d RequireExplicitMap = false, want true", i)
		}
		if mapping, err := modelMap.Resolve("s1"); i == 0 && (err != nil || mapping.UpstreamModel != "w1") {
			t.Errorf("Resolve(s1) = %+v, %v", mapping, err)
		}
		if _, err := modelMap.Resolve("unknown"); err == nil {
			t.Errorf("provider %d accepted an unlisted model", i)
		} else if want := `no upstream model mapping for client model "unknown"`; err.Error() != want {
			t.Errorf("unknown-model error = %q, want %q", err, want)
		}
	}
}

// TestModelTable_ZeroEntriesKeepsIdentityFallback proves the zero-entry rule:
// without any -model-table value, model resolution keeps its identity fallback.
func TestModelTable_ZeroEntriesKeepsIdentityFallback(t *testing.T) {
	cfg := resolveModelTableArgs(t,
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
	)
	mappings := cfg.Providers[0].TranscodeMappings()
	if len(mappings) != 1 {
		t.Fatalf("mappings = %d, want 1", len(mappings))
	}
	modelMap := mappings[0].Mapping.ModelMap
	if !modelMap.AllowIdentity {
		t.Fatal("zero-entry table must keep identity fallback")
	}
	if modelMap.RequireExplicitMap {
		t.Fatal("zero-entry table must not require an explicit map")
	}
	if _, err := modelMap.Resolve("anything"); err != nil {
		t.Fatalf("identity fallback failed: %v", err)
	}
}

// TestModelTable_DuplicateDefaultPerProviderRejected allows one default per
// provider and rejects a second.
func TestModelTable_DuplicateDefaultPerProviderRejected(t *testing.T) {
	assertModelTableSemanticError(t, []string{
		"-upstream", "https://p",
		"-model-table", "a@p=w;default",
		"-model-table", "b@p=v;default",
	}, `duplicate -model-table default for provider "p": "a" and "b"`)

	// One default per provider is valid, across providers too.
	resolveModelTableArgs(t,
		"-model-table", "a@anthropic=w;default",
		"-model-table", "b@openai=v;default",
		"--provider=anthropic",
		"-upstream", "https://api.anthropic.com",
		"-prefix", "/anthropic",
		"-transcode-messages-chat",
		"--provider=openai",
		"-upstream", "https://api.openai.com",
		"-prefix", "/openai",
		"-transcode-responses-chat",
	)
}

// TestModelTable_FactsIgnoredByProjection proves facts never reach the
// projected resolution map.
func TestModelTable_FactsIgnoredByProjection(t *testing.T) {
	bare := resolveModelTableArgs(t,
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
		"-model-table", "a@openai=w",
	)
	full := resolveModelTableArgs(t,
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
		"-model-table", "a@openai=w;context=128000;max_output=4096;efforts=low+high;modalities=text+image;default",
	)
	bareExact := bare.Providers[0].TranscodeMappings()[0].Mapping.ModelMap.Exact
	fullExact := full.Providers[0].TranscodeMappings()[0].Mapping.ModelMap.Exact
	if !reflect.DeepEqual(bareExact, fullExact) {
		t.Fatalf("facts changed the projected map: %+v vs %+v", bareExact, fullExact)
	}
}

// TestModelTable_TranscodeMappingsDeepCopyIsolated proves both the frozen table
// and the projected maps are isolated from later mutation.
func TestModelTable_TranscodeMappingsDeepCopyIsolated(t *testing.T) {
	cfg := resolveModelTableArgs(t,
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
		"-model-table", "a@openai=w",
	)

	m1 := cfg.Providers[0].TranscodeMappings()
	if len(m1) != 1 {
		t.Fatalf("m1 len = %d, want 1", len(m1))
	}
	m1[0].Mapping.ModelMap.Exact["hacked"] = transcode.ModelMapping{UpstreamModel: "hacked"}

	// Mutating the raw table after resolve must not change resolved mappings.
	cfg.Server.ModelTable = append(cfg.Server.ModelTable, "z@openai=zzz")

	m2 := cfg.Providers[0].TranscodeMappings()
	if _, ok := m2[0].Mapping.ModelMap.Exact["hacked"]; ok {
		t.Error("mutation of the projected map leaked into a later copy")
	}
	if _, ok := m2[0].Mapping.ModelMap.Exact["z"]; ok {
		t.Error("post-resolve table mutation leaked into the frozen projection")
	}
	if len(m2[0].Mapping.ModelMap.Exact) != 1 {
		t.Errorf("Exact = %+v, want exactly the frozen entry", m2[0].Mapping.ModelMap.Exact)
	}
}

// TestModelTableSummaryFormat pins the one startup summary line: sorted by
// surrogate, facts in canonical order, bounds applied to large tables.
func TestModelTableSummaryFormat(t *testing.T) {
	cfg := resolveModelTableArgs(t,
		"-model-table", "opus@anthropic=claude-opus-4-1;context=200000;default",
		"-model-table", "sonnet@anthropic=claude-sonnet-4-5;context=200000;efforts=minimal+low+medium+high;modalities=text+image",
		"-model-table", "gpt4o@openai=gpt-4o;context=128000;default;max_output=16384",
		"-model-table", "gpt4o-mini@openai=gpt-4o-mini;context=128000;max_output=16384",
		"-model-table", "old@openai=gpt-3.5-turbo;deprecated",
		"--provider=anthropic",
		"-upstream", "https://api.anthropic.com",
		"-prefix", "/anthropic",
		"-transcode-messages-chat",
		"--provider=openai",
		"-upstream", "https://api.openai.com",
		"-prefix", "/openai",
		"-transcode-responses-chat",
	)
	want := "model table: 5 surrogates across 2 providers: " +
		"gpt4o@openai->gpt-4o(context=128000,default,max_output=16384), " +
		"gpt4o-mini@openai->gpt-4o-mini(context=128000,max_output=16384), " +
		"old@openai->gpt-3.5-turbo(deprecated), " +
		"opus@anthropic->claude-opus-4-1(context=200000,default), " +
		"sonnet@anthropic->claude-sonnet-4-5(context=200000,efforts=minimal+low+medium+high,modalities=text+image)"
	if got := cfg.ModelTableSummary(); got != want {
		t.Errorf("summary =\n%q\nwant\n%q", got, want)
	}

	// Zero entries: no summary line at all.
	empty := resolveModelTableArgs(t, "-upstream", "https://api.openai.com")
	if got := empty.ModelTableSummary(); got != "" {
		t.Errorf("zero-entry summary = %q, want empty", got)
	}

	// A large table is truncated with an explicit remainder count.
	var args []string
	args = append(args, "-upstream", "https://api.openai.com", "-transcode-responses-chat")
	for i := range 60 {
		args = append(args, "-model-table", "s"+strings.Repeat("0", 2)+string(rune('a'+i%26))+string(rune('a'+i/26))+"@openai=w")
	}
	large := resolveModelTableArgs(t, args...)
	got := large.ModelTableSummary()
	if !strings.HasPrefix(got, "model table: 60 surrogates across 1 providers: ") {
		t.Fatalf("large summary prefix = %q", got)
	}
	if !strings.HasSuffix(got, ", ... (+10 more)") {
		t.Errorf("large summary suffix = %q, want truncation suffix", got)
	}
	if n := strings.Count(got, "@openai->w"); n != 50 {
		t.Errorf("listed entries = %d, want 50", n)
	}
}

// TestModelFactsReachTheServedCatalog proves the flag-to-document round trip:
// every fact parsed from -model-table is served by the mount's catalog, and
// absence is honest (no fabricated values).
func TestModelFactsReachTheServedCatalog(t *testing.T) {
	cfg := resolveModelTableArgs(t,
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
		"-model-table", "m@openai=wire-m;context=200000;max_output=8192;efforts=low+high;modalities=text+image;default",
		"-model-table", "old@openai=wire-old;deprecated",
	)
	catalog, ok := cfg.Providers[0].ModelCatalog()
	if !ok {
		t.Fatal("provider has no catalog snapshot")
	}
	handler, err := transcode.NewCatalogHandler(catalog)
	if err != nil {
		t.Fatal(err)
	}

	serve := func(target string, headers map[string]string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", target, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	codex := serve("/v1/models", nil)
	for _, want := range []string{
		`"slug":"m"`, `"display_name":"openai m"`, `"priority":1`,
		`"truncation_policy":{"mode":"tokens","limit":200000}`,
		`"context_window":200000`, `"max_context_window":200000`,
		`"auto_compact_token_limit":190000`,
		`"input_modalities":["text","image"]`,
		`{"effort":"low","description":"Fast"}`, `{"effort":"high","description":"Thorough"}`,
		`"visibility":"hide"`,
	} {
		if !strings.Contains(codex, want) {
			t.Errorf("codex document missing %s:\n%s", want, codex)
		}
	}
	if strings.Contains(codex, "max_output") || strings.Contains(codex, "max_tokens") {
		t.Errorf("codex document must not carry max_output:\n%s", codex)
	}

	anthropic := serve("/v1/models", map[string]string{"Anthropic-Version": "2023-06-01"})
	for _, want := range []string{`"id":"m"`, `"max_input_tokens":200000`, `"max_tokens":8192`, `"id":"old"`, `"max_tokens":null`} {
		if !strings.Contains(anthropic, want) {
			t.Errorf("anthropic document missing %s:\n%s", want, anthropic)
		}
	}
}

// TestModelFactsDoNotAlterRendering proves facts never reach the request path:
// the same invocation with and without facts renders byte-identical upstream
// request bodies for the same client exchange.
func TestModelFactsDoNotAlterRendering(t *testing.T) {
	render := func(entry string) []byte {
		t.Helper()
		cfg := resolveModelTableArgs(t,
			"-upstream", "https://api.openai.com",
			"-transcode-responses-chat",
			"-model-table", entry,
		)
		mappings := cfg.Providers[0].TranscodeMappings()
		if len(mappings) != 1 {
			t.Fatalf("mappings = %d, want 1", len(mappings))
		}
		var upstreamBody []byte
		handler := transcode.NewTranscodeHandler(
			transcode.HandlerConfig{
				Mapping:  mappings[0].Mapping,
				Upstream: cfg.Providers[0].UpstreamURL(),
				BodyLimits: transcode.BodyLimits{
					AcceptedRequestBytes:    1 << 20,
					SuccessfulResponseBytes: 1 << 20,
				},
			},
			func(req *http.Request) (*http.Response, error) {
				upstreamBody, _ = io.ReadAll(req.Body)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"id":"c","object":"chat.completion","created":1,"model":"wire-m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
					)),
				}, nil
			},
			nil,
		)
		req := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(`{"model":"m","input":"hello world"}`),
		)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if len(upstreamBody) == 0 {
			t.Fatal("upstream body not captured")
		}
		return upstreamBody
	}

	bare := render("m@openai=wire-m")
	full := render("m@openai=wire-m;context=200000;max_output=8192;efforts=low+high;modalities=text+image;default")
	if string(bare) != string(full) {
		t.Fatalf("facts altered the rendered upstream request:\nbare: %s\nfull: %s", bare, full)
	}
}
