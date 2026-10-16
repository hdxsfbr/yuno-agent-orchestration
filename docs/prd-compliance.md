# Requirements compliance

Audit of the built system against both binding specifications, the portal brief
(`docs/spec/portal-prd.md`, graded version) and the attached PDF
(`docs/spec/attached-pdf.md`). Method: an independent read by a second model with the
project's own documents excluded as evidence, plus the coordinator's pass on
deliverables, numbers and time-based behaviour; disagreements resolved by reading the
code. Status as of the wave-4 fix pass (2026-10-16). Evidence is a file, a test, or a
live check; "Partial" says exactly how it differs.

## Status summary

- Functional requirements of both specs: met, with the deviations disclosed below.
- Deliverables: README (setup, configuration, demo steps, architecture diagram, runtime
  and language justification, how to add a template or a channel), ADR, threat model
  with known limitations, OpenAPI, AI usage log, decisions log, baseline and final eval
  reports, `make test` / `make run` / `make eval`, `.env.example` with names and
  defaults only, commit history. The walkthrough video is recorded last and linked
  from the README.
- Impact metrics: on the public split termination 100 %, approval gate 100 %,
  idempotency 100 %, deterministic pass 100 %, end-to-end 100 % (`evals/reports/
  baseline.md`, 17 cases); the final report over every split is 24 of 24 cases with the
  rubric graded by the model, 4 of 4 at 1.000 (`evals/reports/final.md`); `/api/status` p95
  0.9 ms over 200 requests through nginx (`make status-p95`); no secrets in the
  repository, logs, traces or fixtures (scanned at every commit); 24 versioned split
  cases, 7 deterministic evaluators and 1 rubric evaluator.

## Open items

