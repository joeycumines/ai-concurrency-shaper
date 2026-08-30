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
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// TestParseTranscodeRoute verifies the -transcode-route value parsing with
// the clientProtocol@clientPath=upstreamProtocol@upstreamPath format.
func TestParseTranscodeRoute(t *testing.T) {
	m, err := parseTranscodeRoute("responses@/v1/responses=chat-completions@/v1/chat/completions")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.ClientRoute.Path != "/v1/responses" || m.UpstreamPath != "/v1/chat/completions" ||
		m.ClientProtocol != transcode.ClientResponses || m.UpstreamProtocol != transcode.UpstreamChatCompletions {
		t.Errorf("mapping = %+v", m)
	}
	if m.ClientRoute.Method != "POST" {
		t.Errorf("client route method = %q, want POST", m.ClientRoute.Method)
	}

	for _, bad := range []string{
		"",
		"responses@/v1/responses",
		"responses@/v1/responses=chat-completions",
		"@/v1/responses=chat-completions@/v1/chat/completions",
		"responses@=chat-completions@/v1/chat/completions",
		"responses@v1/responses=chat-completions@/v1/chat/completions",
		"responses@/v1/responses=chat-completions@v1/chat/completions",
	} {
		if _, err := parseTranscodeRoute(bad); err == nil {
			t.Errorf("parseTranscodeRoute(%q): want error", bad)
		}
	}
}

// TestParseTranscodeRouteChatClientRejected verifies chat-completions is
// rejected as a client protocol at parse time (chat is upstream-only).
func TestParseTranscodeRouteChatClientRejected(t *testing.T) {
	_, err := parseTranscodeRoute("chat-completions@/v1/chat/completions=responses@/v1/responses")
	if err == nil {
		t.Fatal("expected chat-completions client rejection")
	}
	if !strings.Contains(err.Error(), "upstream-only") {
		t.Fatalf("error = %v", err)
	}
}

// TestParseTranscodeRouteAtInPath verifies paths containing '@' parse
// correctly (the first '@' separates the protocol).
func TestParseTranscodeRouteAtInPath(t *testing.T) {
	m, err := parseTranscodeRoute("responses@/v1/models=chat-completions@/v1/models@predict")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.UpstreamPath != "/v1/models@predict" {
		t.Errorf("upstream path = %q, want /v1/models@predict", m.UpstreamPath)
	}
}

// TestBuildTranscodeMappings verifies the preset flags expand to the correct
// route mappings, appended after any explicit -transcode-route values, and
// that the sensible defaults are applied to every mapping.
func TestBuildTranscodeMappings(t *testing.T) {
	explicit := proxy.TranscodeMapping{Mapping: transcode.Mapping{
		ClientRoute:      mustRouteKey("POST", "/v1/custom"),
		ClientProtocol:   transcode.ClientResponses,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/upstream",
		ModelMap:         transcode.ModelMap{AllowIdentity: true},
		Auth:             transcode.AuthPolicy{Mode: transcode.AuthNone},
	}}

	none, err := buildTranscodeMappings(nil, false, false, false, transcodeCLIOptions{lossPolicy: transcode.StrictLossPolicy()})
	if err != nil {
		t.Fatalf("no flags: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("no flags: mappings = %+v, want none", none)
	}

	all, err := buildTranscodeMappings(
		[]proxy.TranscodeMapping{explicit},
		true,
		false,
		true,
		transcodeCLIOptions{lossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
			transcode.FeatureToolSchemaStrictness: {},
		}}},
	)
	if err != nil {
		t.Fatalf("all flags: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all flags: mappings = %d, want 3", len(all))
	}
	if all[0].ClientRoute != explicit.ClientRoute {
		t.Errorf("mappings[0] = %+v, want explicit route", all[0])
	}
	wantPresets := []struct {
		path     string
		client   transcode.ClientProtocol
		upstream transcode.UpstreamProtocol
	}{
		{"/v1/responses", transcode.ClientResponses, transcode.UpstreamChatCompletions},
		{"/v1/messages", transcode.ClientMessages, transcode.UpstreamResponses},
	}
	for i, want := range wantPresets {
		got := all[i+1]
		if got.ClientRoute.Path != want.path || got.ClientProtocol != want.client || got.UpstreamProtocol != want.upstream {
			t.Errorf("preset %d = %+v, want %s %s->%s", i, got, want.path, want.client, want.upstream)
		}
		if got.ClientRoute.Method != "POST" {
			t.Errorf("preset %d method = %q, want POST", i, got.ClientRoute.Method)
		}
	}

	withChat, err := buildTranscodeMappings(nil, false, true, false, transcodeCLIOptions{lossPolicy: transcode.StrictLossPolicy()})
	if err != nil {
		t.Fatalf("messages-chat: %v", err)
	}
	if len(withChat) != 1 || withChat[0].ClientRoute.Path != "/v1/messages" ||
		withChat[0].UpstreamProtocol != transcode.UpstreamChatCompletions {
		t.Errorf("messages-chat = %+v", withChat)
	}

	single, err := buildTranscodeMappings(nil, true, false, false, transcodeCLIOptions{lossPolicy: transcode.StrictLossPolicy()})
	if err != nil {
		t.Fatalf("responses-chat: %v", err)
	}
	if len(single) != 1 || single[0].ClientRoute.Path != "/v1/responses" {
		t.Errorf("responses-chat = %+v", single)
	}
}

