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

package transcode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestCatalogAnthropicPagination pins cursor filtering, limit slicing, and the
// has_more/first_id/last_id envelope semantics.
func TestCatalogAnthropicPagination(t *testing.T) {
	context := 1000
	handler, err := NewCatalogHandler(CatalogConfig{
		ProviderName: "TestProv",
		Models: []CatalogModel{
			{Surrogate: "a", Context: &context},
			{Surrogate: "b", Context: &context},
			{Surrogate: "c", Context: &context},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	type envelope struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		FirstID *string `json:"first_id"`
		LastID  *string `json:"last_id"`
		HasMore bool    `json:"has_more"`
	}
	fetch := func(query string) envelope {
		t.Helper()
		rec := catalogGet(t, handler, "/v1/models?format=anthropic"+query, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", query, rec.Code, rec.Body.String())
		}
		var document envelope
		if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		return document
	}
	ids := func(document envelope) []string {
		var out []string
		for _, entry := range document.Data {
			out = append(out, entry.ID)
		}
		return out
	}

	full := fetch("")
	if got := strings.Join(ids(full), ","); got != "a,b,c" || full.HasMore {
		t.Errorf("full page = %q has_more=%v", got, full.HasMore)
	}
	if full.FirstID == nil || *full.FirstID != "a" || full.LastID == nil || *full.LastID != "c" {
		t.Errorf("full page cursors = %v/%v", full.FirstID, full.LastID)
	}

	limited := fetch("&limit=2")
	if got := strings.Join(ids(limited), ","); got != "a,b" || !limited.HasMore {
		t.Errorf("limit=2 = %q has_more=%v", got, limited.HasMore)
	}
	if limited.LastID == nil || *limited.LastID != "b" {
		t.Errorf("limit=2 last_id = %v", limited.LastID)
	}

	after := fetch("&after_id=a")
	if got := strings.Join(ids(after), ","); got != "b,c" || after.HasMore {
		t.Errorf("after_id=a = %q has_more=%v", got, after.HasMore)
	}
	if after.FirstID == nil || *after.FirstID != "b" {
		t.Errorf("after_id=a first_id = %v", after.FirstID)
	}

	afterLimit := fetch("&after_id=a&limit=1")
	if got := strings.Join(ids(afterLimit), ","); got != "b" || !afterLimit.HasMore {
		t.Errorf("after_id=a&limit=1 = %q has_more=%v", got, afterLimit.HasMore)
	}

	before := fetch("&before_id=c")
	if got := strings.Join(ids(before), ","); got != "a,b" || before.HasMore {
		t.Errorf("before_id=c = %q has_more=%v", got, before.HasMore)
	}

	beforeLimit := fetch("&before_id=c&limit=1")
	if got := strings.Join(ids(beforeLimit), ","); got != "b" || !beforeLimit.HasMore {
		t.Errorf("before_id=c&limit=1 = %q has_more=%v", got, beforeLimit.HasMore)
	}

	emptyWindow := fetch("&after_id=c")
	if got := ids(emptyWindow); len(got) != 0 || emptyWindow.HasMore {
		t.Errorf("after_id=c = %v has_more=%v, want an honest empty page", got, emptyWindow.HasMore)
	}
	if emptyWindow.FirstID != nil || emptyWindow.LastID != nil {
		t.Errorf("empty page cursors = %v/%v, want null", emptyWindow.FirstID, emptyWindow.LastID)
	}

	intersection := fetch("&after_id=a&before_id=c")
	if got := strings.Join(ids(intersection), ","); got != "b" {
		t.Errorf("after_id=a&before_id=c = %q", got)
	}
	inverted := fetch("&after_id=b&before_id=a")
	if got := ids(inverted); len(got) != 0 {
		t.Errorf("inverted cursors = %v, want empty", got)
	}

	for _, bad := range []string{"&limit=0", "&limit=-1", "&limit=1001", "&limit=abc", "&limit=", "&after_id=", "&before_id=unknown"} {
		rec := catalogGet(t, handler, "/v1/models?format=anthropic"+bad, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400: %s", bad, rec.Code, rec.Body.String())
		}
	}
}

func TestCatalogAnthropicDefaultLimitIsTwenty(t *testing.T) {
	models := make([]CatalogModel, 21)
	for i := range models {
		models[i] = CatalogModel{Surrogate: fmt.Sprintf("m-%02d", i)}
	}
	handler, err := NewCatalogHandler(CatalogConfig{ProviderName: "test", Models: models})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		target   string
		wantLen  int
		wantMore bool
	}{
		{target: "/v1/models?format=anthropic", wantLen: 20, wantMore: true},
		{target: "/v1/models?format=anthropic&limit=1000", wantLen: 21, wantMore: false},
	} {
		rec := catalogGet(t, handler, tc.target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", tc.target, rec.Code, rec.Body.String())
		}
		var document struct {
			Data    []json.RawMessage `json:"data"`
			HasMore bool              `json:"has_more"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		if len(document.Data) != tc.wantLen || document.HasMore != tc.wantMore {
			t.Errorf("%s returned len=%d has_more=%v, want %d/%v", tc.target, len(document.Data), document.HasMore, tc.wantLen, tc.wantMore)
		}
	}
}

func TestCatalogAnthropicOrdersKnownModelsNewestFirst(t *testing.T) {
	handler, err := NewCatalogHandler(CatalogConfig{
		ProviderName: "test",
		Models: []CatalogModel{
			{Surrogate: "older", Created: 100},
			{Surrogate: "newest", Created: 300},
			{Surrogate: "unknown"},
			{Surrogate: "middle", Created: 200},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := catalogGet(t, handler, "/v1/models?format=anthropic", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var document struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(document.Data))
	for _, model := range document.Data {
		got = append(got, model.ID)
	}
	if want := "newest,middle,older,unknown"; strings.Join(got, ",") != want {
		t.Fatalf("model order = %q, want %q", strings.Join(got, ","), want)
	}
}

func TestCatalogGenerationLimitUsesServerErrorMetadata(t *testing.T) {
	handler, err := NewCatalogHandler(CatalogConfig{
		Models: []CatalogModel{{Surrogate: "large", Description: strings.Repeat("x", 2048)}},
		Limits: BodyLimits{GeneratedResponseBytes: MinGeneratedResponseBytes},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"/v1/models?format=openai",
		"/v1/models?format=anthropic",
	} {
		rec := catalogGet(t, handler, target, nil)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s status = %d: %s", target, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"type":"api_error"`) {
			t.Errorf("%s body lacks api_error metadata: %s", target, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "invalid_request_error") {
			t.Errorf("%s server error retained client-error metadata: %s", target, rec.Body.String())
		}
		if strings.Contains(target, "openai") && !strings.Contains(rec.Body.String(), `"code":"internal_server_error"`) {
			t.Errorf("%s body lacks internal_server_error code: %s", target, rec.Body.String())
		}
	}
}

func TestCatalogQueryRejectsMalformedAndAmbiguousValues(t *testing.T) {
	handler := catalogFixture(t)
	for _, target := range []string{
		"/v1/models?format=anthropic;beta=1",
		"/v1/models?format=openai&format=codex",
		"/v1/models?format=anthropic&after_id=kimi-k3&after_id=glm-5.2",
		"/v1/models?format=anthropic&before_id=kimi-k3&before_id=glm-5.2",
		"/v1/models?format=anthropic&limit=1&limit=2",
		"/v1/models?client_version=1&client_version=2",
		"/v1/models?beta=true&beta=false",
		"/v1/models?format=",
		"/v1/models/kimi-k3?format=openai&format=codex",
	} {
		rec := catalogGet(t, handler, target, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400: %s", target, rec.Code, rec.Body.String())
		}
	}
}

func TestCatalogSingleModelErrorIsBounded(t *testing.T) {
	handler, err := NewCatalogHandler(CatalogConfig{
		ProviderName: "test",
		Models:       []CatalogModel{{Surrogate: "known"}},
		Limits: BodyLimits{
			ErrorResponseBytes: catalogMinErrorResponseBytes,
			ErrorMessageBytes:  4,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := catalogGet(t, handler, "/v1/models/"+strings.Repeat("x", 10_000), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Body.Len(); got > catalogMinErrorResponseBytes {
		t.Fatalf("error body = %d bytes, bound = %d", got, catalogMinErrorResponseBytes)
	}
	var document struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
		t.Fatalf("bounded error is not JSON: %v: %s", err, rec.Body.String())
	}
	if len(document.Error.Message) > 4 {
		t.Fatalf("error message = %q, exceeds configured bound 4", document.Error.Message)
	}
}

func TestCatalogRejectsUnusablySmallErrorResponseLimit(t *testing.T) {
	for name, limits := range map[string]BodyLimits{
		"error response":     {ErrorResponseBytes: catalogMinErrorResponseBytes - 1},
		"generated response": {GeneratedResponseBytes: -1},
		"error message":      {ErrorMessageBytes: -1},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewCatalogHandler(CatalogConfig{
				Models: []CatalogModel{{Surrogate: "m"}},
				Limits: limits,
			})
			if err == nil {
				t.Fatal("NewCatalogHandler accepted invalid body limits")
			}
		})
	}
}
