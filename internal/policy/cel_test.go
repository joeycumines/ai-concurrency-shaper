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

package policy

import (
	"net/http"
	"testing"

	"github.com/joeycumines/ai-concurrency-shaper/internal/client"
)

func TestPolicyEngine_AddPolicy(t *testing.T) {
	eng := NewPolicyEngine()
	behavior := NewRetryOn403Behavior()

	err := eng.AddPolicy("test-policy", "originator.is_local", behavior, true)
	if err != nil {
		t.Fatalf("unexpected error adding policy: %v", err)
	}
	if eng.PolicyCount() != 1 {
		t.Errorf("expected 1 policy, got %d", eng.PolicyCount())
	}
	if eng.ActivePolicyCount() != 1 {
		t.Errorf("expected 1 active policy, got %d", eng.ActivePolicyCount())
	}
}

func TestPolicyEngine_AddPolicy_InvalidExpr(t *testing.T) {
	eng := NewPolicyEngine()
	behavior := NewRetryOn403Behavior()

	err := eng.AddPolicy("bad-policy", "not valid cel expression!!!", behavior, true)
	if err == nil {
		t.Error("expected error for invalid CEL expression")
	}
}

func TestPolicyEngine_Evaluate_NoMatch(t *testing.T) {
	eng := NewPolicyEngine()
	behavior := NewRetryOn403Behavior()

	err := eng.AddPolicy("local-only", "originator.is_local", behavior, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	remote := client.Originator{IP: "203.0.113.50", IsLocal: false}
	result := eng.Evaluate(nil, remote, nil)
	if result != nil {
		t.Errorf("expected nil (no match) for remote originator, got %v", result)
	}
}

func TestPolicyEngine_Evaluate_MatchIsLocal(t *testing.T) {
	eng := NewPolicyEngine()
	behavior := NewRetryOn403Behavior()

	err := eng.AddPolicy("local-retry", "originator.is_local", behavior, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	local := client.Originator{IP: "127.0.0.1", IsLocal: true, PID: 1234}
	result := eng.Evaluate(nil, local, nil)
	if result == nil {
		t.Error("expected matching behavior for local originator")
	}
	if result.Name() != "retry-on-403" {
		t.Errorf("expected retry-on-403 behavior, got %q", result.Name())
	}
}

func TestPolicyEngine_EvaluateWithResponse(t *testing.T) {
	eng := NewPolicyEngine()
	behavior := NewRetryOn403Behavior()

	err := eng.AddPolicy("high-failure", "client.failure_count > 3", behavior, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	local := client.Originator{IP: "127.0.0.1", IsLocal: true, PID: 1234}
	state := &client.ClientState{FailureCount: 5, IsCrushed: true}

	req, _ := http.NewRequest("GET", "http://example.com/api", nil)
	result := eng.EvaluateWithResponse(req, local, state, http.StatusForbidden)
	if result == nil {
		t.Error("expected matching behavior for crushed client with 403")
	}
}

func TestPolicyEngine_Evaluate_DisabledPolicy(t *testing.T) {
	eng := NewPolicyEngine()
	behavior := NewRetryOn403Behavior()

	err := eng.AddPolicy("disabled", "originator.is_local", behavior, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	local := client.Originator{IP: "127.0.0.1", IsLocal: true}
	result := eng.Evaluate(nil, local, nil)
	if result != nil {
		t.Errorf("expected nil for disabled policy, got %v", result)
	}
}

func TestPolicyEngine_Evaluate_NoEnabledPolicies(t *testing.T) {
	eng := NewPolicyEngine()

	local := client.Originator{IP: "127.0.0.1", IsLocal: true}
	result := eng.Evaluate(nil, local, nil)
	if result != nil {
		t.Errorf("expected nil when no policies registered, got %v", result)
	}
}

func TestPolicyEngine_Evaluate_ByIP(t *testing.T) {
	eng := NewPolicyEngine()
	behavior := NewRetryOn403Behavior()

	err := eng.AddPolicy("specific-ip", "originator.ip == '10.0.0.1'", behavior, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	matches := client.Originator{IP: "10.0.0.1", IsLocal: false}
	result := eng.Evaluate(nil, matches, nil)
	if result == nil {
		t.Error("expected matching behavior for matching IP")
	}

	noMatch := client.Originator{IP: "10.0.0.2", IsLocal: false}
	result = eng.Evaluate(nil, noMatch, nil)
	if result != nil {
		t.Errorf("expected no match for non-matching IP, got %v", result)
	}
}

func TestPolicyEngine_SetBehavior(t *testing.T) {
	eng := NewPolicyEngine()
	original := NewRetryOn403Behavior()
	replacement := NewCrushedClientBehavior()

	eng.AddPolicy("test", "originator.is_local", original, true)
	eng.SetBehavior("test", replacement)

	local := client.Originator{IP: "127.0.0.1", IsLocal: true}
	result := eng.Evaluate(nil, local, nil)
	if result == nil || result.Name() != "crushed-client-retry" {
		t.Errorf("expected crushed-client-retry behavior after SetBehavior, got %v", result)
	}
}

func TestPolicyEngine_SetEnabled(t *testing.T) {
	eng := NewPolicyEngine()
	behavior := NewRetryOn403Behavior()

	eng.AddPolicy("test", "originator.is_local", behavior, true)
	eng.SetEnabled("test", false)

	local := client.Originator{IP: "127.0.0.1", IsLocal: true}
	result := eng.Evaluate(nil, local, nil)
	if result != nil {
		t.Errorf("expected nil for disabled policy, got %v", result)
	}
}

func TestPolicyEngine_ActivePolicyCount(t *testing.T) {
	eng := NewPolicyEngine()
	behavior := NewRetryOn403Behavior()

	eng.AddPolicy("active", "originator.is_local", behavior, true)
	eng.AddPolicy("disabled", "originator.is_local", behavior, false)

	if eng.ActivePolicyCount() != 1 {
		t.Errorf("expected 1 active policy, got %d", eng.ActivePolicyCount())
	}
}

func TestPolicy_EvaluationWithClientStateNil(t *testing.T) {
	eng := NewPolicyEngine()
	behavior := NewRetryOn403Behavior()

	err := eng.AddPolicy("has-failures", "client.failure_count > 0", behavior, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	local := client.Originator{IP: "127.0.0.1", IsLocal: true}
	// Nil clientState — client.failure_count should be 0.
	result := eng.Evaluate(nil, local, nil)
	if result != nil {
		t.Errorf("expected no match for nil clientState (failure_count=0), got %v", result)
	}
}
