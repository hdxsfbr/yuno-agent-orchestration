# Wave 9 prerequisites: what is landed and frozen before the four agents start

These are the "Prerequisites the coordinator lands before launch" of
`docs/briefs/wave9/00-plan.md`, written as one brief because a Sonnet 5.5 agent lands
them (Andre, 2026-10-18: every subagent on Sonnet). Spec: `docs/DESIGN.md` 5.22 rules
1, 2, 4 and 7 and 5.23 rules 1, 3 and 5 for what the columns, indexes and helpers mean;
decisions D-049 to D-055. Nothing here implements a feature: it adds columns, types,
plumbing and stubs that the agents' code will fill, so that no two of them edit one file.
Every existing test must stay green; the new helpers get their own tests.

Already in the tree and worth reading before you write: `internal/store/migrations/0012_wave8.sql`
(the style), `internal/store/types.go` (`Run`, `MergeRequest`, `TriggerManual` and
friends), `internal/store/runs.go` (`runColumns`, `CreateRun` with its 23505 mapping to
`ErrDuplicateTrigger`, `RetryRun`, `ListRuns`, `ClaimQueuedRun`), `internal/store/schedules.go`
(`Schedule`, `scheduleColumns`, `scheduleJoins`, create and update), `internal/store/projects.go`
(`Project`, `projectColumns`, `scanProject`, `ListProjects`, `CreateProject`,
`UpdateProject`), `internal/engine/contract.go` (`TriggerRequest`), `internal/engine/trigger.go`
(`validateInput`), `internal/config/config.go` with `.env.example` and `compose.yaml`
(`MERGE_REQUEST_SYNC_INTERVAL` is the pattern), `internal/project/git.go` (`cloneBranch`,
`Propose`, `ProposeOnto`, `propose`, `ErrNoChanges`) and `git_onto_test.go`,
`internal/httpapi/server.go` (`routes`) and `internal/httpapi/mergerequests.go` at commit
`e140dad` (`git show e140dad:internal/httpapi/mergerequests.go`: how wave 8's 501 stubs
looked), `internal/httpapi/runs.go` (`listRuns`).

## Deliverables

1. **Migration** `internal/store/migrations/0013_wave9.sql`, commented like 0012:
   ```sql
   ALTER TABLE runs ADD COLUMN merge_request_id TEXT REFERENCES merge_requests (id) ON DELETE SET NULL;
   ALTER TABLE runs ADD COLUMN checkout_sha TEXT;
   CREATE UNIQUE INDEX runs_merge_request_live_idx ON runs (merge_request_id)
       WHERE merge_request_id IS NOT NULL AND status IN ('queued', 'running', 'waiting_approval');
   CREATE UNIQUE INDEX runs_issue_live_idx ON runs (trigger_ref)
       WHERE trigger = 'issue' AND status IN ('queued', 'running', 'waiting_approval');
   ALTER TABLE schedules ADD COLUMN merge_request_id TEXT REFERENCES merge_requests (id) ON DELETE SET NULL;
   ALTER TABLE projects ADD COLUMN intake_enabled BOOLEAN NOT NULL DEFAULT false;
   ALTER TABLE projects ADD COLUMN intake_label TEXT NOT NULL DEFAULT 'yuno';
   ALTER TABLE projects ADD COLUMN intake_workflow_id TEXT REFERENCES workflows (id) ON DELETE SET NULL;
   ALTER TABLE projects ADD COLUMN intake_polled_at TIMESTAMPTZ;
   ALTER TABLE projects ADD COLUMN intake_last_error TEXT;
   ```
   Check how migrations are discovered (`internal/store/migrate.go`) so the file is
   picked up and ordered after 0012.
