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
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// The global model table is one flat namespace shared by every provider:
//
//	entry = surrogate "@" provider "=" wire [ ";" facts ]
//	facts = fact *( ";" fact )
//	fact  = context=pos-int | max_output=pos-int | efforts=effort+... |
//	        modalities=modality+... | default | deprecated
//
// The first '=' splits the surrogate@provider left side from the wire id and
// facts; the first '@' splits the surrogate from the provider; the first ';'
// splits the wire id from the facts; '+' joins list values. A wire id is opaque
// to the shaper and may itself contain '/', ':', '@', '=', '+', '.', '_', '-'.
// Surrogates and providers are restricted to letters, digits, '.', '_' and '-'
// so a catalog-emitted identifier can never collide with a separator.
//
// Facts are presentation-only: they are validated and frozen for the served
// catalog, and no resolution or rendering path reads them.

const (
	modelTableMaxSurrogateLen = 128
	modelTableMaxProviderLen  = 128
	modelTableMaxWireLen      = 256
	modelTableMaxPositiveInt  = 999999999
	// modelTableSummaryMaxListed bounds the startup summary listing so a large
	// catalog cannot flood the log.
	modelTableSummaryMaxListed = 50
)

const (
	modelTableFactVocabulary = "context, max_output, efforts, modalities, default, deprecated"
)

// modelTableEntry is one parsed -model-table entry. Facts stay inert here; only
// the surrogate, provider, and wire id participate in model resolution.
type modelTableEntry struct {
	Surrogate  string
	Provider   string
	Wire       string
	Context    *int
	MaxOutput  *int
	Efforts    []string
	Modalities []string
	Default    bool
	Deprecated bool
	Raw        string
}

// parseModelTableEntry parses and validates one raw -model-table value. Every
// failure names the raw entry verbatim so the operator sees the exact bytes.
func parseModelTableEntry(raw string) (modelTableEntry, error) {
	left, right, ok := strings.Cut(raw, "=")
	if !ok || left == "" {
		return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: want surrogate@provider=wire[;facts]", raw)
	}
	surrogate, provider, ok := strings.Cut(left, "@")
	if !ok {
		return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: want surrogate@provider=wire[;facts]", raw)
	}
	wire, facts, hasFacts := strings.Cut(right, ";")
	if surrogate == "" {
		return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: empty surrogate", raw)
	}
	if provider == "" {
		return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: empty provider", raw)
	}
	if wire == "" {
		return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: empty wire id", raw)
	}
	if !validModelTableIdent(surrogate, modelTableMaxSurrogateLen) {
		return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: invalid surrogate %q: want 1-128 chars of [A-Za-z0-9._-]", raw, surrogate)
	}
	if !validModelTableIdent(provider, modelTableMaxProviderLen) {
		return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: invalid provider %q: want 1-128 chars of [A-Za-z0-9._-]", raw, provider)
	}
	if !validModelTableWire(wire) {
		return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: invalid wire id %q: want 1-256 chars of [A-Za-z0-9._\\-/:@=+]", raw, wire)
	}

	entry := modelTableEntry{Surrogate: surrogate, Provider: provider, Wire: wire, Raw: raw}
	if !hasFacts {
		return entry, nil
	}

	seen := make(map[string]struct{}, 4)
	for _, segment := range strings.Split(facts, ";") {
		if segment == "" || strings.ContainsAny(segment, " \t\n\r") || hasControlByte(segment) {
			return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: empty fact", raw)
		}
		key, value, hasValue := strings.Cut(segment, "=")
		if _, dup := seen[key]; dup {
			return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: duplicate fact %q", raw, key)
		}
		switch key {
		case "context", "max_output":
			n, ok := parseModelTablePositiveInt(value)
			if !hasValue || !ok {
				return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: invalid %s %q: want 1-999999999", raw, key, value)
			}
			if key == "context" {
				entry.Context = &n
			} else {
				entry.MaxOutput = &n
			}
		case "efforts", "modalities":
			values, err := parseModelTableList(raw, key, value)
			if err != nil {
				return modelTableEntry{}, err
			}
			if key == "efforts" {
				entry.Efforts = values
			} else {
				entry.Modalities = values
			}
		case "default":
			if hasValue {
				return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: fact %q takes no value", raw, key)
			}
			if entry.Deprecated {
				return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: default and deprecated cannot combine", raw)
			}
			entry.Default = true
		case "deprecated":
			if hasValue {
				return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: fact %q takes no value", raw, key)
			}
			if entry.Default {
				return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: default and deprecated cannot combine", raw)
			}
			entry.Deprecated = true
		default:
			return modelTableEntry{}, fmt.Errorf("invalid -model-table %q: unknown fact %q (want %s)", raw, key, modelTableFactVocabulary)
		}
		seen[key] = struct{}{}
	}
	return entry, nil
}

