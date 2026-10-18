# Wave 9 backend brief B: issue intake (5.23)

Spec: `docs/DESIGN.md` 5.23 rules 1 to 6 and 8 (rule 7 and the Chief's parts of rules
1, 8 and 10 are A2's), with the amendments to 5.19 rule 3 and 5.21 rule 3; decisions
D-045, D-052, D-053 and Andre's answer on the budget line (D-055); `api/openapi.yaml`
0.6.0: `ProjectIntakeInput`, `ProjectIntake` on `Project`, `IntakePollResult`, `POST
/api/projects/{id}/intake/poll`, `Run.trigger` value `issue`, `reported_at`'s wider
meaning. Read `docs/briefs/wave9/00-plan.md` first: the prerequisites there are ground
truth and frozen, in particular migration `0013_wave9.sql` (the five project columns,
`runs_issue_live_idx`), `store.TriggerIssue`, `store.ErrIssueBusy`, the intake fields and
the `Intake` view on `store.Project` with `fillIntake`, `INTAKE_POLL_INTERVAL` in the
config, the route with its 501 stub in `internal/httpapi/projects_intake.go`, and
`engine.ValidateInput`.

Already in the tree and worth reading before you write: `internal/sched/scheduler.go`
(`Run`, `TickOnce`, `fire`: the loop shape and the `errors.As` cases for the engine's
refusals), `internal/mergereq/sync.go` (a per-record pass with a provider client from
`tools.ProviderFor`, the resolver's `ForRunID`, `cleanError`, the reporter), `internal/project/budget.go`
(`CheckBudget`, `BudgetError`, the `Watcher`), `internal/tools/providers.go`
(`apiClient`, `do`, `errAPIStatus`, `providerMessage`, `newRequestProvider`: your issue
client is built the same way, in your own file), `internal/tools/httpfetch.go`
(`EscapeUntrusted`, `WrapUntrustedFrom`), `internal/engine/contract.go`
(`TriggerRequest`), `internal/httpapi/projects.go` (`projectInput.validate`,
`fieldErrors`, `budgetCap`, `projectView`), `internal/store/chief.go` (`ClaimRunReport`,
`ListUnreportedChannelRuns`: the channel report's queries filter on `trigger = channel`,
which is why `reported_at` is free for issue runs), `internal/store/projects.go`
(`ListProjects`, `UpdateProject`), the fakes `newFakeGitLab` and `newFakeGitHub` for the
style (yours are separate).

## Deliverables

1. **Settings (rule 1).** `internal/httpapi/projects.go`: `projectInput.Intake
   *intakeInput{Enabled bool; Label string; WorkflowID string}`; absent on update keeps
   the stored settings (read the project first, as the token path does), present
   replaces the three; validation with the exact 422 texts on `intake.label`,
   `intake.workflow_id` (`store.GetWorkflow`, `TemplateKey == nil`, `workflow.Parse`,
   `def.Project`, then `engine.ValidateInput(def.InputSchema, {"task":"x"})` joined into
   the message) and `intake.enabled` (no budget cap set, when D-055 says so; otherwise
   leave the check out and say so). `internal/store/projects.go`: `UpdateProjectIntake`
   is not needed if `UpdateProject` carries the columns (prerequisite); add
   `ListIntakeProjects(ctx, q) ([]Project, error)` (live, enabled, with a workflow,
   oldest first), `SetProjectIntakePoll(ctx, q, id string, at time.Time, lastError
   *string) error`. The representation's `intake` comes from the prerequisite's view;
   `projectView` fills `workflow_name` through the join the prerequisite made.
2. **Issue provider (rules 2, 3, 5).** New `internal/tools/issues.go`: `type Issue{Number
   int64; Title, Body, URL string; IsPullRequest bool}`, `type IssueProvider interface {
   ListLabelled(ctx, label string, limit int) ([]Issue, error); EnsureLabels(ctx, labels
   []IssueLabel) error; SwapLabels(ctx, number int64, add, remove string) error; Comment(ctx,
   number int64, body string) error }`, `IssueProviderFor(pc *project.Config, client
   *http.Client) IssueProvider` (and the fake-friendly `newIssueProvider(pc, client, base,
   path)`), GitLab and GitHub implementations over an `apiClient` exactly as rule 2's
   and rule 3's calls say (the GitHub list skips `pull_request` entries; GitHub's
   `DELETE .../labels/<label>` ignores 404; an "already exists" on label creation,
   GitLab 409 and GitHub 422, is success). `IssueLabel{Name, Color string}` and the three
   colours of rule 2. `internal/project/issues.go`: `IssueURL(s store.ProjectSettings, n
   int64) string` and `IssueTask(path string, issue tools.Issue, url string) string`
   building the text of rule 4 (title and body through `tools.EscapeUntrusted`, the body
   cut at 16 KiB with `[cut]`; the `<untrusted_content source="issue #<n>">` block). Your
   own fakes in `issues_test.go` (scripted issue lists, label and comment recorders).
3. **The loop (rules 2, 3, 5, 6).** New package `internal/intake`: `Loop{Pool, Projects
   *project.Resolver, Client *http.Client, Engine Triggerer, Interval, Timeout
   time.Duration, Log, Now func() time.Time, PublicBaseURL string}` with `Run(ctx)`
   (ticker, first pass at once, stops with ctx), `TickOnce(ctx) Summary` and
   `PollProject(ctx, projectID string) (Result{Taken, Closed int; Error string}, error)`
   (the handler's; `ErrIntakeDisabled` when the project's intake is off). Per project:
   the budget first (`project.CheckBudget`; a `*project.BudgetError` skips with the log
   line `intake: project <name> skipped: <text>` and the text into `intake_last_error`),
   the labels once per project per process (a `sync.Map` of ensured project ids), the
   listing (50), the taker per issue with the cap of 10 new runs per pass: `store.LiveRunForTrigger`
   (A1 adds one in `merge_requests.go`; until it lands write `liveIssueRun` in your
   `internal/store/intake.go` and say so) decides between "swap and comment only" and
   "trigger, then swap and comment"; the trigger request exactly as rule 3 (b), with
   `tools.RunURL(PublicBaseURL, run.ID)` in the start comment; `store.ErrIssueBusy` from
   the engine counts as taken; validation, rate-limit and budget refusals go into
   `intake_last_error` and leave the issue. Then the closer over
   `store.ListUnreportedIssueRuns(ctx, q, limit)` (new in `internal/store/intake.go`:
   `trigger = 'issue' AND status IN (terminal) AND reported_at IS NULL`, oldest first;
   with the project id parsed from `trigger_ref`): the label swap, the comment texts of
   rule 5 exactly (the merge request through `store.MergeRequestByRun`; the failure reason
   cut at 300 bytes), then `store.SetRunReportedAt(ctx, q, id, at)` (new; a plain update,
   not `ClaimRunReport`). `SetProjectIntakePoll` after each project with the pass's first
   error or nil. A provider error never stops the pass; a database error ends it with a
   log line. Errors stored go through `obs.Redact` and the token replacement the sync
   loop's `cleanError` does (copy the pattern; do not import `mergereq`).
4. **API (rules 2 and 8).** `internal/httpapi/projects_intake.go`: replace the stub;
   `PollProject` → 200 `{taken, closed, error?}`, 409 `intake_disabled` (`the project's
   intake is off`), 404 for a missing or deleted project. The runs list's `trigger`
   filter is A1's (`runs.go`); `GET /api/runs?trigger=issue` is covered by their
   conformance step. Conformance steps for the poll path (200 with a fake provider, 409,
   404) and a project saved with `intake` as `intakeConformanceSteps(t)` in
   `projects_intake_test.go` (`openapi_test.go` is shared: the coordinator appends the
   call at integration).
5. **Tests.** `internal/httpapi`: T5.23.1 (every text; a valid save; absent keeps),
   T5.23.9's handler part. `internal/tools`: the issue provider against your fakes
   (list with a pull request skipped, labels created and already present, swap with a
   404 on remove, comment). `internal/project`: T5.23.10 (`IssueURL` both providers, with
   and without `.git`) and `IssueTask` (title only, the escape of a planted closing tag,
   the 16 KiB cut). `internal/intake` with the `yuno_test` database, the engine's `newEnv`
   pattern or a fake `Triggerer`, a fixed clock and your fakes: T5.23.2, T5.23.3,
   T5.23.4, T5.23.5 (a live run with the ref: no second run, the swap repeated; the
   index refusing a concurrent insert through `store.CreateRun` directly), T5.23.6 (every
   outcome text; `reported_at` after; a provider failure leaves it null and the next pass
   repeats; `store.ClaimRunReport` on an issue run answers `ErrNotFound`), T5.23.7's
   intake half (re-labelling creates a new run with the same ref once the old one is
   terminal; a retry's cleared `reported_at` is reported again: set it null by hand and
   run the closer). Name tests after the rules.

## Rules of the house

- Everything runs in Docker, from this worktree in a throwaway container on the stack's
  network with your own test database (the compose `go` service cannot be used here:
  no `.env`, and compose would join the live stack). Package tests:

  ```
  docker run --rm --network yuno_default -v "$PWD:/src" -v yuno_gomod:/go/pkg/mod -v yuno_gocache:/root/.cache/go-build -w /src -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 -e DATABASE_URL='postgres://yuno:yuno@postgres:5432/yuno_test_b?sslmode=disable' yuno-go-tools:1.26 sh -c 'go test ./internal/intake/ ./internal/tools/ ./internal/project/ ./internal/store/ ./internal/httpapi/ -count=1 2>&1 | tail -60'
  ```

  vet with `go vet ./...` and `gofmt -l .` in the same container. Write long outputs to a
  file under `/tmp` and read the file. Never run `make run`, `make down`, `make clean`,
  `make test-*`, `make lint` or `docker compose ...` from this directory: a stack is
  running and must not be restarted; never run the whole suite: the coordinator does.
- Files you own: everything under `internal/intake/`; `internal/tools/issues.go` and
  `issues_test.go`; `internal/project/issues.go` and tests; `internal/store/projects.go`
  (the intake queries) and `internal/store/intake.go` and their tests;
  `internal/httpapi/{projects.go,projects_intake.go}` and their tests. Nothing else:
  `types.go`, `runs.go` and `merge_requests.go` in the store, `providers.go`,
  `mergerequest.go`, `chief.go`, `platform.go`, the engine, `server.go` (the coordinator
  starts your loop), `main_test.go`, `web/`, `docs/`, `api/openapi.yaml` are other hands'
  or frozen. If the contract or a frozen type proves wrong, say so in your report with
  the exact mismatch instead of editing it.
- No secrets in fixtures; the token never appears in a comment, a label call's log, an
  `intake_last_error` or a test; provider clients come from `tools.ProviderClient` (no
  redirects). Test tokens look like `glpat-not-a-real-token-00`.
- The issue's title and body are untrusted text: they reach the task only inside the
  block of rule 4 with closing tags escaped, and never a log line in full (log the
  number and the title's first 80 bytes).
- Product text (the comments, the 422 texts, the log line, the 409 message) exactly as
  the rules spell it; times from the injected clock; `>=` comparisons as 5.19 says.
- Comment with the DESIGN rule numbers as the code does. Report any rule you had to
  interpret, and anything you left out.
