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
	"slices"
	"strconv"
	"strings"
	"time"
)

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
	Description                string                 `json:"description,omitempty"`
	Tags                       []string               `json:"tags,omitempty"`
	CostInput                  *float64               `json:"cost_input,omitempty"`
	CostOutput                 *float64               `json:"cost_output,omitempty"`
	Created                    int64                  `json:"created,omitempty"`
}

// codexEffortDescriptions maps the canonical effort vocabulary to the
// description strings real Codex catalogs carry. A level without an entry
// serves an empty description: a truthful blank beats a fabricated one.
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
		DisplayName:                h.displayName(model),
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
		Description:                model.Description,
		Tags:                       slices.Clone(model.Tags),
		CostInput:                  model.CostInput,
		CostOutput:                 model.CostOutput,
		Created:                    model.Created,
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

func (h *CatalogHandler) displayName(model CatalogModel) string {
	provider := model.Provider
	if provider == "" {
		provider = h.provider
	}
	if provider == "" {
		return model.Surrogate
	}
	return provider + " " + model.Surrogate
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

type anthropicPricing struct {
	InputCost  *float64 `json:"input_cost,omitempty"`
	OutputCost *float64 `json:"output_cost,omitempty"`
}

type anthropicCatalogEntry struct {
	Type           string                 `json:"type"`
	ID             string                 `json:"id"`
	DisplayName    string                 `json:"display_name"`
	CreatedAt      string                 `json:"created_at"`
	Description    string                 `json:"description,omitempty"`
	Tags           []string               `json:"tags,omitempty"`
	Pricing        *anthropicPricing      `json:"pricing,omitempty"`
	MaxInputTokens *int                   `json:"max_input_tokens"`
	MaxTokens      *int                   `json:"max_tokens"`
	Capabilities   *anthropicCapabilities `json:"capabilities"`
}

// anthropicEffortLeaves are the effort names the Anthropic capabilities
// object models as leaves: every canonical effort except minimal, which the
// Anthropic contract has no slot for. A leaf is true only when the model's
// table entry advertises it; otherwise it is the honest negative.
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
	createdAt := anthropicCreatedAtEpoch
	if model.Created > 0 {
		createdAt = time.Unix(model.Created, 0).UTC().Format(time.RFC3339)
	}
	entry := anthropicCatalogEntry{
		Type:           anthropicModelType,
		ID:             model.Surrogate,
		DisplayName:    h.displayName(model),
		CreatedAt:      createdAt,
		Description:    model.Description,
		Tags:           slices.Clone(model.Tags),
		MaxInputTokens: model.Context,
		MaxTokens:      model.MaxOutput,
	}
	if model.CostInput != nil || model.CostOutput != nil {
		entry.Pricing = &anthropicPricing{
			InputCost:  model.CostInput,
			OutputCost: model.CostOutput,
		}
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
		len(model.Efforts) > 0 || len(model.Modalities) > 0 ||
		model.CostInput != nil || model.CostOutput != nil ||
		len(model.Tags) > 0 || model.Description != "" || model.Created > 0
}

type openAICatalogDocument struct {
	Object string               `json:"object"`
	Data   []openAICatalogEntry `json:"data"`
}

type openAIPricing struct {
	Input  *float64 `json:"input,omitempty"`
	Output *float64 `json:"output,omitempty"`
}

type openAICatalogEntry struct {
	ID              string         `json:"id"`
	Object          string         `json:"object"`
	Created         int64          `json:"created"`
	OwnedBy         string         `json:"owned_by"`
	Description     string         `json:"description,omitempty"`
	Tags            []string       `json:"tags,omitempty"`
	Pricing         *openAIPricing `json:"pricing,omitempty"`
	ContextWindow   *int           `json:"context_window,omitempty"`
	MaxOutputTokens *int           `json:"max_output_tokens,omitempty"`
}

func (h *CatalogHandler) openAIDocument() openAICatalogDocument {
	entries := make([]openAICatalogEntry, 0, len(h.models))
	for _, model := range h.models {
		if !validCatalogModel(model) {
			continue
		}
		entries = append(entries, h.openAIEntry(model))
	}
	return openAICatalogDocument{Object: openAIListObject, Data: entries}
}

func (h *CatalogHandler) openAIEntry(model CatalogModel) openAICatalogEntry {
	created := model.Created
	ownedBy := model.Provider
	if ownedBy == "" {
		ownedBy = h.provider
	}
	entry := openAICatalogEntry{
		ID:              model.Surrogate,
		Object:          openAIModelObject,
		Created:         created,
		OwnedBy:         ownedBy,
		Description:     model.Description,
		Tags:            slices.Clone(model.Tags),
		ContextWindow:   model.Context,
		MaxOutputTokens: model.MaxOutput,
	}
	if model.CostInput != nil || model.CostOutput != nil {
		entry.Pricing = &openAIPricing{
			Input:  model.CostInput,
			Output: model.CostOutput,
		}
	}
	return entry
}
