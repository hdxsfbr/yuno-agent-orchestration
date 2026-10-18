# Wave 9 backend brief A2: iterate on a merge request, the tools side (5.22), `Closes #n` (5.23)

Spec: `docs/DESIGN.md` 5.22 rules 4, 6, 8 and 10 (with the amendments to 5.16 rules 10
and 14, 5.17 rule 8, 5.15 rule 2 and the 5.7 table), 5.23 rule 7 and the Chief's parts of
5.23 rules 1, 8 and 10 (`list_projects.intake`, `run_status.issue`, the prompt sentence);
decisions D-044, D-050, D-051, D-053; `api/openapi.yaml` 0.6.0 (`GET /api/tools` lists
`read_merge_request` with `chat: true`; nothing else of yours is HTTP). Read
`docs/briefs/wave9/00-plan.md` first: the prerequisites there are ground truth and
frozen, in particular `project.Config.ProposeOnto(ctx, checkout, branch, base, message)`
with `*project.ErrBranchMoved`, `Run.MergeRequestID`, `Run.CheckoutSHA`,
`store.SetRunCheckoutSHA`, `MergeRequest.Label()`, `TriggerIssue`, and the project's
intake columns on `store.Project` (`IntakeEnabled`, `IntakeLabel`). A1 writes
`store.MergeRequestByNumber(ctx, q, projectID, number)` in `merge_requests.go` on the
same day: code against that signature and say in your report if it did not land.

Already in the tree and worth reading before you write: `internal/tools/mergerequest.go`
(`run`, `updateRecorded`, `record`, `requestNoun`, `requestSigil`), `internal/tools/providers.go`
(`apiClient`, `RequestProvider`, the two `Status` implementations and the GraphQL
helper: your `Details` and `Threads` methods sit beside them), `internal/tools/memory.go`
(`recallText`: how a result is fit under the output cap inside one untrusted block),
`internal/tools/httpfetch.go` (`WrapUntrustedFrom`, `EscapeUntrusted`), `internal/tools/chief.go`
(`delegateTaskSchema`, `projectForDelegation`, `describeDelegation`, `NewRunStatus`,
`NewListProjects`, `ChatToolNames`), `internal/tools/platform.go` (`Register` of every
platform tool and `PlatformDeps`), `internal/httpapi/chief.go` (`delegator.Delegate`),
`internal/seed/seed.go` (the prompts and allowlists), `templates/dev_pipeline.json`
(the deployer task), the fakes `newFakeGitLab` (`gitlab_test.go`) and `newFakeGitHub`
(`mergerequest_test.go`).

## Deliverables

