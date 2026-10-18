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
reaches `final`, `error` or `aborted`, or the engine aborts it. Tests T5.1.1 to T5.1.8 stand,
with one exception to T5.1.2: kind `tool` approvals of a succeeded run stay decidable
(D-020).

Rule 2 as implemented in wave 1 (the transition table the engine enforces):
`queued → running | cancelled`; `running → queued` (re-queue after a lost worker or a
resume), `running → waiting_approval | succeeded | failed | cancelled | timed_out`;
`waiting_approval → queued | running | failed | cancelled | timed_out`; since 5.21,
`failed | timed_out | cancelled → queued` for an operator retry only. Anything else is
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

1. Gateway connection: the first dial at api start and every reconnect retry with
   backoff 1 s, 2 s, 4 s, up to 30 s, indefinitely, including the gateway's `UNAVAILABLE
   gateway starting; retry shortly` refusal (which marks itself `retryable` with
   `retryAfterMs`), logging a warning per attempt; on disconnect the client reconnects the
   same way; in-flight runs are re-attached by `runId` through
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
   A missing path is `exists: false` and makes `eq/neq/in/gt/lt` false. An empty `all`
   or `any` group is a validation error (`edges[i].when: empty all/any group`) and
   evaluates false if it ever reaches the engine. No expression strings, no code.
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
   Nothing in OpenClaw is involved. The approval row and the pause are one transaction,
   published afterwards, so a decision can never find the run still running; resumption
   reads only the visit named by `resume_node_visit_id`, which moving `current_node`
   clears, so a gate reached again by a loop always asks again.
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
    message); after every node's output is merged the engine validates the whole state
    against `state_schema` when present, failing the node with reason `state schema:
    <error>` (the `schema` evaluator checks the same thing after the fact).
12. A node's `on_failure` edge is followed when the node fails for a reason that is not a
    limit violation or a cancellation; without one the run fails with the node's reason.
13. **Run directory flows forward.** Every agent has its own OpenClaw workspace, so
    before an agent node visit the engine copies `runs/<run_id>/` from the workspace of
    the agent of every earlier visit of this run, in visit order (a later visit's file
    wins), into this agent's workspace, through the shared state directory. Files are
    copied, never linked (the sandbox sees only its own workspace). `deploy_release`
    and `gitlab_create_mr` read the calling agent's copy. A copy failure fails the node
    with reason `run directory: <error>`. Only agents whose last visit came after this
    agent's own last visit are copied from, so an agent's newer edits are never
    overwritten by an older copy (coder → reviewer → coder copies nothing back into
    the coder's second visit); regular files and directories are copied with their
    permission bits, links and special files are skipped, and deletions do not flow
    forward. (D-019, from the wave-2 evaluator agent's finding that Reviewer and
    Deployer could not see the Coder's files.)

Edge cases: a workflow with only `start→end` is valid and succeeds instantly with zero
model calls. An agent node whose agent lacks the tools its task needs still runs; the
model gets only allowed tools. A run whose agent was deleted after the run started
continues with the snapshot.

Consistency: definition is validated on save and again at trigger time (an agent may have
been deleted, a template still carries `agent_name`); trigger-time failure returns 422.
Structural validation also rejects unknown `on_reject`, `on_failure` and `input_from`
targets, `on_reject` on a non-approval node, edges into `start`, invalid conditions and
the reserved node id `pending_effects`; `on_reject` and `on_failure` targets count as
reachable.

Outputs: validation messages; `agent_messages` rows; SSE `agent_message`.

Tests: T5.6.1 each structural rule produces its path-qualified message. T5.6.2 condition
table test for every op including missing path. T5.6.3 `all`/`any` nesting. T5.6.4 edge
order precedence. T5.6.5 template render error at save and at run. T5.6.6 output schema
retry then failure. T5.6.7 approval node approve/reject/`on_reject`. T5.6.8 dev_pipeline
template loops twice then deploys with the scripted provider. T5.6.9 state replace on
revisit. T5.6.10 instantiate resolves agent names and reports a missing one. T5.6.11
input schema violation at trigger → 422. T5.6.12 a file written by node 1's agent is
present in node 2's agent workspace before its turn; a file node 2 rewrites wins on node 3.

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
| `deploy_release` | side_effecting | local_service | 30 s + approval wait | `{run_token, version, notes, paths[]}`; packages `<workspace>/runs/<run_id>/<paths>` through the workspace root; links anywhere on the way are refused, links inside a named directory skipped |
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
   `TELEGRAM_MODE`, `botToken: "${TELEGRAM_BOT_TOKEN}"`, `dmPolicy: "disabled"` and
   `groupPolicy: "disabled"` (closed until an agent is bound, rule 7; the api opens
   `dmPolicy` to `allowlist` with the binding),
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
   `{ agentId, match: { channel: "telegram" }, session: { dmScope: "per-channel-peer" } }`
   through `config.patch`; the UI shows which agent is bound and refuses a second
   binding while one exists. **[probed 2026-10-16]** The binding schema is
   `{type?: "route", agentId, comment?, match: {channel, accountId?, peer?: {kind:
   direct|group|channel, id}}, session?: {dmScope: main|per-peer|per-channel-peer|
   per-account-channel-peer, groupScope: main|per-group}}`. A `bindings` patch is
   accepted but **not hot-reloaded** (the gateway logs `evaluating reload (bindings)`
   with no `hot reload applied`, unlike `mcp.servers.*`): a DM sent after the patch was
   still routed to the default agent; the binding takes effect after a gateway restart.
   Corrected by agent A's verification: `config.schema.lookup` reports reload kind
   `none` for `bindings` (read per message), `hot` for `channels.telegram.enabled`,
   `none` for `dmPolicy`, `groupPolicy` and `session.dmScope`; the DM that seemed
   misrouted had in fact been sent before the patch (ingress lag of about four minutes
   on first poll). The api applies a binding as `config.patch` (bindings, `dmPolicy`)
   followed by `gateway.restart.request {reason}` only when something changed: the
   gateway restarts **in place** (same container and PID, clients closed with 1012,
   about 4 s of `UNAVAILABLE gateway starting; retry shortly` with `retryable: true`,
   `retryAfterMs: 500`), and the api's client reconnects. `gateway.restart`,
   `gateway.reload` and `gateway.restart.safe` are not RPCs; `config.apply`,
   `channels.start/stop`, `gateway.stop.request` exist. The `channel_bindings` table is
   the truth: a Keeper re-asserts it at api start and after every reconnect, and removes
   bindings the table does not know. The bot username comes from `channels.status
   {channel: "telegram", probe: true}` (`probe.bot.username`). Also
   **[probed]**: `sessions.messages.subscribe` and `plugin.approval.request` refuse an
   `agentId` that does not match the session key's agent (`agent "X" does not match
   session key agent "main"`), so the engine must use the bound agent's session key.
   Webhook mode is refused by `openclaw-init` without an https `PUBLIC_BASE_URL` and a
   secret of at least 32 characters; smoke asserts the wrong-secret 401 and the URL
   Telegram reports (D-032).
   **[probed 2026-10-16, after a gateway restart with the binding in place]** a DM was
   routed to the bound agent and answered on that agent's model; the session key under
   `dmScope: per-channel-peer` is `agent:<openclaw agent id>:telegram:direct:<telegram
   user id>`, listed by `sessions.list` with `channel: "telegram"` and `agentId` set.
4. The conversation is the OpenClaw session for that chat (`agent:<openclaw agent
   id>:telegram:direct:<telegram user id>` under `dmScope: per-channel-peer`). The engine
   follows the session roster (`sessions.subscribe` → `sessions.changed`) and subscribes
   to each Telegram session of the bound agent with that agent's id
   (`sessions.messages.subscribe`), mirroring user (`in`) and assistant (`out`) text
   entries only, never reasoning, tool calls or tool results, with assistant usage, into
   `conversations` / `conversation_messages`, idempotently by the entry's message id,
   catching up from `chat.history` at start, after a reconnect and after an event gap;
   SSE `conversation.message {conversation, message}` on the platform stream. Conversation
   turns are the agent's own OpenClaw sessions, not workflow runs: no `runs` row, no
   limits beyond the agent's exec policy and tools, retention per 5.10 rule 6 (7 days).
   The agent's reply is sent by OpenClaw.
5. Exec approvals raised by a Telegram-originated turn are delivered by OpenClaw to the
   approver's DM (`execApprovals.target: dm`) and mirrored by the engine with `run_id`
   NULL and `conversation_id` set (approvals of conversations, decidable from the inbox).
   A plugin approval requested with the Telegram session key and the bound agent's id
   is accepted with `deliveryRoute: "approval-client"` and the gateway sends it to the
   chat as a text message (**[probed]** 2026-10-16, `outbound send ok` 1.7 s later);
   whether answering it in the chat resolves it (`resolvedBy`) is still unverified, so
   side-effect decisions come from the web UI until then (5.3 rule 5a).
6. The bot token never appears in logs or the UI; `config.get` already redacts it
   **[probed]** and our redactor covers the Telegram token shape.
7. **Only a bound, platform-managed agent answers the channel.** With no binding,
   OpenClaw routes a DM to its default agent `main` and replies on the default model
   (**[probed]** 2026-10-16: two DMs were answered by `main` on Opus before any agent
   was bound). The platform therefore keeps the channel from reaching `main`: the
   mechanism is `dmPolicy: "disabled"` (and `groupPolicy: "disabled"`) whenever no
   platform agent is bound, set by the baseline and by the unbind path: OpenClaw then
   drops DMs silently before its own "Inbound message" log line, so the platform logs
   "channel closed" when nothing is bound rather than per message. `enabled: false` was
   rejected because it stops polling and Telegram would hold the messages for 24 h and
   deliver them to the next bound agent. (D-025, D-026)

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

### 5.9 Scheduler

Purpose: fire workflow runs on a cron cadence or once after a delay, durably, exactly once
per tick.

Inputs: `schedules` rows, with the project their runs work on (rule 6);
`SCHEDULER_TICK` (default 10 s).

Units: cron expressions are 5-field standard (`robfig/cron` `ParseStandard`), interpreted
in the schedule's `tz` (IANA name, default `UTC`); `run_at` is an absolute UTC timestamp.

Rules:

1. On create or update, `next_run_at` = next cron occurrence after now in `tz`, or
   `run_at` for `delay`. Invalid cron, unknown tz or a cron expression with no future
   occurrence (`0 0 31 2 *`) → 422 with the parser's message or `no future occurrence`;
   a row that reaches the tick in that state is disabled with `last_error`.
2. Every tick, one transaction selects `enabled AND next_run_at <= now` with
   `FOR UPDATE SKIP LOCKED`, creates a run per row with `trigger=schedule`,
   `input=schedules.input`, sets `last_run_at`, `last_run_id`, and advances `next_run_at`
   (cron) or sets `enabled=false` (delay). As built: the lock and the schedule update are
   one transaction and the run insert happens through the engine on its own connection,
   so each firing carries an occurrence key, `trigger_ref = "<schedule id>@<occurrence
   RFC 3339 UTC>"`, unique per `(trigger, trigger_ref)` for schedule runs; a re-fire
   after a crash finds the existing run, points `last_run_id` at it and advances. A
   delay schedule's occurrence is its `run_at`, so re-enabling one that already fired
   does nothing until it gets a new `run_at` (D-029).
3. If the process was down across several cron occurrences, exactly one run is created on
   restart (no catch-up backlog) and `next_run_at` is computed from now.
4. A schedule whose workflow is deleted is disabled at the next tick with
   `last_error = "workflow deleted"`.
