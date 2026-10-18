# Wave 8 backend brief B: merge requests as records (5.20)

Spec: `docs/DESIGN.md` 5.20 (every rule) with its amendments to 5.16 rules 10 and 12 and
5.17 rule 8, decision D-042, and `api/openapi.yaml` 0.5.0: the `MergeRequest` schema,
`GET /api/merge-requests`, `GET /api/merge-requests/{id}`, `POST /api/merge-requests/{id}/sync`,
`POST /api/merge-requests/{id}/merge`, and `Run.merge_request`. Read
`docs/briefs/wave8/00-plan.md` first: the prerequisites there are ground truth and
frozen, in particular migration `0012_wave8.sql` (the `merge_requests` table), the
`store.MergeRequest` type and its constants, `Run.MergeRequest`, the four routes with
their 501 stubs in `internal/httpapi/mergerequests.go`, `MERGE_REQUEST_SYNC_INTERVAL` in
the config, and `engine.SendPlatformMessage` / `engine.RunReportsTo`.

Already in the tree and worth reading before you write: `internal/tools/mergerequest.go`
(`exec`, `run`, `checkoutDir`, `mergeRequestOutput`, the `requestProvider` interface),
`internal/tools/providers.go` (`apiClient`, `gitlabProvider`, `githubProvider`,
`errAPIStatus`), `internal/tools/executor.go` (`store.RunSideEffect` wraps a
side-effecting tool's `Exec`), `internal/store/side_effects.go` (`RunSideEffect`: the
key, the lock, `fn`, the result, one transaction), `internal/project/git.go` (`Clone`,
`Propose`, `clearWorkingTree`, `copyTree`, `ErrNoChanges`), `internal/store/tool_calls.go`
(`MergeRequestOfRun`, the trace lookup the record replaces), `internal/httpapi/runs.go`
(`getRun`, `listRuns`, `engineError`), `internal/sched/scheduler.go` (`Run` and
`TickOnce`: the loop shape to mirror), `internal/engine/report.go` (`reportsTo`,
`sendReport`, `reportLateMergeRequest`: the final-state report mirrors them), the fakes
`newFakeGitLab` (`gitlab_test.go`) and `newFakeGitHub(t, bare)` (`mergerequest_test.go`,
with a bare repository to push to) and `newFakeProvider` (`httpapi/projects_test.go`).

## Deliverables

1. **The record is written with the side effect (rule 2).** `store.RunSideEffect` runs `fn`
   inside its transaction but hands it only a context. Add `RunSideEffectTx` whose `fn`
   receives the `pgx.Tx` as well (`RunSideEffect` delegates to it), carry the transaction
   to the tool as `tools.Call.Tx pgx.Tx` set by the executor for side-effecting tools
   (`internal/tools/tool.go`, `executor.go`), and in `open_merge_request` insert the
   record through `call.Tx` right after the provider call succeeds, so a failed insert
   rolls the result back and the key is released (T5.20.1). `internal/store/merge_requests.go`:
   `InsertMergeRequest`, `GetMergeRequest`, `MergeRequestByRun`, `ListMergeRequests(ctx, q,
   MergeRequestFilter{ProjectID, RunID, State string; Limit int})` (newest first, 50 by
   default, 200 at most), `ListMergeRequestsToSync(ctx, q, since time.Time, limit int)`
   (open, created after `since`, `synced_at` nulls first then oldest), `UpdateMergeRequestSync`
   (one statement conditional on `state = 'open'`, writing state, `ci_status`, `head_sha`,
   `unresolved_threads`, `last_sync_error`, `synced_at`, `closed_at`), `MarkMergeRequestReported`,
   `MarkMergeRequestMerged` (state, `closed_at`, `merged_by`), `FillRunMergeRequests(ctx, q,
   runs []Run)`, `BackfillMergeRequests(ctx, q) (inserted int64, error)` (rule 7, from
   `tool_calls` as `MergeRequestOfRun` reads them, `ON CONFLICT DO NOTHING`, runs without a
   `project_id` skipped).
2. **One request per run (rule 2, 5.16 rule 10 amended).** In `mergerequest.go`'s `run`:
   first `MergeRequestByRun`; with a record, `pc.ProposeOnto(ctx, checkout, record.SourceBranch,
   in.Title)` (new in `internal/project/git.go`: `Clone` the record's branch instead of the
   default one, `clearWorkingTree`, `copyTree`, `add --all`, `status --porcelain`; clean
   means `ErrNoChanges`, which answers the record with `replayed: true` and no `updated`;
   otherwise one commit on top with the title as message and a push of `HEAD` to that
   branch) and answer `{number, url, branch, provider, replayed: true, updated: true}`
   from the record; without a record, today's path, then the insert of deliverable 1.
   `mergeRequestOutput` gains `Updated bool json:"updated,omitempty"`. A `Clone` that
   takes a branch keeps `--single-branch --depth 1`; the token handling of 5.16 rule 4
   is untouched.
