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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const fullProvidersJSON = `{
  "providers": [
    {
      "name": "anthropic",
      "upstream": "https://api.anthropic.com",
      "prefix": "/anthropic",
      "limits": ["POST /v1/messages:3@messages", "POST /v1/messages/batches:3@messages"],
      "concurrency": 2,
      "auth_source": "env:FILE_TEST_ANTHROPIC_KEY"
    },
    {
      "name": "openai",
      "upstream": "${FILE_TEST_OPENAI_HOST}",
      "prefix": "/openai",
      "queue_timeout": "45s",
      "retry_skip_429": false,
      "circuit_breaker": false,
      "auth_mode": "bearer",
      "auth_source": "env:FILE_TEST_OPENAI_KEY",
      "anthropic_version": ""
    }
  ]
}`

// TestLoadFile_HappyPath covers a full two-provider load: defaults for absent
// fields, explicit overrides, group limits, auth references, ${ENV} expansion,
// and merge order with a CLI section.
func TestLoadFile_HappyPath(t *testing.T) {
	t.Setenv("FILE_TEST_ANTHROPIC_KEY", "file-anthropic-secret")
	t.Setenv("FILE_TEST_OPENAI_HOST", "https://api.openai.com")
	t.Setenv("FILE_TEST_OPENAI_KEY", "file-openai-secret")

	cfg, err := Parse([]string{"-config", "placeholder.json"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	path := writeFile(t, "providers.json", fullProvidersJSON)
	if err := cfg.LoadFile(path); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if len(cfg.Providers) != 2 {
		t.Fatalf("len(Providers) = %d, want 2", len(cfg.Providers))
	}
	a, b := cfg.Providers[0], cfg.Providers[1]

	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}

	if a.Name != "anthropic" || a.Prefix != "/anthropic" || a.Upstream != "https://api.anthropic.com" {
		t.Errorf("provider a identity = %q/%q/%q", a.Name, a.Prefix, a.Upstream)
	}
	if len(a.Limits) != 2 || a.Limits[0] != "POST /v1/messages:3@messages" {
		t.Errorf("a.Limits = %q", a.Limits)
	}
	if a.Concurrency != 2 {
		t.Errorf("a.Concurrency = %d, want 2", a.Concurrency)
	}
	// Absent fields carry the shared CLI defaults.
	if a.RetryMax != defaultRetryMax || a.RetryMaxBodyMB != defaultRetryMaxBodyMB {
		t.Errorf("a retry defaults = %d/%d, want %d/%d", a.RetryMax, a.RetryMaxBodyMB, defaultRetryMax, defaultRetryMaxBodyMB)
	}
	if a.QueueTimeout != defaultQueueTimeout || a.ReleaseCooldown != defaultReleaseCooldown {
		t.Errorf("a duration defaults drifted: %v/%v", a.QueueTimeout, a.ReleaseCooldown)
	}
	if !a.RetrySkipOn429 || !a.CBEnabled {
		t.Errorf("a bool defaults drifted: skip429=%v cb=%v", a.RetrySkipOn429, a.CBEnabled)
	}
	if a.AuthPolicy() == nil || a.AuthPolicy().Mode != "x-api-key" {
		t.Errorf("a auth policy = %+v, want derived x-api-key (anthropic host)", a.AuthPolicy())
	}

	if b.Upstream != "https://api.openai.com" {
		t.Errorf("b.Upstream = %q, want expanded env value", b.Upstream)
	}
	if b.QueueTimeout != 45*time.Second {
		t.Errorf("b.QueueTimeout = %v, want 45s", b.QueueTimeout)
	}
	if b.RetrySkipOn429 {
		t.Error("b.RetrySkipOn429 = true, want explicit false override")
	}
	if b.CBEnabled {
		t.Error("b.CBEnabled = true, want explicit false override")
	}
	if b.Breaker() != nil {
		t.Error("disabled breaker must not be constructed")
	}
	if b.AuthPolicy() == nil || b.AuthPolicy().Mode != "bearer" {
		t.Errorf("b auth policy = %+v, want bearer", b.AuthPolicy())
	}
}

// TestLoadFile_EnvExpansionFailClosed proves a missing ${VAR} reference is a
// load-time error naming the variable.
func TestLoadFile_EnvExpansionFailClosed(t *testing.T) {
	os.Unsetenv("FILE_TEST_DEFINITELY_UNSET")
	cfg := &Config{Providers: []*Provider{{Name: "cli", Upstream: "https://x", Prefix: "/cli"}}}
	path := writeFile(t, "p.json", `{"providers":[{"name":"a","upstream":"https://${FILE_TEST_DEFINITELY_UNSET}"}]}`)
	err := cfg.LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "FILE_TEST_DEFINITELY_UNSET") {
		t.Fatalf("err = %v, want failure naming the missing variable", err)
	}
}

