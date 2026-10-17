# Wave 7 backend brief (API): projects as records

Spec: `docs/DESIGN.md` 5.17 (rules 1 to 5, 7, 9, 12) and `api/openapi.yaml` (the
contract is already written: `/api/projects`, `/api/projects/{id}`,
`/api/projects/{id}/check`, the `Project*` schemas, `RunCreate.project_id`, `Run.project`
and `Run.project_id`, `ApprovalItem.run_status`, `Status` without `project`). Decision
D-038 in `DECISIONS.md`.

Already in the tree (ground truth, do not change): `internal/store/projects.go`
(`Project`, `ProjectSettings`, `ProjectLastRun`, `CreateProject(ctx, q, p, tokenEnc)`,
`GetProject`, `GetProjectByName`, `GetProjectByRepoURL`, `ListProjects`,
`UpdateProject`, `SetProjectToken`, `DeleteProject`, `GetProjectTokenEnc`,
`FillProjectLastRuns`), migration `0010_projects.sql`, `internal/project/cipher.go`
(`Cipher.Seal(projectID, token)`, `Open`, `ErrNoKey`, `ErrCannotRead`),
`internal/project/project.go` (`ValidateRepoURL`, `InferProvider`, `ParseProvider`,
`ValidateBranch`, `ConfigOf`, `Config.APIBase()`, `Config.Path()`),
`internal/project/resolver.go` (`Resolver{DB, Cipher}.Token(ctx, id)`),
`internal/project/bootstrap.go` (`Bootstrap(ctx, q, cipher, cfg)`),
`engine.TriggerRequest.ProjectID`, `tools.Delegation.ProjectID`, and in
`internal/httpapi/server.go` the fields `s.cipher` and `s.projects` plus the
`platformDeps` wiring. The engine's own validation of `project_id` (required for a
workflow with `project: true`, refused otherwise, unknown id) is another agent's work in
`internal/engine`; the API only passes `project_id` through (already done in `runs.go`).

## Deliverables

