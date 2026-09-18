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
	"math"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
)

// The gateway-hosted model catalog answers a mount's discovery request from the
// frozen global model table. It never contacts an upstream: the document is
// rendered from validated configuration, so absence is honest (an absent fact is
// omitted or null, never fabricated) and every listed identifier resolves on the
// mount that served it (the list is a projection of the same table that seeds
// ModelMap.Exact).
//
// Three client dialects are served from one identifier list:
//   - Codex native: {"models":[...]} (the Responses client's discovery shape;
//     an OpenAI-lean document fails its strict decode and empties the picker).
//   - Anthropic messages list: {"data":[...],"first_id":...,"last_id":...,"has_more":...}.
//   - Lean OpenAI list: {"object":"list","data":[...]}.

// CatalogPath is the list route the catalog serves, on the mount-stripped path.
const CatalogPath = "/v1/models"

// CatalogShape selects the served document dialect.
type CatalogShape string

const (
	CatalogShapeCodex     CatalogShape = "codex"
	CatalogShapeAnthropic CatalogShape = "anthropic"
	CatalogShapeOpenAI    CatalogShape = "openai"
)

// Client-required constant spellings (a strict client rejects unknown or absent
// fields, so these are named once and emitted literally).
const (
	catalogCodexShellCommand   = "shell_command"
	catalogCodexVisibilityList = "list"
	catalogCodexVisibilityHide = "hide"
	catalogTruncationMode      = "tokens"
	anthropicModelType         = "model"
	anthropicCreatedAtEpoch    = "1970-01-01T00:00:00Z"
	openAIListObject           = "list"
	openAIModelObject          = "model"
)

// CatalogModel is one frozen, presentation-only model entry: identity plus the
// facts the documents may carry. No resolution path reads it.
type CatalogModel struct {
	Surrogate   string
	Provider    string
	Context     *int
	MaxOutput   *int
	Efforts     []string
	Modalities  []string
	Default     bool
	Deprecated  bool
	CostInput   *float64
	CostOutput  *float64
	Tags        []string
	Description string
	Created     int64
}

// CatalogConfig is one mount's frozen catalog snapshot. ServesResponses and
// ServesMessages record the mount's transcode client dialects and drive the
// default shape; ParallelToolCalls and StructuredOutputs come from the mount's
// resolved chat capabilities (the ecosystem catalogs observe both true for
// mounts without a chat mapping).
type CatalogConfig struct {
	ProviderName      string
	Models            []CatalogModel
	ServesResponses   bool
	ServesMessages    bool
	ParallelToolCalls bool
	StructuredOutputs bool
	DefaultShape      CatalogShape
	Limits            BodyLimits
}

// CatalogHandler serves the catalog document for one mount.
type CatalogHandler struct {
	provider          string
	models            []CatalogModel
	servesResponses   bool
	servesMessages    bool
	parallelToolCalls bool
	structuredOutputs bool
	defaultCatalog    CatalogShape
	limits            BodyLimits
}

// NewCatalogHandler validates and freezes one mount's catalog snapshot. The
// snapshot is deep-copied; later mutation of cfg cannot change served output.
func NewCatalogHandler(cfg CatalogConfig) (*CatalogHandler, error) {
	limits := cfg.Limits.WithDefaults()
	if err := limits.Validate(); err != nil {
		return nil, fmt.Errorf("body limits: %w", err)
	}
	models := make([]CatalogModel, len(cfg.Models))
	for i, model := range cfg.Models {
		model.Efforts = slices.Clone(model.Efforts)
		model.Modalities = slices.Clone(model.Modalities)
		model.Tags = slices.Clone(model.Tags)
		if model.Context != nil {
			value := *model.Context
			model.Context = &value
		}
		if model.MaxOutput != nil {
			value := *model.MaxOutput
			model.MaxOutput = &value
		}
		if model.CostInput != nil {
			value := *model.CostInput
			model.CostInput = &value
		}
		if model.CostOutput != nil {
			value := *model.CostOutput
			model.CostOutput = &value
		}
		models[i] = model
	}
	return &CatalogHandler{
		provider:          cfg.ProviderName,
		models:            models,
		servesResponses:   cfg.ServesResponses,
		servesMessages:    cfg.ServesMessages,
		parallelToolCalls: cfg.ParallelToolCalls,
		structuredOutputs: cfg.StructuredOutputs,
		defaultCatalog:    cfg.DefaultShape,
		limits:            limits,
	}, nil
}

