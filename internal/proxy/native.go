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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

// NativeRoute declares one natively served client route: the request
// document's model identifier is rewritten per the model map and the
// document is otherwise forwarded verbatim through the transparent engine
// (limiter, retry, breaker, metrics, journal). It is not transcoding: no
// canonical IR is involved, so the transcoding direction rules do not apply
// and chat is a legal native protocol. A request whose model resolves
// without a known Via dialect falls through untouched; a known model on the
// wrong dialect fails closed locally.
type NativeRoute struct {
	RouteKey transcode.RouteKey
	Protocol transcode.NativeProtocol

	// Provider scopes derived conversation keys to the mount. Stamped
	// from the provider's effective name at resolve time.
	Provider string

	// ModelMap resolves surrogates to wire models and carries each
	// mapping's Via dialect. Entries without Via never serve natively.
	ModelMap transcode.ModelMap

	// BodyLimits bounds request/response bodies on this route. Zero
	// values fall back to the proxy defaults.
	BodyLimits transcode.BodyLimits
}

// Validate checks the route shape so a misconfigured native route fails at
// startup, never on the first request.
func (r NativeRoute) Validate() error {
	if r.RouteKey.Method != http.MethodPost {
		return fmt.Errorf(
			"native serving is supported only for POST create routes, got %s %s",
			r.RouteKey.Method,
			r.RouteKey.Path,
		)
	}
	switch r.Protocol {
	case transcode.NativeResponses, transcode.NativeMessages, transcode.NativeChat:
	default:
		return fmt.Errorf("unknown native protocol %q (want responses, messages, or chat)", r.Protocol)
	}
	if len(r.ModelMap.Exact) == 0 &&
		(!r.ModelMap.AllowIdentity || r.ModelMap.RequireExplicitMap) {
		return fmt.Errorf(
			"native route %s %s: model map: no model-resolution policy; enable identity fallback or configure explicit model mappings",
			r.RouteKey.Method,
			r.RouteKey.Path,
		)
	}
	return nil
}

// NativeOption configures natively served routes.
type NativeOption struct {
	routes []NativeRoute
}

// WithNativeRoutes returns an option that registers natively served routes.
func WithNativeRoutes(routes ...NativeRoute) *NativeOption {
	return &NativeOption{routes: routes}
}

func (o *NativeOption) applyProxyOption(cfg *proxyConfig) error {
	for i := range o.routes {
		if err := o.routes[i].Validate(); err != nil {
			return fmt.Errorf("proxy: invalid native route: %w", err)
		}
	}
	cfg.nativeRoutes = append(cfg.nativeRoutes, o.routes...)
	return nil
}

var _ Option = (*NativeOption)(nil)

// nativeAlias carries the model rewrite applied to a natively served
// request so the response path can restore the client-facing alias. It
// also carries the conversation key for first-party header emission
// downstream in the rewrite hook.
type nativeAlias struct {
	surrogate string
	wire      string
	// respCap bounds the response body inspected for the alias restore.
	respCap int64
	// convKey is the stable conversation key derived from the request
	// document (model plus first user text), or "" when derivation had
	// no user text to work with.
	convKey string
}

type nativeAliasKey struct{}

// withNativeAlias stores the alias on the request context.
func withNativeAlias(ctx context.Context, alias nativeAlias) context.Context {
	return context.WithValue(ctx, nativeAliasKey{}, alias)
}

// nativeAliasFromContext returns the alias stored on the request context.
func nativeAliasFromContext(ctx context.Context) (nativeAlias, bool) {
	alias, ok := ctx.Value(nativeAliasKey{}).(nativeAlias)
	return alias, ok
}

// lookupNativeRoute returns the native route for the request method+path,
// or nil when the route is not natively served.
func (p *Proxy) lookupNativeRoute(r *http.Request) *NativeRoute {
	key, err := transcode.NewRouteKey(r.Method, r.URL.Path)
	if err != nil {
		return nil
	}
	return p.nativeRouteMap[key]
}

// nativeAction is the disposition of a natively routed request.
type nativeAction int

const (
	// nativeMiss means the request is not native for its model: the
	// caller falls through to the transcode lookup.
	nativeMiss nativeAction = iota
	// nativeServe means the returned request carries the rewritten body
	// and alias context: the caller serves it with the transparent engine.
	nativeServe
	// nativeError means a local dialect error was already written: the
	// caller must stop.
	nativeError
)

