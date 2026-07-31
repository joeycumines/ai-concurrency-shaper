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

// Package policy provides CEL-based policy evaluation for
// client-aware failure handling in the proxy.
package policy

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"

	"github.com/joeycumines/ai-concurrency-shaper/internal/client"
)

// Policy represents a single CEL-based rule that maps to a specific
// behavior when the CEL expression evaluates to true for a request.
type Policy struct {
	// Name is a unique identifier for the policy.
	Name string
	// Expr is the original CEL expression string.
	Expr string
	// Program is the compiled CEL program.
	Program cel.Program
	// Behavior is the action to take when this policy matches.
	Behavior Behavior
	// Enabled controls whether the policy is active.
	Enabled bool
}

// PolicyEngine evaluates CEL policies against request context.
// It is the central component for originator-aware failure handling.
type PolicyEngine struct {
	mu       sync.RWMutex
	policies []*Policy
	env      *cel.Env
}

// NewPolicyEngine creates a PolicyEngine.
func NewPolicyEngine() *PolicyEngine {
	env, err := cel.NewEnv(
		cel.Variable("originator", cel.DynType),
		cel.Variable("request", cel.DynType),
		cel.Variable("client", cel.DynType),
		cel.Variable("response", cel.DynType),
	)
	if err != nil {
		// cel.NewEnv only returns an error for invalid option
		// configuration; this should never happen with the
		// literal options used here, but handle it defensively.
		panic(fmt.Sprintf("cel.NewEnv: %v", err))
	}
	return &PolicyEngine{
		policies: make([]*Policy, 0),
		env:      env,
	}
}

// AddPolicy compiles the given CEL expression and registers it as a policy.
// The expression has access to the following CEL variables:
//   - originator: a map with keys remote_addr, ip, pid, app_name, is_local, tls_subject
//   - request: a map with keys method, path
//   - client: a map with keys failure_count, is_crushed, last_failure
//   - response: a map with key status_code
//
// Returns an error if the expression fails to compile.
func (e *PolicyEngine) AddPolicy(name, expr string, behavior Behavior, enabled bool) error {
	ast, iss := e.env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return iss.Err()
	}
	prg, err := e.env.Program(ast)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.policies = append(e.policies, &Policy{
		Name:     name,
		Expr:     expr,
		Program:  prg,
		Behavior: behavior,
		Enabled:  enabled,
	})
	return nil
}

// SetBehavior updates the behavior for an existing policy by name.
// Returns true if the policy was found and updated, false otherwise.
func (e *PolicyEngine) SetBehavior(name string, behavior Behavior) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.policies {
		if p.Name == name {
			p.Behavior = behavior
			return true
		}
	}
	return false
}

// SetEnabled toggles whether a policy is active.
// Returns true if the policy was found and updated, false otherwise.
func (e *PolicyEngine) SetEnabled(name string, enabled bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.policies {
		if p.Name == name {
			p.Enabled = enabled
			return true
		}
	}
	return false
}

// Evaluate checks all enabled policies against the request context.
// Returns the first matching behavior, or nil if no policy matches.
func (e *PolicyEngine) Evaluate(req *http.Request, originator client.Originator, clientState *client.ClientState) Behavior {
	return e.evaluateWithResponse(req, originator, clientState, 0)
}

// EvaluateWithResponse is like Evaluate but also includes response
// information in the policy context, enabling response-aware policies.
func (e *PolicyEngine) EvaluateWithResponse(req *http.Request, originator client.Originator, clientState *client.ClientState, statusCode int) Behavior {
	return e.evaluateWithResponse(req, originator, clientState, statusCode)
}

func (e *PolicyEngine) evaluateWithResponse(req *http.Request, originator client.Originator, clientState *client.ClientState, statusCode int) Behavior {
	e.mu.RLock()
	defer e.mu.RUnlock()
	method, path := "", ""
	if req != nil {
		method = req.Method
		if req.URL != nil {
			path = req.URL.Path
		}
	}
	ctx := policyContext(originator, clientState, method, path, statusCode)
	for _, p := range e.policies {
		if !p.Enabled {
			continue
		}
		result, _, err := p.Program.Eval(ctx)
		if err != nil {
			continue
		}
		if result == nil {
			continue
		}
		if result.Type() == types.BoolType {
			if result.Value() == true {
				return p.Behavior
			}
			// Non-true boolean results (false) don't match.
			continue
		}
		// Non-boolean CEL results (strings, ints, lists, etc.) are not
		// policy matches — the expression must evaluate to boolean true.
		continue
	}
	return nil
}

// PolicyCount returns the number of registered policies.
func (e *PolicyEngine) PolicyCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.policies)
}

// ActivePolicyCount returns the number of enabled policies.
func (e *PolicyEngine) ActivePolicyCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	count := 0
	for _, p := range e.policies {
		if p.Enabled {
			count++
		}
	}
	return count
}

// policyContext returns a map for CEL evaluation with the given values.
func policyContext(o client.Originator, s *client.ClientState, method, path string, statusCode int) map[string]any {
	return map[string]any{
		"originator": map[string]any{
			"remote_addr": o.RemoteAddr,
			"ip":          o.IP,
			"pid":         o.PID,
			"app_name":    o.AppName,
			"is_local":    o.IsLocal,
			"tls_subject": o.TLSCertSubject,
		},
		"request": map[string]any{
			"method": method,
			"path":   path,
		},
		"client": clientMap(s),
		"response": map[string]any{
			"status_code": statusCode,
		},
	}
}

// clientMap converts a ClientState to a map for CEL evaluation.
func clientMap(s *client.ClientState) map[string]any {
	if s == nil {
		return map[string]any{
			"failure_count": 0,
			"is_crushed":    false,
			"last_failure":  int64(0),
		}
	}
	lastFailure := int64(0)
	if !s.LastFailure.IsZero() {
		lastFailure = s.LastFailure.Unix()
	}
	return map[string]any{
		"failure_count": s.FailureCount,
		"is_crushed":    s.IsCrushed,
		"last_failure":  lastFailure,
	}
}
