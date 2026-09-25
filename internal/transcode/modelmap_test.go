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
	"fmt"
	"strings"
	"testing"
)

func TestModelMapExplicitMissNamesBoundedServableModels(t *testing.T) {
	exact := make(map[string]ModelMapping, 25)
	for i := range 25 {
		name := fmt.Sprintf("model-%02d", i)
		exact[name] = ModelMapping{
			ClientModel:         name,
			UpstreamModel:       "wire-" + name,
			ClientResponseModel: name,
		}
	}
	exact["zeta"] = ModelMapping{ClientModel: "zeta", UpstreamModel: "wire-zeta"}
	delete(exact, "model-24")

	_, err := (ModelMap{Exact: exact, RequireExplicitMap: true}).Resolve("unknown")
	if err == nil {
		t.Fatal("explicit model-map miss returned nil error")
	}
	message := err.Error()
	if !strings.Contains(message, `no upstream model mapping for client model "unknown"`) {
		t.Fatalf("error does not name the unresolved model: %s", message)
	}
	if !strings.Contains(message, "servable on this mount: model-00, model-01") {
		t.Fatalf("error does not begin with deterministic servable models: %s", message)
	}
	if !strings.Contains(message, "(+5 more)") {
		t.Fatalf("error does not report the bounded remainder: %s", message)
	}
	if strings.Contains(message, "zeta") {
		t.Fatalf("error exceeded the model-list bound: %s", message)
	}
}

func TestModelMapExplicitMissEmptyCatalogIsExplicit(t *testing.T) {
	_, err := (ModelMap{RequireExplicitMap: true}).Resolve("unknown")
	if err == nil {
		t.Fatal("explicit model-map miss returned nil error")
	}
	if got, want := err.Error(), `no upstream model mapping for client model "unknown"; servable on this mount: none`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}
