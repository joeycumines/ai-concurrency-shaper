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
