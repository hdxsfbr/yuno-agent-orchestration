# Wave 8 backend brief C: retry from the failed step (5.21)

Spec: `docs/DESIGN.md` 5.21 (every rule) with its amendment to 5.1 rule 2, decision
D-043, and `api/openapi.yaml` 0.5.0: `POST /api/runs/{id}/retry` with `RunRetry` and
`Run.retries`. Read `docs/briefs/wave8/00-plan.md` first: the prerequisites there are
ground truth and frozen, in particular migration `0012_wave8.sql` (`runs.retries`),
`Run.Retries` with `retries` in `runColumns`, the route with its 501 stub in
`internal/httpapi/runs_retry.go`, `Service.Retry` in `contract.go` with the stub in
`internal/engine/retry.go` and in `main_test.go`'s `fakeEngine`.

Already in the tree and worth reading before you write: `internal/engine/run.go`
(`runRun`: the resumed path, `started_at`, `FailRunningNodes`, `OpenRootSpan`, the root
span's attributes, `finishRun`), `internal/engine/engine.go` (`Cancel` and `transition`
as the shape of an operator action; `emit`, `logEvent`), `internal/engine/limits.go`
(`resolveLayeredLimits`, `negativeLimits`, `checkLimits`), `internal/engine/report.go`
(`reportKey`, `reportRun`, `runReportText`), `internal/store/runs.go` (`SetRunStatus`,
`RequestCancel`, `RequeueStaleRuns`: `attempt < 2` is the crash re-queue that `attempt = 1`
on retry keeps per retry), `internal/store/engine_queries.go` (`TransitionRun`,
`RequeuePausedRunAt`, `SetRunStartedAt`), `internal/httpapi/runs.go` (`cancelRun`,
`engineError`), `internal/engine/main_test.go` (`newEnv`, the scripted provider) and
`graph_test.go` (`devPipeline`), `internal/engine/walk.go` (where `current_node` moves,
`nodeFailed` and `on_failure`).

## Deliverables

1. **The retry (rules 1, 2, 3).** `internal/engine/retry.go`: `Retry(ctx, runID, by string,
   limits *store.Limits) (*store.Run, error)` replacing the stub. In one transaction with
   the run row locked (`store.GetRunForUpdate`, new in `runs.go`): status in `failed`,
   `timed_out`, `cancelled` and `current_node` set, else `*RetryError{Code: "run_not_retryable",
   Message: "run is <status>" | "run has no step to retry from"}`; `retries < 5` else
   `{Code: "retry_limit", Message: "run was retried 5 times"}`; when `limits` is given,
   resolve it as a trigger does (`resolveLayeredLimits` over the snapshot's workflow limits
   and the config; negative values are a `*ValidationError`, clamping as usual) and use
   the result as the run's limits; then compare `step_count`, `tool_call_count` and
   `cost_usd_micros` with `max_steps`, `max_tool_calls` and `max_cost_usd`, a counter `>=`
   its cap giving `{Code: "limits_exhausted", Details: [{path: "max_steps", message:
   "max_steps: 40 of 40 used"}, ...]}` (every exhausted one). Then `store.RetryRun(ctx, tx,
   id, limits *Limits, now)`: `status = queued`, `retries + 1`, `attempt = 1`,
   `failure_reason`, `finished_at`, `cancel_requested`, `cancel_by`, `resume_node_visit_id`,
   `reported_at` cleared, `started_at = NULL` and `paused_total_ms = 0` (the time limit
   restarts when the run is next claimed: `runRun` already sets `started_at` when it is
   nil), `limits` replaced when given, `WHERE status IN (...)`, returning the row; no row
   means a concurrent retry won, answer `run_not_retryable`. The transition table in
   `transition` (or wherever `invalid transition` is raised) gains `failed | timed_out |
   cancelled → queued` for this path only. After the commit: a run event `retry` with
   `{retries, from_node, by, limits}` through `e.emit` (persisted by the recorder, so the
   UI replays it as SSE `run.retry`), `run.status`, and a log line `retry <n>: continuing
   from <node>`. The project budget check of 5.19 rule 3 for a run with a project is the
   coordinator's at integration (agent A's function); leave a `// 5.19 rule 3: budget
   check (integration)` comment where it goes.
