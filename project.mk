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

# Prune the generated scratch/gomod dependency copies from module discovery:
# they are ephemeral working state (gitignored), and third-party library
# modules cannot satisfy this project's lint targets (e.g. deadcode requires
# a main package).
GO_MODULE_PATHS_EXCLUDE_PATTERNS ?= %/scratch/gomod

# Exclude betteralign and grit from the default tools
GO_TOOLS ?= $(filter-out $(GO_PKG_BETTERALIGN) $(GO_PKG_GRIT),$(GO_TOOLS_DEFAULT))

# Disable betteralign targets for all modules
GO_MODULE_SLUGS_NO_BETTERALIGN ?= $(GO_MODULE_SLUGS)

# Enable deadcode targets for all modules
GO_MODULE_SLUGS_USE_DEADCODE ?= $(GO_MODULE_SLUGS)

# Use .deadcodeignore file for deadcode false-positive filtering
# N.B. relative to the go module it applies to
DEADCODE_IGNORE_PATTERNS_FILE ?= .deadcodeignore

# Treat any unignored deadcode finding as a lint error (Hana-san directive
# 2026-08-26): unreachable code is a defect to remove, not a note to file.
DEADCODE_ERROR_ON_UNIGNORED ?= true

# Preserve the Makefile's own default goal. project.mk is included (line ~161)
# before the Makefile defines `all` (~line 382), so without this the first
# target defined below would become the default goal — GNU Make picks the
# first target in parse order, and includes count.
.DEFAULT_GOAL := $(GO_TARGET_PREFIX)all

# field-recapture — manual, credentials-gated capture tooling (task 31).
#
# Purpose: refresh internal/transcode/testcorpus/testdata/field/ with new
# real-gateway wire bytes (qwen-style chat stream/non-stream bodies carrying
# the provider-extension spellings, and the codex multi-turn responses
# request) so the field-capture regression harness
# (internal/transcode/field_capture_replay_test.go) replays the exact bytes
# a live provider sent, through the production decode functions.
#
# SAFETY — read before running:
#   * Credentials required: FIELD_UPSTREAM must be a gateway you are
#     authorized to use; running the probes costs real tokens. Export the
#     gateway credential as FIELD_KEY (bearer token) first. This tooling
#     never reads or writes credential files and never echoes the secret:
#     the probe feeds the bearer header to curl via a here-string read from
#     the environment at runtime (`$$FIELD_KEY`), so the token never appears
#     in a make-expanded argv, in `make -n`, or in the process list.
#   * NEVER point FIELD_UPSTREAM at, or set FIELD_PORT to, the shared proxy
#     ports 11240/11241/11242 owned by other users — the guards below refuse
#     those ports outright. Use your own gateway URL and a private port.
#   * NEVER blanket-kill processes. The throwaway shaper is stopped by
#     exact PID only, via `make field-recapture-stop`.
#   * Manual-only: these targets are outside the default `all` graph, are
#     never run by CI, and make no network calls unless FIELD_UPSTREAM and
#     FIELD_KEY are explicitly provided on the command line.

FIELD_PORT ?= 11243
FIELD_DIR ?= scratch/field-recapture
FIELD_UPSTREAM ?=
FIELD_KEY ?=
FIELD_MODEL ?= qwen3.8-27b

.PHONY: field-recapture
field-recapture: ## [manual, credentials-gated] Boot a throwaway shaper for field recapture. Requires FIELD_UPSTREAM (never 11240/11241/11242); stop with field-recapture-stop.
ifeq ($(FIELD_UPSTREAM),)
	@echo "field-recapture: refusing to run: FIELD_UPSTREAM is empty."
	@echo "  This tooling sends real requests to a live gateway and costs tokens."
	@echo "  Set FIELD_UPSTREAM=<gateway base url you are authorized to use>,"
	@echo "  export FIELD_KEY=<gateway bearer token>, then re-run."
	@echo "  NEVER point it at the shared proxies on 11240/11241/11242."
	@exit 1
