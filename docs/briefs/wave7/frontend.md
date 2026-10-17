# Wave 7 frontend brief: projects in the UI

Spec: `docs/DESIGN.md` 5.17 (rules 3, 4, 7, 12, 13) and `api/openapi.yaml` (the
contract: `/api/projects` and `/api/projects/{id}` with the `Project`, `ProjectInput`,
`ProjectCheck` and `ProjectLastRun` schemas, `POST /api/projects/{id}/check`,
`RunCreate.project_id` (the `project` boolean is gone), `Run.project_id` and
`Run.project` (a snapshot `{name, repo_url, provider, default_branch, sandbox_network}`),
`ApprovalItem.run_status`, and `Status` without its `project` member). The backend is
other agents' work; code against the contract.

## Deliverables

1. `web/src/lib/api.ts`: types `ProjectSettings`, `Project`, `ProjectInput`,
   `ProjectLastRun`, `ProjectCheck`; `Run.project_id?`, `Run.project?`;
   `RunCreate.project_id?` replacing `project`; `ApprovalItem.run_status?`; remove
   `Status.project` and `ProjectStatus`; functions `listProjects`, `getProject`,
   `createProject`, `updateProject`, `deleteProject`, `checkProject`; a
   `web/src/lib/useProjects.ts` hook (`projectsKey`, `useProjects`) like `useSkills`.
2. `web/src/pages/ProjectsPage.tsx` and `web/src/components/project/ProjectDialog.tsx`:
   the table (name, repository shown as host and path, provider as "GitLab"/"GitHub",
   default branch, the last run's toolchain as a badge with the run's status, a
   "Network on"/"Network off" badge), New project, Edit, Check, Delete with
   `ConfirmDialog` ("Runs that used this project keep their history."). The form:
   name, repository URL, provider select ("Detect from the URL", "GitLab", "GitHub";
   send `""` for detect), default branch (placeholder `main`), token as
   `type="password"` with `autoComplete="off"` (required on create; on edit empty with
   the hint "A token is set. Enter a new one to replace it." and left out of the body
   when empty), sandbox network checkbox with a hint that it lets agents install
   dependencies and is for trusted repositories, description. 422 details map onto the
   fields as `SkillDialog` does. Check: `POST .../check` from the row, the result's
   `message` shown under the row with a green tone when `ok && can_push !== false &&
   branch_exists !== false` and a red tone otherwise (`role="status"`). Routes
   `/projects` and `/projects/:id` (the latter opens the dialog for that project, and
   falls back to the list when the id is unknown); the nav entry "Projects" sits
   between Workflows and Runs.
3. `RunWorkflowDialog.tsx`: when `workflow.definition.project` is true, a required
   "Project" select of the live projects (label `name`, first option "Choose a
   project"); when no project exists, the sentence "This workflow works on a project
   repository. Create one on the Projects page first." with a link to `/projects` and
   the Start button disabled. The request carries `project_id`. No selector when the
   workflow does not opt in.
4. `QuickRunDialog.tsx`: replace the status query and the checkbox with an optional
   "Project" select ("None" plus the live projects) shown only when projects exist;
   the request carries `project_id` when one is chosen.
5. Runs page: a "Project" column with the snapshot's name linking to
   `/projects/<project_id>` ("—" without one). Run header: a "Project" item with the
   same link.
6. Status page: the project panel becomes "Projects": the live projects (name,
   repository, provider, branch, network badge) from `/api/projects`, with a link to
   the Projects page, or "None yet. Create one on the Projects page." Remove the old
   single-project panel and its tests.
7. `ApprovalCard.tsx`: a `runEnded` prop; when true and the approval is pending and of
   kind `tool`, show "This run has already ended. Approving still executes the call;
   rejecting discards it." (`role="note"`). `ApprovalsSection` derives it from
   `runStatus` (terminal), `ApprovalsPage` from `a.run_status`.
8. `Builder.tsx`: the project switch's hint becomes "Each run of this workflow names a
   project, chosen when the run starts. The project's repository is cloned into the
   first agent's run directory, later agents receive the checkout and the Deployer can
   open a merge request."
9. Tests (vitest, with `installFakeApi` and fixtures): `ProjectsPage.test.tsx` (list
   renders the columns and badges; create posts the body with the token; edit shows
   the hint and omits `token` when left empty; check shows the message; delete asks
   and then sends DELETE; `/projects/:id` opens the dialog), the Run dialog (selector
   required for a project workflow, the no-project sentence, absent otherwise, body
   carries `project_id`), the quick-run select, the runs list and header link, the
   Status projects list, the approval card note. Keep every existing test green or
   update it where the contract changed (`StatusPage.test.tsx`, `AgentsPage.test.tsx`,
   `RunsPage.test.tsx`, `RunDetailPage*.test.tsx`, `WorkflowsPage.test.tsx`).
10. `make test-web` and `make lint` clean (eslint, tsc).

## Rules of the house

- Everything runs in Docker through the Makefile (`make test-web`, `make lint`; a
  single file with `docker compose --profile tools run --rm node pnpm vitest run
  src/pages/ProjectsPage.test.tsx`). Nothing installed on the host. Never run `make
  run`, `make down` or `make clean`: a stack is running and must not be restarted.
- Touch only `web/`. Do not edit `internal/`, `api/`, `docs/`, the Makefile or compose.
- No developer commentary in product copy: labels and hints are for the end user, never
  mention design rules, stubs or the backend.
- Match the existing components (`Badge`, `Card`, `Modal`, `ConfirmDialog`, `fields`,
  `QueryState`) and the pages' patterns (`SkillsPage`, `SkillDialog`, `RunsPage`).
- Report any rule you had to interpret, and anything you left out.