1. **`open_merge_request` on a run that names a request (rule 4).** In `run`, after the
   advisory lock: read the run (`store.GetRun` through `call.Tx`); when
   `run.MergeRequestID` is set, `store.GetMergeRequest` is the record to update (then the
   run's own record by `MergeRequestByRun` as today). `updateRecorded` takes the base:
   `pc.ProposeOnto(ctx, checkout, rec.SourceBranch, base, in.Title)` with `base` =
   `*run.CheckoutSHA` when set, `""` otherwise (log `branch guard skipped: no checkout
   sha` through the result's text is not possible: return it as a `Result` note? No: the
   tool has no run log; leave the guard skipped silently and say so in your report; the
   engine's log line of the edge case is A1's if they want it). A `*project.ErrBranchMoved`
   is the `*ModelError` with the exact text of rule 4; after a successful push write
   `store.SetRunCheckoutSHA(ctx, call.Tx, call.RunID, proposal.Commit)` (a failure is a
   tool error naming the step, the push having landed). The same-run path (the run's own
   record) passes `""` as the base: unchanged behaviour, L4 stays recorded for it. The
   tool's description says a run that updates a request pushes onto it and that `replayed`
   without `updated` means nothing new was pushed.
2. **`Closes #<n>` (5.23 rule 7).** In `run`, when the request is created (not found open,
   not replayed) and `run.Trigger == store.TriggerIssue`, append `\n\nCloses #<n>` (the
   number after `#` in `trigger_ref`) to the description unless it already contains
   `closes #<n>` ignoring case; never on an update.
3. **`read_merge_request` (rule 6).** New `internal/tools/readmergerequest.go`:
   `ReadMergeRequestName = "read_merge_request"`, `NewReadMergeRequest(cfg
   ReadMergeRequestConfig{Projects ProjectResolver; Pool *pgxpool.Pool; Client
   *http.Client; MaxOutputBytes int; APIBase, RepoPath string})`, risk `read_only`, hits
   `external`, timeout 15 s, `Untrusted: true`, `Chat: true`, schema with `tokenProperties`
   plus `number` (string) and `project` (string), both optional at the schema level (the
   rule decides). Resolution as rule 6: run token → `Projects.ForRunID` and the run's
   request (`FillRunMergeRequests`-equivalent: `runs.merge_request_id` then
   `MergeRequestByRun`) when `number` is empty, the texts `this run has no project` and
   `this run has no merge request; give number`; chat token → `project` by name through
   the same matching as `projectForDelegation` (reuse it or factor a `projectByName`),
   `give project and number` when either is missing, and the project's token through a
   new `ProjectResolver.ForProjectID`-style read: the resolver has `Token(ctx,
   projectID)` and `ConfigOf(settings, token)`; take `store.GetProject` for the settings
   (a deleted project answers `no project named X`). `number` parsing: `12`, `!12`, `#12`,
   a URL whose path has `/merge_requests/<n>` or `/pull/<n>`; else `number must be a
   merge request number or URL`. Provider methods in `providers.go` on `RequestProvider`:
   `Details(ctx, number) (RequestDetails, error)` (title, description, state as the store
   constants, source and target branch, URL, head sha; `ErrRequestNotFound` on 404) and
   `Threads(ctx, number) ([]Thread, error)` (`Thread{ID, Path string; Line *int; Author,
   Body string; CreatedAt *time.Time}`; GitLab from the discussions, GitHub from the
   GraphQL query of rule 6 with `comments(first: 1)`; a GitHub GraphQL refusal returns a
   typed `*ErrThreadsUnavailable{Message}` so the tool sets `threads: null` and
   `threads_error`), and the CI status through today's `Status` (or a shared `ciStatus`
   helper: do not duplicate the mapping tables). Output exactly the JSON of rule 6
   (`threads` oldest first, at most 50; `description` cut at 8192 bytes and each `body` at
   2048 with `…`; `threads_omitted` counting the newest dropped until the whole text fits
   under `MaxOutputBytes` inside `WrapUntrustedFrom(ReadMergeRequestName, json)`; the
   model error `no <Label()> on project <name>` on 404). Register it in `platform.go`
   always (`PlatformDeps` gains what it needs); `ChatToolNames` gains it (sorted) and
   the test that keeps the registry's `Chat` flags in step must still pass. Extend the two
   fakes with the details, discussions-with-positions and GraphQL-threads routes, scripted.
4. **The Chief (rule 8; 5.23 rules 1 and 8).** `chief.go`: `delegateTaskSchema` gains
   `merge_request` (string, the description from the rule); `delegateInput.MergeRequest
   *string`; after the project is resolved, `mergeRequestForDelegation(ctx, pool,
   projectID, raw)`: `merge_request needs a project` without one, the number or URL parse
   of deliverable 3 (share the parser), `store.MergeRequestByNumber`, the texts `no
   <merge request !n|pull request #n> on project <name>` (the noun from the project's
   provider when there is no record) and `<Label()> is <state>` for a final one; the id
   goes on `Delegation.MergeRequestID` (add the field; `internal/httpapi/chief.go`
   `Delegate` passes it as `TriggerRequest.MergeRequestID`). `describeDelegation` ends
   with `, updating <merge request !n|pull request #n>` when given (the sigil from the
   input's form or the project's provider: the description has no database, so use `!`
   when the input carried `!` or a GitLab URL, `#` when it carried `#` or a GitHub URL,
   and `!` for a bare number; say so in your report). `run_status`: `merge_request.role`
   (`updates` when `run.MergeRequestID` names the record, else `opened`), `issue:
   {number, url}` for `run.Trigger == TriggerIssue` (the URL from `project.IssueURL`,
   B's helper in `internal/project/issues.go`; until it lands build it inline as the
   rule says: `<repo url without .git>/-/issues/<n>` GitLab, `/issues/<n>` GitHub).
   `list_projects`: `intake: {enabled, label}` from the project's columns.