endif
	@case "$(FIELD_UPSTREAM)" in \
		*:11240*|*:11241*|*:11242*) \
			echo "field-recapture: refusing: FIELD_UPSTREAM appears to target a shared proxy port (11240/11241/11242)."; \
			exit 1 ;; \
	esac
	@case "$(FIELD_PORT)" in \
		11240|11241|11242) \
			echo "field-recapture: refusing: FIELD_PORT=$(FIELD_PORT) is a shared proxy port (11240/11241/11242)."; \
			exit 1 ;; \
	esac
	@if lsof -nP -iTCP:$(FIELD_PORT) -sTCP:LISTEN >/dev/null 2>&1; then \
		echo "field-recapture: refusing: port $(FIELD_PORT) is already in use."; \
		echo "  Stop the existing listener or set FIELD_PORT to another private port."; \
		exit 1; \
	fi
	@mkdir -p $(FIELD_DIR)
	@go build -o $(FIELD_DIR)/shaper-bin .
	@$(FIELD_DIR)/shaper-bin -bind 127.0.0.1:$(FIELD_PORT) \
		-upstream $(FIELD_UPSTREAM) \
		-transcode-messages-chat \
		-transcode-auth bearer \
		-transcode-auth-source env:FIELD_KEY \
		>$(FIELD_DIR)/proxy.log 2>&1 & echo $$! >$(FIELD_DIR)/proxy.pid
	@sleep 1
	@if ! kill -0 $$(cat $(FIELD_DIR)/proxy.pid) 2>/dev/null; then \
		echo "field-recapture: proxy failed to start; last log lines:"; \
		tail -5 $(FIELD_DIR)/proxy.log; \
		exit 1; \
	fi
	@echo "field-recapture: shaper pid $$(cat $(FIELD_DIR)/proxy.pid) on 127.0.0.1:$(FIELD_PORT) -> $(FIELD_UPSTREAM)"
	@echo "field-recapture: capture raw provider bytes with 'make field-recapture-probe',"
	@echo "  then drive real clients through the shaper (messages dialect):"
	@echo "    ANTHROPIC_AUTH_TOKEN=probe ANTHROPIC_BASE_URL=http://127.0.0.1:$(FIELD_PORT) \\"
	@echo "      ANTHROPIC_MODEL=$(FIELD_MODEL) claude -p 'Remember the number 8463' --session-id <uuid>"
	@echo "  Stop with 'make field-recapture-stop' (exact-PID kill)."

.PHONY: field-recapture-probe
field-recapture-probe: ## [manual, credentials-gated] Save raw upstream stream + non-stream chat bytes to $(FIELD_DIR)/raw/. Requires FIELD_UPSTREAM and FIELD_KEY.
ifeq ($(FIELD_UPSTREAM),)
	@echo "field-recapture-probe: refusing to run: FIELD_UPSTREAM is empty."; exit 1
endif
ifeq ($(FIELD_KEY),)
	@echo "field-recapture-probe: refusing to run: FIELD_KEY is empty (credentials required)."; exit 1
endif
	@case "$(FIELD_UPSTREAM)" in \
		*:11240*|*:11241*|*:11242*) \
			echo "field-recapture-probe: refusing: shared proxy port (11240/11241/11242)."; \
			exit 1 ;; \
	esac
	@mkdir -p $(FIELD_DIR)/raw
	@curl -sS --fail-with-body --max-time 120 -N --config - \
		-H 'Content-Type: application/json' \
		-d '{"model":"$(FIELD_MODEL)","stream":true,"messages":[{"role":"user","content":"Remember the number 8463 for later."}]}' \
		$(FIELD_UPSTREAM)/v1/chat/completions \
		<<<"header = \"Authorization: Bearer $$FIELD_KEY\"" \
		>$(FIELD_DIR)/raw/stream.sse || (rm -f $(FIELD_DIR)/raw/stream.sse; exit 1)
	@curl -sS --fail-with-body --max-time 120 --config - \
		-H 'Content-Type: application/json' \
		-d '{"model":"$(FIELD_MODEL)","stream":false,"messages":[{"role":"user","content":"What number did I ask you to remember?"}]}' \
		$(FIELD_UPSTREAM)/v1/chat/completions \
		<<<"header = \"Authorization: Bearer $$FIELD_KEY\"" \
		>$(FIELD_DIR)/raw/nonstream.json || (rm -f $(FIELD_DIR)/raw/nonstream.json; exit 1)
	@echo "field-recapture-probe: saved raw bytes (grep for secrets before committing anything):"
	@ls -l $(FIELD_DIR)/raw/stream.sse $(FIELD_DIR)/raw/nonstream.json
	@echo "  Refresh testdata/field/ fixtures from these bytes, then run:"
	@echo "    go test ./internal/transcode/ -run TestFieldCapture -count=1"

.PHONY: field-recapture-stop
field-recapture-stop: ## [manual] Stop the field-recapture shaper by exact PID + identity check (never a blanket kill).
	@if [ -f $(FIELD_DIR)/proxy.pid ]; then \
		pid=$$(cat $(FIELD_DIR)/proxy.pid); \
		if kill -0 $$pid 2>/dev/null; then \
			if ! ps -p $$pid -o command= 2>/dev/null | grep -q "$(FIELD_DIR)/shaper-bin"; then \
				echo "field-recapture-stop: pid $$pid is alive but is NOT the field-recapture shaper (stale pid file; PID was recycled)."; \
				echo "  Removing the stale pid file without killing anything."; \
				rm -f $(FIELD_DIR)/proxy.pid; \
				exit 1; \
			fi; \
			kill $$pid; \
			for i in 1 2 3 4 5 6 7 8 9 10; do \
				kill -0 $$pid 2>/dev/null || break; \
				sleep 0.5; \
			done; \
			kill -0 $$pid 2>/dev/null && echo "field-recapture-stop: pid $$pid did not exit" && exit 1; \
			echo "field-recapture-stop: stopped pid $$pid"; \
		else \
			echo "field-recapture-stop: pid $$pid not running (already stopped)"; \
		fi; \
	else \
		echo "field-recapture-stop: no pid file at $(FIELD_DIR)/proxy.pid"; \
	fi

