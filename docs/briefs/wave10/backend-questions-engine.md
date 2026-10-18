# Wave 10 backend brief A: questions to the operator, the engine side (5.24)

Spec: `docs/DESIGN.md` 5.24 rules 2, 3, 4, 5, 8, 10 and 11 (the API part), with the
amendments they make to 5.3 (the fourth kind), 5.21 rules 2 and 4 and 5.6's entry
check; decisions D-046 and D-056; `api/openapi.yaml` 0.7.0: `Approval.kind` value
`question`, `Approval.options`, `Approval.answer`, `POST /api/approvals/{id}/answer`,
`RunNode.question_id`. Read `docs/briefs/wave10/00-plan.md` first: the prerequisites
there are ground truth and frozen, in particular migration `0014_wave10.sql`
(`approvals.options`, `approvals.answer`, `run_nodes.question_id`, `run_nodes.parked_at`,
the partial index), `ApprovalKindQuestion`, the `Asker` methods on the engine interface
(`AskOperator`, `WaitQuestion`, `AnswerQuestion`, stubbed in `internal/engine/wave10.go`:
replace the bodies in your `questions.go` and delete the stubs), and the `Questions:`
paragraph of `protocol.go` (B's tool and prompts assume exactly these behaviours).

Already in the tree and worth reading before you write: `internal/engine/approvals.go`
(`RequestApproval`: the waiter, the insert, `v.pause`, the span and the emit; `WaitApproval`;
`DecideApproval` and `decideNode`; `expireApproval`, `sweepApprovals`, `failRun`,
`closeWaitingRun`, `requeueIfUnattended`, `visitOf`), `internal/engine/node.go`
(`turnFinal`: the turn that ends without `finish`; `startTurn(ctx, "Continue.")`: the
next-turn path you reuse; `pause`, `resumeIfClear`, `end`, `do`, `post`, `finish`),
`internal/engine/walk.go` (`resume`, `enter` with `CountNodeVisits`, `agentNode`,
`approvalNode` and `pauseAt`: the paused outcome of a node), `internal/engine/retry.go`
(`exhaustedLimits` and the `CountNodeVisits` call), `internal/store/engine_queries.go`
(`PauseRunAt`, `ResumeRunAt`, `RequeuePausedRunAt`, `InsertApprovalAndPause`),
`internal/store/runs.go` (`approvalSelect`, `InsertApproval`, `DecideApprovalAt`,
`GetApprovalForRunNode`, `ListPendingApprovals`, `CountNodeVisits`),
`internal/httpapi/approvals.go` (`decide`, `decodeOptionalBody`, `writeNotPending`),
`internal/httpapi/agents.go` (`checkInteractionRule`), `internal/httpapi/mcp_gate_test.go`
and `internal/engine/approval_races_test.go` (how the gate and its races are tested
with the scripted provider and a short `MCPToolWait`), `internal/engine/approvals_test.go`.

## Deliverables

1. **`AskOperator` (rule 2).** `internal/engine/questions.go`: `func (e *Engine)
   AskOperator(ctx, token, question string, options []string) (*store.Approval, error)`.
   The token resolves as `RequestApproval` does (`e.tokens.get`; unknown is
   `ErrInvalidRunToken`). Under the visit's loop (`v.do`): count the step's questions
   (`store.CountStepQuestions(ctx, q, runID, nodeID)`, rows of kind `question` with the
   run and node id, any status, in your `store/approvals.go`) and refuse the fourth
   with `&tools.ModelError{Message: "ask fewer questions: this step already asked 3"}`;
   refuse a second pending question of this visit (`v.pendingQuestion != ""`) with
   `one question at a time: question <id> is still pending`. Then the row: `Kind:
   store.ApprovalKindQuestion`, `Message: obs.Redact(question)`, `Options` the JSON
   array (nil when none), `RunNodeID: v.rn.ID`, `NodeID: v.node.ID`, `ExpiresAt: now +
   approval_timeout_seconds` (the same default as `RequestApproval`); the waiter
   registered before the insert (as `RequestApproval`), `store.InsertApproval`, the
   `approval_wait` span with `kind: question`, `v.toolBlocking[a.ID] = true` and
   `v.pause`, `v.pendingQuestion = a.ID`, SSE `approval.requested`, the log line
   `<agent name> asks: <first 200 bytes of the question>`, then
   `e.announceQuestion(ctx, run, a)` (P landed it in `report.go` as a stub; B writes
   its body, the notice of rule 6; you only call it). A
   visit that ended between the token check and the insert drops the waiter and
   expires the row with `step finished` as `RequestApproval` does with `run ended`.
   `WaitQuestion(ctx, id)` is `WaitApproval(ctx, id)`.
2. **Results for the model (rule 4).** The tool (B's) maps the decided row to the
   result: your job is the row and the state. When the waiter gets a decision:
   `approved` → the row carries `Answer`; `rejected` → the decline; `expired` → the
   unanswered case; the visit resumes through `unblock` and `resumeIfClear` as a tool
   approval does. When the wait ends with no decision (B's tool sees `ctx.Err()` from
   `WaitQuestion`), B calls `e.QuestionPending(ctx, token, id)`: add it to
   `questions.go` (not on the interface: B calls it through a second, optional
   interface `tools.QuestionParker` that your engine satisfies; declare it in B's file,
   you implement the method): under `v.do` it sets `v.parkedOn = id`, clears
   `v.toolBlocking[id]` and calls `resumeIfClear` so the run and node return to
   `running` (rule 4's "the turn goes on"). `v.pendingQuestion` stays set.
3. **Decision delivery and the park (rule 5, Consistency).** `AnswerQuestion(ctx, id,
   answer, by)`: validates the row (`store.GetApproval`; not a question →
   `ErrNotQuestion`, a new sentinel in `questions.go`; not pending → `ErrNotPending`),
   records it with `store.AnswerApprovalAt(ctx, q, id, obs.Redact(answer), by, now)`
   (`approved`, `answer`, `decided_at`, `decided_by`; `ErrNotFound` when no longer
   pending, as `DecideApprovalAt`), closes the span and emits `approval.decided`
   (`announceDecision`), then delivers. `DecideApproval` gains the case
   `store.ApprovalKindQuestion` → `decideQuestion` for a decline (`rejected`) and the
   expiry path; `approve` on a question is refused by the HTTP layer (deliverable 6)
   and by `DecideApproval` with a `*ValidationError` `answer: a question needs an
   answer`. Delivery, in this order, in one function `deliverQuestion(ctx, a)`:
   (i) `store.RequeueParkedRun(ctx, q, a.RunID, a.RunNodeID, now)`: `UPDATE runs SET
   status = 'queued', paused_total_ms = ..., paused_at = NULL WHERE id = $1 AND status
   = 'waiting_approval' AND resume_node_visit_id = $2 AND EXISTS (SELECT 1 FROM
   run_nodes WHERE id = $2 AND parked_at IS NOT NULL) RETURNING ...` (the paused
   interval added as `RequeuePausedRunAt` does); when a row changes: close the parked
   visit's row `succeeded` without output (`store.FinishRunNode(..., NodeSucceeded,
   nil, now)`), emit `node.status` and `run.status`, log `question answered; continuing
   <node>`, done. (ii) `e.deliver(a)` (a waiter): done. (iii) the live visit (`visitOf`):
   `v.do(func(){ v.answered = a })`; when `do` returns `ErrInvalidRunToken` (the loop
   is gone) go back to (i) once more; when it succeeds, done: `turnFinal` (deliverable
   4) sends the next turn. (iv) nothing applies and the run is `waiting_approval` with
   no pending approvals (the api restarted while the call blocked): re-queue as
   `requeueIfUnattended` does. Every step is idempotent; a failure in (i) after the
   decision is logged and the next approval sweep (deliverable 5) runs
   `requeueParkedDecided`, which repeats (i) for every parked run whose question is
   decided.
4. **The turn's end (rule 5).** `node.go` `turnFinal`, after the `finished()` checks
   and before the "No finish" reconcile: when `v.parkedOn != ""` and no `finish` was
   accepted: re-read the question (`store.GetApproval`); if `v.answered != nil` or the
   row is decided → `v.parkedOn = ""`, `v.pendingQuestion = ""`, build the paragraph
   with `e.answerParagraph(ctx, run, node)` (deliverable 7) plus `Continue your task and
   call yuno__finish when done.` and `v.startTurn(ctx, message)` (the "Continue." path:
   a non-nil outcome ends the visit); else park: `store.ParkVisit(ctx, pool, v.run.ID,
   v.rn.ID, now)` in one transaction sets `run_nodes.parked_at = now` and
   `PauseRunAt(run, rn)` (rule 4 had returned the run and the node to `running` when the
   wait ended, so set `rn.Status = NodeWaitingApproval`, save it, and emit `node.status`
   and `run.status`), log `<agent> waits for the answer to question
   <id>; the step continues when it arrives`, and end the visit with `outcome{paused:
   true}` (`v.end` with a paused outcome: extend `end` so a paused outcome skips the
   limit check and is stored). A failed `ParkVisit` ends the visit `failed` with `park:
   <error>` and expires the question `step finished`. `walk.agentNode`: after
   `e.collectArtifacts(wctx, v)` and `v.finish(wctx, res)`, `if res.paused { return "",
   &res }` before the `nodeFailed` branch (the worker lets the run go as an approval
   node does; `v.finish` must not mark a paused visit's row finished: make `finish`
   skip `FinishRunNode` for `res.paused`). Any other end of the visit (`finish`
   accepted, a reply result, error, abort, limit, cancel) with `v.pendingQuestion != ""`
   expires the question with `step finished` (`expireApproval`; in `end` or `finish`,
   after the result is fixed so a decision a moment earlier is 409 for the decider).
   `v.pendingQuestion` is cleared when its row is decided (`approvalDecided` path for
   the waiter case; your `answered` handoff for the live case).
5. **Expiry, cancel and the sweeper (rule 8).** `expireApproval` gains the case
   `ApprovalKindQuestion`: a waiter gets it (`deliver`, the tool answers
   `unanswered`); a parked run (the `RequeueParkedRun` predicate holds, read with
   `store.IsParkedOn(ctx, q, runID, rnID)`) fails with `question unanswered`
   (`failRun`, whose paused-run path transitions `waiting_approval → failed` and runs
   `closeWaitingRun`: the parked visit's row fails with the same reason through
   `unfinishedVisits`); a live visit past the wait: nothing more (the tool already
   returned `pending`; the turn's end finds the row decided and sends the "did not
   answer in time" paragraph, deliverable 7). `sweepApprovals` already fails the run on
   expiry with `approval expired`: for a question the reason must be `question
   unanswered`, so branch on the kind. `cancelWaitingRun` and `closeWaitingRun` expire
   it with `run cancelled` as today (nothing to do beyond the kind being handled).
   `sweepApprovals` also calls `requeueParkedDecided` (deliverable 3).
6. **HTTP (rules 3 and 11).** `internal/httpapi/approvals.go`: `answerApproval`
   replaces P's 501 stub: body `{answer}` required (`decodeBody`), trimmed, 1 to 4096
   bytes (`answer: 1 to 4096 characters`), 404 unknown, the row's kind not `question` →
   422 `answer: only a question can be answered`, not pending → 409 `not_pending` as
   `writeNotPending`, then `s.engine.AnswerQuestion(ctx, id, answer, decidedBy)`
   mapped like `decide` (`ErrNotPending` 409, `ErrNotFound` 404, `ErrNotImplemented`
   503). `approveApproval` on a question → 422 `answer: a question needs an answer`
   (check the kind in `decide` before calling the engine); `rejectApproval` on a
   question works as a decline (no change but a test). `internal/httpapi/agents.go`
   `checkInteractionRule`: `name == tools.AskOperatorName && value ==
   tools.PolicyRequireApproval` → `must be auto or blocked` on the path
   `interaction_rules.ask_operator` (rule 1's refusal; B owns the tool, you own the
   form's check). Conformance: `questionConformanceSteps(t) []conformanceStep` in your
   `approvals_test.go` covering the answer path (200, 404, 409, 422 on a tool approval)
   and an approval listing that shows `options` and `answer`; the coordinator appends
   it.
7. **The continuation visit and the paragraph (rules 5 and 7).** `store.ContinuationQuestion(ctx,
   q, runID, nodeID) (*Approval, error)`: the newest decided question of the step whose
   visit has `parked_at` set and that no `run_nodes.question_id` names; `ErrNotFound`
   when none. `store.CountNodeLoopVisits(ctx, q, runID, nodeID) (int, error)`: visits
   with `question_id IS NULL`; `walk.enter` and `retry.exhaustedLimits`'s caller use it
   instead of `CountNodeVisits` (which stays for the visit number). `walk.agentNode`:
   after `newVisit`, `ContinuationQuestion` sets `v.rn.QuestionID` (saved) when found.
   `store.ListStepQuestions(ctx, q, runID, nodeID) ([]Approval, error)`: the step's
   decided questions, oldest first. `e.answerParagraph(ctx, run, node) string`: the
   blocks of rule 7 (`Earlier in this step you asked the operator: "<question>"` then
   `The operator answered: <answer>` / `The operator declined to answer<: <reason>>.
   Proceed without the answer and state what you assumed.` / `The operator did not
   answer in time. Proceed without the answer and state what you assumed.`), one per
   decided question, separated by blank lines; empty when none. `walk.renderTask`
   appends it (after the rendered task, before the protocol block that `taskMessageWith`
   adds) for every agent visit, so a continuation, a retry and a loop revisit all carry
   it.
8. **Tests** (`internal/engine/wave10_questions_test.go`, `internal/store/approvals_test.go`,
   `internal/httpapi/approvals_test.go`): T5.24.2 to T5.24.9 and T5.24.12 as the design
   lists them, with the scripted provider and a short `MCPToolWait` as the gate tests
   do: the in-wait answer (the row's `answer`, the run paused then running, the waiter
   fed), the decline and the 1 s expiry, the park after the wait (`parked_at`, run and
   node `waiting_approval`, the worker free, `RequeueParkedRun` on the answer, the
   continuation visit's `question_id`, the paragraph in its task,
   `CountNodeLoopVisits` versus `CountNodeVisits`), the decision between the wait and
   the turn's end (the next turn's message, no park), the finish after pending (`step
   finished`, 409 later), the parked expiry (`question unanswered`, the retry's
   paragraph with the unanswered line), the cancel while parked, the fourth and the
   concurrent second question, the API cases and `interaction_rules.ask_operator`.

## Rules of the house

- Everything runs in Docker, from this worktree in a throwaway container on the stack's
  network with your own test database:

  ```
  docker run --rm --network yuno_default -v "$PWD:/src" -v yuno_gomod:/go/pkg/mod -v yuno_gocache:/root/.cache/go-build -w /src -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 -e DATABASE_URL='postgres://yuno:yuno@postgres:5432/yuno_test_a1?sslmode=disable' yuno-go-tools:1.26 sh -c 'go test ./internal/engine/ ./internal/store/ ./internal/httpapi/ -count=1 2>&1 | tail -60'
  ```

  vet with `go vet ./...` and `gofmt -l .` in the same container. Write long outputs to
  a file under `/tmp` and read the file. Never run `make run`, `make down`, `make
  clean`, `make test-*`, `make lint` or `docker compose ...` from this directory: a
  stack is running and must not be restarted; never run the whole suite: the
  coordinator does.
- Files you own: `internal/engine/{questions.go, approvals.go, node.go, walk.go,
  retry.go}` and `wave10_questions_test.go`; `internal/store/{approvals.go,
  engine_queries.go}` and their tests; `internal/httpapi/{approvals.go, agents.go}` and
  their tests. Nothing else: `types.go`, `runs.go` (store), `contract.go`, `wave10.go`
  (engine and tools), `protocol.go`, `report.go`, the migrations, `server.go`,
  `main_test.go`, `web/`, `docs/`, `api/openapi.yaml` are other hands' or frozen. If the
  contract or a frozen type proves wrong, say so in your report with the exact change
  you need; do not edit it. B edits `report.go` and `chief.go` at the same time: call
  B's `announceQuestion` by name and leave its body to B.
- Rule texts are exact: copy them from 5.24, do not paraphrase. Every error the model
  sees is a `*tools.ModelError`; every refusal the operator sees is a 422 detail or a
  409 as the design says. No reasoning text in any fixture.
- Commit nothing; the coordinator commits. Report: the files changed, the test
  output's last lines, every rule you had to interpret, and anything you found wrong
  in the design.
