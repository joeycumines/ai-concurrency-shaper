# MIT License
#
# Copyright (c) 2026 Joseph Cumines
#
# Permission is hereby granted, free of charge, to any person obtaining a copy
# of this software and associated documentation files (the "Software"), to deal
# in the Software without restriction, including without limitation the rights
# to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
# copies of the Software, and to permit persons to whom the Software is
# furnished to do so, subject to the following conditions:
#
# The above copyright notice and this permission notice shall be included in all
# copies or substantial portions of the Software.
#
# THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
# IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
# FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
# AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
# LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
# OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
# SOFTWARE.

# Source: https://gist.github.com/joeycumines/3352c393c1bf43df72b120ae9134168d
# Example: https://github.com/joeycumines/go-utilpkg

# Usage
# ---
# Extensible multi-module Makefile tailored for Go projects.
#
# This Makefile is designed to be used in a monorepo, where multiple Go modules
# are contained within a single repository. The implementor's primary use case
# was easing overhead of managing many _personal_ projects, as one author.
# The `grit` tool is a key factor in this, see comments in-source, for more
# details.
#
# Go Module Targets
# ---
# Each of these targets has multiple corresponding sub-targets, and with the
# primary target being to run all of them, in parallel, if enabled.
# The relative paths of modules are mapped to period-separated slugs, which may
# be used as a suffix, for any of these targets.
#
# For example:
#   make -j4 $(GO_TARGET_PREFIX)all                                  # all checks for all modules with parallelism of 4
#   make $(GO_TARGET_PREFIX)staticcheck.dir1.dir-2.modulerootdir     # staticcheck in ./dir1/dir-2/modulerootdir
#   make $(GO_TARGET_PREFIX)all.dir1.dir-2.modulerootdir             # all checks in ./dir1/dir-2/modulerootdir
#
# Sub-directory Makefiles
# ---
# In a similar vein to Go modules, Makefiles are also discovered, and exposed
# using pattern and implicit rules, to implement targets to:
#
#   1. Run the default target in the subdirectory (e.g. `make $(GO_TARGET_PREFIX)run.dir1`)
#   2. Run a specific target in the subdirectory (e.g. `make $(GO_TARGET_PREFIX)run-<target>.dir1`)
#
# Please be aware that these targets are primarily for convenience. Limitations
# exist, e.g. each of these invocations are separate, and therefore cannot
# avoid duplicated work, and may be at risk of concurrency-related problems.
#
# Customization
# ---
# The behavior of this implementation is quite configurable, e.g. commands,
# flags, and settings controlling certain behavior, are exposed, and documented
# in the source. Makefiles are also composable by nature, though global scoping
# can cause issues. Be mindful, when choosing how to integrate this Makefile.
# While no guarantees are provided, an effort to maintain compatibility has,
# and will continue, to be made, e.g. as features are added, or tweaked.
#
# Multiple customization patterns are supported, including:
#
#   1. Environment variables or command line arguments
#   2. Creating a ./config.mk (uncommitted, user-specific)
#   3. Creating a ./project.mk (committed, project-specific)
#   4. Including* this Makefile from another Makefile
#
# (*) This is the most likely to break, e.g. ROOT_MAKEFILE would likely need to
#     be set in the including Makefile, as would PROJECT_ROOT.
#
# Make Subprocesses Reevaluating This Makefile
# ---
# Beware: Some targets use `$(MAKE) ... -f $(ROOT_MAKEFILE) ...`, running in
# independent subprocesses. This is intentional but can affect behavior.
# Valid uses:
#
#   1. Target acts as a script with arguments
#   2. Optional prerequisites (not possible using order-only prerequisites)
#
# Details:
# Case 1 is simple convenience - "script" implies no dependencies.
# Case 2 enables multiple configurations/ordering (e.g., `test` standalone
# vs. after `build`). This sometimes necessitates alias targets and accepts
# known tradeoffs like isolated dependencies and potential duplicate work.

# What is `grit` and how to use it?
# ---
# Godoc: https://pkg.go.dev/github.com/joeycumines/grit
#
# Preface:
#
# The tooling provided by this Makefile allows you to publish modules to their
# own repositories, from a single monorepo. This is useful when there are
# tricky dependencies between modules, but you want to be able to publish them
# independently, manage GitHub issues independently, etc.
#
# Caveats:
#
# - Branch selection defaults to `GRIT_BRANCH`. Per-destination source and
#   target branch overrides are supported via `GRIT_SRC_BRANCH.<slug>` and
#   `GRIT_DST_BRANCH.<slug>` respectively.
# - Reverse (inbound) syncing can sync all configured destinations via
#   `grit-pull`, or a single destination via `grit-pull.<grit-dst-slug>`
#
# Configuration:
#
# `GRIT_DST` is a map of _directory_ to _destination repository_, e.g.
# `./logiface/logrus$(MAP_SEPARATOR)https://github.com/example/ilogrus.git`.
# Directories are relative to the repository root, formatted like the output
# of the module/sub-Makefile discovery, i.e. "." or "./path/to/dir". Each
# directory is converted to a grit slug, using the same convention as
# elsewhere (leading "./" removed, "/" replaced with SLUG_SEPARATOR, "." mapped
# to "root"), which is used as the target suffix, e.g. `grit.logiface.logrus`.
# N.B. Grit destinations are a wholly separate namespace: they need not be Go
# modules, and Go modules without a `GRIT_DST` entry get no grit targets.
#
# `GRIT_BRANCH` (default "main") is the fallback branch for endpoints. Optional
# `GRIT_SRC_BRANCH.<slug>` and `GRIT_DST_BRANCH.<slug>` variables override the
# source (this repository) and destination (target repository) branches.
#
# Usage:
#
# 1. Prepare the target repository (presumably the canonical one per go.mod)
# 2. Update `project.mk`, setting `GRIT_SRC` (if you haven't already) and
#   `GRIT_DST`, optionally setting `GRIT_BRANCH` (defaults to "main") and
#   per-destination branch overrides like `GRIT_SRC_BRANCH.eventloop`
# 3. Sync _from_ the target to the source (this repository) like
#   `make grit-init GRIT_INIT_TARGET=grit-dst-slug`, where grit-dst-slug is the
#   slug of the directory used as the map key, in GRIT_DST, note that you may
#   need to specify GRIT_FLAGS='-push -linearize' (see the docs)
# 4. Add the new module to the Go workspace
# 5. Run (either automatically or manually) `make grit` to sync _to_ the
#   configured target(s), to propagate
# 6. To sync existing destinations back into this repository, on demand,
#   run `make grit-pull` (or `make grit-pull.<grit-dst-slug>`)

# simple variables that either represent invariants, or need to be interacted
# with in an imperative manner, e.g. "building" values across includes, without
# separating the output of each include into its own discrete variable

# windows gnu make seems to append includes to the end of MAKEFILE_LIST
# hence the simple variable assignment, prior to any includes
ifeq ($(ROOT_MAKEFILE),)
ROOT_MAKEFILE := $(abspath $(lastword $(MAKEFILE_LIST)))
endif

# used to support changing the working directory + resolve relative paths
ifeq ($(PROJECT_ROOT),)
PROJECT_ROOT := $(patsubst %/,%,$(dir $(ROOT_MAKEFILE)))
endif

# N.B. this is a multi-platform makefile
# so far only two switching cases have been required (Windows and Unix)
ifeq ($(IS_WINDOWS),)
ifeq ($(OS),Windows_NT)
IS_WINDOWS := true
else
IS_WINDOWS := false
endif
endif

# ---

# includes, for optionally-configurable, standalone use
# N.B. The file extension .mk is somewhat standard. The file extension .mak is
# supported for historical reasons, applicable to a specific project.

# optional project-specific (committed) overrides and extensions
-include $(PROJECT_ROOT)/project.mk $(PROJECT_ROOT)/project.mak
# user-specific (uncommitted) overrides and extensions
# N.B. to use this, add the following to .gitignore: /config.mk
#      and, if you use docker, add the following to .dockerignore: config.mk
-include $(PROJECT_ROOT)/config.mk $(PROJECT_ROOT)/config.mak

# ---

# special-case post-include variables

# There is a built-in help target. This variable is to mitigate collisions.
# Setting SKIP_FURTHER_MAKEFILE_HELP=true will disable the help target.
ifeq ($(SKIP_FURTHER_MAKEFILE_HELP),)
SKIP_FURTHER_MAKEFILE_HELP := false
endif