5. **Prompts and templates (rule 10; 5.23 rule 10).** `internal/seed/seed.go`: the
   Coder's, Reviewer's and Deployer's prompts gain the sentences of 5.22 rule 10
   verbatim; `read_merge_request` joins the Coder's and the Reviewer's allowlists; the
   Chief's prompt gains the sentence of 5.22 rule 8 and the one of 5.23 rule 10 (its
   allowlist follows `ChatToolNames`). `templates/dev_pipeline.json`: the deployer task
   says "open or update a merge request" and names both statuses
   (`merge_request_opened`, `merge_request_updated`); the template's description
   mentions updating. `internal/openclaw/materialize.go`: `ProjectRule` gains the
   clause "or pushed onto the merge request the run updates" after "when a merge request
   is opened". `seed_test.go` and the materialiser tests follow; `seed --reset-shipped`
   already re-applies prompts, allowlists and template definitions.
6. **Tests.** `internal/tools`: T5.22.4 through the tool with the fakes and a bare
   repository whose branch the checkout was cloned from (one commit on top with the
   clone's commit as parent, `replayed` and `updated`, no second record, `checkout_sha`
   updated; a second call passes the guard; an unchanged tree answers `replayed` only),
   T5.22.5 (a commit pushed between the clone and the call answers the text and pushes
   nothing; an empty `checkout_sha` skips the guard), T5.22.6's tool half (a named record
   turned merged answers the final-state text), T5.23.8 (`Closes #<n>` appended, not
   duplicated, not on update, not for a manual run), T5.22.7 (every case listed in the
   design's test line, both fakes), T5.22.8 (`delegate_task` cases, the approval sentence,
   `run_status.merge_request.role` and `issue`, `list_projects.intake`). `internal/seed`
   and `internal/openclaw`: T5.22.12.

## Rules of the house

- Everything runs in Docker, from this worktree in a throwaway container on the stack's
  network with your own test database (the compose `go` service cannot be used here:
  no `.env`, and compose would join the live stack). Package tests:

  ```
  docker run --rm --network yuno_default -v "$PWD:/src" -v yuno_gomod:/go/pkg/mod -v yuno_gocache:/root/.cache/go-build -w /src -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 -e DATABASE_URL='postgres://yuno:yuno@postgres:5432/yuno_test_a2?sslmode=disable' yuno-go-tools:1.26 sh -c 'go test ./internal/tools/ ./internal/httpapi/ ./internal/seed/ ./internal/openclaw/ -count=1 2>&1 | tail -60'
  ```

  vet with `go vet ./...` and `gofmt -l .` in the same container. Write long outputs to a
  file under `/tmp` and read the file. Never run `make run`, `make down`, `make clean`,
  `make test-*`, `make lint` or `docker compose ...` from this directory: a stack is
  running and must not be restarted; never run the whole suite: the coordinator does.
- Files you own: `internal/tools/{mergerequest.go,providers.go,readmergerequest.go,chief.go,platform.go}`
  and their tests and fakes; `internal/httpapi/chief.go` and tests; `internal/seed/seed.go`
  and tests; `templates/dev_pipeline.json`; `internal/openclaw/materialize.go` and tests.
  Nothing else: `git.go`, `types.go`, `merge_requests.go` (A1 writes `MergeRequestByNumber`
  there), `issues.go` (B's provider methods; yours are `Details` and `Threads` only),
  the engine, `server.go`, `runs.go`, `web/`, `docs/`, `api/openapi.yaml` are other hands'
  or frozen. If a frozen signature or the contract proves wrong, report the exact
  mismatch instead of editing it.
- The token never appears in a result, a record, a log line or a fixture; provider text
  goes through `obs.Redact`; the texts a request carries (description, thread bodies)
  reach the model only inside the untrusted block with closing tags escaped (5.7 rule
  5). Test tokens look like `glpat-not-a-real-token-00`; provider clients come from
  `ProviderClient` (no redirects).
- Product text (the model-facing errors, the prompt sentences, the tool descriptions)
  exactly as the rules spell it; `Label()` for every request noun where a record exists.
- Comment with the DESIGN rule numbers as the code does. Report any rule you had to
  interpret, and anything you left out.
