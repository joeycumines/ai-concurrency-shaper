package main

// The startup wiring for the gateway-hosted model catalog: buildProvider hands
// the resolved provider's frozen snapshot to the proxy, so a configured table
// serves GET /v1/models locally through the composed binary path.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/config"
	"github.com/joeycumines/ai-concurrency-shaper/internal/router"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func TestBuildProviderServesConfiguredCatalog(t *testing.T) {
	cfg, err := config.Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
		"-model-table", "m@openai=wire-m;context=1000",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	prx, _, _, err := buildProvider(cfg.Providers[0])
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	rec := httptest.NewRecorder()
	prx.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"models":[`, `"slug":"m"`, `"context_window":1000`} {
		if !strings.Contains(body, want) {
			t.Errorf("catalog body missing %q: %s", want, body)
		}
	}
}

func TestCatalogSuiteWiring_SingleUnnamedProvider(t *testing.T) {
	cfg, err := config.Parse([]string{
		"-upstream", "https://api.openai.com",
		"-prefix", "/openai",
		"-transcode-responses-chat",
		"-model-table", "m@openai=wire-m;context=1000",
		"-catalog-suite", "/suite",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	pr := cfg.Providers[0]
	if pr.Name != "" {
		t.Fatalf("expected unnamed provider, got %q", pr.Name)
	}
	if pr.EffectiveName() != "openai" {
		t.Fatalf("expected effective name openai, got %q", pr.EffectiveName())
	}

	prx, _, _, err := buildProvider(pr)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	proxiesByName := map[string]http.Handler{
		pr.EffectiveName(): prx,
	}

	allModels := cfg.ModelTable()
	suite := cfg.CatalogSuites()[0]
	var suiteModels []transcode.CatalogModel
	for _, m := range allModels {
		if suite.Provider != "" && m.Provider != suite.Provider {
			continue
		}
		suiteModels = append(suiteModels, m)
	}
	var modelRoutes []router.ModelRoute
	for _, sm := range suiteModels {
		if targetProxy := proxiesByName[sm.Provider]; targetProxy != nil {
			modelRoutes = append(modelRoutes, router.ModelRoute{
				Model:    sm.Surrogate,
				Provider: sm.Provider,
				Handler:  targetProxy,
			})
		}
	}
	if len(modelRoutes) != 1 {
		t.Fatalf("modelRoutes count = %d, want 1", len(modelRoutes))
	}
	if modelRoutes[0].Model != "m" || modelRoutes[0].Provider != "openai" {
		t.Errorf("modelRoute[0] = %+v, want model m provider openai", modelRoutes[0])
	}
}