1. `internal/httpapi/projects.go`: handlers `listProjects`, `createProject`,
   `getProject`, `updateProject`, `deleteProject`, `checkProject`, routed in
   `server.go` exactly as the contract names them. Validation per 5.17 rules 1 to 3
   with path-qualified 422 details (`name`, `repo_url`, `provider`, `default_branch`,
   `token`, `description`); use the `project` package helpers for the URL, provider
   (inferred when empty, stored resolved), and branch; names trimmed, 1 to 64
   characters, unique ignoring case among live projects (check first, and map the
   unique index violation like `skills.go` does: `a project named "X" already exists`).
   `token` is required on create, optional on update (absent or empty keeps the stored
   one); it is sealed with `s.cipher` under the project's id and stored with
   `CreateProject` / `SetProjectToken`; when `s.cipher == nil`, a create, or an update
   that carries a token, answers 503 `project_tokens_unavailable` ("PROJECT_TOKEN_KEY
   is not set; no token can be stored"). The representation is the `store.Project`
   (never the token; `token_set` only) with `last_run` filled by
   `FillProjectLastRuns` on list and get. DELETE is `store.DeleteProject` (204). The
   read-only fields are accepted and ignored on input, as `skillInput` does.
2. The check (rule 5): `internal/tools/projectcheck.go`, `func CheckProject(ctx,
   client *http.Client, pc project.Config, apiBase string) CheckResult` using the
   package's `apiClient` (`providers.go`) and the auth helpers: GitLab `GET <base>`
   then `GET <base>/repository/branches/<branch>` reading `can_push`; GitHub
   `GET <base>` reading `default_branch` and `permissions.push`, then
   `GET <base>/branches/<branch>` for existence. `CheckResult{OK, Message,
   DefaultBranch, BranchExists, CanPush *bool, CheckedAt}` with the sentences of rule
   5; a 401/403 is "The token was refused (<status>).", a 404 on the repository is
   "Repository not found (404): check the URL and the token's scope.", transport
   errors and a 10 s timeout are `ok: false` with a short message. The token never
   appears in any message (`obs.Redact` the provider's text if you include it). The
   handler decrypts with `s.projects.Token(ctx, id)`; a decrypt failure is a result
   (`ok: false`, the resolver's message), not an HTTP error. Add
   `Deps.CheckTransport http.RoundTripper` (nil: default) so the httpapi test can point
   the check at an httptest fake, like `FetchTransport`.
3. `GET /api/status`: drop the `project` member and `projectStatus` (the contract no
   longer has it); fix `server_test.go` accordingly.
4. `internal/httpapi/server.go` `Run`: after `store.Migrate`, build the cipher from
   `cfg.ProjectTokenKey` and call `project.Bootstrap`; log `project bootstrapped from
   PROJECT_*` with the name on creation, a warning with the error when the setting is
   wrong or the key is missing, nothing when there is nothing to do.
5. `internal/httpapi/chief.go` delegator: pass `ProjectID: req.ProjectID` into the
   `TriggerRequest`.
6. `ApprovalItem.run_status` (rule 12): `store.ListApprovalItems` fills `RunStatus
   string` (`json:"run_status,omitempty"`) from the runs of the listed approvals
   (one query over the ids); the inbox handler needs no change beyond that.
7. Tests, in `internal/httpapi/projects_test.go` and
   `internal/tools/projectcheck_test.go` (T5.17.2, T5.17.3, T5.17.9): the 422 details
   listed in DESIGN; the representation never contains the token string anywhere in
   any response body (assert on the raw bytes, create and update with a fresh token,
   list and get); `token_set` true after create; PUT without `token` keeps `token_set`
   and leaves the stored blob unchanged (read `GetProjectTokenEnc` before and after);
   503 without a key; DELETE then GET 404 and the project gone from the list; the
   check against the existing fakes (`newFakeGitLab`, `newFakeGitHub` in
   `gitlab_test.go` and `mergerequest_test.go`: `refuse` answers 401 and `noPush`
   reports no push rights; the GitLab fake serves `/api/v4/projects/group%2Fdemo`, the
   GitHub fake `/repos/owner/demo`); the httpapi check endpoint through
   `CheckTransport`; the conformance steps in `openapi_test.go` for every new path;
   `GET /api/tools` lists `open_merge_request` without any project configured;
   `runs_test.go` updated for `project_id` (it still refers to the removed `project`
   boolean on line 33 and 62, which is why the package does not compile yet).
   `testConfig()` should carry a fixed `ProjectTokenKey` so the handlers have a cipher.

## Rules of the house

- Everything runs in Docker through compose; run package tests with
  `docker compose --profile tools run --rm go go test ./internal/httpapi/ ./internal/tools/ -count=1`
  (the go service has `DATABASE_URL` for the `yuno_test` database) and vet with
  `docker compose --profile tools run --rm --no-deps go go vet ./internal/httpapi/ ./internal/tools/`.
  Never run `make run`, `make down`, `make clean` or anything that restarts the running
  stack; never run the whole suite (`make test-go`): the coordinator does.
- Files you own: `internal/httpapi/{projects.go,projects_test.go,server.go,server_test.go,runs.go,runs_test.go,openapi_test.go,chief.go,main_test.go}`,
  `internal/store/approvals.go`, `internal/tools/{projectcheck.go,projectcheck_test.go}`.
  Do not edit anything else: `internal/engine`, `internal/tools/{chief,mergerequest,platform,providers}.go`
  and their tests, `internal/store/{projects,runs,types}.go`, `internal/project`,
  `internal/seed`, `web/`, `docs/`, `api/openapi.yaml` are other hands' or already final.
  If the contract in `api/openapi.yaml` proves wrong in the conformance test, say so in
  your report with the exact mismatch instead of editing it.
- No secrets in fixtures: test tokens look like `glpat-not-a-real-token-00`.
- Follow the existing patterns (`skills.go` for CRUD, `fieldErrors`, `writeValidation`,
  `storeError`, `decodeBody`), and comment with the DESIGN rule numbers as the code does.
- Report any rule you had to interpret, and anything you left out.
