# Yuno Agent Orchestration Platform

A platform where operators configure AI agents (role, prompt, model, tool allowlist,
limits, schedules, memory, guardrails, interaction rules) without code changes, connect
them into multi-agent workflows with branching and feedback loops, and run them on a real
runtime against real tools, with a human approval gate in front of every side effect.
One agent is reachable over Telegram.

Agents execute on OpenClaw; this repository is the control plane around it (workflow
engine, approvals, limits, traces, evals, channel, UI). See `docs/adr/0001-runtime.md`.

Status: wave 1 of 4 (the tracer bullet) is done. Working end to end: agent configuration
in the UI and API, materialised into OpenClaw (config entry, exec policy, workspace
files); single-agent quick runs executed by OpenClaw with `exec` in a Docker sandbox;
the platform MCP server with the typed `finish` tool; the model proxy that keeps the
Anthropic key out of the gateway; a deterministic scripted model for offline tests and
evals; live run traces over SSE with tokens and cost; step, time, cost, tool-call and
cancellation limits; and `make eval` with three cases and a baseline report. Not yet
(waves 2 to 4, `docs/DESIGN.md` section 9): multi-agent workflows and the visual
builder, templates, approvals and the gated side-effect tools, Telegram, memory,
skills, schedules, the full eval suite and the rubric evaluator. This file is kept
truthful at every commit; sections marked "not yet" are not yet.

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
| `make test` | Go unit tests (against the `yuno_test` database) and frontend tests |
| `make test-integration` | Live tests against the running gateway, run serially |
| `make probe-openclaw` | A real OpenClaw turn with a sandboxed `exec`, the integration canary |
| `make eval` | Run the evaluation suite offline with the scripted provider; `make eval-baseline` stores the report |
| `make lint` | `go vet`, `staticcheck`, `eslint`, `tsc` |
| `make logs` | Follow the api logs |
| `make tunnel` | Expose the web port via a cloudflared quick tunnel for Telegram webhooks |
| `make clean` | Stop everything and drop the database volume |

## Configuration

All settings are environment variables, listed with their defaults in `.env.example`.
The model upstream is `MODEL_UPSTREAM=anthropic` with `ANTHROPIC_API_KEY`, or
`MODEL_UPSTREAM=openrouter` with `OPENROUTER_API_KEY`: OpenRouter serves the same Claude
models through the Anthropic messages format, and the proxy rewrites model ids
(`claude-opus-5-5` becomes `anthropic/claude-opus-5.5`) and the auth header, so agents
are configured identically either way.
Secrets never go in the repository; the log handler redacts anything shaped like an API
key, bot token or bearer token (see `internal/obs/redact.go`).

Per-agent model settings: `thinking` is the reasoning control on OpenClaw (the agent's
`thinkingDefault`), and `max_tokens` reaches OpenClaw as the agent's `params.maxTokens`.
OpenClaw has no counterpart to `effort`; the API still accepts the field for
compatibility, but it changes nothing and the agent form no longer offers it.

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