3. **Providers (rule 3, rule 5).** Export the provider slice the sync loop needs from
   `internal/tools/providers.go`: `type RequestProvider interface { BranchExists; FindOpen;
   Create; Status(ctx, number int64) (RequestStatus, error); Merge(ctx, number int64) error }`
   with `ProviderFor(pc *project.Config, client *http.Client) RequestProvider` (the tool's
   `providerFor` uses it). `RequestStatus{State, CIStatus string; HeadSHA string;
   UnresolvedThreads *int; ClosedAt *time.Time; Err string}` from the calls and mapping
   table of rule 3, exactly (GitLab: the merge request, its discussions' first 100, its
   pipelines' first entry; GitHub: the pull, GraphQL `reviewThreads(first: 100) { isResolved }`
   at `<api base>/graphql` with the bearer token, a GraphQL refusal leaving
   `UnresolvedThreads` nil and `Err` set, then check runs and the combined status of the
   head). A 404 is `ErrRequestNotFound`. `Merge`: GitLab `PUT .../merge` with
   `should_remove_source_branch: true`, GitHub `PUT .../merge` with `merge_method: merge`;
   405, 406, 409 and 422 become `*ErrNotMergeable{Message}` (the first line of the
   provider's text); other non-2xx follow 5.5 rule 4 as today. Extend the two fakes to
   serve these routes with scripted answers.
4. **The sync loop (rules 3, 4, 8).** New package `internal/mergereq`: `Sync{Pool, Projects
   *project.Resolver, Client *http.Client, Reporter, Interval time.Duration, Log, Now
   func() time.Time}` with `Run(ctx)` (ticker, stops with ctx), `TickOnce(ctx) (synced,
   failed int)` (at most 50 records of the last 14 days, each under a 10 s timeout,
   provider errors into `last_sync_error`, a database error ends the pass with a log line),
   `SyncOne(ctx, id string) (*store.MergeRequest, error)` (the handler's), and
   `Backfill(ctx)`. The token comes from `Projects.Token(ctx, projectID)` (works for a
   deleted project; `project.ErrCannotRead` is recorded and the record skipped). When a
   sync or a merge moves a record to merged or closed and `engine.RunReportsTo(run)` says
   the run reports to a conversation, send the line of rule 4 (`!<n>` on GitLab, `#<n>`
   on GitHub) with key `report-<run id>-mr-<state>` through `Reporter.SendPlatformMessage`
   and mark `reported_state` only when the send was not deferred. `Reporter` is an
   interface the engine satisfies. The coordinator starts the loop and runs the backfill
   in `server.go`.
5. **API (rules 5, 6).** `internal/httpapi/mergerequests.go`: replace the stubs; `sync`
   answers the refreshed record; `merge` checks state and `ci_status` (409
   `merge_request_not_open`, 409 `merge_request_ci_not_passed` with the texts of rule 5),
   calls `Merge`, on success `MarkMergeRequestMerged` with `merged_by: "operator"` and the
   report of rule 4, on `*ErrNotMergeable` 409 `merge_request_not_mergeable`, on a timeout
   or 5xx a re-sync then 502 `provider_unavailable` when still open. `runs.go`: fill
   `merge_request` on get and on list (`FillRunMergeRequests`). The run header and
   `run_status` move to the record: `MergeRequestOfRun` is deleted with its callers
   (`report.go`'s late-grant lookup and `run_status` are the coordinator's; say in your
   report which callers remain). The merge needs the provider client: take it from
   `Deps.CheckTransport` as the check does (`s.checkClient`).
6. **Tests.** `internal/store`: T5.20.1 (a `fn` that inserts the record then fails leaves
   neither row), the backfill (T5.20.8), the conditional sync update. `internal/project`:
   `ProposeOnto` against a bare repository (a commit on top, `ErrNoChanges` on a clean
   tree). `internal/tools`: T5.20.2 through the tool with the fakes (second call pushes
   on top, answers replayed and updated; unchanged tree replayed only; never a second
   request), T5.20.3 as a table per provider over the fakes' scripted answers, T5.20.4,
   the merge refusals. `internal/mergereq`: T5.20.5 (once per state, keyed, a restart
   re-sends nothing, with a fake reporter), T5.20.9. `internal/httpapi`: T5.20.6,
   T5.20.7, T5.20.10 (`Run.merge_request` on get and list), and the conformance steps for
   the four paths as a `mergeRequestConformanceSteps(t)` in `mergerequests_test.go`
   (`openapi_test.go` is shared: the coordinator appends the call at integration).

## Rules of the house

- Everything runs in Docker through compose: package tests with
  `docker compose --profile tools run --rm go go test ./internal/store/ ./internal/project/ ./internal/tools/ ./internal/mergereq/ ./internal/httpapi/ -count=1`,
  vet with `docker compose --profile tools run --rm --no-deps go go vet ./...`. Never run
  `make run`, `make down`, `make clean` or anything that restarts the running stack;
  never run the whole suite: the coordinator does.
- Files you own: `internal/store/{merge_requests.go,side_effects.go}` and tests;
  `internal/tools/{mergerequest.go,providers.go,tool.go,executor.go}` and their tests and
  fakes (`gitlab_test.go`, `mergerequest_test.go`); `internal/project/git.go` and tests;
  everything under `internal/mergereq/`; `internal/httpapi/{mergerequests.go,runs.go}`
  and their tests. Nothing else: `types.go`, the migrations, `server.go`, `main_test.go`,
  `chief.go`, `report.go`, `engine.go`, `node.go`, `run.go`, `projects.go`, `web/`,
  `docs/`, `api/openapi.yaml` are other hands' or frozen. If the contract or a frozen
  type proves wrong, say so in your report with the exact mismatch instead of editing it.
- The token never appears in a message, a record, a log line or a test fixture; the
  provider's text goes through `obs.Redact` before it is stored in `last_sync_error`.
  Test tokens look like `glpat-not-a-real-token-00`. Provider clients come from
  `ProviderClient` (no redirects).
- Follow the existing patterns (`skills.go` for handlers, `storeError`, `writeError`,
  `decodeBody`; the scheduler for the loop; the DESIGN rule comments).
- Report any rule you had to interpret, and anything you left out.
