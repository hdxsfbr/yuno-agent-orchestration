# Yuno Agent Orchestration Platform

A platform where operators configure AI agents (role, prompt, model, tool allowlist,
limits, schedules, memory, guardrails, interaction rules) without code changes, connect
them into multi-agent workflows with branching and feedback loops, and run them on a real
runtime against real tools, with a human approval gate in front of every side effect.
One agent is reachable over Telegram.

Agents execute on OpenClaw; this repository is the control plane around it (workflow
engine, approvals, limits, traces, evals, channel, UI). See `docs/adr/0001-runtime.md`.

Status: waves 1 and 2 of 4 are done. Working end to end: agent configuration in the UI
and API, materialised into OpenClaw (config entry, exec policy, workspace files);
multi-agent workflows as graphs with conditions, feedback loops and typed state, built
in the visual builder or loaded from the two shipped templates; agent turns executed by
OpenClaw with `exec` in a Docker sandbox and the run directory handed forward from agent
to agent; the platform tools over MCP (`finish`, `deploy_release`, `http_fetch`,
`memory_save`, `memory_recall`, and `gitlab_create_mr` when configured) with
idempotency keys and a human approval gate in front of every side effect, answered from
the approvals inbox or the run page; approval nodes in workflows; exec approvals from
the gateway; retries; live traces over SSE with tokens, cost, agent messages and state;
step, time, cost, tool-call, loop and cancellation limits; `make eval` with twelve
public cases, seven evaluators and a baseline report. Not yet (waves 3 and 4,
`docs/DESIGN.md` section 9): Telegram, agent memory in the prompt, skills, schedules,
guardrails, the full eval suite with the rubric evaluator, the final docs. This file is
kept truthful at every commit; sections marked "not yet" are not yet.

## Run it

Requirements on the host: Docker (with compose v2) with a usable Docker socket, `make`
and `curl`. Nothing else is installed on the host; every toolchain runs in a container.
OpenClaw needs the socket to create agent sandboxes as sibling containers, and keeps its
state and agent workspaces under `./data/openclaw` (git-ignored).

```bash
cp .env.example .env     # fill in ANTHROPIC_API_KEY to use a real model; optional otherwise
make run                 # generates platform tokens, builds images, starts postgres,
                         # openclaw, deploytarget, api, web
make seed                # the six demo agents the workflow templates name
make smoke               # every service answers; gateway, socket and sandbox image checked
make probe-openclaw      # a real OpenClaw turn with a sandboxed exec, through the gateway
```

The api shares the gateway container's network namespace, so if the `openclaw`
container is ever recreated on its own, recreate `api` too (`make run` does both).

Then open http://localhost:8080. Agents lists the seeded agents, Workflows has the two
templates ("From template" makes an editable copy; Run asks for the input), Approvals is
the inbox, Runs shows every run with its live trace. `make help` lists every target. The
ones that matter:

| Target | What it does |
|---|---|
| `make run` | Build and start the stack, wait for readiness, print the URL |
| `make smoke` | Hit every service in the running stack and check the sandbox is isolated |
| `make test` | Go unit tests (against the `yuno_test` database) and frontend tests |
| `make test-integration` | Live tests against the running gateway, run serially |
| `make test-race` | The engine package under the race detector (installs gcc in the throwaway tools container) |
| `make probe-openclaw` | A real OpenClaw turn with a sandboxed `exec`, the integration canary |
| `make probe-approval` | Verifies the gateway's plugin approval calls the side-effect gate relies on |
| `make seed` | Creates the six demo agents the workflow templates name (Coder, Reviewer, Deployer, Researcher, Critic, Writer); idempotent |
| `make eval` | Run the evaluation suite offline with the scripted provider; `make eval-baseline` stores the report |
| `make lint` | `go vet`, `staticcheck`, `eslint`, `tsc` |
| `scripts/mutation-check.sh` | Breaks a line of code, runs a test command, restores the file, and says whether the test caught it |
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
Tool settings: `DEPLOYTARGET_URL` (the local release store `deploy_release` publishes
to), `HTTP_FETCH_ALLOWLIST` (hosts `http_fetch` may reach; empty means none),
`GITLAB_BASE_URL`, `GITLAB_PROJECT_ID` and `GITLAB_TOKEN` (all three register
`gitlab_create_mr`), `MCP_TOOL_WAIT_SECONDS` (how long a gated tool call waits for the
human before the agent continues with `pending_approval`, default 540), `TOOL_OUTPUT_MAX_BYTES`. The memory
tools need the agent's `memory_policy.enabled`.
Secrets never go in the repository; the log handler redacts anything shaped like an API
key, bot token or bearer token (see `internal/obs/redact.go`).

## Tools

Platform tools are served to OpenClaw over MCP from the api (`/mcp`, bearer token) and
reach the model as `yuno__<name>`; every call carries the run token, is checked against
the agent's allowlist and interaction rules, and is recorded with its full input and
output. OpenClaw's built-ins run inside the agent's Docker sandbox.

| Tool | Risk | Talks to | Timeout | Input |
|---|---|---|---|---|
| `finish` | computational | postgres | 5 s | `{run_token, result}` validated against the node's output schema |
| `deploy_release` | side-effecting, approval by default | deploy target | 30 s + approval wait | `{run_token, version, notes, paths[]}` from `runs/<run id>/` |
| `gitlab_create_mr` | side-effecting, approval by default | GitLab (when configured) | 30 s + approval wait | `{run_token, title, description, paths[]}` |
| `http_fetch` | read-only | allowlisted https hosts | 15 s | `{run_token, url}`; output wrapped as untrusted content |
| `memory_save` | computational | postgres | 5 s | `{run_token, key, value, ttl_days?}` |
| `memory_recall` | read-only | postgres | 5 s | `{run_token, query}` |
| `read`, `write`, `edit`, `apply_patch`, `ls`, `exec`, `process` | OpenClaw built-ins | the sandbox | OpenClaw's | |

Side effects are idempotent: the key is a hash of run, node visit, tool and canonical
input; a repeat replays the stored result and says so (`replayed: true`), and the deploy
target enforces the same key on its side.

## Workflows and approvals

A workflow is a graph of `start`, `agent`, `approval` and `end` nodes with edges that
carry optional conditions over the typed run state (`/reviewer/verdict eq approved`);
edges are tried in order and the first true one is taken, so a feedback loop is just an
edge back, bounded by `max_loop_visits`. Each agent node's task is a template over the
run input and the state so far; the agent's `finish` result is validated against the
node's output schema and becomes `state[<node>]`. The shipped templates live in
`templates/` (`dev_pipeline`: Coder → Reviewer ⇄ feedback → human approval → Deployer;
`research_pipeline`: Researcher → Critic ⇄ feedback → Writer). To add one, drop a JSON
file there with `key`, `name`, `description` and `definition`, naming agents by
`agent_name`; it loads at the next api start, and "From template" resolves the names to
your agents.

Approvals come in three kinds and share one inbox: a side-effecting tool call (the MCP
call blocks up to `MCP_TOOL_WAIT_SECONDS`; approved runs it in the same turn, rejected tells the
agent, and a late approval is executed by the platform with the same idempotency key), a
workflow `approval` node (the run pauses and releases its worker), and an OpenClaw exec
approval for a command outside the agent's policy. Every approval is also registered
with the gateway so OpenClaw's own surfaces can answer it.

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
internal/workflow/  graph definition, validation, conditions, task templates
internal/tools/     platform tools served over MCP, idempotent executor, policy
internal/seed/      the six demo agents (`yuno seed`)
internal/model/     model proxy, scripted offline provider, price table
internal/store/     pgx repositories and embedded SQL migrations
internal/deploytarget/ local release store the deploy tool publishes to
internal/events/    in-process event hub behind SSE
internal/obs/       logging and redaction
internal/pathcheck/ relative-path validation shared by tools and the deploy target
api/openapi.yaml    the HTTP contract, served at /openapi.yaml and validated in tests
web/                React + Vite + TypeScript UI
docker/             Dockerfiles (api, web, the OpenClaw sandbox image) and nginx config
scripts/            smoke.sh, openclaw-probe.mjs, openclaw-probe-approval.mjs, ensure-env.sh
templates/          the two workflow templates, loaded at api start
evals/              versioned eval cases (twelve public, two private) and reports
docs/briefs/        the briefs the build agents worked from, per wave
docs/reviews/       independent review findings and dispositions, per wave
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
