# Wave 9 plan: iterate on an open merge request, issue intake

Spec: `docs/DESIGN.md` 5.22 and 5.23 (decisions D-044, D-045 and D-049 to D-053), the
roadmap `docs/ROADMAP.md`, and the contract `api/openapi.yaml` 0.6.0 (the wave 9 path,
schemas and fields carry `x-planned: wave 9` or "Planned for wave 9" in their
descriptions; the coordinator removes the markers at integration, once each is served).
The two open lines at the end of 5.22 and 5.23 are Andre's; the wave does not start
until they are answered and recorded (D-054, D-055).

## Agents

| Agent | Brief | Sections | Model |
|---|---|---|---|
| A1, backend iteration: engine, store, API | `backend-iteration-engine.md` | 5.22 rules 1, 2, 3, 7, 9, 11 | Sonnet 5.5 |
| A2, backend iteration: tools and prompts | `backend-iteration-tools.md` | 5.22 rules 4, 6, 8, 10; 5.23 rule 7 and the Chief's parts of rules 1, 8, 10 | Sonnet 5.5 |
| B, backend intake | `backend-intake.md` | 5.23 rules 1 to 6, 8 | Sonnet 5.5 |
| C, frontend | `frontend.md` | 5.22 rule 12, 5.23 rule 9 | Sonnet 5.5 |

Andre chose Sonnet 5.5 for every agent (2026-10-17, kept for wave 9); the delicate parts
(the queue-time checks and their index, the branch guard, the label state machine and
its crash recovery) are covered by the designs' exact rules and the independent review
rather than by a stronger builder. 5.22 is the roadmap's L-sized feature, so it is split
in two along the engine/tools seam, with the git helpers and the shared types frozen as
prerequisites.

All four start together. No two write the same file (partition below). Shared types,
the migration, the routes, the config, the git helpers and the engine interface are the
coordinator's prerequisites, landed before launch and frozen for the wave.

## Prerequisites the coordinator lands before launch (ground truth for every agent)

1. `internal/store/migrations/0013_wave9.sql`: `runs.merge_request_id TEXT REFERENCES
   merge_requests (id) ON DELETE SET NULL`, `runs.checkout_sha TEXT`;
   `CREATE UNIQUE INDEX runs_merge_request_live_idx ON runs (merge_request_id) WHERE
   merge_request_id IS NOT NULL AND status IN ('queued', 'running', 'waiting_approval')`
   (5.22 rule 1); `CREATE UNIQUE INDEX runs_issue_live_idx ON runs (trigger_ref) WHERE
   trigger = 'issue' AND status IN ('queued', 'running', 'waiting_approval')` (5.23 rule
   3); `schedules.merge_request_id TEXT REFERENCES merge_requests (id) ON DELETE SET
   NULL` (5.22 rule 7); `projects.intake_enabled BOOLEAN NOT NULL DEFAULT false`,
   `projects.intake_label TEXT NOT NULL DEFAULT 'yuno'`, `projects.intake_workflow_id TEXT
   REFERENCES workflows (id) ON DELETE SET NULL`, `projects.intake_polled_at TIMESTAMPTZ`,
   `projects.intake_last_error TEXT` (5.23 rule 1).
