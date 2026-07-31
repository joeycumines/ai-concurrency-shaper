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
	"testing"
)

func TestParsePolicyDefinition_Valid(t *testing.T) {
	def, err := ParsePolicyDefinition("crushed-retry:originator.is_local && client.failure_count > 3:retry-on-403:true")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if def.Name != "crushed-retry" {
		t.Errorf("expected name crushed-retry, got %q", def.Name)
	}
	if def.Expr != "originator.is_local && client.failure_count > 3" {
		t.Errorf("unexpected expr: %q", def.Expr)
	}
	if def.Behavior != "retry-on-403" {
		t.Errorf("expected behavior retry-on-403, got %q", def.Behavior)
	}
	if !def.Enabled {
		t.Error("expected Enabled=true")
	}
}

func TestParsePolicyDefinition_InvalidFormat(t *testing.T) {
	_, err := ParsePolicyDefinition("only-name")
	if err == nil {
		t.Error("expected error for invalid format")
	}
}

func TestParsePolicyDefinition_EmptyName(t *testing.T) {
	_, err := ParsePolicyDefinition(":expression:behavior:true")
	if err == nil {
		t.Error("expected error for empty name")
	}
}

func TestParsePolicyDefinition_InvalidEnabled(t *testing.T) {
	_, err := ParsePolicyDefinition("name:expr:behavior:maybe")
	if err == nil {
		t.Error("expected error for invalid enabled value")
	}
}

func TestParsePolicyDefinition_EnabledFalse(t *testing.T) {
	def, err := ParsePolicyDefinition("name:expr:behavior:false")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if def.Enabled {
		t.Error("expected Enabled=false")
	}
}

func TestParsePolicyDefinition_EnabledVariants(t *testing.T) {
	variants := []string{"true", "True", "TRUE", "1", "yes", "Yes", "YES"}
	for _, v := range variants {
		def, err := ParsePolicyDefinition("name:expr:behavior:" + v)
		if err != nil {
			t.Errorf("unexpected error for %q: %v", v, err)
		}
		if !def.Enabled {
			t.Errorf("expected Enabled=true for %q", v)
		}
	}
}

func TestResolveBehavior_Known(t *testing.T) {
	b := ResolveBehavior("retry-on-403")
	if b == nil {
		t.Fatal("expected non-nil behavior for retry-on-403")
	}
	if b.Name() != "retry-on-403" {
		t.Errorf("expected name retry-on-403, got %q", b.Name())
	}

	b2 := ResolveBehavior("crushed-client-retry")
	if b2 == nil {
		t.Fatal("expected non-nil behavior for crushed-client-retry")
	}
	if b2.Name() != "crushed-client-retry" {
		t.Errorf("expected name crushed-client-retry, got %q", b2.Name())
	}
}

func TestResolveBehavior_Unknown(t *testing.T) {
	b := ResolveBehavior("nonexistent")
	if b != nil {
		t.Errorf("expected nil for unknown behavior, got %v", b)
	}
}
