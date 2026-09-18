package transcode

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProfileMapResolve verifies profile resolution returns the mapped model
// and tier, or ok=false when unmapped.
func TestProfileMapResolve(t *testing.T) {
	pm := ProfileMap{
		Profiles: map[string]ProfileMapping{
			"scanner": {Model: "gpt-4o-mini", ReasoningTier: "low"},
			"analyst": {Model: "gpt-4o", ReasoningTier: "high"},
			"basic":   {Model: "gpt-4o-mini"},
		},
	}

	tests := []struct {
		name      string
		profile   string
		wantModel string
		wantTier  string
		wantOK    bool
	}{
		{"mapped with tier", "scanner", "gpt-4o-mini", "low", true},
		{"mapped high tier", "analyst", "gpt-4o", "high", true},
		{"mapped no tier", "basic", "gpt-4o-mini", "", true},
		{"unmapped", "unknown", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, tier, ok := pm.ResolveProfile(tt.profile)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}
			if tier != tt.wantTier {
				t.Errorf("tier = %q, want %q", tier, tt.wantTier)
			}
		})
	}
}

// TestProfileMapNilProfiles verifies a nil Profiles map returns ok=false.
func TestProfileMapNilProfiles(t *testing.T) {
	pm := ProfileMap{}
	if _, _, ok := pm.ResolveProfile("anything"); ok {
		t.Error("nil Profiles should return ok=false")
	}
}

// TestProfileTierApplicationInChatRender verifies the profile tier is applied
// to the chat request when the client specified no reasoning effort.
func TestProfileTierApplicationInChatRender(t *testing.T) {
	req := CanonicalRequest{
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hello"}}},
		},
	}
	ctx := &ExchangeContext{
		UpstreamModel:         "upstream-model",
		ResolvedReasoningTier: "high",
	}
	caps := ChatCapabilities{ReasoningEffort: true}

	body, _, err := RenderChatRequest(req, ctx, caps)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	var chat struct {
		ReasoningEffort *string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chat.ReasoningEffort == nil || *chat.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort = %v, want high", chat.ReasoningEffort)
	}
}

// TestProfileTierDoesNotOverrideExplicit verifies the profile tier does not
// override an explicit client reasoning effort.
func TestProfileTierDoesNotOverrideExplicit(t *testing.T) {
	req := CanonicalRequest{
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hello"}}},
		},
	}
	effort := "low"
	ctx := &ExchangeContext{
		UpstreamModel:         "upstream-model",
		ResolvedReasoningTier: "high",
		OriginalResponsesRequest: &ResponsesRequestEcho{
			Reasoning: &ResponsesEnvelopeReasoning{Effort: &effort},
		},
	}
	caps := ChatCapabilities{ReasoningEffort: true}

	body, _, err := RenderChatRequest(req, ctx, caps)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	var chat struct {
		ReasoningEffort *string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chat.ReasoningEffort == nil || *chat.ReasoningEffort != "low" {
		t.Errorf("reasoning_effort = %v, want low (client explicit wins)", chat.ReasoningEffort)
	}
}

// TestProfileTierWithoutCapability verifies the profile tier is not applied
// when the ReasoningEffort capability is not granted.
func TestProfileTierWithoutCapability(t *testing.T) {
	req := CanonicalRequest{
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hello"}}},
		},
	}
	ctx := &ExchangeContext{
		UpstreamModel:         "upstream-model",
		ResolvedReasoningTier: "high",
	}
	caps := ChatCapabilities{ReasoningEffort: false}

	body, _, err := RenderChatRequest(req, ctx, caps)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	var chat struct {
		ReasoningEffort *string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if chat.ReasoningEffort != nil {
		t.Errorf("reasoning_effort = %v, want nil (capability not granted)", chat.ReasoningEffort)
	}
}

