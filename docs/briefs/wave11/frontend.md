# Wave 11 frontend brief F: fork and join in the builder and on the run page

Spec: `docs/DESIGN.md` 5.26 rule 13 (the UI), with rule 2's validation messages (shown
on the node or edge they name), rule 12's representation and the edge cases;
`api/openapi.yaml` 0.8.0: `WorkflowNode.kind` values `fork` and `join`,
`WorkflowNode.join`, `WorkflowNode.checkout`, `RunNode.branch_id`, `Trace.branches`
(`RunBranch`), the SSE event `branch.status` (`{branch}`). Read
`docs/briefs/wave11/00-plan.md` first. The backend lands at the same time, so build
against the contract and the fakes the existing tests use; the coordinator runs the UI
against the real API at integration.

Already in the tree and worth reading before you write: `web/src/lib/api.ts`
(`NodeKind`, `NODE_KINDS`, `WorkflowNode`, `RunNode`, `Trace`, `SSEEvent`),
`web/src/lib/workflow.ts` (`KIND_LABEL`, `addNode`, `nodeIdProblem`, `outgoing`,
`stateReferences`), `web/src/lib/workflowErrors.ts` and its test (how 422 details map
onto nodes and edges), `web/src/components/workflow/{Builder.tsx, Canvas.tsx,
StepNode.tsx, NodePanel.tsx, EdgePanel.tsx, Outline.tsx, Problems.tsx,
TemplatesDrawer.tsx}` and `web/src/pages/WorkflowBuilderPage.test.tsx`,
`web/src/lib/trace.ts` (`applyEvent`, `buildTimelineEntries`, `RunView`) and
`trace.test.ts`, `web/src/lib/useRunTrace.ts` (how SSE events refresh the view),
`web/src/components/run/{Timeline.tsx, RunHeader.tsx}` and their tests,
`web/src/components/Badge.tsx` (`StatusBadge`), `web/src/pages/RunDetailPage.tsx`.

## Deliverables

1. **API client** (`api.ts`): `NodeKind` and `NODE_KINDS` gain `"fork"` and `"join"`
   (order: start, agent, approval, fork, join, end); `WorkflowNode` gains `join?:
   string` and `checkout?: string`; `RunNode` gains `branch_id?: string`; `RunBranch` as
   the contract (`status: "running" | "waiting_approval" | "succeeded" | "failed" |
   "cancelled"`); `Trace` gains `branches?: RunBranch[]`. `trace.ts`: `RunView` carries
   `branches` (empty when the trace has none); `applyEvent` handles `branch.status`
   (`{branch}`, an upsert by id for this run only, as `node.status` does).
2. **Builder (rule 13).** `KIND_LABEL` gains `fork: "Fork"`, `join: "Join"`; the
   add-node menu and the outline list both; `addNode` gives a fork `join: ""` and a
   join nothing. `StepNode`/`Canvas`: an accent of their own for fork and join (dark
   mode too); a fork's node shows `→ <join>` under its id when set. `NodePanel`: for a
   fork, a Join selector over the workflow's join nodes (`Join` label; when the workflow
   has none, the sentence `Add a join node for this fork to end at.` in place of the
   selector) and the text `Every edge whose condition holds starts a branch; the
   branches meet at the join.`; for a join, the sentence naming the fork that joins
   there (`Joins the branches of <fork>.`, or `No fork names this join yet.`), the list
   of that fork's heads (its outgoing edges' targets, in edge order), and, only when
   `def.project` is true, a Checkout selector (`None` default, then each head) under
   `Branches run at once and meet here; the checkout of the chosen branch continues past
   the join.`; neither kind shows the agent, task, schema, `input_from`, message,
   `on_reject` or `on_failure` fields (a join keeps the outgoing-edges list as every
   non-end node does; a fork too). `EdgePanel`: on an edge whose source is a fork, the
   line `Every edge whose condition holds starts a branch.` above the condition editor.
   Validation: the 422 details of 5.26 rule 2 are path-qualified like today's
   (`nodes[i].join`, `nodes[i].checkout`, `nodes[n].agent_id`, `edges[k].to`,
   `nodes[n].input_from[j]`, `nodes[n].on_failure`, `nodes[i]`): make sure
   `workflowErrors` maps `nodes[i].join` and `nodes[i].checkout` onto the node and keeps
   the message text; nothing else should need a change, verify with a test.