None blocking. Verified live on 2026-10-16: two messages from Andre's phone delivered
by Telegram over the webhook through the tunnel (Telegram's own address, answered 200),
answered by the bound Coder within five seconds, mirrored into the platform and shown
live on the Channels page during the recording. Still unverified: a reply to the
approval prompt the bot sends in Telegram resolving the approval (the prompt is
delivered; decisions are made in the web UI).

## Fixed from the audit (2026-10-16)

| # | Finding | Fix |
|---|---|---|
| 1 | Webhook verification never exercised | Webhook mode through a public tunnel to a webhook-only listener; config refuses webhook mode without https and a 32-char secret; smoke asserts 401 on a wrong secret and the registered URL (D-032) |
| 2 | API open: the UI sent no token | Token prompt and bearer on every request and stream; OpenAPI bearer scheme (D-031) |
| 3 | Deliverables missing | Docs written, final eval over every split, video recorded and linked |
| 5 | Deleted agents stayed in the runtime; 133 leftovers crashed the gateway | Delete reaches the gateway and the workspace; start-time reconciliation (D-030); gateway heap 3 GB, 4 GB limit, restart policies |
| 6 | A gateway restart broke the stack | Api watchdog exits when its namespace dies; nginx re-resolves; measured recovery about two minutes after an in-place restart |
| 8 | Chat agent had stale memory and workflow instructions | MEMORY.md re-rendered on save and bind; chat instruction in AGENTS.md |
| 9 | Inert conversation-window field | Removed |
| 10 | Limits copy promised agent limits in workflows | Copy corrected (D-021) |
| 12 | One-command paths from a fresh checkout | `make run` seeds; test and eval prepare the env; eval checks the stack is up |
| 15 | Env example gaps and invalid values; names not passed through compose | Every name with a valid default, passed through compose; dead variable removed; test |
| 16 | `state_schema` not enforced at runtime | Validated after every node merge |
| 17, 18 | No channel or live status on the Agents list | Channel column and live state from `channels` and `activity` |
| 22 | OpenRouter keys and run tokens not redacted | Redacted in logs and stored transcripts |
| 26 | Data race in the event hub | Atomic counter; race run clean |
| 27 | `/api/channels` could block 5 s | Bot name cached at start and bind |
| 28 | No OpenAPI security scheme | Added |
| 29 | Postgres published on all interfaces | Bound to localhost |
| 30 | Eval runs recorded as manual | Tagged `eval` |
| 31 | Recalled facts not marked untrusted | Wrapped as untrusted content |
| 32 | README leftovers | Fixed |
| 34 | Unpinned tools | Pinned |
| pre-flight | Seeded agents never pushed; version shown as "dev" | Materialised at start; version from the commit |

## Disclosed deviations (D-033 unless noted)

| # | Spec sentence | What the platform does | Why |
|---|---|---|---|
| 4 | PRD: "approval gate before any risky or side-effecting action" | An operator may set a side-effecting tool to `auto` per agent, with a warning; templates never do | D-009, Andre's choice: the PDF defines interaction rules as "what the agent can do autonomously vs. what requires approval" |
| 7 | PRD: "never store/expose chain-of-thought" | The platform stores and shows no reasoning; OpenClaw keeps its own session transcripts (thinking included) in its state directory | The platform's records are what the brief grades; the runtime's internals are documented in the threat model |
| 11 | Both: schedules per agent | Schedules target workflows; a single-agent schedule uses that agent's quick-run workflow | One scheduler model |
| 13 | PRD: "deterministic local fallback is mandatory; no cloud dependency may be required to evaluate" | `make eval` is fully offline on the scripted model; UI runs need a model key | The evaluation path is the one that must not need the cloud |
| 14 | PRD: evaluation runner | Runs are replayed from scripts; end-to-end success means the expected outcome was reached, including expected failures | Determinism; the live-model run is the demo and the recorded trace |
| 19 | Guardrails | Rate and cost guardrails cover runs, not chat conversations | Conversations are the agent's own sessions; the sender allowlist bounds them |
| 20 | Idempotency | The key includes the node visit; a loop revisit with the same input is a new effect | DESIGN 5.4 edge case, by design |
| 21 | "100 % of runs terminate within configured limits" | The termination evaluator accepts one step past `max_steps` when the reason names the limit; cost may overshoot by one call | The model call that trips the limit is recorded before the abort lands |
| 33 | Agent `effort` field | Accepted, no effect on OpenClaw | README says so |
| minor | Status history in the UI, SSE replay cap of 1000 events, best-effort hand-off write | As is | Documented in the threat model's limitations |

## Requirement table

Legend: Met / Partial / Gap. Evidence is `file:line`, a test, or a live check.

| Requirement (spec) | Status | Evidence |
|---|---|---|
| Agent CRUD with name, role, system prompt, model, tool allowlist, limits, channels (both) | Met | `internal/httpapi/agents.go`, `TestAgentLifecycle`, `web/src/components/agent/AgentForm.tsx` |
| Agent configuration: schedules, memory with retention, skills, interaction rules, guardrails (both) | Met | `internal/sched`, `internal/memory`, `internal/httpapi/skills.go`, `internal/tools/policy.go`, `internal/guardrails`; D-033 for per-agent schedules |
| Workflow builder, visual, with conditions and feedback loops (both) | Met | `web/src/components/workflow/*`, `internal/workflow/cond.go`, `TestDevPipelineEndToEnd` |
| The dev pipeline loop configurable, not hardcoded (PDF) | Met | `templates/dev_pipeline.json` edges; `internal/engine/walk.go` knows no agent names |
| At least two templates to load and modify (PDF) | Met | `templates/dev_pipeline.json`, `templates/research_pipeline.json`, `TestInstantiate…` |
| Manual trigger plus a recurring or delayed schedule (PRD) | Met | `POST /api/runs`, `internal/sched/scheduler.go`, `TestTickFiresCron`, `TestTickDelayFiresOnce` |
| Two or more cooperating reasoning components over shared typed, validated state (PRD) | Met | `walk.go` state merge, output schemas (`tokens.go`), state schema at runtime (wave 4), eval cases 004–006 |
| Agents communicate asynchronously; message history persisted and shown (both) | Met | `agent_messages` rows (`walk.go`), run page Agent messages section |
| 3+ real tools, 2 read-only/computational + 1 approval-gated side-effecting, ≥2 hitting real services (PRD) | Met | `internal/tools/platform.go`: `http_fetch`, `memory_recall` (read-only), `memory_save`, `finish` (computational), `deploy_release`, `gitlab_create_mr` (side-effecting, gated); deploy target, Postgres, sandboxed exec |
| Tools with explicit schemas, timeouts, error handling, untrusted-content handling (PRD) | Met | `tool.go` schema and timeout, `httpfetch.go:47-63` wrapper, `memory_recall` wrapper (wave 4) |
| Human approval/rejection interface enforced before risky actions (both) | Met, deviation 4 | `mcp.go` gate, `walk.go` approval nodes, inbox and run page, `approval_gate` evaluator 100 % |
| Deterministic termination: step and time limits plus cancellation (PRD) | Met, deviation 21 | `limits.go`, `engine.go` cancel, `termination` evaluator 100 % |
| Retry and backoff on tool and model failure (PRD) | Met | `node.go` send and provider retries, `executor.go` transient retries, tests T5.5.2–T5.5.7 |
| Idempotency on side effects (PRD) | Met, deviation 20 | `side_effects.go`, deploy target key, `idempotency` evaluator 100 %, eval 010 |
| External channel with verified inbound webhooks, persisted UI-visible history (PRD) | Met | Webhook mode via tunnel, smoke 401 check and registered URL, `conversations` tables, Channels page |
| Agent responds using its personality, tools and memory from the messaging app (PDF) | Met, partial on platform tools | Bound Coder answered live on its model; OpenClaw built-in tools; MEMORY.md re-rendered on save and bind; platform MCP tools need a run token and are not available in chat (threat model limitation) |
| UI shows which channel each agent is connected to (PDF) | Met | Agents list Channel column, agent page Telegram line, Channels page |
| Live monitoring: agent status, logs, inter-agent messages incl. channel messages, task progress, tokens/cost (PDF) | Met | SSE run and conversation events, Runs and run page, Agents list live state, Channels transcripts |
| Run inspection with correlated traces, tool calls, token and cost metrics (PRD) | Met | `store.Trace`, spans, `RunHeader.tsx`, `Timeline.tsx` |
| Real runtime executing agent logic, not a mockup (both) | Met | OpenClaw `chat.send` per visit (`node.go`), `/api/openclaw/status` live |
| Reproducible evaluation runner: ≥20 versioned split cases, ≥3 deterministic + ≥1 rubric (PRD) | Met | `evals/cases/v1/` 24 cases, 7 evaluators + rubric (`internal/evals`), reports in `evals/reports/` |
| Working end-to-end demo with 2+ agents executing a real task; a human chatting through the channel (both) | Met | Dev pipeline live run in the video; Telegram conversation in the video |
| Web UI, persistence layer, real-time communication (PDF) | Met | `web/`, Postgres migrations 0001–0007, SSE (`sse.go`) |
| One-command local run, no cloud needed to evaluate (both) | Met, deviation 13 | `make run`, `make eval` offline |
| HTTP API with a machine-readable contract (PRD) | Met | `api/openapi.yaml`, `TestOpenAPIConformance` |
| Standard build and dependency management, unit and integration tests (PRD) | Met | `go.mod`, `pnpm-lock.yaml`, `make test`, `make test-integration` |
| Never store or expose chain-of-thought (PRD) | Met, deviation 7 | `node.go` drops reasoning streams; trace strips reasoning parts; T5.7.8 |
| Commit no secrets; .env.example names only (PRD) | Met | `.gitignore`, pre-commit scan, `.env.example` |
| README: architecture diagram, setup, runtime justification, how to add a template or channel (PDF); ADR, threat model, OpenAPI docs, AI_USAGE.md, decisions log, eval report baseline + final, video (PRD) | Met | `README.md` sections, `docs/adr/0001-runtime.md`, `docs/threat-model.md`, `api/openapi.yaml`, `AI_USAGE.md`, `DECISIONS.md`, `evals/reports/baseline.md` and `final.md`, video link in README |
| Clear separation of UI, runtime integration and persistence (PDF) | Met | `web/`, `internal/{httpapi,engine,tools,openclaw}`, `internal/store` |
| Tests for critical paths: agent creation, workflow execution, message delivery (PDF) | Met | `agents_test.go`, `graph_test.go`, `conversations_test.go` |
| API status endpoints under 500 ms at p95 (PRD) | Met | `make status-p95`: p95 0.9 ms |
| Language justified in README/ADR (both) | Met | README "Runtime choice", ADR "Language" |

## Could not verify

- A reply to the approval prompt in Telegram resolving the approval (needs Andre during
  the demo rehearsal).
- A fresh clone on another machine (`make run` timing); the stack has only run here.
