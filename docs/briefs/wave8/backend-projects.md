# Wave 8 backend brief A: project instructions and notes (5.18), project budgets (5.19)

Spec: `docs/DESIGN.md` 5.18 (every rule) and 5.19 (every rule), decisions D-040 and
D-041, and `api/openapi.yaml` 0.5.0: `Project.instructions`, `Project.max_cost_usd_per_day`,
`Project.max_cost_usd_per_month`, `Project.spend` (`ProjectSpend`), the same three input
fields on `ProjectInput`, `ProjectFact`, `GET /api/projects/{id}/facts`, `DELETE
/api/projects/{id}/facts/{factId}`, and 429 `project_budget_reached` on `POST /api/runs`
(the mapping in `engineError` is the coordinator's, see below). Read `docs/briefs/wave8/00-plan.md`
first: the prerequisites there are ground truth and frozen, in particular migration
`0012_wave8.sql`, the `Project`, `ProjectSpend` and `ProjectFact` types, the two routes
with their 501 stubs in `internal/httpapi/projects_facts.go`, `BUDGET_TICK` in the
config, and `engine.SendPlatformMessage` / `engine.RunReportsTo` in `report.go`.

Already in the tree and worth reading before you write: `internal/project/toolchain.go`
(`Instructions(c Config, runID string, tc Toolchain)` builds the protocol lines of 5.16
rule 9; `internal/engine/node.go` `taskMessage` calls it), `internal/memory/memory.go`
(`Save`, `Recall`, `Newest`, `List`, `Delete`, `SweepExpired`, the `live` filter and
the word-match rule, which the project notes mirror), `internal/memory/sweeper.go`
(`Sweeper.Also []SweepFunc`), `internal/tools/memory.go` (the two tools, `errMemoryDisabled`,
`memoryPolicy`), `internal/tools/chief.go` (`NewListProjects`, the delegation path),
`internal/httpapi/chief.go` (`delegator.Delegate` maps engine refusals to tool
errors), `internal/httpapi/projects.go` (validation with `fieldErrors`, `writeValidation`),
`internal/store/projects.go` (`FillProjectLastRuns` is the pattern for `FillProjectSpend`),
`internal/engine/engine.go` `queueRun` (the transaction with `guardrails.CheckRateLimit`,
where the budget check goes), `internal/sched/scheduler.go` `fire` (how a
`*guardrails.RateLimitError` becomes `last_error` and advances), `internal/seed/seed.go`
(the Coder and Reviewer prompts), `internal/openclaw/materialize.go` (the `AGENTS.md`
memory text).

## Deliverables

1. **Instructions and notes in the task (5.18 rules 1, 4).** `internal/httpapi/projects.go`:
   validate `instructions` (trim; at most 8192 bytes; no control character but `\n` and
   `\t`; the two 422 texts; absent clears) and store it with the other settings
   (`store.UpdateProject` / `CreateProject` carry it; add the column to the queries in
   `internal/store/projects.go`). `internal/project/instructions.go`: `Extras{Instructions
   string; Notes []store.ProjectFact}` and `InstructionsWith(c Config, runID string, tc
   Toolchain, x Extras) string`, which appends the two blocks of rule 4 after the sandbox
   network line with the exact headings, the `- <key>: <value>` lines (value cut at 300
   bytes plus `…`), and the 12 KiB cap (instructions first, oldest notes dropped first);
   `Instructions` keeps working as the empty-extras case. `internal/engine/node.go`
   `taskMessage`: for a project visit, load the project row's `instructions` and the 10
   newest live notes (`memory.NewestProjectFacts`) by the run's `project_id` and call
   `InstructionsWith`; a database error fails the visit with `project notes: <error>` as
   a task template error does.
2. **Project notes (5.18 rules 2, 3, 5, 6).** `internal/memory/project.go`: `SaveProjectFact(ctx,
   db Beginner, f *store.ProjectFact, now)` (upsert by `(project_id, key)`, `ttl_days`
   default 90 through a helper like `ExpiresAt`, cap 100 with the oldest by `created_at`
   evicted first in the same transaction, `MaxValueBytes` reused), `RecallProjectFacts(ctx,
   q, projectID, query string, limit int, now)` (5.10 rule 4's word match over the project's
   live notes, newest by `updated_at`), `NewestProjectFacts`, `ListProjectFacts` (with the
   agent's name joined, null when gone), `DeleteProjectFact(ctx, q, projectID, factID)`,
   `SweepExpiredProjectFacts`, appended to the sweeper in `sweeper.go`. `internal/tools/memory.go`:
   the optional `scope` property (`enum: ["agent", "project"]`, default `agent`) on both
   schemas with a one-line description each; `project` needs `call.RunID` set and a run
   whose `ProjectID` is not nil (`store.GetRun`), else the model error `project notes need
   a run on a project`; the memory policy gate stays as it is; a project save records
   `agent_id = call.AgentID` and `source_run_id = call.RunID`. `internal/httpapi/projects_facts.go`:
   replace the stubs (list newest first; delete 204; 404 `project not found` for a missing
   or soft-deleted project, 404 `memory fact not found`).
3. **Prompts (5.18 rule 8).** `internal/seed/seed.go`: the Coder's and Reviewer's prompts
   gain one sentence each about recalling and saving project notes (`memory_save` with
   `scope: "project"`); `internal/openclaw/materialize.go`: the memory section of
   `AGENTS.md` names the project scope in one sentence. `seed_test.go` and the
   materialiser tests follow. `make seed-reset` already re-applies prompts.
4. **Budget caps and spend (5.19 rules 1, 2, 7, 8).** `internal/httpapi/projects.go`: the
   two cap fields (USD floats in, micro-USD stored; `> 0` and `<= 1000000`, the four 422
   texts; absent or null clears). `internal/store/projects.go`: `ProjectSpend(ctx, q,
   projectID string, from, to time.Time) (int64, error)` and `FillProjectSpend(ctx, q,
   projects []Project, now)` (one query per window for the list), with `DayUntil` and
   `MonthUntil`. `internal/project/budget.go`: `Window{Kind, Start, End}` and `Windows(now)
   (day, month Window)` (UTC calendar day and month, end exclusive), `BudgetError{ProjectID,
   ProjectName, Window, CapMicros, SpendMicros, WindowEnd}` with `Error()` = the text of
   rule 3, and `CheckBudget(ctx, q store.Querier, p *store.Project, now) error` (day first,
   then month; `>=`). `internal/tools/chief.go` `list_projects`: `budget` per rule 8.
5. **Refusal at queue time (5.19 rule 3).** `internal/engine/engine.go` `queueRun`: inside
   the transaction, after the rate check and only when `proj != nil`, `project.CheckBudget(ctx,
   tx, proj, e.clock.Now())`; a `*project.BudgetError` is logged like a rate limit and
   returned as is. `internal/sched/scheduler.go` `fire`: an `errors.As` case for it, the
   text into `last_error`, advance. `internal/httpapi/chief.go` `delegator.Delegate`: the
   same case, `the run was not started: <text>`. The HTTP mapping (429
   `project_budget_reached` with the details of rule 3) belongs in `engineError` in
   `runs.go`, which agent B owns: write it as `func budgetRefusal(w http.ResponseWriter,
   be *project.BudgetError)` in `projects.go` and say in your report that the one-line
   case in `engineError` is the coordinator's; the retry path (5.21) gets the check from
   the coordinator too.
6. **Alerts (5.19 rules 5, 6).** `internal/project/budget.go`: `Watcher{Pool, Tick, Log,
   Now func() time.Time, Reporter}` with `Run(ctx)` (ticker, stops with ctx) and
   `TickOnce(ctx) (sent int)`; `Reporter` is an interface with `SendPlatformMessage(ctx,
   runID, conversationID, key, text string) (deferred bool)` (the engine satisfies it).
   Per live project with a cap, per window: compute the spend, decide `warning` (`>= 80 %`)
   and `reached` (`>= cap`), skip levels already in `project_budget_alerts`, resolve the
   destination (`store.GetChannelBinding(ctx, q, "telegram")` then the agent's newest
   conversation: add `NewestConversationOfAgent(ctx, q, agentID)` to
   `internal/store/conversations.go`), send with the key of rule 6, and insert the row only
   when the send was not deferred; without a destination log one line and insert the row.
   The texts of rule 5 exactly; money printed with two decimals. The coordinator starts
   the watcher in `server.go`.
7. **Tests.** `internal/project`: T5.19.1 (validation lives in httpapi; here the window
   edges of T5.19.2 and `CheckBudget` at `cap - 1`, `cap`, `cap + 1`), T5.19.5 and
   T5.19.6 and T5.19.7 with a fake reporter (deferred, then not), a fixed clock and the
   `yuno_test` database. `internal/memory`: T5.18.2, T5.18.5, T5.18.6, T5.18.9.
   `internal/tools`: T5.18.3, T5.18.4 (scope project with a chat token and with a run
   without a project), and `list_projects.budget`. `internal/engine`: T5.18.7, T5.18.8
   (the rendered task of a project visit, with `newEnv` and the scripted provider as
   `graph_test.go` does), T5.19.3 for a run request (the typed error from `Trigger`),
   T5.19.4. `internal/sched`: T5.19.3 for a schedule. `internal/httpapi`: T5.18.1,
   T5.18.10, T5.19.1, T5.19.8, the delegation case of T5.19.3 through `delegator`, and
   the conformance steps for the two facts paths and the new project fields as a
   `projectWave8ConformanceSteps(t)` in `projects_facts_test.go` (`openapi_test.go` is
   shared: the coordinator appends the call to `TestOpenAPIConformance` at integration). `internal/seed` and `internal/openclaw`:
   T5.18.12. Name tests after the rules as the codebase does.

## Rules of the house

- Everything runs in Docker through compose: package tests with
  `docker compose --profile tools run --rm go go test ./internal/project/ ./internal/memory/ ./internal/tools/ ./internal/engine/ ./internal/sched/ ./internal/httpapi/ ./internal/seed/ ./internal/openclaw/ -count=1`
  (the go service has `DATABASE_URL` for the `yuno_test` database), vet with
  `docker compose --profile tools run --rm --no-deps go go vet ./...`. Never run `make run`,
  `make down`, `make clean` or anything that restarts the running stack; never run the
  whole suite (`make test-go`): the coordinator does.
- Files you own: `internal/project/{instructions.go,budget.go}` and their tests;
  `internal/memory/{project.go,sweeper.go}` and tests; `internal/store/{projects.go,project_facts.go,conversations.go}`
  and tests; `internal/tools/{memory.go,chief.go}` and tests; `internal/httpapi/{projects.go,projects_facts.go,chief.go}`
  and tests; `internal/engine/engine.go` (inside `queueRun` only), `internal/engine/node.go`
  (inside `taskMessage` only) and a new `internal/engine/wave8_projects_test.go`;
  `internal/sched/scheduler.go` and tests; `internal/seed/seed.go` and tests;
  `internal/openclaw/materialize.go` and tests. Nothing else: `types.go`, the migrations,
  `server.go`, `main_test.go`, `runs.go`, `report.go`, `run.go`, `walk.go`, `contract.go`,
  `mergerequest.go`, `providers.go`, `git.go`, `web/`, `docs/`, `api/openapi.yaml` are
  other hands' or frozen. If the contract or a frozen type proves wrong, say so in your
  report with the exact mismatch instead of editing it.
- No secrets in fixtures; no real repository URLs with tokens; test tokens look like
  `glpat-not-a-real-token-00`.
- Money is micro-USD everywhere but the API boundary; comparisons use `>=` as the rules
  say; windows are UTC; the clock is injected.
- Comment with the DESIGN rule numbers as the code does; product text (the alert lines,
  the tool error texts, the 422 texts) exactly as the rules spell them.
- Report any rule you had to interpret, and anything you left out.
