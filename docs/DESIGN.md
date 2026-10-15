# Design: AI Agent Orchestration Platform

Status: revision 2, 2026-10-15, after the reassessment that treats both brief versions as
binding. OpenClaw is the execution engine (DECISIONS.md D-011). This is the spec the code is
built from. Every numbered rule has a test. Rules marked **[decision]** are product choices.
Facts about OpenClaw marked **[probed]** were verified against a live OpenClaw 2026.10.3
gateway in Docker on 2026-10-15; facts marked **[docs]** come from docs.openclaw.ai and are
re-verified by the wave that uses them.

## 1. Goals and non-goals

Goals, the union of the portal PRD and the PDF:

- Configure agents without code changes: name, role, system prompt, model, tool allowlist,
  operating limits, channels, schedules, memory, skills, guardrails, interaction rules.
- Agents execute on OpenClaw, which runs the model loop and the tools. The chosen runtime
  "must actually execute the agent logic".
- Two or more agents cooperate through shared, typed, validated run state, asynchronously,
  with the inter-agent conversation persisted and visible in the UI.
- Visual workflow builder with execution order, conditions and feedback loops; at least two
  templates.
- Human approval gate before every risky action; deterministic termination (step, time,
  cost limits, cancellation); retry with backoff; idempotent side effects.
- One agent reachable over Telegram with verified inbound webhooks and persisted history.
- Live monitoring over SSE: status, logs, inter-agent messages, tool calls, tokens, cost.
- Reproducible eval runner: 20+ versioned split cases, 3+ deterministic evaluators, 1+
  rubric evaluator, baseline and final reports, fully offline.
- One-command local run; no cloud dependency required to evaluate.

Non-goals: multi-tenancy and user accounts; WhatsApp and Slack (adapter interface only);
horizontal scaling; token-by-token streaming of model text to the UI.

## 2. Architecture

```
  browser ───▶ web (nginx)  ──/api,/webhooks,/events──▶ api (Go)        ┐ shared network
                 └─/deploy/─▶ deploytarget (Go)                         │ namespace with
                                                                        │ the gateway
  Telegram ─────────────────── webhook / long-poll ──▶ api              │ (loopback trust)
                                                                        │
  api ◀── ws://127.0.0.1:18789 ──▶ openclaw gateway (Node, OpenClaw)    ┘
  api ◀── http://127.0.0.1:8080/mcp ─── OpenClaw MCP client (platform tools)
  api ◀── http://127.0.0.1:8080/model-proxy/* ─── OpenClaw model providers
  api ──▶ postgres (system of record)      api ──▶ api.anthropic.com (key held by api only)
```

Services in compose: `postgres`, `openclaw` (gateway), `api` (shares the gateway's network
namespace, see 5.14 rule 1), `web`, `deploytarget`; profiles `eval`, `tunnel`, `tools`,
`sandbox` (OpenClaw Docker sandboxing, see 5.14 rule 12).

Responsibilities:

