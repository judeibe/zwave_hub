# zwave_hub — the whole command surface, in one file (constitution X).
#
# `make` with no arguments lists every target below. Each target is the only
# supported way to run its action: CI calls these same targets, so a green
# local run and a green pipeline mean the same thing. Defaults live in the
# variables here; override them at the call site rather than editing the file
# (`make image TAG=v1.2.3`), so no required argument is hidden in someone's shell.
#
# Requires GNU Make 3.81 features only (the system make on macOS) and a POSIX
# /bin/sh. No .ONESHELL, no $(file ...), no `!=`, no pipefail.

SHELL := /bin/sh

GO      ?= go
BIN     ?= bin/zwave-hub
TAG     ?= dev
BIND    ?= 0.0.0.0:8080
IMAGE   ?= zwave-hub
COMPOSE ?= docker compose -f deploy/compose.yaml

.DEFAULT_GOAL := help

.PHONY: help build test test-contract lint run-mock image up down rollback-check

# help is the default goal so a newcomer can list every command in 30 seconds;
# the listing is generated from the `##` text on each target, which is what
# keeps it from drifting out of date with the targets that exist.
help: ## List every target with a one-line description
	@printf 'Targets (default goal: help):\n'
	@awk 'BEGIN { FS = ":.*## " } /^[a-zA-Z0-9_-]+:.*## / { printf "  %-16s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# The only build path: the static binary CI ships and the sidecar dependencies
# the image's first stage installs. Anything built another way is not this one.
build: ## Build the static Go binary and the sidecar's node_modules
	@mkdir -p $(dir $(BIN))
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o "$(BIN)" ./cmd/zwave-hub
	@if [ -f sidecar/package-lock.json ]; then \
		cd sidecar && npm ci; \
	else \
		echo "build: sidecar/package-lock.json absent, skipped npm ci"; \
	fi

# Unit and integration tests live behind one command with no flags to remember,
# so the tests a developer runs and the tests CI runs are the same set. Packages
# under an npm node_modules tree are excluded: `make build` installs the sidecar
# there and one dependency (flatted) ships Go source, which `./...` would
# otherwise compile as if it were ours.
test: ## Run unit and integration tests (go test ./...)
	@pkgs=$$($(GO) list ./... | grep -v '/node_modules/'); \
	if [ -z "$$pkgs" ]; then echo "test: no Go packages found" >&2; exit 1; fi; \
	$(GO) test $$pkgs

# The sidecar is the only third-party boundary, so it is exercised as a real
# process: this spawns `zwave-server --mock-driver` and asserts the handshake,
# the 1013 retry and the restart-on-key-change behaviour. An absent suite exits
# non-zero rather than reporting a meaningless green.
test-contract: ## Run sidecar protocol tests against a real zwave-server --mock-driver child
	@if [ -d tests/contract ]; then \
		$(GO) test -tags contract -count=1 ./tests/contract/...; \
	else \
		echo "test-contract: tests/contract/ is not present yet; T036 adds the first contract test." >&2; \
		echo "test-contract: it will run: go test -tags contract -count=1 ./tests/contract/..." >&2; \
		echo "test-contract: no protocol test ran; exiting non-zero so an absent suite is not a green." >&2; \
		exit 1; \
	fi

# One lint gate, identical locally and in CI: formatting is checked rather than
# silently fixed, vet runs over every package, and the key-material scanner
# proves no handler or event ever renders a 32-hex key value (SC-004).
lint: ## Check gofmt, go vet and the key-material scanner test
	@unformatted=$$(gofmt -l . | grep -v '/node_modules/'); \
	if [ -n "$$unformatted" ]; then \
		echo "lint: gofmt reports unformatted files (fix with: gofmt -w .):" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi
	@pkgs=$$($(GO) list ./... | grep -v '/node_modules/'); \
	if [ -z "$$pkgs" ]; then echo "lint: no Go packages found" >&2; exit 1; fi; \
	$(GO) vet $$pkgs
	$(GO) test ./internal/... -run TestNoKeyMaterial -count=1

