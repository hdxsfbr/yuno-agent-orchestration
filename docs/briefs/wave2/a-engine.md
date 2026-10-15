# Agent A — multi-node engine, approvals, retries (Opus)

Read `docs/briefs/wave2/common.md` first. Design: DESIGN 5.1, 5.2, 5.3 (all rules, 5a is
verified fact), 5.5, 5.6, 5.14. Tests to make true: T5.1.3, T5.1.4, T5.2.8, T5.2.9,
T5.3.3–T5.3.9 (engine side), T5.5.2–T5.5.3, T5.6.2–T5.6.9, T5.6.11.

## You own

`internal/engine/**`, `internal/workflow/**`, `internal/openclaw/**` (plugin approval RPC
wrappers and event types; keep the materialiser and baseline as they are),
`internal/store/engine_queries.go`, `internal/store/migrations/0003_*.sql` if you need
one. Append-only: `internal/store/types.go`, `internal/config/config.go`.

## What exists

Wave 1 executes one agent node per run (`run.go` `singleAgentNode`, `node.go` `visit`:
one OpenClaw turn on a node-scoped session, run token, finish protocol, limits, exec
approvals from `session.approval` events, cancellation, stale-run re-queue). Read
`run.go`, `node.go`, `limits.go`, `tokens.go`, `engine.go` end to end before changing
anything; the fake gateway in `main_test.go` drives scripted turns.

## Build

1. **Graph execution (5.6 rules 1–12, 5.1 rule 5).** Replace the single-node flow with
   a walk from `start`: outgoing edges in definition order, first true `when` wins
   (`workflow` package: `Evaluate(cond, state) bool` with JSON-pointer `path`, ops
   `eq neq in exists gt lt`, `all`/`any`; a missing path is `exists:false` and makes the
   others false); `no matching edge from <node>` fails the run. Each agent node visit is
   a `run_nodes` row with `visit` = how many times this node has run in this run;
   `max_loop_visits` (reason `loop limit <n> exceeded for <node>`). Task rendering:
   Go `text/template` over `{input, state}` (`{{.input.task}}`,
   `{{with .state.reviewer}}…{{end}}`; missing keys render empty, not `<no value>`:
   use `Option("missingkey=zero")` or equivalent and test it), then `$RUN_ID`
   substitution, then the protocol block as today. `input_from`: after the rendered
   task, append a "Context from earlier nodes" block with the JSON of `state[<n>]` for
   each listed node that has output (`start` means the run input). State merge:
   `state[<node>] = output` (replace). `agent_messages` row `from`→`to` with the output
   once the next edge is chosen; SSE `agent_message {agent_message}`; `current_node`
   kept current. `on_failure` edges (rule 12). The `templates/dev_pipeline.json`
   definition must run end to end on the scripted provider (T5.6.8 with
   `max_loop_visits` reached once).
2. **Trigger of workflow runs (`Trigger` with `WorkflowID`).** Validate structure
   (`ValidateStructure`), that every agent node has `agent_id` (a node with only
   `agent_name` → 422 `nodes[i].agent_id: template must be instantiated`) naming an
   existing agent, and `input` against `input_schema` (422, path-qualified). Limits
   layering as today. The run-level model override (`over.Model`) applies to every
   node's session.
3. **Approval nodes (5.6 rule 8, 5.3 rule 9).** Kind `node` approval with the rendered
   message, run and node `waiting_approval`, `paused_at`; release the worker (the run
   must survive an api restart while waiting: persist what resumption needs, the
   existing `resume_node_visit_id` and `current_node` are there for this). Decision →
   `waiting_approval → queued`, a worker claims it and continues along the first true
   edge; reject → `on_reject` edge else `rejected at <node>`.
