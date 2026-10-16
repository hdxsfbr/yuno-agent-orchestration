# Every target runs inside Docker. Nothing here needs a toolchain on the host beyond
# docker, make and curl.

COMPOSE      := docker compose
GO           := $(COMPOSE) --profile tools run --rm go
NODE         := $(COMPOSE) --profile tools run --rm node
WEB_PORT     ?= 8080

.DEFAULT_GOAL := help

.PHONY: help env run smoke probe-openclaw stop down clean logs ps test test-go test-web \
        test-integration lint tidy install migrate seed eval eval-baseline eval-final \
        tunnel build openclaw-shell openclaw-logs openclaw-validate prune-sandboxes

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

env: ## Create .env from the example and generate missing platform tokens
	@test -f .env || cp .env.example .env
	@./scripts/ensure-env.sh
	@mkdir -p data/openclaw

run: env ## Build and start everything, wait for ready, print the URL
	$(COMPOSE) --profile build build sandbox-image
	$(COMPOSE) up --build -d postgres deploytarget openclaw api web
	@echo "waiting for the stack..."
	@for i in $$(seq 1 120); do \
	  if curl -fsS -m 2 http://localhost:$(WEB_PORT)/readyz >/dev/null 2>&1; then \
	    echo "ready: http://localhost:$(WEB_PORT)"; exit 0; fi; sleep 1; done; \
	  echo "stack did not become ready; see: make ps, make logs, make openclaw-logs"; $(COMPOSE) ps -a; exit 1

smoke: ## Hit every service in the running stack and check isolation (scripts/smoke.sh)
	@WEB_PORT=$(WEB_PORT) ./scripts/smoke.sh

probe-openclaw: ## Run the OpenClaw integration canary against the running gateway
	$(COMPOSE) --profile tools run --rm probe

probe-approval: ## Verify the gateway's plugin approval RPCs the side-effect gate uses (DESIGN 5.3 rule 5)
	$(COMPOSE) --profile tools run --rm --entrypoint node probe /scripts/openclaw-probe-approval.mjs

build: ## Build images without starting
	$(COMPOSE) --profile build build

stop: ## Stop containers, keep data
	$(COMPOSE) stop

down: ## Stop and remove containers, keep volumes and ./data
	$(COMPOSE) --profile tools --profile eval --profile tunnel --profile build down --remove-orphans

clean: ## Stop and remove containers, volumes and ./data (drops the database and OpenClaw state)
	$(COMPOSE) --profile tools --profile eval --profile tunnel --profile build down --remove-orphans --volumes
	@docker ps -aq --filter "name=openclaw-sbx-" | xargs -r docker rm -f
	rm -rf data/openclaw

logs: ## Follow api logs
	$(COMPOSE) logs -f api

openclaw-logs: ## Follow gateway logs
	$(COMPOSE) logs -f openclaw

openclaw-shell: ## Shell with the OpenClaw CLI inside the running gateway container
	$(COMPOSE) exec openclaw bash

openclaw-validate: ## Validate openclaw.json against the gateway's schema (inside the running gateway)
	$(COMPOSE) exec openclaw node openclaw.mjs config validate

prune-sandboxes: ## Remove every OpenClaw sandbox container now (they are recreated on demand)
	@docker ps -aq --filter "name=openclaw-sbx-" | xargs -r docker rm -f >/dev/null && echo "sandbox containers removed"

ps: ## Show container status
	$(COMPOSE) ps

tidy: ## go mod tidy inside the Go container
	$(GO) go mod tidy

install: ## pnpm install inside the Node container
	$(NODE) pnpm install

test: test-go test-web ## Run all tests

test-go: ## Go unit tests (uses the yuno_test database)
	$(GO) go test ./... -count=1

test-race: ## Engine package under the race detector (cgo: gcc installed in the throwaway container)
	$(COMPOSE) --profile tools run --rm --no-deps go sh -c 'apk add --no-cache gcc musl-dev >/dev/null 2>&1; CGO_ENABLED=1 go test -race ./internal/engine/ -count=1'

test-integration: ## Live tests against the running gateway (serial: the packages patch gateway config)
	$(GO) go test -tags integration -p 1 ./internal/openclaw/ ./internal/engine/ -run TestIntegration -count=1

test-web: ## Frontend tests
	$(NODE) pnpm test

lint: ## go vet, staticcheck, eslint, tsc
	$(GO) go vet ./...
	$(GO) sh -c 'go run honnef.co/go/tools/cmd/staticcheck@latest ./...'
	$(NODE) pnpm lint

migrate: ## Apply database migrations
	$(COMPOSE) run --rm api migrate

seed: ## Create the demo agents the workflow templates name (templates load at api start)
	$(COMPOSE) run --rm api seed

eval: ## Run the evaluation suite (offline by default: EVAL_PROVIDER=scripted)
	$(COMPOSE) --profile eval build eval
	$(COMPOSE) --profile eval run --rm --no-deps eval

eval-baseline: eval ## Run evals and store the report as the baseline
	@cp "$$(ls -t evals/reports/*.md | head -1)" evals/reports/baseline.md && echo "wrote evals/reports/baseline.md"

eval-final: export EVAL_SPLIT ?= all
eval-final: eval ## Run every split (public and private) and store the report as the final
	@cp "$$(ls -t evals/reports/*.md | head -1)" evals/reports/final.md && echo "wrote evals/reports/final.md"

tunnel: ## Expose the web port through a cloudflared quick tunnel (demo only)
	$(COMPOSE) --profile tunnel up tunnel