# If set, then ALL targets except:
#  help, h, run.<./**/Makefile path as slug>, run-%.<./**/Makefile path as slug>
# will be prefixed with this value.
# If you wish to use this as part of a larger project, you might set this like:
#   GO_TARGET_PREFIX := go.
# Within your root Makefile (which would also need to set ROOT_MAKEFILE), or
# within project.mk.
ifeq ($(GO_TARGET_PREFIX),)
GO_TARGET_PREFIX :=
GO_MK_VAR_PREFIX :=
else
# Simply-expand, attempt to ensure value is set consistently, avoid re-eval.
GO_TARGET_PREFIX := $(GO_TARGET_PREFIX)
# This is a mitigation for collisions. For use in monorepo multi-lang projects.
# Only applies to certain variables, e.g. CLEAN_PATHS.
GO_MK_VAR_PREFIX := GO_
endif
# export the prefixes (normally not necessary, just for sanity)
export GO_TARGET_PREFIX
export GO_MK_VAR_PREFIX

# ---

# intended to be provided on the command line, for certain targets

# additional make flags to be used by the pattern targets like run.%, and implicit targets like run-%.<path>
# (used to run subdir makefiles)
RUN_FLAGS ?=

# determines the output of the debug-vars target
# N.B. only _defined_ variables will be present in the output
$(eval $(GO_MK_VAR_PREFIX)DEBUG_VARS ?= ROOT_MAKEFILE PROJECT_ROOT PROJECT_NAME IS_WINDOWS GO_MODULE_PATHS GO_MODULE_PATHS_EXCLUDE_PATTERNS GO_MODULE_SLUGS GO_MODULE_SLUGS_NO_PACKAGES GO_MODULE_SLUGS_EXCL_NO_PACKAGES GO_MODULE_SLUGS_NO_UPDATE GO_MODULE_SLUGS_EXCL_NO_UPDATE GO_MODULE_SLUGS_NO_STATICCHECK GO_MODULE_SLUGS_EXCL_NO_STATICCHECK GO_MODULE_SLUGS_NO_FIX GO_MODULE_SLUGS_EXCL_NO_FIX GO_PACKAGES $$(addprefix GO_PACKAGES.,$$(GO_MODULE_SLUGS)) SUBDIR_MAKEFILE_PATHS_EXCLUDE_PATTERNS SUBDIR_MAKEFILE_PATHS SUBDIR_MAKEFILE_SLUGS GO_TARGET_PREFIX MAKEFILE_TARGET_PREFIXES $$(MAKEFILE_TARGET_PREFIXES) $$(foreach v,CLEAN_PATHS ALL_TARGETS BUILD_TARGETS LINT_TARGETS VET_TARGETS STATICCHECK_TARGETS BETTERALIGN_TARGETS DEADCODE_TARGETS TEST_TARGETS COVER_TARGETS FMT_TARGETS GENERATE_TARGETS FIX_TARGETS UPDATE_TARGETS TIDY_TARGETS GO_DOC_TARGETS GRIT_TARGETS GRIT_PULL_TARGETS,$$(GO_MK_VAR_PREFIX)$$v) GRIT_SRC GRIT_DST GRIT_BRANCH $$(addprefix GRIT_SRC_BRANCH.,$$(GRIT_DST_SLUGS)) $$(addprefix GRIT_DST_BRANCH.,$$(GRIT_DST_SLUGS)) GRIT_INIT_TARGET GRIT_DST_PATHS GRIT_DST_SLUGS _GRIT_DST_MAP)

# ---

# intended to be configurable via config.mk

PROJECT_NAME ?= $(notdir $(PROJECT_ROOT))
# set (build) these to support dynamically building the help target with replacements
MAKEFILE_TARGET_PREFIXES ?=
GO ?= go
GO_FLAGS ?=
GO_TEST_FLAGS ?=
GO_PACKAGES ?= ./...
# callable variables, with param $1 being a go module slug (see go_module_slug_to_path)
# N.B. empty values are treated as unset: an explicitly empty GO_PACKAGES or
# GO_PACKAGES.<slug> falls back to ./..., rather than invoking the tools with no
# package patterns (which would silently limit them to the module root)
go_module_slug_to_packages = $(or $(GO_PACKAGES.$1),$(GO_PACKAGES),./...)
GO_TEST ?= $(GO) -C $(call go_module_slug_to_path,$1) test $(GO_FLAGS) $(GO_TEST_FLAGS)
GO_BUILD ?= $(GO) -C $(call go_module_slug_to_path,$1) build $(GO_FLAGS)
GO_VET ?= $(GO) -C $(call go_module_slug_to_path,$1) vet $(GO_FLAGS)
# simple command variables
GO_FMT ?= $(GO) fmt
GO_GENERATE ?= $(GO) generate
GO_FIX ?= $(GO) fix
GO_COVERAGE_MODULE_FILE ?= coverage.out
GO_COVERAGE_ALL_MODULES_FILE ?= coverage-all.out
GO_TOOL_COVER ?= $(GO) tool cover
GO_DOC_FLAGS ?= -all
GRIT ?= $(GO) tool $(GO_PKG_GRIT)
GRIT_FLAGS ?= -push
GRIT_PULL_FLAGS ?= $(GRIT_FLAGS)
GRIT_BRANCH ?= main
GRIT_SRC ?=
# Map of _directory_ to _destination repository_, e.g.
# `./logiface/logrus$(MAP_SEPARATOR)https://github.com/example/ilogrus.git`.
# Directories are formatted like the output of the module/sub-Makefile
# discovery, i.e. "." or "./path/to/dir" - see the grit usage docs above.
# N.B. Grit destinations are independent of Go module and sub-Makefile
# discovery - the directory need not be a Go module.
GRIT_DST ?=
# A grit destination slug, i.e. an entry in GRIT_DST_SLUGS, for grit-init
GRIT_INIT_TARGET ?=
# callable variable, with param $1 being a grit destination slug (see grit_dst_slug_to_path)
GRIT_MODULE_COMMAND ?= $(GRIT) $(GRIT_FLAGS) $(call grit_dst_slug_to_local,$1) $(call grit_dst_slug_to_remote,$1)
# callable variable, with param $1 being a grit destination slug (see grit_dst_slug_to_path)
GRIT_PULL_MODULE_COMMAND ?= $(GRIT) $(GRIT_PULL_FLAGS) $(call grit_dst_slug_to_remote,$1) $(call grit_dst_slug_to_local,$1)
STATICCHECK ?= $(call go_tool_binary_path,$(GO_PKG_STATICCHECK))
STATICCHECK_FLAGS ?=
BETTERALIGN ?= $(call go_tool_binary_path,$(GO_PKG_BETTERALIGN))
BETTERALIGN_FLAGS ?=
DEADCODE ?= $(if $(or $(filter true,$(DEADCODE_ERROR_ON_UNIGNORED)),$(and $(DEADCODE_IGNORE_PATTERNS_FILE),$(wildcard $(DEADCODE_IGNORE_PATTERNS_FILE)))),$(call go_tool_binary_path,$(GO_PKG_SIMPLE_COMMAND_OUTPUT_FILTER)) -v $(and $(filter true,$(DEADCODE_ERROR_ON_UNIGNORED)),-e on-content) $(addprefix -f ,$(and $(DEADCODE_IGNORE_PATTERNS_FILE),$(wildcard $(DEADCODE_IGNORE_PATTERNS_FILE)))) -- ,)$(call go_tool_binary_path,$(GO_PKG_DEADCODE))
DEADCODE_FLAGS ?=
# N.B. If set, by default, used with simple-command-output-filter to exclude false-positives.
# Contains glob-like patterns to excluded lines from the deadcode output. See that tool's docs:
#   https://pkg.go.dev/github.com/joeycumines/simple-command-output-filter#section-readme
# The file's path is relative to the module root, where the ignores apply.
DEADCODE_IGNORE_PATTERNS_FILE ?= # .deadcodeignore
# If set to true then, by default, the simple-command-output-filter tool will
# be used to treat any detected deadcode, not otherwise ignored, as an error.
DEADCODE_ERROR_ON_UNIGNORED ?= false
# for the tools target, to update the root go.mod (only relevant when setting up or updating this makefile)
GO_TOOLS ?= $(GO_TOOLS_DEFAULT)
# Used to prune _paths_ when searching for go modules. Single wildcard (%) supported. May match intermediate directories.
# Example: %/vendor %/node_modules ./managed-separately
GO_MODULE_PATHS_EXCLUDE_PATTERNS ?=
# used to special-case modules for tools which fail if they find no packages (e.g. go vet)
GO_MODULE_SLUGS_NO_PACKAGES ?=
# used to exclude modules from the update* targets
GO_MODULE_SLUGS_NO_UPDATE ?=
# used to exclude modules from the betteralign targets
GO_MODULE_SLUGS_NO_BETTERALIGN ?=
# used to exclude modules from the staticcheck targets
GO_MODULE_SLUGS_NO_STATICCHECK ?=
# used to exclude modules from the fix targets
GO_MODULE_SLUGS_NO_FIX ?=
# used to include modules in the deadcode targets
GO_MODULE_SLUGS_USE_DEADCODE ?=
# Used to prune _paths_ when searching for nested Makefiles. Single wildcard (%) supported. May match intermediate directories.
# Example: %/vendor %/node_modules ./managed-separately
SUBDIR_MAKEFILE_PATHS_EXCLUDE_PATTERNS ?=

