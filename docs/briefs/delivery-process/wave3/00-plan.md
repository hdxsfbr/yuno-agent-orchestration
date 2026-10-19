# Delivery process wave 3: waves that advance themselves (DESIGN 5.29 rules 10 to 15, 17)

Sonnet 5.5 agents on disjoint files, one test database each, never `make` or `compose`
(the local stack is running). Each agent runs its packages' tests, `go vet`, `gofmt -l`
and staticcheck 2026.2.2 in throwaway containers before reporting; web runs `pnpm test`,
eslint and `tsc --noEmit`.

## Shared contracts (fixed here; build against them)

- `tools.RequestProvider.DefaultBranchStatus(ctx) (BranchStatus, error)` with
  `BranchStatus{State string; SHA string; At time.Time; URL string}`. `State` is one of
  `passed | failed | running | pending | none` (none: no pipeline or check on the default
  branch). GitLab: the newest pipeline with `ref=<default branch>`. GitHub: the combined
  status and check runs of the default branch's head commit.
- `tools.IssueProvider.Close(ctx, number int64) error` (GitLab `state_event=close`,
  GitHub `state=closed`; closing a closed issue is fine).
- `tools.AdvanceWaveFunc func(ctx context.Context, runID string) (AdvanceResult, error)`
  and `AdvanceResult{PlanID string; Wave int; Closed int; Released int; Last bool}` with
  JSON `plan_id, wave, closed, released, last`. A refusal (paused, not a ship run, wave
  not current) is `*tools.AdvanceRefused{Reason string}`, which the tool returns as a
  model error. `PlatformDeps.AdvanceWave` is late-bound in `server.go` like `Escalate`.
- Ship runs: `trigger = 'plan'`, `trigger_ref = 'plan:<plan id>:wave:<n>'`, unique on
  `(trigger, trigger_ref)` for `trigger = 'plan'` (migration `0018_waves.sql`). Input
  `{plan_id, feature, wave, last_wave}`.
- `ship_pipeline` nodes: `smoke_test` (approval) → `integrator` (agent, `auto_after:
  smoke_test`, tools `memory_save`, `advance_wave`, `escalate`, `finish`; output `{wave,
  last, lessons: [string]}`) → `accept_feature` (approval) when `state.integrator.last`
  is true, else `end`. `accept_feature` → `end`. No `on_reject` (a rejection ends the run
  `rejected`, which the plan side turns into a pause).
- Plan detail JSON (`GET /api/plans/{id}`) gains `main: {state, sha, at, url} | null` (the
  watcher's last read) and `ship_runs: [{wave, run_id, status}]`. Plan event `detail`
  carries `run_id` where a run is involved.

## Partition

A. Providers and the tool: `DefaultBranchStatus` on GitLab and GitHub
(`internal/tools/providers.go`), `IssueProvider.Close` (`issues.go`), the `advance_wave`
tool (`internal/tools/waves.go`: input `{run_token}`, side-effecting, idempotency key per
plan and wave, calls `AdvanceWave`), registration in `platform.go`,
`openclaw.PlatformTools`, the Integrator's grant in `internal/seed/seed.go`. Add the two
interface methods first, with the fakes, so B compiles.

B. The watcher and the plan lifecycle: `internal/waves/` (the loop every
`WAVE_WATCH_INTERVAL`, rule 12; release, rule 12; `Advance` for `AdvanceWaveFunc`, rule
14), migration `0018_waves.sql` (ship-run uniqueness; `plans.main_state, main_sha,
main_at, main_url`), `internal/store/plans.go` additions, `internal/plans/` (ship run
terminal: smoke or accept rejected → pause with the reason; accepted last wave → `done`
and `feature_done`; `accept_waiting` when the accept approval opens), plan detail
additions in `internal/httpapi/plans.go`, `api/openapi.yaml`, wiring in `server.go`.

C. `templates/ship_pipeline.json` per the contract, `seed.TemplateKeys`, the template
tests next to the existing delivery pipeline tests (`internal/engine/
delivery_pipelines_test.go`, `internal/httpapi/templates_delivery_test.go`).

D. The fake forge (rule 17) and eval support: `internal/forgefake/`, `cmd/yuno` role
`forgefake`, a compose service under profile `eval` with the Makefile's eval targets
bringing it up, and the eval runner's support for a case with a project on the fake forge
(`project:` block, `forge:` steps: merge open MRs, set the default branch's pipeline,
read issues). Eval 028 itself comes after A to C land.

E. Frontend: the wave timeline on the project page's Plans section (waves in order, each
with its tickets, ship run, smoke and accept approvals, main's status), and the approval
and run pages linking a ship run to its plan (`trigger_ref plan:<id>:wave:<n>`).
