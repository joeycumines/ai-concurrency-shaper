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

package config_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeferredVisionClauses_TypesRemainAbsent asserts that deferred/niche vision
// types (M4 client auth, G6 session affinity/stickiness, G7 client identity/LocalAddr rotation)
// remain absent from the codebase. Reintroducing any of these must be a deliberate,
// blueprint-documented design change rather than accidental drift.
func TestDeferredVisionClauses_TypesRemainAbsent(t *testing.T) {
	forbiddenTypeNames := []string{
		"AffinityConfig",
		"SessionAffinity",
		"LocalAddrPool",
		"VirtualKeyManager",
		"VirtualKey",
		"UserAgentRotator",
	}

	rootPath := filepath.Join("..", "..")
	fset := token.NewFileSet()

	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "scratch" || name == "vendor" || name == ".gemini" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}

		ast.Inspect(node, func(n ast.Node) bool {
			typeSpec, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			for _, forbidden := range forbiddenTypeNames {
				if typeSpec.Name.Name == forbidden {
					t.Errorf("found forbidden deferred type %q in %s; reintroducing this capability requires an explicit blueprint change", forbidden, path)
				}
			}
			return true
		})
		return nil
	})

	if err != nil {
		t.Fatalf("walking codebase failed: %v", err)
	}
}
