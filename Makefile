.DEFAULT_GOAL := help
SHELL := /bin/bash

BINARY_DIR := bin
MODULE     := github.com/zevoxa/chowk-backend

# Embed version metadata at link time so a running binary can report exactly what
# it is. Without this, "which commit is in production" becomes guesswork.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS    := -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# --- dependencies -----------------------------------------------------------

.PHONY: up
up: ## Start Postgres and Redis, waiting until both are healthy
	docker compose up -d --wait

.PHONY: down
down: ## Stop containers, keeping data
	docker compose down

.PHONY: reset
reset: ## Destroy containers AND data, then re-create and migrate
	docker compose down -v
	docker compose up -d --wait
	$(MAKE) migrate

.PHONY: psql
psql: ## Open a psql shell
	docker compose exec postgres psql -U cloudkitchen -d cloudkitchen

# --- migrations -------------------------------------------------------------

.PHONY: migrate
migrate: ## Apply pending migrations
	go run ./cmd/migrate

.PHONY: migrate-status
migrate-status: ## Show applied and pending migrations
	go run ./cmd/migrate -status

.PHONY: erd
erd: ## Regenerate docs/ERD.md from the live database catalog
	# Generated rather than hand-written: a schema doc written from memory is wrong
	# within a week and nothing checks it. This reads information_schema and
	# pg_constraint, so it cannot drift from the database it describes.
	go run ./cmd/erdgen

.PHONY: migration
migration: ## Create a new migration: make migration name=add_wallet
	@if [ -z "$(name)" ]; then echo "usage: make migration name=add_wallet"; exit 1; fi
	@next=$$(printf "%04d" $$(( $$(ls migrations/*.sql 2>/dev/null | wc -l | tr -d ' ') + 1 ))); \
	file="migrations/$${next}_$(name).sql"; \
	printf -- "-- %s\n--\n-- Why this change:\n\n" "$${next}_$(name)" > $$file; \
	echo "created $$file"

# --- run --------------------------------------------------------------------

.PHONY: run
run: ## Run the API server
	go run -ldflags "$(LDFLAGS)" ./cmd/api

.PHONY: worker
worker: ## Run the background worker
	go run -ldflags "$(LDFLAGS)" ./cmd/worker

.PHONY: seed
seed: ## Seed the Khansama kitchen and menu for local development
	go run ./cmd/seed

.PHONY: dev
dev: up migrate run ## Start dependencies, migrate, then run the API

# --- build ------------------------------------------------------------------

.PHONY: build
build: ## Build all binaries into ./bin
	@mkdir -p $(BINARY_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BINARY_DIR)/api ./cmd/api
	go build -ldflags "$(LDFLAGS)" -o $(BINARY_DIR)/worker ./cmd/worker
	go build -ldflags "$(LDFLAGS)" -o $(BINARY_DIR)/migrate ./cmd/migrate
	@ls -lh $(BINARY_DIR)

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf $(BINARY_DIR) coverage.out

# --- quality ----------------------------------------------------------------

.PHONY: test
test: ## Run tests with the race detector
	# -race is not optional here. This codebase runs concurrent event handlers and
	# shared caches; a data race that only shows up under production load is the
	# worst class of bug to debug, and the detector finds it in CI for free.
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and open the coverage report
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

.PHONY: fmt
fmt: ## Format all Go code
	gofmt -w .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint if installed
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; run: brew install golangci-lint"; \
	fi

.PHONY: tidy
tidy: ## Tidy and verify go.mod
	go mod tidy
	go mod verify

.PHONY: check
check: fmt vet test ## Everything CI runs. Run this before pushing.

# --- architecture enforcement ----------------------------------------------

.PHONY: check-boundaries
check-boundaries: ## Fail if one module imports another
	# The modular-monolith rule is only real if something enforces it. Code review
	# will not catch this reliably -- a single convenience import is how a codebase
	# quietly becomes a tangle that can never be split into services.
	@echo "checking module boundaries..."
	@violations=0; \
	for dir in internal/modules/*/; do \
		mod=$$(basename $$dir); \
		found=$$(grep -rhoE "$(MODULE)/internal/modules/[a-z]+" $$dir --include="*.go" 2>/dev/null \
			| sed "s|.*/modules/||" | sort -u | grep -v "^$$mod$$" || true); \
		if [ -n "$$found" ]; then \
			echo "  VIOLATION: module '$$mod' imports: $$(echo $$found | tr '\n' ' ')"; \
			echo "             route this through internal/contracts instead"; \
			violations=1; \
		fi; \
	done; \
	if [ $$violations -eq 0 ]; then echo "  ok: no cross-module imports"; else exit 1; fi