2. `internal/store/types.go`: `TriggerIssue = "issue"`; `Run` gains `MergeRequestID
   *string` (`db:"merge_request_id" json:"merge_request_id,omitempty"`) and `CheckoutSHA
   *string` (`db:"checkout_sha" json:"checkout_sha,omitempty"`); `MergeRequest` gains
   the method `Label() string` (`merge request !12` on GitLab, `pull request #12` on
   GitHub: every refusal text of 5.22 and 5.23 uses it); `ErrMergeRequestBusy` and
   `ErrIssueBusy` (the two live-run indexes' 23505, mapped in `CreateRun` and `RetryRun`
   like `ErrDuplicateTrigger`). `runs.go`: `runColumns`, `CreateRun` and `RetryRun` carry
   the two columns; `SetRunCheckoutSHA(ctx, q, id, sha string) error`; `ListRuns` takes a
   `RunFilter{Status, ProjectID, Trigger, MergeRequestID string; Limit int}` (the
   handler's call updated). `schedules.go`: `Schedule` gains `MergeRequestID *string` and
   `MergeRequestNumber *int64` (joined through `LEFT JOIN merge_requests m ON m.id =
   s.merge_request_id`, kept when the record is gone like `project_name`), carried by
   create and update. `projects.go`: `Project` gains the five intake columns as
   `IntakeEnabled bool`, `IntakeLabel string`, `IntakeWorkflowID *string`,
   `IntakePolledAt *time.Time`, `IntakeLastError *string` (all `json:"-"`), the view
   `Intake *ProjectIntake` (`db:"-" json:"intake,omitempty"`, filled by `fillIntake` after
   every scan with `WorkflowName` joined), and `ProjectIntake{Enabled bool; Label string;
   WorkflowID *string; WorkflowName *string; PolledAt *time.Time; LastError *string}` with
   the contract's JSON names; `CreateProject` and `UpdateProject` carry the three settings.
   The intake queries themselves are B's.
3. `internal/engine/contract.go`: `TriggerRequest.MergeRequestID string`.
   `internal/engine/trigger.go`: `validateInput` is exported as `ValidateInput(schema,
   input json.RawMessage) []string` (B's settings check uses it; the name only).
4. `internal/config/config.go`, `.env.example`, `compose.yaml`: `INTAKE_POLL_INTERVAL`
   (default 120 s), so `TestEnvExampleAndComposeCoverConfig` stays green.
5. `internal/project/git.go`: `CloneBranch(ctx, dest, branch string) error` (today's
   `cloneBranch`, exported) and `HeadSHA(ctx, repo string) (string, error)` (`git
   rev-parse HEAD`, trimmed, 40 hex); `ProposeOnto(ctx, checkout, branch, base, message
   string) (Proposal, error)` gains `base`: after the private clone of `branch`, when
   `base != ""` and the clone's head is not `base`, it returns `*ErrBranchMoved{Branch,
   Head, Base string}` (`Error()` is the text of 5.22 rule 4) and copies nothing; `""`
   skips the guard (the 5.20 same-run path and runs cloned before the migration).
   `Proposal.Commit` is the pushed commit's sha. Tests in `git_onto_test.go` for the
   guard (moved, equal, skipped). Frozen for the wave: A2 passes `run.CheckoutSHA`, A1
   records it.
6. `internal/httpapi/server.go`: the route `POST /api/projects/{id}/intake/poll` pointing
   at `pollIntake` in `projects_intake.go`, answering 501 `not_implemented` until B
   replaces it. `internal/httpapi/runs.go`: `listRuns` passes the `RunFilter` (the two new
   query parameters are A1's).

## File partition

| Agent | Owns (create or edit) | Must not touch |
|---|---|---|
| A1 | `internal/engine/{trigger.go,engine.go,node.go,retry.go,report.go}` and the new `internal/engine/wave9_iteration_test.go` (plus `report_test.go` for rule 9); `internal/store/merge_requests.go` and tests; `internal/httpapi/{runs.go,schedules.go}` and their tests; `internal/sched/scheduler.go` and tests | everything of A2, B and C; `types.go`, `runs.go`, `schedules.go` (store), `projects.go` (store), `contract.go`, `git.go`, migrations, `server.go`, `main_test.go`, `api/openapi.yaml` (report mismatches instead) |
| A2 | `internal/tools/{mergerequest.go,providers.go,readmergerequest.go,chief.go,platform.go}` and their tests and fakes (`gitlab_test.go`, `mergerequest_test.go`, `mergerequest_record_test.go`, `chief_test.go`, new `readmergerequest_test.go`); `internal/httpapi/chief.go` and tests; `internal/seed/seed.go` and tests; `templates/dev_pipeline.json`; `internal/openclaw/materialize.go` and tests | A1's, B's and C's files; `git.go`, `types.go`, `api/openapi.yaml`, `web/`, `docs/` |
| B | new package `internal/intake/`; `internal/tools/issues.go` and `issues_test.go` (its own fakes); `internal/project/issues.go` and tests; `internal/store/projects.go` (the intake queries) and `internal/store/intake.go` and their tests; `internal/httpapi/{projects.go,projects_intake.go}` and their tests | A1's, A2's and C's files; `types.go`, `runs.go` (store), `providers.go`, `mergerequest.go`, `chief.go`, `server.go`, `engine/*`, `api/openapi.yaml`, `web/`, `docs/` |
| C | `web/src/**` | everything outside `web/` |

A1 touches `engine.go` only inside `queueRun` (the record's checks and the 23505
mapping), `node.go` only inside `prepareProject`, `cloneInto` and the task block
(`taskMessageWith`), `retry.go` only for the two busy mappings, `report.go` only in
`runReportText`. A2's `providers.go` edits add methods (`Details`, `Threads`) and leave
the existing ones alone; B's issue methods live in `issues.go` on the same `apiClient`,
so the two never edit one function. If an agent finds it needs a file it does not own,
it stops and says so in its report rather than editing it.

## Integration (coordinator, after the agents report)

1. `server.go` `Run`: start B's loop (`go (&intake.Loop{...}).Run(ctx)` beside the
   scheduler, the budget watcher and the merge request sync) with the dependencies
   (`project.Resolver`, `s.checkClient`, the engine as `Triggerer`, the clock, the log);
   `platformDeps` passes the resolver and the client to A2's `read_merge_request`.
2. `openapi_test.go`: append the agents' conformance step functions
   (`iterationConformanceSteps`, `intakeConformanceSteps`) to `TestOpenAPIConformance`,
   add the new fields to `TestOpenAPIDocumentsChannelAndSchedulerFields`'s table, and
   remove the `x-planned` markers and "Planned for wave 9" phrases from
   `api/openapi.yaml`. Full suite: `make test-go`, `make test-web`, `make lint` (from
   the main checkout, never from a worktree while the stack runs).
3. One independent review scoped to the wave's new code (two Sonnet 5.5 reviewer agents
   with disjoint scopes as in wave 8, codex if its quota is back); filtered visibly into
   `docs/reviews/wave9-review.md`; one fix pass; one commit; merge to main when green
   (main auto-deploys).
4. Derived docs: README (Projects: iterating on a request, `read_merge_request`, the
   intake; the Chief's `merge_request` argument and `run_status.issue`), `docs/deploy.md`
   (`INTAKE_POLL_INTERVAL`, the `make seed-reset` reminder for the new prompts and
   allowlists), `docs/threat-model.md` (an issue's text is the task: whoever may label
   issues on the repository starts runs on the project, bounded by its budget; the
   request's threads reach the model as untrusted content; the branch guard; the known
   limitation L4 narrowed to the same-run path), `AI_USAGE.md`, `docs/test-results.md`
   (suite numbers, live verification), `docs/ROADMAP.md` "What happens next".
5. Live verification on the public instance after the deploy and `make seed-reset`
   (the token stays on the host; nothing printed):
   - iteration: on the playground's open `!4` (or a fresh Dev pipeline run's request),
     Andre leaves one review thread on a line; a run request with `merge_request_id`
     (then a Telegram message "fix the review comment on !4" to the Chief, which Andre
     sends) clones `yuno/…` (the `project_clone` span names the branch and the number),
     the Coder's trace shows `read_merge_request` returning the thread, the Deployer's
     `open_merge_request` answers `replayed` and `updated`, the request shows a second
     commit on the provider, and the record's sync shows the new head; a second request
     naming `!4` while the run is live is refused with `is being updated by run`;
   - the branch guard: a commit pushed by hand onto the branch during a run makes the
     Deployer's call answer the branch-moved text and push nothing;
   - intake: a budget on the playground, intake enabled with the Dev pipeline, Andre
     opens an issue and labels it `yuno`; Poll now (or the two-minute tick) creates the
     run, the issue carries `yuno:running` and the start comment; when the run ends the
     issue carries `yuno:done`, the finished comment with the request's link, and the
     request's description ends with `Closes #<n>`; re-labelling after a forced failure
     creates a new run.

## Review scope

New code only: the files in the partition, plus the prerequisites. The reviewer is told
which decisions stand (D-044, D-045, D-049 to D-055) so it does not re-open them, and
which limitation stays (L4 on the same-run push path, 5.20).
