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
	Surrogate  string
	Context    *int
	MaxOutput  *int
	Efforts    []string
	Modalities []string
	Default    bool
	Deprecated bool
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
		if model.Context != nil {
			value := *model.Context
			model.Context = &value
		}
		if model.MaxOutput != nil {
			value := *model.MaxOutput
			model.MaxOutput = &value
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
	switch {
	case h.servesResponses && !h.servesMessages:
		return CatalogShapeCodex
	case h.servesMessages && !h.servesResponses:
		return CatalogShapeAnthropic
	default:
		return CatalogShapeOpenAI
	}
}

// ServeHTTP renders the selected catalog document.
func (h *CatalogHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

// codexPriorities assigns the 1-based priority of every listed model in
// declaration order: a lone default takes priority 1 and the rest follow in
// declaration order, otherwise declaration order decides. Only listed (valid)
// models receive a priority, so a skipped entry cannot leave a numbering gap.
func (h *CatalogHandler) codexPriorities(listed []int) map[int]int {
	priorities := make(map[int]int, len(listed))
	defaultIndex := -1
	for _, i := range listed {
		if h.models[i].Default {
			defaultIndex = i
			break
		}
	}
	next := 1
	if defaultIndex >= 0 {
		priorities[defaultIndex] = next
		next++
	}
	for _, i := range listed {
		if i == defaultIndex {
			continue
		}
		priorities[i] = next
		next++
	}
	return priorities
}

type codexCatalogDocument struct {
	Models []codexCatalogEntry `json:"models"`
}

type codexTruncationPolicy struct {
	Mode  string `json:"mode"`
	Limit int    `json:"limit"`
}

type codexReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

type codexCatalogEntry struct {
	Slug                       string                 `json:"slug"`
	DisplayName                string                 `json:"display_name"`
	SupportedInAPI             bool                   `json:"supported_in_api"`
	ShellType                  string                 `json:"shell_type"`
	Visibility                 string                 `json:"visibility"`
	Priority                   int                    `json:"priority"`
	SupportVerbosity           bool                   `json:"support_verbosity"`
	SupportsParallelToolCalls  bool                   `json:"supports_parallel_tool_calls"`
	TruncationPolicy           *codexTruncationPolicy `json:"truncation_policy,omitempty"`
	ExperimentalSupportedTools []string               `json:"experimental_supported_tools"`
	BaseInstructions           string                 `json:"base_instructions"`
	InputModalities            []string               `json:"input_modalities"`
	ContextWindow              *int                   `json:"context_window,omitempty"`
	MaxContextWindow           *int                   `json:"max_context_window,omitempty"`
	AutoCompactTokenLimit      *int                   `json:"auto_compact_token_limit,omitempty"`
	SupportedReasoningLevels   []codexReasoningLevel  `json:"supported_reasoning_levels"`
}

// codexEffortDescriptions maps the canonical effort vocabulary to the
// description strings real Codex catalogs carry.
var codexEffortDescriptions = map[string]string{
	"minimal": "Fast",
	"low":     "Fast",
	"medium":  "Balanced",
	"high":    "Thorough",
}

func (h *CatalogHandler) codexDocument() codexCatalogDocument {
	listed := make([]int, 0, len(h.models))
	for i, model := range h.models {
		if validCatalogModel(model) {
			listed = append(listed, i)
		}
	}
	priorities := h.codexPriorities(listed)
	entries := make([]codexCatalogEntry, 0, len(listed))
	for _, i := range listed {
		entries = append(entries, h.codexEntry(h.models[i], priorities[i]))
	}
	return codexCatalogDocument{Models: entries}
}

func (h *CatalogHandler) codexEntry(model CatalogModel, priority int) codexCatalogEntry {
	entry := codexCatalogEntry{
		Slug:                       model.Surrogate,
		DisplayName:                h.displayName(model.Surrogate),
		SupportedInAPI:             true,
		ShellType:                  catalogCodexShellCommand,
		Visibility:                 catalogCodexVisibilityList,
		Priority:                   priority,
		SupportVerbosity:           true,
		SupportsParallelToolCalls:  h.parallelToolCalls,
		ExperimentalSupportedTools: []string{},
		BaseInstructions:           "",
		InputModalities:            catalogModalities(model.Modalities),
		SupportedReasoningLevels:   catalogReasoningLevels(model.Efforts),
	}
	if model.Deprecated {
		entry.Visibility = catalogCodexVisibilityHide
	}
	if model.Context != nil {
		limit := *model.Context
		entry.TruncationPolicy = &codexTruncationPolicy{Mode: catalogTruncationMode, Limit: limit}
		entry.ContextWindow = &limit
		entry.MaxContextWindow = &limit
		compact := limit * 95 / 100
		entry.AutoCompactTokenLimit = &compact
	}
	return entry
}

func catalogReasoningLevels(efforts []string) []codexReasoningLevel {
	levels := make([]codexReasoningLevel, 0, len(efforts))
	for _, effort := range efforts {
		levels = append(levels, codexReasoningLevel{
			Effort:      effort,
			Description: codexEffortDescriptions[effort],
		})
	}
	return levels
}

// catalogModalities returns the declared modalities, or the honest default the
// ecosystem schema applies (text) when none are declared.
func catalogModalities(modalities []string) []string {
	if len(modalities) == 0 {
		return []string{"text"}
	}
	return slices.Clone(modalities)
}

func (h *CatalogHandler) displayName(surrogate string) string {
	if h.provider == "" {
		return surrogate
	}
	return h.provider + " " + surrogate
}

type anthropicCatalogDocument struct {
	Data    []anthropicCatalogEntry `json:"data"`
	FirstID *string                 `json:"first_id"`
	LastID  *string                 `json:"last_id"`
	HasMore bool                    `json:"has_more"`
}

type anthropicSupport struct {
	Supported bool `json:"supported"`
}

type anthropicEffort struct {
	Supported bool              `json:"supported"`
	Low       *anthropicSupport `json:"low,omitempty"`
	Medium    *anthropicSupport `json:"medium,omitempty"`
	High      *anthropicSupport `json:"high,omitempty"`
	Max       *anthropicSupport `json:"max,omitempty"`
	XHigh     *anthropicSupport `json:"xhigh,omitempty"`
}

type anthropicThinking struct {
	Supported bool `json:"supported"`
	Types     struct {
		Adaptive anthropicSupport `json:"adaptive"`
		Enabled  anthropicSupport `json:"enabled"`
	} `json:"types"`
}

type anthropicCapabilities struct {
	Batch             anthropicSupport  `json:"batch"`
	Citations         anthropicSupport  `json:"citations"`
	CodeExecution     anthropicSupport  `json:"code_execution"`
	ContextManagement anthropicSupport  `json:"context_management"`
	Effort            anthropicEffort   `json:"effort"`
	ImageInput        anthropicSupport  `json:"image_input"`
	PDFInput          anthropicSupport  `json:"pdf_input"`
	StructuredOutputs anthropicSupport  `json:"structured_outputs"`
	Thinking          anthropicThinking `json:"thinking"`
}

type anthropicCatalogEntry struct {
	Type           string                 `json:"type"`
	ID             string                 `json:"id"`
	DisplayName    string                 `json:"display_name"`
	CreatedAt      string                 `json:"created_at"`
	MaxInputTokens *int                   `json:"max_input_tokens"`
	MaxTokens      *int                   `json:"max_tokens"`
	Capabilities   *anthropicCapabilities `json:"capabilities"`
}

// anthropicEffortLeaves are the effort names the Anthropic capabilities object
// models as leaves. The canonical vocabulary covers low/medium/high; max and
// xhigh are emitted false so a client that reads them sees an honest negative
// rather than a missing key.
var anthropicEffortLeaves = []string{"low", "medium", "high", "max", "xhigh"}

func (h *CatalogHandler) anthropicDocument(query map[string][]string) (anthropicCatalogDocument, error) {
	start, end := 0, len(h.models)
	if afterID, ok := queryValue(query, "after_id"); ok {
		index := h.modelIndex(afterID)
		if index < 0 {
			return anthropicCatalogDocument{}, fmt.Errorf("unknown after_id %q (%s)", afterID, h.servableSuffix())
		}
		start = index + 1
	}
	if beforeID, ok := queryValue(query, "before_id"); ok {
		index := h.modelIndex(beforeID)
		if index < 0 {
			return anthropicCatalogDocument{}, fmt.Errorf("unknown before_id %q (%s)", beforeID, h.servableSuffix())
		}
		if index < end {
			end = index
		}
	}
	if end < start {
		end = start
	}
	models := h.models[start:end]

	limit := 0
	if raw, ok := queryValue(query, "limit"); ok {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			return anthropicCatalogDocument{}, fmt.Errorf("invalid limit %q (want an integer 1-1000)", raw)
		}
		limit = parsed
	}

	valid := make([]CatalogModel, 0, len(models))
	for _, model := range models {
		if !validCatalogModel(model) {
			continue
		}
		valid = append(valid, model)
	}
	models = valid

	hasMore := false
	if limit > 0 && len(models) > limit {
		models = models[:limit]
		hasMore = true
	}

	document := anthropicCatalogDocument{
		Data:    make([]anthropicCatalogEntry, 0, len(models)),
		HasMore: hasMore,
	}
	for _, model := range models {
		document.Data = append(document.Data, h.anthropicEntry(model))
	}
	if len(models) > 0 {
		first := models[0].Surrogate
		last := models[len(models)-1].Surrogate
		document.FirstID = &first
		document.LastID = &last
	}
	return document, nil
}

