# Wave 9 frontend brief C: merge request selectors, the iteration marks, intake settings and card, the trigger filter

Spec: `docs/DESIGN.md` 5.22 rule 12 and 5.23 rule 9, and `api/openapi.yaml` 0.6.0 (the
contract: `RunCreate.merge_request_id`, `Run.merge_request_id`, `Run.checkout_sha`,
`Run.merge_request` filled for a run that names a request, `GET /api/runs?trigger=&merge_request_id=`,
`ScheduleInput.merge_request_id`, `Schedule.merge_request_id` and `merge_request_number`,
`Project.intake` (`ProjectIntake`), `ProjectInput.intake` (`ProjectIntakeInput`), `POST
/api/projects/{id}/intake/poll` (`IntakePollResult`, 409 `intake_disabled`), `Run.trigger`
value `issue` with `trigger_ref` `<project id>#<n>`). The backend is other agents' work;
code against the contract. Read `docs/briefs/wave9/00-plan.md` first.

Already in the tree and worth reading before you write: `web/src/lib/api.ts` (`Run`,
`RunCreate`, `Project`, `ProjectInput`, `Schedule`, `MergeRequest`, `listRuns`,
`listMergeRequests`, `delegatedConversationId`), `web/src/components/workflow/RunWorkflowDialog.tsx`
and `web/src/components/QuickRunDialog.tsx` (the project selector: the merge request
selector sits under it), `web/src/components/schedule/*` (the form with its project
selector), `web/src/components/run/{RunHeader.tsx,MergeRequestCard.tsx}`,
`web/src/components/project/{ProjectDialog.tsx,ProjectMergeRequests.tsx,BudgetCard.tsx}`,
`web/src/pages/{ProjectPage.tsx,RunsPage.tsx}` (`RunsPage`'s status filter and the
trigger cell), `web/src/lib/useProjects.ts`, `web/src/test/{fakeApi.ts,fixtures.ts}`.

## Deliverables

1. `web/src/lib/api.ts`: the new fields and types (`Run.merge_request_id?`,
   `Run.checkout_sha?`, `RunTrigger` gains `"issue"`, `RunCreate.merge_request_id?`,
   `ScheduleInput.merge_request_id?: string | null`, `Schedule.merge_request_id?`,
   `Schedule.merge_request_number?`, `ProjectIntake`, `ProjectIntakeInput`,
   `Project.intake?`, `ProjectInput.intake?`, `IntakePollResult`); `listRuns` takes
   `trigger` and `merge_request_id`; `pollIntake(projectId)`. Helpers: `issueRef(run)`
   returning `{number, url}` for an issue run from `run.trigger_ref` (`<project id>#<n>`)
   and `run.project.repo_url` (`<repo url without .git>/-/issues/<n>` on GitLab,
   `/issues/<n>` on GitHub; undefined without a project snapshot), and
   `mergeRequestLabel(mr)` (`!12` or `#12` by provider) if none exists yet.
2. **Merge request selector** (5.22 rule 12), one component
   `web/src/components/project/MergeRequestSelect.tsx` used by `RunWorkflowDialog`,
   `QuickRunDialog` and the schedule form: shown only once a project is chosen; loads
   `listMergeRequests({project_id, state: "open"})`; options "None: open a new one"
   (default, sends no `merge_request_id`) and one per record as `!12 <title>` (`#12` on
   GitHub); hint "The run works on the request's branch and pushes onto it."; a loading
   and an error state like the project selector's; the server's 422 detail on
   `merge_request_id` maps onto the field. The schedule form sends `null` to clear and
   leaves the field out when untouched on edit, as `project_id` does; changing the
   project resets the selection to None.
3. **Run header and card** (5.22 rule 12): `RunHeader` shows `Updates merge request !12`
   (`pull request #12`), linked to the provider, next to the project link when
   `run.merge_request_id` is set; `MergeRequestCard` adds the line "This run updates it;
   opened by run <id>" with a link to `/runs/<merge_request.run_id>` when
   `run.merge_request_id === run.merge_request.id` and `run.merge_request.run_id !==
   run.id`. The runs list shows `!12` after the project name for such a run.
