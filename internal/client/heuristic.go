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

// Package client provides originator identification and client state tracking
// for smarter failure handling in the proxy.
package client

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Heuristic identifies an application from a local process.
// Implementations use OS-specific mechanisms (e.g., /proc filesystem)
// to extract application identity from a process ID.
type Heuristic interface {
	// Name returns a unique identifier for this heuristic.
	Name() string
	// Identify attempts to identify the application for the given PID.
	// Returns the application name and true if successful, or an empty
	// string and false if the PID cannot be identified.
	Identify(pid int) (string, bool)
}

// HeuristicRegistry manages a collection of heuristics for identifying
// local applications. Heuristics are tried in registration order; the
// first match wins. The registry is safe for concurrent use.
type HeuristicRegistry struct {
	mu         sync.RWMutex
	heuristics []Heuristic
}

// NewHeuristicRegistry creates a registry with the built-in heuristics
// in a sensible default order.
func NewHeuristicRegistry() *HeuristicRegistry {
	return &HeuristicRegistry{
		heuristics: []Heuristic{
			&ProcExeName{},
			&ProcComm{},
			&ProcCmdLine{},
		},
	}
}

// Register adds a custom heuristic to the registry. It is tried in
// registration order, before built-in heuristics.
func (r *HeuristicRegistry) Register(h Heuristic) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.heuristics = append([]Heuristic{h}, r.heuristics...)
}

// Identify attempts to identify the application for the given PID
// by trying each registered heuristic in order. Returns the first
// successful identification.
func (r *HeuristicRegistry) Identify(pid int) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, h := range r.heuristics {
		if name, ok := h.Identify(pid); ok {
			return name, true
		}
	}
	return "", false
}

// ProcExeName identifies an application by resolving the symlink
// target of /proc/<pid>/exe and extracting the base name.
type ProcExeName struct{}

func (p *ProcExeName) Name() string { return "proc-exe-name" }

func (p *ProcExeName) Identify(pid int) (string, bool) {
	exePath, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return "", false
	}
	return filepath.Base(exePath), true
}

// ProcComm identifies an application by reading /proc/<pid>/comm
// which contains the process name.
type ProcComm struct{}

func (p *ProcComm) Name() string { return "proc-comm" }

func (p *ProcComm) Identify(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return "", false
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return "", false
	}
	return name, true
}

// ProcCmdLine identifies an application by reading /proc/<pid>/cmdline
// and extracting the first argument (the program name).
type ProcCmdLine struct{}

func (p *ProcCmdLine) Name() string { return "proc-cmdline" }

func (p *ProcCmdLine) Identify(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return "", false
	}
	// cmdline is null-separated; find the first null byte.
	if len(data) == 0 {
		return "", false
	}
	end := 0
	for end < len(data) && data[end] != 0 {
		end++
	}
	if end == 0 {
		return "", false
	}
	name := filepath.Base(string(data[:end]))
	if name == "" {
		return "", false
	}
	return name, true
}

// IdentifyApplication attempts to identify the application
// that owns the given PID using the provided registry. If no
// registry is provided, the default built-in registry is used.
// The default registry is reused across calls to avoid repeated
// allocation.
func IdentifyApplication(pid int, registry *HeuristicRegistry) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	if registry == nil {
		registry = defaultHeuristicRegistry()
	}
	return registry.Identify(pid)
}

var defaultRegistryOnce sync.Once
var defaultRegistry *HeuristicRegistry

func defaultHeuristicRegistry() *HeuristicRegistry {
	defaultRegistryOnce.Do(func() {
		defaultRegistry = NewHeuristicRegistry()
	})
	return defaultRegistry
}

// IdentifyApplicationReader reads /proc/<pid>/exe, /proc/<pid>/comm,
// and /proc/<pid>/cmdline using the provided reader interface for
// testability and platform abstraction. It tries ProcExeName first,
// then ProcComm, then ProcCmdLine.
type Reader interface {
	ReadFile(path string) ([]byte, error)
	Readlink(path string) (string, error)
}

// IdentifyApplicationFromReader identifies the application for a PID
// using the provided reader abstraction. This enables testing
// without real filesystem access and supports alternative
// implementations for different platforms.
func IdentifyApplicationFromReader(pid int, reader Reader) (string, bool) {
	if pid <= 0 || reader == nil {
		return "", false
	}
	// Try exe symlink first (most specific).
	exePath, err := reader.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err == nil && exePath != "" {
		return filepath.Base(exePath), true
	}
	// Try comm next.
	commData, err := reader.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err == nil {
		name := strings.TrimSpace(string(commData))
		if name != "" {
			return name, true
		}
	}
	// Fall back to cmdline.
	cmdlineData, err := reader.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err == nil && len(cmdlineData) > 0 {
		end := 0
		for end < len(cmdlineData) && cmdlineData[end] != 0 {
			end++
		}
		if end > 0 {
			name := filepath.Base(string(cmdlineData[:end]))
			if name != "" {
				return name, true
			}
		}
	}
	return "", false
}
