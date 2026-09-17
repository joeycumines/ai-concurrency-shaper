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

package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// ModelRoute maps a model identifier to the provider handler responsible for serving it.
type ModelRoute struct {
	Model    string
	Provider string
	Handler  http.Handler
}

// SuiteConfig configures a CatalogSuiteHandler.
type SuiteConfig struct {
	Name           string
	Prefix         string
	CatalogHandler *transcode.CatalogHandler
	DefaultShape   transcode.CatalogShape
	ModelRoutes    []ModelRoute
	Fallback       http.Handler // optional fallback handler when 1 provider is available
}

// CatalogSuiteHandler serves catalog queries and routes model-specific completion
// requests to the appropriate target provider.
type CatalogSuiteHandler struct {
	name           string
	prefix         string
	catalogHandler *transcode.CatalogHandler
	defaultShape   transcode.CatalogShape
	modelMap       map[string]http.Handler
	fallback       http.Handler
}

// NewCatalogSuiteHandler builds a new CatalogSuiteHandler.
func NewCatalogSuiteHandler(cfg SuiteConfig) *CatalogSuiteHandler {
	models := make(map[string]http.Handler, len(cfg.ModelRoutes))
	for _, mr := range cfg.ModelRoutes {
		if mr.Model != "" && mr.Handler != nil {
			models[mr.Model] = mr.Handler
		}
	}
	return &CatalogSuiteHandler{
		name:           cfg.Name,
		prefix:         cfg.Prefix,
		catalogHandler: cfg.CatalogHandler,
		defaultShape:   cfg.DefaultShape,
		modelMap:       models,
		fallback:       cfg.Fallback,
	}
}

// ServeHTTP handles requests to the catalog suite mount.
// The incoming request path is already stripped of the suite prefix.
func (h *CatalogSuiteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqPath := r.URL.Path

	// Catalog discovery routes: GET /v1/models and GET /v1/models/{model}
	if r.Method == http.MethodGet && (reqPath == transcode.CatalogPath || strings.HasPrefix(reqPath, transcode.CatalogPath+"/")) {
		if h.catalogHandler != nil {
			h.catalogHandler.ServeHTTP(w, r)
			return
		}
		// Empty / unbacked catalog: return empty list or 404
		h.writeEmptyCatalogOr404(w, r)
		return
	}

	// Completion routes: inspect "model" in POST body
	if r.Method == http.MethodPost && isCompletionRoute(reqPath) {
		h.serveCompletion(w, r)
		return
	}

	// Any other route: if fallback handler exists, forward
	if h.fallback != nil {
		h.fallback.ServeHTTP(w, r)
		return
	}

	http.NotFound(w, r)
}

func isCompletionRoute(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/responses", "/v1/messages":
		return true
	default:
		return false
	}
}

func (h *CatalogSuiteHandler) serveCompletion(w http.ResponseWriter, r *http.Request) {
	shape := h.shapeForCompletion(r)

	// Read body with limit (up to 10MB) to peek model
	const maxPeekBytes = 10 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPeekBytes+1))
	_ = r.Body.Close()
	if err != nil {
		writeDialectError(w, shape, http.StatusBadRequest, "failed to read request body")
		return
	}
	if int64(len(body)) > maxPeekBytes {
		writeDialectError(w, shape, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	modelID := peekModelField(body)
	if modelID == "" {
		writeDialectError(w, shape, http.StatusBadRequest, "missing or empty 'model' field in request")
		return
	}

	target := h.modelMap[modelID]
	if target == nil {
		if h.fallback != nil {
			target = h.fallback
		} else {
			if len(h.modelMap) == 0 {
				writeDialectError(w, shape, http.StatusServiceUnavailable, fmt.Sprintf("no provider configured to serve model %q", modelID))
			} else {
				writeDialectError(w, shape, http.StatusNotFound, fmt.Sprintf("model %q not found in catalog suite", modelID))
			}
			return
		}
	}

	// Restore request body for target
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	target.ServeHTTP(w, r)
}

func peekModelField(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

func (h *CatalogSuiteHandler) shapeForCompletion(r *http.Request) transcode.CatalogShape {
	switch r.URL.Path {
	case "/v1/responses":
		return transcode.CatalogShapeCodex
	case "/v1/messages":
		return transcode.CatalogShapeAnthropic
	default:
		if h.defaultShape != "" {
			return h.defaultShape
		}
		return transcode.CatalogShapeOpenAI
	}
}

func (h *CatalogSuiteHandler) writeEmptyCatalogOr404(w http.ResponseWriter, r *http.Request) {
	shape := h.defaultShape
	if shape == "" {
		shape = transcode.CatalogShapeOpenAI
	}
	if strings.HasPrefix(r.URL.Path, transcode.CatalogPath+"/") {
		modelID := strings.TrimPrefix(r.URL.Path, transcode.CatalogPath+"/")
		modelID = strings.Trim(modelID, "/")
		writeModelNotFoundError(w, shape, modelID)
		return
	}

	var body []byte
	switch shape {
	case transcode.CatalogShapeCodex:
		body = []byte(`{"models":[]}`)
	case transcode.CatalogShapeAnthropic:
		body = []byte(`{"data":[],"has_more":false}`)
	default:
		body = []byte(`{"object":"list","data":[]}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func writeDialectError(w http.ResponseWriter, shape transcode.CatalogShape, status int, message string) {
	var body []byte
	switch shape {
	case transcode.CatalogShapeAnthropic:
		body, _ = json.Marshal(struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}{
			Type: "error",
			Error: struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}{
				Type:    errTypeForStatus(status),
				Message: message,
			},
		})
	default:
		body, _ = json.Marshal(struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Param   any    `json:"param"`
				Code    string `json:"code"`
			} `json:"error"`
		}{
			Error: struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Param   any    `json:"param"`
				Code    string `json:"code"`
			}{
				Message: message,
				Type:    errTypeForStatus(status),
				Param:   nil,
				Code:    errCodeForStatus(status),
			},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeModelNotFoundError(w http.ResponseWriter, shape transcode.CatalogShape, modelID string) {
	var body []byte
	switch shape {
	case transcode.CatalogShapeAnthropic:
		body, _ = json.Marshal(struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}{
			Type: "error",
			Error: struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}{
				Type:    "not_found_error",
				Message: fmt.Sprintf("model: %s not found", modelID),
			},
		})
	default:
		body, _ = json.Marshal(struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Param   any    `json:"param"`
				Code    string `json:"code"`
			} `json:"error"`
		}{
			Error: struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Param   any    `json:"param"`
				Code    string `json:"code"`
			}{
				Message: fmt.Sprintf("The model %q does not exist", modelID),
				Type:    "invalid_request_error",
				Param:   "model",
				Code:    "model_not_found",
			},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(body)
}

func errTypeForStatus(status int) string {
	switch status {
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusServiceUnavailable:
		return "api_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	default:
		return "api_error"
	}
}

func errCodeForStatus(status int) string {
	switch status {
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	default:
		return "api_error"
	}
}
