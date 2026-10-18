# Wave 9 backend brief A1: iterate on a merge request, the engine side (5.22)

Spec: `docs/DESIGN.md` 5.22 rules 1, 2, 3, 7, 9 and 11, with the amendments they make to
5.16 rules 3 and 9, 5.9 rule 6, 5.20 rule 6 and 5.21 rules 2 and 3; decisions D-044,
D-049 and D-050; `api/openapi.yaml` 0.6.0: `RunCreate.merge_request_id`,
`Run.merge_request_id`, `Run.checkout_sha`, `GET /api/runs?trigger=&merge_request_id=`,
`ScheduleInput.merge_request_id`, `Schedule.merge_request_id` and
`merge_request_number`, and the 422 details on `merge_request_id`. Read
`docs/briefs/wave9/00-plan.md` first: the prerequisites there are ground truth and
frozen, in particular migration `0013_wave9.sql` (the two columns on `runs`, the two
live-run indexes, `schedules.merge_request_id`), `Run.MergeRequestID`, `Run.CheckoutSHA`,
`MergeRequest.Label()`, `ErrMergeRequestBusy` and `ErrIssueBusy`, `SetRunCheckoutSHA`,
`RunFilter`, `TriggerRequest.MergeRequestID`, and `project.Config.CloneBranch` and
`HeadSHA`.

Already in the tree and worth reading before you write: `internal/engine/trigger.go`
(`triggerWorkflow`, `projectForRequest`, `ProjectForRequest`: the shape your merge
request check follows), `internal/engine/engine.go` (`Trigger` for the quick run,
`queueRun`: the transaction with the rate and budget checks and the typed-error
mapping), `internal/engine/node.go` (`prepareProject`, `cloneInto`, `taskMessageWith`:
where the clone branch and the protocol paragraph go), `internal/engine/retry.go`
(`Retry`, `RetryError`), `internal/engine/report.go` (`runReportText`, `requestLabel`),
`internal/store/merge_requests.go` (`MergeRequestByRun`, `FillRunMergeRequests`),
`internal/httpapi/runs.go` (`runCreate`, `listRuns`, `engineError`),
`internal/httpapi/schedules.go` (`applySchedule`: how `project_id` is validated through
`engine.ProjectForRequest`, kept, dropped and cleared), `internal/sched/scheduler.go`
(`fire`: the `errors.As` cases that become `last_error` or disable the schedule),
`internal/engine/project_test.go` and `wave8_projects_test.go` (the local bare
repository as the remote, `newEnv`, the scripted provider), `docs/reviews/wave8-review.md`
(L4: why the clone takes the branch).

## Deliverables

1. **Queue-time checks (rule 1).** `internal/engine/trigger.go`: `MergeRequestForRequest(ctx,
   q store.Querier, id string, proj *store.Project) (*store.MergeRequest, []string,
   error)`, exported like `ProjectForRequest` so the schedule handler reuses it: an
   empty id answers nil; otherwise the checks of rule 1 in order, each a path-qualified
   message on `merge_request_id` with the exact texts (`the workflow does not use a
   project` when `proj == nil`; `no merge request with id <id>`; `<Label()> belongs to
   project <name>` with the record's project read through `store.GetProject` or, for a
   deleted one, `GetProjectAny`-style read of the name: add `store.ProjectNameAny(ctx, q,
   id)` in `merge_requests.go` if nothing serves; `<Label()> is <state>`; `<Label()> is
   being updated by run <id>` when the record's own run (`merge_requests.run_id`) is
   live, read with `store.GetRun`). Call it from `triggerWorkflow` and from `Trigger`'s
   quick-run path after the project check; put the record's id on the run in `queueRun`
   (`run.MergeRequestID`) and map `store.ErrMergeRequestBusy` from `CreateRun` to a
   `*ValidationError{Messages: ["merge_request_id: <Label()> is being updated by run
   <id>"]}` (find the live run with a new `store.LiveRunForMergeRequest(ctx, q, mrID)` in
   `merge_requests.go`). `store.MergeRequestByNumber(ctx, q, projectID string, number
   int64)` (A2's Chief needs it; write it here, it is your file) and `FillRunMergeRequests`
   filling from `runs.merge_request_id` when set, else from `merge_requests.run_id`
   (rule 1, 5.20 rule 6 amended); `GetMergeRequest` is unchanged.
2. **The clone (rules 2 and 3).** `node.go`: when `v.run.MergeRequestID` is set,
   `cloneInto` clones the record's `source_branch` (`pc.CloneBranch`) instead of
   `pc.Clone`; read the record once in `prepareProject` (`store.GetMergeRequest`; a
   missing record fails the visit with `project clone: merge request <id> no longer
   exists`). After any successful clone, `project.Config.HeadSHA(repo)` and
   `store.SetRunCheckoutSHA` (every project run, rule 2; a failed write fails the run
   like `record toolchain:`), `v.run.CheckoutSHA` refreshed. The `project_clone` span
   carries `branch` = the branch cloned and, for a named request, `merge_request:
   <number>`; the log line names the branch cloned. Nothing checks the record's state at
   clone time (rule 3, D-044). `taskMessageWith`: the `Merge request:` paragraph of rule 5
   after `project.InstructionsWith`'s first line (the repository line) and before the
   sandbox network line: write it in `project.InstructionsWith`'s caller by splitting
   the text at the first newline, or add `project.Extras.MergeRequest *store.MergeRequest`
   and render it inside `internal/project/instructions.go`... which is not your file: so
   render it in `node.go`, with the exact two sentences of rule 5 (`Label()`, the URL, the
   branch), and keep `project.Instructions` untouched. The repository line's `branch` must
   be the source branch: pass a copy of `*v.project` with `DefaultBranch` set to the
   source branch to `InstructionsWith` for the text only (the real default branch stays
   the merge target).
3. **Retry (5.21 rules 2 and 3 amended).** `retry.go`: `store.ErrMergeRequestBusy` from
   `RetryRun` becomes `*RetryError{Code: "run_not_retryable", Message: "<Label()> is
   being updated by run <id>"}` and `store.ErrIssueBusy` becomes `{Code:
   "run_not_retryable", Message: "issue #<n> already has a running run <id>"}` (the
   number is the part of `trigger_ref` after `#`; the live run through
   `store.GetRunByTrigger`-style lookup of the live one: add `store.LiveRunForTrigger(ctx,
   q, trigger, ref)` in `merge_requests.go` or reuse A1's own helper).
4. **Report-back (rule 9).** `report.go` `runReportText`: for a run with
   `MergeRequestID` the sentence is ` Updated merge request: <url>` (`Updated pull
   request:` on GitHub) when the trace holds an executed `open_merge_request` call whose
   output has `updated: true` (`store.ListToolCalls`, the newest executed one), and
   nothing about the request otherwise; a run without `MergeRequestID` keeps today's
   text.
5. **Schedules (rule 7).** `internal/httpapi/schedules.go`: `scheduleInput.MergeRequestID
   *string`; `applySchedule` keeps it when absent, drops it when `workflow_id` or
   `project_id` changes, clears it on `""`, and validates it with
   `engine.MergeRequestForRequest` against the schedule's project (422 details on
   `merge_request_id`; `merge_request_id: needs a project` when the schedule has none).
   `internal/sched/scheduler.go` `fire`: pass `MergeRequestID` in the trigger request; a
   `*engine.ValidationError` whose only message is `merge_request_id: <Label()> is
   merged` or `... is closed` disables the schedule with `last_error = "<Label()> is
   <state>"` (rule 7, D-049; match on a typed marker rather than the text: give
   `MergeRequestForRequest` a sibling `MergeRequestFinal(err error) (label, state string,
   ok bool)` or return a `*MergeRequestStateError` wrapped in the validation error, your
   choice, documented); every other refusal keeps today's behaviour (`last_error`,
   advance).
