# Agent C — workflow builder, approvals inbox, run page additions (Sonnet)

Read `docs/briefs/wave2/common.md` first. Design: DESIGN 5.3 rule 8 (what an approval
shows), 5.6 (the definition shape, rules 1–4, 8, 9), section 7, and the UI rules in
`/home/hdx/.claude/CLAUDE.md` (no implementation commentary in product copy).

## You own

`web/**` only. `@xyflow/react` 12 is already a dependency; add nothing else unless it is
unavoidable (say so in the report).

## Backend contract you code against (being built in parallel; mock it in tests)

Shapes are the Go structs in `internal/store/types.go` serialised with their `json` tags
(`Workflow`, `Approval`, `ToolCall`, `AgentMessage`, `Run`, `RunNode`) and
`internal/workflow/def.go` (`Definition`, `Node`, `Edge`, `Condition`).

- `GET /api/workflows` → `Workflow[]`; `POST /api/workflows {name, description,
  definition}` → `Workflow`; `GET/PUT/DELETE /api/workflows/{id}`;
  `POST /api/workflows/validate {definition}` → 200 `{valid: true}` or 422
  `{error: {code, message, details: [{path, message}]}}` (paths like `edges[2].to`,
  `nodes[1].task`).
- `GET /api/workflow-templates` → `[{key, name, description, definition}]`;
  `POST /api/workflow-templates/{key}/instantiate {name?}` → `Workflow` (422 with
  `nodes[i].agent_name` details when a named agent does not exist).
- `POST /api/runs {workflow_id, input}` → `Run` (exists); 422 details when `input`
  fails the workflow's `input_schema`.
- `GET /api/approvals?status=pending|approved|rejected|expired&run_id=` →
  `[{...Approval, tool_call?: ToolCall}]`; `POST /api/approvals/{id}/approve {reason?,
  always?}` and `POST /api/approvals/{id}/reject {reason?}` → `Approval`; 409 when
  already decided.
- `GET /api/agents` (exists) for the agent picker; `GET /api/tools` for tool risk.
- SSE on `/api/events` and `/api/runs/{id}/events` (existing hook `useEventStream`):
  new events `approval.requested {approval}`, `approval.decided {approval}`,
  `agent_message {agent_message}`; `run.status {run}` now carries `state` and
  `current_node`; `node.status {node}` for every node visit (several nodes per run,
  `visit` > 1 on loops).

## Build

1. **Workflows page** (`/workflows`): list (name, version, nodes count, template badge,
   updated), "New workflow", "From template" drawer (templates list → instantiate →
   open the new workflow), delete with confirmation.
2. **Builder** (`/workflows/:id`, `/workflows/new`): React Flow canvas with node kinds
   `start`, `agent`, `approval`, `end`; side panel editor for the selected node: agent
   node → agent picker (from `/api/agents`), task (textarea, placeholders `{{.input.x}}`
   and `{{.state.node.field}}` explained in one short hint), output schema (JSON
   editor with parse errors inline), `input_from` (multi-select of node ids),
   `on_failure` (node select); approval node → message, `on_reject`; edge editor →
   condition builder (`path`, `op`, `value`; nested `all`/`any` allowed as raw JSON
   with a visible fallback), edge order matters: show the order number and allow
   reordering among a node's outgoing edges. Limits panel (the seven limits). Input
   schema and state schema as JSON editors. Validate button → 422 details rendered
   inline next to the node or edge they name (map `nodes[i]`/`edges[i]` paths to
   elements) and in a list; Save (POST/PUT); Run button → input form generated from
   `input_schema` properties (string, number, boolean; everything else raw JSON) →
   POST `/api/runs` → navigate to the run page. Layout: auto-layout nodes left to right
   from `start` on first open (a simple layered layout by BFS depth is enough), then
   remember positions in the definition under a `ui: {positions: {id: {x, y}}}` key
   (the backend keeps unknown keys).
3. **Approvals inbox** (`/approvals`): pending by default with a status filter; each
   item shows kind, the run (link), node, message, requested and expires times, and for
   tool approvals the tool name and full input (JsonView); Approve / Reject with an
   optional reason (Approve-always only for kind `exec`); live via SSE; 409 shown as
   "already decided". Nav badge with the pending count.
4. **Run page**: an Approvals section (same rows, approve/reject inline); an Agent
   messages section (from → to, payload JsonView, time); State (JsonView of
   `run.state`, live); the timeline already groups by node, make sure several nodes and
   repeat visits read well (`visit 2`), and `waiting_approval` runs show what they wait
   for. Trigger column on the Runs list stays.
5. Nav: Workflows and Approvals entries; keep the existing pages and tests green.

## Tests (vitest + testing-library, fake fetch and SSE as in `RunDetailPage.test.tsx`)

Builder: definition round-trips through the editors (edit a condition, reorder edges,
save payload is correct); validation details land on the right node/edge; template
instantiate flow; run dialog builds input from `input_schema`. Inbox: lists pending,
approve sends the reason, 409 message, SSE `approval.decided` removes the row. Run
page: approvals section actions, agent messages render, state updates on `run.status`.

## Report

Per common.md, plus the list of API shapes you assumed beyond the ones above.