# configurable, but unlikely to need to be configured

# separates keys and values, see also the map_* functions
MAP_SEPARATOR ?= :
# path separator (/ replacement) for slugs
SLUG_SEPARATOR ?= .
GO_TOOLS_DEFAULT ?= \
		$(GO_PKG_BETTERALIGN) \
		$(GO_PKG_GRIT) \
		$(GO_PKG_STATICCHECK) \
		$(if $(GO_MODULE_SLUGS_USE_DEADCODE),$(GO_PKG_DEADCODE) $(if $(or $(filter true,$(DEADCODE_ERROR_ON_UNIGNORED)),$(DEADCODE_IGNORE_PATTERNS_FILE)),$(GO_PKG_SIMPLE_COMMAND_OUTPUT_FILTER),),)
GO_PKG_BETTERALIGN ?= github.com/dkorunic/betteralign/cmd/betteralign
GO_PKG_GRIT ?= github.com/joeycumines/grit
GO_PKG_STATICCHECK ?= honnef.co/go/tools/cmd/staticcheck
GO_PKG_DEADCODE ?= golang.org/x/tools/cmd/deadcode
GO_PKG_SIMPLE_COMMAND_OUTPUT_FILTER ?= github.com/joeycumines/simple-command-output-filter
# paths to be deleted on clean - use $($(GO_MK_VAR_PREFIX)CLEAN_PATHS) to get
$(eval $(GO_MK_VAR_PREFIX)CLEAN_PATHS ?= $$(GO_COVERAGE_ALL_MODULES_FILE) $$(addsuffix /$$(GO_COVERAGE_MODULE_FILE),$$(GO_MODULE_PATHS)))

# ---

# Recursive wildcard match function, with support for optional pruning.
#
# Signature: $(call rwildcard,<dir>,<pattern>,[filter-out])
#
# $1: directory to search (requires a trailing slash, e.g., "src/")
# $2: pattern to match (e.g., "*.go")
# $3: (optional) A whitespace-separated list of patterns to $(filter-out ...).
#     Applied to both files and directories, "guarding" further recursion.
rwildcard = \
$(call _rwildcard_filter_out,$(wildcard $1$2),$3) \
$(foreach d,\
$(call _rwildcard_filter_out,$(patsubst %/./,%,$(wildcard $1*/./)),$3),\
$(call rwildcard,$d/,$2,$3)\
)
_rwildcard_filter_out = $(if $2,$(filter-out $2,$1),$1)

# looks up a value in a map, $1 is the map, $2 is the key associated with the value
map_value_by_key = $(patsubst $2$(MAP_SEPARATOR)%,%,$(filter $2$(MAP_SEPARATOR)%,$1))
# looks up a key in a map, $1 is the map, $2 is the value associated with the key
map_key_by_value = $(patsubst %$(MAP_SEPARATOR)$2,%,$(filter %$(MAP_SEPARATOR)$2,$1))
# builds a new map, from a set of keys, using a transform function to build values from the keys
# $1 are the keys, $2 is the transform function
map_transform_keys = $(foreach v,$1,$v$(MAP_SEPARATOR)$(call $2,$v))
# extracts only the keys from a map variable, $1 is the map variable
map_keys = $(foreach v,$1,$(word 1,$(subst $(MAP_SEPARATOR), ,$v)))

# convert a path to a slug, e.g. ./logiface/logrus -> logiface.logrus, with special case for root
slug_transform = $(if $(filter .,$1),root,$(subst /,$(SLUG_SEPARATOR),$(patsubst ./%,%,$1)))
# attempts to perform the opposite of slug_parse, note that it may not be possible to recover the original path
slug_parse = $(if $(filter root,$1),$(SLUG_SEPARATOR),$(SLUG_SEPARATOR)/$(subst $(SLUG_SEPARATOR),/,$(filter-out root,$1)))

# escaping for use in recipies, e.g.: echo $(call escape_command_arg,$(MESSAGE))
# WARNING: you may get unexpected results under windows, e.g. if MESSAGE is empty, in the above example
# N.B. the cmd.exe-style escaping above is only correct when make.exe routes the
# recipe to cmd.exe. Recipes that also pass a quoted argument (e.g.
# "GO_PACKAGES=$(call go_module_slug_to_packages,$*)") are routed to sh.exe on
# the Windows port when it is available, where the ^-escapes are wrong - on
# Windows with sh.exe installed, avoid shell metacharacters (& | < > ^ %) in
# *_FLAGS values
ifeq ($(IS_WINDOWS),true)
escape_command_arg ?= $(strip $(subst %,%%,$(subst |,^|,$(subst >,^>,$(subst <,^<,$(subst &,^&,$(subst ^,^^,$1)))))))
else
escape_command_arg ?= '$(subst ','\'',$1)'
endif

# includes workaround for https://github.com/golang/go/issues/72824
# (the workaround is running go tool -n _twice_)
#
# `go tool -n` prints the tool path in native format: backslash-separated on
# Windows. Normalize to forward slashes so recipes still work when make.exe
# routes them through a POSIX shell (e.g. the sh.exe bundled with MSYS2 or Git
# Bash, which make.exe uses whenever the recipe contains shell metacharacters):
# a backslash path would have every separator eaten as a shell escape.
# CreateProcess accepts forward slashes in executable paths, so direct
# execution via cmd.exe is unaffected.
go_tool_binary_path = $(if $(shell $(GO) tool -C $(PROJECT_ROOT) -n $1),$(subst \,/,$(shell $(GO) tool -C $(PROJECT_ROOT) -n $1)),$(error no go tool found for $1))

go_module_path_to_slug = $(call map_value_by_key,$(_GO_MODULE_MAP),$1)
go_module_slug_to_path = $(call map_key_by_value,$(_GO_MODULE_MAP),$1)

subdir_makefile_path_to_slug = $(call map_value_by_key,$(_SUBDIR_MAKEFILE_MAP),$1)
subdir_makefile_slug_to_path = $(call map_key_by_value,$(_SUBDIR_MAKEFILE_MAP),$1)

# the prefix used to sync a grit destination directory, e.g. "" for the
# repository root, or "logiface/" for ./logiface
grit_dst_path_to_prefix = $(if $(filter .,$1),,$(patsubst ./%,%,$1)/)
# conversion between grit destination directories and slugs, e.g. . -> root,
# and ./logiface/logrus -> logiface.logrus (same convention as go modules)
grit_dst_path_to_slug = $(call map_value_by_key,$(_GRIT_DST_MAP),$1)
grit_dst_slug_to_path = $(call map_key_by_value,$(_GRIT_DST_MAP),$1)
# normalized lookup that fails loudly for unknown slugs
grit_dst_slug_to_path_or_error = $(or $(call grit_dst_slug_to_path,$1),$(error not a configured grit destination slug: $1))
# branch selection for a grit destination slug; undefined falls back to GRIT_BRANCH
grit_dst_slug_to_src_branch = $(if $(filter undefined,$(origin GRIT_SRC_BRANCH.$1)),$(GRIT_BRANCH),$(or $(strip $(GRIT_SRC_BRANCH.$1)),$(error GRIT_SRC_BRANCH.$1 must not be empty)))
grit_dst_slug_to_dst_branch = $(if $(filter undefined,$(origin GRIT_DST_BRANCH.$1)),$(GRIT_BRANCH),$(or $(strip $(GRIT_DST_BRANCH.$1)),$(error GRIT_DST_BRANCH.$1 must not be empty)))
# the destination repository for a grit destination slug
grit_dst_slug_to_repo = $(or $(call map_value_by_key,$(GRIT_DST),$(call grit_dst_slug_to_path_or_error,$1)),$(error no GRIT_DST entry for grit destination slug: $1))
# the source (this repository), and target (the destination repository), as
# grit endpoints, for a grit destination slug
grit_dst_slug_to_local = $(GRIT_SRC),$(call grit_dst_path_to_prefix,$(call grit_dst_slug_to_path_or_error,$1)),$(call grit_dst_slug_to_src_branch,$1)
grit_dst_slug_to_remote = $(call grit_dst_slug_to_repo,$1),,$(call grit_dst_slug_to_dst_branch,$1)