// modelTableEfforts is the closed reasoning-effort vocabulary the shaper can
// honestly advertise.
var modelTableEfforts = map[string]struct{}{
	"minimal": {},
	"low":     {},
	"medium":  {},
	"high":    {},
}

// modelTableModalities is the closed input-modality vocabulary: the three
// modalities the served client shapes can carry.
var modelTableModalities = map[string]struct{}{
	"text":  {},
	"image": {},
	"audio": {},
}

// parseModelTableList parses a '+' joined list fact value against its closed
// vocabulary.
func parseModelTableList(raw, key, value string) ([]string, error) {
	values := strings.Split(value, "+")
	for _, item := range values {
		switch key {
		case "efforts":
			if _, ok := modelTableEfforts[item]; !ok {
				return nil, fmt.Errorf("invalid -model-table %q: unknown effort %q (want minimal, low, medium, high)", raw, item)
			}
		case "modalities":
			if _, ok := modelTableModalities[item]; !ok {
				return nil, fmt.Errorf("invalid -model-table %q: unknown modality %q (want text, image, audio)", raw, item)
			}
		}
	}
	return values, nil
}

// parseModelTablePositiveInt accepts decimal 1..999999999 with no sign and no
// whitespace. The length bound keeps the conversion free of overflow.
func parseModelTablePositiveInt(value string) (int, bool) {
	if value == "" || len(value) > len("999999999") {
		return 0, false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// hasControlByte reports whether s contains an ASCII control byte.
func hasControlByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

func validModelTableIdent(s string, max int) bool {
	if s == "" || len(s) > max {
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

func validModelTableWire(s string) bool {
	if s == "" || len(s) > modelTableMaxWireLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == '/', r == ':', r == '@', r == '=', r == '+':
		default:
			return false
		}
	}
	return true
}

// resolveModelTable parses, validates, and freezes the global -model-table. It
// runs after validateMulti derived every effective provider name and before any
// provider resolves, so a bad table fails startup before any socket binds.
// Zero configured entries skip everything and leave provider resolution exactly
// as it was.
func (c *Config) resolveModelTable() error {
	if len(c.Server.ModelTable) == 0 {
		return nil
	}

	entries := make([]modelTableEntry, 0, len(c.Server.ModelTable))
	seenSurrogate := make(map[string]struct{}, len(c.Server.ModelTable))
	for _, raw := range c.Server.ModelTable {
		entry, err := parseModelTableEntry(raw)
		if err != nil {
			return err
		}
		if _, dup := seenSurrogate[entry.Surrogate]; dup {
			return fmt.Errorf("duplicate -model-table surrogate %q", entry.Surrogate)
		}
		seenSurrogate[entry.Surrogate] = struct{}{}
		entries = append(entries, entry)
	}

	for _, p := range c.Providers {
		if len(p.TranscodeModelMap) > 0 {
			return fmt.Errorf("-model-table and -transcode-model cannot be combined (provider %q): configure the global table only", effectiveName(p))
		}
	}

	known := make([]string, 0, len(c.Providers))
	for _, p := range c.Providers {
		known = append(known, effectiveName(p))
	}
	sort.Strings(known)
	knownSet := make(map[string]struct{}, len(known))
	for _, name := range known {
		knownSet[name] = struct{}{}
	}
	for _, entry := range entries {
		if _, ok := knownSet[entry.Provider]; !ok {
			return fmt.Errorf("unknown -model-table provider %q in %q: no provider named %q (known: %s)",
				entry.Provider, entry.Raw, entry.Provider, strings.Join(known, ", "))
		}
	}

	byProvider := make(map[string][]modelTableEntry, len(c.Providers))
	for _, name := range known {
		for _, entry := range entries {
			if entry.Provider == name {
				byProvider[name] = append(byProvider[name], cloneModelTableEntry(entry))
			}
		}
	}

	for _, p := range c.Providers {
		var defaults []string
		for _, entry := range byProvider[effectiveName(p)] {
			if entry.Default {
				defaults = append(defaults, entry.Surrogate)
			}
		}
		if len(defaults) > 1 {
			sort.Strings(defaults)
			return fmt.Errorf("duplicate -model-table default for provider %q: %q and %q", effectiveName(p), defaults[0], defaults[1])
		}
	}

	for _, p := range c.Providers {
		if !hasTranscodeRoutes(p) {
			continue
		}
		if len(byProvider[effectiveName(p)]) == 0 {
			return fmt.Errorf("provider %q has transcode routes but no -model-table entry names it", effectiveName(p))
		}
	}

	c.modelTable = make([]modelTableEntry, len(entries))
	for i, entry := range entries {
		c.modelTable[i] = cloneModelTableEntry(entry)
	}
	c.modelTableByProvider = byProvider
	return nil
}

// hasTranscodeRoutes reports whether the provider configures any transcode
// route, preset, or explicit route pattern.
func hasTranscodeRoutes(p *Provider) bool {
	return len(p.TranscodeRoutes) > 0 ||
		p.TranscodeResponsesChat ||
		p.TranscodeMessagesChat ||
		p.TranscodeMessagesResponses
}

func cloneModelTableEntry(entry modelTableEntry) modelTableEntry {
	cloned := entry
	if entry.Context != nil {
		value := *entry.Context
		cloned.Context = &value
	}
	if entry.MaxOutput != nil {
		value := *entry.MaxOutput
		cloned.MaxOutput = &value
	}
	cloned.Efforts = slices.Clone(entry.Efforts)
	cloned.Modalities = slices.Clone(entry.Modalities)
	return cloned
}

// ModelTableSummary renders the single startup summary of the configured global
// table, or "" when no table is configured. Entries list sorted by surrogate
// then provider, each as surrogate@provider->wire followed by its facts in
// canonical order when present; the listing is bounded.
func (c *Config) ModelTableSummary() string {
	if len(c.modelTable) == 0 {
		return ""
	}
	entries := slices.Clone(c.modelTable)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Surrogate != entries[j].Surrogate {
			return entries[i].Surrogate < entries[j].Surrogate
		}
		return entries[i].Provider < entries[j].Provider
	})

	providers := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		providers[entry.Provider] = struct{}{}
	}

	listed := min(len(entries), modelTableSummaryMaxListed)
	parts := make([]string, 0, listed)
	for _, entry := range entries[:listed] {
		parts = append(parts, entry.Surrogate+"@"+entry.Provider+"->"+entry.Wire+entry.factSuffix())
	}
	summary := fmt.Sprintf("model table: %d surrogates across %d providers: %s",
		len(entries), len(providers), strings.Join(parts, ", "))
	if len(entries) > listed {
		summary += fmt.Sprintf(", ... (+%d more)", len(entries)-listed)
	}
	return summary
}

