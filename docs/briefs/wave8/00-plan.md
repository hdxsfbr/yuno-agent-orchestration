# Wave 8 plan: project instructions and notes, budgets, merge request records, retry

Spec: `docs/DESIGN.md` 5.18, 5.19, 5.20 and 5.21 (decisions D-040 to D-043), the
roadmap `docs/ROADMAP.md`, and the contract `api/openapi.yaml` 0.5.0 (the wave 8 paths
and schemas carry `x-planned: wave 8` and "planned" in their descriptions; the coordinator
removes the markers at integration, once each path is served).

## Agents

| Agent | Brief | Sections | Model |
|---|---|---|---|
| A, backend projects | `backend-projects.md` | 5.18, 5.19 | Sonnet 5.5 |
| B, backend merge requests | `backend-merge-requests.md` | 5.20 | Sonnet 5.5 |
| C, backend retry | `backend-retry.md` | 5.21 | Sonnet 5.5 |
| D, frontend | `frontend.md` | UI of all four | Sonnet 5.5 |

Andre chose Sonnet 5.5 for every agent (2026-10-17); the delicate parts (money windows,
the retry transition, one record per run) are covered by the designs' exact rules and
the independent review rather than by a stronger builder.

All four start together. No two write the same file (partition below). Shared types,
the migration, the routes and the engine interface are the coordinator's prerequisites,
landed before launch, and frozen for the wave.

## Prerequisites the coordinator lands before launch (ground truth for every agent)

1. `internal/store/migrations/0012_wave8.sql`: `projects.instructions TEXT NOT NULL
   DEFAULT ''`, `projects.budget_day_usd_micros BIGINT`, `projects.budget_month_usd_micros
   BIGINT`; `project_facts` (5.18 rule 2; unique `(project_id, key)`, cascade on the
   project, set null on the agent); `project_budget_alerts` (5.19 rule 5; primary key
   `(project_id, window, window_start, level)`); `merge_requests` (5.20 rule 1; unique
   `(project_id, number)`, unique `run_id`, cascade on the run); `runs.retries INT NOT
   NULL DEFAULT 0`; index `runs_project_created_idx ON runs (project_id, created_at)`.
2. `internal/store/types.go`: `Project` gains `Instructions string`,
   `BudgetDayUSDMicros *int64`, `BudgetMonthUSDMicros *int64`, `Spend *ProjectSpend`
   (`db:"-"`); new `ProjectSpend{DayUSDMicros, MonthUSDMicros int64; DayUntil,
   MonthUntil time.Time}` (JSON as the contract: `day_usd`, `month_usd`, `day_until`,
   `month_until`, the money as USD floats through a marshaller); `ProjectFact`;
   `MergeRequest` with the constants `MergeRequestOpen|Merged|Closed` and
   `CIStatusNone|Pending|Passed|Failed`; `Run` gains `Retries int` and `MergeRequest
   *MergeRequest` (`db:"-"`). `runs.go`'s `runColumns` and `CreateRun` carry `retries`;
   `projects.go`'s column list carries the three new columns. The spend, facts, alert
   and merge request queries are the agents' (A, A, A, B).
3. `internal/config/config.go`, `.env.example`, `compose.yaml`: `BUDGET_TICK` (default
   60 s) and `MERGE_REQUEST_SYNC_INTERVAL` (default 120 s), so
   `TestEnvExampleAndComposeCoverConfig` stays green.
4. `internal/httpapi/server.go`: the seven routes of the contract, pointing at handler
   methods that answer 501 `not_implemented` until their agent replaces them:
   `listProjectFacts`, `deleteProjectFact` in `projects_facts.go` (A);
   `listMergeRequests`, `getMergeRequest`, `syncMergeRequest`, `mergeMergeRequest` in
   `mergerequests.go` (B); `retryRun` in `runs_retry.go` (C).
5. `internal/engine/contract.go`: `Service` gains `Retry(ctx, runID, by string, limits
   *store.Limits) (*store.Run, error)`; `internal/engine/retry.go` holds a stub that
   returns `errors.New("retry: not implemented")` (C replaces it);
   `internal/httpapi/main_test.go`'s `fakeEngine` gets the same stub.
6. `internal/engine/report.go`: two exported helpers over what exists, `RunReportsTo(run
   *store.Run) (conversationID string, ok bool)` (the `reportsTo` test plus the
   conversation id from `trigger_ref`) and `SendPlatformMessage(ctx, runID,
   conversationID, key, text string) (deferred bool)` (`sendReport` with the key given
   by the caller; with an empty `runID` the log lines go to `slog` instead of the run's
   events). A's alerts and B's final-state reports call them.

