# Test results, 2026-10-16

Real run on the submission commit's parent (`0c3afdd` plus the smoke fix), on the
development machine (Linux, Docker 28, compose v2), everything inside containers. There
was no CI service when this was recorded; these commands were run by hand before every
commit and recorded here for this one. A GitLab pipeline was added afterwards
(`.gitlab-ci.yml`, `docs/deploy.md`): its deploy job runs on the public host's runner and
has deployed from it; its test job, which runs these same commands, waits for a `yuno-ci`
runner on a second Docker host and has not run yet.

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