2. **Types and store plumbing.** `internal/store/types.go`: `TriggerIssue = "issue"`
   beside the other triggers; `Run` gains `MergeRequestID *string` (`db:"merge_request_id"
   json:"merge_request_id,omitempty"`) and `CheckoutSHA *string` (`db:"checkout_sha"
   json:"checkout_sha,omitempty"`), commented with DESIGN 5.22 rules 1 and 2;
   `MergeRequest` gains `func (m *MergeRequest) Label() string` returning `merge request
   !<n>` for GitLab and `pull request #<n>` for GitHub (every refusal text of 5.22 and
   5.23 uses it); `ErrMergeRequestBusy = errors.New("another live run is updating this
   merge request")` and `ErrIssueBusy = errors.New("this issue already has a live run")`.
   `internal/store/runs.go`: `runColumns` and `CreateRun` carry `merge_request_id` and
   `checkout_sha`; `CreateRun` and `RetryRun` map a 23505 on `runs_merge_request_live_idx`
   to `ErrMergeRequestBusy` and on `runs_issue_live_idx` to `ErrIssueBusy` (the
   `ErrDuplicateTrigger` pattern; `RetryRun` returns them instead of `ErrNotFound` for
   those two constraint names); `SetRunCheckoutSHA(ctx, q, id, sha string) error`;
   `ListRuns(ctx, q, f RunFilter) ([]Run, error)` with `RunFilter{Status, ProjectID,
   Trigger, MergeRequestID string; Limit int}` (empty members do not filter; the limit
   rule as today), and every caller updated (`internal/httpapi/runs.go`, `grep -rn
   "store.ListRuns"`). `internal/store/schedules.go`: `Schedule` gains `MergeRequestID
   *string` (`db:"merge_request_id" json:"merge_request_id,omitempty"`) and
   `MergeRequestNumber *int64` (`db:"merge_request_number" json:"merge_request_number,omitempty"`,
   joined as `m.number AS merge_request_number` through `LEFT JOIN merge_requests m ON
   m.id = s.merge_request_id`, so it survives the record's deletion like `project_name`
   does not need to: the record's row is kept by `ON DELETE SET NULL`, so the join simply
   finds nothing then), carried by `CreateSchedule` and the update, and the `DueSchedule`
   scan still works. `internal/store/projects.go`: `Project` gains `IntakeEnabled bool
   db:"intake_enabled" json:"-"`, `IntakeLabel string db:"intake_label" json:"-"`,
   `IntakeWorkflowID *string db:"intake_workflow_id" json:"-"`, `IntakePolledAt *time.Time
   db:"intake_polled_at" json:"-"`, `IntakeLastError *string db:"intake_last_error"
   json:"-"`, `IntakeWorkflowName *string db:"intake_workflow_name" json:"-"` (joined:
   `projectColumns` becomes a select over `projects p LEFT JOIN workflows w ON w.id =
   p.intake_workflow_id` with `w.name AS intake_workflow_name`; adjust every query that
   uses `projectColumns`), and the view `Intake *ProjectIntake db:"-" json:"intake,omitempty"`
   filled by `fillIntake()` right after every scan (`scanProject`, `ListProjects`); `type
   ProjectIntake struct { Enabled bool json:"enabled"; Label string json:"label";
   WorkflowID *string json:"workflow_id,omitempty"; WorkflowName *string
   json:"workflow_name,omitempty"; PolledAt *time.Time json:"polled_at,omitempty";
   LastError *string json:"last_error,omitempty" }`; `CreateProject` and `UpdateProject`
   carry `intake_enabled`, `intake_label`, `intake_workflow_id` (the poll columns are
   written by B's own query later). `ProjectSettings` (the run's snapshot) is unchanged.
3. **Engine contract.** `internal/engine/contract.go`: `TriggerRequest.MergeRequestID
   string` with a comment citing DESIGN 5.22 rule 1 (the engine ignores it for now: A1
   reads it). `internal/engine/trigger.go`: rename `validateInput` to `ValidateInput`
   (exported, same body and doc; every caller updated).
4. **Config.** `internal/config/config.go`: `IntakePollInterval time.Duration` from
   `INTAKE_POLL_INTERVAL` (default 120 s) beside `MergeRequestSyncInterval`;
   `.env.example` gets the commented line in the same block as the other intervals;
   `compose.yaml` gets `INTAKE_POLL_INTERVAL: ${INTAKE_POLL_INTERVAL:-120s}` in the api
   env block, so `TestEnvExampleAndComposeCoverConfig` stays green.
