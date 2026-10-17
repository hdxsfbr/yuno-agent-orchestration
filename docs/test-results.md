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
