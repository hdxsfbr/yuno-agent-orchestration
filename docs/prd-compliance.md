# Requirements compliance

Audit of the built system against both binding specifications: the portal brief
(`docs/spec/portal-prd.md`, version 1 of 2026-10-12, the graded version, re-read from the
portal on 2026-10-19) and the attached PDF (`docs/spec/attached-pdf.md`). Method, on
2026-10-19 after wave 11: two independent Opus auditors, one per spec, each fanning out to
read-only sub-audits, with the project's own documents excluded as evidence; the
coordinator's own pass on deliverables, the OpenAPI contract against the router (67
operations, all matching) and a live `make eval` on the current build; every
disagreement settled by reading the code. Evidence is a file, a test, or a live check.

## Status summary

- Functional requirements of both specs: met, with the deviations disclosed below.
- Evaluation on this build (2026-10-19, scripted model, every split): 24 of 24 cases,
  every evaluator 1.000, 15 of 15 cases that expect success succeeded
  (`evals/reports/final.md`). The public split alone passed 17 of 17 on three runs in a
  row after the fix below.
- Deliverables: README, ADR, threat model with known limitations, OpenAPI, AI usage log,
  decisions log, baseline and final eval reports, `make test` / `make run` / `make eval`,
  `.env.example`, commit history, and the walkthrough video `docs/demo/yuno-demo.mp4`
  (8 min 27 s, recorded 2026-10-19 against the public instance).

## Open items

None blocking.

## Fixed on 2026-10-19 (this audit)

| Finding | Fix |
|---|---|
| `make eval` at 6 of 17: the gateway answered every config.patch with "a recovery restart is required" after agents were deleted and created in quick succession, so agent saves returned 502 | The client requests the restart, confirms the patch with `config.get`, and every gateway call waits out a restart (`internal/openclaw/patch_recovery.go`, `restart_wait.go`, tests) |
| Editing an agent blanked its `MEMORY.md` until the next re-render (the Telegram agent answered without its facts) | Memory re-rendered after every materialize (`TestMaterializeRendersMemoryFacts`) |
| Memory on did not offer the memory tools | Added when the policy is on (`TestToolListsMemoryOnAddsMemoryTools`) |
| `run_status` output and provider error bodies reached the model unmarked | Wrapped as untrusted (`chief_test.go`, `httperr_test.go`) |
| Agent `channels` field stored but unused | Read-only, derived from the binding (`TestAgentChannelsAreReadOnly`) |
| "End-to-end success" counted expected failures | Reports show "expected outcome reached" and "succeeded (cases expecting success)", by split |
| README, ADR and threat model statements out of date (status, template and case counts, the allowlist default, node kinds, layout, the add-a-channel and add-a-template steps, sandbox and Telegram wording, the video) | Rewritten against the code; the ten-scene video replaces the wave 4 one |

## Fixed earlier (wave 4 audit, 2026-10-16)

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
| 35 | PDF: "Agent B picks it up" | Agent B reads agent A's output from the run's shared, validated state in the same worker; `agent_messages` is the persisted trail of each hand-off, not a queue B reads | One state model for loops, forks and resumption (DESIGN 5.3); the run itself is queued and claimed |
| 36 | Both: memory with retention on the channel | An expired conversation is deleted from Postgres; the gateway keeps its own Telegram session | The gateway owns chat sessions; facts memory expires on both sides |
| 37 | PRD: "repeated side-effect requests produce at most one effect" | An operator retry from a failed step, or a run resumed after an api restart, starts a new visit and can repeat a `deploy_release`; merge requests stay one per run (lock and unique index) | The key includes the visit by design (deviation 20); the threat model states the limit |
| 38 | PRD: approval gate | Without `API_TOKEN` (the local default) the API, approvals included, is open to anyone who can reach it; every non-local host sets it | Single-operator local setup; the README and threat model say so |
| 39 | Chief of Staff on Telegram | `run_status` looks a run up by its id; there is no tool to list recent runs | Not in either brief; found while recording the demo |



## Requirement table

Legend: Met / Partial / Gap. Evidence is `file:line`, a test, or a live check.

