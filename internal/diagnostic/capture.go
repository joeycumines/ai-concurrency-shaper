// Package diagnostic retains bounded, request-scoped evidence. It never writes
// to disk, parses native payloads, or changes an admission decision.
package diagnostic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const MaxCaptureBytes = 1 << 20
const MaxDecisions = 128
const MaxFailures = 8
const MaxStages = 32

type LegName string

const (
	ClientRequest      LegName = "client_request"
	UpstreamRequest    LegName = "upstream_request"
	UpstreamResponse   LegName = "upstream_response"
	DownstreamResponse LegName = "downstream_response"
)

type Leg struct {
	Name          LegName `json:"name"`
	Observed      bool    `json:"observed"`
	ObservedBytes int64   `json:"observed_bytes"`
	BytesKnown    bool    `json:"bytes_known"`
	CapturedBytes int     `json:"captured_bytes"`
	Complete      bool    `json:"complete"`
	Truncated     bool    `json:"truncated"`
	ContentType   string  `json:"content_type,omitempty"`
	Status        int     `json:"status,omitempty"`
	Body          []byte  `json:"body,omitempty"`
}
type Decision struct {
	Stage   string `json:"stage"`
	Feature string `json:"feature"`
	Path    string `json:"path"`
	Note    bool   `json:"note"`
	Detail  string `json:"detail,omitempty"`
}
type Failure struct {
	Stage       string `json:"stage"`
	Kind        string `json:"kind"`
	Path        string `json:"path,omitempty"`
	Feature     string `json:"feature,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Detail      string `json:"detail,omitempty"`
}
type Stage struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}
type Contract struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}
type Snapshot struct {
	Version          int               `json:"version"`
	RequestID        string            `json:"request_id"`
	Started          time.Time         `json:"started"`
	ClientProtocol   string            `json:"client_protocol,omitempty"`
	UpstreamProtocol string            `json:"upstream_protocol,omitempty"`
	Model            string            `json:"model,omitempty"`
	ClientModel      string            `json:"client_model,omitempty"`
	Stream           bool              `json:"stream"`
	Capabilities     []string          `json:"capabilities,omitempty"`
	AllowedLosses    []string          `json:"allowed_losses,omitempty"`
	ClientVersions   map[string]string `json:"client_versions,omitempty"`
	Contracts        []Contract        `json:"contracts,omitempty"`
	Stages           []Stage           `json:"stages,omitempty"`
	Decisions        []Decision        `json:"decisions,omitempty"`
	Failures         []Failure         `json:"failures,omitempty"`
	Legs             [4]Leg            `json:"legs"`
	DroppedStages    uint64            `json:"dropped_stages"`
	DroppedDecisions uint64            `json:"dropped_decisions"`
	DroppedFailures  uint64            `json:"dropped_failures"`
	CaptureBudget    int               `json:"capture_budget"`
	Sealed           bool              `json:"sealed"`
}

// Capture serializes updates and returns independent snapshots. Each of four
// legs receives a fixed share of the total budget, so an oversized input cannot
// consume the evidence budget for the failed response.
type Capture struct {
	mu     sync.Mutex
	s      Snapshot
	limits [4]int
}

type contextKey struct{}

func WithContext(ctx context.Context, c *Capture) context.Context {
	return context.WithValue(ctx, contextKey{}, c)
}
func FromContext(ctx context.Context) *Capture {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(contextKey{}).(*Capture)
	return c
}

func New(requestID string, budget int) *Capture {
	budget = max(0, min(budget, MaxCaptureBytes))
	c := &Capture{s: Snapshot{Version: 1, RequestID: bound(requestID, 128), Started: time.Now(), CaptureBudget: budget}}
	for i, n := range []LegName{ClientRequest, UpstreamRequest, UpstreamResponse, DownstreamResponse} {
		c.s.Legs[i].Name = n
		c.limits[i] = budget / 4
		if i < budget%4 {
			c.limits[i]++
		}
	}
	return c
}

func index(n LegName) int {
	switch n {
	case ClientRequest:
		return 0
	case UpstreamRequest:
		return 1
	case UpstreamResponse:
		return 2
	case DownstreamResponse:
		return 3
	}
	return -1
}
func bound(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) > n {
		s = s[:n]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return strings.Clone(s)
}

func (c *Capture) Protocols(client, upstream string, capabilities, allowed []string, contracts []Contract) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.Sealed {
		return
	}
	c.s.ClientProtocol = bound(client, 64)
	c.s.UpstreamProtocol = bound(upstream, 64)
	c.s.Capabilities = boundedStrings(capabilities, 128)
	c.s.AllowedLosses = boundedStrings(allowed, 128)
	c.s.Contracts = append([]Contract(nil), contracts[:min(len(contracts), 16)]...)
}
func boundedStrings(v []string, n int) []string {
	out := make([]string, min(len(v), n))
	for i := range out {
		out[i] = bound(v[i], 256)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
func (c *Capture) Models(client, upstream string, stream bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.Sealed {
		return
	}
	c.s.ClientModel = bound(client, 512)
	c.s.Model = bound(upstream, 512)
	c.s.Stream = stream
}

// Versions retains only explicit protocol/SDK version headers, never arbitrary
// HTTP headers or User-Agent. Values stay local and are removed from safe export.
func (c *Capture) Versions(h http.Header) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.Sealed {
		return
	}
	c.s.ClientVersions = make(map[string]string)
	for _, k := range []string{"Anthropic-Version", "Anthropic-Beta", "X-Stainless-Lang", "X-Stainless-Package-Version", "X-Stainless-Runtime", "X-Stainless-Runtime-Version"} {
		if v := h.Get(k); v != "" {
			c.s.ClientVersions[k] = bound(v, 256)
		}
	}
}
func (c *Capture) Stage(name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.Sealed {
		return
	}
	if len(c.s.Stages) >= MaxStages {
		c.s.DroppedStages++
		return
	}
	c.s.Stages = append(c.s.Stages, Stage{bound(name, 64), time.Now()})
}
func (c *Capture) Decision(d Decision) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.Sealed {
		return
	}
	d.Stage = bound(d.Stage, 64)
	d.Feature = bound(d.Feature, 128)
	d.Path = bound(d.Path, 512)
	d.Detail = bound(d.Detail, 1024)
	for _, v := range c.s.Decisions {
		if v.Stage == d.Stage && v.Feature == d.Feature && v.Path == d.Path && v.Note == d.Note {
			return
		}
	}
	if len(c.s.Decisions) >= MaxDecisions {
		c.s.DroppedDecisions++
		return
	}
	c.s.Decisions = append(c.s.Decisions, d)
}

// OmitDecisions propagates the codec report's own truncation. Their loss/note
// kinds are unknown, so omissions must not be fabricated as approved losses.
func (c *Capture) OmitDecisions(n int) {
	if c == nil || n <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.Sealed {
		return
	}
	v := uint64(n)
	if math.MaxUint64-c.s.DroppedDecisions < v {
		c.s.DroppedDecisions = math.MaxUint64
	} else {
		c.s.DroppedDecisions += v
	}
}

// Failure fingerprints use taxonomy and protocol direction only. Raw error
// messages, paths, model names and payload values never affect the fingerprint.
// The local snapshot preserves the path separately for debugging.
func (c *Capture) Failure(stage, kind, path, feature, detail string) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.Sealed {
		return ""
	}
	stage = bound(stage, 64)
	kind = bound(kind, 64)
	feature = bound(feature, 128)
	h := sha256.Sum256([]byte(strings.Join([]string{"diagnostic-v1", c.s.ClientProtocol, c.s.UpstreamProtocol, stage, kind, feature}, "\x00")))
	fp := hex.EncodeToString(h[:12])
	if len(c.s.Failures) >= MaxFailures {
		c.s.DroppedFailures++
		return fp
	}
	c.s.Failures = append(c.s.Failures, Failure{stage, kind, bound(path, 512), feature, fp, bound(detail, 2048)})
	return fp
}
func (c *Capture) Metadata(n LegName, contentType string, status int) {
	if c == nil {
		return
	}
	i := index(n)
	if i < 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.Sealed {
		return
	}
	c.s.Legs[i].Observed = true
	c.s.Legs[i].ContentType = bound(contentType, 256)
	c.s.Legs[i].Status = status
}
func (c *Capture) Append(n LegName, p []byte) {
	if c == nil {
		return
	}
	i := index(n)
	if i < 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.Sealed {
		return
	}
	l := &c.s.Legs[i]
	l.Observed = true
	l.BytesKnown = true
	l.ObservedBytes += int64(len(p))
	room := max(0, c.limits[i]-len(l.Body))
	l.Body = append(l.Body, p[:min(room, len(p))]...)
	l.CapturedBytes = len(l.Body)
	l.Truncated = l.ObservedBytes > int64(l.CapturedBytes)
}
func (c *Capture) Complete(n LegName, complete bool) {
	if c == nil {
		return
	}
	i := index(n)
	if i < 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.Sealed {
		return
	}
	c.s.Legs[i].Observed = true
	c.s.Legs[i].Complete = complete
	if complete {
		c.s.Legs[i].BytesKnown = true
	}
}

// ObserveBytes counts a selected native response without copying its opaque
// payload. Native bodies remain in the existing journal capture policy.
func (c *Capture) ObserveBytes(n LegName, count int) {
	if c == nil || count < 0 {
		return
	}
	i := index(n)
	if i < 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.Sealed {
		return
	}
	l := &c.s.Legs[i]
	l.Observed = true
	l.BytesKnown = true
	l.ObservedBytes += int64(count)
	l.Truncated = l.ObservedBytes > int64(l.CapturedBytes)
}
func (c *Capture) SetBody(n LegName, p []byte, contentType string) {
	if c == nil {
		return
	}
	c.Metadata(n, contentType, 0)
	c.Append(n, p)
	c.Complete(n, true)
}
func (c *Capture) Seal() *Snapshot {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.s.Sealed = true
	c.mu.Unlock()
	return c.Snapshot()
}
func (c *Capture) Snapshot() *Snapshot {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Clone(&c.s)
}

// FirstFailureFingerprint reads the safe grouping key without copying bodies.
func (c *Capture) FirstFailureFingerprint() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.s.Failures) == 0 {
		return ""
	}
	return c.s.Failures[0].Fingerprint
}
func Clone(src *Snapshot) *Snapshot {
	return clone(src, true)
}

// CloneMetadata copies evidence without allocating or exposing retained body
// prefixes. Other private fields are still present; an export policy must
// separately redact model names, paths and free text as appropriate.
func CloneMetadata(src *Snapshot) *Snapshot {
	return clone(src, false)
}

func clone(src *Snapshot, bodies bool) *Snapshot {
	if src == nil {
		return nil
	}
	s := *src
	s.Capabilities = append([]string(nil), src.Capabilities...)
	s.AllowedLosses = append([]string(nil), src.AllowedLosses...)
	s.Contracts = append([]Contract(nil), src.Contracts...)
	s.Stages = append([]Stage(nil), src.Stages...)
	s.Decisions = append([]Decision(nil), src.Decisions...)
	s.Failures = append([]Failure(nil), src.Failures...)
	if src.ClientVersions != nil {
		s.ClientVersions = make(map[string]string, len(src.ClientVersions))
		maps.Copy(s.ClientVersions, src.ClientVersions)
	}
	for i := range s.Legs {
		s.Legs[i].Body = nil
		if bodies {
			s.Legs[i].Body = append([]byte(nil), src.Legs[i].Body...)
		}
	}
	return &s
}

type bodyReader struct {
	io.ReadCloser
	c   *Capture
	leg LegName
}

func (r *bodyReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.c.Append(r.leg, p[:n])
	}
	if err == io.EOF {
		r.c.Complete(r.leg, true)
	}
	return n, err
}
func (c *Capture) WrapBody(n LegName, b io.ReadCloser, ct string, status int) io.ReadCloser {
	if c == nil || b == nil {
		return b
	}
	c.Metadata(n, ct, status)
	return &bodyReader{b, c, n}
}