6. **API (rule 11).** `internal/httpapi/runs.go`: `runCreate.MergeRequestID` passed to the
   trigger; `listRuns` reads `trigger` (one of the five values, else 422 `trigger: must be
   one of ...`) and `merge_request_id` into the `RunFilter`. The 422 details already flow
   through `engineError` (`validationDetails`). Conformance steps for the new parameters,
   a run request with an unknown `merge_request_id` (422), and the schedule fields as
   `iterationConformanceSteps(t)` in `runs_iteration_test.go` (`openapi_test.go` is
   shared: the coordinator appends the call at integration).
7. **Tests.** `internal/engine/wave9_iteration_test.go` with `newEnv`, the scripted
   provider and the local bare repository (`project_test.go`'s helpers): T5.22.1 (every
   text in order, the happy path, the busy mapping from the index by inserting a live
   run first), T5.22.2 (the branch's file present and the default branch's later commit
   absent; the span attributes; `checkout_sha` equal to `git rev-parse` of the bare
   repository's branch; a deleted branch fails with `project clone:`), T5.22.3 (the
   paragraph present and absent), T5.22.6's engine half (a record set merged after
   queueing still clones), the retry mappings of deliverable 3 (both indexes), T5.22.10
   (`report_test.go`). `internal/store`: `MergeRequestByNumber`, `LiveRunForMergeRequest`,
   `FillRunMergeRequests` from the named record. `internal/httpapi`: the filters, the
   422 details, the schedule round trip (kept, dropped on a workflow or project change,
   cleared), the conformance steps. `internal/sched`: T5.22.9's tick half (the id passed;
   a final request disables with the text; a busy refusal advances with `last_error`).
   Name tests after the rules.

## Rules of the house

- Everything runs in Docker through compose from the main checkout, or in a throwaway
  container from a worktree: package tests with
  `docker compose --profile tools run --rm go go test ./internal/engine/ ./internal/store/ ./internal/httpapi/ ./internal/sched/ -count=1`,
  vet with `docker compose --profile tools run --rm --no-deps go go vet ./...`. Never run
  `make run`, `make down`, `make clean` or anything that restarts the running stack;
  never run the whole suite: the coordinator does.
- Files you own: `internal/engine/{trigger.go,engine.go,node.go,retry.go,report.go}`,
  `wave9_iteration_test.go` and `report_test.go`; `internal/store/merge_requests.go` and
  tests; `internal/httpapi/{runs.go,schedules.go}` and their tests; `internal/sched/scheduler.go`
  and tests. Nothing else: `types.go`, `runs.go` and `schedules.go` in the store,
  `contract.go`, `git.go`, `instructions.go`, the migrations, `server.go`, `main_test.go`,
  `mergerequest.go`, `providers.go`, `chief.go`, `projects.go`, `web/`, `docs/`,
  `api/openapi.yaml` are other hands' or frozen. If the contract or a frozen type proves
  wrong, say so in your report with the exact mismatch instead of editing it.
- The token never appears in a message, a span, a log line or a fixture; git errors go
  through `project.Config`'s redaction as today. Test tokens look like
  `glpat-not-a-real-token-00`.
- Product text (the 422 details, the protocol paragraph, the report sentence, the
  `last_error` texts) exactly as the rules spell it; `Label()` for every request noun.
- Comment with the DESIGN rule numbers as the code does. Report any rule you had to
  interpret, and anything you left out.