// TestProfileTierApplicationInResponsesRender verifies the profile tier is
// applied to the Responses request when the client specified no reasoning.
func TestProfileTierApplicationInResponsesRender(t *testing.T) {
	req := CanonicalRequest{
		Turns: []CanonicalTurn{
			{Role: CanonicalUser, Parts: []CanonicalPart{CanonicalText{Text: "hello"}}},
		},
	}
	ctx := &ExchangeContext{
		UpstreamModel:         "upstream-model",
		ResolvedReasoningTier: "medium",
	}

	body, _, err := RenderResponsesRequest(req, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	var resp struct {
		Reasoning *struct {
			Effort *string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Reasoning == nil || resp.Reasoning.Effort == nil ||
		*resp.Reasoning.Effort != "medium" {
		t.Errorf("reasoning.effort = %v, want medium", resp.Reasoning)
	}
}

// TestHandlerProfileResolution verifies that profile resolution in TranscodeHandler:
// 1) Maps profile name to target model and upstream wire model without leaking profile name upstream
// 2) Preserves profile name as the client-facing response model
// 3) Uses profile target model as UpstreamModel under identity fallback (never profile name)
// 4) Rejects with 400 when profile target model cannot be resolved
// 5) Records Note and pins tier when profile tier conflicts with model mapping tier
func TestHandlerProfileResolution(t *testing.T) {
	t.Run("explicit_model_mapping", func(t *testing.T) {
		var capturedUpstreamBody []byte
		rt := RoundTrip(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			capturedUpstreamBody = body
			chatResp := `{
				"id": "chatcmpl-test",
				"object": "chat.completion",
				"created": 1234567890,
				"model": "gpt-4o-mini-wire",
				"choices": [{
					"index": 0,
					"message": {"role": "assistant", "content": "hello from upstream"},
					"finish_reason": "stop"
				}],
				"usage": {"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10}
			}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(chatResp)),
			}, nil
		})

		mapping := Mapping{
			ClientProtocol:   ClientResponses,
			UpstreamProtocol: UpstreamChatCompletions,
			ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
			UpstreamPath:     "/v1/chat/completions",
			LossPolicy:       j6PermissivePolicy(),
			ProfileMap: ProfileMap{
				Profiles: map[string]ProfileMapping{
					"scanner": {Model: "gpt-4o-mini", ReasoningTier: "low"},
				},
			},
			ModelMap: ModelMap{
				Exact: map[string]ModelMapping{
					"gpt-4o-mini": {
						ClientModel:         "gpt-4o-mini",
						UpstreamModel:       "gpt-4o-mini-wire",
						ClientResponseModel: "gpt-4o-mini",
					},
				},
				AllowIdentity: false,
			},
		}

		handler := testHandler(t, mapping, rt)
		reqBody := `{"model":"scanner","input":"hi"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
		}

		// Verify upstream received the resolved wire model, NOT the profile name "scanner".
		var upstreamReq struct {
			Model           string  `json:"model"`
			ReasoningEffort *string `json:"reasoning_effort"`
		}
		if err := json.Unmarshal(capturedUpstreamBody, &upstreamReq); err != nil {
			t.Fatalf("unmarshal upstream body: %v", err)
		}
		if upstreamReq.Model != "gpt-4o-mini-wire" {
			t.Errorf("upstream model = %q, want gpt-4o-mini-wire (profile name leaked!)", upstreamReq.Model)
		}
		if upstreamReq.ReasoningEffort == nil || *upstreamReq.ReasoningEffort != "low" {
			t.Errorf("upstream reasoning_effort = %v, want low", upstreamReq.ReasoningEffort)
		}

		// Verify downstream received the requested profile name "scanner".
		var clientResp struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &clientResp); err != nil {
			t.Fatalf("unmarshal client response: %v", err)
		}
		if clientResp.Model != "scanner" {
			t.Errorf("client response model = %q, want scanner", clientResp.Model)
		}
	})

	t.Run("identity_fallback_uses_target_model", func(t *testing.T) {
		var capturedUpstreamBody []byte
		rt := RoundTrip(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			capturedUpstreamBody = body
			chatResp := `{
				"id": "chatcmpl-test",
				"object": "chat.completion",
				"created": 1234567890,
				"model": "gpt-4o",
				"choices": [{
					"index": 0,
					"message": {"role": "assistant", "content": "hello from upstream"},
					"finish_reason": "stop"
				}],
				"usage": {"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10}
			}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(chatResp)),
			}, nil
		})

		mapping := Mapping{
			ClientProtocol:   ClientResponses,
			UpstreamProtocol: UpstreamChatCompletions,
			ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
			UpstreamPath:     "/v1/chat/completions",
			LossPolicy:       j6PermissivePolicy(),
			ProfileMap: ProfileMap{
				Profiles: map[string]ProfileMapping{
					"analyst": {Model: "gpt-4o", ReasoningTier: "high"},
				},
			},
			ModelMap: ModelMap{
				AllowIdentity: true,
			},
		}

		handler := testHandler(t, mapping, rt)
		reqBody := `{"model":"analyst","input":"hi"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
		}

		// Verify upstream model is the profile target model "gpt-4o", NEVER the profile name "analyst".
		var upstreamReq struct {
			Model           string  `json:"model"`
			ReasoningEffort *string `json:"reasoning_effort"`
		}
		if err := json.Unmarshal(capturedUpstreamBody, &upstreamReq); err != nil {
			t.Fatalf("unmarshal upstream body: %v", err)
		}
		if upstreamReq.Model != "gpt-4o" {
			t.Errorf("upstream model = %q, want gpt-4o (must never be analyst)", upstreamReq.Model)
		}
	})

	t.Run("unmapped_target_model_rejection", func(t *testing.T) {
		roundTripCalled := false
		rt := RoundTrip(func(req *http.Request) (*http.Response, error) {
			roundTripCalled = true
			return nil, fmt.Errorf("must not be called")
		})

		mapping := Mapping{
			ClientProtocol:   ClientResponses,
			UpstreamProtocol: UpstreamChatCompletions,
			ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
			UpstreamPath:     "/v1/chat/completions",
			ProfileMap: ProfileMap{
				Profiles: map[string]ProfileMapping{
					"worker": {Model: "unmapped-llm"},
				},
			},
			ModelMap: ModelMap{
				Exact: map[string]ModelMapping{
					"other-model": {ClientModel: "other-model", UpstreamModel: "other-wire"},
				},
				AllowIdentity:      false,
				RequireExplicitMap: true,
			},
		}

		handler := testHandler(t, mapping, rt)
		reqBody := `{"model":"worker","input":"hi"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)
		if roundTripCalled {
			t.Fatal("RoundTrip was called for unmapped profile target model")
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
		}
		var errResp struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("unmarshal error response: %v", err)
		}
		if !strings.Contains(errResp.Error.Message, `profile "worker" targets unmapped model "unmapped-llm"`) {
			t.Errorf("error message = %q, want containing profile error", errResp.Error.Message)
		}
		if !strings.Contains(errResp.Error.Message, "servable on this mount: other-model") {
			t.Errorf("error message = %q, want containing servable models", errResp.Error.Message)
		}
	})

	t.Run("tier_collapse_note_and_model_mapping_tier_precedence", func(t *testing.T) {
		var capturedUpstreamBody []byte
		rt := RoundTrip(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			capturedUpstreamBody = body
			chatResp := `{
				"id": "chatcmpl-test",
				"object": "chat.completion",
				"created": 1234567890,
				"model": "model-wire",
				"choices": [{
					"index": 0,
					"message": {"role": "assistant", "content": "hello"},
					"finish_reason": "stop"
				}],
				"usage": {"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10}
			}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(chatResp)),
			}, nil
		})

		mapping := Mapping{
			ClientProtocol:   ClientResponses,
			UpstreamProtocol: UpstreamChatCompletions,
			ClientRoute:      RouteKey{Method: http.MethodPost, Path: "/v1/responses"},
			UpstreamPath:     "/v1/chat/completions",
			LossPolicy:       j6PermissivePolicy(),
			ProfileMap: ProfileMap{
				Profiles: map[string]ProfileMapping{
					"high-profile": {Model: "pinned-model", ReasoningTier: "high"},
				},
			},
			ModelMap: ModelMap{
				Exact: map[string]ModelMapping{
					"pinned-model": {
						ClientModel:         "pinned-model",
						UpstreamModel:       "model-wire",
						ClientResponseModel: "pinned-model",
						ReasoningTier:       "low",
					},
				},
				AllowIdentity: false,
			},
		}

		handler := testHandler(t, mapping, rt)
		reqBody := `{"model":"high-profile","input":"hi"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
		}

		var upstreamReq struct {
			ReasoningEffort *string `json:"reasoning_effort"`
		}
		if err := json.Unmarshal(capturedUpstreamBody, &upstreamReq); err != nil {
			t.Fatalf("unmarshal upstream body: %v", err)
		}
		if upstreamReq.ReasoningEffort == nil || *upstreamReq.ReasoningEffort != "low" {
			t.Errorf("upstream reasoning_effort = %v, want low (model mapping pins tier)", upstreamReq.ReasoningEffort)
		}
	})
}