// factSuffix renders the entry's presentation-only facts in canonical key
// order, or "" when the entry carries none.
func (e modelTableEntry) factSuffix() string {
	var parts []string
	if e.Context != nil {
		parts = append(parts, "context="+strconv.Itoa(*e.Context))
	}
	if e.Default {
		parts = append(parts, "default")
	}
	if e.Deprecated {
		parts = append(parts, "deprecated")
	}
	if len(e.Efforts) > 0 {
		parts = append(parts, "efforts="+strings.Join(e.Efforts, "+"))
	}
	if e.MaxOutput != nil {
		parts = append(parts, "max_output="+strconv.Itoa(*e.MaxOutput))
	}
	if len(e.Modalities) > 0 {
		parts = append(parts, "modalities="+strings.Join(e.Modalities, "+"))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, ",") + ")"
}

// modelMapFromTable projects one provider's frozen table subset into the
// existing ModelMap.Exact, so the existing resolution path serves requests
// unchanged. The projection is the only mapping set for a provider whose table
// has entries: identity fallback is disabled and every unlisted surrogate is a
// local client error.
func modelMapFromTable(subset []modelTableEntry) transcode.ModelMap {
	exact := make(map[string]transcode.ModelMapping, len(subset))
	for _, entry := range subset {
		exact[entry.Surrogate] = transcode.ModelMapping{
			ClientModel:         entry.Surrogate,
			UpstreamModel:       entry.Wire,
			ClientResponseModel: entry.Surrogate,
		}
	}
	return transcode.ModelMap{
		Exact:              exact,
		AllowIdentity:      false,
		RequireExplicitMap: true,
	}
}