func (h *CatalogHandler) modelIndex(surrogate string) int {
	for i, model := range h.models {
		if model.Surrogate == surrogate {
			return i
		}
	}
	return -1
}

func (h *CatalogHandler) servableSuffix() string {
	names := make([]string, 0, len(h.models))
	for _, model := range h.models {
		names = append(names, model.Surrogate)
	}
	return "servable on this mount: " + strings.Join(names, ", ")
}

func (h *CatalogHandler) anthropicEntry(model CatalogModel) anthropicCatalogEntry {
	entry := anthropicCatalogEntry{
		Type:           anthropicModelType,
		ID:             model.Surrogate,
		DisplayName:    h.displayName(model.Surrogate),
		CreatedAt:      anthropicCreatedAtEpoch,
		MaxInputTokens: model.Context,
		MaxTokens:      model.MaxOutput,
	}
	if catalogHasFacts(model) {
		capabilities := anthropicCapabilities{
			Batch:             anthropicSupport{},
			Citations:         anthropicSupport{},
			CodeExecution:     anthropicSupport{},
			ContextManagement: anthropicSupport{},
			Effort:            anthropicEffort{Supported: len(model.Efforts) > 0},
			ImageInput:        anthropicSupport{Supported: slices.Contains(model.Modalities, "image")},
			PDFInput:          anthropicSupport{},
			StructuredOutputs: anthropicSupport{Supported: h.structuredOutputs},
		}
		for _, leaf := range anthropicEffortLeaves {
			leafCopy := anthropicSupport{Supported: slices.Contains(model.Efforts, leaf)}
			switch leaf {
			case "low":
				capabilities.Effort.Low = &leafCopy
			case "medium":
				capabilities.Effort.Medium = &leafCopy
			case "high":
				capabilities.Effort.High = &leafCopy
			case "max":
				capabilities.Effort.Max = &leafCopy
			case "xhigh":
				capabilities.Effort.XHigh = &leafCopy
			}
		}
		capabilities.Thinking = anthropicThinking{Supported: len(model.Efforts) > 0}
		capabilities.Thinking.Types.Enabled.Supported = len(model.Efforts) > 0
		entry.Capabilities = &capabilities
	}
	return entry
}