// nativeRouteAction validates a natively served request and rewrites its
// model identifier from surrogate to wire. An unknown Via dialect is a
// miss (fall through with the body intact); a known model on the wrong
// dialect fails closed with a local dialect error; every other failure is
// likewise local.
func (p *Proxy) nativeRouteAction(w http.ResponseWriter, r *http.Request, nr *NativeRoute) (*http.Request, nativeAction) {
	if isNativeUpgrade(r) {
		writeNativeDialectError(w, nr.Protocol, http.StatusBadRequest,
			"upgrade requests are not supported on natively served routes")
		return r, nativeError
	}
	if enc := r.Header.Get("Content-Encoding"); enc != "" &&
		!strings.EqualFold(strings.TrimSpace(enc), "identity") {
		writeNativeDialectError(w, nr.Protocol, http.StatusUnsupportedMediaType,
			"content-encoding is not supported on natively served routes")
		return r, nativeError
	}

	limits := nr.BodyLimits.WithDefaults()
	if r.Body == nil {
		writeNativeDialectError(w, nr.Protocol, http.StatusBadRequest, "missing request body")
		return r, nativeError
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limits.AcceptedRequestBytes+1))
	_ = r.Body.Close()
	if err != nil {
		writeNativeDialectError(w, nr.Protocol, http.StatusBadRequest, "read request body: "+err.Error())
		return r, nativeError
	}
	if int64(len(body)) > limits.AcceptedRequestBytes {
		writeNativeDialectError(w, nr.Protocol, http.StatusRequestEntityTooLarge, "request body too large")
		return r, nativeError
	}
	// restoreOriginal puts the consumed bytes back so a native miss falls
	// through to transcode/transparent handling with the body intact.
	restoreOriginal := func() *http.Request {
		out := *r
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		return &out
	}

	// Structural validation only. A native path converts nothing, so the
	// client contract's feature-coverage rules do not apply: every field
	// the upstream can serve — modeled or not, required or optional —
	// must pass through. The tolerant wire decode still enforces the
	// always-reject structural set (duplicate keys at any depth, trailing
	// values, malformed syntax) because that protects this proxy's own
	// parsing, and it skips unknown envelope fields so documented
	// optional controls (service_tier, cache_control, tools without an
	// explicit strict, ...) reach the upstream that owns them. The
	// dialect's own error is authoritative for anything semantic.
	var doc map[string]json.RawMessage
	if err := wire.DecodeTolerant(body, &doc); err != nil {
		writeNativeDialectError(w, nr.Protocol, http.StatusBadRequest, "natively served request: "+err.Error())
		return r, nativeError
	}
	rawModel, ok := doc["model"]
	if !ok {
		writeNativeDialectError(w, nr.Protocol, http.StatusBadRequest, "natively served request: missing model field")
		return r, nativeError
	}
	var clientModel string
	if err := json.Unmarshal(rawModel, &clientModel); err != nil || clientModel == "" {
		writeNativeDialectError(w, nr.Protocol, http.StatusBadRequest, "natively served request: model field must be a non-empty string")
		return r, nativeError
	}

	mapping, err := nr.ModelMap.Resolve(clientModel)
	if err != nil {
		writeNativeDialectError(w, nr.Protocol, http.StatusBadRequest, "natively served request: "+err.Error())
		return r, nativeError
	}
	if mapping.Via == "" {
		// Unknown native dialect (identity resolution): not native, and
		// there is nothing to say about it — fall through with the body
		// intact, preserving legacy transparent behavior.
		return restoreOriginal(), nativeMiss
	}
	// Known model, wrong dialect for this route. When a transcode mapping
	// covers the same client route, fall through so the mapping can
	// convert to the model's native target; otherwise fail closed locally
	// naming the model's native dialect, because forwarding verbatim would
	// send a surrogate the upstream does not know.
	if mapping.Via != nr.Protocol {
		if _, fallback := p.transcodeHandlerMap[nr.RouteKey]; fallback {
			return restoreOriginal(), nativeMiss
		}
		writeNativeDialectError(w, nr.Protocol, http.StatusNotFound,
			fmt.Sprintf("model %q is natively served as %s, not on %s", clientModel, mapping.Via, nr.RouteKey.Path))
		return r, nativeError
	}

	out, ok := rewriteTopLevelModel(body, mapping.UpstreamModel)
	if !ok {
		writeNativeDialectError(w, nr.Protocol, http.StatusInternalServerError, "natively served request: internal error")
		return r, nativeError
	}
	if int64(len(out)) > limits.DecodedRequestBytes {
		writeNativeDialectError(w, nr.Protocol, http.StatusRequestEntityTooLarge, "request body too large")
		return r, nativeError
	}

	rewritten := *r
	rewritten.Body = io.NopCloser(bytes.NewReader(out))
	rewritten.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(out)), nil
	}
	rewritten.ContentLength = int64(len(out))
	rewritten.TransferEncoding = nil
	convKey := transcode.DeriveClientKey(nr.Provider, r.RemoteAddr)
	if firstText := transcode.FirstUserText(nr.Protocol, body); firstText != "" {
		convKey = transcode.DeriveConversationKey(nr.Provider, clientModel, firstText)
	}
	return rewritten.WithContext(withNativeAlias(rewritten.Context(), nativeAlias{
		surrogate: mapping.ClientResponseModel,
		wire:      mapping.UpstreamModel,
		respCap:   limits.SuccessfulResponseBytes,
		convKey:   convKey,
	})), nativeServe
}