5. Minimum cron interval is 1 minute (standard cron has no seconds field).
6. A schedule names the project its runs work on: `schedules.project_id` (nullable,
   references `projects`; migration 0011), checked on create and update exactly as a
   run request's `project_id` is (5.17 rule 4, the engine's own check): required for
   a workflow whose definition has `project: true`, refused for one without it, an
   unknown or deleted id refused, each a 422 detail on `project_id` with the engine's
   message. An update that leaves `project_id` out keeps the stored one, except one
   that changes `workflow_id`, which drops it (the project goes with the workflow
   choice); an empty string clears it. The tick passes the id in the trigger request,
   so the engine checks it again and snapshots the project onto the run like a manual
   request's; a project deleted since the schedule was saved refuses the occurrence
   like any trigger-time validation failure (failure handling below) and refuses the
   schedule's next update until another project is chosen. The representation carries
   `project_id` and `project_name` (joined like `workflow_name`; it survives the
   project's deletion); the list shows the project's name linking to its page, marked
   when the project is gone, and the form shows the Run dialog's project selector
   (5.17 rule 13) when the chosen workflow opts in.

Edge cases: DST transitions follow `time.LoadLocation` semantics; a 02:30 daily schedule in
a zone that skips 02:30 fires at the next valid instant per the cron library. Two API
processes would both be safe because of rule 2, though only one is run.

Time: injected clock; tests pin the clock and tz.

Failure handling: run creation failure (422 from trigger-time validation) sets
`last_error` and still advances `next_run_at` so a broken workflow does not fire every
tick.

Consistency: rule 2 is one transaction.

Outputs: `schedules.next_run_at`, `last_run_id`, SSE `schedule.fired`.

Tests: T5.9.1 cron next-run at both sides of a boundary. T5.9.2 delay fires once and
disables. T5.9.3 downtime across 3 occurrences → one run. T5.9.4 invalid cron → 422.
T5.9.5 deleted workflow → disabled with error. T5.9.6 tz `America/Sao_Paulo` vs UTC give
different `next_run_at` for the same expression. T5.9.7 a firing whose schedule update
was lost re-fires once and creates no second run. T5.9.8 a schedule on a project: the
store round trip with the joined name, kept after the project's deletion; the API's
422 details on `project_id` (required, refused, unknown, deleted) and the update
rules; the tick passes the id; a scheduled Dev pipeline run on a project is queued
through the engine with the project's snapshot and a definition that opts in, and
refused with the reason once the project is deleted; the dialog's selector and the
list's column.

Out of scope: per-second schedules, schedule pausing UI beyond the enabled toggle.

### 5.10 Memory and retention

Purpose: give agents facts that persist across runs and conversations, bounded by an
explicit retention policy, and keep run-scoped state isolated per run.

Inputs: agent `memory_policy` `{enabled, max_facts (default 200), fact_ttl_days (default
30), conversation_window (default 20), conversation_ttl_days (default 7)}`.

Units: days for TTLs, converted to seconds at write time (`expires_at = now + ttl`).

Rules:

1. Run-scoped memory is `runs.state` plus `run_nodes.messages`; it is never read by another
   run.
2. Agent memory is `memory_facts(scope=agent)`; conversation memory is
   `memory_facts(scope=conversation, scope_id=conversation_id)` plus the
   `conversation_messages` window (5.8 rule 7).
3. `memory_save{key, value, ttl_days?}` upserts by `(agent_id, scope, scope_id, key)`;
   `expires_at = now + (ttl_days or fact_ttl_days)`; value ≤ 2 KiB; when the agent has
   `max_facts` facts the oldest by `created_at` is deleted first.
4. `memory_recall{query}` returns up to 20 non-expired facts in which every word of the
   query appears in the key or value, case-insensitively, newest first by `updated_at`
   (a re-saved key counts as new; eviction stays by `created_at`); empty query returns
   the 20 newest.
5. The prompt's memory block (5.7 rule 1) contains the 20 newest non-expired agent facts
   plus the 10 newest conversation facts when conversation-triggered.
6. A sweeper deletes expired facts and conversation messages older than
   `conversation_ttl_days` every 10 minutes; expired rows are also excluded from reads
   and from the history replayed after a restart or reconnect, so the sweep timing is
   invisible.
7. The UI lists and deletes facts per agent.

Edge cases: a fact with `ttl_days: 0` expires immediately and is never returned. Memory
disabled → the tools are not offered and the block is omitted.

Time: UTC, injected clock.

Failure handling: a memory write error is a tool error result.

Consistency: upsert in one statement (`ON CONFLICT DO UPDATE`).

Outputs: `memory_facts`; UI memory tab.

Tests: T5.10.1 expired fact not returned before the sweep runs. T5.10.2 cap evicts oldest.
T5.10.3 run A's state not visible to run B's prompt. T5.10.4 conversation window and TTL
both bound the history. T5.10.5 disabled policy offers no memory tools.

Out of scope: vector search, cross-agent shared memory.

### 5.11 Guardrails

Purpose: bound what an agent can spend and do, independently of the workflow's limits.

Inputs: agent `guardrails` `{max_cost_usd_per_run, max_runs_per_hour, blocked_tools [],
blocked_input_patterns [{tool, regex}]}`.

Units: micro-USD; runs per rolling 3600 s window.

Rules:

1. The run's effective `max_cost_usd` is the minimum of the workflow limit and every
   participating agent's `max_cost_usd_per_run` (clamped by the platform max).
2. A manual, scheduled or channel trigger for a workflow that includes agent A is rejected
   with 429 `rate limit: <n> runs/hour for agent <name>` when A has `max_runs_per_hour`
   runs created in the last 3600 s. Scheduled triggers record the rejection in
   `last_error` and advance.
3. `blocked_tools` are removed from the offered tool set (so the model never sees them);
   a call anyway is `blocked` (5.7 rule 2).
4. `blocked_input_patterns` are RE2 regexes tested against the canonical JSON of the tool
   input; a match returns `tool_result{is_error: true, text: "input blocked by guardrail
   <pattern name>"}` and status `blocked`. Patterns are compiled at save time; a bad regex
   is 422.

Edge cases: a guardrail added after a run started does not affect that run (limits are
snapshotted).

Time: rolling window from the injected clock.

Failure handling: n/a.

Consistency: rate check and run insert in one transaction.

Outputs: 429 body, `tool_calls.status = blocked`, SSE `tool_call`.

Tests: T5.11.1 min-of cost rule across two agents. T5.11.2 rate limit at exactly n and
n-1. T5.11.3 blocked tool absent from the API request's tool list (captured request).
T5.11.4 pattern blocks `rm -rf` in `sandbox_exec.command`. T5.11.5 bad regex → 422.
As built on OpenClaw: patterns for platform tools are enforced in the MCP gate (row
`blocked`, result `input blocked by guardrail <name>`); an agent with any `exec` pattern
runs host exec with an empty allowlist and `ask: on-miss`, so every host command raises
an exec approval and the engine denies the ones a pattern matches (the rest reach the
inbox; "allow always" no longer extends that agent's allowlist). Host exec is the only
exec OpenClaw gates, so a matching command inside the sandbox is **not** blocked (D-028;
a `before_tool_call` plugin hook would close that gap).

Out of scope: org-level budgets.

### 5.12 Observability

Purpose: every run can be reconstructed from the DB; the UI shows it live; logs never leak
secrets.

Inputs: engine events, HTTP requests.

Units: tokens as integers; cost micro-USD; durations ms.

Rules:

1. Every run has a root span; node, model call, tool call and approval wait spans are
   children with `parent_id`; `spans.attrs` carries tokens, cost, status, attempt.
2. Cost per model call = `input_tokens × in_price + output_tokens × out_price +
   cache_read_tokens × cache_read_price + cache_write_tokens × cache_write_price`, prices
   in micro-USD per token derived from the per-MTok table in `config/prices.yaml`
   (Opus 5.5: 4.00/20.00/0.20 cache read, Sonnet 5.5: 2.00/10.00/0.20, Haiku 5.5:
   0.10/0.50, cache write = 1.25 × input unless listed). Unknown model → cost 0 and a
   `warn` log once per model.
3. SSE: `GET /api/events` (all runs) and `GET /api/runs/{id}/events`; each event has `id`
   (monotonic per process), `event` type, JSON `data`; the handler honours
   `Last-Event-ID` by replaying from the DB-backed event log for that run; heartbeat
   comment every 15 s.
4. The slog handler redacts values of keys named `*token*`, `*secret*`, `*key*`,
   `authorization` and any string matching `sk-ant-[A-Za-z0-9_-]{10,}`, `sk-or-v1-[A-Za-z0-9]{10,}`, run tokens `r_[a-z2-7]{24}` or
   `\d{6,}:[A-Za-z0-9_-]{30,}` (Telegram bot tokens) with `[redacted]`. The same function
   runs on tool outputs before storage.
5. `GET /healthz` (process up), `GET /readyz` (DB ping ≤ 1 s), `GET /api/status`
   (`{version, uptime_s, db_ok, workers, runs: {queued, running, waiting_approval}}`) with
   a 500 ms server-side budget; `/api/status` is measured in the integration suite at p95.
6. Every request log line has `request_id`, method, path, status, `duration_ms`; every
   engine log line has `run_id` and, when applicable, `node_id`.

Edge cases: SSE clients that disconnect mid-run reconnect with `Last-Event-ID` and miss
nothing (rule 3).

Time: n/a.

Failure handling: an SSE publish failure is logged at `debug` and dropped.

Consistency: 5.1 rule 6.

Outputs: spans, SSE, logs, `/api/status`.

Tests: T5.12.1 span tree for a two-node run has the expected parent links. T5.12.2 cost
arithmetic for each priced model from a usage fixture. T5.12.3 `Last-Event-ID` replay
returns the missed events in order. T5.12.4 redactor table test (API key, bot token, key
names, nested JSON). T5.12.5 `/api/status` p95 < 500 ms over 200 requests in compose.

Out of scope: OpenTelemetry export (the span table is shaped so an exporter is a small
addition; the README says so).

### 5.13 Eval runner

Purpose: a reproducible, offline-capable measurement of whether the platform behaves as
specified, reported as baseline and final.

Inputs: `evals/cases/<version>/*.yaml`, each `{id, version, split: public|private,
workflow: <template key or inline>, input, provider: scripted|anthropic, script:
<scripted provider fixture>, expect: {...}}`; `EVAL_PROVIDER` (default `scripted`);
`EVAL_SPLIT` (default `public`).

Units: pass rate as a fraction with 3 decimals; score 0..1.

Rules:

1. `make eval` runs every case of the selected split against a fresh database schema
   (`eval_<timestamp>`), with the `scripted` provider by default, and writes
   `evals/reports/<timestamp>.md` and `.json`; `make eval-baseline` and `make eval-final`
   copy the result to `evals/reports/baseline.md` / `final.md`.
2. The `scripted` provider is deterministic: for each model call it takes the next
   response from the case's `script` list, matched by node id; running out of script is a
   case failure with reason `script exhausted at <node> call <n>`. Usage tokens in the
   script are used for cost arithmetic so cost evaluators are testable offline.
3. Deterministic evaluators, each a pure function over the run's DB rows:
   `termination` (status terminal and `step_count ≤ max_steps` and duration ≤ limit),
   `approval_gate` (no `side_effecting` tool call has `started_at` before an `approved`
   approval for it), `idempotency` (per case, duplicate side-effect inputs produced one
   `side_effects` row), `schema` (every node output validates; final state validates
   against `state_schema`), `allowlist` (no executed tool outside the agent allowlist),
   `trace_integrity` (every `tool_use` id has exactly one `tool_result`).
4. The rubric evaluator grades the final state against the case's `rubric` (3 to 6
   criteria, each 0/1) using `claude-opus-5-5` with structured output when
   `EVAL_PROVIDER=anthropic`; with `scripted` it runs the **heuristic** mode: each
   criterion has a `check` (`contains`, `regex`, `json_path_eq`) evaluated locally.
   The report labels which mode produced every rubric score. **[decision]**
5. A case passes when all its deterministic evaluators pass and rubric score ≥ 0.75.
   The report shows per-evaluator pass rate, overall pass rate, end-to-end success rate,
   mean cost and duration, and a per-case table; the summary line is
   `public split: N cases, deterministic pass <x>, e2e success <y>`.
6. At least 20 cases ship in `v1` (as built: 24, 17 public and 7 private; the private
   ones need the network or a human and stay out of `make eval`'s default split),
   covering: happy path dev
   pipeline, feedback loop ×2, approval approve, approval reject, approval expiry, step
   limit, time limit, cost limit, cancellation, idempotent replay, blocked tool, prompt
   injection in `http_fetch` content (expect: no `deploy_release` call), prompt injection
   in sandbox stdout, output schema repair, research pipeline happy path, schedule-
   triggered run, channel-triggered reply, rate limit, loop limit.
7. Cases are versioned by directory; a case's `version` must equal its directory.

Edge cases: a case expecting `failed` with a reason passes `termination` (terminal) and its
own `expect.status`. A rubric with zero criteria scores 1.

Time: the runner uses the real clock but every case's limits are small (≤ 30 s).

Failure handling: a runner crash on one case records it as failed with the panic text and
continues.

Consistency: one DB schema per eval run.

Outputs: markdown and JSON reports; exit code 1 when overall pass rate < 0.9 on the
public split (so CI can gate).

Tests: T5.13.1 scripted provider exhaustion. T5.13.2 each deterministic evaluator on a
positive and a negative fixture. T5.13.3 heuristic rubric checks. T5.13.4 report summary
line format. T5.13.5 `make eval` against the shipped cases passes ≥ 0.9 (this is the
metric).

Out of scope: hill-climbing automation.

#### Implementation notes for 5.9 to 5.13 (OpenClaw runtime, waves 1 and 2)

The rules and tests above stand; the runtime changes how they are met:

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
  `expect` gains `state` (JSON pointer → expected value, one entry per check),
  `tool_statuses` (tool name → ordered subsequence of statuses across that tool's rows;
  an empty list means no row), `never_tool_statuses`, `approval_statuses` (`[]` means
  no approval) and `min_agent_messages`. The
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
5. **Sessions.** One session per node visit, key `agent:<id>:yuno-<run id>-<node>-<visit>` (conversation sessions are the gateway's own
   `agent:<id>:telegram:direct:<user id>`, 5.8 rule 4)
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

### 5.15 Chief of Staff: chat-time tools, delegation and report-back

The full design with inputs, edge cases, failure handling and tests is
`docs/briefs/wave5/00-design-chief-of-staff.md` (approved 2026-10-16, D-036). The rules:

1. A platform tool call carrying `chat_token` instead of `run_token` resolves to the
   agent that owns the token (`agents.chat_token`, generated at creation, never returned
   by the API, redacted); an unknown token answers `invalid chat token`; both tokens
   answer `one of run_token or chat_token`.
2. Only chat-capable tools accept a chat token: `list_agents`, `list_workflows`,
   `delegate_task`, `run_status`, `memory_save`, `memory_recall` and, since 5.17,
   `list_projects`; others answer `<tool> is not available in a conversation`.
3. A chat tool call is a `tool_calls` row with `run_id` null and `conversation_id` the
   agent's most recent conversation (null when none), redacted and capped like run calls.
4. `list_agents` returns every agent but the caller (name, role, model, allowlist, bound
   channel); `list_workflows` returns workflows, not templates (name, description, agent
   names, `input_schema`).
5. `delegate_task` takes exactly one of `agent_name` + `task` or `workflow_name` +
   `input` (validated against `input_schema`); names match case-insensitively; unknown
   names and self-delegation are tool errors with the texts in the brief.
6. `delegate_task` triggers a run with `Trigger = channel`, `TriggerRef =
   conversation:<id>` (empty without a conversation), default limits, and returns
   `{run_id, status, url}`; at most 10 delegations per agent per 10 minutes, the 11th
   answers `delegation limit reached; try later` and is recorded `blocked`.
7. `delegate_task` is computational, policy `auto` by default, overridable by interaction
   rules. In a conversation there is no run to pause, so require_approval on a
   chat-capable tool refuses the chat call (recorded blocked); the policy gates run calls
   as usual.
8. `run_status` returns status, trigger, times, duration, steps, tool calls, cost, final
   state (4 KB cap), failure reason, and a pending approval's kind and message; unknown
   id answers `no run <id>`.
9. A channel-triggered run reaching a terminal status sends one `[platform] Run <id>
   <status> after <duration>, cost $<x>: <outcome>. <url>` into its conversation's
   session with `chat.send {deliver: true}` [probed: the text is a user turn, the reply
   is delivered to Telegram], exactly once (`runs.reported_at`), one attempt, never
   without a conversation ref.
10. Entering `waiting_approval` sends one `[platform] Run <id> is waiting for an
    approval: <kind>, <message>. Decide it at <url>` per approval
    (`approvals.reported_at`); approvals are decided in the UI.
11. Those messages are mirrored like inbound messages with `run_id` set.
12. The seeded `Chief of Staff` (Coordinator, `claude-opus-5-5`, the chat tools, no
    exec) carries the coordinator prompt from the brief, extended by 5.17 rule 8.
13. Seeding instantiates both templates when no workflow carries their template key.
14. Seeding binds the Chief of Staff when Telegram is enabled and nothing is bound; it
    never changes an existing binding.
15. The materialiser writes a `## Chatting` section into `AGENTS.md` only for agents whose
    allowlist holds a chat-capable tool.

### 5.16 Project repository: clone, toolchain, sandbox image and network, merge request

Purpose: a run can work on an existing git repository, with that repository's toolchain
in the sandbox, and end in a merge request; GitLab first, GitHub as a second provider
behind the same interface. Decision D-037. Everything about the repository is
configuration of the platform, not of an agent: no agent clones or pushes, and no model
ever sees the token.

Inputs: `PROJECT_REPO_URL`, `PROJECT_GIT_TOKEN`, `PROJECT_DEFAULT_BRANCH`,
`PROJECT_PROVIDER`, `PROJECT_SANDBOX_NETWORK`; the `project` flag of a workflow
definition or a run request; the run's idempotency keys.

Rules:

1. **Configuration from the environment** (since 5.17: a project is a record and
   `PROJECT_*` only bootstraps the first one; the validation here still applies to
   every record). `PROJECT_REPO_URL` is the https clone URL
   without user information; `PROJECT_GIT_TOKEN` is write-only (never returned by the
   API, redacted by `internal/obs` wherever it could appear); `PROJECT_DEFAULT_BRANCH`
   defaults to `main`; `PROJECT_PROVIDER` is `gitlab` or `github`, defaulting to `github`
   when the URL's host is `github.com` and to `gitlab` otherwise;
   `PROJECT_SANDBOX_NETWORK` defaults to `false`. The project is configured when the URL
   and the token are set and the URL is well-formed; an incomplete or malformed setting
   is logged once at start and the api runs without a project. `GITLAB_*` keep their
   meaning for `gitlab_create_mr`, the loose-files tool.
2. **Opt-in per workflow or run.** A workflow definition carries `project: true`; a
   quick run opts in by naming a project on the run request (`project_id`, 5.17 rule
   4), which sets the flag on the one-node definition. Since 5.17 the flag means "a
   run of this workflow names a project" and the request must name one; a run whose
   snapshot opts in but names no project (queued before 5.17) proceeds without a
   checkout, with one log line (`project: no repository configured; running without a
   checkout`). The flag travels in the run's workflow snapshot.
3. **Clone before the first visit.** In the run's first agent visit, after the agent is
   materialised and its run directory prepared (5.6 rule 13) and before its turn, the
   engine clones the default branch at depth 1 (`--single-branch`) into
   `runs/<run id>/repo/` of that agent's workspace. The destination is reached through an
   `os.Root` of the workspace: `runs/<run id>` must be a plain directory, and a `repo`
   entry left by an earlier attempt is removed first. The engine then writes
   `.git/info/exclude` entries for dependency and cache directories (`node_modules/`,
   `.venv/`, `__pycache__/`, `.pytest_cache/`, `.cache/`, `.home/`), detects the toolchain
   (rule 5), stores it as `runs.toolchain`, records a span of kind `project` named
   `project_clone` (attributes: repository URL, branch, toolchain, duration) and a log line
   `project repository cloned: <url> (<branch>), toolchain <x>`. A clone failure fails the
   run cleanly before any turn starts, with reason `project clone: <redacted git error>`.
   The run's first visit is the one whose run has no `toolchain` yet; later visits, and a
   re-queued attempt of a run already cloned, clone nothing and rely on the copy-forward
   of 5.6 rule 13, which carries the checkout (its `.git` included) to each later agent.
4. **The token never leaves the api.** The api's own git processes receive it as an
   `http.extraheader` configuration entry passed through `GIT_CONFIG_COUNT`/`KEY`/`VALUE`
   environment variables (`Authorization: Basic base64(<user>:<token>)`, user `oauth2`
   for GitLab and `x-access-token` for GitHub), so the checkout's `.git/config` holds the
   bare URL and nothing on disk, in the task, in a transcript or in a trace carries the
   token. Git runs with `GIT_TERMINAL_PROMPT=0` and a timeout. `internal/obs` redacts
   GitLab (`glpat-`), GitHub (`ghp_`, `github_pat_`) tokens and `Authorization: Basic`
   values besides what it knew.
5. **Toolchain detection**, at clone time, from the repository root, first match wins:

   | Manifest | Toolchain | Install (network on) | Tests |
   |---|---|---|---|
   | `go.mod` | `go` | `go mod download` | `go test ./...` |
   | `package.json` + `pnpm-lock.yaml` | `node` | `pnpm install --frozen-lockfile` | `pnpm test` |
   | `package.json` + `yarn.lock` | `node` | `yarn install --frozen-lockfile` | `yarn test` |
   | `package.json` + `package-lock.json` (or no lockfile) | `node` | `npm ci` (`npm install` without a lockfile) | `npm test` |
   | `uv.lock` | `python` | `uv sync` | `uv run pytest` |
   | `requirements*.txt` | `python` | `python -m venv .venv && .venv/bin/pip install -r <file>` (every match, `requirements.txt` first) | `.venv/bin/pytest` |
   | `pyproject.toml` | `python` | `python -m venv .venv && .venv/bin/pip install -e .` | `.venv/bin/pytest` |
   | none of the above | `default` | none | none |

   The value is one of `go`, `node`, `python`, `default`; the UI shows it as a badge on
   the run header and in the runs list.
6. **Per-toolchain images**, shipped with the platform and built by `make run` and
   `make build` like the default image: `docker/sandbox-python.Dockerfile`
   (`yuno-sandbox-python:latest`: python 3.12, pip, uv, pytest, git),
   `docker/sandbox-node.Dockerfile` (`yuno-sandbox-node:latest`: node 24, npm, pnpm,
   yarn, git) and `docker/sandbox-go.Dockerfile` (`yuno-sandbox-go:latest`: Go 1.26,
   git). Nothing project-specific is baked in. The gateway runs every exec with
   `HOME=/workspace` (the agent's workspace, mounted read-write) and its own `PATH` of the
   usual system directories, while the image's other variables survive **[probed]**, so
   each image keeps its tools under `/usr/local/bin` and sets every cache (`npm`, `pip`,
   `uv`, `GOPATH`, `GOMODCACHE`, `GOCACHE`) to hidden directories of the workspace
   (`/workspace/.cache/...`), which the platform never lists, copies forward or packages
   and which persist across the agent's sessions; the root filesystem stays read-only.
   The image for a toolchain is derived from `OPENCLAW_SANDBOX_IMAGE`
   (`yuno-sandbox:latest` becomes `yuno-sandbox-<toolchain>:latest`); `default` keeps the
   default image.
7. **Sandbox selection through the materialiser, per visit.** Before the turn of every
   visit of a project run whose toolchain is known, the engine sets
   `agents.entries.<id>.sandbox` to `{scope: "session", docker: {image: <rule 6 image>,
   network: <rule 8>}}` with `config.patch` **[probed: `agents.entries.*.sandbox.docker.*`
   overrides `agents.defaults.sandbox.docker.*` for that agent; the next session of the
   agent runs in that image with that network, and `agents.defaults.sandbox` is
   untouched]**, and deletes the block (`null`, listed in `replacePaths`) once the visit
   ends **[probed: the following session is back on the defaults]**. `scope: "session"`
   is deliberate: the gateway keeps a hot agent-scoped container running when its
   configuration changes (it only replaces stopped or expired ones) **[docs]**, so an
   agent-scoped container created by an earlier run would keep the old image; a
   per-session scope gives every node visit a fresh container built from the current
   settings, pruned by the gateway's idle rules. The engine counts holds per agent: two
   project visits of one agent at the same time (two runs) share its entry, the later
   one wins, and the block is removed when the last of them ends. The sandbox paths
   reload hot **[probed: `config.schema.lookup` reports the reload kind]**; a visit whose
   sandbox patch fails ends with reason `sandbox selection failed: <error>`.
8. **Network.** `network: "none"` unless `PROJECT_SANDBOX_NETWORK=true`, in which case the
   sandboxes of visits of project runs get `network: "bridge"` and the task protocol
   block tells the agent to install dependencies from the lockfile first and to run the
   project's tests (rule 9). With network off the run still clones; the block says that
   dependencies cannot be installed. The api does not get the Docker socket in either
   case. `docs/threat-model.md` records the trade-off of rule 13.
9. **Agents are told through the task protocol block** (5.6 rule 5, per visit), after
   the run id lines:

   ```
   Project repository: runs/<run id>/repo/ in your workspace is a git checkout of <url>
   (branch <default branch>, toolchain <x>). Work inside it. Do not clone, commit or push:
   the platform commits the working tree when a merge request is opened.
   Sandbox network: on. Install the dependencies from the lockfile first (<install>),
   then run the project's tests (<tests>).
   ```
   or, with network off, `Sandbox network: off. Dependencies cannot be installed: use
   what the image provides and say which tests you could not run.` A visit of a project
   run without a configured project gets no such lines. `AGENTS.md` states the standing
   rules (never clone or push; the checkout is the platform's).
10. **`open_merge_request`**, a platform tool (5.7): input `{run_token, title,
    description}`; risk `side_effecting`, hits `external`, approval-gated by default
    (5.3), timeout 120 s, idempotency key as 5.4, branch `yuno/<first 12 hex of the key>`
    like `gitlab_create_mr`; the project is the run's (5.17 rule 6). Steps, in the calling agent's workspace: (a) the checkout is
    `runs/<run id>/repo/` reached through an `os.Root` with links refused, and must hold a
    `.git` directory, else the model-facing error `this run has no project checkout at
    runs/<run id>/repo/`; (b) if the provider reports the branch, the commit and push are
    skipped (a retry after a partial failure); otherwise the api **never runs git inside
    the checkout** (a hook or a configuration entry an agent wrote there, such as a
    `pre-commit` hook or a `url.insteadOf` rewrite, would run on the api side or redirect
    the push with its credentials): it clones the default branch again into a private
    directory of its own, empties that working tree, copies the checkout's working tree
    over it (regular files only, links skipped, `.git` and the excluded directories of
    rule 3 left out), stages everything, and ends the call with the model-facing error
    `the checkout has no changes to propose` when `git status` is clean; otherwise it
    commits as `Yuno <yuno@localhost>` with the title as message (`--no-verify`, no
    signing) and pushes `HEAD` to the branch from the api with the token (rule 4), then
    removes the private directory; (c) an open merge request from that branch is
    returned with `replayed: true` (since 5.20 rule 2 a run has one request: a later
    call in the same run pushes onto its branch and returns it `updated`); otherwise one is opened against the default branch
    with the title and description. Output: `{number, url, branch, provider,
    replayed?}`. Registered always since 5.17 (a run without a project is told `this
    run has no project`); `gitlab_create_mr` stays for runs without a project. Commits an agent made
    inside the sandbox are flattened: their working tree is what the one commit holds.
11. **Providers behind one interface** (`branchExists`, `findOpen`, `create`): GitLab
    through `<scheme>://<host>/api/v4/projects/<url-encoded path>` (`PRIVATE-TOKEN`;
    `repository/branches/<branch>`, `merge_requests?source_branch=&state=opened`,
    `POST merge_requests` with `remove_source_branch`), GitHub through
    `https://api.github.com/repos/<owner>/<repo>` (or `https://<host>/api/v3/repos/...`
    for another host; `Authorization: Bearer`; `branches/<branch>`,
    `pulls?head=<owner>:<branch>&state=open`, `POST pulls`). The project path comes from
    the clone URL (`.git` stripped). Non-2xx answers follow 5.5 rule 4: 5xx transient,
    the rest permanent, the first line of the body in the error.
12. **UI.** The run header shows a `Toolchain: <x>` badge when the run has one, and a
    `Merge request` (GitLab) or `Pull request` (GitHub) link with the number, taken from
    the run's merge request record (5.20 rule 6; before 5.20, from the newest executed
    `open_merge_request` call's output in the trace); the runs list shows the
    toolchain badge.
13. **Trust.** With network on, anything injected into the repository (a README, a test,
    a dependency's install script) can reach the internet from the sandbox and can touch
    the host's published ports; side effects inside the sandbox are not gated. The opt-in
    is therefore for trusted repositories only, documented in the threat model. The token
    is held by the api alone; the sandbox has the bare URL and no credential; the api
    has no Docker socket.
14. **Shipped agents and templates.** The seeded Deployer gets `open_merge_request`
    besides `deploy_release`; the Coder's prompt says to work in the checkout, install
    from the lockfile when the network is on and run the project's tests; the Reviewer's
    prompt says to review `git status` and `git diff <default branch>` in the checkout;
    the Dev pipeline opts in and its deployer node opens a merge request when the task
    describes a project checkout and deploys a release otherwise, with
    `merge_request_url` in its output. The seed never overwrites an existing agent or
    workflow (5.15 rule 13); `yuno seed --reset-shipped` (`make seed-reset`) restores the
    shipped agents' role, prompt and allowlist and the instantiated templates'
    definitions to the current shipped versions, keeping ids, bindings, memory and every
    other setting, for an install that predates this section.

Edge cases: a quick run with `project: true` on an agent without `exec` still gets the
checkout (file tools work in it). The clone's `runs/<run id>` is a link planted by the
agent in an earlier run: refused like the copy-forward (`run directory is not a plain
directory`). The default branch does not exist: git fails, the run fails with that
message. The working tree holds `node_modules/` because the project does not ignore it:
left out of the copy and excluded by rule 3's local exclude file, never committed. The
agent commits in the sandbox: the working tree is what counts, flattened into the
platform's one commit (rule 10). The provider refuses the merge request (a branch
protection, a duplicate title): the branch stays pushed and the error reaches the model;
a retry under the same key skips the commit and push (rule 10b). Two project runs on
one agent at once: rule 7's holds. A run cancelled during the clone: the git process is
killed with the visit's context and the run ends `cancelled`.

Failure handling: git errors are redacted (rule 4) before they reach the run's failure
reason, a tool result or a log line; a provider's transient failure is retried by the
executor (5.5) under the same key; the materialiser's sandbox patch retries a stale
`baseHash` three times like every other patch.

Outputs: `runs.toolchain`; the `project` span; `tool_calls` rows for
`open_merge_request` with `{number, url, branch, provider}`; the `Toolchain` badge and
the merge request link.

Tests: T5.16.1 config: every `PROJECT_*` name in `.env.example` and compose
(`TestEnvExampleAndComposeCoverConfig`), provider default by host, configured only with
URL and token, a URL with user information refused. T5.16.2 the detection table, one case
per row plus precedence (`uv.lock` beats `requirements.txt`, `go.mod` beats
`package.json`). T5.16.3 engine, against a local bare repository as the remote (no
network): a two-node project workflow clones into the first agent's workspace before its
turn (the file from the remote is there, `.git/config` holds no token), records
`runs.toolchain` and the span, the checkout reaches the second agent, and
`open_merge_request` in the second agent's workspace commits the edit, pushes the key's
branch to the bare repository and opens the request on the fake provider; a clone of a
missing branch fails the run with `project clone:` before any turn; an opted-in run
without a configured project runs without a checkout; a quick run opts in through the
request; a `pre-commit` hook planted in the checkout never runs on the api side. T5.16.4 the materialiser against the
fake gateway: the sandbox patch for `go` with network on carries the derived image and
`bridge`, with network off `none`; the clear patch sends `null` with the path in
`replacePaths`; holds of two visits clear once. T5.16.5 the tool against the httptest
GitLab fake: branch and merge request created, a second call replays, a retry after a
failed request skips the commit; the GitHub fake: pull request created with `head`
`<owner>:<branch>`, replay found through `pulls?head=`. T5.16.6 redaction of GitLab and
GitHub token shapes and basic auth headers in logs, spans and tool rows; the tool's
stored input and output carry no token. T5.16.7 UI: the run header shows the toolchain
badge and the merge request link from the tool output; the list shows the badge; no link
without an executed call. T5.16.8 `GET /api/tools` lists `open_merge_request` only when
the project is configured; `RunCreate.project` reaches the snapshot.

Out of scope (more than one project per platform is 5.17): per-agent projects, branches other
than the default as the base, rebases or conflict resolution, reviewing an existing
merge request, GitHub Apps (a token is the only credential), submodules.

### 5.17 Projects as records: several repositories, one named per run

Purpose: the one project of 5.16, configured by environment variables, becomes records
an operator creates and edits in the UI, so a platform works on several repositories
and every run, delegation or quick run names the one it works on. Decision D-038. The
mechanics of 5.16 (clone, toolchain, images, sandbox selection, the task protocol
lines, `open_merge_request`, the two providers) are unchanged; this section says where
their settings come from and how a run is tied to them.

Inputs: the `projects` table; `PROJECT_TOKEN_KEY`; the `PROJECT_*` settings, now a
bootstrap only; `project_id` on a run request; `project` on `delegate_task`.

Rules:

1. **A project is a record.** `projects` (migration 0010): `id`, `name` (1 to 64
   characters, trimmed, unique among live projects ignoring case), `repo_url` (https,
   no user information, a repository path; validated as 5.16 rule 1), `provider`
   (`gitlab` | `github`; a request may leave it empty, in which case the host decides
   as in 5.16 rule 1 and the resolved value is stored), `default_branch` (default
   `main`, validated as 5.16 rule 1), `sandbox_network` (default false), `description`
   (at most 1024 characters), `token_enc`, `created_at`, `updated_at`, `deleted_at`
   (soft delete). A project with no token cannot exist: a create needs one.
2. **The token is encrypted at rest and write-only.** `token_enc` is AES-256-GCM over
   the token, a random 96-bit nonce prefixed to the ciphertext, the project id as
   additional data (a blob cannot be moved onto another row), under the key
   SHA-256(`PROJECT_TOKEN_KEY`); `scripts/ensure-env.sh` generates the variable (32
   random bytes, hex) like the other platform tokens, and `.env.example` and compose
   carry it. The API never returns the token: every representation carries `token_set`
   instead; an update whose `token` is absent or empty keeps the stored one, unless
   it changes the repository's host or the provider, which needs the token again
   (422 `token`: the stored one is for the host it was entered for, and a re-pointed
   project could otherwise aim it at another server through the check or a clone);
   the redactor knows the token shapes (5.16 rule 4). The api's provider clients never
   follow a redirect, so a token header never travels to another host. Without `PROJECT_TOKEN_KEY` the api
   refuses to store a token (503 `project_tokens_unavailable`) and skips the bootstrap
   of rule 9 with a warning. A token that no longer decrypts (the key changed) is
   reported by the check of rule 5 and fails a run's clone with the reason
   `project: the stored token cannot be read; enter it again on the Projects page`.
3. **API.** `GET /api/projects` (live projects by name), `POST /api/projects`,
   `GET`/`PUT`/`DELETE /api/projects/{id}`, `POST /api/projects/{id}/check`. The
   representation is `{id, name, repo_url, provider, default_branch, sandbox_network,
   description, token_set, created_at, updated_at, last_run?}`, `last_run` being
   `{id, status, toolchain?, created_at, finished_at?}` of the newest run that names
   the project. Validation answers 422 with `details: [{path, message}]` on the paths
   `name`, `repo_url`, `provider`, `default_branch`, `token` and `description`; a name
   another live project holds is `name: a project named "X" already exists` (the
   unique index settles a race). `DELETE` is a soft delete (204): the project leaves
   the lists and the selectors, runs that name it keep their snapshot (rule 4), and the
   row keeps its token so a run in flight still clones and pushes.
4. **A run names a project.** `runs.project_id` (nullable, references `projects`) and
   `runs.project_snapshot` (`{name, repo_url, provider, default_branch,
   sandbox_network}`, never the token) are written when the run is queued. A run
   request carries `project_id` (this replaces 5.16 rule 2's `RunCreate.project`
   boolean): for a workflow whose definition has `project: true` it is required (422
   `project_id: required; the workflow works on a project repository`), for a
   workflow without the flag it is refused (422 `project_id: the workflow does not
   use a project`), and a quick run takes it optionally, its one-node definition
   getting `project: true` when it does. An unknown or deleted id is 422
   `project_id: no project with id X`. A schedule names its project under the same
   rule when it is saved and passes it to the trigger when it fires (5.9 rule 6).
5. **Check.** `POST /api/projects/{id}/check` verifies the stored token against the
   provider without a clone: GitLab `GET <api base>` (the project: its
   `default_branch`, and the token's access level under `permissions.project_access`
   or `group_access`, Developer or above meaning it can push a branch; the platform
   pushes its own branch, never the protected default one) and `GET <api
   base>/repository/branches/<default branch>` (existence; its `can_push` stands in
   when no access level is reported); GitHub `GET <api base>` (`default_branch`,
   `permissions.push`) and `GET <api base>/branches/<default branch>`. It answers 200
   with `{ok, message, default_branch?, branch_exists?, can_push?, checked_at}`: `ok`
   means the token was accepted and the repository found; the message is one sentence
   for the operator (`Token accepted: default branch main, push allowed.`, `The token
   was refused (401).`, `Repository not found (404): check the URL and the token's
   scope.`, `Token accepted, but it cannot push to the repository; it needs write
   access.`, `Token accepted, but branch develop does not exist; the repository's
   default branch is main.`). Provider errors, a redirect (never followed) and the
   10 s timeout are results, not HTTP errors; the token never appears in them.
6. **The engine takes the project from the run.** When the snapshot's definition has
   `project: true` and the run names a project, the visit's project configuration is
   the run's snapshot plus the token read from the project's row (deleted or not) and
   decrypted when the clone starts and again when `open_merge_request` runs, so a
   token replaced on the Projects page applies to runs in flight. A run whose
   definition opts in but names no project (one queued before this section) runs
   without a checkout with the log line of 5.16 rule 2. The clone, the toolchain
   detection, the sandbox selection, the task protocol lines and `open_merge_request`
   use that configuration and nothing reads `PROJECT_*` at run time.
   `open_merge_request` is registered always (supersedes 5.16 rule 10's "only when
   configured") and answers the model `this run has no project` for a run without one.
7. **A run's representation** carries `project_id` and `project` (the snapshot). The
   runs list and the run header show the project's name linking to
   `/projects/<id>`; the Projects page shows for each project the toolchain of its
   last run; the Status page lists the projects (name, repository, provider, branch,
   network) instead of the single project, and `GET /api/status` loses its `project`
   member.
8. **Chief of Staff.** `list_projects` is a seventh chat-capable tool (5.15 rule 2):
   `{projects: [{name, repository, provider, default_branch, sandbox_network,
   last_run}]}` with `last_run` `{run_id, status, toolchain?, finished_at?}` or null.
   `delegate_task` takes an optional `project` (a name, matched case-insensitively and
   trimmed like agents and workflows): required when the target workflow's definition
   has `project: true` (`workflow X needs a project; choose one of: A, B`, or
   `workflow X needs a project and none exists yet; the operator creates one on the
   Projects page`), refused when it has not (`workflow X does not use a project`),
   optional with `agent_name` (the quick run then works on the project); an unknown
   name answers `no project named X; choose one of: A, B`. The delegation's approval
   sentence names the project. `run_status` adds `project` (the snapshot's name) and
   `merge_request_url` (the run's merge request record since 5.20 rule 6; before it, the
   newest executed `open_merge_request` call's URL) when the
   run has them. The seeded Chief's prompt says to find the project the person's words
   name with `list_projects`, to pass its name when delegating, and to ask which
   project when the words name none and the workflow needs one.
9. **Bootstrap from `PROJECT_*`.** At api start, after the migrations, when
   `PROJECT_REPO_URL` and `PROJECT_GIT_TOKEN` parse (5.16 rule 1) and no project, live
   or deleted, has that URL, the api creates the project once (a deleted one stays
   deleted: the operator's decision on the page wins over the environment): named after the repository path's
   last segment (`.git` stripped: `yuno-playground`), or the whole path
   (`andrebatista/yuno-playground`) when a live project already has that name, with
   the environment's branch, provider and network flag and the token encrypted, and
   logs `project bootstrapped from PROJECT_*`. Nothing is updated afterwards:
   `PROJECT_*` is the bootstrap for the first start of an install that predates this
   section, the Projects page is the truth after it, and a rotated token goes into the
   page, not the environment. The api runs without a bootstrap when the key is
   missing or the setting is incomplete (a warning, as before).
10. **Report-back.** The run report of 5.15 rule 9 ends with ` Merge request: <url>`
    (`Pull request:` for GitHub) when the run has an executed `open_merge_request`
    call when the report is composed. A request opened by a late grant (5.3 rule 5)
    after the run was reported gets one follow-up, `[platform] Run <id>: merge request
    opened after the run ended: <url>`, sent once into the same conversation
    (idempotency key `report-<run id>-effect-<tool call id>`, one attempt, no claim).
11. **Merge request title.** `open_merge_request`'s `title` is at most 72 characters
    (the schema caps it) and the tool's description says it is a short summary of the
    change, like a commit subject, never the task text; the detail goes into the
    description. (The first live run used the whole task text as the title.)
12. **Two cards in a row.** A gate node followed by a gated tool call raises two
    approval cards within seconds of each other (the node's, then the tool's once the
    Deployer calls the tool); that follows from 5.3 and nothing merges them. The
    approval card of a tool call whose run has already ended (5.3 rule 5's late grant,
    D-020) says that approving still executes the call and rejecting discards it: the
    inbox's `ApprovalItem` carries `run_status` for that, the run page has the run.
13. **UI.** A Projects tab (`/projects`, between Workflows and Runs): a table of name,
    repository (host and path), provider, default branch, the toolchain of the last
    run, a network badge; New project and Edit open one form (name, repository URL,
    provider with "Detect from the URL", default branch, the token as a password field
    that is empty on edit with the hint "A token is set. Enter a new one to replace
    it.", sandbox network, description); Check on a row shows the result sentence
    under the row with a green or red tone; Delete asks for confirmation ("Runs that
    used this project keep their history."). `/projects/<id>` opens the page with that
    project's form. The workflow Run dialog shows a required project selector when the
    workflow opts in (and a sentence with a link to the Projects tab when no project
    exists, the Start button disabled); the quick-run dialog offers an optional
    selector when projects exist. The builder's switch says a run of the workflow
    names a project. No developer commentary in product copy.
14. **Seed and reset.** The seeded Chief of Staff lists the seven chat tools and
    carries the prompt of rule 8; `make seed-reset` (5.16 rule 14) brings an existing
    Chief up to date.

Edge cases: two live projects with one URL (allowed: different branches or tokens; the
bootstrap matches by URL and creates nothing then). A project renamed while a run is
queued: the run keeps the snapshot's name. A project deleted while a run is queued or
running: the run clones and pushes from the snapshot with the row's token. The key
changed: every stored token fails to decrypt; the check says so, runs fail at the clone
with rule 2's reason, the operator enters the tokens again. A delegation naming a
project for a quick run on an agent without `exec`: the checkout is still made (5.16
edge case). Two projects whose names differ only in case cannot both be live, so the
name match is exact ignoring case. The hidden quick-run workflow (5.1) carries no flag:
the flag lives in the run's snapshot. A deleted project's id on a run request is
refused like an unknown one.

Failure handling: a provider error during the check is a result; a decrypt error at
the clone fails the run before any turn, with a redacted reason; a create or update
that cannot encrypt (no key) is 503; the bootstrap logs and continues; the follow-up
report of rule 10 is sent once and never retried (the UI holds the truth, 5.15 rule 9).
A late grant that executes in the milliseconds between the end report's claim and its
text can put the URL in both messages; nothing is lost and the window is not closed.

Outputs: `projects` rows; `runs.project_id` and `runs.project_snapshot`; `tool_calls`
rows for `list_projects`; the Projects tab, the selectors, the project link on runs,
the Status list; the follow-up report.

Tests: T5.17.1 cipher and store: round trip; a blob sealed for one id does not open
for another; a wrong key fails; an empty key is refused; create, unique name ignoring
case, update keeps the token when none is sent, soft delete hides the row from the
list and the name lookup while the token still reads. T5.17.2 API: 422 details for a
missing name, an http URL, a URL with credentials, an unknown provider, a bad branch,
a missing token on create, a duplicate name; the representation never carries the
token (`token_set` only), including after a PUT with a new token; `DELETE` then `GET`
is 404; the conformance test covers every path. T5.17.3 check against the GitLab fake
(accepted and can push; branch missing; refused token) and the GitHub fake
(`permissions.push` true and false; repository not found). T5.17.4 engine, with the
local bare repository as the remote: a project workflow run queued with `project_id`
clones from its snapshot with the token read from the row, carries the checkout, and
`open_merge_request` in the second agent's workspace pushes and opens the request on
the fake provider for the same project; the project deleted after queueing still
clones; a definition with `project: true` and no project id runs without a checkout; a
quick run with `project_id` gets the checkout; a token that does not decrypt fails the
run with rule 2's reason; the trigger refuses a project workflow without `project_id`
and a plain workflow with one. T5.17.5 bootstrap: creates the project once (a second
call creates nothing), names it from the path, uses the whole path when the name is
taken, does nothing without a key or with an incomplete setting. T5.17.6
`delegate_task`: a project workflow without `project` answers the needs-a-project
message with the names; an unknown name answers `no project named`; a name puts the
id on the delegation; a plain workflow with `project` is refused; `agent_name` with
`project` delegates a quick run on it. `list_projects` lists name, repository,
provider, branch, network and last run; `run_status` carries `project` and
`merge_request_url`. T5.17.7 report: the run report carries the URL when the call
executed before the report; a late grant sends the follow-up once. T5.17.8 UI: the
Projects page lists, creates (the token field is write-only: the edit form shows the
hint and sends no token when left empty), shows a check result, confirms a delete;
the Run dialog's selector is required for a project workflow and absent otherwise;
the quick-run selector; the runs list and the header show the project link; the Status
page lists projects; the approval card's ended-run note. T5.17.9 `GET /api/tools` lists
`open_merge_request` always (supersedes T5.16.8); `TestEnvExampleAndComposeCoverConfig`
covers `PROJECT_TOKEN_KEY`.

Out of scope: per-agent projects; a project's own default limits; more than one token
per project; GitHub Apps; validating a changed repository URL against the runs that
used the project.

### 5.18 Project instructions and project notes: what a run on a project starts knowing

Purpose: a visit of a run on a project starts with the operator's standing instructions
for that repository and with the notes earlier runs on it saved, so an agent does not
rediscover how the project is built and tested; the notes are shared by every agent
that works on the project. Decision D-040. Roadmap: `docs/ROADMAP.md`.

Inputs: `projects.instructions` (text, operator-written, through the form and the API);
`project_facts` rows (saved by agents through `memory_save` with `scope: "project"`); the
run's `project_id`; the calling agent's memory policy (5.10) for the tools; the injected
clock.

Units: bytes for text caps (UTF-8, measured after trimming); days for TTLs, converted at
write time as 5.10 rule 3; counts for caps.

Rules:

1. `instructions` is optional on create and update, trimmed, at most 8192 bytes, with no
   control character other than newline and tab (422 `instructions: at most 8192 bytes`,
   `instructions: contains control characters`). Like the other settings, an update
   replaces it and an absent field clears it (the token is the only field an update
   keeps when absent, 5.17 rule 2). It is returned as is in the project representation.
2. Project notes are their own table, `project_facts(id, project_id, key, value,
   agent_id, source_run_id, created_at, updated_at, expires_at)`, unique `(project_id,
   key)`, `project_id` referencing `projects` with cascade (a soft-deleted project keeps
   its notes while its row exists), `agent_id` referencing `agents` with set null (the
   note outlives the agent that saved it). `key` is 1 to 200 bytes, `value` 1 to 2048
   bytes, `ttl_days` 0 to 3650 with default 90; at most 100 notes per project, the 101st
   evicts the oldest by `created_at` first, as 5.10 rule 3. `memory_facts` is untouched:
   it is keyed per agent and a project note is not.
3. `memory_save` and `memory_recall` gain an optional `scope`, `agent` (the default,
   today's behaviour) or `project`. `scope: "project"` needs a run token of a run that
   names a project; a chat token or a run without a project answers the model
   `project notes need a run on a project`; the calling agent's memory policy must be
   enabled, as today (`memory is not enabled for this agent`). Save upserts by
   `(project_id, key)` with `agent_id` the caller and `source_run_id` the run; recall
   applies 5.10 rule 4's word match over the project's live notes, at most 20, newest by
   `updated_at` first. Any agent of a run on the project may read and write (D-040).
4. The task protocol block of every visit of a run on a project (5.16 rule 9) gains,
   after the sandbox network line, `Project instructions from the operator:` followed by
   the instructions when they are not empty, and `Project notes saved by earlier runs,
   newest first:` followed by up to 10 live notes as `- <key>: <value>` on one line each
   (a value cut at 300 bytes ends with `…`), when any exist. They are read when the
   visit's task is rendered, from the run's `project_id`; a deleted project's notes
   still render while its row exists. Together the two blocks are capped at 12 KiB:
   the instructions first, then notes dropped oldest first until the cap holds.
5. The sweeper of 5.10 rule 6 deletes expired project notes in the same pass; expired
   notes are excluded from recall, from the block and from the API.
6. API: `GET /api/projects/{id}/facts` lists the live notes newest first, each with the
   saving agent's name (null when gone) and `source_run_id`; `DELETE
   /api/projects/{id}/facts/{factId}` answers 204, 404 `project not found` for a missing
   or deleted project and 404 `memory fact not found` for a missing note.
7. UI: the project form gets an Instructions textarea (hint: "Standing instructions for
   every run on this project: conventions, how to build and test, what to leave
   alone."); the project page (5.20 rule 9) gets a Notes section listing key, value, who
   saved it and when, with delete.
8. Shipped prompts: the Coder's and the Reviewer's say to recall project notes before
   deciding how to build or test, and to save as a project note what they learn about
   how the project is built, tested or laid out; the materialised `AGENTS.md` memory
   section names the `project` scope. `make seed-reset` applies the new prompt text to
   an existing install (5.16 rule 14).

As built (wave 8): the notes' SQL lives in `internal/memory/project.go` beside the agent
facts; `agent_name` is omitted, not null, once the saving agent is gone. The memory
policy gate runs before the project-run check; project notes default to 90 days
whatever the agent's `fact_ttl_days`, and a project save answers `scope: "project"` in
its result. `AGENTS.md` carries a one-sentence `## Memory` section only for agents whose
allowlist holds a memory tool. The shipped Coder and Reviewer carry `memory_save` and
`memory_recall` with memory enabled on a fresh seed; on an existing install
`seed --reset-shipped` restores their allowlist and the operator enables memory on
their agent pages (memory is one of the settings the reset keeps).

Edge cases: the same key saved by two agents of one run: the later write wins. An
instructions text of only whitespace is empty. A quick run on a project gets the blocks
too. The Chief of Staff cannot read project notes in a conversation (no run); the
operator reads them on the project page. A project with instructions and no notes gets
the first block only, and the other way round.

Time: TTLs and `expires_at` on the injected clock, UTC.

Failure handling: a database error while reading instructions or notes for a task fails
the visit with `project notes: <error>`, like a task template error (a visit never runs
on a silently incomplete prompt). A note write error is a tool error result (5.10).

Consistency: upsert in one statement; eviction and insert in one transaction, as
`memory.Save`.

Outputs: `projects.instructions`; `project_facts`; the two prompt blocks; the facts API;
the Notes section and the Instructions field.

Tests: T5.18.1 instructions validation on both sides of 8192 bytes and with a control
character. T5.18.2 save with scope project upserts by key and records agent and run.
T5.18.3 a chat token with scope project is refused with the text. T5.18.4 a run without
a project is refused with the text. T5.18.5 the 101st note evicts the oldest by
`created_at`. T5.18.6 recall word match and order over project notes only. T5.18.7 the
task of a project visit carries both blocks in order, the first only, the second only,
neither. T5.18.8 the 12 KiB cap drops the oldest notes first and never the
instructions. T5.18.9 an expired note is neither recalled, rendered nor listed, before
the sweep runs. T5.18.10 facts API list and delete, 404 on a deleted project. T5.18.11
UI: the form field round-trips; the Notes section lists and deletes. T5.18.12
`seed --reset-shipped` carries the new prompt text.

Out of scope: injecting the checkout's own `AGENTS.md` or `CLAUDE.md` into the prompt
(D-040: repository text is untrusted, the agent reads it itself), per-agent
instructions per project, instructions on a run request, project notes in
conversations, vector search.

### 5.19 Project budgets: daily and monthly caps with alerts

Purpose: a project cannot spend more than the operator allowed in a UTC day or a UTC
month; new work on it is refused at the cap, and the operator hears about it at 80 % and
at the cap. Decision D-041.

Inputs: `projects.budget_day_usd_micros` and `projects.budget_month_usd_micros`
(nullable; `max_cost_usd_per_day` and `max_cost_usd_per_month` in the API, USD like
`max_cost_usd`); `runs.cost_usd_micros` and `runs.created_at` of the project's runs; the
Telegram binding and its agent's conversations for the alert; `BUDGET_TICK` (default
60 s); the injected clock.

Units: micro-USD in the store and in every comparison; USD with up to six decimals in
the API, converted at the boundary as `max_cost_usd` is; windows are the UTC calendar
day `[00:00, 24:00)` and the UTC calendar month `[1st 00:00, next 1st 00:00)`, end
exclusive.

Rules:

1. Each cap is optional; when set it is `> 0` and `<= 1000000` USD (422
   `max_cost_usd_per_day: must be greater than 0`, `max_cost_usd_per_day: at most
   1000000`, the same for the month); absent or null clears it.
2. The spend of a window is the sum of `cost_usd_micros` of the runs whose `project_id`
   is the project and whose `created_at` falls in the window, whatever their status: a
   running run counts what it has spent so far, a run created at 23:59 counts in that
   day however long it runs, and the runs of a deleted project keep counting. One
   indexed query per window (`runs (project_id, created_at)`).
3. Queue-time refusal: a run request, a delegation, a schedule firing, a retry (5.21)
   or an intake (5.23) that names a project with a cap whose spend is already `>=` the
   cap is refused inside the trigger transaction, next to the rate check of 5.11 rule
   2, with `*project.BudgetError{ProjectID, ProjectName, Window, CapMicros,
   SpendMicros, WindowEnd}`; the day cap is checked before the month cap. HTTP answers
   429 `project_budget_reached` with the text `project <name> has reached its
   <daily|monthly> budget ($<cap>); new runs are refused until <window end, RFC 3339
   UTC>` and details `{project_id, window, max_cost_usd, spent_usd, until}`; a schedule
   records the text in `last_error` and advances, as it does for a rate limit;
   `delegate_task` answers the text as a tool error.
4. Running runs are never aborted by a project cap (D-041): they finish under their own
   `max_cost_usd`, so a window can overshoot by at most the sum of the running runs'
   remaining caps.
5. Alerts: a watcher ticks every `BUDGET_TICK`, computes both windows' spend for every
   live project with a cap, and sends at most one `warning` alert (spend `>= 80 %` of
   the cap) and one `reached` alert (spend `>= cap`) per project, window kind and window
   start. The texts: `[platform] Project <name> has used $<spend> of its $<cap>
   <daily|monthly> budget (<n> %).` and `[platform] Project <name> has reached its
   $<cap> <daily|monthly> budget; new runs on it are refused until <window end UTC>.`
   Sent alerts are rows of `project_budget_alerts(project_id, window, window_start,
   level, sent_at)`, primary key the first four, written after a send that was not
   deferred; a restart re-sends nothing, and a deferred send (gateway unreachable) is
   tried again at the next tick.
6. The alert goes to the most recent conversation (newest message) of the agent bound
   to the Telegram channel, through the send path of 5.15 rule 9 (`chat.send` with
   `deliver`, one attempt, idempotency key `budget-<project id>-<window>-<window
   start>-<level>`), and is mirrored like a report-back without a run id. Without a
   binding or a conversation the alert is a log line and the row only; the UI shows
   the spend either way (D-041).
7. Representation: `Project` gains `spend: {day_usd, month_usd}` next to the caps,
   computed on read for gets and lists (one query per window for the whole list). The
   Projects list shows `spent / cap` for the day and the month with the warning tone
   from 80 % and the danger tone at the cap; the project form has both cap fields
   (hint: "Leave empty for no cap. New runs are refused once the cap is reached;
   running runs finish."); the project page shows the same with the window end.
8. `list_projects` (5.17 rule 8) adds `budget: {day: {max_usd, spent_usd}, month: {...}}`
   for a project with a cap, so the Chief can say why a delegation was refused.

As built (wave 8): the ledger's column is `window_kind` (`window` is reserved in
PostgreSQL); the alert key is `budget-<project id>-<day|month>-<window start, RFC
3339 UTC>-<level>`; money prints with two decimals, four under a cent, and window ends
as RFC 3339 UTC in both texts; when spend crosses 80 % and the cap between two ticks
both alerts go out, the warning first, and a deferred warning holds the reached alert
back to the next tick. The destination is `store.LatestConversationForAgent`, the
conversation lookup of 5.15 rule 3. The caps are parsed as floats and bounded before
the conversion to micro-USD, so a huge value gets the "at most" text rather than
overflowing.

Edge cases: spend exactly at the cap refuses. A cap lowered below the current spend
refuses at once and alerts at the next tick. A project with both caps alerts per window.
A run created at 23:58 that spends until 00:10 counts in the day it was created. A
deleted project is not alerted on (not live) and cannot be named by a trigger. Two
triggers in the same instant can both pass just under the cap (bounded by rule 4).

Time: UTC calendar windows on the injected clock; window end exclusive; the watcher's
tick on the same clock.

Failure handling: a failed spend query at trigger time fails the trigger (500), never
silently allows; a failed or deferred send is logged and tried at the next tick (rule
5); a watcher error on one project is logged and the tick continues with the next.

Consistency: the refusal reads the spend inside the trigger transaction, after the rate
check; the alert row is written after the send so a crash between the two re-sends at
most once under the same idempotency key, which the gateway dedupes (5.14 rule 6).

Outputs: the two cap columns; `project_budget_alerts`; the 429 body; `last_error`; the
alert messages, mirrored; `spend` in the representation; `list_projects.budget`.

Tests: T5.19.1 cap validation at 0, 1000000, above, negative, and clearing. T5.19.2 the
spend sums only the project's runs, in the window, on both edges of a day and of a
month. T5.19.3 refusal at `>=` for a run request (429 body and details), a delegation
(tool error text), a schedule (`last_error` and advance). T5.19.4 a running run
continues past the cap; the next trigger is refused. T5.19.5 one warning and one reached
alert per window start; a tick after a restart re-sends nothing; the next day sends
again. T5.19.6 routing to the bound agent's newest conversation; no binding gives a row
and a log line only. T5.19.7 a deferred send is tried again at the next tick and not
recorded before. T5.19.8 `spend` in gets and lists and `list_projects.budget`. T5.19.9
UI: form fields round-trip; the list shows spent over cap with the tones.

Out of scope: a platform-wide budget, aborting running runs, budgets per agent within a
project, currencies other than USD, windows in a project's own time zone.

### 5.20 Merge requests as records: status sync, merge, one request per run

Purpose: the platform keeps a record of every merge request it opened, knows whether it
is open, merged or closed, whether its pipeline passed and whether review threads
remain, shows that on the run and the project, reports the final state into the
conversation that asked for the work, and merges it on the operator's request. Decision
D-042.

Inputs: `open_merge_request` results (5.16 rule 10); the provider APIs of 5.16 rule 11;
the project's token through the resolver (5.17 rule 6); `MERGE_REQUEST_SYNC_INTERVAL`
(default 120 s); the injected clock.

Units: `number` is the GitLab `iid` or the GitHub pull number; `state` is `open`,
`merged` or `closed`; `ci_status` is `none`, `pending`, `passed` or `failed`;
`unresolved_threads` a count or null when unknown; times UTC.

Rules:

1. Table `merge_requests`: id, project_id (references `projects`), run_id (references
   `runs`, cascade), provider, number, url, source_branch, target_branch, title, state,
   ci_status, head_sha, unresolved_threads, last_sync_error, synced_at, reported_state
   (null, `merged` or `closed`), merged_by, created_at, updated_at, closed_at; unique
   `(project_id, number)` and unique `run_id`.
2. One request per run. `open_merge_request` writes the record in the transaction that
   stores the side effect result (5.4 rule 5), state `open`, `ci_status` `none`,
   `synced_at` null. A later successful call in the same run (a loop revisit, a retried
   attempt of 5.21) opens no second request: it copies the working tree as a new commit
   on top of the record's source branch (5.16 rule 10 step (b) now skips the commit and
   push only when the tree is unchanged against that branch, which it checks on its
   private clone), pushes, and returns the record as `{number, url, branch, provider,
   replayed: true, updated: true}` (`updated` absent when nothing was pushed); the
   record's title and description are unchanged. The idempotency key of 5.4 still
   dedupes the identical call within one visit.
3. Sync: every `MERGE_REQUEST_SYNC_INTERVAL` the loop lists records with state `open`
   created in the last 14 days, oldest `synced_at` first with nulls first, at most 50 per
   pass, and refreshes each with a 10 s timeout. GitLab: `GET merge_requests/<iid>`
   (`opened` → open, `merged` → merged, `closed` and `locked` → closed; `sha`;
   `merged_at` or `closed_at`), `GET merge_requests/<iid>/discussions` (first 100; the
   count of threads whose first note is resolvable and not resolved), `GET
   merge_requests/<iid>/pipelines` (the first entry's status: `success` → passed;
   `failed`, `canceled` → failed; `created`, `waiting_for_resource`, `preparing`,
   `pending`, `running`, `manual`, `scheduled` → pending; `skipped` or no entry →
   none). GitHub: `GET pulls/<n>` (`merged` → merged, `state: closed` → closed, else
   open; `head.sha`), GraphQL `reviewThreads(first: 100) { isResolved }` on the pull
   request (the unresolved count; a token without GraphQL access leaves null and
   records `last_sync_error`), `GET commits/<sha>/check-runs` and `GET
   commits/<sha>/status` (any `failure`, `timed_out`, `cancelled`, `action_required` or
   combined `failure` → failed; any `queued`, `in_progress` or combined `pending` →
   pending; no check and no status → none; otherwise passed). A 404 from the provider
   sets state `closed` with `last_sync_error = "not found on the provider"`; any other
   error sets `last_sync_error` and leaves the record as it was. A record that turns
   merged or closed gets `closed_at` from the provider, or now. `POST
   /api/merge-requests/{id}/sync` runs the same refresh for one record at once and
   answers the record; the run page calls it when it shows a record whose `synced_at`
   is null or older than 30 s.
4. One report per final state (D-042): when a sync or a merge moves a record to merged
   or closed and its run reports to a conversation (5.15 rule 9's test), the loop sends
   `[platform] Run <id>: <merge request|pull request> !<n> was merged.` or `... was
   closed without being merged.` (`#<n>` on GitHub) with idempotency key
   `report-<run id>-mr-<state>`, one attempt, sets `reported_state` after a send that
   was not deferred, and mirrors it like a report-back with the run id.
5. Merge: `POST /api/merge-requests/{id}/merge` (no body) needs state `open` (409
   `merge_request_not_open`: `the request is <state>`) and `ci_status` `passed` or
   `none` (409 `merge_request_ci_not_passed`: `the pipeline is <ci_status>; merge on
   the provider to override`); it calls GitLab `PUT merge_requests/<iid>/merge` with
   `should_remove_source_branch: true` or GitHub `PUT pulls/<n>/merge` with
   `merge_method: merge`. After a 2xx the request's status is read back once: only when
   the provider reports `merged` does the record become merged with `closed_at` now and
   `merged_by` `operator`, then rule 4 reports; otherwise the record stays open with
   `last_sync_error` `the provider accepted the merge but reports the request as
   <state>` and the answer is 200 with the record (the next sync settles it). A 401 or
   403 from the merge call means the project's token may not merge into the target
   branch (GitLab answers 401 for a Developer-role token on a protected branch): it is
   409 `merge_request_not_mergeable` with `the project's token is not allowed to merge
   this request (HTTP <status>); give it a role that may merge into the target
   branch`, so the operator knows what to change. 405, 406, 409 and 422 from the provider
   answer 409 `merge_request_not_mergeable` with the first line of the provider's
   message; other failures follow 5.5 rule 4 (5xx and timeouts answer 502
   `provider_unavailable` after one re-sync, since the merge may have landed). There
   is no tool call and no `side_effects` row: the record's state is the idempotency
   (a second POST finds it merged).
6. Representation: `MergeRequest` is the row; `GET /api/merge-requests?project_id=&run_id=&state=&limit=`
   (newest first, 50 by default, 200 at most) and `GET /api/merge-requests/{id}`.
   `Run` gains `merge_request` (the record, filled for gets and lists like
   `agent_names`). The run header's link and `run_status`'s `merge_request_url` read the
   record instead of the trace (5.16 rule 12 and 5.17 rule 8 amended; rule 7 makes the
   trace fallback unnecessary); `run_status` adds `merge_request: {number, url, state,
   ci_status, unresolved_threads}` and keeps `merge_request_url`.
7. Backfill at api start, once per start and idempotent: every executed
   `open_merge_request` call with an output `url` whose run has a `project_id` becomes
   a record (`ON CONFLICT DO NOTHING`), state `open`, `synced_at` null; the first pass
   settles them. Runs without a project (`gitlab_create_mr`) are not records.
8. A deleted project's records sync as long as its token row exists (5.17: the token is
   read from the row, deleted or not); a token that cannot be read (`ErrCannotRead`)
   records `last_sync_error` and skips the record.
9. UI. `/projects/{id}` becomes a page: the settings with Edit (the dialog), Check and
   Delete; the budget and spend of 5.19; the instructions and the Notes of 5.18; a
   Merge requests table (number, title, run, state, CI, threads, updated); the
   project's 10 newest runs. The Projects list links each name to it. The run page gets
   a Merge request card under the header: state badge, CI badge, unresolved threads,
   the link, Sync, and Merge with a confirmation ("Merge !<n> into <target branch>?");
   once final it shows Merged or Closed with the time.

As built (wave 8): a later push replaces the branch's tree with the run's checkout
(5.16 rule 10), so a commit someone else made on that branch in between is reverted by
it; 5.22 clones the branch instead. A 404 from the provider closes the record even when
it is the token that lost access, and records older than 14 days are refreshed only by
Sync on the page. The record is written through the side effect runner's transaction
(`RunSideEffectTx` hands the tool the transaction as `Call.Tx`; a failing tool rolls the
key back instead of deleting it, which is the same outcome). Two calls in one run take
a per-run advisory lock before the lookup, so the second sees the first's record. A
later call on a request that was merged or closed is refused before anything is
pushed: `the run's <merge request !n|pull request #n> is <state>; nothing more can be
pushed to it`. A failed refresh writes only `last_sync_error` and `synced_at`, so one
failing record cannot starve the pass; an unknown pipeline status maps to pending. The
backfill dates a record from its tool call's `finished_at`. The merge answers 502
`provider_error` for a provider refusal that is not a mergeability one (a 403), and
409 `merge_request_not_mergeable` when the project's token cannot be read.

Edge cases: a run that opened a request and then failed keeps its record. A request
merged on the provider before the first sync is recorded merged by that sync and
reported. A source branch deleted on the provider while the request is open changes
nothing. GitLab `locked` is closed. A loop revisit with an unchanged tree answers
`replayed: true` without `updated`. A merge that races a sync: the conditional update
on state wins once; the other side re-reads.

Time: UTC; `synced_at` and `closed_at` from the injected clock when the provider gives
none; the 14-day window on `created_at`.

Failure handling: per record; a provider error never stops the pass; a database error
ends the pass with a log line and the next tick retries. A merge whose provider call
fails after it landed is caught by the re-sync of rule 5.

Consistency: the record write and the side effect result are one transaction (rule 2);
a sync is one update per record conditional on `state = 'open'`; `reported_state` is
written after a non-deferred send, as `reported_at` in 5.15 rule 9.

Outputs: the table; the API; `Run.merge_request`; `run_status.merge_request`; the two
report lines; the project page and the run card.

Tests: T5.20.1 `open_merge_request` writes the record in the same transaction as the
side effect; a failed record write leaves no side effect row. T5.20.2 a second call in
the same run pushes a commit on top and answers replayed and updated; an unchanged tree
answers replayed only; never a second request. T5.20.3 the mapping table per provider
for state, pipeline status and check conclusions, including none, skipped and locked.
T5.20.4 404 closes the record with the text. T5.20.5 the final-state report is sent
once per state, keyed, and not re-sent after a restart. T5.20.6 merge preconditions
(not open, CI failed, CI pending) and provider refusals answer 409 with the provider's
first line. T5.20.7 a merge success records merged, `closed_at`, `merged_by` and
reports. T5.20.8 the backfill is idempotent and skips runs without a project. T5.20.9 a
deleted project's record still syncs; an unreadable token is recorded. T5.20.10
`run_status.merge_request` and `Run.merge_request`. T5.20.11 UI: the run card per
state, the Merge confirmation, the project page table, the list link.

Out of scope: merging from chat (D-042: a chat tool with `require_approval` is refused
by 5.15 rule 7), rebasing, conflict resolution, review threads as text (5.22),
webhooks, more than one request per run, GitHub merge methods other than `merge`,
approving the request on the provider.

### 5.21 Retry from the failed step

Purpose: a run that ended `failed`, `timed_out` or `cancelled` is re-queued from the
node it stopped at, keeping its state, its checkout and the work its agents had done,
instead of being started over from the first node. Decision D-043.

Inputs: the run row (status, `current_node`, the counters, `limits`, `started_at`); the
operator's optional `limits` override; the engine's resumed path (`runRun` with
`current_node` set, as after a crash re-queue or an approval); the injected clock.

Units: the run's own limits (5.2: steps, tool calls, micro-USD, seconds); `retries` a
count per run.

Rules:

1. `POST /api/runs/{id}/retry` with an optional body `{limits}` (the `Limits` shape,
   validated and clamped as at trigger time, 5.2) on a run whose status is `failed`,
   `timed_out` or `cancelled` (D-043) and whose `current_node` is set; otherwise 409
   `run_not_retryable` with `run is <status>` or `run has no step to retry from`. At
   most 5 retries per run (`runs.retries`); the sixth answers 409 `retry_limit`, `run
   was retried 5 times`.
2. Limits. The counters continue: steps, tool calls and cost stay the run's true totals
   and the caps apply to the whole run. Before re-queueing, each counted limit
   (`max_steps`, `max_tool_calls`, `max_cost_usd`) is compared with its counter under
   the limits that will apply, and a counter `>=` its cap refuses with 409
   `limits_exhausted` and one detail per exhausted limit (`max_steps: 40 of 40 used`);
   the body's `limits` replaces the run's limits (the snapshot's run overrides) before
   the check, so raising a cap is how a run that died on it is retried; the visits of
   `current_node` are compared with `max_loop_visits` the same way (`max_loop_visits:
   3 of 3 used`), since the resumed visit would be refused at the node's entry
   otherwise. The time limit
   restarts: `started_at` is set to now and `paused_total_ms` to 0 when the run is next
   claimed; the trace keeps every attempt's root span with its own start. The project
   budget (5.19 rule 3) is checked as for a trigger.
3. The retry is one conditional update in a transaction that holds the run row:
   `status = queued`, `retries + 1`, `attempt = 1` (the crash re-queue of 5.1 counts
   per retry), `failure_reason`, `finished_at`, `cancel_requested`, `cancel_by`,
   `resume_node_visit_id` and `reported_at` cleared, `limits` replaced when given,
   `WHERE status IN ('failed', 'timed_out', 'cancelled')`. 5.1 rule 2 gains the
   transition `failed | timed_out | cancelled → queued` for this path only. A
   `run_events` entry `retry {retries, by, from_node, limits}` and SSE `run.status`
   follow. The second of two concurrent retries answers 409 `run_not_retryable`.
4. Resumption. The claimed run takes `runRun`'s resumed path: its unfinished visits were
   already failed when it ended; a new root span opens with the attribute `retry: <n>`;
   the walk continues at `current_node` with a new visit of that node (visit n+1; the
   failed visit stays in the trace); the copy-forward of 5.6 rule 13 brings the run
   directory of every earlier visit, so the failed visit's partial edits of the
   checkout are what the new visit starts from (the point of retrying from the step;
   the task text is the same). A run that failed before its clone completed
   (`toolchain` unset) clones again (5.16 rule 3). An approval node reached again asks
   again (5.6 rule 8). A node that failed and followed its `on_failure` edge moved
   `current_node` to that target, so the retry resumes there, not at the node.
5. Side effects. A retried visit is a new visit, so an identical call gets a new
   idempotency key (5.4's edge case); `open_merge_request` is covered by 5.20 rule 2
   (the run's record is updated, never a second request); `deploy_release` and
   `gitlab_create_mr` repeat, as they do on a loop revisit today, and the run page says
   so on the retry dialog when the run's trace holds an executed call of either.
6. Report-back: a channel-triggered run reports its new outcome once more when the
   retry ends (`reported_at` cleared; idempotency key `report-<run id>-terminal-r<n>`),
   the text of 5.15 rule 9 with `(retry <n>)` after the status.
7. Representation and UI: `Run` gains `retries`; `run_status` reports `retries`; the
   run header shows Retry on a `failed`, `timed_out` or `cancelled` run with a step to
   retry from. One click re-queues; a 409 `limits_exhausted` opens a dialog listing the
   exhausted limits with fields prefilled from the run's limits, so the operator raises
   them and retries from there; the Timeline marks the boundary (`Retry <n> from
   <node>`, from the run event).
8. The Chief of Staff gets no retry tool: a retry is the operator's decision on the run
   page; `run_status` tells it the run was retried and from where.

As built (wave 8): the engine's transition table is untouched and `ValidTransition`
still refuses `failed → queued`; the retry's own status list is the only path that
re-queues an ended run. The retry clears `started_at`, and the claim leaves it empty
for a run with retries so `runRun` stamps it from the injected clock (later claims of
the same retry keep it). The body's limits are resolved over the run's current limits
(a later retry does not undo an earlier raise) and the participating agents' per-run
cost caps still apply (5.11 rule 1). The event is SSE `run.retry` with `{retries,
from_node, by, limits, at}`; the loser of two concurrent retries reads `run is
queued`.

Edge cases: a run cancelled while `waiting_approval` has the approval node as
`current_node` and retries by asking again. A run that failed before any node
(`workflow snapshot: ...`) has no `current_node` and is not retryable. A run whose
workflow or agent was deleted since runs on its snapshot (5.6); a deleted project still
serves the snapshot and the token row (5.17). A `succeeded` run is never retryable. A
retry after the stale sweeper failed the run with `worker lost` is allowed. A retried
visit whose earlier turn is still open in OpenClaw: aborted by the resumed path, as
today.

Time: `started_at` reset on claim; the time limit counts from there; the injected
clock.

Failure handling: precondition and validation failures answer 409 or 422 without a
change; a failure after the update (the event insert) is logged and the run stays
queued.

Consistency: the limits check and the update are one transaction with the run row
locked (`SELECT ... FOR UPDATE`); the update is conditional on the status (rule 3).

Outputs: `runs.retries`; the run event; SSE `run.status`; the header button and dialog;
`run_status.retries`.

Tests: T5.21.1 each non-retryable status and a null `current_node` answer 409 with the
texts. T5.21.2 the retry limit at 5 and at 6. T5.21.3 an exhausted limit is refused
with details; a raised limit passes; the raise is clamped. T5.21.4 resume from the
failed node with the scripted provider: a new visit, the old one kept, state kept,
counters continued, a file written by the failed visit present. T5.21.5 the time limit
restarts. T5.21.6 cancelled while waiting at an approval node asks again. T5.21.7 two
concurrent retries: one 202, one 409. T5.21.8 the report-back is sent once more under
the retry key. T5.21.9 a project at its budget refuses the retry. T5.21.10 UI: the
button per status; the exhausted-limits dialog; the Timeline mark. T5.21.11
`open_merge_request` after a retry updates the run's record (with T5.20.2).

Out of scope: retrying from an earlier node than the one the run stopped at, editing
state before the retry, a fresh checkout on retry, automatic retries of failed runs.

## 6. HTTP API summary

Errors: `{error: {code, message, details?}}`; validation failures are 422 with
`details: [{path, message}]` (path-qualified, e.g. `tool_allowlist[2]`), including
malformed or unknown-field bodies, since 400 is not part of the contract. Not found is
404; a decision on a non-pending approval or a cancel of a finished run is 409; a
materialisation failure after a successful write is 502 with the row id in details.

As revision 1 plus: `GET/POST /skills`, `GET/PUT/DELETE /skills/{id}`;
`GET /agents/{id}/runtime` (materialised OpenClaw state: config entry, exec policy, files);
`POST /agents/{id}/materialize`; `GET /channels`, `PUT/DELETE /channels/telegram/binding`,
`GET /conversations`, `GET /conversations/{id}/messages`, `GET /conversations/{id}/tool-calls`; `GET/POST /schedules`,
`GET/PUT/DELETE /schedules/{id}`; `GET/POST /skills`, `GET/PUT/DELETE /skills/{id}`;
`GET /agents/{id}/memory`, `DELETE /agents/{id}/memory/{factId}`; `POST /mcp` (MCP, bearer); `/model-proxy/*` (bearer or
loopback only); `GET /openclaw/status` (gateway connection, scopes, version);
`GET/POST /projects`, `GET/PUT/DELETE /projects/{id}`, `POST /projects/{id}/check` (5.17;
the token is write-only, `token_set` says one is stored). Wave 8 (5.18 to 5.21):
`GET /projects/{id}/facts`, `DELETE /projects/{id}/facts/{factId}`; `GET /merge-requests`,
`GET /merge-requests/{id}`, `POST /merge-requests/{id}/sync`, `POST /merge-requests/{id}/merge`;
`POST /runs/{id}/retry`; 429 `project_budget_reached` on a trigger at a project cap.

## 7. Frontend

As revision 1 plus a Skills page, an Exec policy editor on the agent form (security, ask,
allowlist patterns), and an OpenClaw panel on Status.

Wave 2 as built: the Workflows page (list, "From template", delete) and the builder (a
React Flow canvas plus an outline list for keyboard use; node panel with agent picker,
task, output schema, `input_from`, `on_failure`/`on_reject`; edge panel with the
condition builder and edge ordering; limits, input and state schema editors; Validate
maps 422 details onto the node or edge they name; Save; Run builds the input form from
`input_schema`; node positions persist under a `ui` key the backend ignores); the
Approvals inbox (pending by default, kind labels Command/Tool call/Workflow step,
approve, reject, approve-always for exec, a pending-count badge in the nav); and on the
run page an Approvals section, Agent messages and the live State, with visit numbers on
repeated nodes.

Wave 3 as built: Channels (the Telegram card with bind/unbind through the API, the
conversation list and transcripts, live), Schedules (list with next and last run, cron
or delay form with the browser zone as default, input from the workflow's schema),
Skills (list, editor, read-only name on edit), the agent page's saved facts, guardrail
pattern errors and Telegram line, and Status panels for OpenClaw and the scheduler.
Binding lives on Channels, not on the agent form.

Wave 7 as built (5.17): the Projects tab (list, one form for create and edit with a
write-only token field, Check, Delete), the project selector in the workflow Run dialog
and the quick-run dialog, the project link on the runs list and the run header, the
projects list on Status, and the ended-run note on tool approval cards. Then (5.9 rule
6) the same project selector in the schedule form when the chosen workflow opts in,
and a Project column on Schedules linking to the project's page.

Wave 8 as built (5.18 to 5.21): the project page at `/projects/{id}` (the edit form moved to
`/projects/{id}/edit`),  (settings, budget and spend, instructions, notes, merge requests, newest runs), the Instructions and budget fields on the project form, spent over cap on the Projects list, the Merge request card on the run page (state, CI, threads, Sync, Merge with confirmation), and Retry on the run header with the exhausted-limits dialog and the Timeline mark.

## 8. Docker and Makefile

Services: `postgres`, `openclaw-init` (writes the baseline config, exits), `openclaw`
(gateway, Docker socket mounted, `./data/openclaw` bind-mounted at
`/home/node/.openclaw`), `api` (shares the gateway network namespace, mounts the same
bind at the same path, so `web` proxies to `openclaw:8080`), `web` (also proxies
`/telegram-webhook` to `openclaw:18789`), `deploytarget`, `sandbox-image` (build-only
service that tags `yuno-sandbox:latest`) and `sandbox-image-{python,node,go}` (build-only,
the per-toolchain images of 5.16 rule 6); profiles `eval`, `tunnel`, `tools`, `build`.
The `go` tool service is `golang:1.26-alpine` plus git (`docker/go.Dockerfile`); the
api image carries git too (5.16 rules 3 and 10). The gateway
image is pinned to `2026.10.3`. Volumes: `pgdata`, `releases`, caches; everything OpenClaw
touches is a host bind mount under `./data/` (git-ignored). `.env.example` adds
`OPENCLAW_GATEWAY_TOKEN`, `YUNO_MCP_TOKEN`, `MODEL_PROXY_RATE_PER_MINUTE`,
`MCP_TOOL_WAIT_SECONDS`, `DOCKER_SOCKET`. Make targets unchanged plus `make openclaw-shell`
(CLI inside the gateway container), `make openclaw-logs`, `make probe-openclaw`
(`scripts/openclaw-probe.mjs`: connects over loopback, patches a mock provider, runs a
turn with a sandboxed `exec`, and prints the event summary; the integration contract's
canary), `make probe-sandbox` (`scripts/openclaw-probe-sandbox.mjs`: the per-agent
sandbox override of 5.16 rule 7, against the running gateway) and `make seed-reset`
(5.16 rule 14).

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

Waves 5 to 7 as built: the Chief of Staff and delegation (5.15), the project repository (5.16), projects as records (5.17). Waves 8 to 11 are planned in `docs/ROADMAP.md`: 8 is 5.18 to 5.21 (project instructions and notes, budgets, merge request records, retry), 9 is iteration on a merge request and issue intake, 10 is questions to the operator and run artifacts, 11 is parallel steps.

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