// catalogShapeFor validates the request's shape signals and returns the
// selected dialect. format wins over client_version, which wins over the
// Anthropic client headers, which win over the mount default. An unknown format
// value is rejected with the default dialect's error envelope.
func (h *CatalogHandler) catalogShapeFor(r *http.Request) (CatalogShape, error) {
	query := r.URL.Query()

	if values, ok := query["format"]; ok {
		if len(values) == 0 || strings.TrimSpace(values[0]) == "" {
			return h.defaultShape(), fmt.Errorf("empty catalog format (want codex, anthropic, or openai)")
		}
		switch strings.ToLower(strings.TrimSpace(values[0])) {
		case "codex", "responses":
			return CatalogShapeCodex, nil
		case "anthropic", "messages":
			return CatalogShapeAnthropic, nil
		case "openai", "chat", "chat-completions":
			return CatalogShapeOpenAI, nil
		}
		return h.defaultShape(), fmt.Errorf("unknown catalog format %q (want codex, anthropic, or openai)", values[0])
	}
	if _, ok := query["client_version"]; ok {
		return CatalogShapeCodex, nil
	}
	if strings.TrimSpace(r.Header.Get("Anthropic-Version")) != "" ||
		strings.TrimSpace(r.Header.Get("Anthropic-Beta")) != "" {
		return CatalogShapeAnthropic, nil
	}
	return h.defaultShape(), nil
}

// defaultShape is the mount's dialect default: a Responses mount is native to
// Codex discovery, a Messages mount to Anthropic clients, and everything else
// (a passthrough-only mount, or one serving both dialects with no single native
// shape) lists the same identifiers in the lean OpenAI document.
func (h *CatalogHandler) defaultShape() CatalogShape {
	if h.defaultCatalog != "" {
		return h.defaultCatalog
	}
	switch {
	case h.servesResponses && !h.servesMessages:
		return CatalogShapeCodex
	case h.servesMessages && !h.servesResponses:
		return CatalogShapeAnthropic
	default:
		return CatalogShapeOpenAI
	}
}

