# Yuno Agent Orchestration Platform

A platform where operators configure AI agents (role, prompt, model, tool allowlist,
limits, schedules, memory, guardrails, interaction rules) without code changes, connect
them into multi-agent workflows with branching and feedback loops, and run them on a real
runtime against real tools, with a human approval gate in front of every side effect.
One agent is reachable over Telegram.

Agents execute on OpenClaw; this repository is the control plane around it (workflow
engine, approvals, limits, traces, evals, channel, UI). See `docs/adr/0001-runtime.md`.

Status: scaffold. The OpenClaw integration, run engine, agents and UI land in the waves
described in `docs/DESIGN.md` section 9. This file is kept truthful at every commit;
sections marked "not yet" are not yet.

## Run it

Requirements on the host: Docker (with compose v2) with a usable Docker socket, `make`
and `curl`. Nothing else is installed on the host; every toolchain runs in a container.
OpenClaw needs the socket to create agent sandboxes as sibling containers, and keeps its
state and agent workspaces under `./data/openclaw` (git-ignored).

```bash
cp .env.example .env     # fill in ANTHROPIC_API_KEY to use a real model; optional otherwise
make run                 # generates platform tokens, builds images, starts postgres,
                         # openclaw, deploytarget, api, web
make smoke               # every service answers; gateway, socket and sandbox image checked
make probe-openclaw      # a real OpenClaw turn with a sandboxed exec, through the gateway
```

Then open http://localhost:8080. `make help` lists every target. The ones that matter:

| Target | What it does |
|---|---|
| `make run` | Build and start the stack, wait for readiness, print the URL |
| `make smoke` | Hit every service in the running stack and check the sandbox is isolated |
| `make test` | Go unit and integration tests (against the `yuno_test` database) and frontend tests |
| `make eval` | Run the evaluation suite offline with the scripted provider (not yet) |
| `make lint` | `go vet`, `staticcheck`, `eslint`, `tsc` |
| `make logs` | Follow the api logs |
| `make tunnel` | Expose the web port via a cloudflared quick tunnel for Telegram webhooks |
| `make clean` | Stop everything and drop the database volume |

## Configuration

All settings are environment variables, listed with their defaults in `.env.example`.
Secrets never go in the repository; the log handler redacts anything shaped like an API
key, bot token or bearer token (see `internal/obs/redact.go`).

## Layout

```
cmd/yuno/           one binary: api | migrate | seed | openclaw-init | deploytarget | eval
internal/httpapi/   HTTP routes, middleware, SSE, MCP and model-proxy endpoints
internal/engine/    run state machine, limits, approvals, resumption
internal/openclaw/  baseline gateway config (openclaw-init) and the gateway client
internal/workflow/  graph definition, validation, conditions (not yet)
internal/tools/     platform tools served over MCP
internal/model/     model proxy, scripted offline provider, price table
internal/store/     pgx repositories and embedded SQL migrations
internal/deploytarget/ local release store the deploy tool publishes to
internal/events/    in-process event hub behind SSE
internal/obs/       logging and redaction
internal/pathcheck/ relative-path validation shared by tools and the deploy target
api/openapi.yaml    the HTTP contract, served at /openapi.yaml and validated in tests
web/                React + Vite + TypeScript UI
docker/             Dockerfiles (api, web, the OpenClaw sandbox image) and nginx config
scripts/            smoke.sh, openclaw-probe.mjs, ensure-env.sh
templates/          workflow templates loaded at startup
evals/              versioned eval cases and reports
data/openclaw/      OpenClaw state and agent workspaces (created by make run, git-ignored)
docs/DESIGN.md      the design the code is built from, rule by rule
docs/adr/           architecture decision records
DECISIONS.md        assumptions and decisions log
AI_USAGE.md         how AI was used to build this
```

## Architecture, runtime choice, threat model, evaluation report, demo

Not yet. Each lands with its wave; the final README will carry the architecture diagram,
the runtime justification (summarised from `docs/adr/0001-runtime.md`), the threat model
and known limitations, the tool table, and how to add a workflow template or a messaging
channel.