// resolveModelCatalog builds this provider's frozen catalog snapshot from its
// model-table subset and resolved mappings. A provider with no entries gets no
// catalog (nil), so its discovery route stays a transparent passthrough.
func (p *Provider) resolveModelCatalog(modelTable []modelTableEntry) {
	if len(modelTable) == 0 {
		return
	}
	catalog := transcode.CatalogConfig{
		ProviderName: effectiveName(p),
		Models:       make([]transcode.CatalogModel, 0, len(modelTable)),
		// Defaults match the ecosystem observation for mounts with no chat
		// mapping; a chat mapping's resolved capabilities override below.
		ParallelToolCalls: true,
		StructuredOutputs: true,
	}
	for _, entry := range modelTable {
		model := transcode.CatalogModel{
			Surrogate:  entry.Surrogate,
			Efforts:    slices.Clone(entry.Efforts),
			Modalities: slices.Clone(entry.Modalities),
			Default:    entry.Default,
			Deprecated: entry.Deprecated,
		}
		if entry.Context != nil {
			value := *entry.Context
			model.Context = &value
		}
		if entry.MaxOutput != nil {
			value := *entry.MaxOutput
			model.MaxOutput = &value
		}
		catalog.Models = append(catalog.Models, model)
	}
	capabilitiesSet := false
	for i := range p.transcodeMappings {
		mapping := &p.transcodeMappings[i].Mapping
		switch mapping.ClientProtocol {
		case transcode.ClientResponses:
			catalog.ServesResponses = true
		case transcode.ClientMessages:
			catalog.ServesMessages = true
		}
		if !capabilitiesSet && mapping.UpstreamProtocol == transcode.UpstreamChatCompletions {
			catalog.ParallelToolCalls = mapping.ChatCapabilities.ParallelToolCalls
			catalog.StructuredOutputs = mapping.ChatCapabilities.StructuredOutputs
			capabilitiesSet = true
		}
	}
	p.modelCatalog = &catalog
}

// ModelCatalog returns a deep copy of this provider's frozen catalog snapshot,
// or false when no -model-table entry names it.
func (p *Provider) ModelCatalog() (transcode.CatalogConfig, bool) {
	if p.modelCatalog == nil {
		return transcode.CatalogConfig{}, false
	}
	out := *p.modelCatalog
	out.Models = make([]transcode.CatalogModel, len(p.modelCatalog.Models))
	for i, model := range p.modelCatalog.Models {
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
		out.Models[i] = model
	}
	return out, true
}