| Concern | Owner |
|---|---|
| Agent, workflow, schedule, skill, memory, channel binding CRUD; UI | api (Go) + web |
| Materialising an agent into OpenClaw (config entry, workspace files, exec policy) | api, over gateway RPC |
| Running a node: start a turn, stream events, enforce limits, abort | api engine, over gateway RPC |
| The model loop, built-in tools (`exec`, `read`, `write`, `edit`, `apply_patch`, `ls`, `process`), skills prompt, context | OpenClaw |
| Platform tools: typed `finish`, gated `deploy_release` and `gitlab_create_mr`, allowlisted `http_fetch`, `memory_save`, `memory_recall` | api MCP server, called by OpenClaw |
| Model access: key injection, request shaping (no reasoning text), usage capture, scripted offline provider | api model proxy, called by OpenClaw |
| Exec approvals (commands outside an agent's allowlist) | OpenClaw asks; api is the approval client; UI and Telegram decide |
| Side-effect approvals, idempotency, retries, state machine, traces, SSE, evals, scheduler, Telegram | api |

Layers inside `api`, dependency direction top to bottom: `httpapi` (routes, SSE, MCP and
proxy endpoints), `engine` (run state machine, node execution), `openclaw` (gateway client:
connect, RPC, event demux, agent materialisation), `workflow`, `tools` (platform tool
executors and MCP schemas), `model` (proxy, scripted provider, prices), `channel`, `store`,
`sched`, `memory`, `guard`, `evals`, `obs`.

## 3. Stack

| Concern | Choice | Why |
|---|---|---|
| Runtime | OpenClaw `ghcr.io/openclaw/openclaw:2026.10.3`, pinned | PDF requirement; Andre runs it; gateway RPC covers agents, config, turns, approvals, usage **[probed]** |
| Control plane | Go 1.26, stdlib `net/http`, `nhooyr.io/websocket` (or `gorilla/websocket`) for the gateway client | Suggested by the brief; one static binary; goroutines for the event pump |
| MCP server | `github.com/modelcontextprotocol/go-sdk`, streamable HTTP | OpenClaw's native MCP client speaks it **[docs]** |
| DB | Postgres 17, pgx v5, embedded SQL migrations | System of record for everything the UI and evals read |
| Model | Anthropic API via the api proxy; `claude-opus-5-5` default per agent | Key never enters the gateway container |
| Frontend | React 19, Vite, TypeScript, Tailwind, React Flow, TanStack Query | Visual builder is required |
| Real-time | SSE from api | One direction suffices |
| Contract | `api/openapi.yaml`, served and validated in tests | Required machine-readable contract |

## 4. Domain model

As in revision 1 (tables `agents`, `workflows`, `runs`, `run_nodes`, `model_calls`,
`tool_calls`, `side_effects`, `approvals`, `agent_messages`, `spans`, `run_events`,
`schedules`, `channel_bindings`, `conversations`, `conversation_messages`,
`channel_updates`, `memory_facts`, `eval_runs`, `eval_results`) with these changes:

- `agents` gains `openclaw_agent_id` (the stable slug used in `agents.entries`), `skills`
  JSONB (skill names), `thinking` (OpenClaw thinking level), `exec_policy` JSONB
  (`{security, ask, allowlist[]}`), `materialized_hash` (hash of the last config pushed).
- `skills` table: id, name (`^[a-z0-9]+(-[a-z0-9]+)*$`), description, body (markdown),
  created_at, updated_at. Materialised as `<workspace>/skills/<name>/SKILL.md`.
- `run_nodes` gains `session_key`, `openclaw_run_id`; `messages` holds the transcript
  mirrored from `session.message` events (never reasoning parts).
- `model_calls` rows come from `session.message` assistant entries (provider, model,
  usage, cost, stopReason, responseId) **[probed]**.
- `tool_calls` rows come from `agent` events with `stream: item` (`toolCallId`, `name`,
  status, error, timings) plus `command_output` **[probed]**; platform tool calls come from
  the MCP server directly and carry the full input and output.
- `workspace_files` is dropped. The workspace is the agent's OpenClaw workspace; run
  artifacts live under `<workspace>/runs/<run_id>/` and the deploy tools read from there.
- `approvals.kind` is `exec` (OpenClaw exec approval id in `external_id`), `tool`
  (platform side effect) or `node` (workflow approval node).

## 5. Design briefs

### 5.1 Run engine and state machine

Unchanged from revision 1 except rule 5a: an `agent` node visit is one OpenClaw turn on a
node-scoped session (5.14 rule 5); the visit ends when the `chat` event for that `runId`
reaches `final`, `error` or `aborted`, or the engine aborts it. Tests T5.1.1 to T5.1.8 stand.

Rule 2 as implemented in wave 1 (the transition table the engine enforces):
`queued → running | cancelled`; `running → queued` (re-queue after a lost worker or a
resume), `running → waiting_approval | succeeded | failed | cancelled | timed_out`;
`waiting_approval → queued | running | failed | cancelled | timed_out`. Anything else is
rejected with `invalid transition <from>→<to>`.

### 5.2 Termination limits and cancellation

Rules 1 to 10 stand with these definitions:

- A step is one assistant message recorded by OpenClaw for the run (`session.message`
  with `role: assistant` and a `usage` block) **[probed]**; the engine increments
  `step_count` on each. `max_node_iterations` bounds steps within one visit.
- Cost is `usage.cost.total` from the same entries, converted to micro-USD, cross-checked
  against our own price table from the token counts; the larger is recorded.
- Enforcement is by `chat.abort { sessionKey, runId }` **[docs]**; after abort the engine
  waits up to 10 s for the `aborted` state, then marks the run regardless.
- Checks run on every event that increments a counter and on a 2 s tick, not only
  before a model call, because the loop is in OpenClaw. A violation aborts the turn and
  ends the run with the reason texts of revision 1, plus `tool call limit <n> exceeded`
  for `max_tool_calls`. Once `finish` has been accepted only cancellation and the time
  limit still apply to the remainder of the turn.
- Limits are layered at trigger time: the request's limits, then the agent's own
  limits, then the defaults, then the platform clamp; negative values are a validation
  error.
- Retry backoff (5.5) is 1, 2, 4, 8 … s capped at 30 s without jitter, so tests are
  deterministic; a wait longer than the time remaining times the run out.

Tests T5.2.1 to T5.2.10 stand, driven by the scripted provider (5.14 rule 9).

### 5.3 Approval gate

Two kinds of gated action, one approvals table, one UI.

Rules:

1. **Exec in the sandbox is autonomous.** Every managed agent runs `exec` inside its
   OpenClaw Docker sandbox (5.14 rule 12), which has no network and only the agent's
   workspace mounted, so sandboxed commands need no human approval. Each agent's exec
   policy is still pushed with `exec.approvals.set` **[probed]** so that `elevated`
   (host) exec, which templates never enable, is `security: allowlist`, `ask: on-miss`,
   `askFallback: deny`. An elevated or node exec outside the allowlist raises
   `exec.approval.requested` to approval clients; the api connection declares
   `caps: ["approvals", "exec-approvals"]` (5.14 rule 2) and is one. Without that
   capability OpenClaw denies headless requests with reason `no-route` **[probed]**.
2. The engine records every `session.approval` event (`phase: pending | terminal`,
   `approval.id`, `presentation.commandText`, `expiresAtMs`, `decision`, `reason`)
   **[probed]** as an `approvals` row of kind `exec` and sets the run to
   `waiting_approval`.
3. Approving calls `exec.approval.resolve { id, decision: allow-once | allow-always |
   deny }` **[docs]**; `allow-always` also appends the pattern to the agent's allowlist in
   our DB so the UI shows the effective policy. The same approval can be answered in
   Telegram with `/approve <id> allow-once|allow-always|deny` by a configured approver
   **[docs]**; the engine learns the outcome from the `terminal` event either way.
4. OpenClaw's own approval expiry (`expiresAtMs`, 30 minutes **[probed]**) is shorter than
   our `approval_timeout_seconds`; the engine resolves `deny` itself when our timeout is
   shorter, and treats an OpenClaw expiry as a rejection with reason `expired`.
5. **Side-effect approvals.** Platform tools with `risk: side_effecting` are served over
   MCP (5.7). With policy `require_approval` the MCP handler records the tool call
   (`awaiting_approval`, with its idempotency key), asks the engine for an approval of
   kind `tool` (`RequestApproval`), which stores the row, registers it with the gateway
   as a plugin approval (rule 5a, id in `external_id`), sets the run and node to
   `waiting_approval` and emits SSE `approval.requested`; the handler then **blocks the
   MCP call** (`WaitApproval`) until a decision or `MCP_TOOL_WAIT` (default 540 s, below
   OpenClaw's MCP `requestTimeoutMs` of 600 s). On approval within the wait, the effect
   executes and the result returns to the model in the same turn. On rejection, the
   tool result is `{"status":"rejected","reason":...}`, not an error, so the agent can
   finish cleanly. On timeout, the tool result is
   `{"status":"pending_approval","approval_id":...}`; the node continues, and when the
   approval is later granted the engine executes the effect itself through
   `Deps.Effects.ExecuteApproved` with the same idempotency key and records the outcome
   in `runs.state.pending_effects[]` (a later rejection is recorded there too). A
   decision made while a handler still waits is delivered to that handler and never also
   executed by the engine. **[decision]**
5a. **Gateway registration, verified 2026-10-15 on 2026.10.3 (`make probe-approval`).**
   `plugin.approval.request { title, description, detail?, severity: info|warning|critical,
   toolName, toolCallId, mcpTool: {server: "yuno", tool}, allowedDecisions:
   ["allow-once", "deny"], agentId?, sessionKey?, timeoutMs, twoPhase: true }` returns at
   once with `{status: "accepted", id: "plugin:<uuid>", deliveryRoute, createdAtMs,
   expiresAtMs}`. Without `twoPhase` the call itself blocks until the decision or the
   timeout, so the engine always sets it. `timeoutMs` is at most 600 000 (default
   120 000); the engine passes `MCP_TOOL_WAIT` in milliseconds. Other approval clients
   receive `plugin.approval.requested {approvalKind: "plugin", id, request, createdAtMs,
   expiresAtMs}` and `plugin.approval.list {}` lists the pending ones. Any decision
   produces `plugin.approval.resolved {id, decision: allow-once|allow-always|deny,
   resolvedBy, ts, request}` on every client; the gateway's own timeout produces the
   same event with `decision: "deny"` and `resolvedBy: null`, which the engine
   **ignores**: our approval stays pending until our own expiry (5.2 rule 10) and can
   still be decided from the web UI, which then skips the gateway call.
   `plugin.approval.resolve { id, decision }` returns `{ok: true}`; a second decision
   fails with `APPROVAL_ALREADY_RESOLVED` and an unknown or expired id with
   `APPROVAL_NOT_FOUND`; both mean the gateway side is already settled and our row is
   decided anyway. `plugin.approval.waitDecision { id, timeoutMs }` returns `{id,
   decision|null, terminalReason: user|timeout}`; the engine relies on the event, not on
   this call. Pending plugin approvals do not survive a gateway restart. Whether a plugin
   approval reaches a Telegram chat is verified with the channel in wave 3; until then
   decisions come from the web UI and the gateway's own clients. **[probed]**
6. Effective policy for a platform tool = agent `interaction_rules[tool]`, else
   `require_approval` for `side_effecting`, else `auto`. Operators may set a
   side-effecting tool to `auto` (D-009); the UI warns; templates never do.
7. `blocked` platform tools are not advertised to that agent (OpenClaw `tools.deny` on
   the MCP tool name) and the MCP server refuses them anyway with
   `tool <name> is blocked for this agent`.
8. Decisions on non-pending approvals return 409. A run cancelled while waiting marks the
   approval `expired` with reason `run cancelled` and, for exec approvals, resolves `deny`.
9. Workflow `approval` nodes (5.6 rule 8) are kind `node`; nothing in OpenClaw is involved.

Edge cases: an exec approval raised by a session the engine is not tracking (manual
OpenClaw use) is recorded with `run_id = NULL` and shown in the inbox. Two MCP calls from
one turn that both need approval are two approvals; OpenClaw runs the tool calls of one
turn concurrently (**[probed]** 2026-10-15: a `write` and an `exec` issued together
started 4 ms apart), so both MCP calls block at once and either may be decided first.

Tests: T5.3.1 allowlisted command runs without an approval row (scripted provider
requests `python3 -m pytest`). T5.3.2 non-allowlisted command raises an approval; approve
→ command output event; deny → tool error returned to the model. T5.3.3 MCP side effect
blocks, approve within wait → executed once, result in same turn. T5.3.4 reject → status
rejected result. T5.3.5 wait timeout → pending result, later approval → executed by
engine once, state updated. T5.3.6 `auto` policy executes without approval and the audit
row says so. T5.3.7 cancel while waiting → approval expired, exec resolved deny. T5.3.8 double decision → 409. T5.3.9 gateway timeout event (resolvedBy null)
leaves the approval pending; a later web decision still applies.

### 5.4 Idempotent side effects

Purpose: a repeated request for the same side effect produces at most one effect, across
retries, re-queues, duplicate approvals and the engine's fallback execution.

Inputs: tool call input (JSON), run id, node visit id (`run_nodes.id`), tool name.

Rules:

1. `idempotency_key = sha256(run_id || node_visit_id || tool_name || canonical_json(input))`,
   hex, computed by the MCP handler before the approval gate. Canonical JSON: keys sorted,
   no insignificant whitespace, UTF-8. The key is stored on the `tool_calls` row when the
   call is recorded (`awaiting_approval` or `executed`).
2. Before executing a `side_effecting` tool, the executor inserts
   `side_effects(idempotency_key)` in a transaction. On unique violation it returns the
   stored `result` and records the tool call with `output.replayed = true`.
3. The side effect's external request carries the key (`Idempotency-Key` header for the
   deploy target and any HTTP tool; GitLab uses a branch name derived from the key).
   Outbound channel messages (wave 3) are deduped by `(conversation_id, run_id,
   node_visit_id, sha256(text))`.
4. The deploy target enforces the same key server side: a second `POST /releases` with a
   known key returns `200` with the original release, never creates a second one.
5. The stored `result` is written in the same transaction as the key only after the
   external call succeeds. If the external call fails, the key row is deleted so a retry
   can try again (the external service's own key handles the double-send case).
6. The engine's fallback execution (5.3 rule 5) and an MCP retry of the same call share
   the key, so whichever runs second replays.

Edge cases: the same input in a later node visit (a loop) is a new effect by design (the
key includes the visit). A retry after a network timeout where the first request did
land: the external service returns the original by key (rule 4); the platform stores it.

Consistency: rules 2 and 5 are one transaction with `SELECT ... FOR UPDATE` on the key.

Outputs: `tool_calls.idempotency_key`, `output.replayed`, `side_effects` rows in the
run's trace (`side_effects[]`).

Tests: T5.4.1 same input twice in one visit → one external call (counting fake), second
result replayed. T5.4.2 different visit → two effects. T5.4.3 external failure deletes the
key and a retry succeeds with one effect. T5.4.4 deploy target: two POSTs same key → one
release, same id. T5.4.5 canonical JSON: key order and whitespace differences produce the
same key. T5.4.6 fallback execution after an MCP replay of the same call → one effect.

Out of scope: cross-run dedupe.

### 5.5 Retry and backoff

Purpose: survive transient gateway, model and tool failures without turning them into
run failures, while bounding total time.

Inputs: gateway RPC errors and disconnects; `chat` events with `state: error`
(`errorKind`, `errorMessage`, `errorDetail{provider, model, httpStatus, ...}`) **[docs]**;
MCP tool errors classified by the executor.

Rules:

1. Gateway connection: on disconnect the client reconnects with backoff 1 s, 2 s, 4 s,
   up to 30 s, indefinitely; in-flight runs are re-attached by `runId` through
   `chat.history` and `sessions.messages.subscribe` replay, not restarted.
2. `chat.send` transport failures (RPC error, timeout 15 s) retry up to 5 times with the
   same `idempotencyKey` (OpenClaw dedupes by it **[probed]**); backoff per 5.2 (1, 2,
   4, 8 … s capped at 30 s, no jitter).
3. A turn that ends in `chat` state `error` with a provider HTTP status in
   {408, 409, 429, 500, 502, 503, 529} or a transport error is retried as a new turn on
   the same session with the message `Continue.` up to 3 times (OpenClaw already applies
   its own provider failover and retries first **[docs]**). Other errors fail the node with
   reason `model error: <errorKind>`.
4. Platform tool retries per the registry entry: `read_only` and `computational` tools
   default `max_attempts: 3` on errors the executor classifies as transient (timeout,
   5xx, connection refused); `side_effecting` tools default `max_attempts: 2` and are
   only retried after the idempotency key is recorded (5.4), never before approval. A
   non-JSON body from an external service is a permanent error, not transient.
5. Every attempt is a span and a `warn` log line.

Edge cases: a wait longer than the time remaining times the run out (5.2). Failure
handling: after the last attempt a model failure fails the node with reason
`model error: <errorKind>` and a tool failure returns an error result to the model,
which may try something else. Tests: T5.5.1 disconnect mid-run →
reattach, no duplicate turn. T5.5.2 `chat.send` RPC failure twice then success → one run.
T5.5.3 provider 529 twice then success → three turns, one node. T5.5.4 non-retryable error
→ node failed with reason. T5.5.5 deadline nearer than backoff → timed_out.

### 5.6 Workflow graph, conditions and typed state

Purpose: let an operator define, in the UI, which agents run in what order, with branching
and feedback loops, and have the engine enforce typed state between them.

Inputs: `workflows.definition` JSON (the shipped `templates/dev_pipeline.json` is the
reference example):

```json
{
  "input_schema": { "type": "object", "properties": {"task": {"type": "string"}}, "required": ["task"] },
  "state_schema": { "type": "object", "properties": {...} },
  "nodes": [
    {"id": "start", "kind": "start"},
    {"id": "coder", "kind": "agent", "agent_id": "...", "task": "...template...",
     "output_schema": {...}, "input_from": ["start", "reviewer"]},
    {"id": "reviewer", "kind": "agent", ...},
    {"id": "gate", "kind": "approval", "message": "Deploy {{.state.coder.summary}}?"},
    {"id": "deployer", "kind": "agent", ...},
    {"id": "end", "kind": "end"}
  ],
  "edges": [
    {"from": "start", "to": "coder"},
    {"from": "coder", "to": "reviewer"},
    {"from": "reviewer", "to": "coder", "when": {"path": "/reviewer/verdict", "op": "eq", "value": "changes_requested"}},
    {"from": "reviewer", "to": "gate", "when": {"path": "/reviewer/verdict", "op": "eq", "value": "approved"}},
    {"from": "gate", "to": "deployer"},
    {"from": "deployer", "to": "end"}
  ],
  "limits": {...}
}
```

Rules:

1. Exactly one `start` node and at least one `end` node. Node ids unique, `[a-z0-9_]{1,40}`.
2. Every edge references existing nodes; every non-`end` node has at least one outgoing
   edge; every node is reachable from `start`. Violations return HTTP 422 with one
   message per violation, path-qualified (`edges[2].to: unknown node 'reviewr'`).
3. Conditions are `{path, op, value}` with `op ∈ {eq, neq, in, exists, gt, lt}` over
   `runs.state` by JSON pointer, composable with `{"all": [...]}` and `{"any": [...]}`.
   A missing path is `exists: false` and makes `eq/neq/in/gt/lt` false. No expression
   strings, no code.
4. An edge without `when` is always true. Edges from a node are evaluated in array order;
   the first true one is taken (5.1 rule 5). A node with no true outgoing edge and not of
   kind `end` ends the run `failed` with reason `no matching edge from <node>`.
5. An `agent` node runs the referenced agent as one OpenClaw turn on a node-scoped
   session (5.14 rule 5) with `task` rendered by Go `text/template` over `{input, state}`
   (`{{.input.task}}`, `{{with .state.reviewer}}{{.feedback}}{{end}}`); the literal
   `$RUN_ID` in the rendered task is replaced by the run id and the fixed protocol block
   (run token, output schema, finish instruction) is appended. Template errors fail the
   node at run time and are also reported at save time by rendering against an empty
   input and state.
6. The agent's `finish` tool input must validate against `output_schema`; the validated
   object is written to `state[<node_id>]` (replace, never deep merge, so a revisit
   replaces the previous output) and an `agent_messages` row records `from=<node>`
   `to=<next node>` with the output payload once the next edge is chosen (SSE
   `agent_message`).
7. The MCP `finish` tool validates the input against the node's `output_schema`; on
   failure it returns the validation message as a tool result and the turn continues;
   the third consecutive failure fails the node with reason `output schema: <first
   error>`. If the turn ends without a `finish` call, the engine looks for one JSON
   object in the final assistant text that validates; otherwise the node fails with
   reason `agent did not call finish`.
8. An `approval` node is a human gate without a tool: it creates an approval of kind
   `node` with the rendered message, sets the run to `waiting_approval` and releases the
   worker (the wait does not count toward the time limit, 5.2 rule 4); approve re-queues
   the run, which continues along the node's first true edge; reject follows the node's
   `on_reject` edge if present, else fails the run with reason `rejected at <node>`.
   Nothing in OpenClaw is involved.
9. Workflow templates are JSON files under `templates/` (`{key, name, description,
   definition}`) loaded at api startup into `workflows` rows with `template_key` set
   (updated in place when the file changed, never deleted). Template agent nodes name
   their agent with `agent_name` instead of `agent_id`, so a template is not runnable as
   stored; `POST /workflow-templates/{key}/instantiate` copies it into a new editable
   workflow with every `agent_name` resolved to the id of the agent of that name
   (case-insensitive; 422 `nodes[i].agent_name: no agent named 'X'` when missing; `make
   seed` creates the six demo agents). Shipped: `dev_pipeline` (Coder → Reviewer ⇄
   feedback → approval → Deployer) and `research_pipeline` (Researcher → Critic ⇄
   feedback → Writer).
10. The loop in `dev_pipeline` is data: the `reviewer→coder` edge and `max_loop_visits`.
    Nothing in Go knows the words coder or reviewer.
11. `input` is validated against `input_schema` at trigger time (422 with the schema
    message); the final state is checked against `state_schema` by the `schema`
    evaluator, not by the engine.
12. A node's `on_failure` edge is followed when the node fails for a reason that is not a
    limit violation or a cancellation; without one the run fails with the node's reason.

Edge cases: a workflow with only `start→end` is valid and succeeds instantly with zero
model calls. An agent node whose agent lacks the tools its task needs still runs; the
model gets only allowed tools. A run whose agent was deleted after the run started
continues with the snapshot.

Consistency: definition is validated on save and again at trigger time (an agent may have
been deleted, a template still carries `agent_name`); trigger-time failure returns 422.

Outputs: validation messages; `agent_messages` rows; SSE `agent_message`.

Tests: T5.6.1 each structural rule produces its path-qualified message. T5.6.2 condition
table test for every op including missing path. T5.6.3 `all`/`any` nesting. T5.6.4 edge
order precedence. T5.6.5 template render error at save and at run. T5.6.6 output schema
retry then failure. T5.6.7 approval node approve/reject/`on_reject`. T5.6.8 dev_pipeline
template loops twice then deploys with the scripted provider. T5.6.9 state replace on
revisit. T5.6.10 instantiate resolves agent names and reports a missing one. T5.6.11
input schema violation at trigger → 422.

Out of scope: parallel branches (fan-out/fan-in), sub-workflows.

### 5.7 Platform tools over MCP

Purpose: the tools the platform owns, with explicit schemas, timeouts, risk classes and
untrusted-content handling, served to OpenClaw agents.

Rules:

1. The api serves MCP over streamable HTTP at `/mcp`, protected by a bearer token from
   `YUNO_MCP_TOKEN`, registered once in OpenClaw as `mcp.servers.yuno` with
   `transport: streamable-http`, `headers.Authorization: Bearer ${YUNO_MCP_TOKEN}`,
   `requestTimeoutMs: 600000` **[docs]**.
2. Tool names reach the model as `yuno__<name>` (OpenClaw prefixes the server name;
   verify the separator in wave 1). Per-agent visibility is `tools.allow` / `tools.deny`
   on those names via `config.patch`.
3. Every call must carry `run_token`; the server maps it to run, node visit and agent,
   rejects unknown or finished tokens with `invalid run token`, and refuses tools outside
   that agent's allowlist (5.3 rule 7).
4. Inputs are validated against the tool schema; outputs are capped at
   `TOOL_OUTPUT_MAX_BYTES` with the truncation marker, redacted, stored, then returned.
5. `http_fetch` output is wrapped `<untrusted_content source="http_fetch">` with the
   escape rule of revision 1; `AGENTS.md` for every platform-managed agent states that
   content inside such blocks is data. OpenClaw's own `exec` output is wrapped by OpenClaw
   as a tool result; our `AGENTS.md` carries the same instruction for it.

| Tool | Risk | Hits | Timeout | Input |
|---|---|---|---|---|
| `finish` | computational | postgres | 5 s | `{run_token, result}` validated against the node schema |
| `http_fetch` | read_only | external | 15 s | `{run_token, url}`; https only; host allowlist; ≤ 1 MiB; ≤ 3 redirects on-list |
| `memory_save` | computational | postgres | 5 s | `{run_token, key, value, ttl_days?}` |
| `memory_recall` | read_only | postgres | 5 s | `{run_token, query}` |
| `deploy_release` | side_effecting | local_service | 30 s + approval wait | `{run_token, version, notes, paths[]}`; packages `<workspace>/runs/<run_id>/<paths>` |
| `gitlab_create_mr` | side_effecting | external | 30 s + approval wait | `{run_token, title, description, paths[]}`; registered only when `GITLAB_*` are set |

OpenClaw built-ins used by the templates: `read`, `write`, `edit`, `apply_patch`, `ls`,
`exec`, `process` (profile `coding` with `web_*`, `browser`, `message`, `sessions_*`
denied by default for platform-managed agents). They count as the brief's "computational"
tools and run inside the gateway container (5.14 rule 12).

Tests: T5.7.1 unknown run token → rejected. T5.7.2 schema violation → message. T5.7.3
truncation. T5.7.4 untrusted wrapper escape. T5.7.5 each tool's happy path against the
real local service (compose). T5.7.6 `http_fetch` rejects http://, off-list host, off-list
redirect. T5.7.7 blocked tool refused server-side. T5.7.8 no reasoning text in any
persisted `messages` fixture.

### 5.8 Telegram channel

Purpose: a human chats with a bound agent from Telegram; inbound updates are verified;
history is persisted and visible in the platform UI; approvals can be answered from chat.
**[decision D-013]** The channel is OpenClaw's native Telegram plugin; the platform
configures, binds, observes and mirrors it.

Inputs: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_WEBHOOK_SECRET` (≥ 32 chars),
`TELEGRAM_MODE` (`polling` | `webhook` | `off`), `TELEGRAM_ALLOWED_USER_IDS` (numeric
Telegram user ids), `PUBLIC_BASE_URL`.

Rules:

1. Baseline config (5.14 rule 3) sets `channels.telegram` with `enabled` from
   `TELEGRAM_MODE`, `botToken: "${TELEGRAM_BOT_TOKEN}"`, `dmPolicy: "allowlist"`,
   `allowFrom` and `commands.ownerAllowFrom` from `TELEGRAM_ALLOWED_USER_IDS`,
   `execApprovals: { enabled: true, approvers: <same ids>, target: "dm" }`, and in
   webhook mode `webhookUrl: "<PUBLIC_BASE_URL>/telegram-webhook"` and `webhookSecret`
   **[docs]**. OpenClaw registers the webhook with Telegram on startup and serves the route
   on the gateway port; nginx proxies `/telegram-webhook` to `openclaw:18789`.
2. Verification is OpenClaw's: the Telegram secret header is checked, the body validated,
   the update committed to the durable ingress queue, and only then an empty 200 with
   `x-openclaw-delivery-accepted: durable` is returned; failures get an error status
   **[docs]**. `scripts/smoke.sh` sends a request with a wrong secret through nginx and
   asserts a non-200 response, so the behaviour is demonstrated, not assumed.
3. Binding: the agent form's "Telegram" toggle writes `bindings[]` with
   `{ agentId, match: { channel: "telegram" } }` through `config.patch` **[docs]**; the
   UI shows which agent is bound and refuses a second binding while one exists.
4. The conversation is the OpenClaw session for that chat. The engine subscribes to the
   session roster (`sessions.subscribe`) and to each Telegram session
   (`sessions.messages.subscribe`) and mirrors `session.message` entries into
   `conversations` / `conversation_messages` (direction, text, usage) so the platform UI
   shows the full history and the eval runner can assert on it. The agent's reply is
   sent by OpenClaw.
5. Exec and plugin approvals raised by a Telegram-originated turn are delivered by
   OpenClaw to approver DMs and answered with `/approve <id> ...` **[docs]**; the engine
   mirrors their lifecycle like any other approval (5.3).
6. The bot token never appears in logs or the UI; `config.get` already redacts it
   **[probed]** and our redactor covers the Telegram token shape.

Edge cases: a message from a non-allowlisted user is dropped by OpenClaw and never
reaches the agent; the UI shows the allowlist. Polling and webhook modes are mutually
exclusive; switching requires a gateway restart (`channels.telegram` reload kind is
checked at init).

Tests: T5.8.1 wrong secret through nginx → non-200 (compose smoke). T5.8.2 baseline
config renders the expected `channels.telegram` block for each mode (unit). T5.8.3
binding toggle produces the `bindings[]` patch and refuses a duplicate. T5.8.4 a
`session.message` on a Telegram session key is mirrored into `conversation_messages`
(unit, fixture event). T5.8.5 token absent from logs when config is fetched.

Out of scope: our own webhook adapter (revision 1), group chats, media.

### 5.9 Scheduler, 5.10 Memory, 5.11 Guardrails, 5.12 Observability, 5.13 Eval runner

Unchanged in rules and tests, with these implementation notes:

- 5.10: facts are additionally materialised into the agent's `MEMORY.md` (newest 20
  non-expired facts, bounded to 4000 chars) before each turn so OpenClaw's own memory
  loading sees them; `memory_search` and `memory_get` built-ins stay allowed.
- 5.11: `blocked_tools` map to `tools.deny`; `blocked_input_patterns` for `exec` map to
  the agent's exec policy (`security: allowlist` means a non-matching command asks or is
  denied); patterns for platform tools are enforced in the MCP server.
- 5.12: per-call token and cost come from OpenClaw usage entries; spans are built from
  `agent` lifecycle and item events; the model proxy adds a span per upstream request with
  the upstream request id.
- 5.13: the scripted provider is served by the model proxy (5.14 rule 9); eval cases pin
  the script with a `script_id: <id>` marker the engine writes into the task message,
  and scripted tool arguments may use the placeholders `$RUN_ID` and `$RUN_TOKEN`, which
  the provider substitutes from the task message. The `termination` evaluator accepts
  `step_count <= max_steps + 1` when the failure reason names the step limit, because
  the model call that trips the limit is recorded before OpenClaw honours the abort and
  one more may complete meanwhile (wave 1 interpretation). The gate is the share of
  public cases where every evaluator passed; end-to-end success is the share whose
  final status equals `expect.status`. A text-only scripted reply ends the turn, so
  cases that must exceed a step limit use tool-call steps.
- 5.13, wave 2 case schema: a case has either `agent` + `task` (a quick run) or
  `workflow` + `input`, where `workflow` is an inline definition whose agent nodes use
  `agent_name` resolved against the case's own `agents: {<name>: {system_prompt,
  tool_allowlist, limits}}` map; the runner creates those agents (named
  `eval-<case>-<name>-<suffix>`), creates the workflow, triggers it with the scripted
  model override and deletes both afterwards. The script is consumed across nodes in
  execution order. `actions.approvals: [{decision: approve|reject, reason?}]` are applied
  in order to the run's pending approvals as the runner sees them
  (`GET /api/approvals?run_id=&status=pending`, then `POST /api/approvals/{id}/approve`
  or `/reject`); `actions.approval_wait_seconds` leaves one pending to test expiry.
  `expect` gains `state` (JSON pointer → expected value, one entry per check) and
  `tool_statuses` (tool name → list of statuses the run must have recorded). The
  `approval_gate`, `idempotency`, `schema` and `allowlist` evaluators read the trace,
  which carries `side_effects[]` from wave 2.

### 5.14 OpenClaw integration contract

Purpose: everything the engine assumes about the gateway, in one place, each line tagged
with how it was verified.

Rules:

1. **Transport and trust.** The api container runs with `network_mode: "service:openclaw"`
   and connects to `ws://127.0.0.1:18789`. Over loopback a `client.id: gateway-client`,
   `client.mode: backend` connect frame with `auth.token` = the shared gateway token is
   granted the requested operator scopes without device pairing **[probed]**; from any
   other address the same frame connects with no scopes **[probed]**, and non-enum
   `client.id` or `client.mode` values are rejected **[probed]**. Token comes from
   `OPENCLAW_GATEWAY_TOKEN` in both containers.
2. The connect frame requests scopes `operator.read`, `operator.write`, `operator.admin`,
   `operator.approvals` and declares `caps: ["approvals", "exec-approvals"]` (the
   literals `GATEWAY_CLIENT_CAPS.APPROVALS` and `EXEC_APPROVALS` in the 2026.10.3
   bundle **[probed]**) so exec approvals route to us (5.3 rule 1). Protocol 4; frames
   are `req`/`res`/`event` **[probed]**.
3. **Baseline config** is written by `yuno openclaw-init` into the bind-mounted state
   directory before the gateway starts, only when absent or marked `meta.managedBy:
   yuno`: gateway token auth and `bind: lan`; `models.providers.anthropic` with `baseUrl`
   pointing at the api model proxy; `models.providers.scripted` (OpenAI-compatible)
   pointing at the proxy's scripted endpoint; `mcp.servers.yuno`; `agents.defaults`
   (workspace root, model, `tools.profile: coding`, `thinkingDefault`,
   `reasoningDefault: off`, the sandbox block of rule 12); `tools.deny` for web, browser,
   messaging, nodes and automation groups; `tools.sandbox.tools.alsoAllow`;
   `channels.telegram` and `commands.ownerAllowFrom` from the Telegram variables (5.8).
   Everything after that is RPC. A re-run with unchanged inputs writes nothing.
4. **Agent materialisation** (on create, update, and before every run when
   `materialized_hash` differs): `agents.create { name, workspace }` **[probed]**: it
   accepts only those two fields, derives the OpenClaw agent id by slugging `name`, and
   refuses an id that exists; the platform therefore passes its own stable id as the
   `name` (`"ag"` plus the last ten hex characters of the agent UUID, the last because
   the first characters of a UUIDv7 are a timestamp), stores the returned id, and sets
   the display name afterwards through the config entry;
   `config.patch` with `raw` JSON and `baseHash` from `config.get` **[probed]**, setting
   `agents.entries.<id>` (`model.primary`, `thinkingDefault`, `reasoningDefault: "off"`,
   `tools.{profile,allow,deny}`, `skills`, `identity`); `exec.approvals.set` with the
   agent's policy and `baseHash` **[probed]**; workspace files `SOUL.md` (role and system
   prompt), `AGENTS.md` (platform operating rules: finish protocol, untrusted content,
   run directory, files through the file tools rather than exec, dependent tool calls in
   separate turns), `MEMORY.md`, and `skills/<name>/SKILL.md` written through the shared
   workspace volume (`agents.files.set` is used for the bootstrap files it accepts; the
   rest go through the volume). `models.providers` and `agents.defaults.model` patches
   reload hot **[probed]**; the engine checks `config.schema.lookup` `reloadKind` for any
   path it patches and refuses `restart` paths at runtime.
5. **Sessions.** One session per node visit, key `agent:<id>:yuno-<run id>-<node>-<visit>`
   (the full run id: the first characters of a UUIDv7 are a timestamp, so a prefix
   collides for runs started within the same minute);
   one per Telegram conversation, key `agent:<id>:tg-<chat_id>`. Sessions are created on
   the first `chat.send` or `sessions.patch` **[probed]**; a per-run model override is
   `sessions.patch { key, model: "provider/model" }` before the first turn **[probed]**.
   Finished node sessions are archived after the run reaches a terminal state and its
   trace is stored (`sessions.delete { key }` **[probed]**), keeping OpenClaw's store
   small. The engine creates `runs/<run_id>/` in the workspace before the first turn.
6. **Turns.** `chat.send { sessionKey, agentId, message, idempotencyKey }` returns
   `{ runId, status: "started" }`, where `runId` equals the idempotency key; a repeated
   key returns `status: "ok"` and the engine reads the outcome from `chat.history`
   **[probed]**. `chat.abort { sessionKey, runId }` returns `{ ok, aborted, runIds }` and
   the turn reaches `aborted` within about 300 ms **[probed]**. Tool results are not
   broadcast as `session.message`; they come back from `chat.history` as
   `{ role: "toolResult", toolCallId, toolName, content }`, so the engine fills tool
   outputs from history when a visit ends; tool calls in transcript entries are parts
   `{ type: "toolCall", id, name, arguments }` **[probed]**. The engine subscribes with
   `sessions.messages.subscribe { key, agentId, includeApprovals: true }` **[probed]** and
   demuxes by `runId`: `chat` (`status` with `phase`, `delta`, `final` with `stopReason`
   and `message`, `error`, `aborted`), `agent` streams `run_status`, `lifecycle`
   (`start`, `model` with provider and model, `end`), `assistant` (text deltas), `usage`,
   `item` (tool call `start`/`end` with `toolCallId`, `name`, `status`, `error`),
   `command_output`; `session.message` (transcript entries, assistant ones with `usage`
   and `cost`); `session.approval` **[probed]**.
7. **Run token.** The task message carries `run_token: r_<24 base32 chars>` valid for the
   node visit; MCP calls present it (5.7 rule 3). It is not a secret from the agent, only a
   binding; it is revoked when the visit ends.
8. **Model proxy.** `/model-proxy/anthropic/v1/*` reverse-proxies to the Anthropic API,
   injecting `x-api-key` from api's environment, rewriting any non-null `thinking` to
   `{type: "adaptive"}` (dropping `display` and `budget_tokens`) and leaving a body
   without `thinking` untouched, forwarding `anthropic-beta` headers, logging one line
   per upstream response with the upstream `request-id` and duration, and enforcing
   `MODEL_PROXY_RATE_PER_MINUTE` (default 60) with 429 and `Retry-After` beyond it. Both
   proxy mounts accept loopback callers only (403 otherwise): the gateway shares the
   api's network namespace, while every other container reaches the api as
   `openclaw:8080` and must not be able to spend the key. The gateway container never
   holds the key. The MCP endpoint is stateless, so an api restart needs no session
   recovery from OpenClaw's MCP client.
9. **Scripted provider.** `/model-proxy/scripted/v1/chat/completions` implements the
   OpenAI chat completions shape OpenClaw sends (streaming SSE with `tool_calls` deltas and
   a final usage chunk) **[probed]**; it reads the run token from the request messages,
   looks up the case script for that run and node, and replays the next step (text or a
   tool call to a built-in or platform tool). Exhaustion returns a `500` so the turn ends
   with an error the eval records as `script exhausted at <node> call <n>`.
10. **Usage.** Tokens and cost per assistant message come from `session.message` /
    `chat.history` entries (`usage.input`, `usage.output`, `usage.cacheRead`,
    `usage.cacheWrite`, `usage.cost.total`) **[probed]**; OpenClaw computes cost from the
    provider's configured per-model `cost` (USD per million tokens) **[docs]**, which the
    baseline config sets from our price table.
11. **No chain of thought.** `reasoningDefault: off` on every managed agent, the proxy
    strips `thinking.display`, and the engine drops any `agent` event with
    `stream: reasoning` and any transcript part of type `thinking` before storage.
12. **Exec isolation: Docker sandbox, always on. [decision D-012]** Baseline config sets
    `agents.defaults.sandbox: { mode: "all", scope: "agent", workspaceAccess: "rw",
    docker: { image: "yuno-sandbox:latest", network: "none", readOnlyRoot: true,
    capDrop: ["ALL"], pidsLimit: 256, memory: "1g", cpus: 1, tmpfs: ["/tmp"] } }` and
    `tools.sandbox.tools.alsoAllow: ["bundle-mcp"]` so platform MCP tools stay callable
    in sandbox mode **[docs]**. The gateway container gets the host Docker socket
    (`OPENCLAW_SANDBOX=1`, `OPENCLAW_DOCKER_SOCKET=/var/run/docker.sock`) and creates
    sibling containers named `openclaw-sbx-*`; the state directory and workspaces are
    **bind mounts from the host** (`./data/openclaw`), because named volumes cannot be
    re-mounted into siblings **[docs]**; OpenClaw translates gateway-side workspace paths
    to host paths by inspecting its own container's mounts **[probed]**: it finds its
    container id in `/proc/self/mountinfo`, runs `docker inspect` and `docker exec` on
    itself through the socket, so the gateway image must carry a docker CLI (the
    upstream image has none) and the container must run with the socket's group id
    (`group_add`). `docker/openclaw.Dockerfile` is the pinned upstream image plus the
    static docker CLI. The sandbox image is built by compose from
    `docker/sandbox.Dockerfile` (python 3.12, pytest, git, a `sandbox` user). The threat
    model records that the socket is root-equivalent on the host and that the gateway
    container is therefore a privileged component, while agent code itself runs with no
    network and no socket.
13. **Health.** `GET /healthz` on the gateway port **[probed]**; api `readyz` also checks
    the gateway connection is open and scoped.

Tests: T5.14.1 connect over loopback yields all four scopes (compose integration).
T5.14.2 baseline config is idempotent (init twice → one write). T5.14.3 materialise an
agent → `agents.list` shows it, `config.get` has the entry, workspace has the files,
`exec.approvals.get` has the policy; re-materialise with no change → no RPC writes.
T5.14.4 a scripted turn produces `model_calls`, `tool_calls` and spans with the expected
counts. T5.14.5 abort ends the turn within 10 s. T5.14.6 proxy injects the key, strips
`display`, records the request id (httptest upstream). T5.14.7 scripted provider streams
a tool call OpenClaw executes (compose integration). T5.14.8 reasoning events are dropped.

## 6. HTTP API summary

Errors: `{error: {code, message, details?}}`; validation failures are 422 with
`details: [{path, message}]` (path-qualified, e.g. `tool_allowlist[2]`), including
malformed or unknown-field bodies, since 400 is not part of the contract. Not found is
404; a decision on a non-pending approval or a cancel of a finished run is 409; a
materialisation failure after a successful write is 502 with the row id in details.

As revision 1 plus: `GET/POST /skills`, `GET/PUT/DELETE /skills/{id}`;
`GET /agents/{id}/runtime` (materialised OpenClaw state: config entry, exec policy, files);
`POST /agents/{id}/materialize`; `POST /mcp` (MCP, bearer); `/model-proxy/*` (bearer or
loopback only); `GET /openclaw/status` (gateway connection, scopes, version).

## 7. Frontend

As revision 1 plus a Skills page, an Exec policy editor on the agent form (security, ask,
allowlist patterns), and an OpenClaw panel on Status.

## 8. Docker and Makefile

Services: `postgres`, `openclaw-init` (writes the baseline config, exits), `openclaw`
(gateway, Docker socket mounted, `./data/openclaw` bind-mounted at
`/home/node/.openclaw`), `api` (shares the gateway network namespace, mounts the same
bind at the same path, so `web` proxies to `openclaw:8080`), `web` (also proxies
`/telegram-webhook` to `openclaw:18789`), `deploytarget`, `sandbox-image` (build-only
service that tags `yuno-sandbox:latest`); profiles `eval`, `tunnel`, `tools`. The gateway
image is pinned to `2026.10.3`. Volumes: `pgdata`, `releases`, caches; everything OpenClaw
touches is a host bind mount under `./data/` (git-ignored). `.env.example` adds
`OPENCLAW_GATEWAY_TOKEN`, `YUNO_MCP_TOKEN`, `MODEL_PROXY_RATE_PER_MINUTE`,
`MCP_TOOL_WAIT_SECONDS`, `DOCKER_SOCKET`. Make targets unchanged plus `make openclaw-shell`
(CLI inside the gateway container), `make openclaw-logs`, and `make probe-openclaw`
(`scripts/openclaw-probe.mjs`: connects over loopback, patches a mock provider, runs a
turn with a sandboxed `exec`, and prints the event summary; the integration contract's
canary).

## 9. Build order (tracer bullet, then widen)

Wave 1, tracer bullet: compose with postgres, openclaw, api, web, deploytarget; gateway
client with reconnect and event demux; `openclaw-init`; agent CRUD and materialisation;
model proxy (anthropic + scripted); MCP server with `finish` only; a single-agent "quick
run" from the agent page; run detail page with live trace; `make run`, `make smoke`,
`make test`; `make eval` with 3 cases and the `termination` and `trace_integrity`
evaluators; baseline report. Done when a Coder agent, prompted from the browser, writes a
Python function and runs its tests through OpenClaw's `exec`, and the trace streams live.

Wave 2: workflow graph, conditions, loops, typed state; builder; templates; exec
approvals and side-effect approvals; `deploy_release`, `gitlab_create_mr`, `http_fetch`;
idempotency; retries; limits and cancellation; approvals inbox; evaluators.

Wave 3: Telegram adapter and conversation sessions; memory and `MEMORY.md`; skills;
guardrails; scheduler; channels, schedules and skills pages; chat approvals; tunnel.

Wave 4: eval cases to 20+, rubric evaluator, final report; README, diagram, ADRs, threat
model, AI_USAGE.md, DECISIONS.md; OpenAPI tests; status p95; demo script.

## 10. Product decisions and assumptions

Signed off (DECISIONS.md D-006 to D-014). The three asked in this revision, answered on
2026-10-15:

- D-012, 5.14 rule 12: OpenClaw Docker sandboxing is always on (Andre chose this over
  the recommended in-container allowlist default).
- D-013, 5.8: OpenClaw's native Telegram channel (Andre chose this over the recommended
  in-repo adapter).
- D-014, 5.3 rule 5: gated platform tools block the MCP call up to a wait, then fall
  back to engine-side execution after approval.

Assumptions made without asking: `claude-opus-5-5` default; single operator; sessions
per node visit are deleted after the run is stored; OpenClaw's own cron and sub-agent
tools are denied for platform-managed agents so orchestration stays in the workflow
engine.
