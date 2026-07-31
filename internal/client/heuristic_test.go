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

package client

import (
	"os"
	"strings"
	"testing"
)

// mockReader implements the Reader interface for testing heuristic
// identification without real filesystem access.
type mockReader struct {
	readlink func(path string) (string, error)
	readFile func(path string) ([]byte, error)
}

func (m *mockReader) Readlink(path string) (string, error) {
	if m.readlink != nil {
		return m.readlink(path)
	}
	return "", os.ErrNotExist
}

func (m *mockReader) ReadFile(path string) ([]byte, error) {
	if m.readFile != nil {
		return m.readFile(path)
	}
	return nil, os.ErrNotExist
}

func TestProcExeName_Valid(t *testing.T) {
	r := &mockReader{
		readlink: func(path string) (string, error) {
			if strings.Contains(path, "/exe") {
				return "/usr/bin/python3", nil
			}
			return "", os.ErrNotExist
		},
	}
	name, ok := IdentifyApplicationFromReader(1234, r)
	if !ok {
		t.Fatal("expected identification to succeed")
	}
	if name != "python3" {
		t.Errorf("expected python3, got %q", name)
	}
}

func TestProcExeName_MissingExe(t *testing.T) {
	r := &mockReader{
		readlink: func(path string) (string, error) {
			return "", os.ErrNotExist
		},
		readFile: func(path string) ([]byte, error) {
			if strings.Contains(path, "/comm") {
				return []byte("python\n"), nil
			}
			return nil, os.ErrNotExist
		},
	}
	name, ok := IdentifyApplicationFromReader(1234, r)
	if !ok {
		t.Fatal("expected identification to succeed via comm fallback")
	}
	if name != "python" {
		t.Errorf("expected python, got %q", name)
	}
}

func TestProcComm_Valid(t *testing.T) {
	r := &mockReader{
		readFile: func(path string) ([]byte, error) {
			if strings.Contains(path, "/comm") {
				return []byte("node\n"), nil
			}
			return nil, os.ErrNotExist
		},
	}
	name, ok := IdentifyApplicationFromReader(5678, r)
	if !ok {
		t.Fatal("expected identification to succeed")
	}
	if name != "node" {
		t.Errorf("expected node, got %q", name)
	}
}

func TestProcCmdLine_Valid(t *testing.T) {
	r := &mockReader{
		readFile: func(path string) ([]byte, error) {
			if strings.Contains(path, "/cmdline") {
				return []byte("go\x00run\x00main.go"), nil
			}
			return nil, os.ErrNotExist
		},
	}
	name, ok := IdentifyApplicationFromReader(9012, r)
	if !ok {
		t.Fatal("expected identification to succeed")
	}
	if name != "go" {
		t.Errorf("expected go, got %q", name)
	}
}

func TestIdentifyApplicationFromReader_UnknownPID(t *testing.T) {
	r := &mockReader{
		readlink: func(path string) (string, error) {
			return "", os.ErrNotExist
		},
	}
	name, ok := IdentifyApplicationFromReader(99999, r)
	if ok {
		t.Errorf("expected identification to fail for unknown PID, got %q", name)
	}
	if name != "" {
		t.Errorf("expected empty name, got %q", name)
	}
}

func TestIdentifyApplicationFromReader_NilReader(t *testing.T) {
	name, ok := IdentifyApplicationFromReader(1234, nil)
	if ok {
		t.Errorf("expected identification to fail with nil reader, got %q", name)
	}
}

func TestIdentifyApplicationFromReader_ZeroPID(t *testing.T) {
	r := &mockReader{
		readlink: func(path string) (string, error) {
			return "/usr/bin/test", nil
		},
	}
	name, ok := IdentifyApplicationFromReader(0, r)
	if ok {
		t.Errorf("expected identification to fail for PID 0, got %q", name)
	}
	if name != "" {
		t.Errorf("expected empty name, got %q", name)
	}
}

func TestHeuristicRegistry_Identify(t *testing.T) {
	reg := NewHeuristicRegistry()

	// Register a custom heuristic that always matches.
	reg.Register(&customHeuristic{name: "custom", id: "my-app"})

	name, ok := reg.Identify(1234)
	if !ok {
		t.Fatal("expected identification to succeed")
	}
	// Custom heuristics are tried first (prepended).
	if name != "my-app" {
		t.Errorf("expected my-app, got %q", name)
	}
}

type customHeuristic struct {
	name string
	id   string
}

func (h *customHeuristic) Name() string { return h.name }
func (h *customHeuristic) Identify(pid int) (string, bool) {
	return h.id, true
}

func TestIdentifyApplication_UsesDefaultRegistry(t *testing.T) {
	// On systems without /proc, this should return false.
	name, ok := IdentifyApplication(1, nil)
	// We don't assert ok or not-ok because the result depends on
	// whether /proc exists on the test system. This test just
	// verifies no panic.
	_ = name
	_ = ok
}

func TestHeuristicRegistry_ConcurrentAccess(t *testing.T) {
	reg := NewHeuristicRegistry()
	done := make(chan bool, 10)
	for range 10 {
		go func() {
			reg.Identify(1234)
			done <- true
		}()
	}
	for range 10 {
		<-done
	}
}

func TestIdentifyApplicationNilRegistry(t *testing.T) {
	// With a nil PID, should fail immediately.
	name, ok := IdentifyApplication(0, nil)
	if ok {
		t.Errorf("expected failure for PID 0, got %q", name)
	}
}
