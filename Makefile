# Every target runs inside Docker. Nothing here needs a toolchain on the host beyond
# docker, make and curl.

COMPOSE      := docker compose
GO           := $(COMPOSE) --profile tools run --rm go
GO_CI        := $(COMPOSE) --profile tools run --rm go-ci
NODE         := $(COMPOSE) --profile tools run --rm node
WEB_PORT     ?= 8080

.DEFAULT_GOAL := help

.PHONY: help env run smoke probe-openclaw probe-deliver probe-sandbox stop down clean logs ps test test-go test-web web-deps \
	ci-up ci-test ci-lint ci-clean \
        test-integration lint tidy install migrate seed seed-reset eval eval-baseline eval-final \
        tunnel build openclaw-shell openclaw-logs openclaw-validate prune-sandboxes

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

env: ## Create .env from the example and generate missing platform tokens
	@test -f .env || cp .env.example .env
	@./scripts/ensure-env.sh
	@mkdir -p data/openclaw

# Images carry the commit they were built from (/api/status and the Status page show it).
run build: export VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

run: env ## Build and start everything, wait for ready, seed the demo agents, print the URL
	$(COMPOSE) --profile build build sandbox-image sandbox-image-python sandbox-image-node sandbox-image-go
	$(COMPOSE) up --build -d postgres deploytarget openclaw api web
	@echo "waiting for the stack..."
	@for i in $$(seq 1 120); do \
	  if curl -fsS -m 2 http://localhost:$(WEB_PORT)/readyz >/dev/null 2>&1; then exit 0; fi; sleep 1; done; \
	  echo "stack did not become ready; see: make ps, make logs, make openclaw-logs"; $(COMPOSE) ps -a; exit 1
	$(COMPOSE) run --rm api seed
	@echo "ready: http://localhost:$(WEB_PORT)"

smoke: ## Hit every service in the running stack and check isolation (scripts/smoke.sh)
	@WEB_PORT=$(WEB_PORT) ./scripts/smoke.sh

probe-openclaw: ## Run the OpenClaw integration canary against the running gateway
	$(COMPOSE) --profile tools run --rm probe

probe-approval: ## Verify the gateway's plugin approval RPCs the side-effect gate uses (DESIGN 5.3 rule 5)
	$(COMPOSE) --profile tools run --rm --entrypoint node probe /scripts/openclaw-probe-approval.mjs

probe-deliver: ## Verify chat.send {deliver: true} reaches Telegram (sends one message; needs a Telegram session)
	$(COMPOSE) --profile tools run --rm --entrypoint node probe /scripts/openclaw-probe-deliver.mjs

probe-sandbox: ## Verify a per-agent sandbox image and network override on the running gateway (DESIGN 5.16 rule 7)
	$(COMPOSE) --profile tools run --rm --no-deps --entrypoint node probe /scripts/openclaw-probe-sandbox.mjs

build: ## Build images without starting
	$(COMPOSE) --profile build build

stop: ## Stop containers, keep data
	$(COMPOSE) stop

down: ## Stop and remove containers, keep volumes and ./data
	$(COMPOSE) --profile tools --profile eval --profile tunnel --profile build down --remove-orphans

clean: ## Stop and remove containers, volumes and ./data (drops the database and OpenClaw state)
	$(COMPOSE) --profile tools --profile eval --profile tunnel --profile build down --remove-orphans --volumes
	@docker ps -aq --filter "name=openclaw-sbx-" | xargs -r docker rm -f
	@# The gateway's sandbox leaves root-owned directories under data/openclaw; remove as root in a container.
	docker run --rm -v "$(CURDIR)/data:/data" alpine:3.20 rm -rf /data/openclaw

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

test: env test-go test-web ## Run all tests

test-go: env ## Go unit tests (uses the yuno_test database)
	$(GO) go test ./... -count=1

test-race: ## Engine and event hub under the race detector (cgo: gcc installed in the throwaway container)
	$(COMPOSE) --profile tools run --rm --no-deps go sh -c 'apk add --no-cache gcc musl-dev >/dev/null 2>&1; CGO_ENABLED=1 go test -race ./internal/engine/ ./internal/events/ -count=1'

test-integration: ## Live tests against the running gateway (serial: the packages patch gateway config)
	$(GO) go test -tags integration -p 1 ./internal/openclaw/ ./internal/engine/ -run TestIntegration -count=1