// ServeHTTP renders the selected catalog document or single model item.
func (h *CatalogHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqPath := path.Clean(r.URL.Path)
	if after, ok := strings.CutPrefix(reqPath, CatalogPath+"/"); ok {
		modelID := strings.Trim(after, "/")
		if modelID != "" && !strings.Contains(modelID, "/") {
			h.serveSingleModel(w, r, modelID)
			return
		}
		http.NotFound(w, r)
		return
	}
	if reqPath != CatalogPath {
		http.NotFound(w, r)
		return
	}

	shape, err := h.catalogShapeFor(r)
	if err != nil {
		h.writeDialectError(w, shape, http.StatusBadRequest, err)
		return
	}
	query := r.URL.Query()
	if err := h.validateQuery(shape, query); err != nil {
		h.writeDialectError(w, shape, http.StatusBadRequest, err)
		return
	}

	var document any
	switch shape {
	case CatalogShapeCodex:
		document = h.codexDocument()
	case CatalogShapeAnthropic:
		document, err = h.anthropicDocument(query)
	case CatalogShapeOpenAI:
		document = h.openAIDocument()
	default:
		err = fmt.Errorf("unsupported catalog shape %q", shape)
	}
	if err != nil {
		h.writeDialectError(w, shape, http.StatusBadRequest, err)
		return
	}

	body, err := marshalCatalogDocument(document, h.limits.GeneratedResponseBytes)
	if err != nil {
		h.writeDialectError(w, shape, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// validateQuery rejects a query key the selected shape does not allow; the
// catalog never forwards a query upstream, so an unknown key is a client error
// rather than a silent drop.
func (h *CatalogHandler) validateQuery(shape CatalogShape, query map[string][]string) error {
	allowed := map[string]struct{}{"format": {}, "beta": {}, "client_version": {}}
	if shape == CatalogShapeAnthropic {
		allowed["after_id"] = struct{}{}
		allowed["before_id"] = struct{}{}
		allowed["limit"] = struct{}{}
	}
	for key := range query {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown query parameter %q for the %s catalog", key, shape)
		}
	}
	return nil
}

// marshalCatalogDocument renders the document and enforces the generated
// response bound before any header can commit.
func marshalCatalogDocument(document any, limit int64) ([]byte, error) {
	body, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("render catalog document: %w", err)
	}
	if limit > 0 && int64(len(body)) > limit {
		return nil, fmt.Errorf("catalog document is %d bytes, over the %d byte bound", len(body), limit)
	}
	return body, nil
}

// writeDialectError writes a bounded dialect error envelope. The message bound
// keeps a hostile servable list from amplifying client-visible text.
func (h *CatalogHandler) writeDialectError(w http.ResponseWriter, shape CatalogShape, status int, err error) {
	message := boundCatalogMessage(err.Error(), h.limits.ErrorMessageBytes)
	var body []byte
	switch shape {
	case CatalogShapeAnthropic:
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
			}{Type: "invalid_request_error", Message: message},
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
			}{Message: message, Type: "invalid_request_error", Param: nil, Code: "bad_request"},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// boundCatalogMessage truncates an error message at the configured bound with
// the same ellipsis discipline as the transcode handler.
func boundCatalogMessage(message string, max int) string {
	if max <= 0 || len(message) <= max {
		return message
	}
	if max > 3 {
		return message[:max-3] + "…"
	}
	return message[:max]
}

func (h *CatalogHandler) serveSingleModel(w http.ResponseWriter, r *http.Request, modelID string) {
	shape, err := h.catalogShapeFor(r)
	if err != nil {
		h.writeDialectError(w, shape, http.StatusBadRequest, err)
		return
	}
	query := r.URL.Query()
	if err := h.validateSingleModelQuery(shape, query); err != nil {
		h.writeDialectError(w, shape, http.StatusBadRequest, err)
		return
	}

	var (
		foundIdx = -1
		listed   []int
	)
	for i := range h.models {
		if validCatalogModel(h.models[i]) {
			listed = append(listed, i)
			if h.models[i].Surrogate == modelID {
				foundIdx = i
			}
		}
	}
	if foundIdx < 0 {
		h.writeModelNotFoundError(w, shape, modelID)
		return
	}
	found := &h.models[foundIdx]

	var document any
	switch shape {
	case CatalogShapeCodex:
		priorities := h.codexPriorities(listed)
		document = h.codexEntry(*found, priorities[foundIdx])
	case CatalogShapeAnthropic:
		document = h.anthropicEntry(*found)
	case CatalogShapeOpenAI:
		document = h.openAIEntry(*found)
	default:
		h.writeDialectError(w, shape, http.StatusBadRequest, fmt.Errorf("unsupported catalog shape %q", shape))
		return
	}

	body, err := marshalCatalogDocument(document, h.limits.GeneratedResponseBytes)
	if err != nil {
		h.writeDialectError(w, shape, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (h *CatalogHandler) validateSingleModelQuery(shape CatalogShape, query map[string][]string) error {
	allowed := map[string]struct{}{"format": {}, "beta": {}, "client_version": {}}
	for key := range query {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown query parameter %q for the %s single-model query", key, shape)
		}
	}
	return nil
}

func (h *CatalogHandler) writeModelNotFoundError(w http.ResponseWriter, shape CatalogShape, modelID string) {
	var body []byte
	switch shape {
	case CatalogShapeAnthropic:
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

// validCatalogModel is the render-time backstop: startup validation is the
// gate, and this re-check guarantees one malformed entry can never fail (or
// corrupt) the whole listing.
func validCatalogModel(model CatalogModel) bool {
	if model.Surrogate == "" {
		return false
	}
	if model.Context != nil && *model.Context <= 0 {
		return false
	}
	if model.MaxOutput != nil && *model.MaxOutput <= 0 {
		return false
	}
	for _, effort := range model.Efforts {
		if !ValidModelEffort(effort) {
			return false
		}
	}
	for _, modality := range model.Modalities {
		if !ValidModelModality(modality) {
			return false
		}
	}
	if model.CostInput != nil && (*model.CostInput < 0 || math.Signbit(*model.CostInput) || math.IsNaN(*model.CostInput) || math.IsInf(*model.CostInput, 0)) {
		return false
	}
	if model.CostOutput != nil && (*model.CostOutput < 0 || math.Signbit(*model.CostOutput) || math.IsNaN(*model.CostOutput) || math.IsInf(*model.CostOutput, 0)) {
		return false
	}
	if model.Created < 0 {
		return false
	}
	return true
}

// ModelEfforts is the closed reasoning-effort vocabulary the shaper can
// honestly advertise: the canonical four efforts plus xhigh and max, which
// real Responses clients advertise in their own model catalogs and dispatch
// on the wire. The -model-table grammar and the catalog render-time re-check
// both consume this single definition, so they cannot contradict each other.
var ModelEfforts = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// ModelModalities is the closed input-modality vocabulary: the three
// modalities the served client shapes can carry.
var ModelModalities = []string{"text", "image", "audio"}

var (
	modelEffortSet   = buildVocabularySet(ModelEfforts)
	modelModalitySet = buildVocabularySet(ModelModalities)
)

func buildVocabularySet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[v] = struct{}{}
	}
	return set
}

// ValidModelEffort reports whether s is in the closed effort vocabulary.
func ValidModelEffort(s string) bool { _, ok := modelEffortSet[s]; return ok }

// ValidModelModality reports whether s is in the closed modality vocabulary.
func ValidModelModality(s string) bool { _, ok := modelModalitySet[s]; return ok }

// queryValue returns the first value of a query key and whether the key was
// present at all: an explicitly empty value is malformed input, not an absent
// parameter, so cursors and limit parse it strictly.
func queryValue(query map[string][]string, key string) (string, bool) {
	values, ok := query[key]
	if !ok {
		return "", false
	}
	if len(values) == 0 {
		return "", true
	}
	return values[0], true
}
