package transcode

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/diagnostic"
	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

//go:embed contracts.lock.json
var diagnosticContractManifest []byte

var diagnosticContracts = loadDiagnosticContracts()

func loadDiagnosticContracts() []diagnostic.Contract {
	var m map[string]struct {
		Version string `json:"version"`
		SHA256  string `json:"snapshot_sha256"`
	}
	if err := json.Unmarshal(diagnosticContractManifest, &m); err != nil {
		panic(err)
	}
	out := make([]diagnostic.Contract, 0, len(m)+1)
	for name, v := range m {
		out = append(out, diagnostic.Contract{Name: name, Version: v.Version, SHA256: v.SHA256})
	}
	slices.SortFunc(out, func(a, b diagnostic.Contract) int { return strings.Compare(a.Name, b.Name) })
	h := sha256.Sum256(diagnosticContractManifest)
	return append(out, diagnostic.Contract{Name: "contracts.lock.json", SHA256: hex.EncodeToString(h[:])})
}

func (h *TranscodeHandler) startDiagnostic(r *http.Request) {
	c := diagnostic.FromContext(r.Context())
	if c == nil {
		return
	}
	var capabilities, allowed []string
	v := reflect.ValueOf(h.cfg.Mapping.ChatCapabilities)
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Kind() == reflect.Bool && v.Field(i).Bool() {
			capabilities = append(capabilities, t.Field(i).Name)
		}
	}
	for k := range h.cfg.Mapping.LossPolicy.Allowed {
		allowed = append(allowed, string(k))
	}
	c.Protocols(string(h.cfg.Mapping.ClientProtocol), string(h.cfg.Mapping.UpstreamProtocol), capabilities, allowed, diagnosticContracts)
	c.Versions(r.Header)
	c.Stage("request_decode")
}

func (h *TranscodeHandler) diagnosticFailure(r *http.Request, stage string, err error) {
	if err == nil || r == nil {
		return
	}
	c := diagnostic.FromContext(r.Context())
	if c == nil {
		return
	}
	kind, path, feature := "conversion", "", ""
	switch {
	case errors.Is(err, errStreamTruncated):
		kind = "truncated_stream"
	case errors.Is(err, errRequestBodyTooLarge), errors.Is(err, errDecodedRequestTooLarge), errors.Is(err, errEchoTooLarge):
		kind = "body_limit"
	default:
		if e, ok := errors.AsType[*wire.DecodeError](err); ok {
			kind = e.Kind.String()
			path = e.Path
		} else if e, ok := errors.AsType[*UnsupportedFeatureError](err); ok {
			kind = "unsupported_feature"
			path = e.Path
			if LossKeyDescription(Feature(e.Feature)) != "" {
				feature = e.Feature
			}
		} else if e, ok := errors.AsType[*wire.UnsupportedTypeError](err); ok {
			kind = "unsupported_union"
			path = e.Path
		} else if e, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			kind = "field_type"
			path = e.Field
		} else if _, ok := errors.AsType[*UpstreamWireError](err); ok {
			kind = "upstream_wire"
		} else if _, ok := errors.AsType[*UpstreamSemanticFailureError](err); ok {
			kind = "upstream_semantic"
		} else if _, ok := errors.AsType[*UnrepresentableError](err); ok {
			kind = "unrepresentable"
		}
	}
	// encoding/json reports only the unknown leaf, never its nesting. A
	// descendant path states that uncertainty instead of inventing a location.
	if path == "" {
		if _, after, ok := strings.Cut(err.Error(), "json: unknown field "); ok {
			var leaf string
			if json.Unmarshal([]byte(after), &leaf) == nil {
				path = "$.." + leaf
				kind = "unknown_field"
			}
		}
	}
	fp := c.Failure(stage, kind, path, feature, err.Error())
	if o := observation.FromContext(r.Context()); o != nil {
		o.SetFingerprint(fp)
	}
}

func (h *TranscodeHandler) diagnosticReport(r *http.Request, stage string, report ConversionReport) {
	c := diagnostic.FromContext(r.Context())
	c.OmitDecisions(report.Dropped)
	var losses, notes int
	for _, v := range report.Losses {
		isNote := v.Kind == NoteRecord
		if isNote {
			notes++
		} else {
			losses++
		}
		c.Decision(diagnostic.Decision{Stage: stage, Feature: string(v.Feature), Path: v.Path, Detail: v.Detail, Note: isNote})
	}
	if o := observation.FromContext(r.Context()); o != nil {
		o.Conversion(losses, notes, report.Dropped)
	}
}

func reportObservedUsage(r *http.Request, u CanonicalUsage, source string) {
	if u.Unknown() {
		return
	}
	if o := observation.FromContext(r.Context()); o != nil {
		o.ReportUsage(observation.Usage{
			Input: u.InputTokens, Output: u.OutputTokens, Total: u.TotalTokens,
			InputKnown: u.InputKnown, OutputKnown: u.OutputKnown, TotalKnown: u.TotalKnown, Source: source,
		})
	}
}
