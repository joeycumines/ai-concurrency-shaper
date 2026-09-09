package config

// Live compatibility failure (2026-09-07): Claude Code marks failed tool
// results with is_error:true routinely, and the CLI default policy rejected
// the whole request (tool_result_error_status was registered but not
// default-approved). The default profile now approves the key so the
// documented permissive encoding (the visible [tool_result_error] prefix)
// applies out of the box; the strict programmatic policy, -transcode-strict
// -defaults, and an explicit negation still reject.

import (
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func TestDefaultPolicyApprovesToolResultErrorStatus(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-messages-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatal(err)
	}
	policy := cfg.Providers[0].TranscodeMappings()[0].Mapping.LossPolicy
	if !policy.Allows(transcode.FeatureToolResultErrorStatus) {
		t.Fatal("the CLI default policy must approve tool_result_error_status (live Claude Code is_error results)")
	}
}

func TestStrictDefaultsStillRejectToolResultErrorStatus(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-messages-chat",
		"-transcode-strict-defaults",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatal(err)
	}
	policy := cfg.Providers[0].TranscodeMappings()[0].Mapping.LossPolicy
	if policy.Allows(transcode.FeatureToolResultErrorStatus) {
		t.Fatal("-transcode-strict-defaults must withdraw the tool_result_error_status approval")
	}
}

func TestNegationWithdrawsToolResultErrorStatusDefault(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-messages-chat",
		"-transcode-allow-loss", "!tool_result_error_status",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatal(err)
	}
	policy := cfg.Providers[0].TranscodeMappings()[0].Mapping.LossPolicy
	if policy.Allows(transcode.FeatureToolResultErrorStatus) {
		t.Fatal("an explicit negation must withdraw the tool_result_error_status default")
	}
}

func TestDefaultPolicyApprovesRequestCitations(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-messages-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatal(err)
	}
	policy := cfg.Providers[0].TranscodeMappings()[0].Mapping.LossPolicy
	if !policy.Allows(transcode.FeatureRequestCitations) {
		t.Fatal("the CLI default policy must approve request_citations for replayed conversation history")
	}
}

func TestStrictDefaultsStillRejectRequestCitations(t *testing.T) {
	cfg, err := Parse([]string{
		"-upstream", "https://api.openai.com",
		"-transcode-messages-chat",
		"-transcode-strict-defaults",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ResolveAndValidate(); err != nil {
		t.Fatal(err)
	}
	policy := cfg.Providers[0].TranscodeMappings()[0].Mapping.LossPolicy
	if policy.Allows(transcode.FeatureRequestCitations) {
		t.Fatal("-transcode-strict-defaults must withdraw the request_citations approval")
	}
}