3. **Run page (rule 13).** `buildTimelineEntries`: a fork visit (`RunNode` whose
   `node_id` is a fork in the run's snapshot, or, simpler and contract-only, a node row
   that is the `fork_visit_id` of one or more branches) becomes one entry holding its
   branches (from `view.branches` with that `fork_visit_id`, in `started_at` order) and,
   for each branch, the node groups of the visits whose `branch_id` is that branch, in
   their order; the join's row (the node row of the fork's `join` is not known from the
   trace: use the row whose `started_at` equals its `finished_at` and whose `output` is
   `{"branches": [...]}` only if you must; otherwise treat the join row as an ordinary
   node card titled by its id, which is acceptable) follows. `Timeline`: the fork card
   has the fork's id, its status badge, `Started`/`Finished`, and a grid with one column
   per branch: the head (monospace), a `StatusBadge` for the branch status (`kind`
   `branch`, the five statuses; `waiting_approval` reads `Waiting for an answer`), the
   branch's node cards stacked (each existing `NodeCard`, `aria-label` `Node <id>,
   branch <head>`), the branch's `failure_reason` as the node cards show theirs. On a
   narrow screen the columns stack. The join card: `Joined <n> branches` with its time.
   `RunHeader`: while the run is not terminal and `current_node` names a node that is
   the fork of the newest fork visit (there are branches whose fork visit's `node_id`
   equals `current_node`), the step line reads `<fork id> (<n> branches running)` with
   `n` the branches whose status is `running` or `waiting_approval`; otherwise as
   today. The existing `Asks:` line needs no change.
4. **Templates drawer**: nothing to add if it lists what the API returns; verify with
   a test that a template with `fork` and `join` nodes renders in the drawer and
   instantiates.
5. **Tests** (vitest + testing-library, as the existing component tests): adding a fork
   and a join from the menu; the fork panel's selector and the empty sentence; the join
   panel's fork sentence, heads and Checkout selector (shown only with `project`); the
   edge panel's line; `workflowErrors` mapping of `nodes[i].join` and
   `nodes[i].checkout`; `trace.test.ts` for `branch.status`; the Timeline's fork card
   with two columns and their node cards, a failed branch's reason, the join card; the
   header line with the count; `api.test.ts` for the types if it checks them; `pnpm
   lint` clean.

## Rules of the house

- Everything runs in Docker, from this worktree in a throwaway container:

  ```
  docker run --rm -u "$(id -u):$(id -g)" -v "$PWD/web:/app" -w /app -e HOME=/tmp -e COREPACK_ENABLE_DOWNLOAD_PROMPT=0 -e COREPACK_HOME=/tmp/corepack -e npm_config_cache=/tmp/npm-cache node:24-slim sh -c 'mkdir -p /tmp/corepack-bin /tmp/corepack && corepack enable --install-directory /tmp/corepack-bin && export PATH=/tmp/corepack-bin:$PATH && pnpm test && pnpm lint'
  ```

  Write long outputs to a file under `/tmp` and read the file. Never run `make run`,
  `make down`, `make clean`, `make test-*`, `make lint` or `docker compose ...` from
  this directory: a stack is running and must not be restarted.
- Files you own: `web/src/**`. Nothing outside `web/`: the contract, the Go code and the
  docs are other hands'. If the contract lacks something you need, say so in your
  report; do not invent a field.
- Product copy is for the operator: the texts above and the design's, no developer
  notes, no placeholders. Match the existing components (`Button`, `Badge`, `Card`,
  `fields`), dark mode included.
- Commit nothing; the coordinator commits. Report: the files changed, the test
  output's last lines, every rule you had to interpret, and anything you found wrong
  in the design.