4. **Tool approvals: implement the contract** (`RequestApproval`, `WaitApproval`,
   `DecideApproval` in `engine.go`; delete the stubs). `RequestApproval`: token → visit;
   insert the approval (kind `tool`, `tool_call_id`, message, `expires_at` =
   now + `approval_timeout_seconds`), register with the gateway two-phase per 5.3 rule
   5a (new wrappers in `internal/openclaw/rpc.go`: `PluginApprovalRequest`,
   `PluginApprovalResolve`, event type `PluginApprovalResolvedEvent`), store the gateway
   id in `external_id`, run and node → `waiting_approval` with `paused_at`, SSE
   `approval.requested {approval}`, return. `WaitApproval`: a waiter registry keyed by
   approval id; returns the decided approval or `ctx.Err()`. `DecideApproval`: 409
   semantics via `ErrNotPending`; `store.ErrNotFound` for unknown ids; by kind:
   `exec` → `exec.approval.resolve` (`approve` = allow-once, `approve_always` =
   allow-always and append the command pattern to the agent's stored exec allowlist,
   `reject` = deny); `tool` → decide the row, `plugin.approval.resolve` (ignore
   `APPROVAL_ALREADY_RESOLVED` and `APPROVAL_NOT_FOUND`), resume the run
   (`waiting_approval → running`, `paused_total_ms`), SSE `approval.decided`, then
   deliver to the waiter if one is registered, else if approved call
   `Deps.Effects.ExecuteApproved(toolCallID)` and append `{approval_id, tool_call_id,
   status, output}` to `runs.state.pending_effects[]` (also on rejection, with status
   rejected); `node` → as item 3. Gateway events `plugin.approval.resolved` with
   `resolvedBy` non-null are decisions by `openclaw` (same path); with `resolvedBy`
   null they are ignored. Cancel while waiting: approvals `expired` with reason
   `run cancelled`, exec ones resolved `deny`, plugin ones resolved deny. Expiry
   sweeper every 30 s: pending approvals past `expires_at` → `expired`, run `failed`
   with reason `approval expired` (5.2 rule 10); for a `tool` approval the waiter gets
   the expired row and the MCP handler answers the model with status `rejected`,
   reason `approval expired`.
5. **Retries (5.5 rules 2–3).** Check what wave 1 already does for `chat.send`
   transport failures and for `chat` `state: error`; implement what is missing: same
   `idempotencyKey` on transport retry (5 attempts, backoff 1 2 4 8 … s capped 30 s,
   deadline-aware), provider status in {408, 409, 429, 500, 502, 503, 529} → new turn
   `Continue.` up to 3 times, other errors `model error: <errorKind>`; every attempt a
   span and a `warn` log.
6. **Platform tool items.** Skip `yuno__*` items in the item-event recorder (see
   common.md); keep spans if cheap.
7. **Fake gateway in tests.** Extend it with `plugin.approval.request/resolve` and the
   `plugin.approval.resolved` event, and `exec.approval.resolve`, so every approval path
   is driven end to end on the scripted provider with a fake `SideEffectExecutor`
   (counting) in `Deps.Effects`.

## Tests (name them after the design)

T5.6.2 condition table (every op, missing path, `all`/`any`); T5.6.4 edge order; T5.6.5
template error at save (via Trigger) and at run; T5.6.7 approval node approve, reject,
`on_reject`; T5.6.8 dev pipeline with a feedback loop then deploy (scripted); T5.6.9
state replace on revisit; T5.2.8 loop limit; T5.2.9 approval expiry; T5.3.3 approved
within the wait → delivered to the waiter, executor not called; T5.3.5 wait abandoned →
grant → executor called once, `pending_effects` written; T5.3.7 cancel while waiting;
T5.3.8 double decision → `ErrNotPending`; T5.3.9 gateway timeout event ignored; T5.5.2
and T5.5.3 retry paths; T5.6.11 input schema at trigger; T5.1.3/T5.1.4.

## Report

Per common.md. Name the exact SSE event payloads you emit (`agent_message`,
`approval.requested`, `approval.decided`, `node.status`, `run.status`) so agent C can
match them, and the `runs.state.pending_effects[]` entry shape.