web-deps: ## Install the UI's dependencies into web/node_modules (pnpm, frozen lockfile; a no-op when current)
	$(NODE) pnpm install --frozen-lockfile --prefer-offline

test-web: env web-deps ## Frontend tests
	$(NODE) pnpm test

lint: web-deps ## go vet, staticcheck, eslint, tsc
	$(GO) go vet ./...
	$(GO) sh -c 'go run honnef.co/go/tools/cmd/staticcheck@2026.2.2 ./...'
	$(NODE) pnpm lint

# The CI test job (.gitlab-ci.yml): the suites and lint against postgres alone. The unit
# tests use a fake gateway, so nothing of OpenClaw, the sandbox images or the smoke test
# runs there (the deploy job runs `make smoke` on the droplet). The Go module and build
# caches are named volumes that ci-clean keeps between jobs.
ci-up: env ## CI: start postgres only and wait for it
	$(COMPOSE) up -d --wait postgres

ci-test: env ## CI: what `make test` runs, against postgres alone
	$(GO_CI) go test ./... -count=1
	$(MAKE) test-web

ci-lint: web-deps ## CI: what `make lint` runs, against postgres alone
	$(GO_CI) go vet ./...
	$(GO_CI) sh -c 'go run honnef.co/go/tools/cmd/staticcheck@2026.2.2 ./...'
	$(NODE) pnpm lint

ci-clean: ## CI: remove the containers and the data volumes; keep the Go module and build caches
	$(COMPOSE) --profile tools down --remove-orphans
	@docker volume rm -f yuno_pgdata yuno_artifacts yuno_releases >/dev/null 2>&1 || true

migrate: ## Apply database migrations
	$(COMPOSE) run --rm api migrate

seed: ## Create the demo agents, the Chief of Staff, both template workflows, and bind the Chief to Telegram when nothing is bound (idempotent)
	$(COMPOSE) run --rm api seed

seed-reset: ## seed, then restore the shipped agents' prompts and allowlists (and the Claude model of one moved to OpenAI) and the template workflows to the shipped versions (keeps everything else)
	$(COMPOSE) run --rm api seed --reset-shipped

# The fake forge (DESIGN 5.29 rule 17): started for the eval targets only. The api accepts its
# http URL as a project's repository only while EVAL_FORGE_URL names it, so the api is
# recreated with the setting here; make run recreates it without.
eval: export EVAL_FORGE_URL ?= http://forgefake:8090
# The wave loop (case 028) waits on three of the api's own loops: the merge request sync, the
# issue intake and the wave watcher. Their defaults (120, 120 and 60 s) would stretch the case
# to many minutes, so the eval run sets them to 5 s when it recreates the api; make run restores
# the defaults.
eval: export MERGE_REQUEST_SYNC_INTERVAL ?= 5s
eval: export INTAKE_POLL_INTERVAL ?= 5s
eval: export WAVE_WATCH_INTERVAL ?= 5s
eval: env ## Run the evaluation suite against the running stack (offline by default: EVAL_PROVIDER=scripted)
	@curl -fsS -m 3 http://localhost:$(WEB_PORT)/readyz >/dev/null 2>&1 || { echo 'the stack is not running: run `make run` first'; exit 1; }
	$(COMPOSE) --profile eval build eval forgefake
	$(COMPOSE) --profile eval up -d --wait forgefake
	$(COMPOSE) --profile eval up -d --wait --no-deps api
	$(COMPOSE) --profile eval run --rm --no-deps eval

eval-baseline: eval ## Run evals and store the report as the baseline
	@cp "$$(ls -t evals/reports/*.md | head -1)" evals/reports/baseline.md && echo "wrote evals/reports/baseline.md"

eval-final: export EVAL_SPLIT ?= all
eval-final: eval ## Run every split (public and private) and store the report as the final
	@cp "$$(ls -t evals/reports/*.md | head -1)" evals/reports/final.md && echo "wrote evals/reports/final.md"

tunnel: ## Expose the web port through a cloudflared quick tunnel (demo only)
	$(COMPOSE) --profile tunnel up tunnel

# ---- public deployment (docs/deploy.md) ----------------------------------------------------
# A plain Docker host (a DigitalOcean droplet) runs the same compose file plus Caddy for TLS.
# scripts/deploy/ holds the droplet creation, bootstrap, deploy and runner scripts.