5. **Git helpers.** `internal/project/git.go`: export `cloneBranch` as `CloneBranch(ctx,
   dest, branch string) error` (`Clone` calls it); `HeadSHA(ctx, repo string) (string,
   error)` (`git rev-parse HEAD` in `repo`, trimmed; an error for anything but 40 hex);
   `ProposeOnto(ctx, checkout, branch, base, message string) (Proposal, error)` gains
   `base`: after the private clone of `branch`, when `base != ""` and the clone's head
   (`HeadSHA`) differs, return `*ErrBranchMoved{Branch, Head, Base string}` whose
   `Error()` is exactly `the branch <branch> has new commits since this run cloned it;
   nothing was pushed. Finish and report it; a new run on the request starts from the
   branch as it is now` and copy nothing; `""` skips the guard. `propose` takes the base
   through; `Propose` passes `""`. Update the one caller (`internal/tools/mergerequest.go`
   `updateRecorded`: pass `""` for now, A2 passes the run's sha later) so the tree
   builds. Tests in `git_onto_test.go`: the guard refuses when a commit was pushed to the
   branch after `base`, passes when equal, is skipped with `""`; `HeadSHA` on a fresh
   clone.
6. **Route and stub.** `internal/httpapi/server.go`: `mux.HandleFunc("POST
   /api/projects/{id}/intake/poll", s.pollIntake)` next to the projects routes;
   `internal/httpapi/projects_intake.go`: `pollIntake` answering 501 `not_implemented`
   with the message `planned for wave 9` as wave 8's stubs did. `internal/httpapi/runs.go`
   `listRuns`: build the `RunFilter` from the existing `status`, `project_id` and `limit`
   (the two new query parameters are A1's).
7. **Tests.** `internal/store`: migration applies on a fresh schema (the existing
   migrate tests); `CreateRun` with `MergeRequestID` and `CheckoutSHA` round-trips;
   `ErrMergeRequestBusy` from a second live run naming one record (insert a
   `merge_requests` row through `InsertMergeRequest` on a first run, then two runs with
   `MergeRequestID`: the second insert fails; a terminal first run lets the second in);
   `ErrIssueBusy` the same way with `Trigger: TriggerIssue` and one `TriggerRef`;
   `RetryRun` returning the busy errors; `SetRunCheckoutSHA`; `ListRuns` with each
   filter; the schedule round trip with `merge_request_id` and the joined number; the
   project round trip with the three intake settings and `Intake` filled, `WorkflowName`
   joined. `internal/project`: deliverable 5's tests. `internal/config`:
   `TestEnvExampleAndComposeCoverConfig`. Then the whole `./internal/...` must be green
   against your database.

## Rules of the house

- Everything runs in Docker. From this worktree the compose `go` service cannot be used
  (no `.env` here, and compose would join the live stack), so run tests in a throwaway
  container on the stack's network with your own test database:

  ```
  docker run --rm --network yuno_default -v "$PWD:/src" -v yuno_gomod:/go/pkg/mod -v yuno_gocache:/root/.cache/go-build -w /src -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 -e DATABASE_URL='postgres://yuno:yuno@postgres:5432/yuno_test_pre?sslmode=disable' yuno-go-tools:1.26 sh -c 'go test ./internal/... -count=1 2>&1 | tail -40'
  ```

  (replace the package list for a quicker loop; `go vet ./...` and `gofmt -l .` the same
  way). Write long outputs to a file under `/tmp` and read the file. Never run `make
  run`, `make down`, `make clean`, `make test-*`, `make lint` or `docker compose ...`
  from this directory: a stack is running and must not be restarted.
- Files you may touch: the ones named above, and only there. Do not start any feature
  work (no queue-time checks, no clone of a branch, no intake loop, no UI): the four
  agents own those files from the moment you report. If a prerequisite proves wrong or
  impossible as written, say so in your report with the exact mismatch and your proposal
  instead of improvising a different shape.
- No secrets in fixtures; test tokens look like `glpat-not-a-real-token-00`.
- Comment with the DESIGN rule numbers as the code does. Report any rule you had to
  interpret, and anything you left out, as a short list at the end.