## File partition

| Agent | Owns (create or edit) | Must not touch |
|---|---|---|
| A | `internal/project/{instructions.go,budget.go}` and tests; `internal/memory/{project.go,sweeper.go}` and tests; `internal/store/{projects.go,project_facts.go,conversations.go}` and tests; `internal/tools/{memory.go,chief.go}` and tests; `internal/httpapi/{projects.go,projects_facts.go,chief.go}` and tests; `internal/engine/{engine.go,node.go}` and `engine_wave8_test.go`; `internal/sched/scheduler.go` and tests; `internal/seed/seed.go` and tests; `internal/openclaw/materialize.go` and tests | everything of B, C and D; `types.go`, migrations, `server.go`, `report.go`, `run.go`, `walk.go`, `contract.go`, `api/openapi.yaml` (report mismatches instead) |
| B | `internal/store/{merge_requests.go,side_effects.go}` and tests; `internal/tools/{mergerequest.go,providers.go,tool.go,executor.go}` and their tests and fakes (`gitlab_test.go`, `mergerequest_test.go`); `internal/project/git.go` and tests; new package `internal/mergereq/`; `internal/httpapi/{mergerequests.go,runs.go}` and tests | A's, C's and D's files; `chief.go` (the coordinator adds `run_status.merge_request` at integration with B's store function) |
| C | `internal/engine/{retry.go,run.go,report.go}` and `retry_test.go`; `internal/store/runs.go` and tests; `internal/httpapi/runs_retry.go` and tests | A's, B's and D's files; `engine.go`, `node.go`, `walk.go`, `contract.go`; `chief.go` (the coordinator adds `run_status.retries`) |
| D | `web/src/**` | everything outside `web/` |

A touches `engine.go` only inside `queueRun`'s transaction (the budget check next to
the rate check) and `node.go` only inside `taskMessage` (the instructions and notes
blocks); C touches `run.go` only in `runRun` (the time limit restart and the root span
attribute). If an agent finds it needs a file it does not own, it stops and says so in
its report rather than editing it.

## Integration (coordinator, after the agents report)

1. `server.go` `Run`: start A's budget watcher and B's sync loop (`go x.Run(ctx)` beside
   the scheduler and the sweeper), run B's backfill after the migrations and the
   bootstrap, pass the dependencies (`project.Resolver`, the provider client, the
   engine as reporter, the clock).
2. `tools/chief.go` `run_status`: `merge_request` from B's `store.MergeRequestByRun`
   (keeping `merge_request_url`), `retries` from the run (5.20 rule 6, 5.21 rule 7);
   `report.go`'s late-grant lookup moves to the record too. `runs.go` `engineError`: the
   429 `project_budget_reached` case with A's `budgetRefusal`; `retry.go`: A's
   `project.CheckBudget` for a run with a project (5.19 rule 3).
3. `openapi_test.go`: append the three agents' conformance step functions to
   `TestOpenAPIConformance` and remove the `x-planned` markers from `api/openapi.yaml`.
   Full suite: `make test-go`, `make test-web`, `make lint`.
4. One independent review scoped to the wave's new code (codex if its quota is back,
   otherwise two Claude reviewer agents as in waves 5 and 7); filtered visibly; one fix
   pass; one commit; merge to main when green (main auto-deploys).
5. Derived docs: README (Projects: instructions, notes, budgets, merge requests, retry;
   the Chief's `run_status` fields), `docs/deploy.md` (`BUDGET_TICK`,
   `MERGE_REQUEST_SYNC_INTERVAL`, the `make seed-reset` reminder for the new prompts),
   `docs/threat-model.md` (operator text in prompts, notes written by agents are
   model-written text later models read, the merge action, the token's GraphQL use),
   `AI_USAGE.md`, `docs/test-results.md` (suite numbers, live verification).
6. Live verification on the public instance after the deploy and `make seed-reset`:
   instructions on the playground render in a run's task; a budget set low refuses a
   delegation with the text and the alert reaches Telegram (Andre sends the message);
   a Dev pipeline run's record syncs (state, CI) and the Merge button merges it; a run
   failed on purpose retries from its node.

## Review scope

New code only: the files in the partition, plus the prerequisites. The reviewer is told
which decisions stand (D-040 to D-043) so it does not re-open them.