.PHONY: build-public run-public caddy-logs deploy

build-public: export COMPOSE_PROFILES = public
build-public: env ## Build every image run-public uses, without replacing a running container
	$(COMPOSE) --profile build build sandbox-image sandbox-image-python sandbox-image-node sandbox-image-go
	$(COMPOSE) build deploytarget openclaw api web
	$(COMPOSE) pull --quiet postgres caddy || true

run-public: export COMPOSE_PROFILES = public
run-public: ## run, plus Caddy TLS for DOMAIN (needs DOMAIN in .env; WEB_BIND=127.0.0.1 on a public host)
	@grep -qE '^DOMAIN=.+' .env || { echo "DOMAIN is not set in .env (see docs/deploy.md)"; exit 1; }
	@grep -qE '^WEB_BIND=127\.0\.0\.1' .env || echo "warning: WEB_BIND is not 127.0.0.1, the plain-http port $(WEB_PORT) is reachable from outside"
	$(MAKE) run
	$(COMPOSE) up -d caddy
	@dom=$$(grep -E '^DOMAIN=' .env | cut -d= -f2-); echo "waiting for https://$$dom ..."; \
	for i in $$(seq 1 60); do \
	  if curl -fsS -m 5 "https://$$dom/readyz" >/dev/null 2>&1; then echo "ready: https://$$dom"; exit 0; fi; sleep 2; done; \
	echo "https://$$dom is not answering yet (certificate issuance can take a minute): make caddy-logs"; exit 1

caddy-logs: ## Follow the TLS proxy's logs
	$(COMPOSE) --profile public logs -f caddy

deploy: ## Deploy REF (default main) to HOST over SSH: HOST=1.2.3.4 [REF=main] [ENV_FILE=.env.prod] [DEPLOY_USER=yuno]
	@test -n "$(HOST)" || { echo "usage: make deploy HOST=<ip or name> [REF=main] [ENV_FILE=path] [DEPLOY_USER=yuno]"; exit 2; }
	./scripts/deploy/deploy.sh --host "$(HOST)" --ref "$(or $(REF),main)" --user "$(or $(DEPLOY_USER),yuno)" $(if $(ENV_FILE),--env-file "$(ENV_FILE)")

status-p95: ## Measure GET /api/status latency over 200 requests (p95 must stay under 500 ms)
	./scripts/status-p95.sh 200

# ---- demo video (scripts/demo/README.md) ------------------------------------------------------
# Footage is recorded in a Playwright container and stitched in an ffmpeg container; nothing here
# needs a browser or ffmpeg on the host. SCENES=04,06 picks scenes; LIVE=1 lets them change state.

DEMO_DIR := scripts/demo

.PHONY: demo-record demo-stitch demo-clean

demo-record: ## Record the demo scenes in a browser container (SCENES=04,06 to pick; LIVE=1 changes state)
	@mkdir -p $(DEMO_DIR)/out
	HOST_UID=$$(id -u) HOST_GID=$$(id -g) \
	DEMO_COMMIT="$${DEMO_COMMIT:-$$(git rev-parse --short HEAD)}" \
	DEMO_REPO_URL="$$(git remote get-url origin 2>/dev/null | sed -E 's#^(https?://)[^@/]*@#\1#; s#\.git$$##')" \
	$(COMPOSE) --profile tools run --rm -T --no-deps demo

demo-stitch: ## Cut the recorded waits, mux narration, write scripts/demo/out/demo.mp4 (ffmpeg in a container)
	@./$(DEMO_DIR)/stitch.sh

demo-clean: ## Delete recorded footage, stills and cuts (scripts/demo/out); the cards and narration stay
	rm -rf $(DEMO_DIR)/out

demo-narrate: ## Voice the demo narration (scripts/demo/narration/*.txt) with Chatterbox and the operator's reference voice (GPU, ~/code/chatterbox-play)
	./scripts/demo/narrate-chatterbox.sh

omm-view: ## Serve the architecture diagrams under .omm/ at http://<host>:$${OMM_PORT:-3030} (oh-my-mermaid viewer, in a container)
	HOST_UID=$$(id -u) HOST_GID=$$(id -g) $(COMPOSE) --profile tools run --rm --no-deps --service-ports omm view

omm-scan: ## Regenerate .omm/ architecture docs: run /omm-scan in Claude Code; this target only validates what exists
	./scripts/omm.sh validate

