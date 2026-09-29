# Copyright (C) 2026 Joseph Cumines
#
# This program is free software: you can redistribute it and/or modify
# it under the terms of the GNU General Public License as published by
# the Free Software Foundation, either version 3 of the License, or
# (at your option) any later version.
#
# This program is distributed in the hope that it will be useful,
# but WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
# GNU General Public License for more details.
#
# You should have received a copy of the GNU General Public License
# along with this program.  If not, see <https://www.gnu.org/licenses/>.

# project.mk - project-specific configuration for the Makefile

# Exclude betteralign and grit from the default tools
GO_TOOLS ?= $(filter-out $(GO_PKG_BETTERALIGN) $(GO_PKG_GRIT),$(GO_TOOLS_DEFAULT))

# Disable betteralign targets for all modules
GO_MODULE_SLUGS_NO_BETTERALIGN ?= $(GO_MODULE_SLUGS)

# Enable deadcode targets for all modules
GO_MODULE_SLUGS_USE_DEADCODE ?= $(GO_MODULE_SLUGS)

# Use .deadcodeignore file for deadcode false-positive filtering
DEADCODE_IGNORE_PATTERNS_FILE ?= .deadcodeignore

# Treat any unignored deadcode finding as a lint error: unreachable code is a
# defect to remove, not a note to file.
DEADCODE_ERROR_ON_UNIGNORED ?= true

# Per-test-binary timeout. Go's default is 10m, which this suite does not fit
# inside: the internal/transcode package drives maxStreamTotalEvents (1<<20)
# events through a real converter, and measured 275s-507s under -race across
# runs of identical bytes on one idle 10-core machine. The top of that range
# leaves the gate one bad machine away from a spurious "test timed out", which
# is a harness artefact reported as a product failure. The test is sound and
# its event budget is the point of it, so the fix is an honest ceiling with
# headroom rather than a weakened budget. A profiling pass confirmed the cost
# is genuine per-event JSON decode plus GC (O(1) per event), not an accident
# worth optimising away.
#
# `override` is required: CI invokes `make test GO_TEST_FLAGS=-race`, and a
# command-line variable would otherwise replace this one instead of joining it.
# With `override` the flags compose into `-race -timeout=30m`. Go's flag
# parsing is last-wins and this is appended last, so GO_TEST_TIMEOUT (not a
# -timeout written into GO_TEST_FLAGS) is the way to change the ceiling.
GO_TEST_TIMEOUT ?= 30m
override GO_TEST_FLAGS += -timeout=$(GO_TEST_TIMEOUT)