// TestLoadFile_RejectsBadInput covers unknown fields, malformed JSON, empty
// provider arrays, bad durations, and unreadable files — each with a precise
// error.
func TestLoadFile_RejectsBadInput(t *testing.T) {
	base := &Config{Providers: []*Provider{{Name: "cli", Upstream: "https://x", Prefix: "/cli"}}}

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"unknown field", `{"providers":[{"name":"a","upstream":"https://x","unknown_field":1}]}`, "unknown field"},
		{"malformed json", `{providers:[`, "-config"},
		{"empty providers", `{"providers":[]}`, "must not be empty"},
		{"bad duration", `{"providers":[{"name":"a","upstream":"https://x","queue_timeout":"soon"}]}`, "invalid duration"},
		{"missing upstream caught later", `{"providers":[{"name":"a"}]}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, "p.json", tc.content)
			err := base.LoadFile(path)
			if tc.want == "" {
				if err == nil {
					t.Fatal("nil error, want failure")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}

	t.Run("unreadable file names the path", func(t *testing.T) {
		err := (&Config{}).LoadFile(filepath.Join(t.TempDir(), "absent.json"))
		if err == nil || !strings.Contains(err.Error(), "absent.json") {
			t.Fatalf("err = %v, want failure naming the path", err)
		}
	})
}

// TestLoadFile_MergeOrderAndDuplicates pins that CLI sections come after file
// providers and duplicate names surface through ResolveAndValidate.
func TestLoadFile_MergeOrderAndDuplicates(t *testing.T) {
	t.Run("file first then cli section", func(t *testing.T) {
		cfg, err := Parse([]string{
			"-config", "unused.json",
			"--provider=fromcli",
			"-upstream", "https://cli",
			"-prefix", "/cli",
			"-concurrency", "9",
		})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		path := writeFile(t, "p.json", `{"providers":[{"name":"fromfile","upstream":"https://file","prefix":"/file"}]}`)
		if err := cfg.LoadFile(path); err != nil {
			t.Fatalf("LoadFile: %v", err)
		}
		if len(cfg.Providers) != 2 {
			t.Fatalf("len(Providers) = %d, want 2", len(cfg.Providers))
		}
		if cfg.Providers[0].Name != "fromfile" || cfg.Providers[1].Name != "fromcli" {
			t.Errorf("order = %q,%q; want fromfile,fromcli", cfg.Providers[0].Name, cfg.Providers[1].Name)
		}
		if cfg.Providers[1].Concurrency != 9 {
			t.Errorf("CLI section concurrency = %d, want 9", cfg.Providers[1].Concurrency)
		}
		if err := cfg.ResolveAndValidate(); err != nil {
			t.Fatalf("ResolveAndValidate: %v", err)
		}
	})

	t.Run("duplicate names rejected by validation", func(t *testing.T) {
		cfg, err := Parse([]string{
			"-config", "unused.json",
			"--provider=dupe",
			"-upstream", "https://cli",
			"-prefix", "/cli",
		})
		if err != nil {
			t.Fatal(err)
		}
		path := writeFile(t, "p.json", `{"providers":[{"name":"dupe","upstream":"https://file","prefix":"/file"}]}`)
		if err := cfg.LoadFile(path); err != nil {
			t.Fatal(err)
		}
		err = cfg.ResolveAndValidate()
		if err == nil || !strings.Contains(err.Error(), "duplicate provider name") {
			t.Fatalf("err = %v, want duplicate-name rejection", err)
		}
	})

	t.Run("legacy implicit provider replaced", func(t *testing.T) {
		cfg, err := Parse([]string{"-config", "unused.json", "-tui"})
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Providers) != 1 {
			t.Fatalf("legacy Parse should yield one implicit provider")
		}
		path := writeFile(t, "p.json", `{"providers":[{"name":"only","upstream":"https://file"}]}`)
		if err := cfg.LoadFile(path); err != nil {
			t.Fatal(err)
		}
		if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "only" {
			t.Fatalf("implicit legacy provider was not replaced: %+v", cfg.Providers)
		}
	})

	t.Run("configured legacy provider conflicts with config", func(t *testing.T) {
		cfg, err := Parse([]string{"-config", "unused.json", "-upstream", "https://legacy"})
		if err != nil {
			t.Fatal(err)
		}
		path := writeFile(t, "p.json", `{"providers":[{"name":"a","upstream":"https://file"}]}`)
		err = cfg.LoadFile(path)
		if err == nil {
			t.Fatal("nil error: -config with a configured legacy provider must be ambiguous")
		}
		// The configured legacy provider survives untouched so the error
		// surfaces as the documented ambiguity, not silent replacement.
		if cfg.Providers[0].Upstream != "https://legacy" {
			t.Errorf("legacy provider mutated: %+v", cfg.Providers[0])
		}
	})

	t.Run("lone tuning flag counts as configured", func(t *testing.T) {
		cfg, err := Parse([]string{"-config", "unused.json", "-concurrency", "8"})
		if err != nil {
			t.Fatal(err)
		}
		path := writeFile(t, "p.json", `{"providers":[{"name":"a","upstream":"https://file"}]}`)
		err = cfg.LoadFile(path)
		if err == nil || !strings.Contains(err.Error(), "-config cannot be combined") {
			t.Fatalf("err = %v, want ambiguity rejection (tuning flag must not be silently dropped)", err)
		}
	})

	t.Run("trailing json content rejected", func(t *testing.T) {
		cfg, err := Parse([]string{"-config", "unused.json"})
		if err != nil {
			t.Fatal(err)
		}
		path := writeFile(t, "p.json", `{"providers":[{"name":"a","upstream":"https://x"}]} {"providers":[]}`)
		err = cfg.LoadFile(path)
		if err == nil || !strings.Contains(err.Error(), "unexpected content after") {
			t.Fatalf("err = %v, want trailing-content rejection", err)
		}
	})
}