2. **Resumption (rule 4).** `run.go` `runRun`: the root span of a retried claim carries
   `retry: run.Retries` beside `attempt`; nothing else should be needed because the
   resumed path (`current_node` set, unfinished visits failed, `OpenRootSpan` finding
   none and a new one opening) is what a retry rides on. Verify with the test of T5.21.4
   that the walk creates visit n+1 of the node, keeps `state`, continues the counters,
   and that the copy-forward of 5.6 rule 13 brings the failed visit's file to the new
   visit; fix what does not hold, inside `run.go` or `retry.go` only, and say so.
3. **Report-back (rule 6).** `report.go`: `reportRun` uses the key `report-<run id>-terminal-r<n>`
   when `run.Retries > 0` (`reportRunID` must still parse it) and the text of 5.15 rule 9
   gets ` (retry <n>)` after the status; `reported_at` was cleared by the retry so the
   sweep picks the run up again.
4. **API (rule 1, rule 7).** `internal/httpapi/runs_retry.go`: replace the stub; decode
   the optional body (`{limits}` as `RunCreate` decodes limits), call `s.engine.Retry(ctx,
   id, "operator", limits)`, map `*engine.RetryError` to 409 with its code, message and
   details, a `*engine.ValidationError` to 422 (`validationDetails`), `store.ErrNotFound`
   to 404, a `*project.BudgetError` is the coordinator's; 202 with the run. Add the
   conformance steps (202, 409 on a running run, 404) as a `retryConformanceSteps(t)`
   in `runs_retry_test.go` (`openapi_test.go` is shared: the coordinator appends the
   call at integration).
5. **Tests.** `internal/engine/retry_test.go` with `newEnv` and the scripted provider:
   T5.21.1 (every status, null `current_node`), T5.21.2, T5.21.3 (exhausted, raised,
   clamped), T5.21.4, T5.21.5 (a run timed out, retried, runs its full time limit again),
   T5.21.6 (cancelled at an approval node: a new approval is created on resume),
   T5.21.7 (two goroutines, one 202, one `run_not_retryable`), T5.21.8 (the report key and
   text, with the fake gateway's record of `chat.send` calls as `report_test.go` does).
   `internal/store`: `RetryRun` conditional on status, `GetRunForUpdate`. `internal/httpapi`:
   the handler's mappings and the conformance steps. Name tests after the rules.

## Rules of the house

- Everything runs in Docker through compose: package tests with
  `docker compose --profile tools run --rm go go test ./internal/engine/ ./internal/store/ ./internal/httpapi/ -count=1`,
  vet with `docker compose --profile tools run --rm --no-deps go go vet ./...`. Never run
  `make run`, `make down`, `make clean` or anything that restarts the running stack;
  never run the whole suite: the coordinator does.
- Files you own: `internal/engine/{retry.go,run.go,report.go}` and `retry_test.go`
  (and `report_test.go` for T5.21.8); `internal/store/runs.go` and tests;
  `internal/httpapi/runs_retry.go` and tests. Nothing else: `engine.go`, `node.go`,
  `walk.go`, `contract.go`, `limits.go`, `types.go`, `engine_queries.go`, the migrations,
  `server.go`, `main_test.go`, `runs.go`, `chief.go`, `web/`, `docs/`, `api/openapi.yaml`
  are other hands' or frozen. If you need a change in a frozen file (a transition helper
  in `engine_queries.go`, say), write the function in `runs.go` or `retry.go` instead and
  explain in your report; if the contract or a type proves wrong, report the exact
  mismatch instead of editing it.
- The clock is injected (`e.clock`); counters are the run's true totals and never reset;
  comparisons use `>=` as the rules say.
- Comment with the DESIGN rule numbers as the code does; error texts exactly as the
  rules spell them.
- Report any rule you had to interpret, and anything you left out.
