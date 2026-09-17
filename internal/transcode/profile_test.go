package transcode

import (
	"encoding/json"
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
