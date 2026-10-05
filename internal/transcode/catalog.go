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
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
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
	catalogCodexShellCommand                = "shell_command"
	catalogCodexVisibilityList              = "list"
	catalogCodexVisibilityHide              = "hide"
	catalogTruncationMode                   = "tokens"
	catalogCodexFallbackContextTokens       = 32768
	catalogMinErrorResponseBytes            = 128
	anthropicModelType                      = "model"
	MaxModelCreatedUnix               int64 = 253402300799
	anthropicCreatedAtEpoch                 = "1970-01-01T00:00:00Z"
	openAIListObject                        = "list"
	openAIModelObject                       = "model"
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
// default shape; ParallelToolCalls comes from the mount's resolved chat
// capability.
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
	if err := cfg.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("body limits: %w", err)
	}
	limits := cfg.Limits.WithDefaults()
	if limits.ErrorResponseBytes < catalogMinErrorResponseBytes {
		return nil, fmt.Errorf("body limits: ErrorResponseBytes must be at least %d for catalog errors", catalogMinErrorResponseBytes)
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

// strictCatalogQuery parses the raw query instead of URL.Query so malformed
// escapes and semicolon separators cannot disappear into a partial map. Every
// recognized key is single-valued: repeated selectors and cursors are
// ambiguous and must not be silently resolved to the first value.
func strictCatalogQuery(r *http.Request) (map[string][]string, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("invalid catalog query: %w", err)
	}
	for key, values := range query {
		if len(values) != 1 {
			return nil, fmt.Errorf("query parameter %q must appear exactly once", key)
		}
		if strings.TrimSpace(values[0]) == "" {
			return nil, fmt.Errorf("query parameter %q must not be empty", key)
		}
	}
	return query, nil
}

// catalogShapeFor validates the request's shape signals and returns the
// selected dialect. format wins over client_version, which wins over the
// Anthropic client headers, which win over the mount default. An unknown format
// value is rejected with the default dialect's error envelope.
func (h *CatalogHandler) catalogShapeFor(r *http.Request) (CatalogShape, error) {
	query, err := strictCatalogQuery(r)
	if err != nil {
		return h.defaultShape(), err
	}

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
	query, err := strictCatalogQuery(r)
	if err != nil {
		h.writeDialectError(w, shape, http.StatusBadRequest, err)
		return
	}
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
// keeps a hostile servable list from amplifying client-visible text. The
// type/code are the catalog's own, kept verbatim from before the client-error
// unification: generic client faults render invalid_request_error/bad_request
// and local failures render api_error/internal_server_error, pinned by tests.
// The single-model 404 keeps its deliberate model-specific rendering
// (not_found_error / model_not_found, pinned by tests) at its own call site
// instead of in this generic table.
func (h *CatalogHandler) writeDialectError(w http.ResponseWriter, shape CatalogShape, status int, err error) {
	message := BoundErrorMessage(err.Error(), h.limits.ErrorMessageBytes)
	errorType, errorCode := "invalid_request_error", "bad_request"
	if status >= http.StatusInternalServerError {
		errorType, errorCode = "api_error", "internal_server_error"
	}
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
			}{Type: errorType, Message: message},
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
			}{Message: message, Type: errorType, Param: nil, Code: errorCode},
		})
	}
	h.writeErrorBody(w, shape, status, body)
}

// writeErrorBody applies the generated error-response bound before committing
// headers. The small fallback preserves a dialect envelope even when a custom
// ErrorMessageBytes is larger than ErrorResponseBytes; NewCatalogHandler keeps
// the configured limit large enough for that fallback.
func (h *CatalogHandler) writeErrorBody(w http.ResponseWriter, shape CatalogShape, status int, body []byte) {
	if int64(len(body)) > h.limits.ErrorResponseBytes {
		if status >= http.StatusInternalServerError {
			if shape == CatalogShapeAnthropic {
				body = []byte(`{"type":"error","error":{"type":"api_error","message":"catalog error"}}`)
			} else {
				body = []byte(`{"error":{"message":"catalog error","type":"api_error","param":null,"code":"internal_server_error"}}`)
			}
		} else if shape == CatalogShapeAnthropic {
			body = []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"catalog error"}}`)
		} else {
			body = []byte(`{"error":{"message":"catalog error","type":"invalid_request_error","param":null,"code":"bad_request"}}`)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *CatalogHandler) serveSingleModel(w http.ResponseWriter, r *http.Request, modelID string) {
	shape, err := h.catalogShapeFor(r)
	if err != nil {
		h.writeDialectError(w, shape, http.StatusBadRequest, err)
		return
	}
	query, err := strictCatalogQuery(r)
	if err != nil {
		h.writeDialectError(w, shape, http.StatusBadRequest, err)
		return
	}
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
	message := BoundErrorMessage(
		fmt.Sprintf("model: %s not found", modelID),
		h.limits.ErrorMessageBytes,
	)
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
				Type:    "invalid_request_error",
				Param:   "model",
				Code:    "model_not_found",
			},
		})
	}
	h.writeErrorBody(w, shape, http.StatusNotFound, body)
}

// validCatalogModel is the render-time backstop: startup validation is the
// gate, and this re-check guarantees one malformed entry can never fail (or
// corrupt) the whole listing.
func validCatalogModel(model CatalogModel) bool {
	if !validCatalogSurrogate(model.Surrogate) {
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
	if model.Created < 0 || model.Created > MaxModelCreatedUnix {
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

// ModelModalities is the closed input-modality vocabulary the gateway
// advertises. Text and image ride a wire encoding in every dialect; audio is
// receivable on the chat upstream surface (input_audio arm) and advertised
// accordingly; video has no wire encoding in any dialect today and is
// vocabulary-only (advertisable, never receivable — see
// scratch/modality-av-research.md).
var ModelModalities = []string{"text", "image", "audio", "video"}

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

// MaxCatalogIdentLen caps a catalog identifier: the same cap the -model-table
// grammar enforces, so a table-accepted surrogate is never refused at render.
const MaxCatalogIdentLen = 128

// ValidCatalogIdent reports whether s is a valid catalog identifier: 1-128
// chars of [A-Za-z0-9._-], excluding the dot segments. It is the single
// identifier grammar shared by the -model-table parser, the catalog-suite
// names, and the catalog render-time backstop — one definition, so a grammar
// change propagates everywhere by construction.
func ValidCatalogIdent(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > MaxCatalogIdentLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// validCatalogSurrogate enforces the identifier grammar that also makes a
// single-model path addressable as exactly one segment.
func validCatalogSurrogate(s string) bool {
	return ValidCatalogIdent(s)
}

// TopLevelModel extracts the top-level "model" field from a request document
// under one documented policy shared by the suite router, the native route,
// and the byte-surgical rewriter: structural problems (duplicate keys at any
// depth, trailing values, malformed syntax) are errors; a present model must
// be a JSON string or null (numbers, objects, and nested-only documents do
// not count — null decodes as "" per encoding/json, joining the empty
// verdict below); absence is reported as ("", nil) so the caller decides between miss
// and refuse. An empty string is returned as-is, also with nil error — the
// caller owns the empty verdict (the suite reports missing-or-empty, the
// native path reports must-be-non-empty-string). Probes confirm suite, native,
// and rewrite verdicts agree on every shape in this policy.
func TopLevelModel(body []byte) (string, error) {
	var doc map[string]json.RawMessage
	if err := wire.DecodeTolerant(body, &doc); err != nil {
		return "", err
	}
	raw, ok := doc["model"]
	if !ok {
		return "", nil
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return "", err
	}
	return model, nil
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
