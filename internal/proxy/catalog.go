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

package proxy

import (
	"net/http"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// ModelCatalogOption installs a mount's frozen model catalog: the gateway
// answers GET /v1/models locally from the snapshot, in the unlimited admission
// class, without ever contacting the upstream.
type ModelCatalogOption struct {
	config transcode.CatalogConfig
}

// WithModelCatalog returns an option that serves config as this proxy's model
// catalog. The snapshot is cloned; later caller mutation cannot change served
// output. A mount with no catalog configured serves no catalog route at all
// (GET /v1/models passes through to the upstream unchanged).
func WithModelCatalog(config transcode.CatalogConfig) *ModelCatalogOption {
	return &ModelCatalogOption{config: config}
}

func (o *ModelCatalogOption) applyProxyOption(cfg *proxyConfig) error {
	cloned := o.config
	cloned.Models = append([]transcode.CatalogModel(nil), o.config.Models...)
	for i := range cloned.Models {
		cloned.Models[i].Efforts = append([]string(nil), o.config.Models[i].Efforts...)
		cloned.Models[i].Modalities = append([]string(nil), o.config.Models[i].Modalities...)
		if o.config.Models[i].Context != nil {
			value := *o.config.Models[i].Context
			cloned.Models[i].Context = &value
		}
		if o.config.Models[i].MaxOutput != nil {
			value := *o.config.Models[i].MaxOutput
			cloned.Models[i].MaxOutput = &value
		}
	}
	cfg.modelCatalog = &cloned
	return nil
}

// catalogServes reports whether the request is this mount's catalog list route.
// The gate is exact after canonicalization: GET /v1/models and its trailing
// slash / dot-segment equivalents are served locally, while every other method
// or path falls through unchanged.
func (p *Proxy) catalogServes(r *http.Request) bool {
	if p.catalog == nil || r.Method != http.MethodGet {
		return false
	}
	key, err := transcode.NewRouteKey(r.Method, r.URL.Path)
	if err != nil {
		return false
	}
	return key.Path == transcode.CatalogPath
}

// serveCatalog answers a catalog request locally. The exchange is a clean
// passthrough completion: the request was registered in flight as unlimited
// (never queued, never limited), the journal and metrics record it through the
// same finalize path as any other completed exchange, and the circuit breaker
// is not consulted — a local answer must not be gated by upstream health.
func (p *Proxy) serveCatalog(rec *statusRecorder, r *http.Request) {
	p.catalog.ServeHTTP(rec, r)
	if rec.writeFailed {
		rec.aborted = true
	}
	rec.admissionCompleted = true
}
