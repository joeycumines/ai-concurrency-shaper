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
	"fmt"
	"strings"
)

// PolicyDefinition represents a parsed policy from a CLI flag.
// The format is: name:expression:behavior:enabled
// Example: crushed-retry:originator.is_local && client.failure_count > 3:retry-on-403:true
type PolicyDefinition struct {
	Name     string
	Expr     string
	Behavior string
	Enabled  bool
}

// ParsePolicyDefinition parses a policy definition string in the format
// name:expression:behavior:enabled. The expression may contain colons
// (e.g., IPv6 addresses, ternary operators, string literals with colons).
// Parsing is performed from the right to safely isolate the simple
// boolean and behavior fields, treating everything between the first
// colon and the second-to-last colon as the CEL expression.
func ParsePolicyDefinition(s string) (*PolicyDefinition, error) {
	// Find the last colon (separates enabled from behavior+expr).
	lastColon := strings.LastIndex(s, ":")
	if lastColon < 0 {
		return nil, fmt.Errorf("invalid policy format %q: expected name:expr:behavior:enabled", s)
	}
	enabledStr := strings.TrimSpace(s[lastColon+1:])

	// Find the second-to-last colon (separates behavior from expr+name).
	secondLastColon := strings.LastIndex(s[:lastColon], ":")
	if secondLastColon < 0 {
		return nil, fmt.Errorf("invalid policy format %q: expected name:expr:behavior:enabled", s)
	}
	behaviorStr := strings.TrimSpace(s[secondLastColon+1 : lastColon])

	// The first colon separates the name from the expression.
	firstColon := strings.Index(s, ":")
	if firstColon < 0 || firstColon >= secondLastColon {
		return nil, fmt.Errorf("invalid policy format %q: expected name:expr:behavior:enabled", s)
	}

	name := strings.TrimSpace(s[:firstColon])
	if name == "" {
		return nil, fmt.Errorf("invalid policy format %q: name must not be empty", s)
	}
	expr := strings.TrimSpace(s[firstColon+1 : secondLastColon])
	if expr == "" {
		return nil, fmt.Errorf("invalid policy format %q: expression must not be empty", s)
	}
	if behaviorStr == "" {
		return nil, fmt.Errorf("invalid policy format %q: behavior must not be empty", s)
	}
	var enabledBool bool
	switch enabledStr {
	case "true", "True", "TRUE", "1", "yes", "Yes", "YES":
		enabledBool = true
	case "false", "False", "FALSE", "0", "no", "No", "NO", "":
		enabledBool = false
	default:
		return nil, fmt.Errorf("invalid policy format %q: enabled must be true or false, got %q", s, enabledStr)
	}
	return &PolicyDefinition{
		Name:     name,
		Expr:     expr,
		Behavior: behaviorStr,
		Enabled:  enabledBool,
	}, nil
}

// ResolveBehavior returns the Behavior implementation for the given
// behavior name. Returns nil if the name is not recognized.
func ResolveBehavior(name string) Behavior {
	switch name {
	case "retry-on-403":
		return NewRetryOn403Behavior()
	case "crushed-client-retry":
		return NewCrushedClientBehavior()
	default:
		return nil
	}
}
