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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
)

const catalogSuiteMinErrorBytes = 128

// ModelRoute maps a model identifier to the provider handler responsible for serving it.
type ModelRoute struct {
	Model           string
	Provider        string
	Handler         http.Handler
	SupportedRoutes map[transcode.RouteKey]struct{}
}

// SuiteConfig configures a CatalogSuiteHandler.
type SuiteConfig struct {
	Name              string
	Prefix            string
	CatalogHandler    *transcode.CatalogHandler
	DefaultShape      transcode.CatalogShape
	ModelRoutes       []ModelRoute
	Fallback          http.Handler // optional fallback handler when 1 provider is available
	FallbackRoutes    map[transcode.RouteKey]struct{}
	Limits            transcode.BodyLimits
	InspectionTimeout time.Duration
	Admission         *CatalogSuiteAdmission
	Strict            bool
}

type suiteModelTarget struct {
	handler         http.Handler
	supportedRoutes map[transcode.RouteKey]struct{}
}

// CatalogSuiteHandler serves catalog queries and routes model-specific completion
// requests to the appropriate target provider.
type CatalogSuiteHandler struct {
	name              string
	prefix            string
	catalogHandler    *transcode.CatalogHandler
	defaultShape      transcode.CatalogShape
	modelMap          map[string]suiteModelTarget
	fallback          http.Handler
	fallbackRoutes    map[transcode.RouteKey]struct{}
	limits            transcode.BodyLimits
	inspectionTimeout time.Duration
	strict            bool
	admission         *CatalogSuiteAdmission
}

// NewCatalogSuiteHandler builds a new CatalogSuiteHandler.
func NewCatalogSuiteHandler(cfg SuiteConfig) *CatalogSuiteHandler {
	limits := cfg.Limits.WithDefaults()
	if limits.ErrorResponseBytes < catalogSuiteMinErrorBytes {
		limits.ErrorResponseBytes = catalogSuiteMinErrorBytes
	}
	models := make(map[string]suiteModelTarget, len(cfg.ModelRoutes))
	for _, mr := range cfg.ModelRoutes {
		if mr.Model != "" && mr.Handler != nil {
			models[mr.Model] = suiteModelTarget{
				handler:         mr.Handler,
				supportedRoutes: cloneRouteSet(mr.SupportedRoutes),
			}
		}
	}
	admission := cfg.Admission
	if admission == nil {
		admission = NewCatalogSuiteAdmission(limits)
	}
	inspectionTimeout := cfg.InspectionTimeout
	if inspectionTimeout <= 0 {
		inspectionTimeout = catalogSuiteInspectionWait
	}
	return &CatalogSuiteHandler{
		name:              cfg.Name,
		prefix:            cfg.Prefix,
		catalogHandler:    cfg.CatalogHandler,
		defaultShape:      cfg.DefaultShape,
		modelMap:          models,
		fallback:          cfg.Fallback,
		fallbackRoutes:    cloneRouteSet(cfg.FallbackRoutes),
		limits:            limits,
		inspectionTimeout: inspectionTimeout,
		strict:            cfg.Strict,
		admission:         admission,
	}
}

func cloneRouteSet(routes map[transcode.RouteKey]struct{}) map[transcode.RouteKey]struct{} {
	if len(routes) == 0 {
		return nil
	}
	out := make(map[transcode.RouteKey]struct{}, len(routes))
	for route := range routes {
		out[route] = struct{}{}
	}
	return out
}