# Development without hardware runs the same binary in the same mode the
# contract tests use, against a throwaway data directory, so a mock run can
# never touch a real installation.
run-mock: ## Run the hub with ZWAVE_HUB_DRIVER_MODE=mock on a temporary data directory
	@data=$$(mktemp -d); \
	echo "run-mock: data dir $$data"; \
	ZWAVE_HUB_DRIVER_MODE=mock ZWAVE_HUB_DATA_DIR="$$data" ZWAVE_HUB_BIND="$(BIND)" \
		$(GO) run ./cmd/zwave-hub

# The image is built from deploy/Dockerfile with no CI-only arguments, so the
# tag a developer builds is byte-for-byte the recipe the pipeline runs. The
# Dockerfile arrives with a later task; until then this fails by name, not by
# handing docker a path that does not exist.
image: ## Build the two-stage image from deploy/Dockerfile
	@if [ ! -f deploy/Dockerfile ]; then \
		echo "image: deploy/Dockerfile is not present yet; T072 adds it." >&2; \
		echo "image: it will run: docker build -f deploy/Dockerfile -t $(IMAGE):$(TAG) ." >&2; \
		echo "image: nothing was built; docker build was not run." >&2; \
		exit 1; \
	fi
	docker build -f deploy/Dockerfile -t $(IMAGE):$(TAG) .

# Bringing the stack up means up *and* ready: the /readyz wait is part of the
# target so no caller invents its own polling loop.
up: ## Start the compose stack and wait for GET /readyz
	@if [ ! -f deploy/compose.yaml ]; then \
		echo "up: deploy/compose.yaml is not present yet; T073 adds it." >&2; \
		echo "up: it will run: $(COMPOSE) up -d, then poll GET http://$(BIND)/readyz" >&2; \
		echo "up: nothing was started." >&2; \
		exit 1; \
	fi
	$(COMPOSE) up -d
	@printf 'up: waiting for http://%s/readyz' '$(BIND)'; \
	tries=0; \
	until curl -fsS "http://$(BIND)/readyz" >/dev/null 2>&1; do \
		tries=$$((tries + 1)); \
		if [ "$$tries" -ge 60 ]; then \
			echo; \
			echo "up: /readyz did not answer within 60s; inspect: $(COMPOSE) logs zwave-hub" >&2; \
			exit 1; \
		fi; \
		printf '.'; \
		sleep 1; \
	done; \
	echo ' ready'

# Not in T004's target list, but quickstart.md §3 documents "make up / make
# down | Compose lifecycle", and a stack you can only start is not a command
# surface. Same guard as up.
down: ## Stop and remove the compose stack (quickstart.md §3)
	@if [ ! -f deploy/compose.yaml ]; then \
		echo "down: deploy/compose.yaml is not present yet; T073 adds it." >&2; \
		echo "down: it will run: $(COMPOSE) down" >&2; \
		echo "down: nothing was stopped." >&2; \
		exit 1; \
	fi
	$(COMPOSE) down

# Constitution VII: rollback is tested, not assumed. One target wrapping one
# script, invoked the same way a deploy pipeline invokes it. The script arrives
# with a later task; until then the missing file is named, not guessed.
rollback-check: ## Run the rollback drill: previous tag against a copy of the current database
	@if [ ! -f scripts/rollback-check.sh ]; then \
		echo "rollback-check: scripts/rollback-check.sh is not present yet; it lands with T077." >&2; \
		echo "rollback-check: it must start the previous image tag against a copy of the current database" >&2; \
		echo "rollback-check: and assert GET /readyz plus a readable audit trail." >&2; \
		echo "rollback-check: no rollback was exercised." >&2; \
		exit 1; \
	fi
	sh scripts/rollback-check.sh