# paths formatted like ". ./logiface ./logiface/logrus ./logiface/testsuite ./logiface/zerolog"
GO_MODULE_PATHS := $(patsubst %/go.mod,%,$(call rwildcard,./,go.mod,$(GO_MODULE_PATHS_EXCLUDE_PATTERNS)))
# used by go_module_path_to_slug and go_module_slug_to_path to lookup an associated path/slug
_GO_MODULE_MAP := $(call map_transform_keys,$(GO_MODULE_PATHS),slug_transform)
# example: root logiface logiface.logrus logiface.testsuite logiface.zerolog
GO_MODULE_SLUGS := $(foreach d,$(GO_MODULE_PATHS),$(call go_module_path_to_slug,$d))
# sanity check the path and slug lookups
ifneq ($(GO_MODULE_PATHS),$(foreach d,$(GO_MODULE_SLUGS),$(call go_module_slug_to_path,$d)))
$(error GO_MODULE_PATHS contains unsupported paths)
endif
ifneq ($(GO_MODULE_SLUGS),$(foreach d,$(GO_MODULE_PATHS),$(call go_module_path_to_slug,$d)))
$(error GO_MODULE_SLUGS contains unsupported paths)
endif
GO_MODULE_SLUGS_EXCL_NO_PACKAGES := $(filter-out $(GO_MODULE_SLUGS_NO_PACKAGES),$(GO_MODULE_SLUGS))
GO_MODULE_SLUGS_EXCL_NO_UPDATE := $(filter-out $(GO_MODULE_SLUGS_NO_UPDATE),$(GO_MODULE_SLUGS))
GO_MODULE_SLUGS_EXCL_NO_BETTERALIGN := $(filter-out $(GO_MODULE_SLUGS_NO_BETTERALIGN),$(GO_MODULE_SLUGS_EXCL_NO_PACKAGES))
# because GO_MODULE_SLUGS_EXCL_NO_BETTERALIGN is composite (with no packages), and we need a target for _all_ modules
GO_MODULE_SLUGS_INCL_NO_BETTERALIGN := $(filter-out $(GO_MODULE_SLUGS_EXCL_NO_BETTERALIGN),$(GO_MODULE_SLUGS))
# because GO_MODULE_SLUGS_EXCL_NO_STATICCHECK is composite (with no packages and no staticcheck), and we need a target for _all_ modules
# N.B. modules in GO_MODULE_SLUGS_NO_PACKAGES and/or GO_MODULE_SLUGS_NO_STATICCHECK
# get an empty no-op staticcheck.<slug> target - the aggregate staticcheck
# target silently skips them (parity with the betteralign no-op targets)
GO_MODULE_SLUGS_EXCL_NO_STATICCHECK := $(filter-out $(GO_MODULE_SLUGS_NO_STATICCHECK),$(GO_MODULE_SLUGS_EXCL_NO_PACKAGES))
GO_MODULE_SLUGS_INCL_NO_STATICCHECK := $(filter-out $(GO_MODULE_SLUGS_EXCL_NO_STATICCHECK),$(GO_MODULE_SLUGS))
# because GO_MODULE_SLUGS_EXCL_NO_FIX is composite (with no packages and no fix), and we need a target for _all_ modules
# N.B. modules in GO_MODULE_SLUGS_NO_PACKAGES and/or GO_MODULE_SLUGS_NO_FIX
# get an empty no-op fix.<slug> target - the aggregate fix target silently
# skips them (parity with the betteralign no-op targets)
GO_MODULE_SLUGS_EXCL_NO_FIX := $(filter-out $(GO_MODULE_SLUGS_NO_FIX),$(GO_MODULE_SLUGS_EXCL_NO_PACKAGES))
GO_MODULE_SLUGS_INCL_NO_FIX := $(filter-out $(GO_MODULE_SLUGS_EXCL_NO_FIX),$(GO_MODULE_SLUGS))
GO_MODULE_SLUGS_INCL_USE_DEADCODE := $(filter $(GO_MODULE_SLUGS_USE_DEADCODE),$(GO_MODULE_SLUGS_EXCL_NO_PACKAGES))
# because GO_MODULE_SLUGS_INCL_USE_DEADCODE is composite (with no packages), and we need a target for _all_ modules
GO_MODULE_SLUGS_EXCL_USE_DEADCODE := $(filter-out $(GO_MODULE_SLUGS_INCL_USE_DEADCODE),$(GO_MODULE_SLUGS))