// TestBuildTranscodeMappingsDefaults verifies the sensible out-of-the-box
// defaults land on every CLI mapping.
func TestBuildTranscodeMappingsDefaults(t *testing.T) {
	mappings, err := buildTranscodeMappings(
		nil,
		true,
		true,
		false,
		transcodeCLIOptions{
			lossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
				transcode.FeatureImageInput: {},
			}},
			capabilities: transcode.ChatCapabilities{StopSequences: true},
			clientQuery:  map[string]struct{}{"foo": {}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 2 {
		t.Fatalf("mappings = %d, want 2", len(mappings))
	}
	for i, m := range mappings {
		cap := m.Mapping.ChatCapabilities
		if !cap.ProviderReasoningText || !cap.ParallelToolCalls {
			t.Errorf("mapping %d capabilities = %+v, want the compatible core", i, cap)
		}
		if cap.ReasoningEffort || cap.DeveloperRole {
			t.Errorf("mapping %d: fidelity-only capabilities must be opt-in, got %+v", i, cap)
		}
		if !cap.StopSequences {
			t.Errorf("mapping %d: CLI stop_sequences capability not merged", i)
		}
		if !m.Mapping.LossPolicy.Allows(transcode.FeatureReasoningSummary) ||
			!m.Mapping.LossPolicy.Allows(transcode.FeatureUsageCacheWriteUnknown) ||
			!m.Mapping.LossPolicy.Allows(transcode.FeatureAnthropicControls) {
			t.Errorf("mapping %d: default losses missing", i)
		}
		if !m.Mapping.LossPolicy.Allows(transcode.FeatureRequestReasoning) ||
			!m.Mapping.LossPolicy.Allows(transcode.FeatureDeveloperRole) {
			t.Errorf("mapping %d: compatibility-default losses missing", i)
		}
		if !m.Mapping.LossPolicy.Allows(transcode.FeatureImageInput) {
			t.Errorf("mapping %d: CLI image_input loss not merged", i)
		}
		if _, ok := m.Mapping.AllowedClientQuery["beta"]; !ok {
			t.Errorf("mapping %d: default beta query not allowed", i)
		}
		if _, ok := m.Mapping.AllowedClientQuery["foo"]; !ok {
			t.Errorf("mapping %d: CLI query not merged", i)
		}
	}
}

// TestBuildTranscodeMappingsNegation proves `!name` negations withdraw the
// sensible defaults on every CLI mapping.
func TestBuildTranscodeMappingsNegation(t *testing.T) {
	mappings, err := buildTranscodeMappings(
		nil,
		true,
		true,
		false,
		transcodeCLIOptions{
			negatedCapabilities: map[string]struct{}{
				"provider_reasoning_text": {},
			},
			negatedQuery: map[string]struct{}{"beta": {}},
			negatedLosses: map[transcode.Feature]struct{}{
				transcode.FeatureBuiltinTools: {},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 2 {
		t.Fatalf("mappings = %d, want 2", len(mappings))
	}
	for i, m := range mappings {
		cap := m.Mapping.ChatCapabilities
		if cap.ProviderReasoningText {
			t.Errorf("mapping %d: !provider_reasoning_text did not withdraw the default", i)
		}
		if !cap.ParallelToolCalls {
			t.Errorf("mapping %d: unrelated default capabilities withdrawn: %+v", i, cap)
		}
		if cap.ReasoningEffort || cap.DeveloperRole {
			t.Errorf("mapping %d: opt-in capabilities must be off by default: %+v", i, cap)
		}
		if _, ok := m.Mapping.AllowedClientQuery["beta"]; ok {
			t.Errorf("mapping %d: !beta did not withdraw the default query", i)
		}
		if m.Mapping.LossPolicy.Allows(transcode.FeatureBuiltinTools) {
			t.Errorf("mapping %d: !builtin_tools did not withdraw the default loss", i)
		}
		if !m.Mapping.LossPolicy.Allows(transcode.FeatureReasoningSummary) ||
			!m.Mapping.LossPolicy.Allows(transcode.FeatureRequestReasoning) {
			t.Errorf("mapping %d: unrelated default losses withdrawn", i)
		}
	}

	mappings, err = buildTranscodeMappings(
		nil,
		true,
		false,
		false,
		transcodeCLIOptions{
			negatedCapabilities: map[string]struct{}{"parallel_tool_calls": {}},
			capabilities:        transcode.ChatCapabilities{StopSequences: true},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	cap := mappings[0].Mapping.ChatCapabilities
	if !cap.StopSequences {
		t.Fatal("explicit positive lost under an unrelated negation")
	}
	if cap.ParallelToolCalls {
		t.Fatal("negated default still present")
	}
}

// TestBuildTranscodeMappingsStrictDefaults proves -transcode-strict-defaults
// yields the blank-slate configuration.
func TestBuildTranscodeMappingsStrictDefaults(t *testing.T) {
	mappings, err := buildTranscodeMappings(
		nil,
		true,
		false,
		false,
		transcodeCLIOptions{
			strictDefaults: true,
			capabilities:   transcode.ChatCapabilities{ImageInput: true},
			clientQuery:    map[string]struct{}{"custom": {}},
			lossPolicy: transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
				transcode.FeatureTopK: {},
			}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 1 {
		t.Fatalf("mappings = %d, want 1", len(mappings))
	}
	m := mappings[0].Mapping
	if m.ChatCapabilities.ReasoningEffort || m.ChatCapabilities.DeveloperRole ||
		m.ChatCapabilities.ProviderReasoningText || m.ChatCapabilities.ParallelToolCalls {
		t.Fatalf("capabilities = %+v, want blank slate", m.ChatCapabilities)
	}
	if !m.ChatCapabilities.ImageInput {
		t.Fatal("explicit positive capability lost under strict defaults")
	}
	if len(m.AllowedClientQuery) != 1 {
		t.Fatalf("allowed query = %v, want only the explicit custom", m.AllowedClientQuery)
	}
	if _, ok := m.AllowedClientQuery["custom"]; !ok {
		t.Fatal("explicit positive query lost under strict defaults")
	}
	if m.LossPolicy.Allows(transcode.FeatureReasoningSummary) ||
		m.LossPolicy.Allows(transcode.FeatureResponsesControls) ||
		m.LossPolicy.Allows(transcode.FeatureAnthropicControls) ||
		m.LossPolicy.Allows(transcode.FeatureBuiltinTools) {
		t.Fatalf("loss policy allows defaults, want blank slate")
	}
	if !m.LossPolicy.Allows(transcode.FeatureTopK) {
		t.Fatal("explicit positive loss lost under strict defaults")
	}
}

// TestParseNegatedLosses verifies the -transcode-allow-loss values with
// `!name` negations.
func TestParseNegatedLosses(t *testing.T) {
	allowed, negated, err := parseNegatedLosses("top_k", "!builtin_tools", "image_input")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := allowed[transcode.FeatureTopK]; !ok {
		t.Fatal("top_k missing from positives")
	}
	if _, ok := allowed[transcode.FeatureImageInput]; !ok {
		t.Fatal("image_input missing from positives")
	}
	if _, ok := negated[transcode.FeatureBuiltinTools]; !ok {
		t.Fatal("builtin_tools missing from negations")
	}
	if _, ok := allowed[transcode.FeatureBuiltinTools]; ok {
		t.Fatal("negated name leaked into positives")
	}

	onlyNegatedAllowed, onlyNegated, err := parseNegatedLosses("!builtin_tools")
	if err != nil {
		t.Fatalf("parseNegatedLosses(!builtin_tools): %v", err)
	}
	if len(onlyNegatedAllowed) != 0 {
		t.Fatalf("onlyNegatedAllowed = %v, want empty", onlyNegatedAllowed)
	}
	if _, ok := onlyNegated[transcode.FeatureBuiltinTools]; !ok {
		t.Fatal("builtin_tools missing from onlyNegated")
	}

	if _, _, err := parseNegatedLosses("!bogus"); err == nil {
		t.Fatal("unknown negated loss accepted")
	} else if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("error = %v", err)
	}
	if _, _, err := parseNegatedLosses("top_k", "!top_k"); err == nil {
		t.Fatal("conflicting loss accepted")
	} else if !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("error = %v", err)
	}
	if _, _, err := parseNegatedLosses("!"); err == nil {
		t.Fatal("empty negation accepted")
	}
	if _, _, err := parseNegatedLosses("!all"); err == nil {
		t.Fatal("legacy broad negation accepted")
	}
}

// TestParseChatCapabilities verifies the -transcode-chat-capability vocabulary.
func TestParseChatCapabilities(t *testing.T) {
	cap, negated, err := parseChatCapabilities([]string{"reasoning_effort", "developer_role parallel_tool_calls"})
	if err != nil {
		t.Fatal(err)
	}
	if !cap.ReasoningEffort || !cap.DeveloperRole || !cap.ParallelToolCalls {
		t.Fatalf("capabilities = %+v", cap)
	}
	if cap.ImageInput || cap.StopSequences {
		t.Fatalf("capabilities = %+v, want untouched fields false", cap)
	}
	if len(negated) != 0 {
		t.Fatalf("negated = %v, want none", negated)
	}

	cap, negated, err = parseChatCapabilities([]string{"!reasoning_effort", "stop_sequences"})
	if err != nil {
		t.Fatal(err)
	}
	if cap.ReasoningEffort || !cap.StopSequences {
		t.Fatalf("capabilities = %+v", cap)
	}
	if _, ok := negated["reasoning_effort"]; !ok {
		t.Fatalf("negated = %v, want reasoning_effort", negated)
	}

	if _, _, err := parseChatCapabilities([]string{"bogus"}); err == nil {
		t.Fatal("unknown capability accepted")
	} else if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("error = %v", err)
	}
	if _, _, err := parseChatCapabilities([]string{"!bogus"}); err == nil {
		t.Fatal("unknown negated capability accepted")
	} else if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("error = %v", err)
	}
	if _, _, err := parseChatCapabilities([]string{"reasoning_effort", "!reasoning_effort"}); err == nil {
		t.Fatal("conflicting capability accepted")
	} else if !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("error = %v", err)
	}
	if _, _, err := parseChatCapabilities([]string{"all"}); err == nil {
		t.Fatal("broad legacy name accepted")
	}
	if _, _, err := parseChatCapabilities([]string{"!"}); err == nil {
		t.Fatal("empty negation accepted")
	}

	cap, negated, err = parseChatCapabilities([]string{"system_anywhere"})
	if err != nil {
		t.Fatal(err)
	}
	if !cap.SystemAnywhere {
		t.Fatalf("capabilities = %+v, want SystemAnywhere", cap)
	}
	if len(negated) != 0 {
		t.Fatalf("negated = %v, want none", negated)
	}
	cap, negated, err = parseChatCapabilities([]string{"!system_anywhere"})
	if err != nil {
		t.Fatal(err)
	}
	if cap.SystemAnywhere {
		t.Fatalf("capabilities = %+v, want SystemAnywhere unset", cap)
	}
	if _, ok := negated["system_anywhere"]; !ok {
		t.Fatalf("negated = %v, want system_anywhere", negated)
	}
}

// TestParseClientQuery verifies the -transcode-allow-client-query parsing.
func TestParseClientQuery(t *testing.T) {
	q, negated, err := parseClientQuery([]string{"beta", "api-version foo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := q["beta"]; !ok {
		t.Fatal("beta missing")
	}
	if _, ok := q["api-version"]; !ok {
		t.Fatal("api-version missing")
	}
	if _, ok := q["foo"]; !ok {
		t.Fatal("foo missing")
	}
	if len(negated) != 0 {
		t.Fatalf("negated = %v, want none", negated)
	}

	q, negated, err = parseClientQuery([]string{"!beta", "extra"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := q["beta"]; ok {
		t.Fatal("negated beta still in positives")
	}
	if _, ok := q["extra"]; !ok {
		t.Fatal("extra missing")
	}
	if _, ok := negated["beta"]; !ok {
		t.Fatalf("negated = %v, want beta", negated)
	}

	if _, _, err := parseClientQuery([]string{"beta", "!beta"}); err == nil {
		t.Fatal("conflicting query name accepted")
	}

	for _, bad := range []string{"a=b", "a&b", "a#b", "a?b"} {
		if _, _, err := parseClientQuery([]string{bad}); err == nil {
			t.Errorf("parseClientQuery(%q): want error", bad)
		}
	}
	if _, _, err := parseClientQuery([]string{"!a=b"}); err == nil {
		t.Error("parseClientQuery(!a=b): want error")
	}
	empty, negated, err := parseClientQuery([]string{" "})
	if err != nil || len(empty) != 0 || len(negated) != 0 {
		t.Fatalf("parseClientQuery(whitespace) = %v, %v; want empty sets", empty, err)
	}
}

// TestBuildTranscodeMappingsConflict verifies enabling both Messages presets
// fails before proxy.New runs.
func TestBuildTranscodeMappingsConflict(t *testing.T) {
	_, err := buildTranscodeMappings(nil, false, true, true, transcodeCLIOptions{lossPolicy: transcode.StrictLossPolicy()})
	if err == nil {
		t.Fatal("expected both-messages-preset conflict")
	}
	if !strings.Contains(err.Error(), "both map /v1/messages") {
		t.Fatalf("error = %v", err)
	}
}

// TestParseTranscodeModelMap verifies the -transcode-model parsing.
func TestParseTranscodeModelMap(t *testing.T) {
	empty, err := parseTranscodeModelMap(nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.AllowIdentity != true {
		t.Fatal("empty map must allow identity")
	}

	models, err := parseTranscodeModelMap([]string{
		"claude-3=claude-3",
		"gpt-4o=gpt-4o-mini",
	})
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := models.Resolve("claude-3")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.UpstreamModel != "claude-3" || mapping.ClientResponseModel != "claude-3" {
		t.Fatalf("mapping = %+v", mapping)
	}
	mapping, err = models.Resolve("gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.UpstreamModel != "gpt-4o-mini" {
		t.Fatalf("upstream model = %q", mapping.UpstreamModel)
	}

	for _, bad := range []string{"no-equals", "=x", "x="} {
		if _, err := parseTranscodeModelMap([]string{bad}); err == nil {
			t.Errorf("parseTranscodeModelMap(%q): want error", bad)
		}
	}
}

// TestParseTranscodeAuth verifies the auth CLI contract.
func TestParseTranscodeAuth(t *testing.T) {
	policy, err := parseTranscodeAuth("auto", "inbound", "", "2023-06-01")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != transcode.AuthAuto || !policy.Inbound || policy.AnthropicVersion != "2023-06-01" {
		t.Fatalf("policy = %+v", policy)
	}

	policy, err = parseTranscodeAuth("header", "env:UPSTREAM_KEY", "X-Upstream-Key", "")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != transcode.AuthCustomHeader || policy.Inbound || policy.CustomHeader != "X-Upstream-Key" {
		t.Fatalf("policy = %+v", policy)
	}
	if policy.Secret == nil {
		t.Fatal("secret source missing")
	}

	if _, err := parseTranscodeAuth("auto", "bogus", "", ""); err == nil {
		t.Fatal("expected invalid source rejection")
	}

	// AuthNone must NEVER enable inbound credential extraction or require secret resolution (review-15 finding 1, 2)
	nonePolicy, err := parseTranscodeAuth("none", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if nonePolicy.Mode != transcode.AuthNone || nonePolicy.Inbound || nonePolicy.Secret != nil {
		t.Fatalf("nonePolicy = %+v, want Mode=AuthNone, Inbound=false, Secret=nil", nonePolicy)
	}

	noneWithSource, err := parseTranscodeAuth("none", "env:UNSET_ENV_KEY", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if noneWithSource.Mode != transcode.AuthNone || noneWithSource.Inbound || noneWithSource.Secret != nil {
		t.Fatalf("noneWithSource = %+v, want Mode=AuthNone, Inbound=false, Secret=nil", noneWithSource)
	}
}

// TestParseTranscodeRouteMessagesUpstreamRejected proves the CLI rejects a
// messages upstream at parse time.
func TestParseTranscodeRouteMessagesUpstreamRejected(t *testing.T) {
	_, err := parseTranscodeRoute("responses@/v1/responses=messages@/v1/messages")
	if err == nil {
		t.Fatal("expected messages upstream rejection")
	}
	if !strings.Contains(err.Error(), "want responses or chat-completions") {
		t.Fatalf("error = %v", err)
	}
}

// TestParseTranscodeAuthExternalSignerRejectedAtStartup proves the CLI
// cannot configure an external signer.
func TestParseTranscodeAuthExternalSignerRejectedAtStartup(t *testing.T) {
	policy, err := parseTranscodeAuth("external-signer", "inbound", "", "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := transcode.NewRouteKey(http.MethodPost, "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	mapping := transcode.Mapping{
		ClientRoute:      key,
		ClientProtocol:   transcode.ClientResponses,
		UpstreamProtocol: transcode.UpstreamChatCompletions,
		UpstreamPath:     "/v1/chat/completions",
		Auth:             policy,
	}
	if err := mapping.Validate(); err == nil {
		t.Fatal("expected external-signer rejection without a signer")
	} else if !strings.Contains(err.Error(), "requires a signer") {
		t.Fatalf("error = %v", err)
	}
}

// TestBuildTranscodeMappingsAppliesLossPolicy proves the CLI loss policy is
// applied to every mapping.
func TestBuildTranscodeMappingsAppliesLossPolicy(t *testing.T) {
	policy := transcode.LossPolicy{Allowed: map[transcode.Feature]struct{}{
		transcode.FeatureUsageUnknown: {},
	}}
	mappings, err := buildTranscodeMappings(nil, false, true, false, transcodeCLIOptions{lossPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 1 {
		t.Fatalf("mappings = %d", len(mappings))
	}
	if !mappings[0].Mapping.LossPolicy.Allows(transcode.FeatureUsageUnknown) {
		t.Fatal("loss policy not applied to the mapping")
	}
	if mappings[0].Mapping.LossPolicy.Allows(transcode.FeatureOutputPhase) {
		t.Fatal("loss policy leaks unapproved features")
	}
}

// TestExplicitModelMapRejectsUnknownModel proves identity fallback applies
// only when no model mappings were supplied.
func TestExplicitModelMapRejectsUnknownModel(t *testing.T) {
	m, err := parseTranscodeModelMap([]string{"client-a=upstream-a"})
	if err != nil {
		t.Fatal(err)
	}
	if m.AllowIdentity {
		t.Fatal("explicit mapping must disable identity fallback")
	}
	if _, err := m.Resolve("client-a"); err != nil {
		t.Fatalf("mapped model rejected: %v", err)
	}
	if _, err := m.Resolve("unknown"); err == nil {
		t.Fatal("unknown model passed through an explicit mapping")
	}

	m, err = parseTranscodeModelMap(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.AllowIdentity {
		t.Fatal("no mappings must keep identity fallback")
	}
	if _, err := m.Resolve("anything"); err != nil {
		t.Fatalf("identity fallback failed: %v", err)
	}

	if _, err := parseTranscodeModelMap([]string{"a=b", "a=c"}); err == nil {
		t.Fatal("duplicate client model accepted")
	}
}

// TestPerProviderTranscodeRouteIsolation verifies that two providers can
// mount identical client routes without error, while duplicate routes within
// a single provider fail closed.
func TestPerProviderTranscodeRouteIsolation(t *testing.T) {
	// 1. Two providers with identical client route -> valid
	args := []string{
		"--provider=anthropic",
		"-upstream", "https://api.anthropic.com",
		"-prefix", "/anthropic",
		"-transcode-route", "responses@/v1/responses=chat-completions@/v1/chat/completions",
		"--provider=openai",
		"-upstream", "https://api.openai.com",
		"-prefix", "/openai",
		"-transcode-route", "responses@/v1/responses=chat-completions@/v1/chat/completions",
	}
	cfg, err := Parse(args)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(cfg.Providers))
	}
	for i, p := range cfg.Providers {
		if len(p.TranscodeMappings()) != 1 {
			t.Errorf("provider %d mappings = %d, want 1", i, len(p.TranscodeMappings()))
		}
	}

	// 2. Duplicate client route within a single provider -> fail closed
	badArgs := []string{
		"--provider=anthropic",
		"-upstream", "https://api.anthropic.com",
		"-prefix", "/anthropic",
		"-transcode-route", "responses@/v1/responses=chat-completions@/v1/chat/completions",
		"-transcode-responses-chat", // also maps POST /v1/responses
	}
	badCfg, err := Parse(badArgs)
	if err != nil {
		t.Fatalf("Parse badArgs: %v", err)
	}
	err = badCfg.ResolveAndValidate()
	if err == nil {
		t.Fatal("expected duplicate route rejection within single provider")
	}
	if !strings.Contains(err.Error(), "duplicate transcode mapping") {
		t.Fatalf("error = %v", err)
	}
}

// TestFileSecretSourceBounded proves the secret file read is bounded at
// maxSecretFileBytes with one-byte overflow detection, and the file is
// re-read per request so atomic replacement rotates the credential.
func TestFileSecretSourceBounded(t *testing.T) {
	dir := t.TempDir()

	t.Run("oversized file rejected", func(t *testing.T) {
		path := dir + "/too-large"
		if err := os.WriteFile(path, []byte(strings.Repeat("k", maxSecretFileBytes+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		source := fileSecretSource(path)
		if _, err := source.Secret(context.Background()); err == nil {
			t.Fatal("oversized secret file accepted")
		} else if !strings.Contains(err.Error(), "byte bound") {
			t.Fatalf("err = %v, want the bound error", err)
		}
	})

	t.Run("at bound accepted and trimmed", func(t *testing.T) {
		path := dir + "/at-bound"
		content := strings.Repeat("k", maxSecretFileBytes)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		source := fileSecretSource(path)
		secret, err := source.Secret(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(secret) != maxSecretFileBytes {
			t.Fatalf("secret length = %d, want %d", len(secret), maxSecretFileBytes)
		}
	})

	t.Run("rotation without restart", func(t *testing.T) {
		path := dir + "/rotating"
		if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		source := fileSecretSource(path)
		if secret, _ := source.Secret(context.Background()); secret != "first" {
			t.Fatalf("secret = %q", secret)
		}
		replacement := dir + "/replacement"
		if err := os.WriteFile(replacement, []byte("second\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
		if secret, _ := source.Secret(context.Background()); secret != "second" {
			t.Fatalf("rotated secret = %q", secret)
		}
	})
}

// TestResolveAndValidate_TranscodeAuthInheritance verifies auth policy precedence:
// 1. Inherits provider auth policy when no per-route auth is declared.
// 2. Explicit -transcode-auth-source overrides provider auth policy.
// 3. Explicit -transcode-auth none overrides provider auth policy to AuthNone.
// 4. Unset or empty env var in -transcode-auth-source fails closed at ResolveAndValidate.
func TestResolveAndValidate_TranscodeAuthInheritance(t *testing.T) {
	t.Setenv("PROVIDER_SECRET", "provider-val")
	t.Setenv("ROUTE_SECRET", "route-val")
	t.Setenv("EMPTY_SECRET", "  ")

	// 1. Inherited provider auth
	{
		cfg, err := Parse([]string{
			"-upstream", "https://api.openai.com",
			"-auth-source", "env:PROVIDER_SECRET",
			"-transcode-responses-chat",
		})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if err := cfg.ResolveAndValidate(); err != nil {
			t.Fatalf("ResolveAndValidate: %v", err)
		}
		mappings := cfg.Providers[0].TranscodeMappings()
		if len(mappings) != 1 {
			t.Fatalf("mappings = %d, want 1", len(mappings))
		}
		authP := mappings[0].Mapping.Auth
		if authP.Mode != transcode.AuthBearer {
			t.Errorf("auth mode = %q, want bearer", authP.Mode)
		}
		if authP.Secret == nil {
			t.Fatal("secret source is nil")
		}
		sec, err := authP.Secret.Secret(context.Background())
		if err != nil || sec != "provider-val" {
			t.Errorf("secret = %q, err = %v, want provider-val", sec, err)
		}
	}

	// 2. Explicit transcode auth override
	{
		cfg, err := Parse([]string{
			"-upstream", "https://api.openai.com",
			"-auth-source", "env:PROVIDER_SECRET",
			"-transcode-responses-chat",
			"-transcode-auth-source", "env:ROUTE_SECRET",
		})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if err := cfg.ResolveAndValidate(); err != nil {
			t.Fatalf("ResolveAndValidate: %v", err)
		}
		mappings := cfg.Providers[0].TranscodeMappings()
		authP := mappings[0].Mapping.Auth
		sec, err := authP.Secret.Secret(context.Background())
		if err != nil || sec != "route-val" {
			t.Errorf("override secret = %q, err = %v, want route-val", sec, err)
		}
	}

	// 3. Explicit transcode auth none override
	{
		cfg, err := Parse([]string{
			"-upstream", "https://api.openai.com",
			"-auth-source", "env:PROVIDER_SECRET",
			"-transcode-responses-chat",
			"-transcode-auth", "none",
		})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if err := cfg.ResolveAndValidate(); err != nil {
			t.Fatalf("ResolveAndValidate: %v", err)
		}
		mappings := cfg.Providers[0].TranscodeMappings()
		authP := mappings[0].Mapping.Auth
		if authP.Mode != transcode.AuthNone {
			t.Errorf("auth mode = %q, want none", authP.Mode)
		}
	}

	// 4. Unset env var fails closed at ResolveAndValidate
	{
		cfg, err := Parse([]string{
			"-upstream", "https://api.openai.com",
			"-transcode-responses-chat",
			"-transcode-auth-source", "env:NONEXISTENT_KEY_XYZ",
		})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		err = cfg.ResolveAndValidate()
		if err == nil {
			t.Fatal("expected unset env var to fail ResolveAndValidate")
		}
		if !strings.Contains(err.Error(), "NONEXISTENT_KEY_XYZ") {
			t.Fatalf("err = %v, want mentioning NONEXISTENT_KEY_XYZ", err)
		}
	}

	// 5. Empty env var fails closed at ResolveAndValidate
	{
		cfg, err := Parse([]string{
			"-upstream", "https://api.openai.com",
			"-transcode-responses-chat",
			"-transcode-auth-source", "env:EMPTY_SECRET",
		})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		err = cfg.ResolveAndValidate()
		if err == nil {
			t.Fatal("expected empty env var to fail ResolveAndValidate")
		}
		if !strings.Contains(err.Error(), "empty") {
			t.Fatalf("err = %v, want mentioning empty", err)
		}
	}
}

// TestResolveAndValidate_BodyLimits_Propagation verifies that Provider.RetryMaxBodyMB
// propagates to each TranscodeMapping.BodyLimits.RetryReplayBytes where zero (H2).
func TestResolveAndValidate_BodyLimits_Propagation(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-retry-max-body-mb", "2",
		"-transcode-responses-chat",
		"-transcode-messages-responses",
		"-transcode-allow-loss", "tool_schema_strictness",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	mappings := cfg.Providers[0].TranscodeMappings()
	if len(mappings) != 2 {
		t.Fatalf("mappings count = %d, want 2", len(mappings))
	}
	for i, m := range mappings {
		if got := m.BodyLimits.RetryReplayBytes; got != 2<<20 {
			t.Errorf("mapping %d (%s %s) RetryReplayBytes = %d, want %d", i, m.ClientRoute.Method, m.ClientRoute.Path, got, 2<<20)
		}
	}
}

// TestResolveAndValidate_FileSecretSource_RotationRequiresRestart proves that
// file: credentials are resolved once at startup and wrapped as static secrets,
// so in-place file modifications mid-run do not change the credential on either
// passthrough or transcoded routes without a restart (H6).
func TestResolveAndValidate_FileSecretSource_RotationRequiresRestart(t *testing.T) {
	dir := t.TempDir()
	secretPath := dir + "/secret.key"
	if err := os.WriteFile(secretPath, []byte("initial-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-auth-source", "file:" + secretPath,
		"-transcode-responses-chat",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}

	// Overwrite the file on disk mid-run
	if err := os.WriteFile(secretPath, []byte("rotated-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Provider-level auth policy still returns initial-token
	provAuth := cfg.Providers[0].AuthPolicy()
	if provAuth == nil || provAuth.Secret == nil {
		t.Fatal("provider auth policy or secret source is nil")
	}
	provSec, err := provAuth.Secret.Secret(context.Background())
	if err != nil || provSec != "initial-token" {
		t.Fatalf("provider auth secret after file overwrite = %q, err = %v, want initial-token", provSec, err)
	}

	// 2. Inherited transcode route auth policy also still returns initial-token
	mappings := cfg.Providers[0].TranscodeMappings()
	if len(mappings) != 1 {
		t.Fatalf("mappings = %d, want 1", len(mappings))
	}
	tcSec, err := mappings[0].Mapping.Auth.Secret.Secret(context.Background())
	if err != nil || tcSec != "initial-token" {
		t.Fatalf("transcode auth secret after file overwrite = %q, err = %v, want initial-token", tcSec, err)
	}
}

// TestResolveAndValidate_TranscodeAuth_NoneWithUnsetEnvVar proves that setting
// -transcode-auth none skips secret resolution even if -transcode-auth-source specifies
// an unset environment variable (review-15 finding 2).
func TestResolveAndValidate_TranscodeAuth_NoneWithUnsetEnvVar(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
		"-transcode-auth", "none",
		"-transcode-auth-source", "env:DEFINITELY_UNSET_VAR_XYZ_123",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate with -transcode-auth none failed: %v", err)
	}
	mappings := cfg.Providers[0].TranscodeMappings()
	if len(mappings) != 1 {
		t.Fatalf("mappings = %d, want 1", len(mappings))
	}
	if mappings[0].Mapping.Auth.Mode != transcode.AuthNone {
		t.Errorf("auth mode = %q, want none", mappings[0].Mapping.Auth.Mode)
	}
	if mappings[0].Mapping.Auth.Inbound {
		t.Error("Inbound = true, want false")
	}
	if mappings[0].Mapping.Auth.Secret != nil {
		t.Error("Secret is not nil, want nil")
	}
}

// TestProvider_TranscodeMappings_DeepCopyIsIsolated verifies that mutating maps
// returned by Provider.TranscodeMappings() does not alter the provider's internal state (review-15 finding 7).
func TestProvider_TranscodeMappings_DeepCopyIsIsolated(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
		"-transcode-model", "client-m=upstream-m",
		"-transcode-allow-loss", "top_k",
		"-transcode-allow-client-query", "custom_param",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}

	// First copy
	m1 := cfg.Providers[0].TranscodeMappings()
	if len(m1) != 1 {
		t.Fatalf("m1 len = %d, want 1", len(m1))
	}

	// Mutate maps in m1
	m1[0].Mapping.ModelMap.Exact["mutated"] = transcode.ModelMapping{UpstreamModel: "hacked"}
	m1[0].Mapping.LossPolicy.Allowed[transcode.FeatureImageInput] = struct{}{}
	m1[0].Mapping.AllowedClientQuery["hacked_param"] = struct{}{}

	// Second copy
	m2 := cfg.Providers[0].TranscodeMappings()
	if _, ok := m2[0].Mapping.ModelMap.Exact["mutated"]; ok {
		t.Error("mutation of ModelMap leaked into second copy")
	}
	if _, ok := m2[0].Mapping.LossPolicy.Allowed[transcode.FeatureImageInput]; ok {
		t.Error("mutation of LossPolicy leaked into second copy")
	}
	if _, ok := m2[0].Mapping.AllowedClientQuery["hacked_param"]; ok {
		t.Error("mutation of AllowedClientQuery leaked into second copy")
	}
}

// TestResolveAndValidate_Transcode_RetryReplayBytes_ZeroWhenRetriesDisabled proves
// that when RetryMax is 0, RetryReplayBytes remains 0 even if RetryMaxBodyMB is configured (review-15 finding 4, review-18 finding 4).
func TestResolveAndValidate_Transcode_RetryReplayBytes_ZeroWhenRetriesDisabled(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-retry", "0",
		"-retry-max-body-mb", "5",
		"-transcode-responses-chat",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate: %v", err)
	}
	mappings := cfg.Providers[0].TranscodeMappings()
	if len(mappings) != 1 {
		t.Fatalf("mappings = %d, want 1", len(mappings))
	}
	if got := mappings[0].BodyLimits.RetryReplayBytes; got != 0 {
		t.Errorf("RetryReplayBytes = %d, want 0 when retries disabled", got)
	}
}

// TestResolveAndValidate_TranscodeMaxBodyMB_DefaultsAndOverrides tests that
// transcode request and response memory limits default to 10 MiB out of the box
// and can be customized via flags (Review 19 #A3, Review 20 #1).
func TestResolveAndValidate_TranscodeMaxBodyMB_DefaultsAndOverrides(t *testing.T) {
	// 1. Defaults: 10 MiB
	cfgDefault, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
	})
	if err != nil {
		t.Fatalf("Parse (default): %v", err)
	}
	if err := cfgDefault.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate (default): %v", err)
	}
	pDefault := cfgDefault.Providers[0]
	if pDefault.TranscodeMaxRequestMB != 10 {
		t.Errorf("TranscodeMaxRequestMB default = %d, want 10", pDefault.TranscodeMaxRequestMB)
	}
	if pDefault.TranscodeMaxResponseMB != 10 {
		t.Errorf("TranscodeMaxResponseMB default = %d, want 10", pDefault.TranscodeMaxResponseMB)
	}
	mappingsDefault := pDefault.TranscodeMappings()
	if len(mappingsDefault) != 1 {
		t.Fatalf("mappingsDefault len = %d, want 1", len(mappingsDefault))
	}
	if got := mappingsDefault[0].BodyLimits.DecodedRequestBytes; got != 10<<20 {
		t.Errorf("DecodedRequestBytes = %d, want %d", got, 10<<20)
	}
	if got := mappingsDefault[0].BodyLimits.SuccessfulResponseBytes; got != 10<<20 {
		t.Errorf("SuccessfulResponseBytes = %d, want %d", got, 10<<20)
	}

	// 2. Custom overrides
	cfgCustom, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
		"-transcode-max-request-mb", "25",
		"-transcode-max-response-mb", "40",
	})
	if err != nil {
		t.Fatalf("Parse (custom): %v", err)
	}
	if err := cfgCustom.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate (custom): %v", err)
	}
	pCustom := cfgCustom.Providers[0]
	mappingsCustom := pCustom.TranscodeMappings()
	if got := mappingsCustom[0].BodyLimits.DecodedRequestBytes; got != 25<<20 {
		t.Errorf("DecodedRequestBytes custom = %d, want %d", got, 25<<20)
	}
	if got := mappingsCustom[0].BodyLimits.SuccessfulResponseBytes; got != 40<<20 {
		t.Errorf("SuccessfulResponseBytes custom = %d, want %d", got, 40<<20)
	}

	// 3. Negative validation
	cfgNeg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-responses-chat",
		"-transcode-max-request-mb", "-1",
	})
	if err != nil {
		t.Fatalf("Parse (negative): %v", err)
	}
	if err := cfgNeg.ResolveAndValidate(); err == nil {
		t.Fatal("ResolveAndValidate with negative max-request-mb should fail")
	}
}

// TestResolveAndValidate_TranscodeAuth_InvalidModesAndCustomHeaders verifies
// that invalid auth modes and invalid/reserved custom header names fail startup
// validation (Review 19 #C1).
func TestResolveAndValidate_TranscodeAuth_InvalidModesAndCustomHeaders(t *testing.T) {
	cases := []struct {
		name    string
		flags   []string
		wantErr string
	}{
		{
			name: "unknown_mode",
			flags: []string{
				"-upstream", "https://api.openai.com",
				"-transcode-responses-chat",
				"-transcode-auth", "bogus",
			},
			wantErr: "unknown auth mode",
		},
		{
			name: "colon_syntax_rejected",
			flags: []string{
				"-upstream", "https://api.openai.com",
				"-transcode-responses-chat",
				"-transcode-auth", "header:X-Custom-Key",
			},
			wantErr: "unknown auth mode",
		},
		{
			name: "header_mode_empty_header_name",
			flags: []string{
				"-upstream", "https://api.openai.com",
				"-transcode-responses-chat",
				"-transcode-auth", "header",
			},
			wantErr: "custom auth header is empty",
		},
		{
			name: "header_mode_invalid_header_name",
			flags: []string{
				"-upstream", "https://api.openai.com",
				"-transcode-responses-chat",
				"-transcode-auth", "header",
				"-transcode-auth-header", "Invalid Header Name",
			},
			wantErr: "not a valid HTTP field name",
		},
		{
			name: "header_mode_reserved_header_name",
			flags: []string{
				"-upstream", "https://api.openai.com",
				"-transcode-responses-chat",
				"-transcode-auth", "header",
				"-transcode-auth-header", "Authorization",
			},
			wantErr: "reserved by the proxy pipeline",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse(tc.flags)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = cfg.ResolveAndValidate()
			if err == nil {
				t.Fatal("ResolveAndValidate expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestParseNegatedLosses_AllCanonicalFeatures verifies that every canonical
// Feature can be passed positively or with '!' negation and is correctly mapped
// without silent drop (Review 19 #C2).
func TestParseNegatedLosses_AllCanonicalFeatures(t *testing.T) {
	for _, feat := range transcode.RegisteredLossKeys() {
		name := string(feat)
		// Positive
		allowed, negated, err := parseNegatedLosses(name)
		if err != nil {
			t.Fatalf("parseNegatedLosses(%q): %v", name, err)
		}
		if _, ok := allowed[feat]; !ok {
			t.Errorf("feature %s was not added to allowed map", name)
		}
		if len(negated) != 0 {
			t.Errorf("expected empty negated map for positive feature %s", name)
		}

		// Negated
		negName := "!" + name
		allowed, negated, err = parseNegatedLosses(negName)
		if err != nil {
			t.Fatalf("parseNegatedLosses(%q): %v", negName, err)
		}
		if _, ok := negated[feat]; !ok {
			t.Errorf("feature %s was not added to negated map", name)
		}
		if len(allowed) != 0 {
			t.Errorf("expected empty allowed map for negated feature %s", name)
		}
	}
}

// TestResolveAndValidate_MessagesResponses_StrictDefaultsRequiresLoss verifies
// that -transcode-messages-responses under -transcode-strict-defaults demands
// explicit -transcode-allow-loss tool_schema_strictness (Review 19 #C3).
func TestResolveAndValidate_MessagesResponses_StrictDefaultsRequiresLoss(t *testing.T) {
	// Without explicit loss approval under strict defaults -> fail
	cfgWithoutLoss, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-messages-responses",
		"-transcode-strict-defaults",
	})
	if err != nil {
		t.Fatalf("Parse (without loss): %v", err)
	}
	if err := cfgWithoutLoss.ResolveAndValidate(); err == nil {
		t.Fatal("expected ResolveAndValidate to fail without tool_schema_strictness under strict defaults")
	} else if !strings.Contains(err.Error(), "tool_schema_strictness") {
		t.Errorf("expected error mentioning tool_schema_strictness, got: %v", err)
	}

	// With explicit loss approval -> pass
	cfgWithLoss, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-messages-responses",
		"-transcode-strict-defaults",
		"-transcode-allow-loss", "tool_schema_strictness",
	})
	if err != nil {
		t.Fatalf("Parse (with loss): %v", err)
	}
	if err := cfgWithLoss.ResolveAndValidate(); err != nil {
		t.Fatalf("ResolveAndValidate (with loss): %v", err)
	}
}