// catalogHasFacts reports whether the model carries any presentation fact; a
// fact-free model serves a minimal entry (capabilities null, absent optional
// fields) rather than a fabricated one.
func catalogHasFacts(model CatalogModel) bool {
	return model.Context != nil || model.MaxOutput != nil ||
		len(model.Efforts) > 0 || len(model.Modalities) > 0
}

type openAICatalogDocument struct {
	Object string               `json:"object"`
	Data   []openAICatalogEntry `json:"data"`
}

type openAICatalogEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (h *CatalogHandler) openAIDocument() openAICatalogDocument {
	entries := make([]openAICatalogEntry, 0, len(h.models))
	for _, model := range h.models {
		if !validCatalogModel(model) {
			continue
		}
		entries = append(entries, openAICatalogEntry{
			ID:      model.Surrogate,
			Object:  openAIModelObject,
			Created: 0,
			OwnedBy: h.provider,
		})
	}
	return openAICatalogDocument{Object: openAIListObject, Data: entries}
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
		if _, ok := catalogEffortSet[effort]; !ok {
			return false
		}
	}
	for _, modality := range model.Modalities {
		if _, ok := catalogModalitySet[modality]; !ok {
			return false
		}
	}
	return true
}

// catalogEffortSet and catalogModalitySet mirror the canonical fact
// vocabularies for the render-time re-check.
var (
	catalogEffortSet = map[string]struct{}{
		"minimal": {},
		"low":     {},
		"medium":  {},
		"high":    {},
	}
	catalogModalitySet = map[string]struct{}{
		"text":  {},
		"image": {},
		"audio": {},
	}
)

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