| Requirement (spec) | Status | Evidence |
|---|---|---|
| Agent CRUD with name, role, system prompt, model, tool allowlist, limits, channels (both) | Met (channels read-only, derived from the binding made on the Channels page) | `internal/httpapi/agents.go`, `TestAgentLifecycle`, `web/src/components/agent/AgentForm.tsx` |
| Agent configuration: schedules, memory with retention, skills, interaction rules, guardrails (both) | Met, deviations 11, 19, 36 | `internal/sched`, `internal/memory`, `internal/httpapi/skills.go`, `internal/tools/policy.go`, `internal/guardrails`; D-033 for per-agent schedules |
| Workflow builder, visual, with conditions and feedback loops (both) | Met | `web/src/components/workflow/*`, `internal/workflow/cond.go`, `TestDevPipelineEndToEnd` |
| The dev pipeline loop configurable, not hardcoded (PDF) | Met | `templates/dev_pipeline.json` edges; `internal/engine/walk.go` knows no agent names |
| At least two templates to load and modify (PDF) | Met | `templates/dev_pipeline.json`, `templates/research_pipeline.json`, `templates/review_panel.json`, `TestInstantiate…` |
| Manual trigger plus a recurring or delayed schedule (PRD) | Met | `POST /api/runs`, `internal/sched/scheduler.go`, `TestTickFiresCron`, `TestTickDelayFiresOnce` |
| Two or more cooperating reasoning components over shared typed, validated state (PRD) | Met | `walk.go` state merge, output schemas (`tokens.go`), state schema at runtime (wave 4), eval cases 004–006 |
| Agents communicate asynchronously; message history persisted and shown (both) | Met, deviation 35 | `agent_messages` rows (`walk.go`), run page Agent messages section |
| 3+ real tools, 2 read-only/computational + 1 approval-gated side-effecting, ≥2 hitting real services (PRD) | Met | `internal/tools/platform.go`: `http_fetch`, `memory_recall` (read-only), `memory_save`, `finish` (computational), `deploy_release`, `gitlab_create_mr` (side-effecting, gated); deploy target, Postgres, sandboxed exec |
| Tools with explicit schemas, timeouts, error handling, untrusted-content handling (PRD) | Met (run_status and provider error bodies wrapped, 2026-10-19) | `tool.go` schema and timeout, `httpfetch.go:47-63` wrapper, `memory_recall` wrapper (wave 4) |
| Human approval/rejection interface enforced before risky actions (both) | Met, deviation 4 | `mcp.go` gate, `walk.go` approval nodes, inbox and run page, `approval_gate` evaluator 100 % |
| Deterministic termination: step and time limits plus cancellation (PRD) | Met, deviation 21 | `limits.go`, `engine.go` cancel, `termination` evaluator 100 % |
| Retry and backoff on tool and model failure (PRD) | Met | `node.go` send and provider retries, `executor.go` transient retries, tests T5.5.2–T5.5.7 |
| Idempotency on side effects (PRD) | Met, deviations 20, 37 | `side_effects.go`, deploy target key, `idempotency` evaluator 100 %, eval 010 |
| External channel with verified inbound webhooks, persisted UI-visible history (PRD) | Met | Webhook mode via tunnel, smoke 401 check and registered URL, `conversations` tables, Channels page |
| Agent responds using its personality, tools and memory from the messaging app (PDF) | Met, partial on platform tools | Bound Coder answered live on its model; OpenClaw built-in tools; MEMORY.md re-rendered on save and bind; platform MCP tools need a run token and are not available in chat (threat model limitation) |
| UI shows which channel each agent is connected to (PDF) | Met | Agents list Channel column, agent page Telegram line, Channels page |
| Live monitoring: agent status, logs, inter-agent messages incl. channel messages, task progress, tokens/cost (PDF) | Met | SSE run and conversation events, Runs and run page, Agents list live state, Channels transcripts |
| Run inspection with correlated traces, tool calls, token and cost metrics (PRD) | Met | `store.Trace`, spans, `RunHeader.tsx`, `Timeline.tsx` |
| Real runtime executing agent logic, not a mockup (both) | Met | OpenClaw `chat.send` per visit (`node.go`), `/api/openclaw/status` live |
| Reproducible evaluation runner: ≥20 versioned split cases, ≥3 deterministic + ≥1 rubric (PRD) | Met: `evals/reports/final.md` 24 of 24 on 2026-10-19 | `evals/cases/v1/` 24 cases, 7 evaluators + rubric (`internal/evals`), reports in `evals/reports/` |
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

- A Telegram reply to an approval prompt resolving the approval; a declined question;
  a branch failing and stopping its siblings, a parked branch and the fork card in a
  browser (unit tests cover them).
- Whether the OpenClaw gateway compares the Telegram secret in constant time (third-party
  code; the platform refuses webhook mode without https and a 32-character secret).
- A fresh clone on another machine.
