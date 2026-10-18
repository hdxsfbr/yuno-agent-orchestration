# Test results, 2026-10-18 (wave 9: iterate on an open merge request, issue intake; DESIGN 5.22 and 5.23)

Run on the development machine from the worktree branch `claude/eloquent-wu-44b501`
after the fix pass and before its merge to `main`, everything inside throwaway containers
on the stack's network (the compose tool services cannot be used from a worktree while
the stack runs); CI runs the same commands on `main` and deploys the commit.

| Command | Result |
|---|---|
| `go test ./... -count=1` (against a `yuno_test_coord` database; what `make test-go` runs) | 21 packages ok, 0 failures (new package `internal/intake`; new: the queue-time checks and the live-run indexes, the branch clone and `checkout_sha`, the branch guard, `read_merge_request` and its fakes, the Chief's `merge_request` argument, schedules naming a request, the intake settings, provider issue client, taker and closer, the poll endpoint, `Closes #<n>`, the conformance steps of both features) |
| `pnpm test` (vitest; what `make test-web` runs) | 43 files, 827 tests passed (new: the merge request selector in three dialogs, the header and card lines, the Runs column, the Intake section and card, the Trigger filter and issue links, the schedule toggle's detail) |
| `go vet ./...`, staticcheck 2026.2.2, `gofmt -l`, eslint, `tsc --noEmit` (what `make lint` runs) | clean (`internal/obs/redact.go` was unformatted before the wave and is untouched) |
| review | two Sonnet 5.5 reviewer agents (backend; API, store and UI): one high (a second run for an issue whose label swap never worked), five mediums and the kept lows fixed in one pass by two Sonnet 5.5 fix agents, the rest recorded as limitations; `docs/reviews/wave9-review.md` |

Live verification on the public instance, 2026-10-18 18:37 to 18:43 UTC, from the
coordinator session over ssh with `scripts/deploy/verify-wave9.sh` fed on stdin (the API
token stays in the host's `.env`; nothing printed):

| Step | Result |
|---|---|
| deploy | pipeline on `6db1207` (the fix-pass commit, `main` fast-forwarded from the worktree branch): migration `0013_wave9.sql` applied, `/api/status` at `6db1207` at 18:37:38 UTC |
| `make seed-reset` | Coder, Reviewer, Deployer and Chief of Staff reset (the `read_merge_request` allowlist entries and the prompt lines of 5.22 rule 10 and rule 8), Dev pipeline and Research pipeline reset; the Telegram binding kept |
| iterate on `!4` (5.22) | `POST /api/runs` with the Dev pipeline, the playground and `merge_request_id` of the record of `!4`: queued with `merge_request_id` set; a second request a second later refused `422 merge_request_id: merge request !4 is being updated by run <id>`; the run cloned `yuno/5084648102e2`, the Coder and the Reviewer called `read_merge_request` four times (0 unresolved threads, the request as one untrusted block), the gate and the tool approval were approved from the API, the Deployer's `open_merge_request` answered `replayed: true, updated: true` on `!4`, and the run succeeded in 20 steps at $0.32; `checkout_sha` moved to the pushed commit `a4c59df…`, and `POST /api/merge-requests/{id}/sync` then read `head_sha a4c59df…` back from GitLab (the request has the new commit on top of its branch) |
| intake (5.23) | `PUT /api/projects/{id}` with `intake {enabled, label yuno, workflow Dev pipeline}` on the playground (caps $5 a day, $30 a month set since wave 8) answered the settings with `workflow_name`; `POST .../intake/poll` answered `{taken: 0, closed: 0}` with `polled_at` set and no `last_error` (the three derived labels were created on the repository). No issue carried the label yet: taking an issue, the label swap, the comments and `Closes #<n>` wait for Andre to label one; the two-minute tick then takes it and this table gains the row |
| not verified live | the branch guard (a commit pushed by hand onto the branch during a run) and the Telegram phrasing "on !4" to the Chief (Andre sends); both are covered by the unit tests |


# Test results, 2026-10-17 (wave 8: project instructions and notes, budgets, merge request records, retry; DESIGN 5.18 to 5.21)

Run on the development machine on the `wave8` branch after the fix pass and before its
merge to `main`, everything inside containers; CI runs the same commands on `main` and
deploys the commit.

| Command | Result |
|---|---|
| `make test-go` (`go test ./... -count=1` against the `yuno_test` database) | 20 packages ok, 0 failures (new package `internal/mergereq`; new: project notes and the two task blocks, the budget windows, refusal and alert ledger, the merge request record in the side effect's transaction, the provider status and merge mappings, the sync loop and its reports, the retry transaction with its exhausted-limits checks, the facts, merge request and retry handlers and their conformance steps) |
| `make test-web` (vitest) | 40 files, 669 tests passed (new: the project page and its cards, the budget and instructions fields, the spend column, the merge request card, Retry with the exhausted-limits dialog, the timeline mark, `run.retry` in the trace) |
| `make lint` (`go vet`, staticcheck 2026.2.2, eslint, `tsc --noEmit`) | clean |
| review | two Sonnet 5.5 reviewer agents (backend; API, store and UI): no high, four mediums and the kept lows fixed in one pass by two Sonnet fix agents, the rest recorded as limitations; `docs/reviews/wave8-review.md` |

Live verification on the public instance, 2026-10-18 01:34 to 01:45 UTC, from the
coordinator session over ssh with a verification script fed on stdin (the API token
stayed on the host; nothing printed):

| Step | Result |
|---|---|
| deploy | pipeline on `0ff8401`: migration `0012_wave8.sql` applied (the `project_facts`, `project_budget_alerts` and `merge_requests` tables exist), `/api/status` at the merge commit |
| `make seed-reset` | the Coder and the Reviewer reset (the project-notes prompt lines and the memory tools in their allowlists); memory then enabled on both through the API |
| instructions | PUT on `yuno-playground` with a 293-byte text: stored and returned, `spend` present, no token in the body |
| a Dev pipeline run on the playground | "add a gcd(a, b) function computing the greatest common divisor, with tests": the gate and the merge request card approved from the API; succeeded in 10 steps and 10 tool calls at $0.19; `merge_request` on the run: `!4`, open, pipeline none, branch `yuno/5084648102e2`; Sync answered the head sha, 0 unresolved threads, no error |
| the task | the first visit's task carried the "Project instructions from the operator" block; the Coder saved the note `yuno-playground build/test layout`, and the Reviewer's task carried the "Project notes saved by earlier runs" block with it |
| budget | the daily cap set to $0.18 against $0.19 spent today: a run request answered `429 project_budget_reached` with the text and the details (`window: day`, `spent_usd`, `until 2026-10-19T00:00:00Z`); within the next watcher tick the warning and the reached alerts were sent into the Chief of Staff's conversation (two `run reported` lines keyed `budget-<project>-day-…`, delivered to Telegram); the caps then set to $5 a day and $30 a month |
| merge from the API | refused by GitLab with 401: the playground's token has the Developer role and `main` is protected, so it may read the request but not merge it. The first deploy answered `502 provider_error`; the follow-up commit maps a 401 or 403 on the merge call to `409 merge_request_not_mergeable` with "the project's token is not allowed to merge this request (HTTP 401); give it a role that may merge into the target branch". Merging !4 from the page needs the token's role raised on GitLab (operator's call); the merge success path is covered by the unit tests against the fakes |
| retry | a quick run with `max_steps: 1` failed with `step limit 1 exceeded` on visit 1; the bare retry answered `409 limits_exhausted` with `max_steps: 1 of 1 used`; the retry with `max_steps: 12` answered 202 with `retries: 1`, and the run succeeded on visit 2 in 5 steps at $0.03 |


## Wave 7 (projects as records, DESIGN 5.17), 2026-10-17

Run on the development machine on the `projects-as-records` branch before its merge
as `f08aac5`, everything inside containers; CI ran the same commands on `main` and
deployed the commit.

| Command | Result |
|---|---|
| `make test-go` (`go test ./... -count=1` against the `yuno_test` database) | 19 packages ok, 0 failures (new: cipher and store round trip, projects API and the write-only token, the check against the GitLab and GitHub fakes and a redirect, engine clone from a project snapshot with the local bare remote, bootstrap idempotency, `delegate_task`'s project branches, `list_projects`, the merge request in the report and the late follow-up) |
| `make test-web` (vitest) | 32 files, 451 tests passed (new: Projects page and form, the selectors in both run dialogs, the project link on runs, the Status list, the ended-run note) |
| `make lint` (`go vet`, staticcheck 2026.2.2, eslint, `tsc --noEmit`) | clean |
| review | codex out of quota; two Claude reviewer agents (Opus on the engine, tools and UI; Sonnet on the API and the check) found no token leak and nothing above medium; the medium findings (provider clients following redirects, a PUT re-pointing a project's host without a new token, the bootstrap recreating a deleted project) and the low ones worth it were fixed before the commit |

Live verification on the public instance, 2026-10-17 19:24 to 19:28 UTC, from the
coordinator session over ssh (the API token stayed on the host; nothing printed):

| Step | Result |
|---|---|
| deploy | pipeline on `f08aac5`: migration `0010_projects.sql` applied, `PROJECT_TOKEN_KEY` generated into the host's `.env` by `make run-public`, `/api/status` without its `project` member |
| bootstrap | `project bootstrapped from PROJECT_*`: `yuno-playground` (GitLab, `main`, network off, `token_set` true, no token in any response) |
| `make seed-reset` | the Chief of Staff reset (the new prompt and `list_projects`), both template workflows reset |
| Check on the bootstrapped project | `Token accepted: default branch main, push allowed.` (`branch_exists` and `can_push` true) |
| a second project | created through the API with the same repository and a token: `token_set` true and no token in the body; Check green; deleted (204); the list back to one project |
| a Dev pipeline run naming the project | `project_clone` span: `main` at depth 1 in 619 ms, toolchain `python`; Coder added `power(base, exponent)` with tests; Reviewer approved; the gate and the merge request card were approved from the API within the tool's wait (no late grant this time); the Deployer's title was `Add power(base, exponent) function with tests` (46 characters); run succeeded in 62 s at $0.17 with 11 steps and 8 tool calls |
| the merge request | `yuno-playground!2` from branch `yuno/e8dfa53268bd` against `main` |

The last check, a Telegram message to the Chief of Staff naming the playground
project, was sent by Andre the same evening and reported as working; the Chief's
`list_projects` and the project on `delegate_task` are also covered by the unit tests.

# Test results, 2026-10-17 (project repository, DESIGN 5.16)

Run by hand on the development machine before the merge of the `project-repository`
branch, everything inside containers; CI runs the same commands on `main`.

| Command | Result |
|---|---|
| `make test-go` (`go test ./... -count=1` against the `yuno_test` database) | 19 packages ok, 0 failures (new: `internal/project`, engine clone/carry/propose, tool against GitLab and GitHub fakes, materialiser sandbox patch, seed reset) |
| `make test-web` (vitest) | 27 files, 370 tests passed (new: toolchain badge, merge request link, builder opt-in, status panel) |
| `make lint` (`go vet`, staticcheck 2026.2.2, eslint, `tsc --noEmit`) | clean |
| `make probe-sandbox` with `yuno-sandbox-node:latest` and `yuno-sandbox-go:latest` (per-agent image and network override, hot reload, restore) | all checks passed, twice |
| image builds (`docker/sandbox-{python,node,go}.Dockerfile`, `docker/go.Dockerfile`) | built; each image answers with its toolchain as uid 1000 |

# Test results, 2026-10-16

Real run on the submission commit's parent (`0c3afdd` plus the smoke fix), on the
development machine (Linux, Docker 28, compose v2), everything inside containers. There
was no CI service when this was recorded; these commands were run by hand before every
commit and recorded here for this one. A GitLab pipeline was added afterwards
(`.gitlab-ci.yml`, `docs/deploy.md`) and now runs these same commands on every push to
`main`; see the CI run below.

| Command | Result |
|---|---|
| `make test-go` (`go test ./... -count=1` against the `yuno_test` database) | 19 packages ok, 0 failures |
| `make test-web` (vitest) | 27 files, 325 tests passed |
| `make lint` (`go vet`, staticcheck 2026.2.2, eslint, `tsc --noEmit`) | clean |
| `make test-race` (engine and event packages under the race detector) | ok, 0 races |
| `make test-integration` (serial live tests against the running gateway) | ok (openclaw 4.7 s, engine 8.0 s) |
| `make probe-openclaw` (a real sandboxed turn through the gateway) | all checks passed |
| `make probe-approval` (plugin approval RPCs) | all checks passed |
| `make clean && make run` (fresh database, migrations 0001–0007, seed) | ready, version stamped `1941f75` at the time |
| `make smoke` (webhook mode, API token set) | 12 checks passed |
| `make status-p95` (200 requests to `/api/status` through nginx) | p50 0.7 ms, p95 0.9 ms, max 2.1 ms |
| `EVAL_PROVIDER=anthropic make eval-final` (every split, rubric graded by the model) | 24 of 24 cases passed, end-to-end 24 of 24, rubric 4 of 4 at 1.000; report `evals/reports/final.md` |
| `make eval-baseline` (public split, wave 3) | 17 of 17, `evals/reports/baseline.md` |

Live Telegram check, 06:38 and 06:41 UTC: two direct messages delivered over the
webhook (nginx log shows Telegram's address and a 200), answered by the bound Coder
in 5 s and 3 s, mirrored into `conversations` (API count 2 then 4).

Fresh checkout (`scripts/fresh-checkout.sh`, 2026-10-16 06:45 UTC): an empty clone with
`.env.example` copied to `.env` and no secrets; `make run` reached readiness in 34 s
(images built, migrations, seed), `make smoke` passed, `make test-go` passed all 19
packages, `make clean` succeeded.

Known partial check: the Telegram approval prompt's reply path (the prompt reaches the
chat; a reply resolving it has not been observed).

## CI run, 2026-10-16

Pipeline 57534 on commit `8aa891c`, test job on the `yuno-ci` droplet runner (fresh
checkout, example environment, scripted model, Telegram off), 4 min 53 s:

| Step | Result |
|---|---|
| `make run` | stack built and ready, demo agents seeded |
| `make test-go` | 18 packages ok, 0 failures |
| `make test-web` | 27 files, 325 tests passed |
| `make lint` | clean |
| `make smoke` | 10 checks passed, Telegram checks skipped (mode off) |
| deploy job (played by hand after the green test) | `make run-public` and `make smoke` on the public host, 12 of 12 |

The first two CI runs failed and both failures were real: `TestEnvExampleAndComposeCoverConfig`
did not know that compose reads `COMPOSE_PROFILES` from `.env` itself (test fixed), and a
fresh checkout has no `web/node_modules`, which the development machine always had
(`make test-web` and `make lint` now install them first). Pipeline 57515 and 57524 hold
the failing traces.

## Wave 5 (the Chief of Staff), 2026-10-16

Pipeline 57708 on commit `12e6478`, test job on the `yuno-ci` runner, 5 min 1 s:

| Step | Result |
|---|---|
| `make run` | stack built and ready; seed created the six demo agents, the Chief of Staff and both template workflows |
| `make test-go` | 18 packages ok, 0 failures (new: chat-token gate, chat tools, report-back, seed, migration 0008 upgrade test) |
| `make test-web` | 27 files, 362 tests passed |
| `make lint` | clean |
| `make smoke` | 10 checks passed, Telegram checks skipped (mode off) |
| deploy job | retried once (an untracked copy of the new probe on the host blocked the checkout), then `make run-public` and `make smoke` 12 of 12 |

Live verification on the public instance, 2026-10-17 00:35 to 00:47 UTC, from the
operator's phone: the Chief of Staff answered a roster question with `list_agents` and
`list_workflows`, delegated "invert a binary tree, with tests" to Coder (`delegate_task`,
run `01a1474b`, trigger `channel`, succeeded in 25 s at $0.06), the platform's report
was mirrored with the run link six milliseconds after the run ended and relayed by the
Chief one second later, and a follow-up question was answered with `run_status`. One
defect found and fixed afterwards: the conversation mirror dropped an assistant reply that
followed two parallel tool calls (the gateway announces such a turn first with only its
tool calls, then with its text under the same message id; the mirror marked the id seen
too early). The review is `docs/reviews/wave5-review.md`.

## Wave 6 (project repositories) live verification, 2026-10-17

Public instance at `b5b8f64` with `PROJECT_*` pointing at a private playground project on
the same GitLab (`andrebatista/yuno-playground`: a two-function Python package with
tests), sandbox network off. A Dev pipeline run started from the API with the task "add a
multiply function with tests":

| Step | Result |
|---|---|
| clone | `project_clone` span: main at depth 1 in 638 ms, toolchain `python` |
| Coder | added `multiply` and four test cases on the checkout, `python -m pytest` 3 passed |
| Reviewer | read `git diff main`, re-ran the tests, approved |
| gate | approved in the UI by the operator |
| Deployer | called `open_merge_request`; the tool approval was not answered within the gate's nine-minute wait, the Deployer finished with `pending_approval` and the run succeeded |
| late grant | the operator approved the tool card afterwards; the engine executed the call itself: branch `yuno/014865386a60`, merge request !1 against `main`, recorded under `pending_effects` and `side_effects` |

Cost of the run: $0.18. The merge request's diff is the four lines of `multiply` and
eight lines of tests; its description carries what changed, the test runs and the review
verdict. One nit for later: the Deployer used the whole task text as the merge request's
title.
