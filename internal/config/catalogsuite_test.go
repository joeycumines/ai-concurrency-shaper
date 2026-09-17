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
	"reflect"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func TestParseCatalogSuite_Valid(t *testing.T) {
	cases := []struct {
		raw      string
		wantName string
		wantPfx  string
		wantMods []string
		wantFmt  transcode.CatalogShape
		wantProv string
		wantStr  bool
	}{
		{
			raw:      "/fleet",
			wantName: "fleet",
			wantPfx:  "/fleet",
			wantMods: nil,
			wantFmt:  "",
			wantProv: "",
			wantStr:  true,
		},
		{
			raw:      "custom@/suite1=gpt-4o+claude-3;format=codex;provider=prov1;strict=false",
			wantName: "custom",
			wantPfx:  "/suite1",
			wantMods: []string{"gpt-4o", "claude-3"},
			wantFmt:  transcode.CatalogShapeCodex,
			wantProv: "prov1",
			wantStr:  false,
		},
		{
			raw:      "/all=*;format=anthropic",
			wantName: "all",
			wantPfx:  "/all",
			wantMods: nil,
			wantFmt:  transcode.CatalogShapeAnthropic,
			wantProv: "",
			wantStr:  true,
		},
		{
			raw:      "my-openai@/openai-suite;format=openai",
			wantName: "my-openai",
			wantPfx:  "/openai-suite",
			wantMods: nil,
			wantFmt:  transcode.CatalogShapeOpenAI,
			wantProv: "",
			wantStr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			cfg, err := parseCatalogSuite(tc.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", cfg.Name, tc.wantName)
			}
			if cfg.Prefix != tc.wantPfx {
				t.Errorf("Prefix = %q, want %q", cfg.Prefix, tc.wantPfx)
			}
			if !reflect.DeepEqual(cfg.Models, tc.wantMods) {
				t.Errorf("Models = %v, want %v", cfg.Models, tc.wantMods)
			}
			if cfg.Format != tc.wantFmt {
				t.Errorf("Format = %q, want %q", cfg.Format, tc.wantFmt)
			}
			if cfg.Provider != tc.wantProv {
				t.Errorf("Provider = %q, want %q", cfg.Provider, tc.wantProv)
			}
			if cfg.Strict != tc.wantStr {
				t.Errorf("Strict = %v, want %v", cfg.Strict, tc.wantStr)
			}
		})
	}
}

func TestParseCatalogSuite_Invalid(t *testing.T) {
	invalids := []string{
		"",
		"noprefix",
		"/suite;format=unknown",
		"/suite;format=",
		"/suite;provider=",
		"/suite;unknown_opt=val",
		"/suite=invalid!model",
	}
	for _, raw := range invalids {
		t.Run(raw, func(t *testing.T) {
			_, err := parseCatalogSuite(raw)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", raw)
			}
		})
	}
}

func TestResolveCatalogSuites_ZeroProviders(t *testing.T) {
	// Zero providers with catalog suite configured: succeeds
	cfg := &Config{
		Server: Server{
			CatalogSuites: []string{"/catalog"},
			ModelTable:    []string{"m1@p1=wire1;context=128000"},
		},
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate failed for zero providers with catalog suite: %v", err)
	}
	suites := cfg.CatalogSuites()
	if len(suites) != 1 || suites[0].Prefix != "/catalog" {
		t.Fatalf("unexpected suites: %+v", suites)
	}
	models := cfg.ModelTable()
	if len(models) != 1 || models[0].Surrogate != "m1" {
		t.Fatalf("unexpected model table: %+v", models)
	}
}

func TestResolveCatalogSuites_PrefixOverlap(t *testing.T) {
	// Overlap between two catalog suites
	cfg := &Config{
		Server: Server{
			CatalogSuites: []string{"/suite", "/suite/sub"},
		},
	}
	if err := cfg.ResolveAndValidate(); err == nil {
		t.Fatal("expected error for overlapping catalog suites, got nil")
	}

	// Overlap between catalog suite and provider
	cfg2 := &Config{
		Server: Server{
			CatalogSuites: []string{"/openai"},
		},
		Providers: []*Provider{
			{
				Name:     "openai",
				Upstream: "https://api.openai.com",
				Prefix:   "/openai",
			},
		},
	}
	if err := cfg2.ResolveAndValidate(); err == nil {
		t.Fatal("expected error for catalog suite overlapping with provider prefix, got nil")
	}
}
