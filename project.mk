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