// ServeHTTP handles requests to the catalog suite mount.
// The incoming request path is already stripped of the suite prefix.
func (h *CatalogSuiteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqPath := path.Clean(r.URL.Path)

	// Catalog discovery routes: GET /v1/models and GET /v1/models/{model}
	if r.Method == http.MethodGet && isCatalogRoute(reqPath) {
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

	// Any other route: if fallback handler exists and suite is not strict, forward
	if !h.strict && h.fallback != nil {
		h.fallback.ServeHTTP(w, r)
		return
	}

	// An unmatched path on a suite mount answers in the suite's own dialect,
	// like every other error it emits. Go's bare "404 page not found" in
	// text/plain leaks the implementation into an otherwise dialect-correct
	// API and is inconsistent with the count_tokens handling above, whose
	// comment states the rule: the answer must be dialect-shaped rather than
	// a bare 404 page. The shape comes from the default, because an unknown
	// path names no dialect of its own.
	h.writeDialectError(w, h.defaultCatalogShape(), http.StatusNotFound,
		"not found on this suite mount")
}

func isCatalogRoute(path string) bool {
	if path == transcode.CatalogPath {
		return true
	}
	if rem, ok := strings.CutPrefix(path, transcode.CatalogPath+"/"); ok {
		return rem != "" && !strings.Contains(rem, "/")
	}
	return false
}

func isCompletionRoute(path string) bool {
	switch path {
	case "/v1/responses", "/v1/messages", "/v1/chat/completions",
		// Claude Code probes token counting when a gateway advertises the
		// Anthropic Messages format. It is optional (the client falls back
		// to local estimation), but the answer must still be a
		// dialect-shaped response rather than a bare 404 page.
		"/v1/messages/count_tokens":
		return true
	default:
		return false
	}
}

func (h *CatalogSuiteHandler) serveCompletion(w http.ResponseWriter, r *http.Request) {
	shape := h.shapeForCompletion(r)
	maxPeekBytes := h.limits.AcceptedRequestBytes
	if r.ContentLength > maxPeekBytes {
		h.writeDialectError(w, shape, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	releaseSlot, ok := h.acquireBufferSlot(r)
	if !ok {
		return
	}
	var slotHandedOff bool
	defer func() {
		if !slotHandedOff {
			releaseSlot()
		}
	}()

	controller := http.NewResponseController(w)
	// The deadline guards the inspection read and nothing else, so it comes
	// off the moment the body is in hand — before the target handler runs.
	// Left armed it would outlive the inspection it exists for, and nothing
	// about the body reliably disarms it: on HTTP/1.1 the server clears its
	// own read deadline from its next-read bookkeeping, which can run after
	// this handler has already returned, so an armed deadline would be live
	// for the whole target exchange. On HTTP/2 the stream deadline is a
	// self-firing time.AfterFunc that only an explicit zero deadline stops.
	// Lifting it is therefore this handler's job, and it is done before
	// dispatch rather than left to the protocol. The deferred clear stays as
	// the backstop for the error paths, where the inspection ended before EOF.
	deadlineArmed := controller.SetReadDeadline(time.Now().Add(h.inspectionTimeout)) == nil
	clearDeadline := func() {
		if deadlineArmed {
			deadlineArmed = false
			_ = controller.SetReadDeadline(time.Time{})
		}
	}
	defer clearDeadline()

	var timedOut atomic.Bool
	if r.Body != nil {
		originalBody := r.Body
		timer := time.AfterFunc(h.inspectionTimeout, func() {
			timedOut.Store(true)
			_ = originalBody.Close()
		})
		body, err := io.ReadAll(io.LimitReader(originalBody, maxPeekBytes+1))
		timer.Stop()
		_ = originalBody.Close()
		clearDeadline()
		if timedOut.Load() || errors.Is(err, os.ErrDeadlineExceeded) {
			h.writeDialectError(w, shape, http.StatusRequestTimeout, "timed out reading request body")
			return
		}
		if err != nil {
			h.writeDialectError(w, shape, http.StatusBadRequest, "failed to read request body")
			return
		}
		if int64(len(body)) > maxPeekBytes {
			h.writeDialectError(w, shape, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		modelID, err := peekModelField(body)
		if err != nil {
			h.writeDialectError(w, shape, http.StatusBadRequest, "invalid request body")
			return
		}
		if modelID == "" {
			h.writeDialectError(w, shape, http.StatusBadRequest, "missing or empty 'model' field in request")
			return
		}

		target, found := h.modelMap[modelID]
		if !found {
			if h.strict || h.fallback == nil {
				if len(h.modelMap) == 0 {
					h.writeDialectError(w, shape, http.StatusServiceUnavailable, fmt.Sprintf("no provider configured to serve model %q", modelID))
				} else {
					h.writeDialectError(w, shape, http.StatusNotFound, fmt.Sprintf("model %q not found in catalog suite", modelID))
				}
				return
			}
			target = suiteModelTarget{handler: h.fallback, supportedRoutes: h.fallbackRoutes}
		}
		routeKey := transcode.RouteKey{Method: http.MethodPost, Path: path.Clean(r.URL.Path)}
		if _, ok := target.supportedRoutes[routeKey]; !ok {
			h.writeDialectError(w, shape, http.StatusNotFound, fmt.Sprintf("model %q is not available for %s", modelID, routeKey.Path))
			return
		}

		// The catalog is finished with the body. The target takes ownership
		// and the admission token comes back as soon as it spends the body,
		// rather than when the target's own upstream exchange finishes; the
		// backstop covers a target that returns or panics without touching
		// the body at all.
		handoff := newCatalogHandoff(body, releaseSlot)
		slotHandedOff = true
		defer handoff.drain()

		r.Body = handoff.primaryBody()
		r.GetBody = func() (io.ReadCloser, error) {
			return handoff.replayBody(), nil
		}
		r.ContentLength = int64(len(body))
		r.TransferEncoding = nil
		target.handler.ServeHTTP(w, r)
		return
	}
	h.writeDialectError(w, shape, http.StatusBadRequest, "missing request body")
}

func peekModelField(body []byte) (string, error) {
	var probe struct {
		Model string `json:"model"`
	}
	if err := wire.DecodeTolerant(body, &probe); err != nil {
		return "", err
	}
	return probe.Model, nil
}

func (h *CatalogSuiteHandler) shapeForCompletion(r *http.Request) transcode.CatalogShape {
	switch path.Clean(r.URL.Path) {
	case "/v1/responses":
		return transcode.CatalogShapeCodex
	case "/v1/messages", "/v1/messages/count_tokens":
		return transcode.CatalogShapeAnthropic
	case "/v1/chat/completions":
		// Chat completions is an OpenAI-shaped API; its errors must not
		// be rendered in whatever shape the suite's catalog defaults to.
		return transcode.CatalogShapeOpenAI
	default:
		if h.defaultShape != "" {
			return h.defaultShape
		}
		return transcode.CatalogShapeOpenAI
	}
}

func (h *CatalogSuiteHandler) writeEmptyCatalogOr404(w http.ResponseWriter, r *http.Request) {
	shape := h.defaultCatalogShape()
	reqPath := path.Clean(r.URL.Path)
	if after, ok := strings.CutPrefix(reqPath, transcode.CatalogPath+"/"); ok {
		modelID := strings.Trim(after, "/")
		if modelID != "" {
			h.writeModelNotFoundError(w, shape, modelID)
			return
		}
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

// defaultCatalogShape is the suite's configured catalog dialect, defaulting to
// OpenAI when none is set — the same fallback writeEmptyCatalogOr404 uses, so
// discovery and an unmatched path agree on the suite's shape.
func (h *CatalogSuiteHandler) defaultCatalogShape() transcode.CatalogShape {
	if h.defaultShape != "" {
		return h.defaultShape
	}
	return transcode.CatalogShapeOpenAI
}

func (h *CatalogSuiteHandler) writeDialectError(w http.ResponseWriter, shape transcode.CatalogShape, status int, message string) {
	message = boundSuiteMessage(message, h.limits.ErrorMessageBytes)
	var body []byte
	switch shape {
	case transcode.CatalogShapeAnthropic:
		errType := errTypeForStatus(status)
		if status == http.StatusNotFound {
			errType = "not_found_error"
		}
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
				Type:    errType,
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
	if int64(len(body)) > h.limits.ErrorResponseBytes {
		body = fallbackSuiteErrorBody(shape, status)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func fallbackSuiteErrorBody(shape transcode.CatalogShape, status int) []byte {
	if shape == transcode.CatalogShapeAnthropic {
		errType := errTypeForStatus(status)
		if status == http.StatusNotFound {
			errType = "not_found_error"
		}
		body, _ := json.Marshal(struct {
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
			}{Type: errType, Message: "catalog suite error"},
		})
		return body
	}
	body, _ := json.Marshal(struct {
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
		}{Message: "catalog suite error", Type: errTypeForStatus(status), Param: nil, Code: errCodeForStatus(status)},
	})
	return body
}

func boundSuiteMessage(message string, max int) string {
	if max <= 0 || len(message) <= max {
		return message
	}
	if max > 3 {
		return message[:max-3] + "…"
	}
	return message[:max]
}

func (h *CatalogSuiteHandler) writeModelNotFoundError(w http.ResponseWriter, shape transcode.CatalogShape, modelID string) {
	modelID = boundSuiteMessage(modelID, h.limits.ErrorMessageBytes)
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
	if int64(len(body)) > h.limits.ErrorResponseBytes {
		body = fallbackSuiteErrorBody(shape, http.StatusNotFound)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(body)
}

func errTypeForStatus(status int) string {
	switch status {
	case http.StatusNotFound:
		return "invalid_request_error"
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