// rewriteTopLevelModel returns body with the top-level "model" value
// replaced by wireModel, preserving every other byte exactly. It reports
// false when the document has no top-level model member (already rejected
// earlier) or is not a JSON object.
func rewriteTopLevelModel(body []byte, wireModel string) ([]byte, bool) {
	quoted, err := json.Marshal(wireModel)
	if err != nil {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, false
		}
		afterKey := dec.InputOffset()
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		valueEnd := dec.InputOffset()
		if key != "model" {
			continue
		}
		// The value begins at the first non-space byte after the ':'
		// that follows the key.
		start := int(afterKey)
		for start < len(body) && body[start] != ':' {
			start++
		}
		if start >= len(body) {
			return nil, false
		}
		start++
		for start < len(body) && (body[start] == ' ' || body[start] == '\t' ||
			body[start] == '\n' || body[start] == '\r') {
			start++
		}
		end := int(valueEnd)
		if start > end {
			return nil, false
		}
		out := make([]byte, 0, len(body)-(end-start)+len(quoted))
		out = append(out, body[:start]...)
		out = append(out, quoted...)
		out = append(out, body[end:]...)
		return out, true
	}
	return nil, false
}

// rewriteNativeResponseAlias restores the client-facing model alias on a
// natively served non-streaming JSON response. It is tolerant by design:
// the upstream contract is subject to change, so any unexpected shape,
// encoding, or size passes through untouched — never truncated, never
// failed. Streaming responses are never buffered and stay byte-identical.
func rewriteNativeResponseAlias(res *http.Response) error {
	if res == nil || res.Request == nil {
		return nil
	}
	alias, ok := nativeAliasFromContext(res.Request.Context())
	if !ok {
		return nil
	}
	ct := res.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return nil
	}
	if !strings.HasPrefix(strings.TrimSpace(strings.Split(ct, ";")[0]), "application/json") {
		return nil
	}
	if enc := res.Header.Get("Content-Encoding"); enc != "" &&
		!strings.EqualFold(strings.TrimSpace(enc), "identity") {
		return nil
	}
	if res.Body == nil {
		return nil
	}
	cap := alias.respCap
	if cap <= 0 {
		return nil
	}
	// A declared length over the inspection bound skips buffering
	// entirely: the body streams through untouched.
	if res.ContentLength > cap {
		return nil
	}
	// Unknown or fitting lengths are probed without consuming: an
	// over-bound body is re-concatenated ahead of the unread remainder,
	// so the exchange can never truncate.
	probe, err := io.ReadAll(io.LimitReader(res.Body, cap+1))
	if err != nil {
		res.Body = &nativePrefixBody{Reader: io.MultiReader(bytes.NewReader(probe), res.Body), closer: res.Body}
		return nil
	}
	if int64(len(probe)) > cap {
		res.Body = &nativePrefixBody{Reader: io.MultiReader(bytes.NewReader(probe), res.Body), closer: res.Body}
		return nil
	}
	// Within bounds the limit reader reached EOF: probe holds the full
	// body, so the original can be closed and replaced safely.
	_ = res.Body.Close()
	restore := func() {
		res.Body = io.NopCloser(bytes.NewReader(probe))
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(probe, &doc); err != nil {
		restore()
		return nil
	}
	rawModel, ok := doc["model"]
	if !ok {
		restore()
		return nil
	}
	var wire string
	if err := json.Unmarshal(rawModel, &wire); err != nil || wire != alias.wire {
		restore()
		return nil
	}
	restored, err := json.Marshal(alias.surrogate)
	if err != nil {
		restore()
		return nil
	}
	doc["model"] = restored
	out, err := json.Marshal(doc)
	if err != nil {
		restore()
		return nil
	}
	res.Body = io.NopCloser(bytes.NewReader(out))
	res.ContentLength = int64(len(out))
	res.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

// nativePrefixBody re-concatenates probed bytes ahead of an unconsumed
// remainder. Close propagates to the original body so the upstream
// connection is never leaked by the inspection.
type nativePrefixBody struct {
	io.Reader
	closer io.Closer
}

func (b *nativePrefixBody) Close() error {
	return b.closer.Close()
}

// isNativeUpgrade reports whether the request asks for a protocol upgrade,
// which natively served routes reject like transcoded routes do.
func isNativeUpgrade(r *http.Request) bool {
	if r == nil || r.Header == nil {
		return false
	}
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") &&
			r.Header.Get("Upgrade") != "" {
			return true
		}
	}
	return false
}

// writeNativeDialectError renders a local error in the route's client
// dialect. Chat has no client error shape of its own, so it uses the
// OpenAI (responses) envelope like every other non-messages path.
func writeNativeDialectError(w http.ResponseWriter, protocol transcode.NativeProtocol, status int, message string) {
	target := transcode.ClientResponses
	if protocol == transcode.NativeMessages {
		target = transcode.ClientMessages
	}
	_ = transcode.WriteDialectHTTPError(w, target, transcode.CanonicalAPIError{
		Status:  status,
		Message: message,
	})
}
