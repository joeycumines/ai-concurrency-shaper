package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// TestNativeRouteAcceptsViaGeminiModel proves a provider with a native route
// and a via=gemini model-table entry starts: the closed Via vocabulary must
// include gemini for every native route, not only Gemini routes.
func TestNativeRouteAcceptsViaGeminiModel(t *testing.T) {
	route := NativeRoute{
		RouteKey: transcode.RouteKey{Method: http.MethodPost, Path: "/v1/chat/completions"},
		Protocol: transcode.NativeChat,
		ModelMap: transcode.ModelMap{Exact: map[string]transcode.ModelMapping{
			"alias": {
				ClientModel:         "alias",
				UpstreamModel:       "gemini-2.5-pro",
				ClientResponseModel: "alias",
				Via:                 transcode.NativeGemini,
			},
		}},
	}
	if err := route.Validate(); err != nil {
		t.Fatalf("Validate rejected a via=gemini model on a native chat route: %v", err)
	}
}

func TestWriteNativeDialectErrorGemini(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", nil)
	writeNativeDialectError(rec, req, transcode.NativeGemini, http.StatusBadRequest, "request body is not JSON")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var env struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %q is not a Google error envelope: %v", rec.Body.String(), err)
	}
	if env.Error.Code != http.StatusBadRequest || env.Error.Status != "INVALID_ARGUMENT" ||
		env.Error.Message != "request body is not JSON" {
		t.Fatalf("envelope = %+v, want a 400 INVALID_ARGUMENT Google error", env.Error)
	}
}