# paths for the grit destinations, formatted like GO_MODULE_PATHS, i.e. "." or
# "./path/to/dir", e.g. ". ./logiface ./logiface/logrus"
GRIT_DST_PATHS := $(call map_keys,$(GRIT_DST))
# used by grit_dst_path_to_slug and grit_dst_slug_to_path to lookup an
# associated path/slug
_GRIT_DST_MAP := $(call map_transform_keys,$(GRIT_DST_PATHS),slug_transform)
# slugs for the grit destinations, without a leading ./, / replaced with ., and
# the path . mapped to root
GRIT_DST_SLUGS := $(foreach d,$(GRIT_DST_PATHS),$(call grit_dst_path_to_slug,$d))
# sanity checks for GRIT_DST, note that these are _intentionally_ strict, and
# will fail the build, since otherwise the grit targets would just silently
# no-op (or worse), e.g. if a key were to be renamed
ifneq ($(filter-out . $(foreach d,$(GRIT_DST_PATHS),$(if $(filter ./%,$d),$d)),$(GRIT_DST_PATHS)),)
$(error GRIT_DST keys must be "." or "./"-prefixed directories, without a trailing slash, got: $(filter-out . $(foreach d,$(GRIT_DST_PATHS),$(if $(filter ./%,$d),$d)),$(GRIT_DST_PATHS)) - to migrate from the old module-slug format, use the module path, e.g. ./logiface/logrus)
endif
ifneq ($(filter %/,$(GRIT_DST_PATHS)),)
$(error GRIT_DST keys must not have a trailing slash, got: $(filter %/,$(GRIT_DST_PATHS)) - e.g. use ./logiface/logrus rather than ./logiface/logrus/)
endif
ifneq ($(or $(filter %/..,$(GRIT_DST_PATHS)),$(findstring /../,$(GRIT_DST_PATHS))),)
$(error GRIT_DST keys must not contain ".." path components, got: $(strip $(foreach d,$(GRIT_DST_PATHS),$(if $(or $(filter %/..,$d),$(findstring /../,$d)),$d))) - e.g. use ./logiface/logrus rather than ./logiface/../logiface/logrus)
endif
# N.B. the predicates below are deliberately list-level (like the ".." check
# above): `$(findstring ...)` on the joined list and `$(filter %/.,...)` are
# safe because keys never contain spaces, so // and /. can only occur within a
# single key. Do NOT use a $(foreach ...) here - GNU Make joins foreach
# iterations with spaces, so N empty iterations expand to N-1 spaces (non-empty)
ifneq ($(or $(findstring //,$(GRIT_DST_PATHS)),$(findstring /./,$(GRIT_DST_PATHS)),$(filter %/.,$(GRIT_DST_PATHS))),)
$(error GRIT_DST keys must not contain empty, "." or ".." path components, got: $(strip $(foreach d,$(GRIT_DST_PATHS),$(if $(or $(findstring //,$d),$(findstring /./,$d),$(filter %/.,$d)),$d))) - e.g. use ./logiface/logrus rather than ./logiface//logrus, ./logiface/./logrus, or ./logiface/logrus/.)
endif
# sanity check the slug lookups, i.e. that the paths can be recovered from the
# slugs (detects duplicates, e.g. due to SLUG_SEPARATOR != .)
ifneq ($(GRIT_DST_PATHS),$(foreach d,$(GRIT_DST_SLUGS),$(call grit_dst_slug_to_path,$d)))
$(error GRIT_DST contains duplicate grit destination slugs, got: $(strip $(foreach s,$(GRIT_DST_SLUGS),$(if $(filter-out 1,$(words $(call grit_dst_slug_to_path,$s))),$s))))
endif
# check that all destinations are configured, i.e. with a value
$(foreach d,$(GRIT_DST_PATHS),$(if $(call map_value_by_key,$(GRIT_DST),$d),,$(error missing GRIT_DST destination repository for $d)))
$(if $(strip $(GRIT_BRANCH)),,$(error GRIT_BRANCH must not be empty))

# subdirectories which contain a file named "Makefile", formatted with a leading ".", and no trailing slash
# note that the root Makefile (this file) is excluded
SUBDIR_MAKEFILE_PATHS := $(filter-out .,$(patsubst %/Makefile,%,$(call rwildcard,./,Makefile,$(SUBDIR_MAKEFILE_PATHS_EXCLUDE_PATTERNS))))
# used by subdir_makefile_path_to_slug and subdir_makefile_slug_to_path to lookup an associated path/slug
_SUBDIR_MAKEFILE_MAP := $(call map_transform_keys,$(SUBDIR_MAKEFILE_PATHS),slug_transform)
# slugs for subdirectories, without a leading ./, / replaced with ., and the path . mapped to root
SUBDIR_MAKEFILE_SLUGS := $(foreach d,$(SUBDIR_MAKEFILE_PATHS),$(call subdir_makefile_path_to_slug,$d))

# sanity check the path and slug lookups
ifneq ($(SUBDIR_MAKEFILE_PATHS),$(foreach d,$(SUBDIR_MAKEFILE_SLUGS),$(call subdir_makefile_slug_to_path,$d)))
$(error SUBDIR_MAKEFILE_PATHS contains unsupported paths)
endif
ifneq ($(SUBDIR_MAKEFILE_SLUGS),$(foreach d,$(SUBDIR_MAKEFILE_PATHS),$(call subdir_makefile_path_to_slug,$d)))
$(error SUBDIR_MAKEFILE_SLUGS contains unsupported paths)
endif

# ---

##@ Standard Targets

.PHONY: $(GO_TARGET_PREFIX)all
$(GO_TARGET_PREFIX)all: ## Builds all, and (non-standard per GNU) runs all checks.
	@

.PHONY: $(GO_TARGET_PREFIX)clean
$(GO_TARGET_PREFIX)clean: ## Cleans up outputs of other targets, e.g. removing coverage files.
ifeq ($(IS_WINDOWS),true)
	del /Q /S $(subst /,\,$($(GO_MK_VAR_PREFIX)CLEAN_PATHS))
else
	rm -rf $($(GO_MK_VAR_PREFIX)CLEAN_PATHS)
endif

# ---

##@ Go Module Targets

# all, all.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)ALL_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)all.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)all
$(GO_TARGET_PREFIX)all: $($(GO_MK_VAR_PREFIX)ALL_TARGETS) ## Builds, then lints and tests (modules in parallel, two stages).

.PHONY: $($(GO_MK_VAR_PREFIX)ALL_TARGETS)
$($(GO_MK_VAR_PREFIX)ALL_TARGETS): $(GO_TARGET_PREFIX)all.%: $(GO_TARGET_PREFIX)_all__build.% $(GO_TARGET_PREFIX)_all__lint.% $(GO_TARGET_PREFIX)_all__test.%

.PHONY: $(addprefix $(GO_TARGET_PREFIX)_all__build.,$(GO_MODULE_SLUGS))
$(addprefix $(GO_TARGET_PREFIX)_all__build.,$(GO_MODULE_SLUGS)): $(GO_TARGET_PREFIX)_all__build.%:
	@$(MAKE) --no-print-directory $(GO_TARGET_PREFIX)build.$*

.PHONY: $(addprefix $(GO_TARGET_PREFIX)_all__lint.,$(GO_MODULE_SLUGS))
$(addprefix $(GO_TARGET_PREFIX)_all__lint.,$(GO_MODULE_SLUGS)): $(GO_TARGET_PREFIX)_all__lint.%: $(GO_TARGET_PREFIX)_all__build.%
	@$(MAKE) --no-print-directory $(GO_TARGET_PREFIX)lint.$*

.PHONY: $(addprefix $(GO_TARGET_PREFIX)_all__test.,$(GO_MODULE_SLUGS))
$(addprefix $(GO_TARGET_PREFIX)_all__test.,$(GO_MODULE_SLUGS)): $(GO_TARGET_PREFIX)_all__test.%: $(GO_TARGET_PREFIX)_all__build.%
	@$(MAKE) --no-print-directory $(GO_TARGET_PREFIX)test.$*

# build, build.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)BUILD_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)build.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)build
$(GO_TARGET_PREFIX)build: $($(GO_MK_VAR_PREFIX)BUILD_TARGETS) ## Runs the go build tool.

.PHONY: $($(GO_MK_VAR_PREFIX)BUILD_TARGETS)
$($(GO_MK_VAR_PREFIX)BUILD_TARGETS): $(GO_TARGET_PREFIX)build.%:
	$(call GO_BUILD,$*) $(call go_module_slug_to_packages,$*)

# lint, lint.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)LINT_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)lint.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)lint
$(GO_TARGET_PREFIX)lint: $($(GO_MK_VAR_PREFIX)LINT_TARGETS) ## Runs the vet, staticcheck, betteralign, and deadcode targets.

.PHONY: $($(GO_MK_VAR_PREFIX)LINT_TARGETS)
$($(GO_MK_VAR_PREFIX)LINT_TARGETS): $(GO_TARGET_PREFIX)lint.%: $(GO_TARGET_PREFIX)vet.% $(GO_TARGET_PREFIX)staticcheck.% $(GO_TARGET_PREFIX)betteralign.% $(GO_TARGET_PREFIX)deadcode.%

# vet, vet.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)VET_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)vet.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)vet
$(GO_TARGET_PREFIX)vet: $($(GO_MK_VAR_PREFIX)VET_TARGETS) ## Runs the go vet tool.

.PHONY: $(addprefix $(GO_TARGET_PREFIX)vet.,$(GO_MODULE_SLUGS_EXCL_NO_PACKAGES))
$(addprefix $(GO_TARGET_PREFIX)vet.,$(GO_MODULE_SLUGS_EXCL_NO_PACKAGES)): $(GO_TARGET_PREFIX)vet.%:
	$(call GO_VET,$*) $(call go_module_slug_to_packages,$*)

.PHONY: $(addprefix $(GO_TARGET_PREFIX)vet.,$(GO_MODULE_SLUGS_NO_PACKAGES))
$(addprefix $(GO_TARGET_PREFIX)vet.,$(GO_MODULE_SLUGS_NO_PACKAGES)): $(GO_TARGET_PREFIX)vet.%:

# staticcheck, staticcheck.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)STATICCHECK_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)staticcheck.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)staticcheck
$(GO_TARGET_PREFIX)staticcheck: $($(GO_MK_VAR_PREFIX)STATICCHECK_TARGETS) ## Runs the staticcheck tool.

.PHONY: $(addprefix $(GO_TARGET_PREFIX)staticcheck.,$(GO_MODULE_SLUGS_EXCL_NO_STATICCHECK))
$(addprefix $(GO_TARGET_PREFIX)staticcheck.,$(GO_MODULE_SLUGS_EXCL_NO_STATICCHECK)): $(GO_TARGET_PREFIX)staticcheck.%:
	$(MAKE) -s -C $(call go_module_slug_to_path,$*) -f $(ROOT_MAKEFILE) $(GO_TARGET_PREFIX)_staticcheck STATICCHECK_FLAGS=$(call escape_command_arg,$(STATICCHECK_FLAGS)) "GO_PACKAGES=$(call go_module_slug_to_packages,$*)"

.PHONY: $(addprefix $(GO_TARGET_PREFIX)staticcheck.,$(GO_MODULE_SLUGS_INCL_NO_STATICCHECK))
$(addprefix $(GO_TARGET_PREFIX)staticcheck.,$(GO_MODULE_SLUGS_INCL_NO_STATICCHECK)): $(GO_TARGET_PREFIX)staticcheck.%:

.PHONY: $(GO_TARGET_PREFIX)_staticcheck
$(GO_TARGET_PREFIX)_staticcheck:
	$(STATICCHECK) $(STATICCHECK_FLAGS) $(GO_PACKAGES)

# betteralign, betteralign.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)BETTERALIGN_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)betteralign.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)betteralign
$(GO_TARGET_PREFIX)betteralign: $($(GO_MK_VAR_PREFIX)BETTERALIGN_TARGETS) ## Runs the betteralign tool.

.PHONY: $(addprefix $(GO_TARGET_PREFIX)betteralign.,$(GO_MODULE_SLUGS_EXCL_NO_BETTERALIGN))
$(addprefix $(GO_TARGET_PREFIX)betteralign.,$(GO_MODULE_SLUGS_EXCL_NO_BETTERALIGN)): $(GO_TARGET_PREFIX)betteralign.%:
	$(MAKE) -s -C $(call go_module_slug_to_path,$*) -f $(ROOT_MAKEFILE) $(GO_TARGET_PREFIX)_betteralign BETTERALIGN_FLAGS=$(call escape_command_arg,$(BETTERALIGN_FLAGS)) "GO_PACKAGES=$(call go_module_slug_to_packages,$*)"

.PHONY: $(addprefix $(GO_TARGET_PREFIX)betteralign.,$(GO_MODULE_SLUGS_INCL_NO_BETTERALIGN))
$(addprefix $(GO_TARGET_PREFIX)betteralign.,$(GO_MODULE_SLUGS_INCL_NO_BETTERALIGN)): $(GO_TARGET_PREFIX)betteralign.%:

.PHONY: $(GO_TARGET_PREFIX)_betteralign
$(GO_TARGET_PREFIX)_betteralign:
	$(BETTERALIGN) $(BETTERALIGN_FLAGS) $(GO_PACKAGES)

# deadcode, deadcode.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)DEADCODE_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)deadcode.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)deadcode
$(GO_TARGET_PREFIX)deadcode: $($(GO_MK_VAR_PREFIX)DEADCODE_TARGETS) ## Runs the deadcode tool.

.PHONY: $(addprefix $(GO_TARGET_PREFIX)deadcode.,$(GO_MODULE_SLUGS_INCL_USE_DEADCODE))
$(addprefix $(GO_TARGET_PREFIX)deadcode.,$(GO_MODULE_SLUGS_INCL_USE_DEADCODE)): $(GO_TARGET_PREFIX)deadcode.%:
	$(MAKE) -s -C $(call go_module_slug_to_path,$*) -f $(ROOT_MAKEFILE) $(GO_TARGET_PREFIX)_deadcode DEADCODE_FLAGS=$(call escape_command_arg,$(DEADCODE_FLAGS)) "GO_PACKAGES=$(call go_module_slug_to_packages,$*)"

.PHONY: $(addprefix $(GO_TARGET_PREFIX)deadcode.,$(GO_MODULE_SLUGS_EXCL_USE_DEADCODE))
$(addprefix $(GO_TARGET_PREFIX)deadcode.,$(GO_MODULE_SLUGS_EXCL_USE_DEADCODE)): $(GO_TARGET_PREFIX)deadcode.%:

.PHONY: $(GO_TARGET_PREFIX)_deadcode
$(GO_TARGET_PREFIX)_deadcode:
	$(DEADCODE) $(DEADCODE_FLAGS) $(GO_PACKAGES)

# test, test.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)TEST_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)test.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)test
$(GO_TARGET_PREFIX)test: $($(GO_MK_VAR_PREFIX)TEST_TARGETS) ## Runs the go test tool.

.PHONY: $(addprefix $(GO_TARGET_PREFIX)test.,$(GO_MODULE_SLUGS_EXCL_NO_PACKAGES))
$(addprefix $(GO_TARGET_PREFIX)test.,$(GO_MODULE_SLUGS_EXCL_NO_PACKAGES)): $(GO_TARGET_PREFIX)test.%:
	$(call GO_TEST,$*) $(call go_module_slug_to_packages,$*)

.PHONY: $(addprefix $(GO_TARGET_PREFIX)test.,$(GO_MODULE_SLUGS_NO_PACKAGES))
$(addprefix $(GO_TARGET_PREFIX)test.,$(GO_MODULE_SLUGS_NO_PACKAGES)): $(GO_TARGET_PREFIX)test.%:

# cover, cover.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)COVER_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)cover.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)cover
$(GO_TARGET_PREFIX)cover: $($(GO_MK_VAR_PREFIX)COVER_TARGETS) ## Runs the go test tool with -covermode=count and generates a coverage report.
	echo mode: count >$(GO_COVERAGE_ALL_MODULES_FILE)
	$(foreach d,$(GO_MODULE_SLUGS_EXCL_NO_PACKAGES),$(cover__TEMPLATE))
	$(GO_TOOL_COVER) -html=$(GO_COVERAGE_ALL_MODULES_FILE)
ifeq ($(IS_WINDOWS),true)
define cover__TEMPLATE =
type $(subst /,\,$(call go_module_slug_to_path,$d)/$(GO_COVERAGE_MODULE_FILE)) | more +1 | findstr /v /r "^$$" >>$(GO_COVERAGE_ALL_MODULES_FILE)

endef
else
define cover__TEMPLATE =
tail -n +2 $(call go_module_slug_to_path,$d)/$(GO_COVERAGE_MODULE_FILE) >>$(GO_COVERAGE_ALL_MODULES_FILE)

endef
endif

.PHONY: $(addprefix $(GO_TARGET_PREFIX)cover.,$(GO_MODULE_SLUGS_EXCL_NO_PACKAGES))
$(addprefix $(GO_TARGET_PREFIX)cover.,$(GO_MODULE_SLUGS_EXCL_NO_PACKAGES)): $(GO_TARGET_PREFIX)cover.%:
	$(call GO_TEST,$*) -coverprofile=$(GO_COVERAGE_MODULE_FILE) -covermode=count $(call go_module_slug_to_packages,$*)

.PHONY: $(addprefix $(GO_TARGET_PREFIX)cover.,$(GO_MODULE_SLUGS_NO_PACKAGES))
$(addprefix $(GO_TARGET_PREFIX)cover.,$(GO_MODULE_SLUGS_NO_PACKAGES)): $(GO_TARGET_PREFIX)cover.%:

# fmt, fmt.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)FMT_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)fmt.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)fmt
$(GO_TARGET_PREFIX)fmt: $($(GO_MK_VAR_PREFIX)FMT_TARGETS) ## Runs the go fmt command.

.PHONY: $($(GO_MK_VAR_PREFIX)FMT_TARGETS)
$($(GO_MK_VAR_PREFIX)FMT_TARGETS): $(GO_TARGET_PREFIX)fmt.%:
	$(MAKE) -s -C $(call go_module_slug_to_path,$*) -f $(ROOT_MAKEFILE) $(GO_TARGET_PREFIX)_fmt "GO_PACKAGES=$(call go_module_slug_to_packages,$*)"

.PHONY: $(GO_TARGET_PREFIX)_fmt
$(GO_TARGET_PREFIX)_fmt:
	$(GO_FMT) $(GO_PACKAGES)

# generate, generate.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)GENERATE_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)generate.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)generate
$(GO_TARGET_PREFIX)generate: $($(GO_MK_VAR_PREFIX)GENERATE_TARGETS) ## Runs the go generate command.

.PHONY: $($(GO_MK_VAR_PREFIX)GENERATE_TARGETS)
$($(GO_MK_VAR_PREFIX)GENERATE_TARGETS): $(GO_TARGET_PREFIX)generate.%:
	$(MAKE) -s -C $(call go_module_slug_to_path,$*) -f $(ROOT_MAKEFILE) $(GO_TARGET_PREFIX)_generate "GO_PACKAGES=$(call go_module_slug_to_packages,$*)"

.PHONY: $(GO_TARGET_PREFIX)_generate
$(GO_TARGET_PREFIX)_generate:
	$(GO_GENERATE) $(GO_PACKAGES)

# fix, fix.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)FIX_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)fix.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)fix
$(GO_TARGET_PREFIX)fix: $($(GO_MK_VAR_PREFIX)FIX_TARGETS) ## Runs the go fix command.

.PHONY: $(addprefix $(GO_TARGET_PREFIX)fix.,$(GO_MODULE_SLUGS_EXCL_NO_FIX))
$(addprefix $(GO_TARGET_PREFIX)fix.,$(GO_MODULE_SLUGS_EXCL_NO_FIX)): $(GO_TARGET_PREFIX)fix.%:
	$(MAKE) -s -C $(call go_module_slug_to_path,$*) -f $(ROOT_MAKEFILE) $(GO_TARGET_PREFIX)_fix "GO_PACKAGES=$(call go_module_slug_to_packages,$*)"

.PHONY: $(addprefix $(GO_TARGET_PREFIX)fix.,$(GO_MODULE_SLUGS_INCL_NO_FIX))
$(addprefix $(GO_TARGET_PREFIX)fix.,$(GO_MODULE_SLUGS_INCL_NO_FIX)): $(GO_TARGET_PREFIX)fix.%:

.PHONY: $(GO_TARGET_PREFIX)_fix
$(GO_TARGET_PREFIX)_fix:
	$(GO_FIX) $(GO_PACKAGES)

# update, update.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)UPDATE_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)update.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)update
$(GO_TARGET_PREFIX)update: $($(GO_MK_VAR_PREFIX)UPDATE_TARGETS) ## Runs go get -u -t ./..., go get -u tool, then go mod tidy.

.PHONY: $(addprefix $(GO_TARGET_PREFIX)update.,$(GO_MODULE_SLUGS_EXCL_NO_UPDATE))
$(addprefix $(GO_TARGET_PREFIX)update.,$(GO_MODULE_SLUGS_EXCL_NO_UPDATE)): $(GO_TARGET_PREFIX)update.%:
	@$(MAKE) -C $(call go_module_slug_to_path,$*) -f $(ROOT_MAKEFILE) $(GO_TARGET_PREFIX)_update

.PHONY: $(addprefix $(GO_TARGET_PREFIX)update.,$(GO_MODULE_SLUGS_NO_UPDATE))
$(addprefix $(GO_TARGET_PREFIX)update.,$(GO_MODULE_SLUGS_NO_UPDATE)): $(GO_TARGET_PREFIX)update.%: $(GO_TARGET_PREFIX)tidy.%

# N.B. Uses the "tool" reserved package - see `go help packages | less`.
.PHONY: $(GO_TARGET_PREFIX)_update
$(GO_TARGET_PREFIX)_update:
	$(GO) get -u -t ./...
	$(GO) get -u tool
	$(GO) mod tidy

# tidy, tidy.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)TIDY_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)tidy.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)tidy
$(GO_TARGET_PREFIX)tidy: $($(GO_MK_VAR_PREFIX)TIDY_TARGETS) ## Runs go mod tidy.

.PHONY: $($(GO_MK_VAR_PREFIX)TIDY_TARGETS)
$($(GO_MK_VAR_PREFIX)TIDY_TARGETS): $(GO_TARGET_PREFIX)tidy.%:
	@$(MAKE) -C $(call go_module_slug_to_path,$*) -f $(ROOT_MAKEFILE) $(GO_TARGET_PREFIX)_tidy

.PHONY: $(GO_TARGET_PREFIX)_tidy
$(GO_TARGET_PREFIX)_tidy:
	$(GO) mod tidy

# doc, doc.<go module slug>

$(eval $(GO_MK_VAR_PREFIX)GO_DOC_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)doc.,$$(GO_MODULE_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)doc
$(GO_TARGET_PREFIX)doc: ## Runs the go doc tool specifying -http. Module variants default to text.
	$(GO) -C $(PROJECT_ROOT) doc $(if $(filter -http --http -http=% --http=%,$(GO_DOC_FLAGS)),,-http) $(GO_DOC_FLAGS)

.PHONY: $($(GO_MK_VAR_PREFIX)GO_DOC_TARGETS)
$($(GO_MK_VAR_PREFIX)GO_DOC_TARGETS): $(GO_TARGET_PREFIX)doc.%:
	$(GO) -C $(PROJECT_ROOT)/$(call go_module_slug_to_path,$*) doc $(GO_DOC_FLAGS)

##@ Grit Targets

# grit, grit.<grit destination slug>, grit-pull, grit-pull.<grit destination slug>

$(eval $(GO_MK_VAR_PREFIX)GRIT_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)grit.,$$(GRIT_DST_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)grit
$(GO_TARGET_PREFIX)grit: $($(GO_MK_VAR_PREFIX)GRIT_TARGETS) ## Runs grit to sync configured directories to their target repositories.

##+ grit.<grit destination slug>: Runs grit to sync one GRIT_DST directory to its repository.
.PHONY: $(addprefix $(GO_TARGET_PREFIX)grit.,$(GRIT_DST_SLUGS))
$(addprefix $(GO_TARGET_PREFIX)grit.,$(GRIT_DST_SLUGS)): $(GO_TARGET_PREFIX)grit.%:
	$(call GRIT_MODULE_COMMAND,$*)

# grit-pull, grit-pull.<grit destination slug>

$(eval $(GO_MK_VAR_PREFIX)GRIT_PULL_TARGETS := $$(addprefix $$(GO_TARGET_PREFIX)grit-pull.,$$(GRIT_DST_SLUGS)))

.PHONY: $(GO_TARGET_PREFIX)grit-pull
$(GO_TARGET_PREFIX)grit-pull: $($(GO_MK_VAR_PREFIX)GRIT_PULL_TARGETS) ## Runs grit to sync configured repositories back into their directories.

##+ grit-pull.<grit destination slug>: Runs grit to sync one GRIT_DST repository back into its directory.
.PHONY: $(addprefix $(GO_TARGET_PREFIX)grit-pull.,$(GRIT_DST_SLUGS))
$(addprefix $(GO_TARGET_PREFIX)grit-pull.,$(GRIT_DST_SLUGS)): $(GO_TARGET_PREFIX)grit-pull.%:
	$(call GRIT_PULL_MODULE_COMMAND,$*)

# N.B. Unlike the grit.<slug> and grit-pull.<slug> targets, grit-init is a
# "script" target, for a GRIT_DST entry whose directory does not yet exist. It
# refuses to run if the directory is already present - use grit-pull.<slug> for
# that case. The directory is never the repository root.

.PHONY: $(GO_TARGET_PREFIX)grit-init
$(GO_TARGET_PREFIX)grit-init: ## Runs grit to initialize a new GRIT_DST, see Makefile for docs.
ifeq ($(IS_WINDOWS),true)
	if exist $(subst /,\,$(_grit_init_DIR)) exit 1
else
	if [ -e "$(_grit_init_DIR)" ] || [ -L "$(_grit_init_DIR)" ]; then exit 1; fi
endif
	$(GRIT) $(GRIT_FLAGS) $(_grit_init_REMOTE) $(_grit_init_LOCAL)

_grit_init_SLUG = $(or $(GRIT_INIT_TARGET),$(error GRIT_INIT_TARGET is not set))
_grit_init_PATH = $(call grit_dst_slug_to_path_or_error,$(_grit_init_SLUG))
_grit_init_DIR = $(or $(patsubst ./%,%,$(filter-out .,$(_grit_init_PATH))),$(error refusing to grit-init the repository root))
_grit_init_REMOTE = $(call grit_dst_slug_to_remote,$(_grit_init_SLUG))
_grit_init_LOCAL = $(call grit_dst_slug_to_local,$(_grit_init_SLUG))

# ---

##@ Sub-Makefile Targets

##+ run.<./**/Makefile path as slug>: Runs make at the given path.
# This is a pattern rule. These per-makefile default targets will show up in
# shell completion. They're a separate process, i.e. are independent.

SUBDIR_MAKEFILE_TARGETS := $(addprefix run.,$(SUBDIR_MAKEFILE_SLUGS))

.PHONY: $(SUBDIR_MAKEFILE_TARGETS)
$(SUBDIR_MAKEFILE_TARGETS): run.%:
	@$(MAKE) -C $(call subdir_makefile_slug_to_path,$*) $(RUN_FLAGS)

# makefile implicit rules

##+ run-%.<./**/Makefile path as slug>: Runs make target at the given path.
# Note that eval is necessary to make this work properly, as a pattern rule can
# only be used once. The $(GO_TARGET_PREFIX)FORCE target is used as a dummy,
# since GNU Make requires .PHONY targets to be explicit (not implicit).
define _run_TEMPLATE =
run-%.$2: $(GO_TARGET_PREFIX)FORCE
	@$$(MAKE) -C $1 $(RUN_FLAGS) $$*

endef
# warning: simply-expanded
$(foreach d,$(SUBDIR_MAKEFILE_PATHS),$(eval $(call _run_TEMPLATE,$d,$(call subdir_makefile_path_to_slug,$d))))

# ---

##@ Other Targets

.PHONY: $(GO_TARGET_PREFIX)tools
$(GO_TARGET_PREFIX)tools: ## Uses go get -tool to add the tools for _this_ Makefile to go.mod.
	$(foreach tool,$(GO_TOOLS),$(_tools_TEMPLATE))
define _tools_TEMPLATE =
$(GO) get -tool $(tool)

endef

.PHONY: $(GO_TARGET_PREFIX)debug-vars
$(GO_TARGET_PREFIX)debug-vars: ## Prints the values of the specified variables.
	$(foreach debug_var,$($(GO_MK_VAR_PREFIX)DEBUG_VARS),$(if $(filter undefined,$(origin $(debug_var))),,$(_debug_vars_TEMPLATE)))
define _debug_vars_TEMPLATE =
@echo $(debug_var)=$(call escape_command_arg,$($(debug_var)))

endef

ifneq ($(IS_WINDOWS),true)
ifneq ($(SKIP_FURTHER_MAKEFILE_HELP),true)
SKIP_FURTHER_MAKEFILE_HELP := true
ifndef MAKEFILE_HELP_SCRIPT
define _MAKEFILE_HELP_SCRIPT :=
# Run a command with args, auto-detecting color stripping and paging of stdout.
run_with_smart_human_readable_output() {
  if ! [ $$# -ge 1 ]; then
    echo "Usage: run_with_smart_human_readable_output <command> [args...]" >&2
    return 2
  fi

  # ansi color stripping sed script https://stackoverflow.com/a/51141872
  # N.B. local variables are not POSIX
  _run_with_smart_human_readable_output_strip_color='s/\x1B\[[0-9;]\{1,\}[A-Za-z]//g'

  if ! [ -t 1 ]; then
    # non-terminal output (e.g., piped or redirected) - strip color
    "$$@" | sed "$$_run_with_smart_human_readable_output_strip_color"
    return "$$?"
  fi

  # terminal output...

  # check if color is supported
  command -v tput >/dev/null 2>&1 &&
  _run_with_smart_human_readable_output_tput_colors="$$(tput colors 2>/dev/null)" ||
  _run_with_smart_human_readable_output_tput_colors=0

  # run command, with pager, if available
  if command -v less >/dev/null 2>&1; then
    if [ "$$_run_with_smart_human_readable_output_tput_colors" -gt 0 ]; then
      "$$@" | less -R
    else
      "$$@" | sed "$$_run_with_smart_human_readable_output_strip_color" | less
    fi
  elif command -v more >/dev/null; then
    # note 1: the above deliberately leaves stderr alone, so that error from at
    # least one `command` builtin is shown (idk, might be some weird shells
    # around)
    # note 2: didnt bother checking if `more` (consistently) supports color
    "$$@" | sed "$$_run_with_smart_human_readable_output_strip_color" | more
  elif [ "$$_run_with_smart_human_readable_output_tput_colors" -gt 0 ]; then
    "$$@"
  else
    "$$@" | sed "$$_run_with_smart_human_readable_output_strip_color"
  fi
} &&
generate_help='
BEGIN {
  FS = ":.*##";
  # Print the initial usage message
  printf "\nUsage:\n  $(or $(notdir $(MAKE)),make) \033[36m<target>\033[0m\n";
  in_usage_block = 0;       # Flag: currently inside a documentation block
  usage_marker_found = 0;   # Flag: Saw "# Usage" line, looking for "# ---"
  current_doc_file = "";    # Key (relative path or special marker) for storing docs
  doc_separator = "\n\n";   # Separator between doc blocks if multiple in one file
  # Format for target help lines.
  # - \033[36m: ANSI escape code for cyan color
  # - %-18s: Left-justify string in a field of 18 characters
  # - \033[0m: ANSI escape code to reset color/attributes
  target_format = "  \033[36m%-35s\033[0m %s\n";
}

# Match section headers (##@ ) - Print only when not capturing docs.
/^##@ / {
  if (!in_usage_block) {
    printf "\n\033[1m%s\033[0m\n", substr($$0, 5);
  }
}

# Match target lines (target: ... ## description) - Print only when not capturing docs.
# N.B. Inclusive of target prefix variables - that is handled afterwards.
/^[\$$\(\)a-zA-Z0-9._%-]+:.*?##/ {
  if (!in_usage_block) {
    printf target_format, $$1, $$2;
  }
}

# Manually documented targets, like "##+ target: [description]".
/^##\+.*:/ {
  if (!in_usage_block) {
    colon_pos = index($$0, ":");
    if (colon_pos > 0) {
      manual_target = substr($$0, 1, colon_pos - 1);
      sub(/^##\+ */, "", manual_target);
      sub(/ +$$/, "", manual_target);

      manual_description = substr($$0, colon_pos + 1);
      sub(/^ +/, "", manual_description);
      sub(/ +$$/, "", manual_description);

      printf target_format, manual_target, manual_description;
    }
  }
}

# --- Documentation Block Parsing Logic ---

# Detect start of documentation block: line 1 "# Usage"
# Note: Exact match, case-sensitive. Anchored start/end.
/^# Usage$$/ {
  usage_marker_found = 1; # Mark that we found the first part
  next; # Skip processing this line further, move to the next line
}

# Detect start of documentation block: line 2 "# ---" (only if line 1 matched)
# Note: Exact match, case-sensitive. Anchored start/end.
/^# ---$$/ && usage_marker_found {
  in_usage_block = 1;       # We are now officially inside a documentation block
  usage_marker_found = 0;   # Reset the marker for the next potential block

  # Calculate relative path for this file
  rel_path = FILENAME;
  # Ensure project_root has no trailing slash for safety, then remove prefix
  # Use gsub for global replacement in case project_root appears elsewhere, though unlikely here.
  gsub(/\/$$/, "", project_root); # Remove trailing slash from project_root if exists
  # Escape potential regex special chars in project_root before using in sub() if needed,
  # but direct string prefix removal is usually safe here.
  # Add ^ to anchor the substitution at the beginning.
  sub("^" project_root "/", "", rel_path);

  # Determine the storage key: special for "Makefile", relative path otherwise
  # Use tolower() for case-insensitive comparison of the filename part
  if (tolower(rel_path) == "makefile") {
    current_doc_file = "__MAIN_MAKEFILE__"; # Special key for root Makefile
  } else {
    current_doc_file = rel_path; # Use relative path as key
  }

  # Initialize documentation storage if this is the first block for this file
  if (!(current_doc_file in makefile_docs)) {
    makefile_docs[current_doc_file] = "";
  } else if (makefile_docs[current_doc_file] != "") {
    # If adding another block from the *same* file, add a separator
    makefile_docs[current_doc_file] = makefile_docs[current_doc_file] doc_separator;
  }
  next; # Skip processing the "---" line itself
}

# Capture documentation lines (lines starting with '#' while inside a block)
in_usage_block && /^#/ {
  line = $$0;
  # Remove leading "# " or just "#"
  sub(/^# ?/, "", line);
  # Append the cleaned line, prefixed with indent, suffixed with a newline character
  makefile_docs[current_doc_file] = makefile_docs[current_doc_file] "  " line "\n";
  next; # Process next line
}

# End of documentation block (non-comment line encountered while inside a block)
in_usage_block && !/^#/ {
  in_usage_block = 0;     # Exit documentation capture mode
  current_doc_file = "";  # Clear the current file key
  # Reset the initial marker just in case, though !/^#/ below also handles it
  usage_marker_found = 0;
  # IMPORTANT: This line itself is NOT processed further in this cycle.
  # If it were a target or header, it would be missed. This is a simplification:
  # assumes documentation blocks are not immediately followed by lines
  # that *also* need processing by other rules in the same cycle.
  # The checks `if (!in_usage_block)` in other rules prevent them from running
  # while capturing, so this non-comment line effectively stops capture and is ignored.
}

# Reset usage marker if we see a non-matching line while waiting for "# ---"
!/^# ---$$/ && usage_marker_found {
  usage_marker_found = 0; # Failed to find "# ---" immediately after "# Usage"
}

# --- END Block: Print Collected Documentation ---

END {
  # Check if any documentation was collected
  doc_exists = 0;
  for (file_key in makefile_docs) {
    # Remove trailing newline from the collected block before checking if empty
    gsub(/\n$$/, "", makefile_docs[file_key]);
    if (makefile_docs[file_key] != "") {
      doc_exists = 1;
      break;
    }
  }

  if (doc_exists) {
    # Print a main header for the documentation section
    printf "\n\033[1mNotes\033[0m\n";

    # Print main Makefile documentation first if it exists and is not empty
    main_doc_key = "__MAIN_MAKEFILE__";
    if (main_doc_key in makefile_docs && makefile_docs[main_doc_key] != "") {
      # Header with underline
      printf "\n\033[4mMakefile:\033[0m\n%s\n", makefile_docs[main_doc_key];
    }

    # Print documentation from other Makefiles
    # Awk array iteration order is not guaranteed, but often insertion order or hash order.
    # Sorting keys would require GNU awk typically. For simplicity, accept awk default order.
    for (file_key in makefile_docs) {
      if (file_key != main_doc_key && makefile_docs[file_key] != "") {
        # Header with underline, showing the relative path
        printf "\n\033[4m%s:\033[0m\n%s\n", file_key, makefile_docs[file_key];
      }
    }
  }
}
' &&
deduplicated_makefile_list="$$(printf '%s\n' "$$MAKEFILE_LIST" | awk '{for(i=NF;i>=1;i--)if(!a[$$i]++)s=$$i (s==""?"":" ")s;printf "%s",s}')" &&
help_text="$$(awk -v project_root=$(call escape_command_arg,$(PROJECT_ROOT)) "$$generate_help" $${deduplicated_makefile_list})" &&
help_text="$$(echo "$$help_text" | sed $(foreach target_prefix,GO_TARGET_PREFIX $(MAKEFILE_TARGET_PREFIXES), -e s/\$$\($(call escape_command_arg,$(target_prefix))\)/$(call escape_command_arg,$($(target_prefix)))/g\;))" &&
run_with_smart_human_readable_output echo "$$help_text"
endef
export _MAKEFILE_HELP_SCRIPT
MAKEFILE_HELP_SCRIPT := eval "$$_MAKEFILE_HELP_SCRIPT"
endif

.PHONY: help
help: ## Display this help.
	@export MAKEFILE_LIST=$(call escape_command_arg,$(MAKEFILE_LIST)); $(MAKEFILE_HELP_SCRIPT)

.PHONY: h
h: help ## Alias for help.
endif
endif

# ---

# misc targets users can ignore

# we use .PHONY, but there's an edge case requiring this pattern
.PHONY: $(GO_TARGET_PREFIX)FORCE
$(GO_TARGET_PREFIX)FORCE:
