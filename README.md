# Yuno Agent Orchestration Platform

A platform where operators configure AI agents (role, prompt, model, tool allowlist,
limits, schedules, memory, guardrails, interaction rules) without code changes, connect
them into multi-agent workflows with branching and feedback loops, and run them on a real
runtime against real tools, with a human approval gate in front of every side effect.
One agent is reachable over Telegram.

Agents execute on OpenClaw; this repository is the control plane around it (workflow
engine, approvals, limits, traces, evals, channel, UI). See `docs/adr/0001-runtime.md`.

Status: waves 1, 2 and 3 of 4 are done. Working end to end: agent configuration in the UI
and API, materialised into OpenClaw (config entry, exec policy, workspace files);
multi-agent workflows as graphs with conditions, feedback loops and typed state, built
in the visual builder or loaded from the two shipped templates; agent turns executed by
OpenClaw with `exec` in a Docker sandbox and the run directory handed forward from agent
to agent; the platform tools over MCP (`finish`, `deploy_release`, `http_fetch`,
`memory_save`, `memory_recall`, and `gitlab_create_mr` when configured) with
idempotency keys and a human approval gate in front of every side effect, answered from
the approvals inbox or the run page; approval nodes in workflows; exec approvals from
the gateway; retries; live traces over SSE with tokens, cost, agent messages and state;
step, time, cost, tool-call, loop and cancellation limits; one agent reachable over
Telegram through OpenClaw's native channel, with the conversation mirrored into the
platform and approvals from chat sessions in the inbox; cron and delay schedules;
per-agent guardrails (cost cap, runs per hour, blocked tools, input patterns); agent
memory with a retention policy, rendered into the agent's memory file; skills; and
`make eval` with seventeen public cases, seven evaluators and a rubric evaluator in
heuristic and model modes. Not yet (wave 4, `docs/DESIGN.md` section 9): the final
documentation (architecture diagram, threat model, demo script) and the last
polish. This file is kept truthful at every commit; sections marked "not yet" are not
yet.

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
the inbox, Runs shows every run with its live trace, Channels binds one agent to Telegram
and shows the conversations, Schedules fires workflows on a cron or once (each firing is
recorded once per occurrence; a once-schedule that fired needs a new time to run again),
Skills holds the markdown skills agents can list, and Status shows the gateway and the
scheduler's next due time. `make help` lists every target. The
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
Telegram: set `TELEGRAM_MODE=polling` (or `webhook` with `PUBLIC_BASE_URL` from `make
tunnel` and a `TELEGRAM_WEBHOOK_SECRET`), `TELEGRAM_BOT_TOKEN` from BotFather and
`TELEGRAM_ALLOWED_USER_IDS` (numeric ids; only these users can talk to the bot or
approve from chat). The channel stays closed until an agent is bound on the Channels page
or with `PUT /api/channels/telegram/binding`; binding restarts the gateway in place
(about five seconds). Conversations are the agent's own sessions, not runs; they appear
under Channels with their transcript.

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

Guardrails per agent: a cost cap per run (the run's limit becomes the minimum across its
agents), runs per hour (429 at trigger, recorded on a schedule's last error), blocked
tools (removed from the agent's tool list and refused by the gate), and input patterns
(RE2 over the tool input; a match is refused with `input blocked by guardrail <name>`).
An agent with patterns on `exec` runs host commands only through an exec approval (the
platform denies the ones a pattern matches, the rest reach the inbox); the sandbox is the
one place OpenClaw does not gate, so a matching command inside it is not blocked (D-028).
Memory: with `memory_policy.enabled`, `memory_save` and `memory_recall` keep up to
`max_facts` facts (default 200, 30-day TTL, oldest evicted) and the 20 newest are written
into the agent's `MEMORY.md` before each turn.

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
internal/channels/  Telegram binding and the keeper that re-asserts it on the gateway
internal/sched/     cron parsing and the scheduler tick
internal/guardrails/ cost cap, rate limit, blocked tools, input patterns
internal/memory/    facts, retention sweeper, MEMORY.md rendering
internal/model/     model proxy, scripted offline provider, price table
internal/store/     pgx repositories and embedded SQL migrations
internal/deploytarget/ local release store the deploy tool publishes to
internal/events/    in-process event hub behind SSE
internal/obs/       logging and redaction
internal/pathcheck/ relative-path validation shared by tools and the deploy target
api/openapi.yaml    the HTTP contract, served at /openapi.yaml and validated in tests
web/                React + Vite + TypeScript UI
docker/             Dockerfiles (api, web, the OpenClaw sandbox image) and nginx config
scripts/            smoke.sh, the three gateway probes (turn, approval, telegram), ensure-env.sh, mutation-check.sh
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
