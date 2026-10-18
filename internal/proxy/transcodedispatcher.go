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
	"io"
	"net/http"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

type targetTranscodeHandler struct {
	mapping *TranscodeMapping
	handler http.Handler
}

// multiTranscodeDispatcher dispatches a single client route (such as POST /v1/messages)
// across multiple transcode handlers based on the request's target model dialect (Via).
type multiTranscodeDispatcher struct {
	routeKey         transcode.RouteKey
	modelMap         transcode.ModelMap
	acceptedBytes    int64
	chatHandler      http.Handler
	responsesHandler http.Handler
}

func newMultiTranscodeDispatcher(routeKey transcode.RouteKey, targets []targetTranscodeHandler) http.Handler {
	d := &multiTranscodeDispatcher{
		routeKey: routeKey,
	}
	for _, t := range targets {
		if len(d.modelMap.Exact) == 0 || len(t.mapping.ModelMap.Exact) > 0 || !t.mapping.ModelMap.AllowIdentity {
			d.modelMap = t.mapping.ModelMap
		}
		if limits := t.mapping.BodyLimits.WithDefaults(); limits.AcceptedRequestBytes > d.acceptedBytes {
			d.acceptedBytes = limits.AcceptedRequestBytes
		}
		switch t.mapping.Mapping.UpstreamProtocol {
		case transcode.UpstreamResponses:
			d.responsesHandler = t.handler
		case transcode.UpstreamChatCompletions:
			d.chatHandler = t.handler
		}
	}
	if d.acceptedBytes == 0 {
		d.acceptedBytes = transcode.BodyLimits{}.WithDefaults().AcceptedRequestBytes
	}
	return d
}

func (d *multiTranscodeDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var targetVia transcode.NativeProtocol
	if miss, ok := nativeMissFromContext(r.Context()); ok {
		targetVia = miss.via
	}
	if targetVia == "" && r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, d.acceptedBytes+1))
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		if err == nil && int64(len(body)) <= d.acceptedBytes {
			if model, present, err := transcode.TopLevelModelPresent(body); err == nil && present && model != "" {
				if resolved, err := d.modelMap.Resolve(model); err == nil {
					targetVia = resolved.Via
				}
			}
		}
	}
	if targetVia == transcode.NativeResponses && d.responsesHandler != nil {
		d.responsesHandler.ServeHTTP(w, r)
		return
	}
	if d.chatHandler != nil {
		d.chatHandler.ServeHTTP(w, r)
		return
	}
	if d.responsesHandler != nil {
		d.responsesHandler.ServeHTTP(w, r)
		return
	}
	http.Error(w, "no transcode handler available for route", http.StatusNotFound)
}
