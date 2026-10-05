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
	// A mapping's Via is the dialect this route decides against, so a value
	// outside the closed vocabulary could never match a route's Protocol and
	// would leave every request for that model failing closed at request
	// time. The configured paths cannot produce one, so this is defence in
	// depth for a library caller.
	for clientModel, m := range r.ModelMap.Exact {
		switch m.Via {
		case "", transcode.NativeResponses, transcode.NativeMessages, transcode.NativeChat:
		default:
			return fmt.Errorf(
				"native route %s %s: model %q declares unknown native dialect %q (want responses, messages, or chat)",
				r.RouteKey.Method, r.RouteKey.Path, clientModel, m.Via,
			)
		}
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
	// convKey is the stable conversation key the rewrite hook should prefer:
	// derived from the request document (model plus first user text) when it
	// carries user text, else from the client address. It is never empty,
	// so the hook's empty-check is a no-op kept for defence in depth.
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

// lookupNativeRouteKey returns the native route for an already-built key.
// The key is built once per request and shared with the transcode lookup so
// the hot path pays route-key construction (method normalization plus full
// path canonicalization) at most once.
func (p *Proxy) lookupNativeRouteKey(key transcode.RouteKey) *NativeRoute {
	if len(p.nativeRoutes) == 0 {
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
//
// The framing checks (upgrade, content-encoding, body size) run BEFORE the
// model is resolved, so a request carrying any of them is refused locally
// even for a model that would have taken the miss branch and been forwarded
// verbatim. That is deliberate and matches how a transcoded route behaves —
// these are requests the native engine cannot represent at all — but it does
// mean transparency is not unconditional for those three shapes.
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
		// Bounded for the same reason as the decode failure below: the text
		// originates in the transport and need not be free of request bytes.
		writeNativeDialectError(w, nr.Protocol, http.StatusBadRequest,
			transcode.BoundErrorMessage("read request body: "+err.Error(), limits.ErrorMessageBytes))
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
	// Extraction uses the shared TopLevelModel policy so the suite router,
	// this path, and the rewriter below agree on what counts as a readable
	// model field (duplicate keys, non-string values, nested objects).
	clientModel, err := transcode.TopLevelModel(body)
	if err != nil {
		// The decode error can quote a key name straight out of the request
		// ("duplicate JSON key %q"), so it is client-controlled text and is
		// bounded like every other client string this path reflects.
		writeNativeDialectError(w, nr.Protocol, http.StatusBadRequest,
			transcode.BoundErrorMessage("natively served request: "+err.Error(), limits.ErrorMessageBytes))
		return r, nativeError
	}
	if clientModel == "" {
		// Absent and empty share one verdict here: without a model name there
		// is nothing to resolve, and an empty string resolves to nothing.
		// (TopLevelModel reports a non-string model as an error above, so
		// reaching here with "" means absent-or-empty, never type-corrupt.)
		writeNativeDialectError(w, nr.Protocol, http.StatusBadRequest, "natively served request: model field must be a non-empty string")
		return r, nativeError
	}

	mapping, err := nr.ModelMap.Resolve(clientModel)
	if err != nil {
		writeNativeDialectError(w, nr.Protocol, http.StatusBadRequest,
			transcode.BoundErrorMessage("natively served request: "+err.Error(), limits.ErrorMessageBytes))
		return r, nativeError
	}
	if mapping.Via == "" {
		// Unknown native dialect (identity resolution): not native, and
		// there is nothing to say about it — fall through with the body
		// intact, preserving legacy transparent behavior.
		return restoreOriginal(), nativeMiss
	}
	// Known model, wrong dialect for this route. When a transcode mapping
	// covers the same client route, fall through so the exchange is served
	// by conversion instead — note the mapping renders whatever upstream
	// protocol it was configured with, which need not be the model's Via
	// dialect; the mapping is the operator's chosen serving mode for this
	// route. Without such a mapping, fail closed locally naming the model's
	// native dialect, because forwarding verbatim would send a surrogate
	// the upstream does not know.
	if mapping.Via != nr.Protocol {
		if _, fallback := p.transcodeHandlerMap[nr.RouteKey]; fallback {
			return restoreOriginal(), nativeMiss
		}
		writeNativeDialectError(w, nr.Protocol, http.StatusNotFound,
			transcode.BoundErrorMessage(
				fmt.Sprintf("model %q is natively served as %s, not on %s", clientModel, mapping.Via, nr.RouteKey.Path),
				limits.ErrorMessageBytes))
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
// false when the document is not a JSON object, has no single top-level model
// member, or has more than one — a duplicate is left untouched because which
// value a client reads is its parser's decision, not ours.
// It is the writer half of the model-field policy: callers run it only after
// TopLevelModel already validated the document, so its false paths are
// unreachable defence in depth, not a third verdict. It must stay
// byte-surgical (never decode-and-re-encode: that would reorder keys,
// collapse duplicates, and re-escape output), which is why extraction lives
// in TopLevelModel and only the replacement lives here.
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
	var out []byte
	found := false
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
		if found {
			// Two top-level model keys. Which one a client reads is decided by
			// its parser, not by us, so replacing one of them could put the
			// client alias somewhere no parser looks while the value it does
			// read keeps the upstream model. Leaving the document alone is the
			// honest outcome, and it is the same rule the request path gets
			// from the always-reject duplicate-key check.
			return nil, false
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
		out = make([]byte, 0, len(body)-(end-start)+len(quoted))
		out = append(out, body[:start]...)
		out = append(out, quoted...)
		out = append(out, body[end:]...)
		found = true
	}
	if !found {
		return nil, false
	}
	return out, true
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
	bound := alias.respCap
	if bound <= 0 {
		return nil
	}
	// A declared length over the inspection bound skips buffering
	// entirely: the body streams through untouched.
	if res.ContentLength > bound {
		return nil
	}
	// Unknown or fitting lengths are probed without consuming: an
	// over-bound body is re-concatenated ahead of the unread remainder,
	// so the exchange can never truncate.
	probe, err := io.ReadAll(io.LimitReader(res.Body, bound+1))
	if err != nil {
		res.Body = &nativePrefixBody{Reader: io.MultiReader(bytes.NewReader(probe), res.Body), closer: res.Body}
		return nil
	}
	if int64(len(probe)) > bound {
		res.Body = &nativePrefixBody{Reader: io.MultiReader(bytes.NewReader(probe), res.Body), closer: res.Body}
		return nil
	}
	// Within bounds the limit reader reached EOF: probe holds the full
	// body, so the original can be closed and replaced safely.
	_ = res.Body.Close()
	restore := func() {
		res.Body = io.NopCloser(bytes.NewReader(probe))
	}
	// Only a response that actually carries the wire model is rewritten. One
	// naming anything else — a provider alias, a multi-model reply — is
	// forwarded exactly as it arrived, because stomping it with the client
	// alias would misreport what the upstream said.
	var named struct {
		Model *string `json:"model"`
	}
	if err := json.Unmarshal(probe, &named); err != nil ||
		named.Model == nil || *named.Model != alias.wire {
		restore()
		return nil
	}
	// The rewrite is surgical, exactly as on the request side: only the model
	// value is replaced, so every other byte the upstream sent reaches the
	// client unchanged. Re-encoding the document instead would reorder
	// top-level keys, collapse duplicates, and re-escape every '<', '>' and
	// '&' as \uXXXX — a sixfold amplification of ordinary model output, with
	// nothing bounding the result.
	out, ok := rewriteTopLevelModel(probe, alias.surrogate)
	if !ok {
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
// which natively served routes reject like transcoded routes do. It delegates
// to the transcode handler's classifier so both boundaries classify
// identically by construction — an upgrade rejected on a transcoded route is
// rejected on a native route too, never silently forwarded where one path
// refuses it.
func isNativeUpgrade(r *http.Request) bool {
	if r == nil || r.Header == nil {
		return false
	}
	return transcode.IsUpgradeRequest(r)
}

// writeNativeDialectError renders a local error in the route's client
// dialect. Chat has no client error shape of its own, so it uses the
// OpenAI (responses) envelope like every other non-messages path.
//
// A 5xx from this path is a proxy-local defect (the upstream was never
// seen), so it sets the recorder's proxyGeneratedError fact: the exchange
// classification must count it as a local failure, never an upstream one —
// a purely local bug must not open the circuit breaker against a healthy
// upstream. Client-fault statuses (4xx) leave no marker; they are not
// upstream health signals either way.
func writeNativeDialectError(w http.ResponseWriter, protocol transcode.NativeProtocol, status int, message string) {
	target := transcode.ClientResponses
	if protocol == transcode.NativeMessages {
		target = transcode.ClientMessages
	}
	if status >= 500 {
		if rec, ok := w.(*statusRecorder); ok && !rec.terminalWritten {
			rec.proxyGeneratedError = true
		}
	}
	_ = transcode.WriteDialectHTTPError(w, target, transcode.CanonicalAPIError{
		Status:  status,
		Message: message,
	})
}