4. **Project page's Merge requests table** (`ProjectMergeRequests.tsx`): a "Runs" column
   with the opening run's link and, when any, "+<n> updates" linking to
   `/runs?merge_request_id=<id>`; the count from `listRuns({merge_request_id, limit:
   200})` per record is too many requests for a table: fetch
   `listRuns({project_id, limit: 200})` once and count client-side by
   `merge_request_id`, which the page already loads for its newest runs (reuse the
   query). `RunsPage` reads `merge_request_id` from the URL like `status`.
5. **Intake section on the project form** (5.23 rule 9, `ProjectDialog.tsx`): under a
   "Issue intake" label, an "Enabled" switch, a "Label" field (default `yuno`; hint
   "Issues carrying this label become runs. The platform moves it to <label>:running,
   <label>:done or <label>:failed as the work goes.", rendered with the current label
   value), a "Workflow" selector of the live operator workflows (`listWorkflows`, those
   without a `template_key`; "Choose a workflow" placeholder; required when enabled,
   client check first, the server's 422 details on `intake.label`, `intake.workflow_id`
   and `intake.enabled` map onto the fields); when the project has no cap and the form
   is enabling intake, the sentence "Set a daily or monthly budget before enabling
   intake." under the switch. On edit the section is prefilled from `project.intake`;
   the form sends `intake` only when the section was touched (absent keeps the stored
   settings).
6. **Intake card on the project page** (`web/src/components/project/IntakeCard.tsx`,
   rendered beside the Budget card): "Off" or "On: label <label>, workflow <name>", "Last
   poll <relative time>" or "Not polled yet", the last error as a muted line, and a "Poll
   now" button (disabled when off) calling `pollIntake`, then showing "Took <n>, closed
   <n>" and refreshing the project and its runs; a 409 shows the server's message.
7. **Runs list and header** (5.23 rule 9): a Trigger filter next to the status filter
   (all five values, URL-synced like `status`, passed to `listRuns`); for an issue run
   the trigger cell shows `issue #<n>` linking to the issue (from `issueRef`), and the
   run header shows the same link where the delegated-run link goes today.
8. **Tests** (vitest, `installFakeApi` and fixtures): `MergeRequestSelect.test.tsx`
   (hidden without a project, None by default, options from open records only, the sent
   body with and without `merge_request_id`, the 422 mapping), `RunWorkflowDialog` and
   `QuickRunDialog` and the schedule form tests extended for the selector,
   `RunHeader.test.tsx` (the Updates line, the issue link), `MergeRequestCard.test.tsx`
   (the updates line present and absent), `ProjectMergeRequests` (the Runs column),
   `ProjectDialog`/`ProjectPage` tests (the Intake section round-trips, absent when
   untouched, the budget sentence, the card's states, Poll now and its 409),
   `RunsPage.test.tsx` (the Trigger filter, the `issue #n` cell, the `!12` mark),
   `api.test.ts` for `issueRef`. Keep every existing test green or update it where the
   contract changed.
9. `make test-web` and `make lint` clean (eslint, tsc).

## Rules of the house

- Everything runs in Docker through the Makefile from the main checkout (`make
  test-web`, `make lint`; a single file with `docker compose --profile tools run --rm
  node pnpm vitest run src/components/project/IntakeCard.test.tsx`) or in a throwaway
  node container from a worktree. Nothing installed on the host. Never run `make run`,
  `make down` or `make clean`: a stack is running and must not be restarted.
- Touch only `web/`. Do not edit `internal/`, `api/`, `docs/`, the Makefile or compose.
- No developer commentary in product copy: labels and hints are for the end user, never
  mention design rules, waves, stubs or the backend. Times as the rest of the UI shows
  them; request numbers with the provider's sigil.
- Match the existing components (`Badge`, `Card`, `Modal`, `ConfirmDialog`, `fields`,
  `QueryState`, `Button`) and the pages' patterns (`ProjectDialog` for form sections,
  `BudgetCard` for a card with a state line, `RunsPage` for a URL-synced filter).
- Report any rule you had to interpret, and anything you left out.
